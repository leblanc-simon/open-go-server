package db

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leviathan/ogs/internal/config"
	"github.com/leviathan/ogs/internal/exec"
	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/naming"
	"github.com/leviathan/ogs/internal/secret"
	"github.com/leviathan/ogs/internal/store"
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
