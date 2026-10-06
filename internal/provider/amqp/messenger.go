package amqp

import (
	"fmt"
	"maps"
	"slices"

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

// Complete ajoute à la déclaration AMQP d'un projet ce que le préréglage
// Messenger demande et qu'elle ne porte pas encore (D121) : exchanges et files
// absents par leur nom, bindings absents par leur triplet. Elle ne retire ni ne
// modifie rien — vhost, compte, permissions, policies et objets déjà déclarés
// restent tels quels. « amqp messenger » remplaçait toute la déclaration : un
// second transport effaçait le premier, une file ou une policy écrites à la main
// disparaissaient, et l'apply suivant les retirait du broker, messages compris
// (D100). Les notes nomment chaque objet du préréglage qu'une déclaration
// différente sous le même nom empêche de poser.
func Complete(current, preset *model.AMQP) (*model.AMQP, []string) {
	if current == nil {
		return preset, nil
	}
	out := *current
	if out.Vhost == "" {
		out.Vhost = preset.Vhost
	}
	if out.User == "" {
		out.User = preset.User
	}
	if len(out.Permissions) == 0 {
		out.Permissions = maps.Clone(preset.Permissions)
	}
	var notes []string
	out.Exchanges = slices.Clone(current.Exchanges)
	for _, want := range preset.Exchanges {
		i := slices.IndexFunc(out.Exchanges, func(e model.Exchange) bool { return e.Name == want.Name })
		switch {
		case i < 0:
			out.Exchanges = append(out.Exchanges, want)
		case out.Exchanges[i] != want:
			notes = append(notes, fmt.Sprintf("exchange %s déjà déclaré autrement (%s) : gardé tel quel",
				want.Name, out.Exchanges[i].Type))
		}
	}
	out.Queues = slices.Clone(current.Queues)
	for _, want := range preset.Queues {
		i := slices.IndexFunc(out.Queues, func(q model.Queue) bool { return q.Name == want.Name })
		switch {
		case i < 0:
			out.Queues = append(out.Queues, want)
		case !sameQueue(out.Queues[i], want):
			notes = append(notes, fmt.Sprintf("file %s déjà déclarée autrement : gardée telle quelle", want.Name))
		}
	}
	out.Bindings = slices.Clone(current.Bindings)
	for _, want := range preset.Bindings {
		if !slices.Contains(out.Bindings, want) {
			out.Bindings = append(out.Bindings, want)
		}
	}
	return &out, notes
}

// sameQueue compare deux déclarations de file, type et arguments compris.
func sameQueue(a, b model.Queue) bool {
	if a.Type != b.Type || a.Durable != b.Durable || len(a.Arguments) != len(b.Arguments) {
		return false
	}
	for k, v := range a.Arguments {
		if w, ok := b.Arguments[k]; !ok || fmt.Sprint(v) != fmt.Sprint(w) {
			return false
		}
	}
	return true
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
