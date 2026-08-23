package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/goccy/go-yaml"
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

// printf écrit sur la sortie, sauf en mode silencieux ou machine.
func (a *App) printf(format string, args ...any) {
	if a.Quiet || a.Format != "table" {
		return
	}
	fmt.Fprintf(a.Out, format, args...)
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
