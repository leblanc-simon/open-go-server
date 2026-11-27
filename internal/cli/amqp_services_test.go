package cli

import (
	"strings"
	"testing"
)

// « amqp stop » vise les consommateurs du broker de tout le parc, Symfony récent
// (Messenger) comme ancien (rabbitmq:consumer), et eux seuls.
func TestAMQPStopViseLesConsommateursDeToutLeParc(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4")
	h.mustRun("service", "add", "shopfr", "async")
	h.mustRun("service", "add", "shopfr", "veille", "--template", "worker", "--command", "/bin/true")
	h.mustRun("project", "create", "ancien.fr", "--kind", "php-symfony", "--app-version", "3.4")
	h.mustRun("service", "add", "ancienfr", "commandes", "--template", "rabbitmq-consumer")
	h.mustRun("project", "create", "vitrine.fr", "--kind", "php-symfony", "--app-version", "6.4")
	h.mustRunPartial("apply", "--yes")

	out := h.mustRun("amqp", "stop")
	for _, u := range []string{"ogs-shopfr-async@1.service", "ogs-ancienfr-commandes@1.service"} {
		if !strings.Contains(out, u) {
			t.Errorf("%s doit être arrêté :\n%s", u, out)
		}
	}
	if strings.Contains(out, "veille") || strings.Contains(out, "vitrinefr") {
		t.Errorf("seuls les consommateurs du broker sont visés :\n%s", out)
	}

	out = h.mustRun("amqp", "start", "ancienfr")
	if !strings.Contains(out, "ogs-ancienfr-commandes@1.service") || strings.Contains(out, "shopfr") {
		t.Errorf("seuls les projets nommés sont visés :\n%s", out)
	}
	if !strings.Contains(h.read(t, "var/log/ogs/audit.log"), "amqp.stop") {
		t.Errorf("l'arrêt doit être journalisé")
	}
}
