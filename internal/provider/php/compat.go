package php

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

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
func Constraint(root string) (constraint, source string, err error) {
	path := filepath.Join(root, "www", "composer.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
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
	// Une version partielle (« 8.1 ») vaut pour toute la mineure.
	if strings.Count(term, ".") == 1 {
		return target[0] == base[0] && target[1] == base[1], nil
	}
	return compare(target, base) == 0, nil
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

// CheckCompatibility vérifie qu'une version PHP convient à l'application. Le
// deuxième retour décrit la source de la décision, pour que le message d'erreur
// soit vérifiable par l'administrateur.
func CheckCompatibility(root, kind, version string) (ok bool, detail string, err error) {
	constraint, source, err := Constraint(root)
	if err != nil {
		return true, err.Error(), nil // illisible : avertissement, pas blocage
	}
	if constraint == "" {
		if fb, has := fallbackConstraints[kind]; has {
			ok, err := Satisfies(fb, version)
			return ok, fmt.Sprintf("table indicative pour %s (%s)", kind, fb), err
		}
		return true, "aucune contrainte déclarée", nil
	}
	ok, err = Satisfies(constraint, version)
	if err != nil {
		return true, fmt.Sprintf("contrainte %q non évaluable (%s)", constraint, source), nil
	}
	return ok, fmt.Sprintf("contrainte %q lue dans %s", constraint, source), nil
}
