package amqp

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/secret"
	"leblanc.io/open-go-server/internal/store"
)

// memClient est un broker en mémoire : il permet d'éprouver la réconciliation
// sans serveur, y compris les rejeux successifs.
type memClient struct {
	version     string
	vhosts      map[string]bool
	users       map[string]string
	permissions map[string]Permission
	exchanges   map[string]Exchange
	queues      map[string]Queue
	bindings    []Binding
	policies    map[string]Policy
	deleted     []string
	fail        error
	// failUsers et failPerms n'échouent qu'à partir d'une lecture précise : le
	// broker qui redémarre entre deux appels du client HTTP, ou un compte de
	// management dont le tag ne suffit pas à /api/users.
	failUsers error
	failPerms error
	failVhost error
}

func newMemClient() *memClient {
	return &memClient{
		version: "3.13.7",
		vhosts:  map[string]bool{}, users: map[string]string{},
		permissions: map[string]Permission{}, exchanges: map[string]Exchange{},
		queues: map[string]Queue{}, policies: map[string]Policy{},
	}
}

func (m *memClient) Overview(context.Context) (string, error) {
	if m.fail != nil {
		return "", m.fail
	}
	return m.version, nil
}
func (m *memClient) Vhosts(context.Context) ([]Vhost, error) {
	var out []Vhost
	for name := range m.vhosts {
		out = append(out, Vhost{Name: name})
	}
	return out, nil
}
func (m *memClient) EnsureVhost(_ context.Context, name string) error {
	if m.failVhost != nil {
		return m.failVhost
	}
	m.vhosts[name] = true
	return nil
}
func (m *memClient) DeleteVhost(_ context.Context, name string) error {
	delete(m.vhosts, name)
	m.deleted = append(m.deleted, "vhost:"+name)
	return nil
}
func (m *memClient) Users(context.Context) ([]User, error) {
	if m.failUsers != nil {
		return nil, m.failUsers
	}
	var out []User
	for name := range m.users {
		out = append(out, User{Name: name})
	}
	return out, nil
}
func (m *memClient) EnsureUser(_ context.Context, name, password string, _ []string) error {
	m.users[name] = password
	return nil
}
func (m *memClient) DeleteUser(_ context.Context, name string) error {
	delete(m.users, name)
	m.deleted = append(m.deleted, "user:"+name)
	return nil
}
func (m *memClient) Permissions(context.Context) ([]Permission, error) {
	if m.failPerms != nil {
		return nil, m.failPerms
	}
	var out []Permission
	for _, p := range m.permissions {
		out = append(out, p)
	}
	return out, nil
}
func (m *memClient) EnsurePermission(_ context.Context, p Permission) error {
	m.permissions[p.Vhost+"/"+p.User] = p
	return nil
}
func (m *memClient) Exchanges(_ context.Context, vhost string) ([]Exchange, error) {
	var out []Exchange
	for _, e := range m.exchanges {
		if e.Vhost == vhost {
			out = append(out, e)
		}
	}
	return out, nil
}
func (m *memClient) EnsureExchange(_ context.Context, e Exchange) error {
	m.exchanges[e.Vhost+"/"+e.Name] = e
	return nil
}
func (m *memClient) Queues(_ context.Context, vhost string) ([]Queue, error) {
	var out []Queue
	for _, q := range m.queues {
		if q.Vhost == vhost {
			out = append(out, q)
		}
	}
	return out, nil
}
func (m *memClient) EnsureQueue(_ context.Context, q Queue) error {
	// Le broker refuse une redéclaration divergente : on le simule pour vérifier
	// que le provider ne tente jamais l'opération.
	if existing, ok := m.queues[q.Vhost+"/"+q.Name]; ok {
		if len(diffQueue(q, existing)) > 0 {
			return errPrecondition
		}
	}
	// Copie défensive, comme le fait le client HTTP : sans elle, le broker
	// simulé partagerait la table d'arguments de la déclaration, et toute
	// modification ultérieure du projet muterait aussi l'état « observé ».
	stored := q
	stored.Arguments = map[string]any{}
	for k, v := range q.Arguments {
		stored.Arguments[k] = v
	}
	if q.Type != "" && q.Type != "classic" {
		stored.Arguments["x-queue-type"] = q.Type
	}
	m.queues[q.Vhost+"/"+q.Name] = stored
	return nil
}
func (m *memClient) DeleteQueue(_ context.Context, vhost, name string) error {
	delete(m.queues, vhost+"/"+name)
	m.deleted = append(m.deleted, "queue:"+name)
	return nil
}
func (m *memClient) PurgeQueue(_ context.Context, vhost, name string) error {
	if q, ok := m.queues[vhost+"/"+name]; ok {
		q.Messages = 0
		m.queues[vhost+"/"+name] = q
	}
	return nil
}
func (m *memClient) Bindings(_ context.Context, vhost string) ([]Binding, error) {
	var out []Binding
	for _, b := range m.bindings {
		if b.Vhost == vhost {
			out = append(out, b)
		}
	}
	return out, nil
}
func (m *memClient) EnsureBinding(_ context.Context, b Binding) error {
	m.bindings = append(m.bindings, b)
	return nil
}
func (m *memClient) Policies(_ context.Context, vhost string) ([]Policy, error) {
	var out []Policy
	for _, p := range m.policies {
		if p.Vhost == vhost {
			out = append(out, p)
		}
	}
	return out, nil
}
func (m *memClient) EnsurePolicy(_ context.Context, p Policy) error {
	m.policies[p.Vhost+"/"+p.Name] = p
	return nil
}
func (m *memClient) DeletePolicy(_ context.Context, vhost, name string) error {
	delete(m.policies, vhost+"/"+name)
	return nil
}

var errPrecondition = &preconditionError{}

type preconditionError struct{}

func (e *preconditionError) Error() string {
	return "PRECONDITION_FAILED - inequivalent arg for queue"
}

func fixture(t *testing.T) (*Provider, *memClient, *model.Project) {
	t.Helper()
	prefix := t.TempDir()
	paths := config.NewPaths(prefix)
	st, err := store.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	keyPath := filepath.Join(prefix, "master.key")
	if err := secret.GenerateKey(keyPath); err != nil {
		t.Fatal(err)
	}
	ks, err := secret.Open(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	client := newMemClient()
	proj := model.New("shop.fr", naming.EnvProd, model.KindSymfony)
	proj.Spec.App.Version = "6.4"
	if err := proj.Normalize(filepath.Join(prefix, "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	proj.Spec.AMQP = Preset(proj, []string{"async"}, true)
	return &Provider{Paths: paths, Store: st, Keys: ks, Client: client}, client, proj
}

func apply(t *testing.T, p *Provider, proj *model.Project) *engine.Plan {
	t.Helper()
	ctx := context.Background()
	obs, err := p.Observe(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := p.Plan(ctx, proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	plan := &engine.Plan{Project: proj.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatalf("application : %v", err)
	}
	return plan
}

func replan(t *testing.T, p *Provider, proj *model.Project) *engine.Plan {
	t.Helper()
	ctx := context.Background()
	obs, err := p.Observe(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := p.Plan(ctx, proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	plan := &engine.Plan{Project: proj.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	return plan
}

func TestApplyCreatesEverythingThenIsIdempotent(t *testing.T) {
	p, client, proj := fixture(t)
	apply(t, p, proj)

	if !client.vhosts["shopfr"] {
		t.Error("vhost non créé")
	}
	if client.users["shopfr"] == "" {
		t.Error("utilisateur sans mot de passe")
	}
	if _, ok := client.permissions["shopfr/shopfr"]; !ok {
		t.Error("permissions non posées")
	}
	// Le préréglage Messenger crée l'exchange, la queue, sa file d'échec et les
	// bindings correspondants.
	for _, name := range []string{"shopfr/messages", "shopfr/dlx"} {
		if _, ok := client.exchanges[name]; !ok {
			t.Errorf("exchange %s absent", name)
		}
	}
	for _, name := range []string{"shopfr/async", "shopfr/async_failed"} {
		if _, ok := client.queues[name]; !ok {
			t.Errorf("queue %s absente", name)
		}
	}
	if len(client.bindings) != 2 {
		t.Errorf("%d bindings, 2 attendus : %+v", len(client.bindings), client.bindings)
	}

	// Second passage : plus rien à faire.
	if plan := replan(t, p, proj); !plan.Empty() {
		t.Errorf("le second plan doit être vide :\n%s", plan.String())
	}
}

func TestPasswordIsStableAndSealed(t *testing.T) {
	p, client, proj := fixture(t)
	apply(t, p, proj)
	first := client.users["shopfr"]

	// Le mot de passe est scellé, donc relisible sans être régénéré.
	stored, err := p.ensurePassword(proj.ID(), "shopfr")
	if err != nil {
		t.Fatal(err)
	}
	if stored != first {
		t.Error("le mot de passe doit être stable entre deux applications")
	}
	_, ciphertext, err := p.Store.GetSecret(proj.ID(), "amqp/shopfr")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), first) {
		t.Error("le mot de passe ne doit pas figurer en clair dans la base")
	}
}

func TestImmutableQueueArgumentsAreNotApplied(t *testing.T) {
	// Cœur du lot : RabbitMQ refuse toute redéclaration divergente d'une queue.
	// Le provider ne doit jamais tenter l'opération, et doit prévenir du coût.
	p, client, proj := fixture(t)
	apply(t, p, proj)
	// Des messages sont en attente et un consumer est branché.
	q := client.queues["shopfr/async"]
	q.Messages, q.Consumers = 1240, 3
	client.queues["shopfr/async"] = q

	// Le TTL change dans la déclaration.
	for i := range proj.Spec.AMQP.Queues {
		if proj.Spec.AMQP.Queues[i].Name == "async" {
			proj.Spec.AMQP.Queues[i].Arguments["x-message-ttl"] = 3600000
		}
	}
	plan := replan(t, p, proj)
	mutating := plan.Mutating()
	if len(mutating) != 1 {
		t.Fatalf("une seule étape attendue :\n%s", plan.String())
	}
	step := mutating[0]
	if step.Critical {
		t.Error("l'étape doit être non critique : le site reste fonctionnel")
	}
	if !strings.Contains(step.Summary, "recréation requise") ||
		!strings.Contains(step.Summary, "1240 message") {
		t.Errorf("le plan doit annoncer le coût :\n%s", step.Summary)
	}
	err := step.Apply(context.Background())
	if err == nil {
		t.Fatal("l'application doit être refusée sans autorisation explicite")
	}
	for _, want := range []string{"immuables", "1240 message", "3 consumer", "--recreate-queues"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("le message doit contenir %q : %v", want, err)
		}
	}
	// La queue n'a pas été touchée.
	if client.queues["shopfr/async"].Messages != 1240 {
		t.Error("aucun message ne doit avoir été détruit")
	}
	if len(client.deleted) != 0 {
		t.Errorf("aucune suppression ne doit avoir eu lieu : %v", client.deleted)
	}
}

func TestRecreateQueuesWhenAuthorized(t *testing.T) {
	p, client, proj := fixture(t)
	apply(t, p, proj)
	q := client.queues["shopfr/async"]
	q.Messages = 5
	client.queues["shopfr/async"] = q
	for i := range proj.Spec.AMQP.Queues {
		if proj.Spec.AMQP.Queues[i].Name == "async" {
			proj.Spec.AMQP.Queues[i].Type = "classic"
		}
	}
	p.RecreateQueues = true
	plan := replan(t, p, proj)
	mutating := plan.Mutating()
	if len(mutating) != 1 || !strings.Contains(mutating[0].Summary, "5 message(s) perdu(s)") {
		t.Fatalf("le plan doit annoncer la perte :\n%s", plan.String())
	}
	if err := mutating[0].Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.deleted) != 1 || client.deleted[0] != "queue:async" {
		t.Errorf("la queue devait être supprimée puis recréée : %v", client.deleted)
	}
	if client.queues["shopfr/async"].EffectiveType() != "classic" {
		t.Errorf("type après recréation : %q", client.queues["shopfr/async"].EffectiveType())
	}
}

func TestNumericArgumentsFromJSONDoNotDrift(t *testing.T) {
	// L'API renvoie les nombres en float64 : sans normalisation, chaque
	// application signalerait un faux écart sur x-message-ttl.
	want := Queue{Name: "async", Durable: true, Type: "quorum",
		Arguments: map[string]any{"x-message-ttl": 86400000}}
	current := Queue{Name: "async", Durable: true,
		Arguments: map[string]any{"x-queue-type": "quorum", "x-message-ttl": float64(86400000)}}
	if changes := diffQueue(want, current); len(changes) != 0 {
		t.Errorf("faux écart détecté : %v", changes)
	}
	// Un écart réel reste détecté.
	current.Arguments["x-message-ttl"] = float64(3600000)
	if changes := diffQueue(want, current); len(changes) != 1 {
		t.Errorf("l'écart réel doit être détecté : %v", changes)
	}
}

func TestDiffQueueDetectsTypeAndDurability(t *testing.T) {
	cases := []struct {
		name    string
		want    Queue
		current Queue
		expect  string
	}{
		{
			"type", Queue{Type: "quorum", Durable: true}, Queue{Type: "classic", Durable: true},
			"type classic → quorum",
		},
		{
			"durabilité", Queue{Type: "classic", Durable: true}, Queue{Type: "classic"},
			"durable false → true",
		},
		{
			"dlx ajouté",
			Queue{Type: "classic", Durable: true, Arguments: map[string]any{"x-dead-letter-exchange": "dlx"}},
			Queue{Type: "classic", Durable: true},
			"x-dead-letter-exchange absent → dlx",
		},
	}
	for _, c := range cases {
		changes := diffQueue(c.want, c.current)
		if len(changes) != 1 || changes[0] != c.expect {
			t.Errorf("%s : écarts %v, attendu [%s]", c.name, changes, c.expect)
		}
	}
}

func TestUnavailableBrokerIsNonCritical(t *testing.T) {
	p, client, proj := fixture(t)
	client.fail = ErrUnavailable
	ctx := context.Background()
	obs, err := p.Observe(ctx, proj)
	if err != nil {
		t.Fatalf("l'observation ne doit pas échouer : %v", err)
	}
	if obs.Unavailable == nil {
		t.Fatal("l'indisponibilité doit être rapportée")
	}
	steps, err := p.Plan(ctx, proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Critical {
		t.Errorf("une seule étape non critique attendue : %+v", steps)
	}
}

func TestLectureInterrompueNeBloquePasLePlan(t *testing.T) {
	// Les cinq lectures d'Observe passent par le même appel HTTP : « broker
	// indisponible » ne peut pas être non critique sur /api/vhosts et fatal sur
	// /api/users à quelques millisecondes près. Rendue à l'appelant, l'erreur
	// faisait abandonner le plan entier du projet — utilisateur système et vhost
	// web compris — et, sous « --all », les projets suivants du parc avec.
	for _, c := range []struct {
		nom    string
		casser func(*memClient)
	}{
		{"users", func(m *memClient) { m.failUsers = ErrUnavailable }},
		{"permissions", func(m *memClient) { m.failPerms = ErrUnavailable }},
	} {
		p, client, proj := fixture(t)
		c.casser(client)
		ctx := context.Background()
		obs, err := p.Observe(ctx, proj)
		if err != nil {
			t.Fatalf("%s : l'observation ne doit pas échouer : %v", c.nom, err)
		}
		if obs.Unavailable == nil {
			t.Fatalf("%s : l'indisponibilité doit être rapportée", c.nom)
		}
		steps, err := p.Plan(ctx, proj, obs)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 1 || steps[0].Critical {
			t.Errorf("%s : une seule étape non critique attendue : %+v", c.nom, steps)
		}
	}
}

func TestRollbackRemovesCreatedObjects(t *testing.T) {
	p, client, proj := fixture(t)
	ctx := context.Background()
	obs, _ := p.Observe(ctx, proj)
	steps, err := p.Plan(ctx, proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	plan := &engine.Plan{Project: proj.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	boom := engine.NewStep("test/boom", engine.Create, "échec injecté")
	boom.Apply = func(context.Context) error { return errPrecondition }
	plan.Add(boom)

	report, err := engine.Execute(ctx, plan, engine.Options{})
	if err == nil {
		t.Fatal("erreur attendue")
	}
	if !report.RolledBack || report.RollbackErr != nil {
		t.Fatalf("rollback : %+v", report)
	}
	if client.vhosts["shopfr"] {
		t.Error("le vhost créé doit avoir été retiré")
	}
	if _, ok := client.users["shopfr"]; ok {
		t.Error("l'utilisateur créé doit avoir été retiré")
	}
}

func TestPermissionsAreUpdatedWhenNarrowed(t *testing.T) {
	p, client, proj := fixture(t)
	apply(t, p, proj)
	// Restreindre les permissions doit produire une mise à jour.
	proj.Spec.AMQP.Permissions = map[string]string{"configure": "^async", "write": "^async", "read": "^async"}
	plan := replan(t, p, proj)
	mutating := plan.Mutating()
	if len(mutating) != 1 || !strings.Contains(mutating[0].Summary, "^async") {
		t.Fatalf("mise à jour des permissions attendue :\n%s", plan.String())
	}
	if err := mutating[0].Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.permissions["shopfr/shopfr"].Read != "^async" {
		t.Errorf("permissions : %+v", client.permissions["shopfr/shopfr"])
	}
}

func TestPresetMatchesMessengerExpectations(t *testing.T) {
	proj := model.New("shop.fr", naming.EnvProd, model.KindSymfony)
	proj.Spec.App.Version = "6.4"
	if err := proj.Normalize(filepath.Join(t.TempDir(), "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	spec := Preset(proj, []string{"async", "priority"}, true)

	// Tout porte l'identifiant du projet (D23), et le vhost n'a pas de slash.
	if spec.Vhost != "shopfr" || spec.User != "shopfr" {
		t.Errorf("nommage : vhost %q, user %q", spec.Vhost, spec.User)
	}
	if strings.HasPrefix(spec.Vhost, "/") {
		t.Error("le vhost ne doit pas commencer par une barre oblique")
	}
	// Deux transports, chacun avec sa file d'échec.
	names := map[string]model.Queue{}
	for _, q := range spec.Queues {
		names[q.Name] = q
	}
	for _, want := range []string{"async", "async_failed", "priority", "priority_failed"} {
		if _, ok := names[want]; !ok {
			t.Errorf("queue %s absente : %v", want, names)
		}
	}
	// Le transport principal route ses rejets vers l'exchange d'échec, sinon un
	// message rejeté serait simplement perdu.
	async := names["async"]
	if async.Arguments["x-dead-letter-exchange"] != DeadLetterExchange {
		t.Errorf("DLX absent : %+v", async.Arguments)
	}
	if async.Arguments["x-dead-letter-routing-key"] != "async_failed" {
		t.Errorf("clé de rejet : %+v", async.Arguments)
	}
	if async.Type != "quorum" {
		t.Errorf("le transport principal doit être en quorum : %q", async.Type)
	}
	// Un binding par queue.
	if len(spec.Bindings) != 4 {
		t.Errorf("%d bindings, 4 attendus : %+v", len(spec.Bindings), spec.Bindings)
	}
}

func TestPresetWithoutFailedQueues(t *testing.T) {
	proj := model.New("shop.fr", naming.EnvProd, model.KindSymfony)
	if err := proj.Normalize(filepath.Join(t.TempDir(), "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	spec := Preset(proj, []string{"async"}, false)
	if len(spec.Queues) != 1 || spec.Queues[0].Name != "async" {
		t.Errorf("queues : %+v", spec.Queues)
	}
	if _, present := spec.Queues[0].Arguments["x-dead-letter-exchange"]; present {
		t.Error("sans file d'échec, aucun DLX ne doit être déclaré")
	}
	if len(spec.Exchanges) != 1 {
		t.Errorf("un seul exchange attendu : %+v", spec.Exchanges)
	}
}

func TestDSNHasNoEncodedSlash(t *testing.T) {
	dsn := DSN("", 0, "shopfr", "s3cr3t", "shopfr")
	if strings.Contains(dsn, "%2F") {
		t.Errorf("aucun encodage attendu : %s", dsn)
	}
	if dsn != "amqp://shopfr:s3cr3t@127.0.0.1:5672/shopfr" {
		t.Errorf("DSN : %s", dsn)
	}
}

func TestVhostHomonymeNonCreeParOgsNestPasRepris(t *testing.T) {
	// Un vhost et un compte homonymes, créés hors d'ogs, étaient repris en silence
	// — le compte gardait son mot de passe, qu'ogs ne connaissait pas — puis
	// détruits par « project delete », messages compris (R2-40).
	for _, c := range []struct {
		nom           string
		vhost, compte bool
	}{
		{"vhost seul", true, false},
		{"compte seul", false, true},
		{"vhost et compte", true, true},
	} {
		t.Run(c.nom, func(t *testing.T) {
			p, client, proj := fixture(t)
			if c.vhost {
				client.vhosts["shopfr"] = true
			}
			if c.compte {
				client.users["shopfr"] = "celui-de-l-administrateur"
			}
			obs, err := p.Observe(context.Background(), proj)
			if err != nil {
				t.Fatal(err)
			}
			if obs.Owned() {
				t.Fatal("un homonyme sans mot de passe scellé ne doit pas être tenu pour celui d'ogs")
			}
			steps, err := p.Plan(context.Background(), proj, obs)
			if err != nil {
				t.Fatal(err)
			}
			if len(steps) != 1 || steps[0].Critical || steps[0].Apply == nil {
				t.Fatalf("une seule étape non critique de refus est attendue : %+v", steps)
			}
			if err := steps[0].Apply(context.Background()); err == nil ||
				!strings.Contains(err.Error(), "spec.amqp.vhost") {
				t.Errorf("le refus doit nommer le remède : %v", err)
			}
			if c.compte && client.users["shopfr"] != "celui-de-l-administrateur" {
				t.Error("le mot de passe du compte de l'administrateur a changé")
			}
			if len(client.permissions) != 0 || len(client.queues) != 0 {
				t.Error("rien ne doit être déclaré sur des ressources qu'ogs n'a pas créées")
			}
			if _, _, err := p.Store.GetSecret(proj.ID(), "amqp/shopfr"); err == nil {
				t.Error("aucun mot de passe ne doit être scellé pour un compte refusé")
			}
		})
	}
}

func TestVhostDOgsResteLeSien(t *testing.T) {
	// Le compte créé par ogs porte son mot de passe scellé : vhost et compte
	// restent les siens, y compris quand l'un des deux a disparu du broker.
	p, client, proj := fixture(t)
	apply(t, p, proj)
	delete(client.users, "shopfr")
	obs, err := p.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Owned() {
		t.Fatal("le vhost qu'ogs a créé doit rester le sien")
	}
	apply(t, p, proj)
	if client.users["shopfr"] == "" {
		t.Error("le compte disparu doit être recréé")
	}
	if plan := replan(t, p, proj); !plan.Empty() {
		t.Errorf("le plan doit être vide :\n%s", plan.String())
	}
}

func TestRotationRefuseUnCompteNonCreeParOgs(t *testing.T) {
	p, client, proj := fixture(t)
	client.users["shopfr"] = "celui-de-l-administrateur"
	if _, err := p.RotatePassword(context.Background(), proj); err == nil {
		t.Error("la rotation d'un compte qu'ogs n'a pas créé doit être refusée")
	}
	if client.users["shopfr"] != "celui-de-l-administrateur" {
		t.Error("le mot de passe du compte de l'administrateur a changé")
	}
}

func TestVhostDUnAutreDesigneApresCoupEstRefuse(t *testing.T) {
	// Le mot de passe scellé prouve le compte, pas le vhost : désigner après coup
	// le vhost « legacy » de l'administrateur y ouvrait les permissions du compte
	// et y déclarait files et bindings.
	p, client, proj := fixture(t)
	apply(t, p, proj)
	client.vhosts["legacy"] = true
	proj.Spec.AMQP.Vhost = "legacy"
	obs, err := p.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	if obs.VhostOwned() || !obs.UserOwned() {
		t.Fatalf("vhost tenu pour celui d'ogs (%v), ou compte refusé (%v)", obs.VhostOwned(), obs.UserOwned())
	}
	steps, err := p.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Critical {
		t.Fatalf("une seule étape non critique de refus est attendue : %+v", steps)
	}
	if err := steps[0].Apply(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "aucune marque scellée pour le vhost legacy") {
		t.Errorf("le refus doit nommer le vhost : %v", err)
	}
	if _, ok := client.permissions["legacy/shopfr"]; ok {
		t.Error("permissions posées sur le vhost de l'administrateur")
	}
}

func TestVhostDuParcExistantResteLeSien(t *testing.T) {
	// Un vhost créé avant la marque (D93) n'en porte pas : le compte d'ogs qui y a
	// ses permissions suffit, et le parc existant ne voit aucun écart.
	p, _, proj := fixture(t)
	apply(t, p, proj)
	if err := p.Store.DeleteSecret(proj.ID(), VhostMarker("shopfr")); err != nil {
		t.Fatal(err)
	}
	// Le premier plan ne porte que la marque : le critère des permissions s'éteint
	// avec lui.
	plan := replan(t, p, proj)
	if m := plan.Mutating(); len(m) != 1 || !strings.Contains(m[0].Summary, "marque de propriété") {
		t.Fatalf("seule la marque du vhost doit être posée :\n%s", plan.String())
	}
	apply(t, p, proj)
	if !p.sealed(proj.ID(), VhostMarker("shopfr")) {
		t.Fatal("la marque du vhost antérieur doit être scellée")
	}
	if plan := replan(t, p, proj); !plan.Empty() {
		t.Errorf("le plan doit être vide après la migration :\n%s", plan.String())
	}
}

func TestPermissionAccordeeParLAdministrateurNeFaitPasAdopterSonVhost(t *testing.T) {
	// Le critère du parc antérieur, sans borne, rouvrait R2-40 : l'administrateur
	// accorde au compte du projet une permission restreinte sur son vhost commun,
	// spec.amqp.vhost le désigne, et le plan élargissait les droits à « .* ».
	p, client, proj := fixture(t)
	apply(t, p, proj)
	client.vhosts["legacy"] = true
	lecture := Permission{Vhost: "legacy", User: "shopfr", Configure: "", Write: "", Read: "^lecture$"}
	client.permissions["legacy/shopfr"] = lecture
	proj.Spec.AMQP.Vhost = "legacy"
	obs, err := p.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	if obs.VhostOwned() {
		t.Fatal("un projet marqué ne doit adopter aucun vhost par ses permissions")
	}
	steps, err := p.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Critical {
		t.Fatalf("une seule étape non critique de refus est attendue : %+v", steps)
	}
	if client.permissions["legacy/shopfr"] != lecture {
		t.Error("la permission de l'administrateur a changé")
	}
}

func TestMarqueRetireeQuandLeVhostNePeutEtreCree(t *testing.T) {
	// Restée sans vhost, la marque faisait adopter l'homonyme créé ensuite par un
	// autre : la compensation ne couvre pas l'étape en échec.
	p, client, proj := fixture(t)
	client.failVhost = errors.New("broker refuse")
	obs, err := p.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := p.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if err := steps[0].Apply(context.Background()); err == nil {
		t.Fatal("la création refusée doit faire échouer l'étape")
	}
	if p.sealed(proj.ID(), VhostMarker("shopfr")) {
		t.Error("la marque doit être retirée quand le vhost n'a pas été créé")
	}
}

func TestMarqueDuVhostPoseeAvantLui(t *testing.T) {
	// Une interruption entre la création du vhost et celle du compte laissait un
	// vhost d'ogs que le plan suivant refusait pour toujours.
	p, client, proj := fixture(t)
	obs, err := p.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := p.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if err := steps[0].Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !client.vhosts["shopfr"] {
		t.Fatal("la première étape doit créer le vhost")
	}
	obs, err = p.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Owned() {
		t.Fatal("le vhost qu'ogs vient de créer doit rester le sien")
	}
	apply(t, p, proj)
	if plan := replan(t, p, proj); !plan.Empty() {
		t.Errorf("le plan doit être vide :\n%s", plan.String())
	}
}
