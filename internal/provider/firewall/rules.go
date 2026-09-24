package firewall

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"net/netip"
	"os"
	"strings"

	"leblanc.io/open-go-server/internal/config"
)

// Marker ouvre le commentaire de toute règle posée par ogs. C'est lui, et lui
// seul, qui désigne une règle comme sienne : une règle sans marque appartient à
// l'administrateur, et ogs ne la touche jamais.
const Marker = "ogs:"

// Rule est une règle ufw telle qu'ogs la compose ou la reconnaît. Trois formes
// seulement : l'accès à un profil d'application depuis une adresse, le refus
// d'une adresse, la sortie vers un port.
type Rule struct {
	// Action vaut allow ou deny.
	Action string
	// Direction vaut in ou out.
	Direction string
	// App est le profil visé (« ogs-mysql ») ; vide pour un refus ou une sortie.
	App string
	// Port et Proto portent une sortie (« 53 », « any ») ; Proto vaut « any »
	// pour les autres formes.
	Port  string
	Proto string
	// From est l'adresse d'origine, « any » ou une adresse normalisée.
	From    string
	Comment string

	// Profile est le nom du profil dans ufw.yaml, pour l'affichage.
	Profile string
	// Kind nomme la famille de règle : accès, confiance, bannissement, sortie.
	Kind string
}

// Familles de règles.
const (
	KindAllow   = "accès"
	KindTrusted = "confiance"
	KindBan     = "bannissement"
	KindOut     = "sortie"
)

// Key identifie une règle, commentaire exclu : deux règles de même clé sont la
// même pour ufw, qui remplace le commentaire en place plutôt que d'en ajouter
// une seconde.
func (r Rule) Key() string {
	return strings.Join([]string{r.Action, r.Direction, r.App, r.Port, r.Proto, r.From}, "|")
}

// Families rend les familles d'adresses où la règle doit exister : « any »
// s'écrit dans les deux lorsque l'IPv6 est actif.
func (r Rule) Families(ipv6 bool) []int {
	if r.From == config.UFWAny {
		if ipv6 {
			return []int{4, 6}
		}
		return []int{4}
	}
	if p, err := config.ParseUFWAddress(r.From); err == nil && p.Addr().Is6() {
		return []int{6}
	}
	return []int{4}
}

// Describe rend la règle lisible dans un plan.
func (r Rule) Describe() string {
	from := r.From
	if from == config.UFWAny {
		from = "toute adresse"
	}
	switch {
	case r.App != "":
		return r.Profile + " depuis " + from
	case r.Action == "deny":
		return "refus de " + from
	default:
		target := r.Port
		if r.Proto != "any" {
			target += "/" + r.Proto
		}
		return "sortie vers " + target
	}
}

// addArgs rend les arguments d'ufw qui posent la règle. Un refus nouveau est
// placé en tête : ufw retient la première règle qui correspond, et un refus
// posé après une autorisation plus large ne servirait jamais.
func (r Rule) addArgs(prepend bool) []string {
	var args []string
	switch {
	case r.App != "":
		args = []string{r.Action, "from", r.From, "to", "any", "app", r.App}
	case r.Direction == "in":
		args = []string{r.Action, "from", r.From}
		if prepend {
			args = append([]string{"prepend"}, args...)
		}
	default:
		args = []string{r.Action, "out", r.portSpec()}
	}
	if r.Comment != "" {
		args = append(args, "comment", r.Comment)
	}
	return args
}

// deleteArgs rend les arguments qui la retirent. Le commentaire n'y figure
// pas : ufw retire une règle commentée sur sa seule définition.
func (r Rule) deleteArgs() []string {
	switch {
	case r.App != "":
		return []string{"delete", r.Action, "from", r.From, "to", "any", "app", r.App}
	case r.Direction == "in":
		return []string{"delete", r.Action, "from", r.From}
	default:
		return []string{"delete", r.Action, "out", r.portSpec()}
	}
}

func (r Rule) portSpec() string {
	if r.Proto == "" || r.Proto == "any" {
		return r.Port
	}
	return r.Port + "/" + r.Proto
}

// ObservedRule est une règle lue dans user.rules ou user6.rules.
type ObservedRule struct {
	Rule
	// Family vaut 4 ou 6 ; Index est la position dans son fichier.
	Family int
	Index  int
	// Managed dit si la règle porte la marque d'ogs.
	Managed bool
	// Recognized dit si la règle a l'une des trois formes qu'ogs compose. Une
	// règle marquée mais méconnaissable — retouchée à la main, une interface
	// ajoutée — n'est ni reprise ni supprimée : ogs ne saurait pas la retirer
	// sans risquer d'en retirer une autre.
	Recognized bool
	// Verb est l'action telle qu'ufw l'a rangée (« allow », « limit »,
	// « allow_log »…), reconnue ou non : c'est elle qui dit si une règle
	// précédant un refus le rendrait inopérant.
	Verb string
	// Raw est le tuple d'origine, pour l'affichage.
	Raw string
}

// readRules relit un jeu de règles ufw ; un fichier absent n'en porte aucune.
func readRules(path string, family int) ([]ObservedRule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return parseRules(data, family), nil
}

// parseRules analyse les lignes « ### tuple ### » qu'ufw écrit au-dessus de
// chaque règle. C'est sa propre représentation, celle qu'il relit : la sortie
// de « ufw status » est faite pour l'œil et change de forme d'une version à
// l'autre.
func parseRules(data []byte, family int) []ObservedRule {
	var out []ObservedRule
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "### tuple ###") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "### tuple ###"))
		tuple, comment := raw, ""
		// ufw range le commentaire en dernier, encodé en hexadécimal.
		if i := strings.Index(raw, " comment="); i >= 0 {
			tuple = raw[:i]
			if dec, err := hex.DecodeString(strings.TrimSpace(raw[i+len(" comment="):])); err == nil {
				comment = string(dec)
			}
		}
		obs := ObservedRule{Family: family, Index: len(out), Raw: raw,
			Managed: strings.HasPrefix(comment, Marker)}
		obs.Comment = comment
		fields := strings.Fields(tuple)
		if len(fields) > 0 {
			obs.Verb = fields[0]
		}
		obs.Recognized = recognize(fields, &obs.Rule)
		out = append(out, obs)
	}
	return out
}

// recognize remplit la règle si le tuple a l'une des formes qu'ogs compose :
// sans journalisation, sans interface, sans routage, vers toute destination
// depuis tout port.
func recognize(f []string, r *Rule) bool {
	if len(f) != 7 && len(f) != 9 {
		return false
	}
	action, proto, dport, dst, sport, src := f[0], f[1], f[2], f[3], f[4], f[5]
	dir := f[len(f)-1]
	if action != "allow" && action != "deny" {
		return false
	}
	if dir != "in" && dir != "out" {
		return false
	}
	if !isAnyAddress(dst) || sport != "any" {
		return false
	}
	from, ok := normalizeObserved(src)
	if !ok {
		return false
	}
	r.Action, r.Direction, r.From = action, dir, from
	if len(f) == 9 {
		dapp, sapp := f[6], f[7]
		if sapp != "-" {
			return false
		}
		if dapp != "-" {
			r.App = strings.ReplaceAll(dapp, "%20", " ")
			r.Proto = "any"
			return dir == "in"
		}
	}
	switch {
	case dir == "in" && dport == "any" && proto == "any":
		r.Proto = "any"
		return true
	case dir == "out" && from == config.UFWAny && dport != "any":
		r.Port, r.Proto = dport, proto
		return true
	}
	return false
}

func isAnyAddress(s string) bool { return s == "0.0.0.0/0" || s == "::/0" }

// normalizeObserved ramène une adresse relue à la forme qu'ogs compose, pour
// que la comparaison porte sur l'adresse et non sur son écriture.
func normalizeObserved(s string) (string, bool) {
	if isAnyAddress(s) {
		return config.UFWAny, true
	}
	var p netip.Prefix
	var err error
	if strings.Contains(s, "/") {
		p, err = netip.ParsePrefix(s)
	} else {
		var a netip.Addr
		a, err = netip.ParseAddr(s)
		if err == nil {
			p = netip.PrefixFrom(a, a.BitLen())
		}
	}
	if err != nil {
		return "", false
	}
	return config.FormatUFWPrefix(p), true
}

// desiredRules compose les règles voulues. L'ordre est celui de l'affichage :
// les refus d'abord, puis les accès profil par profil, puis les sorties. Deux
// entrées qui aboutissent à la même règle n'en produisent qu'une — la première,
// si bien qu'une autorisation explicite garde son commentaire devant la
// confiance.
func desiredRules(cfg *config.UFW, profiles []Profile) ([]Rule, error) {
	var out []Rule
	seen := map[string]bool{}
	add := func(r Rule) {
		r.Comment = config.SanitizeUFWComment(r.Comment)
		if !seen[r.Key()] {
			seen[r.Key()] = true
			out = append(out, r)
		}
	}

	for _, e := range cfg.Ban {
		addrs, err := cfg.Resolve(e)
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			add(Rule{Action: "deny", Direction: "in", Proto: "any", From: a.String(),
				Kind: KindBan, Comment: banComment(a)})
		}
	}

	var trusted []config.UFWAddress
	for _, e := range cfg.Trusted {
		addrs, err := cfg.Resolve(e)
		if err != nil {
			return nil, err
		}
		trusted = append(trusted, addrs...)
	}
	for _, prof := range profiles {
		if !prof.Available {
			continue
		}
		for _, e := range cfg.Profiles[prof.Name].Allow {
			addrs, err := cfg.Resolve(e)
			if err != nil {
				return nil, err
			}
			for _, a := range addrs {
				add(Rule{Action: "allow", Direction: "in", App: prof.UFWName, Proto: "any",
					From: a.String(), Profile: prof.Name, Kind: KindAllow,
					Comment: allowComment(prof.Name, a, false)})
			}
		}
		for _, a := range trusted {
			add(Rule{Action: "allow", Direction: "in", App: prof.UFWName, Proto: "any",
				From: a.String(), Profile: prof.Name, Kind: KindTrusted,
				Comment: allowComment(prof.Name, a, true)})
		}
	}

	if cfg.OutgoingPolicy() != "allow" {
		for _, name := range cfg.OutgoingAllow() {
			spec, ok := config.UFWOutgoingCatalog[name]
			if !ok {
				spec = name
			}
			port, proto, found := strings.Cut(spec, "/")
			if !found {
				proto = "any"
			}
			add(Rule{Action: "allow", Direction: "out", Port: port, Proto: proto,
				From: config.UFWAny, Kind: KindOut, Comment: Marker + " sortie " + name})
		}
	}
	return out, nil
}

// allowComment compose le commentaire d'un accès. Un commentaire déclaré
// l'emporte ; sinon la règle dit ce qu'« ufw status » ne montre pas — le profil
// et l'adresse y sont déjà : à qui appartient l'adresse, et à quel titre elle
// est là. C'est ce qu'on cherche devant une règle des mois plus tard.
func allowComment(profile string, a config.UFWAddress, trusted bool) string {
	if a.Comment != "" {
		return Marker + " " + a.Comment
	}
	switch {
	case trusted && a.Source != "":
		return Marker + " confiance — " + ownerLabel(a)
	case trusted:
		return Marker + " confiance"
	case a.Any:
		return Marker + " accès public à " + profile
	case a.Source != "":
		return Marker + " " + ownerLabel(a)
	}
	return Marker + " accès à " + profile
}

// banComment compose le commentaire d'un refus.
func banComment(a config.UFWAddress) string {
	switch {
	case a.Comment != "":
		return Marker + " bannie — " + a.Comment
	case a.Source != "":
		return Marker + " bannie — " + ownerLabel(a)
	}
	return Marker + " bannie"
}

func ownerLabel(a config.UFWAddress) string {
	if a.Owner == "" {
		return a.Source
	}
	return a.Source + " (" + a.Owner + ")"
}

// commandLine rend une commande ufw lisible, arguments cités au besoin.
func commandLine(args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, "ufw")
	for _, a := range args {
		if a == "" || strings.ContainsAny(a, " \t'\"") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}
