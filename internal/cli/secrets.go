package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	appprov "leblanc.io/open-go-server/internal/provider/app"
	"leblanc.io/open-go-server/internal/provider/monitoring"
)

// secretScope traduit l'argument des commandes de secrets en portée de keystore.
// Les mots de passe des comptes de supervision n'appartiennent à aucun projet :
// ils vivent sous une portée de parc, dont le nom est volontairement impossible
// à porter par un projet (voir monitoring.SecretScope).
func secretScope(app *App, arg string) (string, error) {
	if arg == monitoring.SecretScope {
		return monitoring.SecretScope, nil
	}
	p, err := app.loadProject(arg)
	if err != nil {
		return "", err
	}
	return p.ID(), nil
}

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
		Long: "L'identifiant « " + monitoring.SecretScope + " » désigne les secrets de portée parc :\n" +
			"les mots de passe des comptes de supervision, qui n'appartiennent à aucun projet.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			scope, err := secretScope(app, args[0])
			if err != nil {
				return err
			}
			if err := app.openStore(); err != nil {
				return err
			}
			names, err := app.Store.SecretNames(scope)
			if err != nil {
				return wrap(CodeError, err)
			}
			return wrap(CodeError, app.emit("SecretList", names, nil, func(tw *tabwriter.Writer) {
				if len(names) == 0 {
					fmt.Fprintf(app.Out, "aucun secret pour %s\n", scope)
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
			scope, err := secretScope(app, args[0])
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
			nonce, ciphertext, err := app.Store.GetSecret(scope, name)
			if err != nil {
				return exitf(CodeUsage, "%v", err)
			}
			plain, err := app.Keys.Open(scope, name, nonce, ciphertext)
			if err != nil {
				return wrap(CodeError, err)
			}
			_ = app.Audit.Record(scope, "secret/"+name, "secrets.show", "ok", "")
			// Le fichier d'environnement gardé par une restauration (D104) est
			// rendu tel quel, octet pour octet : le remède le redirige vers son
			// fichier, et un saut de ligne ajouté en ferait un autre fichier.
			if name == appprov.EnvFileSecret {
				_, err := app.Out.Write(plain)
				return wrap(CodeError, err)
			}
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
