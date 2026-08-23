package db

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"strings"
	"testing"
)

// TestPostgresIntegration valide la syntaxe réellement acceptée par le serveur.
// Il crée puis supprime une base sur le PostgreSQL de la machine : il n'est donc
// exécuté que sur demande explicite, pour ne jamais modifier l'installation de
// quelqu'un à son insu.
//
//	OGS_TEST_POSTGRES=1 go test ./internal/provider/db/
func TestPostgresIntegration(t *testing.T) {
	if os.Getenv("OGS_TEST_POSTGRES") == "" {
		t.Skip("test d'intégration désactivé : exporter OGS_TEST_POSTGRES=1 pour l'exécuter " +
			"(il crée et supprime une base sur le serveur local)")
	}
	clusters := DetectClusters("")
	if len(clusters) == 0 {
		t.Skip("aucun cluster PostgreSQL détecté")
	}
	cluster := clusters[0]
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewPostgresClient(cluster, current.Username)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx := context.Background()
	endpoint, err := client.Endpoint(ctx)
	if err != nil {
		t.Skipf("cluster %s injoignable en tant que %s : %v", cluster.ID(), current.Username, err)
	}
	t.Logf("cluster %s, version %s, port %d", cluster.ID(), endpoint.Version, endpoint.Port)

	name := fmt.Sprintf("ogs_it_%d", os.Getpid())
	t.Cleanup(func() {
		stmts, _ := DropPostgres(name, "", true)
		for _, stmt := range stmts {
			_ = client.Exec(ctx, stmt)
		}
	})

	// Le rôle courant sert de propriétaire : créer un rôle exige un privilège que
	// l'utilisateur de test n'a pas nécessairement.
	stmts, err := CreatePostgres(PostgresSpec{
		Database: name, Role: current.Username, Extensions: []string{"pg_trgm"},
	}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range stmts.Cluster {
		if err := client.Exec(ctx, stmt); err != nil {
			t.Fatalf("requête refusée par le serveur :\n%s\n%v", stmt, err)
		}
	}
	databases, err := client.Databases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !databases[name] {
		t.Fatalf("la base %s n'apparaît pas dans pg_database", name)
	}

	// Durcissement du schéma public et extensions, connecté à la base cible.
	for _, stmt := range stmts.InDatabase {
		if err := client.ExecIn(ctx, name, stmt); err != nil {
			// Une extension peut exiger un privilège supplémentaire : on le signale
			// sans faire échouer la validation de syntaxe.
			if strings.Contains(stmt, "CREATE EXTENSION") {
				t.Logf("extension non installable avec les privilèges courants : %v", err)
				continue
			}
			t.Fatalf("requête refusée par le serveur :\n%s\n%v", stmt, err)
		}
	}

	if exts, err := client.Extensions(ctx, name); err == nil {
		t.Logf("extensions présentes : %v", keys(exts))
	}

	// Suppression : vérifie la clause WITH (FORCE) sur PostgreSQL 13 et au-delà.
	pg, ok := client.(*pgClient)
	if !ok {
		t.Fatal("client PostgreSQL attendu")
	}
	drops, err := DropPostgres(name, "", pg.SupportsForceDrop())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Exec(ctx, drops[0]); err != nil {
		t.Fatalf("suppression refusée :\n%s\n%v", drops[0], err)
	}
	databases, _ = client.Databases(ctx)
	if databases[name] {
		t.Error("la base devrait avoir été supprimée")
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
