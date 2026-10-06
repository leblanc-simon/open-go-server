package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// L'échec de doctor ne vaut que pour un vhost d'ogs activé sous l'ancien moteur
// (relecture de D118) : un vhost seulement disponible, ou le seul complément
// d'unité, ne servent rien.
func TestLaConfigurationResteeDitSiUnVhostEstActive(t *testing.T) {
	prov, _ := fixture(t, "nginx")
	ancien := otherEngine(prov.Engine)
	for _, d := range []string{ancien.AvailableDir(), ancien.EnabledDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	chemin := filepath.Join(ancien.AvailableDir(), "shopfr.conf")
	if err := os.WriteFile(chemin, vhostEcritPour("shopfr", "x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, files := prov.Leftovers()
	if len(files) != 1 || prov.LeftoversServed(files) || prov.LeftoversNote() == "" {
		t.Fatalf("un vhost disponible, non activé : %v", files)
	}
	if err := os.Symlink("../sites-available/shopfr.conf", filepath.Join(ancien.EnabledDir(), "shopfr.conf")); err != nil {
		t.Fatal(err)
	}
	_, files = prov.Leftovers()
	if !prov.LeftoversServed(files) || !strings.HasSuffix(files[0], "(activé)") {
		t.Errorf("le vhost activé doit être dit tel : %v", files)
	}
}
