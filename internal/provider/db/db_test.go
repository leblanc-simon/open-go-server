package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	sceller(t, prov, proj, "mysql", "shopfr")
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
	sceller(t, prov, proj, "mysql", "shopfr")
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
	sceller(t, prov, proj, "mysql", "shopfr")
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
	sceller(t, prov, proj, "mysql", "shopfr")
	sceller(t, prov, proj, "mysql", "shopfr_ro")
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
	adopter(proj)
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
	adopter(proj)
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

// --- Appartenance des bases et des comptes (D91) ---

// sceller simule un compte créé par ogs : son mot de passe est scellé dans la
// portée du projet, comme le fait l'application de toute création.
func sceller(t *testing.T, prov *Provider, proj *model.Project, engine, user string) {
	t.Helper()
	if _, err := prov.ensurePassword(proj.ID(), engine, user); err != nil {
		t.Fatal(err)
	}
}

// adopter marque le projet comme repris d'un serveur existant (« ogs import »).
func adopter(proj *model.Project) {
	if proj.Metadata.Labels == nil {
		proj.Metadata.Labels = map[string]string{}
	}
	proj.Metadata.Labels["adopted"] = "true"
}

func TestCompensationNeSupprimePasUnCompteQuiExistaitDeja(t *testing.T) {
	// Deux bases du même projet partagent un compte : la seconde, neuve, échoue
	// plus loin dans la saga. Sa compensation supprimait aussi le compte — celui
	// dont vit la première base, en production.
	for _, engineName := range []string{"mysql", "postgres"} {
		prov, proj := fixture(t)
		decl := RoleDatabase(proj, engineName, "api")
		decl.User = proj.ID()
		decl.ExtraUsers = []model.DBUser{{User: "lecteur", Privileges: "readonly"}, {User: "neuf", Privileges: "readonly"}}
		proj.Spec.Databases = []model.Database{decl}
		sceller(t, prov, proj, engineName, proj.ID())
		sceller(t, prov, proj, engineName, "lecteur")
		users := map[string]bool{"shopfr@localhost": true, "lecteur@localhost": true}
		if engineName == "postgres" {
			users = map[string]bool{"shopfr": true, "lecteur": true}
		}
		stub := &stubJournal{stubClient: stubClient{
			databases: map[string]bool{"shopfr": true}, users: users,
			grants: map[string]string{"shopfr": "ALTER,CREATE,DELETE,DROP,INDEX,INSERT,SELECT,UPDATE"},
		}}
		prov.clients = map[string]Client{engineName: stub}

		obs, err := prov.Observe(context.Background(), proj)
		if err != nil {
			t.Fatal(err)
		}
		steps, err := prov.Plan(context.Background(), proj, obs)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 1 || steps[0].Action != engine.Create || steps[0].Compensate == nil {
			t.Fatalf("%s : une création compensable attendue : %+v", engineName, steps)
		}
		if err := steps[0].Compensate(context.Background()); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(stub.exécutées, "\n")
		if !strings.Contains(joined, "DROP DATABASE IF EXISTS") || !strings.Contains(joined, "shopfr_api") {
			t.Errorf("%s : la base créée doit être retirée :\n%s", engineName, joined)
		}
		for _, compte := range []string{"'shopfr'", `"shopfr"`, "'lecteur'", `"lecteur"`} {
			if strings.Contains(joined, "DROP USER IF EXISTS "+compte) || strings.Contains(joined, "DROP ROLE IF EXISTS "+compte) {
				t.Errorf("%s : la compensation supprime %s, qui existait avant l'étape :\n%s", engineName, compte, joined)
			}
		}
		if !strings.Contains(joined, "'neuf'") && !strings.Contains(joined, `"neuf"`) {
			t.Errorf("%s : le compte créé par l'étape doit être retiré :\n%s", engineName, joined)
		}
	}
}

func TestPlanRefuseUneBaseOuUnCompteQuOgsNAPasCrees(t *testing.T) {
	// Une base ou un compte homonyme préexistant était repris sans un mot — et
	// supprimé avec le projet. Il ne devient pas celui d'ogs en silence.
	cas := []struct {
		nom, engine string
		stub        stubClient
		attendu     string
	}{
		{"base mysql préexistante", "mysql", stubClient{databases: map[string]bool{"shopfr": true},
			users: map[string]bool{"root@localhost": true}}, "base shopfr"},
		{"compte mysql préexistant", "mysql", stubClient{databases: map[string]bool{},
			users: map[string]bool{"shopfr@localhost": true}}, "compte 'shopfr'@'localhost'"},
		{"base et compte mysql préexistants", "mysql", stubClient{databases: map[string]bool{"shopfr": true},
			users:  map[string]bool{"shopfr@localhost": true},
			grants: map[string]string{"shopfr": "ALTER,CREATE,DELETE,DROP,INDEX,INSERT,SELECT,UPDATE"}}, "base shopfr"},
		{"rôle postgres préexistant", "postgres", stubClient{databases: map[string]bool{},
			users: map[string]bool{"shopfr": true}}, `rôle shopfr`},
		{"base postgres préexistante", "postgres", stubClient{databases: map[string]bool{"shopfr": true},
			users: map[string]bool{}}, "base shopfr"},
	}
	for _, c := range cas {
		prov, proj := fixture(t)
		proj.Spec.Databases = []model.Database{DefaultDatabase(proj, c.engine)}
		stub := &stubJournal{stubClient: c.stub}
		prov.clients = map[string]Client{c.engine: stub}
		obs, err := prov.Observe(context.Background(), proj)
		if err != nil {
			t.Fatal(err)
		}
		steps, err := prov.Plan(context.Background(), proj, obs)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 1 || steps[0].Critical || steps[0].Action == engine.NoOp {
			t.Fatalf("%s : une étape non critique en échec attendue : %+v", c.nom, steps)
		}
		err = steps[0].Apply(context.Background())
		// L'import n'est pas une issue : il refuse un projet déjà déclaré.
		if err == nil || !strings.Contains(err.Error(), c.attendu) || !strings.Contains(err.Error(), "renommer") ||
			strings.Contains(err.Error(), "ogs import") {
			t.Errorf("%s : le refus doit nommer %s et les issues réelles : %v", c.nom, c.attendu, err)
		}
		if len(stub.exécutées) > 0 {
			t.Errorf("%s : rien ne doit être exécuté : %v", c.nom, stub.exécutées)
		}
		if _, err := prov.Password(proj.ID(), c.engine, "shopfr"); err == nil {
			t.Errorf("%s : aucun mot de passe ne doit être scellé", c.nom)
		}

		// Adopté, le projet reprend ses ressources : c'est l'adoption explicite.
		adopter(proj)
		if obs, err = prov.Observe(context.Background(), proj); err != nil {
			t.Fatal(err)
		}
		if steps, err = prov.Plan(context.Background(), proj, obs); err != nil {
			t.Fatal(err)
		}
		if len(steps) != 1 || (steps[0].Action != engine.NoOp && !steps[0].Critical) {
			t.Errorf("%s : un projet adopté ne doit pas être refusé : %+v", c.nom, steps)
		}
	}
}

func TestPlanReconnaitLesBasesQuOgsACreeesAvantD91(t *testing.T) {
	// Un parc existant ne porte aucune marque : ce qu'ogs y a créé se reconnaît
	// au mot de passe scellé du compte et aux droits que ce compte a sur la base.
	// Il doit passer « plan » sans écart.
	for _, engineName := range []string{"mysql", "postgres"} {
		prov, proj := fixture(t)
		decl := DefaultDatabase(proj, engineName)
		decl.ExtraUsers = []model.DBUser{{User: "lecteur", Privileges: "all"}}
		proj.Spec.Databases = []model.Database{decl}
		sceller(t, prov, proj, engineName, "shopfr")
		sceller(t, prov, proj, engineName, "lecteur")
		users := map[string]bool{"shopfr@localhost": true, "lecteur@localhost": true}
		if engineName == "postgres" {
			users = map[string]bool{"shopfr": true, "lecteur": true}
		}
		prov.clients = map[string]Client{engineName: &stubClient{
			databases: map[string]bool{"shopfr": true}, users: users,
			grants: map[string]string{"shopfr": "ALTER,CREATE,DELETE,DROP,INDEX,INSERT,SELECT,UPDATE"},
		}}
		obs, err := prov.Observe(context.Background(), proj)
		if err != nil {
			t.Fatal(err)
		}
		steps, err := prov.Plan(context.Background(), proj, obs)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 1 || steps[0].Action != engine.NoOp {
			t.Errorf("%s : aucun changement attendu : %+v", engineName, steps)
		}
	}
}

func TestDropNeDetruitQueCeQuiEstAOgs(t *testing.T) {
	// La suppression d'un projet non adopté ne détruit que ce qu'ogs a créé : une
	// base préexistante et un compte dont il n'a jamais tenu le mot de passe
	// restent en place, et sont nommés.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	decl.ExtraUsers = []model.DBUser{{User: "lecteur", Privileges: "readonly"}}
	proj.Spec.Databases = []model.Database{decl}
	sceller(t, prov, proj, "mysql", "lecteur")
	stub := &stubJournal{stubClient: stubClient{
		databases: map[string]bool{"shopfr": true},
		users:     map[string]bool{"shopfr@localhost": true, "lecteur@localhost": true},
		grantsPar: map[string]map[string]string{"lecteur": {"shopfr": "SELECT,SHOW VIEW"}},
	}}
	withStub(prov, stub)
	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := prov.Drop(context.Background(), decl, obs.States[0])
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(stub.exécutées, "\n")
	if strings.Contains(joined, "DROP DATABASE") || strings.Contains(joined, "'shopfr'@") {
		t.Errorf("la base et le compte préexistants ne doivent pas être supprimés :\n%s", joined)
	}
	if !strings.Contains(joined, "DROP USER IF EXISTS 'lecteur'@'localhost'") {
		t.Errorf("le compte créé par ogs doit partir :\n%s", joined)
	}
	if k := strings.Join(kept, " ; "); !strings.Contains(k, "base shopfr") || !strings.Contains(k, "'shopfr'@'localhost'") {
		t.Errorf("ce qui reste doit être nommé : %v", kept)
	}

	// Un projet adopté, lui, emporte tout ce qu'il déclare : c'est ce que
	// « project delete --force » a confirmé (D77).
	adopter(proj)
	stub.exécutées = nil
	if obs, err = prov.Observe(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	if kept, err = prov.Drop(context.Background(), decl, obs.States[0]); err != nil || len(kept) > 0 {
		t.Fatalf("rien ne doit rester : %v, %v", kept, err)
	}
	joined = strings.Join(stub.exécutées, "\n")
	if !strings.Contains(joined, "DROP DATABASE IF EXISTS `shopfr`") || !strings.Contains(joined, "'shopfr'@'localhost'") {
		t.Errorf("la base adoptée et son compte doivent partir :\n%s", joined)
	}
}

func TestRotationRefuseLesComptesDAdministration(t *testing.T) {
	// « db user rotate » sur root changeait le mot de passe du compte par lequel
	// ogs administre MariaDB — et le scellait comme un secret du projet.
	for _, c := range []struct{ engine, user string }{
		{"mariadb", "root"}, {"mysql", "mysql"}, {"postgres", "postgres"}, {"postgres", "pg_monitor"},
		{"mysql", "telegraf"},
	} {
		prov, proj := fixture(t)
		decl := DefaultDatabase(proj, c.engine)
		decl.ExtraUsers = []model.DBUser{{User: c.user, Privileges: "all"}}
		proj.Spec.Databases = []model.Database{decl}
		adopter(proj)
		stub := &stubJournal{}
		prov.clients = map[string]Client{c.engine: stub}
		if _, err := prov.RotatePassword(context.Background(), proj, decl, c.user); err == nil ||
			!strings.Contains(err.Error(), "réservé") {
			t.Errorf("%s %s : la rotation doit être refusée : %v", c.engine, c.user, err)
		}
		if len(stub.exécutées) > 0 {
			t.Errorf("%s %s : rien ne doit être exécuté : %v", c.engine, c.user, stub.exécutées)
		}
		if _, err := prov.Password(proj.ID(), c.engine, c.user); err == nil {
			t.Errorf("%s %s : aucun secret ne doit être scellé", c.engine, c.user)
		}
	}
}

func TestRotationNeReprendPasUnCompteEtranger(t *testing.T) {
	// Sur un projet non adopté, la rotation d'un compte dont ogs n'a jamais tenu
	// le mot de passe en ferait le sien sans adoption.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	proj.Spec.Databases = []model.Database{decl}
	stub := &stubJournal{}
	withStub(prov, stub)
	if _, err := prov.RotatePassword(context.Background(), proj, decl, "shopfr"); err == nil {
		t.Error("la rotation d'un compte étranger doit être refusée")
	}
	if len(stub.exécutées) > 0 {
		t.Errorf("rien ne doit être exécuté : %v", stub.exécutées)
	}
	sceller(t, prov, proj, "mysql", "shopfr")
	if _, err := prov.RotatePassword(context.Background(), proj, decl, "shopfr"); err != nil {
		t.Errorf("le compte d'ogs doit pouvoir changer de mot de passe : %v", err)
	}
}

// stubMoteur est un moteur MySQL qui tient l'état de ce qu'on lui fait créer, et
// qui refuse la première requête commençant par echoue — de quoi éprouver une
// création interrompue, puis le plan suivant.
type stubMoteur struct {
	stubJournal
	echoue string
}

var (
	reCreeBase   = regexp.MustCompile("^CREATE DATABASE IF NOT EXISTS `([^`]+)`")
	reCreeCompte = regexp.MustCompile(`^CREATE USER IF NOT EXISTS '([^']+)'@'([^']+)'`)
	reAccorde    = regexp.MustCompile("^GRANT .* ON `([^`]+)`\\.\\* TO '([^']+)'@")
)

func (c *stubMoteur) Exec(_ context.Context, stmt string) error {
	if c.echoue != "" && strings.HasPrefix(stmt, c.echoue) {
		c.echoue = ""
		return errors.New("refusé par le moteur")
	}
	c.exécutées = append(c.exécutées, stmt)
	if m := reCreeBase.FindStringSubmatch(stmt); m != nil {
		c.databases[m[1]] = true
	}
	if m := reCreeCompte.FindStringSubmatch(stmt); m != nil {
		c.users[m[1]+"@"+m[2]] = true
	}
	if m := reAccorde.FindStringSubmatch(stmt); m != nil {
		if c.grantsPar[m[2]] == nil {
			c.grantsPar[m[2]] = map[string]string{}
		}
		c.grantsPar[m[2]][strings.ReplaceAll(m[1], `\_`, "_")] = "ALTER,CREATE,DELETE,DROP,INDEX,INSERT,SELECT,UPDATE"
	}
	return nil
}

func TestUneCreationMySQLInterrompueSeRepareAuPlanSuivant(t *testing.T) {
	// La base était créée avant son compte et ses droits : un CREATE USER refusé
	// (plugin indisponible, politique de mots de passe) ou un GRANT en échec
	// laissait une base sans droits, qu'ogs tenait ensuite pour étrangère — et une
	// étape en échec n'est jamais compensée. Plus aucun apply ne réparait.
	for _, echec := range []string{"CREATE USER", "GRANT", "CREATE DATABASE"} {
		prov, proj := fixture(t)
		proj.Spec.Databases = []model.Database{DefaultDatabase(proj, "mysql")}
		stub := &stubMoteur{stubJournal: stubJournal{stubClient: stubClient{
			databases: map[string]bool{}, users: map[string]bool{"root@localhost": true},
			grantsPar: map[string]map[string]string{},
		}}, echoue: echec}
		withStub(prov, stub)

		obs, _ := prov.Observe(context.Background(), proj)
		steps, err := prov.Plan(context.Background(), proj, obs)
		if err != nil {
			t.Fatal(err)
		}
		if err := steps[0].Apply(context.Background()); err == nil {
			t.Fatalf("%s : l'échec simulé n'a pas eu lieu", echec)
		}
		obs, _ = prov.Observe(context.Background(), proj)
		if steps, err = prov.Plan(context.Background(), proj, obs); err != nil {
			t.Fatal(err)
		}
		if len(steps) != 1 || !steps[0].Critical || steps[0].Action == engine.NoOp {
			t.Fatalf("%s : le plan suivant doit reprendre la création, obtenu %+v", echec, steps)
		}
		if err := steps[0].Apply(context.Background()); err != nil {
			t.Fatalf("%s : la reprise doit réussir : %v", echec, err)
		}
		obs, _ = prov.Observe(context.Background(), proj)
		if st := obs.States[0]; !st.DBOwned || !st.UserOwned {
			t.Errorf("%s : base et compte doivent être à ogs après la reprise : %+v", echec, st)
		}
	}
}

func TestLesComptesReservesDejaDeclaresSontRefusesSansArreterLePlan(t *testing.T) {
	// Un projet d'avant D91 qui déclarait root se chargeait jusque-là : le refuser
	// au chargement arrêtait tout le parc. La base est refusée, rien n'est
	// exécuté ; un compte additionnel réservé ne fait refuser que lui.
	prov, proj := fixture(t)
	prov.Reserved = []string{"metriques"}
	decl := DefaultDatabase(proj, "mysql")
	decl.User = "root"
	proj.Spec.Databases = []model.Database{decl}
	sceller(t, prov, proj, "mysql", "root")
	stub := &stubJournal{stubClient: stubClient{
		databases: map[string]bool{"shopfr": true}, users: map[string]bool{"root@localhost": true},
		grants: map[string]string{"shopfr": "ALTER,CREATE,DELETE,DROP,INDEX,INSERT,SELECT,UPDATE"},
	}}
	withStub(prov, stub)
	obs, _ := prov.Observe(context.Background(), proj)
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Critical {
		t.Fatalf("une étape non critique attendue : %+v", steps)
	}
	if err := steps[0].Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "réservé") {
		t.Errorf("le refus doit dire le compte réservé : %v", err)
	}
	if len(stub.exécutées) > 0 {
		t.Errorf("rien ne doit être exécuté : %v", stub.exécutées)
	}

	// Compte additionnel réservé — celui que la configuration désigne : la base et
	// son compte principal restent réconciliés.
	decl = DefaultDatabase(proj, "mysql")
	decl.ExtraUsers = []model.DBUser{{User: "metriques", Privileges: "readonly"}}
	proj.Spec.Databases = []model.Database{decl}
	withStub(prov, &stubJournal{stubClient: stubClient{databases: map[string]bool{},
		users: map[string]bool{"root@localhost": true}}})
	obs, _ = prov.Observe(context.Background(), proj)
	if steps, err = prov.Plan(context.Background(), proj, obs); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[0].Critical || !steps[1].Critical || steps[1].Action != engine.Create {
		t.Fatalf("un refus pour le compte réservé et la création de la base attendus : %+v", steps)
	}
	if diff, _ := steps[1].Diff(); strings.Contains(diff, "metriques") {
		t.Errorf("la création ne doit pas viser le compte réservé :\n%s", diff)
	}
}

func TestDropNeSupprimeJamaisUnCompteReserve(t *testing.T) {
	// Un projet d'avant D91 qui déclarait root en tient le mot de passe scellé :
	// le compte passait pour être à ogs, et un projet adopté l'emportait sous
	// --force. DROP USER 'root'@'localhost' coupe ogs du moteur.
	for _, adopté := range []bool{false, true} {
		prov, proj := fixture(t)
		decl := DefaultDatabase(proj, "mariadb")
		decl.User = "root"
		decl.ExtraUsers = []model.DBUser{{User: "mysql", Privileges: "all"}}
		proj.Spec.Databases = []model.Database{decl}
		if adopté {
			adopter(proj)
		}
		sceller(t, prov, proj, "mariadb", "root")
		sceller(t, prov, proj, "mariadb", "mysql")
		stub := &stubJournal{stubClient: stubClient{
			databases: map[string]bool{"shopfr": true},
			users:     map[string]bool{"root@localhost": true, "mysql@localhost": true},
			grants:    map[string]string{"shopfr": "ALTER,CREATE,DELETE,DROP,INDEX,INSERT,SELECT,UPDATE"},
		}}
		prov.clients = map[string]Client{"mariadb": stub}
		obs, _ := prov.Observe(context.Background(), proj)
		kept, err := prov.Drop(context.Background(), decl, obs.States[0])
		if err != nil {
			t.Fatal(err)
		}
		if joined := strings.Join(stub.exécutées, "\n"); strings.Contains(joined, "DROP USER") {
			t.Errorf("adopté=%v : un compte réservé ne part jamais :\n%s", adopté, joined)
		}
		if k := strings.Join(kept, " ; "); !strings.Contains(k, "'root'@'localhost'") || !strings.Contains(k, "'mysql'@'localhost'") {
			t.Errorf("adopté=%v : les comptes réservés restés doivent être nommés : %v", adopté, kept)
		}
	}
}

func TestDropRetireLesMotsDePasseDesComptesSupprimes(t *testing.T) {
	// Laissé au keystore, le mot de passe d'un compte supprimé faisait d'un
	// homonyme recréé plus tard hors d'ogs le compte d'un projet du même nom.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	proj.Spec.Databases = []model.Database{decl}
	sceller(t, prov, proj, "mysql", "shopfr")
	withStub(prov, &stubJournal{stubClient: stubClient{
		databases: map[string]bool{"shopfr": true}, users: map[string]bool{"shopfr@localhost": true},
		grants: map[string]string{"shopfr": "ALTER,CREATE,DELETE,DROP,INDEX,INSERT,SELECT,UPDATE"},
	}})
	obs, _ := prov.Observe(context.Background(), proj)
	if _, err := prov.Drop(context.Background(), decl, obs.States[0]); err != nil {
		t.Fatal(err)
	}
	if prov.sealed(proj.ID(), "mysql", "shopfr") {
		t.Error("le mot de passe du compte supprimé doit quitter le keystore")
	}
}

func TestUnCompteAdditionnelEtrangerNeBloqueQueLui(t *testing.T) {
	// Les rôles additionnels PostgreSQL étaient observés sans être créés jusqu'à
	// leur prise en charge : l'exploitant les a créés à la main. Un seul d'entre
	// eux faisait refuser toute l'étape, et la dérive du compte principal n'était
	// plus corrigée.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "postgres")
	decl.Extensions = []string{"pg_trgm"}
	decl.ExtraUsers = []model.DBUser{{User: "lecteur", Privileges: "readonly"}}
	proj.Spec.Databases = []model.Database{decl}
	sceller(t, prov, proj, "postgres", "shopfr")
	stub := &stubJournal{stubClient: stubClient{
		databases: map[string]bool{"shopfr": true}, users: map[string]bool{"shopfr": true, "lecteur": true},
		grants: map[string]string{"shopfr": "OWNER"},
	}}
	prov.clients = map[string]Client{"postgres": stub}
	obs, _ := prov.Observe(context.Background(), proj)
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("deux étapes attendues, le refus du compte et la base : %+v", steps)
	}
	var refus, base *engine.Step
	for _, s := range steps {
		if strings.HasSuffix(s.Resource, "/lecteur") {
			refus = s
		} else {
			base = s
		}
	}
	if refus == nil || refus.Critical || base == nil || base.Action == engine.NoOp {
		t.Fatalf("le refus doit viser le seul compte, la base rester réconciliée : %+v", steps)
	}
	if err := refus.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "rôle lecteur") {
		t.Errorf("le refus doit nommer le rôle : %v", err)
	}
	if diff, _ := base.Diff(); !strings.Contains(diff, "pg_trgm") || strings.Contains(diff, "lecteur") {
		t.Errorf("la base doit être réconciliée sans toucher au rôle étranger :\n%s", diff)
	}
}

func TestDropRetireLeMotDePasseAuFilDesSuppressions(t *testing.T) {
	// Une suppression interrompue en route gardait les mots de passe de tous les
	// comptes, même de ceux déjà supprimés.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	decl.ExtraUsers = []model.DBUser{{User: "lecteur", Privileges: "readonly"}}
	proj.Spec.Databases = []model.Database{decl}
	sceller(t, prov, proj, "mysql", "shopfr")
	sceller(t, prov, proj, "mysql", "lecteur")
	stub := &stubMoteur{stubJournal: stubJournal{stubClient: stubClient{
		databases: map[string]bool{"shopfr": true},
		users:     map[string]bool{"shopfr@localhost": true, "lecteur@localhost": true},
		grants:    map[string]string{"shopfr": "ALTER,CREATE,DELETE,DROP,INDEX,INSERT,SELECT,UPDATE"},
		grantsPar: map[string]map[string]string{},
	}}, echoue: "DROP USER IF EXISTS 'lecteur'"}
	withStub(prov, stub)
	obs, _ := prov.Observe(context.Background(), proj)
	if _, err := prov.Drop(context.Background(), decl, obs.States[0]); err == nil {
		t.Fatal("l'échec simulé n'a pas eu lieu")
	}
	if prov.sealed(proj.ID(), "mysql", "shopfr") {
		t.Error("le mot de passe du compte supprimé doit quitter le keystore")
	}
	if !prov.sealed(proj.ID(), "mysql", "lecteur") {
		t.Error("le mot de passe du compte resté en place doit être gardé")
	}
}

// poserSocket crée un faux socket — un fichier suffit, seul l'emplacement et
// les droits sont jugés — dans un répertoire au mode donné.
func poserSocket(t *testing.T, path string, dirMode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), dirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o777); err != nil {
		t.Fatal(err)
	}
}

func postgresConf(t *testing.T, prefix string) {
	t.Helper()
	dir := filepath.Join(prefix, "etc", "postgresql", "16", "main")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "postgresql.conf"), []byte("port = 5432\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUnSocketDansTmpNEstJamaisRetenu(t *testing.T) {
	// Tout compte local pose un socket dans /tmp au nom qu'ogs cherchait : le
	// moteur arrêté, root s'y connectait et lui livrait les mots de passe du
	// parc (S1R-02).
	// Le répertoire est ici en 0755, à l'identité courante : checkSocket
	// l'admettrait. Seule l'absence de /tmp des emplacements cherchés protège.
	prefix := t.TempDir()
	poserSocket(t, filepath.Join(prefix, "tmp", "mysql.sock"), 0o755)
	poserSocket(t, filepath.Join(prefix, "tmp", ".s.PGSQL.5432"), 0o755)
	postgresConf(t, prefix)
	if socket, ok := FindMySQLSocket(prefix); ok {
		t.Errorf("DÉFAUT : socket MySQL retenu dans /tmp : %s", socket)
	}
	clusters := DetectClusters(prefix)
	if len(clusters) != 1 || clusters[0].Socket != "" {
		t.Errorf("DÉFAUT : socket PostgreSQL retenu dans /tmp : %+v", clusters)
	}
}

func TestUnSocketDansUnRepertoireOuvertATousEstRefuse(t *testing.T) {
	prov, proj := fixture(t)
	prefix := prov.Paths.Prefix()
	poserSocket(t, filepath.Join(prefix, "run", "mysqld", "mysqld.sock"), 0o777)
	poserSocket(t, filepath.Join(prefix, "var", "run", "postgresql", ".s.PGSQL.5432"), 0o1777)
	postgresConf(t, prefix)
	if socket, ok := FindMySQLSocket(prefix); ok {
		t.Errorf("socket retenu dans un répertoire inscriptible par tous : %s", socket)
	}
	_, err := prov.clientFor(DefaultDatabase(proj, "mysql"))
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "inscriptible par tous") {
		t.Errorf("l'indisponibilité doit nommer le refus : %v", err)
	}
	clusters := DetectClusters(prefix)
	if len(clusters) != 1 || clusters[0].Socket != "" {
		t.Fatalf("socket PostgreSQL retenu : %+v", clusters)
	}
	// Sans socket admis, ne pas retomber sur le répertoire par défaut : ce serait
	// s'y connecter quand même.
	if _, err := clusters[0].SocketDir(prefix); !errors.Is(err, ErrUnavailable) {
		t.Errorf("repli sur le répertoire du socket refusé : %v", err)
	}
	if _, err := prov.clientFor(DefaultDatabase(proj, "postgres")); !errors.Is(err, ErrUnavailable) {
		t.Errorf("client PostgreSQL ouvert sur un socket refusé : %v", err)
	}
}

func TestUnSocketDUnAutreCompteQueLeMoteurEstRefuse(t *testing.T) {
	prefix := t.TempDir()
	socket := filepath.Join(prefix, "run", "mysqld", "mysqld.sock")
	poserSocket(t, socket, 0o755)
	if _, ok := FindMySQLSocket(prefix); !ok {
		t.Fatal("un socket de l'identité courante doit être admis")
	}
	// L'identité courante n'est plus de confiance : le fichier est alors celui
	// d'un compte quelconque.
	orig := trustedIDs
	t.Cleanup(func() { trustedIDs = orig })
	trustedIDs = func(string) (map[uint32]bool, map[uint32]bool) {
		return map[uint32]bool{0: true}, map[uint32]bool{0: true}
	}
	if os.Getuid() == 0 {
		t.Skip("sous root, le fichier appartient à root")
	}
	if _, ok := FindMySQLSocket(prefix); ok {
		t.Error("DÉFAUT : socket d'un compte quelconque admis")
	}
}

func TestJugementDUnSocket(t *testing.T) {
	// Chaque motif de refus, sans changer de propriétaire : 111 est le moteur,
	// 1000 un compte de projet.
	uids := map[uint32]bool{0: true, 111: true}
	gids := map[uint32]bool{0: true, 111: true}
	sain := socketFacts{path: "/run/mysqld/mysqld.sock", dir: "/run/mysqld", uid: 111,
		dirUID: 111, dirGID: 111, dirPerm: 0o755}
	for _, c := range []struct {
		nom    string
		modif  func(*socketFacts)
		refuse string
	}{
		{"socket et répertoire du moteur", func(*socketFacts) {}, ""},
		{"répertoire Debian de PostgreSQL, 2775 au groupe du moteur", func(f *socketFacts) { f.dirPerm = 0o775 }, ""},
		{"répertoire de root", func(f *socketFacts) { f.dirUID, f.dirGID = 0, 0 }, ""},
		{"lien symbolique", func(f *socketFacts) { f.link = true }, "lien symbolique"},
		{"socket d'un compte de projet", func(f *socketFacts) { f.uid = 1000 }, "appartient à l'uid 1000"},
		{"répertoire d'un compte de projet", func(f *socketFacts) { f.dirUID = 1000 }, "répertoire /run/mysqld appartient"},
		{"répertoire inscriptible par tous", func(f *socketFacts) { f.dirPerm = 0o777 }, "inscriptible par tous"},
		{"sticky bit, comme /tmp", func(f *socketFacts) { f.dirPerm = 0o777 | os.ModeSticky }, "inscriptible par tous"},
		{"groupe étranger inscriptible", func(f *socketFacts) { f.dirPerm, f.dirGID = 0o775, 1000 }, "groupe 1000"},
		{"groupe étranger en lecture seule", func(f *socketFacts) { f.dirGID = 1000 }, ""},
	} {
		f := sain
		c.modif(&f)
		err := judgeSocket(f, "mysql", uids, gids)
		switch {
		case c.refuse == "" && err != nil:
			t.Errorf("%s : refusé à tort : %v", c.nom, err)
		case c.refuse != "" && (err == nil || !strings.Contains(err.Error(), c.refuse)):
			t.Errorf("%s : refus « %s » attendu, obtenu %v", c.nom, c.refuse, err)
		}
	}
}

func TestUnLienALaPlaceDuSocketEstRefuse(t *testing.T) {
	prefix := t.TempDir()
	vrai := filepath.Join(prefix, "ailleurs", "mysqld.sock")
	poserSocket(t, vrai, 0o755)
	lien := filepath.Join(prefix, "run", "mysqld", "mysqld.sock")
	if err := os.MkdirAll(filepath.Dir(lien), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(vrai, lien); err != nil {
		t.Fatal(err)
	}
	if _, err := MySQLSocket(prefix); err == nil || !strings.Contains(err.Error(), "lien symbolique") {
		t.Errorf("un lien à la place du socket doit être refusé, et dit : %v", err)
	}
}

func TestLEcouteTCPDoitAppartenirAuMoteur(t *testing.T) {
	// Moteur arrêté, ou n'écoutant que sur son socket : son port est à qui s'y
	// lie, et le mot de passe présenté par la restauration avec lui.
	dir := t.TempDir()
	orig := procNet
	t.Cleanup(func() { procNet = orig })
	procNet = dir
	origIDs := trustedIDs
	t.Cleanup(func() { trustedIDs = origIDs })
	trustedIDs = func(string) (map[uint32]bool, map[uint32]bool) {
		return map[uint32]bool{0: true, 111: true}, map[uint32]bool{0: true}
	}
	const head = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	ecrire := func(tcp, tcp6 string) {
		t.Helper()
		for name, body := range map[string]string{"tcp": head + tcp, "tcp6": head + tcp6} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 5432 = 0x1538.
	ecrire("   0: 0100007F:1538 00000000:0000 0A 00000000:00000000 00:00000000 00000000   111        0 1 1\n", "")
	if err := checkTCPListener(5432, "postgres", "127.0.0.1"); err != nil {
		t.Errorf("écoute du moteur refusée : %v", err)
	}
	ecrire("   0: 0100007F:1538 00000000:0000 0A 00000000:00000000 00:00000000 00000000 65534        0 1 1\n", "")
	if err := checkTCPListener(5432, "postgres", "127.0.0.1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("DÉFAUT : écoute d'un compte quelconque admise : %v", err)
	}
	ecrire("", "   0: 00000000000000000000000000000000:1538 00000000000000000000000000000000:0000 0A "+
		"00000000:00000000 00:00000000 00000000  1000        0 1 1\n")
	if err := checkTCPListener(5432, "postgres", "127.0.0.1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("DÉFAUT : écoute IPv6 joker d'un compte quelconque admise : %v", err)
	}
	// Une connexion établie n'est pas une écoute, et un autre port ne compte pas.
	ecrire("   0: 0100007F:1538 0100007F:9C40 01 00000000:00000000 00:00000000 00000000  1000        0 1 1\n"+
		"   1: 0100007F:0CEA 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 1 1\n", "")
	if err := checkTCPListener(5432, "postgres", "127.0.0.1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("port libre : refus attendu, obtenu %v", err)
	}
}

func TestLEcouteTCPEstJugeeSelonLaFamilleDeLaConnexion(t *testing.T) {
	// Une écoute du moteur sur ::1 ne reçoit pas 127.0.0.1 : ce port reste à qui
	// s'y lie entre le contrôle et la connexion.
	dir := t.TempDir()
	orig, origIDs, origV6 := procNet, trustedIDs, bindV6Only
	t.Cleanup(func() { procNet, trustedIDs, bindV6Only = orig, origIDs, origV6 })
	procNet = dir
	trustedIDs = func(string) (map[uint32]bool, map[uint32]bool) {
		return map[uint32]bool{0: true, 111: true}, map[uint32]bool{0: true}
	}
	bindV6Only = func() string { return "0" }
	const head = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	v6 := func(addr, uid string) string {
		return "   0: " + addr + ":0CEA 00000000000000000000000000000000:0000 0A 00000000:00000000 " +
			"00:00000000 00000000 " + uid + " 0 1 1\n"
	}
	for _, c := range []struct {
		nom, tcp6, user, target string
		admis                   bool
	}{
		{"::1 seul, connexion vers 127.0.0.1", v6(hexV6Loopback, "111"), "mysql", "127.0.0.1", false},
		{"::1 seul, connexion vers ::1", v6(hexV6Loopback, "111"), "mysql", "::1", true},
		{":: de MySQL, double pile", v6(hexV6Any, "111"), "mysql", "127.0.0.1", true},
		{":: de PostgreSQL, toujours V6ONLY", v6(hexV6Any, "111"), "postgres", "127.0.0.1", false},
		{":: étranger", v6(hexV6Any, "1000"), "mysql", "::1", false},
	} {
		for name, body := range map[string]string{"tcp": head, "tcp6": head + c.tcp6} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// 3306 = 0x0CEA.
		err := checkTCPListener(3306, c.user, c.target)
		if (err == nil) != c.admis {
			t.Errorf("%s : admis attendu %v, obtenu %v", c.nom, c.admis, err)
		}
	}
	bindV6Only = func() string { return "1" }
	if err := os.WriteFile(filepath.Join(dir, "tcp6"), []byte(head+v6(hexV6Any, "111")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkTCPListener(3306, "mysql", "127.0.0.1"); err == nil {
		t.Error("bindv6only=1 : « :: » ne reçoit pas l'IPv4")
	}
}

func TestPostgresRefuseUnJeuBorneSurLeComptePrincipal(t *testing.T) {
	// Le compte principal est propriétaire de la base : « readonly » y était
	// annoncé par le plan et sans effet — le compte supprimait les tables
	// (S1R-13). Le plan le refuse, sans rien changer à la base en place.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "postgres")
	decl.Privileges = "readonly"
	proj.Spec.Databases = []model.Database{decl}
	// Base et rôle créés par ogs (D91) : seul le jeu déclaré est en cause.
	obs := Observed{States: []DBState{{Declaration: decl, DBExists: true, UserExists: true,
		UserOwned: true, DBOwned: true,
		Grants: map[string]string{decl.Name: "OWNER"}, Extensions: map[string]bool{},
		ExtraUsers: map[string]bool{}, ExtraOwned: map[string]bool{}}}}
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("%d étapes", len(steps))
	}
	s := steps[0]
	if s.Action == engine.NoOp || s.Critical {
		t.Fatalf("DÉFAUT : un jeu borné sur le propriétaire doit être refusé, sans être critique : %+v", s)
	}
	if strings.Contains(s.Summary, "privilèges readonly") {
		t.Errorf("le plan annonce un jeu qu'il n'applique pas : %s", s.Summary)
	}
	if err := s.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "compte additionnel") {
		t.Errorf("le refus doit nommer la porte de sortie : %v", err)
	}
	// « all » reste conforme : rien ne change pour un site en place.
	decl.Privileges = "all"
	obs.States[0].Declaration = decl
	if steps, err = prov.Plan(context.Background(), proj, obs); err != nil || steps[0].Action != engine.NoOp {
		t.Errorf("base conforme en « all » : %v, %+v", err, steps)
	}
}
