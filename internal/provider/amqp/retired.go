package amqp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
)

// Ce fichier porte ce qu'une déclaration ne demande plus (D100) : les files,
// bindings, exchanges et policies qu'ogs a déclarés dans le vhost du projet puis
// retirés de la déclaration, les vhosts et comptes d'ogs qu'un spec.amqp
// changé — ou retiré — a laissés derrière lui, et leur suppression avec le
// projet.
//
// Rien de tout cela ne se juge sur le broker seul : l'application crée ses
// propres files et exchanges dans son vhost (Messenger, auto_setup), et retirer
// ce qu'elle a créé ferait osciller l'apply ou la casserait. La preuve est, comme
// pour le vhost et le compte (D93), un secret scellé au projet : la trace
// DeclaredMarker de ce qu'ogs a déclaré dans le vhost au dernier apply.

// DeclaredMarker nomme le secret qui garde la trace de ce qu'ogs a déclaré dans
// un vhost. Il voyage avec l'archive du projet, comme les autres marques.
func DeclaredMarker(vhost string) string { return "amqp-declared/" + vhost }

// userSecretPrefix préfixe le mot de passe scellé d'un compte du projet.
const userSecretPrefix = "amqp/"

// Declared est la trace de ce qu'ogs a déclaré dans un vhost.
type Declared struct {
	Exchanges []string          `json:"exchanges,omitempty"`
	Queues    []string          `json:"queues,omitempty"`
	Bindings  []DeclaredBinding `json:"bindings,omitempty"`
	Policies  []string          `json:"policies,omitempty"`
}

// DeclaredBinding identifie un binding déclaré.
type DeclaredBinding struct {
	Exchange   string `json:"exchange"`
	Queue      string `json:"queue"`
	RoutingKey string `json:"routingKey,omitempty"`
}

func (b DeclaredBinding) key() string { return b.Exchange + "\x00" + b.Queue + "\x00" + b.RoutingKey }

// declaredOf rend ce que la déclaration demande, trié.
func declaredOf(spec *model.AMQP) Declared {
	var d Declared
	for _, e := range spec.Exchanges {
		d.Exchanges = append(d.Exchanges, e.Name)
	}
	for _, q := range spec.Queues {
		d.Queues = append(d.Queues, q.Name)
	}
	for _, b := range spec.Bindings {
		d.Bindings = append(d.Bindings, DeclaredBinding{b.Exchange, b.Queue, b.RoutingKey})
	}
	for _, pol := range spec.Policies {
		d.Policies = append(d.Policies, pol.Name)
	}
	d.normalize()
	return d
}

func (d *Declared) normalize() {
	d.Exchanges = uniqueSorted(d.Exchanges)
	d.Queues = uniqueSorted(d.Queues)
	d.Policies = uniqueSorted(d.Policies)
	seen := map[string]bool{}
	var bindings []DeclaredBinding
	for _, b := range d.Bindings {
		if !seen[b.key()] {
			seen[b.key()] = true
			bindings = append(bindings, b)
		}
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].key() < bindings[j].key() })
	d.Bindings = bindings
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func (d Declared) equal(o Declared) bool {
	a, _ := json.Marshal(d)
	b, _ := json.Marshal(o)
	return string(a) == string(b)
}

// ObsoleteVhost est un vhost qu'ogs a créé pour le projet (sa marque est
// scellée) et que la déclaration ne désigne plus.
type ObsoleteVhost struct {
	Name   string
	Exists bool
	// Queues porte les files observées : un vhost dont une file a des messages ou
	// des consommateurs n'est pas retiré par un apply.
	Queues   []Queue
	QueueErr error
	// Contested nomme un autre projet qui en porte aussi la preuve : ce n'est
	// alors plus la ressource de ce seul projet.
	Contested string
}

// ObsoleteUser est un compte qu'ogs a créé pour le projet (son mot de passe
// est scellé) et que la déclaration ne désigne plus.
type ObsoleteUser struct {
	Name      string
	Exists    bool
	Contested string
}

// leftovers énumère, d'après les secrets du projet, les vhosts et comptes qu'ogs
// a créés pour lui et que la déclaration ne désigne plus, ainsi que les traces
// de déclaration.
func (p *Provider) leftovers(project string, spec *model.AMQP) (vhosts, users, traces []string, err error) {
	if p.Store == nil {
		return nil, nil, nil, nil
	}
	names, err := p.Store.SecretNames(project)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, n := range names {
		switch {
		case strings.HasPrefix(n, VhostMarker("")):
			v := strings.TrimPrefix(n, VhostMarker(""))
			if spec == nil || v != spec.Vhost {
				vhosts = append(vhosts, v)
			}
		case strings.HasPrefix(n, DeclaredMarker("")):
			traces = append(traces, strings.TrimPrefix(n, DeclaredMarker("")))
		case strings.HasPrefix(n, userSecretPrefix):
			u := strings.TrimPrefix(n, userSecretPrefix)
			if spec == nil || u != spec.User {
				users = append(users, u)
			}
		}
	}
	sort.Strings(vhosts)
	sort.Strings(users)
	sort.Strings(traces)
	return vhosts, users, traces, nil
}

// HasLeftovers dit si le projet porte encore la preuve d'un vhost ou d'un compte
// que sa déclaration ne désigne plus : l'apply doit alors les juger, même sans
// spec.amqp. Une lecture qui échoue vaut absence : rien n'est retiré sur un
// keystore illisible.
func (p *Provider) HasLeftovers(proj *model.Project) bool {
	vhosts, users, _, err := p.leftovers(proj.ID(), proj.Spec.AMQP)
	return err == nil && len(vhosts)+len(users) > 0
}

// contested nomme un autre projet dont le keystore porte le même secret : la
// ressource n'est plus alors la preuve d'un seul projet — le vhost supprimé à la
// main puis recréé par un voisin, par exemple. Une lecture qui échoue vaut
// contestation : c'est le sens qui ne supprime rien.
func (p *Provider) contested(project, name string) string {
	if p.Store == nil {
		return "(keystore indisponible)"
	}
	rows, err := p.Store.Projects()
	if err != nil {
		return "(liste des projets illisible)"
	}
	for _, r := range rows {
		if r.ID == project {
			continue
		}
		if p.sealed(r.ID, name) {
			return r.ID
		}
	}
	return ""
}

// contestedBy complète contested par les déclarations du parc : un autre projet
// qui déclare le vhost ou le compte le conteste, marqué ou non.
func (p *Provider) contestedBy(project, secret string, declares func(*model.AMQP) bool) string {
	if c := p.contested(project, secret); c != "" {
		return c
	}
	if p.Fleet == nil {
		return ""
	}
	all, err := p.Fleet()
	if err != nil {
		return "(parc illisible)"
	}
	for _, other := range all {
		if other.ID() != project && other.Spec.AMQP != nil && declares(other.Spec.AMQP) {
			return other.ID()
		}
	}
	return ""
}

// readDeclared relit la trace de ce qu'ogs a déclaré dans un vhost. Une trace
// absente ou illisible vaut une trace vide : rien n'est alors retiré.
func (p *Provider) readDeclared(project, vhost string) Declared {
	if p.Store == nil || p.Keys == nil {
		return Declared{}
	}
	name := DeclaredMarker(vhost)
	nonce, ciphertext, err := p.Store.GetSecret(project, name)
	if err != nil {
		return Declared{}
	}
	plain, err := p.Keys.Open(project, name, nonce, ciphertext)
	if err != nil {
		return Declared{}
	}
	var d Declared
	if err := json.Unmarshal(plain, &d); err != nil {
		return Declared{}
	}
	d.normalize()
	return d
}

func (p *Provider) sealDeclared(project, vhost string, d Declared) error {
	if p.Store == nil || p.Keys == nil {
		return fmt.Errorf("keystore indisponible : impossible de garder la trace du vhost %s", vhost)
	}
	d.normalize()
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	name := DeclaredMarker(vhost)
	nonce, ciphertext, err := p.Keys.Seal(project, name, data)
	if err != nil {
		return err
	}
	return p.Store.PutSecret(project, name, nonce, ciphertext)
}

// retirement suit, le temps d'un apply, les retraits qui ont abouti : la trace
// écrite en fin de plan garde tout ce qui n'a pas pu partir.
type retirement struct {
	pending Declared
	done    map[string]bool
}

func (r *retirement) mark(key string) { r.done[key] = true }

// remaining rend la trace à sceller : la déclaration, plus les retraits qui
// n'ont pas abouti.
func (r *retirement) remaining(current Declared) Declared {
	out := current
	for _, e := range r.pending.Exchanges {
		if !r.done["e:"+e] {
			out.Exchanges = append(out.Exchanges, e)
		}
	}
	for _, q := range r.pending.Queues {
		if !r.done["q:"+q] {
			out.Queues = append(out.Queues, q)
		}
	}
	for _, b := range r.pending.Bindings {
		if !r.done["b:"+b.key()] {
			out.Bindings = append(out.Bindings, b)
		}
	}
	for _, pol := range r.pending.Policies {
		if !r.done["p:"+pol] {
			out.Policies = append(out.Policies, pol)
		}
	}
	out.normalize()
	return out
}

// retiredSteps retire ce qu'ogs a déclaré dans le vhost et que la déclaration
// ne porte plus, puis scelle la nouvelle trace. Les retraits ne sont jamais
// critiques : un objet resté en trop ne doit pas faire compenser un déploiement
// par ailleurs fonctionnel.
//
// Une file n'est retirée que vide et sans consommateur, ce que le broker éprouve
// lui-même au moment du retrait (if-empty, if-unused) : ses messages sont des
// données, qu'aucune archive ne garde. Sinon elle reste, l'étape le dit (code 6),
// et la trace la garde pour le passage suivant. Ce qui est déjà absent est
// oublié ; ce que l'application a créé elle-même n'est jamais dans la trace.
func (p *Provider) retiredSteps(proj *model.Project, spec *model.AMQP, obs Observed) []*engine.Step {
	current := declaredOf(spec)
	prev := obs.Declared
	r := &retirement{done: map[string]bool{}}
	in := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	wantedBindings := map[string]bool{}
	for _, b := range current.Bindings {
		wantedBindings[b.key()] = true
	}
	for _, b := range prev.Bindings {
		if !wantedBindings[b.key()] {
			r.pending.Bindings = append(r.pending.Bindings, b)
		}
	}
	for _, q := range prev.Queues {
		if !in(current.Queues, q) {
			r.pending.Queues = append(r.pending.Queues, q)
		}
	}
	for _, e := range prev.Exchanges {
		if !in(current.Exchanges, e) {
			r.pending.Exchanges = append(r.pending.Exchanges, e)
		}
	}
	for _, pol := range prev.Policies {
		if !in(current.Policies, pol) {
			r.pending.Policies = append(r.pending.Policies, pol)
		}
	}

	vhost := spec.Vhost
	var steps []*engine.Step
	nonCritical := func(s *engine.Step) *engine.Step { s.Critical = false; return s }

	// Sans le plugin management, exchanges, bindings et policies ne se lisent
	// pas : ils restent dans la trace, jamais tenus pour absents.
	if !obs.Degraded {
		for _, b := range r.pending.Bindings {
			b := b
			var found *Binding
			for i := range obs.Bindings {
				o := obs.Bindings[i]
				if o.Source == b.Exchange && o.Destination == b.Queue && o.RoutingKey == b.RoutingKey {
					found = &o
					break
				}
			}
			if found == nil {
				r.mark("b:" + b.key())
				continue
			}
			s := nonCritical(engine.NewStep(fmt.Sprintf("amqp/binding/%s/%s/%s", vhost, b.Exchange, b.Queue),
				engine.Delete, fmt.Sprintf("binding %s → %s (clé %s), retiré de la déclaration",
					b.Exchange, b.Queue, orDefault(b.RoutingKey, "«vide»"))))
			target := *found
			s.Apply = func(ctx context.Context) error {
				if err := p.Client.DeleteBinding(ctx, target); err != nil {
					return err
				}
				r.mark("b:" + b.key())
				return nil
			}
			steps = append(steps, s)
		}
	}
	for _, name := range r.pending.Queues {
		name := name
		q, ok := obs.Queues[name]
		if !ok {
			r.mark("q:" + name)
			continue
		}
		resource := "amqp/queue/" + vhost + "/" + name
		if q.Messages > 0 || q.Consumers > 0 {
			s := nonCritical(engine.NewStep(resource, engine.Delete, fmt.Sprintf(
				"queue %s, retirée de la déclaration — conservée (%d message(s), %d consommateur(s))",
				name, q.Messages, q.Consumers)))
			messages, consumers := q.Messages, q.Consumers
			s.Apply = func(context.Context) error {
				return fmt.Errorf("la file %s n'est plus déclarée mais porte %d message(s) et %d consommateur(s) : "+
					"ogs ne la retire que vide et sans consommateur — arrêter ses consommateurs et la vider "+
					"(ogs amqp queue purge %s %s), ou la déclarer de nouveau", name, messages, consumers,
					proj.ID(), name)
			}
			steps = append(steps, s)
			continue
		}
		s := nonCritical(engine.NewStep(resource, engine.Delete,
			fmt.Sprintf("queue %s, retirée de la déclaration (vide)", name)))
		s.Apply = func(ctx context.Context) error {
			if err := p.Client.DeleteIdleQueue(ctx, vhost, name); err != nil {
				return fmt.Errorf("retrait de la file %s : %w — elle n'est retirée que vide et sans "+
					"consommateur", name, err)
			}
			r.mark("q:" + name)
			return nil
		}
		steps = append(steps, s)
	}
	if !obs.Degraded {
		for _, name := range r.pending.Exchanges {
			name := name
			if _, ok := obs.Exchanges[name]; !ok {
				r.mark("e:" + name)
				continue
			}
			s := nonCritical(engine.NewStep("amqp/exchange/"+vhost+"/"+name, engine.Delete,
				fmt.Sprintf("exchange %s, retiré de la déclaration", name)))
			s.Apply = func(ctx context.Context) error {
				if err := p.Client.DeleteExchange(ctx, vhost, name); err != nil {
					return fmt.Errorf("retrait de l'exchange %s : %w — il n'est retiré que sans binding", name, err)
				}
				r.mark("e:" + name)
				return nil
			}
			steps = append(steps, s)
		}
		for _, name := range r.pending.Policies {
			name := name
			if _, ok := obs.Policies[name]; !ok {
				r.mark("p:" + name)
				continue
			}
			s := nonCritical(engine.NewStep("amqp/policy/"+vhost+"/"+name, engine.Delete,
				fmt.Sprintf("policy %s, retirée de la déclaration", name)))
			s.Apply = func(ctx context.Context) error {
				if err := p.Client.DeletePolicy(ctx, vhost, name); err != nil {
					return err
				}
				r.mark("p:" + name)
				return nil
			}
			steps = append(steps, s)
		}
	}

	// La trace, en dernier : elle retient la déclaration et ce qui n'a pas pu
	// partir. Elle n'est écrite qu'à l'application — « plan » n'écrit rien.
	resource := "amqp/declared/" + vhost
	if len(steps) == 0 && obs.DeclaredSealed && prev.equal(r.remaining(current)) {
		return nil
	}
	s := nonCritical(engine.NewStep(resource, engine.Update,
		fmt.Sprintf("vhost amqp %s — trace de ce qu'ogs y déclare", vhost)))
	// La trace ne tient que l'état d'ogs : la poser n'est pas un écart du broker
	// (un parc antérieur à D100 la reçoit au premier apply de chaque projet).
	s.Bookkeeping = true
	project := proj.ID()
	s.Apply = func(context.Context) error { return p.sealDeclared(project, vhost, r.remaining(current)) }
	// Un déploiement défait rend la trace d'avant : celle qu'il a écrite
	// retiendrait des objets que la compensation vient de retirer.
	hadTrace := obs.DeclaredSealed
	s.Compensate = func(context.Context) error {
		if hadTrace {
			return p.sealDeclared(project, vhost, prev)
		}
		return p.Store.DeleteSecret(project, DeclaredMarker(vhost))
	}
	return append(steps, s)
}

// obsoleteSteps retire les vhosts et comptes qu'ogs a créés pour le projet et
// que la déclaration ne désigne plus — spec.amqp changé, ou retiré. Le secret qui
// les prouve part après eux, jamais avant : un retrait en échec le laisse, et la
// ressource reste celle d'ogs.
//
// Un vhost dont une file porte des messages ou des consommateurs n'est pas
// retiré par un apply : ses messages sont des données, et une application pas
// encore reconfigurée s'y sert encore. « project delete » l'emporte, lui : c'est
// le consentement explicite, comme pour le vhost déclaré.
//
// vhostReady et userReady disent si le vhost et le compte déclarés existaient à
// l'observation : sinon, l'apply qui les crée garde les anciens, retirés à
// l'apply suivant (D100).
func (p *Provider) obsoleteSteps(proj *model.Project, obs Observed, vhostReady, userReady bool) []*engine.Step {
	project := proj.ID()
	var steps []*engine.Step
	for _, v := range obs.ObsoleteVhosts {
		v := v
		resource := "amqp/vhost/" + v.Name
		switch {
		case v.Exists && !vhostReady:
			steps = append(steps, engine.NewStep(resource, engine.NoOp, fmt.Sprintf(
				"vhost amqp %s, que la déclaration ne désigne plus — conservé jusqu'au prochain apply, après la "+
					"création du nouveau", v.Name)))
		case v.Contested != "":
			s := engine.NewStep(resource, engine.Delete, fmt.Sprintf(
				"vhost amqp %s, que la déclaration ne désigne plus — conservé : le projet %s le déclare ou en porte aussi la marque",
				v.Name, v.Contested))
			s.Critical = false
			reason := fmt.Errorf("le vhost %s est à %s, mais le projet %s le déclare ou en porte la marque : ogs ne le retire pas", v.Name,
				project, v.Contested)
			s.Apply = func(context.Context) error { return reason }
			steps = append(steps, s)
		case !v.Exists:
			s := engine.NewStep(resource, engine.Delete,
				fmt.Sprintf("vhost amqp %s, absent du broker — oubli de sa marque", v.Name))
			s.Critical = false
			s.Apply = func(context.Context) error { return p.forgetVhost(project, v.Name) }
			steps = append(steps, s)
		default:
			busy := v.QueueErr
			if busy == nil {
				var held []string
				for _, q := range v.Queues {
					if q.Messages > 0 || q.Consumers > 0 {
						held = append(held, fmt.Sprintf("%s (%d message(s), %d consommateur(s))",
							q.Name, q.Messages, q.Consumers))
					}
				}
				if len(held) > 0 {
					busy = fmt.Errorf("ses files ne sont pas vides ou sont consommées : %s — les vider et "+
						"arrêter leurs consommateurs, puis relancer l'apply ; « ogs project delete » le retire "+
						"avec ses messages", strings.Join(held, ", "))
				}
			}
			if busy != nil {
				s := engine.NewStep(resource, engine.Delete, fmt.Sprintf(
					"vhost amqp %s, que la déclaration ne désigne plus — conservé", v.Name))
				s.Critical = false
				reason := fmt.Errorf("vhost amqp %s conservé : %w", v.Name, busy)
				s.Apply = func(context.Context) error { return reason }
				steps = append(steps, s)
				continue
			}
			s := engine.NewStep(resource, engine.Delete, fmt.Sprintf(
				"vhost amqp %s, que la déclaration ne désigne plus (files vides)", v.Name))
			s.Critical = false
			s.Apply = func(ctx context.Context) error {
				if err := p.Client.DeleteVhost(ctx, v.Name); err != nil {
					return err
				}
				return p.forgetVhost(project, v.Name)
			}
			steps = append(steps, s)
		}
	}
	for _, u := range obs.ObsoleteUsers {
		u := u
		resource := "amqp/user/" + u.Name
		switch {
		case u.Exists && !userReady:
			steps = append(steps, engine.NewStep(resource, engine.NoOp, fmt.Sprintf(
				"compte amqp %s, que la déclaration ne désigne plus — conservé jusqu'au prochain apply, après la "+
					"création du nouveau", u.Name)))
		case u.Contested != "":
			s := engine.NewStep(resource, engine.Delete, fmt.Sprintf(
				"compte amqp %s, que la déclaration ne désigne plus — conservé : le projet %s le déclare ou en "+
					"tient aussi le mot de passe", u.Name, u.Contested))
			s.Critical = false
			reason := fmt.Errorf("le compte %s est à %s, mais le projet %s le déclare ou en tient le mot de passe : ogs ne le retire pas", u.Name,
				project, u.Contested)
			s.Apply = func(context.Context) error { return reason }
			steps = append(steps, s)
		case !u.Exists:
			s := engine.NewStep(resource, engine.Delete,
				fmt.Sprintf("compte amqp %s, absent du broker — oubli de son mot de passe", u.Name))
			s.Critical = false
			s.Apply = func(context.Context) error { return p.Store.DeleteSecret(project, userSecretPrefix+u.Name) }
			steps = append(steps, s)
		default:
			s := engine.NewStep(resource, engine.Delete,
				fmt.Sprintf("compte amqp %s, que la déclaration ne désigne plus", u.Name))
			s.Critical = false
			s.Apply = func(ctx context.Context) error {
				if err := p.Client.DeleteUser(ctx, u.Name); err != nil {
					return err
				}
				return p.Store.DeleteSecret(project, userSecretPrefix+u.Name)
			}
			steps = append(steps, s)
		}
	}
	return steps
}

// forgetVhost retire la marque d'un vhost et la trace de ce qu'ogs y
// déclarait.
func (p *Provider) forgetVhost(project, vhost string) error {
	if err := p.Store.DeleteSecret(project, VhostMarker(vhost)); err != nil {
		return err
	}
	return p.Store.DeleteSecret(project, DeclaredMarker(vhost))
}

// DropReport dit ce que la suppression d'un projet a retiré du broker, et ce
// qu'elle y a laissé.
type DropReport struct {
	Removed  []string
	Warnings []string
}

// DropProject retire du broker, à la suppression du projet, tout ce qu'ogs a
// créé pour lui : le vhost et le compte déclarés, et ceux qu'une déclaration
// changée a laissés derrière elle — chacun jugé sur sa propre preuve (D93) —, les
// files du vhost partant avec lui. Le secret qui prouve une ressource part après
// elle, jamais avant : à son retour, tout secret « amqp/ » ou « amqp-vhost/ » du
// projet a disparu, ou prouve une ressource encore présente — un retrait en
// échec, un homonyme d'un autre projet, un broker injoignable.
func (p *Provider) DropProject(ctx context.Context, proj *model.Project) DropReport {
	var rep DropReport
	warn := func(format string, args ...any) { rep.Warnings = append(rep.Warnings, fmt.Sprintf(format, args...)) }
	project := proj.ID()
	spec := proj.Spec.AMQP
	vhosts, users, traces, err := p.leftovers(project, spec)
	if err != nil {
		warn("secrets amqp du projet illisibles : %v", err)
	}
	if spec == nil && len(vhosts)+len(users)+len(traces) == 0 {
		return rep
	}
	// Sans accès au broker et sans aucune preuve, ogs n'a rien créé pour le
	// projet : il n'y a rien à retirer, ni à signaler.
	proved := len(vhosts)+len(users) > 0 ||
		spec != nil && (p.sealed(project, VhostMarker(spec.Vhost)) || p.sealed(project, userSecretPrefix+spec.User))
	if p.Client == nil && !proved {
		for _, t := range traces {
			if err := p.Store.DeleteSecret(project, DeclaredMarker(t)); err != nil {
				warn("secret %s : %v", DeclaredMarker(t), err)
			}
		}
		return rep
	}
	obs, err := p.Observe(ctx, proj)
	if err == nil && obs.Unavailable != nil {
		err = obs.Unavailable
	}
	if err != nil {
		if spec != nil {
			warn("vhost amqp %s et compte %s conservés : %v", spec.Vhost, spec.User, err)
		}
		for _, v := range vhosts {
			warn("vhost amqp %s conservé : %v", v, err)
		}
		for _, u := range users {
			warn("compte amqp %s conservé : %v", u, err)
		}
		return rep
	}
	drop := func(name string) {
		if err := p.Store.DeleteSecret(project, name); err != nil {
			warn("secret %s : %v", name, err)
		}
	}
	kept := map[string]bool{}
	if spec != nil {
		switch {
		case !obs.VhostOwned():
			kept[spec.Vhost] = true
			warn("vhost amqp %s conservé : ogs ne l'a pas créé (aucune marque scellée)", spec.Vhost)
		case obs.VhostExists:
			if err := p.Client.DeleteVhost(ctx, spec.Vhost); err != nil {
				kept[spec.Vhost] = true
				warn("suppression du vhost amqp : %v", err)
				break
			}
			rep.Removed = append(rep.Removed, "vhost amqp "+spec.Vhost)
			fallthrough
		default:
			// La marque part avec le vhost : restée au keystore, elle ferait tenir
			// pour sien l'homonyme qu'un autre créerait ensuite, et qu'un projet
			// recréé sous le même identifiant reprendrait puis supprimerait.
			drop(VhostMarker(spec.Vhost))
			drop(DeclaredMarker(spec.Vhost))
		}
	}
	for _, v := range obs.ObsoleteVhosts {
		switch {
		case v.Contested != "":
			kept[v.Name] = true
			warn("vhost amqp %s conservé : le projet %s le déclare ou en porte aussi la marque", v.Name, v.Contested)
		case v.Exists:
			if err := p.Client.DeleteVhost(ctx, v.Name); err != nil {
				kept[v.Name] = true
				warn("suppression du vhost amqp %s : %v", v.Name, err)
				continue
			}
			rep.Removed = append(rep.Removed, "vhost amqp "+v.Name)
			fallthrough
		default:
			drop(VhostMarker(v.Name))
			drop(DeclaredMarker(v.Name))
		}
	}
	// Une trace sans vhost qu'ogs garde n'a plus rien à retracer.
	for _, t := range traces {
		if !kept[t] && (spec == nil || t != spec.Vhost) && !contains(vhosts, t) {
			drop(DeclaredMarker(t))
		}
	}
	if spec != nil {
		switch {
		case !obs.UserOwned():
			warn("compte amqp %s conservé : ogs ne l'a pas créé (aucun mot de passe scellé)", spec.User)
		case obs.UserExists:
			if err := p.Client.DeleteUser(ctx, spec.User); err != nil {
				warn("suppression du compte amqp : %v", err)
				break
			}
			rep.Removed = append(rep.Removed, "compte amqp "+spec.User)
			fallthrough
		default:
			drop(userSecretPrefix + spec.User)
		}
	}
	for _, u := range obs.ObsoleteUsers {
		switch {
		case u.Contested != "":
			warn("compte amqp %s conservé : le projet %s en tient aussi le mot de passe", u.Name, u.Contested)
		case u.Exists:
			if err := p.Client.DeleteUser(ctx, u.Name); err != nil {
				warn("suppression du compte amqp %s : %v", u.Name, err)
				continue
			}
			rep.Removed = append(rep.Removed, "compte amqp "+u.Name)
			fallthrough
		default:
			drop(userSecretPrefix + u.Name)
		}
	}
	return rep
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
