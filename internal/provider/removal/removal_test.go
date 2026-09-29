package removal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/fs"
)

func fichier(t *testing.T) (File, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "shopfr.conf")
	contenu := fs.Render(fs.Header{Project: "shopfr", Resource: "essai"}, []byte("corps\n"), "#")
	if err := os.WriteFile(path, contenu, 0o640); err != nil {
		t.Fatal(err)
	}
	return File{
		Resource: "essai", Summary: "retrait d'essai", Path: path, Project: "shopfr",
		Backups: filepath.Join(dir, "backups"),
		Now:     func() time.Time { return time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC) },
	}, path
}

// Le plan a vu un fichier intact, l'exploitant l'a retouché — ou remplacé —
// avant de confirmer : c'est l'état au moment du retrait qui décide, pas celui du
// plan. Et le refus reste non critique, comme au plan : critique, il faisait
// compenser tout le déploiement pour un fichier qu'on renonce à retirer.
func TestUnFichierRetoucheApresLePlanNEstPasRetire(t *testing.T) {
	for _, cas := range []struct {
		nom      string
		remplace func(data []byte) []byte
	}{
		{"retouché", func(data []byte) []byte { return append(data, "ajout\n"...) }},
		{"remplacé par un fichier sans la marque", func([]byte) []byte { return []byte("à la main\n") }},
	} {
		t.Run(cas.nom, func(t *testing.T) {
			f, path := fichier(t)
			s, err := f.Step()
			if err != nil {
				t.Fatal(err)
			}
			if s == nil || !s.Critical || strings.Contains(s.Summary, "hors ogs") {
				t.Fatalf("un retrait ordinaire est attendu : %+v", s)
			}
			data, _ := os.ReadFile(path)
			nouveau := cas.remplace(data)
			if err := os.WriteFile(path, nouveau, 0o640); err != nil {
				t.Fatal(err)
			}

			compense := false
			avant := engine.NewStep("essai/avant", engine.Create, "étape déjà appliquée")
			avant.Apply = func(context.Context) error { return nil }
			avant.Compensate = func(context.Context) error { compense = true; return nil }
			plan := &engine.Plan{Project: "shopfr"}
			plan.Add(avant)
			plan.Add(s)
			report, err := engine.Execute(context.Background(), plan, engine.Options{})
			if err != nil || compense || report.RolledBack {
				t.Fatalf("un retrait refusé à l'application ne doit rien compenser : %v (compensé : %v)", err, compense)
			}
			if !strings.Contains(strings.Join(report.Warnings, "\n"), path) {
				t.Errorf("le refus doit être rendu en avertissement : %v", report.Warnings)
			}
			if got, _ := os.ReadFile(path); string(got) != string(nouveau) {
				t.Error("le fichier changé après le plan a été retiré")
			}
		})
	}
}

// La compensation rend le fichier tel qu'il était — contenu et mode — et les
// liens qui le désignaient, avec leur cible d'origine.
func TestLaCompensationRendLeFichierSonModeEtSesLiens(t *testing.T) {
	f, path := fichier(t)
	lien := filepath.Join(filepath.Dir(path), "actif.conf")
	if err := os.Symlink("shopfr.conf", lien); err != nil {
		t.Fatal(err)
	}
	f.Links = func() []string { return []string{lien} }
	avant, _ := os.ReadFile(path)
	s, err := f.Step()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, lien} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s devait être retiré : %v", p, err)
		}
	}
	if err := s.Compensate(context.Background()); err != nil {
		t.Fatal(err)
	}
	apres, err := os.ReadFile(path)
	if err != nil || string(apres) != string(avant) {
		t.Fatalf("le fichier n'a pas retrouvé son contenu : %v", err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Errorf("mode %04o, attendu 0640", info.Mode().Perm())
	}
	if cible, err := os.Readlink(lien); err != nil || cible != "shopfr.conf" {
		t.Errorf("le lien n'a pas été rétabli à l'identique : %q, %v", cible, err)
	}
}

// Le refus nomme le propriétaire du fichier là où InspectRemoval l'a lu : une
// ligne ajoutée au-dessus de l'en-tête d'un voisin ne fait pas dire au message
// qu'il ne porte pas la marque d'ogs.
func TestLeRefusNommeLeProprietaireLuCommeInspectRemoval(t *testing.T) {
	f, path := fichier(t)
	voisin := fs.Render(fs.Header{Project: "shopfr_api", Resource: "essai"}, []byte("corps\n"), "#")
	if err := os.WriteFile(path, append([]byte("# ajout\n"), voisin...), 0o640); err != nil {
		t.Fatal(err)
	}
	s, err := f.Step()
	if err != nil || s == nil {
		t.Fatalf("un refus est attendu : %v, %v", s, err)
	}
	if s.Critical || !strings.Contains(s.Summary, "géré pour le projet shopfr_api") {
		t.Errorf("résumé du refus : %q (critique : %v)", s.Summary, s.Critical)
	}
	err = s.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "appartient au projet shopfr_api") {
		t.Errorf("le refus doit nommer le propriétaire : %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("le fichier du voisin a été retiré : %v", statErr)
	}
}

// Un fichier dont l'état ne se lit pas n'est ni un conflit ni une faute de
// déclaration : l'erreur est typée, pour que l'appelant la classe.
func TestUnFichierIlisibleRendUneErreurDExamen(t *testing.T) {
	f, _ := fichier(t)
	f.Inspect = func() (fs.State, error) { return fs.StateAbsent, os.ErrPermission }
	_, err := f.Step()
	var ie *InspectError
	if !errors.As(err, &ie) || !errors.Is(err, os.ErrPermission) {
		t.Errorf("erreur d'examen attendue : %v", err)
	}
}
