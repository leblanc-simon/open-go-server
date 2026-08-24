package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	// Audit consigne le résultat de chaque étape.
	Audit func(step *Step, outcome string, detail string)
}

// ErrDryRun est renvoyé lorsque l'exécution a été simulée.
var ErrDryRun = errors.New("exécution simulée")

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
	// déclare en avoir besoin plus tôt : ceux déjà effectués sont alors retenus
	// pour ne pas être rejoués.
	pending := plan.Reloads()
	flush := func(ctx context.Context) {
		if opts.Reload == nil || len(pending) == 0 {
			return
		}
		reste := pending[:0:0]
		for _, r := range pending {
			if err := opts.Reload(ctx, r); err != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("rechargement %s : %v", r, err))
				continue
			}
			report.Reloaded = append(report.Reloaded, r)
		}
		pending = reste
	}

	var applied []*Step
	for _, s := range plan.Mutating() {
		if s.FlushReloads {
			flush(ctx)
		}
		if err := ctx.Err(); err != nil {
			report.Warnings = append(report.Warnings, "exécution interrompue : "+err.Error())
			rollback(context.WithoutCancel(ctx), applied, report, opts)
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
			if opts.Audit != nil {
				opts.Audit(s, "ok", "")
			}
		case !s.Critical:
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
			rollback(ctx, applied, report, opts)
			return report, fmt.Errorf("étape %s : %w", s.Resource, err)
		}
		report.Outcomes = append(report.Outcomes, outcome)
	}

	// Rechargements groupés, une fois chacun, après toutes les écritures.
	flush(ctx)
	return report, nil
}

// rollback compense les étapes appliquées, en ordre inverse. Si une compensation
// échoue, le déroulage s'arrête : l'état exact atteint est remonté plutôt que
// masqué par des tentatives supplémentaires.
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
		if err := s.Compensate(ctx); err != nil {
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
