package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/leviathan/ogs/internal/config"
	"github.com/leviathan/ogs/internal/engine"
	"github.com/leviathan/ogs/internal/exec"
	"github.com/leviathan/ogs/internal/fs"
	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/naming"
	"github.com/leviathan/ogs/internal/provider/php"
	"github.com/leviathan/ogs/internal/store"
	"github.com/leviathan/ogs/internal/template"
)

func fixture(t *testing.T, engineName string) (*Provider, config.Paths) {
	t.Helper()
	prefix := t.TempDir()
	paths := config.NewPaths(prefix)
	eng, err := NewEngine(engineName, prefix)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{eng.AvailableDir(), eng.EnabledDir(), paths.ACMEChallenge()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []string{"7.4", "8.5"} {
		if err := os.MkdirAll(paths.PHPPoolDir(v), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, bin := range []string{paths.PHPBin(v), paths.PHPFpmBin(v)} {
			if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	st, err := store.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := func() time.Time { return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) }
	tmpl := template.New(paths.TemplatesDir())
	rec := exec.NewRecorder()
	return &Provider{
		Paths: paths, Exec: rec, Templates: tmpl, Engine: eng, Now: now,
		PHP: &php.Provider{Paths: paths, Exec: rec, Templates: tmpl, Store: st, Now: now},
	}, paths
}

func project(t *testing.T, host string, kind model.AppKind, version, php string) *model.Project {
	t.Helper()
	p := model.New(host, naming.EnvProd, kind)
	p.Spec.App.Version = version
	if err := p.Normalize(filepath.Join(t.TempDir(), "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	if php != "" && len(p.Spec.PHP.Pools) > 0 {
		p.Spec.PHP.Pools[0].Version = php
	}
	p.Spec.Domains = append(p.Spec.Domains, model.Domain{Name: "www." + host, Role: "alias"})
	return p
}

func buildMain(t *testing.T, prov *Provider, p *model.Project) *VHostPlan {
	t.Helper()
	vhosts := prov.vhosts(p)
	if len(vhosts) == 0 {
		t.Fatal("aucun vhost déduit")
	}
	bp, err := prov.Build(p, vhosts[0])
	if err != nil {
		t.Fatal(err)
	}
	return bp
}

func TestApacheSymfonyVHost(t *testing.T) {
	prov, paths := fixture(t, "apache")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	bp := buildMain(t, prov, p)
	c := string(bp.Content)
	for _, want := range []string{
		"ServerName shop.fr",
		"ServerAlias www.shop.fr",
		"AllowOverride None", // front controller, pas de .htaccess
		"FallbackResource /index.php",
		"SetHandler \"proxy:unix:" + paths.PHPSocket("8.5", 18501, "shopfr") + "|fcgi://localhost\"",
		"Alias /.well-known/acme-challenge",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("vhost Apache incomplet, %q absent :\n%s", want, c)
		}
	}
	if strings.Contains(c, "VirtualHost *:443") {
		t.Error("sans TLS, aucun bloc 443 ne doit être produit")
	}
}

func TestApachePrestashopUsesHtaccess(t *testing.T) {
	prov, _ := fixture(t, "apache")
	p := project(t, "boutique.fr", model.KindPrestashop, "", "7.4")
	bp := buildMain(t, prov, p)
	c := string(bp.Content)
	if !strings.Contains(c, "AllowOverride All") {
		t.Error("Prestashop s'appuie sur .htaccess : AllowOverride All est requis")
	}
	if strings.Contains(c, "FallbackResource") {
		t.Error("Prestashop gère son routage par .htaccess, pas par FallbackResource")
	}
	if !strings.Contains(c, filepath.Join(p.Spec.Layout.Root, "www")+"\n") &&
		!strings.Contains(c, "DocumentRoot "+filepath.Join(p.Spec.Layout.Root, "www")) {
		t.Errorf("le docroot de Prestashop est www :\n%s", c)
	}
}

func TestNginxPrestashopCarriesRewrites(t *testing.T) {
	// Nginx ne lit pas .htaccess : sans ces règles, le site est à moitié
	// fonctionnel. C'est le piège principal du portage Apache → Nginx.
	prov, _ := fixture(t, "nginx")
	p := project(t, "boutique.fr", model.KindPrestashop, "", "7.4")
	bp := buildMain(t, prov, p)
	c := string(bp.Content)
	for _, want := range []string{
		"rewrite ^/api/?(.*)$",
		"/img/p/",
		"deny all;",
		"fastcgi_pass unix:",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("règle Prestashop absente (%q) :\n%s", want, c)
		}
	}
}

func TestNginxSymfonyFrontController(t *testing.T) {
	prov, _ := fixture(t, "nginx")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	bp := buildMain(t, prov, p)
	c := string(bp.Content)
	if !strings.Contains(c, "try_files $uri /index.php$is_args$args;") {
		t.Errorf("front controller Symfony absent :\n%s", c)
	}
	if strings.Contains(c, "rewrite ^/api/") {
		t.Error("les règles Prestashop ne doivent pas s'appliquer à Symfony")
	}
	// Aucun autre script PHP ne doit être exécutable.
	if !strings.Contains(c, "location ~ \\.php$ {\n        return 404;") {
		t.Errorf("les scripts PHP hors front controller doivent être refusés :\n%s", c)
	}
}

func TestSymfonyLegacyDocrootAndController(t *testing.T) {
	prov, _ := fixture(t, "apache")
	p := project(t, "ancien.fr", model.KindSymfony, "3.4", "7.4")
	bp := buildMain(t, prov, p)
	c := string(bp.Content)
	if !strings.Contains(c, filepath.Join("www", "web")) {
		t.Errorf("Symfony 3.4 sert www/web :\n%s", c)
	}
	if !strings.Contains(c, "FallbackResource /app.php") {
		t.Errorf("Symfony 3.4 entre par app.php :\n%s", c)
	}
}

func TestPrepAddsNoindex(t *testing.T) {
	prov, _ := fixture(t, "nginx")
	p := model.New("shop.fr", naming.EnvPrep, model.KindSymfony)
	p.Spec.App.Version = "6.4"
	if err := p.Normalize(filepath.Join(t.TempDir(), "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	p.Spec.PHP.Pools[0].Version = "8.5"
	bp := buildMain(t, prov, p)
	c := string(bp.Content)
	if !strings.Contains(c, "noindex, nofollow") {
		t.Errorf("une préproduction doit interdire l'indexation :\n%s", c)
	}
	if !strings.Contains(c, "Disallow: /") {
		t.Error("robots.txt doit interdire l'exploration")
	}
}

func TestApplyWritesAndEnables(t *testing.T) {
	prov, _ := fixture(t, "apache")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	ctx := context.Background()
	obs, err := prov.Observe(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	plan := &engine.Plan{Project: p.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	if got := plan.Reloads(); len(got) != 1 || got[0] != "web:apache" {
		t.Errorf("un seul rechargement attendu : %v", got)
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	available := filepath.Join(prov.Engine.AvailableDir(), "shopfr.conf")
	enabled := filepath.Join(prov.Engine.EnabledDir(), "shopfr.conf")
	if _, err := os.Stat(available); err != nil {
		t.Fatalf("vhost non écrit : %v", err)
	}
	target, err := os.Readlink(enabled)
	if err != nil {
		t.Fatalf("vhost non activé : %v", err)
	}
	if !strings.HasSuffix(target, "shopfr.conf") {
		t.Errorf("lien d'activation inattendu : %s", target)
	}

	// Idempotence.
	obs, _ = prov.Observe(ctx, p)
	steps, _ = prov.Plan(ctx, p, obs)
	plan2 := &engine.Plan{Project: p.ID()}
	for _, s := range steps {
		plan2.Add(s)
	}
	if !plan2.Empty() {
		t.Errorf("second plan non vide :\n%s", plan2.String())
	}
}

func TestRedirectVHostForRedirectRole(t *testing.T) {
	prov, _ := fixture(t, "apache")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	p.Spec.Domains = append(p.Spec.Domains, model.Domain{Name: "ancien-shop.fr", Role: "redirect", Target: "shop.fr"})
	vhosts := prov.vhosts(p)
	if len(vhosts) != 2 {
		t.Fatalf("%d vhosts, 2 attendus : %+v", len(vhosts), vhosts)
	}
	bp, err := prov.Build(p, vhosts[1])
	if err != nil {
		t.Fatal(err)
	}
	c := string(bp.Content)
	if !strings.Contains(c, "ancien-shop.fr") || !strings.Contains(c, "shop.fr$1") {
		t.Errorf("vhost de redirection inattendu :\n%s", c)
	}
	if bp.Name != "shopfr_redirect" {
		t.Errorf("nom du vhost de redirection : %s", bp.Name)
	}
}

func TestObserveDetectsDomainConflict(t *testing.T) {
	prov, _ := fixture(t, "apache")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	foreign := filepath.Join(prov.Engine.AvailableDir(), "ancien-site.conf")
	if err := os.WriteFile(foreign, []byte("<VirtualHost *:80>\n  ServerName shop.fr\n</VirtualHost>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	obs, err := prov.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Conflicts["shop.fr"] != foreign {
		t.Fatalf("conflit non détecté : %+v", obs.Conflicts)
	}
	if _, err := prov.Plan(context.Background(), p, obs); err == nil {
		t.Error("un domaine déjà servi doit interrompre le plan")
	}
}

func TestObserveIgnoresForeignVHostsWithoutConflict(t *testing.T) {
	prov, _ := fixture(t, "nginx")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	foreign := filepath.Join(prov.Engine.AvailableDir(), "autre.conf")
	if err := os.WriteFile(foreign, []byte("server {\n  server_name autre.fr;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	obs, err := prov.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.Conflicts) != 0 || len(obs.Stale) != 0 {
		t.Errorf("un vhost étranger sans conflit doit être laissé tranquille : %+v", obs)
	}
}

func TestStaleVHostRemoved(t *testing.T) {
	prov, _ := fixture(t, "apache")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	stale := filepath.Join(prov.Engine.AvailableDir(), "shopfr_ancien.conf")
	if err := fs.WriteAtomic(stale, []byte("# ancien vhost\n"), 0o644, fs.KeepOwner); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	obs, _ := prov.Observe(ctx, p)
	if len(obs.Stale) != 1 {
		t.Fatalf("vhost obsolète non repéré : %+v", obs.Stale)
	}
	steps, _ := prov.Plan(ctx, p, obs)
	plan := &engine.Plan{Project: p.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("le vhost obsolète devrait avoir été supprimé")
	}
}

func TestDetectEngineRefusesAmbiguity(t *testing.T) {
	prefix := t.TempDir()
	for _, d := range []string{"/etc/apache2/sites-available", "/etc/nginx/sites-available"} {
		if err := os.MkdirAll(filepath.Join(prefix, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := DetectEngine("", prefix); err == nil {
		t.Error("deux moteurs installés doivent imposer un choix explicite")
	}
	if e, err := DetectEngine("nginx", prefix); err != nil || e.Name() != "nginx" {
		t.Errorf("le moteur demandé doit primer : %v, %v", e, err)
	}
}

func TestDetectEngineWithoutServer(t *testing.T) {
	if _, err := DetectEngine("", t.TempDir()); err == nil {
		t.Error("sans serveur web installé, la détection doit échouer explicitement")
	}
}

func TestFPMReferencePerEngine(t *testing.T) {
	apache, _ := NewEngine("apache", "")
	nginx, _ := NewEngine("nginx", "")
	if got := apache.FPMReference("/run/php/x.sock", true); got != "unix:/run/php/x.sock|fcgi://localhost" {
		t.Errorf("Apache : %q", got)
	}
	if got := nginx.FPMReference("/run/php/x.sock", true); got != "unix:/run/php/x.sock" {
		t.Errorf("Nginx : %q", got)
	}
	if got := apache.FPMReference("127.0.0.1:18501", false); got != "fcgi://127.0.0.1:18501" {
		t.Errorf("Apache TCP : %q", got)
	}
	if got := nginx.FPMReference("127.0.0.1:18501", false); got != "127.0.0.1:18501" {
		t.Errorf("Nginx TCP : %q", got)
	}
}

func TestEveryTemplateServesHTTPSWhenTLSIsEnabled(t *testing.T) {
	// Le TLS n'est pas l'affaire des seuls sites PHP : un site statique, une
	// application derrière un proxy — donc tous les runtimes non-PHP — et un
	// domaine redirigé doivent tous être servis en HTTPS dès qu'un certificat
	// existe. Sans ce contrôle, le certificat est émis puis jamais présenté.
	cas := []struct {
		kind    model.AppKind
		version string
		role    string // « redirect » pour éprouver le vhost de redirection
	}{
		{kind: model.KindStatic},
		{kind: model.KindPHPPlain},
		{kind: model.KindSymfony, version: "6.4"},
		{kind: model.KindPrestashop, version: "8.1"},
		{kind: model.KindGo},
		{kind: model.KindNode},
		{kind: model.KindStatic, role: "redirect"},
	}
	for _, moteur := range []string{"apache", "nginx"} {
		for _, c := range cas {
			nom := moteur + "/" + string(c.kind) + c.role
			t.Run(nom, func(t *testing.T) {
				prov, _ := fixture(t, moteur)
				prov.CertRef = func(*model.Project) (string, bool) { return "shopfr", true }
				p := project(t, "shop.fr", c.kind, c.version, "8.5")

				// Une application derrière un proxy déclare sa cible : c'est le
				// provider applicatif qui la fournit en exploitation.
				if c.kind == model.KindGo || c.kind == model.KindNode {
					p.Spec.Web.VHosts = prov.vhosts(p)
					p.Spec.Web.VHosts[0].Options = map[string]any{
						"proxyTarget": "unix:/run/ogs/shopfr/app.sock|http://localhost/",
					}
				}
				vhosts := prov.vhosts(p)
				if c.role == "redirect" {
					p.Spec.Domains = append(p.Spec.Domains,
						model.Domain{Name: "ancien.fr", Role: "redirect"})
					vhosts = prov.vhosts(p)
					if len(vhosts) < 2 {
						t.Fatal("aucun vhost de redirection déduit")
					}
					vhosts = vhosts[1:]
				}
				bp, err := prov.Build(p, vhosts[0])
				if err != nil {
					t.Fatal(err)
				}
				c := string(bp.Content)
				attendus := []string{"443"}
				if moteur == "apache" {
					attendus = append(attendus, "SSLEngine on", "SSLCertificateFile", "SSLCertificateKeyFile")
				} else {
					attendus = append(attendus, "listen 443 ssl", "ssl_certificate", "ssl_certificate_key")
				}
				for _, want := range attendus {
					if !strings.Contains(c, want) {
						t.Errorf("le TLS est activé mais %q est absent :\n%s", want, c)
					}
				}
				// Et le challenge doit rester servi en clair, sans quoi le
				// renouvellement échouerait.
				if !strings.Contains(c, "acme-challenge") {
					t.Errorf("le challenge ACME n'est plus servi :\n%s", c)
				}
			})
		}
	}
}

func TestNginxSnippetDefinesProxyVariables(t *testing.T) {
	// Le vhost de proxy emploie $connection_upgrade, qui n'existe pas par défaut.
	// Sans ce fragment de portée http, nginx refuse l'ensemble de sa
	// configuration : un seul site applicatif emporterait tous les autres au
	// prochain rechargement.
	prov, paths := fixture(t, "nginx")
	step, err := prov.PlanSnippet()
	if err != nil {
		t.Fatal(err)
	}
	if step == nil {
		t.Fatal("nginx doit produire un fragment de portée serveur")
	}
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(paths.Prefix(), "/etc/nginx/conf.d/ogs.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"map $http_upgrade $connection_upgrade", "default upgrade", "close"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("%q absent du fragment :\n%s", want, data)
		}
	}
	// Rejoué, il ne doit plus rien changer.
	again, err := prov.PlanSnippet()
	if err != nil {
		t.Fatal(err)
	}
	if again.Mutates() {
		t.Errorf("le fragment déjà conforme ne doit pas être réécrit : %s", again)
	}

	// Apache n'a pas de réglage de portée serveur : aucune étape.
	apache, _ := fixture(t, "apache")
	step, err = apache.PlanSnippet()
	if err != nil {
		t.Fatal(err)
	}
	if step != nil {
		t.Errorf("Apache ne doit produire aucun fragment : %s", step)
	}
}

func TestProxyVHostReferencesOnlyDefinedVariables(t *testing.T) {
	// Toute variable employée par un vhost doit être fournie par nginx ou par le
	// fragment d'ogs : une variable inconnue est une erreur de configuration, pas
	// un avertissement.
	prov, _ := fixture(t, "nginx")
	prov.CertRef = func(*model.Project) (string, bool) { return "shopfr", true }
	p := project(t, "shop.fr", model.KindGo, "", "")
	p.Spec.Web.VHosts = prov.vhosts(p)
	p.Spec.Web.VHosts[0].Options = map[string]any{"proxyTarget": "127.0.0.1:20101"}
	bp, err := prov.Build(p, prov.vhosts(p)[0])
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := prov.Templates.Render("nginx/http-snippet.conf", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	// Les variables intégrées à nginx qu'emploient nos templates.
	integrees := map[string]bool{
		"$http_upgrade": true, "$host": true, "$remote_addr": true, "$scheme": true,
		"$proxy_add_x_forwarded_for": true, "$request_uri": true, "$uri": true,
		"$is_args": true, "$args": true, "$document_root": true,
		"$fastcgi_script_name": true, "$1": true, "$2": true, "$3": true, "$4": true, "$5": true,
	}
	for _, mot := range regexp.MustCompile(`\$[a-z_]+`).FindAllString(string(bp.Content), -1) {
		if integrees[mot] || strings.Contains(string(fragment), mot) {
			continue
		}
		t.Errorf("variable %s employée par le vhost mais définie nulle part", mot)
	}
}

// logPaths n'extrait que des chemins : le format nommé d'un CustomLog Apache et
// le « off » de nginx n'en sont pas, et les prendre pour des fichiers ferait
// refuser tout rechargement.
func TestLogPathsIgnoresNonPaths(t *testing.T) {
	const vhost = `# Généré par ogs
<VirtualHost *:80>
    ErrorLog /home/shop.fr/logs/apache-error.log
    CustomLog /home/shop.fr/logs/apache-access.log combined
    CustomLog "|/usr/bin/rotatelogs /var/log/x" common
</VirtualHost>
server {
    access_log /home/shop.fr/logs/nginx-access.log;
    error_log  /home/shop.fr/logs/nginx-error.log;
    access_log off;
}
`
	got := logPaths(vhost)
	want := []string{
		"/home/shop.fr/logs/apache-error.log",
		"/home/shop.fr/logs/apache-access.log",
		"/home/shop.fr/logs/nginx-access.log",
		"/home/shop.fr/logs/nginx-error.log",
	}
	if len(got) != len(want) {
		t.Fatalf("chemins extraits : %v, attendu %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("chemin %d : %q, attendu %q", i, got[i], want[i])
		}
	}
}

// Un journal inaccessible doit faire refuser le rechargement : le maître ouvre
// tous les journaux au redémarrage et s'arrête si l'un résiste, emportant les
// sites de tout le serveur — ce qu'« apachectl -t » ne voit pas.
func TestReloadRefusedWhenLogUnwritable(t *testing.T) {
	p, _ := fixture(t, "apache")
	logDir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(logDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(logDir, 0o755) })
	vhost := "# Généré par ogs — NE PAS MODIFIER À LA MAIN\n<VirtualHost *:80>\n    ErrorLog " +
		filepath.Join(logDir, "apache-error.log") + "\n</VirtualHost>\n"
	if err := os.WriteFile(filepath.Join(p.Engine.EnabledDir(), "shopfr.conf"), []byte(vhost), 0o644); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root écrit partout : le refus ne peut pas être éprouvé")
	}
	err := p.CheckLogs(context.Background())
	if err == nil {
		t.Fatal("un journal non inscriptible doit faire refuser le rechargement")
	}
	if !strings.Contains(err.Error(), "apache-error.log") {
		t.Errorf("le message doit nommer le journal fautif : %v", err)
	}

	// Un vhost qu'ogs n'a pas écrit ne relève pas de lui.
	if err := os.WriteFile(filepath.Join(p.Engine.EnabledDir(), "shopfr.conf"),
		[]byte("<VirtualHost *:80>\n    ErrorLog "+filepath.Join(logDir, "autre.log")+
			"\n</VirtualHost>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckLogs(context.Background()); err != nil {
		t.Errorf("un vhost étranger ne doit pas faire refuser le rechargement : %v", err)
	}
}

// Un rechargement qui rend 0 mais laisse le serveur mort doit être signalé, et le
// serveur relevé : systemd journalise « Reloaded » puis la mort du maître, et un
// parc entier resterait hors ligne sans que rien ne le dise.
func TestReloadDetectsDeadServer(t *testing.T) {
	p, _ := fixture(t, "apache")
	rec := exec.NewRecorder()
	p.Exec = rec
	// Le Recorder se dit simulé : Reload court-circuiterait. Le comportement est
	// éprouvé sur unitActive et sur la commande de relance, qui portent la logique.
	rec.Errors["systemctl is-active"] = errors.New("exit status 3")
	if p.unitActive(context.Background()) {
		t.Error("une unité inactive ne doit pas être vue active")
	}
	delete(rec.Errors, "systemctl is-active")
	if !p.unitActive(context.Background()) {
		t.Error("une unité active doit être vue active")
	}
}

// Le complément d'unité énumère les journaux de tout le parc, chaque chemin
// facultatif : un ReadWritePaths pointant sur un répertoire absent fait échouer le
// service en 226/NAMESPACE, et un projet supprimé y reste jusqu'au prochain apply.
func TestPlanDropInListsLogDirs(t *testing.T) {
	p, _ := fixture(t, "apache")
	a := model.New("shop.fr", naming.EnvProd, model.KindStatic)
	b := model.New("autre.fr", naming.EnvProd, model.KindStatic)
	for _, proj := range []*model.Project{a, b} {
		if err := proj.Normalize("/home", "preprod-pp.net"); err != nil {
			t.Fatal(err)
		}
	}
	steps, err := p.PlanDropIn(context.Background(), []*model.Project{b, a})
	if err != nil {
		t.Fatal(err)
	}
	step := steps[0]
	if step.Action != engine.Create {
		t.Errorf("action %s, attendu une création", step.Action)
	}
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(p.DropInPath())
	if err != nil {
		t.Fatal(err)
	}
	got := string(content)
	for _, want := range []string{
		"[Service]",
		"ReadWritePaths=-/home/autre.fr/logs",
		"ReadWritePaths=-/home/shop.fr/logs",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q attendu dans :\n%s", want, got)
		}
	}
	// L'ordre est déterministe, sans quoi le fichier oscillerait entre deux états
	// et chaque apply redémarrerait le serveur web.
	if strings.Index(got, "autre.fr") > strings.Index(got, "shop.fr") {
		t.Error("les répertoires doivent être triés")
	}
	// Deuxième passage : le fichier est conforme, donc aucune action — un
	// redémarrage du moteur à chaque apply serait une coupure gratuite.
	again, err := p.PlanDropIn(context.Background(), []*model.Project{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].Action != engine.NoOp {
		t.Errorf("étapes %+v, attendu une seule sans action", again)
	}
}

// underHome borne le contrôle au territoire que ProtectHome couvre : refuser un
// journal sous /var/log au prétexte d'un ProtectHome serait un faux positif.
func TestUnderHome(t *testing.T) {
	for path, want := range map[string]bool{
		"/home/shop.fr/logs/apache-error.log": true,
		"/root/logs/x.log":                    true,
		"/run/user/1000/x.log":                true,
		"/var/log/apache2/error.log":          false,
		"/homeless/x.log":                     false,
		"/home":                               true,
	} {
		if got := underHome(path); got != want {
			t.Errorf("underHome(%q) = %v, attendu %v", path, got, want)
		}
	}
}
