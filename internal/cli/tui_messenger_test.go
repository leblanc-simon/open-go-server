package cli

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/provider/unit"
	"leblanc.io/open-go-server/internal/tui"
)

// Les noms saisis dans l'assistant sont nettoyés : « async, high » déclarait la
// file « high » précédée d'une espace, que le worker « high » ne consommait pas.
func TestAssistantNettoieLesNomsDeTransport(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	app := h.ouvrirApp(t, true)
	b := &tuiBackend{app: app}
	if out, err := b.Create(context.Background(), tui.WizardAnswers{
		Host: "shop.fr", Kind: "php-symfony", AppVersion: "6.4", PHPVersion: "8.5", Env: "prod",
		Messenger: " async , high ,", Instances: " 2 ",
	}); err != nil {
		t.Fatalf("Create : %v\n%s", err, out)
	}
	p, err := app.loadProject("shopfr")
	if err != nil {
		t.Fatal(err)
	}
	var transports []string
	for _, svc := range p.Spec.Services {
		transports = append(transports, svc.Params["transport"])
	}
	if !slices.Equal(transports, []string{"async", "high"}) {
		t.Errorf("transports %q, attendus async et high", transports)
	}
	if len(p.Spec.Services) != 2 {
		t.Fatalf("deux services attendus : %+v", p.Spec.Services)
	}
	for _, svc := range p.Spec.Services {
		if svc.Template != unit.TemplateMessenger || svc.Instances != 2 {
			t.Errorf("service %s : template %s, %d instance(s)", svc.Name, svc.Template, svc.Instances)
		}
	}
}

// Symfony avant 4.1 n'a pas Messenger : la création n'est plus refusée, les
// workers passent par rabbitmq:consumer, avec le préréglage complet mais sans
// exchange de rejet ni file d'échec.
func TestAssistantBasculeSurRabbitMQConsumerAvantSymfony41(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	app := h.ouvrirApp(t, true)
	b := &tuiBackend{app: app}
	if out, err := b.Create(context.Background(), tui.WizardAnswers{
		Host: "ancien.fr", Kind: "php-symfony", AppVersion: "3.4", PHPVersion: "7.4", Env: "prod",
		Messenger: "async",
	}); err != nil {
		t.Fatalf("Create : %v\n%s", err, out)
	}
	p, err := app.loadProject("ancienfr")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Spec.Services) != 1 || p.Spec.Services[0].Template != unit.TemplateConsumer ||
		p.Spec.Services[0].Params["transport"] != "async" {
		t.Errorf("un service rabbitmq-consumer « async » attendu : %+v", p.Spec.Services)
	}
	a := p.Spec.AMQP
	if a == nil || a.Vhost != "ancienfr" || a.User != "ancienfr" {
		t.Fatalf("vhost et compte AMQP attendus : %+v", a)
	}
	if len(a.Queues) != 1 || a.Queues[0].Name != "async" || len(a.Exchanges) != 1 {
		t.Errorf("une file async et un exchange attendus, sans file d'échec : %+v", a)
	}
}

// La déclaration basculée s'applique : unité rabbitmq-consumer, vhost sans objet.
func TestSiteCreateSymfony34AvecMessengerSApplique(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.fakeBroker(t, newBrokerState())
	h.mustRunPartial("site", "create", "ancien.fr",
		"--kind", "php-symfony", "--app-version", "3.4", "--php", "7.4",
		"--messenger", "async", "--service", "symfony-messenger:async:2", "--yes")
	unite := h.read(t, filepath.Join("etc", "systemd", "system", "ogs-ancienfr-rabbitmq-consumer-async@.service"))
	if !strings.Contains(unite, "rabbitmq:consumer async") {
		t.Errorf("unité rabbitmq-consumer attendue :\n%s", unite)
	}
}
