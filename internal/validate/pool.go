package validate

import (
	"fmt"
	"regexp"
	"strings"
)

// Règles des réglages d'un pool PHP-FPM (D103). Elles ne sont **jamais**
// éprouvées au chargement d'un projet : une déclaration ancienne qui les enfreint
// arrêterait sinon tout le parc — « cert renew » du minuteur compris. Elles le
// sont au plan et à l'apply du projet visé, aux points d'écriture d'une
// déclaration et par « doctor ».

var (
	// rePoolKey est la grammaire d'une directive de pool : lettres, chiffres,
	// « . », « _ » et « - » — pm.max_children, request_slowlog_timeout…
	rePoolKey = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	// rePoolIniKey est celle des directives à crochets : une valeur d'ini, ou une
	// variable d'environnement.
	rePoolIniKey = regexp.MustCompile(`^(php_value|php_flag|php_admin_value|php_admin_flag|env)\[[A-Za-z0-9._-]+\]$`)
	// rePoolEnvKey est le nom d'une variable de « spec.php.pools[].env », rendue
	// en env[<nom>].
	rePoolEnvKey = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	// reFunctionName est un nom de fonction PHP.
	reFunctionName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// rePresetName est la forme d'un nom de preset. Qu'il soit connu ne se juge
	// qu'avec la configuration de la machine, donc au plan.
	rePresetName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

// PoolKeyProblem dit pourquoi une clé de réglage ne peut pas être rendue, vide si
// elle le peut. Une clé hors grammaire ferait écrire au pool une ligne que FPM
// interpréterait autrement qu'elle ne se lit — ou une seconde directive.
func PoolKeyProblem(key string) string {
	switch {
	case strings.ContainsAny(key, "\n\r\x00"):
		return "la clé tient sur une seule ligne"
	case strings.Contains(key, "["):
		if !rePoolIniKey.MatchString(CanonicalPoolKey(key)) {
			return "clé à crochets hors grammaire FPM (php_value, php_flag, php_admin_value, " +
				"php_admin_flag ou env, suivis de [nom] en lettres, chiffres, « . », « _ », « - »)"
		}
	case !rePoolKey.MatchString(key):
		return "clé hors grammaire FPM (lettres, chiffres, « . », « _ », « - »)"
	}
	return ""
}

// PoolValueProblem dit pourquoi une valeur de réglage ne peut pas être rendue :
// une fin de ligne y injecterait une directive dans le pool, que FPM lit en root.
//
// Une valeur portant « ; » (commentaire) ou « ${ » (variable de l'environnement
// du maître) est rendue entre apostrophes, que l'analyseur d'ini lit
// littéralement (PoolValueNeedsQuote) ; elle ne peut donc porter ni apostrophe
// ni guillemet — avec un guillemet, l'exploitant cite déjà à sa façon, et les
// apostrophes en changeraient le sens. Un guillemet seul ne déclenche jamais la
// citation : la valeur est rendue nue, lue par FPM comme avant D103 (« "a" "b" »
// donne « ab », « "a\"b" » donne « a"b ») ; non apparié, FPM refuserait le pool
// au démarrage, d'où le refus nommé ici.
func PoolValueProblem(value string) string {
	if strings.ContainsAny(value, "\n\r\x00") {
		return "la valeur tient sur une seule ligne"
	}
	special := strings.Contains(value, ";") || strings.Contains(value, "${")
	switch {
	case special && strings.Contains(value, `"`):
		return "une valeur portant « ; » ou « ${ » est rendue entre apostrophes, et ne peut donc pas porter de guillemet"
	case special && strings.Contains(value, "'"):
		return "une valeur portant « ; » ou « ${ » est rendue entre apostrophes, et ne peut donc pas en porter"
	case unescapedQuotes(value)%2 != 0:
		return "guillemet non apparié : FPM refuserait le pool"
	}
	return ""
}

// unescapedQuotes compte les guillemets que l'analyseur d'ini ne lit pas comme
// échappés (« \" »).
func unescapedQuotes(value string) int {
	n := 0
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\':
			i++
		case '"':
			n++
		}
	}
	return n
}

// PoolValueNeedsQuote dit si une valeur doit être rendue entre apostrophes :
// « ; » et « ${ » seuls l'exigent. Les autres restent nues : une citation ferait
// d'« E_ALL & ~E_DEPRECATED » une chaîne, là où l'analyseur évalue l'expression.
func PoolValueNeedsQuote(value string) bool {
	return strings.Contains(value, ";") || strings.Contains(value, "${")
}

// CanonicalPoolKey rend la forme sous laquelle FPM reconnaît une directive :
// minuscules hors crochets — FPM compare les noms sans tenir compte de la
// casse —, le nom entre crochets gardé tel quel.
func CanonicalPoolKey(key string) string {
	if i := strings.Index(key, "["); i >= 0 {
		return strings.ToLower(key[:i]) + key[i:]
	}
	return strings.ToLower(key)
}

// PoolEnvProblem contrôle une variable de « spec.php.pools[].env ».
func PoolEnvProblem(key, value string) string {
	if !rePoolEnvKey.MatchString(key) {
		return "nom de variable hors grammaire (lettres, chiffres, « . », « _ », « - »)"
	}
	return PoolValueProblem(value)
}

// LockedPoolKey dit si une clé échappe aux surcharges, de la machine comme d'un
// projet, et pourquoi. La clé est jugée sous sa forme canonique : FPM ne
// distingue pas « User » de « user ». Ce sont les invariants que le template pose
// lui-même ou que le socle d'ogs fixe :
//   - user et group : l'identité du pool, celle du projet ;
//   - listen et ceux des listen.* qui en règlent l'accès (D31) — owner, group,
//     mode, acl_users, acl_groups, allowed_clients. listen.backlog et
//     listen.setfib (FreeBSD) restent libres : ils ne donnent aucun accès ;
//   - slowlog et access.log : écrits par le maître FPM, **en root**, au chemin
//     donné — un chemin déclaré ferait écrire root où l'on veut ;
//   - php_admin_value[error_log] : le journal pré-créé sous logs.root (D71) ;
//   - include : le maître, en root, lit le fichier nommé, qui peut déclarer un
//     pool entier — « [autre] user = root » ;
//   - prefix : il change la résolution des chemins relatifs du pool, slowlog et
//     listen compris.
//
// chroot, chdir, process.priority et les rlimit_* restent libres : ils ne
// donnent rien au-delà de l'identité du pool.
func LockedPoolKey(key string) (string, bool) {
	switch CanonicalPoolKey(key) {
	case "user", "group":
		return "identité du pool, celle du projet", true
	case "listen", "listen.owner", "listen.group", "listen.mode", "listen.acl_users", "listen.acl_groups",
		"listen.allowed_clients":
		return "point d'écoute du pool et ses droits (D31)", true
	case "slowlog", "access.log":
		return "journal ouvert par le maître FPM en root (D71)", true
	case "php_admin_value[error_log]":
		return "journal des erreurs pré-créé sous logs.root (D71)", true
	case "include":
		return "fichier lu par le maître FPM en root, qui peut déclarer un pool", true
	case "prefix":
		return "base des chemins relatifs du pool, slowlog et listen compris", true
	}
	return "", false
}

// FunctionNameProblem contrôle un nom de fonction de disableFunctions ou
// d'allowFunctions.
func FunctionNameProblem(name string) string {
	if !reFunctionName.MatchString(name) {
		return fmt.Sprintf("nom de fonction %q invalide", name)
	}
	return ""
}

// PresetName contrôle la forme d'un nom de preset.
func PresetName(name string) bool { return rePresetName.MatchString(name) }
