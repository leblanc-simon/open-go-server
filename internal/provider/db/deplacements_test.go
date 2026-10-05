package db

import (
	"context"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

func TestLeDeroulageDUnSecondDeplacementRendLaMarque(t *testing.T) {
	// Hôte changé deux fois, le second apply déroulé : la compensation rend la
	// marque d'avant au lieu de l'effacer (D100). Elle prouve encore les comptes
	// du premier déplacement : revenu à « % », l'ancien compte sous localhost
	// part, et la suppression du projet emporte tout.
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
	if marque := prov.movedHosts(proj.ID(), "mysql", "shopfr"); len(marque) != 2 {
		t.Fatalf("la marque du premier déplacement doit revenir : %v", marque)
	}

	proj.Spec.Databases[0].Host = "%"
	for i := 0; i < 2; i++ {
		if errs := appliquer(t, planifier(t, prov, proj)); len(errs) > 0 {
			t.Fatalf("apply %d : %v", i, errs)
		}
	}
	if stub.users["shopfr@localhost"] || !stub.users["shopfr@%"] {
		t.Fatalf("seul 'shopfr'@'%%' doit rester : %v", stub.users)
	}
	obs, _ := prov.Observe(context.Background(), proj)
	for _, st := range obs.States {
		if kept, err := prov.Drop(context.Background(), st.Declaration, st); err != nil || len(kept) > 0 {
			t.Errorf("suppression : conservés %v, erreur %v", kept, err)
		}
	}
	if len(stub.users) != 0 {
		t.Errorf("la suppression du projet doit emporter ses comptes : %v", stub.users)
	}
}

func TestUnAdopteDeclareSousLocalhostGardeSonCompteReel(t *testing.T) {
	// Projet adopté d'un parc antérieur à D78 : déclaration « localhost », compte
	// réel 'shopfr'@'%', et un mot de passe scellé du temps où ogs créait
	// l'homonyme sous localhost. Le refus de D78 tient, apply après apply, et le
	// compte réel ne part jamais.
	prov, proj := fixture(t)
	adopter(proj)
	decl := DefaultDatabase(proj, "mysql")
	decl.Host = "localhost"
	proj.Spec.Databases = []model.Database{decl}
	sceller(t, prov, proj, "mysql", "shopfr")
	stub := vivant([]string{"shopfr"}, []string{"shopfr@%"},
		map[string]map[string]string{"shopfr": {"shopfr": tousDroits}})
	withStub(prov, stub)
	for i := 0; i < 2; i++ {
		errs := appliquer(t, planifier(t, prov, proj))
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), "existe déjà sous un autre hôte") {
			t.Errorf("apply %d : le refus de D78 est attendu : %v", i, errs)
		}
	}
	if !stub.users["shopfr@%"] || stub.users["shopfr@localhost"] {
		t.Errorf("le compte réel doit rester seul : %v", stub.users)
	}
}

func TestUnAdopteDontLHoteChangeGardeSonCompteReel(t *testing.T) {
	// Projet adopté, hôte juste (« % ») : l'apply ne scelle aucun mot de passe
	// (D111), puisqu'il n'en pose aucun. L'exploitant change ensuite l'hôte :
	// l'adopté ne déplace jamais son compte (D100), le refus de D78 tient, et le
	// compte réel reste.
	prov, proj := fixture(t)
	adopter(proj)
	decl := DefaultDatabase(proj, "mysql")
	decl.Host = "%"
	proj.Spec.Databases = []model.Database{decl}
	stub := vivant([]string{"shopfr"}, []string{"shopfr@%"},
		map[string]map[string]string{"shopfr": {"shopfr": "SELECT"}})
	withStub(prov, stub)
	if errs := appliquer(t, planifier(t, prov, proj)); len(errs) > 0 {
		t.Fatal(errs)
	}
	if prov.sealed(proj.ID(), "mysql", "shopfr") {
		t.Error("aucun mot de passe ne doit être scellé pour un compte adopté dont ogs n'a rien posé")
	}
	stub.exécutées = nil
	proj.Spec.Databases[0].Host = "localhost"
	for i := 0; i < 2; i++ {
		if errs := appliquer(t, planifier(t, prov, proj)); len(errs) != 1 {
			t.Errorf("apply %d : le refus de D78 est attendu : %v", i, errs)
		}
	}
	if len(stub.exécutées) > 0 || !stub.users["shopfr@%"] {
		t.Errorf("rien ne doit être exécuté, le compte réel doit rester : %v %v", stub.exécutées, stub.users)
	}
}
