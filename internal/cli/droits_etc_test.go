package cli

import (
	iofs "io/fs"
	"os"
	"path/filepath"
	"testing"
)

// Ce que la création et la vie d'un projet écrivent sous /etc/ogs naît au régime
// qu'« ogs harden » exige (0700, 0600) : les déclarations naissaient en 0644, et
// chaque nouveau projet rendait un écart à harden.
func TestEtcOgsResteAuRegimeDeHarden(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static", "--db", "mysql")
	h.mustRun("db", "add", "shopfr", "--role", "api")
	h.mustRunPartial("site", "create", "blog.fr", "--kind", "static", "--group", "acme", "--yes")
	h.mustRun("template", "extract", "nginx/proxy.conf")
	h.mustRunPartial("project", "delete", "blogfr", "--yes")
	verifierRegime(t, h.path("etc", "ogs"))

	// Une déclaration que l'exploitant a voulue lisible du groupe le reste : ogs
	// ne referme pas plus qu'il ne desserre.
	decl := h.path("etc", "ogs", "projects", "shopfr.yaml")
	if err := os.Chmod(decl, 0o640); err != nil {
		t.Fatal(err)
	}
	h.mustRun("db", "add", "shopfr", "--role", "stats")
	if info, err := os.Stat(decl); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("mode de la déclaration réécrite : %04o %v, attendu 0640", info.Mode().Perm(), err)
	}
}

func verifierRegime(t *testing.T, racine string) {
	t.Helper()
	err := filepath.WalkDir(racine, func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		switch {
		case d.IsDir() && mode&^0o700 != 0:
			t.Errorf("%s : répertoire en %04o, attendu 0700", path, mode)
		case info.Mode().IsRegular() && mode&^0o600 != 0:
			t.Errorf("%s : fichier en %04o, au plus 0600", path, mode)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
