package cli

import (
	"slices"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

// Ce que la déclaration AMQP ne porte plus quitte le broker (R1-31, D100).

func (h *harness) projetMessenger(t *testing.T, state *brokerState) {
	t.Helper()
	h.mustRun("init")
	h.fakeBroker(t, state)
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "3.4", "--php", "7.4")
	h.mustRun("amqp", "messenger", "shopfr", "--transport", "async", "--failed")
	h.mustRun("apply", "shopfr", "--yes")
}

func TestAMQPFileRetireeDeLaDeclarationQuitteLeBroker(t *testing.T) {
	h := newHarness(t)
	state := newBrokerState()
	h.projetMessenger(t, state)
	state.queueList = []map[string]any{{
		"name": "async", "vhost": "shopfr", "durable": true,
		"arguments": map[string]any{"x-dead-letter-exchange": "dlx",
			"x-dead-letter-routing-key": "async_failed"},
	}, {
		"name": "async_failed", "vhost": "shopfr", "durable": true,
		"arguments": map[string]any{},
	}}
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.AMQP.Queues = slices.DeleteFunc(p.Spec.AMQP.Queues,
			func(q model.Queue) bool { return q.Name == "async_failed" })
		p.Spec.AMQP.Bindings = slices.DeleteFunc(p.Spec.AMQP.Bindings,
			func(b model.Binding) bool { return b.Queue == "async_failed" })
	})
	state.writes = nil
	h.mustRun("apply", "shopfr", "--yes")
	if !slices.Contains(state.writes, "DELETE /api/queues/shopfr/async_failed") {
		t.Errorf("la file retirée de la déclaration doit quitter le broker : %v", state.writes)
	}
	if slices.Contains(state.writes, "DELETE /api/queues/shopfr/async") {
		t.Errorf("la file déclarée reste : %v", state.writes)
	}
}

func TestAMQPVhostEtCompteRemplacesQuittentLeBroker(t *testing.T) {
	h := newHarness(t)
	state := newBrokerState()
	h.projetMessenger(t, state)
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.AMQP.Vhost, p.Spec.AMQP.User = "shopfr2", "shopfr2"
	})
	state.writes = nil
	h.mustRun("apply", "shopfr", "--yes")
	// L'apply qui crée les remplaçants garde les anciens ; le suivant les retire
	// (D100).
	for _, early := range []string{"DELETE /api/vhosts/shopfr", "DELETE /api/users/shopfr"} {
		if slices.Contains(state.writes, early) {
			t.Errorf("%s dès l'apply qui crée le remplaçant : %v", early, state.writes)
		}
	}
	h.mustRun("apply", "shopfr", "--yes")
	for _, want := range []string{"DELETE /api/vhosts/shopfr", "DELETE /api/users/shopfr"} {
		if !slices.Contains(state.writes, want) {
			t.Errorf("%s attendu : %v", want, state.writes)
		}
	}
	if !state.vhosts["shopfr2"] || !state.users["shopfr2"] {
		t.Error("le nouveau vhost et le nouveau compte doivent être posés")
	}
}

func TestAMQPRetireDeLaDeclarationQuitteLeBroker(t *testing.T) {
	h := newHarness(t)
	state := newBrokerState()
	h.projetMessenger(t, state)
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) { p.Spec.AMQP = nil })
	state.writes = nil
	h.mustRun("apply", "shopfr", "--yes")
	for _, want := range []string{"DELETE /api/vhosts/shopfr", "DELETE /api/users/shopfr"} {
		if !slices.Contains(state.writes, want) {
			t.Errorf("%s attendu : %v", want, state.writes)
		}
	}
}

func TestSuppressionRetireAussiLAncienVhostAMQP(t *testing.T) {
	// La déclaration a changé de vhost et de compte sans apply : les anciens,
	// créés par ogs, survivaient à « project delete » avec leurs preuves.
	h := newHarness(t)
	state := newBrokerState()
	h.projetMessenger(t, state)
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.AMQP.Vhost, p.Spec.AMQP.User = "shopfr2", "shopfr2"
	})
	state.writes = nil
	h.run("project", "delete", "shopfr", "--yes")
	for _, want := range []string{"DELETE /api/vhosts/shopfr", "DELETE /api/users/shopfr"} {
		if !slices.Contains(state.writes, want) {
			t.Errorf("%s attendu : %v", want, state.writes)
		}
	}
	// Le projet recréé ne reprend pas un homonyme créé depuis par un autre.
	state.vhosts["shopfr"] = true
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("amqp", "messenger", "shopfr", "--transport", "async")
	if out, code := h.run("apply", "shopfr", "--yes"); code != CodePartial {
		t.Errorf("le vhost d'un autre doit être refusé au projet recréé (code %d) :\n%s", code, out)
	}
}
