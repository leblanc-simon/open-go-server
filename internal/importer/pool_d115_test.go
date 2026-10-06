package importer

import (
	"context"
	"os"
	"testing"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/provider/php"
)

// R4-33 (D115) : l'adoption ne reprenait du pool que sa version. Le premier
// apply basculait le site sur le preset par défaut — douze workers à 256M —, et
// perdait ce qui le tenait en charge, sa limite de mémoire, ses variables.
func TestAdoptionRepriseLesReglagesDuPool(t *testing.T) {
	scanner, _ := legacyServer(t)
	pool := `; pool réglé à la main
[boutique]
user = boutique
group = boutique
listen = /run/php/php8.1-fpm-boutique.sock
listen.owner = www-data
pm = dynamic
pm.max_children = 40 ; heure de pointe
PM.Start_Servers = 8
php_admin_value[memory_limit] = 1G
php_admin_value[sendmail_path] = "/usr/sbin/sendmail -t -i -f boutique@boutique.fr"
php_flag[display_errors] = off
php_value[error_reporting] = E_ALL & ~E_DEPRECATED
slowlog = /var/log/boutique-slow.log
env[APP_SECRET] = "s3cr;et"
env[HOSTNAME] = $HOSTNAME
php_value[session.save_path] = /var/www/boutique/sessions
php_admin_value[include_path] = .:${HOME}/lib

[autre]
pm.max_children = 99
`
	if err := os.WriteFile(scanner.Paths.PHPPoolFile("8.1", "boutique"), []byte(pool), 0o644); err != nil {
		t.Fatal(err)
	}
	resources, _ := scanner.Scan(context.Background())
	candidates, _ := Correlate(resources, "/home", "preprod-pp.net")
	var boutique Candidate
	for _, c := range candidates {
		if c.Host == "boutique.fr" {
			boutique = c
		}
	}
	p, notes, err := Adopt(boutique, "/home", "preprod-pp.net")
	if err != nil {
		t.Fatal(err)
	}
	decl := p.Spec.PHP.Pools[0]
	if decl.Preset != "custom" {
		t.Errorf("preset %q : « custom » attendu, qui n'ajoute rien aux réglages repris", decl.Preset)
	}
	want := map[string]string{
		"pm":                             "dynamic",
		"pm.max_children":                "40",
		"pm.start_servers":               "8",
		"php_admin_value[memory_limit]":  "1G",
		"php_admin_value[sendmail_path]": `"/usr/sbin/sendmail -t -i -f boutique@boutique.fr"`,
		"php_flag[display_errors]":       "off",
		"php_value[error_reporting]":     "E_ALL & ~E_DEPRECATED",
		// Sans effet en php_value face au socle : repris en php_admin_value.
		"php_admin_value[session.save_path]": "/var/www/boutique/sessions",
		"php_admin_value[include_path]":      ".:${HOME}/lib",
	}
	for k, v := range want {
		if got := decl.Settings[k]; got == nil || *got != v {
			t.Errorf("%s = %v, attendu %q", k, got, v)
		}
	}
	if len(decl.Settings) != len(want) {
		t.Errorf("réglages repris en trop ou en moins : %d, %d attendus", len(decl.Settings), len(want))
	}
	// « $HOSTNAME » relaie la variable du maître : reprise telle quelle.
	if decl.Env["APP_SECRET"] != `"s3cr;et"` || decl.Env["HOSTNAME"] != "$HOSTNAME" || len(decl.Env) != 2 {
		t.Errorf("variables : %v", decl.Env)
	}
	if !containsSubstring(notes, "slowlog") || !containsSubstring(notes, "littéralement : php_admin_value[include_path]") {
		t.Errorf("ce qui n'est pas repris doit être nommé : %v", notes)
	}

	// La déclaration ainsi écrite passe le plan sans refus ni écart, et rend le
	// pool tel qu'il était réglé.
	paths := config.NewPaths(t.TempDir())
	if err := os.MkdirAll(paths.PHPPoolDir("8.1"), 0o755); err != nil {
		t.Fatal(err)
	}
	at, err := p.AppType()
	if err != nil {
		t.Fatal(err)
	}
	r := php.Resolve(config.Default(), paths, p, decl, at)
	if len(r.Problems) > 0 {
		t.Errorf("la déclaration adoptée ne doit rien faire écarter ni refuser : %v", r.Problems)
	}
	if r.Settings["pm.max_children"] != "40" || r.Settings["php_admin_value[memory_limit]"] != "1G" {
		t.Errorf("rendu : %v", r.Settings)
	}
	if _, ok := r.Settings["pm.process_idle_timeout"]; ok {
		t.Error("le preset par défaut ne doit rien ajouter au pool adopté")
	}
}
