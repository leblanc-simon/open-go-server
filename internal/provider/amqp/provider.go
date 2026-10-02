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
	// Fleet rend les déclarations du parc : un vhost ou un compte qu'un autre
	// projet déclare n'est jamais retiré comme obsolète, preuve ou non — un
	// voisin antérieur à D93 n'en porte aucune (D100). Nil, ou en échec, tout
	// retrait d'obsolète est contesté : c'est le sens qui ne supprime rien.
	Fleet func() ([]*model.Project, error)
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
	// Incomplete nomme les lectures du vhost en échec — exchanges, files,
	// bindings, policies. Une lecture en échec n'est pas une absence (Q-20) :
	// aucune création ni aucun retrait ne se fonde sur elle, et la trace de
	// D100 reste telle quelle.
	Incomplete []string
	// Degraded indique que le broker est piloté sans le plugin management.
	Degraded bool
	// Sealed dit si le keystore porte le mot de passe du compte pour ce projet :
	// il est scellé juste avant qu'ogs crée le compte, jamais autrement.
	Sealed bool
	// VhostSealed dit si le keystore porte la marque du vhost pour ce projet
	// (VhostMarker), scellée juste avant qu'ogs crée le vhost.
	VhostSealed bool
	// Marked dit si le projet porte au moins une marque de vhost, quel qu'il
	// soit : il a été planifié depuis D93, et le critère du parc antérieur ne
	// vaut plus pour lui (VhostOwned).
	Marked bool
	// Declared est la trace de ce qu'ogs a déclaré dans le vhost au dernier
	// apply (DeclaredMarker), vide si elle est absente ; DeclaredSealed dit si
	// elle existe.
	Declared       Declared
	DeclaredSealed bool
	// ObsoleteVhosts et ObsoleteUsers sont les vhosts et comptes qu'ogs a créés
	// pour le projet et que la déclaration ne désigne plus (D100).
	ObsoleteVhosts []ObsoleteVhost
	ObsoleteUsers  []ObsoleteUser
}

// VhostMarker nomme le secret qui marque un vhost créé par ogs. Sa valeur est le
// nom du vhost : seul compte qu'il existe, scellé au projet.
func VhostMarker(vhost string) string { return "amqp-vhost/" + vhost }

// UserOwned dit si le compte observé, s'il existe, est celui qu'ogs a créé pour
// le projet (D93) : son mot de passe est scellé.
func (o Observed) UserOwned() bool { return !o.UserExists || o.Sealed }

// VhostOwned dit si le vhost observé, s'il existe, est celui qu'ogs a créé pour
// le projet (D93). Le secret du compte ne le prouve pas : il prouve le compte, et
// un spec.amqp.vhost changé vers le vhost d'un autre le laisserait intact. La
// preuve est la marque du vhost, ou le critère du parc antérieur (Legacy).
func (o Observed) VhostOwned() bool { return !o.VhostExists || o.VhostSealed || o.Legacy() }

// Legacy dit si le vhost est admis par le seul critère du parc antérieur à D93 :
// un compte d'ogs qui y a déjà ses permissions, dans un projet qui ne porte
// encore **aucune** marque de vhost. Sans cette dernière condition, le critère
// valait pour toujours : il suffisait que l'administrateur accorde au compte du
// projet une permission, même restreinte, sur son vhost commun, puis que
// spec.amqp.vhost le désigne, pour que le plan y élargisse les droits à « .* »
// et que la suppression l'emporte avec ses messages. Le premier plan qui admet un
// vhost ainsi en scelle la marque (vhostStep) : le critère s'éteint avec lui.
func (o Observed) Legacy() bool {
	return o.VhostExists && !o.VhostSealed && !o.Marked && o.Sealed && o.Permission != nil
}

// Owned dit si le vhost et le compte observés, quand ils existent, sont ceux
// qu'ogs a créés pour le projet (D93). Le broker ne porte aucune marque : ce sont
// les secrets scellés au keystore qui font foi. Un vhost ou un compte homonymes
// sans eux appartiennent à quelqu'un d'autre — l'administrateur, une application
// restée hors d'ogs.
func (o Observed) Owned() bool { return o.VhostOwned() && o.UserOwned() }

// Observe inspecte le broker.
func (p *Provider) Observe(ctx context.Context, proj *model.Project) (Observed, error) {
	obs := Observed{
		Exchanges: map[string]Exchange{},
		Queues:    map[string]Queue{},
		Policies:  map[string]Policy{},
	}
	spec := proj.Spec.AMQP
	// Les vhosts et comptes qu'ogs a créés pour le projet et que la déclaration
	// ne désigne plus se connaissent par ses secrets (D100), spec.amqp retiré
	// compris. Un keystore illisible n'en fait connaître aucun : rien n'est alors
	// retiré.
	oldVhosts, oldUsers, _, _ := p.leftovers(proj.ID(), spec)
	if spec == nil && len(oldVhosts)+len(oldUsers) == 0 {
		return obs, nil
	}
	if p.Client == nil {
		obs.Unavailable = fmt.Errorf("%w : aucun accès configuré", ErrUnavailable)
		return obs, nil
	}
	_, obs.Degraded = p.Client.(*ctlClient)
	version, err := p.Client.Overview(ctx)
	if err != nil {
		obs.Unavailable = err
		return obs, nil
	}
	obs.Version = version
	vhosts, err := p.Client.Vhosts(ctx)
	if err != nil {
		obs.Unavailable = err
		return obs, nil
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
	vhostExists := map[string]bool{}
	for _, v := range vhosts {
		vhostExists[v.Name] = true
	}
	userExists := map[string]bool{}
	for _, u := range users {
		userExists[u.Name] = true
	}
	for _, name := range oldVhosts {
		v := ObsoleteVhost{Name: name, Exists: vhostExists[name],
			Contested: p.contestedBy(proj.ID(), VhostMarker(name), func(a *model.AMQP) bool { return a.Vhost == name })}
		if v.Exists {
			v.Queues, v.QueueErr = p.Client.Queues(ctx, name)
		}
		obs.ObsoleteVhosts = append(obs.ObsoleteVhosts, v)
	}
	for _, name := range oldUsers {
		obs.ObsoleteUsers = append(obs.ObsoleteUsers, ObsoleteUser{Name: name, Exists: userExists[name],
			Contested: p.contestedBy(proj.ID(), userSecretPrefix+name, func(a *model.AMQP) bool { return a.User == name })})
	}
	if spec == nil {
		return obs, nil
	}
	obs.Sealed = p.sealed(proj.ID(), userSecretPrefix+spec.User)
	obs.VhostSealed = p.sealed(proj.ID(), VhostMarker(spec.Vhost))
	obs.Marked = obs.VhostSealed || p.marked(proj.ID())
	obs.DeclaredSealed = p.sealed(proj.ID(), DeclaredMarker(spec.Vhost))
	obs.Declared = p.readDeclared(proj.ID(), spec.Vhost)

	vhost := spec.Vhost
	obs.VhostExists = vhostExists[vhost]
	obs.UserExists = userExists[spec.User]
	perms, err := p.Client.Permissions(ctx)
	if err != nil {
		obs.Unavailable = err
		return obs, nil
	}
	for i := range perms {
		if perms[i].Vhost == vhost && perms[i].User == spec.User {
			obs.Permission = &perms[i]
		}
	}
	if !obs.VhostExists {
		return obs, nil
	}
	incomplete := func(what string, err error) {
		obs.Incomplete = append(obs.Incomplete, fmt.Sprintf("%s (%v)", what, err))
	}
	if exchanges, err := p.Client.Exchanges(ctx, vhost); err == nil {
		for _, e := range exchanges {
			obs.Exchanges[e.Name] = e
		}
	} else {
		incomplete("exchanges", err)
	}
	if queues, err := p.Client.Queues(ctx, vhost); err == nil {
		for _, q := range queues {
			obs.Queues[q.Name] = q
		}
	} else {
		incomplete("files", err)
	}
	if bindings, err := p.Client.Bindings(ctx, vhost); err == nil {
		obs.Bindings = bindings
	} else {
		incomplete("bindings", err)
	}
	if policies, err := p.Client.Policies(ctx, vhost); err == nil {
		for _, pol := range policies {
			obs.Policies[pol.Name] = pol
		}
	} else {
		incomplete("policies", err)
	}
	return obs, nil
}

// Plan calcule les étapes du vhost, dans l'ordre des dépendances : vhost,
// utilisateur, permissions, exchanges, queues, bindings, policies.
func (p *Provider) Plan(ctx context.Context, proj *model.Project, obs Observed) ([]*engine.Step, error) {
	spec := proj.Spec.AMQP
	if spec == nil {
		// La déclaration ne porte plus d'AMQP : ce qu'ogs avait créé pour le
		// projet se retire comme un vhost ou un compte remplacés (D100).
		if len(obs.ObsoleteVhosts)+len(obs.ObsoleteUsers) == 0 && obs.Unavailable == nil {
			return nil, nil
		}
		if obs.Unavailable != nil {
			// Comme pour les bases : un broker injoignable ou désinstallé alors que
			// plus rien ne le déclare n'est pas un défaut du projet. Une étape en
			// échec rendait le code 6 à chaque apply et à chaque diff, sans issue.
			// Les preuves restent, et le prochain apply qui le joint retire ce
			// qu'elles prouvent.
			return []*engine.Step{engine.NewStep("amqp/obsolete/"+proj.ID(), engine.NoOp,
				fmt.Sprintf("ressources amqp que la déclaration ne porte plus — broker injoignable, "+
					"conservées avec leurs preuves (%v)", obs.Unavailable))}, nil
		}
		return p.obsoleteSteps(proj, obs, true, true), nil
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
	if !obs.Owned() {
		return []*engine.Step{foreignStep(spec, obs)}, nil
	}

	var steps []*engine.Step
	steps = append(steps, p.vhostStep(proj, spec, obs))
	userStep, err := p.userStep(proj, spec, obs)
	if err != nil {
		return nil, err
	}
	steps = append(steps, userStep)
	steps = append(steps, p.permissionStep(spec, obs))
	// Une lecture du vhost en échec n'est pas une absence (Q-20) : créer ce
	// qu'on n'a pas pu lire, ou retirer de la trace ce qu'on n'a pas vu, se
	// fonderait sur du vide. Vhost, compte et permissions, lus sans faute, sont
	// réconciliés ; le reste attend une lecture complète, et l'étape le dit.
	if len(obs.Incomplete) > 0 {
		s := engine.NewStep("amqp/incomplete/"+spec.Vhost, engine.Update, fmt.Sprintf(
			"vhost amqp %s — lecture incomplète (%s) : exchanges, files, bindings et policies non réconciliés",
			spec.Vhost, strings.Join(obs.Incomplete, ", ")))
		s.Critical = false
		reason := fmt.Errorf("lecture du vhost %s en échec : %s — rien n'est créé ni retiré sur cette base ; "+
			"relancer l'apply une fois le broker joignable", spec.Vhost, strings.Join(obs.Incomplete, ", "))
		s.Apply = func(context.Context) error { return reason }
		steps = append(steps, s)
		return steps, nil
	}
	steps = append(steps, p.exchangeSteps(spec, obs)...)
	queueSteps, err := p.queueSteps(spec, obs)
	if err != nil {
		return nil, err
	}
	steps = append(steps, queueSteps...)
	steps = append(steps, p.bindingSteps(spec, obs)...)
	steps = append(steps, p.policySteps(spec, obs)...)
	// Ce que la déclaration ne porte plus vient après ce qu'elle porte : un
	// binding retiré part avant sa file, une file avant son exchange, et le vhost
	// remplacé une fois le nouveau en place (D100). Rien de cela quand le vhost
	// ou le compte déclarés sont refusés : l'issue la plus probable est de revenir
	// à la déclaration précédente.
	steps = append(steps, p.retiredSteps(proj, spec, obs)...)
	// L'ancien vhost ou l'ancien compte ne partent que si leur remplaçant existait
	// à l'observation : l'apply qui le crée les garde, sans quoi une saga
	// déroulée plus loin retirerait le nouveau et laisserait le projet sans
	// aucun (D100).
	steps = append(steps, p.obsoleteSteps(proj, obs, obs.VhostExists, obs.UserExists)...)
	return steps, nil
}

// foreignStep refuse de toucher à un vhost ou à un compte homonymes qu'ogs n'a pas
// créés. Les reprendre, c'était laisser le projet sans mot de passe — le compte
// gardait le sien, que personne ne pouvait donner à l'application —, puis les
// détruire à « project delete » avec les messages en attente de leur véritable
// propriétaire. Aucune étape n'est planifiée sur eux, pas même les permissions :
// ouvrir le vhost d'un autre au compte du projet serait pire que de ne rien faire.
// L'étape n'est pas critique — un site sans broker reste déployable, comme sous
// l'indisponibilité — et son échec rend le code 6.
func foreignStep(spec *model.AMQP, obs Observed) *engine.Step {
	var what []string
	if !obs.VhostOwned() {
		what = append(what, "le vhost "+spec.Vhost)
	}
	if !obs.UserOwned() {
		what = append(what, "le compte "+spec.User)
	}
	names := strings.Join(what, " et ")
	exists, created, pronoun := "existe", "créé", "le"
	if len(what) > 1 {
		exists, created, pronoun = "existent", "créés", "les"
	}
	s := engine.NewStep("amqp/vhost/"+spec.Vhost, engine.Create, fmt.Sprintf(
		"vhost amqp %s — refusé : %s %s déjà sans avoir été %s par ogs", spec.Vhost, names, exists, created))
	s.Critical = false
	reason := fmt.Errorf("%s : ogs ne %s reprend pas, et ne %s supprimera pas. Déclarer d'autres noms "+
		"(spec.amqp.vhost, spec.amqp.user) qui n'existent pas encore sur le broker, ou retirer l'homonyme "+
		"du broker s'il n'a plus d'usage", ForeignReason(spec, obs), pronoun, pronoun)
	s.Apply = func(context.Context) error { return reason }
	return s
}

// ForeignReason dit pourquoi le vhost ou le compte d'un projet ne sont pas tenus
// pour ceux d'ogs.
func ForeignReason(spec *model.AMQP, obs Observed) string {
	var parts []string
	if !obs.VhostOwned() {
		parts = append(parts, fmt.Sprintf("aucune marque scellée pour le vhost %s", spec.Vhost))
	}
	if !obs.UserOwned() {
		parts = append(parts, fmt.Sprintf("aucun mot de passe scellé pour le compte %s", spec.User))
	}
	return strings.Join(parts, ", ")
}

func (p *Provider) vhostStep(proj *model.Project, spec *model.AMQP, obs Observed) *engine.Step {
	resource := "amqp/vhost/" + spec.Vhost
	project := proj.ID()
	if obs.Legacy() {
		// Migration du parc antérieur à D93 : la marque est posée une fois, et le
		// critère des permissions ne sert plus jamais à ce projet.
		s := engine.NewStep(resource, engine.Update,
			fmt.Sprintf("vhost amqp %s — marque de propriété (vhost antérieur à D93)", spec.Vhost))
		s.Apply = func(context.Context) error { return p.sealMarker(project, spec.Vhost) }
		// Un keystore en échec ne doit pas interrompre l'apply du projet : la
		// marque sera reposée au passage suivant, comme les autres étapes AMQP.
		s.Critical = false
		return s
	}
	if obs.VhostExists {
		return engine.NewStep(resource, engine.NoOp, fmt.Sprintf("vhost amqp %s", spec.Vhost))
	}
	s := engine.NewStep(resource, engine.Create, fmt.Sprintf("vhost amqp %s", spec.Vhost))
	s.Apply = func(ctx context.Context) error {
		// La marque précède le vhost : dans l'ordre inverse, une interruption
		// entre les deux laisserait un vhost d'ogs refusé pour toujours.
		if err := p.sealMarker(project, spec.Vhost); err != nil {
			return err
		}
		if err := p.Client.EnsureVhost(ctx, spec.Vhost); err != nil {
			// Restée sans vhost, la marque ferait adopter l'homonyme qu'un autre
			// créerait ensuite : la compensation ne couvre pas l'étape en échec.
			// Elle n'est retirée que si cette étape l'a posée.
			if !obs.VhostSealed {
				_ = p.Store.DeleteSecret(project, VhostMarker(spec.Vhost))
			}
			return err
		}
		return nil
	}
	s.Compensate = func(ctx context.Context) error {
		if err := p.Client.DeleteVhost(ctx, spec.Vhost); err != nil {
			return err
		}
		return p.Store.DeleteSecret(project, VhostMarker(spec.Vhost))
	}
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

// sealed dit si le keystore porte le secret nommé pour le projet. Une lecture qui
// échoue vaut absence : c'est le sens prudent, celui qui refuse de reprendre et de
// supprimer.
func (p *Provider) sealed(project, name string) bool {
	if p.Store == nil {
		return false
	}
	_, _, err := p.Store.GetSecret(project, name)
	return err == nil
}

// marked dit si le projet porte au moins une marque de vhost. Une lecture qui
// échoue vaut présence : c'est le sens prudent, celui qui refuse le critère du
// parc antérieur.
func (p *Provider) marked(project string) bool {
	if p.Store == nil {
		return true
	}
	names, err := p.Store.SecretNames(project)
	if err != nil {
		return true
	}
	for _, n := range names {
		if strings.HasPrefix(n, VhostMarker("")) {
			return true
		}
	}
	return false
}

// sealMarker scelle la marque d'un vhost qu'ogs s'apprête à créer.
func (p *Provider) sealMarker(project, vhost string) error {
	if p.Store == nil || p.Keys == nil {
		return fmt.Errorf("keystore indisponible : impossible de marquer le vhost %s", vhost)
	}
	name := VhostMarker(vhost)
	nonce, ciphertext, err := p.Keys.Seal(project, name, []byte(vhost))
	if err != nil {
		return err
	}
	return p.Store.PutSecret(project, name, nonce, ciphertext)
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
	// Un compte dont ogs n'a pas scellé le mot de passe n'est pas le sien (D93) :
	// le changer couperait l'application de son propriétaire.
	if !p.sealed(proj.ID(), "amqp/"+user) {
		return "", fmt.Errorf("le compte amqp %s n'a pas été créé par ogs : son mot de passe n'est pas "+
			"changé", user)
	}
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
