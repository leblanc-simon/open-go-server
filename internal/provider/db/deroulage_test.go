package db

import (
	"context"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

// D100 (revue du lot T) : l'utilisateur d'une base est renommé ; l'apply crée le nouveau
// compte, retire l'ancien (étape non critique, sans compensation), puis une étape
// critique plus loin (le vhost, par exemple) échoue et la saga se déroule : la
// compensation de l'étape de la base retire le compte qu'elle a créé.
func TestDéroulageAprèsRenommageGardeUnCompte(t *testing.T) {
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	decl.User = "nouveau"
	proj.Spec.Databases = []model.Database{decl}
	sceller(t, prov, proj, "mysql", "shopfr")
	stub := vivant([]string{"shopfr"}, []string{"shopfr@localhost"},
		map[string]map[string]string{"shopfr": {"shopfr": tousDroits}})
	withStub(prov, stub)

	steps := planifier(t, prov, proj)
	for _, s := range steps {
		t.Logf("étape %s critique=%v : %s", s.Resource, s.Critical, s.Summary)
	}
	if errs := appliquer(t, steps); len(errs) > 0 {
		t.Fatal(errs)
	}
	// Déroulage, comme le moteur : en ordre inverse.
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Compensate != nil {
			if err := steps[i].Compensate(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Logf("comptes après déroulage : %v ; secrets shopfr=%v nouveau=%v", stub.users,
		prov.sealed(proj.ID(), "mysql", "shopfr"), prov.sealed(proj.ID(), "mysql", "nouveau"))
	if !stub.users["shopfr@localhost"] && !stub.users["nouveau@localhost"] {
		t.Errorf("après déroulage, la base n'a plus aucun compte — ni l'ancien ni le nouveau")
	}
	// Et l'apply suivant : la base est-elle encore reconnue comme celle d'ogs ?
	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range obs.States {
		t.Logf("base %s : DBExists=%v DBOwned=%v", st.Declaration.Name, st.DBExists, st.DBOwned)
		if st.DBExists && !st.DBOwned {
			t.Errorf("la base d'ogs est désormais tenue pour étrangère")
		}
	}
	next, _ := prov.Plan(context.Background(), proj, obs)
	for _, s := range next {
		t.Logf("plan suivant : %s critique=%v : %s", s.Resource, s.Critical, s.Summary)
	}
}

func TestDéroulageAprèsChangementDHôteGardeUnCompte(t *testing.T) {
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	decl.Host = "%"
	proj.Spec.Databases = []model.Database{decl}
	sceller(t, prov, proj, "mysql", "shopfr")
	stub := vivant([]string{"shopfr"}, []string{"shopfr@localhost"},
		map[string]map[string]string{"shopfr": {"shopfr": tousDroits}})
	withStub(prov, stub)
	steps := planifier(t, prov, proj)
	if errs := appliquer(t, steps); len(errs) > 0 {
		t.Fatal(errs)
	}
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Compensate != nil {
			_ = steps[i].Compensate(context.Background())
		}
	}
	t.Logf("comptes après déroulage : %v", stub.users)
	if len(stub.users) == 0 {
		t.Errorf("changement d'hôte déroulé, plus aucun compte")
	}
}

// P1 (revue v2) : la même chose sous PostgreSQL. Le rôle d'avant possède la
// base ; l'apply crée le nouveau et lui donne la base. S'il retirait l'ancien
// dans la foulée, la compensation ne pourrait plus lui rendre la base.
func TestDéroulageAprèsRenommagePostgreSQLGardeLAncienRôle(t *testing.T) {
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "postgres")
	decl.User = "nouveau"
	proj.Spec.Databases = []model.Database{decl}
	sceller(t, prov, proj, "postgres", "shopfr")
	stub := vivant([]string{"shopfr"}, []string{"shopfr"},
		map[string]map[string]string{"shopfr": {"shopfr": "OWNER"}})
	stub.postgres = true
	prov.clients = map[string]Client{"postgres": stub}

	steps := planifier(t, prov, proj)
	for _, s := range steps {
		t.Logf("étape %s critique=%v : %s", s.Resource, s.Critical, s.Summary)
	}
	if errs := appliquer(t, steps); len(errs) > 0 {
		t.Fatal(errs)
	}
	if exécutée(stub, `DROP ROLE IF EXISTS "shopfr"`) {
		t.Fatal("le rôle d'avant est retiré dans l'apply qui crée son remplaçant")
	}
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Compensate != nil {
			if err := steps[i].Compensate(context.Background()); err != nil {
				t.Fatalf("compensation : %v", err)
			}
		}
	}
	if !stub.users["shopfr"] {
		t.Error("après déroulage, le rôle d'avant doit rester")
	}
	if !exécutée(stub, `ALTER DATABASE "shopfr" OWNER TO "shopfr"`) {
		t.Errorf("la compensation rend la base au rôle d'avant : %v", stub.exécutées)
	}
	if !prov.sealed(proj.ID(), "postgres", "shopfr") {
		t.Error("le secret du rôle d'avant reste")
	}
}

// P2 (revue v2) : hôte changé, puis « project delete » avant l'apply qui
// retire l'ancien. Le compte sous l'ancien hôte survivait, sans mot de passe
// scellé ni marque : un compte à droits sur une base supprimée, sans preuve.
func TestSuppressionEntreDeuxAppliesRetireLAncienHôte(t *testing.T) {
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
	if !stub.users["shopfr@%"] || !stub.users["shopfr@localhost"] || len(prov.movedHosts(proj.ID(), "mysql", "shopfr")) == 0 {
		t.Fatalf("prémisse : les deux comptes et la marque : %v", stub.users)
	}
	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range obs.States {
		if _, err := prov.Drop(context.Background(), st.Declaration, st); err != nil {
			t.Fatal(err)
		}
	}
	if stub.users["shopfr@localhost"] || stub.users["shopfr@%"] {
		t.Errorf("la suppression retire le compte sous les deux hôtes : %v", stub.users)
	}
	if len(prov.movedHosts(proj.ID(), "mysql", "shopfr")) > 0 || prov.sealed(proj.ID(), "mysql", "shopfr") {
		t.Error("la marque et le mot de passe partent avec les comptes")
	}
}

// P3 (revue v2) : retour à l'hôte d'origine avant le second apply. La marque
// désigne alors l'hôte déclaré : rien de lui ne doit partir, et la marque non
// plus ne doit pas rester à jamais.
func TestRetourALHôteDOrigineOublieLaMarque(t *testing.T) {
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
	proj.Spec.Databases[0].Host = "localhost"
	for i := 0; i < 2; i++ {
		errs := appliquer(t, planifier(t, prov, proj))
		t.Logf("apply %d : %v ; comptes %v", i, errs, stub.users)
	}
	if !stub.users["shopfr@localhost"] {
		t.Fatal("le compte sous l'hôte déclaré ne part jamais")
	}
	if stub.users["shopfr@%"] {
		t.Error("le compte posé sous l'hôte quitté, qu'ogs a créé, doit partir")
	}
	if len(prov.movedHosts(proj.ID(), "mysql", "shopfr")) > 0 {
		t.Error("la marque ne doit pas rester à jamais")
	}
}

// P3 (a) : l'hôte changé deux fois avant l'apply qui retire l'ancien. Les deux
// comptes d'avant sont ceux que la marque porte : le compte est recréé sous le
// troisième hôte, puis les deux anciens partent — sans le refus de D78.
func TestHôteChangéDeuxFoisAvantLeRetrait(t *testing.T) {
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
	if errs := appliquer(t, planifier(t, prov, proj)); len(errs) > 0 {
		t.Fatalf("le second changement ne doit pas être refusé : %v", errs)
	}
	if errs := appliquer(t, planifier(t, prov, proj)); len(errs) > 0 {
		t.Fatal(errs)
	}
	if !stub.users["shopfr@10.0.0.1"] || stub.users["shopfr@localhost"] || stub.users["shopfr@%"] {
		t.Errorf("seul le compte sous l'hôte déclaré doit rester : %v", stub.users)
	}
	if len(prov.movedHosts(proj.ID(), "mysql", "shopfr")) > 0 {
		t.Error("la marque part avec les anciens comptes")
	}
	// Un hôte que la marque ne porte pas reste le refus de D78.
	stub.users["shopfr@192.168.0.9"] = true
	proj.Spec.Databases[0].Host = "localhost"
	if errs := appliquer(t, planifier(t, prov, proj)); len(errs) == 0 {
		t.Error("deux autres hôtes dont un inconnu de la marque : refus attendu")
	}
}
