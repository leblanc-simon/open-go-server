package db

import (
	"context"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

// Hôte changé deux fois, le second apply déroulé : la compensation efface la
// marque entière, au lieu de rendre celle d'avant.
func TestRelectureT3DéroulageDuSecondDéplacementEffaceLaMarque(t *testing.T) {
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	decl.Host = "%"
	proj.Spec.Databases = []model.Database{decl}
	sceller(t, prov, proj, "mysql", "shopfr")
	stub := vivant([]string{"shopfr"}, []string{"shopfr@localhost"},
		map[string]map[string]string{"shopfr": {"shopfr": tousDroits}})
	withStub(prov, stub)
	if errs := appliquer(t, planifier(t, prov, proj)); len(errs) > 0 {
		t.Fatal(errs)
	}
	t.Logf("après 1er déplacement : %v marque=%v", stub.users, prov.movedHosts(proj.ID(), "mysql", "shopfr"))
	proj.Spec.Databases[0].Host = "10.0.0.1"
	steps := planifier(t, prov, proj)
	if errs := appliquer(t, steps); len(errs) > 0 {
		t.Fatal(errs)
	}
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Compensate != nil {
			_ = steps[i].Compensate(context.Background())
		}
	}
	t.Logf("après déroulage du 2e : %v marque=%v", stub.users, prov.movedHosts(proj.ID(), "mysql", "shopfr"))
	if len(prov.movedHosts(proj.ID(), "mysql", "shopfr")) == 0 && stub.users["shopfr@localhost"] && stub.users["shopfr@%"] {
		t.Errorf("deux comptes d'ogs restent, la marque qui les prouvait est effacée")
	}
	// L'exploitant revient à « % » : l'ancien compte sous localhost reste à jamais.
	proj.Spec.Databases[0].Host = "%"
	for i := 0; i < 2; i++ {
		errs := appliquer(t, planifier(t, prov, proj))
		t.Logf("apply %d : %v ; comptes %v", i, errs, stub.users)
	}
	if stub.users["shopfr@localhost"] {
		t.Errorf("'shopfr'@'localhost', créé par ogs, n'est jamais retiré")
	}
	// Et la suppression du projet ?
	obs, _ := prov.Observe(context.Background(), proj)
	for _, st := range obs.States {
		kept, err := prov.Drop(context.Background(), st.Declaration, st)
		t.Logf("drop : kept=%v err=%v comptes=%v", kept, err, stub.users)
	}
	if stub.users["shopfr@localhost"] {
		t.Errorf("après project delete, 'shopfr'@'localhost' reste, avec ses droits, sans être nommé")
	}
}

// Projet adopté d'un parc antérieur à D78 : déclaration « localhost », compte
// réel 'shopfr'@'%' (celui de l'application, mot de passe inconnu d'ogs), un
// mot de passe scellé du temps où ogs créait l'homonyme sous localhost, que
// l'exploitant a retiré à la main. Avant le lot : refus de D78, rien ne bouge.
func TestRelectureT3AdoptéAncienneDéclarationLocalhost(t *testing.T) {
	prov, proj := fixture(t)
	proj.Metadata.Labels = map[string]string{"adopted": "true"}
	decl := DefaultDatabase(proj, "mysql")
	decl.Host = "localhost"
	proj.Spec.Databases = []model.Database{decl}
	sceller(t, prov, proj, "mysql", "shopfr")
	stub := vivant([]string{"shopfr"}, []string{"shopfr@%"},
		map[string]map[string]string{"shopfr": {"shopfr": tousDroits}})
	withStub(prov, stub)
	for i := 0; i < 2; i++ {
		steps := planifier(t, prov, proj)
		for _, s := range steps {
			t.Logf("apply %d étape %s : %s", i, s.Resource, s.Summary)
		}
		errs := appliquer(t, steps)
		t.Logf("apply %d : erreurs %v ; comptes %v", i, errs, stub.users)
	}
	if !stub.users["shopfr@%"] {
		t.Errorf("le compte réel du projet adopté, 'shopfr'@'%%', est supprimé par l'apply")
	}
}

// Projet adopté, déclaration juste ('%'), appliqué : ogs scelle un mot de passe
// qu'il ne pose jamais sur le compte existant (CREATE USER IF NOT EXISTS).
// L'exploitant change l'hôte : le compte est « recréé avec le même mot de
// passe » — celui scellé, que l'application ignore —, puis l'original part.
func TestRelectureT3AdoptéHôteChangé(t *testing.T) {
	prov, proj := fixture(t)
	proj.Metadata.Labels = map[string]string{"adopted": "true"}
	decl := DefaultDatabase(proj, "mysql")
	decl.Host = "%"
	proj.Spec.Databases = []model.Database{decl}
	stub := vivant([]string{"shopfr"}, []string{"shopfr@%"},
		map[string]map[string]string{"shopfr": {"shopfr": "SELECT"}})
	withStub(prov, stub)
	if errs := appliquer(t, planifier(t, prov, proj)); len(errs) > 0 {
		t.Fatal(errs)
	}
	t.Logf("après adoption : scellé=%v exécutées=%v", prov.sealed(proj.ID(), "mysql", "shopfr"), stub.exécutées)
	stub.exécutées = nil
	proj.Spec.Databases[0].Host = "localhost"
	for i := 0; i < 2; i++ {
		errs := appliquer(t, planifier(t, prov, proj))
		t.Logf("apply %d : %v ; comptes %v", i, errs, stub.users)
	}
	t.Logf("exécutées : %v", stub.exécutées)
	if !stub.users["shopfr@%"] {
		t.Errorf("le compte réel du projet adopté est retiré par l'apply (D100 : jamais, --force compris)")
	}
}
