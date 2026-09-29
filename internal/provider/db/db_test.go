package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/secret"
	"leblanc.io/open-go-server/internal/store"
)

func fixture(t *testing.T) (*Provider, *model.Project) {
	t.Helper()
	prefix := t.TempDir()
	paths := config.NewPaths(prefix)
	st, err := store.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	keyPath := filepath.Join(prefix, "master.key")
	if err := secret.GenerateKey(keyPath); err != nil {
		t.Fatal(err)
	}
	ks, err := secret.Open(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	p := model.New("shop.fr", naming.EnvProd, model.KindSymfony)
	p.Spec.App.Version = "6.4"
	if err := p.Normalize(filepath.Join(prefix, "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	return &Provider{Paths: paths, Exec: exec.NewRecorder(), Store: st, Keys: ks}, p
}

func TestEnsurePasswordIsStableAndSealed(t *testing.T) {
	prov, proj := fixture(t)
	first, err := prov.ensurePassword(proj.ID(), "mysql", "shopfr")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != secret.DefaultPasswordLength {
		t.Errorf("longueur du mot de passe : %d", len(first))
	}
	// Un mot de passe déjà émis ne doit jamais être régénéré : cela invaliderait
	// les configurations applicatives en place.
	second, err := prov.ensurePassword(proj.ID(), "mysql", "shopfr")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("le mot de passe doit être stable entre deux applications")
	}
	// Il est scellé, donc illisible dans la base d'état.
	_, ciphertext, err := prov.Store.GetSecret(proj.ID(), "mysql/shopfr")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), first) {
		t.Error("le mot de passe ne doit pas apparaître en clair dans la base")
	}
	// Et relisible via le keystore.
	got, err := prov.Password(proj.ID(), "mysql", "shopfr")
	if err != nil || got != first {
		t.Errorf("relecture : %q, %v", got, err)
	}
}

func TestEnsurePasswordIsolatesProjects(t *testing.T) {
	prov, proj := fixture(t)
	a, err := prov.ensurePassword(proj.ID(), "mysql", "shopfr")
	if err != nil {
		t.Fatal(err)
	}
	b, err := prov.ensurePassword("autrefr", "mysql", "autrefr")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("deux projets ne doivent pas partager un mot de passe")
	}
	// Le secret d'un projet n'est pas lisible depuis un autre : les données
	// associées du keystore lient le secret à son emplacement.
	if _, err := prov.Password("autrefr", "mysql", "shopfr"); err == nil {
		t.Error("un secret ne doit pas être lisible sous un autre nom")
	}
}

func TestObserveReportsUnavailableEngine(t *testing.T) {
	// Aucun serveur sous le préfixe : l'indisponibilité doit être constatée sans
	// faire échouer l'observation.
	prov, proj := fixture(t)
	proj.Spec.Databases = []model.Database{DefaultDatabase(proj, "mysql")}
	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatalf("l'observation ne doit pas échouer : %v", err)
	}
	if len(obs.States) != 1 || obs.States[0].Unavailable == nil {
		t.Fatalf("l'indisponibilité doit être rapportée : %+v", obs.States)
	}
	if !errors.Is(obs.States[0].Unavailable, ErrUnavailable) {
		t.Errorf("motif inattendu : %v", obs.States[0].Unavailable)
	}
	if !strings.Contains(obs.States[0].Unavailable.Error(), "installé et démarré") {
		t.Errorf("le message doit être actionnable : %v", obs.States[0].Unavailable)
	}
}

func TestPlanUnavailableEngineIsNonCritical(t *testing.T) {
	// Un moteur injoignable ne doit pas annuler la création du site : le site
	// existe, sa base viendra ensuite.
	prov, proj := fixture(t)
	proj.Spec.Databases = []model.Database{DefaultDatabase(proj, "mysql")}
	obs, _ := prov.Observe(context.Background(), proj)
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("%d étapes, 1 attendue", len(steps))
	}
	if steps[0].Critical {
		t.Error("l'étape doit être non critique")
	}
	if err := steps[0].Apply(context.Background()); err == nil {
		t.Error("l'application doit échouer avec le motif d'indisponibilité")
	}
}

// stubClient est un moteur simulé : il permet d'éprouver l'observation et la
// réconciliation sans serveur. Il est branché en peuplant le cache de clients,
// que clientFor consulte avant d'ouvrir quoi que ce soit.
type stubClient struct {
	databases map[string]bool
	users     map[string]bool
	grants    map[string]string
	// grantsPar distingue les comptes : les privilèges d'un compte additionnel ne
	// sont pas ceux du compte principal, et c'est justement l'écart entre les deux
	// qu'il faut pouvoir éprouver.
	grantsPar map[string]map[string]string
	failOn    string
}

func (c *stubClient) fail(what string) error {
	if c.failOn == what {
		return fmt.Errorf("%w : lecture interrompue", ErrUnavailable)
	}
	return nil
}

func (c *stubClient) Endpoint(context.Context) (Endpoint, error) {
	return Endpoint{Engine: "mysql", Host: "127.0.0.1", Port: 3306, Version: "8.0"}, c.fail("endpoint")
}

func (c *stubClient) Databases(context.Context) (map[string]bool, error) {
	return c.databases, c.fail("databases")
}

func (c *stubClient) Users(context.Context) (map[string]bool, error) {
	return c.users, c.fail("users")
}

func (c *stubClient) Grants(_ context.Context, user, _ string) (map[string]string, error) {
	if g, ok := c.grantsPar[user]; ok {
		return g, c.fail("grants")
	}
	return c.grants, c.fail("grants")
}

func (c *stubClient) Extensions(context.Context, string) (map[string]bool, error) {
	return map[string]bool{}, nil
}
func (c *stubClient) Exec(context.Context, string) error           { return nil }
func (c *stubClient) ExecIn(context.Context, string, string) error { return nil }
func (c *stubClient) Close() error                                 { return nil }

func withStub(p *Provider, c Client) { p.clients = map[string]Client{"mysql": c} }

func TestObserveNechouePasSurUneLectureInterrompue(t *testing.T) {
	// Un broker ou un moteur qui tombe entre deux requêtes rendait l'erreur à
	// l'appelant, qui abandonnait la construction du plan entier : ni utilisateur
	// système, ni vhost web, ni certificat — pour une cause que la conception
	// déclare non bloquante.
	for _, étape := range []string{"databases", "users"} {
		prov, proj := fixture(t)
		proj.Spec.Databases = []model.Database{DefaultDatabase(proj, "mysql")}
		withStub(prov, &stubClient{failOn: étape})

		obs, err := prov.Observe(context.Background(), proj)
		if err != nil {
			t.Fatalf("%s : l'observation ne doit pas échouer : %v", étape, err)
		}
		if len(obs.States) != 1 || obs.States[0].Unavailable == nil {
			t.Fatalf("%s : l'indisponibilité doit être rapportée : %+v", étape, obs.States)
		}
		steps, err := prov.Plan(context.Background(), proj, obs)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 1 || steps[0].Critical {
			t.Errorf("%s : une seule étape non critique attendue : %+v", étape, steps)
		}
	}
}

func TestPlanDetecteUneReductionDePrivileges(t *testing.T) {
	// Un passage de « all » à « readonly » doit produire une étape, et la
	// révocation doit précéder le grant : le GRANT de MySQL est additif, sans quoi
	// INSERT, UPDATE et DROP resteraient en place.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	decl.Privileges = string(PrivReadOnly)
	proj.Spec.Databases = []model.Database{decl}
	withStub(prov, &stubClient{
		databases: map[string]bool{"shopfr": true},
		users:     map[string]bool{"shopfr@localhost": true},
		grants:    map[string]string{"shopfr": "ALTER,CREATE,DELETE,DROP,INDEX,INSERT,SELECT,SHOW VIEW,UPDATE"},
	})

	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Action == engine.NoOp {
		t.Fatalf("une étape de correction attendue : %+v", steps)
	}
	diff, err := steps[0].Diff()
	if err != nil {
		t.Fatal(err)
	}
	revoke := strings.Index(diff, "REVOKE ALL PRIVILEGES ON `shopfr`.* FROM 'shopfr'@'localhost'")
	grant := strings.Index(diff, "GRANT SELECT, SHOW VIEW ON `shopfr`.* TO 'shopfr'@'localhost'")
	if revoke < 0 || grant < 0 || revoke > grant {
		t.Errorf("révocation puis grant attendus :\n%s", diff)
	}
}

func TestPlanNoscillePasSurUnJeuAllDejaAccorde(t *testing.T) {
	// L'inverse du défaut précédent : « all » se relit développé, et la liste
	// diffère d'un moteur à l'autre. Exiger l'égalité ferait annoncer
	// éternellement une correction qu'on vient d'appliquer, et un REVOKE avec.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	proj.Spec.Databases = []model.Database{decl}
	withStub(prov, &stubClient{
		databases: map[string]bool{"shopfr": true},
		users:     map[string]bool{"shopfr@localhost": true},
		grants: map[string]string{"shopfr": "ALTER,CREATE,CREATE ROUTINE,CREATE VIEW," +
			"DELETE,DELETE HISTORY,DROP,INDEX,INSERT,SELECT,SHOW VIEW,TRIGGER,UPDATE"},
	})

	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Action != engine.NoOp {
		t.Fatalf("aucun changement attendu : %+v", steps)
	}
}

func TestPlanDetecteUnElargissementDePrivileges(t *testing.T) {
	// Le sens le plus fréquent en exploitation : l'application se prenait des
	// « command denied » alors que le plan annonçait la conformité.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	proj.Spec.Databases = []model.Database{decl}
	withStub(prov, &stubClient{
		databases: map[string]bool{"shopfr": true},
		users:     map[string]bool{"shopfr@localhost": true},
		grants:    map[string]string{"shopfr": "SELECT,SHOW VIEW"},
	})

	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Action == engine.NoOp {
		t.Fatalf("une étape de correction attendue : %+v", steps)
	}
	diff, err := steps[0].Diff()
	if err != nil {
		t.Fatal(err)
	}
	// Aucune révocation : le jeu observé ne déborde pas « all », et un REVOKE sur
	// un compte qui n'a pas le privilège échouerait (ER_NONEXISTING_GRANT).
	if strings.Contains(diff, "REVOKE") {
		t.Errorf("une montée en privilèges ne doit rien révoquer :\n%s", diff)
	}
}

func TestPlanJugeAussiLesPrivilegesDesComptesAdditionnels(t *testing.T) {
	// Un compte additionnel n'était jugé que sur son existence : déclaré
	// « readonly » et créé jadis en « all », il gardait l'écriture sur la base du
	// projet et le plan annonçait la conformité. C'est le compte de lecture d'un
	// outil tiers — celui qu'on borne précisément parce qu'on ne le maîtrise pas.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	decl.ExtraUsers = []model.DBUser{{User: "shopfr_ro", Privileges: "readonly"}}
	proj.Spec.Databases = []model.Database{decl}
	stub := &stubClient{
		databases: map[string]bool{"shopfr": true},
		users:     map[string]bool{"shopfr@localhost": true, "shopfr_ro@localhost": true},
		grantsPar: map[string]map[string]string{
			"shopfr": {"shopfr": "ALTER,CREATE,DELETE,DROP,INDEX,INSERT,SELECT,SHOW VIEW,UPDATE"},
			"shopfr_ro": {"shopfr": "ALTER,CREATE,DELETE,DROP,INDEX,INSERT," +
				"SELECT,SHOW VIEW,UPDATE"},
		},
	}
	withStub(prov, stub)

	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Action == engine.NoOp {
		t.Fatalf("une étape de correction attendue : %+v", steps)
	}
	diff, err := steps[0].Diff()
	if err != nil {
		t.Fatal(err)
	}
	revoke := strings.Index(diff, "REVOKE ALL PRIVILEGES ON `shopfr`.* FROM 'shopfr_ro'@'localhost'")
	grant := strings.Index(diff, "GRANT SELECT, SHOW VIEW ON `shopfr`.* TO 'shopfr_ro'@'localhost'")
	if revoke < 0 || grant < 0 || revoke > grant {
		t.Errorf("révocation puis grant attendus sur le compte additionnel :\n%s", diff)
	}
	// Le compte principal, lui, est conforme : rien à révoquer de son côté.
	if strings.Contains(diff, "FROM 'shopfr'@'localhost'") {
		t.Errorf("le compte principal ne doit rien perdre :\n%s", diff)
	}

	// Une fois borné, le compte additionnel ne doit plus rien faire annoncer.
	stub.grantsPar["shopfr_ro"] = map[string]string{"shopfr": "SELECT,SHOW VIEW"}
	if obs, err = prov.Observe(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	if steps, err = prov.Plan(context.Background(), proj, obs); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Action != engine.NoOp {
		t.Errorf("aucun changement attendu : %+v", steps)
	}
}

// stubJournal retient ce que le provider exécute et les comptes, hôte compris,
// dont il relit les privilèges : c'est l'hôte réel d'un compte qui le désigne.
type stubJournal struct {
	stubClient
	exécutées []string
	relus     []string
}

func (c *stubJournal) Exec(_ context.Context, stmt string) error {
	c.exécutées = append(c.exécutées, stmt)
	return nil
}

func (c *stubJournal) Grants(ctx context.Context, user, host string) (map[string]string, error) {
	c.relus = append(c.relus, user+"@"+host)
	return c.stubClient.Grants(ctx, user, host)
}

func TestObserveRelitLeCompteSousSonHoteReel(t *testing.T) {
	// Un projet adopté déclare son compte sous l'hôte réel, 'shop'@'%'.
	// L'observation doit l'y retrouver — existence et privilèges —, et une
	// correction de privilèges viser ce compte-là, jamais un homonyme sous
	// l'hôte par défaut.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	decl.Name, decl.User, decl.Host = "shop", "shop", "%"
	proj.Spec.Databases = []model.Database{decl}
	stub := &stubJournal{stubClient: stubClient{
		databases: map[string]bool{"shop": true},
		users:     map[string]bool{"shop@%": true, "root@localhost": true},
		grants:    map[string]string{"shop": "ALTER,CREATE,DELETE,DROP,INDEX,INSERT,SELECT,UPDATE"},
	}}
	withStub(prov, stub)

	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	if st := obs.States[0]; !st.UserExists || len(st.OtherHosts) > 0 {
		t.Errorf("le compte 'shop'@'%%' doit être reconnu : %+v", st)
	}
	if len(stub.relus) != 1 || stub.relus[0] != "shop@%" {
		t.Errorf("privilèges relus pour %v, shop@%% attendu", stub.relus)
	}
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Action != engine.NoOp {
		t.Fatalf("aucun changement attendu : %+v", steps)
	}

	stub.grants = map[string]string{"shop": "SELECT"}
	if obs, err = prov.Observe(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	if steps, err = prov.Plan(context.Background(), proj, obs); err != nil {
		t.Fatal(err)
	}
	if err := steps[0].Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	accordé := false
	for _, q := range stub.exécutées {
		if strings.Contains(q, "'localhost'") {
			t.Errorf("requête visant un homonyme sous l'hôte par défaut : %s", q)
		}
		accordé = accordé || q == "GRANT ALL PRIVILEGES ON `shop`.* TO 'shop'@'%'"
	}
	if !accordé {
		t.Errorf("le GRANT doit viser 'shop'@'%%' : %v", stub.exécutées)
	}
}

func TestPlanNeMasquePasUnCompteSousUnAutreHote(t *testing.T) {
	// Une base adoptée avant que l'import ne garde l'hôte réel est déclarée sous
	// 'localhost', alors que son compte est 'shop'@'%'. L'apply créait
	// 'shop'@'localhost' au mot de passe tiré, que MySQL préfère pour toute
	// connexion locale : l'application perdait l'accès à sa base.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	decl.Name, decl.User = "shop", "shop"
	proj.Spec.Databases = []model.Database{decl}
	stub := &stubJournal{stubClient: stubClient{
		databases: map[string]bool{"shop": true},
		users:     map[string]bool{"shop@%": true, "root@localhost": true},
	}}
	withStub(prov, stub)

	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Critical {
		t.Fatalf("une étape non critique attendue : %+v", steps)
	}
	err = steps[0].Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "'shop'@'%'") ||
		!strings.Contains(err.Error(), "host") {
		t.Errorf("le refus doit nommer le compte existant et la porte de sortie : %v", err)
	}
	if len(stub.exécutées) > 0 {
		t.Errorf("rien ne doit être exécuté : %v", stub.exécutées)
	}
	if _, err := prov.Password(proj.ID(), "mysql", "shop"); err == nil {
		t.Error("aucun mot de passe ne doit être scellé pour un compte qu'on refuse de créer")
	}

	// Un compte additionnel suit la même règle.
	decl = DefaultDatabase(proj, "mysql")
	decl.ExtraUsers = []model.DBUser{{User: "lecteur", Privileges: "readonly"}}
	proj.Spec.Databases = []model.Database{decl}
	withStub(prov, &stubJournal{stubClient: stubClient{
		databases: map[string]bool{"shopfr": true},
		users:     map[string]bool{"shopfr@localhost": true, "lecteur@%": true},
		grants:    map[string]string{"shopfr": "ALTER,CREATE,DELETE,DROP,INDEX,INSERT,SELECT,UPDATE"},
	}})
	if obs, err = prov.Observe(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	if steps, err = prov.Plan(context.Background(), proj, obs); err != nil {
		t.Fatal(err)
	}
	if err := steps[0].Apply(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "'lecteur'@'%'") {
		t.Errorf("le compte additionnel masquerait 'lecteur'@'%%' : %v", err)
	}

	// Sans homonyme, le compte d'un projet neuf se crée comme avant.
	withStub(prov, &stubJournal{stubClient: stubClient{
		users: map[string]bool{"root@localhost": true},
	}})
	if obs, err = prov.Observe(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	if steps, err = prov.Plan(context.Background(), proj, obs); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Action != engine.Create || !steps[0].Critical {
		t.Errorf("une création critique attendue : %+v", steps)
	}
}

func TestStatementsForMySQLDeclaration(t *testing.T) {
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	decl.ExtraUsers = []model.DBUser{{User: "shopfr_ro", Privileges: "readonly"}}
	stmts, err := prov.statements(decl, DBState{}, PrivAll, "localhost", "secret")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(stmts, "\n")
	for _, want := range []string{
		"CREATE DATABASE IF NOT EXISTS `shopfr`",
		"CREATE USER IF NOT EXISTS 'shopfr'@'localhost'",
		"GRANT ALL PRIVILEGES ON `shopfr`.* TO 'shopfr'@'localhost'",
		// Le nom d'utilisateur est un littéral exact : il ne doit pas être échappé,
		// contrairement au nom de base côté ON, qui est un motif.
		"CREATE USER IF NOT EXISTS 'shopfr_ro'@'localhost'",
		"GRANT SELECT, SHOW VIEW ON `shopfr`.* TO 'shopfr_ro'@'localhost'",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("requête absente :\n  %s\ndans :\n%s", want, joined)
		}
	}
}

func TestStatementsForPostgresDeclaration(t *testing.T) {
	prov, proj := fixture(t)
	decl := RoleDatabase(proj, "postgres", "analytics")
	decl.Extensions = []string{"pg_trgm"}
	stmts, err := prov.statements(decl, DBState{}, PrivAll, "", "secret")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(stmts, "\n")
	for _, want := range []string{
		`CREATE ROLE "shopfr_analytics" LOGIN PASSWORD 'secret'`,
		`CREATE DATABASE "shopfr_analytics" OWNER "shopfr_analytics"`,
		`CREATE EXTENSION IF NOT EXISTS "pg_trgm"`,
		"REVOKE ALL ON SCHEMA public FROM PUBLIC",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("requête absente :\n  %s\ndans :\n%s", want, joined)
		}
	}
}

func TestDefaultAndRoleDatabaseNaming(t *testing.T) {
	_, proj := fixture(t)
	main := DefaultDatabase(proj, "mysql")
	if main.Name != "shopfr" || main.User != "shopfr" {
		t.Errorf("la base principale porte le nom nu du projet : %+v", main)
	}
	extra := RoleDatabase(proj, "postgres", "analytics")
	if extra.Name != "shopfr_analytics" || extra.User != "shopfr_analytics" {
		t.Errorf("une base additionnelle porte un suffixe de rôle : %+v", extra)
	}
}

func TestDetectClusters(t *testing.T) {
	prefix := t.TempDir()
	// Deux clusters, dont un sur un port non standard : s'y connecter sur 5432
	// viserait le mauvais serveur.
	for _, c := range []struct {
		version, name, conf string
	}{
		{"16", "main", "port = 5433\n"},
		{"17", "second", "#port = 5432\nport = 5434  # commentaire\n"},
	} {
		dir := filepath.Join(prefix, "etc", "postgresql", c.version, c.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "postgresql.conf"), []byte(c.conf), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	clusters := DetectClusters(prefix)
	if len(clusters) != 2 {
		t.Fatalf("%d clusters détectés : %+v", len(clusters), clusters)
	}
	if clusters[0].ID() != "16/main" || clusters[0].Port != 5433 {
		t.Errorf("premier cluster : %+v", clusters[0])
	}
	if clusters[1].ID() != "17/second" || clusters[1].Port != 5434 {
		t.Errorf("second cluster : %+v", clusters[1])
	}
}

func TestClientForUnknownCluster(t *testing.T) {
	prov, proj := fixture(t)
	dir := filepath.Join(prov.Paths.Prefix(), "etc", "postgresql", "16", "main")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "postgresql.conf"), []byte("port = 5432\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	decl := DefaultDatabase(proj, "postgres")
	decl.Cluster = "15/main"
	_, err := prov.clientFor(decl)
	if err == nil || !strings.Contains(err.Error(), "16/main") {
		t.Errorf("l'erreur doit énumérer les clusters détectés : %v", err)
	}
}

func TestErrorsDoNotLeakPasswords(t *testing.T) {
	// Une erreur de moteur ne doit jamais reproduire la requête complète : elle
	// contiendrait le mot de passe en clair, qui finirait dans les journaux et
	// dans l'audit (§15).
	stmt, err := SetMySQLPassword("shopfr", "localhost", "tres-secret-42", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stmt, "tres-secret-42") {
		t.Fatal("la requête doit bien contenir le mot de passe")
	}
	if got := firstWords(stmt); strings.Contains(got, "tres-secret-42") {
		t.Errorf("le résumé de requête laisse fuir le secret : %q", got)
	}
	create, err := CreateMySQL(MySQLSpec{Database: "shopfr", User: "shopfr", Password: "tres-secret-42"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range create.All() {
		if got := firstWords(s); strings.Contains(got, "tres-secret-42") {
			t.Errorf("le résumé de requête laisse fuir le secret : %q", got)
		}
	}
}

func TestDescribeMasksPassword(t *testing.T) {
	// Le plan est affiché et journalisé : il ne doit contenir aucun secret.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	password, err := prov.ensurePassword(proj.ID(), "mysql", decl.User)
	if err != nil {
		t.Fatal(err)
	}
	out, err := prov.describe(decl, DBState{}, PrivAll, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, password) {
		t.Errorf("le plan expose le mot de passe :\n%s", out)
	}
	if !strings.Contains(out, "mot de passe généré") {
		t.Errorf("le plan doit signaler la présence d'un secret sans le révéler :\n%s", out)
	}
}
