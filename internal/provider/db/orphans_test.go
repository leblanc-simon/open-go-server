package db

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
)

const tousDroits = "ALTER,CREATE,DELETE,DROP,INDEX,INSERT,SELECT,UPDATE"

var (
	reSupprimeCompte = regexp.MustCompile(`^DROP USER IF EXISTS '([^']+)'@'([^']+)'`)
	reCreeRole       = regexp.MustCompile(`^CREATE ROLE "([^"]+)"`)
	reSupprimeRole   = regexp.MustCompile(`^DROP ROLE IF EXISTS "([^"]+)"`)
	reProprietaire   = regexp.MustCompile(`^ALTER DATABASE "([^"]+)" OWNER TO "([^"]+)"`)
	reSupprimeBase   = regexp.MustCompile("^DROP DATABASE IF EXISTS [`\"]([^`\"]+)[`\"]")
)

// stubVivant est un moteur qui tient compte de ce qu'on lui fait : comptes créés
// et supprimés, propriété des bases PostgreSQL, requêtes passées dans une base.
type stubVivant struct {
	stubMoteur
	postgres  bool
	dansBases []string
}

func (c *stubVivant) Exec(ctx context.Context, stmt string) error {
	if err := c.stubMoteur.Exec(ctx, stmt); err != nil {
		return err
	}
	if m := reSupprimeBase.FindStringSubmatch(stmt); m != nil {
		delete(c.databases, m[1])
	}
	if m := reSupprimeCompte.FindStringSubmatch(stmt); m != nil {
		delete(c.users, m[1]+"@"+m[2])
	}
	if m := reCreeRole.FindStringSubmatch(stmt); m != nil {
		c.users[m[1]] = true
	}
	if m := reSupprimeRole.FindStringSubmatch(stmt); m != nil {
		delete(c.users, m[1])
	}
	if m := reProprietaire.FindStringSubmatch(stmt); m != nil {
		for _, g := range c.grantsPar {
			delete(g, m[1])
		}
		if c.grantsPar[m[2]] == nil {
			c.grantsPar[m[2]] = map[string]string{}
		}
		c.grantsPar[m[2]][m[1]] = "OWNER"
	}
	return nil
}

func (c *stubVivant) ExecIn(_ context.Context, database, stmt string) error {
	c.dansBases = append(c.dansBases, "["+database+"] "+stmt)
	return nil
}

func (c *stubVivant) Grants(_ context.Context, user, _ string) (map[string]string, error) {
	g := c.grantsPar[user]
	if g == nil {
		g = map[string]string{}
	}
	return g, c.fail("grants")
}

func vivant(databases, users []string, grants map[string]map[string]string) *stubVivant {
	s := &stubVivant{stubMoteur: stubMoteur{stubJournal: stubJournal{stubClient: stubClient{
		databases: map[string]bool{}, users: map[string]bool{}, grantsPar: grants,
	}}}}
	for _, d := range databases {
		s.databases[d] = true
	}
	for _, u := range users {
		s.users[u] = true
	}
	return s
}

// appliquer exécute un plan comme le moteur : dans l'ordre, une étape non
// critique en échec ne l'arrête pas.
func appliquer(t *testing.T, steps []*engine.Step) []error {
	t.Helper()
	var errs []error
	for _, s := range steps {
		if s.Apply == nil {
			continue
		}
		if err := s.Apply(context.Background()); err != nil {
			if s.Critical {
				t.Fatalf("%s : %v", s.Summary, err)
			}
			errs = append(errs, err)
		}
	}
	return errs
}

func planifier(t *testing.T, prov *Provider, proj *model.Project) []*engine.Step {
	t.Helper()
	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	return steps
}

func exécutée(stub *stubVivant, stmt string) bool {
	for _, q := range append(append([]string(nil), stub.exécutées...), stub.dansBases...) {
		if q == stmt {
			return true
		}
	}
	return false
}

func TestUnCompteAdditionnelRetireDeLaDeclarationEstRetire(t *testing.T) {
	// R1-30 : un compte additionnel retiré de spec.databases gardait ses droits et
	// sa faculté de se connecter, indéfiniment.
	t.Run("mysql", func(t *testing.T) {
		prov, proj := fixture(t)
		proj.Spec.Databases = []model.Database{DefaultDatabase(proj, "mysql")}
		sceller(t, prov, proj, "mysql", "shopfr")
		sceller(t, prov, proj, "mysql", "lecteur")
		stub := vivant([]string{"shopfr"}, []string{"shopfr@localhost", "lecteur@localhost"},
			map[string]map[string]string{"shopfr": {"shopfr": tousDroits}, "lecteur": {"shopfr": "SELECT"}})
		withStub(prov, stub)

		steps := planifier(t, prov, proj)
		var retrait *engine.Step
		for _, s := range steps {
			if s.Action == engine.Delete && strings.Contains(s.Summary, "lecteur") {
				retrait = s
			}
		}
		if retrait == nil || retrait.Critical {
			t.Fatalf("un retrait non critique de lecteur attendu : %+v", steps)
		}
		if errs := appliquer(t, steps); len(errs) > 0 {
			t.Fatal(errs)
		}
		if !exécutée(stub, "DROP USER IF EXISTS 'lecteur'@'localhost'") {
			t.Errorf("le compte doit être supprimé : %v", stub.exécutées)
		}
		if prov.sealed(proj.ID(), "mysql", "lecteur") {
			t.Error("son mot de passe doit quitter le keystore, après le retrait")
		}
		if !prov.sealed(proj.ID(), "mysql", "shopfr") {
			t.Error("le compte déclaré doit garder le sien")
		}
		// Rien ne reste à faire : le plan suivant ne le redemande pas.
		for _, s := range planifier(t, prov, proj) {
			if s.Mutates() {
				t.Errorf("plan suivant : %s", s.Summary)
			}
		}
	})
	t.Run("postgres", func(t *testing.T) {
		prov, proj := fixture(t)
		proj.Spec.Databases = []model.Database{DefaultDatabase(proj, "postgres")}
		sceller(t, prov, proj, "postgres", "shopfr")
		sceller(t, prov, proj, "postgres", "lecteur")
		stub := vivant([]string{"shopfr"}, []string{"shopfr", "lecteur"},
			map[string]map[string]string{"shopfr": {"shopfr": "OWNER"}})
		stub.postgres = true
		prov.clients = map[string]Client{"postgres": stub}

		if errs := appliquer(t, planifier(t, prov, proj)); len(errs) > 0 {
			t.Fatal(errs)
		}
		// Ses objets reviennent au propriétaire de la base avant le retrait :
		// DROP OWNED seul supprimerait les tables qu'il a créées.
		for _, want := range []string{
			`[shopfr] REASSIGN OWNED BY "lecteur" TO "shopfr"`,
			`[shopfr] DROP OWNED BY "lecteur"`,
			`DROP ROLE IF EXISTS "lecteur"`,
		} {
			if !exécutée(stub, want) {
				t.Errorf("%s attendu : %v %v", want, stub.exécutées, stub.dansBases)
			}
		}
		if prov.sealed(proj.ID(), "postgres", "lecteur") {
			t.Error("son mot de passe doit quitter le keystore")
		}
	})
}

func TestUnHoteChangeRecreeLeCompteEtRetireLAncien(t *testing.T) {
	// R1-30 : l'hôte d'un compte MySQL changé dans la déclaration faisait refuser
	// le nouveau compte comme masquant l'ancien — qui gardait ses droits pour
	// toujours, la suppression du projet ne visant que l'hôte déclaré.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	decl.Host = "%"
	proj.Spec.Databases = []model.Database{decl}
	sceller(t, prov, proj, "mysql", "shopfr")
	password, _ := prov.Password(proj.ID(), "mysql", "shopfr")
	stub := vivant([]string{"shopfr"}, []string{"shopfr@localhost"},
		map[string]map[string]string{"shopfr": {"shopfr": tousDroits}})
	withStub(prov, stub)

	steps := planifier(t, prov, proj)
	if errs := appliquer(t, steps); len(errs) > 0 {
		t.Fatalf("aucun refus attendu : %v", errs)
	}
	// L'apply qui crée le remplaçant garde l'ancien (D100) : une saga déroulée
	// plus loin retirerait le nouveau, et la base resterait sans compte.
	if !stub.users["shopfr@%"] || !stub.users["shopfr@localhost"] {
		t.Fatalf("premier apply : 'shopfr'@'%%' créé, l'ancien gardé : %v", stub.users)
	}
	// L'apply suivant observe le remplaçant, et retire l'ancien.
	if errs := appliquer(t, planifier(t, prov, proj)); len(errs) > 0 {
		t.Fatalf("aucun refus attendu : %v", errs)
	}
	if !stub.users["shopfr@%"] || stub.users["shopfr@localhost"] {
		t.Errorf("'shopfr'@'%%' doit remplacer 'shopfr'@'localhost' : %v", stub.users)
	}
	if len(prov.movedHosts(proj.ID(), "mysql", "shopfr")) > 0 {
		t.Error("la marque du déplacement part avec l'ancien compte")
	}
	if got, _ := prov.Password(proj.ID(), "mysql", "shopfr"); got != password {
		t.Error("le mot de passe de l'application ne doit pas changer avec l'hôte")
	}
	// L'ordre compte : l'ancien compte ne part qu'une fois le nouveau créé.
	créé, retiré := -1, -1
	for i, q := range stub.exécutées {
		if strings.HasPrefix(q, "CREATE USER IF NOT EXISTS 'shopfr'@'%'") {
			créé = i
		}
		if q == "DROP USER IF EXISTS 'shopfr'@'localhost'" {
			retiré = i
		}
	}
	if créé < 0 || retiré < créé {
		t.Errorf("création puis retrait attendus : %v", stub.exécutées)
	}

	// Plusieurs hôtes : rien ne dit lequel ogs a créé, le refus demeure.
	prov2, proj2 := fixture(t)
	proj2.Spec.Databases = []model.Database{decl}
	sceller(t, prov2, proj2, "mysql", "shopfr")
	stub2 := vivant([]string{"shopfr"}, []string{"shopfr@localhost", "shopfr@10.0.0.1"},
		map[string]map[string]string{"shopfr": {"shopfr": tousDroits}})
	withStub(prov2, stub2)
	if errs := appliquer(t, planifier(t, prov2, proj2)); len(errs) == 0 || len(stub2.exécutées) > 0 {
		t.Errorf("plusieurs hôtes : refus sans requête attendu : %v %v", errs, stub2.exécutées)
	}
}

func TestUnUtilisateurChangeRetireLAncienCompte(t *testing.T) {
	// R1-30 : spec.databases[].user changé faisait refuser la base comme
	// étrangère — l'ancien compte, seul à y avoir ses droits, gardait l'accès.
	t.Run("mysql", func(t *testing.T) {
		prov, proj := fixture(t)
		decl := DefaultDatabase(proj, "mysql")
		decl.User = "nouveau"
		proj.Spec.Databases = []model.Database{decl}
		sceller(t, prov, proj, "mysql", "shopfr")
		stub := vivant([]string{"shopfr"}, []string{"shopfr@localhost"},
			map[string]map[string]string{"shopfr": {"shopfr": tousDroits}})
		withStub(prov, stub)

		if errs := appliquer(t, planifier(t, prov, proj)); len(errs) > 0 {
			t.Fatal(errs)
		}
		if !stub.users["nouveau@localhost"] || !stub.users["shopfr@localhost"] {
			t.Fatalf("premier apply : le nouveau compte créé, l'ancien gardé (D100) : %v", stub.users)
		}
		if errs := appliquer(t, planifier(t, prov, proj)); len(errs) > 0 {
			t.Fatal(errs)
		}
		if !stub.users["nouveau@localhost"] || stub.users["shopfr@localhost"] {
			t.Errorf("le nouveau compte doit remplacer l'ancien : %v", stub.users)
		}
		if prov.sealed(proj.ID(), "mysql", "shopfr") || !prov.sealed(proj.ID(), "mysql", "nouveau") {
			t.Error("le secret de l'ancien compte part, celui du nouveau est scellé")
		}
	})
	t.Run("postgres", func(t *testing.T) {
		prov, proj := fixture(t)
		decl := DefaultDatabase(proj, "postgres")
		decl.User = "nouveau"
		proj.Spec.Databases = []model.Database{decl}
		sceller(t, prov, proj, "postgres", "shopfr")
		stub := vivant([]string{"shopfr"}, []string{"shopfr"},
			map[string]map[string]string{"shopfr": {"shopfr": "OWNER"}})
		prov.clients = map[string]Client{"postgres": stub}

		if errs := appliquer(t, planifier(t, prov, proj)); len(errs) > 0 {
			t.Fatal(errs)
		}
		if exécutée(stub, `DROP ROLE IF EXISTS "shopfr"`) {
			t.Fatal("premier apply : l'ancien rôle reste tant que le nouveau n'était pas observé (D100)")
		}
		// Le banc ne suit pas ALTER DATABASE … OWNER TO : le nouveau rôle possède
		// désormais la base.
		stub.grantsPar = map[string]map[string]string{"nouveau": {"shopfr": "OWNER"}}
		if errs := appliquer(t, planifier(t, prov, proj)); len(errs) > 0 {
			t.Fatal(errs)
		}
		for _, want := range []string{
			`ALTER DATABASE "shopfr" OWNER TO "nouveau"`,
			`[shopfr] REASSIGN OWNED BY "shopfr" TO "nouveau"`,
			`DROP ROLE IF EXISTS "shopfr"`,
		} {
			if !exécutée(stub, want) {
				t.Errorf("%s attendu : %v %v", want, stub.exécutées, stub.dansBases)
			}
		}
		if prov.sealed(proj.ID(), "postgres", "shopfr") {
			t.Error("le secret de l'ancien rôle doit partir")
		}
	})
}

func TestLAncienCompteResteTantQueSonRemplacantManque(t *testing.T) {
	// Le nouveau compte est refusé — un homonyme étranger existe : l'ancien est
	// le seul par lequel l'application atteint sa base, il ne doit pas partir.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	decl.User = "nouveau"
	proj.Spec.Databases = []model.Database{decl}
	sceller(t, prov, proj, "mysql", "shopfr")
	stub := vivant([]string{"shopfr"}, []string{"shopfr@localhost", "nouveau@localhost"},
		map[string]map[string]string{"shopfr": {"shopfr": tousDroits}})
	withStub(prov, stub)

	errs := appliquer(t, planifier(t, prov, proj))
	if len(errs) == 0 {
		t.Fatal("le refus du compte homonyme doit être rendu")
	}
	if !stub.users["shopfr@localhost"] || !prov.sealed(proj.ID(), "mysql", "shopfr") {
		t.Errorf("l'ancien compte et son secret doivent rester : %v", stub.exécutées)
	}
}

func TestUnCompteQuiPorteUneBaseRetireeResteEnPlace(t *testing.T) {
	// « db remove » retire la déclaration sans toucher aux données : le compte de
	// la base retirée est la seule preuve qu'ogs l'a créée. Le retirer la rendrait
	// étrangère à une redéclaration (D91).
	prov, proj := fixture(t)
	proj.Spec.Databases = []model.Database{DefaultDatabase(proj, "mysql")}
	sceller(t, prov, proj, "mysql", "shopfr")
	sceller(t, prov, proj, "mysql", "shopfr_api")
	stub := vivant([]string{"shopfr", "shopfr_api"}, []string{"shopfr@localhost", "shopfr_api@localhost"},
		map[string]map[string]string{"shopfr": {"shopfr": tousDroits}, "shopfr_api": {"shopfr_api": tousDroits}})
	withStub(prov, stub)

	steps := planifier(t, prov, proj)
	if errs := appliquer(t, steps); len(errs) > 0 {
		t.Fatal(errs)
	}
	if !stub.users["shopfr_api@localhost"] || !prov.sealed(proj.ID(), "mysql", "shopfr_api") {
		t.Error("le compte et son secret doivent rester")
	}
	annoncé := false
	for _, s := range steps {
		annoncé = annoncé || strings.Contains(s.Summary, "'shopfr_api'@'localhost' conservé")
	}
	if !annoncé {
		t.Errorf("le plan doit dire pourquoi le compte reste : %+v", steps)
	}
}

func TestUnCompteEmployeAilleursPerdSesDroitsSurLaBaseQuiLeRetire(t *testing.T) {
	// Un compte additionnel retiré d'une base mais déclaré sur une autre reste ;
	// ses droits sur la première partent.
	prov, proj := fixture(t)
	principal := DefaultDatabase(proj, "mysql")
	api := RoleDatabase(proj, "mysql", "api")
	api.ExtraUsers = []model.DBUser{{User: "lecteur", Privileges: "readonly"}}
	proj.Spec.Databases = []model.Database{principal, api}
	for _, u := range []string{"shopfr", "shopfr_api", "lecteur"} {
		sceller(t, prov, proj, "mysql", u)
	}
	stub := vivant([]string{"shopfr", "shopfr_api"},
		[]string{"shopfr@localhost", "shopfr_api@localhost", "lecteur@localhost"},
		map[string]map[string]string{
			"shopfr": {"shopfr": tousDroits}, "shopfr_api": {"shopfr_api": tousDroits},
			"lecteur": {"shopfr": "SELECT", "shopfr_api": "SELECT"},
		})
	withStub(prov, stub)

	if errs := appliquer(t, planifier(t, prov, proj)); len(errs) > 0 {
		t.Fatal(errs)
	}
	if !exécutée(stub, "REVOKE ALL PRIVILEGES ON `shopfr`.* FROM 'lecteur'@'localhost'") {
		t.Errorf("révocation attendue : %v", stub.exécutées)
	}
	if !stub.users["lecteur@localhost"] || !prov.sealed(proj.ID(), "mysql", "lecteur") {
		t.Error("le compte, employé par shopfr_api, doit rester")
	}
}

func TestLaSuppressionRetireLesComptesRetiresAuparavant(t *testing.T) {
	// R1-30 : la suppression du projet ne visait que les comptes encore
	// déclarés ; un compte additionnel retiré auparavant survivait au projet.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	proj.Spec.Databases = []model.Database{decl}
	for _, u := range []string{"shopfr", "lecteur", "fantome", "root"} {
		sceller(t, prov, proj, "mysql", u)
	}
	stub := vivant([]string{"shopfr"}, []string{"shopfr@localhost", "lecteur@localhost", "root@localhost"},
		map[string]map[string]string{"shopfr": {"shopfr": tousDroits}, "lecteur": {"shopfr": "SELECT"}})
	withStub(prov, stub)

	obs, _ := prov.Observe(context.Background(), proj)
	if _, err := prov.Drop(context.Background(), decl, obs.States[0]); err != nil {
		t.Fatal(err)
	}
	results := prov.DropOrphans(context.Background(), proj)
	sort := map[string]OrphanResult{}
	for _, r := range results {
		sort[r.Orphan.User] = r
	}
	if r := sort["lecteur"]; !r.Dropped || r.Kept != nil || stub.users["lecteur@localhost"] {
		t.Errorf("lecteur doit être supprimé : %+v", r)
	}
	if r := sort["fantome"]; !r.Forgotten {
		t.Errorf("le secret d'un compte disparu doit partir : %+v", r)
	}
	if r := sort["root"]; r.Kept == nil || !stub.users["root@localhost"] {
		t.Errorf("un compte réservé ne part jamais : %+v", r)
	}
	for _, u := range []string{"shopfr", "lecteur", "fantome"} {
		if prov.sealed(proj.ID(), "mysql", u) {
			t.Errorf("secret de %s resté au keystore", u)
		}
	}

	// Un moteur injoignable : le compte reste, son secret aussi — il prouve une
	// ressource encore présente.
	prov2, proj2 := fixture(t)
	sceller(t, prov2, proj2, "mysql", "lecteur")
	withStub(prov2, &stubClient{failOn: "users"})
	for _, r := range prov2.DropOrphans(context.Background(), proj2) {
		if r.Kept == nil || r.Dropped || r.Forgotten || !r.Unreachable {
			t.Errorf("moteur injoignable : rien ne doit partir : %+v", r)
		}
	}
	if !prov2.sealed(proj2.ID(), "mysql", "lecteur") {
		t.Error("le secret doit rester tant que le compte n'a pas été retiré")
	}
}

func TestLaSuppressionRetireAussiLeCompteSousLAncienHote(t *testing.T) {
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	decl.Host = "%"
	proj.Spec.Databases = []model.Database{decl}
	sceller(t, prov, proj, "mysql", "shopfr")
	stub := vivant([]string{"shopfr"}, []string{"shopfr@localhost"},
		map[string]map[string]string{"shopfr": {"shopfr": tousDroits}})
	withStub(prov, stub)
	obs, _ := prov.Observe(context.Background(), proj)
	if _, err := prov.Drop(context.Background(), decl, obs.States[0]); err != nil {
		t.Fatal(err)
	}
	if stub.users["shopfr@localhost"] || prov.sealed(proj.ID(), "mysql", "shopfr") {
		t.Errorf("le compte sous l'ancien hôte doit partir avec son secret : %v", stub.users)
	}
}

// Un projet adopté : un compte devenu orphelin — scellé par une rotation — ne
// part jamais par l'apply, --force compris (son --force reprend un fichier
// retouché), et seulement sous « project delete --force » (D77).
func TestUnCompteOrphelinDUnProjetAdoptéExigeForce(t *testing.T) {
	prov, proj := fixture(t)
	proj.Metadata.Labels = map[string]string{"adopted": "true"}
	proj.Spec.Databases = []model.Database{DefaultDatabase(proj, "mysql")}
	sceller(t, prov, proj, "mysql", "shopfr")
	sceller(t, prov, proj, "mysql", "lecteur")
	stub := vivant([]string{"shopfr"}, []string{"shopfr@localhost", "lecteur@localhost"},
		map[string]map[string]string{"shopfr": {"shopfr": tousDroits}, "lecteur": {"shopfr": "SELECT"}})
	withStub(prov, stub)
	for _, force := range []bool{false, true} {
		prov.Force = force
		steps := planifier(t, prov, proj)
		if errs := appliquer(t, steps); len(errs) > 0 {
			t.Fatal(errs)
		}
		named := false
		for _, s := range steps {
			named = named || strings.Contains(s.Summary, "compte d'un projet adopté")
		}
		if !named || !stub.users["lecteur@localhost"] {
			t.Fatalf("apply (force=%v) : le compte reste, nommé : %v", force, stub.users)
		}
	}
	prov.Force = false
	for _, r := range prov.DropOrphans(context.Background(), proj) {
		if r.Dropped || r.Kept == nil {
			t.Errorf("sans --force, la suppression garde le compte d'un projet adopté : %+v", r)
		}
	}
	if !stub.users["lecteur@localhost"] || !prov.sealed(proj.ID(), "mysql", "lecteur") {
		t.Fatalf("sans --force, compte et secret restent : %v", stub.users)
	}
	prov.Force = true
	prov.DropOrphans(context.Background(), proj)
	if stub.users["lecteur@localhost"] {
		t.Error("sous « project delete --force », le compte orphelin part")
	}
}
