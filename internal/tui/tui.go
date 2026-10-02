package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// ErrInterrupted signale une interface fermée sur une interruption : une action
// annulée par Ctrl+C, ou un signal reçu. La commande rend le code 9.
var ErrInterrupted = errors.New("interface interrompue")

// Run lance l'interface sous le contexte de la commande. Elle est refusée si la
// sortie n'est pas un terminal : une TUI dans un pipe ou un cron ne produirait
// que des séquences d'échappement.
func Run(ctx context.Context, backend Backend) error {
	m, err := run(ctx, backend, tea.WithAltScreen())
	// Une sortie demandée pendant une action en a attendu la fin : son compte
	// rendu, que l'écran alternatif emporte, est redonné sur la sortie standard.
	if m != nil && m.quitAfter && len(m.Output()) > 0 {
		fmt.Fprintln(os.Stdout, strings.Join(m.Output(), "\n"))
	}
	if err == nil && m != nil && m.Interrupted() {
		return ErrInterrupted
	}
	return err
}

// run exécute le programme. Bubble Tea traduit lui-même SIGTERM en sortie
// immédiate, sans attendre les commandes en vol — une saga abandonnée entre deux
// étapes, puis le processus qui sort sous elle (Q-101) : son gestionnaire est
// donc écarté, et l'interruption arrive par le contexte, que main annule sur
// SIGINT et SIGTERM. Le modèle arrête alors l'action en vol et ne se ferme
// qu'une fois qu'elle a rendu la main.
func run(ctx context.Context, backend Backend, opts ...tea.ProgramOption) (*Model, error) {
	m := NewWithContext(ctx, backend)
	program := tea.NewProgram(m, append(opts, tea.WithoutSignalHandler())...)
	stop := context.AfterFunc(ctx, func() { program.Send(interruptMsg{}) })
	defer stop()
	final, err := program.Run()
	if err != nil {
		return nil, fmt.Errorf("interface interrompue : %w", err)
	}
	fm, _ := final.(*Model)
	return fm, nil
}
