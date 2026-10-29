package validate

import (
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
)

func newProject(t *testing.T, host string, env naming.Env, kind model.AppKind) *model.Project {
	t.Helper()
	p := model.New(host, env, kind)
	if err := p.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProjectAcceptsNormalizedStatic(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	if err := Project(p, "/home"); err != nil {
		t.Fatalf("projet normalisé refusé : %v", err)
	}
}

func TestNormalizeProvidesDefaultPool(t *testing.T) {
	// La normalisation déclare le pool principal : un type PHP n'a qu'une réponse
	// sensée sur ce point.
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindPrestashop)
	if len(p.Spec.PHP.Pools) != 1 || p.Spec.PHP.Pools[0].Name != model.MainPoolName {
		t.Fatalf("pool par défaut absent : %+v", p.Spec.PHP.Pools)
	}
	if p.Spec.PHP.Pools[0].Version != model.DefaultPHPVersion {
		t.Errorf("version par défaut %q", p.Spec.PHP.Pools[0].Version)
	}
	if err := Project(p, "/home"); err != nil {
		t.Fatalf("projet normalisé refusé : %v", err)
	}
}

// La normalisation n'écrit plus de preset (D103) : un défaut recopié dans la
// déclaration ne se distinguerait plus d'un choix, et ne suivrait jamais
// config.yaml. La descente de la préproduction se fait au calcul du pool
// (provider php, presetDowngrade).
func TestNormalizeNEcritPasDePreset(t *testing.T) {
	prod := newProject(t, "shop.fr", naming.EnvProd, model.KindSymfony)
	prep := newProject(t, "shop.fr", naming.EnvPrep, model.KindSymfony)
	if prod.Spec.PHP.Pools[0].Preset != "" || prep.Spec.PHP.Pools[0].Preset != "" {
		t.Errorf("presets : prod=%q prep=%q, attendus vides", prod.Spec.PHP.Pools[0].Preset, prep.Spec.PHP.Pools[0].Preset)
	}
	// Un preset déclaré est gardé tel quel, celui qu'une déclaration antérieure
	// porte en toutes lettres compris.
	prod.Spec.PHP.Pools[0].Preset = "medium"
	if err := prod.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	if prod.Spec.PHP.Pools[0].Preset != "medium" {
		t.Errorf("le preset déclaré a été réécrit : %q", prod.Spec.PHP.Pools[0].Preset)
	}
}

// Un preset inconnu de la table livrée peut venir de config.yaml : le
// chargement n'en juge que la forme, pour qu'un preset retiré de la
// configuration n'arrête pas tout le parc (D103).
func TestPresetInconnuToléréAuChargement(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindSymfony)
	p.Spec.PHP.Pools[0].Preset = "maison"
	if err := Project(p, "/home"); err != nil {
		t.Errorf("un preset de config.yaml doit passer le chargement : %v", err)
	}
	p.Spec.PHP.Pools[0].Preset = "Mai son"
	if err := Project(p, "/home"); err == nil {
		t.Error("un nom de preset hors grammaire doit être refusé")
	}
}

func TestProjectRequiresPoolForPHPKinds(t *testing.T) {
	// La règle reste un garde-fou pour un document construit sans normalisation.
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindPrestashop)
	p.Spec.PHP.Pools = nil
	err := Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "requiert un pool PHP") {
		t.Fatalf("un type PHP sans pool doit être refusé, obtenu : %v", err)
	}
}

// Au chargement, seule la forme d'une version compte : la liste des versions
// déclarables vient de config.yaml et se juge au plan (D125).
func TestProjectNeJugeQueLaFormeDeLaVersionPHP(t *testing.T) {
	for version, ok := range map[string]bool{"7.1": true, "8.6": true, "9.0": true,
		"7.0": false, "5.6": false, "10.0": false, "8": false} {
		p := newProject(t, "shop.fr", naming.EnvProd, model.KindPrestashop)
		p.Spec.PHP.Pools = []model.Pool{{Name: "web", Version: version}}
		err := Project(p, "/home")
		if ok && err != nil {
			t.Errorf("PHP %s doit être accepté au chargement : %v", version, err)
		}
		if !ok && (err == nil || !strings.Contains(err.Error(), "invalide")) {
			t.Errorf("PHP %s devrait être refusé, obtenu : %v", version, err)
		}
	}
}

func TestPHPVersionSuitLaListe(t *testing.T) {
	if err := PHPVersion("7.1", []string{"7.1", "8.5"}); err != nil {
		t.Error(err)
	}
	if err := PHPVersion("8.4", []string{"7.1", "8.5"}); err == nil || !strings.Contains(err.Error(), "non gérée") {
		t.Errorf("8.4 hors de la liste, obtenu : %v", err)
	}
}

func TestProjectAcceptsEOLPHP(t *testing.T) {
	// PHP 7.4 est en fin de support mais reste utilisable : un Prestashop ancien
	// peut l'exiger (D39).
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindPrestashop)
	p.Spec.PHP.Pools = []model.Pool{{Name: "web", Version: "7.4"}}
	if err := Project(p, "/home"); err != nil {
		t.Fatalf("PHP 7.4 doit être accepté : %v", err)
	}
	for _, v := range []string{"7.1", "7.2", "7.3", "7.4"} {
		if _, eol := EOLDate(v); !eol {
			t.Errorf("PHP %s devrait être signalé en fin de support", v)
		}
	}
	if _, eol := EOLDate("8.4"); eol {
		t.Error("PHP 8.4 ne devrait pas être signalé en fin de support")
	}
}

// Un home hors de naming.homeRoot est refusé par Placement, au plan et à
// l'apply, jamais par Project, qui suit chaque chargement (D120) : changer la
// racine dans config.yaml rendait sinon tout le parc invalide.
func TestPlacementRejectsHomeOutsideRoot(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Layout.Root = "/srv/shop.fr"
	p.Spec.User.Home = "/srv/shop.fr"
	p.Spec.Layout.Docroot = "/srv/shop.fr/www"
	if err := Project(p, "/home"); err != nil {
		t.Fatalf("Project ne juge pas l'appartenance à la racine des homes : %v", err)
	}
	err := Placement(p, "/home", model.DefaultLogsRoot)
	if err == nil || !strings.Contains(err.Error(), "doit être sous /home") {
		t.Fatalf("un home hors racine doit être refusé, obtenu : %v", err)
	}
	p.Metadata.Labels = map[string]string{"adopted": "true"}
	if err := Placement(p, "/home", model.DefaultLogsRoot); err != nil {
		t.Errorf("un projet adopté peut vivre hors de la racine des homes : %v", err)
	}
	p.Spec.Layout.LogDir = "/srv/journaux/shopfr"
	if err := Placement(p, "/home", model.DefaultLogsRoot); err == nil || !strings.Contains(err.Error(), "logs.root") {
		t.Errorf("des journaux hors de logs.root doivent être refusés, adopté compris : %v", err)
	}
}

func TestProjectRejectsBadDatabaseIdentifier(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Databases = []model.Database{{Engine: "mysql", Name: "shop`; DROP", User: "shopfr"}}
	err := Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "nom de base") {
		t.Fatalf("un nom de base non conforme doit être refusé, obtenu : %v", err)
	}
}

func TestProjectRejectsAMQPVhostWithSlash(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.AMQP = &model.AMQP{Vhost: "/shopfr", User: "shopfr"}
	err := Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "%2F") {
		t.Fatalf("un vhost préfixé de / doit être refusé, obtenu : %v", err)
	}
}

func TestProjectRejectsDanglingBinding(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.AMQP = &model.AMQP{
		Vhost: "shopfr", User: "shopfr",
		Queues:   []model.Queue{{Name: "async"}},
		Bindings: []model.Binding{{Exchange: "messages", Queue: "async"}},
	}
	err := Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "non déclaré") {
		t.Fatalf("un binding vers un exchange absent doit être refusé, obtenu : %v", err)
	}
}

func TestCronExpression(t *testing.T) {
	valid := []string{"* * * * *", "*/5 * * * *", "0 3 * * 1", "15,45 2-4 1 1 *", "0 0 1 1 0"}
	for _, e := range valid {
		if err := CronExpression(e); err != nil {
			t.Errorf("expression valide refusée %q : %v", e, err)
		}
	}
	invalid := []string{"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "a * * * *", "*/0 * * * *"}
	for _, e := range invalid {
		if err := CronExpression(e); err == nil {
			t.Errorf("expression invalide acceptée : %q", e)
		}
	}
}

func TestUniquenessDetectsHomeCollision(t *testing.T) {
	// Deux identifiants distincts peuvent réclamer le même home : c'est le cas
	// documenté au §3.3.6, à refuser explicitement.
	a := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	b := newProject(t, "autre.fr", naming.EnvProd, model.KindStatic)
	b.Spec.Layout.Root = a.Spec.Layout.Root
	err := Uniqueness([]*model.Project{a, b}, "preprod-pp.net")
	if err == nil || !strings.Contains(err.Error(), "home") {
		t.Fatalf("collision de home non détectée : %v", err)
	}
}

func TestUnicitéRefuseDesHomesImbriqués(t *testing.T) {
	// Les deux formes que produit l'import : un home englobant (le compte « web »
	// porte /home/web, dont site2.fr occupe un sous-répertoire) et deux docroots
	// imbriqués devenus homes. Chaque fois, supprimer le premier projet effaçait le
	// second, que l'égalité stricte des homes laissait passer.
	adopter := func(host, root string) *model.Project {
		p := newProject(t, host, naming.EnvProd, model.KindStatic)
		p.Metadata.Labels = map[string]string{"adopted": "true"}
		p.Spec.Layout.Root, p.Spec.User.Home = root, root
		p.Spec.Layout.Docroot = root + "/public"
		return p
	}
	for _, c := range []struct{ englobant, imbriqué string }{
		{"/home/web", "/home/web/site2"},
		{"/var/www/boutique", "/var/www/boutique/blog"},
	} {
		// L'ordre du parc ne change rien : le projet englobant est nommé dans
		// les deux cas.
		for _, parc := range [][]*model.Project{
			{adopter("site1.fr", c.englobant), adopter("site2.fr", c.imbriqué)},
			{adopter("site2.fr", c.imbriqué), adopter("site1.fr", c.englobant)},
		} {
			err := Uniqueness(parc, "preprod-pp.net")
			if err == nil {
				t.Errorf("%s et %s : des homes imbriqués doivent être refusés", c.englobant, c.imbriqué)
				continue
			}
			if !strings.Contains(err.Error(), `"`+c.englobant+`", home du projet "site1fr"`) {
				t.Errorf("le refus doit nommer le home englobant et son projet : %v", err)
			}
			// Un parc écrit par une version antérieure peut déjà porter le
			// conflit : le refus le dit, sans quoi l'exploitant le chercherait dans
			// le projet qu'il crée.
			if !strings.Contains(err.Error(), "version antérieure") {
				t.Errorf("le refus doit dire que le conflit peut précéder la commande : %v", err)
			}
		}
	}

	// Un nom qui en prolonge un autre n'est pas imbriqué : la comparaison porte
	// sur les composants du chemin, non sur les caractères.
	parc := []*model.Project{adopter("boutique.fr", "/var/www/boutique"),
		adopter("ancienne.fr", "/var/www/boutique-old")}
	if err := Uniqueness(parc, "preprod-pp.net"); err != nil {
		t.Errorf("deux homes voisins doivent cohabiter : %v", err)
	}
}

func TestUniquenessDetectsIdentifierCollision(t *testing.T) {
	// mon-shop.fr et monshop.fr convergent vers le même identifiant.
	a := newProject(t, "mon-shop.fr", naming.EnvProd, model.KindStatic)
	b := newProject(t, "monshop.fr", naming.EnvProd, model.KindStatic)
	err := Uniqueness([]*model.Project{a, b}, "preprod-pp.net")
	if err == nil || !strings.Contains(err.Error(), "identifiant") {
		t.Fatalf("collision d'identifiant non détectée : %v", err)
	}
}

func TestUniquenessAllowsProdAndPrep(t *testing.T) {
	prod := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	prep := newProject(t, "shop.fr", naming.EnvPrep, model.KindStatic)
	if err := Uniqueness([]*model.Project{prod, prep}, "preprod-pp.net"); err != nil {
		t.Fatalf("production et préproduction doivent cohabiter : %v", err)
	}
	if prod.Spec.Layout.Root == prep.Spec.Layout.Root {
		t.Error("les deux environnements doivent avoir des homes distincts")
	}
}

func TestUniquenessDetectsDomainCollision(t *testing.T) {
	a := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	b := newProject(t, "autre.fr", naming.EnvProd, model.KindStatic)
	b.Spec.Domains = append(b.Spec.Domains, model.Domain{Name: "www.shop.fr", Role: "alias"})
	if err := Uniqueness([]*model.Project{a, b}, "preprod-pp.net"); err == nil {
		t.Fatal("un domaine déjà servi doit être refusé")
	}
}

// Le vhost « b2b » du projet shop s'écrit dans shop_b2b.conf, le fichier
// principal d'un projet shop_b2b : les deux se le réécriraient à chaque apply.
// Les pools étaient déjà comparés ; les fichiers de vhost ne l'étaient pas.
func TestUniquenessDetecteUnFichierDeVhostPartage(t *testing.T) {
	shop := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	shop.Metadata.Name = "shop"
	shop.Spec.Web.VHosts = []model.VHost{
		{Name: model.MainPoolName, Domains: []string{"shop.fr"}},
		{Name: "b2b", Domains: []string{"pro.shop.fr"}},
	}
	voisin := newProject(t, "b2b.shop.fr", naming.EnvProd, model.KindStatic)
	voisin.Metadata.Name = "shop_b2b"
	err := Uniqueness([]*model.Project{shop, voisin}, "preprod-pp.net")
	if err == nil || !strings.Contains(err.Error(), "shop_b2b.conf") {
		t.Fatalf("le fichier de vhost partagé doit être refusé : %v", err)
	}

	// Le vhost de redirection implicite prolonge lui aussi l'identifiant.
	redirige := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	redirige.Metadata.Name = "shop"
	redirige.Spec.Domains = append(redirige.Spec.Domains,
		model.Domain{Name: "shop.com", Role: "redirect", Target: "shop.fr"})
	autre := newProject(t, "promo.shop.fr", naming.EnvProd, model.KindStatic)
	autre.Metadata.Name = "shop_redirect"
	if err := Uniqueness([]*model.Project{redirige, autre}, "preprod-pp.net"); err == nil {
		t.Error("le vhost de redirection implicite doit entrer dans le contrôle")
	}

	// Un identifiant voisin sans fichier commun reste admis.
	api := newProject(t, "api.shop.fr", naming.EnvProd, model.KindStatic)
	api.Metadata.Name = "shop_api"
	if err := Uniqueness([]*model.Project{shop, api}, "preprod-pp.net"); err != nil {
		t.Errorf("des identifiants proches sans fichier commun doivent cohabiter : %v", err)
	}
}

// Le nom réel d'un fichier de vhost n'a de sens que pour un vhost adopté, dont
// il désigne l'original ; il reste un nom simple.
func TestProjectContraintLeFichierDUnVhost(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Web.VHosts = []model.VHost{{Name: "shop.fr", File: "shop.fr", Domains: []string{"shop.fr"}}}
	if err := Project(p, "/home"); err == nil || !strings.Contains(err.Error(), "adopté") {
		t.Errorf("un fichier de vhost hors adoption doit être refusé : %v", err)
	}
	p.Metadata.Labels = map[string]string{"adopted": "true"}
	if err := Project(p, "/home"); err != nil {
		t.Errorf("le fichier d'un vhost adopté doit être admis : %v", err)
	}
	for _, file := range []string{"../apache2.conf", ".shop.fr", "shop fr"} {
		p.Spec.Web.VHosts[0].File = file
		if err := Project(p, "/home"); err == nil {
			t.Errorf("fichier %q admis", file)
		}
	}
}

func TestProjectAcceptsPrimaryAndWWWAlias(t *testing.T) {
	// Le cas le plus courant : servir shop.fr et www.shop.fr. Les deux ont la
	// même forme canonique, mais ce sont bien deux domaines à déclarer.
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Domains = append(p.Spec.Domains, model.Domain{Name: "www.shop.fr", Role: "alias"})
	if err := Project(p, "/home"); err != nil {
		t.Fatalf("principal et alias www refusés : %v", err)
	}
	if err := Uniqueness([]*model.Project{p}, "preprod-pp.net"); err != nil {
		t.Fatalf("un projet ne peut pas entrer en conflit avec lui-même : %v", err)
	}
}

func TestProjectStillDetectsRealDuplicate(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Domains = append(p.Spec.Domains, model.Domain{Name: "shop.fr", Role: "alias"})
	err := Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "deux fois") {
		t.Fatalf("un vrai doublon doit être refusé : %v", err)
	}
}

func TestAdoptedProjectMayLiveOutsideHomeRoot(t *testing.T) {
	// Un projet adopté décrit l'existant : refuser son emplacement rendrait
	// l'import inutilisable sur un serveur où les sites sont sous /var/www.
	p := newProject(t, "boutique.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Layout.Root = "/var/www/boutique"
	p.Spec.User.Home = "/var/www/boutique"
	p.Spec.Layout.Docroot = "/var/www/boutique/public"
	p.Spec.Layout.LogDir = "/var/www/boutique/logs"
	if err := Placement(p, "/home", "/var/www/boutique"); err == nil {
		t.Fatal("sans marque d'adoption, un home hors racine doit être refusé")
	}
	p.Metadata.Labels = map[string]string{"adopted": "true"}
	if err := Project(p, "/home"); err != nil {
		t.Fatalf("un projet adopté doit être accepté : %v", err)
	}
	if err := Placement(p, "/home", "/var/www/boutique"); err != nil {
		t.Fatalf("un projet adopté doit être accepté : %v", err)
	}
}

func TestAdoptedProjectStillRefusesDangerousRoots(t *testing.T) {
	p := newProject(t, "boutique.fr", naming.EnvProd, model.KindStatic)
	p.Metadata.Labels = map[string]string{"adopted": "true"}
	for _, root := range []string{"/", "/etc", "/usr", "/var", "/root"} {
		p.Spec.Layout.Root = root
		p.Spec.User.Home = root
		p.Spec.Layout.Docroot = root
		p.Spec.Layout.LogDir = root
		if err := Project(p, "/home"); err == nil {
			t.Errorf("racine %q acceptée à tort", root)
		}
	}
}

func TestCertificateDNSProviderIsRefusedAtDeclaration(t *testing.T) {
	// Un provider non géré doit être refusé à la déclaration : le découvrir au
	// moment de l'émission ferait échouer un apply qui a déjà créé le site, base
	// et compte compris.
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Certificates = []model.Certificate{{
		Name: "shopfr", Domains: []string{"shop.fr"},
		Challenge: "dns-01", DNSProvider: "cloudflare",
	}}
	err := Project(p, "/home")
	if err == nil {
		t.Fatal("un provider DNS non géré doit être refusé")
	}
	if !strings.Contains(err.Error(), "cloudflare") || !strings.Contains(err.Error(), "ovh") {
		t.Errorf("le message doit nommer le provider refusé et celui qui est géré : %v", err)
	}

	// OVH passe, et un certificat sans provider explicite aussi.
	p.Spec.Certificates[0].DNSProvider = "ovh"
	if err := Project(p, "/home"); err != nil {
		t.Errorf("ovh doit être accepté : %v", err)
	}
	p.Spec.Certificates[0].Challenge = "http-01"
	p.Spec.Certificates[0].DNSProvider = ""
	if err := Project(p, "/home"); err != nil {
		t.Errorf("http-01 sans provider doit être accepté : %v", err)
	}
}

func TestProjectRefusesHomeRootAsProjectRoot(t *testing.T) {
	// La racine des homes appartient à root en 0711 et n'est le home d'aucun
	// projet (D60) : l'accepter ferait chowner /home à l'utilisateur du projet,
	// qui prendrait la main sur le home de tous les autres, et « ogs project
	// delete » l'effacerait tout entière.
	for _, adopted := range []bool{false, true} {
		p := newProject(t, "boutique.fr", naming.EnvProd, model.KindStatic)
		if adopted {
			p.Metadata.Labels = map[string]string{"adopted": "true"}
		}
		p.Spec.Layout.Root = "/home"
		p.Spec.User.Home = "/home"
		p.Spec.Layout.Docroot = "/home/www"
		p.Spec.Layout.LogDir = "/home/logs"
		err := Project(p, "/home")
		if err == nil {
			t.Errorf("racine des homes acceptée à tort (adopté=%v)", adopted)
			continue
		}
		if !strings.Contains(err.Error(), "racine des homes") {
			t.Errorf("le message doit nommer la racine des homes (adopté=%v) : %v", adopted, err)
		}
	}
}

func TestProjectRefusesDangerousRootEvenWhenNotAdopted(t *testing.T) {
	// forbiddenRoot n'était consulté que pour un projet adopté ; un projet
	// ordinaire dont la racine vaut /var doit être refusé pour la même raison.
	p := newProject(t, "boutique.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Layout.Root = "/var"
	p.Spec.User.Home = "/var"
	p.Spec.Layout.Docroot = "/var/www"
	p.Spec.Layout.LogDir = "/var/log"
	err := Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "emplacement interdit") {
		t.Fatalf("racine /var acceptée à tort : %v", err)
	}
}

func TestNomDeServiceRefuseCeQuiElargitLaDelegation(t *testing.T) {
	// Le nom d'un service atterrit verbatim dans une spécification de commande
	// sudoers (D28), où la virgule ouvre une seconde commande, le « : » une
	// nouvelle paire hôtes = commandes et le « # » commente la fin de ligne, et
	// dans un composant de chemin sous /etc/systemd/system, où « .. » sort de
	// l'arborescence.
	for _, nom := range []string{
		"w@1.service, /bin/bash #",
		"../../../../etc/cron.d/pwn",
		"messenger..async",
		"my worker",
		"cron/daily",
		"veille:ALL",
		"veille\nMAILTO=root",
		"-veille",
		".veille",
		"",
		strings.Repeat("w", maxServiceName+1),
	} {
		p := newProject(t, "shop.fr", naming.EnvProd, model.KindSymfony)
		p.Spec.Services = []model.Service{{Name: nom, Template: "symfony-messenger"}}
		if err := Project(p, "/home"); err == nil {
			t.Errorf("nom de service %q accepté à tort", nom)
		}
	}
}

func TestNomDeServiceInhabituelResteAccepte(t *testing.T) {
	// La validation est en amont de tout chargement : un projet déjà en service
	// qu'elle refuserait bloquerait l'apply du parc entier, sans qu'« ogs service
	// remove » ni « ogs project delete » puissent le réparer. Elle ne doit donc
	// refuser que ce qui est dangereux, jamais ce qui est seulement inhabituel :
	// tout cela est un nom d'unité systemd valide, et rien de tout cela n'élargit
	// une règle sudoers.
	for _, nom := range []string{
		"Messenger",
		"messenger.async",
		"Worker_2",
		"consumer.v2-lot3",
		"_veille",
		"9veille",
		strings.Repeat("w", maxServiceName),
	} {
		p := newProject(t, "shop.fr", naming.EnvProd, model.KindSymfony)
		p.Spec.Services = []model.Service{{Name: nom, Template: "symfony-messenger"}}
		if err := Project(p, "/home"); err != nil {
			t.Errorf("nom de service %q refusé à tort : %v", nom, err)
		}
	}

	// Aucun plafond d'instances : un parc en porte de plus de soixante, et le
	// nombre n'est le vecteur de rien.
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindSymfony)
	p.Spec.Services = []model.Service{
		{Name: "messenger-async", Template: "symfony-messenger", Instances: 100},
	}
	if err := Project(p, "/home"); err != nil {
		t.Errorf("cent instances refusées à tort : %v", err)
	}
}

func TestServiceRefuseUnHomonymeEtUnNombreDInstancesNegatif(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindSymfony)
	p.Spec.Services = []model.Service{
		{Name: "messenger-async", Template: "symfony-messenger", Instances: 2},
		{Name: "messenger-async", Template: "symfony-messenger", Instances: 1},
	}
	err := Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "deux fois") {
		t.Fatalf("un service homonyme doit être refusé : %v", err)
	}

	p.Spec.Services = []model.Service{
		{Name: "messenger-async", Template: "symfony-messenger", Instances: -1},
	}
	err = Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "négatif") {
		t.Fatalf("un nombre d'instances négatif doit être refusé : %v", err)
	}

	// Un nom conforme et un nombre d'instances plausible passent.
	p.Spec.Services = []model.Service{
		{Name: "messenger-async", Template: "symfony-messenger", Instances: 3},
		{Name: "worker2", Template: "symfony-messenger"},
	}
	if err := Project(p, "/home"); err != nil {
		t.Errorf("services légitimes refusés : %v", err)
	}
}

func TestNomDeTacheRefuseCeQuiCasseLaLigneDeCron(t *testing.T) {
	// Le nom d'une tâche nomme son verrou (/run/ogs/<id>/<nom>.lock) et son
	// journal, et ce chemin de verrou est recollé sans citation dans la ligne
	// passée au shell. Dans le fichier de /etc/cron.d, « % » coupe la ligne et
	// envoie la suite sur l'entrée standard, un retour à la ligne ouvre une
	// entrée entière : la commande vient d'être échappée par le provider, le nom
	// ne l'est nulle part.
	for _, nom := range []string{
		"purge%F",
		"purge\nMAILTO=root",
		"purge;id",
		"purge`id`",
		"purge$(id)",
		"purge panier",
		"purge>fichier",
		"../../cron.d/pwn",
		"cron/daily",
	} {
		p := newProject(t, "shop.fr", naming.EnvProd, model.KindSymfony)
		p.Spec.Crons = []model.CronJob{{
			Name: nom, Schedule: "0 3 * * *", Command: "bin/console app:purge",
		}}
		if err := Project(p, "/home"); err == nil {
			t.Errorf("nom de tâche %q accepté à tort", nom)
		}
	}
	// Les noms ordinaires restent acceptés : ce sont ceux qu'on tape derrière
	// « ogs cron run <id> ». Le point y est refusé, lui qui sépare le nom du
	// verrou de son extension — un nom de service, qui n'est qu'un nom d'unité,
	// l'admet.
	for _, nom := range []string{"purge-panier", "purge_panier", "Sauvegarde", "sauvegarde2"} {
		p := newProject(t, "shop.fr", naming.EnvProd, model.KindSymfony)
		p.Spec.Crons = []model.CronJob{{
			Name: nom, Schedule: "0 3 * * *", Command: "bin/console app:purge",
		}}
		if err := Project(p, "/home"); err != nil {
			t.Errorf("nom de tâche %q refusé à tort : %v", nom, err)
		}
	}
}

func TestSignatureDKIMRefuseeEnPreproduction(t *testing.T) {
	// Une préproduction ne doit jamais signer au nom du domaine de production : un
	// envoi de test parti vers de vrais clients serait authentifié.
	p := newProject(t, "shop.fr", naming.EnvPrep, model.KindStatic)
	p.Spec.Mail = &model.Mail{DKIM: &model.DKIM{Enabled: true}}
	err := Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "préproduction") {
		t.Fatalf("la signature DKIM doit être refusée en préproduction, obtenu : %v", err)
	}
}

func TestSignatureDKIMLimiteeAuxDomainesDuProjet(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Mail = &model.Mail{DKIM: &model.DKIM{Enabled: true, Domains: []string{"voisin.fr"}}}
	err := Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "n'appartient pas aux domaines du projet") {
		t.Fatalf("un domaine étranger au projet doit être refusé, obtenu : %v", err)
	}
	p.Spec.Mail.DKIM.Domains = []string{"shop.fr", "SHOP.fr."}
	if err := Project(p, "/home"); err == nil || !strings.Contains(err.Error(), "deux fois") {
		t.Fatalf("un domaine déclaré deux fois doit être refusé, obtenu : %v", err)
	}
	p.Spec.Mail.DKIM.Domains = []string{"shop.fr"}
	if err := Project(p, "/home"); err != nil {
		t.Fatalf("un domaine du projet doit être accepté : %v", err)
	}
}

func TestDomainesDKIMSansActivationRefuses(t *testing.T) {
	// Des domaines déclarés sans activation : l'intention est ambiguë, et la
	// signature n'aurait pas lieu sans que rien ne le dise.
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Mail = &model.Mail{DKIM: &model.DKIM{Domains: []string{"shop.fr"}}}
	if err := Project(p, "/home"); err == nil || !strings.Contains(err.Error(), "pas activée") {
		t.Fatalf("des domaines sans activation doivent être refusés, obtenu : %v", err)
	}
}

func TestLesJournauxSontRefusesSousLesHomes(t *testing.T) {
	// Un répertoire du home se renomme sans privilège : un journal que le serveur
	// web ouvre en root ne peut pas y vivre (D71).
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Layout.LogDir = "/home/shop.fr/journaux"
	if err := Project(p, "/home"); err == nil || !strings.Contains(err.Error(), "spec.layout.logDir") {
		t.Fatalf("un répertoire de journaux sous les homes doit être refusé, obtenu %v", err)
	}
}

func TestLeJournalDUneTacheNEstQuUnNom(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Crons = []model.CronJob{{Name: "purge", Schedule: "0 3 * * *", Command: "bin/console x", Log: "purge.log"}}
	if err := Project(p, "/home"); err != nil {
		t.Fatalf("un nom de fichier doit être accepté : %v", err)
	}
	for _, log := range []string{"/etc/cron.d/x", "../x.log", "a b.log", ".."} {
		p.Spec.Crons[0].Log = log
		if err := Project(p, "/home"); err == nil || !strings.Contains(err.Error(), ".log") {
			t.Errorf("%q doit être refusé, obtenu %v", log, err)
		}
	}
}

func TestCustomAppCommand(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*model.Project)
		refused string
	}{
		{"commande absente", func(p *model.Project) {}, "exige la commande"},
		{"chemin relatif", func(p *model.Project) { p.Spec.App.Command = "bin/serveur --port ${PORT}" }, "chemin absolu"},
		// Les préfixes d'ExecStart « + » et « ! » lanceraient la commande en root.
		{"préfixe privilégié", func(p *model.Project) { p.Spec.App.Command = "+/usr/bin/serveur" }, "chemin absolu"},
		{"saut de ligne", func(p *model.Project) { p.Spec.App.Command = "/usr/bin/serveur\nUser=root" }, "une seule ligne"},
		{"runtime ignoré", func(p *model.Project) {
			p.Spec.App.Command = "/usr/bin/serveur"
			p.Spec.App.Runtime = "22.11.0"
		}, "spec.app.runtime"},
		{"entrypoint ignoré", func(p *model.Project) {
			p.Spec.App.Command = "/usr/bin/serveur"
			p.Spec.App.Entrypoint = "server.js"
		}, "spec.app.entrypoint"},
		{"variable invalide", func(p *model.Project) {
			p.Spec.App.Command = "/usr/bin/serveur"
			p.Spec.App.Env = map[string]string{"NOM-INVALIDE": "x"}
		}, "nom de variable"},
		{"écoute inconnue", func(p *model.Project) {
			p.Spec.App.Command = "/usr/bin/serveur"
			p.Spec.App.Listen = "tcp"
		}, "socket ou port"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newProject(t, "api.fr", naming.EnvProd, model.KindCustom)
			c.mutate(p)
			err := Project(p, "/home")
			if err == nil || !strings.Contains(err.Error(), c.refused) {
				t.Fatalf("attendu un refus contenant %q, obtenu : %v", c.refused, err)
			}
		})
	}

	p := newProject(t, "api.fr", naming.EnvProd, model.KindCustom)
	p.Spec.App.Command = "/home/api.fr/www/bin/serveur --listen ${LISTEN_ADDR}"
	p.Spec.App.EnvFile = true
	if err := Project(p, "/home"); err != nil {
		t.Fatalf("une application custom complète doit être acceptée : %v", err)
	}
}

func TestCommandAndEnvFileReservedToTheirKinds(t *testing.T) {
	p := newProject(t, "api.fr", naming.EnvProd, model.KindNode)
	p.Spec.App.Runtime = "22.11.0"
	p.Spec.App.Command = "/usr/bin/node autre.js"
	if err := Project(p, "/home"); err == nil || !strings.Contains(err.Error(), "réservé au type custom") {
		t.Fatalf("une commande sur un type à runtime doit être refusée, obtenu : %v", err)
	}
	// Le fichier d'environnement vaut pour toute application supervisée…
	p.Spec.App.Command = ""
	p.Spec.App.EnvFile = true
	if err := Project(p, "/home"); err != nil {
		t.Fatalf("envFile doit être accepté pour Node : %v", err)
	}
	// … et pour elles seules.
	s := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	s.Spec.App.EnvFile = true
	if err := Project(s, "/home"); err == nil || !strings.Contains(err.Error(), "spec.app.envFile") {
		t.Fatalf("envFile sur un site statique doit être refusé, obtenu : %v", err)
	}
}

func TestDBAccountsRefuseLesComptesSGBDReserves(t *testing.T) {
	// Un projet pouvait déclarer root, postgres ou le compte de supervision :
	// il en recevait des droits sur sa base, et « db user rotate » changeait le
	// mot de passe du compte d'administration du moteur (D91).
	for _, c := range []struct {
		engine, user string
		extra        bool
	}{
		{"mariadb", "root", false}, {"mysql", "mysql", false}, {"mysql", "telegraf", true},
		{"postgres", "postgres", false}, {"postgres", "root", true}, {"postgres", "pg_monitor", false},
		{"postgres", "telegraf", false}, {"mysql", "Root", false},
	} {
		p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
		d := model.Database{Engine: c.engine, Name: "shopfr", User: "shopfr", Privileges: "all"}
		if c.extra {
			d.ExtraUsers = []model.DBUser{{User: c.user, Privileges: "readonly"}}
		} else {
			d.User = c.user
		}
		p.Spec.Databases = []model.Database{d}
		err := DBAccounts(p)
		if err == nil || !strings.Contains(err.Error(), "réservé") {
			t.Errorf("%s %s : le compte doit être refusé : %v", c.engine, c.user, err)
		}
		// Project, qui juge tout projet chargé, ne le refuse pas : un seul projet
		// d'avant D91 aurait arrêté les commandes de tout le parc.
		if err := Project(p, "/home"); err != nil {
			t.Errorf("%s %s : Project ne doit pas refuser un projet déjà déclaré : %v", c.engine, c.user, err)
		}
	}
	// Les noms que la configuration désigne s'y ajoutent.
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Databases = []model.Database{{Engine: "mysql", Name: "shopfr", User: "metriques", Privileges: "all"}}
	if err := Project(p, "/home"); err != nil {
		t.Fatalf("un nom ordinaire doit passer : %v", err)
	}
	if err := DBAccounts(p, "metriques"); err == nil || !strings.Contains(err.Error(), "configuration") {
		t.Errorf("le compte de supervision configuré doit être refusé : %v", err)
	}
	if err := DBAccounts(p, "autre"); err != nil {
		t.Errorf("aucun refus attendu : %v", err)
	}
}

// Le nom d'un certificat devient un répertoire de /var/lib/ogs/acme : libre, il
// visait le wildcard partagé (« @… », rangé sous _shared-…) ou sortait du
// répertoire par « / » et « .. » — et l'émission y écrivait une clé privée, sa
// compensation y faisait un RemoveAll (R1-08).
func TestNomDeCertificatRefuseCeQuiSortDeSonEspace(t *testing.T) {
	for _, nom := range []string{
		"@wildcard-prep", "_shared-wildcard-prep", "../../../etc/ssl", "a/b", ".", "..", ".cache",
	} {
		p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
		p.Spec.Certificates = []model.Certificate{{Name: nom, Domains: []string{"shop.fr"}}}
		err := CertificateNames(p)
		if err == nil || !strings.Contains(err.Error(), "spec.certificates[0].name") {
			t.Errorf("le nom de certificat %q doit être refusé : %v", nom, err)
		}
		// Le chargement du parc, lui, ne s'arrête pas sur une déclaration fautive :
		// il arrêterait le renouvellement de tous les certificats.
		if err := Project(p, "/home"); err != nil {
			t.Errorf("Project ne doit pas refuser %q, que plan et apply refusent : %v", nom, err)
		}
	}
	// Un nom hérité qui ne sort de rien — un joker, une espace, un nom long —
	// fonctionnait : il continue de fonctionner.
	for _, nom := range []string{"", "shopfr", "shop.fr", "*.cc.fr", "shop fr", "Shop_FR", strings.Repeat("a", 80)} {
		p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
		p.Spec.Certificates = []model.Certificate{{Name: nom, Domains: []string{"shop.fr"}}}
		if err := CertificateNames(p); err != nil {
			t.Errorf("le nom de certificat %q doit être accepté : %v", nom, err)
		}
	}
}

// Le jeu d'identifiants DNS désigné est un fichier de /etc/ogs/credentials/dns
// (D97) : un nom qui en sort est refusé au projet visé, jamais au chargement.
func TestJeuDIdentifiantsDNSDesigneParUnCertificat(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Certificates = []model.Certificate{{Domains: []string{"shop.fr"}, DNSCredentials: "../master"}}
	if err := CertificateNames(p); err == nil || !strings.Contains(err.Error(), "dnsCredentials") {
		t.Errorf("le jeu « ../master » doit être refusé : %v", err)
	}
	if err := Project(p, "/home"); err != nil {
		t.Errorf("le chargement ne doit pas le refuser : %v", err)
	}
	p.Spec.Certificates[0].DNSCredentials = "client-a"
	if err := CertificateNames(p); err != nil {
		t.Errorf("le jeu « client-a » doit être accepté : %v", err)
	}
}

// Deux certificats d'un même projet sous un même nom se partagent un
// répertoire : chaque apply réémettait l'un sur l'autre, et sous ACME le quota
// de l'autorité s'épuisait. Refusé aux projets visés ; le chargement du parc,
// lui, le tolère.
func TestNomDeCertificatEnDoubleDansLeProjet(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Certificates = []model.Certificate{
		{Name: "shopfr", Domains: []string{"shop.fr"}},
		{Domains: []string{"www.shop.fr"}}, // nom implicite : l'identifiant
	}
	if got := DuplicateCertificates(p); len(got) != 1 || got[0] != "shopfr" {
		t.Fatalf("doublon non relevé : %v", got)
	}
	if err := CertificateNames(p); err == nil || !strings.Contains(err.Error(), "shopfr") {
		t.Fatalf("le doublon doit être refusé au projet visé : %v", err)
	}
	if err := Project(p, "/home"); err != nil {
		t.Fatalf("un doublon hérité ne doit pas refuser la déclaration : %v", err)
	}
}

// Une collision entre deux voisins n'arrête pas la commande qui vise un
// troisième projet : elle sera refusée à la leur.
func TestUniciteRestreinteAuxProjetsVises(t *testing.T) {
	a := newProject(t, "aa.fr", naming.EnvProd, model.KindStatic)
	b := newProject(t, "bb.fr", naming.EnvProd, model.KindStatic)
	c := newProject(t, "cc.fr", naming.EnvProd, model.KindStatic)
	b.Spec.Certificates = []model.Certificate{{Name: "aafr", Domains: []string{"bb.fr"}}}
	parc := []*model.Project{a, b, c}
	if err := UniquenessFor(parc, "preprod-pp.net", "ccfr"); err != nil {
		t.Errorf("ccfr n'est dans aucun conflit : %v", err)
	}
	for _, id := range []string{"aafr", "bbfr"} {
		if err := UniquenessFor(parc, "preprod-pp.net", id); err == nil {
			t.Errorf("%s est dans le conflit : il doit être refusé", id)
		}
	}
}

// Un certificat nommé d'après l'identifiant d'un voisin s'écrivait dans le
// répertoire du certificat implicite de ce voisin : le vhost du voisin
// présentait alors le certificat d'un autre, et la compensation d'une émission
// ratée le supprimait.
func TestUnicitéDesNomsDeCertificat(t *testing.T) {
	a := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	b := newProject(t, "autre.fr", naming.EnvProd, model.KindStatic)
	b.Spec.Certificates = []model.Certificate{{Name: "shopfr", Domains: []string{"autre.fr"}}}
	err := Uniqueness([]*model.Project{a, b}, "preprod-pp.net")
	if err == nil || !strings.Contains(err.Error(), "certificat") {
		t.Fatalf("un certificat au nom du voisin doit être refusé : %v", err)
	}

	b.Spec.Certificates[0].Name = "commun"
	a.Spec.Certificates = []model.Certificate{{Name: "commun", Domains: []string{"shop.fr"}}}
	err = Uniqueness([]*model.Project{a, b}, "preprod-pp.net")
	if err == nil || !strings.Contains(err.Error(), "commun") {
		t.Fatalf("deux projets ne partagent pas un nom de certificat : %v", err)
	}

	// Un projet qui nomme son certificat d'après son propre identifiant ne
	// rencontre que lui-même.
	a.Spec.Certificates[0].Name = "shopfr"
	b.Spec.Certificates[0].Name = "autrefr"
	if err := Uniqueness([]*model.Project{a, b}, "preprod-pp.net"); err != nil {
		t.Fatalf("chacun son certificat : %v", err)
	}
}

// Un vhost déclaré sert ses propres domaines, que spec.domains ne liste pas
// forcément : la clé « domaine » ne lisait que ces derniers, et blogfr pouvait
// servir shop.fr par spec.web.vhosts — le voisin détourné, puis refusé à son
// propre apply sur le vhost de l'autre (R4-02).
func TestUnicitéLitLesDomainesDesVhostsDéclarés(t *testing.T) {
	shop := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	blog := newProject(t, "blog.fr", naming.EnvProd, model.KindStatic)
	blog.Spec.Web.VHosts = []model.VHost{
		{Name: model.MainPoolName, Domains: []string{"blog.fr"}},
		{Name: "promo", Domains: []string{"Shop.fr."}},
	}
	err := UniquenessFor([]*model.Project{shop, blog}, "preprod-pp.net", "blogfr")
	if err == nil || !strings.Contains(err.Error(), `"shop.fr" est déjà utilisé par le projet "shopfr"`) {
		t.Fatalf("le domaine d'un vhost déclaré doit entrer dans le contrôle : %v", err)
	}
	// Un même domaine dans spec.domains et dans un vhost du même projet n'est
	// pas un conflit.
	blog.Spec.Web.VHosts[1].Domains = []string{"www.blog.fr"}
	if err := Uniqueness([]*model.Project{shop, blog}, "preprod-pp.net"); err != nil {
		t.Errorf("un projet ne peut pas entrer en conflit avec lui-même : %v", err)
	}
}

// Le répertoire de surcharge d'un vhost porte son nom, non celui de son
// fichier : un vhost adopté « shopfr », écrit dans le fichier nginx « shopfr »
// sans extension, et le projet shopfr, écrit dans shopfr.conf, n'ont aucun
// fichier commun mais partagent /etc/nginx/projects/shopfr/ — les surcharges de
// l'un s'appliquaient au site de l'autre (R3N-05).
func TestUnicitéDétecteUnRépertoireDeSurchargePartagé(t *testing.T) {
	adopté := newProject(t, "shop-fr.com", naming.EnvProd, model.KindStatic)
	adopté.Metadata.Labels = map[string]string{"adopted": "true"}
	adopté.Spec.Web.VHosts = []model.VHost{{Name: "shopfr", File: "shopfr", Domains: []string{"shop-fr.com"}}}
	shop := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	err := UniquenessFor([]*model.Project{adopté, shop}, "preprod-pp.net", "shopfr")
	if err == nil || !strings.Contains(err.Error(), "répertoire de surcharge") ||
		!strings.Contains(err.Error(), `"shopfrcom"`) || !strings.Contains(err.Error(), "spec.web.vhosts[].name") {
		t.Fatalf("le répertoire de surcharge partagé doit être refusé : %v", err)
	}
	// Un fichier commun est déjà nommé comme tel : le répertoire, qui en découle,
	// n'ajoute pas un second refus pour la même collision.
	adopté.Spec.Web.VHosts[0].File = "shopfr.conf"
	err = Uniqueness([]*model.Project{adopté, shop}, "preprod-pp.net")
	if err == nil || !strings.Contains(err.Error(), "fichier de vhost") ||
		strings.Contains(err.Error(), "répertoire de surcharge") {
		t.Errorf("un fichier commun doit être nommé une seule fois, comme fichier : %v", err)
	}
}

// MySQL et MariaDB sont un même serveur pour ogs : le provider les administre
// par le même client et le même socket. Les clés « mysql:x » et « mariadb:x »
// laissaient un projet déclarer la base et le compte d'un voisin (R1-23).
func TestUnicitéConfondMySQLEtMariaDB(t *testing.T) {
	shop := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	shop.Spec.Databases = []model.Database{{Engine: "mysql", Name: "shopfr", User: "shopfr"}}
	blog := newProject(t, "blog.fr", naming.EnvProd, model.KindStatic)
	blog.Spec.Databases = []model.Database{{Engine: "mariadb", Name: "shopfr", User: "blogfr",
		ExtraUsers: []model.DBUser{{User: "shopfr"}}}}
	err := UniquenessFor([]*model.Project{shop, blog}, "preprod-pp.net", "blogfr")
	if err == nil || !strings.Contains(err.Error(), "base de données") ||
		!strings.Contains(err.Error(), "utilisateur de base") {
		t.Fatalf("une base et un compte MariaDB homonymes d'un voisin MySQL doivent être refusés : %v", err)
	}
	// PostgreSQL reste un autre moteur.
	blog.Spec.Databases = []model.Database{{Engine: "postgres", Name: "shopfr", User: "shopfr"}}
	if err := Uniqueness([]*model.Project{shop, blog}, "preprod-pp.net"); err != nil {
		t.Errorf("deux moteurs distincts doivent cohabiter : %v", err)
	}
}

// Le refus d'un domaine nomme le nom déclaré quand sa forme canonique en
// diffère : « www.shop.fr » refusé sous la seule clé « shop.fr » égarait.
func TestUnicitéNommeLeDomaineDéclaré(t *testing.T) {
	shop := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	blog := newProject(t, "blog.fr", naming.EnvProd, model.KindStatic)
	blog.Spec.Web.VHosts = []model.VHost{
		{Name: model.MainPoolName, Domains: []string{"blog.fr"}},
		{Name: "promo", Domains: []string{"WWW.Shop.fr"}},
	}
	err := Uniqueness([]*model.Project{shop, blog}, "preprod-pp.net")
	if err == nil || !strings.Contains(err.Error(), `(demandé par "blogfr" pour "WWW.Shop.fr")`) {
		t.Fatalf("le refus doit nommer le domaine déclaré : %v", err)
	}
	blog.Spec.Web.VHosts[1].Domains = []string{"shop.fr"}
	if err := Uniqueness([]*model.Project{shop, blog}, "preprod-pp.net"); err == nil || strings.Contains(err.Error(), " pour ") {
		t.Errorf("un nom déjà canonique n'est pas répété : %v", err)
	}
}

func TestUnicitéDesRépertoiresDeJournaux(t *testing.T) {
	// Les journaux partent avec le projet par un effacement récursif (D71) : un
	// logDir recopié d'un voisin, ou logé sous le sien, faisait effacer ses
	// journaux à la suppression (R1-26, D114).
	a := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	b := newProject(t, "blog.fr", naming.EnvProd, model.KindStatic)
	if a.Spec.Layout.LogDir == "" || a.Spec.Layout.LogDir == b.Spec.Layout.LogDir {
		t.Fatalf("journaux attendus distincts : %q, %q", a.Spec.Layout.LogDir, b.Spec.Layout.LogDir)
	}
	if err := Uniqueness([]*model.Project{a, b}, "preprod-pp.net"); err != nil {
		t.Fatalf("deux projets ordinaires doivent cohabiter : %v", err)
	}
	for _, logDir := range []string{a.Spec.Layout.LogDir, a.Spec.Layout.LogDir + "/", a.Spec.Layout.LogDir + "/blog"} {
		b.Spec.Layout.LogDir = logDir
		err := UniquenessFor([]*model.Project{a, b}, "preprod-pp.net", "blogfr")
		if err == nil || !strings.Contains(err.Error(), "répertoire de journaux") ||
			!strings.Contains(err.Error(), "shopfr") {
			t.Errorf("logDir %q : le conflit avec les journaux de shopfr doit être nommé : %v", logDir, err)
		}
	}
	// Un nom qui en prolonge un autre n'est pas imbriqué.
	b.Spec.Layout.LogDir = a.Spec.Layout.LogDir + "-ancien"
	if err := Uniqueness([]*model.Project{a, b}, "preprod-pp.net"); err != nil {
		t.Errorf("deux répertoires voisins doivent cohabiter : %v", err)
	}
}

// Une cible de redirection que le modèle écarte (D119) — pas un nom d'hôte, ou
// une boucle — n'est jamais refusée au chargement : la cible était ignorée
// avant D119, et une déclaration servie jusque-là arrêterait sinon tout le parc,
// « cert renew » compris. Le plan du projet et doctor la signalent.
func TestUneCibleDeRedirectionEcarteeNArretePasLeChargement(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Domains = append(p.Spec.Domains,
		model.Domain{Name: "url.fr", Role: "redirect", Target: "https://autre.fr/page"},
		model.Domain{Name: "a.fr", Role: "redirect", Target: "b.fr"},
		model.Domain{Name: "b.fr", Role: "redirect", Target: "a.fr"})
	if len(p.RedirectProblems()) == 0 {
		t.Fatal("le modèle doit écarter ces cibles")
	}
	if err := Project(p, "/home"); err != nil {
		t.Errorf("le chargement ne doit pas refuser une cible écartée : %v", err)
	}
	if err := Uniqueness([]*model.Project{p}, "preprod-pp.net"); err != nil {
		t.Errorf("ni l'unicité : %v", err)
	}
}
