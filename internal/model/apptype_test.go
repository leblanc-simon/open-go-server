package model

import "testing"

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
