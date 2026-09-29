package model

import (
	"path"
	"strings"
	"testing"
)

func TestResolveSymfonyVariants(t *testing.T) {
	legacy, err := Resolve(KindSymfony, "3.4")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Docroot != "www/web" || legacy.FrontController != "app.php" || legacy.EnvVar != "SYMFONY_ENV" {
		t.Errorf("Symfony 3.4 mal résolu : %+v", legacy)
	}
	for _, v := range []string{"4.4", "5.4", "6.4", "7.2", "8.0"} {
		modern, err := Resolve(KindSymfony, v)
		if err != nil {
			t.Fatal(err)
		}
		if modern.Docroot != "www/public" || modern.FrontController != "index.php" || modern.EnvVar != "APP_ENV" {
			t.Errorf("Symfony %s mal résolu : %+v", v, modern)
		}
	}
}

func TestResolvePrestashop(t *testing.T) {
	ps, err := Resolve(KindPrestashop, "8")
	if err != nil {
		t.Fatal(err)
	}
	if ps.Docroot != "www" {
		t.Errorf("Prestashop doit servir www, obtenu %q", ps.Docroot)
	}
	if !ps.NeedsPHP {
		t.Error("Prestashop requiert un pool PHP")
	}
}

func TestResolveProxyKinds(t *testing.T) {
	for _, k := range []AppKind{KindGo, KindNode, KindPython, KindRuby} {
		at, err := Resolve(k, "")
		if err != nil {
			t.Fatal(err)
		}
		if at.Docroot != "" || at.VHostTemplate != "proxy" || at.Runtime == "" {
			t.Errorf("%s mal résolu : %+v", k, at)
		}
	}
}

func TestResolveUnknown(t *testing.T) {
	if _, err := Resolve("php-drupal", ""); err == nil {
		t.Error("type inconnu accepté")
	}
}

func TestSupportsMessenger(t *testing.T) {
	cases := map[string]bool{"3.4": false, "4.0": false, "4.1": true, "4.4": true, "6.4": true, "8.0": true}
	for v, want := range cases {
		if got := SupportsMessenger(KindSymfony, v); got != want {
			t.Errorf("Symfony %s : Messenger = %v, attendu %v", v, got, want)
		}
	}
	if SupportsMessenger(KindPrestashop, "8") {
		t.Error("Messenger ne s'applique pas à Prestashop")
	}
}

// matchesAny dit si le chemin rel, relatif à www, est désigné par l'un des
// motifs — composant par composant, comme le fait la garde de la copie.
func matchesAny(t *testing.T, patterns []string, rel string) bool {
	t.Helper()
	for _, p := range patterns {
		pc, rc := strings.Split(p, "/"), strings.Split(rel, "/")
		if len(pc) != len(rc) {
			continue
		}
		ok := true
		for i := range pc {
			m, err := path.Match(pc[i], rc[i])
			if err != nil {
				t.Fatalf("motif %q invalide : %v", p, err)
			}
			ok = ok && m
		}
		if ok {
			return true
		}
	}
	return false
}

func TestLaConfigurationDesTypesDesigneSesFichiersDeSecrets(t *testing.T) {
	// Un clone ne recopie pas ce qu'un type range par convention dans la
	// configuration d'un environnement ; il recopie ce qui n'en porte que les
	// valeurs par défaut, versionnées — le .env de Symfony.
	cas := []struct {
		kind       AppKind
		version    string
		retenus    []string
		recopiés   []string
		sansConfig bool
	}{
		{KindSymfony, "7.2", []string{".env.local", ".env.local.php", ".env.prod.local",
			"config/secrets/prod/prod.decrypt.private.php", "var/cache"},
			[]string{".env", ".env.prod", "config/secrets/prod/prod.encrypt.public.php", "var/log", "public/index.php"}, false},
		{KindSymfony, "3.4", []string{"app/config/parameters.yml", "var/cache"},
			[]string{"app/config/parameters.yml.dist", "web/app.php"}, false},
		{KindPrestashop, "8", []string{"app/config/parameters.php", "app/config/parameters.yml",
			"config/settings.inc.php", "var/cache"}, []string{"index.php", "img", "config/defines.inc.php"}, false},
		{KindNode, "", []string{".env", ".env.local", ".env.production", ".env.production.local"},
			[]string{".env.example", "server.js"}, false},
		{KindRuby, "", []string{".env", "config/master.key", "config/credentials/production.key"},
			[]string{"config/credentials/production.yml.enc", "config/routes.rb"}, false},
		{KindGo, "", []string{".env"}, []string{"serveur"}, false},
		{KindPython, "", []string{".env"}, []string{"app.py"}, false},
		{KindCustom, "", []string{".env"}, []string{"bin/serveur"}, false},
		{KindPHPPlain, "", nil, []string{"wp-config.php", ".env"}, true},
		{KindStatic, "", nil, []string{"index.html"}, true},
	}
	for _, c := range cas {
		at, err := Resolve(c.kind, c.version)
		if err != nil {
			t.Fatal(err)
		}
		all := append(append([]string(nil), at.Config...), at.Cache...)
		if c.sansConfig != (len(all) == 0) {
			t.Errorf("%s : configuration %v", c.kind, all)
		}
		for _, rel := range c.retenus {
			if !matchesAny(t, all, rel) {
				t.Errorf("%s %s : %s doit être retenu d'un clone (%v)", c.kind, c.version, rel, all)
			}
		}
		for _, rel := range c.recopiés {
			if matchesAny(t, all, rel) {
				t.Errorf("%s %s : %s doit être recopié (%v)", c.kind, c.version, rel, all)
			}
		}
	}
}
