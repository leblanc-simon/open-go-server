package php

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSatisfies(t *testing.T) {
	cases := []struct {
		constraint string
		version    string
		want       bool
	}{
		// Symfony 3.4 : ^5.5.9|>=7.0.8 — ne monte pas en PHP 8.
		{"^5.5.9|>=7.0.8 <8.0", "7.4", true},
		{"^5.5.9|>=7.0.8 <8.0", "8.0", false},
		{"^5.5.9|>=7.0.8 <8.0", "8.3", false},
		// Symfony 6.4.
		{">=8.1", "8.1", true},
		{">=8.1", "8.0", false},
		{">=8.1", "8.5", true},
		// Symfony 7.
		{">=8.2", "8.5", true},
		{">=8.2", "8.1", false},
		// Caret : reste dans la majeure.
		{"^8.1", "8.5", true},
		{"^8.1", "8.0", false},
		{"^8.1", "9.0", false},
		{"^7.4|^8.0", "8.3", true},
		{"^7.4|^8.0", "7.4", true},
		{"^7.4|^8.0", "7.2", false},
		// Tilde : reste dans la mineure quand elle est précisée.
		{"~8.1.0", "8.1", true},
		{"~8.1.0", "8.2", false},
		{"~8.1", "8.4", true},
		// Intervalle Prestashop.
		{">=7.1 <8.2", "8.1", true},
		{">=7.1 <8.2", "8.2", false},
		{">=7.1 <8.2", "7.4", true},
		// Cas dégénérés.
		{"*", "8.5", true},
		{"", "8.5", true},
		{"8.1", "8.1", true},
		{"8.1", "8.2", false},
		{"8.1.*", "8.1", true},
	}
	for _, c := range cases {
		got, err := Satisfies(c.constraint, c.version)
		if err != nil {
			t.Errorf("Satisfies(%q, %q) : %v", c.constraint, c.version, err)
			continue
		}
		if got != c.want {
			t.Errorf("Satisfies(%q, %q) = %v, attendu %v", c.constraint, c.version, got, c.want)
		}
	}
}

func TestConstraintFromComposer(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "www"), 0o755); err != nil {
		t.Fatal(err)
	}
	composer := `{"require":{"php":">=8.1","symfony/framework-bundle":"6.4.*"}}`
	if err := os.WriteFile(filepath.Join(root, "www", "composer.json"), []byte(composer), 0o644); err != nil {
		t.Fatal(err)
	}
	c, source, err := Constraint(root)
	if err != nil {
		t.Fatal(err)
	}
	if c != ">=8.1" {
		t.Errorf("contrainte %q", c)
	}
	if source == "" {
		t.Error("la source doit être indiquée pour que la décision soit vérifiable")
	}
}

func TestConstraintPlatformWins(t *testing.T) {
	// La plateforme configurée est celle sur laquelle les dépendances ont été
	// résolues : elle prime sur require.php.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "www"), 0o755); err != nil {
		t.Fatal(err)
	}
	composer := `{"require":{"php":">=7.4"},"config":{"platform":{"php":"8.1.0"}}}`
	if err := os.WriteFile(filepath.Join(root, "www", "composer.json"), []byte(composer), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _, err := Constraint(root)
	if err != nil {
		t.Fatal(err)
	}
	if c != "8.1.0" {
		t.Errorf("contrainte %q, attendue 8.1.0", c)
	}
}

func TestCheckCompatibility(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "www"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Symfony 3.4 : la bascule vers PHP 8 doit être refusée.
	composer := `{"require":{"php":"^5.5.9|>=7.0.8 <8.0"}}`
	if err := os.WriteFile(filepath.Join(root, "www", "composer.json"), []byte(composer), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, detail, err := CheckCompatibility(root, "php-symfony", "8.3")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("PHP 8.3 doit être refusé pour un Symfony 3.4")
	}
	if detail == "" {
		t.Error("le motif du refus doit être explicite")
	}
	if ok, _, _ := CheckCompatibility(root, "php-symfony", "7.4"); !ok {
		t.Error("PHP 7.4 doit être accepté")
	}
}

func TestCheckCompatibilityFallsBackWithoutComposer(t *testing.T) {
	root := t.TempDir()
	ok, detail, err := CheckCompatibility(root, "php-prestashop", "8.3")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("la table indicative doit refuser PHP 8.3 pour Prestashop (%s)", detail)
	}
	if ok, _, _ := CheckCompatibility(root, "php-prestashop", "7.4"); !ok {
		t.Error("PHP 7.4 doit convenir à Prestashop")
	}
	// Type inconnu de la table : on n'invente pas de contrainte.
	if ok, _, _ := CheckCompatibility(root, "static", "8.5"); !ok {
		t.Error("sans contrainte connue, la version doit être acceptée")
	}
}

func TestCheckCompatibilityToleratesBrokenComposer(t *testing.T) {
	// Un composer.json illisible ne doit pas bloquer l'exploitation : il produit
	// un avertissement, pas un refus.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "www"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "www", "composer.json"), []byte("{cassé"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, detail, err := CheckCompatibility(root, "php-symfony", "8.3")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("un composer.json illisible ne doit pas bloquer : %s", detail)
	}
}
