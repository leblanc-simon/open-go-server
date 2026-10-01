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
	if err := proj.Normalize(filepath.Join(prefix, "home"), "preprod-pp.net",
		model.WithLogsRoot(filepath.Join(prefix, "var", "log", "ogs-projects"))); err != nil {
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
	if !strings.Contains(text, filepath.Join(".local", "bin", "php")) {
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

// La ligne qu'un exploitant ajoute au fichier cron le fait refuser à l'écriture ;
// elle ne doit pas partir, sans --force ni copie, au retrait de la dernière
// tâche déclarée.
func TestRetraitDUnFichierCronRetoucheExigeForceEtSauvegarde(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Crons = []model.CronJob{{Name: "purge", Schedule: "0 3 * * *", Command: "bin/console app:purge"}}
	ctx := context.Background()
	applique(t, prov, proj)
	path := prov.Path(proj.ID())
	data, _ := os.ReadFile(path)
	retouche := append(data, []byte("0 4 * * * shopfr /home/shop.fr/www/bin/backup.sh\n")...)
	if err := os.WriteFile(path, retouche, 0o644); err != nil {
		t.Fatal(err)
	}

	proj.Spec.Crons = nil
	steps, err := prov.Plan(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || !strings.Contains(steps[0].Summary, "modifié hors ogs") {
		t.Fatalf("le retrait doit être refusé : %+v", steps)
	}
	if err := steps[0].Apply(ctx); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("le refus doit nommer --force : %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(retouche) {
		t.Fatal("le fichier retouché a été retiré sans --force")
	}
	if got := sauvegardes(t, prov, path); len(got) != 0 {
		t.Fatalf("aucune sauvegarde ne doit être créée par un refus : %v", got)
	}

	prov.Force = true
	steps, err = prov.Plan(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || !strings.Contains(steps[0].Summary, "retrait forcé") {
		t.Fatalf("le plan doit annoncer le retrait forcé : %+v", steps)
	}
	if err := steps[0].Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("le fichier devait être retiré sous --force : %v", err)
	}
	baks := sauvegardes(t, prov, path)
	if len(baks) != 1 {
		t.Fatalf("une sauvegarde attendue, %d trouvée(s)", len(baks))
	}
	if saved, _ := os.ReadFile(baks[0]); string(saved) != string(retouche) {
		t.Errorf("la sauvegarde doit porter la ligne de l'exploitant :\n%s", saved)
	}
}

// Un fichier posé sous le nom réservé sans la marque d'ogs n'est pas à lui : il
// reste en place, --force compris, et l'étape le dit.
func TestFichierCronEtrangerJamaisRetire(t *testing.T) {
	prov, proj := fixture(t)
	path := prov.Path(proj.ID())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const corps = "* * * * * root /usr/local/bin/sauvegarde\n"
	if err := os.WriteFile(path, []byte(corps), 0o644); err != nil {
		t.Fatal(err)
	}
	prov.Force = true
	steps, err := prov.Plan(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Critical || !strings.Contains(steps[0].Summary, "laissé en place") {
		t.Fatalf("un avertissement non critique est attendu : %+v", steps)
	}
	if err := steps[0].Apply(context.Background()); err == nil {
		t.Error("l'étape doit dire que le fichier est laissé en place")
	}
	if got, _ := os.ReadFile(path); string(got) != corps {
		t.Error("un fichier cron étranger a été retiré")
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

func TestBuildEchappeLePourcent(t *testing.T) {
	// Un « % » non échappé coupe la ligne de cron : le reste part sur l'entrée
	// standard, la redirection vers le journal comprise, et la tâche échoue sans
	// laisser de trace.
	prov, proj := fixture(t)
	proj.Spec.Crons = []model.CronJob{{
		Name: "rapport", Schedule: "0 3 * * *",
		Command: "bin/console app:rapport --jour=$(date +%F)",
	}}
	content, jobs, err := prov.Build(proj)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if strings.Contains(text, "+%F") || !strings.Contains(text, `+\%F`) {
		t.Errorf("le pourcent doit être échappé :\n%s", text)
	}
	// La redirection doit rester sur la même ligne que la commande.
	if !strings.Contains(text, `+\%F) >> `) {
		t.Errorf("la redirection doit suivre la commande :\n%s", text)
	}
	// La tâche rendue reste la chaîne vraie : « ogs cron run » la passe à /bin/sh,
	// qui ne défait pas la contre-oblique entre apostrophes.
	if !strings.Contains(jobs[0].Command, "+%F") || strings.Contains(jobs[0].Command, `\%`) {
		t.Errorf("la commande exécutable ne doit pas être échappée : %q", jobs[0].Command)
	}
}

func TestBuildRespecteUnPourcentDejaEchappe(t *testing.T) {
	// L'idiome connu « date +\%F » est déjà correct : le redoubler écrirait « \\% »,
	// que cron rendrait comme une contre-oblique suivie d'un saut de ligne.
	prov, proj := fixture(t)
	proj.Spec.Crons = []model.CronJob{{
		Name: "sauvegarde", Schedule: "0 4 * * *",
		Command: `bin/console app:dump --nom=sauvegarde-$(date +\%F).sql`,
	}}
	content, _, err := prov.Build(proj)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if strings.Contains(text, `\\%`) {
		t.Errorf("le pourcent déjà échappé doit être laissé tel quel :\n%s", text)
	}
	if !strings.Contains(text, `+\%F`) {
		t.Errorf("échappement perdu :\n%s", text)
	}
}

func TestPlanResteIdempotentAvecUnPourcent(t *testing.T) {
	// L'échappement est déterministe : l'empreinte du corps ne doit pas varier
	// d'un plan à l'autre, sinon le fichier resterait éternellement « en écart ».
	prov, proj := fixture(t)
	proj.Spec.Crons = []model.CronJob{{
		Name: "rapport", Schedule: "0 3 * * *",
		Command: "bin/console app:rapport --jour=$(date +%F)",
	}}
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

// applique exécute le plan du provider et rend les étapes calculées.
func applique(t *testing.T, prov *Provider, proj *model.Project) {
	t.Helper()
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
}

// sauvegardes énumère les copies d'un fichier déposées dans le répertoire de
// sauvegardes du parc, et refuse qu'il en reste une à côté de l'original :
// /etc/cron.d est relu en entier — par cron, qui ignore les noms ponctués, mais
// aussi par ogs lorsqu'il inventorie le parc.
func sauvegardes(t *testing.T, prov *Provider, path string) []string {
	t.Helper()
	restes, err := filepath.Glob(path + ".ogs-bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(restes) != 0 {
		t.Errorf("une sauvegarde ne doit jamais rester dans %s : %v", filepath.Dir(path), restes)
	}
	matches, err := filepath.Glob(filepath.Join(prov.Paths.BackupsDir(), filepath.Base(path)+".ogs-bak-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func TestPlanEcraseAvecForceEtSauvegarde(t *testing.T) {
	prov, proj := fixture(t)
	proj.Spec.Crons = []model.CronJob{{Name: "x", Schedule: "* * * * *", Command: "bin/console x"}}
	ctx := context.Background()
	applique(t, prov, proj)

	path := prov.Path(proj.ID())
	data, _ := os.ReadFile(path)
	const ajout = "\n* * * * * root /bin/evil\n"
	if err := os.WriteFile(path, append(data, []byte(ajout)...), 0o644); err != nil {
		t.Fatal(err)
	}

	// Sans --force, le refus tient et rien n'est écrit.
	steps, err := prov.Plan(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if strings.Contains(s.Summary, "modifié hors ogs") {
			if err := s.Apply(ctx); err == nil {
				t.Error("l'écrasement doit être refusé sans --force")
			}
		}
	}
	if got := sauvegardes(t, prov, path); len(got) != 0 {
		t.Fatalf("aucune sauvegarde ne doit être créée par un refus : %v", got)
	}

	// Avec --force, l'étape redevient une mise à jour ordinaire, et la
	// sauvegarde est la seule trace qui reste de la version manuelle.
	prov.Force = true
	steps, err = prov.Plan(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	var forcée *engine.Step
	for _, s := range steps {
		if strings.Contains(s.Summary, "écrasement forcé") {
			forcée = s
		}
	}
	if forcée == nil {
		t.Fatalf("le plan doit annoncer l'écrasement forcé")
	}
	if err := forcée.Apply(ctx); err != nil {
		t.Fatalf("l'écrasement forcé doit réussir : %v", err)
	}
	after, _ := os.ReadFile(path)
	if strings.Contains(string(after), "/bin/evil") {
		t.Error("le fichier doit avoir été réécrit")
	}
	baks := sauvegardes(t, prov, path)
	if len(baks) != 1 {
		t.Fatalf("une sauvegarde attendue, %d trouvée(s)", len(baks))
	}
	saved, _ := os.ReadFile(baks[0])
	if !strings.Contains(string(saved), "/bin/evil") {
		t.Errorf("la sauvegarde doit contenir l'original :\n%s", saved)
	}

	// Le plan suivant est de nouveau conforme : la sauvegarde ne doit pas être
	// prise pour un fichier cron par ogs ni par cron lui-même (un point dans le
	// nom la fait ignorer).
	prov.Force = false
	steps, err = prov.Plan(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Action != engine.NoOp {
			t.Errorf("étape inattendue après l'écrasement forcé : %s", s.Summary)
		}
	}
}

func TestPlanRefuseUnFichierEtranger(t *testing.T) {
	// Un fichier posé par un autre outil ne porte pas la marque d'ogs : le
	// message doit le dire, et non parler d'une modification.
	prov, proj := fixture(t)
	proj.Spec.Crons = []model.CronJob{{Name: "x", Schedule: "* * * * *", Command: "bin/console x"}}
	path := prov.Path(proj.ID())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("* * * * * root /bin/true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	steps, err := prov.Plan(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	var refus *engine.Step
	for _, s := range steps {
		if strings.Contains(s.Summary, "non géré par ogs") {
			refus = s
		}
	}
	if refus == nil {
		t.Fatalf("un fichier étranger doit être signalé comme tel")
	}
	err = refus.Apply(ctx)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("le refus doit nommer la porte de sortie : %v", err)
	}
	// Le refus dit où l'original partira : le répertoire commun, jamais /etc/cron.d.
	if err == nil || !strings.Contains(err.Error(), prov.Paths.BackupsDir()) {
		t.Errorf("le refus doit dire où l'original sera sauvegardé : %v", err)
	}
}

func TestCommandSePlaceDansLeRepertoireDeTravail(t *testing.T) {
	// Le répertoire de travail n'est pas qu'un préfixe de commande : une tâche
	// qui lit un chemin relatif doit le trouver là, non dans le home où cron la
	// lance.
	prov, proj := fixture(t)
	proj.Spec.Crons = []model.CronJob{{
		Name: "export", Schedule: "0 4 * * *", Command: "/usr/local/bin/export.sh",
	}}
	_, jobs, err := prov.Build(proj)
	if err != nil {
		t.Fatal(err)
	}
	www := filepath.Join(proj.Spec.Layout.Root, "www")
	want := "cd '" + www + "' && /usr/bin/flock -n "
	if !strings.HasPrefix(jobs[0].Command, want) {
		t.Errorf("la tâche doit commencer par se placer dans www/ :\n%s", jobs[0].Command)
	}

	// Un répertoire explicite est cité : une espace ou une apostrophe ne doit
	// pas découper la ligne.
	proj.Spec.Crons[0].Workdir = "/srv/l'export du jour"
	_, jobs, err = prov.Build(proj)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(jobs[0].Command, `cd '/srv/l'\''export du jour' && `) {
		t.Errorf("répertoire de travail mal cité :\n%s", jobs[0].Command)
	}
}

func TestCommandSansVerrouSePlaceAussiDansLeRepertoire(t *testing.T) {
	prov, proj := fixture(t)
	lock := false
	proj.Spec.Crons = []model.CronJob{{
		Name: "scheduler", Schedule: "* * * * *", Command: "bin/console schedule:run", Lock: &lock,
	}}
	_, jobs, err := prov.Build(proj)
	if err != nil {
		t.Fatal(err)
	}
	www := filepath.Join(proj.Spec.Layout.Root, "www")
	want := "cd '" + www + "' && " + filepath.Join(model.BinDir(proj.Spec.Layout.Root), "php") +
		" " + filepath.Join(www, "bin", "console") + " schedule:run"
	if jobs[0].Command != want {
		t.Errorf("commande :\n%s\nattendu :\n%s", jobs[0].Command, want)
	}
}

func TestPHPFixeLaVersionDeLaTache(t *testing.T) {
	prov, proj := fixture(t)
	bin := prov.Paths.PHPBin("8.2")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	proj.Spec.Crons = []model.CronJob{
		{Name: "console", Schedule: "* * * * *", Command: "bin/console app:sync", PHP: "8.2"},
		{Name: "script", Schedule: "* * * * *", Command: "php scripts/purge.php --vieux", PHP: "8.2"},
		{Name: "lien", Schedule: "* * * * *", Command: "php scripts/purge.php"},
	}
	_, jobs, err := prov.Build(proj)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jobs[0].Command, bin+" "+filepath.Join(proj.Spec.Layout.Root, "www", "bin", "console")+" app:sync") {
		t.Errorf("bin/console doit passer par PHP 8.2 :\n%s", jobs[0].Command)
	}
	if !strings.HasSuffix(jobs[1].Command, bin+" scripts/purge.php --vieux") {
		t.Errorf("« php … » doit passer par PHP 8.2 :\n%s", jobs[1].Command)
	}
	// Sans « php: », « php … » passe par le lien géré (D30), au lieu d'être
	// pris pour un exécutable de www/.
	link := filepath.Join(model.BinDir(proj.Spec.Layout.Root), "php")
	if !strings.HasSuffix(jobs[2].Command, link+" scripts/purge.php") {
		t.Errorf("« php … » doit passer par le lien géré :\n%s", jobs[2].Command)
	}
}

func TestPHPRefuseCeQuIlNePeutAppliquer(t *testing.T) {
	prov, proj := fixture(t)
	cas := map[string]model.CronJob{
		"commande non PHP":  {Name: "x", Schedule: "* * * * *", Command: "/usr/local/bin/x.sh", PHP: "8.2"},
		"version invalide":  {Name: "x", Schedule: "* * * * *", Command: "bin/console a", PHP: "8"},
		"version non gérée": {Name: "x", Schedule: "* * * * *", Command: "bin/console a", PHP: "9.0"},
		"version absente":   {Name: "x", Schedule: "* * * * *", Command: "bin/console a", PHP: "8.3"},
	}
	for nom, job := range cas {
		proj.Spec.Crons = []model.CronJob{job}
		if _, _, err := prov.Build(proj); err == nil {
			t.Errorf("%s : refus attendu", nom)
		}
	}
	// En mode simulé, aucun binaire n'est posé : la version n'est pas cherchée.
	prov.Simulated = true
	proj.Spec.Crons = []model.CronJob{cas["version absente"]}
	if _, _, err := prov.Build(proj); err != nil {
		t.Errorf("mode simulé : %v", err)
	}
}
