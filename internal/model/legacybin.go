package model

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// LegacyLinkNames sont les liens que l'ancien .bin/ portait (D70) : ce sont les
// seuls qu'ogs y a jamais posés, donc les seuls que la migration retire, et les
// seuls dont elle réécrit les références dans une déclaration (D116).
var LegacyLinkNames = []string{"php", "node", "python", "ruby", "bundle", "app"}

// LegacyBinRef est une valeur de la déclaration qui désigne un lien de l'ancien
// .bin/.
type LegacyBinRef struct {
	// Field nomme la valeur pour l'exploitant : « spec.crons[export].command ».
	Field string
	// Path la désigne dans le YAML, clé par clé : une chaîne pour une clé, un
	// entier pour un rang de liste.
	Path []any
	// Value est la valeur déclarée ; Rewritten, sa réécriture vers .local/bin, ou
	// "" quand la référence n'est pas réécrite (forme relative, que seul
	// l'exploitant sait résoudre).
	Value, Rewritten string
	// Links nomme les liens de l'ancien .bin que la valeur désignait ; la
	// réécriture fait désigner leur remplaçant (LegacyReplacement), et n'a de
	// sens que s'il est en place (D116).
	Links []string
}

// legacyLinkAlt est l'alternative des noms de liens, pour les expressions.
var legacyLinkAlt = strings.Join(LegacyLinkNames, "|")

// legacyRelative reconnaît une référence relative à .bin/<lien> : en tête de mot,
// éventuellement précédée de « ../ ». Sa résolution dépend du répertoire de
// travail de la tâche ou du service : ogs ne la réécrit pas, il la nomme.
var legacyRelative = regexp.MustCompile(`(?:^|[\s'"=:;&|(])(?:\.\./)*\.bin/(?:` + legacyLinkAlt + `)(?:$|[^A-Za-z0-9_.+-])`)

// legacyPrefixes rend les écritures de l'ancien répertoire qu'une commande peut
// porter et qu'ogs sait réécrire : le chemin absolu sous la racine, et, si le
// home du compte est cette racine, « ~ », « $HOME » et « ${HOME} ».
func legacyPrefixes(p *Project) []string {
	root := filepath.Clean(p.Spec.Layout.Root)
	out := []string{root + "/" + LegacyLinkDir + "/"}
	if home := p.Spec.User.Home; home == "" || filepath.Clean(home) == root {
		out = append(out, "~/"+LegacyLinkDir+"/", "$HOME/"+LegacyLinkDir+"/", "${HOME}/"+LegacyLinkDir+"/")
	}
	return out
}

// VenvPython est, relatif à la racine, l'interpréteur d'un projet Python qui
// active son venv : ~/.venv est un lien de répertoire vers le venv de la
// version (D123), que Python traverse ; ~/.local/bin/python désigne
// <venv>/bin/python, et Python, qui cherche pyvenv.cfg à côté du chemin
// invoqué sans résoudre ce lien, lance par lui l'interpréteur de base.
const VenvPython = VenvLinkName + "/bin/python"

// VenvLinkName est le lien de répertoire, à la racine du home d'un projet
// Python, qui désigne le venv de la version déclarée (D123).
const VenvLinkName = ".venv"

// isPython dit si le projet est une application Python.
func (p *Project) isPython() bool {
	at, err := p.AppType()
	return err == nil && at.Runtime == "python"
}

// LegacyReplacement rend, relatif à la racine, ce qui remplace le lien name de
// l'ancien .bin : ~/.local/bin/<name>, sauf python dans un projet Python, que
// remplace ~/.venv/bin/python — une tâche réécrite vers ~/.local/bin/python
// tournerait hors du venv, sans ses dépendances (D116, D123).
func (p *Project) LegacyReplacement(name string) string {
	if name == "python" && p.isPython() {
		return VenvPython
	}
	return LinkDir + "/" + name
}

// rewriteLegacy réécrit, dans une valeur, chaque <préfixe>.bin/<lien> en
// <préfixe><remplaçant> (LegacyReplacement), pour les seuls liens qu'ogs posait
// et seulement en fin de mot : « .bin/php8.1 » ou « .bin/phpunit » ne sont pas à
// lui.
func rewriteLegacy(p *Project, v string) (string, []string, bool) {
	changed := false
	var links []string
	for _, prefix := range legacyPrefixes(p) {
		base := strings.TrimSuffix(prefix, LegacyLinkDir+"/")
		re := regexp.MustCompile(`(^|[^A-Za-z0-9_.$/{}~-])` + regexp.QuoteMeta(prefix) +
			`(` + legacyLinkAlt + `)($|[^A-Za-z0-9_.+-])`)
		for {
			loc := re.FindStringSubmatchIndex(v)
			if loc == nil {
				break
			}
			// Remplacement un à un : deux occurrences contiguës partagent le
			// séparateur, qu'un ReplaceAll consommerait pour la première.
			if name := v[loc[4]:loc[5]]; !slices.Contains(links, name) {
				links = append(links, name)
			}
			name := v[loc[4]:loc[5]]
			v = v[:loc[3]] + base + p.LegacyReplacement(name) + v[loc[5]:]
			changed = true
		}
	}
	return v, links, changed
}

// LegacyBinRefs énumère les valeurs de la déclaration qui désignent un lien de
// l'ancien .bin/ : commandes des tâches, paramètres des services (la commande
// d'un worker, une variable « env. »), commande et variables de l'application.
// Les hooks n'y figurent pas : un hook vit sous /etc/ogs/hooks (§11), jamais
// dans le home.
//
// Le projet doit être normalisé — la racine en vient.
func (p *Project) LegacyBinRefs() []LegacyBinRef {
	var out []LegacyBinRef
	add := func(field string, path []any, v string) {
		if r, links, ok := rewriteLegacy(p, v); ok {
			out = append(out, LegacyBinRef{Field: field, Path: path, Value: v, Rewritten: r, Links: links})
			return
		}
		if legacyRelative.MatchString(v) {
			out = append(out, LegacyBinRef{Field: field, Path: path, Value: v})
		}
	}
	p.eachCommandValue(add)
	return out
}

// eachCommandValue appelle f pour chaque valeur déclarée qui porte une commande
// ou un chemin : commandes des tâches, paramètres des services, commande et
// variables de l'application.
func (p *Project) eachCommandValue(f func(field string, path []any, v string)) {
	for i, c := range p.Spec.Crons {
		f(fmt.Sprintf("spec.crons[%s].command", c.Name), []any{"spec", "crons", i, "command"}, c.Command)
	}
	for i, s := range p.Spec.Services {
		for _, k := range sortedKeys(s.Params) {
			f(fmt.Sprintf("spec.services[%s].params.%s", s.Name, k),
				[]any{"spec", "services", i, "params", k}, s.Params[k])
		}
	}
	f("spec.app.command", []any{"spec", "app", "command"}, p.Spec.App.Command)
	for _, k := range sortedKeys(p.Spec.App.Env) {
		f("spec.app.env."+k, []any{"spec", "app", "env", k}, p.Spec.App.Env[k])
	}
}

// localPython reconnaît un appel à .local/bin/python, sous quelque préfixe que
// ce soit, en fin de mot : « python3 » ou « python3.12 » ne sont pas le lien
// d'ogs.
var localPython = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])\.local/bin/python(?:$|[^A-Za-z0-9_.+-])`)

// LocalPythonRefs nomme les valeurs déclarées d'un projet Python qui appellent
// ~/.local/bin/python : l'interpréteur de base y tourne, venv inactif (D123),
// et ~/.venv/bin/python est ce qu'elles doivent invoquer. doctor les signale ;
// ni le chargement ni le plan ne les refusent — ce lien reste celui du shell.
func (p *Project) LocalPythonRefs() []string {
	if !p.isPython() {
		return nil
	}
	var out []string
	p.eachCommandValue(func(field string, _ []any, v string) {
		if localPython.MatchString(v) {
			out = append(out, field)
		}
	})
	return out
}

// WithLegacyBinRefs rend une copie du projet où les réécritures de refs sont
// portées, sans rien écrire ni toucher à p : crons et unités se calculent ainsi
// sur les nouveaux chemins dans l'apply même qui réécrit la déclaration, et un
// plan recalculé sur le même projet retrouve les mêmes références (D116). Ce
// que la réécriture modifie — tâches, services et leurs paramètres, variables
// de l'application — est dupliqué ; le reste est partagé, en lecture.
func (p *Project) WithLegacyBinRefs(refs []LegacyBinRef) *Project {
	q := *p
	q.Spec.Crons = slices.Clone(p.Spec.Crons)
	q.Spec.Services = slices.Clone(p.Spec.Services)
	for i := range q.Spec.Services {
		q.Spec.Services[i].Params = maps.Clone(q.Spec.Services[i].Params)
	}
	q.Spec.App.Env = maps.Clone(p.Spec.App.Env)
	for _, r := range refs {
		if r.Rewritten == "" {
			continue
		}
		switch r.Path[1] {
		case "crons":
			q.Spec.Crons[r.Path[2].(int)].Command = r.Rewritten
		case "services":
			q.Spec.Services[r.Path[2].(int)].Params[r.Path[4].(string)] = r.Rewritten
		case "app":
			if r.Path[2] == "command" {
				q.Spec.App.Command = r.Rewritten
			} else {
				q.Spec.App.Env[r.Path[3].(string)] = r.Rewritten
			}
		}
	}
	return &q
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
