package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// WizardAnswers porte les réponses de l'assistant de création.
type WizardAnswers struct {
	Host       string
	Kind       string
	AppVersion string
	PHPVersion string
	Env        string
	Database   string
	Messenger  string
	Instances  string
}

// step est une question de l'assistant. L'accès au champ passe par des fonctions
// et non par un pointeur : la structure de l'assistant étant copiée à la
// construction, un pointeur vers son champ référencerait la structure d'origine,
// abandonnée — et les réponses seraient silencieusement perdues.
type step struct {
	label string
	help  string
	// helpOf rend une aide qui dépend des réponses déjà données ; elle l'emporte
	// sur help. Le host en a besoin : ce qu'il faut saisir dépend de
	// l'environnement, et le montrer vaut mieux que le décrire.
	helpOf  func(WizardAnswers, string) string
	get     func(WizardAnswers) string
	set     func(*WizardAnswers, string)
	choices []string
	// optional autorise une réponse vide.
	optional bool
}

// wizard est l'assistant pas-à-pas.
type wizard struct {
	answers WizardAnswers
	steps   []step
	index   int
	input   string
	err     string
	done    bool
	// prepDomain sert l'aide de l'étape du host : annoncer le nom réellement
	// servi évite la saisie d'un host déjà construit.
	prepDomain string
}

// newWizard construit l'assistant. Les valeurs par défaut sont celles de la
// création en ligne de commande : l'assistant ne doit pas produire un projet
// différent de « ogs site create ».
func newWizard(prepDomain string) wizard {
	w := wizard{prepDomain: prepDomain, answers: WizardAnswers{
		Kind: "php-symfony", AppVersion: "auto", PHPVersion: "8.3", Env: "prod",
	}}
	w.steps = []step{
		// L'environnement est demandé avant le host : en préproduction, le host
		// saisi n'est pas celui qui sera servi, et l'annoncer après coup n'aide
		// personne.
		{label: "Environnement",
			get:     func(a WizardAnswers) string { return a.Env },
			set:     func(a *WizardAnswers, v string) { a.Env = v },
			choices: []string{"prod", "prep"}},
		{label: "Host canonique du site", helpOf: hostHelp,
			get: func(a WizardAnswers) string { return a.Host },
			set: func(a *WizardAnswers, v string) { a.Host = v }},
		{label: "Type d'application",
			get: func(a WizardAnswers) string { return a.Kind },
			set: func(a *WizardAnswers, v string) { a.Kind = v },
			choices: []string{"php-symfony", "php-prestashop", "php-plain", "static",
				"go", "node", "python", "ruby"}},
		{label: "Version applicative", help: "« auto » lit le composer.json",
			get: func(a WizardAnswers) string { return a.AppVersion },
			set: func(a *WizardAnswers, v string) { a.AppVersion = v }, optional: true},
		{label: "Version PHP", help: "7.4 à 8.5",
			get: func(a WizardAnswers) string { return a.PHPVersion },
			set: func(a *WizardAnswers, v string) { a.PHPVersion = v }, optional: true},
		{label: "Base de données", help: "vide pour aucune",
			get:     func(a WizardAnswers) string { return a.Database },
			set:     func(a *WizardAnswers, v string) { a.Database = v },
			choices: []string{"", "mysql", "mariadb", "postgres"}, optional: true},
		{label: "Transports Messenger", help: "séparés par une virgule, vide pour aucun",
			get: func(a WizardAnswers) string { return a.Messenger },
			set: func(a *WizardAnswers, v string) { a.Messenger = v }, optional: true},
		{label: "Instances de worker", help: "vide si aucun transport",
			get: func(a WizardAnswers) string { return a.Instances },
			set: func(a *WizardAnswers, v string) { a.Instances = v }, optional: true},
	}
	return w
}

// current renvoie l'étape courante.
func (w *wizard) current() step { return w.steps[w.index] }

// help rend l'aide de l'étape courante.
func (w *wizard) help() string {
	s := w.current()
	if s.helpOf != nil {
		return s.helpOf(w.answers, w.prepDomain)
	}
	return s.help
}

// hostHelp explique quel host saisir. En préproduction, le host servi est
// construit sur le domaine dédié : le montrer sur l'exemple est le seul moyen
// d'éviter la saisie d'un host déjà construit, qui le construirait deux fois.
func hostHelp(a WizardAnswers, prepDomain string) string {
	if a.Env != "prep" {
		return "le host servi, exemple : shop.fr"
	}
	domain := prepDomain
	if domain == "" {
		domain = "<domaine de préproduction>"
	}
	return fmt.Sprintf("host de production, exemple : shop.fr → shop.%s", domain)
}

// wizardKey traite les touches de l'assistant.
func (m *Model) wizardKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	w := &m.wizard
	switch key.Type {
	case tea.KeyEsc:
		// Sortir de l'assistant ne perd rien : aucun projet n'est écrit avant la
		// dernière étape.
		m.mode = modeList
		m.status = "création abandonnée"
		return m, nil
	case tea.KeyEnter:
		value := strings.TrimSpace(w.input)
		s := w.current()
		if value == "" {
			// Une réponse vide reprend le défaut, s'il en existe un.
			value = s.get(w.answers)
		}
		if value == "" && !s.optional {
			w.err = "cette réponse est obligatoire"
			return m, nil
		}
		if len(s.choices) > 0 && value != "" {
			valid := false
			for _, c := range s.choices {
				if c == value {
					valid = true
				}
			}
			if !valid {
				w.err = "valeur attendue : " + strings.Join(nonEmpty(s.choices), ", ")
				return m, nil
			}
		}
		s.set(&w.answers, value)
		w.err = ""
		w.input = ""
		if w.index == len(w.steps)-1 {
			answers := w.answers
			m.mode = modeList
			m.busy = true
			m.status = "création en cours…"
			return m, func() tea.Msg {
				out, err := m.backend.Create(context.Background(), answers)
				if err != nil {
					return outputMsg{err: err}
				}
				return outputMsg{lines: strings.Split(strings.TrimRight(out, "\n"), "\n")}
			}
		}
		w.index++
		return m, nil
	case tea.KeyBackspace:
		if w.input != "" {
			w.input = w.input[:len(w.input)-1]
		}
		return m, nil
	case tea.KeyRunes, tea.KeySpace:
		w.input += string(key.Runes)
		if key.Type == tea.KeySpace {
			w.input += " "
		}
		return m, nil
	}
	return m, nil
}

func nonEmpty(items []string) []string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
