package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

// Run lance l'interface. Elle est refusée si la sortie n'est pas un terminal :
// une TUI dans un pipe ou un cron ne produirait que des séquences d'échappement.
func Run(backend Backend) error {
	program := tea.NewProgram(New(backend), tea.WithAltScreen())
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("interface interrompue : %w", err)
	}
	return nil
}
