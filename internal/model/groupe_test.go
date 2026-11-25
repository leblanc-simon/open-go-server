package model

import (
	"testing"

	"leblanc.io/open-go-server/internal/naming"
)

func TestNormalizeRangeLeProjetGroupeSousSonGroupe(t *testing.T) {
	// Un projet groupé vit sous <racine des homes>/<groupe>/<host effectif>,
	// préproduction comprise (D135) ; le compte garde l'identifiant.
	for _, c := range []struct {
		env  naming.Env
		root string
	}{
		{naming.EnvProd, "/home/acme/shop.fr"},
		{naming.EnvPrep, "/home/acme/shop.prep.example"},
	} {
		p := New("shop.fr", c.env, KindStatic)
		p.Spec.Group = "acme"
		if err := p.Normalize("/home", "prep.example"); err != nil {
			t.Fatal(err)
		}
		if p.Spec.Layout.Root != c.root || p.Spec.User.Home != c.root {
			t.Errorf("%s : racine %q, home %q, attendu %q", c.env, p.Spec.Layout.Root, p.Spec.User.Home, c.root)
		}
		if p.Spec.User.Name != p.ID() {
			t.Errorf("%s : le compte doit rester l'identifiant, obtenu %q", c.env, p.Spec.User.Name)
		}
	}
}

func TestNormalizeGardeLaRacineEcriteDUnProjetGroupe(t *testing.T) {
	// Le groupe est figé à la création : Normalize ne remplit que les vides, et
	// une racine écrite n'est jamais recalculée d'après spec.group.
	p := New("shop.fr", naming.EnvProd, KindStatic)
	p.Spec.Group = "acme"
	p.Spec.Layout.Root = "/home/shop.fr"
	if err := p.Normalize("/home", "prep.example"); err != nil {
		t.Fatal(err)
	}
	if p.Spec.Layout.Root != "/home/shop.fr" {
		t.Errorf("racine recalculée : %q", p.Spec.Layout.Root)
	}
}

func TestNormalizeRefuseUnGroupeQuiSortiraitDeLaRacine(t *testing.T) {
	for _, g := range []string{"..", "a/b", "ac_me"} {
		p := New("shop.fr", naming.EnvProd, KindStatic)
		p.Spec.Group = g
		if err := p.Normalize("/home", "prep.example"); err == nil {
			t.Errorf("groupe %q accepté, racine %q", g, p.Spec.Layout.Root)
		}
	}
}
