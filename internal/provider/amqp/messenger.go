package amqp

import (
	"fmt"

	"leblanc.io/open-go-server/internal/model"
)

// DefaultExchange est l'exchange dans lequel Symfony Messenger publie.
const DefaultExchange = "messages"

// DeadLetterExchange reçoit les messages rejetés.
const DeadLetterExchange = "dlx"

// FailedSuffix nomme la file d'échec associée à un transport.
const FailedSuffix = "_failed"

// Preset construit la déclaration AMQP d'un projet pour Symfony Messenger : un
// exchange direct, une queue par transport, un binding par clé de routage, et
// une file d'échec par transport si elle est demandée.
//
// C'est la configuration qu'attend le transport AMQP de Messenger ; la produire
// ici évite à l'exploitant de déduire les noms à la main, où une erreur ne se
// manifeste qu'au premier message perdu.
func Preset(proj *model.Project, transports []string, withFailed bool) *model.AMQP {
	if len(transports) == 0 {
		transports = []string{"async"}
	}
	spec := &model.AMQP{
		Vhost: proj.ID(),
		User:  proj.ID(),
		Permissions: map[string]string{
			"configure": ".*",
			"write":     ".*",
			"read":      ".*",
		},
		Exchanges: []model.Exchange{
			{Name: DefaultExchange, Type: "direct", Durable: true},
		},
	}
	if withFailed {
		spec.Exchanges = append(spec.Exchanges,
			model.Exchange{Name: DeadLetterExchange, Type: "direct", Durable: true})
	}
	for _, transport := range transports {
		queue := model.Queue{
			Name: transport, Type: "quorum", Durable: true,
			Arguments: map[string]any{},
		}
		if withFailed {
			// Un message rejeté part vers l'exchange d'échec plutôt que d'être
			// perdu : c'est ce qui permet de le rejouer après correction.
			queue.Arguments["x-dead-letter-exchange"] = DeadLetterExchange
			queue.Arguments["x-dead-letter-routing-key"] = transport + FailedSuffix
		}
		spec.Queues = append(spec.Queues, queue)
		spec.Bindings = append(spec.Bindings, model.Binding{
			Exchange: DefaultExchange, Queue: transport, RoutingKey: transport,
		})
		if withFailed {
			spec.Queues = append(spec.Queues, model.Queue{
				Name: transport + FailedSuffix, Type: "classic", Durable: true,
			})
			spec.Bindings = append(spec.Bindings, model.Binding{
				Exchange: DeadLetterExchange, Queue: transport + FailedSuffix,
				RoutingKey: transport + FailedSuffix,
			})
		}
	}
	return spec
}

// TransportDSN construit la chaîne de connexion d'un transport Messenger.
func TransportDSN(host string, port int, user, password, vhost, transport string) string {
	dsn := DSN(host, port, user, password, vhost)
	if transport == "" {
		return dsn
	}
	return fmt.Sprintf("%s?exchange[name]=%s&queues[%s]", dsn, DefaultExchange, transport)
}

// DSN construit la chaîne de connexion du broker. Le vhost étant déclaré sans
// barre oblique initiale (D9), aucun encodage %2F n'est nécessaire.
func DSN(host string, port int, user, password, vhost string) string {
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 5672
	}
	return fmt.Sprintf("amqp://%s:%s@%s:%d/%s", user, password, host, port, vhost)
}
