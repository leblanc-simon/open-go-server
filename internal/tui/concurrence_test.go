package tui

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
)

// backendBloquant tient Apply jusqu'à ce que le test le libère, ou que son
// contexte soit annulé : c'est une saga en vol.
type backendBloquant struct {
	fakeBackend
	libérer  chan struct{}
	démarré  chan struct{}
	enVol    atomic.Int32
	maxEnVol atomic.Int32
	terminés atomic.Int32
	annulé   atomic.Bool
	once     sync.Once
}

func nouveauBackendBloquant(t *testing.T) *backendBloquant {
	b := &backendBloquant{libérer: make(chan struct{}), démarré: make(chan struct{}, 8)}
	b.projects = append(b.projects, project(t, "shop.fr", naming.EnvProd))
	return b
}

func (b *backendBloquant) action(ctx context.Context) {
	n := b.enVol.Add(1)
	if n > b.maxEnVol.Load() {
		b.maxEnVol.Store(n)
	}
	b.démarré <- struct{}{}
	select {
	case <-b.libérer:
	case <-ctx.Done():
		b.annulé.Store(true)
	}
	b.enVol.Add(-1)
	b.terminés.Add(1)
}

func (b *backendBloquant) Apply(ctx context.Context, _ *model.Project) (string, error) {
	b.action(ctx)
	return "fini", ctx.Err()
}

func (b *backendBloquant) Restart(ctx context.Context, _ *model.Project) error {
	b.action(ctx)
	return ctx.Err()
}

// lancer applique les touches au modèle et rend la dernière commande, sans
// l'exécuter : le test décide quand l'action tourne.
func lancer(t *testing.T, m *Model, keys ...tea.Msg) tea.Cmd {
	t.Helper()
	var cmd tea.Cmd
	for _, k := range keys {
		_, cmd = m.Update(k)
		// « a » calcule d'abord le plan que la confirmation montre (D112) : cette
		// lecture revient avant la touche suivante.
		if km, ok := k.(tea.KeyMsg); ok && km.String() == "a" && cmd != nil {
			_, cmd = m.Update(cmd())
		}
	}
	return cmd
}

func estQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// Une action à la fois : une seconde, demandée pendant la première, est refusée
// en le disant (Q-101).
func TestUneSeuleActionÀLaFois(t *testing.T) {
	b := nouveauBackendBloquant(t)
	m := New(b)
	drive(m, m.Init()())
	apply := lancer(t, m, key("a"), key("o"))
	if apply == nil {
		t.Fatal("la confirmation doit lancer l'action")
	}
	if cmd := lancer(t, m, key("s")); cmd != nil || m.mode == modeConfirm {
		t.Error("une seconde action ne doit pas être proposée pendant la première")
	}
	if m.Status() == "" {
		t.Error("le refus doit être dit")
	}
	go apply()
	<-b.démarré
	close(b.libérer)
}

// « q » pendant une action attend qu'elle ait rendu la main ; Ctrl+C l'annule
// par son contexte, puis attend aussi. Les deux rendaient tea.Quit sur-le-champ,
// la saga abandonnée entre deux étapes.
func TestQuitterPendantUneActionAttendSaFin(t *testing.T) {
	for _, touche := range []string{"q", "ctrl+c"} {
		t.Run(touche, func(t *testing.T) {
			b := nouveauBackendBloquant(t)
			m := New(b)
			drive(m, m.Init()())
			apply := lancer(t, m, key("a"), key("o"))
			msgs := make(chan tea.Msg, 1)
			go func() { msgs <- apply() }()
			<-b.démarré

			var k tea.Msg = key("q")
			if touche == "ctrl+c" {
				k = tea.KeyMsg{Type: tea.KeyCtrlC}
			}
			if _, cmd := m.Update(k); estQuit(cmd) {
				t.Fatal("la sortie ne doit pas précéder la fin de l'action")
			}
			if touche == "q" {
				if b.annulé.Load() {
					t.Error("« q » attend la fin, il n'annule pas")
				}
				close(b.libérer)
			}
			msg := <-msgs
			if touche == "ctrl+c" && !b.annulé.Load() {
				t.Error("Ctrl+C doit annuler le contexte de l'action")
			}
			if _, cmd := m.Update(msg); !estQuit(cmd) {
				t.Error("la sortie demandée doit suivre la fin de l'action")
			}
			// Une action annulée rend le code 9 ; attendue jusqu'au bout, non.
			if m.Interrupted() != (touche == "ctrl+c") {
				t.Errorf("interrompue = %v après %s", m.Interrupted(), touche)
			}
		})
	}
}

// SIGTERM, reçu par le vrai programme : main annule le contexte de la commande,
// et le programme ne rend la main qu'après l'action. Bubble Tea traduisait le
// signal en sortie immédiate, et l'action tournait encore quand Run revenait.
func TestSIGTERMAttendLaFinDeLAction(t *testing.T) {
	b := nouveauBackendBloquant(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inR, inW := io.Pipe()
	defer inW.Close()
	done := make(chan *Model, 1)
	go func() {
		m, err := run(ctx, b, tea.WithInput(inR), tea.WithOutput(io.Discard))
		if err != nil {
			t.Error(err)
		}
		done <- m
	}()
	// Laisser charger l'inventaire, puis appliquer.
	time.Sleep(100 * time.Millisecond)
	for _, k := range []string{"a", "o"} {
		if _, err := inW.Write([]byte(k)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case <-b.démarré:
	case <-time.After(5 * time.Second):
		t.Fatal("l'action n'a pas démarré")
	}
	cancel()
	select {
	case m := <-done:
		if b.terminés.Load() != 1 || !b.annulé.Load() {
			t.Errorf("le programme est sorti avant la fin de l'action : terminés=%d annulé=%v",
				b.terminés.Load(), b.annulé.Load())
		}
		if m == nil || !m.Quitting() || !m.Interrupted() {
			t.Error("le modèle doit avoir demandé la sortie, sur une interruption (code 9)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("le programme ne s'est pas fermé après l'interruption")
	}
	if b.maxEnVol.Load() != 1 {
		t.Errorf("%d actions simultanées", b.maxEnVol.Load())
	}
}

// Ctrl+C en mode filtre (« / » pendant une action) était avalé par le filtre :
// il interrompt l'action comme ailleurs.
func TestCtrlCEnModeFiltreInterromptLAction(t *testing.T) {
	b := nouveauBackendBloquant(t)
	m := New(b)
	drive(m, m.Init()())
	apply := lancer(t, m, key("a"), key("o"))
	msgs := make(chan tea.Msg, 1)
	go func() { msgs <- apply() }()
	<-b.démarré
	if _, cmd := m.Update(key("/")); cmd != nil {
		t.Fatal("le filtre ne lance rien")
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); estQuit(cmd) {
		t.Fatal("la sortie ne doit pas précéder la fin de l'action")
	}
	msg := <-msgs
	if !b.annulé.Load() {
		t.Error("Ctrl+C en mode filtre doit annuler l'action")
	}
	if _, cmd := m.Update(msg); !estQuit(cmd) || !m.Interrupted() {
		t.Error("l'interface doit se fermer sur l'interruption, une fois l'action revenue")
	}
}

// Un Ctrl+C arrivé quand l'action avait déjà abouti ne la fait pas compter pour
// interrompue : le code de la commande suit son résultat.
func TestCtrlCTardifNeFaitPasRendreLeCode9(t *testing.T) {
	b := nouveauBackendBloquant(t)
	m := New(b)
	drive(m, m.Init()())
	apply := lancer(t, m, key("a"), key("o"))
	msgs := make(chan tea.Msg, 1)
	go func() { msgs <- apply() }()
	<-b.démarré
	close(b.libérer)
	msg := <-msgs // l'action a abouti, son message n'est pas encore traité
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); estQuit(cmd) {
		t.Fatal("la sortie attend le retour de l'action")
	}
	if _, cmd := m.Update(msg); !estQuit(cmd) {
		t.Fatal("la sortie demandée doit suivre la fin de l'action")
	}
	if m.Interrupted() {
		t.Error("une action aboutie ne rend pas le code 9")
	}
}

// SIGHUP d'une session ssh coupée (D122) : main annule le contexte, et la
// lecture du terminal échoue en EIO. Bubble Tea sortait alors de Run sur
// l'erreur de lecture, sans attendre l'action en vol : le processus sortait
// sous la saga.
func TestTerminalFerméAttendLaFinDeLAction(t *testing.T) {
	b := nouveauBackendBloquant(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inR, inW := io.Pipe()
	type fin struct {
		err error
		// terminés est relevé au retour de run, avant toute autre chose.
		terminés int32
	}
	done := make(chan fin, 1)
	go func() {
		_, err := run(ctx, b, tea.WithInput(inR), tea.WithOutput(io.Discard))
		done <- fin{err, b.terminés.Load()}
	}()
	time.Sleep(100 * time.Millisecond)
	for _, k := range []string{"a", "o"} {
		if _, err := inW.Write([]byte(k)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case <-b.démarré:
	case <-time.After(5 * time.Second):
		t.Fatal("l'action n'a pas démarré")
	}
	// Le terminal disparaît d'abord, le signal arrive ensuite : la lecture a
	// échoué avant que l'interruption n'atteigne le modèle.
	_ = inW.CloseWithError(syscall.EIO)
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case f := <-done:
		if f.terminés != 1 || !b.annulé.Load() {
			t.Errorf("le programme est sorti avant la fin de l'action : terminés=%d annulé=%v",
				f.terminés, b.annulé.Load())
		}
		if !errors.Is(f.err, ErrInterrupted) {
			t.Errorf("une interruption doit rendre le code 9 : %v", f.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("le programme ne s'est pas fermé après l'interruption")
	}
}
