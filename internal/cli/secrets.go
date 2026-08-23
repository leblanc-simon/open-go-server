package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newSecretsCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{Use: "secrets", Short: "Consulter et renouveler les secrets"}
	cmd.AddCommand(newSecretsListCommand(app), newSecretsShowCommand(app), newSecretsEnvCommand(app))
	return cmd
}

func newSecretsListCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list <id>",
		Aliases: []string{"ls"},
		Short:   "Lister les secrets d'un projet, sans leur valeur",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			if err := app.openStore(); err != nil {
				return err
			}
			names, err := app.Store.SecretNames(p.ID())
			if err != nil {
				return wrap(CodeError, err)
			}
			return wrap(CodeError, app.emit("SecretList", names, nil, func(tw *tabwriter.Writer) {
				if len(names) == 0 {
					fmt.Fprintf(app.Out, "aucun secret pour %s\n", p.ID())
					return
				}
				fmt.Fprintln(tw, "SECRET")
				for _, n := range names {
					fmt.Fprintf(tw, "%s\n", n)
				}
			}))
		},
	}
}

func newSecretsShowCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id> <secret>",
		Short: "Afficher la valeur d'un secret",
		Long: "Seule voie de divulgation d'un secret, et elle est journalisée : le nom du\n" +
			"secret consulté est consigné à l'audit, jamais sa valeur.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			if err := app.openStore(); err != nil {
				return err
			}
			if app.Keys == nil {
				return exitf(CodeDependency, "keystore indisponible : vérifier %s", app.Paths.MasterKey())
			}
			name := args[1]
			nonce, ciphertext, err := app.Store.GetSecret(p.ID(), name)
			if err != nil {
				return exitf(CodeUsage, "%v", err)
			}
			plain, err := app.Keys.Open(p.ID(), name, nonce, ciphertext)
			if err != nil {
				return wrap(CodeError, err)
			}
			_ = app.Audit.Record(p.ID(), "secret/"+name, "secrets.show", "ok", "")
			fmt.Fprintln(app.Out, string(plain))
			return nil
		},
	}
}

func newSecretsEnvCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "env <id>",
		Short: "Émettre les chaînes de connexion applicatives",
		Long: "Produit les variables d'environnement prêtes à être reprises dans le .env de\n" +
			"l'application : DATABASE_URL pour la base principale, MESSENGER_TRANSPORT_DSN\n" +
			"pour le transport AMQP.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
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
			entries, err := provs.DB.Env(cmd.Context(), p)
			if err != nil {
				return exitf(CodeDependency, "%v", err)
			}
			if len(entries) == 0 {
				app.printf("aucune chaîne de connexion : le projet ne déclare ni base ni transport\n")
				return nil
			}
			_ = app.Audit.Record(p.ID(), "secret/env", "secrets.env", "ok", "")
			if app.Format != "table" {
				return wrap(CodeError, app.emit("EnvList", entries, nil, nil))
			}
			for _, e := range entries {
				fmt.Fprintf(app.Out, "%s=%q\n", e.Name, e.Value)
			}
			return nil
		},
	}
}
