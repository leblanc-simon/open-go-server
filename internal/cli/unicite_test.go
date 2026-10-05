package cli

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
	appprov "leblanc.io/open-go-server/internal/provider/app"
	"leblanc.io/open-go-server/internal/store"
	"leblanc.io/open-go-server/internal/tui"
)

// ajouterDomaine ajoute un alias à la déclaration d'un projet, à la main, comme
// le ferait un exploitant qui édite le YAML.
func (h *harness) ajouterDomaine(t *testing.T, id, domaine string) string {
	t.Helper()
	decl := filepath.Join("etc", "ogs", "projects", id+".yaml")
	avant := h.read(t, decl)
	apres := strings.Replace(avant, "  domains:\n",
		"  domains:\n    - name: "+domaine+"\n      role: alias\n", 1)
	if apres == avant {
		t.Fatalf("aucune liste de domaines dans %s :\n%s", decl, avant)
	}
	if err := os.WriteFile(h.path(decl), []byte(apres), 0o644); err != nil {
		t.Fatal(err)
	}
	return h.path(decl)
}

// Le contrôle d'unicité ne portait que sur les projets visés : « apply blogfr »
// ou « plan -f » comparaient blogfr à lui-même, et un domaine déjà servi par
// shopfr entrait sans un mot — deux vhosts pour le même nom, le serveur web
// servant l'un ou l'autre selon l'ordre de ses fichiers (R1-07).
func TestUnApplyCibleEprouveLUniciteContreToutLeParc(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr")
	h.mustRun("project", "create", "blog.fr")
	decl := h.ajouterDomaine(t, "blogfr", "shop.fr")

	for _, args := range [][]string{
		{"plan", "blogfr"},
		{"apply", "blogfr", "--yes"},
		{"plan", "-f", decl},
		{"apply", "-f", decl, "--yes"},
	} {
		out, code := h.run(args...)
		if code != CodeConflict {
			t.Errorf("« ogs %s » : code %d, attendu %d (conflit) :\n%s",
				strings.Join(args, " "), code, CodeConflict, out)
			continue
		}
		if !strings.Contains(out, "shopfr") {
			t.Errorf("« ogs %s » : le refus doit nommer le détenteur :\n%s", strings.Join(args, " "), out)
		}
	}
}

// « db add » écrivait la déclaration sans rien éprouver : la base de rôle
// « api » du projet shop porte le nom shop_api, base principale du projet
// shop_api, et le premier apply de shop donnait tous les droits sur la base du
// voisin à son propre compte.
func TestDBAddRefuseUneBaseDejaDeclareeParUnVoisin(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--id", "shop")
	h.mustRun("project", "create", "api.shop.fr", "--id", "shop_api", "--db", "mysql")
	decl := filepath.Join("etc", "ogs", "projects", "shop.yaml")
	avant := h.read(t, decl)

	out, code := h.run("db", "add", "shop", "--role", "api")
	if code != CodeConflict {
		t.Fatalf("db add : code %d, attendu %d (conflit) :\n%s", code, CodeConflict, out)
	}
	if h.read(t, decl) != avant {
		t.Error("la déclaration a été écrite malgré le conflit")
	}
}

// « site clone » ne vérifiait que l'absence du fichier de destination : une
// préproduction dont le host était déjà servi par un autre projet était écrite
// puis appliquée.
func TestSiteCloneRefuseUneDestinationEnConflitAvecLeParc(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr")
	h.mustRun("project", "create", "autre.fr")
	h.ajouterDomaine(t, "autrefr", "shop.preprod-pp.net")

	out, code := h.run("site", "clone", "shopfr", "--to", "prep", "--yes")
	if code != CodeConflict {
		t.Fatalf("site clone : code %d, attendu %d (conflit) :\n%s", code, CodeConflict, out)
	}
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "shopfrprep.yaml")); !os.IsNotExist(err) {
		t.Errorf("la déclaration de la destination a été écrite malgré le conflit : %v", err)
	}
}

// « project restore » réécrivait la déclaration archivée sans la confronter au
// parc : un domaine repris entre-temps par un autre projet se retrouvait servi
// deux fois.
func TestProjectRestoreRefuseUnProjetEnConflitAvecLeParc(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr")
	h.mustRunPartial("apply", "--yes")
	h.mustRunPartial("project", "delete", "shopfr", "--yes")
	entries, err := os.ReadDir(h.path("var", "lib", "ogs", "archives"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("aucune archive : %v", err)
	}
	h.mustRun("project", "create", "shop.com")
	h.ajouterDomaine(t, "shopcom", "shop.fr")

	out, code := h.run("project", "restore", h.path("var", "lib", "ogs", "archives", entries[0].Name()), "--yes")
	if code != CodeConflict {
		t.Fatalf("project restore : code %d, attendu %d (conflit) :\n%s", code, CodeConflict, out)
	}
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "shopfr.yaml")); !os.IsNotExist(err) {
		t.Errorf("la déclaration restaurée a été écrite malgré le conflit : %v", err)
	}
}

// ajouterCertificats insère une liste de certificats en tête de spec, à la main.
func (h *harness) ajouterCertificats(t *testing.T, id, liste string) {
	t.Helper()
	decl := filepath.Join("etc", "ogs", "projects", id+".yaml")
	avant := h.read(t, decl)
	if strings.Contains(avant, "certificates:") {
		t.Fatalf("%s déclare déjà des certificats :\n%s", decl, avant)
	}
	apres := strings.Replace(avant, "spec:\n", "spec:\n  certificates:\n"+liste, 1)
	if err := os.WriteFile(h.path(decl), []byte(apres), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Une collision entre deux voisins n'arrête pas la commande qui vise un
// troisième projet : bbfr, qui nomme son certificat d'après aafr, est refusé à
// son apply, non à celui de ccfr.
func TestUneCollisionEntreVoisinsNArretePasUnTroisiemeProjet(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	for _, host := range []string{"aa.fr", "bb.fr", "cc.fr"} {
		h.mustRun("project", "create", host, "--kind", "static")
	}
	h.ajouterCertificats(t, "bbfr", "    - name: aafr\n      domains: [bb.fr]\n      challenge: selfsigned\n")
	if out, code := h.run("plan", "ccfr"); code != CodeOK && code != CodePartial {
		t.Errorf("plan ccfr : code %d, la collision ne le concerne pas :\n%s", code, out)
	}
	if out, code := h.run("plan", "bbfr"); code != CodeConflict {
		t.Errorf("plan bbfr : code %d, attendu %d :\n%s", code, CodeConflict, out)
	}
	// La création d'un projet sans rapport non plus : project create, site
	// create (simulé), et l'adoption passent par le même contrôle.
	h.mustRun("project", "create", "dd.fr", "--kind", "static")
	h.mustRun("--dry-run", "site", "create", "ee.fr", "--kind", "static")
	// Et la création qui entre elle-même en collision reste refusée.
	if out, code := h.run("project", "create", "aa.fr", "--kind", "static", "--id", "autre"); code != CodeConflict {
		t.Errorf("project create d'un host déjà servi : code %d, attendu %d :\n%s", code, CodeConflict, out)
	}
}

// Un nom de certificat hérité, que la version précédente acceptait, ne doit pas
// arrêter le parc entier : chargé avec lui, le renouvellement de tous les
// certificats, status, et le recalcul du parc à la suppression d'un voisin
// (D68) s'arrêtaient en code 3 sur la seule déclaration fautive.
func TestUnNomDeCertificatHeriteNArretePasLeParc(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	for _, host := range []string{"shop.fr", "cc.fr", "dd.fr", "ee.fr"} {
		h.mustRun("project", "create", host, "--kind", "static")
	}
	h.mustRunPartial("apply", "--yes")
	// Un joker comme nom : il ne sort de rien, il continue de fonctionner.
	h.ajouterCertificats(t, "ccfr", "    - name: \"*.cc.fr\"\n      domains: [cc.fr]\n      challenge: selfsigned\n")
	// Deux certificats sans nom : même répertoire, refusé au seul ddfr.
	h.ajouterCertificats(t, "ddfr", "    - domains: [dd.fr]\n      challenge: selfsigned\n"+
		"    - domains: [dd.fr]\n      challenge: selfsigned\n")
	// Un nom qui vise le wildcard partagé : refusé, mais au seul projet qui le porte.
	h.ajouterCertificats(t, "eefr", "    - name: \"@wildcard-prep\"\n      domains: [ee.fr]\n      challenge: selfsigned\n")

	if out, code := h.run("plan", "ccfr"); code != CodeOK && code != CodePartial {
		t.Errorf("plan ccfr : code %d\n%s", code, out)
	}
	if out, code := h.run("plan", "ddfr"); code != CodeValidation {
		t.Errorf("plan ddfr : code %d, attendu %d :\n%s", code, CodeValidation, out)
	}
	if out, code := h.run("plan", "eefr"); code != CodeValidation {
		t.Errorf("plan eefr : code %d, attendu %d :\n%s", code, CodeValidation, out)
	}
	if out, code := h.run("cert", "renew", "--all", "--yes", "--quiet"); code == CodeValidation || code == CodeError {
		t.Errorf("cert renew --all : code %d, le parc ne doit pas s'arrêter sur eefr :\n%s", code, out)
	}
	if out, code := h.run("status"); code == CodeValidation {
		t.Errorf("status : code %d\n%s", code, out)
	}
	out := h.mustRunPartial("project", "delete", "shopfr", "--yes")
	logrotate := h.read(t, filepath.Join("etc", "logrotate.d", "ogs"))
	if strings.Contains(logrotate, "shopfr") || !strings.Contains(logrotate, "ccfr") {
		t.Errorf("la rotation n'a pas été recalculée à la suppression :\n%s\n%s", logrotate, out)
	}
}

// ouvrirApp prépare une App sur le préfixe du banc, comme le fait la racine des
// commandes.
func (h *harness) ouvrirApp(t *testing.T, avecBase bool) *App {
	t.Helper()
	app := newApp()
	app.Prefix, app.Quiet, app.Out, app.Err = h.prefix, true, io.Discard, io.Discard
	if err := app.setup(); err != nil {
		t.Fatal(err)
	}
	if avecBase {
		if err := app.openStore(); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(app.close)
	return app
}

// La TUI appliquait et déclarait sans éprouver l'unicité : un alias shop.fr
// ajouté à blogfr, que « ogs apply blogfr » refuse, y était appliqué — second
// vhost pour shop.fr —, et l'assistant écrivait un site dont un voisin servait
// déjà le host (R1-07).
func TestLaTUIEprouveLUniciteContreLeParc(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("project", "create", "blog.fr", "--kind", "static")
	h.ajouterDomaine(t, "blogfr", "shop.fr")
	h.ajouterDomaine(t, "shopfr", "news.fr")

	app := h.ouvrirApp(t, true)
	b := &tuiBackend{app: app}
	p, err := app.loadProject("blogfr")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := b.Apply(context.Background(), p); ExitCode(err) != CodeConflict {
		t.Errorf("Apply(blogfr) : code %d, attendu %d :\n%s", ExitCode(err), CodeConflict, out)
	}
	if _, err := os.Stat(h.path("etc", "apache2", "sites-available", "blogfr.conf")); !os.IsNotExist(err) {
		t.Errorf("le vhost de blogfr a été écrit malgré le conflit : %v", err)
	}

	if out, err := b.Create(context.Background(), tui.WizardAnswers{Host: "news.fr", Kind: "static", Env: "prod"}); ExitCode(err) != CodeConflict {
		t.Errorf("Create(news.fr) : code %d, attendu %d :\n%s", ExitCode(err), CodeConflict, out)
	}
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "newsfr.yaml")); !os.IsNotExist(err) {
		t.Errorf("la déclaration de newsfr a été écrite malgré le conflit : %v", err)
	}
}

// doctor relit le port dans l'unité écrite et n'attribue rien : sans base
// d'état, Endpoint rendait le premier port de la plage pour tout projet, et
// l'écoute étrangère du vrai port passait inaperçue (D94).
func TestDoctorNommeLEcouteEtrangereSansRienAttribuer(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "aa.fr", "--kind", "go")
	h.mustRun("project", "create", "bb.fr", "--kind", "go")
	h.mustRunPartial("apply", "--yes")
	unit := h.read(t, filepath.Join("etc", "systemd", "system", "ogs-bbfr-app@.service"))
	if !strings.Contains(unit, "Environment=PORT=21002") {
		t.Fatalf("bbfr devait recevoir 21002 :\n%s", unit)
	}
	for _, avecBase := range []bool{false, true} {
		if !avecBase {
			if err := os.Rename(h.path("var", "lib", "ogs", "state.db"), h.path("var", "lib", "ogs", "state.db.hors")); err != nil {
				t.Fatal(err)
			}
		}
		app := h.ouvrirApp(t, avecBase)
		provs, err := app.buildProviders()
		if err != nil {
			t.Fatal(err)
		}
		provs.App.Owner = func(p *model.Project) (int, int, error) {
			if p.ID() == "aafr" {
				return 2001, 2001, nil
			}
			return 2002, 2002, nil
		}
		provs.App.Listeners = func() ([]appprov.Listener, error) {
			return []appprov.Listener{
				{IP: net.IPv4(127, 0, 0, 1), Port: 21001, UID: 2001},
				{IP: net.IPv4(127, 0, 0, 1), Port: 21002, UID: 1001},
			}, nil
		}
		projects, err := app.loadProjects()
		if err != nil {
			t.Fatal(err)
		}
		var avant []store.Allocation
		if avecBase {
			avant, _ = app.Store.Allocations("")
		}
		var lignes []string
		appListenerChecks(provs, projects, func(name, status, detail, _ string) {
			lignes = append(lignes, status+" "+detail)
		})
		provs.close()
		if len(lignes) != 1 || !strings.HasPrefix(lignes[0], "échec projet bbfr") ||
			!strings.Contains(lignes[0], "uid 1001") || !strings.Contains(lignes[0], ":21002") {
			t.Errorf("base %v : seule l'écoute étrangère de bbfr doit être nommée : %q", avecBase, lignes)
		}
		if avecBase {
			après, _ := app.Store.Allocations("")
			if len(après) != len(avant) {
				t.Errorf("doctor a écrit dans la base d'état : %v → %v", avant, après)
			}
		} else if _, err := os.Stat(h.path("var", "lib", "ogs", "state.db")); !os.IsNotExist(err) {
			t.Errorf("doctor a créé la base d'état : %v", err)
		}
		if !avecBase {
			if err := os.Rename(h.path("var", "lib", "ogs", "state.db.hors"), h.path("var", "lib", "ogs", "state.db")); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Un pool en écoute TCP écarte les ports que la machine tient, comme une
// application : le branchement se fait à la construction des providers.
func TestLesPoolsTCPVoientLesPortsEnEcoute(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	app := h.ouvrirApp(t, true)
	provs, err := app.buildProviders()
	if err != nil {
		t.Fatal(err)
	}
	defer provs.close()
	provs.App.Listeners = func() ([]appprov.Listener, error) {
		return []appprov.Listener{{IP: net.IPv4(127, 0, 0, 1), Port: 18501, UID: 1001}}, nil
	}
	if provs.PHP.PortsInUse == nil || !provs.PHP.PortsInUse()[18501] {
		t.Fatal("le provider PHP ne voit pas les ports en écoute")
	}
}

// doctor nomme les déclarations de certificat que le chargement tolère.
func TestDoctorNommeLesCertificatsACorriger(t *testing.T) {
	p := model.New("shop.fr", "prod", model.KindStatic)
	p.Metadata.Name = "shopfr"
	p.Spec.Certificates = []model.Certificate{
		{Name: "@wildcard-prep", Domains: []string{"shop.fr"}},
		{Domains: []string{"shop.fr"}}, {Domains: []string{"www.shop.fr"}},
	}
	var lignes []string
	certificateDeclarationChecks([]*model.Project{p}, func(_, status, detail, _ string) {
		lignes = append(lignes, status+" "+detail)
	})
	if len(lignes) != 1 || !strings.HasPrefix(lignes[0], "échec") ||
		!strings.Contains(lignes[0], "@wildcard-prep") || !strings.Contains(lignes[0], "\"shopfr\"") {
		t.Errorf("doctor doit nommer le nom refusé et le doublon : %q", lignes)
	}
}

// retoucher remplace une fois un fragment de la déclaration d'un projet, à la
// main, comme le ferait un exploitant qui édite le YAML.
func (h *harness) retoucher(t *testing.T, id, avant, apres string) {
	t.Helper()
	decl := filepath.Join("etc", "ogs", "projects", id+".yaml")
	contenu := h.read(t, decl)
	nouveau := strings.Replace(contenu, avant, apres, 1)
	if nouveau == contenu {
		t.Fatalf("%q absent de %s :\n%s", avant, decl, contenu)
	}
	if err := os.WriteFile(h.path(decl), []byte(nouveau), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Les trois clés du lot Y7 — moteur MySQL/MariaDB confondu, domaines des vhosts
// déclarés, répertoire de surcharge — s'éprouvent au plan et à l'apply du projet
// visé, jamais au chargement : une déclaration ancienne déjà en conflit laisse
// status et le renouvellement du parc tourner, et doctor la nomme.
func TestUnicitéY7AuPlanJamaisAuChargement(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--db", "mysql")
	h.mustRun("project", "create", "blog.fr", "--kind", "php-symfony", "--db", "mariadb")
	h.mustRun("project", "create", "news.fr", "--kind", "static")
	h.mustRun("project", "create", "cc.fr", "--kind", "static")

	// R1-23 : la base et le compte de shopfr, déclarés sous « mariadb ».
	h.retoucher(t, "blogfr", "      name: blogfr\n      user: blogfr\n", "      name: shopfr\n      user: shopfr\n")
	// R4-02 : un vhost déclaré qui sert le domaine d'un voisin.
	h.retoucher(t, "newsfr", "spec:\n", "spec:\n  web:\n    vhosts:\n      - name: main\n        domains: [news.fr]\n"+
		"      - name: promo\n        domains: [cc.fr]\n")

	for _, cas := range []struct{ id, attendu string }{
		{"blogfr", "base de données"},
		{"newsfr", `"cc.fr" est déjà utilisé par le projet "ccfr"`},
	} {
		for _, args := range [][]string{{"plan", cas.id}, {"apply", cas.id, "--yes"}} {
			out, code := h.run(args...)
			if code != CodeConflict || !strings.Contains(out, cas.attendu) {
				t.Errorf("« ogs %s » : code %d, attendu %d et %q :\n%s",
					strings.Join(args, " "), code, CodeConflict, cas.attendu, out)
			}
		}
	}
	if _, err := os.Stat(h.path("etc", "apache2", "sites-available", "newsfr_promo.conf")); !os.IsNotExist(err) {
		t.Errorf("le vhost qui détourne cc.fr a été écrit : %v", err)
	}

	// Le chargement n'en dit rien : le parc continue de tourner.
	if out, code := h.run("status"); code == CodeValidation || code == CodeConflict {
		t.Errorf("status : code %d, le chargement ne doit pas éprouver l'unicité :\n%s", code, out)
	}
	if out, code := h.run("cert", "renew", "--all", "--yes", "--quiet"); code == CodeValidation || code == CodeConflict || code == CodeError {
		t.Errorf("cert renew --all : code %d\n%s", code, out)
	}

	app := h.ouvrirApp(t, false)
	projects, err := app.loadProjects()
	if err != nil {
		t.Fatal(err)
	}
	var lignes []string
	fleetUniquenessChecks(projects, app.Cfg.PrepDomain(), func(_, status, detail, _ string) {
		lignes = append(lignes, status+" "+detail)
	})
	joint := strings.Join(lignes, "\n")
	for _, attendu := range []string{"mysql:shopfr", "cc.fr"} {
		if !strings.Contains(joint, attendu) {
			t.Errorf("doctor doit nommer le conflit %q :\n%s", attendu, joint)
		}
	}
}

// R3N-05 : le vhost adopté « shopfr » s'écrit dans le fichier nginx « shopfr »,
// sans extension ; le projet shop.fr, d'identifiant shopfr, n'a aucun fichier en
// commun avec lui mais aurait inclus ses surcharges, /etc/<moteur>/projects/shopfr/.
func TestCreationRefuseLeRepertoireDeSurchargeDUnVhostAdopte(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop-fr.com", "--kind", "static")
	h.retoucher(t, "shopfrcom", "  env: prod\n", "  env: prod\n  labels:\n    adopted: \"true\"\n")
	h.retoucher(t, "shopfrcom", "spec:\n", "spec:\n  web:\n    vhosts:\n      - name: shopfr\n        file: shopfr\n"+
		"        domains: [shop-fr.com]\n")

	out, code := h.run("project", "create", "shop.fr", "--kind", "static")
	if code != CodeConflict || !strings.Contains(out, "répertoire de surcharge") {
		t.Fatalf("project create shop.fr : code %d, attendu %d (répertoire de surcharge) :\n%s", code, CodeConflict, out)
	}
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "shopfr.yaml")); !os.IsNotExist(err) {
		t.Errorf("la déclaration a été écrite malgré le conflit : %v", err)
	}

	// doctor dit quoi renommer, et que l'ancien répertoire resterait orphelin.
	adopté, err := h.ouvrirApp(t, false).loadProject("shopfrcom")
	if err != nil {
		t.Fatal(err)
	}
	shop := model.New("shop.fr", "prod", model.KindStatic)
	shop.Metadata.Name = "shopfr"
	shop.Spec.Domains = []model.Domain{{Name: "shop.fr", Role: "primary"}}
	var remèdes []string
	fleetUniquenessChecks([]*model.Project{adopté, shop}, "preprod-pp.net", func(_, status, _, fix string) {
		remèdes = append(remèdes, status+" "+fix)
	})
	if len(remèdes) != 1 || !strings.Contains(remèdes[0], "spec.web.vhosts[].name") || !strings.Contains(remèdes[0], "orphelin") {
		t.Errorf("remède de doctor : %q", remèdes)
	}
}
