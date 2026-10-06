package removal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/fs"
)

// La ressource de l'en-tête est relue au retrait (D121) : un autre provider a
// pu réécrire le fichier sous la sienne entre le plan et l'application.
func TestLaRessourceEstReluaAuRetrait(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ogs-appfr-app@.service")
	ecrire := func(resource string) {
		t.Helper()
		if err := os.WriteFile(path, fs.Render(fs.Header{Project: "appfr", Resource: resource},
			[]byte("[Service]\n"), "#"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ecrire("unit/app")
	f := File{Resource: "unit/service/app", Summary: "unité obsolète", Path: path, Project: "appfr",
		Backups: filepath.Join(dir, "sauvegardes"),
		Owns:    func(r string) bool { return strings.HasPrefix(r, "unit/") }}
	s, err := f.Step()
	if err != nil || s == nil {
		t.Fatalf("le modèle marqué unit/app doit être à retirer au plan : %v %v", s, err)
	}
	ecrire("app/app")
	if err := s.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("réécrit sous app/app, le fichier n'est plus à retirer : %v", err)
	}
	if s, _ := f.Step(); s != nil {
		t.Errorf("le plan suivant ne doit plus rien retirer : %s", s.Summary)
	}
}
