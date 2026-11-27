package cli

import (
	"os"
	"path/filepath"
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

// D137 : le plan global rattache les consommateurs au broker installé sur la
// machine, et l'unité part avec lui.
func TestLePlanGlobalRattacheLesConsommateursAuBrokerLocal(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	unité := h.path("etc", "systemd", "system", "ogs-amqp.service")
	h.mustRunPartial("apply", "--yes")
	if _, err := os.Stat(unité); !os.IsNotExist(err) {
		t.Fatalf("sans broker local, aucune unité : %v", err)
	}
	broker := h.path("lib", "systemd", "system", "rabbitmq-server.service")
	if err := os.MkdirAll(filepath.Dir(broker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broker, []byte("[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.mustRunPartial("apply", "--yes")
	if !strings.Contains(out, "ogs-amqp.service") {
		t.Errorf("le plan doit annoncer l'unité :\n%s", out)
	}
	if !strings.Contains(h.read(t, "etc/systemd/system/ogs-amqp.service"), "ExecStop=/usr/bin/ogs amqp stop --quiet") {
		t.Errorf("l'unité doit arrêter les consommateurs à l'arrêt du broker")
	}
	if err := os.Remove(broker); err != nil {
		t.Fatal(err)
	}
	h.mustRunPartial("apply", "--yes")
	if _, err := os.Stat(unité); !os.IsNotExist(err) {
		t.Errorf("le broker désinstallé, l'unité part : %v", err)
	}
}
