package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Une déclaration groupée, écrite à la main, se planifie sous le répertoire de
// son groupe, bases préfixées (D135).
func TestLePlanDUnProjetGroupeLeRangeSousSonGroupe(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	decl := filepath.Join(t.TempDir(), "blogfr.yaml")
	contenu := `apiVersion: ogs/v1
kind: Project
metadata:
  host: blog.fr
spec:
  group: acme
  app:
    kind: static
  databases:
    - engine: mysql
      name: acme_blogfr
      user: blogfr
`
	if err := os.WriteFile(decl, []byte(contenu), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("plan", "-f", decl)
	if !strings.Contains(out, filepath.Join("home", "acme", "blog.fr")) {
		t.Errorf("le plan doit poser le home sous le répertoire du groupe :\n%s", out)
	}
	if !strings.Contains(out, "acme_blogfr") {
		t.Errorf("le plan doit nommer la base préfixée :\n%s", out)
	}
}

// Un groupe ajouté après la création ne déplace pas le home : le plan du projet
// le refuse en code 3, sans arrêter celui d'un voisin (D135, D120).
func TestUnGroupeAjouteApresCoupNArretePasLesVoisins(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr")
	h.mustRun("project", "create", "blog.fr")
	rel := filepath.Join("etc", "ogs", "projects", "blogfr.yaml")
	avant := h.read(t, rel)
	apres := strings.Replace(avant, "\nspec:\n", "\nspec:\n  group: acme\n", 1)
	if apres == avant {
		t.Fatalf("aucune section spec dans %s :\n%s", rel, avant)
	}
	if err := os.WriteFile(h.path(rel), []byte(apres), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := h.run("plan", "blogfr")
	if code != CodeValidation || !strings.Contains(out, "spec.group") {
		t.Errorf("« plan blogfr » : code %d, attendu %d, avec spec.group :\n%s", code, CodeValidation, out)
	}
	h.mustRunPartial("plan", "shopfr")
}

// En mode préfixé, l'apply de deux membres d'un groupe crée un seul répertoire
// de groupe, en 0701, et les deux homes dessous (D135).
func TestLApplyDeDeuxMembresCreeLeRepertoireDuGroupe(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	for _, host := range []string{"shop.fr", "blog.fr"} {
		id := strings.ReplaceAll(host, ".", "")
		decl := h.path(filepath.Join("etc", "ogs", "projects", id+".yaml"))
		contenu := "apiVersion: ogs/v1\nkind: Project\nmetadata:\n  host: " + host +
			"\nspec:\n  group: acme\n  app:\n    kind: static\n"
		if err := os.WriteFile(decl, []byte(contenu), 0o644); err != nil {
			t.Fatal(err)
		}
		h.mustRunPartial("apply", id, "--yes")
	}
	info, err := os.Lstat(h.path(filepath.Join("home", "acme")))
	if err != nil || info.Mode().Perm() != 0o701 {
		t.Fatalf("répertoire du groupe : %v, %v", info, err)
	}
	for _, host := range []string{"shop.fr", "blog.fr"} {
		if _, err := os.Lstat(h.path(filepath.Join("home", "acme", host))); err != nil {
			t.Errorf("home de %s : %v", host, err)
		}
	}
}

// déclarerGroupé dépose la déclaration d'un membre du groupe acme et l'applique.
func (h *harness) déclarerGroupé(t *testing.T, host string) string {
	t.Helper()
	id := strings.ReplaceAll(host, ".", "")
	contenu := "apiVersion: ogs/v1\nkind: Project\nmetadata:\n  host: " + host +
		"\nspec:\n  group: acme\n  app:\n    kind: static\n"
	if err := os.WriteFile(h.path("etc", "ogs", "projects", id+".yaml"), []byte(contenu), 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustRunPartial("apply", id, "--yes")
	return id
}

func existe(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// Le répertoire du groupe part avec son dernier membre, jamais avant (D135).
func TestLeRepertoireDuGroupePartAvecSonDernierMembre(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.déclarerGroupé(t, "shop.fr")
	h.déclarerGroupé(t, "blog.fr")
	groupe := h.path("home", "acme")

	h.mustRunPartial("project", "delete", "shopfr", "--yes")
	if existe(t, filepath.Join(groupe, "shop.fr")) || !existe(t, filepath.Join(groupe, "blog.fr")) {
		t.Fatal("seul le home de shopfr doit partir")
	}
	out := h.mustRunPartial("project", "delete", "blogfr", "--yes")
	if existe(t, groupe) {
		t.Errorf("le répertoire du groupe doit partir avec son dernier membre :\n%s", out)
	}
}

// --keep-data laisse le home, donc le répertoire du groupe.
func TestKeepDataLaisseLeRepertoireDuGroupe(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.déclarerGroupé(t, "shop.fr")
	h.mustRunPartial("project", "delete", "shopfr", "--keep-data", "--yes")
	if !existe(t, h.path("home", "acme", "shop.fr")) {
		t.Error("--keep-data doit laisser le home et son répertoire de groupe")
	}
}

// Une restauration recrée le répertoire du groupe, en 0701, d'après la
// déclaration archivée.
func TestLaRestaurationRecreeLeRepertoireDuGroupe(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.déclarerGroupé(t, "shop.fr")
	h.mustRunPartial("project", "delete", "shopfr", "--yes")
	if existe(t, h.path("home", "acme")) {
		t.Fatal("le répertoire du groupe doit être parti")
	}
	h.mustRunPartial("project", "restore", h.derniereArchive(t), "--yes")
	info, err := os.Lstat(h.path("home", "acme"))
	if err != nil || info.Mode().Perm() != 0o701 {
		t.Fatalf("répertoire du groupe après restauration : %v, %v", info, err)
	}
	if !existe(t, h.path("home", "acme", "shop.fr")) {
		t.Error("home absent après restauration")
	}
	if decl := h.read(t, filepath.Join("etc", "ogs", "projects", "shopfr.yaml")); !strings.Contains(decl, "group: acme") {
		t.Errorf("la déclaration restaurée doit garder son groupe :\n%s", decl)
	}
}

// Un projet dont la racine est le répertoire d'un groupe emporterait tous ses
// membres : sa suppression est refusée.
func TestLaSuppressionRefuseUneRacineEgaleAuRepertoireDUnGroupe(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.déclarerGroupé(t, "shop.fr")
	h.mustRun("project", "create", "blog.fr")
	rel := filepath.Join("etc", "ogs", "projects", "blogfr.yaml")
	decl := h.read(t, rel)
	avant := h.path("home", "blog.fr")
	if !strings.Contains(decl, "root: "+avant) {
		t.Fatalf("racine introuvable dans la déclaration :\n%s", decl)
	}
	decl = strings.ReplaceAll(decl, avant, h.path("home", "acme"))
	if err := os.WriteFile(h.path(rel), []byte(decl), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := h.run("project", "delete", "blogfr", "--yes")
	if code != CodeConflict || !strings.Contains(out, "shopfr") {
		t.Errorf("la suppression doit être refusée en nommant le membre (code %d), obtenu %d :\n%s",
			CodeConflict, code, out)
	}
	if !existe(t, h.path("home", "acme", "shop.fr")) {
		t.Error("le home d'un membre du groupe a été effacé")
	}
}
