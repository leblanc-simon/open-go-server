package cli

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/naming"
	"github.com/leviathan/ogs/internal/provider/db"
	"github.com/leviathan/ogs/internal/spec"
	"github.com/leviathan/ogs/internal/validate"
)

func newProjectCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "project",
		Aliases: []string{"projects"},
		Short:   "Gérer les projets",
	}
	cmd.AddCommand(
		newProjectCreateCommand(app),
		newProjectListCommand(app),
		newProjectShowCommand(app),
		newProjectExportCommand(app),
	)
	return cmd
}

func newProjectCreateCommand(app *App) *cobra.Command {
	var (
		kind    string
		envName string
		id      string
		hostOpt string
		appVer  string
		phpVer  string
		withDB  string
	)
	cmd := &cobra.Command{
		Use:   "create <host>",
		Short: "Déclarer un projet et écrire son fichier d'état désiré",
		Long: "Écrit /etc/ogs/projects/<id>.yaml. Rien n'est appliqué au système : c'est\n" +
			"« ogs apply » qui réconcilie. L'identifiant est dérivé du host, sauf --id.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := parseEnv(envName)
			if err != nil {
				return err
			}
			kindVal := model.AppKind(kind)
			if _, err := model.Resolve(kindVal, appVer); err != nil {
				return exitf(CodeUsage, "%s (types connus : %s)", err, kindsList())
			}
			p := model.New(args[0], env, kindVal)
			if appVer != "" {
				p.Spec.App.Version = appVer
			}
			if id != "" {
				if err := naming.ValidateIdentifier(id); err != nil {
					return exitf(CodeUsage, "%s", err)
				}
				p.Metadata.Name = id
			}
			if phpVer != "" {
				p.Spec.PHP.Pools = []model.Pool{{Name: model.MainPoolName, Version: phpVer}}
			}
			if hostOpt != "" {
				// Un host explicite dissocie le nom servi du host canonique.
				p.Spec.Domains = []model.Domain{{Name: hostOpt, Role: "primary"}}
			}
			if err := app.prepare(p); err != nil {
				return err
			}
			// La base est déclarée après la normalisation : son nom dérive de
			// l'identifiant du projet, qui n'est connu qu'à ce moment.
			if withDB != "" {
				switch withDB {
				case "mysql", "mariadb", "postgres":
					p.Spec.Databases = []model.Database{db.DefaultDatabase(p, withDB)}
				default:
					return exitf(CodeUsage, "moteur %q inconnu (mysql, mariadb ou postgres)", withDB)
				}
				if err := app.prepare(p); err != nil {
					return err
				}
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
			if app.DryRun {
				data, err := spec.Encode(p)
				if err != nil {
					return wrap(CodeError, err)
				}
				app.printf("--dry-run : %s serait écrit avec le contenu suivant.\n\n", path)
				fmt.Fprint(app.Out, string(data))
				return nil
			}
			if err := spec.Save(path, p); err != nil {
				return wrap(CodeError, err)
			}
			if err := app.openStore(); err == nil {
				_ = app.Audit.Record(p.ID(), "project", "project.create", "ok", path)
			}
			at, _ := p.AppType()
			app.printf("✓ projet %s déclaré : %s\n", p.ID(), path)
			app.printf("  host       %s\n", p.Metadata.Host)
			app.printf("  home       %s\n", p.Spec.Layout.Root)
			if at.Docroot != "" {
				app.printf("  docroot    %s\n", p.Spec.Layout.Docroot)
			}
			app.printf("\nProchaine étape :\n  ogs plan %s\n  ogs apply %s\n", p.ID(), p.ID())
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&kind, "kind", string(model.KindStatic), "type d'application ("+kindsList()+")")
	f.StringVar(&appVer, "app-version", "auto", "version applicative (« auto » ou par exemple 3.4)")
	f.StringVar(&envName, "env", "prod", "environnement : prod ou prep")
	f.StringVar(&id, "id", "", "forcer l'identifiant au lieu de le dériver du host")
	f.StringVar(&hostOpt, "host", "", "host servi, s'il diffère du host canonique")
	f.StringVar(&phpVer, "php", "", "version PHP du pool principal (défaut "+model.DefaultPHPVersion+")")
	f.StringVar(&withDB, "db", "", "déclarer une base : mysql, mariadb ou postgres")
	return cmd
}

func kindsList() string {
	kinds := model.Kinds()
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return joinComma(out)
}

func joinComma(items []string) string {
	s := ""
	for i, v := range items {
		if i > 0 {
			s += ", "
		}
		s += v
	}
	return s
}

// projectSummary est la vue exposée par les sorties machine.
type projectSummary struct {
	ID      string   `json:"id" yaml:"id"`
	Host    string   `json:"host" yaml:"host"`
	Env     string   `json:"env" yaml:"env"`
	Kind    string   `json:"kind" yaml:"kind"`
	Home    string   `json:"home" yaml:"home"`
	Docroot string   `json:"docroot,omitempty" yaml:"docroot,omitempty"`
	Domains []string `json:"domains" yaml:"domains"`
}

func summarize(p *model.Project) projectSummary {
	domains := make([]string, 0, len(p.Spec.Domains))
	for _, d := range p.Spec.Domains {
		domains = append(domains, d.Name)
	}
	return projectSummary{
		ID: p.ID(), Host: p.Metadata.Host, Env: p.Metadata.Env,
		Kind: string(p.Spec.App.Kind), Home: p.Spec.Layout.Root,
		Docroot: p.Spec.Layout.Docroot, Domains: domains,
	}
}

func newProjectListCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "Lister les projets déclarés",
		RunE: func(cmd *cobra.Command, _ []string) error {
			projects, err := app.loadProjects()
			if err != nil {
				return err
			}
			items := make([]projectSummary, 0, len(projects))
			for _, p := range projects {
				items = append(items, summarize(p))
			}
			return wrap(CodeError, app.emit("ProjectList", items, nil, func(tw *tabwriter.Writer) {
				if len(items) == 0 {
					fmt.Fprintln(app.Out, "aucun projet déclaré")
					return
				}
				fmt.Fprintln(tw, "IDENTIFIANT\tHOST\tENV\tTYPE\tHOME")
				for _, it := range items {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", it.ID, it.Host, it.Env, it.Kind, it.Home)
				}
			}))
		},
	}
}

func newProjectShowCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Afficher le détail d'un projet",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			return wrap(CodeError, app.emit("Project", summarize(p), nil, func(tw *tabwriter.Writer) {
				at, _ := p.AppType()
				fmt.Fprintf(tw, "identifiant\t%s\n", p.ID())
				fmt.Fprintf(tw, "host\t%s\n", p.Metadata.Host)
				fmt.Fprintf(tw, "environnement\t%s\n", p.Metadata.Env)
				fmt.Fprintf(tw, "type\t%s (version %s)\n", p.Spec.App.Kind, p.Spec.App.Version)
				fmt.Fprintf(tw, "utilisateur\t%s:%s (shell %s)\n", p.Spec.User.Name, p.Spec.User.Group, p.Spec.User.Shell)
				fmt.Fprintf(tw, "accès web\t%s\n", p.Spec.User.WebAccess)
				fmt.Fprintf(tw, "home\t%s (mode %s)\n", p.Spec.Layout.Root, p.Spec.Layout.Mode)
				if at.Docroot != "" {
					fmt.Fprintf(tw, "docroot\t%s\n", p.Spec.Layout.Docroot)
				}
				fmt.Fprintf(tw, "logs\t%s\n", p.Spec.Layout.LogDir)
				for _, d := range p.Spec.Domains {
					fmt.Fprintf(tw, "domaine\t%s (%s)\n", d.Name, orValue(d.Role, "primary"))
				}
				for _, db := range p.Spec.Databases {
					fmt.Fprintf(tw, "base\t%s %s (utilisateur %s)\n", db.Engine, db.Name, db.User)
				}
				for _, pool := range p.Spec.PHP.Pools {
					fmt.Fprintf(tw, "pool php\t%s version %s (%s)\n", pool.Name, pool.Version, orValue(pool.Preset, "medium"))
				}
			}))
		},
	}
}

func newProjectExportCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "export <id>",
		Short: "Écrire le YAML normalisé du projet sur la sortie standard",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			data, err := spec.Encode(p)
			if err != nil {
				return wrap(CodeError, err)
			}
			_, err = app.Out.Write(data)
			return wrap(CodeError, err)
		},
	}
}

func orValue(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
