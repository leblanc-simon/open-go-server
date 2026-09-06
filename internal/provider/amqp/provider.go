package amqp

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/secret"
	"leblanc.io/open-go-server/internal/store"
)

// Provider réconcilie le vhost RabbitMQ d'un projet.
type Provider struct {
	Paths  config.Paths
	Store  *store.Store
	Keys   *secret.Keystore
	Client Client
	// RecreateQueues autorise la suppression et la recréation d'une queue dont les
	// arguments ont changé. Jamais implicite : l'opération détruit les messages en
	// attente.
	RecreateQueues bool
	// Endpoint porte l'hôte et le port AMQP, pour les chaînes de connexion.
	Host string
	Port int
	// ManagementURL est l'adresse de l'API d'administration. La supervision en a
	// besoin : c'est par elle que les métriques du broker sont lues.
	ManagementURL string
}

// Name identifie le provider.
func (p *Provider) Name() string { return "amqp" }

// Observed est l'état réel du vhost d'un projet.
type Observed struct {
	Unavailable error
	Version     string
	VhostExists bool
	UserExists  bool
	Permission  *Permission
	Exchanges   map[string]Exchange
	Queues      map[string]Queue
	Bindings    []Binding
	Policies    map[string]Policy
	// Degraded indique que le broker est piloté sans le plugin management.
	Degraded bool
}

// Observe inspecte le broker.
func (p *Provider) Observe(ctx context.Context, proj *model.Project) (Observed, error) {
	obs := Observed{
		Exchanges: map[string]Exchange{},
		Queues:    map[string]Queue{},
		Policies:  map[string]Policy{},
	}
	if proj.Spec.AMQP == nil {
		return obs, nil
	}
	if p.Client == nil {
		obs.Unavailable = fmt.Errorf("%w : aucun accès configuré", ErrUnavailable)
		return obs, nil
	}
	version, err := p.Client.Overview(ctx)
	if err != nil {
		obs.Unavailable = err
		return obs, nil
	}
	obs.Version = version

	vhost := proj.Spec.AMQP.Vhost
	vhosts, err := p.Client.Vhosts(ctx)
	if err != nil {
		obs.Unavailable = err
		return obs, nil
	}
	for _, v := range vhosts {
		if v.Name == vhost {
			obs.VhostExists = true
		}
	}
	// Une lecture qui échoue après Overview et Vhosts n'est pas plus fatale que
	// les précédentes : les cinq passent par le même appel HTTP, et « broker
	// indisponible » ne peut pas être non critique sur /api/vhosts et fatal sur
	// /api/users à quelques millisecondes près. Rendue à l'appelant, l'erreur
	// faisait abandonner le plan **entier** — ni utilisateur système, ni vhost web,
	// ni certificat — et, sous « --all », les projets suivants du parc avec.
	users, err := p.Client.Users(ctx)
	if err != nil {
		obs.Unavailable = err
		return obs, nil
	}
	for _, u := range users {
		if u.Name == proj.Spec.AMQP.User {
			obs.UserExists = true
		}
	}
	perms, err := p.Client.Permissions(ctx)
	if err != nil {
		obs.Unavailable = err
		return obs, nil
	}
	for i := range perms {
		if perms[i].Vhost == vhost && perms[i].User == proj.Spec.AMQP.User {
			obs.Permission = &perms[i]
		}
	}
	if !obs.VhostExists {
		return obs, nil
	}
	if exchanges, err := p.Client.Exchanges(ctx, vhost); err == nil {
		for _, e := range exchanges {
			obs.Exchanges[e.Name] = e
		}
	}
	if queues, err := p.Client.Queues(ctx, vhost); err == nil {
		for _, q := range queues {
			obs.Queues[q.Name] = q
		}
	}
	if bindings, err := p.Client.Bindings(ctx, vhost); err == nil {
		obs.Bindings = bindings
	}
	if policies, err := p.Client.Policies(ctx, vhost); err == nil {
		for _, pol := range policies {
			obs.Policies[pol.Name] = pol
		}
	}
	return obs, nil
}

// Plan calcule les étapes du vhost, dans l'ordre des dépendances : vhost,
// utilisateur, permissions, exchanges, queues, bindings, policies.
func (p *Provider) Plan(ctx context.Context, proj *model.Project, obs Observed) ([]*engine.Step, error) {
	spec := proj.Spec.AMQP
	if spec == nil {
		return nil, nil
	}
	if obs.Unavailable != nil {
		s := engine.NewStep("amqp/vhost/"+spec.Vhost, engine.Create,
			fmt.Sprintf("vhost amqp %s — broker indisponible", spec.Vhost))
		// Non critique : un site dont le broker est absent reste déployable.
		s.Critical = false
		reason := obs.Unavailable
		s.Apply = func(context.Context) error { return reason }
		return []*engine.Step{s}, nil
	}

	var steps []*engine.Step
	steps = append(steps, p.vhostStep(spec, obs))
	userStep, err := p.userStep(proj, spec, obs)
	if err != nil {
		return nil, err
	}
	steps = append(steps, userStep)
	steps = append(steps, p.permissionStep(spec, obs))
	steps = append(steps, p.exchangeSteps(spec, obs)...)
	queueSteps, err := p.queueSteps(spec, obs)
	if err != nil {
		return nil, err
	}
	steps = append(steps, queueSteps...)
	steps = append(steps, p.bindingSteps(spec, obs)...)
	steps = append(steps, p.policySteps(spec, obs)...)
	return steps, nil
}

func (p *Provider) vhostStep(spec *model.AMQP, obs Observed) *engine.Step {
	resource := "amqp/vhost/" + spec.Vhost
	if obs.VhostExists {
		return engine.NewStep(resource, engine.NoOp, fmt.Sprintf("vhost amqp %s", spec.Vhost))
	}
	s := engine.NewStep(resource, engine.Create, fmt.Sprintf("vhost amqp %s", spec.Vhost))
	s.Apply = func(ctx context.Context) error { return p.Client.EnsureVhost(ctx, spec.Vhost) }
	s.Compensate = func(ctx context.Context) error { return p.Client.DeleteVhost(ctx, spec.Vhost) }
	return s
}

func (p *Provider) userStep(proj *model.Project, spec *model.AMQP, obs Observed) (*engine.Step, error) {
	resource := "amqp/user/" + spec.User
	if obs.UserExists {
		return engine.NewStep(resource, engine.NoOp, fmt.Sprintf("utilisateur amqp %s", spec.User)), nil
	}
	s := engine.NewStep(resource, engine.Create, fmt.Sprintf("utilisateur amqp %s", spec.User))
	s.Apply = func(ctx context.Context) error {
		password, err := p.ensurePassword(proj.ID(), spec.User)
		if err != nil {
			return err
		}
		return p.Client.EnsureUser(ctx, spec.User, password, nil)
	}
	s.Compensate = func(ctx context.Context) error { return p.Client.DeleteUser(ctx, spec.User) }
	return s, nil
}

func (p *Provider) permissionStep(spec *model.AMQP, obs Observed) *engine.Step {
	want := desiredPermission(spec)
	resource := "amqp/permission/" + spec.Vhost + "/" + spec.User
	summary := fmt.Sprintf("permissions de %s sur %s (configure %s, write %s, read %s)",
		spec.User, spec.Vhost, want.Configure, want.Write, want.Read)
	if obs.Permission != nil &&
		obs.Permission.Configure == want.Configure &&
		obs.Permission.Write == want.Write &&
		obs.Permission.Read == want.Read {
		return engine.NewStep(resource, engine.NoOp, summary)
	}
	action := engine.Create
	if obs.Permission != nil {
		action = engine.Update
	}
	s := engine.NewStep(resource, action, summary)
	s.Apply = func(ctx context.Context) error { return p.Client.EnsurePermission(ctx, want) }
	return s
}

// desiredPermission construit les permissions attendues. Les motifs portent sur
// le vhost du projet uniquement : le cloisonnement vient du vhost, pas des motifs.
func desiredPermission(spec *model.AMQP) Permission {
	get := func(key, fallback string) string {
		if spec.Permissions == nil {
			return fallback
		}
		if v, ok := spec.Permissions[key]; ok && v != "" {
			return v
		}
		return fallback
	}
	return Permission{
		Vhost: spec.Vhost, User: spec.User,
		Configure: get("configure", ".*"),
		Write:     get("write", ".*"),
		Read:      get("read", ".*"),
	}
}

func (p *Provider) exchangeSteps(spec *model.AMQP, obs Observed) []*engine.Step {
	var steps []*engine.Step
	for _, decl := range spec.Exchanges {
		decl := decl
		want := Exchange{
			Vhost: spec.Vhost, Name: decl.Name, Type: orDefault(decl.Type, "direct"),
			Durable: decl.Durable,
		}
		resource := "amqp/exchange/" + spec.Vhost + "/" + decl.Name
		summary := fmt.Sprintf("exchange %s (%s)", decl.Name, want.Type)
		if current, ok := obs.Exchanges[decl.Name]; ok {
			if current.Type == want.Type && current.Durable == want.Durable {
				steps = append(steps, engine.NewStep(resource, engine.NoOp, summary))
				continue
			}
			// Le type d'un exchange est immuable, comme les arguments d'une queue.
			s := engine.NewStep(resource, engine.Update,
				fmt.Sprintf("exchange %s : type %s → %s (recréation nécessaire)",
					decl.Name, current.Type, want.Type))
			s.Critical = false
			s.Apply = func(context.Context) error {
				return fmt.Errorf("le type d'un exchange est immuable : supprimer %s puis relancer, "+
					"ou déclarer un nouvel exchange", decl.Name)
			}
			steps = append(steps, s)
			continue
		}
		s := engine.NewStep(resource, engine.Create, summary)
		s.Apply = func(ctx context.Context) error { return p.Client.EnsureExchange(ctx, want) }
		steps = append(steps, s)
	}
	return steps
}

// immutableQueueArgs énumère les arguments qu'un broker refuse de redéclarer
// autrement. En pratique tout argument l'est : RabbitMQ répond PRECONDITION_FAILED
// dès que la redéclaration diffère.
var immutableQueueArgs = []string{
	"x-queue-type",
	"x-message-ttl",
	"x-expires",
	"x-dead-letter-exchange",
	"x-dead-letter-routing-key",
	"x-max-length",
	"x-max-length-bytes",
	"x-max-priority",
	"x-overflow",
	"x-single-active-consumer",
}

// QueueDrift décrit un écart entre la queue déclarée et la queue observée.
type QueueDrift struct {
	Name      string
	Changes   []string
	Messages  int
	Consumers int
}

// String rend l'écart lisible dans le plan.
func (d QueueDrift) String() string {
	return fmt.Sprintf("queue %s : %s", d.Name, strings.Join(d.Changes, ", "))
}

// diffQueue compare la déclaration et l'observation, et renvoie les écarts
// portant sur des propriétés immuables.
func diffQueue(want Queue, current Queue) []string {
	var changes []string
	if want.Durable != current.Durable {
		changes = append(changes, fmt.Sprintf("durable %v → %v", current.Durable, want.Durable))
	}
	if wantType, currentType := orDefault(want.Type, "classic"), current.EffectiveType(); wantType != currentType {
		changes = append(changes, fmt.Sprintf("type %s → %s", currentType, wantType))
	}
	for _, key := range immutableQueueArgs {
		if key == "x-queue-type" {
			continue // déjà traité par le type effectif
		}
		wantValue, wantOK := want.Arguments[key]
		currentValue, currentOK := current.Arguments[key]
		switch {
		case !wantOK && !currentOK:
			continue
		case wantOK != currentOK, !sameArgument(wantValue, currentValue):
			changes = append(changes, fmt.Sprintf("%s %s → %s",
				key, describeArg(currentValue, currentOK), describeArg(wantValue, wantOK)))
		}
	}
	sort.Strings(changes)
	return changes
}

// sameArgument compare deux valeurs d'argument. Le JSON de l'API renvoie les
// nombres en float64 : une comparaison directe avec un entier déclaré en YAML
// signalerait un faux écart à chaque application.
func sameArgument(a, b any) bool {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		return af == bf
	}
	return reflect.DeepEqual(a, b)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func describeArg(v any, present bool) string {
	if !present {
		return "absent"
	}
	if f, ok := toFloat(v); ok && f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%v", v)
}

func (p *Provider) queueSteps(spec *model.AMQP, obs Observed) ([]*engine.Step, error) {
	var steps []*engine.Step
	for _, decl := range spec.Queues {
		decl := decl
		want := Queue{
			Vhost: spec.Vhost, Name: decl.Name, Durable: decl.Durable,
			Type: orDefault(decl.Type, "classic"), Arguments: decl.Arguments,
		}
		resource := "amqp/queue/" + spec.Vhost + "/" + decl.Name
		current, exists := obs.Queues[decl.Name]
		if !exists {
			s := engine.NewStep(resource, engine.Create,
				fmt.Sprintf("queue %s (%s%s)", decl.Name, want.Type, describeArgs(want.Arguments)))
			s.Apply = func(ctx context.Context) error { return p.Client.EnsureQueue(ctx, want) }
			s.Compensate = func(ctx context.Context) error {
				return p.Client.DeleteQueue(ctx, spec.Vhost, decl.Name)
			}
			steps = append(steps, s)
			continue
		}
		changes := diffQueue(want, current)
		if len(changes) == 0 {
			steps = append(steps, engine.NewStep(resource, engine.NoOp,
				fmt.Sprintf("queue %s (%d message(s), %d consumer(s))",
					decl.Name, current.Messages, current.Consumers)))
			continue
		}
		// Les arguments d'une queue sont immuables : le broker refuse toute
		// redéclaration divergente. La seule voie est la recréation, qui détruit
		// les messages en attente — elle ne peut donc jamais être implicite.
		drift := QueueDrift{Name: decl.Name, Changes: changes,
			Messages: current.Messages, Consumers: current.Consumers}
		if !p.RecreateQueues {
			s := engine.NewStep(resource, engine.Update,
				fmt.Sprintf("%s — recréation requise (%d message(s) en attente)",
					drift.String(), current.Messages))
			s.Critical = false
			s.Apply = func(context.Context) error {
				return fmt.Errorf("les arguments d'une queue sont immuables : recréer %s détruirait "+
					"%d message(s) en attente et %d consumer(s) seraient déconnectés — "+
					"relancer avec --recreate-queues pour l'accepter",
					decl.Name, current.Messages, current.Consumers)
			}
			steps = append(steps, s)
			continue
		}
		s := engine.NewStep(resource, engine.Update,
			fmt.Sprintf("%s — recréation (%d message(s) perdu(s))", drift.String(), current.Messages))
		s.Apply = func(ctx context.Context) error {
			if err := p.Client.DeleteQueue(ctx, spec.Vhost, decl.Name); err != nil {
				return err
			}
			return p.Client.EnsureQueue(ctx, want)
		}
		steps = append(steps, s)
	}
	return steps, nil
}

func describeArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	parts := make([]string, 0, len(args))
	for _, k := range SortedKeys(args) {
		parts = append(parts, fmt.Sprintf("%s=%v", k, args[k]))
	}
	return ", " + strings.Join(parts, " ")
}

func (p *Provider) bindingSteps(spec *model.AMQP, obs Observed) []*engine.Step {
	var steps []*engine.Step
	for _, decl := range spec.Bindings {
		decl := decl
		want := Binding{
			Vhost: spec.Vhost, Source: decl.Exchange, Destination: decl.Queue,
			DestinationType: "queue", RoutingKey: decl.RoutingKey,
		}
		resource := fmt.Sprintf("amqp/binding/%s/%s/%s", spec.Vhost, decl.Exchange, decl.Queue)
		summary := fmt.Sprintf("binding %s → %s (clé %s)",
			decl.Exchange, decl.Queue, orDefault(decl.RoutingKey, "«vide»"))
		if bindingExists(obs.Bindings, want) {
			steps = append(steps, engine.NewStep(resource, engine.NoOp, summary))
			continue
		}
		s := engine.NewStep(resource, engine.Create, summary)
		s.Apply = func(ctx context.Context) error { return p.Client.EnsureBinding(ctx, want) }
		steps = append(steps, s)
	}
	return steps
}

func bindingExists(observed []Binding, want Binding) bool {
	for _, b := range observed {
		if b.Source == want.Source && b.Destination == want.Destination &&
			b.RoutingKey == want.RoutingKey {
			return true
		}
	}
	return false
}

func (p *Provider) policySteps(spec *model.AMQP, obs Observed) []*engine.Step {
	var steps []*engine.Step
	for _, decl := range spec.Policies {
		decl := decl
		want := Policy{
			Vhost: spec.Vhost, Name: decl.Name, Pattern: decl.Pattern,
			Definition: decl.Definition, ApplyTo: "queues",
		}
		resource := "amqp/policy/" + spec.Vhost + "/" + decl.Name
		summary := fmt.Sprintf("policy %s sur %s", decl.Name, decl.Pattern)
		if current, ok := obs.Policies[decl.Name]; ok &&
			current.Pattern == want.Pattern && sameDefinition(current.Definition, want.Definition) {
			steps = append(steps, engine.NewStep(resource, engine.NoOp, summary))
			continue
		}
		action := engine.Create
		if _, ok := obs.Policies[decl.Name]; ok {
			action = engine.Update
		}
		s := engine.NewStep(resource, action, summary)
		s.Apply = func(ctx context.Context) error { return p.Client.EnsurePolicy(ctx, want) }
		steps = append(steps, s)
	}
	return steps
}

func sameDefinition(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !sameArgument(av, bv) {
			return false
		}
	}
	return true
}

// ensurePassword génère et scelle le mot de passe du compte AMQP, ou le relit.
func (p *Provider) ensurePassword(project, user string) (string, error) {
	if p.Store == nil || p.Keys == nil {
		return "", fmt.Errorf("keystore indisponible : impossible de gérer le mot de passe de %s", user)
	}
	name := "amqp/" + user
	nonce, ciphertext, err := p.Store.GetSecret(project, name)
	if err == nil {
		plain, err := p.Keys.Open(project, name, nonce, ciphertext)
		if err != nil {
			return "", err
		}
		return string(plain), nil
	}
	password, err := secret.Password(0)
	if err != nil {
		return "", err
	}
	nonce, ciphertext, err = p.Keys.Seal(project, name, []byte(password))
	if err != nil {
		return "", err
	}
	if err := p.Store.PutSecret(project, name, nonce, ciphertext); err != nil {
		return "", err
	}
	return password, nil
}

// RotatePassword régénère le mot de passe du compte AMQP.
func (p *Provider) RotatePassword(ctx context.Context, proj *model.Project) (string, error) {
	if proj.Spec.AMQP == nil {
		return "", fmt.Errorf("le projet %s ne déclare pas de transport AMQP", proj.ID())
	}
	user := proj.Spec.AMQP.User
	password, err := secret.Password(0)
	if err != nil {
		return "", err
	}
	if err := p.Client.EnsureUser(ctx, user, password, nil); err != nil {
		return "", err
	}
	name := "amqp/" + user
	nonce, ciphertext, err := p.Keys.Seal(proj.ID(), name, []byte(password))
	if err != nil {
		return "", err
	}
	if err := p.Store.PutSecret(proj.ID(), name, nonce, ciphertext); err != nil {
		return "", err
	}
	return password, nil
}
