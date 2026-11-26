package cli

import (
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
