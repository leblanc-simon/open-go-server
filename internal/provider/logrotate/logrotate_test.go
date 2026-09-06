package logrotate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/template"
)

func fixture(t *testing.T) (*Provider, []*model.Project) {
	t.Helper()
	prefix := t.TempDir()
	paths := config.NewPaths(prefix)
	proj := model.New("shop.fr", naming.EnvProd, model.KindSymfony)
	proj.Spec.App.Version = "6.4"
	if err := proj.Normalize(filepath.Join(prefix, "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	prov := &Provider{
		Paths:     paths,
		Templates: template.New(paths.TemplatesDir()),
		Config:    config.Default(),
		Now:       func() time.Time { return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) },
	}
	return prov, []*model.Project{proj}
}

func TestPlanEstIdempotent(t *testing.T) {
	prov, projects := fixture(t)
	ctx := context.Background()
	step, err := prov.Plan(projects, "apache", "apache2.service")
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	step, err = prov.Plan(projects, "apache", "apache2.service")
	if err != nil {
		t.Fatal(err)
	}
	if step.Mutates() {
		t.Errorf("second plan non vide : %s", step.Summary)
	}
}

func TestFichierDeRotationModifieALaMain(t *testing.T) {
	// Le fichier est recalculé depuis le parc entier, mais une directive ajoutée
	// à la main — une taille, un « postrotate » — n'en disparaîtrait pas moins
	// sans un mot : les cinq états d'internal/fs valent ici comme ailleurs (§6.4).
	prov, projects := fixture(t)
	ctx := context.Background()
	step, err := prov.Plan(projects, "apache", "apache2.service")
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	path := prov.Paths.LogrotateFile()
	data, _ := os.ReadFile(path)
	const ajout = "\n# maxsize posé à la main\n"
	if err := os.WriteFile(path, append(data, []byte(ajout)...), 0o644); err != nil {
		t.Fatal(err)
	}

	step, err = prov.Plan(projects, "apache", "apache2.service")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(step.Summary, "modifié hors ogs") {
		t.Fatalf("la modification manuelle doit être signalée : %s", step.Summary)
	}
	if err := step.Apply(ctx); err == nil {
		t.Error("l'écrasement doit être refusé sans --force")
	}
	if intact, _ := os.ReadFile(path); !strings.Contains(string(intact), "posé à la main") {
		t.Error("le refus ne doit rien écrire")
	}

	// --force : écrasement, précédé de la sauvegarde.
	prov.Force = true
	step, err = prov.Plan(projects, "apache", "apache2.service")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(step.Summary, "écrasement forcé") {
		t.Fatalf("le plan doit annoncer l'écrasement forcé : %s", step.Summary)
	}
	if err := step.Apply(ctx); err != nil {
		t.Fatalf("l'écrasement forcé doit réussir : %v", err)
	}
	if after, _ := os.ReadFile(path); strings.Contains(string(after), "posé à la main") {
		t.Error("le fichier doit avoir été réécrit")
	}
	baks, _ := filepath.Glob(filepath.Join(prov.Paths.BackupsDir(), "*.ogs-bak-*"))
	if len(baks) != 1 {
		t.Fatalf("une sauvegarde attendue, %d trouvée(s)", len(baks))
	}
	if saved, _ := os.ReadFile(baks[0]); !strings.Contains(string(saved), "posé à la main") {
		t.Error("la sauvegarde doit contenir l'original")
	}
}

func TestSauvegardeDeposeeHorsDeLogrotateD(t *testing.T) {
	// logrotate lit tout /etc/logrotate.d, hors une liste de suffixes tabous où
	// « .ogs-bak-<horodatage> » ne figure pas : une sauvegarde déposée à côté de
	// l'original y serait une seconde rotation des mêmes journaux, et deux
	// directives pour un même chemin arrêtent logrotate pour tout le parc.
	prov, projects := fixture(t)
	ctx := context.Background()
	path := prov.Paths.LogrotateFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("/var/log/x.log { daily }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prov.Force = true
	step, err := prov.Plan(projects, "apache", "apache2.service")
	if err != nil {
		t.Fatal(err)
	}
	// Le plan nomme le répertoire : une sauvegarde qu'il faut chercher n'en est
	// pas une, et l'horodatage n'existe pas encore quand le plan s'affiche — un
	// résumé qui l'annoncerait afficherait son gabarit jusque dans la ligne de
	// succès.
	if !strings.Contains(step.Summary, prov.Paths.BackupsDir()) {
		t.Errorf("le plan doit nommer le répertoire de sauvegarde : %s", step.Summary)
	}
	if strings.Contains(step.Summary, "<horodatage>") {
		t.Errorf("le résumé sert aussi de ligne de succès : il ne peut pas porter un gabarit : %s", step.Summary)
	}
	if err := step.Apply(ctx); err != nil {
		t.Fatalf("l'écrasement forcé doit réussir : %v", err)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "ogs" {
			t.Errorf("%s n'a rien à faire dans %s : logrotate le chargerait",
				e.Name(), filepath.Dir(path))
		}
	}
	baks, _ := filepath.Glob(filepath.Join(prov.Paths.BackupsDir(), "ogs.ogs-bak-*"))
	if len(baks) != 1 {
		t.Fatalf("une sauvegarde attendue dans %s, %d trouvée(s)", prov.Paths.BackupsDir(), len(baks))
	}
	if saved, _ := os.ReadFile(baks[0]); !strings.Contains(string(saved), "/var/log/x.log") {
		t.Errorf("la sauvegarde doit contenir l'original : %q", saved)
	}
}

func TestFichierDeRotationEtranger(t *testing.T) {
	// Un fichier posé par un autre outil ne porte pas la marque d'ogs : le
	// message doit le dire, et non parler d'une modification.
	prov, projects := fixture(t)
	path := prov.Paths.LogrotateFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("/var/log/x.log { daily }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	step, err := prov.Plan(projects, "apache", "apache2.service")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(step.Summary, "non géré par ogs") {
		t.Fatalf("un fichier étranger doit être signalé comme tel : %s", step.Summary)
	}
	err = step.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("le refus doit nommer la porte de sortie : %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), prov.Paths.BackupsDir()) {
		t.Errorf("le refus doit dire où l'original sera sauvegardé : %v", err)
	}
}
