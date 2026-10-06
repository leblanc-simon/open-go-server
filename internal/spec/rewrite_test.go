package spec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteValuesNeToucheQueLesValeursVisees(t *testing.T) {
	src := `# déclaration de l'équipe
spec:
  crons:   # tâches
    - name: export
      command: "/h/.bin/php a.php"   # ancien /h/.bin/php
    - {name: b, command: '/h/.bin/php b.php; /h/.bin/php c.php'}
  services:
    - name: w
      params:
        command: /h/.bin/php w.php
        env.X: /h/.bin/php
  description: /h/.bin/php
`
	edits := []Edit{
		{Path: []any{"spec", "crons", 0, "command"}, Old: "/h/.bin/php a.php", New: "/h/.local/bin/php a.php"},
		{Path: []any{"spec", "crons", 1, "command"}, Old: "/h/.bin/php b.php; /h/.bin/php c.php",
			New: "/h/.local/bin/php b.php; /h/.local/bin/php c.php"},
		{Path: []any{"spec", "services", 0, "params", "command"}, Old: "/h/.bin/php w.php", New: "/h/.local/bin/php w.php"},
		{Path: []any{"spec", "services", 0, "params", "env.X"}, Old: "/h/.bin/php", New: "/h/.local/bin/php"},
	}
	out, err := RewriteValues([]byte(src), edits)
	if err != nil {
		t.Fatal(err)
	}
	want := `# déclaration de l'équipe
spec:
  crons:   # tâches
    - name: export
      command: "/h/.local/bin/php a.php"   # ancien /h/.bin/php
    - {name: b, command: '/h/.local/bin/php b.php; /h/.local/bin/php c.php'}
  services:
    - name: w
      params:
        command: /h/.local/bin/php w.php
        env.X: /h/.local/bin/php
  description: /h/.bin/php
`
	if string(out) != want {
		t.Errorf("obtenu :\n%s\nattendu :\n%s", out, want)
	}
}

func TestRewriteValuesRefuseCeQuIlNeSaitPasReecrireEnPlace(t *testing.T) {
	for name, src := range map[string]string{
		"bloc":     "spec:\n  app:\n    command: |\n      /h/.bin/php a.php\n",
		"repliée":  "spec:\n  app:\n    command: /usr/bin/env\n      /h/.bin/php a.php\n",
		"échappée": "spec:\n  app:\n    command: \"/h/\\x2ebin/php a.php\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			old, repl := "/h/.bin/php a.php", "/h/.local/bin/php a.php"
			if name == "repliée" {
				old, repl = "/usr/bin/env "+old, "/usr/bin/env "+repl
			}
			_, err := RewriteValues([]byte(src), []Edit{{Path: []any{"spec", "app", "command"}, Old: old,
				New: repl}})
			if !errors.Is(err, ErrManualEdit) {
				t.Errorf("réécriture à la main attendue, obtenu %v", err)
			}
		})
	}
}

func TestRewriteValuesRefuseUneValeurChangeeDepuisLePlan(t *testing.T) {
	src := "spec:\n  app:\n    command: /h/.bin/php autre.php\n"
	if _, err := RewriteValues([]byte(src), []Edit{{Path: []any{"spec", "app", "command"},
		Old: "/h/.bin/php a.php", New: "/h/.local/bin/php a.php"}}); err == nil {
		t.Error("une valeur changée depuis le plan doit être refusée")
	}
}

func TestWriteFileAtomicGardeLeModeEtRefuseUnLien(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.yaml")
	if err := os.WriteFile(path, []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("b\n")); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode().Perm())
	}
	link := filepath.Join(dir, "l.yaml")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(link, []byte("c\n")); err == nil {
		t.Error("un lien doit être refusé")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("aucun fichier temporaire ne doit rester : %d entrées", len(entries))
	}
}

func TestRewriteValuesGardeLaMarqueDOrdreEtNommeLaFusion(t *testing.T) {
	src := "\ufeffspec:\n  app:\n    command: /h/.bin/php a  # garde\n"
	out, err := RewriteValues([]byte(src), []Edit{{Path: []any{"spec", "app", "command"},
		Old: "/h/.bin/php a", New: "/h/.local/bin/php a"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "\ufeffspec:\n  app:\n    command: /h/.local/bin/php a  # garde\n"; string(out) != want {
		t.Errorf("obtenu %q", out)
	}
	merge := "base: &b {command: /h/.bin/php a}\nspec:\n  crons:\n    - <<: *b\n      name: e\n"
	_, err = RewriteValues([]byte(merge), []Edit{{Path: []any{"spec", "crons", 0, "command"},
		Old: "/h/.bin/php a", New: "/h/.local/bin/php a"}})
	if !errors.Is(err, ErrManualEdit) || !strings.Contains(err.Error(), "fusion") {
		t.Errorf("une valeur fusionnée doit être nommée comme telle, obtenu %v", err)
	}
}
