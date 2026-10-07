// Package tui porte l'interface en mode texte : inspecteur du parc, actions
// contextuelles et assistant de création.
package tui

import (
	"os"

	"github.com/charmbracelet/lipgloss"
)

// Styles regroupe la mise en forme. Elle se dégrade proprement : sans couleur,
// l'information reste portée par les symboles et la disposition.
type Styles struct {
	Title    lipgloss.Style
	Pane     lipgloss.Style
	Selected lipgloss.Style
	Item     lipgloss.Style
	Label    lipgloss.Style
	Value    lipgloss.Style
	Healthy  lipgloss.Style
	Warning  lipgloss.Style
	Failure  lipgloss.Style
	Help     lipgloss.Style
	Prompt   lipgloss.Style
	Plain    bool
}

// NewStyles construit les styles. NO_COLOR et TERM=dumb sont respectés : un
// terminal qui ne sait pas afficher de couleur ne doit pas recevoir de séquences
// d'échappement.
func NewStyles() Styles {
	plain := os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb"
	if plain {
		return Styles{Plain: true}
	}
	return Styles{
		Title:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("62")),
		Pane:     lipgloss.NewStyle().Padding(0, 1),
		Selected: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")),
		Item:     lipgloss.NewStyle().Foreground(lipgloss.Color("252")),
		Label:    lipgloss.NewStyle().Foreground(lipgloss.Color("244")),
		Value:    lipgloss.NewStyle().Foreground(lipgloss.Color("252")),
		Healthy:  lipgloss.NewStyle().Foreground(lipgloss.Color("42")),
		Warning:  lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
		Failure:  lipgloss.NewStyle().Foreground(lipgloss.Color("196")),
		Help:     lipgloss.NewStyle().Foreground(lipgloss.Color("241")),
		Prompt:   lipgloss.NewStyle().Bold(true),
	}
}

// health rend un état avec son symbole. Les symboles portent l'information : ils
// restent lisibles sans couleur, et distinguent les états pour qui ne les
// perçoit pas.
func (s Styles) health(state string) string {
	symbol, style := "●", s.Healthy
	switch state {
	case "dérive":
		symbol, style = "⚠", s.Warning
	case "erreur", "TLS absent", "services incomplets":
		symbol, style = "✕", s.Failure
	}
	text := symbol + " " + state
	if s.Plain {
		return text
	}
	return style.Render(text)
}

func (s Styles) label(text string) string {
	if s.Plain {
		return text
	}
	return s.Label.Render(text)
}

func (s Styles) value(text string) string {
	if s.Plain {
		return text
	}
	return s.Value.Render(text)
}

func (s Styles) title(text string) string {
	if s.Plain {
		return text
	}
	return s.Title.Render(text)
}

func (s Styles) help(text string) string {
	if s.Plain {
		return text
	}
	return s.Help.Render(text)
}

func (s Styles) selected(text string) string {
	if s.Plain {
		return "▸ " + text
	}
	return s.Selected.Render("▸ " + text)
}

func (s Styles) item(text string) string {
	if s.Plain {
		return "  " + text
	}
	return s.Item.Render("  " + text)
}

func (s Styles) warning(text string) string {
	if s.Plain {
		return text
	}
	return s.Warning.Render(text)
}
