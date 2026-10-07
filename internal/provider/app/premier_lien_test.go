package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
)

// projetPythonSansLienGere rend un projet Python d'avant D70 : ni
// ~/.local/bin/python ni ~/.venv, l'ancien .bin/python désignant www/.venv, créé
// par le pyenv du home pour la version 3.11.9 et pourvu de ses dépendances ; la
// déclaration demande 3.12.7, dont l'installation pose un venv neuf, vide.
func projetPythonSansLienGere(t *testing.T) (*Provider, *model.Project, string, string) {
	t.Helper()
	prov, proj := fixture(t, model.KindPython)
	prov.Exec = &realExecutor{exec.NewRecorder()}
	prov.Installer = &Installer{Exec: prov.Exec}
	root := proj.Spec.Layout.Root
	proj.Spec.App.Runtime = "3.12.7"
	ancien := filepath.Join(root, "www", ".venv")
	pyvenv(t, ancien, filepath.Join(root, ".pyenv", "versions", "3.11.9", "bin"), "3.11.9")
	poserBinaire(t, filepath.Join(ancien, "bin", "gunicorn"))
	if err := os.MkdirAll(filepath.Join(root, model.LegacyLinkDir), 0o710); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ancien, "bin", "python"), filepath.Join(root, model.LegacyLinkDir, "python")); err != nil {
		t.Fatal(err)
	}
	neuf := filepath.Join(root, ".venv-3.12.7")
	pyvenv(t, neuf, filepath.Join(root, ".pyenv", "versions", "3.12.7", "bin"), "3.12.7")
	return prov, proj, ancien, neuf
}

// D123 × D116 : un premier lien Python n'avait aucune garde — elle ne valait
// que si un lien existait déjà. Un projet dont le www/.venv, pourvu de ses
// dépendances, est d'une autre version que la déclarée voyait ses deux liens
// posés vers le venv neuf et vide ; la tâche que D116 réécrit vers
// ~/.local/bin/python passait alors d'un venv qui fonctionnait à un venv vide.
// Un premier lien n'est posé vers un venv sans gunicorn que si aucun venv
// antérieur du projet ne peut servir.
func TestUnPremierLienNeDesignePasUnVenvVideQuandUnAutreSert(t *testing.T) {
	prov, proj, ancien, neuf := projetPythonSansLienGere(t)
	ctx := context.Background()
	steps := planDe(t, prov, proj)
	for _, r := range []string{"app/link/python", "app/link/.venv"} {
		s := étape(t, steps, r)
		if !strings.Contains(s.Summary, "pip install") {
			t.Errorf("%s : le plan doit annoncer les dépendances attendues : %s", r, s.Summary)
		}
		err := s.Apply(ctx)
		if !errorsAsTolerated(err) || !strings.Contains(err.Error(), "-m pip install") ||
			!strings.Contains(err.Error(), ancien) {
			t.Errorf("%s : échec toléré nommant pip et le venv qui sert attendu, obtenu %v", r, err)
		}
	}
	if got := readLink(proj, LinkName(RuntimePython)); got != "" {
		t.Errorf("aucun lien de l'interpréteur ne doit être posé : %s", got)
	}
	if kind, _, _ := fsLinkState(proj.Spec.Layout.Root, VenvLinkName); kind != "absent" {
		t.Errorf("~/.venv ne doit pas être posé : %s", kind)
	}

	// Dépendances installées dans le venv de la version : les liens sont posés.
	poserBinaire(t, filepath.Join(neuf, "bin", "gunicorn"))
	for _, r := range []string{"app/link/python", "app/link/.venv"} {
		s := étape(t, planDe(t, prov, proj), r)
		if s.Apply == nil {
			t.Errorf("%s : lien attendu, obtenu %s", r, s.Summary)
			continue
		}
		if err := s.Apply(ctx); err != nil {
			t.Errorf("%s : %v", r, err)
		}
	}
	if got := readLink(proj, LinkName(RuntimePython)); got != filepath.Join(neuf, "bin", "python") {
		t.Errorf("lien de l'interpréteur : %s", got)
	}
}

// Sans venv antérieur qui puisse servir, un premier lien reste posé vers le
// venv neuf : il n'y a rien à garder (premier apply d'un projet neuf). Un
// www/.venv sans gunicorn ne retient rien non plus.
func TestUnPremierLienSansVenvAnterieurEstPose(t *testing.T) {
	prov, proj, ancien, neuf := projetPythonSansLienGere(t)
	if err := os.Remove(filepath.Join(ancien, "bin", "gunicorn")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, r := range []string{"app/link/python", "app/link/.venv"} {
		s := étape(t, planDe(t, prov, proj), r)
		if s.Apply == nil {
			t.Errorf("%s : lien attendu, obtenu %s", r, s.Summary)
			continue
		}
		if err := s.Apply(ctx); err != nil {
			t.Errorf("%s : %v", r, err)
		}
	}
	if got := readLink(proj, LinkName(RuntimePython)); got != filepath.Join(neuf, "bin", "python") {
		t.Errorf("lien de l'interpréteur : %s", got)
	}
}

// LinkNotReady (D116 × D123) : dans un projet Python, une commande .bin/python
// est réécrite vers ~/.venv/bin/python, qui active le venv ; la réécriture
// attend que ~/.venv désigne un venv pourvu de ses dépendances — ou le venv même
// que désignait l'ancien .bin/python.
func TestLaReecritureVersPythonAttendUnVenvPourvu(t *testing.T) {
	prov, proj, ancien, neuf := projetPythonSansLienGere(t)
	if why := LinkNotReady(proj, "python"); why != "" {
		t.Errorf("~/.venv absent : la garde des remplaçants manquants en décide, obtenu %q", why)
	}
	if err := prov.setVenvLink(proj, neuf); err != nil {
		t.Fatal(err)
	}
	why := LinkNotReady(proj, "python")
	if !strings.Contains(why, neuf) || !strings.Contains(why, "-m pip install") {
		t.Errorf("le venv vide doit être nommé avec la commande pip : %q", why)
	}
	if why := LinkNotReady(proj, "php"); why != "" {
		t.Errorf("seul python est concerné : %q", why)
	}
	poserBinaire(t, filepath.Join(neuf, "bin", "gunicorn"))
	if why := LinkNotReady(proj, "python"); why != "" {
		t.Errorf("venv pourvu : %q", why)
	}
	// Le même venv que l'ancien lien : rien ne change pour la commande.
	if err := os.Remove(filepath.Join(ancien, "bin", "gunicorn")); err != nil {
		t.Fatal(err)
	}
	if err := prov.setVenvLink(proj, ancien); err != nil {
		t.Fatal(err)
	}
	if why := LinkNotReady(proj, "python"); why != "" {
		t.Errorf("même venv que l'ancien .bin/python : %q", why)
	}
}

// Relecture (mineur 3) : un venv dont l'interpréteur ne se lance plus — lien
// pendant — ne retient pas le premier lien ; un www/.venv recopié d'un autre
// home par un clone d'avant D123 le retient, et le message le dit.
func TestUnVenvAnterieurDoitPouvoirSeLancer(t *testing.T) {
	prov, proj, ancien, neuf := projetPythonSansLienGere(t)
	py := filepath.Join(ancien, "bin", "python")
	if err := os.Remove(py); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nulle/part/python3", py); err != nil {
		t.Fatal(err)
	}
	if got := ServingVenvs(proj.Spec.Layout.Root, neuf); len(got) != 0 {
		t.Errorf("un interpréteur pendant ne sert plus : %v", got)
	}

	prov, proj, ancien, _ = projetPythonSansLienGere(t)
	pyvenv(t, ancien, "/home/production/.pyenv/versions/3.11.9/bin", "3.11.9")
	err := étape(t, planDe(t, prov, proj), "app/link/python").Apply(context.Background())
	if !errorsAsTolerated(err) || !strings.Contains(err.Error(), "clone") {
		t.Errorf("le venv recopié par un clone doit être nommé comme tel : %v", err)
	}
}
