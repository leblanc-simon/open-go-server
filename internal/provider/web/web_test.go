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

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/provider/php"
	tlsprov "leblanc.io/open-go-server/internal/provider/tls"
	"leblanc.io/open-go-server/internal/store"
	"leblanc.io/open-go-server/internal/template"
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
	if err := p.Normalize(filepath.Join(t.TempDir(), "home"), "preprod-pp.net",
		model.WithLogsRoot(filepath.Join(t.TempDir(), "ogs-projects"))); err != nil {
		t.Fatal(err)
	}
	if php != "" && len(p.Spec.PHP.Pools) > 0 {
		p.Spec.PHP.Pools[0].Version = php
	}
	p.Spec.Domains = append(p.Spec.Domains, model.Domain{Name: "www." + host, Role: "alias"})
	return p
}

// certificatCouvrant simule le certificat shopfr présent sur le disque, qui
// couvre les noms donnés — les mêmes que la demande.
func certificatCouvrant(noms ...string) func(*model.Project) tlsprov.Coverage {
	return func(*model.Project) tlsprov.Coverage {
		return tlsprov.Coverage{Ref: "shopfr", Present: noms, Requested: noms}
	}
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
	if err := p.Normalize(filepath.Join(t.TempDir(), "home"), "preprod-pp.net",
		model.WithLogsRoot(filepath.Join(t.TempDir(), "ogs-projects"))); err != nil {
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

// vhostEcritPour rend le contenu qu'ogs aurait écrit pour le projet id : c'est
// l'en-tête, et non le nom, qui rattache un fichier à son projet.
func vhostEcritPour(id, corps string) []byte {
	return fs.Render(fs.Header{Project: id, Resource: "web/vhost/" + id, Generated: time.Now()},
		[]byte(corps), "#")
}

// poserVHost écrit un fichier dans sites-available et, si actif, son lien
// d'activation relatif, comme a2ensite.
func poserVHost(t *testing.T, prov *Provider, nom string, contenu []byte, actif bool) (string, string) {
	t.Helper()
	chemin := filepath.Join(prov.Engine.AvailableDir(), nom)
	if err := os.WriteFile(chemin, contenu, 0o644); err != nil {
		t.Fatal(err)
	}
	lien := filepath.Join(prov.Engine.EnabledDir(), nom)
	if actif {
		if err := os.Symlink(filepath.Join("..", "sites-available", nom), lien); err != nil {
			t.Fatal(err)
		}
	}
	return chemin, lien
}

// appliquerVHosts observe, planifie et exécute les vhosts d'un projet.
func appliquerVHosts(t *testing.T, prov *Provider, p *model.Project) (*engine.Plan, *engine.Report, error) {
	t.Helper()
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
	report, err := engine.Execute(ctx, plan, engine.Options{})
	return plan, report, err
}

func TestStaleVHostRemoved(t *testing.T) {
	prov, _ := fixture(t, "apache")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	stale, lien := poserVHost(t, prov, "shopfr_ancien.conf",
		vhostEcritPour("shopfr", "<VirtualHost *:80>\n  ServerName ancien.shop.fr\n</VirtualHost>\n"), true)
	obs, _ := prov.Observe(context.Background(), p)
	if len(obs.Stale) != 1 {
		t.Fatalf("vhost obsolète non repéré : %+v", obs.Stale)
	}
	if _, _, err := appliquerVHosts(t, prov, p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("le vhost obsolète devrait avoir été supprimé")
	}
	// Le lien d'activation suit le sort du fichier : laissé derrière, il
	// pointerait dans le vide, ce que nginx tient pour fatal.
	if _, err := os.Lstat(lien); !os.IsNotExist(err) {
		t.Errorf("le lien d'activation du vhost obsolète devait partir avec lui : %v", err)
	}
}

// Le nom d'un vhost ne dit pas à qui il appartient : « shop_api.conf » semble
// prolonger « shop » d'un rôle, et c'est le vhost principal du projet shop_api.
// L'attribution par préfixe le faisait retirer, lien compris, par l'apply de shop.
func TestObserveNAttribuePasLeVHostDUnVoisinParSonNom(t *testing.T) {
	prov, _ := fixture(t, "apache")
	shop := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	shop.Metadata.Name = "shop"
	chemin, lien := poserVHost(t, prov, "shop_api.conf",
		vhostEcritPour("shop_api", "<VirtualHost *:80>\n  ServerName api.shop.fr\n</VirtualHost>\n"), true)

	obs, err := prov.Observe(context.Background(), shop)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.Stale) != 0 {
		t.Errorf("le vhost du voisin ne doit pas être obsolète pour shop : %v", obs.Stale)
	}
	if _, _, err := appliquerVHosts(t, prov, shop); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{chemin, lien} {
		if _, err := os.Lstat(f); err != nil {
			t.Errorf("%s du voisin a été retiré par l'apply de shop : %v", f, err)
		}
	}
}

// Un vhost posé à la main selon la convention ne porte pas la marque d'ogs : il
// n'est jamais retiré, ni son lien, --force compris. S'il sert un domaine du
// projet, c'est un conflit — le cas que l'attribution par nom masquait en le
// rangeant parmi les obsolètes.
func TestVHostPoseALaMainSelonLaConventionNEstJamaisRetire(t *testing.T) {
	for _, cas := range []struct {
		nom, domaine string
		conflit      bool
	}{
		{"shopfr_maintenance.conf", "maintenance.shop.fr", false},
		{"shopfr_old.conf", "shop.fr", true},
	} {
		t.Run(cas.nom, func(t *testing.T) {
			prov, _ := fixture(t, "apache")
			prov.Force = true
			p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
			corps := []byte("<VirtualHost *:80>\n  ServerName " + cas.domaine + "\n</VirtualHost>\n")
			chemin, lien := poserVHost(t, prov, cas.nom, corps, true)
			obs, err := prov.Observe(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			if len(obs.Stale) != 0 {
				t.Errorf("un vhost non géré ne doit pas être obsolète : %v", obs.Stale)
			}
			if got := obs.Conflicts[cas.domaine] == chemin; got != cas.conflit {
				t.Errorf("conflit sur %s : %v, attendu %v (%v)", cas.domaine, got, cas.conflit, obs.Conflicts)
			}
			if cas.conflit {
				if _, err := prov.Plan(context.Background(), p, obs); err == nil {
					t.Error("un domaine servi par un vhost non géré doit interrompre le plan")
				}
				return
			}
			if _, _, err := appliquerVHosts(t, prov, p); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(chemin); err != nil || string(data) != string(corps) {
				t.Errorf("le vhost posé à la main a été touché : %v", err)
			}
			if _, err := os.Lstat(lien); err != nil {
				t.Errorf("son lien d'activation a été retiré : %v", err)
			}
		})
	}
}

// Un vhost d'ogs retouché à la main n'est retiré, comme il n'est écrasé, que
// sous --force et après sauvegarde — lien d'activation compris.
func TestRetraitDUnVHostRetoucheExigeForceEtSauvegarde(t *testing.T) {
	prov, paths := fixture(t, "nginx")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	retouche := append(vhostEcritPour("shopfr", "server {\n  server_name ancien.shop.fr;\n}\n"),
		"# réglage posé à la main\n"...)
	chemin, lien := poserVHost(t, prov, "shopfr_ancien.conf", retouche, true)

	plan, report, err := appliquerVHosts(t, prov, p)
	if err != nil {
		t.Fatalf("un retrait refusé ne doit pas défaire le reste de l'apply : %v", err)
	}
	if !strings.Contains(plan.String(), "modifié hors ogs") {
		t.Fatalf("le plan doit annoncer le refus du retrait :\n%s", plan.String())
	}
	if !strings.Contains(strings.Join(report.Warnings, "\n"), "--force") {
		t.Errorf("le refus doit nommer --force : %v", report.Warnings)
	}
	if data, _ := os.ReadFile(chemin); string(data) != string(retouche) {
		t.Fatal("le vhost retouché a été retiré sans --force")
	}
	if _, err := os.Lstat(lien); err != nil {
		t.Fatalf("le lien d'un vhost laissé en place doit rester : %v", err)
	}

	prov.Force = true
	plan, _, err = appliquerVHosts(t, prov, p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "retrait forcé") {
		t.Fatalf("le plan doit annoncer le retrait forcé :\n%s", plan.String())
	}
	for _, f := range []string{chemin, lien} {
		if _, err := os.Lstat(f); !os.IsNotExist(err) {
			t.Errorf("%s devait être retiré sous --force : %v", f, err)
		}
	}
	baks, _ := filepath.Glob(filepath.Join(paths.BackupsDir(), "shopfr_ancien.conf.ogs-bak-*"))
	if len(baks) != 1 {
		t.Fatalf("une sauvegarde attendue dans %s, %d trouvée(s)", paths.BackupsDir(), len(baks))
	}
	if saved, _ := os.ReadFile(baks[0]); string(saved) != string(retouche) {
		t.Errorf("la sauvegarde doit porter le vhost retouché :\n%s", saved)
	}
}

// Seuls les liens qui désignent le fichier retiré partent avec lui : un fichier
// régulier déposé dans sites-enabled sous le même nom n'est pas son activation.
func TestLeRetraitDUnVHostNeRetireQueSesLiens(t *testing.T) {
	prov, _ := fixture(t, "nginx")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	stale, _ := poserVHost(t, prov, "shopfr_ancien.conf",
		vhostEcritPour("shopfr", "server {\n  server_name ancien.shop.fr;\n}\n"), false)
	copie := filepath.Join(prov.Engine.EnabledDir(), "shopfr_ancien.conf")
	if err := os.WriteFile(copie, []byte("server { server_name copie.shop.fr; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Un lien d'un autre nom, absolu, vers le fichier retiré.
	autre := filepath.Join(prov.Engine.EnabledDir(), "000-ancien")
	if err := os.Symlink(stale, autre); err != nil {
		t.Fatal(err)
	}
	// Un lien de même nom qu'un autre vhost retiré, mais qui désigne un autre
	// fichier : c'est la cible qui fait l'activation, non le nom.
	vieux, _ := poserVHost(t, prov, "shopfr_vieux.conf",
		vhostEcritPour("shopfr", "server {\n  server_name vieux.shop.fr;\n}\n"), false)
	ailleurs := filepath.Join(t.TempDir(), "ailleurs.conf")
	if err := os.WriteFile(ailleurs, []byte("server { server_name ailleurs.fr; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	homonyme := filepath.Join(prov.Engine.EnabledDir(), "shopfr_vieux.conf")
	if err := os.Symlink(ailleurs, homonyme); err != nil {
		t.Fatal(err)
	}
	if _, _, err := appliquerVHosts(t, prov, p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(copie); err != nil {
		t.Errorf("le fichier régulier de sites-enabled a été retiré : %v", err)
	}
	if _, err := os.Lstat(autre); !os.IsNotExist(err) {
		t.Errorf("le lien vers le vhost retiré devait partir avec lui : %v", err)
	}
	if _, err := os.Stat(vieux); !os.IsNotExist(err) {
		t.Errorf("le vhost %s devait être retiré : %v", vieux, err)
	}
	if cible, err := os.Readlink(homonyme); err != nil || cible != ailleurs {
		t.Errorf("le lien de même nom vers un autre fichier a été touché : %q, %v", cible, err)
	}
}

// Un ancien vhost du projet dont l'exploitant a retouché le haut reste le sien :
// la marque se cherche là où Inspect la cherche. Exigée en tête, elle faisait du
// fichier une configuration étrangère servant un domaine du projet — le plan
// s'arrêtait sur un conflit avec lui-même —, alors que le régime des cinq
// états sait le retirer sous --force, sauvegarde faite.
func TestUnVHostObsoleteRetoucheAuDessusDeSonEnTeteResteAuProjet(t *testing.T) {
	prov, paths := fixture(t, "apache")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	contenu := append([]byte("# garder jusqu'à fin septembre\n"),
		vhostEcritPour("shopfr", "<VirtualHost *:80>\n  ServerName shop.fr\n</VirtualHost>\n")...)
	ancien, lien := poserVHost(t, prov, "shopfr_ancien.conf", contenu, true)

	obs, err := prov.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.Conflicts) != 0 || len(obs.Stale) != 1 || obs.Stale[0] != ancien {
		t.Fatalf("l'ancien vhost retouché doit être obsolète, non un conflit : %+v", obs)
	}
	plan, _, err := appliquerVHosts(t, prov, p)
	if err != nil {
		t.Fatalf("un retrait refusé ne doit pas défaire l'apply : %v", err)
	}
	if !strings.Contains(plan.String(), "modifié hors ogs") {
		t.Errorf("le plan doit annoncer le refus :\n%s", plan.String())
	}
	if data, _ := os.ReadFile(ancien); string(data) != string(contenu) {
		t.Fatal("le vhost retouché a été retiré sans --force")
	}

	prov.Force = true
	if _, _, err := appliquerVHosts(t, prov, p); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{ancien, lien} {
		if _, err := os.Lstat(f); !os.IsNotExist(err) {
			t.Errorf("%s devait être retiré sous --force : %v", f, err)
		}
	}
	baks, _ := filepath.Glob(filepath.Join(paths.BackupsDir(), "shopfr_ancien.conf.ogs-bak-*"))
	if len(baks) != 1 {
		t.Fatalf("une sauvegarde attendue, %d trouvée(s)", len(baks))
	}
	if saved, _ := os.ReadFile(baks[0]); string(saved) != string(contenu) {
		t.Errorf("la sauvegarde doit porter le vhost retouché :\n%s", saved)
	}
}

// Un projet adopté avant que l'import ne relève le nom exact des fichiers porte
// « name: shop » sans « file » : son fichier d'origine shop.fr lui paraît
// étranger. Le renvoyer à « ogs import », qu'il a déjà passé, le ferait tourner
// en rond ; le conflit dit comment désigner le fichier.
func TestLeConflitDUnProjetAdopteDitCommentDesignerSonFichier(t *testing.T) {
	prov, _ := fixture(t, "nginx")
	p := project(t, "shop.fr", model.KindStatic, "", "")
	p.Metadata.Labels = map[string]string{"adopted": "true"}
	p.Spec.Web.VHosts = []model.VHost{{Name: "shop", Domains: []string{"shop.fr"}}}
	poserVHost(t, prov, "shop.fr", []byte("server {\n  listen 80;\n  server_name shop.fr;\n}\n"), true)

	obs, err := prov.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = prov.Plan(context.Background(), p, obs)
	if err == nil || !strings.Contains(err.Error(), "« file: shop.fr »") || strings.Contains(err.Error(), "ogs import") {
		t.Errorf("le conflit doit dire de désigner le fichier d'origine : %v", err)
	}

	// Un projet qui n'est pas adopté garde le conseil d'origine.
	delete(p.Metadata.Labels, "adopted")
	if _, err := prov.Plan(context.Background(), p, obs); err == nil || !strings.Contains(err.Error(), "ogs import") {
		t.Errorf("hors adoption, le conflit renvoie à l'import : %v", err)
	}
}

// Un vhost nginx adopté garde son fichier, sans extension selon l'usage Debian :
// c'est lui qui est réécrit, par --force et après sauvegarde. Le nom tronqué
// (« shop ») faisait écrire shop.conf à côté, et retirer l'original comme le
// site voisin shop.de.
func TestVHostAdopteSansExtensionEstReprisEnPlace(t *testing.T) {
	prov, paths := fixture(t, "nginx")
	p := project(t, "shop.fr", model.KindStatic, "", "")
	p.Metadata.Labels = map[string]string{"adopted": "true"}
	p.Spec.Web.VHosts = []model.VHost{{Name: "shop.fr", File: "shop.fr", Domains: []string{"shop.fr"}}}
	origine := []byte("server {\n  listen 80;\n  server_name shop.fr;\n}\n")
	chemin, lien := poserVHost(t, prov, "shop.fr", origine, true)
	voisinCorps := []byte("server {\n  listen 80;\n  server_name shop.de;\n}\n")
	voisin, voisinLien := poserVHost(t, prov, "shop.de", voisinCorps, true)

	obs, err := prov.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.Stale) != 0 || len(obs.Conflicts) != 0 {
		t.Fatalf("ni obsolète ni conflit attendu : %+v", obs)
	}
	bp := buildMain(t, prov, p)
	if bp.Path != chemin || bp.EnabledPath != lien {
		t.Fatalf("le vhost adopté doit viser son fichier d'origine : %s, %s", bp.Path, bp.EnabledPath)
	}

	// Sans --force : le fichier d'origine, non géré, est refusé et laissé intact.
	if _, _, err := appliquerVHosts(t, prov, p); err == nil {
		t.Fatal("la reprise d'un vhost non géré doit exiger --force")
	}
	if data, _ := os.ReadFile(chemin); string(data) != string(origine) {
		t.Fatal("le vhost d'origine a été touché sans --force")
	}

	prov.Force = true
	if _, _, err := appliquerVHosts(t, prov, p); err != nil {
		t.Fatal(err)
	}
	if owner, _ := fs.ManagedProject(chemin); owner != p.ID() {
		t.Errorf("le fichier d'origine doit être réécrit par ogs pour %s, obtenu %q", p.ID(), owner)
	}
	if _, err := os.Stat(filepath.Join(prov.Engine.AvailableDir(), "shop.conf")); !os.IsNotExist(err) {
		t.Errorf("aucun second fichier ne doit apparaître à côté de l'original : %v", err)
	}
	baks, _ := filepath.Glob(filepath.Join(paths.BackupsDir(), "shop.fr.ogs-bak-*"))
	if len(baks) != 1 {
		t.Fatalf("une sauvegarde de l'original attendue, %d trouvée(s)", len(baks))
	}
	if data, _ := os.ReadFile(voisin); string(data) != string(voisinCorps) {
		t.Error("le vhost du site voisin a été touché")
	}
	if _, err := os.Lstat(voisinLien); err != nil {
		t.Errorf("le lien du site voisin a été retiré : %v", err)
	}
	if got := prov.VHostFiles(p); len(got) != 1 || got[0] != "shop.fr" {
		t.Errorf("fichiers du projet : %v, attendu [shop.fr]", got)
	}
}

// Apache n'inclut que sites-enabled/*.conf : un vhost adopté dont le fichier
// déclaré n'en porte pas l'extension serait écrit, activé et jamais servi.
func TestPlanRefuseSurApacheUnFichierQuIlNeChargeraitPas(t *testing.T) {
	prov, _ := fixture(t, "apache")
	p := project(t, "shop.fr", model.KindStatic, "", "")
	p.Metadata.Labels = map[string]string{"adopted": "true"}
	p.Spec.Web.VHosts = []model.VHost{{Name: "shop.fr", File: "shop.fr", Domains: []string{"shop.fr"}}}
	_, err := prov.Plan(context.Background(), p, Observed{})
	if err == nil || !strings.Contains(err.Error(), "sites-enabled/*.conf") {
		t.Fatalf("un fichier sans .conf doit être refusé sur Apache : %v", err)
	}
	// Une faute de déclaration, que l'appelant rend en code 3, non un conflit.
	var fileErr *FileError
	if !errors.As(err, &fileErr) || fileErr.File != "shop.fr" {
		t.Errorf("le refus doit être une erreur de fichier typée : %T %v", err, err)
	}
	if _, err := os.Stat(filepath.Join(prov.Engine.AvailableDir(), "shop.fr")); !os.IsNotExist(err) {
		t.Errorf("rien ne doit être écrit : %v", err)
	}
	p.Spec.Web.VHosts[0].File = "shop.fr.conf"
	if _, err := prov.Plan(context.Background(), p, Observed{}); err != nil {
		t.Errorf("un fichier en .conf doit être admis : %v", err)
	}
}

// Le vhost « b2b » de shop s'écrit dans shop_b2b.conf, le vhost principal du
// projet shop_b2b : l'en-tête le dit, et le fichier n'est jamais réécrit pour
// shop — --force compris, qui reprend un fichier retouché ou étranger, non celui
// d'un voisin.
func TestPlanRefuseDEcrireLeVHostDUnAutreProjet(t *testing.T) {
	prov, _ := fixture(t, "apache")
	prov.Force = true
	shop := project(t, "shop.fr", model.KindStatic, "", "")
	shop.Metadata.Name = "shop"
	shop.Spec.Web.VHosts = []model.VHost{
		{Name: model.MainPoolName, Domains: []string{"shop.fr"}},
		{Name: "b2b", Domains: []string{"pro.shop.fr"}},
	}
	voisin := vhostEcritPour("shop_b2b", "<VirtualHost *:80>\n  ServerName b2b.shop.fr\n</VirtualHost>\n")
	chemin, _ := poserVHost(t, prov, "shop_b2b.conf", voisin, true)

	obs, err := prov.Observe(context.Background(), shop)
	if err != nil {
		t.Fatal(err)
	}
	_, err = prov.Plan(context.Background(), shop, obs)
	var owned *fs.OwnerError
	if !errors.As(err, &owned) || owned.Owner != "shop_b2b" {
		t.Fatalf("le fichier du voisin doit être un conflit nommant son propriétaire : %v", err)
	}
	if data, _ := os.ReadFile(chemin); string(data) != string(voisin) {
		t.Error("le vhost du voisin a été touché")
	}
}

// L'en-tête du vhost de supervision nomme « parc », qui est aussi un
// identifiant valide : pour un projet ainsi nommé, ogs-status.conf passait pour
// l'un de ses vhosts obsolètes. C'est la ressource inscrite qui le range.
func TestObserveNeRangePasLeVHostDeSupervisionParmiCeuxDuProjetParc(t *testing.T) {
	prov, _ := fixture(t, "nginx")
	parc := project(t, "parc.fr", model.KindSymfony, "6.4", "8.5")
	parc.Metadata.Name = "parc"
	ctx := context.Background()
	steps, err := prov.PlanStatus(ctx, []*model.Project{parc}, "127.0.0.1:8099", "/ogs-status", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Apply != nil {
			if err := s.Apply(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	status := filepath.Join(prov.Engine.AvailableDir(), prov.Engine.FileName(StatusVHostName))
	if owner, _ := fs.ManagedProject(status); owner != "parc" {
		t.Fatalf("l'en-tête du vhost de supervision doit nommer « parc », obtenu %q", owner)
	}
	obs, err := prov.Observe(ctx, parc)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.Stale) != 0 {
		t.Errorf("le vhost de supervision n'est pas un vhost du projet parc : %v", obs.Stale)
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
				prov.Certificate = certificatCouvrant("shop.fr", "www.shop.fr", "ancien.fr")
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

// certificatEnAttente simule le certificat shopfr présent pour les noms déjà
// servis, alors que la demande en porte d'autres : une réémission est en attente.
func certificatEnAttente(presents []string, demandes ...string) func(*model.Project) tlsprov.Coverage {
	return func(*model.Project) tlsprov.Coverage {
		return tlsprov.Coverage{Ref: "shopfr", Present: presents, Requested: demandes}
	}
}

// blocs sépare le rendu d'un vhost en sa partie du port 80 et sa partie du port
// 443, vide sans TLS.
func blocs(t *testing.T, moteur, c string) (port80, port443 string) {
	t.Helper()
	marque := "<VirtualHost *:443>"
	if moteur == "nginx" {
		marque = "listen 443 ssl;"
	}
	i := strings.Index(c, marque)
	if i < 0 {
		return c, ""
	}
	return c[:i], c[i:]
}

// servi dit si un nom figure parmi les noms d'un bloc, pour l'un ou l'autre
// moteur.
func servi(moteur, bloc, nom string) bool {
	if moteur == "apache" {
		return strings.Contains(bloc, "ServerName "+nom+"\n") || strings.Contains(bloc, "ServerAlias "+nom+"\n")
	}
	for _, l := range strings.Split(bloc, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "server_name ") {
			for _, n := range strings.Fields(strings.TrimSuffix(strings.TrimPrefix(l, "server_name "), ";")) {
				if n == nom {
					return true
				}
			}
		}
	}
	return false
}

// redirigeVers renvoie la cible de la redirection du port 80 d'un bloc.
func redirigeVers(moteur, bloc string) string {
	re := regexp.MustCompile(`RewriteRule \^\(\.\*\)\$ (\S+)\$1`)
	if moteur == "nginx" {
		re = regexp.MustCompile(`return 301 (\S+)\$request_uri;`)
	}
	if m := re.FindStringSubmatch(bloc); m != nil {
		return m[1]
	}
	return ""
}

func TestAjoutDAliasGardeLeHTTPSPendantLaReemission(t *testing.T) {
	// Un alias ajouté ne couvre pas encore le certificat en place, qui reste
	// valable pour le reste. Le vhost redescendait en HTTP seul — rechargé ainsi
	// avant même le challenge —, et pour de bon si l'émission échouait : HSTS
	// rendait le site injoignable. Il doit garder son bloc 443 sur le certificat
	// en place, et servir le challenge du nouvel alias sur le port 80.
	for _, moteur := range []string{"apache", "nginx"} {
		t.Run(moteur, func(t *testing.T) {
			prov, _ := fixture(t, moteur)
			p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
			p.Spec.Domains = append(p.Spec.Domains, model.Domain{Name: "boutique.fr", Role: "alias"})
			prov.Certificate = certificatEnAttente([]string{"shop.fr", "www.shop.fr"},
				"shop.fr", "www.shop.fr", "boutique.fr")
			c := string(buildMain(t, prov, p).Content)
			port80, port443 := blocs(t, moteur, c)
			if port443 == "" {
				t.Fatalf("le bloc 443 doit rester servi avec le certificat en place :\n%s", c)
			}
			for _, want := range []string{"certificates/shopfr/fullchain.pem", "Strict-Transport-Security"} {
				if !strings.Contains(port443, want) {
					t.Errorf("%q absent du bloc 443 :\n%s", want, port443)
				}
			}
			if cible := redirigeVers(moteur, port80); cible != "https://shop.fr" {
				t.Errorf("le port 80 doit rediriger vers un nom couvert, obtenu %q :\n%s", cible, port80)
			}
			// Le challenge du nouvel alias : servi sur le port 80, avant la
			// redirection, sans quoi l'émission qui doit le couvrir échoue.
			if !servi(moteur, port80, "boutique.fr") {
				t.Errorf("le port 80 doit servir boutique.fr pour son challenge :\n%s", port80)
			}
			challenge := "location ^~ /.well-known/acme-challenge/"
			if moteur == "apache" {
				challenge = `RewriteCond %{REQUEST_URI} !^/\.well-known/acme-challenge/`
				if !strings.Contains(port80, "Alias /.well-known/acme-challenge") {
					t.Errorf("le répertoire du challenge n'est pas aliasé :\n%s", port80)
				}
			}
			iChallenge := strings.Index(port80, challenge)
			iRedirect := strings.Index(port80, "https://shop.fr")
			if iChallenge < 0 || iChallenge > iRedirect {
				t.Errorf("le challenge doit échapper à la redirection :\n%s", port80)
			}
			// Le bloc 443 le déclare aussi : il le présentera au rechargement qui
			// suit l'émission, sans que le vhost change.
			if !servi(moteur, port443, "boutique.fr") {
				t.Errorf("le bloc 443 doit déclarer boutique.fr :\n%s", port443)
			}
			if steps := prov.PlanTLS(context.Background(), p); len(steps) != 0 {
				t.Errorf("le rendu ne dépend pas de l'émission, aucune bascule attendue : %v", steps)
			}
		})
	}
}

func TestChangementDeDomainePrincipalGardeLeHTTPSDeLAncien(t *testing.T) {
	// Le domaine principal change, l'ancien reste servi en alias : le certificat
	// en place ne couvre que lui. Le vhost reste en HTTPS sous l'ancien nom le
	// temps de l'émission, puis bascule sur le nouveau une fois le certificat
	// réémis.
	for _, moteur := range []string{"apache", "nginx"} {
		t.Run(moteur, func(t *testing.T) {
			prov, _ := fixture(t, moteur)
			p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
			p.Spec.Domains = []model.Domain{
				{Name: "boutique.fr", Role: "primary"},
				{Name: "shop.fr", Role: "alias"},
				{Name: "www.shop.fr", Role: "alias"},
			}
			demande := []string{"boutique.fr", "shop.fr", "www.shop.fr"}
			prov.Certificate = certificatEnAttente([]string{"shop.fr", "www.shop.fr"}, demande...)
			ctx := context.Background()

			bp := buildMain(t, prov, p)
			port80, port443 := blocs(t, moteur, string(bp.Content))
			if port443 == "" {
				t.Fatalf("le bloc 443 doit rester servi avec le certificat en place :\n%s", bp.Content)
			}
			if cible := redirigeVers(moteur, port80); cible != "https://shop.fr" {
				t.Errorf("le port 80 doit rediriger vers le nom que le certificat couvre, obtenu %q", cible)
			}
			if !servi(moteur, port80, "boutique.fr") {
				t.Errorf("le nouveau principal doit être servi sur le port 80 pour son challenge :\n%s", port80)
			}
			if err := prov.writeVHost(ctx, bp); err != nil {
				t.Fatal(err)
			}

			steps := prov.PlanTLS(ctx, p)
			if len(steps) != 1 || !strings.Contains(steps[0].Summary, "bascule du vhost shopfr") {
				t.Fatalf("une bascule attendue après l'émission : %v", steps)
			}
			// Émission échouée : le vhost en place reste, et l'étape le dit.
			err := steps[0].Apply(ctx)
			if err == nil || !strings.Contains(err.Error(), "sans boutique.fr : le vhost reste servi en HTTPS sous shop.fr") {
				t.Errorf("l'échec de l'émission doit être signalé : %v", err)
			}
			if steps[0].Critical {
				t.Error("la bascule doit rester non critique")
			}
			// Émission réussie : le nom déclaré reprend sa place.
			prov.Certificate = certificatCouvrant(demande...)
			if err := steps[0].Apply(ctx); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(bp.Path)
			if err != nil {
				t.Fatal(err)
			}
			port80, port443 = blocs(t, moteur, string(data))
			if cible := redirigeVers(moteur, port80); cible != "https://boutique.fr" {
				t.Errorf("après l'émission, le port 80 doit rediriger vers le principal, obtenu %q", cible)
			}
			if port443 == "" || !servi(moteur, port443, "shop.fr") {
				t.Errorf("l'ancien principal doit rester servi en HTTPS :\n%s", data)
			}
		})
	}
}

func TestRedirectionServieEnHTTPSSeulementSiLeCertificatLaCouvre(t *testing.T) {
	// Le vhost de redirection recevait le TLS avec le certificat du projet, qui
	// ne le couvrait pas : https://ancien-shop.fr rendait une erreur de nom. Son
	// bloc 443 ne doit exister que si le certificat présent couvre ce nom.
	for _, moteur := range []string{"apache", "nginx"} {
		t.Run(moteur, func(t *testing.T) {
			prov, paths := fixture(t, moteur)
			// Le certificat est celui du projet, pas un certificat au nom du vhost.
			prov.CertDir = func(ref string) string { return filepath.Join(paths.ACMEDir(), "certificates", ref) }
			p := project(t, "shop.fr", model.KindStatic, "", "")
			p.Spec.Domains = append(p.Spec.Domains,
				model.Domain{Name: "ancien-shop.fr", Role: "redirect", Target: "shop.fr"})
			demande := []string{"shop.fr", "www.shop.fr", "ancien-shop.fr"}
			prov.Certificate = certificatEnAttente([]string{"shop.fr", "www.shop.fr"}, demande...)
			vhosts := prov.vhosts(p)
			if len(vhosts) != 2 {
				t.Fatalf("un vhost de redirection attendu : %+v", vhosts)
			}
			bp, err := prov.Build(p, vhosts[1])
			if err != nil {
				t.Fatal(err)
			}
			port80, port443 := blocs(t, moteur, string(bp.Content))
			if port443 != "" {
				t.Errorf("aucun bloc 443 sans certificat qui couvre ancien-shop.fr :\n%s", bp.Content)
			}
			// La cible, elle, est couverte : autant y envoyer en https d'emblée.
			if cible := redirigeVers(moteur, port80); cible != "https://shop.fr" {
				t.Errorf("redirection du port 80 : %q", cible)
			}
			steps := prov.PlanTLS(context.Background(), p)
			if len(steps) != 1 || !strings.Contains(steps[0].Summary, "activation TLS du vhost shopfr_redirect") {
				t.Errorf("l'activation du TLS de la redirection suit l'émission : %v", steps)
			}

			prov.Certificate = certificatCouvrant(demande...)
			bp, err = prov.Build(p, vhosts[1])
			if err != nil {
				t.Fatal(err)
			}
			_, port443 = blocs(t, moteur, string(bp.Content))
			if port443 == "" || !servi(moteur, port443, "ancien-shop.fr") ||
				!strings.Contains(port443, "certificates/shopfr/fullchain.pem") {
				t.Errorf("couvert, le domaine redirigé doit être servi en HTTPS :\n%s", bp.Content)
			}
			if cible := redirigeVers(moteur, port443); cible != "https://shop.fr" {
				t.Errorf("redirection du port 443 : %q", cible)
			}
		})
	}
}

func TestPrincipalDevenuRedirectionAboutitPendantLEmission(t *testing.T) {
	// shop.fr, servi en HTTPS, devient une redirection vers boutique.fr. Tant que
	// le certificat ne couvre pas boutique.fr, rien ne le sert en HTTPS : la
	// redirection doit y envoyer en http, et non vers une erreur de nom.
	for _, moteur := range []string{"apache", "nginx"} {
		t.Run(moteur, func(t *testing.T) {
			prov, _ := fixture(t, moteur)
			p := project(t, "shop.fr", model.KindStatic, "", "")
			p.Spec.Domains = []model.Domain{
				{Name: "boutique.fr", Role: "primary"},
				{Name: "shop.fr", Role: "redirect", Target: "boutique.fr"},
			}
			demande := []string{"boutique.fr", "shop.fr"}
			prov.Certificate = certificatEnAttente([]string{"shop.fr"}, demande...)
			vhosts := prov.vhosts(p)
			bp, err := prov.Build(p, vhosts[1])
			if err != nil {
				t.Fatal(err)
			}
			port80, port443 := blocs(t, moteur, string(bp.Content))
			if port443 == "" {
				t.Fatalf("shop.fr reste couvert : son bloc 443 doit rester servi :\n%s", bp.Content)
			}
			for nom, bloc := range map[string]string{"80": port80, "443": port443} {
				if cible := redirigeVers(moteur, bloc); cible != "http://boutique.fr" {
					t.Errorf("port %s : la cible n'est pas encore servie en HTTPS, obtenu %q", nom, cible)
				}
			}
			// Les deux vhosts basculent après l'émission.
			if steps := prov.PlanTLS(context.Background(), p); len(steps) != 2 {
				t.Errorf("deux bascules attendues : %v", steps)
			}
		})
	}
}

func TestUneBasculeIncompleteReecritCeQueLeRepliACouvert(t *testing.T) {
	// Un site neuf dont la redirection est refusée : son vhost est écrit quand
	// rien n'est couvert, vers http://shop.fr. Le repli couvre shop.fr, pas la
	// redirection ; la bascule, qui visait le bloc 443 de celle-ci, abandonnait
	// sans réécrire le schéma, et le visiteur faisait un saut de plus jusqu'au
	// prochain apply.
	for _, moteur := range []string{"apache", "nginx"} {
		t.Run(moteur, func(t *testing.T) {
			prov, _ := fixture(t, moteur)
			ctx := context.Background()
			p := project(t, "shop.fr", model.KindStatic, "", "")
			p.Spec.Domains = append(p.Spec.Domains,
				model.Domain{Name: "ancien-shop.fr", Role: "redirect", Target: "shop.fr"})
			demande := []string{"shop.fr", "www.shop.fr", "ancien-shop.fr"}
			prov.Certificate = certificatEnAttente(nil, demande...)
			redirection := prov.vhosts(p)[1]
			bp, err := prov.Build(p, redirection)
			if err != nil {
				t.Fatal(err)
			}
			if cible := redirigeVers(moteur, string(bp.Content)); cible != "http://shop.fr" {
				t.Fatalf("rien n'est couvert, la cible est visée en http : %q", cible)
			}
			if err := prov.writeVHost(ctx, bp); err != nil {
				t.Fatal(err)
			}
			var bascule *engine.Step
			for _, s := range prov.PlanTLS(ctx, p) {
				if s.Resource == "web/vhost/shopfr_redirect/tls" {
					bascule = s
				}
			}
			if bascule == nil {
				t.Fatal("la redirection doit basculer après l'émission")
			}

			// L'émission s'est repliée sur les noms du vhost principal.
			prov.Certificate = certificatEnAttente([]string{"shop.fr", "www.shop.fr"}, demande...)
			err = bascule.Apply(ctx)
			if partial := (*engine.PartialError)(nil); !errors.As(err, &partial) ||
				!strings.Contains(err.Error(), "sans ancien-shop.fr") {
				t.Errorf("la bascule incomplète doit le dire, et garder son rechargement : %v", err)
			}
			data, err := os.ReadFile(bp.Path)
			if err != nil {
				t.Fatal(err)
			}
			port80, port443 := blocs(t, moteur, string(data))
			if cible := redirigeVers(moteur, port80); cible != "https://shop.fr" {
				t.Errorf("la cible couverte doit être visée en https, obtenu %q :\n%s", cible, data)
			}
			if port443 != "" {
				t.Errorf("la redirection non couverte n'a pas de bloc 443 :\n%s", data)
			}
			// Rien à réécrire : un échec simple, sans rechargement.
			err = bascule.Apply(ctx)
			if partial := (*engine.PartialError)(nil); err == nil || errors.As(err, &partial) {
				t.Errorf("un vhost inchangé ne demande aucun rechargement : %v", err)
			}
		})
	}
}

func TestUneCibleEtrangereEstViseeEnHTTPSQuoiQueCouvreLeCertificat(t *testing.T) {
	// Une redirection déclarée vers un site tiers : ce que couvre notre
	// certificat ne dit rien du schéma de la cible. En préproduction, un wildcard
	// qui ne couvre pas le nom redirigé l'envoyait en http.
	for _, moteur := range []string{"apache", "nginx"} {
		t.Run(moteur, func(t *testing.T) {
			prov, _ := fixture(t, moteur)
			p := project(t, "shop.fr", model.KindStatic, "", "")
			redirection := model.VHost{Name: "ancien", Template: "redirect", Domains: []string{"ancien.fr"},
				Options: map[string]any{"target": "autre-site.com"}}
			p.Spec.Web.VHosts = append(prov.vhosts(p), redirection)
			prov.Certificate = certificatCouvrant("shop.fr", "www.shop.fr")
			bp, err := prov.Build(p, redirection)
			if err != nil {
				t.Fatal(err)
			}
			port80, port443 := blocs(t, moteur, string(bp.Content))
			if port443 != "" {
				t.Errorf("ancien.fr n'est pas couvert : aucun bloc 443 attendu :\n%s", bp.Content)
			}
			if cible := redirigeVers(moteur, port80); cible != "https://autre-site.com" {
				t.Errorf("la cible étrangère doit être visée en https, obtenu %q", cible)
			}
			// Sans TLS voulu, le schéma reste http : c'est le monde que déclare le vhost.
			redirection.TLS = &model.TLS{Enabled: false}
			bp, err = prov.Build(p, redirection)
			if err != nil {
				t.Fatal(err)
			}
			if cible := redirigeVers(moteur, string(bp.Content)); cible != "http://autre-site.com" {
				t.Errorf("TLS écarté : cible %q", cible)
			}
		})
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
	prov.Certificate = certificatCouvrant("shop.fr", "www.shop.fr")
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
		"ReadWritePaths=-/var/log/ogs-projects/autrefr",
		"ReadWritePaths=-/var/log/ogs-projects/shopfr",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q attendu dans :\n%s", want, got)
		}
	}
	// L'ordre est déterministe, sans quoi le fichier oscillerait entre deux états
	// et chaque apply redémarrerait le serveur web.
	if strings.Index(got, "autrefr") > strings.Index(got, "shopfr") {
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

// Le complément d'unité est ce qui remonte les journaux des projets en
// ReadWritePaths (D59). Sa compensation supprimait le fichier : un déroulage
// rendait donc au moteur web l'unité durcie par sa distribution, où /home est en
// lecture seule pour le seul service, et le maître s'arrêtait au démarrage
// suivant — 226/NAMESPACE ou AH00019 — en emportant tout le parc.
func TestCompensationRendLeComplementPrecedent(t *testing.T) {
	p, _ := fixture(t, "apache")
	ctx := context.Background()
	projet := func(host string) *model.Project {
		t.Helper()
		proj := model.New(host, naming.EnvProd, model.KindStatic)
		if err := proj.Normalize("/home", "preprod-pp.net"); err != nil {
			t.Fatal(err)
		}
		return proj
	}
	a, b := projet("shop.fr"), projet("autre.fr")

	steps, err := p.PlanDropIn(ctx, []*model.Project{a})
	if err != nil {
		t.Fatal(err)
	}
	if err := steps[0].Apply(ctx); err != nil {
		t.Fatal(err)
	}
	avant, err := os.ReadFile(p.DropInPath())
	if err != nil {
		t.Fatal(err)
	}

	// Un projet de plus rend le complément obsolète.
	steps, err = p.PlanDropIn(ctx, []*model.Project{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Action != engine.Update {
		t.Fatalf("action %s, attendu une mise à jour", steps[0].Action)
	}
	if err := steps[0].Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if err := steps[0].Compensate(ctx); err != nil {
		t.Fatal(err)
	}
	après, err := os.ReadFile(p.DropInPath())
	if err != nil {
		t.Fatalf("la compensation ne doit pas supprimer le complément d'unité : %v", err)
	}
	if string(après) != string(avant) {
		t.Errorf("la compensation doit rendre le complément précédent :\n%s", après)
	}

	// À l'inverse, compenser une création laisse le système tel qu'elle l'a
	// trouvé : le fichier n'existait pas, il doit disparaître.
	neuf, _ := fixture(t, "apache")
	steps, err = neuf.PlanDropIn(ctx, []*model.Project{a})
	if err != nil {
		t.Fatal(err)
	}
	if err := steps[0].Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if err := steps[0].Compensate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(neuf.DropInPath()); !os.IsNotExist(err) {
		t.Errorf("la compensation d'une création doit retirer le complément : %v", err)
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

// Les surcharges de vhost (D63) : deux contextes par moteur, un répertoire par
// contexte, et le rendu qui fait foi.
func TestIncludeDirsPerEngineAndTemplate(t *testing.T) {
	cas := []struct {
		moteur, gabarit string
		kind            model.AppKind
		contextes       []string
	}{
		{"apache", "php-fpm", model.KindSymfony, []string{IncludeDocroot, IncludeVHost}},
		{"nginx", "php-fpm", model.KindSymfony, []string{IncludeDocroot, IncludeVHost}},
		{"apache", "static", model.KindStatic, []string{IncludeDocroot, IncludeVHost}},
		{"nginx", "static", model.KindStatic, []string{IncludeDocroot, IncludeVHost}},
		// Un vhost de proxy ne sert aucun répertoire : le contexte du docroot
		// n'existe pas pour lui, et nginx refuserait de toute façon un second
		// « location / » dans le même server.
		{"apache", "proxy", model.KindGo, []string{IncludeVHost}},
		{"nginx", "proxy", model.KindGo, []string{IncludeVHost}},
	}
	for _, c := range cas {
		prov, _ := fixture(t, c.moteur)
		php := ""
		if c.kind == model.KindSymfony {
			php = "8.5"
		}
		p := project(t, "shop.fr", c.kind, "", php)
		if c.kind == model.KindGo {
			p.Spec.Web.VHosts = prov.vhosts(p)
			p.Spec.Web.VHosts[0].Options = map[string]any{"proxyTarget": "127.0.0.1:20101"}
		}
		bp := buildMain(t, prov, p)
		var want []string
		for _, ctxName := range c.contextes {
			want = append(want, prov.IncludeDir("shopfr", ctxName))
		}
		if strings.Join(bp.IncludeDirs, " ") != strings.Join(want, " ") {
			t.Errorf("%s/%s : répertoires de surcharge %v, attendu %v",
				c.moteur, c.gabarit, bp.IncludeDirs, want)
		}
		// Le masque est ce qui rend l'inclusion facultative : sans lui, un
		// répertoire absent serait une erreur fatale au démarrage du moteur.
		for _, dir := range want {
			if !strings.Contains(string(bp.Content), dir+"/*.conf") {
				t.Errorf("%s/%s : %s inclus sans masque :\n%s",
					c.moteur, c.gabarit, dir, bp.Content)
			}
		}
	}
}

// Le contexte du docroot doit être inclus dans le bloc du répertoire servi, et
// celui du vhost au niveau du vhost : les confondre changerait la portée des
// directives déposées — un « add_header » dans un location de nginx annule tous
// ceux qu'il hérite du server.
func TestIncludeContextsAreNested(t *testing.T) {
	prov, _ := fixture(t, "nginx")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	bp := buildMain(t, prov, p)
	docroot := prov.IncludeDir("shopfr", IncludeDocroot)
	vus := 0
	for _, line := range strings.Split(string(bp.Content), "\n") {
		if !strings.Contains(line, docroot) {
			continue
		}
		vus++
		// Huit espaces : le second niveau d'indentation, celui d'un location.
		if !strings.HasPrefix(line, "        include ") {
			t.Errorf("l'inclusion du docroot n'est pas dans un location : %q", line)
		}
	}
	if vus != 1 {
		t.Fatalf("%d inclusion(s) du docroot, une seule attendue :\n%s", vus, bp.Content)
	}

	prov, _ = fixture(t, "apache")
	bp = buildMain(t, prov, p)
	c := string(bp.Content)
	directory := c[strings.Index(c, "<Directory "+p.Spec.Layout.Docroot):]
	directory = directory[:strings.Index(directory, "</Directory>")]
	if !strings.Contains(directory, prov.IncludeDir("shopfr", IncludeDocroot)) {
		t.Errorf("l'inclusion du docroot n'est pas dans le <Directory> :\n%s", directory)
	}
	if strings.Contains(directory, prov.IncludeDir("shopfr", IncludeVHost)) {
		t.Error("l'inclusion du vhost ne doit pas être enfermée dans le <Directory>")
	}
}

// Un vhost de redirection n'a rien à personnaliser, et son unique server porte
// le challenge ACME : un « rewrite » déposé là empêcherait tout renouvellement.
func TestRedirectVHostHasNoIncludes(t *testing.T) {
	for _, moteur := range []string{"apache", "nginx"} {
		prov, _ := fixture(t, moteur)
		p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
		p.Spec.Domains = append(p.Spec.Domains,
			model.Domain{Name: "ancien-shop.fr", Role: "redirect", Target: "shop.fr"})
		bp, err := prov.Build(p, prov.vhosts(p)[1])
		if err != nil {
			t.Fatal(err)
		}
		if len(bp.IncludeDirs) != 0 {
			t.Errorf("%s : le vhost de redirection ne doit inclure aucune surcharge : %v",
				moteur, bp.IncludeDirs)
		}
	}
}

// Les répertoires sont créés par l'apply, en 0750 : leur contenu est lu en root
// par le processus maître.
func TestIncludeDirsCreatedByApply(t *testing.T) {
	prov, _ := fixture(t, "nginx")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	ctx := context.Background()
	apply := func() *engine.Plan {
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
		if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
			t.Fatal(err)
		}
		return plan
	}
	apply()
	for _, ctxName := range []string{IncludeVHost, IncludeDocroot} {
		dir := prov.IncludeDir("shopfr", ctxName)
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("répertoire de surcharge non créé : %v", err)
		}
		if info.Mode().Perm() != 0o750 {
			t.Errorf("%s en %04o, 0750 attendu", dir, info.Mode().Perm())
		}
	}

	// Un fichier déposé ne fait pas dériver le vhost : l'empreinte porte sur le
	// corps du fichier généré, que la surcharge ne touche pas.
	surcharge := filepath.Join(prov.IncludeDir("shopfr", IncludeVHost), "10-essai.conf")
	if err := os.WriteFile(surcharge, []byte("client_max_body_size 200M;\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if plan := apply(); !plan.Empty() {
		t.Errorf("une surcharge déposée fait dériver le plan :\n%s", plan.String())
	}
	if _, err := os.Stat(surcharge); err != nil {
		t.Errorf("la surcharge a été emportée par l'apply : %v", err)
	}
}

// L'audit voit ce qu'aucune vérification de configuration ne voit : un fichier
// hors de tout contexte, et un répertoire qu'aucun vhost ne référence plus.
func TestAuditIncludesFindsIgnoredFiles(t *testing.T) {
	prov, _ := fixture(t, "nginx")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	ctx := context.Background()
	obs, _ := prov.Observe(ctx, p)
	steps, err := prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	plan := &engine.Plan{Project: p.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	if got := prov.AuditIncludes(); len(got) != 0 {
		t.Fatalf("audit non vide sur une installation conforme : %+v", got)
	}

	perdu := filepath.Join(prov.Engine.IncludeRoot(), "shopfr", "perdu.conf")
	if err := os.WriteFile(perdu, []byte("# jamais chargé\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	orphelin := filepath.Join(prov.Engine.IncludeRoot(), "disparu", IncludeVHost)
	if err := os.MkdirAll(orphelin, 0o750); err != nil {
		t.Fatal(err)
	}
	var details []string
	for _, pb := range prov.AuditIncludes() {
		details = append(details, pb.Detail)
		if pb.Severe {
			t.Errorf("un fichier ignoré n'est pas une élévation de privilège : %s", pb.Detail)
		}
	}
	joint := strings.Join(details, "\n")
	if !strings.Contains(joint, perdu) {
		t.Errorf("le fichier hors contexte n'est pas signalé :\n%s", joint)
	}
	if !strings.Contains(joint, filepath.Dir(orphelin)) {
		t.Errorf("le répertoire orphelin n'est pas signalé :\n%s", joint)
	}
	// Le répertoire entier étant orphelin, ses contextes ne sont pas détaillés :
	// ce serait répéter la même chose autant de fois qu'il en contient.
	if strings.Contains(joint, orphelin) {
		t.Errorf("le contexte d'un répertoire orphelin est détaillé en doublon :\n%s", joint)
	}
}

// La suppression d'un projet emporte les répertoires vides et laisse le reste :
// un fichier déposé là n'a pas été écrit par ogs.
func TestRemoveIncludesKeepsWhatWasDeposited(t *testing.T) {
	prov, _ := fixture(t, "apache")
	root := prov.Engine.IncludeRoot()
	garni := filepath.Join(root, "shopfr", IncludeVHost)
	vide := filepath.Join(root, "shopfr", IncludeDocroot)
	for _, d := range []string{garni, vide} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(garni, "10-essai.conf"), []byte("# ici\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	kept := prov.RemoveIncludes("shopfr")
	if len(kept) != 1 || kept[0] != garni {
		t.Fatalf("conservés %v, attendu [%s]", kept, garni)
	}
	if _, err := os.Stat(vide); !os.IsNotExist(err) {
		t.Errorf("le répertoire vide devait partir : %v", err)
	}
	if _, err := os.Stat(filepath.Join(garni, "10-essai.conf")); err != nil {
		t.Errorf("le fichier déposé a été emporté : %v", err)
	}
	// Rien à supprimer : l'appel doit rester sans effet et sans erreur.
	if kept := prov.RemoveIncludes("inconnu"); kept != nil {
		t.Errorf("un vhost sans surcharge ne conserve rien : %v", kept)
	}
}

func TestVHostDeSupervisionExposeChaquePoolSansSonSocket(t *testing.T) {
	for _, engineName := range []string{"apache", "nginx"} {
		t.Run(engineName, func(t *testing.T) {
			prov, paths := fixture(t, engineName)
			shop := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
			steps, err := prov.PlanStatus(context.Background(), []*model.Project{shop},
				"127.0.0.1:8099", "/ogs-status", true)
			if err != nil {
				t.Fatal(err)
			}
			if len(steps) == 0 {
				t.Fatal("aucune étape produite pour le vhost de supervision")
			}
			for _, s := range steps {
				if s.Apply != nil {
					if err := s.Apply(context.Background()); err != nil {
						t.Fatalf("%s : %v", s.Resource, err)
					}
				}
			}
			path := filepath.Join(prov.Engine.AvailableDir(), prov.Engine.FileName(StatusVHostName))
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			c := string(content)
			if !strings.Contains(c, "127.0.0.1:8099") {
				t.Errorf("le vhost doit écouter sur la boucle locale :\n%s", c)
			}
			if !strings.Contains(c, "/ogs-status/fpm/shopfr") {
				t.Errorf("le pool du projet doit être exposé :\n%s", c)
			}
			// Le socket figure dans la directive de relais — c'est le serveur web qui
			// le détient —, mais le pool n'est joignable que par le chemin de statut :
			// l'agent n'obtient jamais le socket lui-même.
			if !strings.Contains(c, paths.PHPSocket("8.5", 18501, "shopfr")) {
				t.Errorf("le relais vers le pool est attendu :\n%s", c)
			}
			if !strings.Contains(c, config.PoolStatusPath) {
				t.Errorf("le chemin de statut du pool est attendu :\n%s", c)
			}
		})
	}
}

func TestVHostDeSupervisionRetireQuandDesactive(t *testing.T) {
	prov, _ := fixture(t, "nginx")
	shop := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	steps, err := prov.PlanStatus(context.Background(), []*model.Project{shop},
		"127.0.0.1:8099", "/ogs-status", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Apply != nil {
			if err := s.Apply(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	path := filepath.Join(prov.Engine.AvailableDir(), prov.Engine.FileName(StatusVHostName))
	if _, err := os.Stat(path); err != nil {
		t.Fatal("le vhost devait être écrit")
	}

	off, err := prov.PlanStatus(context.Background(), []*model.Project{shop},
		"127.0.0.1:8099", "/ogs-status", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(off) != 1 {
		t.Fatalf("une étape de retrait attendue, %d obtenue(s)", len(off))
	}
	if err := off[0].Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("le vhost de supervision doit disparaître lorsqu'il est désactivé")
	}
}

// Désactivé, le vhost de supervision suit le régime de son écriture : retouché,
// il n'est retiré, lien compris, que sous --force et sauvegardé.
func TestVHostDeSupervisionRetoucheNEstRetireQueSousForce(t *testing.T) {
	prov, paths := fixture(t, "nginx")
	shop := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	ctx := context.Background()
	steps, err := prov.PlanStatus(ctx, []*model.Project{shop}, "127.0.0.1:8099", "/ogs-status", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Apply != nil {
			if err := s.Apply(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	path := filepath.Join(prov.Engine.AvailableDir(), prov.Engine.FileName(StatusVHostName))
	lien := filepath.Join(prov.Engine.EnabledDir(), prov.Engine.FileName(StatusVHostName))
	data, _ := os.ReadFile(path)
	retouche := append(data, []byte("# accès ouvert au poste d'astreinte\n")...)
	if err := os.WriteFile(path, retouche, 0o644); err != nil {
		t.Fatal(err)
	}

	off, err := prov.PlanStatus(ctx, []*model.Project{shop}, "127.0.0.1:8099", "/ogs-status", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(off) != 1 || !strings.Contains(off[0].Summary, "modifié hors ogs") {
		t.Fatalf("le retrait doit être refusé : %+v", off)
	}
	if err := off[0].Apply(ctx); err == nil {
		t.Error("le retrait d'un vhost retouché doit être refusé sans --force")
	}
	if got, _ := os.ReadFile(path); string(got) != string(retouche) {
		t.Fatal("le vhost retouché a été retiré sans --force")
	}

	prov.Force = true
	off, err = prov.PlanStatus(ctx, []*model.Project{shop}, "127.0.0.1:8099", "/ogs-status", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := off[0].Apply(ctx); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path, lien} {
		if _, err := os.Lstat(f); !os.IsNotExist(err) {
			t.Errorf("%s devait être retiré sous --force : %v", f, err)
		}
	}
	if baks, _ := filepath.Glob(filepath.Join(paths.BackupsDir(), filepath.Base(path)+".ogs-bak-*")); len(baks) != 1 {
		t.Errorf("une sauvegarde attendue, %d trouvée(s)", len(baks))
	}
}

func TestVHostDeSupervisionSansPoolResteValide(t *testing.T) {
	prov, _ := fixture(t, "apache")
	steps, err := prov.PlanStatus(context.Background(), nil, "127.0.0.1:8099", "/ogs-status", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Apply != nil {
			if err := s.Apply(context.Background()); err != nil {
				t.Fatalf("un parc sans pool doit produire un vhost valide : %v", err)
			}
		}
	}
}

// etapeEnEchec imite l'étape critique qui suit les vhosts dans un plan réel —
// sudoers, dont l'apply passe par visudo — et fait dérouler la saga.
func etapeEnEchec() *engine.Step {
	s := engine.NewStep("essai/echec", engine.Create, "étape critique en échec")
	s.Apply = func(context.Context) error { return errors.New("visudo a refusé le fichier") }
	return s
}

func TestCompensationDUnVHostMisAJourRendLAncienContenu(t *testing.T) {
	for _, engineName := range []string{"apache", "nginx"} {
		t.Run(engineName, func(t *testing.T) {
			prov, _ := fixture(t, engineName)
			p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
			ctx := context.Background()

			// Le vhost tel qu'un apply précédent l'a laissé : écrit et activé.
			bp := buildMain(t, prov, p)
			if err := fs.WriteAtomic(bp.Path, bp.Content, 0o644, fs.KeepOwner); err != nil {
				t.Fatal(err)
			}
			if err := prov.enable(ctx, bp); err != nil {
				t.Fatal(err)
			}
			origine := string(bp.Content)

			// Un alias de plus, et le vhost passe en écart : l'étape sera une mise à
			// jour, et l'activation un NoOp puisque le lien est déjà là.
			p.Spec.Domains = append(p.Spec.Domains, model.Domain{Name: "boutique.shop.fr", Role: "alias"})
			maj := buildMain(t, prov, p)
			if maj.State != fs.StateOutdated {
				t.Fatalf("état attendu « à mettre à jour », obtenu « %s »", maj.State)
			}

			plan := &engine.Plan{Project: p.ID()}
			for _, s := range prov.vhostStep(p, maj) {
				plan.Add(s)
			}
			plan.Add(etapeEnEchec())

			report, err := engine.Execute(ctx, plan, engine.Options{})
			if err == nil {
				t.Fatal("l'étape critique devait faire échouer l'exécution")
			}
			if !report.RolledBack {
				t.Fatal("la saga devait être déroulée")
			}
			if report.RollbackErr != nil {
				t.Fatalf("compensation en échec : %v", report.RollbackErr)
			}

			apres, err := os.ReadFile(maj.Path)
			if err != nil {
				t.Fatalf("le vhost a été supprimé au lieu d'être restauré : %v", err)
			}
			if string(apres) != origine {
				t.Errorf("le vhost n'a pas retrouvé son contenu d'origine :\n%s", apres)
			}
			// Stat suit le lien : il échoue si la cible a disparu. Un lien mort dans
			// sites-enabled empêche nginx de démarrer, tout le parc avec lui.
			if _, err := os.Stat(maj.EnabledPath); err != nil {
				t.Errorf("le lien d'activation pointe dans le vide : %v", err)
			}
		})
	}
}

func TestCompensationDuVHostDeSupervisionRendLAncienContenu(t *testing.T) {
	prov, _ := fixture(t, "nginx")
	shop := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	ctx := context.Background()

	steps, err := prov.PlanStatus(ctx, []*model.Project{shop}, "127.0.0.1:8099", "/ogs-status", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Apply != nil {
			if err := s.Apply(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	path := filepath.Join(prov.Engine.AvailableDir(), prov.Engine.FileName(StatusVHostName))
	enabledPath := filepath.Join(prov.Engine.EnabledDir(), prov.Engine.FileName(StatusVHostName))
	origine, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Le préfixe change : le vhost passe en mise à jour, son lien reste en place.
	maj, err := prov.PlanStatus(ctx, []*model.Project{shop}, "127.0.0.1:8099", "/supervision", true)
	if err != nil {
		t.Fatal(err)
	}
	plan := &engine.Plan{Project: "parc"}
	for _, s := range maj {
		plan.Add(s)
	}
	if plan.Empty() {
		t.Fatal("le changement de préfixe devait produire une mise à jour")
	}
	// Dans le plan global, Monitoring.Plan suit le vhost de supervision et ses
	// étapes sont critiques : un fragment Telegraf impossible à écrire déroule.
	plan.Add(etapeEnEchec())

	report, err := engine.Execute(ctx, plan, engine.Options{})
	if err == nil {
		t.Fatal("l'étape critique devait faire échouer l'exécution")
	}
	if report.RollbackErr != nil {
		t.Fatalf("compensation en échec : %v", report.RollbackErr)
	}

	apres, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("le vhost de supervision a été supprimé au lieu d'être restauré : %v", err)
	}
	if string(apres) != string(origine) {
		t.Errorf("le vhost de supervision n'a pas retrouvé son contenu d'origine :\n%s", apres)
	}
	if _, err := os.Stat(enabledPath); err != nil {
		t.Errorf("le lien d'activation pointe dans le vide : %v", err)
	}
}

// blocDePool isole la portion du vhost de supervision consacrée à un pool. Les
// deux templates la font précéder du même commentaire.
func blocDePool(t *testing.T, contenu, pool string) string {
	t.Helper()
	for _, seg := range strings.Split(contenu, "# pool ")[1:] {
		if strings.HasPrefix(seg, pool+" ") {
			return seg
		}
	}
	t.Fatalf("aucun bloc pour le pool %s :\n%s", pool, contenu)
	return ""
}

// rendStatus applique le vhost de supervision et rend son contenu.
func rendStatus(t *testing.T, prov *Provider, projects []*model.Project) string {
	t.Helper()
	ctx := context.Background()
	steps, err := prov.PlanStatus(ctx, projects, "127.0.0.1:8099", "/ogs-status", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Apply != nil {
			if err := s.Apply(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	c, err := os.ReadFile(filepath.Join(prov.Engine.AvailableDir(), prov.Engine.FileName(StatusVHostName)))
	if err != nil {
		t.Fatal(err)
	}
	return string(c)
}

func TestVHostDeSupervisionSuitLeStatutDeChaquePool(t *testing.T) {
	for _, engineName := range []string{"apache", "nginx"} {
		t.Run(engineName, func(t *testing.T) {
			prov, _ := fixture(t, engineName)
			// « pm.status_path » est un réglage de base, que la déclaration du projet
			// surcharge. FPM compare SCRIPT_NAME à la valeur qu'il a retenue : le
			// vhost, qui force cette variable, doit forcer celle du pool.
			shop := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
			shop.Spec.PHP.Pools[0].Settings = map[string]string{
				"pm.status_path": "/ailleurs",
				"ping.path":      "/ailleurs-ping",
			}
			blog := project(t, "blog.fr", model.KindSymfony, "6.4", "8.5")

			c := rendStatus(t, prov, []*model.Project{shop, blog})
			surcharge := blocDePool(t, c, "shopfr")
			defaut := blocDePool(t, c, "blogfr")

			for _, want := range []string{"/ailleurs", "/ailleurs-ping"} {
				if !strings.Contains(surcharge, want) {
					t.Errorf("le pool surchargé doit être interrogé sur %s :\n%s", want, surcharge)
				}
			}
			if strings.Contains(surcharge, config.PoolStatusPath) {
				t.Errorf("le chemin du parc n'a rien à faire dans le bloc du pool surchargé :\n%s", surcharge)
			}
			// Le pool voisin, lui, garde les chemins du parc.
			for _, want := range []string{config.PoolStatusPath, config.PoolPingPath} {
				if !strings.Contains(defaut, want) {
					t.Errorf("le pool sans surcharge doit garder %s :\n%s", want, defaut)
				}
			}
			if strings.Contains(defaut, "/ailleurs") {
				t.Errorf("la surcharge d'un pool a débordé sur un autre :\n%s", defaut)
			}
		})
	}
}

func TestVHostDeSupervisionRefuseUnStatutInjectable(t *testing.T) {
	prov, _ := fixture(t, "nginx")
	shop := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	// Le chemin vient du fichier d'un projet et le vhost est de portée serveur :
	// une directive glissée là serait chargée en root, et une directive dupliquée
	// est fatale à nginx. Une valeur impossible à écrire sans risque retombe sur
	// le défaut du parc — une métrique manquante, jamais un moteur à terre.
	shop.Spec.PHP.Pools[0].Settings = map[string]string{
		"pm.status_path": "/x;\n    deny all",
	}
	c := rendStatus(t, prov, []*model.Project{shop})
	bloc := blocDePool(t, c, "shopfr")
	if strings.Contains(bloc, "deny all") {
		t.Errorf("une directive a été injectée dans le vhost de supervision :\n%s", bloc)
	}
	if !strings.Contains(bloc, config.PoolStatusPath) {
		t.Errorf("le chemin refusé devait retomber sur le défaut du parc :\n%s", bloc)
	}
}

func TestPlanRefuseEtForceUnVHostModifieAlaMain(t *testing.T) {
	prov, paths := fixture(t, "apache")
	p := project(t, "shop.fr", model.KindSymfony, "6.4", "8.5")
	ctx := context.Background()
	obs, _ := prov.Observe(ctx, p)
	steps, err := prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	plan := &engine.Plan{Project: p.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}

	vhost := filepath.Join(prov.Engine.AvailableDir(), "shopfr.conf")
	data, _ := os.ReadFile(vhost)
	const ajout = "\n# réglage posé à la main\n"
	if err := os.WriteFile(vhost, append(data, []byte(ajout)...), 0o644); err != nil {
		t.Fatal(err)
	}

	// Sans --force : refus, fichier intact, aucune sauvegarde.
	obs, _ = prov.Observe(ctx, p)
	steps, _ = prov.Plan(ctx, p, obs)
	var refus *engine.Step
	for _, s := range steps {
		if strings.Contains(s.Summary, "modifié hors ogs") {
			refus = s
		}
	}
	if refus == nil {
		t.Fatal("la modification manuelle doit être détectée")
	}
	if err := refus.Apply(ctx); err == nil {
		t.Error("l'écrasement doit être refusé sans --force")
	}
	if intact, _ := os.ReadFile(vhost); !strings.Contains(string(intact), "posé à la main") {
		t.Error("le refus ne doit rien écrire")
	}

	// Avec --force : mise à jour ordinaire, précédée de la sauvegarde.
	prov.Force = true
	obs, _ = prov.Observe(ctx, p)
	steps, err = prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	var forcée *engine.Step
	for _, s := range steps {
		if strings.Contains(s.Summary, "écrasement forcé") {
			forcée = s
		}
	}
	if forcée == nil {
		t.Fatal("le plan doit annoncer l'écrasement forcé")
	}
	// Le rechargement du moteur reste demandé : le chemin forcé retombe sur
	// l'écriture ordinaire plutôt que d'en dupliquer une seconde.
	if len(forcée.Reload) != 1 || forcée.Reload[0] != "web:apache" {
		t.Errorf("l'écrasement forcé doit porter le rechargement : %v", forcée.Reload)
	}
	if err := forcée.Apply(ctx); err != nil {
		t.Fatalf("l'écrasement forcé doit réussir : %v", err)
	}
	if after, _ := os.ReadFile(vhost); strings.Contains(string(after), "posé à la main") {
		t.Error("le vhost doit avoir été réécrit")
	}
	// La sauvegarde sort du répertoire de configuration : Apache charge
	// sites-available tout entier par l'inclusion, et l'importateur le lit.
	if next, _ := filepath.Glob(vhost + ".ogs-bak-*"); len(next) != 0 {
		t.Errorf("la sauvegarde ne doit pas rester dans sites-available : %v", next)
	}
	baks, _ := filepath.Glob(filepath.Join(paths.BackupsDir(), filepath.Base(vhost)+".ogs-bak-*"))
	if len(baks) != 1 {
		t.Fatalf("une sauvegarde attendue, %d trouvée(s)", len(baks))
	}
	if saved, _ := os.ReadFile(baks[0]); !strings.Contains(string(saved), "posé à la main") {
		t.Error("la sauvegarde doit contenir l'original")
	}

	// La sauvegarde porte le ServerName du vhost écrasé : prise pour une
	// configuration étrangère, elle mettrait le projet en conflit avec lui-même
	// et l'apply suivant refuserait de planifier.
	prov.Force = false
	obs, err = prov.Observe(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.Conflicts) != 0 {
		t.Errorf("la sauvegarde ne doit pas passer pour un vhost concurrent : %v", obs.Conflicts)
	}
	if len(obs.Stale) != 0 {
		t.Errorf("la sauvegarde ne doit pas être prise pour un vhost obsolète : %v", obs.Stale)
	}
	steps, err = prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatalf("l'apply suivant doit rester planifiable : %v", err)
	}
	plan2 := &engine.Plan{Project: p.ID()}
	for _, s := range steps {
		plan2.Add(s)
	}
	if !plan2.Empty() {
		t.Errorf("le plan suivant doit être vide :\n%s", plan2.String())
	}
}

// moteurRefusant se déclare non simulé : validate() court-circuite sur un
// exécuteur simulé, et le chemin d'échec de la revalidation ne serait pas éprouvé.
type moteurRefusant struct{ *exec.Recorder }

func (m *moteurRefusant) Simulated() bool { return false }

// refuserLaValidation installe un binaire de moteur factice et fait échouer sa
// vérification de configuration, comme le ferait une invalidité étrangère au
// fichier qu'ogs vient d'écrire.
func refuserLaValidation(t *testing.T, prov *Provider) {
	t.Helper()
	bin, args := prov.Engine.ValidateArgs()
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := exec.NewRecorder()
	rec.Errors[bin+" "+args[0]] = errors.New("configuration refusée : cause étrangère au fragment")
	prov.Exec = &moteurRefusant{rec}
}

// poserLeFragment dépose un fragment de portée serveur antérieur et rend son corps.
// poserLeFragment écrit un fragment tel qu'une version antérieure d'ogs l'aurait
// laissé : marqué, donc « à mettre à jour » et non « non géré par ogs ». La
// distinction décide de tout depuis que le fragment suit le régime des cinq
// états — un fichier sans marque est refusé, et l'étape n'écrit plus rien.
func poserLeFragment(t *testing.T, prov *Provider) []byte {
	t.Helper()
	chemin := prov.Engine.SnippetPath()
	if err := os.MkdirAll(filepath.Dir(chemin), 0o755); err != nil {
		t.Fatal(err)
	}
	origine := fs.Render(fs.Header{
		Project: "parc", Resource: "web/snippet", Generated: time.Now().Add(-time.Hour),
	}, []byte("map $http_upgrade $connection_upgrade { default upgrade; '' close; }\n"), "#")
	if err := os.WriteFile(chemin, origine, 0o644); err != nil {
		t.Fatal(err)
	}
	return origine
}

// Le fragment de portée serveur porte la « map $connection_upgrade » dont dépend
// tout vhost de proxy du parc. Un exploitant peut légitimement y avoir ajouté un
// réglage : l'écraser en silence le lui retirerait sans un mot, d'où le régime
// des cinq états et sa porte de sortie (§6.4).
func TestFragmentRetoucheALaMainRefusePuisEcraseAvecForce(t *testing.T) {
	prov, _ := fixture(t, "nginx")
	chemin := prov.Engine.SnippetPath()
	if err := os.MkdirAll(filepath.Dir(chemin), 0o755); err != nil {
		t.Fatal(err)
	}
	retouche := []byte("# posé à la main par l'exploitant\nclient_max_body_size 64m;\n")
	if err := os.WriteFile(chemin, retouche, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := prov.PlanSnippet()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(context.Background()); err == nil {
		t.Fatal("un fragment non géré par ogs doit être refusé")
	}
	if apres, _ := os.ReadFile(chemin); string(apres) != string(retouche) {
		t.Errorf("le refus ne doit rien écrire :\n%s", apres)
	}

	prov.Force = true
	s, err = prov.PlanSnippet()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(context.Background()); err != nil {
		t.Fatalf("--force doit écraser : %v", err)
	}
	apres, _ := os.ReadFile(chemin)
	if !strings.Contains(string(apres), fs.Marker) {
		t.Errorf("le fragment devait être réécrit par ogs :\n%s", apres)
	}
	// L'original part dans le répertoire des sauvegardes, jamais à côté : le
	// répertoire des fragments est chargé en entier par le moteur.
	copies, _ := filepath.Glob(filepath.Join(prov.Paths.BackupsDir(), "*.ogs-bak-*"))
	if len(copies) != 1 {
		t.Fatalf("une sauvegarde attendue, %d trouvée(s)", len(copies))
	}
	if data, _ := os.ReadFile(copies[0]); string(data) != string(retouche) {
		t.Errorf("la sauvegarde ne porte pas l'original :\n%s", data)
	}
	if voisines, _ := filepath.Glob(chemin + ".ogs-bak-*"); len(voisines) != 0 {
		t.Errorf("aucune copie ne doit rester dans le répertoire des fragments : %v", voisines)
	}
}

// La revalidation qui suit l'écriture du fragment porte sur la configuration
// entière : elle échoue pour une invalidité étrangère au fragment aussi bien que
// pour le fragment lui-même. Le supprimer alors emporte la « map $connection_upgrade »
// dont dépend tout vhost de proxy du parc, et nginx refuse ensuite de démarrer.
func TestUnFragmentRefuseParLeMoteurRetrouveSonContenuPrecedent(t *testing.T) {
	prov, _ := fixture(t, "nginx")
	origine := poserLeFragment(t, prov)
	refuserLaValidation(t, prov)

	s, err := prov.PlanSnippet()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(context.Background()); err == nil {
		t.Fatal("une configuration refusée doit faire échouer l'étape")
	}
	apres, err := os.ReadFile(prov.Engine.SnippetPath())
	if err != nil {
		t.Fatalf("le fragment a été supprimé au lieu d'être restauré : %v", err)
	}
	if string(apres) != string(origine) {
		t.Errorf("le fragment n'a pas retrouvé son contenu d'origine :\n%s", apres)
	}
}

// L'étape du fragment figure désormais dans le plan de chaque projet : elle est
// soumise au déroulage de sa saga, et la compensation doit rendre le fragment
// précédent — pas le supprimer.
func TestCompensationDuFragmentDePorteeServeurRendLAncienContenu(t *testing.T) {
	t.Run("fragment présent", func(t *testing.T) {
		prov, _ := fixture(t, "nginx")
		origine := poserLeFragment(t, prov)

		plan := &engine.Plan{Project: "shopfr"}
		s, err := prov.PlanSnippet()
		if err != nil {
			t.Fatal(err)
		}
		plan.Add(s)
		plan.Add(etapeEnEchec())

		report, err := engine.Execute(context.Background(), plan, engine.Options{})
		if err == nil {
			t.Fatal("l'étape critique devait faire échouer l'exécution")
		}
		if !report.RolledBack {
			t.Fatal("la saga devait être déroulée")
		}
		if report.RollbackErr != nil {
			t.Fatalf("compensation en échec : %v", report.RollbackErr)
		}
		apres, err := os.ReadFile(prov.Engine.SnippetPath())
		if err != nil {
			t.Fatalf("le fragment a été supprimé au lieu d'être restauré : %v", err)
		}
		if string(apres) != string(origine) {
			t.Errorf("le fragment n'a pas retrouvé son contenu d'origine :\n%s", apres)
		}
	})

	// L'autre bout de l'invariant : la suppression n'est juste que si le fichier
	// était absent avant l'étape.
	t.Run("fragment absent", func(t *testing.T) {
		prov, _ := fixture(t, "nginx")
		plan := &engine.Plan{Project: "shopfr"}
		s, err := prov.PlanSnippet()
		if err != nil {
			t.Fatal(err)
		}
		plan.Add(s)
		plan.Add(etapeEnEchec())

		report, err := engine.Execute(context.Background(), plan, engine.Options{})
		if err == nil {
			t.Fatal("l'étape critique devait faire échouer l'exécution")
		}
		if report.RollbackErr != nil {
			t.Fatalf("compensation en échec : %v", report.RollbackErr)
		}
		if _, err := os.Stat(prov.Engine.SnippetPath()); !os.IsNotExist(err) {
			t.Error("un fragment créé par l'étape doit disparaître au déroulage")
		}
	})
}

// La compensation d'une suppression de vhost doit rendre le lien d'activation
// autant que le fichier : ni Apache ni nginx ne chargent sites-available, et la
// saga annoncerait un retour à l'état initial en laissant le domaine sans service.
func TestCompensationDUneSuppressionDeVHostRetablitLeLienDActivation(t *testing.T) {
	for _, engineName := range []string{"apache", "nginx"} {
		t.Run(engineName, func(t *testing.T) {
			prov, _ := fixture(t, engineName)
			nom := prov.Engine.FileName("shopfr_redir")
			chemin := filepath.Join(prov.Engine.AvailableDir(), nom)
			origine := vhostEcritPour("shopfr", "# ancien vhost de redirection\n")
			if err := fs.WriteAtomic(chemin, origine, 0o644, fs.KeepOwner); err != nil {
				t.Fatal(err)
			}
			lien := filepath.Join(prov.Engine.EnabledDir(), nom)
			rel, err := filepath.Rel(prov.Engine.EnabledDir(), chemin)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(rel, lien); err != nil {
				t.Fatal(err)
			}

			retrait, err := prov.removeStep(chemin, "shopfr")
			if err != nil {
				t.Fatal(err)
			}
			if retrait == nil || !retrait.Critical {
				t.Fatalf("un retrait ordinaire est attendu : %+v", retrait)
			}
			plan := &engine.Plan{Project: "shopfr"}
			plan.Add(retrait)
			plan.Add(etapeEnEchec())

			report, err := engine.Execute(context.Background(), plan, engine.Options{})
			if err == nil {
				t.Fatal("l'étape critique devait faire échouer l'exécution")
			}
			if !report.RolledBack {
				t.Fatal("la saga devait être déroulée")
			}
			if report.RollbackErr != nil {
				t.Fatalf("compensation en échec : %v", report.RollbackErr)
			}
			apres, err := os.ReadFile(chemin)
			if err != nil {
				t.Fatalf("le vhost n'a pas été restauré : %v", err)
			}
			if string(apres) != string(origine) {
				t.Errorf("le vhost n'a pas retrouvé son contenu d'origine :\n%s", apres)
			}
			cible, err := os.Readlink(lien)
			if err != nil {
				t.Fatalf("le vhost est restauré mais n'est plus activé : %v", err)
			}
			if cible != rel {
				t.Errorf("la cible du lien a changé : %q au lieu de %q", cible, rel)
			}
			// Stat suit le lien : il échoue si la cible manque. Nginx tient
			// l'inclusion d'un lien mort pour fatale, tout le parc avec lui.
			if _, err := os.Stat(lien); err != nil {
				t.Errorf("le lien d'activation pointe dans le vide : %v", err)
			}
		})
	}
}

// Sous logs.root (D71), le complément ne porte qu'une entrée, fixe : créer un
// projet ne change plus son contenu, donc ne redémarre plus le serveur web.
func TestPlanDropInSousLaRacineDesJournaux(t *testing.T) {
	p, _ := fixture(t, "apache")
	p.LogsRoot = "/var/log/ogs-projects"
	a := model.New("shop.fr", naming.EnvProd, model.KindStatic)
	if err := a.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	b := model.New("autre.fr", naming.EnvProd, model.KindStatic)
	if err := b.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	render := func(projects ...*model.Project) string {
		steps, err := p.PlanDropIn(context.Background(), projects)
		if err != nil {
			t.Fatal(err)
		}
		if err := steps[0].Apply(context.Background()); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(p.DropInPath())
		return string(data)
	}
	un := render(a)
	if !strings.Contains(un, "ReadWritePaths=-/var/log/ogs-projects\n") || strings.Count(un, "ReadWritePaths") != 1 {
		t.Errorf("une seule entrée, la racine, attendue :\n%s", un)
	}
	steps, err := p.PlanDropIn(context.Background(), []*model.Project{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Mutates() {
		t.Error("un projet de plus ne doit rien changer au complément, donc ne rien redémarrer")
	}
}
