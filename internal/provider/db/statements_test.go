package db

import (
	"strings"
	"testing"
)

func TestCreateMySQLEscapesGrantPattern(t *testing.T) {
	// Le point le plus dangereux du module : sans échappement, GRANT ... ON
	// `shopfr_analytics`.* porte aussi sur shopfrXanalytics.
	stmts, err := CreateMySQL(MySQLSpec{
		Database: "shopfr_analytics", User: "shopfr_ro", Password: "s3cr3t", Privileges: PrivReadOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	grant := stmts.Grants[0]
	if !strings.Contains(grant, "`shopfr\\_analytics`") {
		t.Errorf("underscore non échappé dans le motif :\n%s", grant)
	}
	if !strings.Contains(grant, "SELECT, SHOW VIEW") {
		t.Errorf("privilèges readonly attendus :\n%s", grant)
	}
	if strings.Contains(grant, "ALL PRIVILEGES") {
		t.Error("readonly ne doit jamais produire ALL PRIVILEGES")
	}
}

func TestGrantMySQLPrivilegeSets(t *testing.T) {
	cases := map[Privilege]string{
		PrivAll:      "ALL PRIVILEGES",
		PrivCRUD:     "SELECT, INSERT, UPDATE, DELETE",
		PrivReadOnly: "SELECT, SHOW VIEW",
	}
	for priv, want := range cases {
		got, err := GrantMySQL("shopfr", "shopfr", "localhost", priv)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, want) {
			t.Errorf("privilèges %s : %q", priv, got)
		}
		if !strings.Contains(got, "'shopfr'@'localhost'") {
			t.Errorf("compte mal formé : %q", got)
		}
	}
}

func TestCreateMySQLDefaults(t *testing.T) {
	stmts, err := CreateMySQL(MySQLSpec{Database: "shopfr", User: "shopfr", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	db := stmts.Database[0]
	if !strings.Contains(db, "IF NOT EXISTS") {
		t.Error("la création doit être idempotente")
	}
	if !strings.Contains(db, "utf8mb4") || !strings.Contains(db, "utf8mb4_unicode_ci") {
		t.Errorf("jeu de caractères par défaut : %q", db)
	}
	if !strings.Contains(stmts.User[0], "IF NOT EXISTS") {
		t.Error("la création d'utilisateur doit être idempotente")
	}
	if stmts.Grants[len(stmts.Grants)-1] != "FLUSH PRIVILEGES" {
		t.Error("les privilèges doivent être rechargés")
	}
}

func TestCreateMySQLAuthPlugin(t *testing.T) {
	// Utile pour un client tiers ancien qui accède à la même base.
	stmts, err := CreateMySQL(MySQLSpec{
		Database: "shopfr", User: "shopfr", Password: "x", AuthPlugin: "mysql_native_password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stmts.User[0], "IDENTIFIED WITH mysql_native_password BY") {
		t.Errorf("plugin non appliqué : %q", stmts.User[0])
	}
	// « auto » laisse le défaut du serveur, soit caching_sha2_password sur MySQL 8.
	stmts, _ = CreateMySQL(MySQLSpec{Database: "shopfr", User: "shopfr", Password: "x", AuthPlugin: "auto"})
	if strings.Contains(stmts.User[0], "IDENTIFIED WITH") {
		t.Errorf("« auto » ne doit pas imposer de plugin : %q", stmts.User[0])
	}
}

func TestPasswordLiteralIsQuoted(t *testing.T) {
	// Un mot de passe contenant une apostrophe ne doit pas casser la requête.
	stmt, err := SetMySQLPassword("shopfr", "localhost", "a'b", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stmt, "'a''b'") {
		t.Errorf("apostrophe non doublée : %q", stmt)
	}
}

func TestRejectsInjectionAttempts(t *testing.T) {
	// Les noms d'objets n'étant pas paramétrables en SQL, la liste blanche est la
	// seule défense : elle doit être appliquée à chaque génération.
	bad := []string{
		"shopfr`; DROP DATABASE mysql; --",
		"shop fr",
		"shop-fr",
		"shop.fr",
		"shopfr'",
		"",
		"shopfr; SELECT 1",
	}
	for _, name := range bad {
		if _, err := CreateMySQL(MySQLSpec{Database: name, User: "shopfr"}); err == nil {
			t.Errorf("nom de base accepté à tort : %q", name)
		}
		if _, err := CreateMySQL(MySQLSpec{Database: "shopfr", User: name}); err == nil {
			t.Errorf("nom d'utilisateur accepté à tort : %q", name)
		}
		if _, err := GrantMySQL(name, "shopfr", "localhost", PrivAll); err == nil {
			t.Errorf("GRANT accepté à tort sur %q", name)
		}
		if _, err := CreatePostgres(PostgresSpec{Database: name, Role: "shopfr"}, false, false); err == nil {
			t.Errorf("base PostgreSQL acceptée à tort : %q", name)
		}
	}
}

func TestMySQLUserLengthLimit(t *testing.T) {
	// MySQL 8 limite les noms d'utilisateur à 32 caractères : dépasser produit une
	// erreur lisible plutôt qu'un échec du serveur.
	long := strings.Repeat("a", 33)
	if _, err := CreateMySQL(MySQLSpec{Database: "shopfr", User: long}); err == nil {
		t.Error("un nom d'utilisateur de 33 caractères doit être refusé")
	}
	if _, err := CreateMySQL(MySQLSpec{Database: "shopfr", User: strings.Repeat("a", 32)}); err != nil {
		t.Errorf("32 caractères doivent être acceptés : %v", err)
	}
}

func TestDropMySQL(t *testing.T) {
	stmts, err := DropMySQL("shopfr", "shopfr", "localhost", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 2 {
		t.Fatalf("deux requêtes attendues : %v", stmts)
	}
	if !strings.HasPrefix(stmts[0], "DROP DATABASE IF EXISTS") {
		t.Errorf("suppression de base : %q", stmts[0])
	}
	if !strings.Contains(stmts[1], "DROP USER IF EXISTS 'shopfr'@'localhost'") {
		t.Errorf("suppression d'utilisateur : %q", stmts[1])
	}
}

func TestCreatePostgresSkipsExistingObjects(t *testing.T) {
	// PostgreSQL n'a pas de « IF NOT EXISTS » pour les rôles et les bases :
	// l'idempotence vient de l'observation préalable.
	spec := PostgresSpec{Database: "shopfr", Role: "shopfr", Password: "x"}
	full, err := CreatePostgres(spec, false, false)
	if err != nil {
		t.Fatal(err)
	}
	// Deux créations, plus le durcissement de la base — celui-ci étant idempotent,
	// il est rejoué dans tous les cas de figure.
	creations := func(stmts []string) []string {
		var out []string
		for _, s := range stmts {
			if strings.HasPrefix(s, "CREATE ") {
				out = append(out, s)
			}
		}
		return out
	}
	if len(creations(full.Cluster)) != 2 {
		t.Fatalf("rôle et base attendus : %v", full.Cluster)
	}
	partial, err := CreatePostgres(spec, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if c := creations(partial.Cluster); len(c) != 1 || !strings.Contains(c[0], "CREATE DATABASE") {
		t.Errorf("le rôle existant ne doit pas être recréé : %v", partial.Cluster)
	}
	none, err := CreatePostgres(spec, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(creations(none.Cluster)) != 0 {
		t.Errorf("rien à créer : %v", none.Cluster)
	}
	// Le durcissement de la base, lui, doit être présent même quand tout existe :
	// c'est ainsi qu'une base créée avant cette règle finit par s'y conformer.
	joined := strings.Join(none.Cluster, "\n")
	for _, want := range []string{"REVOKE CONNECT ON DATABASE", "GRANT CONNECT ON DATABASE"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q absent :\n%s", want, joined)
		}
	}
	// Le durcissement du schéma public s'applique dans tous les cas.
	if len(none.InDatabase) < 3 {
		t.Errorf("le durcissement du schéma public doit être appliqué : %v", none.InDatabase)
	}
}

func TestCreatePostgresHardensPublicSchema(t *testing.T) {
	stmts, err := CreatePostgres(PostgresSpec{Database: "shopfr", Role: "shopfr"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(stmts.InDatabase, "\n")
	for _, want := range []string{
		"REVOKE ALL ON SCHEMA public FROM PUBLIC",
		`GRANT ALL ON SCHEMA public TO "shopfr"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("durcissement incomplet, %q absent :\n%s", want, joined)
		}
	}
}

func TestCreatePostgresExtensionsAreDeterministic(t *testing.T) {
	spec := PostgresSpec{Database: "shopfr", Role: "shopfr", Extensions: []string{"pg_trgm", "uuid-ossp"}}
	first, err := CreatePostgres(spec, true, true)
	if err != nil {
		t.Fatal(err)
	}
	spec.Extensions = []string{"uuid-ossp", "pg_trgm"}
	second, err := CreatePostgres(spec, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(first.InDatabase, "|") != strings.Join(second.InDatabase, "|") {
		t.Error("l'ordre des extensions doit être indépendant de la déclaration")
	}
	if !strings.Contains(strings.Join(first.InDatabase, "\n"), `CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`) {
		t.Errorf("extension mal citée : %v", first.InDatabase)
	}
}

func TestCreatePostgresRejectsBadExtension(t *testing.T) {
	_, err := CreatePostgres(PostgresSpec{
		Database: "shopfr", Role: "shopfr", Extensions: []string{"x; DROP DATABASE shopfr"},
	}, true, true)
	if err == nil {
		t.Error("nom d'extension malveillant accepté")
	}
}

func TestCreatePostgresCollationRequiresTemplate0(t *testing.T) {
	stmts, err := CreatePostgres(PostgresSpec{
		Database: "shopfr", Role: "shopfr", Collate: "fr_FR.UTF-8",
	}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	create := stmts.Cluster[0]
	if !strings.Contains(create, "TEMPLATE template0") {
		t.Errorf("imposer une collation exige template0 :\n%s", create)
	}
}

func TestDropPostgresForceOnlyWhenSupported(t *testing.T) {
	with, err := DropPostgres("shopfr", "shopfr", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(with[0], "WITH (FORCE)") {
		t.Errorf("PostgreSQL 13+ accepte WITH (FORCE) : %q", with[0])
	}
	without, err := DropPostgres("shopfr", "shopfr", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(without[0], "FORCE") {
		t.Errorf("WITH (FORCE) n'existe pas avant PostgreSQL 13 : %q", without[0])
	}
}

func TestParsePrivilege(t *testing.T) {
	if p, err := ParsePrivilege(""); err != nil || p != PrivAll {
		t.Errorf("défaut = %q, %v", p, err)
	}
	if p, _ := ParsePrivilege("READONLY"); p != PrivReadOnly {
		t.Errorf("casse non normalisée : %q", p)
	}
	if _, err := ParsePrivilege("superuser"); err == nil {
		t.Error("jeu inconnu accepté")
	}
}

func TestPostgresExtraUserGrantsPresentAndFutureObjects(t *testing.T) {
	// Un compte additionnel doit voir les tables déjà créées et celles à venir :
	// sans privilèges par défaut, il devient aveugle à la première migration.
	for _, cas := range []struct {
		priv     Privilege
		attendu  string
		interdit string
	}{
		{PrivReadOnly, "GRANT SELECT ON ALL TABLES", "INSERT"},
		{PrivCRUD, "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES", "ALL PRIVILEGES ON ALL TABLES"},
		{PrivAll, "GRANT ALL PRIVILEGES ON ALL TABLES", ""},
	} {
		t.Run(string(cas.priv), func(t *testing.T) {
			stmts, err := PostgresExtraUser("shopfr", "shopfr", "lecture", "secret", cas.priv)
			if err != nil {
				t.Fatal(err)
			}
			cluster := strings.Join(stmts.Cluster, "\n")
			if !strings.Contains(cluster, `CREATE ROLE "lecture" LOGIN PASSWORD 'secret'`) {
				t.Errorf("rôle non créé :\n%s", cluster)
			}
			// Sans CONNECT explicite, le compte ne peut pas ouvrir la base : elle a
			// été fermée à PUBLIC.
			if !strings.Contains(cluster, `GRANT CONNECT ON DATABASE "shopfr" TO "lecture"`) {
				t.Errorf("CONNECT non accordé :\n%s", cluster)
			}
			inDB := strings.Join(stmts.InDatabase, "\n")
			for _, want := range []string{cas.attendu, "GRANT USAGE ON SCHEMA public",
				"ALTER DEFAULT PRIVILEGES FOR ROLE \"shopfr\" IN SCHEMA public"} {
				if !strings.Contains(inDB, want) {
					t.Errorf("%q absent :\n%s", want, inDB)
				}
			}
			if cas.interdit != "" && strings.Contains(inDB, cas.interdit) {
				t.Errorf("%q ne devrait pas apparaître :\n%s", cas.interdit, inDB)
			}
		})
	}
	if _, err := PostgresExtraUser("shopfr", "shopfr", "lecture", "x", "inconnu"); err == nil {
		t.Error("un jeu de privilèges inconnu doit être refusé")
	}
}

func TestCreatePostgresAlwaysUsesTemplate0(t *testing.T) {
	// L'encodage est toujours explicite : créer depuis template1 échoue dès que
	// celui-ci a un autre encodage, ce qui est le cas de tout cluster initialisé
	// sans locale.
	stmts, err := CreatePostgres(PostgresSpec{Database: "shopfr", Role: "shopfr"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var creation string
	for _, s := range stmts.Cluster {
		if strings.HasPrefix(s, "CREATE DATABASE") {
			creation = s
		}
	}
	if !strings.Contains(creation, "TEMPLATE template0") {
		t.Errorf("création sans template0 : %q", creation)
	}
	if !strings.Contains(creation, "ENCODING 'UTF8'") {
		t.Errorf("encodage absent : %q", creation)
	}
}

func TestDropRemovesExtraAccounts(t *testing.T) {
	// Un compte additionnel laissé derrière garderait la faculté de
	// s'authentifier, alors que le keystore ne conserve plus son mot de passe.
	mysql, err := DropMySQL("shopfr", "shopfr", "localhost", []string{"lecture", "integration"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(mysql, "\n")
	for _, want := range []string{"DROP USER IF EXISTS 'shopfr'@'localhost'",
		"DROP USER IF EXISTS 'lecture'@'localhost'",
		"DROP USER IF EXISTS 'integration'@'localhost'"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q absent :\n%s", want, joined)
		}
	}

	pg, err := DropPostgres("shopfr", "shopfr", []string{"lecture"}, true)
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(pg, "\n")
	// La base d'abord : les privilèges des rôles y vivent.
	if !strings.HasPrefix(pg[0], "DROP DATABASE") {
		t.Errorf("la base doit être supprimée en premier : %v", pg)
	}
	for _, want := range []string{`DROP ROLE IF EXISTS "shopfr"`, `DROP ROLE IF EXISTS "lecture"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q absent :\n%s", want, joined)
		}
	}
}

func TestMonitoringMySQLBorneLesPrivileges(t *testing.T) {
	stmts, err := MonitoringMySQL("telegraf", "localhost", "motdepasse")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(stmts, "\n")
	// Le compte lit l'état du serveur, jamais les données des projets : un agent
	// de métriques compromis ne doit pas être un accès aux bases de clients.
	if strings.Contains(joined, "ALL PRIVILEGES") {
		t.Errorf("privilèges trop larges :\n%s", joined)
	}
	for _, want := range []string{
		"GRANT PROCESS, REPLICATION CLIENT ON *.*",
		"GRANT SELECT ON `performance\\_schema`.*",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q absent :\n%s", want, joined)
		}
	}
	// Le nom de base placé après ON est un motif : non échappé, le privilège
	// porterait aussi sur « performanceXschema » — une base de projet adoptée
	// pourrait porter ce nom, et l'agent y lirait les données du client.
	if strings.Contains(joined, "ON `performance_schema`") {
		t.Errorf("le souligné n'est pas échappé, le GRANT est un motif :\n%s", joined)
	}
}

func TestLePrivilegeHeriteEstRevoqueAuMotifLitteral(t *testing.T) {
	// L'échappement ne vaut que pour les installations neuves : sur un serveur où
	// le GRANT non échappé a déjà tourné, la ligne joker de mysql.db survit à tous
	// les apply — GRANT est additif, seul un REVOKE la retire. Il vise le motif
	// **littéral**, celui qui a été stocké ; l'échapper ne désignerait rien.
	stmt, err := MonitoringMySQLLegacyRevoke("telegraf", "localhost")
	if err != nil {
		t.Fatal(err)
	}
	const want = "REVOKE SELECT ON `performance_schema`.* FROM 'telegraf'@'localhost'"
	if stmt != want {
		t.Errorf("requête = %q, attendu %q", stmt, want)
	}
	// Et le GRANT, lui, reste échappé : les deux motifs sont deux lignes
	// distinctes, et les confondre reviendrait à révoquer ce qu'on vient
	// d'accorder.
	stmts, err := MonitoringMySQL("telegraf", "localhost", "motdepasse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(stmts, "\n"), "GRANT SELECT ON `performance\\_schema`.*") {
		t.Errorf("le GRANT doit rester échappé :\n%s", strings.Join(stmts, "\n"))
	}
	if _, err := MonitoringMySQLLegacyRevoke("tele'graf", "localhost"); err == nil {
		t.Error("un nom d'utilisateur hors liste blanche doit être refusé")
	}
}

func TestPrivilegesObservesCompares(t *testing.T) {
	// Le passage d'un jeu à un autre doit se voir, et le retour d'un moteur ne
	// doit pas se voir : MariaDB et MySQL ne développent pas « ALL PRIVILEGES »
	// de la même façon, et exiger l'égalité rejouerait éternellement le GRANT.
	const allMySQL = "ALTER,CREATE,CREATE ROUTINE,CREATE TEMPORARY TABLES,CREATE VIEW," +
		"DELETE,DROP,EVENT,EXECUTE,INDEX,INSERT,LOCK TABLES,REFERENCES,SELECT," +
		"SHOW VIEW,TRIGGER,UPDATE"
	cases := []struct {
		nom       string
		observed  string
		priv      Privilege
		satisfait bool
		deborde   bool
	}{
		{"all déjà accordé", allMySQL, PrivAll, true, false},
		{"all sur MariaDB", allMySQL + ",DELETE HISTORY", PrivAll, true, false},
		{"réduction vers readonly", allMySQL, PrivReadOnly, false, true},
		{"réduction vers crud", allMySQL, PrivCRUD, false, true},
		{"readonly conforme", "SELECT,SHOW VIEW", PrivReadOnly, true, false},
		{"élargissement vers all", "SELECT,SHOW VIEW", PrivAll, false, false},
		{"aucun privilège", "", PrivAll, false, false},
	}
	for _, c := range cases {
		if got := MySQLGrantsSatisfied(c.observed, c.priv); got != c.satisfait {
			t.Errorf("%s : conformité %v, attendu %v", c.nom, got, c.satisfait)
		}
		if got := MySQLGrantsOverflow(c.observed, c.priv); got != c.deborde {
			t.Errorf("%s : débordement %v, attendu %v", c.nom, got, c.deborde)
		}
	}
}

func TestMonitoringMySQLRefuseUnNomHorsListeBlanche(t *testing.T) {
	if _, err := MonitoringMySQL("tele'graf", "localhost", "x"); err == nil {
		t.Error("un nom d'utilisateur hors liste blanche doit être refusé")
	}
}

func TestMonitoringPostgresNaPasDeMotDePasse(t *testing.T) {
	stmts, err := MonitoringPostgres("telegraf")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(stmts, "\n")
	// La connexion est locale et authentifiée « peer » : un mot de passe serait
	// un secret de plus à conserver, et à faire tourner, pour rien.
	if strings.Contains(strings.ToUpper(joined), "PASSWORD") {
		t.Errorf("le rôle de supervision ne doit pas porter de mot de passe :\n%s", joined)
	}
	if !strings.Contains(joined, "GRANT pg_monitor TO") {
		t.Errorf("pg_monitor est le rôle attendu :\n%s", joined)
	}
}
