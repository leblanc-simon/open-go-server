package tui

import (
	"fmt"
	"strings"

	"leblanc.io/open-go-server/internal/model"
)

// View rend l'écran courant.
func (m *Model) View() string {
	if m.quitting {
		return ""
	}
	switch m.mode {
	case modeWizard:
		return m.viewWizard()
	case modeDiff:
		return m.viewLines("écarts", m.diff, "aucun écart : le projet est conforme")
	case modeOutput:
		return m.viewLines("résultat", m.output, "(aucune sortie)")
	case modeConfirm:
		return m.viewConfirm()
	}
	return m.viewList()
}

// viewList rend l'inspecteur : liste à gauche, détail à droite.
func (m *Model) viewList() string {
	var b strings.Builder
	b.WriteString(m.styles.title(fmt.Sprintf("ogs — %d projet(s)", len(m.projects))))
	if m.filter != "" || m.filtering {
		b.WriteString(m.styles.help(fmt.Sprintf("   filtre : %s", m.filter)))
		if m.filtering {
			b.WriteString(m.styles.help("▏"))
		}
	}
	b.WriteString("\n\n")

	list := m.visible()
	if len(list) == 0 {
		if m.err != nil {
			b.WriteString("erreur : " + m.err.Error() + "\n")
		} else if m.filter != "" {
			b.WriteString("aucun projet ne correspond au filtre\n")
		} else {
			b.WriteString("aucun projet déclaré. Appuyer sur « n » pour en créer un.\n")
		}
		b.WriteString("\n" + m.helpLine())
		return b.String()
	}

	// Colonne de gauche : les projets, avec leur état s'il est connu.
	width := 26
	rows := make([]string, 0, len(list))
	for i, p := range list {
		label := p.ID()
		if snapshot, ok := m.snapshots[p.ID()]; ok {
			label = fmt.Sprintf("%-16s %s", truncate(p.ID(), 16), m.styles.health(snapshot.Health))
		}
		if i == m.cursor {
			rows = append(rows, m.styles.selected(label))
			continue
		}
		rows = append(rows, m.styles.item(label))
	}

	detail := m.viewDetail(m.current())
	detailLines := strings.Split(detail, "\n")
	for i := 0; i < max(len(rows), len(detailLines)); i++ {
		left := ""
		if i < len(rows) {
			left = rows[i]
		}
		right := ""
		if i < len(detailLines) {
			right = detailLines[i]
		}
		b.WriteString(fmt.Sprintf("%-*s │ %s\n", width, left, right))
	}
	b.WriteString("\n" + m.helpLine())
	if m.status != "" {
		b.WriteString("\n" + m.styles.help(m.status))
	}
	if m.loading > 0 && !m.running {
		b.WriteString("\n" + m.styles.help("observation en cours…"))
	}
	return b.String()
}

// viewDetail rend le panneau de détail d'un projet.
func (m *Model) viewDetail(p *model.Project) string {
	if p == nil {
		return ""
	}
	at, _ := p.AppType()
	var b strings.Builder
	line := func(label, value string) {
		if value == "" {
			return
		}
		b.WriteString(fmt.Sprintf("%-11s %s\n", m.styles.label(label), m.styles.value(value)))
	}
	b.WriteString(m.styles.title(p.ID()) + "  " + m.styles.help(p.Metadata.Env) + "\n")
	line("host", p.Metadata.Host)
	if len(p.Spec.Domains) > 1 {
		names := make([]string, 0, len(p.Spec.Domains))
		for _, d := range p.Spec.Domains[1:] {
			names = append(names, d.Name)
		}
		line("domaines", strings.Join(names, ", "))
	}
	line("type", string(p.Spec.App.Kind))
	line("home", p.Spec.Layout.Root)
	if at.Docroot != "" {
		line("docroot", p.Spec.Layout.Docroot)
	}
	line("utilisateur", p.Spec.User.Name+":"+p.Spec.User.Group)
	for _, pool := range p.Spec.PHP.Pools {
		line("php", fmt.Sprintf("%s (pool %s, %s)", pool.Version, pool.Name, pool.PresetLabel()))
	}
	for _, db := range p.Spec.Databases {
		line("base", fmt.Sprintf("%s %s", db.Engine, db.Name))
	}
	if p.Spec.AMQP != nil {
		line("transport", p.Spec.AMQP.Vhost)
	}

	snapshot, known := m.snapshots[p.ID()]
	if !known {
		b.WriteString("\n" + m.styles.help("état non encore observé"))
		return b.String()
	}
	b.WriteString("\n")
	line("services", snapshot.Services)
	line("files", snapshot.Queues)
	if snapshot.Certificate != "" {
		line("certificat", fmt.Sprintf("%s (%d j)", snapshot.Certificate, snapshot.CertDays))
	}
	if snapshot.Drift == 0 {
		line("dérive", "aucune")
	} else {
		line("dérive", fmt.Sprintf("%d écart(s) — « d » pour le détail", snapshot.Drift))
	}
	for _, problem := range snapshot.Problems {
		b.WriteString(m.styles.help("⚠ "+problem) + "\n")
	}
	return b.String()
}

// viewLines rend un écran de texte défilant.
func (m *Model) viewLines(title string, lines []string, empty string) string {
	var b strings.Builder
	b.WriteString(m.styles.title("ogs — "+title) + "\n\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		b.WriteString(empty + "\n")
	} else {
		limit := m.height - 6
		if limit < 5 {
			limit = 5
		}
		for i, l := range lines {
			if i >= limit {
				b.WriteString(m.styles.help(fmt.Sprintf("… %d ligne(s) de plus", len(lines)-limit)) + "\n")
				break
			}
			b.WriteString(l + "\n")
		}
	}
	b.WriteString("\n" + m.styles.help("⏎ ou q pour revenir"))
	return b.String()
}

// viewConfirm rend la demande de confirmation.
func (m *Model) viewConfirm() string {
	var b strings.Builder
	b.WriteString(m.styles.title("ogs") + "\n\n")
	// Le plan tient dans l'écran : au-delà, ce qui manque est compté plutôt que
	// rogné en silence.
	lines := m.confirm.lines
	room := m.height - 8
	if room < 3 {
		room = 3
	}
	if len(lines) > room {
		hidden := len(lines) - room + 1
		lines = append(append([]string(nil), lines[:room-1]...),
			fmt.Sprintf("… et %d ligne(s) de plus (« d » sur le projet pour le détail)", hidden))
	}
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	if len(lines) > 0 {
		b.WriteString("\n")
	}
	b.WriteString(m.confirm.question + "\n\n")
	b.WriteString(m.styles.help("o pour confirmer, toute autre touche pour annuler"))
	return b.String()
}

// viewWizard rend l'assistant.
func (m *Model) viewWizard() string {
	w := m.wizard
	var b strings.Builder
	b.WriteString(m.styles.title(fmt.Sprintf("ogs — nouveau projet (%d/%d)", w.index+1, len(w.steps))))
	b.WriteString("\n\n")
	// Les réponses déjà données restent visibles : l'assistant doit se relire.
	for i := 0; i < w.index; i++ {
		b.WriteString(fmt.Sprintf("  %-24s %s\n",
			m.styles.label(w.steps[i].label), m.styles.value(w.steps[i].get(w.answers))))
	}
	s := w.current()
	b.WriteString("\n" + m.styles.title(s.label))
	if help := w.help(); help != "" {
		b.WriteString("  " + m.styles.help("("+help+")"))
	}
	b.WriteString("\n")
	if len(s.choices) > 0 {
		b.WriteString(m.styles.help("  choix : "+strings.Join(nonEmpty(s.choices), ", ")) + "\n")
	}
	current := s.get(w.answers)
	if current != "" {
		b.WriteString(m.styles.help("  défaut : "+current) + "\n")
	}
	b.WriteString("\n  > " + w.input + "▏\n")
	if w.err != "" {
		b.WriteString("\n  " + w.err + "\n")
	}
	b.WriteString("\n" + m.styles.help("⏎ valider  ⎋ abandonner"))
	return b.String()
}

// helpLine rend la barre d'aide.
func (m *Model) helpLine() string {
	return m.styles.help("↑↓ naviguer  d écarts  a appliquer  s redémarrer  c certificat  " +
		"n nouveau  r rafraîchir  / filtrer  q quitter")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
