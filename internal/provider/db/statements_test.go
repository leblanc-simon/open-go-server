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
	stmts, err := DropMySQL("shopfr", "shopfr", "localhost")
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
	if len(full.Cluster) != 2 {
		t.Fatalf("rôle et base attendus : %v", full.Cluster)
	}
	partial, err := CreatePostgres(spec, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(partial.Cluster) != 1 || !strings.Contains(partial.Cluster[0], "CREATE DATABASE") {
		t.Errorf("le rôle existant ne doit pas être recréé : %v", partial.Cluster)
	}
	none, err := CreatePostgres(spec, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Cluster) != 0 {
		t.Errorf("rien à créer : %v", none.Cluster)
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
	with, err := DropPostgres("shopfr", "shopfr", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(with[0], "WITH (FORCE)") {
		t.Errorf("PostgreSQL 13+ accepte WITH (FORCE) : %q", with[0])
	}
	without, err := DropPostgres("shopfr", "shopfr", false)
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
