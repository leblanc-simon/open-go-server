package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/cron"
	"leblanc.io/open-go-server/internal/spec"
	"leblanc.io/open-go-server/internal/validate"
)

func newCronCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "cron",
		Aliases: []string{"crons"},
		Short:   "Gérer les tâches planifiées",
	}
	cmd.AddCommand(
		newCronAddCommand(app),
		newCronRemoveCommand(app),
		newCronListCommand(app),
		newCronToggleCommand(app, true),
		newCronToggleCommand(app, false),
		newCronRunCommand(app),
	)
	return cmd
}

func newCronAddCommand(app *App) *cobra.Command {
	var (
		schedule string
		command  string
		noLock   bool
		workdir  string
		php      string
	)
	cmd := &cobra.Command{
		Use:   "add <id> <nom>",
		Short: "Déclarer une tâche planifiée",
		Long: "La tâche est enveloppée dans flock, s'exécute sous l'utilisateur du projet,\n" +
			"depuis son répertoire de travail, et journalise sa sortie dans logs/. L'expression\n" +
			"est validée et ses prochaines occurrences affichées, afin de confirmer qu'elle\n" +
			"correspond à l'intention.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			name := args[1]
			if strings.ContainsAny(name, "/. ") {
				return exitf(CodeUsage, "nom de tâche %q invalide : ni point, ni barre oblique, ni espace", name)
			}
			for _, c := range p.Spec.Crons {
				if c.Name == name {
					return exitf(CodeConflict, "la tâche %s est déjà déclarée", name)
				}
			}
			if command == "" {
				return exitf(CodeUsage, "préciser la commande avec --command")
			}
			if err := validate.CronExpression(schedule); err != nil {
				return exitf(CodeValidation, "expression %q : %v", schedule, err)
			}
			if php != "" {
				if err := validate.PHPVersion(php); err != nil {
					return exitf(CodeValidation, "--php : %v", err)
				}
			}
			next, err := cron.NextOccurrences(schedule, time.Now(), 3)
			if err != nil {
				return exitf(CodeValidation, "expression %q : %v", schedule, err)
			}
			lock := !noLock
			job := model.CronJob{Name: name, Schedule: schedule, Command: command,
				Workdir: workdir, PHP: php, Lock: &lock}
			p.Spec.Crons = append(p.Spec.Crons, job)
			if err := app.prepare(p); err != nil {
				return err
			}
			if app.DryRun {
				app.printf("--dry-run : tâche %s (%s)\n", name, schedule)
				return nil
			}
			if err := spec.Save(app.Paths.ProjectFile(p.ID()), p); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ tâche %s déclarée (%s)\n", name, schedule)
			app.printf("  prochaines exécutions :\n")
			for _, t := range next {
				app.printf("    %s\n", t.Format("02/01/2006 à 15:04"))
			}
			app.printf("\nAppliquer :\n  ogs apply %s\n", p.ID())
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&schedule, "schedule", "", "expression cron à cinq champs")
	f.StringVar(&command, "command", "", "commande à exécuter")
	f.BoolVar(&noLock, "no-lock", false, "ne pas envelopper dans flock")
	f.StringVar(&workdir, "workdir", "", "répertoire de travail (défaut : www/ du projet)")
	f.StringVar(&php, "php", "", "version de PHP de la tâche (défaut : celle du projet, par le lien géré)")
	_ = cmd.MarkFlagRequired("schedule")
	return cmd
}

func newCronRemoveCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <id> <nom>",
		Short: "Retirer une tâche planifiée",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			kept := make([]model.CronJob, 0, len(p.Spec.Crons))
			found := false
			for _, c := range p.Spec.Crons {
				if c.Name == args[1] {
					found = true
					continue
				}
				kept = append(kept, c)
			}
			if !found {
				return exitf(CodeUsage, "aucune tâche %q déclarée dans %s", args[1], p.ID())
			}
			p.Spec.Crons = kept
			if err := spec.Save(app.Paths.ProjectFile(p.ID()), p); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ tâche %s retirée\n", args[1])
			return nil
		},
	}
}

// cronRow est la vue machine d'une tâche.
type cronRow struct {
	Project  string `json:"project" yaml:"project"`
	Name     string `json:"name" yaml:"name"`
	Schedule string `json:"schedule" yaml:"schedule"`
	Command  string `json:"command" yaml:"command"`
	Enabled  bool   `json:"enabled" yaml:"enabled"`
	Next     string `json:"next,omitempty" yaml:"next,omitempty"`
}

func newCronListCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list [id...]",
		Aliases: []string{"ls"},
		Short:   "Lister les tâches planifiées",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := app.targets(args, "")
			if err != nil {
				return err
			}
			now := time.Now()
			var rows []cronRow
			for _, p := range projects {
				for _, c := range p.Spec.Crons {
					enabled := c.Enabled == nil || *c.Enabled
					row := cronRow{Project: p.ID(), Name: c.Name, Schedule: c.Schedule,
						Command: c.Command, Enabled: enabled}
					if next, err := cron.NextOccurrences(c.Schedule, now, 1); err == nil && enabled {
						row.Next = next[0].Format("02/01 15:04")
					}
					rows = append(rows, row)
				}
			}
			return wrap(CodeError, app.emit("CronList", rows, nil, func(tw *tabwriter.Writer) {
				if len(rows) == 0 {
					fmt.Fprintln(app.Out, "aucune tâche déclarée")
					return
				}
				fmt.Fprintln(tw, "PROJET\tTÂCHE\tEXPRESSION\tÉTAT\tPROCHAINE\tCOMMANDE")
				for _, r := range rows {
					state := "active"
					if !r.Enabled {
						state = "désactivée"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
						r.Project, r.Name, r.Schedule, state, orValue(r.Next, "—"), r.Command)
				}
			}))
		},
	}
}

func newCronToggleCommand(app *App, enable bool) *cobra.Command {
	verb, label := "disable", "Désactiver"
	if enable {
		verb, label = "enable", "Activer"
	}
	return &cobra.Command{
		Use:   verb + " <id> <nom>",
		Short: label + " une tâche planifiée",
		Long: "Une tâche désactivée reste dans le fichier cron, en commentaire : la trace est\n" +
			"conservée et la réactivation est immédiate.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			found := false
			for i := range p.Spec.Crons {
				if p.Spec.Crons[i].Name == args[1] {
					value := enable
					p.Spec.Crons[i].Enabled = &value
					found = true
				}
			}
			if !found {
				return exitf(CodeUsage, "aucune tâche %q déclarée dans %s", args[1], p.ID())
			}
			if err := spec.Save(app.Paths.ProjectFile(p.ID()), p); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ tâche %s : %s\n", args[1], map[bool]string{true: "activée", false: "désactivée"}[enable])
			app.printf("\nAppliquer :\n  ogs apply %s\n", p.ID())
			return nil
		},
	}
}

func newCronRunCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "run <id> <nom>",
		Short: "Exécuter une tâche immédiatement",
		Long: "Exécute la tâche sous l'identité du projet et avec le même verrou, pour la\n" +
			"tester sans attendre son échéance.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			var job *model.CronJob
			for i := range p.Spec.Crons {
				if p.Spec.Crons[i].Name == args[1] {
					job = &p.Spec.Crons[i]
				}
			}
			if job == nil {
				return exitf(CodeUsage, "aucune tâche %q déclarée dans %s", args[1], p.ID())
			}
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			_, jobs, err := provs.Cron.Build(p)
			if err != nil {
				return wrap(CodeValidation, err)
			}
			var command string
			for _, j := range jobs {
				if j.Name == args[1] {
					command = j.Command
				}
			}
			if command == "" {
				return exitf(CodeError, "commande introuvable pour %s", args[1])
			}
			app.printf("exécution sous l'identité %s : %s\n", p.Spec.User.Name, command)
			if app.Exec.Simulated() {
				app.printf("= mode simulé : la commande n'est pas exécutée\n")
				return nil
			}
			// Cron place la tâche dans le home de l'utilisateur : s'exécuter ailleurs
			// donnerait un faux résultat, dans un sens comme dans l'autre.
			res, err := app.Exec.Run(cmd.Context(), exec.Command{
				Name: "/bin/sh", Args: []string{"-c", command}, AsUser: p.Spec.User.Name,
				Dir: p.Spec.Layout.Root,
			})
			if res.Stdout != "" {
				fmt.Fprint(app.Out, res.Stdout)
			}
			if err != nil {
				return exitf(CodeError, "%v", err)
			}
			_ = app.Audit.Record(p.ID(), "cron/"+args[1], "cron.run", "ok", "")
			app.printf("✓ tâche %s exécutée\n", args[1])
			return nil
		},
	}
}
