package php

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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
		// Jokers : « 8.* » ne contraint que la majeure, « 7.4.* » la mineure. Le
		// point du joker était compté comme une composante, si bien que « 8.* »
		// exigeait une mineure nulle et refusait toutes les PHP 8 au-delà de 8.0.
		{"8.*", "8.0", true},
		{"8.*", "8.1", true},
		{"8.*", "8.3", true},
		{"8.*", "7.4", false},
		{"8.x", "8.3", true},
		{"7.4.*", "7.4", true},
		{"7.4.*", "8.1", false},
		{"8.1.*", "8.3", false},
		// Un « 8 » nu reste une version exacte, comme Composer le lit.
		{"8", "8.3", false},
		// Le tilde n'est pas concerné : « ~8.1 » borne la majeure, pas la mineure.
		{"~8.1", "8.4", true},
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

func TestCheck(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "www"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Symfony 3.4 : la bascule vers PHP 8 doit être refusée.
	composer := `{"require":{"php":"^5.5.9|>=7.0.8 <8.0"}}`
	if err := os.WriteFile(filepath.Join(root, "www", "composer.json"), []byte(composer), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := Check(root, "php-symfony", "8.3")
	ok, detail := v.OK, v.Detail
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("PHP 8.3 doit être refusé pour un Symfony 3.4")
	}
	if detail == "" {
		t.Error("le motif du refus doit être explicite")
	}
	if v, _ := Check(root, "php-symfony", "7.4"); !v.OK {
		t.Error("PHP 7.4 doit être accepté")
	}
}

func TestCheckFallsBackWithoutComposer(t *testing.T) {
	root := t.TempDir()
	v, err := Check(root, "php-prestashop", "8.3")
	ok, detail := v.OK, v.Detail
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("la table indicative doit refuser PHP 8.3 pour Prestashop (%s)", detail)
	}
	if v, _ := Check(root, "php-prestashop", "7.4"); !v.OK {
		t.Error("PHP 7.4 doit convenir à Prestashop")
	}
	// Type inconnu de la table : on n'invente pas de contrainte.
	if v, _ := Check(root, "static", "8.5"); !v.OK {
		t.Error("sans contrainte connue, la version doit être acceptée")
	}
}

func TestCheckToleratesBrokenComposer(t *testing.T) {
	// Un composer.json illisible ne doit pas bloquer l'exploitation : il produit
	// un avertissement, pas un refus.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "www"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "www", "composer.json"), []byte("{cassé"), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := Check(root, "php-symfony", "8.3")
	ok, detail := v.OK, v.Detail
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("un composer.json illisible ne doit pas bloquer : %s", detail)
	}
}

func TestLaContrainteNeSeLitPasATraversUnLien(t *testing.T) {
	// « php switch » lit composer.json en root, dans le www que l'utilisateur du
	// projet possède : un lien, sur le fichier ou sur www, faisait lire à root
	// n'importe quel fichier du système — et en afficher la contrainte.
	ailleurs := t.TempDir()
	composer := `{"require":{"php":"^5.6 SECRET"}}`
	if err := os.WriteFile(filepath.Join(ailleurs, "composer.json"), []byte(composer), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("composer.json lié", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "www"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(ailleurs, "composer.json"),
			filepath.Join(root, "www", "composer.json")); err != nil {
			t.Fatal(err)
		}
		c, _, err := Constraint(root)
		if err == nil || c != "" {
			t.Errorf("un composer.json lié doit être refusé : contrainte %q, erreur %v", c, err)
		}
	})
	t.Run("www lié", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Symlink(ailleurs, filepath.Join(root, "www")); err != nil {
			t.Fatal(err)
		}
		c, _, err := Constraint(root)
		if err == nil || c != "" {
			t.Errorf("un www lié doit être refusé : contrainte %q, erreur %v", c, err)
		}
		// Le refus ne bloque pas l'exploitation et n'affiche rien de la cible.
		v, err := Check(root, "php-symfony", "8.3")
		ok, detail := v.OK, v.Detail
		if err != nil || !ok || strings.Contains(detail, "SECRET") {
			t.Errorf("refus attendu en avertissement, sans rien de la cible : %v %q %v", ok, detail, err)
		}
	})
}

func TestUnComposerIllisibleDecideParLaTableIndicative(t *testing.T) {
	// Un composer.json illisible ou refusé passait sans contrôle, là où un
	// fichier absent consultait la table indicative : remplacer le fichier par un
	// lien suffisait à désarmer le garde-fou, et le verdict s'annonçait vérifié.
	cas := map[string]func(t *testing.T, www string){
		"lien": func(t *testing.T, www string) {
			permissif := filepath.Join(t.TempDir(), "composer.json")
			if err := os.WriteFile(permissif, []byte(`{"require":{"php":">=7.1"}}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(permissif, filepath.Join(www, "composer.json")); err != nil {
				t.Fatal(err)
			}
		},
		"cassé": func(t *testing.T, www string) {
			if err := os.WriteFile(filepath.Join(www, "composer.json"), []byte("{cassé"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		// Au-delà de la borne, le fichier n'est pas lu — creux, il ne coûterait
		// rien à l'utilisateur et root le lisait en entier. Valide et permissif,
		// il ne doit pas pour autant décider.
		"démesuré": func(t *testing.T, www string) {
			data := append([]byte(`{"require":{"php":">=7.1"}}`), bytes.Repeat([]byte(" "), maxComposerSize)...)
			if err := os.WriteFile(filepath.Join(www, "composer.json"), data, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for nom, prepare := range cas {
		t.Run(nom, func(t *testing.T) {
			root := t.TempDir()
			www := filepath.Join(root, "www")
			if err := os.MkdirAll(www, 0o755); err != nil {
				t.Fatal(err)
			}
			prepare(t, www)
			v, err := Check(root, "php-prestashop", "8.3")
			if err != nil {
				t.Fatal(err)
			}
			if v.OK || !strings.Contains(v.Detail, "table indicative") {
				t.Errorf("la table indicative doit refuser PHP 8.3 à un Prestashop : %+v", v)
			}
			if v.Unread == nil {
				t.Error("le verdict doit dire que composer.json n'a pas été lu")
			}
			if v, _ := Check(root, "php-prestashop", "8.1"); !v.OK || v.Unread == nil {
				t.Errorf("une version de la table passe, sans s'annoncer vérifiée sur l'application : %+v", v)
			}
		})
	}
}
