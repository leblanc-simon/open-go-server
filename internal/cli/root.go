package cli

import (
	"github.com/spf13/cobra"

	"github.com/leviathan/ogs/internal/version"
)

// NewRootCommand construit l'arbre des commandes.
func NewRootCommand() *cobra.Command {
	app := newApp()

	root := &cobra.Command{
		Use:   "ogs",
		Short: "Administration de serveurs web : projets, bases, files, PHP, TLS, services",
		Long: "ogs industrialise le cycle de vie des sites hébergés : utilisateur système,\n" +
			"bases de données, files RabbitMQ, pools PHP-FPM, vhosts, certificats TLS,\n" +
			"tâches planifiées et services systemd, décrits par un fichier YAML et appliqués\n" +
			"par un moteur de réconciliation idempotent.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			// Les sorties suivent celles de cobra, ce qui permet de les capturer en test.
			app.Out, app.Err = cmd.OutOrStdout(), cmd.ErrOrStderr()
			if cmd.Annotations["skip-setup"] == "true" {
				return nil
			}
			return app.setup()
		},
		PersistentPostRun: func(*cobra.Command, []string) { app.close() },
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	f := root.PersistentFlags()
	f.StringVar(&app.Prefix, "prefix", "", "préfixe appliqué à tous les chemins système (essai sans privilèges)")
	f.StringVarP(&app.Format, "output", "o", "table", "format de sortie : table, json ou yaml")
	f.BoolVar(&app.Yes, "yes", false, "ne pas demander de confirmation")
	f.BoolVar(&app.DryRun, "dry-run", false, "calculer le plan sans rien appliquer")
	f.BoolVar(&app.Force, "force", false, "passer outre les protections (fichiers modifiés à la main)")
	f.BoolVar(&app.NonInteractive, "non-interactive", false, "échouer plutôt que de poser une question")
	f.BoolVarP(&app.Quiet, "quiet", "q", false, "limiter la sortie à l'essentiel")
	f.CountVarP(&app.Verbose, "verbose", "v", "augmenter le niveau de détail (répétable)")
	f.BoolVar(&app.NoHooks, "no-hooks", false, "ne pas exécuter les hooks utilisateur")

	root.AddCommand(
		newVersionCommand(app),
		newInitCommand(app),
		newDoctorCommand(app),
		newProjectCommand(app),
		newPlanCommand(app),
		newApplyCommand(app),
		newDiffCommand(app),
	)
	return root
}

func newVersionCommand(app *App) *cobra.Command {
	var licence bool
	cmd := &cobra.Command{
		Use:         "version",
		Short:       "Afficher la version",
		Annotations: map[string]string{"skip-setup": "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if licence {
				cmd.Println(version.Licence)
				return nil
			}
			cmd.Println(version.String())
			return nil
		},
	}
	cmd.Flags().BoolVar(&licence, "license", false, "afficher la licence et l'avertissement de garantie")
	return cmd
}
