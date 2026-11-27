package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/unit"
)

// brokerServiceRow est la vue machine d'un projet visé par « amqp stop|start ».
type brokerServiceRow struct {
	Project  string   `json:"project" yaml:"project"`
	Services []string `json:"services" yaml:"services"`
	Units    []string `json:"units" yaml:"units"`
	Error    string   `json:"error,omitempty" yaml:"error,omitempty"`
}

// newAMQPServicesCommand arrête ou démarre, sur le parc ou les projets nommés,
// les consommateurs du broker : services Messenger (Symfony 4.1 et plus) et
// rabbitmq:consumer (Symfony avant 4.1, D128). Les autres services, la cible et
// l'application supervisée ne sont pas touchés : c'est le geste qui précède une
// maintenance du broker, ou qui arrête la consommation sans couper les sites.
func newAMQPServicesCommand(app *App, verb string) *cobra.Command {
	short := map[string]string{
		"stop":  "Arrêter les consommateurs du broker (Messenger, rabbitmq:consumer) du parc",
		"start": "Démarrer les consommateurs du broker (Messenger, rabbitmq:consumer) du parc",
	}[verb]
	long := "Vise les services déclarés sous les templates " + unit.TemplateMessenger + " (Symfony 4.1\n" +
		"et plus) et " + unit.TemplateConsumer + " (Symfony avant 4.1), de tous les projets, ou des\n" +
		"seuls projets nommés. Les autres services, l'application supervisée et les sites\n" +
		"ne sont pas touchés ; les messages restent dans leurs files.\n\n"
	if verb == "stop" {
		long += "L'arrêt tient jusqu'à « ogs amqp start » ou « ogs service start <id> » :\n" +
			"un apply ne relance pas un service arrêté. Un redémarrage de la machine, si :\n" +
			"la cible de chaque projet tire ses services au démarrage."
	} else {
		long += "Seules les instances activées sont démarrées : un service désactivé\n" +
			"(enabled: false) ou réduit par « service scale » le reste."
	}
	return &cobra.Command{
		Use:   verb + " [id...]",
		Short: short,
		Long:  long,
		RunE: func(cmd *cobra.Command, args []string) error {
			var (
				projects []*model.Project
				refused  int
				err      error
			)
			if len(args) == 0 {
				// Un voisin refusé n'empêche pas d'arrêter les autres (D120) : il est
				// nommé, et compté dans le code de retour — ses workers tournent encore.
				projects, refused, err = app.loadFleetNamed()
			} else {
				projects, err = app.targets(args, "")
			}
			if err != nil {
				return err
			}
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()

			ctx := cmd.Context()
			var rows []brokerServiceRow
			failed := 0
			for _, p := range projects {
				services := unit.BrokerServices(p)
				if len(services) == 0 {
					continue
				}
				row := brokerServiceRow{Project: p.ID(), Services: services}
				var units []string
				switch {
				case app.DryRun && verb == "stop":
					units, err = provs.Unit.RunningServices(ctx, p.ID(), services)
				case app.DryRun:
					units = provs.Unit.EnabledServices(p.ID(), services)
				case verb == "stop":
					units, err = provs.Unit.StopServices(ctx, p.ID(), services)
				default:
					units, err = provs.Unit.StartServices(ctx, p.ID(), services)
				}
				row.Units = units
				if row.Units == nil {
					row.Units = []string{}
				}
				result := "ok"
				if err != nil {
					failed++
					row.Error = err.Error()
					result = "échec"
				}
				if !app.DryRun && len(units) > 0 {
					_ = app.Audit.Record(p.ID(), "unit/broker", "amqp."+verb, result, strings.Join(units, " "))
				}
				rows = append(rows, row)
			}

			err = app.emit("AMQPServices", rows, nil, func(tw *tabwriter.Writer) {
				if len(rows) == 0 {
					fmt.Fprintln(app.Out, "aucun service Messenger ni rabbitmq:consumer déclaré")
					return
				}
				done := map[string]string{"stop": "arrêté", "start": "démarré"}[verb]
				if app.DryRun {
					done = map[string]string{"stop": "serait arrêté", "start": "serait démarré"}[verb]
				}
				for _, r := range rows {
					switch {
					case r.Error != "":
						fmt.Fprintf(tw, "✗ %s\t%s\n", r.Project, r.Error)
					case len(r.Units) == 0 && verb == "stop":
						fmt.Fprintf(tw, "= %s\taucune instance active (%s)\n", r.Project, strings.Join(r.Services, ", "))
					case len(r.Units) == 0:
						fmt.Fprintf(tw, "= %s\taucune instance activée (%s)\n", r.Project, strings.Join(r.Services, ", "))
					default:
						for _, u := range r.Units {
							fmt.Fprintf(tw, "✓ %s\t%s\t%s\n", r.Project, u, done)
						}
					}
				}
			})
			if err != nil {
				return wrap(CodeError, err)
			}
			switch {
			case failed > 0 && failed == len(rows):
				return exitf(CodeError, "%s des consommateurs en échec pour chaque projet visé", verbNoun(verb))
			case failed > 0:
				return exitf(CodePartial, "%s des consommateurs en échec pour %d projet(s) sur %d",
					verbNoun(verb), failed, len(rows))
			case refused > 0:
				return exitf(CodePartial, "%d déclaration(s) refusée(s) : leurs consommateurs n'ont pas été "+
					"visés (ogs doctor)", refused)
			}
			return nil
		},
	}
}

func verbNoun(verb string) string {
	if verb == "stop" {
		return "arrêt"
	}
	return "démarrage"
}
