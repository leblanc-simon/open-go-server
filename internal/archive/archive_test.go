package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// Une arborescence de projet réaliste, avec un lien symbolique.
	root := filepath.Join(dir, "home", "shop.fr")
	for _, sub := range []string{"www/public", "var/log", "logs", ".bin"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "www", "public", "index.php"),
		[]byte("<?php echo 'bonjour';"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("APP_ENV=prod\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/php8.5", filepath.Join(root, ".bin", "php")); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "archives", FileName("shopfr", time.Date(2026, 8, 23, 14, 30, 0, 0, time.UTC)))
	w, err := Create(path, Manifest{Project: "shopfr", Host: "shop.fr", Env: "prod",
		Reason: "suppression du projet", OGSVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddBytes("spec", "project.yaml", "/etc/ogs/projects/shopfr.yaml",
		[]byte("apiVersion: ogs/v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.AddBytes("dump", "dumps/shopfr.sql", "", []byte("-- dump\nCREATE TABLE x;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.AddSecret("mysql/shopfr", []byte("nonce"), []byte("chiffré")); err != nil {
		t.Fatal(err)
	}
	if err := w.AddTree("tree", "tree", root); err != nil {
		t.Fatal(err)
	}
	w.Note("utilisateur shopfr")
	w.Note("base mysql shopfr")
	manifest, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}

	// L'archive est en 0600 : elle contient les données de l'application.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %04o, attendu 0600", info.Mode().Perm())
	}
	if manifest.Secrets != 1 || len(manifest.Removed) != 2 {
		t.Errorf("manifeste : %+v", manifest)
	}

	// Le manifeste est relisible sans extraire.
	read, err := ReadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if read.Project != "shopfr" || read.Version != Version || read.Reason == "" {
		t.Errorf("manifeste relu : %+v", read)
	}
	if len(read.Entries) < 3 {
		t.Errorf("entrées : %+v", read.Entries)
	}

	// Un fichier isolé est extractible.
	spec, err := ReadFile(path, "project.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(spec), "ogs/v1") {
		t.Errorf("contenu : %q", spec)
	}

	// L'arborescence se restaure à l'identique, liens compris.
	dest := filepath.Join(dir, "restore")
	count, err := Extract(path, "tree", dest)
	if err != nil {
		t.Fatal(err)
	}
	if count < 3 {
		t.Errorf("%d fichiers restaurés", count)
	}
	restored, err := os.ReadFile(filepath.Join(dest, "www", "public", "index.php"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), "bonjour") {
		t.Errorf("contenu restauré : %q", restored)
	}
	// Les permissions du .env sont conservées : c'est un fichier sensible.
	if info, err := os.Stat(filepath.Join(dest, ".env")); err != nil {
		t.Errorf(".env absent : %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf(".env restauré en %04o, attendu 0600", info.Mode().Perm())
	}
	target, err := os.Readlink(filepath.Join(dest, ".bin", "php"))
	if err != nil {
		t.Fatalf("lien non restauré : %v", err)
	}
	if target != "/usr/bin/php8.5" {
		t.Errorf("cible du lien : %q", target)
	}

	// Les secrets restent scellés.
	secrets, err := Secrets(path)
	if err != nil {
		t.Fatal(err)
	}
	pair, ok := secrets["mysql/shopfr"]
	if !ok {
		t.Fatalf("secret absent : %v", secrets)
	}
	if string(pair[0]) != "nonce" || string(pair[1]) != "chiffré" {
		t.Errorf("secret restitué : %q, %q", pair[0], pair[1])
	}
}

func TestCreateRefusesExistingArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.tar.zst")
	w, err := Create(path, Manifest{Project: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(path, Manifest{Project: "x"}); err == nil {
		t.Error("écraser une archive existante doit être refusé")
	}
}

func TestAbortLeavesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.tar.zst")
	w, err := Create(path, Manifest{Project: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddBytes("spec", "x.yaml", "", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.Abort()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("une archive interrompue ne doit pas subsister")
	}
}

func TestExtractRefusesPathTraversal(t *testing.T) {
	// Faille classique : une entrée d'archive nommée « ../../etc/passwd »
	// écrirait hors du répertoire cible.
	dir := t.TempDir()
	path := filepath.Join(dir, "piege.tar.zst")
	w, err := Create(path, Manifest{Project: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddBytes("tree", "tree/../../evasion.txt", "", []byte("dehors"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "dest")
	if _, err := Extract(path, "tree", dest); err == nil {
		t.Fatal("l'extraction devait être refusée")
	}
	if _, err := os.Stat(filepath.Join(dir, "evasion.txt")); !os.IsNotExist(err) {
		t.Error("aucun fichier ne doit avoir été écrit hors de la destination")
	}
}

func TestReadManifestOnTruncatedArchive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tronquee.tar.zst")
	w, err := Create(path, Manifest{Project: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddBytes("spec", "x.yaml", "", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Fermeture sans écrire le manifeste, comme le ferait une interruption.
	_ = w.tar.Close()
	_ = w.zstd.Close()
	_ = w.file.Close()
	if _, err := ReadManifest(path); err == nil {
		t.Error("une archive sans manifeste doit être signalée")
	}
}

func TestFileName(t *testing.T) {
	at := time.Date(2026, 8, 23, 14, 30, 5, 0, time.UTC)
	if got := FileName("shopfr", at); got != "shopfr-20260823-143005.tar.zst" {
		t.Errorf("FileName : %q", got)
	}
}

func TestAvailableSpace(t *testing.T) {
	// Archiver sans vérifier l'espace libre reviendrait à échouer au milieu d'une
	// suppression, après avoir déjà retiré des ressources.
	free, err := Available(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if free == 0 {
		t.Error("espace disponible nul : la mesure est-elle correcte ?")
	}
}
