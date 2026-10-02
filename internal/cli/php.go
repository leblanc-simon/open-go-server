package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/hook"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/php"
	"leblanc.io/open-go-server/internal/spec"
)

func newPHPCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{Use: "php", Short: "Gérer les versions et les pools PHP-FPM"}
	cmd.AddCommand(newPHPListCommand(app), newPHPPortsCommand(app), newPHPSwitchCommand(app))
	return cmd
}

func newPHPListCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "Lister les versions PHP détectées et leurs pools",
		RunE: func(cmd *cobra.Command, _ []string) error {
			versions := php.Detect(app.Paths)
			return wrap(CodeError, app.emit("PHPVersionList", versions, nil, func(tw *tabwriter.Writer) {
				if len(versions) == 0 {
					fmt.Fprintf(app.Out, "aucune version PHP détectée sous %s\n", app.Paths.PHPRoot())
					return
				}
				fmt.Fprintln(tw, "VERSION\tÉTAT\tPOOLS\tSUPPORT")
				for _, v := range versions {
					state := "utilisable"
					if !v.Usable {
						state = "inutilisable"
					}
					support := "actif"
					if v.EOL != "" {
						support = "terminé le " + v.EOL
					}
					fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", v.Version, state, v.Pools, support)
				}
			}))
		},
	}
}

func newPHPPortsCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "ports",
		Short: "Afficher les numéros attribués aux pools",
		Long: "Chaque pool reçoit un numéro suivant la norme 1<version><incrément> : php 8.5,\n" +
			"premier pool → 18501. Le numéro figure dans le nom du socket et sert de port\n" +
			"lorsqu'un pool écoute en TCP.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := app.openStore(); err != nil {
				return err
			}
			allocations, err := app.Store.Allocations("")
			if err != nil {
				return wrap(CodeError, err)
			}
			return wrap(CodeError, app.emit("AllocationList", allocations, nil, func(tw *tabwriter.Writer) {
				if len(allocations) == 0 {
					fmt.Fprintln(app.Out, "aucun numéro attribué")
					return
				}
				fmt.Fprintln(tw, "NUMÉRO\tPLAGE\tPROJET\tPOOL")
				for _, a := range allocations {
					fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", a.Number, a.Scope, a.Project, a.Name)
				}
			}))
		},
	}
}

func newPHPSwitchCommand(app *App) *cobra.Command {
	var poolName string
	cmd := &cobra.Command{
		Use:   "switch <id> <version>",
		Short: "Changer la version PHP d'un projet",
		Long: "Modifie la version du pool, puis applique : nouveau pool, vhost réécrit sur le\n" +
			"nouveau socket, lien ~/.local/bin/php mis à jour, ancien pool retiré ensuite. Les\n" +
			"fichiers cron et les unités systemd ne sont pas réécrits : ils invoquent ce lien.\n\n" +
			"La compatibilité est vérifiée d'après la contrainte PHP déclarée par l'application\n" +
			"dans son composer.json. Une bascule incompatible est refusée sans --force.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, version := args[0], args[1]
			if !php.IsSupported(version) {
				return exitf(CodeUsage, "version PHP %q hors périmètre (gérées : %v)", version, php.Supported)
			}
			p, err := app.loadProject(id)
			if err != nil {
				return err
			}
			if len(p.Spec.PHP.Pools) == 0 {
				return exitf(CodeUsage, "le projet %s n'a pas de pool PHP", id)
			}
			target := poolName
			if target == "" {
				target = p.Spec.PHP.Pools[0].Name
			}
			idx := -1
			for i, pool := range p.Spec.PHP.Pools {
				if pool.Name == target {
					idx = i
				}
			}
			if idx < 0 {
				return exitf(CodeUsage, "pool %q non déclaré dans %s", target, id)
			}
			from := p.Spec.PHP.Pools[idx].Version
			if from == version {
				app.printf("= %s : le pool %s est déjà en PHP %s\n", id, target, version)
				return nil
			}

			// Garde-fou de compatibilité : c'est la vérification la plus utile du
			// module PHP. Passer un Symfony 3.4 en PHP 8 rend le site inaccessible
			// immédiatement.
			verdict, err := php.Check(p.Spec.Layout.Root, string(p.Spec.App.Kind), version)
			if err != nil {
				return wrap(CodeError, err)
			}
			ok, detail := verdict.OK, verdict.Detail
			if !ok {
				if !app.Force {
					return exitf(CodeValidation,
						"PHP %s est incompatible avec l'application (%s) ; "+
							"ajouter --force pour l'imposer malgré tout", version, detail)
				}
				app.warnf("⚠ compatibilité forcée : %s\n", detail)
				if app.Audit != nil {
					_ = app.Audit.Record(id, "php/pool/"+target, "php.switch.force", "avertissement", detail)
				}
			} else if verdict.Unread != nil {
				// Rien n'a été vérifié sur l'application elle-même : un ✓ le laisserait croire.
				app.warnf("⚠ composer.json non lu, compatibilité jugée sans lui : %s\n", detail)
			} else {
				app.printf("✓ compatibilité vérifiée : %s\n", detail)
			}

			p.Spec.PHP.Pools[idx].Version = version
			// Le numéro est propre à la version : il doit être réattribué.
			p.Spec.PHP.Pools[idx].Number = "auto"
			if app.DryRun {
				app.printf("--dry-run : le pool %s passerait de PHP %s à PHP %s\n", target, from, version)
				return nil
			}
			if err := spec.Save(app.Paths.ProjectFile(p.ID()), p); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ %s : pool %s, PHP %s → %s\n", id, target, from, version)
			if err := app.openStore(); err == nil {
				_ = app.Audit.Record(id, "php/pool/"+target, "php.switch", "ok", from+" → "+version)
			}
			// La bascule n'est encore que déclarée : le hook signale l'intention, à
			// charge pour lui de suivre l'apply s'il veut constater l'effet.
			_, _ = app.runHook(cmd.Context(), app.hookRunner(), p, hook.PostPHPSwitch,
				hook.Context{Extra: map[string]string{"php_from": from, "php_to": version}})
			app.printf("\nAppliquer la bascule :\n  ogs apply %s\n", id)
			return nil
		},
	}
	cmd.Flags().StringVar(&poolName, "pool", "", "pool visé (défaut : le pool principal "+model.MainPoolName+")")
	return cmd
}
