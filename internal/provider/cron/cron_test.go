package cron

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/template"
)

func fixture(t *testing.T) (*Provider, *model.Project) {
	t.Helper()
	prefix := t.TempDir()
	paths := config.NewPaths(prefix)
	proj := model.New("shop.fr", naming.EnvProd, model.KindSymfony)
	proj.Spec.App.Version = "6.4"
	if err := proj.Normalize(filepath.Join(prefix, "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(proj.Spec.Layout.LogDir, 0o750); err != nil {
		t.Fatal(err)
	}
	prov := &Provider{
		Paths:     paths,
		Templates: template.New(paths.TemplatesDir()),
		Now:       func() time.Time { return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) },
	}
	return prov, proj
}

func TestBuildUsesLockAndInterpreterLink(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Crons = []model.CronJob{{
		Name: "scheduler", Schedule: "* * * * *", Command: "bin/console schedule:run",
	}}
	content, jobs, err := prov.Build(proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("%d tâches", len(jobs))
	}
	text := string(content)
	// Le verrou est posé par défaut : une tâche qui se recouvre est une cause
	// fréquente d'incident.
	if !strings.Contains(text, "/usr/bin/flock -n") {
		t.Errorf("verrou absent :\n%s", text)
	}
	// L'interpréteur passe par le lien géré : une bascule de version PHP ne doit
	// pas exiger de réécrire les crons.
	if !strings.Contains(text, filepath.Join(".bin", "php")) {
		t.Errorf("le lien d'interpréteur doit être employé :\n%s", text)
	}
	if strings.Contains(text, "/usr/bin/php8") {
		t.Error("aucune version de PHP ne doit apparaître en dur")
	}
	// La sortie est journalisée, sans courriel.
	if !strings.Contains(text, `MAILTO=""`) {
		t.Error("MAILTO doit être vide : les sorties vont dans les journaux")
	}
	if !strings.Contains(text, "cron-scheduler.log 2>&1") {
		t.Errorf("redirection absente :\n%s", text)
	}
	// La tâche s'exécute sous l'utilisateur du projet.
	if !strings.Contains(text, "* * * * * shopfr ") {
		t.Errorf("identité d'exécution :\n%s", text)
	}
}

func TestBuildDisabledJobIsCommentedNotRemoved(t *testing.T) {
	// Désactiver conserve la trace : la tâche reste lisible et réactivable.
	prov, proj := fixture(t)
	disabled := false
	proj.Spec.Crons = []model.CronJob{{
		Name: "nettoyage", Schedule: "0 3 * * *", Command: "bin/console app:clean",
		Enabled: &disabled,
	}}
	content, _, err := prov.Build(proj)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if !strings.Contains(text, "# nettoyage (désactivée)") {
		t.Errorf("la désactivation doit être annoncée :\n%s", text)
	}
	if !strings.Contains(text, "# 0 3 * * * shopfr") {
		t.Errorf("la ligne doit être commentée, pas supprimée :\n%s", text)
	}
}

func TestBuildWithoutLock(t *testing.T) {
	prov, proj := fixture(t)
	noLock := false
	proj.Spec.Crons = []model.CronJob{{
		Name: "ping", Schedule: "*/5 * * * *", Command: "/usr/bin/curl -s https://exemple.fr",
		Lock: &noLock,
	}}
	content, _, err := prov.Build(proj)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "flock") {
		t.Errorf("le verrou doit pouvoir être désactivé :\n%s", content)
	}
}

func TestApplyCreatesFileAndLogs(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Crons = []model.CronJob{
		{Name: "scheduler", Schedule: "* * * * *", Command: "bin/console schedule:run"},
		{Name: "nettoyage", Schedule: "0 3 * * *", Command: "bin/console app:clean"},
	}
	ctx := context.Background()
	steps, err := prov.Plan(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	plan := &engine.Plan{Project: proj.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	// Le fichier cron doit être en 0644 : cron refuse un fichier de /etc/cron.d
	// inscriptible par un autre que root.
	info, err := os.Stat(prov.Path(proj.ID()))
	if err != nil {
		t.Fatalf("fichier cron absent : %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode %04o, attendu 0644", info.Mode().Perm())
	}
	// Les journaux sont pré-créés : le répertoire logs/ appartenant à root, la
	// tâche ne pourrait pas les créer elle-même.
	for _, name := range []string{"cron-scheduler.log", "cron-nettoyage.log"} {
		path := filepath.Join(proj.Spec.Layout.LogDir, name)
		if info, err := os.Stat(path); err != nil {
			t.Errorf("journal %s absent : %v", name, err)
		} else if info.Mode().Perm() != 0o640 {
			t.Errorf("%s : mode %04o, attendu 0640", name, info.Mode().Perm())
		}
	}

	// Idempotence.
	steps, err = prov.Plan(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	plan2 := &engine.Plan{Project: proj.ID()}
	for _, s := range steps {
		plan2.Add(s)
	}
	if !plan2.Empty() {
		t.Errorf("second plan non vide :\n%s", plan2.String())
	}
}

func TestRemovingAllJobsDeletesFile(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Crons = []model.CronJob{{Name: "x", Schedule: "* * * * *", Command: "bin/console x"}}
	ctx := context.Background()
	steps, _ := prov.Plan(ctx, proj)
	plan := &engine.Plan{Project: proj.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	proj.Spec.Crons = nil
	steps, err := prov.Plan(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Action != engine.Delete {
		t.Fatalf("suppression attendue : %+v", steps)
	}
	if err := steps[0].Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prov.Path(proj.ID())); !os.IsNotExist(err) {
		t.Error("le fichier devrait avoir été supprimé")
	}
}

func TestPlanDetectsHandEditedFile(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Crons = []model.CronJob{{Name: "x", Schedule: "* * * * *", Command: "bin/console x"}}
	ctx := context.Background()
	steps, _ := prov.Plan(ctx, proj)
	plan := &engine.Plan{Project: proj.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	path := prov.Path(proj.ID())
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(data, []byte("\n* * * * * root /bin/evil\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	var found *engine.Step
	for _, s := range steps {
		if strings.Contains(s.Summary, "modifié hors ogs") {
			found = s
		}
	}
	if found == nil {
		t.Fatal("la modification manuelle doit être détectée")
	}
	if err := found.Apply(ctx); err == nil {
		t.Error("l'écrasement doit être refusé sans --force")
	}
}

func TestNextOccurrences(t *testing.T) {
	from := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		expr  string
		first string
	}{
		{"* * * * *", "2026-08-23 12:01"},
		{"*/15 * * * *", "2026-08-23 12:15"},
		{"0 3 * * *", "2026-08-24 03:00"},
		{"30 2 1 * *", "2026-09-01 02:30"},
		// Dimanche s'écrit 0 ou 7 : les deux doivent fonctionner.
		{"0 4 * * 0", "2026-08-30 04:00"},
		{"0 4 * * 7", "2026-08-30 04:00"},
	}
	for _, c := range cases {
		next, err := NextOccurrences(c.expr, from, 3)
		if err != nil {
			t.Errorf("%q : %v", c.expr, err)
			continue
		}
		if len(next) != 3 {
			t.Errorf("%q : %d occurrences", c.expr, len(next))
			continue
		}
		if got := next[0].Format("2006-01-02 15:04"); got != c.first {
			t.Errorf("%q : première occurrence %s, attendue %s", c.expr, got, c.first)
		}
	}
}

func TestNextOccurrencesRejectsImpossible(t *testing.T) {
	from := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	// 31 février n'existe pas : l'expression est syntaxiquement correcte mais ne
	// se déclenchera jamais, ce qui mérite d'être signalé.
	if _, err := NextOccurrences("0 0 31 2 *", from, 1); err == nil {
		t.Error("une expression sans occurrence doit être signalée")
	}
	if _, err := NextOccurrences("* * * *", from, 1); err == nil {
		t.Error("une expression à quatre champs doit être refusée")
	}
}

func TestCommandRejectsNewline(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Crons = []model.CronJob{{
		Name: "injection", Schedule: "* * * * *",
		Command: "bin/console x\n* * * * * root /bin/evil",
	}}
	if _, _, err := prov.Build(proj); err == nil {
		t.Error("une commande contenant un retour à la ligne doit être refusée")
	}
}

func TestCommandResolvesAgainstWWW(t *testing.T) {
	// Le code applicatif vit dans www/ : résoudre depuis la racine du projet
	// produirait un chemin inexistant, et la tâche échouerait silencieusement.
	prov, proj := fixture(t)
	proj.Spec.Crons = []model.CronJob{{
		Name: "scheduler", Schedule: "* * * * *", Command: "bin/console schedule:run",
	}}
	content, _, err := prov.Build(proj)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(proj.Spec.Layout.Root, "www", "bin", "console")
	if !strings.Contains(string(content), want) {
		t.Errorf("la commande doit être résolue dans www/ :\n%s", content)
	}
	// Un répertoire de travail explicite est respecté.
	proj.Spec.Crons[0].Workdir = filepath.Join(proj.Spec.Layout.Root, "www", "sous-dossier")
	content, _, err = prov.Build(proj)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), filepath.Join("www", "sous-dossier", "bin", "console")) {
		t.Errorf("le répertoire de travail explicite doit primer :\n%s", content)
	}
}

func TestAbsoluteCommandIsLeftAlone(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Crons = []model.CronJob{{
		Name: "sauvegarde", Schedule: "0 2 * * *", Command: "/usr/local/bin/backup.sh --quiet",
	}}
	content, _, err := prov.Build(proj)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "/usr/local/bin/backup.sh --quiet") {
		t.Errorf("une commande absolue ne doit pas être réécrite :\n%s", content)
	}
}
