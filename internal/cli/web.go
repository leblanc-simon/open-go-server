package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/provider/system"
)

func newWebCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{Use: "web", Short: "Gérer le serveur web"}
	cmd.AddCommand(newWebEngineCommand(app), newWebTestCommand(app), newWebReloadCommand(app))
	return cmd
}

func newWebEngineCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "engine",
		Short: "Afficher le moteur web piloté",
		RunE: func(cmd *cobra.Command, _ []string) error {
			eng, err := app.webEngine()
			if err != nil {
				return err
			}
			info := map[string]any{
				"engine":    eng.Name(),
				"available": eng.AvailableDir(),
				"enabled":   eng.EnabledDir(),
				"unit":      eng.Unit(),
				"installed": eng.Installed(),
			}
			return wrap(CodeError, app.emit("WebEngine", info, nil, func(tw *tabwriter.Writer) {
				fmt.Fprintf(tw, "moteur\t%s\n", eng.Name())
				fmt.Fprintf(tw, "installé\t%v\n", eng.Installed())
				fmt.Fprintf(tw, "configurations\t%s\n", eng.AvailableDir())
				fmt.Fprintf(tw, "actives\t%s\n", eng.EnabledDir())
				fmt.Fprintf(tw, "unité\t%s\n", eng.Unit())
			}))
		},
	}
}

func newWebTestCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "test",
		Short: "Vérifier la configuration du serveur web",
		RunE: func(cmd *cobra.Command, _ []string) error {
			eng, err := app.webEngine()
			if err != nil {
				return err
			}
			bin, args := eng.ValidateArgs()
			if app.Exec.Simulated() {
				app.printf("= mode simulé : %s %v n'est pas exécuté\n", bin, args)
				return nil
			}
			res, err := app.Exec.Run(cmd.Context(), execCommand(bin, args))
			if err != nil {
				return exitf(CodeValidation, "%v", err)
			}
			app.printf("✓ configuration %s valide\n", eng.Name())
			if app.Verbose > 0 && res.Stderr != "" {
				fmt.Fprint(app.Out, res.Stderr)
			}
			return nil
		},
	}
}

func newWebReloadCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Recharger le serveur web",
		Long: "Un rechargement suffit à prendre en compte une nouvelle appartenance de groupe :\n" +
			"les workers rappellent initgroups() à chaque fork.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			provs, err := app.buildProviders()
			if err != nil {
				return err
			}
			if err := provs.Web.Reload(cmd.Context()); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ %s rechargé\n", provs.Engine.Unit())
			return nil
		},
	}
}

func newUserCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{Use: "user", Short: "Gérer les utilisateurs de projet"}
	key := &cobra.Command{Use: "key", Short: "Gérer la clé de déploiement"}
	key.AddCommand(newUserKeyShowCommand(app), newUserKeyRotateCommand(app))
	cmd.AddCommand(key)
	return cmd
}

func newUserKeyShowCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Afficher la clé publique de déploiement",
		Long: "Seule la partie publique est restituée : la clé privée reste sur le serveur et\n" +
			"n'est exposée par aucune commande. Cette clé publique est à déclarer en deploy\n" +
			"key, en lecture seule, sur le dépôt du projet.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			pub, err := system.PublicKey(p.Spec.Layout.Root)
			if err != nil {
				return exitf(CodeUsage, "%s", err)
			}
			if app.Format != "table" {
				return wrap(CodeError, app.emit("DeployKey",
					map[string]string{"project": p.ID(), "publicKey": pub}, nil, nil))
			}
			fmt.Fprint(app.Out, pub)
			app.printf("→ à déclarer en deploy key (lecture seule) sur le dépôt du projet\n")
			return nil
		},
	}
}

func newUserKeyRotateCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "rotate <id>",
		Short: "Régénérer la clé de déploiement",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			if err := app.confirm(fmt.Sprintf("régénérer la clé de déploiement de %s ?", p.ID())); err != nil {
				return err
			}
			provs, err := app.buildProviders()
			if err != nil {
				return err
			}
			old, err := system.RotateKey(cmd.Context(), app.Exec, p, provs.System.Identity)
			if err != nil {
				return wrap(CodeError, err)
			}
			if err := app.openStore(); err == nil {
				_ = app.Audit.Record(p.ID(), "system/deploy-key/"+p.ID(), "user.key.rotate", "ok", "")
			}
			pub, err := system.PublicKey(p.Spec.Layout.Root)
			if err == nil {
				fmt.Fprint(app.Out, pub)
			}
			if old != "" {
				app.printf("\nAncienne clé publique, à retirer chez l'hébergeur du dépôt :\n%s", old)
			}
			return nil
		},
	}
}
