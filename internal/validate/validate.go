// Package validate porte les règles transverses : cohérence d'un projet et
// unicité au sein du parc.
package validate

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/naming"
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
	reDBObject  = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)
	reMode      = regexp.MustCompile(`^0[0-7]{3}$`)
	rePHPVer    = regexp.MustCompile(`^[78]\.[0-9]$`)
	reQueueName = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
)

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
	if !filepath.IsAbs(root) {
		add("spec.layout.root", "chemin absolu attendu, obtenu %q", root)
	} else if !isUnder(root, homeRoot) {
		// Un projet adopté décrit l'existant : son arborescence peut vivre hors de
		// la racine conventionnelle, et la refuser rendrait l'import inutilisable.
		// Les emplacements manifestement dangereux restent interdits.
		if !adopted {
			add("spec.layout.root", "doit être sous %s, obtenu %q "+
				"(un projet adopté peut y échapper : voir « ogs import »)", homeRoot, root)
		} else if forbiddenRoot(root) {
			add("spec.layout.root", "emplacement interdit pour une racine de projet : %q", root)
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
	validatePHP(p, add)
	validateAMQP(p, add)
	validateCrons(p, add)
	validateCertificates(p, add)
	return errs.OrNil()
}

// validateCertificates contrôle les certificats déclarés. Le provider DNS est
// refusé dès la déclaration : le découvrir au moment de l'émission ferait échouer
// un apply qui a déjà créé le site.
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
		if strings.ContainsAny(c.Name, "/. ") {
			add(f+".name", "nom %q invalide : ni point, ni barre oblique, ni espace", c.Name)
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
	// Le propriétaire d'une clé est repéré par sa position, et non par son nom :
	// deux projets distincts peuvent porter le même identifiant, et c'est
	// précisément la collision à détecter.
	kinds := []struct {
		label string
		seen  map[string]int
		keys  func(*model.Project) []string
	}{
		{"identifiant", map[string]int{}, func(p *model.Project) []string { return []string{p.Metadata.Name} }},
		{"host effectif", map[string]int{}, func(p *model.Project) []string {
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
					errs = append(errs, Error{
						Field: k.label,
						Message: fmt.Sprintf("%q est déjà utilisé par le projet %q (demandé par %q)",
							key, projects[owner].Metadata.Name, p.Metadata.Name),
					})
					continue
				}
				k.seen[key] = index
			}
		}
	}
	return errs.OrNil()
}

// poolRole traduit le nom logique d'un pool en suffixe de rôle : le pool
// principal (« web ») porte le nom nu du projet, les autres un suffixe.
func poolRole(name string) string {
	if name == "" || name == "web" {
		return ""
	}
	return name
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
