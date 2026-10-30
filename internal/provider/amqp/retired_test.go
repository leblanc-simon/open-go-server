package amqp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/store"
)

// applyReport applique le plan et rend le compte rendu, avertissements
// compris : un retrait refusé n'est pas critique.
func applyReport(t *testing.T, p *Provider, proj *model.Project) *engine.Report {
	t.Helper()
	plan := replan(t, p, proj)
	report, err := engine.Execute(context.Background(), plan, engine.Options{})
	if err != nil {
		t.Fatalf("application : %v", err)
	}
	return report
}

func secretNames(t *testing.T, p *Provider, project string) []string {
	t.Helper()
	names, err := p.Store.SecretNames(project)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestCeQuiQuitteLaDeclarationQuitteLeBroker(t *testing.T) {
	// File, binding, exchange et policy retirés de la déclaration restaient sur
	// le broker (R1-31). Ils partent désormais — mais seulement ce qu'ogs a
	// déclaré : la file que l'application a créée elle-même reste.
	p, client, proj := fixture(t)
	proj.Spec.AMQP.Policies = []model.Policy{{Name: "ttl", Pattern: "^async$",
		Definition: map[string]any{"message-ttl": 60000}}}
	apply(t, p, proj)
	client.queues["shopfr/auto"] = Queue{Name: "auto", Vhost: "shopfr", Durable: true}
	client.bindings = append(client.bindings, Binding{Vhost: "shopfr", Source: "messages",
		Destination: "auto", RoutingKey: "auto"})

	spec := proj.Spec.AMQP
	spec.Queues = slices.DeleteFunc(spec.Queues, func(q model.Queue) bool { return q.Name == "async_failed" })
	spec.Bindings = slices.DeleteFunc(spec.Bindings, func(b model.Binding) bool { return b.Queue == "async_failed" })
	spec.Exchanges = slices.DeleteFunc(spec.Exchanges, func(e model.Exchange) bool { return e.Name == "dlx" })
	spec.Policies = nil
	for i := range spec.Queues {
		delete(spec.Queues[i].Arguments, "x-dead-letter-exchange")
		delete(spec.Queues[i].Arguments, "x-dead-letter-routing-key")
	}
	// La file async garde ses arguments sur le broker : ses écarts ne relèvent pas
	// de ce test.
	q := client.queues["shopfr/async"]
	q.Arguments = map[string]any{}
	client.queues["shopfr/async"] = q

	applyReport(t, p, proj)
	if _, ok := client.queues["shopfr/async_failed"]; ok {
		t.Error("la file retirée de la déclaration doit partir")
	}
	if _, ok := client.exchanges["shopfr/dlx"]; ok {
		t.Error("l'exchange retiré de la déclaration doit partir")
	}
	if _, ok := client.policies["shopfr/ttl"]; ok {
		t.Error("la policy retirée de la déclaration doit partir")
	}
	for _, b := range client.bindings {
		if b.Destination == "async_failed" {
			t.Errorf("le binding retiré doit partir : %+v", b)
		}
	}
	if _, ok := client.queues["shopfr/auto"]; !ok {
		t.Error("la file créée par l'application n'est pas à ogs : elle reste")
	}
	if !slices.ContainsFunc(client.bindings, func(b Binding) bool { return b.Destination == "auto" }) {
		t.Error("le binding créé par l'application reste")
	}
	if plan := replan(t, p, proj); !plan.Empty() {
		t.Errorf("le plan suivant doit être vide :\n%s", plan.String())
	}
}

func TestFileRetireeQuiPorteDesMessagesResteEtLeDit(t *testing.T) {
	p, client, proj := fixture(t)
	apply(t, p, proj)
	q := client.queues["shopfr/async_failed"]
	q.Messages = 7
	client.queues["shopfr/async_failed"] = q
	spec := proj.Spec.AMQP
	spec.Queues = slices.DeleteFunc(spec.Queues, func(q model.Queue) bool { return q.Name == "async_failed" })
	spec.Bindings = slices.DeleteFunc(spec.Bindings, func(b model.Binding) bool { return b.Queue == "async_failed" })

	report := applyReport(t, p, proj)
	if _, ok := client.queues["shopfr/async_failed"]; !ok {
		t.Fatal("une file qui porte des messages ne part pas à l'apply")
	}
	if len(report.Warnings) == 0 || !strings.Contains(report.Warnings[0], "7 message(s)") {
		t.Errorf("le refus doit être dit (code 6) : %+v", report.Warnings)
	}
	// Vidée, elle part au passage suivant : la trace l'a gardée.
	q.Messages = 0
	client.queues["shopfr/async_failed"] = q
	applyReport(t, p, proj)
	if _, ok := client.queues["shopfr/async_failed"]; ok {
		t.Error("vidée, la file doit partir au passage suivant")
	}
}

func TestTraceAbsenteRienNEstRetire(t *testing.T) {
	// Un parc antérieur ne porte aucune trace : ogs ne sait pas ce qu'il a
	// déclaré, et ne retire rien — la trace est posée au premier apply.
	p, client, proj := fixture(t)
	apply(t, p, proj)
	if err := p.Store.DeleteSecret(proj.ID(), DeclaredMarker("shopfr")); err != nil {
		t.Fatal(err)
	}
	proj.Spec.AMQP.Queues = slices.DeleteFunc(proj.Spec.AMQP.Queues,
		func(q model.Queue) bool { return q.Name == "async_failed" })
	applyReport(t, p, proj)
	if _, ok := client.queues["shopfr/async_failed"]; !ok {
		t.Error("sans trace, rien n'est retiré")
	}
	if !slices.Contains(secretNames(t, p, proj.ID()), DeclaredMarker("shopfr")) {
		t.Error("la trace doit être posée")
	}
}

func TestVhostEtCompteRemplacesQuittentLeBroker(t *testing.T) {
	p, client, proj := fixture(t)
	apply(t, p, proj)
	proj.Spec.AMQP.Vhost, proj.Spec.AMQP.User = "shopfr2", "shopfr2"
	applyReport(t, p, proj)
	// L'apply qui crée les remplaçants garde les anciens (D100) : une saga
	// déroulée plus loin retirerait les nouveaux, et le projet resterait sans
	// vhost ni compte.
	if !client.vhosts["shopfr"] || client.users["shopfr"] == "" {
		t.Fatal("premier apply : l'ancien vhost et l'ancien compte restent")
	}
	applyReport(t, p, proj)
	if client.vhosts["shopfr"] {
		t.Error("l'ancien vhost d'ogs, vide, doit partir")
	}
	if _, ok := client.users["shopfr"]; ok {
		t.Error("l'ancien compte d'ogs doit partir")
	}
	if !client.vhosts["shopfr2"] || client.users["shopfr2"] == "" {
		t.Error("le nouveau vhost et le nouveau compte doivent être posés")
	}
	names := secretNames(t, p, proj.ID())
	for _, gone := range []string{VhostMarker("shopfr"), DeclaredMarker("shopfr"), "amqp/shopfr"} {
		if slices.Contains(names, gone) {
			t.Errorf("%s doit quitter le keystore avec sa ressource : %v", gone, names)
		}
	}
	if plan := replan(t, p, proj); !plan.Empty() {
		t.Errorf("le plan suivant doit être vide :\n%s", plan.String())
	}
}

func TestAncienVhostQuiPorteDesMessagesResteJusquALaSuppression(t *testing.T) {
	p, client, proj := fixture(t)
	apply(t, p, proj)
	q := client.queues["shopfr/async"]
	q.Messages = 3
	client.queues["shopfr/async"] = q
	proj.Spec.AMQP.Vhost = "shopfr2"
	applyReport(t, p, proj) // crée shopfr2 ; l'ancien attend l'apply suivant (D100)
	report := applyReport(t, p, proj)
	if !client.vhosts["shopfr"] {
		t.Fatal("un vhost dont une file porte des messages ne part pas à l'apply")
	}
	if len(report.Warnings) == 0 {
		t.Error("le vhost conservé doit être dit")
	}
	if !slices.Contains(secretNames(t, p, proj.ID()), VhostMarker("shopfr")) {
		t.Fatal("conservé, le vhost garde sa preuve")
	}
	// La suppression du projet est le consentement : l'ancien vhost part avec lui.
	rep := p.DropProject(context.Background(), proj)
	if client.vhosts["shopfr"] || client.vhosts["shopfr2"] {
		t.Errorf("les deux vhosts d'ogs partent avec le projet : %v (%v)", client.vhosts, rep.Warnings)
	}
	for _, n := range secretNames(t, p, proj.ID()) {
		if strings.HasPrefix(n, "amqp") {
			t.Errorf("aucun secret amqp ne survit à la suppression : %s", n)
		}
	}
}

func TestAMQPRetireDeLaDeclarationQuitteLeBroker(t *testing.T) {
	p, client, proj := fixture(t)
	apply(t, p, proj)
	proj.Spec.AMQP = nil
	if !p.HasLeftovers(proj) {
		t.Fatal("les preuves du vhost et du compte doivent faire planifier leur retrait")
	}
	applyReport(t, p, proj)
	if client.vhosts["shopfr"] {
		t.Error("le vhost d'ogs doit partir")
	}
	if _, ok := client.users["shopfr"]; ok {
		t.Error("le compte d'ogs doit partir")
	}
	if p.HasLeftovers(proj) {
		t.Errorf("plus rien ne doit rester : %v", secretNames(t, p, proj.ID()))
	}
}

func TestSuppressionDuProjetJugeLesAnciennesPreuves(t *testing.T) {
	// Les preuves d'un ancien vhost et d'un ancien compte restaient au keystore
	// après « project delete » : un projet recréé sous le même identifiant les
	// aurait repris, puis supprimés.
	p, client, proj := fixture(t)
	apply(t, p, proj)
	// La déclaration a changé sans apply : l'ancien vhost et l'ancien compte
	// sont toujours sur le broker.
	proj.Spec.AMQP.Vhost, proj.Spec.AMQP.User = "autre", "autre"
	rep := p.DropProject(context.Background(), proj)
	if client.vhosts["shopfr"] {
		t.Error("l'ancien vhost d'ogs doit partir avec le projet")
	}
	if _, ok := client.users["shopfr"]; ok {
		t.Error("l'ancien compte d'ogs doit partir avec le projet")
	}
	if !slices.Contains(rep.Removed, "vhost amqp shopfr") || !slices.Contains(rep.Removed, "compte amqp shopfr") {
		t.Errorf("les retraits doivent être nommés : %v", rep.Removed)
	}
	if names := secretNames(t, p, proj.ID()); len(names) != 0 {
		t.Errorf("aucun secret ne doit rester : %v", names)
	}
}

// failingDelete fait échouer le retrait du vhost.
type failingDelete struct{ *memClient }

func (f failingDelete) DeleteVhost(context.Context, string) error { return errors.New("broker refuse") }

func TestUnRetraitEnEchecGardeSaPreuve(t *testing.T) {
	p, client, proj := fixture(t)
	apply(t, p, proj)
	p.Client = failingDelete{client}
	rep := p.DropProject(context.Background(), proj)
	if len(rep.Warnings) == 0 {
		t.Error("l'échec doit être dit")
	}
	names := secretNames(t, p, proj.ID())
	if !slices.Contains(names, VhostMarker("shopfr")) {
		t.Errorf("le vhost resté garde sa marque, sans quoi il deviendrait étranger : %v", names)
	}
	if slices.Contains(names, "amqp/shopfr") {
		t.Errorf("le compte, lui, est parti avec son secret : %v", names)
	}
}

func TestPreuveContesteeNeFaitRienRetirer(t *testing.T) {
	// Un ancien vhost supprimé à la main puis recréé par un voisin : le voisin en
	// tient aussi la marque, et ce n'est plus la ressource de ce seul projet.
	p, client, proj := fixture(t)
	apply(t, p, proj)
	if err := p.Store.UpsertProject(store.ProjectRow{ID: "voisin", Host: "voisin.fr", Env: "prod"}); err != nil {
		t.Fatal(err)
	}
	if err := p.sealMarker("voisin", "shopfr"); err != nil {
		t.Fatal(err)
	}
	proj.Spec.AMQP.Vhost = "shopfr2"
	applyReport(t, p, proj)
	if !client.vhosts["shopfr"] {
		t.Error("un vhost dont un autre projet porte la marque ne part pas")
	}
	p.DropProject(context.Background(), proj)
	if !client.vhosts["shopfr"] {
		t.Error("ni à la suppression du projet")
	}
}

// Revue du lot T (B1) : vhost et compte remplacés, puis une étape critique plus
// loin échoue et la saga se déroule. L'ancien vhost et l'ancien compte, retirés
// dans la foulée sans compensation, laissaient le projet sans aucun — la
// compensation retirant les nouveaux.
func TestDéroulageAprèsRemplacementGardeLAncienVhostEtLAncienCompte(t *testing.T) {
	p, client, proj := fixture(t)
	apply(t, p, proj)
	proj.Spec.AMQP.Vhost, proj.Spec.AMQP.User = "shopfr2", "shopfr2"
	plan := replan(t, p, proj)
	échec := engine.NewStep("vhost/web", engine.Create, "étape critique en échec")
	échec.Apply = func(context.Context) error { return errors.New("échec simulé") }
	plan.Add(échec)
	report, err := engine.Execute(context.Background(), plan, engine.Options{})
	if err == nil || !report.RolledBack {
		t.Fatalf("déroulage attendu : %v", err)
	}
	if !client.vhosts["shopfr"] || client.users["shopfr"] == "" {
		t.Errorf("après déroulage, l'ancien vhost et l'ancien compte doivent rester : %v %v",
			client.vhosts, client.users)
	}
	names := secretNames(t, p, proj.ID())
	for _, want := range []string{VhostMarker("shopfr"), "amqp/shopfr"} {
		if !slices.Contains(names, want) {
			t.Errorf("%s doit rester au keystore : %v", want, names)
		}
	}
}

// Un parc antérieur à D100 ne porte aucune trace : la poser est une étape, mais
// pas un écart — « diff » et « status » ne la comptent pas.
func TestLaTracePoséeNEstPasUnÉcart(t *testing.T) {
	p, _, proj := fixture(t)
	apply(t, p, proj)
	if err := p.Store.DeleteSecret(proj.ID(), DeclaredMarker("shopfr")); err != nil {
		t.Fatal(err)
	}
	plan := replan(t, p, proj)
	if len(plan.Mutating()) == 0 {
		t.Fatal("la trace absente doit être posée")
	}
	if drift := plan.Drift(); len(drift) != 0 {
		t.Errorf("poser la trace n'est pas un écart : %v", drift)
	}
}

// Un voisin qui déclare le vhost sans en porter la marque — parc antérieur à
// D93 — le protège aussi : un vhost obsolète ne part pas s'il sert ailleurs.
func TestVhostObsolèteDéclaréParUnVoisinReste(t *testing.T) {
	p, client, proj := fixture(t)
	apply(t, p, proj)
	voisin := model.New("blog.fr", "prod", model.KindStatic)
	voisin.Metadata.Name = "blogfr"
	voisin.Spec.AMQP = &model.AMQP{Vhost: "shopfr", User: "blogfr"}
	p.Fleet = func() ([]*model.Project, error) { return []*model.Project{proj, voisin}, nil }
	proj.Spec.AMQP.Vhost = "shopfr2"
	applyReport(t, p, proj)
	applyReport(t, p, proj)
	if !client.vhosts["shopfr"] {
		t.Error("un vhost qu'un voisin déclare ne part jamais comme obsolète")
	}
}
