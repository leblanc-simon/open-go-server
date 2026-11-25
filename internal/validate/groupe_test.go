package validate

import (
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
)

// groupé crée un projet normalisé dans un groupe (D135).
func groupé(t *testing.T, host, group string) *model.Project {
	t.Helper()
	p := model.New(host, naming.EnvProd, model.KindStatic)
	p.Spec.Group = group
	if err := p.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGroupPlacement(t *testing.T) {
	p := groupé(t, "shop.fr", "acme")
	if err := GroupPlacement(p, "/home"); err != nil {
		t.Fatalf("projet groupé à sa place refusé : %v", err)
	}
	// Un groupe ajouté ou changé après la création ne déplace pas le home.
	p.Spec.Group = "autre"
	if err := GroupPlacement(p, "/home"); err == nil || !strings.Contains(err.Error(), "spec.group") {
		t.Errorf("groupe changé accepté : %v", err)
	}
	ancien := newProject(t, "blog.fr", naming.EnvProd, model.KindStatic)
	ancien.Spec.Group = "acme"
	if err := GroupPlacement(ancien, "/home"); err == nil {
		t.Error("groupe ajouté à un projet existant accepté")
	}
	// Sans groupe, rien à contrôler.
	if err := GroupPlacement(newProject(t, "x.fr", naming.EnvProd, model.KindStatic), "/home"); err != nil {
		t.Errorf("projet sans groupe refusé : %v", err)
	}
}

func TestProjectRefuseLeGroupeDUnProjetAdopte(t *testing.T) {
	p := groupé(t, "shop.fr", "acme")
	if err := Project(p, "/home"); err != nil {
		t.Fatalf("projet groupé refusé : %v", err)
	}
	p.Metadata.Labels = map[string]string{"adopted": "true"}
	if err := Project(p, "/home"); err == nil || !strings.Contains(err.Error(), "spec.group") {
		t.Errorf("projet adopté groupé accepté : %v", err)
	}
	// Un groupe invalide posé après la normalisation est refusé aussi.
	q := groupé(t, "blog.fr", "acme")
	q.Spec.Group = "ac_me"
	if err := Project(q, "/home"); err == nil {
		t.Error("groupe invalide accepté")
	}
}

func TestDatabaseNameLengths(t *testing.T) {
	p := groupé(t, "shop.fr", "acme")
	p.Spec.Databases = []model.Database{
		{Engine: "mysql", Name: strings.Repeat("a", 64), User: "shopfr"},
		{Engine: "postgres", Name: strings.Repeat("b", 63), User: "shopfr"},
	}
	if err := DatabaseNameLengths(p); err != nil {
		t.Fatalf("noms à la limite refusés : %v", err)
	}
	p.Spec.Databases[0].Name += "a"
	p.Spec.Databases[1].Name += "b"
	err := DatabaseNameLengths(p)
	if err == nil || !strings.Contains(err.Error(), "databases[0]") || !strings.Contains(err.Error(), "databases[1]") {
		t.Errorf("noms trop longs : %v", err)
	}
}

func TestUniquenessGroupeQuiPorteLIdentifiantDUnProjet(t *testing.T) {
	// Le préfixe acmefr_ des bases du groupe se lirait comme les bases de rôle
	// du projet acmefr.
	membre := groupé(t, "shop.fr", "acmefr")
	acme := newProject(t, "acme.fr", naming.EnvProd, model.KindStatic)
	err := UniquenessFor([]*model.Project{acme, membre}, "preprod-pp.net", membre.ID())
	if err == nil || !strings.Contains(err.Error(), "groupe") {
		t.Errorf("groupe homonyme d'un projet accepté : %v", err)
	}
}

func TestUniquenessCollisionDeBaseEntreGroupeEtRole(t *testing.T) {
	// Le nom de groupe n'a pas de souligné, mais un identifiant tronqué en porte
	// un (§3.3.2) : acme_shopfr peut aussi être la base d'un voisin. La clé « base
	// de données » de l'unicité porte le nom préfixé, MySQL et MariaDB confondus.
	membre := groupé(t, "shop.fr", "acme")
	membre.Spec.Databases = []model.Database{{Engine: "mysql", Name: "acme_shopfr", User: "shopfr"}}
	voisin := newProject(t, "voisin.fr", naming.EnvProd, model.KindStatic)
	voisin.Spec.Databases = []model.Database{{Engine: "mariadb", Name: "acme_shopfr", User: "voisinfr"}}
	err := UniquenessFor([]*model.Project{voisin, membre}, "preprod-pp.net", membre.ID())
	if err == nil || !strings.Contains(err.Error(), "acme_shopfr") {
		t.Errorf("collision de base acceptée : %v", err)
	}
}

func TestUniquenessHomeDansLeRepertoireDUnAutreGroupe(t *testing.T) {
	membre := groupé(t, "shop.fr", "acme")
	intrus := newProject(t, "blog.fr", naming.EnvProd, model.KindStatic)
	intrus.Spec.Layout.Root = "/home/acme/blog.fr"
	err := UniquenessFor([]*model.Project{membre, intrus}, "preprod-pp.net", intrus.ID())
	if err == nil || !strings.Contains(err.Error(), "répertoire du groupe") {
		t.Errorf("home d'un projet hors groupe dans le répertoire du groupe accepté : %v", err)
	}
	// Deux membres du même groupe cohabitent.
	autre := groupé(t, "blog.fr", "acme")
	if err := UniquenessFor([]*model.Project{membre, autre}, "preprod-pp.net", autre.ID()); err != nil {
		t.Errorf("deux membres du groupe refusés : %v", err)
	}
}

func TestUniquenessHomeEgalAuRepertoireDUnGroupe(t *testing.T) {
	// Supprimer ce projet effacerait tout le groupe : c'est un home imbriqué.
	membre := groupé(t, "shop.fr", "acme")
	racine := newProject(t, "blog.fr", naming.EnvProd, model.KindStatic)
	racine.Spec.Layout.Root = "/home/acme"
	if err := UniquenessFor([]*model.Project{membre, racine}, "preprod-pp.net", racine.ID()); err == nil {
		t.Error("home égal au répertoire d'un groupe accepté")
	}
}
