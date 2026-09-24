package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// UFW est la déclaration du pare-feu, chargée depuis /etc/ogs/ufw.yaml (D72).
//
// Elle vit hors de config.yaml parce qu'elle n'a pas le même rythme : on y
// bannit une adresse un soir de scan, on y ajoute le poste d'un prestataire le
// temps d'une intervention. Son absence est une décision : sans fichier, ogs ne
// touche pas à ufw — ni règle, ni profil, ni politique.
type UFW struct {
	// Active est l'état voulu d'ufw. Nil vaut vrai : déclarer un pare-feu, c'est
	// vouloir qu'il filtre. Faux laisse profils et règles réconciliés, ufw
	// désactivé.
	Active *bool `yaml:"active,omitempty"`
	// Policy fixe les politiques par défaut. Un sens tu garde celle d'ufw.
	Policy UFWPolicy `yaml:"policy,omitempty"`
	// Logging est le niveau de journalisation d'ufw (off, low, medium, high,
	// full). Vide : laissé tel quel.
	Logging string `yaml:"logging,omitempty"`
	// RollbackAfter est le délai au terme duquel une première activation se
	// défait d'elle-même, faute de « ogs firewall confirm ». « 0 » l'interdit.
	RollbackAfter string `yaml:"rollbackAfter,omitempty"`

	// Sources nomme des adresses : à qui elles appartiennent, pourquoi elles sont
	// là. Une règle qui les emploie en tire son commentaire.
	Sources map[string]UFWSource `yaml:"sources,omitempty"`
	// Trusted a accès à tous les profils disponibles, une règle par profil.
	Trusted []UFWEntry `yaml:"trusted,omitempty"`
	// Profiles règle chaque profil d'application : sources autorisées, ports,
	// disponibilité forcée. Un nom hors du catalogue déclare un profil propre.
	Profiles map[string]UFWProfile `yaml:"profiles,omitempty"`
	// Ban énumère les adresses refusées, placées en tête des règles.
	Ban []UFWEntry `yaml:"ban,omitempty"`
	// Outgoing énumère les sorties permises lorsque la politique sortante n'est
	// pas « allow ».
	Outgoing UFWOutgoing `yaml:"outgoing,omitempty"`
}

// UFWPolicy porte les trois politiques par défaut d'ufw.
type UFWPolicy struct {
	Incoming string `yaml:"incoming,omitempty"`
	Outgoing string `yaml:"outgoing,omitempty"`
	// Routed n'est réglé que s'il est déclaré : le défaut d'ufw dépend de la
	// présence du routage, qu'ogs n'administre pas.
	Routed string `yaml:"routed,omitempty"`
}

// UFWSource est un profil d'adresses : un nom, un propriétaire, des adresses.
type UFWSource struct {
	// Owner dit à qui appartiennent les adresses : c'est la question qu'on se
	// pose devant une règle, des mois après l'avoir posée.
	Owner       string   `yaml:"owner,omitempty"`
	Description string   `yaml:"description,omitempty"`
	Addresses   []string `yaml:"addresses"`
}

// UFWProfile règle un profil d'application.
type UFWProfile struct {
	// Allow énumère les sources autorisées, en plus des adresses de confiance.
	Allow []UFWEntry `yaml:"allow,omitempty"`
	// Ports remplace les ports détectés, au format d'ufw (« 2222/tcp »,
	// « 40000:40100/tcp », « 80,443/tcp »).
	Ports []string `yaml:"ports,omitempty"`
	// Passive est la plage de ports passifs du FTP (« 40000:40100 »), lorsqu'elle
	// n'a pas pu être lue dans la configuration du serveur.
	Passive string `yaml:"passive,omitempty"`
	// Available force la disponibilité, ou l'interdit. Nil : ogs en décide
	// d'après ce qui est installé.
	Available *bool `yaml:"available,omitempty"`
	// Title nomme un profil propre dans « ufw app list ».
	Title string `yaml:"title,omitempty"`
	// Units rend un profil propre disponible dès que l'une de ces unités existe.
	Units []string `yaml:"units,omitempty"`
}

// UFWOutgoing énumère les sorties permises.
type UFWOutgoing struct {
	// Allow reçoit des noms du catalogue (dns, http, https, ntp, smtp,
	// submission, smtps, ssh) ou des ports au format d'ufw (« 5432/tcp »). Nil :
	// le catalogue entier.
	Allow []string `yaml:"allow,omitempty"`
}

// UFWEntry désigne des adresses : une source nommée, une adresse littérale ou
// « any ». Elle s'écrit en chaîne (« bureau », « 192.0.2.10 ») ou en table,
// pour lui donner un commentaire.
type UFWEntry struct {
	Source  string `yaml:"source,omitempty"`
	Address string `yaml:"address,omitempty"`
	// Comment remplace le commentaire engendré. Vide, ogs en compose un à partir
	// du profil, de la source et de son propriétaire.
	Comment string `yaml:"comment,omitempty"`
}

// UnmarshalYAML accepte la forme courte : une chaîne est une adresse si elle
// en a la syntaxe, « any » ou le nom d'une source sinon.
func (e *UFWEntry) UnmarshalYAML(data []byte) error {
	var s string
	if err := yaml.Unmarshal(data, &s); err == nil {
		*e = ParseUFWEntry(s)
		return nil
	}
	type sansMéthode UFWEntry
	var brut sansMéthode
	if err := yaml.Unmarshal(data, &brut); err != nil {
		return err
	}
	*e = UFWEntry(brut)
	return nil
}

// ParseUFWEntry classe une chaîne : « any », une adresse, ou une source.
func ParseUFWEntry(s string) UFWEntry {
	s = strings.TrimSpace(s)
	if s == UFWAny || strings.ContainsAny(s, ".:/") || (s != "" && s[0] >= '0' && s[0] <= '9') {
		return UFWEntry{Address: s}
	}
	return UFWEntry{Source: s}
}

// YAMLValue rend la forme la plus courte de l'entrée, pour l'écrire.
func (e UFWEntry) YAMLValue() any {
	if e.Comment == "" {
		if e.Source != "" {
			return e.Source
		}
		return e.Address
	}
	type sansMéthode UFWEntry
	return sansMéthode(e)
}

// Label nomme l'entrée dans les messages.
func (e UFWEntry) Label() string {
	if e.Source != "" {
		return e.Source
	}
	return e.Address
}

// UFWAny désigne toute adresse.
const UFWAny = "any"

// UFWAddress est une adresse résolue.
type UFWAddress struct {
	// Any vaut vrai pour « any » : Prefix est alors vide.
	Any    bool
	Prefix netip.Prefix
	// Source et Owner sont repris de la source nommée qui a fourni l'adresse.
	Source string
	Owner  string
	// Comment est celui de l'entrée, s'il y en a un.
	Comment string
}

// String rend l'adresse sous la forme qu'ufw emploie.
func (a UFWAddress) String() string {
	if a.Any {
		return UFWAny
	}
	return FormatUFWPrefix(a.Prefix)
}

// FormatUFWPrefix écrit une adresse sans masque d'hôte, comme ufw la range.
func FormatUFWPrefix(p netip.Prefix) string {
	if p.IsSingleIP() {
		return p.Addr().String()
	}
	return p.String()
}

// ParseUFWAddress lit une adresse ou un réseau. Un réseau dont les bits d'hôte
// ne sont pas nuls est refusé : ufw le ramènerait à son réseau en IPv4 et le
// garderait tel quel en IPv6, si bien que ce qui s'écrit n'est pas ce qui se
// relit — et le plan le reposerait à chaque apply.
func ParseUFWAddress(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "/") {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%q n'est ni une adresse IP ni un réseau", s)
		}
		addr = addr.Unmap()
		if addr.Zone() != "" {
			return netip.Prefix{}, fmt.Errorf("%q : une zone IPv6 n'a pas de sens dans une règle", s)
		}
		return netip.PrefixFrom(addr, addr.BitLen()), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q n'est pas un réseau valide", s)
	}
	p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits())
	if p.Bits() == 0 {
		return netip.Prefix{}, fmt.Errorf("%q couvre toutes les adresses : écrire « any »", s)
	}
	if m := p.Masked(); m != p {
		return netip.Prefix{}, fmt.Errorf("%q porte des bits d'hôte : écrire %s", s, m)
	}
	return p, nil
}

// SanitizeUFWComment rend un commentaire acceptable par ufw, qui refuse
// l'apostrophe droite — et le français en est plein. La typographique la
// remplace : elle se lit de même, et ufw l'accepte.
func SanitizeUFWComment(s string) string {
	s = strings.ReplaceAll(s, "'", "’")
	return strings.Join(strings.Fields(s), " ")
}

// Valeurs admises.
var (
	ufwPolicies  = []string{"allow", "deny", "reject"}
	ufwLogLevels = []string{"off", "on", "low", "medium", "high", "full"}

	reUFWName  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	reUFWPorts = regexp.MustCompile(`^[0-9]{1,5}(:[0-9]{1,5})?(,[0-9]{1,5}(:[0-9]{1,5})?)*(/(tcp|udp))?$`)
	reUFWRange = regexp.MustCompile(`^([0-9]{1,5}):([0-9]{1,5})$`)
)

// Défauts du pare-feu.
const (
	DefaultUFWIncoming      = "deny"
	DefaultUFWOutgoing      = "allow"
	DefaultUFWRollbackAfter = 5 * time.Minute
	// ufwCommentMax borne un commentaire : la sortie de « ufw status » le
	// recopie en bout de ligne, et un roman n'y apprendrait rien de plus.
	ufwCommentMax = 160
)

// LoadUFW lit la déclaration du pare-feu. Le booléen dit si le fichier existe :
// absent, ogs ne gère pas ufw.
func LoadUFW(path string) (*UFW, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	u := &UFW{}
	if err := yaml.UnmarshalWithOptions(data, u, yaml.Strict()); err != nil {
		return nil, true, err
	}
	if err := u.Validate(); err != nil {
		return nil, true, err
	}
	return u, true, nil
}

// IsActive dit si ufw doit filtrer.
func (u *UFW) IsActive() bool { return u.Active == nil || *u.Active }

// IncomingPolicy, OutgoingPolicy et RoutedPolicy rendent les politiques, défaut appliqué. Routed
// vide signifie « non géré ».
func (u *UFW) IncomingPolicy() string { return firstNonEmptyStr(u.Policy.Incoming, DefaultUFWIncoming) }
func (u *UFW) OutgoingPolicy() string { return firstNonEmptyStr(u.Policy.Outgoing, DefaultUFWOutgoing) }
func (u *UFW) RoutedPolicy() string   { return u.Policy.Routed }

// Rollback rend le délai de retour automatique d'une première activation ;
// zéro l'interdit.
func (u *UFW) Rollback() time.Duration {
	if u.RollbackAfter == "" {
		return DefaultUFWRollbackAfter
	}
	d, _ := parseUFWDuration(u.RollbackAfter)
	return d
}

func parseUFWDuration(s string) (time.Duration, error) {
	if s == "0" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Resolve développe une entrée en adresses.
func (u *UFW) Resolve(e UFWEntry) ([]UFWAddress, error) {
	comment := SanitizeUFWComment(e.Comment)
	switch {
	case e.Source != "" && e.Address != "":
		return nil, fmt.Errorf("« %s » / « %s » : une entrée désigne une source ou une adresse, pas les deux",
			e.Source, e.Address)
	case e.Source != "":
		src, ok := u.Sources[e.Source]
		if !ok {
			return nil, fmt.Errorf("source « %s » inconnue (déclarées : %s)", e.Source, u.sourceNames())
		}
		out := make([]UFWAddress, 0, len(src.Addresses))
		for _, a := range src.Addresses {
			p, err := ParseUFWAddress(a)
			if err != nil {
				return nil, fmt.Errorf("source « %s » : %w", e.Source, err)
			}
			out = append(out, UFWAddress{Prefix: p, Source: e.Source,
				Owner: SanitizeUFWComment(src.Owner), Comment: comment})
		}
		return out, nil
	case e.Address == UFWAny:
		return []UFWAddress{{Any: true, Comment: comment}}, nil
	case e.Address != "":
		p, err := ParseUFWAddress(e.Address)
		if err != nil {
			return nil, err
		}
		// Une adresse littérale prend le propriétaire de la source qui la contient,
		// s'il y en a une : la règle dit alors à qui elle s'adresse sans qu'on ait
		// eu à le répéter.
		addr := UFWAddress{Prefix: p, Comment: comment}
		if name, src, ok := u.SourceOf(p); ok {
			addr.Source, addr.Owner = name, SanitizeUFWComment(src.Owner)
		}
		return []UFWAddress{addr}, nil
	}
	return nil, errors.New("entrée vide")
}

// SourceOf rend la première source (par nom) dont une adresse contient p.
func (u *UFW) SourceOf(p netip.Prefix) (string, UFWSource, bool) {
	for _, name := range u.SortedSources() {
		src := u.Sources[name]
		for _, a := range src.Addresses {
			sp, err := ParseUFWAddress(a)
			if err != nil {
				continue
			}
			if sp.Bits() <= p.Bits() && sp.Contains(p.Addr()) {
				return name, src, true
			}
		}
	}
	return "", UFWSource{}, false
}

// SortedSources énumère les sources par nom.
func (u *UFW) SortedSources() []string {
	names := make([]string, 0, len(u.Sources))
	for n := range u.Sources {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (u *UFW) sourceNames() string {
	if len(u.Sources) == 0 {
		return "aucune"
	}
	return strings.Join(u.SortedSources(), ", ")
}

// Validate contrôle la déclaration. Tout ce qui peut l'être est refusé au
// chargement : une règle fausse découverte à l'apply laisserait le pare-feu à
// mi-chemin, et un pare-feu à mi-chemin est celui qui coupe l'accès.
func (u *UFW) Validate() error {
	var errs []string
	seen := map[string]bool{}
	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if !seen[msg] {
			seen[msg] = true
			errs = append(errs, msg)
		}
	}

	for _, pol := range []struct{ name, value string }{
		{"policy.incoming", u.Policy.Incoming},
		{"policy.outgoing", u.Policy.Outgoing},
		{"policy.routed", u.Policy.Routed},
	} {
		if pol.value != "" && !contains(ufwPolicies, pol.value) {
			fail("%s %q inconnu (attendu : allow, deny ou reject)", pol.name, pol.value)
		}
	}
	if u.Logging != "" && !contains(ufwLogLevels, u.Logging) {
		fail("logging %q inconnu (attendu : %s)", u.Logging, strings.Join(ufwLogLevels, ", "))
	}
	if u.RollbackAfter != "" {
		d, err := parseUFWDuration(u.RollbackAfter)
		switch {
		case err != nil || d < 0:
			fail("rollbackAfter %q invalide : durée attendue, par exemple 5m, ou 0 pour s'en passer", u.RollbackAfter)
		case d > 0 && d < 30*time.Second:
			fail("rollbackAfter %s est trop court pour confirmer depuis une nouvelle session (30s au moins)", d)
		}
	}

	for _, name := range u.SortedSources() {
		src := u.Sources[name]
		if !reUFWName.MatchString(name) {
			fail("sources.%s : nom invalide (minuscules, chiffres et tirets, une lettre en tête)", name)
		}
		if len(src.Addresses) == 0 {
			fail("sources.%s : aucune adresse", name)
		}
		for _, a := range src.Addresses {
			if strings.TrimSpace(a) == UFWAny {
				fail("sources.%s : « any » n'est pas une adresse de source", name)
				continue
			}
			if _, err := ParseUFWAddress(a); err != nil {
				fail("sources.%s : %v", name, err)
			}
		}
		if err := checkComment(src.Owner); err != nil {
			fail("sources.%s.owner : %v", name, err)
		}
	}

	resolve := func(where string, entries []UFWEntry, anyAllowed bool) []UFWAddress {
		var out []UFWAddress
		for i, e := range entries {
			if err := checkComment(e.Comment); err != nil {
				fail("%s[%d].comment : %v", where, i, err)
			}
			if e.Address == UFWAny && !anyAllowed {
				fail("%s[%d] : « any » n'a pas de sens ici", where, i)
				continue
			}
			addrs, err := u.Resolve(e)
			if err != nil {
				fail("%s[%d] : %v", where, i, err)
				continue
			}
			out = append(out, addrs...)
		}
		return out
	}

	allowed := resolve("trusted", u.Trusted, false)
	for _, name := range u.SortedProfiles() {
		prof := u.Profiles[name]
		where := "profiles." + name
		if !reUFWName.MatchString(name) {
			fail("%s : nom invalide (minuscules, chiffres et tirets, une lettre en tête)", where)
		}
		allowed = append(allowed, resolve(where+".allow", prof.Allow, true)...)
		for _, p := range prof.Ports {
			if err := checkUFWPorts(p); err != nil {
				fail("%s.ports : %v", where, err)
			}
		}
		if prof.Passive != "" {
			if name != "ftp" {
				fail("%s.passive : seul le profil ftp a des ports passifs", where)
			} else if err := checkPassive(prof.Passive); err != nil {
				fail("%s.passive : %v", where, err)
			}
		}
		if !IsUFWCatalogProfile(name) && len(prof.Ports) == 0 {
			fail("%s : profil hors catalogue sans « ports » — ogs ne peut pas deviner ce qu'il ouvre "+
				"(catalogue : %s)", where, strings.Join(UFWCatalogProfiles, ", "))
		}
		if IsUFWCatalogProfile(name) && len(prof.Units) > 0 {
			fail("%s.units : réservé aux profils hors catalogue, dont ogs ne sait pas détecter le service", where)
		}
		if err := checkComment(prof.Title); err != nil {
			fail("%s.title : %v", where, err)
		}
	}

	banned := resolve("ban", u.Ban, false)
	// Une adresse à la fois bannie et autorisée est une contradiction que l'ordre
	// des règles trancherait en silence — le bannissement passe en tête, et
	// l'autorisation ne servirait jamais. Mieux vaut la refuser que la deviner.
	for _, b := range banned {
		for _, a := range allowed {
			if a.Any {
				continue
			}
			if b.Prefix.Overlaps(a.Prefix) {
				fail("ban : %s recouvre %s, autorisée par ailleurs%s — retirer l'une des deux",
					b, a, sourceSuffix(a))
			}
		}
	}

	if u.OutgoingPolicy() != "allow" {
		names := u.OutgoingAllow()
		for _, o := range names {
			if _, ok := UFWOutgoingCatalog[o]; ok {
				continue
			}
			if err := checkUFWPorts(o); err != nil || !strings.Contains(o, "/") {
				fail("outgoing.allow : %q n'est ni une sortie du catalogue (%s) ni un port de la forme 5432/tcp",
					o, strings.Join(sortedKeys(UFWOutgoingCatalog), ", "))
			}
		}
		for _, required := range []string{"dns", "https"} {
			if !contains(names, required) {
				fail("outgoing.allow doit comprendre « %s » sous une politique sortante %q : "+
					"sans elle, l'émission des certificats, les déploiements et les mises à jour échouent",
					required, u.OutgoingPolicy())
			}
		}
	} else if u.Outgoing.Allow != nil {
		fail("outgoing.allow n'a d'effet que sous une politique sortante deny ou reject")
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n"))
	}
	return nil
}

// OutgoingAllow rend les sorties permises, catalogue entier par défaut.
func (u *UFW) OutgoingAllow() []string {
	if u.Outgoing.Allow != nil {
		return u.Outgoing.Allow
	}
	return sortedKeys(UFWOutgoingCatalog)
}

// SortedProfiles énumère les profils déclarés par nom.
func (u *UFW) SortedProfiles() []string {
	names := make([]string, 0, len(u.Profiles))
	for n := range u.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// UFWCatalogProfiles énumère les profils qu'ogs sait détecter.
var UFWCatalogProfiles = []string{
	"ssh", "sftp", "ftp", "web", "mysql", "postgresql", "redis",
	"rabbitmq", "rabbitmq-management", "smtp", "submission",
}

// IsUFWCatalogProfile dit si ogs sait détecter ce profil.
func IsUFWCatalogProfile(name string) bool { return contains(UFWCatalogProfiles, name) }

// UFWOutgoingCatalog associe une sortie nommée à ses ports.
var UFWOutgoingCatalog = map[string]string{
	"dns":        "53",
	"http":       "80/tcp",
	"https":      "443/tcp",
	"ntp":        "123/udp",
	"smtp":       "25/tcp",
	"submission": "587/tcp",
	"smtps":      "465/tcp",
	"ssh":        "22/tcp",
}

func sourceSuffix(a UFWAddress) string {
	if a.Source != "" {
		return " (source " + a.Source + ")"
	}
	return ""
}

func checkComment(s string) error {
	if strings.ContainsAny(s, "\n\r") {
		return errors.New("tient sur une ligne")
	}
	if n := len([]rune(s)); n > ufwCommentMax {
		return fmt.Errorf("%d caractères, %d au plus", n, ufwCommentMax)
	}
	return nil
}

// checkUFWPorts contrôle un jeu de ports au format d'ufw. Une liste ou une
// plage exige son protocole : ufw refuse le profil entier sans, et l'écarte
// d'un simple avertissement que personne ne lit.
func checkUFWPorts(s string) error {
	if !reUFWPorts.MatchString(s) {
		return fmt.Errorf("%q : format attendu 22/tcp, 80,443/tcp ou 40000:40100/tcp", s)
	}
	spec, proto, _ := strings.Cut(s, "/")
	if proto == "" && strings.ContainsAny(spec, ",:") {
		return fmt.Errorf("%q : une liste ou une plage de ports exige son protocole (/tcp ou /udp)", s)
	}
	for _, part := range strings.Split(spec, ",") {
		lo, hi, isRange := strings.Cut(part, ":")
		a, _ := strconv.Atoi(lo)
		b := a
		if isRange {
			b, _ = strconv.Atoi(hi)
		}
		if a < 1 || b > 65535 || b < a {
			return fmt.Errorf("%q : port hors de 1–65535 ou plage inversée", s)
		}
	}
	return nil
}

func checkPassive(s string) error {
	m := reUFWRange.FindStringSubmatch(s)
	if m == nil {
		return fmt.Errorf("%q : plage attendue de la forme 40000:40100", s)
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	if a < 1024 || b > 65535 || b <= a {
		return fmt.Errorf("%q : plage attendue entre 1024 et 65535, bornes croissantes", s)
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
