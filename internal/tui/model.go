package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/leviathan/ogs/internal/model"
)

// Backend est ce que la TUI attend de l'application : elle n'appelle jamais un
// provider directement, ce qui la rend éprouvable sans système ni terminal.
type Backend interface {
	// Projects charge l'inventaire.
	Projects() ([]*model.Project, error)
	// Snapshot rassemble l'état observé d'un projet.
	Snapshot(ctx context.Context, p *model.Project) (Snapshot, error)
	// Apply réconcilie un projet et renvoie un compte rendu.
	Apply(ctx context.Context, p *model.Project) (string, error)
	// Diff renvoie les écarts d'un projet, ligne par ligne.
	Diff(ctx context.Context, p *model.Project) ([]string, error)
	// Restart redémarre les services d'un projet.
	Restart(ctx context.Context, p *model.Project) error
	// RenewCertificate renouvelle le certificat d'un projet.
	RenewCertificate(ctx context.Context, p *model.Project) error
	// Create déclare un projet depuis les réponses de l'assistant.
	Create(ctx context.Context, answers WizardAnswers) (string, error)
	// PrepDomain renvoie le domaine de préproduction configuré, dont l'assistant a
	// besoin pour annoncer le host qui sera réellement servi.
	PrepDomain() string
}

// Snapshot est l'état d'un projet tel que la TUI l'affiche.
type Snapshot struct {
	Drift       int
	Services    string
	Certificate string
	CertDays    int
	Queues      string
	Health      string
	Problems    []string
}

// mode est l'écran courant.
type mode int

const (
	modeList mode = iota
	modeDiff
	modeOutput
	modeWizard
	modeConfirm
)

// Model est le modèle Bubble Tea de l'inspecteur.
type Model struct {
	backend  Backend
	styles   Styles
	projects []*model.Project
	// snapshots est renseigné à la demande : observer tout le parc au démarrage
	// rendrait l'ouverture lente sur un serveur chargé.
	snapshots map[string]Snapshot
	cursor    int
	filter    string
	filtering bool
	mode      mode
	width     int
	height    int
	status    string
	err       error
	output    []string
	diff      []string
	wizard    wizard
	confirm   confirmation
	busy      bool
	quitting  bool
}

// confirmation porte une action à confirmer.
type confirmation struct {
	question string
	action   func() tea.Msg
}

// New construit le modèle.
func New(backend Backend) *Model {
	return &Model{
		backend:   backend,
		styles:    NewStyles(),
		snapshots: map[string]Snapshot{},
		width:     100,
		height:    30,
	}
}

// Messages internes.
type (
	projectsLoadedMsg struct{ projects []*model.Project }
	snapshotMsg       struct {
		id       string
		snapshot Snapshot
	}
	diffMsg   struct{ lines []string }
	outputMsg struct {
		lines []string
		err   error
	}
	errMsg struct{ err error }
)

// Init charge l'inventaire.
func (m *Model) Init() tea.Cmd { return m.loadProjects }

func (m *Model) loadProjects() tea.Msg {
	projects, err := m.backend.Projects()
	if err != nil {
		return errMsg{err}
	}
	return projectsLoadedMsg{projects}
}

// visible renvoie les projets correspondant au filtre.
func (m *Model) visible() []*model.Project {
	if m.filter == "" {
		return m.projects
	}
	needle := strings.ToLower(m.filter)
	var out []*model.Project
	for _, p := range m.projects {
		if strings.Contains(strings.ToLower(p.ID()), needle) ||
			strings.Contains(strings.ToLower(p.Metadata.Host), needle) {
			out = append(out, p)
		}
	}
	return out
}

// current renvoie le projet sélectionné.
func (m *Model) current() *model.Project {
	list := m.visible()
	if len(list) == 0 {
		return nil
	}
	if m.cursor >= len(list) {
		m.cursor = len(list) - 1
	}
	return list[m.cursor]
}

// Update traite les messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case projectsLoadedMsg:
		m.projects = msg.projects
		sort.Slice(m.projects, func(i, j int) bool { return m.projects[i].ID() < m.projects[j].ID() })
		m.err = nil
		return m, m.observeCurrent()

	case snapshotMsg:
		m.snapshots[msg.id] = msg.snapshot
		m.busy = false
		return m, nil

	case diffMsg:
		m.diff = msg.lines
		m.mode = modeDiff
		m.busy = false
		return m, nil

	case outputMsg:
		// L'erreur d'action est jointe à la sortie plutôt que placée dans l'état
		// global : le rechargement de l'inventaire qui suit remet celui-ci à zéro
		// et effacerait le message.
		m.output = msg.lines
		if msg.err != nil {
			m.output = append(m.output, "", "erreur : "+msg.err.Error())
		}
		m.mode = modeOutput
		m.busy = false
		// Les états observés sont invalidés : ils datent d'avant l'action.
		m.snapshots = map[string]Snapshot{}
		return m, tea.Batch(m.loadProjects)

	case errMsg:
		m.err = msg.err
		m.busy = false
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey route les touches selon l'écran courant.
func (m *Model) handleKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.filtering {
		switch key.Type {
		case tea.KeyEnter:
			m.filtering = false
			return m, m.observeCurrent()
		case tea.KeyEsc:
			m.filtering = false
			m.filter = ""
			return m, nil
		case tea.KeyBackspace:
			if m.filter != "" {
				m.filter = m.filter[:len(m.filter)-1]
			}
			return m, nil
		case tea.KeyRunes:
			m.filter += string(key.Runes)
			m.cursor = 0
			return m, nil
		}
		return m, nil
	}

	switch m.mode {
	case modeWizard:
		return m.wizardKey(key)
	case modeConfirm:
		switch strings.ToLower(key.String()) {
		case "o", "y", "enter":
			action := m.confirm.action
			m.mode = modeList
			m.busy = true
			m.status = "opération en cours…"
			return m, func() tea.Msg { return action() }
		default:
			m.mode = modeList
			m.status = "annulé"
			return m, nil
		}
	case modeDiff, modeOutput:
		switch key.String() {
		case "q", "esc", "enter":
			m.mode = modeList
			return m, nil
		}
		return m, nil
	}

	switch key.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			return m, m.observeCurrent()
		}
	case "down", "j":
		if m.cursor < len(m.visible())-1 {
			m.cursor++
			return m, m.observeCurrent()
		}
	case "/":
		m.filtering = true
		m.filter = ""
		return m, nil
	case "r":
		return m, m.loadProjects
	case "d":
		p := m.current()
		if p == nil {
			return m, nil
		}
		m.busy = true
		return m, func() tea.Msg {
			lines, err := m.backend.Diff(context.Background(), p)
			if err != nil {
				return errMsg{err}
			}
			return diffMsg{lines}
		}
	case "a":
		p := m.current()
		if p == nil {
			return m, nil
		}
		// Toute action mutante passe par une confirmation : la TUI n'est qu'un
		// pilote du même moteur, sans chemin privilégié.
		m.mode = modeConfirm
		m.confirm = confirmation{
			question: fmt.Sprintf("appliquer les changements de %s ?", p.ID()),
			action: func() tea.Msg {
				out, err := m.backend.Apply(context.Background(), p)
				return outputMsg{lines: strings.Split(strings.TrimRight(out, "\n"), "\n"), err: err}
			},
		}
		return m, nil
	case "s":
		p := m.current()
		if p == nil {
			return m, nil
		}
		m.mode = modeConfirm
		m.confirm = confirmation{
			question: fmt.Sprintf("redémarrer les services de %s ?", p.ID()),
			action: func() tea.Msg {
				if err := m.backend.Restart(context.Background(), p); err != nil {
					return outputMsg{err: err}
				}
				return outputMsg{lines: []string{"services de " + p.ID() + " redémarrés"}}
			},
		}
		return m, nil
	case "c":
		p := m.current()
		if p == nil {
			return m, nil
		}
		m.mode = modeConfirm
		m.confirm = confirmation{
			question: fmt.Sprintf("renouveler le certificat de %s ?", p.ID()),
			action: func() tea.Msg {
				if err := m.backend.RenewCertificate(context.Background(), p); err != nil {
					return outputMsg{err: err}
				}
				return outputMsg{lines: []string{"certificat de " + p.ID() + " renouvelé"}}
			},
		}
		return m, nil
	case "n":
		m.wizard = newWizard(m.backend.PrepDomain())
		m.mode = modeWizard
		return m, nil
	}
	return m, nil
}

// observeCurrent charge l'état du projet sélectionné, s'il n'est pas déjà connu.
func (m *Model) observeCurrent() tea.Cmd {
	p := m.current()
	if p == nil {
		return nil
	}
	if _, known := m.snapshots[p.ID()]; known {
		return nil
	}
	m.busy = true
	return func() tea.Msg {
		snapshot, err := m.backend.Snapshot(context.Background(), p)
		if err != nil {
			return errMsg{err}
		}
		return snapshotMsg{id: p.ID(), snapshot: snapshot}
	}
}

// Quitting indique que le modèle a demandé la sortie.
func (m *Model) Quitting() bool { return m.quitting }

// Status expose le message d'état, pour les tests.
func (m *Model) Status() string { return m.status }
