package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"leblanc.io/open-go-server/internal/model"
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
	// Plan renvoie ce qu'Apply ferait — le plan du projet, puis celui du parc —,
	// ligne par ligne, pour être montré avant la confirmation ; aucune ligne,
	// rien à appliquer (D112).
	Plan(ctx context.Context, p *model.Project) ([]string, error)
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
	// loading compte les lectures en vol (inventaire, état, écarts) ; running
	// dit qu'une action est en vol. Un seul booléen pour les deux faisait
	// retomber l'indicateur à la première observation revenue, l'action
	// tournant encore.
	loading int
	running bool
	// ctx est le contexte de l'interface : celui de la commande, qu'une
	// interruption (SIGINT, SIGTERM) annule. cancelAction annule l'action en vol,
	// dont le contexte en dérive.
	ctx          context.Context
	cancel       context.CancelFunc
	cancelAction context.CancelFunc
	// interrupted dit qu'une action a été interrompue, ou qu'un signal est
	// arrivé : la commande rend alors le code 9, comme en ligne de commande.
	interrupted bool
	// cancelled dit que l'action en vol a été annulée : elle ne compte pour
	// interrompue que si elle échoue — un Ctrl+C arrivé après qu'elle a abouti
	// ne change rien à son résultat.
	cancelled bool
	// quitAfter dit qu'une sortie a été demandée pendant une action : elle
	// attend que l'action ait rendu la main (Q-101).
	quitAfter bool
	quitting  bool
	// gate tient l'action en vol : la fermeture du programme l'attend (D122).
	gate *actionGate
}

// actionGate fait attendre la fermeture de l'interface la fin de l'action en
// vol, et refuse ensuite d'en lancer une. Bubble Tea sort de Run dès que la
// lecture du terminal échoue — EIO, sur le terminal d'une session ssh coupée —,
// sans attendre ses commandes : la saga était abandonnée entre deux étapes,
// le processus sortant sous elle, ce que Q-101 avait fermé pour SIGTERM seul.
type actionGate struct {
	mu     sync.Mutex
	closed bool
}

// run exécute f, sauf si la porte est fermée ; il dit s'il l'a fait.
func (g *actionGate) run(f func()) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	f()
	return true
}

// close ferme la porte, une fois l'action en vol revenue.
func (g *actionGate) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

// confirmation porte une action à confirmer, et ce qu'elle fera : le plan
// d'un apply est montré avant la question, comme en ligne de commande.
type confirmation struct {
	question string
	lines    []string
	action   func(ctx context.Context) tea.Msg
}

// New construit le modèle, sans contexte d'interruption.
func New(backend Backend) *Model { return NewWithContext(context.Background(), backend) }

// NewWithContext construit le modèle sous le contexte de la commande : chaque
// lecture et chaque action en dérivent.
func NewWithContext(ctx context.Context, backend Backend) *Model {
	// Le contexte de l'interface est annulé à sa fermeture : une lecture en vol
	// (observation, écarts) tient la sérialisation du backend, et ne doit pas
	// survivre à l'interface qui l'attend.
	ctx, cancel := context.WithCancel(ctx)
	return &Model{
		cancel:    cancel,
		backend:   backend,
		styles:    NewStyles(),
		snapshots: map[string]Snapshot{},
		width:     100,
		height:    30,
		ctx:       ctx,
		gate:      &actionGate{},
	}
}

// read lance une lecture : elle est comptée jusqu'à son message de retour.
func (m *Model) read(cmd tea.Cmd) tea.Cmd {
	m.loading++
	return cmd
}

// done décompte une lecture revenue.
func (m *Model) done() {
	if m.loading > 0 {
		m.loading--
	}
}

// start lance une action, une seule à la fois : elle reçoit un contexte que
// Ctrl+C et SIGTERM annulent, et l'interface ne se ferme qu'une fois qu'elle a
// rendu la main. Abandonnée en plein vol, une saga laissait ses étapes à demi
// faites, sans déroulage ni rechargement, et le processus sortait sous les
// commandes qu'elle avait lancées.
func (m *Model) start(status string, action func(ctx context.Context) tea.Msg) tea.Cmd {
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancelAction = cancel
	m.running = true
	m.status = status
	return func() tea.Msg {
		defer cancel()
		var msg tea.Msg
		if !m.gate.run(func() { msg = action(ctx) }) {
			return outputMsg{err: context.Canceled}
		}
		return msg
	}
}

// interruptMsg porte une interruption reçue par le processus (SIGINT, SIGTERM) :
// le contexte de la commande est annulé.
type interruptMsg struct{}

// Interrupted dit que l'interface s'est fermée sur une interruption : une action
// annulée, ou un signal reçu.
func (m *Model) Interrupted() bool { return m.interrupted }

// interrupt arrête l'action et les lectures en vol, et ferme l'interface — une
// fois l'action revenue, s'il y en a une. signal dit que l'interruption vient
// du processus (SIGINT, SIGTERM) et non de la touche Ctrl+C.
func (m *Model) interrupt(signal bool) (tea.Model, tea.Cmd) {
	m.cancel()
	if m.running {
		m.cancelled = true
	} else if signal {
		m.interrupted = true
	}
	if !m.running {
		m.quitting = true
		return m, tea.Quit
	}
	m.quitAfter = true
	m.status = "interruption demandée : l'opération s'arrête proprement, l'interface se fermera ensuite…"
	return m, nil
}

// refuseWhileRunning dit, pendant une action, qu'il faut attendre sa fin.
func (m *Model) refuseWhileRunning() bool {
	if m.running {
		m.status = "une opération est en cours : attendre sa fin (Ctrl+C l'interrompt)"
	}
	return m.running
}

// Output rend la dernière sortie d'action, pour l'afficher après la fermeture.
func (m *Model) Output() []string { return m.output }

// Messages internes.
type (
	projectsLoadedMsg struct{ projects []*model.Project }
	snapshotMsg       struct {
		id       string
		snapshot Snapshot
	}
	diffMsg struct{ lines []string }
	planMsg struct {
		project *model.Project
		lines   []string
		err     error
	}
	outputMsg struct {
		lines []string
		err   error
	}
	errMsg struct{ err error }
)

// Init charge l'inventaire.
func (m *Model) Init() tea.Cmd { return m.read(m.loadProjects) }

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

	case interruptMsg:
		return m.interrupt(true)

	case projectsLoadedMsg:
		m.done()
		m.projects = msg.projects
		sort.Slice(m.projects, func(i, j int) bool { return m.projects[i].ID() < m.projects[j].ID() })
		m.err = nil
		return m, m.observeCurrent()

	case snapshotMsg:
		m.done()
		m.snapshots[msg.id] = msg.snapshot
		return m, nil

	case diffMsg:
		m.done()
		m.diff = msg.lines
		m.mode = modeDiff
		return m, nil

	case planMsg:
		m.done()
		// Une action lancée entre-temps, ou un autre écran, l'emporte : le plan
		// n'est plus celui qu'on allait confirmer.
		if m.running || m.mode != modeList {
			return m, nil
		}
		p := msg.project
		switch {
		case msg.err != nil:
			m.status = "plan de " + p.ID() + " incalculable : " + msg.err.Error()
			return m, nil
		case len(msg.lines) == 0:
			m.status = "aucun changement pour " + p.ID()
			return m, nil
		}
		// Toute action mutante passe par une confirmation, le plan sous les yeux :
		// la TUI n'est qu'un pilote du même moteur, sans chemin privilégié (D112).
		m.mode = modeConfirm
		m.confirm = confirmation{
			question: fmt.Sprintf("appliquer les changements de %s ?", p.ID()),
			lines:    msg.lines,
			action: func(ctx context.Context) tea.Msg {
				out, err := m.backend.Apply(ctx, p)
				return outputMsg{lines: strings.Split(strings.TrimRight(out, "\n"), "\n"), err: err}
			},
		}
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
		m.running = false
		m.cancelAction = nil
		// Une action annulée n'est interrompue que si elle a échoué : sinon elle
		// a abouti avant que l'annulation ne l'atteigne, et le code 0 dit vrai.
		if m.cancelled && msg.err != nil {
			m.interrupted = true
		}
		m.cancelled = false
		if m.quitAfter {
			m.quitting = true
			return m, tea.Quit
		}
		m.status = ""
		// Les états observés sont invalidés : ils datent d'avant l'action.
		m.snapshots = map[string]Snapshot{}
		return m, tea.Batch(m.read(m.loadProjects))

	case errMsg:
		m.done()
		m.err = msg.err
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
		case tea.KeyCtrlC:
			m.filtering = false
			return m.interrupt(false)
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
			m.mode = modeList
			return m, m.start("opération en cours…", m.confirm.action)
		default:
			m.mode = modeList
			m.status = "annulé"
			return m, nil
		}
	case modeDiff, modeOutput:
		switch key.String() {
		case "ctrl+c":
			return m.interrupt(false)
		case "q", "esc", "enter":
			m.mode = modeList
			return m, nil
		}
		return m, nil
	}

	switch key.String() {
	case "ctrl+c":
		return m.interrupt(false)
	case "q":
		// Pendant une action, « q » attend qu'elle s'achève : la sortie la
		// laissait en plein vol.
		if m.running {
			m.quitAfter = true
			m.status = "sortie à la fin de l'opération en cours… (Ctrl+C l'interrompt)"
			return m, nil
		}
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
		if m.refuseWhileRunning() {
			return m, nil
		}
		return m, m.read(m.loadProjects)
	case "d":
		p := m.current()
		if p == nil || m.refuseWhileRunning() {
			return m, nil
		}
		ctx := m.ctx
		return m, m.read(func() tea.Msg {
			lines, err := m.backend.Diff(ctx, p)
			if err != nil {
				return errMsg{err}
			}
			return diffMsg{lines}
		})
	case "a":
		p := m.current()
		if p == nil || m.refuseWhileRunning() {
			return m, nil
		}
		// Le plan d'abord, projet et parc : la confirmation le montre (D112).
		ctx := m.ctx
		m.status = "calcul du plan de " + p.ID() + "…"
		return m, m.read(func() tea.Msg {
			lines, err := m.backend.Plan(ctx, p)
			return planMsg{project: p, lines: lines, err: err}
		})
	case "s":
		p := m.current()
		if p == nil || m.refuseWhileRunning() {
			return m, nil
		}
		m.mode = modeConfirm
		m.confirm = confirmation{
			question: fmt.Sprintf("redémarrer les services de %s ?", p.ID()),
			action: func(ctx context.Context) tea.Msg {
				if err := m.backend.Restart(ctx, p); err != nil {
					return outputMsg{err: err}
				}
				return outputMsg{lines: []string{"services de " + p.ID() + " redémarrés"}}
			},
		}
		return m, nil
	case "c":
		p := m.current()
		if p == nil || m.refuseWhileRunning() {
			return m, nil
		}
		m.mode = modeConfirm
		m.confirm = confirmation{
			question: fmt.Sprintf("renouveler le certificat de %s ?", p.ID()),
			action: func(ctx context.Context) tea.Msg {
				if err := m.backend.RenewCertificate(ctx, p); err != nil {
					return outputMsg{err: err}
				}
				return outputMsg{lines: []string{"certificat de " + p.ID() + " renouvelé"}}
			},
		}
		return m, nil
	case "n":
		if m.refuseWhileRunning() {
			return m, nil
		}
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
	// Pendant une action, l'état observé daterait d'avant elle : il est relu
	// après, avec l'inventaire.
	if _, known := m.snapshots[p.ID()]; known || m.running {
		return nil
	}
	ctx := m.ctx
	return m.read(func() tea.Msg {
		snapshot, err := m.backend.Snapshot(ctx, p)
		if err != nil {
			return errMsg{err}
		}
		return snapshotMsg{id: p.ID(), snapshot: snapshot}
	})
}

// Quitting indique que le modèle a demandé la sortie.
func (m *Model) Quitting() bool { return m.quitting }

// Status expose le message d'état, pour les tests.
func (m *Model) Status() string { return m.status }
