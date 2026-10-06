package cli

import (
	"context"
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
	// Notes porte les informations d'un plan, hors décompte (D118).
	Notes []string `json:"notes,omitempty" yaml:"notes,omitempty"`
}

// emit écrit un résultat au format demandé. Le format table est rendu par le
// rappel fourni, qui reste libre de sa mise en forme.
func (a *App) emit(kind string, items any, warnings []string, table func(*tabwriter.Writer)) error {
	return a.emitNotes(kind, items, warnings, nil, table)
}

// emitNotes est emit avec les informations d'un plan, rendues dans leur champ
// en JSON et en YAML ; le rendu en table les affiche lui-même.
func (a *App) emitNotes(kind string, items any, warnings, notes []string, table func(*tabwriter.Writer)) error {
	env := envelope{APIVersion: "ogs/v1", Kind: kind, Items: items, Warnings: orEmpty(warnings), Notes: notes}
	switch a.Format {
	case "json":
		enc := json.NewEncoder(a.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(env)
	case "yaml":
		data, err := yaml.Marshal(env)
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

// readAnswer lit la réponse de l'exploitant à une question. Une interruption
// — Ctrl+C, SIGTERM, SIGHUP — y répond « non » sur-le-champ (D122) : captés par
// ogs, ces signaux ne coupent plus la lecture, qui attendait Entrée, et la
// réponse tapée ensuite faisait poursuivre une commande annulée. Une entrée
// fermée ou en erreur — le terminal d'une session ssh coupée rend EIO — vaut
// refus, comme avant.
func (a *App) readAnswer() (string, error) {
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		fmt.Fprintln(a.Out)
		return "", exitf(CodeCancelled, "opération annulée : interruption reçue")
	}
	type lue struct {
		answer string
		err    error
	}
	// La lecture reste en vol après une interruption : la commande s'achève
	// alors, et elle avec.
	// L'entrée est relevée avant le goroutine : resté en vol après une
	// interruption, il ne relit plus os.Stdin, qu'un appelant peut avoir
	// rendu entre-temps.
	in := os.Stdin
	ch := make(chan lue, 1)
	go func() {
		var answer string
		_, err := fmt.Fscanln(in, &answer)
		ch <- lue{answer, err}
	}()
	select {
	case r := <-ch:
		if ctx.Err() != nil {
			return "", exitf(CodeCancelled, "opération annulée : interruption reçue")
		}
		if r.err != nil {
			return "", exitf(CodeCancelled, "opération annulée")
		}
		return r.answer, nil
	case <-ctx.Done():
		fmt.Fprintln(a.Out)
		return "", exitf(CodeCancelled, "opération annulée : interruption reçue")
	}
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
	answer, err := a.readAnswer()
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "o", "oui", "y", "yes":
		return nil
	}
	return exitf(CodeCancelled, "opération annulée")
}
