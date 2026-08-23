// Package naming implémente les règles de nommage du cahier des charges (D23, D24,
// D31, D36) : dérivation de l'identifiant unique depuis le host, troncature
// déterministe, suffixe d'environnement, numéros de pool PHP et de port applicatif.
package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	// MaxIdentifier est la contrainte la plus stricte : nom d'utilisateur Linux
	// et nom d'utilisateur MySQL 8 sont tous deux limités à 32 caractères.
	MaxIdentifier = 32
	// SuffixPrep est collé sans séparateur pour ne pas introduire d'underscore
	// supplémentaire dans les noms de bases (cf. §3.3.5 : joker dans les GRANT).
	SuffixPrep = "prep"
	// MaxBase réserve la place du suffixe d'environnement, afin que production et
	// préproduction partagent le même identifiant de base.
	MaxBase = MaxIdentifier - len(SuffixPrep)
	// truncKeep + 1 (séparateur) + hashLen == MaxBase
	truncKeep = 21
	hashLen   = 6
	// digitPrefix préfixe un identifiant commençant par un chiffre : useradd
	// refuse par défaut un nom qui ne débute pas par une lettre.
	digitPrefix = "s"
)

// Env est l'environnement d'un projet.
type Env string

const (
	EnvProd Env = "prod"
	EnvPrep Env = "prep"
)

// ParseEnv valide une valeur d'environnement.
func ParseEnv(v string) (Env, error) {
	switch Env(strings.ToLower(strings.TrimSpace(v))) {
	case "", EnvProd:
		return EnvProd, nil
	case EnvPrep:
		return EnvPrep, nil
	default:
		return "", fmt.Errorf("environnement inconnu %q (attendu : prod ou prep)", v)
	}
}

var (
	reIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	reHostLabel  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	reNotKept    = regexp.MustCompile(`[^a-z0-9]`)
)

// NormalizeHost met un host en forme canonique : minuscules, sans espaces, sans
// point final, sans préfixe « www. » (www.shop.fr et shop.fr désignent le même site).
func NormalizeHost(host string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimSuffix(h, ".")
	h = strings.TrimPrefix(h, "www.")
	if h == "" {
		return "", fmt.Errorf("host vide")
	}
	if len(h) > 253 {
		return "", fmt.Errorf("host trop long (%d caractères, maximum 253)", len(h))
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("host %q : au moins deux labels attendus (exemple : shop.fr)", host)
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || !reHostLabel.MatchString(l) {
			return "", fmt.Errorf("host %q : label %q invalide", host, l)
		}
	}
	return h, nil
}

// BaseID dérive l'identifiant de base d'un host canonique : suppression des points
// et des tirets, troncature déterministe au-delà de MaxBase caractères.
//
//	shop.fr                          → shopfr
//	api.shop.fr                      → apishopfr
//	ma-boutique-de-vente-en-ligne-de-chaussures.fr → maboutiquedeventeenli_e20de4
func BaseID(host string) (string, error) {
	h, err := NormalizeHost(host)
	if err != nil {
		return "", err
	}
	id := reNotKept.ReplaceAllString(h, "")
	if id == "" {
		return "", fmt.Errorf("host %q : aucun caractère exploitable pour un identifiant", host)
	}
	if id[0] >= '0' && id[0] <= '9' {
		id = digitPrefix + id
	}
	if len(id) > MaxBase {
		sum := sha256.Sum256([]byte(h))
		id = id[:truncKeep] + "_" + hex.EncodeToString(sum[:])[:hashLen]
	}
	if !reIdentifier.MatchString(id) {
		return "", fmt.Errorf("identifiant dérivé %q invalide", id)
	}
	return id, nil
}

// ID renvoie l'identifiant effectif du projet : l'identifiant de base en
// production, suffixé de « prep » en préproduction. La troncature portant sur le
// host, la base reste identique entre les deux environnements.
func ID(host string, env Env) (string, error) {
	base, err := BaseID(host)
	if err != nil {
		return "", err
	}
	if env == EnvPrep {
		base += SuffixPrep
	}
	if len(base) > MaxIdentifier {
		return "", fmt.Errorf("identifiant %q trop long (%d > %d)", base, len(base), MaxIdentifier)
	}
	return base, nil
}

// EffectiveHost renvoie le host réellement servi. En préproduction, le host est
// construit — jamais dérivé — sur le domaine dédié : <identifiant de prod>.<domaine>.
func EffectiveHost(host string, env Env, prepDomain string) (string, error) {
	h, err := NormalizeHost(host)
	if err != nil {
		return "", err
	}
	if env != EnvPrep {
		return h, nil
	}
	if prepDomain == "" {
		return "", fmt.Errorf("domaine de préproduction non configuré (environments.prep.domain)")
	}
	base, err := BaseID(host)
	if err != nil {
		return "", err
	}
	return base + "." + strings.TrimPrefix(prepDomain, "."), nil
}

// ValidateIdentifier vérifie un identifiant fourni à la main (option --id).
func ValidateIdentifier(id string) error {
	if !reIdentifier.MatchString(id) {
		return fmt.Errorf("identifiant %q invalide : attendu [a-z][a-z0-9_]{1,31}", id)
	}
	if len(id) > MaxIdentifier {
		return fmt.Errorf("identifiant %q trop long (%d > %d)", id, len(id), MaxIdentifier)
	}
	for _, r := range reservedNames {
		if id == r {
			return fmt.Errorf("identifiant %q réservé par le système", id)
		}
	}
	return nil
}

// reservedNames énumère les comptes système qu'un projet ne doit jamais usurper.
var reservedNames = []string{
	"root", "daemon", "bin", "sys", "sync", "games", "man", "lp", "mail", "news",
	"uucp", "proxy", "www_data", "www-data", "backup", "list", "irc", "nobody",
	"systemd", "syslog", "messagebus", "sshd", "mysql", "postgres", "redis",
	"rabbitmq", "ogs",
}

// RoleName suffixe un identifiant pour une ressource additionnelle : la ressource
// principale porte le nom nu, les suivantes un suffixe de rôle explicite.
func RoleName(id, role string) string {
	if role == "" {
		return id
	}
	return id + "_" + role
}

// EscapeMySQLPattern échappe les jokers d'un motif de privilège MySQL. Sans cela,
// GRANT ... ON `a_b`.* porte aussi sur aXb : l'underscore est un joker.
func EscapeMySQLPattern(name string) string {
	r := strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`)
	return r.Replace(name)
}

// PoolNumber calcule le numéro d'un pool PHP-FPM selon la norme
// 1<version sans point><incrément sur 2 chiffres> : php 8.5, 1er pool → 18501.
func PoolNumber(phpVersion string, increment int) (int, error) {
	digits := strings.ReplaceAll(strings.TrimSpace(phpVersion), ".", "")
	if digits == "" {
		return 0, fmt.Errorf("version PHP vide")
	}
	if _, err := strconv.Atoi(digits); err != nil {
		return 0, fmt.Errorf("version PHP %q invalide", phpVersion)
	}
	if increment < 1 || increment > 99 {
		return 0, fmt.Errorf("incrément %d hors plage (1 à 99 pools par version)", increment)
	}
	n, err := strconv.Atoi(fmt.Sprintf("1%s%02d", digits, increment))
	if err != nil {
		return 0, fmt.Errorf("numéro de pool illisible pour PHP %s", phpVersion)
	}
	if n > 65535 {
		return 0, fmt.Errorf("numéro de pool %d hors plage : la norme 1<version><incrément> "+
			"cesse d'être applicable pour PHP %s", n, phpVersion)
	}
	return n, nil
}

// RuntimeCode associe un chiffre à chaque runtime applicatif (norme 2<code><incrément>).
var RuntimeCode = map[string]int{"go": 1, "node": 2, "python": 3, "ruby": 4}

// AppPort calcule le port d'une application non-PHP : 2<code runtime><incrément sur 3 chiffres>.
func AppPort(runtime string, increment int) (int, error) {
	code, ok := RuntimeCode[runtime]
	if !ok {
		return 0, fmt.Errorf("runtime %q inconnu (attendu : go, node, python ou ruby)", runtime)
	}
	if increment < 1 || increment > 999 {
		return 0, fmt.Errorf("incrément %d hors plage (1 à 999 par runtime)", increment)
	}
	return 20000 + code*1000 + increment, nil
}
