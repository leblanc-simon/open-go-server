package runtime

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
)

func fixture(t *testing.T) (*Provider, *model.Project) {
	t.Helper()
	prefix := t.TempDir()
	proj := model.New("shop.fr", naming.EnvProd, model.KindNode)
	if err := proj.Normalize(filepath.Join(prefix, "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	prov := &Provider{
		Paths: config.NewPaths(prefix),
		Now:   func() time.Time { return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) },
	}
	return prov, proj
}

func TestCreationImmediateSuitLaDeclaration(t *testing.T) {
	// Le fichier déclarait 0710 et la création immédiate posait 0750 : deux
	// endroits décidaient du mode d'un même répertoire, et le projet ne pouvait
	// pas y lier son socket tant que systemd-tmpfiles n'avait pas corrigé.
	prov, proj := fixture(t)
	step, err := prov.Plan([]*model.Project{proj})
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	runDir := prov.Paths.ProjectRunDir(proj.ID())
	info, err := os.Stat(runDir)
	if err != nil {
		t.Fatalf("répertoire absent : %v", err)
	}
	if info.Mode().Perm() != ProjectRunMode.Perm() {
		t.Errorf("mode %04o, attendu %04o", info.Mode().Perm(), ProjectRunMode.Perm())
	}
	// La ligne écrite déclare le même mode : c'est elle qui recrée le répertoire
	// après un redémarrage, /run étant un tmpfs vidé.
	line := "d " + runDir + " 0710 shopfr shopfr -"
	if !strings.Contains(string(prov.Build([]*model.Project{proj})), line) {
		t.Errorf("ligne tmpfiles attendue : %q", line)
	}
}

func TestRacineResteTraversable(t *testing.T) {
	// MkdirAll créerait la racine au mode du répertoire demandé : /run/ogs en
	// 0710 root:root, et plus personne n'atteindrait le socket d'un projet.
	prov, proj := fixture(t)
	runDir := prov.Paths.ProjectRunDir(proj.ID())
	if err := EnsureProjectRunDir(runDir, proj); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Dir(runDir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != RunRootMode.Perm() {
		t.Errorf("mode de la racine %04o, attendu %04o", info.Mode().Perm(), RunRootMode.Perm())
	}
}
