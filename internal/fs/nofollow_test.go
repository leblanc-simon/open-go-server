package fs

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestSymlinkAtCreeLesRepertoiresEtPoseLeLien(t *testing.T) {
	root := t.TempDir()
	if err := SymlinkAt(root, ".local/bin", "php", "/usr/bin/php8.4", KeepOwner, false); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(root, ".local", "bin", "php"))
	if err != nil || target != "/usr/bin/php8.4" {
		t.Fatalf("lien attendu vers /usr/bin/php8.4, obtenu %q (%v)", target, err)
	}
	info, _ := os.Stat(filepath.Join(root, ".local", "bin"))
	if info.Mode().Perm() != 0o700 {
		t.Errorf("répertoire créé en %04o, 0700 attendu malgré l'umask", info.Mode().Perm())
	}
	kind, got, err := LinkStateAt(root, ".local/bin", "php")
	if err != nil || kind != EntrySymlink || got != "/usr/bin/php8.4" {
		t.Errorf("état inattendu : %v %q %v", kind, got, err)
	}
	// Le remplacement est atomique et ne laisse aucun temporaire derrière lui.
	if err := SymlinkAt(root, ".local/bin", "php", "/usr/bin/php8.5", KeepOwner, false); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, ".local", "bin"))
	if len(entries) != 1 {
		t.Errorf("un seul lien attendu, obtenu %v", entries)
	}
}

func TestUnComposantRemplaceParUnLienNEstJamaisSuivi(t *testing.T) {
	// L'utilisateur possède ~/.local : il peut y substituer un lien vers un
	// répertoire de son choix, et faire créer par root un fichier où il veut.
	root := t.TempDir()
	ailleurs := t.TempDir()
	if err := os.Symlink(ailleurs, filepath.Join(root, ".local")); err != nil {
		t.Fatal(err)
	}
	err := SymlinkAt(root, ".local/bin", "php", "/usr/bin/php8.4", KeepOwner, false)
	if !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("un composant en lien doit être refusé, obtenu %v", err)
	}
	if entries, _ := os.ReadDir(ailleurs); len(entries) != 0 {
		t.Errorf("root a écrit dans le répertoire désigné par le lien : %v", entries)
	}
	if err := RemoveLinkAt(root, ".local/bin", "php"); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("la suppression doit refuser le même détour, obtenu %v", err)
	}
	if _, _, err := LinkStateAt(root, ".local/bin", "php"); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("l'observation doit refuser le même détour, obtenu %v", err)
	}
}

func TestUnFichierDeLUtilisateurNEstPasRemplaceSansAccord(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".local", "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "php"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if kind, _, _ := LinkStateAt(root, ".local/bin", "php"); kind != EntryOther {
		t.Fatalf("un fichier régulier doit être vu comme EntryOther, obtenu %v", kind)
	}
	err := SymlinkAt(root, ".local/bin", "php", "/usr/bin/php8.4", KeepOwner, false)
	if err == nil || !strings.Contains(err.Error(), "refus") {
		t.Fatalf("refus attendu, obtenu %v", err)
	}
	if err := RemoveLinkAt(root, ".local/bin", "php"); err == nil {
		t.Error("un fichier régulier ne doit pas être supprimé comme un lien")
	}
	if err := SymlinkAt(root, ".local/bin", "php", "/usr/bin/php8.4", KeepOwner, true); err != nil {
		t.Fatalf("avec accord, le remplacement doit passer : %v", err)
	}
}

func TestRemoveEmptyDirAtLaisseCeQuiNEstPasAOgs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".bin")
	if err := os.MkdirAll(dir, 0o710); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"php", "node"} {
		if err := os.Symlink("/nulle/part", filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "outil"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"php", "node", "absent"} {
		if err := RemoveLinkAt(root, ".bin", n); err != nil {
			t.Fatal(err)
		}
	}
	remaining, err := RemoveEmptyDirAt(root, ".bin")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(remaining)
	if strings.Join(remaining, ",") != "outil" {
		t.Errorf("seul « outil » doit rester, obtenu %v", remaining)
	}
	if err := os.Remove(filepath.Join(dir, "outil")); err != nil {
		t.Fatal(err)
	}
	if remaining, err := RemoveEmptyDirAt(root, ".bin"); err != nil || len(remaining) != 0 {
		t.Fatalf("un répertoire vide doit partir : %v %v", remaining, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("le répertoire vide est resté")
	}
}
