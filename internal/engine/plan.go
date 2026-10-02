package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Plan est la liste ordonnée des étapes calculées pour un projet.
type Plan struct {
	Project string
	Steps   []*Step
}

// Add ajoute une étape non nulle au plan.
func (p *Plan) Add(s *Step) {
	if s != nil {
		p.Steps = append(p.Steps, s)
	}
}

// Mutating renvoie les seules étapes qui changent quelque chose.
func (p *Plan) Mutating() []*Step {
	out := make([]*Step, 0, len(p.Steps))
	for _, s := range p.Steps {
		if s.Mutates() {
			out = append(out, s)
		}
	}
	return out
}

// Drift renvoie les étapes qui corrigent un écart du système : les étapes
// mutantes, hors tenue de l'état d'ogs (Step.Bookkeeping).
func (p *Plan) Drift() []*Step {
	out := make([]*Step, 0, len(p.Steps))
	for _, s := range p.Mutating() {
		if !s.Bookkeeping {
			out = append(out, s)
		}
	}
	return out
}

// Empty indique qu'aucun changement n'est nécessaire : c'est le résultat attendu
// d'un apply rejoué sur un système déjà conforme.
func (p *Plan) Empty() bool { return len(p.Mutating()) == 0 }

// Reloads renvoie les intentions de rechargement dédupliquées : cent projets
// rotés ne doivent provoquer qu'un rechargement du serveur web, pas cent.
func (p *Plan) Reloads() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range p.Steps {
		if !s.Mutates() {
			continue
		}
		for _, r := range s.Reload {
			if !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	sort.Strings(out)
	return out
}

// String rend le plan lisible.
func (p *Plan) String() string {
	var b strings.Builder
	for _, s := range p.Steps {
		if !s.Mutates() {
			continue
		}
		b.WriteString("  " + s.String())
		if !s.Critical {
			b.WriteString("  (non bloquant)")
		}
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return "  aucun changement\n"
	}
	return b.String()
}

// StepOutcome est le résultat d'une étape appliquée.
type StepOutcome struct {
	Step        *Step
	Err         error
	Compensated bool
	Skipped     bool
}

// Report est le compte rendu d'une application.
type Report struct {
	Project     string
	Outcomes    []StepOutcome
	Warnings    []string
	RolledBack  bool
	Reloaded    []string
	RollbackErr error
}

// Applied compte les étapes réellement appliquées.
func (r *Report) Applied() int {
	n := 0
	for _, o := range r.Outcomes {
		if o.Err == nil && !o.Skipped {
			n++
		}
	}
	return n
}

// Failed renvoie la première étape critique en échec.
func (r *Report) Failed() *StepOutcome {
	for i := range r.Outcomes {
		if r.Outcomes[i].Err != nil && r.Outcomes[i].Step.Critical {
			return &r.Outcomes[i]
		}
	}
	return nil
}

// Options paramètre l'exécution.
type Options struct {
	// DryRun n'applique rien : le plan et les diffs sont calculés, sans écriture.
	DryRun bool
	// OnStep est appelé avant puis après chaque étape, pour l'affichage.
	OnStep func(*Step, error)
	// OnWarning remonte un avertissement d'étape non critique.
	OnWarning func(*Step, error)
	// Reload exécute une intention de rechargement en fin d'application.
	Reload func(context.Context, string) error
	// OnReloadWarning remonte un rechargement en échec. Il n'est rattaché à
	// aucune étape, donc OnWarning ne le voit pas : sans ce rappel, l'exploitant
	// lisait « 1 avertissement(s) » sans que rien ne dise lequel.
	OnReloadWarning func(target string, err error)
	// Audit consigne le résultat de chaque étape.
	Audit func(step *Step, outcome string, detail string)
}

// ErrDryRun est renvoyé lorsque l'exécution a été simulée.
var ErrDryRun = errors.New("exécution simulée")

// PartialError marque l'aboutissement partiel d'une étape non critique : elle a
// produit un effet, mais pas tout ce qu'elle visait. L'exécution la remonte en
// avertissement, comme un échec, et retient pourtant ses intentions de
// rechargement — un certificat émis pour une partie de ses noms remplace le
// matériel que le serveur garde en mémoire, et sans rechargement il continuerait
// de présenter l'ancien, jusqu'à son expiration.
//
// Elle n'est pas pour autant comptée parmi les étapes à compenser : défaire ce
// qu'elle a produit, sur l'échec d'une étape critique plus loin, retirerait un
// matériel que d'autres étapes ont pu déjà désigner.
type PartialError struct{ Err error }

func (e *PartialError) Error() string { return e.Err.Error() }
func (e *PartialError) Unwrap() error { return e.Err }

// Partial enveloppe l'erreur d'une étape qui n'a abouti qu'en partie.
func Partial(err error) error {
	if err == nil {
		return nil
	}
	return &PartialError{Err: err}
}

// Execute applique le plan en saga : les étapes sont jouées dans l'ordre et,
// si une étape critique échoue, les étapes déjà appliquées sont compensées en
// ordre inverse. Une étape non critique en échec produit un avertissement et
// l'exécution continue : un certificat qui échoue ne doit pas détruire un site
// par ailleurs fonctionnel (D16).
func Execute(ctx context.Context, plan *Plan, opts Options) (*Report, error) {
	report := &Report{Project: plan.Project}
	if opts.DryRun {
		for _, s := range plan.Mutating() {
			report.Outcomes = append(report.Outcomes, StepOutcome{Step: s, Skipped: true})
		}
		return report, nil
	}

	// Les rechargements sont groupés en fin d'exécution, sauf lorsqu'une étape
	// déclare en avoir besoin plus tôt.
	//
	// Les intentions s'accumulent au fil des étapes appliquées, et non d'avance :
	// une étape qui vide la file ne doit pas consommer l'intention d'une étape qui
	// n'a pas encore eu lieu. C'est le cas du lien d'activation d'une instance
	// systemd, créé après le rechargement dont le démarrage avait besoin, et qui
	// exige donc un second rechargement pour être vu.
	var pending []string
	attendre := func(intents []string) {
		for _, r := range intents {
			deja := false
			for _, p := range pending {
				if p == r {
					deja = true
					break
				}
			}
			if !deja {
				pending = append(pending, r)
			}
		}
	}
	flush := func(ctx context.Context) {
		if opts.Reload == nil || len(pending) == 0 {
			return
		}
		aFaire := pending
		pending = nil
		for _, r := range aFaire {
			if err := opts.Reload(ctx, r); err != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("rechargement %s : %v", r, err))
				if opts.OnReloadWarning != nil {
					opts.OnReloadWarning(r, err)
				}
				continue
			}
			report.Reloaded = append(report.Reloaded, r)
		}
	}

	var applied []*Step
	for _, s := range plan.Mutating() {
		if s.FlushReloads {
			flush(ctx)
		}
		if err := ctx.Err(); err != nil {
			report.Warnings = append(report.Warnings, "exécution interrompue : "+err.Error())
			compenser(ctx, applied, report, opts)
			return report, err
		}
		var err error
		if s.Apply != nil {
			err = s.Apply(ctx)
		}
		if opts.OnStep != nil {
			opts.OnStep(s, err)
		}
		outcome := StepOutcome{Step: s, Err: err}
		switch {
		case err == nil:
			applied = append(applied, s)
			attendre(s.Reload)
			if opts.Audit != nil {
				opts.Audit(s, "ok", "")
			}
		case !s.Critical:
			if partial := (*PartialError)(nil); errors.As(err, &partial) {
				attendre(s.Reload)
			}
			report.Warnings = append(report.Warnings, fmt.Sprintf("%s : %v", s.Resource, err))
			if opts.OnWarning != nil {
				opts.OnWarning(s, err)
			}
			if opts.Audit != nil {
				opts.Audit(s, "avertissement", err.Error())
			}
		default:
			if opts.Audit != nil {
				opts.Audit(s, "échec", err.Error())
			}
			report.Outcomes = append(report.Outcomes, outcome)
			compenser(ctx, applied, report, opts)
			return report, fmt.Errorf("étape %s : %w", s.Resource, err)
		}
		report.Outcomes = append(report.Outcomes, outcome)
	}

	// Rechargements groupés, une fois chacun, après toutes les écritures.
	flush(ctx)
	return report, nil
}

// délaiCompensation borne CHAQUE compensation : une compensation bloquée ne doit
// pas figer le processus après un Ctrl-C, où l'opérateur attend justement la
// main. La borne est par compensation et non pour le déroulage entier — un
// budget global expirerait en cours de route et priverait de leur tour les
// dernières compensées, qui sont les étapes système : les premières appliquées
// sont les dernières compensées, tandis qu'un « systemctl stop » lent, appliqué
// tard donc compensé tôt, consomme jusqu'à TimeoutStopSec par instance. Compte,
// groupe et base resteraient en place sur une saga longue.
const délaiCompensation = 2 * time.Minute

// délaiDéroulage plafonne malgré tout l'attente totale, sans quoi N compensations
// lentes multiplieraient le délai par N et l'opérateur ne reprendrait pas la main.
// Il est assez large pour qu'un déroulage ordinaire aille à son terme.
const délaiDéroulage = 15 * time.Minute

// compenser déroule les compensations sur un contexte détaché de celui de
// l'apply, et borné.
//
// L'interruption survient bien plus souvent pendant une étape qu'entre deux :
// le contexte est alors déjà annulé quand l'étape rend son erreur, et
// osexec.CommandContext refuse de démarrer le moindre processus sur un contexte
// terminé — groupdel, userdel ou la suppression d'une base échoueraient sans
// avoir rien fait, laissant compte, groupe et base en place alors que le rapport
// annonce un déroulage. Détacher inconditionnellement est le seul remède fiable :
// toutes les erreurs d'annulation ne remontent pas telles quelles, une commande
// tuée en SIGKILL rendant un *ExitError sans trace du contexte.
func compenser(ctx context.Context, applied []*Step, report *Report, opts Options) {
	ctx, annuler := context.WithTimeout(context.WithoutCancel(ctx), délaiDéroulage)
	defer annuler()
	rollback(ctx, applied, report, opts)
}

// rollback compense les étapes appliquées, en ordre inverse. Chacune reçoit son
// propre délai, pris sur celui du déroulage. Si une compensation échoue, le
// déroulage s'arrête : l'état exact atteint est remonté plutôt que masqué par des
// tentatives supplémentaires.
func rollback(ctx context.Context, applied []*Step, report *Report, opts Options) {
	if len(applied) == 0 {
		return
	}
	report.RolledBack = true
	for i := len(applied) - 1; i >= 0; i-- {
		s := applied[i]
		if s.Compensate == nil {
			continue
		}
		étape, annuler := context.WithTimeout(ctx, délaiCompensation)
		err := s.Compensate(étape)
		annuler()
		if err != nil {
			report.RollbackErr = fmt.Errorf("compensation de %s : %w", s.Resource, err)
			if opts.Audit != nil {
				opts.Audit(s, "compensation en échec", err.Error())
			}
			return
		}
		for j := range report.Outcomes {
			if report.Outcomes[j].Step == s {
				report.Outcomes[j].Compensated = true
			}
		}
		if opts.Audit != nil {
			opts.Audit(s, "compensé", "")
		}
	}
}
