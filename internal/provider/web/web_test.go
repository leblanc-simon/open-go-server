package web

import (
	"context"
	"os"
	"path/filepath"
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
