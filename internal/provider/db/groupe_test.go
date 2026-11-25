package db

import (
	"context"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

func TestLesBasesDUnProjetGroupeSontPrefixees(t *testing.T) {
	// La base porte le préfixe du groupe, le compte jamais : il est borné à 32
	// caractères, qu'un identifiant de préproduction atteint déjà (D135).
	_, proj := fixture(t)
	proj.Spec.Group = "acme"
	main := DefaultDatabase(proj, "mysql")
	if main.Name != "acme_shopfr" || main.User != "shopfr" {
		t.Errorf("base principale : %q, compte %q", main.Name, main.User)
	}
	role := RoleDatabase(proj, "mysql", "analytics")
	if role.Name != "acme_shopfr_analytics" || role.User != "shopfr_analytics" {
		t.Errorf("base de rôle : %q, compte %q", role.Name, role.User)
	}
	proj.Spec.Group = ""
	if d := DefaultDatabase(proj, "mysql"); d.Name != "shopfr" {
		t.Errorf("sans groupe : %q", d.Name)
	}
}

func TestLaVariableDUneBaseDeRoleIgnoreLePrefixeDuGroupe(t *testing.T) {
	// Le rôle se lit sur le nom de la base : sans retrait du préfixe, la variable
	// devenait ACME_SHOPFR_ANALYTICS_DATABASE_URL.
	prov, proj := fixture(t)
	proj.Spec.Group = "acme"
	proj.Spec.Databases = []model.Database{DefaultDatabase(proj, "mysql"), RoleDatabase(proj, "mysql", "analytics")}
	withStub(prov, &stubClient{})
	for _, d := range proj.Spec.Databases {
		sceller(t, prov, proj, d.Engine, d.User)
	}
	entries, err := prov.Env(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	noms := map[string]bool{}
	for _, e := range entries {
		noms[e.Name] = true
	}
	if !noms["DATABASE_URL"] || !noms["ANALYTICS_DATABASE_URL"] {
		t.Errorf("variables : %v", noms)
	}
}
