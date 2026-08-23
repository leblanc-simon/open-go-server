package cli

import (
	"context"
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/leviathan/ogs/internal/engine"
	"github.com/leviathan/ogs/internal/fs"
	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/spec"
	"github.com/leviathan/ogs/internal/store"
	"github.com/leviathan/ogs/internal/validate"
)

// buildPlan observe le système et calcule le plan d'un projet.
func (a *App) buildPlan(ctx context.Context, p *model.Project) (*engine.Plan, error) {
	plan := &engine.Plan{Project: p.ID()}
	prov := a.systemProvider()
	obs, err := prov.Observe(ctx, p)
	if err != nil {
		return nil, wrap(CodeError, err)
	}
	steps, err := prov.Plan(ctx, p, obs)
	if err != nil {
		return nil, wrap(CodeConflict, err)
	}
	for _, s := range steps {
		plan.Add(s)
	}
	return plan, nil
}

// targets résout les projets visés : ceux nommés, ou tous à défaut.
func (a *App) targets(args []string, file string) ([]*model.Project, error) {
	if file != "" {
		p, err := spec.Load(file)
		if err != nil {
			return nil, wrap(CodeUsage, err)
		}
		if err := a.prepare(p); err != nil {
			return nil, err
		}
		return []*model.Project{p}, nil
	}
	if len(args) == 0 {
		return a.loadProjects()
	}
	out := make([]*model.Project, 0, len(args))
	for _, id := range args {
		p, err := a.loadProject(id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// planStep est la vue machine d'une étape.
type planStep struct {
	Project  string `json:"project" yaml:"project"`
	Action   string `json:"action" yaml:"action"`
	Resource string `json:"resource" yaml:"resource"`
	Summary  string `json:"summary" yaml:"summary"`
	Critical bool   `json:"critical" yaml:"critical"`
}

func newPlanCommand(app *App) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "plan [id...]",
		Short: "Calculer les changements nécessaires, sans rien appliquer",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := app.targets(args, file)
			if err != nil {
				return err
			}
			if err := validate.Uniqueness(projects, app.Cfg.PrepDomain()); err != nil {
				return exitf(CodeConflict, "%v", err)
			}
			var items []planStep
			total := 0
			for _, p := range projects {
				plan, err := app.buildPlan(cmd.Context(), p)
				if err != nil {
					return err
				}
				for _, s := range plan.Mutating() {
					items = append(items, planStep{
						Project: p.ID(), Action: s.Action.String(), Resource: s.Resource,
						Summary: s.Summary, Critical: s.Critical,
					})
				}
				total += len(plan.Mutating())
				if app.Format == "table" {
					app.printf("projet %s\n%s", p.ID(), plan.String())
				}
			}
			if app.Format != "table" {
				return wrap(CodeError, app.emit("Plan", items, nil, nil))
			}
			app.printf("\n%d changement(s) au total.\n", total)
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "appliquer un fichier de projet précis")
	return cmd
}

func newDiffCommand(app *App) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "diff [id...]",
		Short: "Afficher l'écart entre l'état désiré et le système",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := app.targets(args, file)
			if err != nil {
				return err
			}
			drifted := 0
			for _, p := range projects {
				plan, err := app.buildPlan(cmd.Context(), p)
				if err != nil {
					return err
				}
				mutating := plan.Mutating()
				if len(mutating) == 0 {
					app.printf("= %s : conforme\n", p.ID())
					continue
				}
				drifted++
				app.printf("~ %s : %d écart(s)\n", p.ID(), len(mutating))
				for _, s := range mutating {
					app.printf("    %s\n", s.String())
					if s.Diff == nil {
						continue
					}
					d, err := s.Diff()
					if err != nil {
						app.printf("      (diff indisponible : %v)\n", err)
						continue
					}
					if d != "" {
						app.printf("%s\n", indent(d, "      "))
					}
				}
			}
			if drifted > 0 {
				// Code distinct : « diff » est utilisable en supervision, où l'écart
				// doit se lire sans analyser la sortie.
				return exitf(CodePartial, "%d projet(s) en écart", drifted)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "comparer un fichier de projet précis")
	return cmd
}

func newApplyCommand(app *App) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "apply [id...]",
		Short: "Appliquer l'état désiré au système",
		Long: "Calcule le plan, demande confirmation, puis applique en saga : une étape\n" +
			"critique en échec déclenche le déroulage des étapes déjà appliquées. Une étape\n" +
			"non critique en échec produit un avertissement et l'exécution continue.",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := app.targets(args, file)
			if err != nil {
				return err
			}
			if err := validate.Uniqueness(projects, app.Cfg.PrepDomain()); err != nil {
				return exitf(CodeConflict, "%v", err)
			}

			// Verrou global : deux applications simultanées ne doivent jamais se
			// croiser sur la configuration du système.
			lock, err := engine.Acquire(app.Paths.GlobalLock())
			if err != nil {
				var locked engine.ErrLocked
				if errors.As(err, &locked) {
					return wrap(CodeLocked, err)
				}
				return wrap(CodeError, err)
			}
			defer lock.Release()

			if err := app.openStore(); err != nil {
				return err
			}

			warnings := 0
			for _, p := range projects {
				plan, err := app.buildPlan(cmd.Context(), p)
				if err != nil {
					return err
				}
				if plan.Empty() {
					app.printf("= %s : aucun changement\n", p.ID())
					continue
				}
				app.printf("projet %s\n%s", p.ID(), plan.String())
				if app.DryRun {
					app.printf("--dry-run : rien n'a été appliqué.\n")
					continue
				}
				if err := app.confirm(fmt.Sprintf("appliquer %d changement(s) sur %s ?",
					len(plan.Mutating()), p.ID())); err != nil {
					return err
				}
				report, err := engine.Execute(cmd.Context(), plan, engine.Options{
					OnStep: func(s *engine.Step, stepErr error) {
						if stepErr == nil {
							app.printf("  ✓ %s\n", s.Summary)
						} else {
							app.printf("  ✗ %s : %v\n", s.Summary, stepErr)
						}
					},
					OnWarning: func(s *engine.Step, stepErr error) {
						app.printf("  ⚠ %s : %v\n", s.Summary, stepErr)
					},
					Audit: func(s *engine.Step, outcome, detail string) {
						_ = app.Audit.Record(p.ID(), s.Resource, "apply."+s.Action.String(), outcome, detail)
					},
				})
				warnings += len(report.Warnings)
				if err != nil {
					if report.RolledBack {
						app.printf("↶ rollback effectué : le système est revenu à son état initial.\n")
					}
					if report.RollbackErr != nil {
						app.printf("✗ le rollback a échoué : %v\n", report.RollbackErr)
						app.printf("  état à vérifier manuellement (audit %s)\n", app.Audit.RunID())
					}
					return wrap(CodeError, err)
				}
				if err := app.recordState(p); err != nil {
					return err
				}
				app.printf("✓ %s : %d étape(s) appliquée(s)", p.ID(), report.Applied())
				if len(report.Warnings) > 0 {
					app.printf(", %d avertissement(s)", len(report.Warnings))
				}
				app.printf(" (audit %s)\n", app.Audit.RunID())
			}
			if warnings > 0 {
				return exitf(CodePartial, "terminé avec %d avertissement(s)", warnings)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "appliquer un fichier de projet précis")
	return cmd
}

// recordState met à jour l'état observé après une application réussie.
func (a *App) recordState(p *model.Project) error {
	data, err := spec.Encode(p)
	if err != nil {
		return wrap(CodeError, err)
	}
	row := store.ProjectRow{
		ID: p.ID(), Host: p.Metadata.Host, Env: p.Metadata.Env,
		EffectiveHost: effectiveHostOf(p), Home: p.Spec.Layout.Root,
		AppKind: string(p.Spec.App.Kind), AppVersion: p.Spec.App.Version,
		SpecHash: fs.Fingerprint(data),
	}
	return wrap(CodeError, a.Store.UpsertProject(row))
}

func effectiveHostOf(p *model.Project) string {
	for _, d := range p.Spec.Domains {
		if d.Role == "" || d.Role == "primary" {
			return d.Name
		}
	}
	return p.Metadata.Host
}

func indent(s, prefix string) string {
	out := ""
	for _, line := range splitLines(s) {
		out += prefix + line + "\n"
	}
	return trimTrailingNewline(out)
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func trimTrailingNewline(s string) string {
	for len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	return s
}

var _ = tabwriter.NewWriter
