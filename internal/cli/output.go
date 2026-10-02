package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/goccy/go-yaml"

	"leblanc.io/open-go-server/internal/engine"
)

// envelope est l'enveloppe stable des sorties machine.
type envelope struct {
	APIVersion string   `json:"apiVersion" yaml:"apiVersion"`
	Kind       string   `json:"kind" yaml:"kind"`
	Items      any      `json:"items" yaml:"items"`
	Warnings   []string `json:"warnings" yaml:"warnings"`
}

// emit écrit un résultat au format demandé. Le format table est rendu par le
// rappel fourni, qui reste libre de sa mise en forme.
func (a *App) emit(kind string, items any, warnings []string, table func(*tabwriter.Writer)) error {
	switch a.Format {
	case "json":
		enc := json.NewEncoder(a.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(envelope{APIVersion: "ogs/v1", Kind: kind, Items: items, Warnings: orEmpty(warnings)})
	case "yaml":
		data, err := yaml.Marshal(envelope{APIVersion: "ogs/v1", Kind: kind, Items: items, Warnings: orEmpty(warnings)})
		if err != nil {
			return err
		}
		_, err = a.Out.Write(data)
		return err
	default:
		if table == nil {
			return nil
		}
		tw := tabwriter.NewWriter(a.Out, 0, 4, 2, ' ', 0)
		table(tw)
		if err := tw.Flush(); err != nil {
			return err
		}
		for _, w := range warnings {
			fmt.Fprintf(a.Out, "⚠ %s\n", w)
		}
		return nil
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// printf écrit la progression sur la sortie, sauf en mode silencieux ou
// machine. Un avertissement, un échec ou ce que l'exploitant doit faire ne
// passent jamais par lui : voir warnf.
func (a *App) printf(format string, args ...any) {
	if a.Quiet || a.Format != "table" {
		return
	}
	fmt.Fprintf(a.Out, format, args...)
}

// warnf écrit un avertissement, un échec ou une consigne, qui ne se taisent
// jamais (D101) : en mode table, ils suivent le fil de la sortie ; sous --quiet
// ou -o json|yaml, ils passent sur la sortie d'erreur, qui laisse la sortie
// standard au seul document. Par printf, « apply -q » rendait le code 6 sans
// dire quel avertissement, et taisait le retour automatique d'ufw.
func (a *App) warnf(format string, args ...any) {
	w := a.Out
	if a.Quiet || a.Format != "table" {
		w = a.Err
	}
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, format, args...)
}

// stepf annonce l'issue d'une étape : une étape critique en échec est un
// avertissement, les autres de la progression. Une étape non critique en échec
// ne dit rien ici (annonceÉtape) : sa ligne est celle d'OnWarning.
func (a *App) stepf(indent string, s *engine.Step, stepErr error) {
	if !annonceÉtape(s, stepErr) {
		return
	}
	line := indent + issueDÉtape(s, stepErr) + "\n"
	if stepErr != nil && s.Critical && !errors.As(stepErr, new(*engine.PartialError)) {
		a.warnf("%s", line)
		return
	}
	a.printf("%s", line)
}

// confirm demande une confirmation. En mode non interactif, l'absence de --yes
// est une erreur explicite plutôt qu'un accord tacite.
func (a *App) confirm(question string) error {
	if a.Yes {
		return nil
	}
	if a.NonInteractive {
		return exitf(CodeCancelled, "confirmation requise pour %s : ajouter --yes en mode non interactif", question)
	}
	fmt.Fprintf(a.Out, "%s [o/N] ", question)
	var answer string
	if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil {
		return exitf(CodeCancelled, "opération annulée")
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "o", "oui", "y", "yes":
		return nil
	}
	return exitf(CodeCancelled, "opération annulée")
}
