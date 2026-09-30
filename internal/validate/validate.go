// Package validate porte les règles transverses : cohérence d'un projet et
// unicité au sein du parc.
package validate

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/provider/tls"
)

// Error est une erreur de validation localisée dans le document.
type Error struct {
	Field   string
	Message string
}

func (e Error) Error() string { return fmt.Sprintf("%s : %s", e.Field, e.Message) }

// Errors agrège les erreurs afin de toutes les remonter d'un coup, plutôt que de
// contraindre l'utilisateur à corriger une ligne à la fois.
type Errors []Error

func (errs Errors) Error() string {
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "\n")
}

// OrNil renvoie nil si aucune erreur n'a été collectée.
func (errs Errors) OrNil() error {
	if len(errs) == 0 {
		return nil
	}
	return errs
}

var (
	reDBObject    = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)
	reMode        = regexp.MustCompile(`^0[0-7]{3}$`)
	rePHPVer      = regexp.MustCompile(`^[78]\.[0-9]$`)
	reQueueName   = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
	reServiceName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
)

// maxServiceName borne le nom d'un service par ce que systemd accepte, et non
// par ce qui paraît raisonnable : un nom d'unité tient dans 255 octets, et le
// nom du service n'en occupe qu'une part — « ogs-<id>-<nom>@<n>.service ». La
// borne est donc large à dessein. Une borne serrée n'écarterait aucun danger et
// invaliderait des projets déjà en service.
const maxServiceName = 200

// supportedPHP énumère les versions gérées (D39).
// dnsProviderOVH est le seul provider DNS géré (D27).
const dnsProviderOVH = "ovh"

var supportedPHP = []string{"7.4", "8.0", "8.1", "8.2", "8.3", "8.4", "8.5"}

// phpEOL indique les versions dont le support est terminé. Elles restent
// utilisables : un Prestashop ancien peut exiger 7.4.
var phpEOL = map[string]string{
	"7.4": "2022-11-28",
	"8.0": "2023-11-26",
	"8.1": "2025-12-31",
}

// EOLDate renvoie la date de fin de support d'une version PHP, si elle est passée.
func EOLDate(version string) (string, bool) {
	d, ok := phpEOL[version]
	return d, ok
}

// Project valide un projet déjà normalisé.
func Project(p *model.Project, homeRoot string) error {
	var errs Errors
	add := func(field, format string, args ...any) {
		errs = append(errs, Error{Field: field, Message: fmt.Sprintf(format, args...)})
	}

	if p.APIVersion != model.APIVersion {
		add("apiVersion", "attendu %q, obtenu %q", model.APIVersion, p.APIVersion)
	}
	if p.Kind != model.KindProject {
		add("kind", "attendu %q, obtenu %q", model.KindProject, p.Kind)
	}
	if err := naming.ValidateIdentifier(p.Metadata.Name); err != nil {
		add("metadata.name", "%v", err)
	}
	if _, err := naming.NormalizeHost(p.Metadata.Host); err != nil {
		add("metadata.host", "%v", err)
	}
	if _, err := naming.ParseEnv(p.Metadata.Env); err != nil {
		add("metadata.env", "%v", err)
	}
	if _, err := p.AppType(); err != nil {
		add("spec.app.kind", "%v", err)
	}

	// Utilisateur et groupe portent l'identifiant du projet (D23). Un projet
	// adopté en est dispensé : renommer un compte existant exigerait de le
	// recréer, ce qui n'est pas une opération d'import.
	adopted := p.Metadata.Labels["adopted"] == "true"
	if !adopted {
		if p.Spec.User.Name != p.Metadata.Name {
			add("spec.user.name", "doit valoir l'identifiant du projet (%q), obtenu %q",
				p.Metadata.Name, p.Spec.User.Name)
		}
		if p.Spec.User.Group != p.Metadata.Name {
			add("spec.user.group", "doit valoir l'identifiant du projet (%q), obtenu %q",
				p.Metadata.Name, p.Spec.User.Group)
		}
	} else {
		if err := naming.ValidateIdentifier(p.Spec.User.Name); err != nil {
			add("spec.user.name", "%v", err)
		}
	}
	if p.Spec.User.UID != "" && p.Spec.User.UID != "auto" {
		if _, err := strconv.Atoi(p.Spec.User.UID); err != nil {
			add("spec.user.uid", "attendu « auto » ou un entier, obtenu %q", p.Spec.User.UID)
		}
	}
	switch p.Spec.User.WebAccess {
	case "group", "acl", "none":
	default:
		add("spec.user.webAccess", "attendu group, acl ou none, obtenu %q", p.Spec.User.WebAccess)
	}

	// Arborescence : le home doit rester sous la racine autorisée.
	root := filepath.Clean(p.Spec.Layout.Root)
	switch {
	case !filepath.IsAbs(root):
		add("spec.layout.root", "chemin absolu attendu, obtenu %q", root)
	case forbiddenRoot(root):
		// Les emplacements manifestement dangereux le sont pour tout le monde :
		// l'import reprend le home réel d'un compte du système, qui peut valoir
		// n'importe quoi. Le contrôle précède donc celui de l'appartenance à la
		// racine, où il n'était atteint que par un projet adopté.
		add("spec.layout.root", "emplacement interdit pour une racine de projet : %q", root)
	case root == filepath.Clean(homeRoot):
		// isUnder accepte l'égalité — filepath.Rel rend « . », qui n'est pas
		// « .. » —, si bien que la racine des homes passait le contrôle. L'apply
		// l'aurait alors chownée à l'utilisateur du projet (system.dirSteps pose
		// le propriétaire du projet sur spec.layout.root), lui donnant la main sur
		// le home de tous les voisins, et « ogs project delete » l'effacerait tout
		// entière. Elle appartient à root en 0711 et n'est le home d'aucun projet
		// (D60).
		add("spec.layout.root", "ne peut pas valoir la racine des homes elle-même (%s) : "+
			"elle appartient à root et n'est le home d'aucun projet (D60)", homeRoot)
	case !isUnder(root, homeRoot):
		// Un projet adopté décrit l'existant : son arborescence peut vivre hors de
		// la racine conventionnelle, et la refuser rendrait l'import inutilisable.
		if !adopted {
			add("spec.layout.root", "doit être sous %s, obtenu %q "+
				"(un projet adopté peut y échapper : voir « ogs import »)", homeRoot, root)
		}
	}
	if p.Spec.User.Home != root {
		add("spec.user.home", "doit valoir spec.layout.root (%q), obtenu %q", root, p.Spec.User.Home)
	}
	if !reMode.MatchString(p.Spec.Layout.Mode) {
		add("spec.layout.mode", "mode octal attendu (exemple 0701), obtenu %q", p.Spec.Layout.Mode)
	}
	for i, d := range p.Spec.Layout.Dirs {
		f := fmt.Sprintf("spec.layout.dirs[%d]", i)
		if d.Path == "" || filepath.IsAbs(d.Path) || strings.Contains(d.Path, "..") {
			add(f+".path", "chemin relatif au projet attendu, sans « .. », obtenu %q", d.Path)
		}
		if !reMode.MatchString(d.Mode) {
			add(f+".mode", "mode octal attendu, obtenu %q", d.Mode)
		}
		if d.Owner != "" && d.Owner != "root" {
			add(f+".owner", "seul « root » est admis comme propriétaire alternatif, obtenu %q", d.Owner)
		}
	}
	// Les journaux vivent hors du home (D71) : le serveur web les ouvre en root, et
	// un répertoire du home se renomme sans privilège — l'utilisateur y posait un
	// journal devenu lien vers un fichier système.
	if ld := filepath.Clean(p.Spec.Layout.LogDir); !filepath.IsAbs(ld) {
		add("spec.layout.logDir", "chemin absolu attendu, obtenu %q", p.Spec.Layout.LogDir)
	} else if isUnder(ld, filepath.Clean(homeRoot)) {
		add("spec.layout.logDir", "ne peut pas vivre sous la racine des homes (%s) : "+
			"l'utilisateur y renommerait le répertoire que le serveur web ouvre en root (D71)", ld)
	}
	if dr := p.Spec.Layout.Docroot; dr != "" && !isUnder(filepath.Clean(dr), root) {
		add("spec.layout.docroot", "doit être sous %q, obtenu %q", root, dr)
	}

	// Domaines : au moins un domaine principal, pas de doublon interne.
	seenDomain := map[string]bool{}
	primary := 0
	for i, d := range p.Spec.Domains {
		f := fmt.Sprintf("spec.domains[%d]", i)
		if _, err := naming.NormalizeHost(d.Name); err != nil {
			add(f+".name", "%v", err)
			continue
		}
		// La comparaison porte sur le nom littéral : « shop.fr » et
		// « www.shop.fr » ont la même forme canonique mais sont bien deux domaines
		// distincts à servir, cas de loin le plus courant.
		literal := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d.Name), "."))
		if seenDomain[literal] {
			add(f+".name", "domaine %q déclaré deux fois", d.Name)
		}
		seenDomain[literal] = true
		switch d.Role {
		case "", "primary":
			primary++
		case "alias":
		case "redirect":
			if d.Target == "" {
				add(f+".target", "une redirection exige une cible")
			}
		default:
			add(f+".role", "rôle %q inconnu (primary, alias ou redirect)", d.Role)
		}
	}
	if len(p.Spec.Domains) > 0 && primary == 0 {
		add("spec.domains", "au moins un domaine principal est requis")
	}

	validateDatabases(p, add)
	validateApp(p, add)
	validateVHosts(p, adopted, add)
	validatePHP(p, add)
	validateAMQP(p, add)
	validateCrons(p, add)
	validateServices(p, add)
	validateCertificates(p, add)
	validateMail(p, add)
	return errs.OrNil()
}

// validateMail contrôle la demande de signature DKIM (D69). Le refus est à la
// validation, non à l'apply : une préproduction ne doit jamais pouvoir signer au
// nom du domaine de production — un envoi de test parti vers de vrais clients
// serait authentifié —, et le découvrir après avoir écrit la clé serait trop tard.
func validateMail(p *model.Project, add func(string, string, ...any)) {
	if p.Spec.Mail == nil || p.Spec.Mail.DKIM == nil {
		return
	}
	dkim := p.Spec.Mail.DKIM
	if !dkim.Enabled {
		if len(dkim.Domains) > 0 {
			// Des domaines sans activation : l'intention est ambiguë, et la
			// signature n'aurait pas lieu sans que rien ne le dise.
			add("spec.mail.dkim.enabled", "des domaines sont déclarés mais la signature n'est pas activée")
		}
		return
	}
	if p.IsPrep() {
		add("spec.mail.dkim", "la signature DKIM est refusée en préproduction : "+
			"elle authentifierait des envois de test au nom du domaine de production")
		return
	}
	// Un domaine signé doit appartenir au projet : c'est ce qui rend la signature
	// unique dans le parc, les domaines l'étant déjà.
	own := map[string]bool{}
	for _, d := range p.Spec.Domains {
		own[strings.ToLower(strings.TrimSuffix(d.Name, "."))] = true
	}
	seen := map[string]bool{}
	for i, d := range dkim.Domains {
		f := fmt.Sprintf("spec.mail.dkim.domains[%d]", i)
		name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
		if _, err := naming.NormalizeHost(name); err != nil || strings.HasPrefix(name, "*") {
			add(f, "nom de domaine attendu, obtenu %q", d)
			continue
		}
		if seen[name] {
			add(f, "domaine %q déclaré deux fois", d)
		}
		seen[name] = true
		if !own[name] {
			add(f, "le domaine %q n'appartient pas aux domaines du projet (spec.domains)", d)
		}
	}
	if len(p.DKIMDomains()) == 0 {
		add("spec.mail.dkim.domains", "aucun domaine à signer : le projet n'a aucun domaine primaire")
	}
}

// validateCertificates contrôle les certificats déclarés. Le provider DNS est
// refusé dès la déclaration : le découvrir au moment de l'émission ferait échouer
// un apply qui a déjà créé le site.
// certificateNames rend les noms de certificat qu'un projet revendique : ceux
// qu'il déclare, l'identifiant pour un certificat sans nom, et l'identifiant
// toujours — c'est le nom du certificat implicite qu'un vhost TLS obtient sans
// rien déclarer, et un voisin qui le prendrait écrirait dans son répertoire.
func certificateNames(p *model.Project) []string {
	out := []string{p.ID()}
	for _, c := range p.Spec.Certificates {
		if c.Name != "" {
			out = append(out, c.Name)
		}
	}
	return out
}

// CertificateNames éprouve les noms de certificat d'un projet (D94). Le nom
// devient un répertoire de /var/lib/ogs/acme/certificates, où l'émission écrit
// une clé privée et que la compensation d'une émission ratée supprime en
// entier : un nom qui sort de l'espace des projets — « / », « . » en tête,
// « @… » qui désigne un certificat partagé, « _shared-… » son répertoire — est
// refusé, comme deux certificats sous un même nom. La règle du nom est celle de
// tls.CheckProjectName.
//
// Elle n'est pas portée par Project, que suit le chargement de tout le parc :
// une seule déclaration fautive aurait arrêté le renouvellement de tous les
// certificats, status, et le recalcul du parc à la suppression d'un voisin
// (D68). Elle est éprouvée pour les projets visés par plan et apply, et doctor
// nomme les déclarations à corriger. Tout autre nom — « *.cc.fr » compris —
// reste accepté : il ne mène nulle part ailleurs.
func CertificateNames(p *model.Project) error {
	var errs Errors
	for i, c := range p.Spec.Certificates {
		// Le jeu d'identifiants désigné est un nom de fichier sous
		// /etc/ogs/credentials/dns (D97) : il ne doit pas en sortir.
		if c.DNSCredentials != "" {
			if err := tls.CheckDNSCredentialsName(c.DNSCredentials); err != nil {
				errs = append(errs, Error{Field: fmt.Sprintf("spec.certificates[%d].dnsCredentials", i),
					Message: err.Error()})
			}
		}
		if c.Name == "" {
			continue
		}
		if msg := CertificateNameProblem(c.Name); msg != "" {
			errs = append(errs, Error{Field: fmt.Sprintf("spec.certificates[%d].name", i), Message: msg})
		}
	}
	// Deux certificats sous un même nom — deux sans nom, qui prennent tous deux
	// l'identifiant — se partagent un répertoire : chaque apply réémettait l'un
	// sur l'autre, le plan n'était jamais vide, et sous ACME le quota de
	// l'autorité s'épuisait jusqu'au refus, laissant un site sans certificat.
	for _, name := range DuplicateCertificates(p) {
		errs = append(errs, Error{Field: "spec.certificates", Message: fmt.Sprintf(
			"deux certificats s'appellent %q (l'identifiant du projet pour un certificat sans nom) : "+
				"ils s'écriraient dans le même répertoire — donner un nom distinct à chacun", name)})
	}
	return errs.OrNil()
}

// CertificateNameProblem dit pourquoi un nom de certificat sort de l'espace des
// projets, vide s'il n'en sort pas.
func CertificateNameProblem(name string) string {
	switch {
	case strings.ContainsAny(name, "/\x00"):
		return fmt.Sprintf("nom de certificat %q invalide : « / » mènerait hors du répertoire des certificats", name)
	case strings.HasPrefix(name, "."):
		return fmt.Sprintf("nom de certificat %q invalide : un « . » en tête désigne le répertoire "+
			"des certificats ou son parent", name)
	case strings.HasPrefix(name, "@"), strings.HasPrefix(name, "_shared-"):
		return fmt.Sprintf("nom de certificat %q invalide : « @… » et « _shared-… » désignent un "+
			"certificat partagé, qu'un projet ne déclare pas", name)
	}
	return ""
}

// DuplicateCertificates nomme les certificats qu'un projet déclare deux fois
// sous un même nom — l'identifiant pour un certificat sans nom : ils
// s'écrivent dans le même répertoire, le dernier émis écrasant l'autre.
// CertificateNames le refuse aux projets visés ; doctor le nomme.
func DuplicateCertificates(p *model.Project) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range p.Spec.Certificates {
		name := c.Name
		if name == "" {
			name = p.ID()
		}
		if seen[name] {
			out = append(out, name)
		}
		seen[name] = true
	}
	return out
}

func validateCertificates(p *model.Project, add func(string, string, ...any)) {
	for i, c := range p.Spec.Certificates {
		f := fmt.Sprintf("spec.certificates[%d]", i)
		switch c.Challenge {
		case "", "http-01", "selfsigned":
		case "dns-01":
			if c.DNSProvider != "" && c.DNSProvider != dnsProviderOVH {
				add(f+".dnsProvider", "provider %q non géré : ogs ne gère qu'%q",
					c.DNSProvider, dnsProviderOVH)
			}
		default:
			add(f+".challenge", "challenge %q inconnu (http-01, dns-01 ou selfsigned)", c.Challenge)
		}
		if len(c.Domains) == 0 {
			add(f+".domains", "au moins un domaine est requis")
		}
	}
}

func validateDatabases(p *model.Project, add func(string, string, ...any)) {
	for i, db := range p.Spec.Databases {
		f := fmt.Sprintf("spec.databases[%d]", i)
		switch db.Engine {
		case "mysql", "mariadb", "postgres":
		default:
			add(f+".engine", "moteur %q inconnu (mysql, mariadb ou postgres)", db.Engine)
		}
		// Les identifiants SQL ne sont pas paramétrables : la liste blanche est la
		// seule défense contre l'injection par nom d'objet (§15).
		if !reDBObject.MatchString(db.Name) {
			add(f+".name", "nom de base %q invalide : caractères autorisés [a-zA-Z0-9_]", db.Name)
		}
		if !reDBObject.MatchString(db.User) {
			add(f+".user", "nom d'utilisateur %q invalide : caractères autorisés [a-zA-Z0-9_]", db.User)
		}
		if len(db.User) > naming.MaxIdentifier && db.Engine != "postgres" {
			add(f+".user", "nom d'utilisateur %q trop long (%d > %d)", db.User, len(db.User), naming.MaxIdentifier)
		}
		switch db.Privileges {
		case "", "all", "crud", "readonly":
		default:
			add(f+".privileges", "jeu de privilèges %q inconnu (all, crud ou readonly)", db.Privileges)
		}
		for j, u := range db.ExtraUsers {
			if !reDBObject.MatchString(u.User) {
				add(fmt.Sprintf("%s.extraUsers[%d].user", f, j), "nom %q invalide", u.User)
			}
		}
	}
}

// reservedDBAccounts énumère, par moteur, les comptes qu'aucun projet ne doit
// déclarer (D91) : ceux du moteur lui-même, celui par lequel ogs l'administre et
// ceux de la supervision sous leur nom par défaut. Un projet qui les déclarait
// recevait des droits sur sa base, « db user rotate » changeait le mot de passe
// du compte d'administration — sous MariaDB, root perd alors l'authentification
// par socket dont ogs dépend pour tout le parc — et le scellait comme un secret
// du projet, que « secrets env » remettait à son application. Les noms à point ou
// à tiret (mysql.sys, mariadb.sys, debian-sys-maint) sont déjà exclus par la
// liste blanche des identifiants ; ils figurent ici pour que la règle ne dépende
// pas d'elle.
var reservedDBAccounts = map[string][]string{
	"mysql": {"root", "mysql", "mysql.sys", "mysql.session", "mysql.infoschema", "mariadb.sys",
		"debian-sys-maint", "public", "telegraf"},
	"postgres": {"postgres", "root", "public", "telegraf"},
}

// ReservedDBAccount dit pourquoi un nom de compte SGBD est réservé, ou rend une
// chaîne vide. extra porte les noms que la configuration désigne — les comptes de
// supervision, le rôle d'administration PostgreSQL — et que la seule déclaration
// d'un projet ne connaît pas. La comparaison ignore la casse : « Root » est un
// autre compte pour le moteur, mais il n'a sa place dans aucun projet.
func ReservedDBAccount(engine, user string, extra ...string) string {
	if engine == "mariadb" {
		engine = "mysql"
	}
	name := strings.ToLower(user)
	if engine == "postgres" && strings.HasPrefix(name, "pg_") {
		// PostgreSQL réserve le préfixe à ses rôles prédéfinis : pg_monitor existe
		// déjà, et le déclarer en ferait le propriétaire de la base du projet.
		return "préfixe pg_ réservé aux rôles prédéfinis de PostgreSQL"
	}
	for _, r := range reservedDBAccounts[engine] {
		if name == r {
			return "compte du moteur, de son administration ou de la supervision"
		}
	}
	for _, r := range extra {
		if r != "" && name == strings.ToLower(r) {
			return "compte que la configuration d'ogs désigne pour la supervision ou l'administration"
		}
	}
	return ""
}

// DBAccounts refuse les comptes SGBD réservés d'un projet, y compris ceux que la
// configuration désigne (voir ReservedDBAccount).
//
// Il ne fait pas partie de Project, et c'est voulu : Project juge tout projet
// chargé, et un seul projet d'avant D91 déclarant root rendait alors le code 3 à
// toute commande de parc — apply, cert renew du timer, project delete du fautif
// comme d'un voisin, dont le plan global ne se recalculait plus. Le contrôle
// vaut aux points d'écriture d'une déclaration ; ce qui est déjà déclaré est
// refusé par le provider, base par base, en étape non bloquante.
func DBAccounts(p *model.Project, extra ...string) error {
	var errs Errors
	checkDBAccounts(p, extra, func(field, format string, args ...any) {
		errs = append(errs, Error{Field: field, Message: fmt.Sprintf(format, args...)})
	})
	return errs.OrNil()
}

func checkDBAccounts(p *model.Project, extra []string, add func(string, string, ...any)) {
	for i, db := range p.Spec.Databases {
		f := fmt.Sprintf("spec.databases[%d]", i)
		if why := ReservedDBAccount(db.Engine, db.User, extra...); why != "" {
			add(f+".user", "compte %q réservé (%s) : déclarer un compte propre au projet", db.User, why)
		}
		for j, u := range db.ExtraUsers {
			if why := ReservedDBAccount(db.Engine, u.User, extra...); why != "" {
				add(fmt.Sprintf("%s.extraUsers[%d].user", f, j),
					"compte %q réservé (%s) : déclarer un compte propre au projet", u.User, why)
			}
		}
	}
}

// reEnvName est le nom d'une variable d'environnement tel que systemd l'accepte.
var reEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateApp contrôle la déclaration d'une application supervisée, et d'abord
// la commande d'une application custom (D75), recopiée telle quelle dans
// ExecStart.
func validateApp(p *model.Project, add func(string, string, ...any)) {
	at, err := model.Resolve(p.Spec.App.Kind, p.Spec.App.Version)
	if err != nil {
		return // déjà signalé sur spec.app.kind
	}
	app := p.Spec.App
	if p.Spec.App.Kind == model.KindCustom {
		command := strings.TrimSpace(app.Command)
		switch {
		case command == "":
			add("spec.app.command", "le type custom exige la commande à lancer")
		case strings.ContainsAny(command, "\n\r\x00"):
			// Un saut de ligne ferait écrire à ogs une directive de plus dans l'unité.
			add("spec.app.command", "la commande tient sur une seule ligne")
		case !strings.HasPrefix(command, "/"):
			// Le chemin absolu écarte aussi les préfixes d'ExecStart : « + » et « ! »
			// lanceraient la commande en root, au mépris de User=.
			add("spec.app.command", "le programme doit être désigné par un chemin absolu "+
				"(obtenu %q) ; les variables s'écrivent ${PORT}, ${SOCKET_PATH}", strings.Fields(command)[0])
		}
		// Ignorés sans un mot, ils laisseraient croire qu'ils s'appliquent.
		if app.Runtime != "" {
			add("spec.app.runtime", "sans objet pour le type custom : ogs n'installe aucun runtime")
		}
		if app.Entrypoint != "" {
			add("spec.app.entrypoint", "sans objet pour le type custom : la commande complète va dans spec.app.command")
		}
	} else if app.Command != "" {
		add("spec.app.command", "réservé au type custom : le type %q déduit sa commande de son runtime", app.Kind)
	}
	if at.Runtime == "" {
		if app.EnvFile {
			add("spec.app.envFile", "le type %q n'a pas d'application supervisée", app.Kind)
		}
		return
	}
	switch app.Listen {
	case "", "socket", "port":
	default:
		add("spec.app.listen", "attendu socket ou port, obtenu %q", app.Listen)
	}
	if app.Instances < 0 {
		add("spec.app.instances", "nombre d'instances négatif (%d)", app.Instances)
	}
	for _, k := range sortedEnvKeys(app.Env) {
		if !reEnvName.MatchString(k) {
			add("spec.app.env", "nom de variable %q invalide", k)
		}
		if strings.ContainsAny(app.Env[k], "\n\r\x00") {
			add("spec.app.env", "la valeur de %s tient sur une seule ligne", k)
		}
	}
}

func sortedEnvKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func validatePHP(p *model.Project, add func(string, string, ...any)) {
	at, err := model.Resolve(p.Spec.App.Kind, p.Spec.App.Version)
	if err == nil && at.NeedsPHP && len(p.Spec.PHP.Pools) == 0 {
		add("spec.php.pools", "le type d'application %q requiert un pool PHP", p.Spec.App.Kind)
	}
	seen := map[string]bool{}
	for i, pool := range p.Spec.PHP.Pools {
		f := fmt.Sprintf("spec.php.pools[%d]", i)
		if pool.Name == "" {
			add(f+".name", "nom de pool requis")
		}
		if seen[pool.Name] {
			add(f+".name", "pool %q déclaré deux fois", pool.Name)
		}
		seen[pool.Name] = true
		if !rePHPVer.MatchString(pool.Version) {
			add(f+".version", "version %q invalide (exemple : 8.3)", pool.Version)
			continue
		}
		if !contains(supportedPHP, pool.Version) {
			add(f+".version", "version %q non gérée (gérées : %s)", pool.Version, strings.Join(supportedPHP, ", "))
		}
		switch pool.Listen {
		case "", "unix", "tcp":
		default:
			add(f+".listen", "attendu unix ou tcp, obtenu %q", pool.Listen)
		}
		switch pool.Preset {
		case "", "micro", "small", "medium", "large", "custom":
		default:
			add(f+".preset", "preset %q inconnu", pool.Preset)
		}
		if pool.Number != "" && pool.Number != "auto" {
			n, err := strconv.Atoi(pool.Number)
			if err != nil || n < 1 || n > 65535 {
				add(f+".number", "numéro %q invalide", pool.Number)
			}
		}
	}
}

func validateAMQP(p *model.Project, add func(string, string, ...any)) {
	if p.Spec.AMQP == nil {
		return
	}
	a := p.Spec.AMQP
	if a.Vhost == "" {
		add("spec.amqp.vhost", "vhost requis")
	}
	if strings.HasPrefix(a.Vhost, "/") {
		add("spec.amqp.vhost", "le vhost ne doit pas commencer par « / » : cela impose un encodage %%2F dans les DSN")
	}
	if a.User == "" {
		add("spec.amqp.user", "utilisateur requis")
	}
	queues := map[string]bool{}
	for i, q := range a.Queues {
		f := fmt.Sprintf("spec.amqp.queues[%d]", i)
		if !reQueueName.MatchString(q.Name) {
			add(f+".name", "nom de queue %q invalide", q.Name)
		}
		if queues[q.Name] {
			add(f+".name", "queue %q déclarée deux fois", q.Name)
		}
		queues[q.Name] = true
		switch q.Type {
		case "", "classic", "quorum":
		default:
			add(f+".type", "type %q inconnu (classic ou quorum)", q.Type)
		}
	}
	exchanges := map[string]bool{}
	for _, e := range a.Exchanges {
		exchanges[e.Name] = true
	}
	for i, b := range a.Bindings {
		f := fmt.Sprintf("spec.amqp.bindings[%d]", i)
		if !exchanges[b.Exchange] {
			add(f+".exchange", "exchange %q non déclaré", b.Exchange)
		}
		if !queues[b.Queue] {
			add(f+".queue", "queue %q non déclarée", b.Queue)
		}
	}
}

// interditsJournal énumère ce qui n'a rien à faire dans le nom du journal d'une
// tâche : les caractères du nom de tâche, le point excepté — « purge.log » est
// le nom attendu —, et la barre oblique en tête, qui en ferait un chemin.
const interditsJournal = "/ \t\r\n%;&|<>()$`\"'\\*?[]{}#~!\x00"

// interditsCron énumère ce qui n'a rien à faire dans le nom d'une tâche, lequel
// sert à trois choses à la fois.
//
// Composant de chemin d'abord : le verrou /run/ogs/<id>/<nom>.lock et le journal
// logs/cron-<nom>.log — d'où le refus de la barre oblique et du point, « .. »
// remontant l'arborescence.
//
// Argument d'une commande de shell ensuite : ce chemin de verrou est recollé
// sans citation dans la ligne passée à /bin/sh, par cron comme par « ogs cron
// run ». L'espace l'y découpe, et « ; », « | » ou l'accent grave y enchaînent
// une seconde commande sous l'identité du projet.
//
// Contenu d'un fichier de /etc/cron.d enfin : « % » n'y est pas un caractère
// ordinaire — cron le remplace par un saut de ligne et fait passer la suite pour
// l'entrée standard de la commande. Le provider échappe la commande et le chemin
// de journal ; le nom, lui, ne l'est nulle part. Et un retour à la ligne, écrit
// tel quel dans le commentaire qui précède la tâche, n'y coupe pas une commande :
// il ouvre une entrée de cron entière, dont le champ d'utilisateur est libre.
const interditsCron = "/. \t\r\n%;&|<>()$`\"'\\*?[]{}#~!"

func validateCrons(p *model.Project, add func(string, string, ...any)) {
	seen := map[string]bool{}
	for i, c := range p.Spec.Crons {
		f := fmt.Sprintf("spec.crons[%d]", i)
		if c.Name == "" {
			add(f+".name", "nom requis")
		}
		if seen[c.Name] {
			add(f+".name", "tâche %q déclarée deux fois", c.Name)
		}
		seen[c.Name] = true
		if strings.ContainsAny(c.Name, interditsCron) {
			add(f+".name", "nom %q invalide : ni point, ni barre oblique, ni espace, ni « %% », "+
				"ni métacaractère de shell — ce nom devient un fichier de verrou, un fichier "+
				"de journal et une ligne de /etc/cron.d", c.Name)
		}
		if err := CronExpression(c.Schedule); err != nil {
			add(f+".schedule", "%v", err)
		}
		if c.Command == "" {
			add(f+".command", "commande requise")
		}
		if strings.ContainsAny(c.Command, "\n\r") {
			add(f+".command", "la commande ne peut pas contenir de retour à la ligne")
		}
		// Le journal d'une tâche est pré-créé par root, au nom du projet : un
		// chemin lui ferait poser ce fichier n'importe où. Seul un nom est admis,
		// dans le répertoire des journaux du projet (D71).
		if c.Log != "" && (c.Log == "." || c.Log == ".." || strings.ContainsAny(c.Log, interditsJournal)) {
			add(f+".log", "nom de fichier attendu, sans chemin ni métacaractère, obtenu %q (D71)", c.Log)
		}

	}
}

// validateServices contrôle les services déclarés. Le nom d'un service n'est pas
// une simple étiquette : il devient composant du nom d'unité systemd, composant
// de chemin sous /etc/systemd/system, et surtout élément d'une spécification de
// commande sudoers quand la délégation est en style « systemctl » (D28). Là, une
// virgule ouvre une seconde commande à laquelle le tag NOPASSWD se propage, un
// « : » ouvre une nouvelle paire hôtes = commandes et un « # » commente la fin de
// ligne : la règle produite reste syntaxiquement irréprochable pour visudo, et
// l'utilisateur du projet obtient un shell root sans mot de passe. Un « .. »
// sortirait quant à lui de /etc/systemd/system.
//
// La règle se cale donc sur ce qu'un nom d'unité systemd peut porter — lettres,
// chiffres, tiret, souligné et point —, moins les deux points, que systemd admet
// mais que sudoers relit, et moins l'arobase, qui sépare déjà le modèle de son
// instance. Le premier caractère est en outre une lettre, un chiffre ou un
// souligné : un tiret de tête passerait pour une option de systemctl, un point de
// tête pour un fichier caché. Refuser au-delà — la majuscule, par exemple —
// n'écarterait aucun danger et condamnerait un projet déjà en service : la
// validation étant en amont de tout chargement, un seul fichier refusé bloque
// l'apply du parc entier, sans qu'« ogs service remove » ni « ogs project
// delete » puissent le réparer.
func validateServices(p *model.Project, add func(string, string, ...any)) {
	seen := map[string]bool{}
	for i, s := range p.Spec.Services {
		f := fmt.Sprintf("spec.services[%d]", i)
		switch {
		case s.Name == "":
			add(f+".name", "nom requis")
		case !reServiceName.MatchString(s.Name):
			add(f+".name", "nom %q invalide : lettres, chiffres, tiret, souligné et point "+
				"seulement, en commençant par une lettre, un chiffre ou un souligné — ce nom "+
				"devient un nom d'unité systemd et une spécification de commande sudoers",
				s.Name)
		case strings.Contains(s.Name, ".."):
			add(f+".name", "nom %q invalide : « .. » sortirait de /etc/systemd/system", s.Name)
		case len(s.Name) > maxServiceName:
			add(f+".name", "nom %q trop long : %d caractères au plus", s.Name, maxServiceName)
		}
		if seen[s.Name] {
			// Deux services homonymes produisent deux étapes de même ressource sur
			// le même fichier d'unité : elles se marchent dessus dans le plan comme
			// dans l'audit.
			add(f+".name", "service %q déclaré deux fois", s.Name)
		}
		seen[s.Name] = true
		// Pas de plafond : le nombre d'instances n'est le vecteur de rien — il est
		// rendu par %d — et aucune décision n'en arbitre un. En inventer un
		// invaliderait un parc qui tourne.
		if s.Instances < 0 {
			add(f+".instances", "nombre d'instances négatif (%d)", s.Instances)
		}
	}
}

// CronExpression valide une expression cron à cinq champs.
func CronExpression(expr string) error {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return fmt.Errorf("cinq champs attendus (min heure jour mois jour-semaine), %d obtenu(s)", len(fields))
	}
	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	for i, f := range fields {
		if err := cronField(f, bounds[i][0], bounds[i][1]); err != nil {
			return fmt.Errorf("champ %d (%q) : %w", i+1, f, err)
		}
	}
	return nil
}

func cronField(f string, min, max int) error {
	for _, part := range strings.Split(f, ",") {
		if part == "" {
			return fmt.Errorf("élément vide")
		}
		if slash := strings.Index(part, "/"); slash >= 0 {
			step, err := strconv.Atoi(part[slash+1:])
			if err != nil || step < 1 {
				return fmt.Errorf("pas d'incrément invalide")
			}
			part = part[:slash]
			if part == "*" {
				continue
			}
		}
		if part == "*" {
			continue
		}
		lo, hi, found := strings.Cut(part, "-")
		vals := []string{lo}
		if found {
			vals = append(vals, hi)
		}
		for _, v := range vals {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("valeur %q non numérique", v)
			}
			if n < min || n > max {
				return fmt.Errorf("valeur %d hors bornes (%d à %d)", n, min, max)
			}
		}
	}
	return nil
}

// Uniqueness vérifie l'unicité des ressources au sein du parc. Les collisions de
// home sont contrôlées séparément des identifiants : deux projets aux
// identifiants distincts peuvent réclamer le même répertoire (§3.3.6).
func Uniqueness(projects []*model.Project, prepDomain string) error {
	var errs Errors
	for _, c := range uniquenessConflicts(projects, prepDomain) {
		errs = append(errs, c.err)
	}
	return errs.OrNil()
}

// UniquenessFor éprouve l'unicité sur tout le parc, mais ne retient que les
// conflits où l'un des projets nommés est détenteur ou demandeur (D94). Une
// collision entre deux autres projets ne doit pas arrêter la commande qui n'en
// vise aucun : elle sera refusée à la leur.
func UniquenessFor(projects []*model.Project, prepDomain string, ids ...string) error {
	visés := map[string]bool{}
	for _, id := range ids {
		visés[id] = true
	}
	var errs Errors
	for _, c := range uniquenessConflicts(projects, prepDomain) {
		if visés[c.a] || visés[c.b] {
			errs = append(errs, c.err)
		}
	}
	return errs.OrNil()
}

// conflict est une collision entre deux projets, nommés par identifiant.
type conflict struct {
	err  Error
	a, b string
}

func uniquenessConflicts(projects []*model.Project, prepDomain string) []conflict {
	var errs []conflict
	// Le propriétaire d'une clé est repéré par sa position, et non par son nom :
	// deux projets distincts peuvent porter le même identifiant, et c'est
	// précisément la collision à détecter.
	kinds := []struct {
		label string
		seen  map[string]int
		keys  func(*model.Project) []string
	}{
		{"identifiant", map[string]int{}, func(p *model.Project) []string { return []string{p.Metadata.Name} }},
		// Le host effectif est lu sur le domaine primaire, et seulement calculé à
		// défaut : un projet déclaré avant un changement de convention sert toujours
		// le nom inscrit dans sa déclaration, et le recalculer inventerait des
		// conflits entre des vhosts qui n'en ont aucun.
		{"host effectif", map[string]int{}, func(p *model.Project) []string {
			if d := p.PrimaryDomain(); d != "" {
				return []string{d}
			}
			h, err := naming.EffectiveHost(p.Metadata.Host, p.Env(), prepDomain)
			if err != nil {
				return nil
			}
			return []string{h}
		}},
		{"home", map[string]int{}, func(p *model.Project) []string {
			return []string{filepath.Clean(p.Spec.Layout.Root)}
		}},
		{"utilisateur système", map[string]int{}, func(p *model.Project) []string {
			return []string{p.Spec.User.Name}
		}},
		{"domaine", map[string]int{}, func(p *model.Project) []string {
			out := make([]string, 0, len(p.Spec.Domains))
			for _, d := range p.Spec.Domains {
				if h, err := naming.NormalizeHost(d.Name); err == nil {
					out = append(out, h)
				}
			}
			return out
		}},
		{"base de données", map[string]int{}, func(p *model.Project) []string {
			out := make([]string, 0, len(p.Spec.Databases))
			for _, db := range p.Spec.Databases {
				out = append(out, db.Engine+":"+db.Name)
			}
			return out
		}},
		{"utilisateur de base", map[string]int{}, func(p *model.Project) []string {
			var out []string
			for _, db := range p.Spec.Databases {
				out = append(out, db.Engine+":"+db.User)
				for _, u := range db.ExtraUsers {
					out = append(out, db.Engine+":"+u.User)
				}
			}
			return out
		}},
		{"vhost AMQP", map[string]int{}, func(p *model.Project) []string {
			if p.Spec.AMQP == nil {
				return nil
			}
			return []string{p.Spec.AMQP.Vhost}
		}},
		{"pool PHP", map[string]int{}, func(p *model.Project) []string {
			out := make([]string, 0, len(p.Spec.PHP.Pools))
			for _, pool := range p.Spec.PHP.Pools {
				out = append(out, naming.RoleName(p.Metadata.Name, poolRole(pool.Name)))
			}
			return out
		}},
		// Un fichier de vhost prolonge l'identifiant d'un rôle, comme un pool : le
		// vhost « b2b » du projet shop s'écrit dans shop_b2b.conf, qui est le
		// fichier principal d'un projet shop_b2b. Les deux se réécriraient ce
		// fichier à chaque apply, chacun servant ses domaines à la place de
		// l'autre.
		{"fichier de vhost", map[string]int{}, vhostFiles},
		// Le nom d'un certificat est son répertoire : deux projets sous un même
		// nom écrasent le certificat l'un de l'autre, clé privée comprise (D94).
		{"certificat", map[string]int{}, certificateNames},
	}
	for index, p := range projects {
		for _, k := range kinds {
			for _, key := range k.keys(p) {
				if key == "" {
					continue
				}
				if owner, taken := k.seen[key]; taken {
					// Un même projet peut légitimement produire deux fois la même
					// clé : « shop.fr » et « www.shop.fr » partagent leur forme
					// canonique. Seul un conflit entre projets distincts compte.
					if owner == index {
						continue
					}
					errs = append(errs, conflict{Error{
						Field: k.label,
						Message: fmt.Sprintf("%q est déjà utilisé par le projet %q (demandé par %q)",
							key, projects[owner].Metadata.Name, p.Metadata.Name),
					}, projects[owner].Metadata.Name, p.Metadata.Name})
					continue
				}
				k.seen[key] = index
			}
		}
	}
	// L'égalité des homes ne suffit pas : un home imbriqué dans celui d'un autre
	// projet est effacé avec lui, et « ogs project delete » du premier emportait
	// le second, toujours déclaré et servi. L'import en produit sans y penser : un
	// docroot /var/www/boutique devient un home, celui de blog.boutique.fr
	// (/var/www/boutique/blog) un autre, dessous. L'égalité reste signalée par la
	// clé « home » ci-dessus, une seule fois.
	//
	// Un parc écrit avant ce contrôle peut déjà porter deux homes imbriqués : le
	// refus vaut alors pour toute commande qui éprouve l'unicité, même au sujet
	// d'un troisième projet. Le message le dit, sans quoi l'exploitant chercherait
	// le conflit dans le projet qu'il est en train de créer.
	for i := range projects {
		for j := i + 1; j < len(projects); j++ {
			ri := filepath.Clean(projects[i].Spec.Layout.Root)
			rj := filepath.Clean(projects[j].Spec.Layout.Root)
			if ri == rj || !Overlap(ri, rj) {
				continue
			}
			outer, inner := projects[i], projects[j]
			if isUnder(ri, rj) {
				outer, inner = inner, outer
			}
			errs = append(errs, conflict{Error{
				Field: "home",
				Message: fmt.Sprintf("%q, home du projet %q, contient %q, celui du projet %q : "+
					"supprimer le premier effacerait le second — corriger spec.layout.root de l'un des deux, "+
					"le conflit pouvant précéder la commande en cours (une version antérieure d'ogs l'acceptait)",
					filepath.Clean(outer.Spec.Layout.Root), outer.Metadata.Name,
					filepath.Clean(inner.Spec.Layout.Root), inner.Metadata.Name),
			}, outer.Metadata.Name, inner.Metadata.Name})
		}
	}
	return errs
}

// Overlap dit si deux chemins se recouvrent — égaux, ou l'un sous l'autre —, au
// sens des composants et non des caractères : /var/www/boutique-old n'est pas
// sous /var/www/boutique. Effacer l'un emporterait alors tout ou partie de
// l'autre.
func Overlap(a, b string) bool {
	return isUnder(a, b) || isUnder(b, a)
}

// poolRole traduit le nom logique d'un pool en suffixe de rôle : le pool
// principal (« web ») porte le nom nu du projet, les autres un suffixe.
func poolRole(name string) string {
	if name == "" || name == "web" {
		return ""
	}
	return name
}

// vhostFiles énumère les fichiers de vhost qu'un projet écrit dans
// sites-available, par la règle même du provider web (model.Project.VHostFile) :
// le nom réel d'un vhost adopté, sinon le nom nu du projet pour le vhost
// principal et un suffixe de rôle pour les autres — y compris le vhost de
// redirection qu'un domaine « redirect » fait naître sans être déclaré.
func vhostFiles(p *model.Project) []string {
	vhosts := p.VHosts()
	out := make([]string, 0, len(vhosts))
	for _, vh := range vhosts {
		out = append(out, p.VHostFile(vh))
	}
	return out
}

// validateVHosts contrôle les vhosts déclarés. Le nom réel d'un fichier
// (« file ») est réservé aux vhosts adoptés, dont il désigne l'original : un
// projet d'ogs suit la convention de nommage. Il reste un nom simple — ni
// chemin, ni fichier caché, que le masque d'inclusion de nginx ne charge pas.
func validateVHosts(p *model.Project, adopted bool, add func(string, string, ...any)) {
	seen := map[string]bool{}
	files := vhostFiles(p)
	for i, vh := range p.Spec.Web.VHosts {
		f := fmt.Sprintf("spec.web.vhosts[%d]", i)
		if vh.File != "" {
			if !adopted {
				add(f+".file", "réservé à un vhost adopté (« ogs import ») : "+
					"un projet d'ogs nomme ses fichiers d'après son identifiant")
			}
			if vh.File != filepath.Base(vh.File) || strings.HasPrefix(vh.File, ".") ||
				strings.ContainsAny(vh.File, " \t\r\n\x00") {
				add(f+".file", "nom de fichier simple attendu, obtenu %q", vh.File)
			}
		}
		if i < len(files) {
			if seen[files[i]] {
				add(f+".name", "le fichier %s est déjà celui d'un autre vhost du projet", files[i])
			}
			seen[files[i]] = true
		}
	}
}

// forbiddenRoot refuse les emplacements qu'aucune racine de projet ne devrait
// occuper : les toucher mettrait le système en péril.
func forbiddenRoot(root string) bool {
	forbidden := []string{"/", "/etc", "/usr", "/bin", "/sbin", "/lib", "/boot",
		"/dev", "/proc", "/sys", "/run", "/var", "/var/lib", "/var/log", "/root", "/tmp"}
	clean := filepath.Clean(root)
	for _, f := range forbidden {
		if clean == f {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func isUnder(path, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
