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

func TestNormalizeDowngradesPresetForPrep(t *testing.T) {
	prod := newProject(t, "shop.fr", naming.EnvProd, model.KindSymfony)
	prep := newProject(t, "shop.fr", naming.EnvPrep, model.KindSymfony)
	if prod.Spec.PHP.Pools[0].Preset != "medium" || prep.Spec.PHP.Pools[0].Preset != "small" {
		t.Errorf("presets : prod=%q prep=%q", prod.Spec.PHP.Pools[0].Preset, prep.Spec.PHP.Pools[0].Preset)
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

func TestProjectRejectsUnsupportedPHP(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindPrestashop)
	p.Spec.PHP.Pools = []model.Pool{{Name: "web", Version: "7.2"}}
	err := Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "non gérée") {
		t.Fatalf("PHP 7.2 devrait être refusé, obtenu : %v", err)
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
	if _, eol := EOLDate("7.4"); !eol {
		t.Error("PHP 7.4 devrait être signalé en fin de support")
	}
	if _, eol := EOLDate("8.4"); eol {
		t.Error("PHP 8.4 ne devrait pas être signalé en fin de support")
	}
}

func TestProjectRejectsHomeOutsideRoot(t *testing.T) {
	p := newProject(t, "shop.fr", naming.EnvProd, model.KindStatic)
	p.Spec.Layout.Root = "/srv/shop.fr"
	p.Spec.User.Home = "/srv/shop.fr"
	err := Project(p, "/home")
	if err == nil || !strings.Contains(err.Error(), "doit être sous /home") {
		t.Fatalf("un home hors racine doit être refusé, obtenu : %v", err)
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
	if err := Project(p, "/home"); err == nil {
		t.Fatal("sans marque d'adoption, un home hors racine doit être refusé")
	}
	p.Metadata.Labels = map[string]string{"adopted": "true"}
	if err := Project(p, "/home"); err != nil {
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
