package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/amqp"
	"leblanc.io/open-go-server/internal/spec"
)

func newAMQPCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "amqp",
		Aliases: []string{"rabbit", "rabbitmq"},
		Short:   "Gérer les vhosts, files et échanges RabbitMQ",
	}
	creds := &cobra.Command{Use: "credentials", Short: "Gérer les identifiants d'administration"}
	creds.AddCommand(newAMQPCredentialsSetCommand(app))
	queue := &cobra.Command{Use: "queue", Short: "Gérer les files"}
	queue.AddCommand(newAMQPQueueAddCommand(app), newAMQPQueuePurgeCommand(app))
	cmd.AddCommand(
		newAMQPStatusCommand(app),
		newAMQPMessengerCommand(app),
		queue,
		creds,
	)
	return cmd
}

func newAMQPCredentialsSetCommand(app *App) *cobra.Command {
	var (
		url           string
		user          string
		password      string
		amqpPort      int
		brokerAccount string
	)
	cmd := &cobra.Command{
		Use:   "set --user <compte>",
		Short: "Enregistrer les identifiants d'administration du broker",
		Long: "Les identifiants sont écrits en 0600 sous /etc/ogs/credentials/. Ils donnent\n" +
			"un accès d'administration complet au broker : les traiter comme un secret de\n" +
			"premier rang. Ne pas employer le compte « guest », restreint à la boucle locale\n" +
			"et connu de tous.\n\n" +
			"Le mot de passe est demandé sans écho sur un terminal, et lu sur l'entrée\n" +
			"standard sinon (une ligne) : « printf '%s\\n' \"$mdp\" | ogs amqp credentials set\n" +
			"--user ogs-admin ». L'option --password, déconseillée, le laisse dans le journal\n" +
			"de sudo et l'historique du shell.\n\n" +
			"Vers cette machine, les identifiants ne partent que vers un port tenu par\n" +
			"root ou par le compte du broker (--broker-account, nom ou uid) : broker arrêté,\n" +
			"tout compte local peut ouvrir le port de management. Un broker en conteneur\n" +
			"publié par DNAT (Docker sans userland-proxy) se désigne par son adresse (--url).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if user == "" {
				return exitf(CodeUsage, "--user est requis")
			}
			password, err := app.saisieSecrets(cmd).valeur("password", password,
				"mot de passe du compte "+user)
			if err != nil {
				return err
			}
			if user == "guest" {
				app.warnf("⚠ le compte « guest » est un compte par défaut connu : " +
					"créer un compte d'administration dédié est préférable.\n")
			}
			if brokerAccount == amqp.DefaultBrokerAccount {
				brokerAccount = ""
			}
			creds := amqp.Credentials{URL: url, User: user, Password: password, AMQPPort: amqpPort,
				BrokerAccount: brokerAccount}
			data, err := yaml.Marshal(creds)
			if err != nil {
				return wrap(CodeError, err)
			}
			path := filepath.Join(app.Paths.Etc(), "credentials", "amqp.yaml")
			if err := fs.EnsureDir(filepath.Dir(path), 0o700, fs.KeepOwner); err != nil {
				return wrap(CodeError, err)
			}
			header := []byte("# Identifiants d'administration RabbitMQ (API management).\n")
			if err := fs.WriteAtomic(path, append(header, data...), 0o600, fs.KeepOwner); err != nil {
				return wrap(CodeError, err)
			}
			if err := app.openStore(); err == nil {
				_ = app.Audit.Record("", "amqp/credentials", "amqp.credentials.set", "ok", "")
			}
			app.printf("✓ identifiants enregistrés : %s (mode 0600)\n", path)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&url, "url", amqp.DefaultManagementURL, "URL de l'API management")
	f.StringVar(&user, "user", "", "compte d'administration")
	f.StringVar(&password, "password", "",
		"mot de passe (déconseillé : visible dans le journal de sudo ; l'omettre pour le saisir)")
	f.IntVar(&amqpPort, "amqp-port", 5672, "port du protocole AMQP, pour les chaînes de connexion")
	f.StringVar(&brokerAccount, "broker-account", amqp.DefaultBrokerAccount,
		"compte système du broker, par son nom ou son uid : seul admis avec root à tenir le port de management "+
			"sur cette machine")
	return cmd
}

// queueRow est la vue machine d'une file observée.
type queueRow struct {
	Project   string `json:"project" yaml:"project"`
	Vhost     string `json:"vhost" yaml:"vhost"`
	Name      string `json:"name" yaml:"name"`
	Type      string `json:"type" yaml:"type"`
	Messages  int    `json:"messages" yaml:"messages"`
	Consumers int    `json:"consumers" yaml:"consumers"`
}

func newAMQPStatusCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "status [id...]",
		Aliases: []string{"list", "ls"},
		Short:   "Afficher l'état des vhosts et des files",
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

			var rows []queueRow
			var warnings []string
			for _, p := range projects {
				if p.Spec.AMQP == nil {
					continue
				}
				obs, err := provs.AMQP.Observe(cmd.Context(), p)
				if err != nil {
					return wrap(CodeError, err)
				}
				if obs.Unavailable != nil {
					warnings = append(warnings, fmt.Sprintf("%s : %v", p.ID(), obs.Unavailable))
					continue
				}
				// Les files d'un homonyme ne sont pas celles du projet : les
				// afficher sous son nom le ferait croire (D93).
				if !obs.Owned() {
					warnings = append(warnings, fmt.Sprintf("%s : vhost %s non créé par ogs (%s), files non "+
						"affichées", p.ID(), p.Spec.AMQP.Vhost, amqp.ForeignReason(p.Spec.AMQP, obs)))
					continue
				}
				for _, name := range sortedQueueNames(obs.Queues) {
					q := obs.Queues[name]
					rows = append(rows, queueRow{
						Project: p.ID(), Vhost: q.Vhost, Name: q.Name,
						Type: q.EffectiveType(), Messages: q.Messages, Consumers: q.Consumers,
					})
				}
			}
			return wrap(CodeError, app.emit("QueueList", rows, warnings, func(tw *tabwriter.Writer) {
				if len(rows) == 0 {
					fmt.Fprintln(app.Out, "aucune file observée")
					return
				}
				fmt.Fprintln(tw, "PROJET\tVHOST\tFILE\tTYPE\tMESSAGES\tCONSUMERS")
				for _, r := range rows {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\n",
						r.Project, r.Vhost, r.Name, r.Type, r.Messages, r.Consumers)
				}
			}))
		},
	}
}

func sortedQueueNames(queues map[string]amqp.Queue) []string {
	out := make([]string, 0, len(queues))
	for name := range queues {
		out = append(out, name)
	}
	// Tri simple : l'ordre d'affichage doit être stable d'un appel à l'autre.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func newAMQPMessengerCommand(app *App) *cobra.Command {
	var (
		transports []string
		noFailed   bool
	)
	cmd := &cobra.Command{
		Use:   "messenger <id>",
		Short: "Déclarer le transport Symfony Messenger d'un projet",
		Long: "Produit la configuration attendue par le transport AMQP de Messenger : un\n" +
			"exchange direct, une file par transport, un binding par clé de routage et une\n" +
			"file d'échec pour rejouer les messages rejetés.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			at, err := p.AppType()
			if err != nil {
				return exitf(CodeValidation, "%s", err)
			}
			// Messenger n'existe qu'à partir de Symfony 4.1 : le proposer sur un
			// projet en 3.4 produirait une configuration inutilisable.
			if p.Spec.App.Kind == model.KindSymfony &&
				!model.SupportsMessenger(p.Spec.App.Kind, p.Spec.App.Version) {
				return exitf(CodeUsage, "Symfony %s n'a pas le composant Messenger "+
					"(introduit en 4.1) : employer le template rabbitmq-consumer", p.Spec.App.Version)
			}
			_ = at
			p.Spec.AMQP = amqp.Preset(p, transports, !noFailed)
			if err := app.prepare(p); err != nil {
				return err
			}
			// Le vhost AMQP est une clé d'unicité : un voisin a pu le déclarer.
			if err := app.checkFleetUniqueness(p); err != nil {
				return err
			}
			if app.DryRun {
				data, err := spec.Encode(p)
				if err != nil {
					return wrap(CodeError, err)
				}
				fmt.Fprint(app.Out, string(data))
				return nil
			}
			if err := spec.Save(app.Paths.ProjectFile(p.ID()), p); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ transport AMQP déclaré pour %s\n", p.ID())
			app.printf("  vhost      %s\n", p.Spec.AMQP.Vhost)
			app.printf("  files      %s\n", strings.Join(queueNames(p.Spec.AMQP), ", "))
			app.printf("\nAppliquer :\n  ogs apply %s\n", p.ID())
			return nil
		},
	}
	f := cmd.Flags()
	f.StringSliceVar(&transports, "transport", []string{"async"}, "transports à déclarer (répétable)")
	f.BoolVar(&noFailed, "no-failed", false, "ne pas créer les files d'échec")
	return cmd
}

func queueNames(spec *model.AMQP) []string {
	out := make([]string, 0, len(spec.Queues))
	for _, q := range spec.Queues {
		out = append(out, q.Name)
	}
	return out
}

func newAMQPQueueAddCommand(app *App) *cobra.Command {
	var (
		queueType string
		durable   bool
		ttl       int
		dlx       string
		exchange  string
		key       string
	)
	cmd := &cobra.Command{
		Use:   "add <id> <file>",
		Short: "Déclarer une file supplémentaire",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			if p.Spec.AMQP == nil {
				return exitf(CodeUsage, "le projet %s ne déclare pas de transport AMQP : "+
					"commencer par « ogs amqp messenger %s »", p.ID(), p.ID())
			}
			name := args[1]
			for _, q := range p.Spec.AMQP.Queues {
				if q.Name == name {
					return exitf(CodeConflict, "la file %s est déjà déclarée", name)
				}
			}
			queue := model.Queue{Name: name, Type: queueType, Durable: durable,
				Arguments: map[string]any{}}
			if ttl > 0 {
				queue.Arguments["x-message-ttl"] = ttl
			}
			if dlx != "" {
				queue.Arguments["x-dead-letter-exchange"] = dlx
			}
			p.Spec.AMQP.Queues = append(p.Spec.AMQP.Queues, queue)
			if exchange != "" {
				p.Spec.AMQP.Bindings = append(p.Spec.AMQP.Bindings, model.Binding{
					Exchange: exchange, Queue: name, RoutingKey: orValue(key, name),
				})
			}
			if err := app.prepare(p); err != nil {
				return err
			}
			if err := spec.Save(app.Paths.ProjectFile(p.ID()), p); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ file %s déclarée (%s)\n", name, queueType)
			app.printf("  les arguments d'une file sont immuables : les modifier ensuite exigera " +
				"« --recreate-queues », ce qui détruit les messages en attente.\n")
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&queueType, "type", "quorum", "type de file : classic ou quorum")
	f.BoolVar(&durable, "durable", true, "file durable")
	f.IntVar(&ttl, "ttl", 0, "durée de vie des messages, en millisecondes")
	f.StringVar(&dlx, "dlx", "", "exchange de rejet")
	f.StringVar(&exchange, "exchange", amqp.DefaultExchange, "exchange à lier")
	f.StringVar(&key, "routing-key", "", "clé de routage (défaut : nom de la file)")
	return cmd
}

func newAMQPQueuePurgeCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "purge <id> <file>",
		Short: "Vider une file",
		Long:  "Supprime tous les messages en attente. L'opération est irréversible.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			if p.Spec.AMQP == nil {
				return exitf(CodeUsage, "le projet %s ne déclare pas de transport AMQP", p.ID())
			}
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			obs, err := provs.AMQP.Observe(cmd.Context(), p)
			if err != nil {
				return wrap(CodeError, err)
			}
			if obs.Unavailable != nil {
				return exitf(CodeDependency, "%v", obs.Unavailable)
			}
			// Vider la file d'un vhost qu'ogs n'a pas créé, c'était détruire les
			// messages de son propriétaire (D93).
			if !obs.Owned() {
				return exitf(CodeConflict, "vhost amqp %s : %s — ogs n'y vide aucune file",
					p.Spec.AMQP.Vhost, amqp.ForeignReason(p.Spec.AMQP, obs))
			}
			queue, ok := obs.Queues[args[1]]
			if !ok {
				return exitf(CodeUsage, "aucune file %q dans le vhost %s", args[1], p.Spec.AMQP.Vhost)
			}
			// Le nombre de messages est annoncé avant confirmation : c'est ce que
			// l'opération va détruire.
			if err := app.confirm(fmt.Sprintf(
				"vider la file %s (%d message(s) seront perdus) ?", queue.Name, queue.Messages)); err != nil {
				return err
			}
			if err := provs.AMQP.Client.PurgeQueue(cmd.Context(), p.Spec.AMQP.Vhost, queue.Name); err != nil {
				return wrap(CodeError, err)
			}
			_ = app.Audit.Record(p.ID(), "amqp/queue/"+queue.Name, "amqp.queue.purge", "ok",
				fmt.Sprintf("%d message(s)", queue.Messages))
			app.printf("✓ file %s vidée (%d message(s) supprimés)\n", queue.Name, queue.Messages)
			return nil
		},
	}
}
