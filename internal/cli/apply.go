package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/leviathan/ogs/internal/config"
	"github.com/leviathan/ogs/internal/engine"
	"github.com/leviathan/ogs/internal/exec"
	"github.com/leviathan/ogs/internal/fs"
	"github.com/leviathan/ogs/internal/hook"
	"github.com/leviathan/ogs/internal/model"
	appprov "github.com/leviathan/ogs/internal/provider/app"
	"github.com/leviathan/ogs/internal/spec"
	"github.com/leviathan/ogs/internal/store"
	"github.com/leviathan/ogs/internal/template"
	"github.com/leviathan/ogs/internal/validate"
)

// buildPlan observe le système et calcule le plan d'un projet. L'ordre des
// providers suit celui des dépendances : utilisateur et arborescence, puis pools
// PHP, puis vhosts — un vhost référence un socket, qui référence un pool, qui
// tourne sous un utilisateur.
func (a *App) buildPlan(ctx context.Context, provs *providers, p *model.Project) (*engine.Plan, error) {
	plan := &engine.Plan{Project: p.ID()}

	sysObs, err := provs.System.Observe(ctx, p)
	if err != nil {
		return nil, wrap(CodeError, err)
	}
	sysSteps, err := provs.System.Plan(ctx, p, sysObs)
	if err != nil {
		return nil, wrap(CodeConflict, err)
	}
	for _, s := range sysSteps {
		plan.Add(s)
	}

	// Les bases viennent après l'utilisateur et avant les pools : une application
	// n'a pas besoin de sa base pour que son pool existe, mais l'ordre du cahier
	// des charges place les données avant l'exécution.
	if len(p.Spec.Databases) > 0 {
		dbObs, err := provs.DB.Observe(ctx, p)
		if err != nil {
			return nil, wrap(CodeError, err)
		}
		dbSteps, err := provs.DB.Plan(ctx, p, dbObs)
		if err != nil {
			return nil, wrap(CodeValidation, err)
		}
		for _, s := range dbSteps {
			plan.Add(s)
		}
	}

	if p.Spec.AMQP != nil {
		amqpObs, err := provs.AMQP.Observe(ctx, p)
		if err != nil {
			return nil, wrap(CodeError, err)
		}
		amqpSteps, err := provs.AMQP.Plan(ctx, p, amqpObs)
		if err != nil {
			return nil, wrap(CodeValidation, err)
		}
		for _, s := range amqpSteps {
			plan.Add(s)
		}
	}

	at, err := p.AppType()
	if err != nil {
		return nil, exitf(CodeValidation, "%s", err)
	}
	var stale []*engine.Step
	if at.NeedsPHP {
		phpObs, err := provs.PHP.Observe(ctx, p)
		if err != nil {
			return nil, wrap(CodeError, err)
		}
		phpSteps, err := provs.PHP.Plan(ctx, p, phpObs)
		if err != nil {
			return nil, wrap(CodeDependency, err)
		}
		for _, s := range phpSteps {
			plan.Add(s)
		}
		// Les suppressions de pools sont différées après la réécriture des vhosts.
		stale = provs.PHP.PlanStale(ctx, p, phpObs)
	}

	// Application non-PHP : runtime, unité et attente de disponibilité. Elle
	// précède le vhost de proxy, qui exposerait sinon une erreur 502.
	if appprov.Applies(p) {
		appObs, err := provs.App.Observe(ctx, p)
		if err != nil {
			return nil, wrap(CodeError, err)
		}
		appSteps, err := provs.App.Plan(ctx, p, appObs)
		if err != nil {
			return nil, wrap(CodeValidation, err)
		}
		for _, s := range appSteps {
			plan.Add(s)
		}
	}

	if len(p.Spec.Domains) > 0 {
		webObs, err := provs.Web.Observe(ctx, p)
		if err != nil {
			return nil, wrap(CodeError, err)
		}
		webSteps, err := provs.Web.Plan(ctx, p, webObs)
		if err != nil {
			return nil, wrap(CodeConflict, err)
		}
		for _, s := range webSteps {
			plan.Add(s)
		}

		// Les certificats viennent après le vhost HTTP : le challenge HTTP-01
		// exige que /.well-known/acme-challenge réponde déjà. Puis le vhost est
		// basculé en TLS (§6.2, étapes 7 à 9).
		certSteps, err := provs.TLS.Plan(ctx, p)
		if err != nil {
			return nil, wrap(CodeValidation, err)
		}
		for _, s := range certSteps {
			plan.Add(s)
		}
		for _, s := range provs.Web.PlanTLS(ctx, p) {
			plan.Add(s)
		}
	}
	for _, s := range stale {
		plan.Add(s)
	}

	// Tâches planifiées et services : ils viennent après le site, puisqu'ils
	// s'appuient sur son arborescence et son interpréteur.
	cronSteps, err := provs.Cron.Plan(ctx, p)
	if err != nil {
		return nil, wrap(CodeValidation, err)
	}
	for _, s := range cronSteps {
		plan.Add(s)
	}

	unitObs, err := provs.Unit.Observe(ctx, p)
	if err != nil {
		return nil, wrap(CodeError, err)
	}
	unitSteps, err := provs.Unit.Plan(ctx, p, unitObs)
	if err != nil {
		return nil, wrap(CodeValidation, err)
	}
	for _, s := range unitSteps {
		plan.Add(s)
	}

	// La délégation est écrite en dernier : en style systemctl, elle énumère les
	// instances, dont le nombre vient d'être arrêté.
	if len(p.Spec.Services) > 0 || p.Spec.Sudo != nil {
		sudoStep, err := provs.Sudoers.Plan(ctx, p, unitObs.Instances)
		if err != nil {
			return nil, wrap(CodeValidation, err)
		}
		plan.Add(sudoStep)
	}
	return plan, nil
}

// buildGlobalPlan calcule les étapes qui portent sur l'ensemble du parc.
func (a *App) buildGlobalPlan(provs *providers, projects []*model.Project) (*engine.Plan, error) {
	plan := &engine.Plan{Project: "*"}
	step, err := provs.Logrotate.Plan(projects, provs.Engine.Name(), provs.Engine.Unit())
	if err != nil {
		return nil, wrap(CodeError, err)
	}
	plan.Add(step)

	// Répertoires d'exécution : verrous de tâches et sockets applicatifs.
	// Les réglages de portée serveur du moteur web : les vhosts de proxy en
	// dépendent, et le fichier ne relève d'aucun projet en particulier.
	snippet, err := provs.Web.PlanSnippet()
	if err != nil {
		return nil, wrap(CodeError, err)
	}
	plan.Add(snippet)

	runtimeStep, err := provs.Runtime.Plan(projects)
	if err != nil {
		return nil, wrap(CodeError, err)
	}
	plan.Add(runtimeStep)

	// Timer de renouvellement : ressource de portée serveur, installée dès qu'un
	// certificat est en jeu.
	binary, err := os.Executable()
	if err != nil || a.Prefix != "" {
		binary = "/usr/bin/ogs"
	}
	units, err := provs.TLS.RenewUnitPlan(template.New(a.Paths.TemplatesDir()), binary, "")
	if err != nil {
		return nil, wrap(CodeError, err)
	}
	for _, s := range units {
		plan.Add(s)
	}
	return plan, nil
}

// reloadFunc route les intentions de rechargement vers le bon provider. Elles
// sont dédupliquées par le moteur : un seul rechargement par cible, en fin
// d'application.
func (a *App) reloadFunc(provs *providers) func(context.Context, string) error {
	return func(ctx context.Context, target string) error {
		switch {
		case strings.HasPrefix(target, "php:"):
			version := strings.TrimPrefix(target, "php:")
			a.printf("  ↻ rechargement de %s\n", config.PHPUnit(version))
			return provs.PHP.Reload(ctx, version)
		case strings.HasPrefix(target, "web:"):
			a.printf("  ↻ rechargement de %s\n", provs.Engine.Unit())
			return provs.Web.Reload(ctx)
		case target == "systemd:daemon-reload":
			a.printf("  ↻ systemctl daemon-reload\n")
			return a.systemctl(ctx, "daemon-reload")
		case target == "systemd:tmpfiles":
			a.printf("  ↻ systemd-tmpfiles --create\n")
			return a.systemctlBin(ctx, "systemd-tmpfiles", "--create")
		case strings.HasPrefix(target, "systemd:enable:"):
			unit := strings.TrimPrefix(target, "systemd:enable:")
			a.printf("  ↻ activation de %s\n", unit)
			return a.systemctl(ctx, "enable", "--now", unit)
		}
		return fmt.Errorf("cible de rechargement inconnue : %s", target)
	}
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
			if err := app.openStore(); err != nil {
				return err
			}
			provs, err := app.buildProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			var items []planStep
			total := 0
			for _, p := range projects {
				plan, err := app.buildPlan(cmd.Context(), provs, p)
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
			global, err := app.buildGlobalPlan(provs, projects)
			if err != nil {
				return err
			}
			for _, s := range global.Mutating() {
				items = append(items, planStep{
					Project: "*", Action: s.Action.String(), Resource: s.Resource,
					Summary: s.Summary, Critical: s.Critical,
				})
			}
			if len(global.Mutating()) > 0 {
				total += len(global.Mutating())
				if app.Format == "table" {
					app.printf("parc\n%s", global.String())
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
			if err := app.openStore(); err != nil {
				return err
			}
			provs, err := app.buildProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			drifted := 0
			for _, p := range projects {
				plan, err := app.buildPlan(cmd.Context(), provs, p)
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
			provs, err := app.buildProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			reload := app.reloadFunc(provs)
			warnings := 0
			for _, p := range projects {
				plan, err := app.buildPlan(cmd.Context(), provs, p)
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
				// Le hook « preApply » garde la porte : son refus arrête l'opération.
				if _, err := app.runHook(cmd.Context(), provs.Hooks, p, hook.PreApply, hook.Context{}); err != nil {
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
					Reload: reload,
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
				// Le hook « postApply » constate : son échec est un avertissement.
				resources := make([]string, 0, len(report.Outcomes))
				for _, o := range report.Outcomes {
					resources = append(resources, o.Step.Resource)
				}
				if warned, _ := app.runHook(cmd.Context(), provs.Hooks, p, hook.PostApply,
					hook.Context{Resources: resources}); warned {
					warnings++
				}
			}
			// Rotation des journaux : elle porte sur tout le parc, donc après les
			// projets et une seule fois.
			if !app.DryRun {
				global, err := app.buildGlobalPlan(provs, projects)
				if err != nil {
					return err
				}
				if !global.Empty() {
					report, err := engine.Execute(cmd.Context(), global, engine.Options{
						OnStep: func(s *engine.Step, stepErr error) {
							if stepErr == nil {
								app.printf("  ✓ %s\n", s.Summary)
							}
						},
						OnWarning: func(s *engine.Step, stepErr error) {
							app.printf("  ⚠ %s : %v\n", s.Summary, stepErr)
						},
						Audit: func(s *engine.Step, outcome, detail string) {
							_ = app.Audit.Record("", s.Resource, "apply."+s.Action.String(), outcome, detail)
						},
						// Sans cette fonction, les rechargements du plan global n'étaient
						// jamais exécutés : systemd-tmpfiles ne créait pas les répertoires
						// d'exécution — les verrous de tâches échouaient donc tous — et le
						// timer de renouvellement des certificats n'était jamais activé.
						Reload: reload,
					})
					if err != nil {
						return wrap(CodeError, err)
					}
					warnings += len(report.Warnings)
				}
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

// systemctl pilote systemd. En mode simulé, aucune commande n'est exécutée.
func (a *App) systemctl(ctx context.Context, args ...string) error {
	return a.systemctlBin(ctx, "systemctl", args...)
}

// systemctlBin exécute un binaire systemd.
func (a *App) systemctlBin(ctx context.Context, bin string, args ...string) error {
	if a.Exec.Simulated() {
		return nil
	}
	_, err := a.Exec.Run(ctx, exec.Command{Name: bin, Args: args})
	return err
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
