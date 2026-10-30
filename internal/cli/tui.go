package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/hook"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/tls"
	"leblanc.io/open-go-server/internal/provider/unit"
	"leblanc.io/open-go-server/internal/spec"
	"leblanc.io/open-go-server/internal/tui"
)

func newTUICommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Ouvrir l'inspecteur en mode texte",
		Long: "Navigue dans le parc, affiche l'état de chaque projet et propose les actions\n" +
			"courantes. Toute action mutante demande confirmation et passe par le même\n" +
			"moteur que la ligne de commande : il n'y a pas de chemin privilégié.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !term.IsTerminal(int(os.Stdout.Fd())) {
				return exitf(CodeUsage, "l'interface exige un terminal : "+
					"employer « ogs status » ou « ogs project list --output json » dans un script")
			}
			if err := app.openStore(); err != nil {
				return err
			}
			app.tolerateFleet = true
			err := tui.Run(cmd.Context(), &tuiBackend{app: app, cmd: cmd})
			if errors.Is(err, tui.ErrInterrupted) {
				return exitf(CodeCancelled, "interruption reçue : l'action en cours a été annulée")
			}
			return wrap(CodeError, err)
		},
	}
}

// tuiBackend relie l'interface aux providers. L'interface ne connaît jamais un
// provider directement : elle reste ainsi éprouvable sans système.
//
// Ses appels sont sérialisés (mu) : chacun s'exécute dans sa propre goroutine,
// et tous partagent l'App — Apply y détournait Out et posait Yes pendant qu'une
// observation ou une seconde action les lisaient (Q-101).
type tuiBackend struct {
	app *App
	cmd *cobra.Command
	mu  sync.Mutex
	// shown retient, par projet, les plans que Plan a montrés.
	shown map[string]map[string]planKeys
}

// PrepDomain renvoie le domaine de préproduction configuré.
func (b *tuiBackend) PrepDomain() string { return b.app.Cfg.PrepDomain() }

func (b *tuiBackend) Projects() ([]*model.Project, []string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// L'inspecteur montre le parc lisible ; une déclaration refusée est rendue à
	// l'interface, qui la nomme à l'écran, et toute action repasse par
	// checkFleetUniqueness, qui refuse (D120). Rien n'est écrit sur la sortie :
	// Bubble Tea pilote l'écran, et une ligne écrite à côté le brouillait.
	list, refused, err := b.app.loadFleet()
	if err != nil {
		return nil, nil, err
	}
	lines := make([]string, len(refused))
	for i, err := range refused {
		lines[i] = err.Error()
	}
	return list, lines, nil
}

func (b *tuiBackend) Snapshot(ctx context.Context, p *model.Project) (tui.Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// L'affichage ne réserve aucun numéro (D100).
	b.app.Store.PreviewAllocations(true)
	defer b.app.Store.PreviewAllocations(false)
	provs, err := b.app.buildProviders()
	if err != nil {
		return tui.Snapshot{}, err
	}
	defer provs.close()
	snapshot := tui.Snapshot{Health: "sain"}
	plan, err := b.app.buildPlan(ctx, provs, p)
	if err != nil {
		snapshot.Health = "erreur"
		snapshot.Problems = []string{err.Error()}
		return snapshot, nil
	}
	snapshot.Drift = len(plan.Drift())
	if snapshot.Drift > 0 {
		snapshot.Health = "dérive"
	}
	if len(p.Spec.Services) > 0 {
		if obs, err := provs.Unit.Observe(ctx, p); err == nil {
			active, declared := 0, 0
			for _, svc := range p.Spec.Services {
				want := svc.Instances
				if want <= 0 {
					want = 1
				}
				declared += want
				active += obs.Instances[svc.Name]
			}
			snapshot.Services = fmt.Sprintf("%d/%d", active, declared)
			if active < declared {
				snapshot.Health = "services incomplets"
				snapshot.Problems = append(snapshot.Problems,
					fmt.Sprintf("%d instance(s) sur %d actives", active, declared))
			}
		}
	}
	if p.Spec.AMQP != nil {
		if obs, err := provs.AMQP.Observe(ctx, p); err == nil && obs.Unavailable == nil {
			total := 0
			for _, q := range obs.Queues {
				total += q.Messages
			}
			snapshot.Queues = fmt.Sprintf("%d file(s), %d message(s)", len(obs.Queues), total)
		}
	}
	if ref, ready := provs.TLS.CertificateFor(p); ref != "" {
		snapshot.Certificate = ref
		if !ready {
			état, problème := étatDuCertificat(provs, p, ref)
			snapshot.Problems = append(snapshot.Problems, problème)
			if snapshot.Health == "sain" {
				snapshot.Health = "TLS " + état
			}
		} else if info, ok, _ := provs.TLS.Store.Read(ref); ok {
			snapshot.CertDays = info.DaysLeft(nowFunc())
			if info.NeedsRenewal(nowFunc()) {
				snapshot.Problems = append(snapshot.Problems, "certificat à renouveler")
			}
		}
	}
	return snapshot, nil
}

// Plan calcule ce qu'Apply ferait : le plan du projet, puis celui du parc,
// sans rien réserver, et le retient pour qu'Apply n'exécute rien qui n'ait été
// montré (D112).
func (b *tuiBackend) Plan(ctx context.Context, p *model.Project) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.app.Store.PreviewAllocations(true)
	defer b.app.Store.PreviewAllocations(false)
	provs, err := b.app.buildProviders()
	if err != nil {
		return nil, err
	}
	defer provs.close()
	plan, err := b.app.buildPlan(ctx, provs, p)
	if err != nil {
		return nil, err
	}
	var lines []string
	if !plan.Empty() {
		lines = append(lines, "projet "+p.ID())
		lines = append(lines, strings.Split(strings.TrimRight(plan.String(), "\n"), "\n")...)
	}
	lines = append(lines, noteLines(plan)...)
	global, err := b.app.buildGlobalPlan(ctx, provs, []*model.Project{p})
	if err != nil {
		return nil, fmt.Errorf("plan du parc : %w", err)
	}
	if !global.Empty() {
		lines = append(lines, "parc")
		lines = append(lines, strings.Split(strings.TrimRight(global.String(), "\n"), "\n")...)
	}
	lines = append(lines, noteLines(global)...)
	if b.shown == nil {
		b.shown = map[string]map[string]planKeys{}
	}
	b.shown[p.ID()] = map[string]planKeys{p.ID(): keysOf(plan), "*": keysOf(global)}
	return lines, nil
}

// Apply passe par le chemin d'« ogs apply » (D112) : unicité contre le parc
// sous le verrou, hooks preApply et postApply, plan du parc, rechargements en
// échec affichés, postCreate au premier apply d'un projet que l'assistant a
// déclaré. La confirmation est celle de l'interface, donnée sur le plan que Plan
// a montré : ce qui s'y ajoute depuis est refusé, faute de pouvoir le demander.
func (b *tuiBackend) Apply(ctx context.Context, p *model.Project) (string, error) {
	// La sortie est capturée pour être rendue dans l'interface : l'utilisateur
	// doit voir exactement ce que la ligne de commande aurait affiché.
	b.mu.Lock()
	defer b.mu.Unlock()
	var buf bytes.Buffer
	a := b.app
	previousOut, previousErr := a.Out, a.Err
	previousYes, previousNI := a.Yes, a.NonInteractive
	a.Out, a.Err, a.Yes, a.NonInteractive = &buf, &buf, false, true
	defer func() {
		a.Out, a.Err, a.Yes, a.NonInteractive = previousOut, previousErr, previousYes, previousNI
		// Le plan du parc laisse la base en aperçu ; l'interface continue.
		a.Store.PreviewAllocations(false)
	}()
	err := a.applyFleet(ctx, []*model.Project{p}, applyRun{
		confirmed: true, shown: b.shown[p.ID()], audit: "tui.apply.",
	})
	delete(b.shown, p.ID())
	return buf.String(), err
}

func (b *tuiBackend) Diff(ctx context.Context, p *model.Project) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.app.Store.PreviewAllocations(true)
	defer b.app.Store.PreviewAllocations(false)
	provs, err := b.app.buildProviders()
	if err != nil {
		return nil, err
	}
	defer provs.close()
	plan, err := b.app.buildPlan(ctx, provs, p)
	if err != nil {
		return nil, err
	}
	lines := diffLines(plan)
	// Le parc aussi : c'est ce qu'Apply exécutera après le projet (D112).
	global, err := b.app.buildGlobalPlan(ctx, provs, []*model.Project{p})
	if err != nil {
		return nil, fmt.Errorf("plan du parc : %w", err)
	}
	if !global.Empty() {
		lines = append(lines, "", "parc")
		lines = append(lines, diffLines(global)...)
	}
	return lines, nil
}

// noteLines rend les informations d'un plan, hors décompte (D118), comme
// printNotes les affiche en ligne de commande.
func noteLines(plan *engine.Plan) []string {
	var lines []string
	for _, n := range plan.Notes {
		lines = append(lines, "  ℹ "+n)
	}
	return lines
}

// diffLines rend les changements d'un plan avec leur diff, ligne par ligne.
func diffLines(plan *engine.Plan) []string {
	var lines []string
	for _, s := range plan.Mutating() {
		lines = append(lines, s.String())
		if s.Diff == nil {
			continue
		}
		if detail, err := s.Diff(); err == nil && detail != "" {
			for _, l := range strings.Split(strings.TrimRight(detail, "\n"), "\n") {
				lines = append(lines, "    "+l)
			}
		}
	}
	return lines
}

func (b *tuiBackend) Restart(ctx context.Context, p *model.Project) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	provs, err := b.app.buildProviders()
	if err != nil {
		return err
	}
	defer provs.close()
	if err := provs.Unit.Control(ctx, p, "restart"); err != nil {
		return err
	}
	return b.app.Audit.Record(p.ID(), "unit/target", "tui.restart", "ok", "")
}

func (b *tuiBackend) RenewCertificate(ctx context.Context, p *model.Project) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	provs, err := b.app.buildProviders()
	if err != nil {
		return err
	}
	defer provs.close()
	requests, err := provs.TLS.Requests(p)
	if err != nil {
		return err
	}
	if len(requests) == 0 {
		if ref := provs.TLS.SharedRef(p); ref != "" {
			if req, ok := provs.TLS.SharedRequest(ref); ok {
				requests = []tls.Request{req}
			}
		}
	}
	// Les partagés que désignent ses vhosts se renouvellent avec lui (D102).
	requests = append(requests, provs.TLS.DesignatedShared(p)...)
	if len(requests) == 0 {
		return fmt.Errorf("le projet %s ne demande aucun certificat", p.ID())
	}
	done, errs := provs.TLS.Renew(ctx, requests, true)
	// Comme « ogs cert renew » : le serveur garde en mémoire le matériel qu'il a
	// chargé, et continuerait de présenter l'ancien certificat — y compris quand
	// le renouvellement s'est replié sur les noms du vhost principal.
	if len(done) > 0 {
		if err := provs.Web.Reload(ctx); err != nil {
			return fmt.Errorf("certificat renouvelé, mais le serveur web n'a pas été rechargé : %w", err)
		}
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return b.app.Audit.Record(p.ID(), "tls/certificate", "tui.cert.renew", "ok", "")
}

func (b *tuiBackend) Create(ctx context.Context, answers tui.WizardAnswers) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	opts := siteOptions{
		kind: answers.Kind, appVersion: answers.AppVersion, phpVersion: answers.PHPVersion,
		env: answers.Env, database: answers.Database, tls: true, apply: false,
	}
	// Les noms sont nettoyés une fois pour toutes : « async, high » nommait sinon
	// la file « high » précédée d'une espace, que le worker « high » ne consommait pas.
	for _, transport := range strings.Split(answers.Messenger, ",") {
		if transport = strings.TrimSpace(transport); transport != "" {
			opts.messenger = append(opts.messenger, transport)
		}
	}
	if len(opts.messenger) > 0 {
		instances := strings.TrimSpace(answers.Instances)
		if instances == "" {
			instances = "1"
		}
		// buildSite bascule ce service sur rabbitmq-consumer sous Symfony < 4.1.
		for _, transport := range opts.messenger {
			opts.services = append(opts.services,
				fmt.Sprintf("%s:%s:%s", unit.TemplateMessenger, transport, instances))
		}
	}
	p, err := b.app.buildSite(answers.Host, opts)
	if err != nil {
		return "", err
	}
	path := b.app.Paths.ProjectFile(p.ID())
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("le projet %s existe déjà", p.ID())
	}
	// Les gardes de « site create », dans le même ordre : unicité contre le parc
	// (D94), comptes réservés (D91), home tenu par un autre compte (D95), puis le
	// hook preCreate, qui peut refuser avant que rien ne soit écrit (D112).
	var buf bytes.Buffer
	a := b.app
	previousOut, previousErr := a.Out, a.Err
	a.Out, a.Err = &buf, &buf
	defer func() { a.Out, a.Err = previousOut, previousErr }()
	if err := a.checkFleetUniqueness(p); err != nil {
		return buf.String(), err
	}
	if err := a.refuseReservedDBAccounts(p); err != nil {
		return buf.String(), err
	}
	if err := a.preexistingHome(ctx, p); err != nil {
		return buf.String(), err
	}
	if _, err := a.runHook(ctx, a.hookRunner(), p, hook.PreCreate, hook.Context{}); err != nil {
		return buf.String(), err
	}
	if err := spec.Save(path, p); err != nil {
		return buf.String(), err
	}
	_ = a.Audit.Record(p.ID(), "site", "tui.site.create", "ok", path)
	// postCreate attend le premier apply réussi (D112).
	a.markDeclared(p)
	return buf.String() + fmt.Sprintf("projet %s déclaré : %s\n\nappuyer sur « a » pour l'appliquer", p.ID(), path), nil
}
