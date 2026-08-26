package cli

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/unit"
	"leblanc.io/open-go-server/internal/spec"
)

func newServiceCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "service",
		Aliases: []string{"services"},
		Short:   "Gérer les services systemd des projets",
	}
	cmd.AddCommand(
		newServiceAddCommand(app),
		newServiceRemoveCommand(app),
		newServiceListCommand(app),
		newServiceControlCommand(app, "start", "Démarrer les services d'un projet"),
		newServiceControlCommand(app, "stop", "Arrêter les services d'un projet"),
		newServiceControlCommand(app, "restart", "Redémarrer les services d'un projet"),
		newServiceScaleCommand(app),
		newServiceLogsCommand(app),
	)
	return cmd
}

func newServiceAddCommand(app *App) *cobra.Command {
	var (
		templateName string
		instances    int
		transport    string
		command      string
		timeLimit    string
		memoryLimit  string
	)
	cmd := &cobra.Command{
		Use:   "add <id> <nom>",
		Short: "Déclarer un service",
		Long: "Ajoute la déclaration au fichier du projet ; « ogs apply » installe l'unité et\n" +
			"démarre les instances. Les unités sont templatisées : mettre à l'échelle ne\n" +
			"duplique aucun fichier.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			name := args[1]
			for _, s := range p.Spec.Services {
				if s.Name == name {
					return exitf(CodeConflict, "le service %s est déjà déclaré", name)
				}
			}
			params := map[string]string{}
			if transport != "" {
				params["transport"] = transport
			}
			if command != "" {
				params["command"] = command
			}
			if timeLimit != "" {
				params["timeLimit"] = timeLimit
			}
			if memoryLimit != "" {
				params["memoryLimit"] = memoryLimit
			}
			svc := model.Service{Name: name, Template: templateName,
				Instances: instances, Params: params}
			p.Spec.Services = append(p.Spec.Services, svc)
			if err := app.prepare(p); err != nil {
				return err
			}
			// Le rendu est validé avant d'écrire la déclaration : une erreur de
			// template doit être signalée maintenant, pas au prochain apply.
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			if _, err := provs.Unit.Build(p, svc); err != nil {
				return exitf(CodeValidation, "%s", err)
			}
			if err := spec.Save(app.Paths.ProjectFile(p.ID()), p); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ service %s déclaré (%s, %d instance(s))\n", name, templateName, instances)
			app.printf("\nAppliquer :\n  ogs apply %s\n", p.ID())
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&templateName, "template", unit.TemplateMessenger,
		"template : "+joinComma(unit.Templates()))
	f.IntVar(&instances, "instances", 1, "nombre d'instances à démarrer")
	f.StringVar(&transport, "transport", "", "transport Messenger consommé")
	f.StringVar(&command, "command", "", "commande à superviser (template worker)")
	f.StringVar(&timeLimit, "time-limit", "", "durée de vie d'un worker, en secondes")
	f.StringVar(&memoryLimit, "memory-limit", "", "limite mémoire d'un worker")
	return cmd
}

func newServiceRemoveCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <id> <nom>",
		Short: "Retirer un service",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			kept := make([]model.Service, 0, len(p.Spec.Services))
			found := false
			for _, s := range p.Spec.Services {
				if s.Name == args[1] {
					found = true
					continue
				}
				kept = append(kept, s)
			}
			if !found {
				return exitf(CodeUsage, "aucun service %q déclaré dans %s", args[1], p.ID())
			}
			p.Spec.Services = kept
			if err := spec.Save(app.Paths.ProjectFile(p.ID()), p); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ service %s retiré de la déclaration\n", args[1])
			app.printf("  « ogs apply %s » arrêtera et désactivera ses instances.\n", p.ID())
			return nil
		},
	}
}

// serviceRow est la vue machine d'un service.
type serviceRow struct {
	Project   string `json:"project" yaml:"project"`
	Name      string `json:"name" yaml:"name"`
	Template  string `json:"template" yaml:"template"`
	Declared  int    `json:"declared" yaml:"declared"`
	Active    int    `json:"active" yaml:"active"`
	Transport string `json:"transport,omitempty" yaml:"transport,omitempty"`
}

func newServiceListCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list [id...]",
		Aliases: []string{"ls"},
		Short:   "Lister les services et leurs instances",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := app.targets(args, "")
			if err != nil {
				return err
			}
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			var rows []serviceRow
			for _, p := range projects {
				if len(p.Spec.Services) == 0 {
					continue
				}
				obs, err := provs.Unit.Observe(cmd.Context(), p)
				if err != nil {
					return wrap(CodeError, err)
				}
				for _, svc := range p.Spec.Services {
					declared := svc.Instances
					if declared <= 0 {
						declared = 1
					}
					rows = append(rows, serviceRow{
						Project: p.ID(), Name: svc.Name,
						Template: orValue(svc.Template, unit.TemplateMessenger),
						Declared: declared, Active: obs.Instances[svc.Name],
						Transport: svc.Params["transport"],
					})
				}
			}
			return wrap(CodeError, app.emit("ServiceList", rows, nil, func(tw *tabwriter.Writer) {
				if len(rows) == 0 {
					fmt.Fprintln(app.Out, "aucun service déclaré")
					return
				}
				fmt.Fprintln(tw, "PROJET\tSERVICE\tTEMPLATE\tDÉCLARÉES\tACTIVES")
				for _, r := range rows {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\n",
						r.Project, r.Name, r.Template, r.Declared, r.Active)
				}
			}))
		},
	}
}

func newServiceControlCommand(app *App, verb, short string) *cobra.Command {
	return &cobra.Command{
		Use:   verb + " <id>",
		Short: short,
		Long: "Agit sur la cible du projet, donc sur tous ses services à la fois. Le\n" +
			"redémarrage est ordonné et laisse aux workers le temps de terminer le message\n" +
			"en cours.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			if err := provs.Unit.Control(cmd.Context(), p, verb); err != nil {
				return wrap(CodeError, err)
			}
			_ = app.Audit.Record(p.ID(), "unit/target", "service."+verb, "ok", "")
			app.printf("✓ %s : %s\n", unit.TargetName(p.ID()), verb)
			return nil
		},
	}
}

func newServiceScaleCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "scale <id> <service> <nombre>",
		Short: "Ajuster le nombre d'instances d'un service",
		Long: "Opération d'exploitation : elle agit immédiatement sans modifier la\n" +
			"déclaration du projet. Le prochain « apply » rétablira le nombre déclaré.",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			count, err := strconv.Atoi(args[2])
			if err != nil || count < 0 {
				return exitf(CodeUsage, "nombre d'instances invalide : %q", args[2])
			}
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			changed, err := provs.Unit.Scale(cmd.Context(), p, args[1], count)
			if err != nil {
				return wrap(CodeError, err)
			}
			if len(changed) == 0 {
				app.printf("= %s : déjà %d instance(s)\n", args[1], count)
				return nil
			}
			for _, c := range changed {
				app.printf("  %s\n", c)
			}
			_ = app.Audit.Record(p.ID(), "unit/"+args[1], "service.scale", "ok", args[2])
			app.printf("✓ %s : %d instance(s)\n", args[1], count)
			app.printf("  la déclaration du projet reste inchangée ; « ogs apply » la rétablira.\n")
			return nil
		},
	}
}

func newServiceLogsCommand(app *App) *cobra.Command {
	var (
		follow bool
		lines  int
	)
	cmd := &cobra.Command{
		Use:   "logs <id> <service>",
		Short: "Consulter les journaux d'un service",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			if err := provs.Unit.Logs(cmd.Context(), p, args[1], follow, lines); err != nil {
				return exitf(CodeDependency, "%v", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "suivre les journaux")
	cmd.Flags().IntVarP(&lines, "lines", "n", 50, "nombre de lignes à afficher")
	return cmd
}

// newMyCommand est la surface déléguée à l'utilisateur d'un projet.
func newMyCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "my",
		Short: "Piloter les services de son propre projet (surface déléguée par sudo)",
		Long: "Réservée à l'utilisateur d'un projet, via une règle sudoers générée par ogs.\n" +
			"La commande ne prend aucun argument de projet : il est déduit de l'appelant,\n" +
			"donc aucune autre cible n'est atteignable.",
		Annotations: map[string]string{"skip-root": "true"},
	}
	for _, verb := range []string{"start", "stop", "restart", "status"} {
		verb := verb
		cmd.AddCommand(&cobra.Command{
			Use:         verb,
			Short:       verb + " les services de son projet",
			Annotations: map[string]string{"skip-root": "true"},
			RunE: func(c *cobra.Command, _ []string) error {
				p, err := app.callerProject()
				if err != nil {
					return err
				}
				provs, err := app.certProviders()
				if err != nil {
					return err
				}
				defer provs.close()
				if err := provs.Unit.Control(c.Context(), p, verb); err != nil {
					return wrap(CodeError, err)
				}
				_ = app.Audit.Record(p.ID(), "unit/target", "my."+verb, "ok", callerDescription())
				app.printf("✓ %s : %s\n", unit.TargetName(p.ID()), verb)
				return nil
			},
		})
	}
	return cmd
}

// callerProject déduit le projet de l'appelant. getuid() ne convient pas : sudo
// appelle setresuid(0, 0, 0), donc l'UID réel vaut 0 dans le processus délégué.
// La source est SUDO_UID, que sudo positionne après avoir purgé l'environnement
// (directive env_reset, active par défaut).
func (a *App) callerProject() (*model.Project, error) {
	raw := os.Getenv("SUDO_UID")
	if raw == "" {
		if os.Getuid() != 0 {
			raw = strconv.Itoa(os.Getuid())
		} else {
			return nil, exitf(CodeUsage, "« ogs my » identifie l'appelant par SUDO_UID, absent ici : "+
				"employer « ogs service <verbe> <id> », qui est explicite")
		}
	}
	uid, err := strconv.Atoi(raw)
	if err != nil {
		return nil, exitf(CodeUsage, "SUDO_UID illisible (%q)", raw)
	}
	u, err := user.LookupId(raw)
	if err != nil {
		return nil, exitf(CodeUsage, "aucun compte d'uid %d", uid)
	}
	projects, err := a.loadProjects()
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		if p.Spec.User.Name == u.Username {
			return p, nil
		}
	}
	return nil, exitf(CodePermission,
		"le compte %s ne correspond à aucun projet géré par ogs", u.Username)
}

// callerDescription décrit l'appelant pour l'audit.
func callerDescription() string {
	if u := os.Getenv("SUDO_USER"); u != "" {
		return fmt.Sprintf("%s (SUDO_UID %s)", u, os.Getenv("SUDO_UID"))
	}
	return fmt.Sprintf("uid %d", os.Getuid())
}
