package system

import (
	"os"
	"path/filepath"
	"testing"
)

// Le venv d'une version de Python vit à la racine du home (D123) : une saga de
// création déroulée le laisse comme .pyenv, et l'apply suivant doit reprendre
// le home. Un fichier de ce nom n'est pas un venv.
func TestLaCreationRepriseLeVenvDUneVersion(t *testing.T) {
	p, prov, _, _ := fixture(t)
	root := p.Spec.Layout.Root
	homePreexistant(t, p)
	if err := os.MkdirAll(filepath.Join(root, ".venv-3.12.7", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, ".venv-3.12.7"), filepath.Join(root, ".venv")); err != nil {
		t.Fatal(err)
	}
	if err := orphelin(t, prov, p, 1004); err != nil {
		t.Errorf("le venv d'une version et son lien sont du squelette d'ogs : %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".venv-faux"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := orphelin(t, prov, p, 1004); err == nil {
		t.Error("un fichier au nom d'un venv n'est pas un venv")
	}
}
