package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// projetPythonAncienBin rend un projet Python d'avant D70 : ni
// ~/.local/bin/python ni ~/.venv, l'ancien .bin/python désignant www/.venv, et
// une tâche qui l'invoque.
func projetPythonAncienBin(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "py.fr", "--kind", "python", "--runtime", "3.12.7", "--tls=false", "--yes")
	root := h.path("home", "py.fr")
	for _, l := range []string{filepath.Join(root, ".local", "bin", "python"), filepath.Join(root, ".venv")} {
		if err := os.Remove(l); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	ancien := filepath.Join(root, "www", ".venv")
	pyvenvDe(t, ancien, root, "3.12.7")
	déposer(t, filepath.Join(ancien, "bin", "gunicorn"), "#!/bin/sh\n")
	if err := os.MkdirAll(filepath.Join(root, ".bin"), 0o710); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ancien, "bin", "python"), filepath.Join(root, ".bin", "python")); err != nil {
		t.Fatal(err)
	}
	h.mustRun("cron", "add", "pyfr", "nuit", "--schedule", "0 3 * * *",
		"--command", root+"/.bin/python manage.py nettoyer")
	return h, root
}

// Arbitrage du lot Z (D116 × D123) : ~/.local/bin/python désigne
// <venv>/bin/python, et Python n'active pas un venv atteint par un lien vers son
// interpréteur — une tâche réécrite vers lui tournait sur l'interpréteur de
// base, sans les dépendances du venv. Dans un projet Python, la migration
// réécrit .bin/python vers ~/.venv/bin/python, une fois ~/.venv posé, et retire
// .bin ensuite. Sous préfixe, rien n'est installé : la garde des dépendances se
// tait (comme les contrôles des liens), et seul le remplaçant est attendu.
func TestLaTachePythonEstReecriteVersLeVenv(t *testing.T) {
	h, root := projetPythonAncienBin(t)
	decl := h.path("etc", "ogs", "projects", "pyfr.yaml")
	for i := 0; i < 3; i++ {
		h.mustRunPartial("apply", "pyfr", "--yes")
	}
	got, err := os.ReadFile(decl)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), root+"/.venv/bin/python manage.py") || strings.Contains(string(got), ".local/bin/python") {
		t.Errorf("la tâche doit désigner ~/.venv/bin/python :\n%s", got)
	}
	if cron := h.read(t, "etc/cron.d/ogs-pyfr"); !strings.Contains(cron, root+"/.venv/bin/python manage.py") {
		t.Errorf("le fichier de tâches doit suivre :\n%s", cron)
	}
	if _, err := os.Lstat(filepath.Join(root, ".bin")); !os.IsNotExist(err) {
		t.Errorf("l'ancien .bin doit partir une fois ~/.venv en place : %v", err)
	}
}

// Tant que ~/.venv manque, la tâche garde .bin/python et l'étape dit ce qu'elle
// attend : le message ne parle pas d'une forme qu'ogs ne réécrit pas.
func TestLaTachePythonAttendLeLienDuVenv(t *testing.T) {
	h, root := projetPythonAncienBin(t)
	// ~/.venv occupé par un répertoire réel : ogs ne le remplace jamais.
	if err := os.MkdirAll(filepath.Join(root, ".venv"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := h.run("apply", "pyfr", "--yes")
	if code != CodePartial {
		t.Errorf("code %d, attendu %d :\n%s", code, CodePartial, out)
	}
	contientTout(t, out, "spec.crons[nuit].command", "réécrite à l'apply suivant une fois "+filepath.Join(root, ".venv"))
	if strings.Contains(out, "ne réécrit pas") {
		t.Errorf("une réécriture reportée n'est pas une forme non réécrivable :\n%s", out)
	}
	if got := h.read(t, "etc/ogs/projects/pyfr.yaml"); !strings.Contains(got, root+"/.bin/python manage.py") {
		t.Errorf("la déclaration ne doit pas être réécrite :\n%s", got)
	}
}

// doctor nomme une commande qui appelle ~/.local/bin/python, avec le remède ;
// ni le plan ni l'apply ne la refusent.
func TestDoctorNommeLesAppelsAPythonHorsVenv(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "py.fr", "--kind", "python", "--runtime", "3.12.7", "--tls=false", "--yes")
	root := h.path("home", "py.fr")
	h.mustRun("cron", "add", "pyfr", "nuit", "--schedule", "0 3 * * *",
		"--command", root+"/.local/bin/python manage.py nettoyer")
	h.mustRunPartial("apply", "pyfr", "--yes")
	out, _ := h.run("doctor")
	l := lignesDoctor(out, "python hors venv pyfr")
	if !strings.Contains(l, "spec.crons[nuit].command") || !strings.Contains(l, root+"/.venv/bin/python") {
		t.Errorf("doctor doit nommer la tâche et le remède :\n%s", out)
	}
}
