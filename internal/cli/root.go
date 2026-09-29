package cli

import (
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"leblanc.io/open-go-server/internal/version"
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
			// La complétion est interrogée par le shell, sous l'identité de
			// l'utilisateur : elle ne peut ni exiger root ni ouvrir la base.
			if isCompletionRequest(cmd) {
				return nil
			}
			if cmd.Annotations["skip-setup"] == "true" {
				return nil
			}
			if cmd.Annotations["skip-root"] == "true" || parentAllowsNonRoot(cmd) {
				app.allowNonRoot = true
			}
			return app.setup()
		},
		PersistentPostRun: func(*cobra.Command, []string) { app.close() },
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Sans argument sur un terminal, l'inspecteur est plus utile que l'aide.
			if term.IsTerminal(int(os.Stdout.Fd())) {
				return newTUICommand(app).RunE(cmd, nil)
			}
			return cmd.Help()
		},
	}

	// Une option inconnue ou mal formée est une erreur d'usage (code 2), non une
	// erreur d'exécution : les sous-commandes héritent de ce traitement.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return wrap(CodeUsage, err)
	})

	f := root.PersistentFlags()
	f.StringVar(&app.Prefix, "prefix", "", "préfixe appliqué à tous les chemins système (essai sans privilèges)")
	f.StringVarP(&app.Format, "output", "o", "table", "format de sortie : table, json ou yaml")
	f.BoolVar(&app.Yes, "yes", false, "ne pas demander de confirmation")
	f.BoolVar(&app.DryRun, "dry-run", false, "calculer le plan sans rien appliquer")
	f.BoolVar(&app.Force, "force", false,
		"écraser ou retirer les fichiers modifiés à la main, après sauvegarde dans /var/lib/ogs/backups")
	f.BoolVar(&app.NonInteractive, "non-interactive", false, "échouer plutôt que de poser une question")
	f.BoolVarP(&app.Quiet, "quiet", "q", false, "limiter la sortie à l'essentiel")
	f.CountVarP(&app.Verbose, "verbose", "v", "augmenter le niveau de détail (répétable)")
	f.BoolVar(&app.NoHooks, "no-hooks", false, "ne pas exécuter les hooks utilisateur")
	f.BoolVar(&app.RecreateQueues, "recreate-queues", false,
		"autoriser la recréation des queues dont les arguments ont changé (détruit les messages en attente)")

	root.AddCommand(
		newVersionCommand(app),
		newInitCommand(app),
		newDoctorCommand(app),
		newHardenCommand(app),
		newProjectCommand(app),
		newPlanCommand(app),
		newApplyCommand(app),
		newDiffCommand(app),
		newPHPCommand(app),
		newWebCommand(app),
		newUserCommand(app),
		newTemplateCommand(app),
		newDBCommand(app),
		newSecretsCommand(app),
		newCertCommand(app),
		newAMQPCommand(app),
		newServiceCommand(app),
		newMyCommand(app),
		newCronCommand(app),
		newSiteCommand(app),
		newStatusCommand(app),
		newTUICommand(app),
		newImportCommand(app),
		newRuntimeCommand(app),
		newAppCommand(app),
		newMailCommand(app),
		newFirewallCommand(app),
	)
	// Le générateur de scripts de complétion n'a pas besoin du système.
	if c, _, err := root.Find([]string{"completion"}); err == nil && c != root {
		c.Annotations = map[string]string{"skip-setup": "true"}
	}
	installCompletions(root, app)
	localize(root)
	return root
}

// isCompletionRequest reconnaît les commandes internes par lesquelles le shell
// demande des propositions.
func isCompletionRequest(cmd *cobra.Command) bool {
	switch cmd.Name() {
	case cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
		return true
	}
	return false
}

// parentAllowsNonRoot propage la dérogation aux sous-commandes de « my ».
func parentAllowsNonRoot(cmd *cobra.Command) bool {
	for c := cmd.Parent(); c != nil; c = c.Parent() {
		if c.Annotations["skip-root"] == "true" {
			return true
		}
	}
	return false
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
