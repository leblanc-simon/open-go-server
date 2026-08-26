package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/naming"
)

// fakeBackend est un parc en mémoire : l'interface est ainsi éprouvable sans
// système, sans terminal et sans provider.
type fakeBackend struct {
	projects  []*model.Project
	snapshots map[string]Snapshot
	applied   []string
	restarted []string
	renewed   []string
	created   []WizardAnswers
	applyErr  error
	diffLines []string
}

func (f *fakeBackend) PrepDomain() string { return "preprod-pp.net" }

func (f *fakeBackend) Projects() ([]*model.Project, error) { return f.projects, nil }

func (f *fakeBackend) Snapshot(_ context.Context, p *model.Project) (Snapshot, error) {
	if s, ok := f.snapshots[p.ID()]; ok {
		return s, nil
	}
	return Snapshot{Health: "sain"}, nil
}

func (f *fakeBackend) Apply(_ context.Context, p *model.Project) (string, error) {
	f.applied = append(f.applied, p.ID())
	if f.applyErr != nil {
		return "✗ échec", f.applyErr
	}
	return "✓ utilisateur\n✓ vhost\n\n2 étape(s) appliquée(s)", nil
}

func (f *fakeBackend) Diff(_ context.Context, p *model.Project) ([]string, error) {
	return f.diffLines, nil
}

func (f *fakeBackend) Restart(_ context.Context, p *model.Project) error {
	f.restarted = append(f.restarted, p.ID())
	return nil
}

func (f *fakeBackend) RenewCertificate(_ context.Context, p *model.Project) error {
	f.renewed = append(f.renewed, p.ID())
	return nil
}

func (f *fakeBackend) Create(_ context.Context, answers WizardAnswers) (string, error) {
	f.created = append(f.created, answers)
	return "projet créé", nil
}

func project(t *testing.T, host string, env naming.Env) *model.Project {
	t.Helper()
	p := model.New(host, env, model.KindSymfony)
	p.Spec.App.Version = "6.4"
	if err := p.Normalize(filepath.Join(t.TempDir(), "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	return p
}

// drive envoie une suite de touches au modèle et renvoie la vue finale.
func drive(m *Model, keys ...tea.Msg) string {
	for _, msg := range keys {
		updated, cmd := m.Update(msg)
		m = updated.(*Model)
		// Les commandes sont exécutées en série : le test reste déterministe.
		for cmd != nil {
			msg := cmd()
			if msg == nil {
				break
			}
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, c := range batch {
					if inner := c(); inner != nil {
						updated, _ = m.Update(inner)
						m = updated.(*Model)
					}
				}
				break
			}
			updated, cmd = m.Update(msg)
			m = updated.(*Model)
		}
	}
	return m.View()
}

func key(s string) tea.Msg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func typeText(text string) []tea.Msg {
	out := make([]tea.Msg, 0, len(text))
	for _, r := range text {
		out = append(out, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return out
}

func TestListShowsProjectsAndDetail(t *testing.T) {
	backend := &fakeBackend{
		projects: []*model.Project{project(t, "shop.fr", naming.EnvProd), project(t, "autre.fr", naming.EnvProd)},
		snapshots: map[string]Snapshot{
			"shopfr": {Health: "sain", Services: "2/2", Certificate: "shopfr", CertDays: 87},
		},
	}
	m := New(backend)
	view := drive(m, m.Init()())
	if !strings.Contains(view, "shopfr") || !strings.Contains(view, "autrefr") {
		t.Errorf("les projets doivent être listés :\n%s", view)
	}
	// Le détail porte sur le projet sélectionné.
	if !strings.Contains(view, "php-symfony") {
		t.Errorf("le détail doit décrire le projet :\n%s", view)
	}
	// La barre d'aide énonce les actions disponibles.
	for _, want := range []string{"appliquer", "quitter", "filtrer"} {
		if !strings.Contains(view, want) {
			t.Errorf("aide incomplète, %q absent :\n%s", want, view)
		}
	}
}

func TestNavigationChangesDetail(t *testing.T) {
	backend := &fakeBackend{projects: []*model.Project{
		project(t, "autre.fr", naming.EnvProd), project(t, "shop.fr", naming.EnvProd),
	}}
	m := New(backend)
	_ = drive(m, m.Init()())
	first := m.current().ID()
	view := drive(m, key("down"))
	if m.current().ID() == first {
		t.Error("la navigation doit changer la sélection")
	}
	if !strings.Contains(view, m.current().ID()) {
		t.Errorf("le détail doit suivre la sélection :\n%s", view)
	}
}

func TestFilterNarrowsList(t *testing.T) {
	backend := &fakeBackend{projects: []*model.Project{
		project(t, "shop.fr", naming.EnvProd), project(t, "autre.fr", naming.EnvProd),
	}}
	m := New(backend)
	_ = drive(m, m.Init()())
	msgs := append([]tea.Msg{key("/")}, typeText("shop")...)
	view := drive(m, msgs...)
	if strings.Contains(view, "autrefr") {
		t.Errorf("le filtre doit masquer les autres projets :\n%s", view)
	}
	if !strings.Contains(view, "shopfr") {
		t.Errorf("le projet filtré doit rester visible :\n%s", view)
	}
	// Échap efface le filtre.
	view = drive(m, key("esc"))
	if !strings.Contains(view, "autrefr") {
		t.Errorf("échap doit rétablir la liste complète :\n%s", view)
	}
}

func TestApplyRequiresConfirmation(t *testing.T) {
	// Toute action mutante passe par une confirmation : la TUI n'est qu'un pilote
	// du même moteur, sans chemin privilégié.
	backend := &fakeBackend{projects: []*model.Project{project(t, "shop.fr", naming.EnvProd)}}
	m := New(backend)
	_ = drive(m, m.Init()())
	view := drive(m, key("a"))
	if !strings.Contains(view, "appliquer les changements") {
		t.Errorf("une confirmation est attendue :\n%s", view)
	}
	if len(backend.applied) != 0 {
		t.Error("rien ne doit être appliqué avant confirmation")
	}
	// Une touche quelconque annule.
	view = drive(m, key("x"))
	if len(backend.applied) != 0 {
		t.Errorf("l'annulation doit être respectée : %v", backend.applied)
	}
	if m.Status() != "annulé" {
		t.Errorf("état après annulation : %q", m.Status())
	}
	// Confirmer applique et montre la sortie.
	view = drive(m, key("a"), key("o"))
	if len(backend.applied) != 1 || backend.applied[0] != "shopfr" {
		t.Errorf("application attendue : %v", backend.applied)
	}
	if !strings.Contains(view, "étape(s) appliquée(s)") {
		t.Errorf("la sortie du moteur doit être rendue :\n%s", view)
	}
}

func TestApplyErrorIsShown(t *testing.T) {
	backend := &fakeBackend{
		projects: []*model.Project{project(t, "shop.fr", naming.EnvProd)},
		applyErr: fmt.Errorf("étape system/user/shopfr : permission refusée"),
	}
	m := New(backend)
	_ = drive(m, m.Init()())
	view := drive(m, key("a"), key("o"))
	if !strings.Contains(view, "permission refusée") {
		t.Errorf("l'erreur doit être affichée :\n%s", view)
	}
}

func TestDiffScreen(t *testing.T) {
	backend := &fakeBackend{
		projects:  []*model.Project{project(t, "shop.fr", naming.EnvProd)},
		diffLines: []string{"~ répertoire logs : mode 0777 → 0750", "    --- attendu", "    +++ obtenu"},
	}
	m := New(backend)
	_ = drive(m, m.Init()())
	view := drive(m, key("d"))
	if !strings.Contains(view, "0777 → 0750") {
		t.Errorf("les écarts doivent être affichés :\n%s", view)
	}
	// Retour à la liste.
	view = drive(m, key("q"))
	if !strings.Contains(view, "naviguer") {
		t.Errorf("q doit revenir à la liste :\n%s", view)
	}
}

func TestRestartAndRenewAskConfirmation(t *testing.T) {
	backend := &fakeBackend{projects: []*model.Project{project(t, "shop.fr", naming.EnvProd)}}
	m := New(backend)
	_ = drive(m, m.Init()())
	drive(m, key("s"), key("o"))
	if len(backend.restarted) != 1 {
		t.Errorf("redémarrage attendu : %v", backend.restarted)
	}
	// L'écran de résultat se ferme avant l'action suivante.
	drive(m, key("q"), key("c"), key("o"))
	if len(backend.renewed) != 1 {
		t.Errorf("renouvellement attendu : %v", backend.renewed)
	}
}

func TestWizardCollectsAnswers(t *testing.T) {
	backend := &fakeBackend{}
	m := New(backend)
	_ = drive(m, m.Init()())
	msgs := []tea.Msg{key("n")}
	msgs = append(msgs, key("enter")) // environnement : prod
	msgs = append(msgs, typeText("shop.fr")...)
	msgs = append(msgs, key("enter"))         // host
	msgs = append(msgs, key("enter"))         // type : défaut php-symfony
	msgs = append(msgs, typeText("6.4")...)   //
	msgs = append(msgs, key("enter"))         // version applicative
	msgs = append(msgs, typeText("8.5")...)   //
	msgs = append(msgs, key("enter"))         // version PHP
	msgs = append(msgs, typeText("mysql")...) //
	msgs = append(msgs, key("enter"))         // base
	msgs = append(msgs, typeText("async")...) //
	msgs = append(msgs, key("enter"))         // transports
	msgs = append(msgs, typeText("2")...)     //
	msgs = append(msgs, key("enter"))         // instances
	view := drive(m, msgs...)

	if len(backend.created) != 1 {
		t.Fatalf("une création attendue : %+v", backend.created)
	}
	answers := backend.created[0]
	if answers.Host != "shop.fr" || answers.Kind != "php-symfony" || answers.PHPVersion != "8.5" {
		t.Errorf("réponses collectées : %+v", answers)
	}
	if answers.Database != "mysql" || answers.Messenger != "async" || answers.Instances != "2" {
		t.Errorf("réponses collectées : %+v", answers)
	}
	if !strings.Contains(view, "projet créé") {
		t.Errorf("le résultat doit être affiché :\n%s", view)
	}
}

// L'assistant annonce le host qui sera servi : une préproduction saisie avec un
// host déjà construit le construirait deux fois.
func TestWizardHostHelpAnnouncesPrepHost(t *testing.T) {
	backend := &fakeBackend{}
	m := New(backend)
	_ = drive(m, m.Init()())
	msgs := []tea.Msg{key("n")}
	msgs = append(msgs, typeText("prep")...)
	msgs = append(msgs, key("enter")) // environnement : prep
	view := drive(m, msgs...)
	if !strings.Contains(view, "shop.preprod-pp.net") {
		t.Errorf("l'aide doit montrer le host construit :\n%s", view)
	}
	if !strings.Contains(view, "host de production") {
		t.Errorf("l'aide doit dire quel host saisir :\n%s", view)
	}
}

func TestWizardRejectsUnknownChoice(t *testing.T) {
	backend := &fakeBackend{}
	m := New(backend)
	_ = drive(m, m.Init()())
	msgs := []tea.Msg{key("n")}
	msgs = append(msgs, key("enter")) // environnement : prod
	msgs = append(msgs, typeText("shop.fr")...)
	msgs = append(msgs, key("enter"))
	msgs = append(msgs, typeText("php-drupal")...)
	msgs = append(msgs, key("enter"))
	view := drive(m, msgs...)
	if !strings.Contains(view, "valeur attendue") {
		t.Errorf("un choix hors liste doit être refusé :\n%s", view)
	}
	if len(backend.created) != 0 {
		t.Error("aucune création ne doit avoir lieu")
	}
}

func TestWizardRequiresHost(t *testing.T) {
	backend := &fakeBackend{}
	m := New(backend)
	_ = drive(m, m.Init()())
	view := drive(m, key("n"), key("enter"), key("enter"))
	if !strings.Contains(view, "obligatoire") {
		t.Errorf("le host est obligatoire :\n%s", view)
	}
}

func TestWizardEscapeAbandonsWithoutWriting(t *testing.T) {
	backend := &fakeBackend{}
	m := New(backend)
	_ = drive(m, m.Init()())
	msgs := []tea.Msg{key("n")}
	msgs = append(msgs, key("enter")) // environnement : prod
	msgs = append(msgs, typeText("shop.fr")...)
	msgs = append(msgs, key("esc"))
	drive(m, msgs...)
	if len(backend.created) != 0 {
		t.Error("abandonner l'assistant ne doit rien écrire")
	}
	if m.Status() != "création abandonnée" {
		t.Errorf("état : %q", m.Status())
	}
}

func TestEmptyParcInvitesCreation(t *testing.T) {
	m := New(&fakeBackend{})
	view := drive(m, m.Init()())
	if !strings.Contains(view, "aucun projet déclaré") {
		t.Errorf("le parc vide doit être annoncé :\n%s", view)
	}
	if !strings.Contains(view, "« n »") {
		t.Errorf("l'action à faire doit être indiquée :\n%s", view)
	}
}

func TestQuit(t *testing.T) {
	m := New(&fakeBackend{projects: []*model.Project{project(t, "shop.fr", naming.EnvProd)}})
	_ = drive(m, m.Init()())
	drive(m, key("q"))
	if !m.Quitting() {
		t.Error("q doit demander la sortie")
	}
	if m.View() != "" {
		t.Error("la vue doit être vide après la sortie, pour ne pas laisser l'écran sali")
	}
}

func TestPlainStylesWithoutColour(t *testing.T) {
	// Un terminal sans couleur ne doit pas recevoir de séquences d'échappement.
	t.Setenv("NO_COLOR", "1")
	styles := NewStyles()
	if !styles.Plain {
		t.Fatal("NO_COLOR doit désactiver la couleur")
	}
	rendered := styles.health("dérive")
	if strings.Contains(rendered, "\x1b[") {
		t.Errorf("séquence d'échappement présente : %q", rendered)
	}
	// L'information reste portée par le symbole.
	if !strings.HasPrefix(rendered, "⚠") {
		t.Errorf("le symbole doit rester : %q", rendered)
	}
}

func TestHealthSymbolsDistinguishStates(t *testing.T) {
	styles := Styles{Plain: true}
	cases := map[string]string{"sain": "●", "dérive": "⚠", "erreur": "✕"}
	for state, symbol := range cases {
		if got := styles.health(state); !strings.HasPrefix(got, symbol) {
			t.Errorf("état %q : %q, symbole %q attendu", state, got, symbol)
		}
	}
}
