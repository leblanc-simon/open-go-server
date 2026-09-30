package tls

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/challenge"
	"github.com/goccy/go-yaml"
)

// Jeux d'identifiants DNS (D97).
//
// Un jeu est un fichier de /etc/ogs/credentials/dns : le provider qu'il sert, les
// zones dont il répond, et les clés de son API. Plusieurs jeux d'un même provider
// coexistent — un compte OVH par client, ou un jeton restreint à chaque zone, ce
// qu'OVH permet (/domain/zone/<zone>/*) et qui vaut mieux qu'un jeton unique
// capable d'écrire dans toutes les zones du compte. Le nom du fichier est celui du
// jeu : l'ancien ovh.yaml, sans zone, reste le jeu par défaut du provider.
//
// Une zone déclarée est un engagement : c'est elle qui fait choisir le jeu d'un
// nom, et elle seule qui fait passer un certificat au challenge DNS-01 sans qu'il
// le demande. Un jeu sans zone ne sert qu'une demande explicite — sans quoi
// l'ovh.yaml du wildcard de préproduction ferait tenter DNS-01 à chaque site du
// parc, sur des zones que son compte ne porte pas.

// DNSCredentials est un jeu d'identifiants DNS.
type DNSCredentials struct {
	// Name est le nom du jeu, celui de son fichier sans « .yaml ».
	Name string
	// Provider est le provider DNS qu'il sert (« ovh »).
	Provider string
	// Zones porte les zones dont il répond, en minuscules, sans point final.
	Zones []string
	// Values porte les clés de l'API. Jamais affichées.
	Values map[string]string
}

// dnsReservedKeys sont les clés du fichier qui ne sont pas des identifiants.
var dnsReservedKeys = []string{"provider", "zones"}

// CheckDNSCredentialsName contrôle le nom d'un jeu : c'est un nom de fichier sous
// /etc/ogs/credentials/dns, qu'une déclaration de projet peut désigner.
func CheckDNSCredentialsName(name string) error {
	if name == "" || len(name) > 64 {
		return fmt.Errorf("nom de jeu d'identifiants DNS %q invalide : 1 à 64 caractères", name)
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_' || r == '.') && i > 0:
		default:
			return fmt.Errorf("nom de jeu d'identifiants DNS %q invalide : minuscules, chiffres, "+
				"« - », « _ » et « . », hors première position", name)
		}
	}
	return nil
}

// NormalizeZone ramène une zone à sa forme comparée : minuscules, sans point final.
func NormalizeZone(zone string) (string, error) {
	z := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(zone)), ".")
	if strings.Contains(z, "*") {
		return "", fmt.Errorf("zone %q : une zone ne porte pas de joker", zone)
	}
	if err := validateDomain(z); err != nil {
		return "", fmt.Errorf("zone %q invalide : %w", zone, err)
	}
	return z, nil
}

// dnsName ramène un nom de certificat au nom dont le challenge prouve la
// maîtrise : lego présente celui d'un joker sans son « *. ».
func dnsName(domain string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(domain), "*."), ".")
}

// ZoneFor rend la plus longue des zones du jeu qui contient le nom.
func (c DNSCredentials) ZoneFor(domain string) (string, bool) {
	name := dnsName(domain)
	best := ""
	for _, z := range c.Zones {
		if (name == z || strings.HasSuffix(name, "."+z)) && len(z) > len(best) {
			best = z
		}
	}
	return best, best != ""
}

// DNSCredentialsFile rend le chemin du fichier d'un jeu.
func DNSCredentialsFile(dir, name string) string {
	return filepath.Join(dir, name+".yaml")
}

// ReadDNSCredentials lit un jeu. Il vit hors de la base d'état, dans un fichier
// restreint à root : il donne un droit d'écriture sur des zones entières.
func ReadDNSCredentials(dir, name string) (DNSCredentials, error) {
	if err := CheckDNSCredentialsName(name); err != nil {
		return DNSCredentials{}, err
	}
	path := DNSCredentialsFile(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DNSCredentials{}, fmt.Errorf("jeu d'identifiants DNS %s absent : l'enregistrer avec "+
				"« ogs cert dns-provider set %s » (%s)", name, name, path)
		}
		return DNSCredentials{}, err
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
		return DNSCredentials{}, fmt.Errorf("%s est lisible au-delà de son propriétaire (mode %04o) : "+
			"corriger avec chmod 0600", path, info.Mode().Perm())
	}
	raw := map[string]any{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return DNSCredentials{}, fmt.Errorf("%s : %w", path, err)
	}
	creds := DNSCredentials{Name: name, Provider: DNSProviderOVH, Values: map[string]string{}}
	for key, value := range raw {
		switch key {
		case "provider":
			s, ok := value.(string)
			if !ok {
				return DNSCredentials{}, fmt.Errorf("%s : « provider » doit être une chaîne", path)
			}
			creds.Provider = strings.ToLower(strings.TrimSpace(s))
		case "zones":
			list, ok := value.([]any)
			if !ok && value != nil {
				return DNSCredentials{}, fmt.Errorf("%s : « zones » doit être une liste", path)
			}
			for _, item := range list {
				s, ok := item.(string)
				if !ok {
					return DNSCredentials{}, fmt.Errorf("%s : zone %v invalide", path, item)
				}
				z, err := NormalizeZone(s)
				if err != nil {
					return DNSCredentials{}, fmt.Errorf("%s : %w", path, err)
				}
				if !slices.Contains(creds.Zones, z) {
					creds.Zones = append(creds.Zones, z)
				}
			}
		default:
			switch v := value.(type) {
			case string:
				creds.Values[key] = v
			case nil:
			case map[string]any, []any:
				return DNSCredentials{}, fmt.Errorf("%s : « %s » doit être une valeur simple", path, key)
			default:
				creds.Values[key] = fmt.Sprint(v)
			}
		}
	}
	if creds.Provider != DNSProviderOVH {
		return DNSCredentials{}, fmt.Errorf("%s : provider DNS %q non géré : ogs ne gère qu'%q",
			path, creds.Provider, DNSProviderOVH)
	}
	sort.Strings(creds.Zones)
	return creds, nil
}

// LoadDNSCredentials lit tous les jeux du répertoire, par ordre de nom. Un
// fichier illisible est écarté et nommé, sans priver les autres : il ne doit pas
// empêcher le renouvellement des certificats qu'il ne sert pas.
func LoadDNSCredentials(dir string) ([]DNSCredentials, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{err}
	}
	var sets []DNSCredentials
	var errs []error
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok || e.IsDir() {
			continue
		}
		creds, err := ReadDNSCredentials(dir, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		sets = append(sets, creds)
	}
	return sets, errs
}

// DNSZoneConflicts nomme les zones que deux jeux revendiquent : le choix du jeu
// d'un nom n'y serait plus déterminé.
func DNSZoneConflicts(sets []DNSCredentials) []string {
	owners := map[string][]string{}
	for _, s := range sets {
		for _, z := range s.Zones {
			owners[z] = append(owners[z], s.Name)
		}
	}
	var out []string
	for z, names := range owners {
		if len(names) > 1 {
			out = append(out, fmt.Sprintf("zone %s revendiquée par %s", z, strings.Join(names, " et ")))
		}
	}
	sort.Strings(out)
	return out
}

// byZone rend le jeu dont une zone contient le nom, la plus longue l'emportant.
// Deux jeux de même zone rendent une erreur : aucun des deux n'est plus légitime.
func byZone(sets []DNSCredentials, domain string) (DNSCredentials, bool, error) {
	var best DNSCredentials
	var bestZone string
	var rivals []string
	for _, s := range sets {
		z, ok := s.ZoneFor(domain)
		switch {
		case !ok:
		case len(z) > len(bestZone):
			best, bestZone, rivals = s, z, []string{s.Name}
		case len(z) == len(bestZone):
			rivals = append(rivals, s.Name)
		}
	}
	if len(rivals) > 1 {
		return DNSCredentials{}, false, fmt.Errorf("zone %s revendiquée par plusieurs jeux d'identifiants DNS (%s) : "+
			"n'en garder qu'un, ou désigner celui du certificat par « dnsCredentials »",
			bestZone, strings.Join(rivals, ", "))
	}
	return best, bestZone != "", nil
}

// ResolveDNSCredentials choisit le jeu qui prouve la maîtrise d'un nom : celui que
// la demande désigne, sinon celui dont une zone contient le nom, sinon le jeu sans
// zone du provider, s'il est seul.
func ResolveDNSCredentials(sets []DNSCredentials, domain, explicit, provider string) (DNSCredentials, error) {
	name := dnsName(domain)
	if explicit != "" {
		for _, s := range sets {
			if s.Name != explicit {
				continue
			}
			if provider != "" && s.Provider != provider {
				return DNSCredentials{}, fmt.Errorf("le jeu d'identifiants DNS %s sert %s, non %s",
					s.Name, s.Provider, provider)
			}
			// Des zones déclarées bornent le jeu : un nom hors d'elles relève d'un autre
			// compte, et l'API le refuserait au moment du challenge.
			if _, ok := s.ZoneFor(name); len(s.Zones) > 0 && !ok {
				return DNSCredentials{}, fmt.Errorf("le jeu d'identifiants DNS %s ne couvre pas %s (zones : %s)",
					s.Name, name, strings.Join(s.Zones, ", "))
			}
			return s, nil
		}
		return DNSCredentials{}, fmt.Errorf("jeu d'identifiants DNS %s absent : l'enregistrer avec "+
			"« ogs cert dns-provider set %s »", explicit, explicit)
	}
	if s, ok, err := byZone(sets, name); err != nil || ok {
		return s, err
	}
	var defaults []string
	var found DNSCredentials
	for _, s := range sets {
		if len(s.Zones) == 0 && (provider == "" || s.Provider == provider) {
			defaults = append(defaults, s.Name)
			found = s
		}
	}
	switch len(defaults) {
	case 1:
		return found, nil
	case 0:
		return DNSCredentials{}, fmt.Errorf("aucun jeu d'identifiants DNS ne couvre %s : déclarer sa zone "+
			"(« ogs cert dns-provider set <nom> --zone <zone> »)", name)
	}
	return DNSCredentials{}, fmt.Errorf("plusieurs jeux d'identifiants DNS sans zone (%s) pour %s : "+
		"déclarer leurs zones, ou désigner celui du certificat par « dnsCredentials »",
		strings.Join(defaults, ", "), name)
}

// AutoDNS dit si une demande peut passer d'elle-même au challenge DNS-01 : chacun
// de ses noms relève d'une zone déclarée, sans ambiguïté. Un seul nom hors zone
// la laisse en HTTP-01 — une commande ACME ne choisit pas son challenge nom à
// nom — et un jeu sans zone n'y suffit jamais.
func AutoDNS(sets []DNSCredentials, domains []string) (provider string, ok bool) {
	if len(domains) == 0 {
		return "", false
	}
	for _, d := range domains {
		s, found, err := byZone(sets, d)
		if err != nil || !found {
			return "", false
		}
		if provider != "" && provider != s.Provider {
			return "", false
		}
		provider = s.Provider
	}
	return provider, true
}

// routedDNS aiguille le challenge de chaque nom vers le provider de son jeu :
// une même commande ACME peut porter des noms de plusieurs zones, donc de
// plusieurs comptes.
type routedDNS struct {
	byName            map[string]challenge.Provider
	timeout, interval time.Duration
}

func (r *routedDNS) provider(domain string) (challenge.Provider, error) {
	if p, ok := r.byName[dnsName(domain)]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("aucun jeu d'identifiants DNS retenu pour %s", domain)
}

func (r *routedDNS) Present(domain, token, keyAuth string) error {
	p, err := r.provider(domain)
	if err != nil {
		return err
	}
	return p.Present(domain, token, keyAuth)
}

func (r *routedDNS) CleanUp(domain, token, keyAuth string) error {
	p, err := r.provider(domain)
	if err != nil {
		return err
	}
	return p.CleanUp(domain, token, keyAuth)
}

// Timeout retient l'attente la plus longue des providers aiguillés : la
// propagation de la zone la plus lente décide.
func (r *routedDNS) Timeout() (time.Duration, time.Duration) { return r.timeout, r.interval }

// routeDNS résout le jeu de chaque nom de la demande et construit le provider de
// chaque jeu une seule fois. Un seul jeu rend son provider tel quel.
func routeDNS(sets []DNSCredentials, req Request,
	build func(DNSCredentials) (challenge.Provider, error)) (challenge.Provider, error) {
	built := map[string]challenge.Provider{}
	r := &routedDNS{byName: map[string]challenge.Provider{}, timeout: 60 * time.Second, interval: 2 * time.Second}
	for _, d := range req.Domains {
		s, err := ResolveDNSCredentials(sets, d, req.DNSCredentials, req.DNSProvider)
		if err != nil {
			return nil, err
		}
		p, ok := built[s.Name]
		if !ok {
			if p, err = build(s); err != nil {
				return nil, fmt.Errorf("jeu d'identifiants DNS %s : %w", s.Name, err)
			}
			built[s.Name] = p
			if t, ok := p.(challenge.ProviderTimeout); ok {
				timeout, interval := t.Timeout()
				r.timeout, r.interval = max(r.timeout, timeout), max(r.interval, interval)
			}
		}
		r.byName[dnsName(d)] = p
	}
	if len(built) == 1 {
		for _, p := range built {
			return p, nil
		}
	}
	return r, nil
}
