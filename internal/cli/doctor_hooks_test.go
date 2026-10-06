package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R3-21 : un hook lié hors de /etc/ogs/hooks était exécuté sans un mot, là où
// HOOKS.md le disait refusé. Le refuser casserait des parcs qui en dépendent
// (D117) : doctor le signale, et un hook qui serait refusé l'est dès doctor.
func TestDoctorSignaleUnHookLieHorsDuRepertoire(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	ailleurs := filepath.Join(t.TempDir(), "notifier")
	if err := os.WriteFile(ailleurs, []byte("#!/bin/sh\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ailleurs, 0o750); err != nil {
		t.Fatal(err)
	}
	lien := h.path("etc", "ogs", "hooks", "post-apply")
	if err := os.Symlink(ailleurs, lien); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.path("etc", "ogs", "hooks", "pre-delete"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := h.run("doctor")
	if !strings.Contains(out, lien) || !strings.Contains(out, "hors de") {
		t.Errorf("doctor doit signaler le lien qui sort du répertoire :\n%s", out)
	}
	if !strings.Contains(out, "n'est pas exécutable") {
		t.Errorf("doctor doit signaler le hook qui serait refusé :\n%s", out)
	}
}
