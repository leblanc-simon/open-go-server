package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/leviathan/ogs/internal/engine"
	"github.com/leviathan/ogs/internal/exec"
	"github.com/leviathan/ogs/internal/hook"
	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/naming"
	"github.com/leviathan/ogs/internal/provider/amqp"
	"github.com/leviathan/ogs/internal/provider/db"
	"github.com/leviathan/ogs/internal/provider/unit"
	"github.com/leviathan/ogs/internal/spec"
	"github.com/leviathan/ogs/internal/validate"
)

func newSiteCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "site",
		Aliases: []string{"sites"},
		Short:   "Créer et dupliquer des sites complets",
	}
	cmd.AddCommand(newSiteCreateCommand(app), newSiteCloneCommand(app))
	return cmd
}

// siteOptions rassemble les choix d'une création composite.
type siteOptions struct {
	kind       string
	appVersion string
	runtime    string
	phpVersion string
	env        string
	id         string
	aliases    []string
	redirects  []string
	database   string
	messenger  []string
	tls        bool
	selfSigned bool
	services   []string
	crons      []string
	engineName string
	preset     string
	basicAuth  bool
	apply      bool
}

func newSiteCreateCommand(app *App) *cobra.Command {
	var opts siteOptions
	cmd := &cobra.Command{
		Use:   "create <host>",
		Short: "Créer un site complet en une opération",
		Long: "Assemble l'ensemble des ressources d'un site — utilisateur, arborescence, base,\n" +
			"transport, pool PHP, vhost, certificat, tâches et services — puis applique le\n" +
			"tout en une saga : si une étape critique échoue, les précédentes sont défaites.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.buildSite(args[0], opts)
			if err != nil {
				return err
			}
			path := app.Paths.ProjectFile(p.ID())
			if _, err := os.Stat(path); err == nil {
				return exitf(CodeConflict, "le projet %s existe déjà (%s)", p.ID(), path)
			}
			existing, err := app.loadProjects()
			if err != nil {
				return err
			}
			if err := validate.Uniqueness(append(existing, p), app.Cfg.PrepDomain()); err != nil {
				return exitf(CodeConflict, "conflit avec le parc existant :\n%v", err)
			}

			app.printf("site %s\n", p.ID())
			app.summarizeSite(p)
			if app.DryRun {
				data, err := spec.Encode(p)
				if err != nil {
					return wrap(CodeError, err)
				}
				app.printf("\n--dry-run : %s serait écrit avec le contenu suivant.\n\n", path)
				fmt.Fprint(app.Out, string(data))
				return nil
			}
			// Le hook « preCreate » garde la porte : il peut refuser la création
			// avant que rien n'ait été écrit.
			if _, err := app.runHook(cmd.Context(), app.hookRunner(), p, hook.PreCreate, hook.Context{}); err != nil {
				return err
			}
			if err := spec.Save(path, p); err != nil {
				return wrap(CodeError, err)
			}
			if err := app.openStore(); err == nil {
				_ = app.Audit.Record(p.ID(), "site", "site.create", "ok", path)
			}
			app.printf("\n✓ déclaration écrite : %s\n", path)
			if !opts.apply {
				app.printf("\nAppliquer :\n  ogs apply %s\n", p.ID())
				return nil
			}
			return app.applyProjects(cmd, []*model.Project{p})
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.kind, "kind", string(model.KindSymfony), "type d'application ("+kindsList()+")")
	f.StringVar(&opts.appVersion, "app-version", "auto", "version applicative")
	f.StringVar(&opts.runtime, "runtime", "", "version du runtime pour Node, Ruby ou Python (exemple 22.14.0)")
	f.StringVar(&opts.phpVersion, "php", "", "version PHP (défaut "+model.DefaultPHPVersion+")")
	f.StringVar(&opts.env, "env", "prod", "environnement : prod ou prep")
	f.StringVar(&opts.id, "id", "", "forcer l'identifiant")
	f.StringSliceVar(&opts.aliases, "alias", nil, "domaine servi en plus du principal (répétable)")
	f.StringSliceVar(&opts.redirects, "redirect", nil, "domaine redirigé vers le principal (répétable)")
	f.StringVar(&opts.database, "db", "", "déclarer une base : mysql, mariadb ou postgres")
	f.StringSliceVar(&opts.messenger, "messenger", nil, "transports Messenger à déclarer (répétable)")
	f.BoolVar(&opts.tls, "tls", true, "demander un certificat TLS")
	f.BoolVar(&opts.selfSigned, "self-signed", false, "certificat auto-signé (développement)")
	f.StringSliceVar(&opts.services, "service", nil,
		"service à déclarer, sous la forme template:transport:instances (répétable)")
	f.StringSliceVar(&opts.crons, "cron", nil,
		"tâche à déclarer, sous la forme « expression:commande » (répétable)")
	f.StringVar(&opts.engineName, "engine", "", "moteur web : apache ou nginx")
	f.StringVar(&opts.preset, "preset", "", "dimensionnement du pool PHP")
	f.BoolVar(&opts.basicAuth, "basic-auth", false, "protéger la préproduction par authentification")
	f.BoolVar(&opts.apply, "apply", true, "appliquer immédiatement")
	return cmd
}

// buildSite assemble un projet complet à partir des options.
func (a *App) buildSite(host string, opts siteOptions) (*model.Project, error) {
	env, err := parseEnv(opts.env)
	if err != nil {
		return nil, err
	}
	kind := model.AppKind(opts.kind)
	if _, err := model.Resolve(kind, opts.appVersion); err != nil {
		return nil, exitf(CodeUsage, "%s (types connus : %s)", err, kindsList())
	}
	p := model.New(host, env, kind)
	if opts.runtime != "" {
		p.Spec.App.Runtime = opts.runtime
	}
	if opts.appVersion != "" {
		p.Spec.App.Version = opts.appVersion
	}
	if opts.id != "" {
		if err := naming.ValidateIdentifier(opts.id); err != nil {
			return nil, exitf(CodeUsage, "%s", err)
		}
		p.Metadata.Name = opts.id
	}
	if opts.phpVersion != "" {
		p.Spec.PHP.Pools = []model.Pool{{Name: model.MainPoolName, Version: opts.phpVersion}}
	}
	if opts.engineName != "" {
		p.Spec.Web.Engine = opts.engineName
	}
	// Première normalisation : l'identifiant et le host effectif en découlent, et
	// tout le reste s'y adosse.
	if err := a.prepare(p); err != nil {
		return nil, err
	}
	if opts.preset != "" && len(p.Spec.PHP.Pools) > 0 {
		p.Spec.PHP.Pools[0].Preset = opts.preset
	}

	// Domaines : le principal vient de la normalisation, les alias et redirections
	// s'y ajoutent.
	for _, alias := range opts.aliases {
		p.Spec.Domains = append(p.Spec.Domains, model.Domain{Name: alias, Role: "alias"})
	}
	primary := p.Spec.Domains[0].Name
	for _, redirect := range opts.redirects {
		p.Spec.Domains = append(p.Spec.Domains,
			model.Domain{Name: redirect, Role: "redirect", Target: primary})
	}

	if opts.database != "" {
		switch opts.database {
		case "mysql", "mariadb", "postgres":
			p.Spec.Databases = []model.Database{db.DefaultDatabase(p, opts.database)}
		default:
			return nil, exitf(CodeUsage, "moteur %q inconnu (mysql, mariadb ou postgres)", opts.database)
		}
	}
	if len(opts.messenger) > 0 {
		if kind == model.KindSymfony && !model.SupportsMessenger(kind, p.Spec.App.Version) {
			return nil, exitf(CodeUsage, "Symfony %s n'a pas le composant Messenger "+
				"(introduit en 4.1) : employer un service rabbitmq-consumer", p.Spec.App.Version)
		}
		p.Spec.AMQP = amqp.Preset(p, opts.messenger, true)
	}
	if !opts.tls {
		// Le certificat doit disparaître avec le TLS : le laisser déclaré ferait
		// solliciter l'autorité — et consommer un quota — pour un site qui reste
		// délibérément en HTTP.
		p.Spec.Certificates = nil
		// Le réglage est porté par le projet, non par les vhosts : ceux-ci sont le
		// plus souvent implicites, et la liste est vide à ce stade.
		p.Spec.Web.TLS = &model.TLS{Enabled: false}
		for i := range p.Spec.Web.VHosts {
			p.Spec.Web.VHosts[i].TLS = &model.TLS{Enabled: false}
		}
	}
	if opts.selfSigned {
		p.Spec.Certificates = []model.Certificate{{
			Name: p.ID(), Domains: servedDomains(p), Challenge: "selfsigned",
		}}
	}
	for _, raw := range opts.services {
		svc, err := parseServiceOption(raw)
		if err != nil {
			return nil, exitf(CodeUsage, "%s", err)
		}
		p.Spec.Services = append(p.Spec.Services, svc)
	}
	for _, raw := range opts.crons {
		job, err := parseCronOption(raw)
		if err != nil {
			return nil, exitf(CodeUsage, "%s", err)
		}
		p.Spec.Crons = append(p.Spec.Crons, job)
	}
	if opts.basicAuth {
		if p.Spec.Sudo == nil {
			p.Spec.Sudo = &model.Sudo{}
		}
		// L'authentification de préproduction est portée par le vhost ; on la note
		// dans les labels pour que la génération la reprenne.
		if p.Metadata.Labels == nil {
			p.Metadata.Labels = map[string]string{}
		}
		p.Metadata.Labels["basicAuth"] = "true"
	}
	// Seconde normalisation et validation, une fois toutes les ressources posées.
	if err := a.prepare(p); err != nil {
		return nil, err
	}
	return p, nil
}

// servedDomains renvoie les domaines à couvrir par un certificat.
func servedDomains(p *model.Project) []string {
	var out []string
	for _, d := range p.Spec.Domains {
		switch d.Role {
		case "", "primary", "alias":
			out = append(out, d.Name)
		}
	}
	return out
}

// parseServiceOption lit « template:transport:instances ».
func parseServiceOption(raw string) (model.Service, error) {
	parts := strings.Split(raw, ":")
	if parts[0] == "" {
		return model.Service{}, fmt.Errorf("service %q : template manquant "+
			"(attendu template:transport:instances)", raw)
	}
	svc := model.Service{Template: parts[0], Instances: 1, Params: map[string]string{}}
	transport := ""
	if len(parts) > 1 && parts[1] != "" {
		transport = parts[1]
		svc.Params["transport"] = transport
	}
	if len(parts) > 2 && parts[2] != "" {
		n, err := strconv.Atoi(parts[2])
		if err != nil || n < 1 {
			return model.Service{}, fmt.Errorf("service %q : nombre d'instances invalide", raw)
		}
		svc.Instances = n
	}
	svc.Name = parts[0]
	if transport != "" {
		svc.Name = strings.TrimPrefix(parts[0], "symfony-") + "-" + transport
	}
	return svc, nil
}

// parseCronOption lit « expression:commande ». La découpe se fait au premier
// deux-points : une expression cron n'en contient jamais, alors qu'une commande
// en contient souvent (« bin/console schedule:run »).
func parseCronOption(raw string) (model.CronJob, error) {
	idx := strings.Index(raw, ":")
	if idx <= 0 || idx == len(raw)-1 {
		return model.CronJob{}, fmt.Errorf("tâche %q : format attendu « expression:commande »", raw)
	}
	schedule := strings.TrimSpace(raw[:idx])
	command := strings.TrimSpace(raw[idx+1:])
	if err := validate.CronExpression(schedule); err != nil {
		return model.CronJob{}, fmt.Errorf("tâche %q : %w", raw, err)
	}
	name := "tache"
	if fields := strings.Fields(command); len(fields) > 0 {
		base := filepath.Base(fields[len(fields)-1])
		name = strings.NewReplacer(":", "-", ".", "-", "/", "-").Replace(base)
	}
	return model.CronJob{Name: name, Schedule: schedule, Command: command}, nil
}

// summarizeSite affiche ce qui va être créé.
func (a *App) summarizeSite(p *model.Project) {
	at, _ := p.AppType()
	a.printf("  host         %s\n", p.Spec.Domains[0].Name)
	a.printf("  utilisateur  %s:%s\n", p.Spec.User.Name, p.Spec.User.Group)
	a.printf("  home         %s\n", p.Spec.Layout.Root)
	if at.Docroot != "" {
		a.printf("  docroot      %s\n", p.Spec.Layout.Docroot)
	}
	for _, pool := range p.Spec.PHP.Pools {
		a.printf("  php          %s (pool %s, %s)\n", pool.Version, pool.Name, pool.Preset)
	}
	for _, d := range p.Spec.Databases {
		a.printf("  base         %s %s (utilisateur %s)\n", d.Engine, d.Name, d.User)
	}
	if p.Spec.AMQP != nil {
		a.printf("  transport    vhost %s, files %s\n", p.Spec.AMQP.Vhost,
			strings.Join(queueNames(p.Spec.AMQP), ", "))
	}
	for _, svc := range p.Spec.Services {
		a.printf("  service      %s (%s, %d instance(s))\n", svc.Name, svc.Template, svc.Instances)
	}
	for _, c := range p.Spec.Crons {
		a.printf("  tâche        %s (%s)\n", c.Name, c.Schedule)
	}
}

func newSiteCloneCommand(app *App) *cobra.Command {
	var (
		to       string
		withData bool
	)
	cmd := &cobra.Command{
		Use:   "clone <id>",
		Short: "Dupliquer un site vers un autre environnement",
		Long: "Crée le projet de destination avec la même configuration, copie le contenu de\n" +
			"www/ puis, si demandé, restaure un dump de la base source dans la base cible.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			source, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			env, err := parseEnv(to)
			if err != nil {
				return err
			}
			if env == source.Env() {
				return exitf(CodeUsage, "le projet %s est déjà en environnement %s", source.ID(), to)
			}
			target, err := app.cloneProject(source, env)
			if err != nil {
				return err
			}
			path := app.Paths.ProjectFile(target.ID())
			if _, err := os.Stat(path); err == nil {
				return exitf(CodeConflict, "le projet %s existe déjà (%s)", target.ID(), path)
			}
			app.printf("clone de %s vers %s (%s)\n", source.ID(), target.ID(), to)
			app.summarizeSite(target)
			if app.DryRun {
				app.printf("\n--dry-run : rien n'a été écrit.\n")
				return nil
			}
			// Le hook « preCreate » garde la porte : il peut refuser la création
			// avant que rien n'ait été écrit.
			if _, err := app.runHook(cmd.Context(), app.hookRunner(), target, hook.PreCreate, hook.Context{}); err != nil {
				return err
			}
			if err := spec.Save(path, target); err != nil {
				return wrap(CodeError, err)
			}
			// Un succès partiel n'interrompt pas la copie du contenu : le code est
			// conservé et rendu à la fin.
			partial := error(nil)
			if err := app.applyProjects(cmd, []*model.Project{target}); err != nil {
				if ExitCode(err) != CodePartial {
					return err
				}
				partial = err
			}

			// Copie du contenu applicatif, une fois l'arborescence en place.
			srcWWW := filepath.Join(source.Spec.Layout.Root, "www")
			dstWWW := filepath.Join(target.Spec.Layout.Root, "www")
			if err := app.copyTree(cmd, srcWWW, dstWWW, target); err != nil {
				app.printf("⚠ copie du contenu : %v\n", err)
			} else {
				app.printf("✓ contenu copié : %s → %s\n", srcWWW, dstWWW)
			}

			if withData && len(source.Spec.Databases) > 0 && len(target.Spec.Databases) > 0 {
				if err := app.cloneData(cmd, source, target); err != nil {
					app.printf("⚠ copie des données : %v\n", err)
				}
			}
			_ = app.Audit.Record(target.ID(), "site", "site.clone", "ok", "depuis "+source.ID())
			app.printf("\n✓ %s créé depuis %s\n", target.ID(), source.ID())
			return partial
		},
	}
	cmd.Flags().StringVar(&to, "to", "prep", "environnement de destination")
	cmd.Flags().BoolVar(&withData, "with-data", true, "copier aussi le contenu de la base")
	return cmd
}

// cloneProject construit le projet de destination depuis la source.
func (a *App) cloneProject(source *model.Project, env naming.Env) (*model.Project, error) {
	clone := *source
	clone.Metadata.Name = ""
	clone.Metadata.Env = string(env)
	clone.Metadata.Description = source.Metadata.Description
	// Tout ce qui porte l'identifiant doit être recalculé : l'oublier ferait
	// pointer la préproduction sur les ressources de la production.
	clone.Spec.User = model.User{}
	clone.Spec.Layout = model.Layout{}
	clone.Spec.Domains = nil
	clone.Spec.Web.VHosts = nil
	clone.Spec.Certificates = nil
	clone.Spec.Databases = nil
	clone.Spec.AMQP = nil
	if err := a.prepare(&clone); err != nil {
		return nil, err
	}
	for _, d := range source.Spec.Databases {
		decl := db.DefaultDatabase(&clone, d.Engine)
		decl.Cluster = d.Cluster
		decl.Extensions = d.Extensions
		decl.Privileges = d.Privileges
		clone.Spec.Databases = append(clone.Spec.Databases, decl)
	}
	if source.Spec.AMQP != nil {
		transports := make([]string, 0, len(source.Spec.AMQP.Queues))
		for _, q := range source.Spec.AMQP.Queues {
			if !strings.HasSuffix(q.Name, amqp.FailedSuffix) {
				transports = append(transports, q.Name)
			}
		}
		clone.Spec.AMQP = amqp.Preset(&clone, transports, true)
	}
	if err := a.prepare(&clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

// copyTree recopie une arborescence sous l'identité du projet cible.
func (a *App) copyTree(cmd *cobra.Command, src, dst string, target *model.Project) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("source absente : %w", err)
	}
	if a.Exec.Simulated() {
		a.printf("= mode simulé : la copie n'est pas exécutée\n")
		return nil
	}
	// cp -a préserve les permissions et les liens ; le chown final rend le
	// contenu au propriétaire du projet cible.
	if _, err := a.Exec.Run(cmd.Context(), exec.Command{
		Name: "/bin/cp", Args: []string{"-a", src + "/.", dst},
	}); err != nil {
		return err
	}
	_, err := a.Exec.Run(cmd.Context(), exec.Command{
		Name: "/bin/chown",
		Args: []string{"-R", target.Spec.User.Name + ":" + target.Spec.User.Group, dst},
	})
	return err
}

// cloneData transfère le contenu de la base source vers la base cible.
func (a *App) cloneData(cmd *cobra.Command, source, target *model.Project) error {
	provs, err := a.buildProviders()
	if err != nil {
		return err
	}
	defer provs.close()
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("ogs-clone-%s-%d.sql", source.ID(), time.Now().Unix()))
	defer os.Remove(tmp)
	if _, err := provs.DB.Dump(cmd.Context(), source, source.Spec.Databases[0], tmp); err != nil {
		return fmt.Errorf("sauvegarde de la source : %w", err)
	}
	if err := provs.DB.Restore(cmd.Context(), target, target.Spec.Databases[0], tmp); err != nil {
		return fmt.Errorf("restauration dans la cible : %w", err)
	}
	a.printf("✓ données copiées : %s → %s\n", source.Spec.Databases[0].Name, target.Spec.Databases[0].Name)
	return nil
}

// applyProjects applique une liste de projets, avec verrou et rapport.
func (a *App) applyProjects(cmd *cobra.Command, projects []*model.Project) error {
	lock, err := engine.Acquire(a.Paths.GlobalLock())
	if err != nil {
		return wrap(CodeLocked, err)
	}
	defer lock.Release()
	if err := a.openStore(); err != nil {
		return err
	}
	provs, err := a.buildProviders()
	if err != nil {
		return err
	}
	defer provs.close()
	reload := a.reloadFunc(provs)
	warnings := 0
	for _, p := range projects {
		plan, err := a.buildPlan(cmd.Context(), provs, p)
		if err != nil {
			return err
		}
		if plan.Empty() {
			a.printf("= %s : aucun changement\n", p.ID())
			continue
		}
		a.printf("\n%s\n", plan.String())
		if err := a.confirm(fmt.Sprintf("appliquer %d changement(s) sur %s ?",
			len(plan.Mutating()), p.ID())); err != nil {
			return err
		}
		report, err := engine.Execute(cmd.Context(), plan, engine.Options{
			OnStep: func(s *engine.Step, stepErr error) {
				if stepErr == nil {
					a.printf("  ✓ %s\n", s.Summary)
				} else {
					a.printf("  ✗ %s : %v\n", s.Summary, stepErr)
				}
			},
			OnWarning: func(s *engine.Step, stepErr error) {
				a.printf("  ⚠ %s : %v\n", s.Summary, stepErr)
			},
			Audit: func(s *engine.Step, outcome, detail string) {
				_ = a.Audit.Record(p.ID(), s.Resource, "apply."+s.Action.String(), outcome, detail)
			},
			Reload: reload,
		})
		warnings += len(report.Warnings)
		if err != nil {
			if report.RolledBack {
				a.printf("↶ rollback effectué : le système est revenu à son état initial.\n")
			}
			if report.RollbackErr != nil {
				a.printf("✗ le rollback a échoué : %v\n", report.RollbackErr)
			}
			return wrap(CodeError, err)
		}
		if err := a.recordState(p); err != nil {
			return err
		}
		a.printf("✓ %s : %d étape(s) appliquée(s)", p.ID(), report.Applied())
		if len(report.Warnings) > 0 {
			a.printf(", %d avertissement(s)", len(report.Warnings))
		}
		a.printf(" (audit %s)\n", a.Audit.RunID())
	}

	for _, p := range projects {
		if warned, _ := a.runHook(cmd.Context(), provs.Hooks, p, hook.PostCreate, hook.Context{}); warned {
			warnings++
		}
	}

	global, err := a.buildGlobalPlan(provs, projects)
	if err != nil {
		return err
	}
	if !global.Empty() {
		if _, err := engine.Execute(cmd.Context(), global, engine.Options{
			OnStep: func(s *engine.Step, stepErr error) {
				if stepErr == nil {
					a.printf("  ✓ %s\n", s.Summary)
				}
			},
			OnWarning: func(s *engine.Step, stepErr error) {
				a.printf("  ⚠ %s : %v\n", s.Summary, stepErr)
			},
			// Les rechargements du plan global comptent autant que ceux d'un projet :
			// c'est là que les répertoires d'exécution sont créés et que le timer de
			// renouvellement est activé.
			Reload: a.reloadFunc(provs),
		}); err != nil {
			return wrap(CodeError, err)
		}
	}

	// Récapitulatif : les chaînes de connexion sont ce que l'exploitant doit
	// reporter dans la configuration applicative.
	for _, p := range projects {
		if len(p.Spec.Databases) == 0 && p.Spec.AMQP == nil {
			continue
		}
		entries, err := provs.DB.Env(cmd.Context(), p)
		if err != nil || len(entries) == 0 {
			continue
		}
		a.printf("\nchaînes de connexion de %s :\n", p.ID())
		for _, e := range entries {
			a.printf("  %s=%q\n", e.Name, e.Value)
		}
	}
	if warnings > 0 {
		return exitf(CodePartial, "terminé avec %d avertissement(s)", warnings)
	}
	return nil
}

// statusRow est la vue consolidée d'un projet.
type statusRow struct {
	Project   string   `json:"project" yaml:"project"`
	Env       string   `json:"env" yaml:"env"`
	Host      string   `json:"host" yaml:"host"`
	PHP       string   `json:"php,omitempty" yaml:"php,omitempty"`
	Drift     int      `json:"drift" yaml:"drift"`
	Services  string   `json:"services,omitempty" yaml:"services,omitempty"`
	Certifate string   `json:"certificate,omitempty" yaml:"certificate,omitempty"`
	Health    string   `json:"health" yaml:"health"`
	Problems  []string `json:"problems,omitempty" yaml:"problems,omitempty"`
}

func newStatusCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status [id...]",
		Short: "Vue consolidée de l'état des projets",
		Long: "Rassemble dérive de configuration, services actifs et échéance des certificats.\n" +
			"Le code de retour est non nul dès qu'un problème est détecté, ce qui rend la\n" +
			"commande utilisable en supervision.",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := app.targets(args, "")
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

			var rows []statusRow
			problems := 0
			for _, p := range projects {
				row := statusRow{Project: p.ID(), Env: p.Metadata.Env, Host: p.Spec.Domains[0].Name,
					Health: "sain"}
				if len(p.Spec.PHP.Pools) > 0 {
					row.PHP = p.Spec.PHP.Pools[0].Version
				}
				plan, err := app.buildPlan(cmd.Context(), provs, p)
				if err != nil {
					row.Health = "erreur"
					row.Problems = append(row.Problems, err.Error())
					problems++
					rows = append(rows, row)
					continue
				}
				row.Drift = len(plan.Mutating())
				if row.Drift > 0 {
					row.Health = "dérive"
					problems++
				}
				if len(p.Spec.Services) > 0 {
					obs, err := provs.Unit.Observe(cmd.Context(), p)
					if err == nil {
						active, declared := 0, 0
						for _, svc := range p.Spec.Services {
							want := svc.Instances
							if want <= 0 {
								want = 1
							}
							declared += want
							active += obs.Instances[svc.Name]
						}
						row.Services = fmt.Sprintf("%d/%d", active, declared)
						if active < declared {
							row.Health = "services incomplets"
							row.Problems = append(row.Problems,
								fmt.Sprintf("%d instance(s) sur %d actives", active, declared))
							problems++
						}
					}
				}
				if ref, ready := provs.TLS.CertificateFor(p); ref != "" {
					if !ready {
						row.Certifate = "absent"
						row.Problems = append(row.Problems, "certificat "+ref+" indisponible")
						if row.Health == "sain" {
							row.Health = "TLS absent"
						}
						problems++
					} else if info, ok, _ := provs.TLS.Store.Read(ref); ok {
						days := info.DaysLeft(time.Now())
						row.Certifate = fmt.Sprintf("%d j", days)
						if info.NeedsRenewal(time.Now()) {
							row.Problems = append(row.Problems,
								fmt.Sprintf("certificat %s à renouveler (%d jours)", ref, days))
							problems++
						}
					}
				}
				rows = append(rows, row)
			}

			var warnings []string
			for _, r := range rows {
				warnings = append(warnings, r.Problems...)
			}
			if err := app.emit("StatusList", rows, warnings, func(tw *tabwriter.Writer) {
				if len(rows) == 0 {
					fmt.Fprintln(app.Out, "aucun projet déclaré")
					return
				}
				fmt.Fprintln(tw, "PROJET\tENV\tHOST\tPHP\tÉCARTS\tSERVICES\tTLS\tÉTAT")
				for _, r := range rows {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
						r.Project, r.Env, r.Host, orValue(r.PHP, "—"), r.Drift,
						orValue(r.Services, "—"), orValue(r.Certifate, "—"), r.Health)
				}
			}); err != nil {
				return wrap(CodeError, err)
			}
			if problems > 0 {
				return exitf(CodePartial, "%d problème(s) détecté(s)", problems)
			}
			return nil
		},
	}
}

// unitTarget expose le nom de cible pour les messages.
func unitTarget(id string) string { return unit.TargetName(id) }
