package cli

import (
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/spec"
)

// Chaque base de la source garde son rôle dans le clone : toutes devenaient la
// base principale, et deux bases d'un même moteur y prenaient le même nom.
func TestLeCloneGardeLeRoleDeChaqueBase(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static", "--group", "acme", "--db", "mysql")
	h.mustRun("db", "add", "shopfr", "--role", "api")
	h.mustRun("db", "add", "shopfr", "--engine", "postgres")
	h.mustRunPartial("apply", "--yes")
	out, code := h.run("site", "clone", "shopfr", "--to", "prep", "--yes")
	if code != CodeOK && code != CodePartial {
		t.Fatalf("clone en échec (code %d) :\n%s", code, out)
	}
	clone, err := spec.Load(config.NewPaths(h.prefix).ProjectFile("shopfrprep"))
	if err != nil {
		t.Fatal(err)
	}
	attendu := []struct{ engine, name, user string }{
		{"mysql", "acme_shopfrprep", "shopfrprep"},
		{"mysql", "acme_shopfrprep_api", "shopfrprep_api"},
		{"postgres", "acme_shopfrprep", "shopfrprep"},
	}
	if len(clone.Spec.Databases) != len(attendu) {
		t.Fatalf("bases du clone : %+v", clone.Spec.Databases)
	}
	for i, a := range attendu {
		d := clone.Spec.Databases[i]
		if d.Engine != a.engine || d.Name != a.name || d.User != a.user {
			t.Errorf("base %d : %s %s (%s), attendu %s %s (%s)", i, d.Engine, d.Name, d.User, a.engine, a.name, a.user)
		}
	}
}

// Une source adoptée porte des noms qu'ogs n'a pas tirés : le clone en dérive
// un rôle, numérote ce qui se recouvrirait, et garde un compte partagé partagé.
func TestLeCloneDUneSourceAdopteeNeFusionneAucuneBase(t *testing.T) {
	source := model.New("shop.fr", naming.EnvProd, model.KindStatic)
	clone := model.New("shop.fr", naming.EnvPrep, model.KindStatic)
	for _, p := range []*model.Project{source, clone} {
		if err := p.Normalize("/home", "prep.example"); err != nil {
			t.Fatal(err)
		}
	}
	source.Spec.Databases = []model.Database{
		{Engine: "mysql", Name: "boutique", User: "boutique"},
		{Engine: "mysql", Name: "Boutique-Stats", User: "boutique"},
		{Engine: "mysql", Name: "shopfr", User: "shopfr"},
		{Engine: "mysql", Name: "autre", User: "shopfr"},
	}
	got := cloneDatabases(source, clone)
	attendu := []struct{ name, user string }{
		{"shopfrprep_boutique", "shopfrprep_boutique"},
		{"shopfrprep_boutique_stats", "shopfrprep_boutique"},
		{"shopfrprep", "shopfrprep"},
		{"shopfrprep_2", "shopfrprep"},
	}
	for i, a := range attendu {
		if got[i].Name != a.name || got[i].User != a.user {
			t.Errorf("base %d : %s (%s), attendu %s (%s)", i, got[i].Name, got[i].User, a.name, a.user)
		}
	}
}

// --with-data ne copie que la première base : les autres, créées vides, sont
// nommées, en simulation comme à l'exécution, et le clone rend un succès partiel.
func TestLeCloneAvecDonneesNommeLesBasesLaisseesVides(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static", "--db", "mysql")
	h.mustRun("db", "add", "shopfr", "--role", "api")
	h.mustRunPartial("apply", "--yes")
	out, _ := h.run("--dry-run", "site", "clone", "shopfr", "--to", "prep", "--with-data")
	if !strings.Contains(out, "--with-data ne copie que la base shopfrprep : mysql shopfrprep_api resteraient vides") {
		t.Errorf("la simulation doit nommer la base laissée vide :\n%s", out)
	}
	out, code := h.run("site", "clone", "shopfr", "--to", "prep", "--with-data", "--yes")
	if code != CodePartial {
		t.Errorf("code %d, attendu %d (succès partiel)", code, CodePartial)
	}
	for _, attendu := range []string{
		"--with-data ne copie que la base shopfrprep : mysql shopfrprep_api restent vides",
		"1 base(s) laissée(s) vide(s)",
	} {
		if !strings.Contains(out, attendu) {
			t.Errorf("%q absent de la sortie :\n%s", attendu, out)
		}
	}
	// --with-data=false (la copie est le défaut) : aucune base n'est dite vide.
	h2 := newHarness(t)
	h2.mustRun("init")
	h2.mustRun("project", "create", "shop.fr", "--kind", "static", "--db", "mysql")
	h2.mustRun("db", "add", "shopfr", "--role", "api")
	h2.mustRunPartial("apply", "--yes")
	if out, _ := h2.run("site", "clone", "shopfr", "--to", "prep", "--with-data=false", "--yes"); strings.Contains(out, "laissée(s) vide(s)") {
		t.Errorf("sans copie des données, rien n'est dit vide :\n%s", out)
	}
}
