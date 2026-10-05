package php

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// fpmScénarisé est un systemd et un FPM factices, non simulés : « is-active »
// répond selon le rang de l'appel, et une commande peut recevoir un effet de
// bord — c'est le dialogue entier avec systemd qu'il faut éprouver (D105).
type fpmScénarisé struct {
	*exec.Recorder
	// actif rend l'état de l'unité au n-ième appel à « is-active » (n ≥ 1).
	actif func(n int) bool
	// activé rend l'état de « is-enabled ».
	activé   bool
	restarts []string
	effets   map[string]func() error
	n, r     int
}

func (f *fpmScénarisé) Simulated() bool { return false }

func (f *fpmScénarisé) Run(ctx context.Context, cmd exec.Command) (exec.Result, error) {
	res, err := f.Recorder.Run(ctx, cmd)
	key := cmd.Name
	if len(cmd.Args) > 0 {
		key += " " + cmd.Args[0]
	}
	if effet, ok := f.effets[key]; ok {
		if e := effet(); e != nil {
			return exec.Result{ExitCode: 1}, e
		}
	}
	if cmd.Name == "systemctl" && len(cmd.Args) > 0 {
		switch cmd.Args[0] {
		case "is-active":
			f.n++
			if f.actif != nil && !f.actif(f.n) {
				return exec.Result{ExitCode: 3}, errors.New("inactive")
			}
			return exec.Result{}, nil
		case "is-enabled":
			if !f.activé {
				return exec.Result{ExitCode: 1}, errors.New("disabled")
			}
			return exec.Result{}, nil
		case "show":
			if f.r < len(f.restarts) {
				f.r++
				return exec.Result{Stdout: f.restarts[f.r-1] + "\n"}, nil
			}
			return exec.Result{Stdout: "0\n"}, nil
		}
	}
	return res, err
}

// fenêtreCourte réduit la fenêtre du constat après rechargement.
func fenêtreCourte(t *testing.T) {
	t.Helper()
	avant := délaiConstat
	délaiConstat = 20 * time.Millisecond
	t.Cleanup(func() { délaiConstat = avant })
}

func fpmAvecPool(t *testing.T, f *fpmScénarisé) *Provider {
	t.Helper()
	prov, paths, _ := fixture(t, "8.5")
	if f.Recorder == nil {
		f.Recorder = exec.NewRecorder()
	}
	prov.Exec = f
	if err := fs.WriteAtomic(paths.PHPPoolFile("8.5", "shopfr"), []byte("[shopfr]\n"), 0o644, fs.KeepOwner); err != nil {
		t.Fatal(err)
	}
	return prov
}

// R3N-12 : retirer un pool ne passe par aucune écriture, donc par aucun « -t ».
// Le rechargement qui suivait relisait toute la configuration — un pool de
// l'administrateur dont le compte a disparu — et le maître mourait pour toute la
// version, ogs annonçant un succès. Refusé, le maître garde ce qu'il sert.
func TestReloadRefuseUneConfigurationQueFPMRejette(t *testing.T) {
	fenêtreCourte(t)
	f := &fpmScénarisé{Recorder: exec.NewRecorder(), activé: true}
	prov := fpmAvecPool(t, f)
	f.Errors[prov.Paths.PHPFpmBin("8.5")+" -t"] = errors.New("[pool legacy] cannot get uid for user 'legacy'")
	err := prov.Reload(context.Background(), "8.5")
	if err == nil || !strings.Contains(err.Error(), "rechargement refusé") ||
		!strings.Contains(err.Error(), "continue de servir") {
		t.Fatalf("le rechargement doit être refusé et le dire : %v", err)
	}
	for _, c := range systemctlAppelés(f.Recorder) {
		if strings.HasPrefix(c, "reload") || strings.HasPrefix(c, "restart") || strings.HasPrefix(c, "stop") {
			t.Errorf("aucun signal ne doit atteindre un maître dont la configuration est refusée : %v",
				systemctlAppelés(f.Recorder))
		}
	}
}

// R3N-12 : « systemctl reload » rend 0 dès l'envoi de USR2. Le maître qui meurt
// ensuite doit être constaté, relevé, et le fait signalé.
func TestReloadRelèveUnFPMQueLeRechargementATué(t *testing.T) {
	fenêtreCourte(t)
	f := &fpmScénarisé{Recorder: exec.NewRecorder(), activé: true,
		actif: func(n int) bool { return n == 1 }}
	prov := fpmAvecPool(t, f)
	err := prov.Reload(context.Background(), "8.5")
	if err == nil || !strings.Contains(err.Error(), "le rechargement a arrêté php8.5-fpm") {
		t.Fatalf("la mort du maître doit être signalée : %v", err)
	}
	joined := strings.Join(systemctlAppelés(f.Recorder), " | ")
	if !strings.Contains(joined, "reset-failed php8.5-fpm | start php8.5-fpm") {
		t.Errorf("le maître doit être relevé, compteur d'échecs acquitté : %s", joined)
	}
}

// Un maître relancé par systemd pendant la fenêtre est mort lui aussi : les
// requêtes en cours ont été coupées.
func TestReloadSignaleUnFPMRelancéParSystemd(t *testing.T) {
	fenêtreCourte(t)
	f := &fpmScénarisé{Recorder: exec.NewRecorder(), activé: true, restarts: []string{"0", "1"}}
	prov := fpmAvecPool(t, f)
	err := prov.Reload(context.Background(), "8.5")
	if err == nil || !strings.Contains(err.Error(), "que systemd a relancé") {
		t.Fatalf("un redémarrage automatique doit être signalé : %v", err)
	}
}

func TestReloadSainRendNil(t *testing.T) {
	fenêtreCourte(t)
	f := &fpmScénarisé{Recorder: exec.NewRecorder(), activé: true}
	prov := fpmAvecPool(t, f)
	if err := prov.Reload(context.Background(), "8.5"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(systemctlAppelés(f.Recorder), " | ")
	if !strings.Contains(joined, "reload php8.5-fpm") || strings.Contains(joined, "| enable") {
		t.Errorf("un FPM sain et activé se recharge seulement : %s", joined)
	}
}

// R1-49 : une unité qui tourne sans être activée ne repartirait pas au
// redémarrage de la machine.
func TestReloadActiveUnFPMQuiTourneSansÊtreActivé(t *testing.T) {
	fenêtreCourte(t)
	f := &fpmScénarisé{Recorder: exec.NewRecorder(), activé: false}
	prov := fpmAvecPool(t, f)
	if err := prov.Reload(context.Background(), "8.5"); err != nil {
		t.Fatal(err)
	}
	calls := systemctlAppelés(f.Recorder)
	if calls[len(calls)-1] != "enable php8.5-fpm" {
		t.Errorf("l'unité doit être activée, sans redémarrage : %v", calls)
	}
}

// Un FPM arrêté dont la configuration est refusée : la cause est nommée, et
// rien n'est tenté.
func TestReloadNommeLaCauseDUnDémarrageImpossible(t *testing.T) {
	f := &fpmScénarisé{Recorder: exec.NewRecorder(), actif: func(int) bool { return false }}
	prov := fpmAvecPool(t, f)
	f.Errors[prov.Paths.PHPFpmBin("8.5")+" -t"] = errors.New("code 78")
	err := prov.Reload(context.Background(), "8.5")
	if err == nil || !strings.Contains(err.Error(), "démarrage de php8.5-fpm refusé") {
		t.Fatalf("la validation doit nommer la cause : %v", err)
	}
	if strings.Contains(strings.Join(systemctlAppelés(f.Recorder), " | "), "enable --now") {
		t.Errorf("rien ne doit être démarré : %v", systemctlAppelés(f.Recorder))
	}
}

// R1-49 : un pool conforme ne produisait aucune étape, donc aucune intention :
// un FPM mort — tué par la rotation de la nuit, par exemple — n'était jamais
// relevé tant que la déclaration ne changeait pas.
func TestPlanRelèveUneUnitéFPMArrêtée(t *testing.T) {
	prov, _, _ := fixture(t, "8.5")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	ctx := context.Background()
	appliquer := func() *engine.Plan {
		t.Helper()
		obs, err := prov.Observe(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		steps, err := prov.Plan(ctx, p, obs)
		if err != nil {
			t.Fatal(err)
		}
		plan := &engine.Plan{Project: p.ID()}
		for _, s := range steps {
			plan.Add(s)
		}
		return plan
	}
	if _, err := engine.Execute(ctx, appliquer(), engine.Options{}); err != nil {
		t.Fatal(err)
	}
	// Pool écrit, FPM sain : rien à faire — le plan ne doit pas osciller.
	f := &fpmScénarisé{Recorder: exec.NewRecorder(), activé: true}
	prov.Exec = f
	if plan := appliquer(); !plan.Empty() {
		t.Fatalf("un FPM sain ne doit produire aucune étape :\n%s", plan.String())
	}
	// FPM mort : une étape non bloquante porte l'intention de le relever.
	f.actif = func(int) bool { return false }
	plan := appliquer()
	var unité *engine.Step
	for _, s := range plan.Mutating() {
		if s.Resource == "php/unit/8.5" {
			unité = s
		}
	}
	if unité == nil {
		t.Fatalf("une unité arrêtée doit être relevée :\n%s", plan.String())
	}
	if unité.Critical || len(unité.Reload) != 1 || unité.Reload[0] != "php:8.5" {
		t.Errorf("étape non bloquante portant php:8.5 attendue : %+v", unité)
	}
}

// Le motif voisin du lot X : la remise en place qui suit un refus de « -t »
// était jetée. Un pool refusé qui reste en place doit se dire.
func TestPoolRefuséDontLaRemiseEnPlaceÉchoue(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root passe outre les droits du répertoire")
	}
	prov, paths, _ := fixture(t, "8.5")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	dir := paths.PHPPoolDir("8.5")
	f := &fpmScénarisé{Recorder: exec.NewRecorder(), activé: true}
	f.effets = map[string]func() error{
		prov.Paths.PHPFpmBin("8.5") + " -t": func() error {
			if err := os.Chmod(dir, 0o555); err != nil {
				return err
			}
			return errors.New("code 78")
		},
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	prov.Exec = f
	ctx := context.Background()
	obs, err := prov.Observe(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	var pool *engine.Step
	for _, s := range steps {
		if strings.HasPrefix(s.Resource, "php/pool/") && s.Mutates() {
			pool = s
		}
	}
	if pool == nil {
		t.Fatal("étape de pool attendue")
	}
	err = pool.Apply(ctx)
	if err == nil || !strings.Contains(err.Error(), "remise en place") {
		t.Fatalf("l'échec de la remise en place doit être dit : %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "shopfr.conf")); statErr != nil {
		t.Fatalf("le pool refusé est resté, c'est ce que l'erreur doit dire : %v", statErr)
	}
}
