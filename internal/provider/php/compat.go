package php

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"leblanc.io/open-go-server/internal/fs"
)

// maxComposerSize borne ce que root lit de composer.json. Ceux des frameworks
// tiennent en quelques dizaines de Kio ; un fichier plus gros qu'un Mio n'est pas
// lu, et c'est la table indicative qui décide.
const maxComposerSize = 1 << 20

// composerFile décrit le sous-ensemble de composer.json qui nous intéresse.
type composerFile struct {
	Require map[string]string `json:"require"`
	Config  struct {
		Platform map[string]string `json:"platform"`
	} `json:"config"`
}

// Constraint lit la contrainte PHP déclarée par l'application. C'est la source
// d'autorité (D38) : plutôt qu'une matrice figée dans l'outil, qui vieillirait à
// chaque sortie de framework, on lit ce que l'application exige réellement.
// La plateforme configurée (config.platform.php) prime, car c'est celle sur
// laquelle les dépendances ont été résolues.
//
// Le fichier est lu en root, par « php switch », dans un répertoire que
// l'utilisateur du projet possède : ni www ni composer.json ne sont suivis s'ils
// sont des liens, et un fichier spécial n'est pas lu. Un os.ReadFile par le nom
// lisait n'importe quel fichier du système — et sans fin sur un lien vers
// /dev/zero, ou pour toujours sur une FIFO. La lecture est bornée
// (maxComposerSize) : un composer.json creux de plusieurs Gio ne coûte rien à
// l'utilisateur, et root le lisait en entier en mémoire. Un refus rend une
// erreur, que Check traite comme un fichier illisible.
//
// Le propriétaire du fichier n'est pas contrôlé : composer.json est une
// déclaration de l'utilisateur, qui y écrit la contrainte qu'il veut, et un
// projet adopté peut porter un composer.json déployé par root, que le refuser
// ferait juger par la seule table indicative. Un lien physique vers le fichier
// d'un autre compte n'en livrerait qu'une contrainte PHP, ou un caractère dans
// l'erreur d'analyse.
func Constraint(root string) (constraint, source string, err error) {
	path := filepath.Join(root, "www", "composer.json")
	www, err := fs.OpenDirAt(root, "www")
	var data []byte
	if err == nil {
		defer www.Close()
		data, _, err = www.ReadFile("composer.json", -1, maxComposerSize)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", nil
		}
		return "", "", err
	}
	var c composerFile
	if err := json.Unmarshal(data, &c); err != nil {
		return "", path, fmt.Errorf("%s illisible : %w", path, err)
	}
	if v := c.Config.Platform["php"]; v != "" {
		return v, path + " (config.platform.php)", nil
	}
	if v := c.Require["php"]; v != "" {
		return v, path + " (require.php)", nil
	}
	return "", path, nil
}

// Satisfies évalue une contrainte Composer contre une version PHP. Le sous-ensemble
// couvert est celui réellement rencontré dans les composer.json : ^, ~, >=, >, <=,
// <, =, les alternatives séparées par « || » et les conjonctions séparées par un
// espace ou une virgule.
func Satisfies(constraint, version string) (bool, error) {
	constraint = strings.TrimSpace(constraint)
	if constraint == "" || constraint == "*" {
		return true, nil
	}
	target, err := parse(version)
	if err != nil {
		return false, err
	}
	// Composer accepte « || » et « | » comme séparateur d'alternatives ; les
	// contraintes réelles utilisent les deux (Symfony 3.4 : « ^5.5.9|>=7.0.8 »).
	for _, alternative := range strings.Split(strings.ReplaceAll(constraint, "||", "|"), "|") {
		ok, err := satisfiesAll(alternative, target)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func satisfiesAll(expr string, target [3]int) (bool, error) {
	expr = strings.ReplaceAll(expr, ",", " ")
	for _, term := range strings.Fields(expr) {
		ok, err := satisfiesOne(term, target)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func satisfiesOne(term string, target [3]int) (bool, error) {
	switch {
	case strings.HasPrefix(term, "^"):
		// ^8.1 autorise toute version 8.x supérieure ou égale, mais pas 9.
		base, err := parse(strings.TrimPrefix(term, "^"))
		if err != nil {
			return false, err
		}
		return compare(target, base) >= 0 && target[0] == base[0], nil
	case strings.HasPrefix(term, "~"):
		// ~8.1 autorise 8.1.x et au-delà dans la même mineure ; ~8.1.2 de même.
		raw := strings.TrimPrefix(term, "~")
		base, err := parse(raw)
		if err != nil {
			return false, err
		}
		if strings.Count(raw, ".") <= 1 {
			return compare(target, base) >= 0 && target[0] == base[0], nil
		}
		return compare(target, base) >= 0 && target[0] == base[0] && target[1] == base[1], nil
	case strings.HasPrefix(term, ">="):
		return compareTo(term, ">=", target)
	case strings.HasPrefix(term, "<="):
		return compareTo(term, "<=", target)
	case strings.HasPrefix(term, "!="):
		base, err := parse(strings.TrimPrefix(term, "!="))
		if err != nil {
			return false, err
		}
		return compare(target, base) != 0, nil
	case strings.HasPrefix(term, ">"):
		return compareTo(term, ">", target)
	case strings.HasPrefix(term, "<"):
		return compareTo(term, "<", target)
	case strings.HasPrefix(term, "="):
		term = strings.TrimPrefix(term, "=")
	}
	base, err := parse(term)
	if err != nil {
		return false, err
	}
	switch precision(term) {
	case 1:
		// « 8.* » et « 8.x » ne contraignent que la majeure. Un « 8 » nu reste une
		// version exacte, comme Composer le lit, d'où le contrôle du joker.
		if strings.ContainsAny(term, "*x") {
			return target[0] == base[0], nil
		}
	case 2:
		// Une version partielle (« 8.1 », « 8.1.* ») vaut pour toute la mineure.
		return target[0] == base[0] && target[1] == base[1], nil
	}
	return compare(target, base) == 0, nil
}

// precision rend le nombre de composantes réellement fournies par un terme, le
// joker exclu : « 8 » et « 8.* » en fournissent une, « 8.1 » et « 8.1.* » deux.
// Compter les points du terme brut, comme on le faisait, comptait celui du
// joker : « 8.* » exigeait alors une mineure nulle que « parse » n'avait jamais
// lue, et n'était donc satisfait par aucune version au-delà de 8.0.
func precision(term string) int {
	term = strings.TrimSpace(term)
	for _, sep := range []string{"-", "+", "@"} {
		if i := strings.Index(term, sep); i > 0 {
			term = term[:i]
		}
	}
	term = strings.TrimSuffix(term, ".*")
	n := 0
	for _, part := range strings.SplitN(term, ".", 3) {
		if part == "" || part == "*" || part == "x" {
			break
		}
		n++
	}
	return n
}

func compareTo(term, op string, target [3]int) (bool, error) {
	base, err := parse(strings.TrimPrefix(term, op))
	if err != nil {
		return false, err
	}
	c := compare(target, base)
	switch op {
	case ">=":
		return c >= 0, nil
	case "<=":
		return c <= 0, nil
	case ">":
		return c > 0, nil
	case "<":
		return c < 0, nil
	}
	return false, fmt.Errorf("opérateur %q non géré", op)
}

// parse lit une version en trois composantes, en ignorant les suffixes de
// pré-publication (8.1.0-RC1) qui n'ont pas d'incidence sur notre décision.
func parse(v string) ([3]int, error) {
	var out [3]int
	v = strings.TrimSpace(v)
	for _, sep := range []string{"-", "+", "@"} {
		if i := strings.Index(v, sep); i > 0 {
			v = v[:i]
		}
	}
	v = strings.TrimSuffix(v, ".*")
	if v == "" {
		return out, fmt.Errorf("version vide")
	}
	for i, part := range strings.SplitN(v, ".", 3) {
		if part == "*" || part == "x" {
			break
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return out, fmt.Errorf("version %q illisible", v)
		}
		out[i] = n
	}
	return out, nil
}

func compare(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		switch {
		case a[i] > b[i]:
			return 1
		case a[i] < b[i]:
			return -1
		}
	}
	return 0
}

// fallbackConstraints est la table indicative, utilisée seulement en l'absence de
// composer.json exploitable. Elle est volontairement grossière : la valeur
// d'autorité reste celle déclarée par l'application.
var fallbackConstraints = map[string]string{
	"php-prestashop": ">=7.1 <8.2",
	"php-symfony":    ">=7.4",
}

// Verdict est la réponse du garde-fou de compatibilité.
type Verdict struct {
	// OK dit si la version convient.
	OK bool
	// Detail décrit la source de la décision, pour que le message soit
	// vérifiable par l'administrateur.
	Detail string
	// Unread porte la raison pour laquelle composer.json n'a pas pu être lu —
	// illisible, lien refusé —, nil s'il l'a été ou s'il est absent.
	Unread error
}

// Check vérifie qu'une version PHP convient à l'application.
//
// Un composer.json qui ne peut pas être lu décide comme un composer.json absent :
// par la table indicative. Le traiter en simple avertissement désarmait le
// garde-fou, et à volonté depuis que ni un lien ni un fichier spécial ne sont
// lus : il suffisait à l'utilisateur de remplacer le fichier par un lien pour
// qu'un Symfony passe sans contrôle à une version qui le casse. Unread le dit,
// pour que l'appelant ne l'annonce pas comme vérifié sur l'application.
func Check(root, kind, version string) (Verdict, error) {
	constraint, source, unread := Constraint(root)
	if unread != nil || constraint == "" {
		v := Verdict{OK: true, Detail: "aucune contrainte déclarée", Unread: unread}
		if fb, has := fallbackConstraints[kind]; has {
			ok, err := Satisfies(fb, version)
			if err != nil {
				return v, err
			}
			v.OK, v.Detail = ok, fmt.Sprintf("table indicative pour %s (%s)", kind, fb)
		} else if unread != nil {
			v.Detail = "aucune table indicative pour " + kind
		}
		if unread != nil {
			v.Detail = fmt.Sprintf("%v — %s", unread, v.Detail)
		}
		return v, nil
	}
	ok, err := Satisfies(constraint, version)
	if err != nil {
		return Verdict{OK: true, Detail: fmt.Sprintf("contrainte %q non évaluable (%s)", constraint, source)}, nil
	}
	return Verdict{OK: ok, Detail: fmt.Sprintf("contrainte %q lue dans %s", constraint, source)}, nil
}
