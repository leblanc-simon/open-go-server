package tls

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/store"
	"leblanc.io/open-go-server/internal/template"
)

// Provider réconcilie les certificats, de projet comme de portée serveur.
type Provider struct {
	Paths  config.Paths
	Cfg    *config.Config
	Store  Store
	DB     *store.Store
	Issuer Issuer
	// Fallback émet quand l'émetteur principal est indisponible ou que le
	// challenge demandé est « selfsigned ».
	Fallback Issuer
	Now      func() time.Time
	// Force autorise l'écrasement d'un fichier qu'ogs n'a pas écrit, après
	// sauvegarde horodatée (§6.4). Sans lui, le refus n'aurait aucune porte de
	// sortie et le plan global resterait bloqué.
	Force bool
	// WebReload est l'intention de rechargement du serveur web retenu
	// (« web:apache », « web:nginx »), portée par toute étape qui émet. Un
	// certificat réémis remplace un matériel que le serveur garde en mémoire :
	// sans rechargement, il continue de présenter l'ancien — celui qui ne couvre
	// pas l'alias ajouté, ou qui expire —, et le vhost, dont le rendu n'a pas
	// changé, ne déclenche rien de lui-même.
	WebReload string
	// DNSSets fournit les jeux d'identifiants DNS dont les zones font passer une
	// demande au challenge DNS-01 d'elle-même (D97) ; à défaut, ceux de
	// /etc/ogs/credentials/dns, lus une fois.
	DNSSets   func() []DNSCredentials
	dnsSets   []DNSCredentials
	dnsLoaded bool
}

// dnsCredentialSets rend les jeux d'identifiants DNS. Un fichier illisible est
// écarté : la demande qu'il aurait servie reste en HTTP-01, comme avant qu'il
// n'existe, et « doctor » le nomme.
func (p *Provider) dnsCredentialSets() []DNSCredentials {
	if p.DNSSets != nil {
		return p.DNSSets()
	}
	if !p.dnsLoaded {
		p.dnsSets, _ = LoadDNSCredentials(p.Paths.DNSCredsDir())
		p.dnsLoaded = true
	}
	return p.dnsSets
}

// chooseChallenge fixe le challenge d'une demande qui n'en déclare pas : DNS-01
// si des identifiants sont nommés, ou si chacun de ses noms relève d'une zone
// déclarée (D97) — le certificat s'obtient alors avant que le DNS ne désigne la
// machine, et un joker sans rien déclarer de plus ; HTTP-01 sinon.
func (p *Provider) chooseChallenge(req *Request, declared string) {
	if declared != "" {
		return
	}
	if req.DNSCredentials != "" {
		req.Challenge = ChallengeDNS01
		return
	}
	if provider, ok := AutoDNS(p.dnsCredentialSets(), req.Domains); ok {
		req.Challenge, req.DNSProvider, req.AutoDNS = ChallengeDNS01, provider, true
	}
}

// refusStep produit l'étape qui refuse d'écraser un fichier qu'ogs n'a pas
// écrit. L'unité et le timer de renouvellement portent le même en-tête que le
// reste : une périodicité changée à la main disparaîtrait sans un mot, et avec
// elle la seule chose qui renouvelle les certificats du parc. Le message
// distingue les deux causes — un fichier « non géré » n'a jamais porté la marque
// d'ogs, un fichier « modifié hors ogs » a été retouché après avoir été généré.
func refusStep(resource, summary, path, backups string, state fs.State) *engine.Step {
	libelle, motif := "modifié hors ogs", "a été modifié en dehors d'ogs"
	if state == fs.StateForeign {
		libelle, motif = "non géré par ogs", "existe déjà sans porter la marque d'ogs"
	}
	s := engine.NewStep(resource, engine.Update, summary+" — fichier "+libelle)
	s.Apply = func(context.Context) error {
		return fmt.Errorf("%s %s : relancer avec --force pour l'écraser "+
			"(l'original sera sauvegardé dans %s)", path, motif, backups)
	}
	return s
}

// Name identifie le provider.
func (p *Provider) Name() string { return "tls" }

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Requests énumère les certificats attendus par un projet. Un projet en
// préproduction ne demande rien : il s'appuie sur le wildcard partagé, ce qui
// évite une émission par site (D26).
func (p *Provider) Requests(proj *model.Project) ([]Request, error) {
	if ref := p.SharedRef(proj); ref != "" {
		return nil, nil
	}
	var out []Request
	declared := map[string]bool{}
	for _, c := range proj.Spec.Certificates {
		ch, err := ParseChallenge(c.Challenge)
		if err != nil {
			return nil, err
		}
		req := Request{
			Name: c.Name, Domains: c.Domains, Challenge: ch,
			DNSProvider: c.DNSProvider, DNSCredentials: c.DNSCredentials, KeyType: c.KeyType, Staging: c.Staging,
		}
		if req.Name == "" {
			req.Name = proj.ID()
		}
		if err := CheckProjectName(req.Name); err != nil {
			return nil, fmt.Errorf("projet %s : %w", proj.ID(), err)
		}
		if len(req.Domains) == 0 {
			req.Domains = CertifiedDomains(proj)
		}
		// Un joker déclaré est essentiel dès qu'il couvre un nom servi : le
		// comparer nom à nom l'écartait du repli, et www.shop.fr, qu'il couvrait,
		// perdait sa couverture au premier repli réussi.
		req.Essential = covering(req.Domains, servedDomains(proj))
		p.chooseChallenge(&req, c.Challenge)
		if err := req.Validate(); err != nil {
			return nil, err
		}
		declared[req.Name] = true
		out = append(out, req)
	}
	if len(out) > 0 {
		return out, nil
	}
	// Aucun certificat déclaré : on en déduit un depuis les domaines servis, dès
	// lors qu'un vhost demande TLS.
	if !wantsTLS(proj) {
		return nil, nil
	}
	domains := CertifiedDomains(proj)
	if len(domains) == 0 {
		return nil, nil
	}
	req := Request{Name: proj.ID(), Domains: domains, Challenge: ChallengeHTTP01,
		Essential: servedDomains(proj)}
	p.chooseChallenge(&req, "")
	if err := req.Validate(); err != nil {
		return nil, err
	}
	return []Request{req}, nil
}

// SharedRef renvoie la référence du certificat partagé applicable au projet.
func (p *Provider) SharedRef(proj *model.Project) string {
	if p.Cfg == nil || !proj.IsPrep() {
		return ""
	}
	// Un host de préproduction hors convention ne peut pas être couvert par le
	// wildcard : il lui faut son propre certificat.
	ref := p.Cfg.Environments["prep"].Certificate
	if ref == "" {
		return ""
	}
	shared, ok := p.SharedRequest(ref)
	if !ok {
		return ""
	}
	// Le critère porte sur les domaines servis, non sur les redirections : une
	// redirection que le wildcard ne couvre pas ne vaut pas une émission propre à
	// la préproduction, que D26 écarte. Son vhost reste alors en HTTP seul, le
	// rendu ne présentant jamais un certificat qui ne couvre pas ses noms.
	info := Info{Domains: shared.Domains}
	if !info.Covers(servedDomains(proj)) {
		return ""
	}
	return ref
}

// SharedRequest construit la demande d'un certificat de portée serveur.
func (p *Provider) SharedRequest(ref string) (Request, bool) {
	if p.Cfg == nil {
		return Request{}, false
	}
	name := strings.TrimPrefix(ref, SharedPrefix)
	decl, ok := p.Cfg.Certificates[name]
	if !ok {
		return Request{}, false
	}
	ch, err := ParseChallenge(decl.Challenge)
	if err != nil {
		return Request{}, false
	}
	req := Request{
		Name: SharedPrefix + name, Domains: decl.Domains, Challenge: ch,
		DNSProvider: decl.DNSProvider, DNSCredentials: decl.DNSCredentials, KeyType: decl.KeyType,
	}
	p.chooseChallenge(&req, decl.Challenge)
	return req, true
}

// SharedRequests énumère tous les certificats de portée serveur configurés.
func (p *Provider) SharedRequests() []Request {
	if p.Cfg == nil {
		return nil
	}
	names := make([]string, 0, len(p.Cfg.Certificates))
	for name := range p.Cfg.Certificates {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Request, 0, len(names))
	for _, name := range names {
		if req, ok := p.SharedRequest(SharedPrefix + name); ok {
			out = append(out, req)
		}
	}
	return out
}

// CertificateFor renvoie la référence du certificat qu'un vhost doit employer,
// et si son matériel est déjà présent sur le disque.
func (p *Provider) CertificateFor(proj *model.Project) (ref string, available bool) {
	if shared := p.SharedRef(proj); shared != "" {
		// La même règle que pour le certificat d'un projet : un wildcard expiré
		// n'est plus présenté (Coverage), et l'annoncer disponible ferait dire à
		// « status » qu'une préproduction repassée en HTTP seul est saine.
		info, ok, _ := p.Store.Read(shared)
		return shared, ok && !info.Expired(p.now())
	}
	reqs, err := p.Requests(proj)
	if err != nil || len(reqs) == 0 {
		return "", false
	}
	ref = reqs[0].Name
	info, ok, err := p.Store.Read(ref)
	if err != nil || !ok {
		return ref, false
	}
	// « Disponible » veut dire ici complet : présent, non expiré, couvrant toute
	// la demande. C'est la question des rapports d'état. Le rendu des vhosts, lui,
	// ne s'y fie pas — il suit ce que couvre le matériel présent (Coverage).
	return ref, info.Covers(reqs[0].Domains) && !info.Expired(p.now())
}

// Coverage décrit, pour le rendu des vhosts d'un projet, le certificat qu'ils
// emploient : sa référence, les noms que couvre le matériel présent sur le
// disque, et ceux que couvrira le certificat demandé une fois émis.
//
// Les deux ne se confondent pas, et c'est tout l'enjeu. Un alias ajouté, un
// domaine principal qui change font diverger la demande du matériel présent,
// qui reste pourtant valable pour les noms qu'il couvrait. Juger le vhost sur la
// demande le faisait redescendre en HTTP seul le temps de la réémission — donc
// rechargé ainsi avant le challenge —, et pour de bon si elle échouait : HSTS
// rendait alors le site injoignable à qui l'avait déjà visité, et les autres
// recevaient sur le port 443 le vhost par défaut, celui d'un autre client.
type Coverage struct {
	// Ref est la référence du certificat, vide si le projet n'en demande aucun.
	Ref string
	// Present porte les noms du certificat présent, vide s'il est absent ou
	// expiré : un certificat expiré n'est plus présenté.
	Present []string
	// Requested porte les domaines demandés : ceux que couvrira le certificat
	// une fois émis.
	Requested []string
}

// Serves dit si le certificat présent couvre un nom.
func (c Coverage) Serves(name string) bool { return coveredBy(c.Present, name) }

// WillServe dit si le certificat demandé couvrira un nom, une fois émis.
func (c Coverage) WillServe(name string) bool { return coveredBy(c.Requested, name) }

// Complete dit si le certificat présent couvre toute la demande : aucune
// émission n'a alors de quoi changer le rendu d'un vhost.
func (c Coverage) Complete() bool {
	return len(c.Present) > 0 && Info{Domains: c.Present}.Covers(c.Requested)
}

// Coverage renvoie ce que le rendu des vhosts d'un projet doit savoir de son
// certificat.
func (p *Provider) Coverage(proj *model.Project) Coverage {
	var cov Coverage
	if shared := p.SharedRef(proj); shared != "" {
		cov.Ref = shared
		if req, ok := p.SharedRequest(shared); ok {
			cov.Requested = req.Domains
		}
	} else {
		reqs, err := p.Requests(proj)
		if err != nil || len(reqs) == 0 {
			return Coverage{}
		}
		cov.Ref, cov.Requested = reqs[0].Name, reqs[0].Domains
	}
	if info, ok, err := p.Store.Read(cov.Ref); err == nil && ok && !info.Expired(p.now()) {
		cov.Present = info.Domains
	}
	return cov
}

// wantsTLS indique qu'au moins un vhost demande du TLS. En l'absence de
// déclaration explicite, le TLS est souhaité par défaut : un site public sans
// certificat n'est plus acceptable.
func wantsTLS(proj *model.Project) bool {
	if proj.Spec.Web.TLSDisabled() {
		return false
	}
	if len(proj.Spec.Web.VHosts) == 0 {
		return true
	}
	for _, vh := range proj.Spec.Web.VHosts {
		if vh.TLS == nil || vh.TLS.Enabled {
			return true
		}
	}
	return false
}

// servedDomains renvoie les domaines servis : les domaines principaux et les
// alias, ceux du vhost principal.
func servedDomains(proj *model.Project) []string {
	var out []string
	for _, d := range proj.Spec.Domains {
		switch d.Role {
		case "", "primary", "alias":
			out = append(out, d.Name)
		}
	}
	return out
}

// CertifiedDomains renvoie les domaines que le certificat d'un projet doit
// couvrir : les domaines servis, puis les redirections. Un domaine redirigé a
// autant besoin d'être chiffré (D43) — c'est une adresse connue, référencée, et
// souvent un sous-domaine du principal, que le HSTS « includeSubDomains » de
// celui-ci oblige le navigateur à joindre en HTTPS. L'en exclure servait son
// vhost 443 avec un certificat qui ne le couvrait pas.
//
// La crainte qui les avait écartées est fondée : une commande ACME échoue tout
// entière sur un seul nom refusé, et une redirection dont le DNS pointe ailleurs,
// dont l'enregistrement a expiré ou dont l'AAAA mène à une autre machine ferait
// échouer le renouvellement du principal — le site tomberait à l'échéance, des
// semaines après une mise à jour qui n'avait rien cassé —, et un alias ajouté à
// côté d'elle ne serait jamais couvert. Deux choses l'écartent : le rendu suit le
// matériel présent (Coverage), non la demande, et l'émission se replie par
// paliers quand la demande complète échoue, les redirections écartées les
// premières (fallbacks). La redirection n'est alors couverte que lorsqu'elle peut
// l'être ; le vhost principal, dès qu'il peut l'être.
//
// Une redirection qu'aucune demande ACME ne peut porter en est écartée : une
// adresse IP, que la validation des domaines admet — rediriger
// http://203.0.113.10 vers le site a du sens. La garder faisait refuser la
// demande entière à la validation, donc le projet entier : plus d'apply, et le
// renouvellement du parc, qui passe outre un projet en erreur, cessait sans un
// mot de renouveler le site. Son vhost reste en HTTP seul, aucun certificat ne
// couvrant son nom.
func CertifiedDomains(proj *model.Project) []string {
	out := servedDomains(proj)
	for _, d := range proj.Spec.Domains {
		if d.Role == "redirect" && validateDomain(d.Name) == nil {
			out = append(out, d.Name)
		}
	}
	return out
}

// Plan calcule les étapes d'émission et de renouvellement.
func (p *Provider) Plan(_ context.Context, proj *model.Project) ([]*engine.Step, error) {
	reqs, err := p.Requests(proj)
	if err != nil {
		return nil, err
	}
	if shared := p.SharedRef(proj); shared != "" {
		if req, ok := p.SharedRequest(shared); ok {
			reqs = append(reqs, req)
		}
	}
	var steps []*engine.Step
	for _, req := range reqs {
		step, err := p.certificateStep(req)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	return steps, nil
}

// certificateStep produit l'étape d'un certificat. Elle est toujours non
// critique : un certificat qui échoue laisse le site en ligne en HTTP, avec un
// avertissement et une commande de reprise (D16).
func (p *Provider) certificateStep(req Request) (*engine.Step, error) {
	resource := "tls/certificate/" + req.Name
	info, present, err := p.Store.Read(req.Name)
	if err != nil {
		return nil, err
	}
	now := p.now()
	// Le rechargement n'est acquis qu'à l'émission réussie ou repliée : le moteur
	// ne retient les intentions que des étapes appliquées, ou abouties en partie
	// (stepError), et un échec laisse le serveur sur le certificat qu'il présente
	// déjà.
	var reload []string
	if p.WebReload != "" {
		reload = []string{p.WebReload}
	}
	switch {
	case present && info.Covers(req.Domains) && !info.NeedsRenewal(now):
		return engine.NewStep(resource, engine.NoOp,
			fmt.Sprintf("certificat %s (expire dans %d jours)", req.Name, info.DaysLeft(now))), nil
	case present && !info.Covers(req.Domains):
		s := engine.NewStep(resource, engine.Update,
			fmt.Sprintf("certificat %s : domaines à couvrir modifiés (%s)",
				req.Name, strings.Join(req.Domains, ", ")))
		s.Critical = false
		// Loin de l'échéance, le certificat en place reste servi : le repli n'a lieu
		// que s'il couvre un nom de plus sans en retirer aucun. Près d'elle, il garde
		// le site quoi qu'il en coûte aux autres noms.
		urgent := info.NeedsRenewal(now)
		s.Apply = func(ctx context.Context) error { return stepError(p.issueOrFallback(ctx, req, urgent)) }
		// Le vidage recharge un vhost qui garde son bloc 443 sur le certificat en
		// place : il n'expose que les nouveaux noms au challenge du port 80.
		s.FlushReloads = req.servesChallenge()
		s.Reload = reload
		return s, nil
	case present:
		s := engine.NewStep(resource, engine.Update,
			fmt.Sprintf("renouvellement du certificat %s (expire dans %d jours)",
				req.Name, info.DaysLeft(now)))
		s.Critical = false
		s.Apply = func(ctx context.Context) error { return stepError(p.issueOrFallback(ctx, req, true)) }
		s.FlushReloads = req.servesChallenge()
		s.Reload = reload
		return s, nil
	}
	s := engine.NewStep(resource, engine.Create,
		fmt.Sprintf("certificat %s pour %s (%s)", req.Name, strings.Join(req.Domains, ", "), req.Method()))
	s.Critical = false
	s.Apply = func(ctx context.Context) error { return stepError(p.issueOrFallback(ctx, req, true)) }
	s.Reload = reload
	// Le challenge HTTP-01 se fait servir par le vhost : il doit être chargé, non
	// seulement écrit. Sans ce vidage, la première émission d'un site neuf échoue
	// en 404, le serveur n'ayant pas encore relu sa configuration.
	s.FlushReloads = req.servesChallenge()
	// La compensation ne retire que ce que cette exécution a créé.
	s.Compensate = func(context.Context) error { return p.Store.Remove(req.Name) }
	return s, nil
}

// stepError traduit l'issue d'une émission pour le moteur : un repli réussi est
// un aboutissement partiel, qui garde le rechargement de l'étape — le certificat
// a changé sur le disque, et le serveur présenterait sinon l'ancien.
func stepError(err error) error {
	if Written(err) {
		return engine.Partial(err)
	}
	return err
}

// Written dit si une émission en échec a tout de même écrit un certificat : un
// repli par paliers (PartialError), ou par HTTP-01 (DNSFallbackError).
func Written(err error) bool {
	partial, fallback := (*PartialError)(nil), (*DNSFallbackError)(nil)
	return errors.As(err, &partial) || errors.As(err, &fallback)
}

// DNSFallbackError signale un certificat émis par HTTP-01 alors que sa zone
// déclarée le destinait à DNS-01 (D97) : le certificat est complet, mais les
// identifiants de la zone ont échoué, et l'émission suivante qui ne pourra pas
// passer par le vhost — avant une bascule DNS — échouera aussi.
type DNSFallbackError struct {
	Name  string
	Cause error
}

func (e *DNSFallbackError) Error() string {
	return fmt.Sprintf("certificat %s émis par http-01, le challenge dns-01 de sa zone ayant échoué : %v "+
		"(vérifier les identifiants avec « ogs cert dns-provider list »)", e.Name, e.Cause)
}

func (e *DNSFallbackError) Unwrap() error { return e.Cause }

// PartialError signale une émission repliée : la demande complète a échoué, et le
// certificat n'a été émis que pour les noms essentiels (Request.Essential).
type PartialError struct {
	Name string
	// Covered porte les noms que couvre le certificat émis, Missing ceux de la
	// demande qu'il ne couvre pas.
	Covered []string
	Missing []string
	// Dropped porte ceux des noms écartés que le certificat remplacé couvrait :
	// une redirection validée autrefois, refusée aujourd'hui. Son vhost, écrit en
	// HTTPS, présente désormais un certificat qui ne la couvre pas — jusqu'au
	// prochain apply, qui le repasse en HTTP seul.
	Dropped []string
	// Cause est l'échec de la demande complète.
	Cause error
}

func (e *PartialError) Error() string {
	msg := fmt.Sprintf("certificat %s émis pour %s seulement — %s non couvert(s) : %v",
		e.Name, strings.Join(e.Covered, ", "), strings.Join(e.Missing, ", "), e.Cause)
	if len(e.Dropped) > 0 {
		msg += fmt.Sprintf(" (%s n'est plus couvert : « ogs apply » retire son bloc 443)",
			strings.Join(e.Dropped, ", "))
	}
	return msg
}

func (e *PartialError) Unwrap() error { return e.Cause }

// fallbacks rend les demandes de repli d'une émission échouée, par paliers, de la
// plus large à la plus étroite. Une commande ACME échoue tout entière sur un seul
// nom refusé, et son erreur ne nomme ce nom que dans un texte propre à lego,
// qu'ogs ne lit pas : chaque palier écarte une classe de noms dont l'échec est à
// craindre, en commençant par la moins précieuse.
//
//  1. sans les redirections en attente — une redirection que le certificat en
//     place ne couvre pas, dont le DNS ne pointe peut-être pas ici ;
//  2. sans aucune redirection — une redirection validée autrefois peut être
//     refusée aujourd'hui, son domaine ayant expiré ;
//  3. ce que couvre le certificat en place — un alias ou un nouveau principal
//     ajouté avant la bascule de son DNS fait échouer les deux paliers qui
//     précèdent, et ce palier garde les redirections déjà servies, dont un
//     www.shop.fr que le HSTS « includeSubDomains » du site oblige à joindre en
//     HTTPS ;
//  4. les noms du vhost principal que couvre le certificat en place — le site, à
//     défaut de tout le reste.
//
// Sans certificat en place, les paliers se réduisent aux noms du vhost
// principal : un site neuf dont la redirection ne pointe pas encore ici reçoit
// quand même le sien. Un palier vide, ou identique à la demande ou à un palier
// précédent, est omis. Une demande sans noms essentiels — un certificat partagé —
// est d'un seul tenant : aucun palier.
func fallbacks(req Request, inPlace *Info) []Request {
	if len(req.Essential) == 0 {
		return nil
	}
	var kept []string
	if inPlace != nil {
		kept = among(req.Domains, inPlace.Domains)
	}
	essential := req.Essential
	paliers := [][]string{
		keep(req.Domains, func(n string) bool { return slices.Contains(essential, n) || slices.Contains(kept, n) }),
		essential,
		kept,
		keep(essential, func(n string) bool { return slices.Contains(kept, n) }),
	}
	vus := map[string]bool{strings.Join(req.Domains, ","): true}
	var out []Request
	for _, names := range paliers {
		key := strings.Join(names, ",")
		if len(names) == 0 || vus[key] {
			continue
		}
		vus[key] = true
		fb := req
		// Un repli est d'un seul tenant : il ne se replie pas à son tour.
		fb.Domains, fb.Essential = names, nil
		out = append(out, fb)
	}
	return out
}

// among rend les noms de names que couvre covering, dans leur ordre.
func among(names, covering []string) []string {
	return keep(names, func(n string) bool { return coveredBy(covering, n) })
}

// covering rend les noms de names qui couvrent l'un de ceux de served, dans leur
// ordre : un joker compte dès qu'il couvre un nom servi.
func covering(names, served []string) []string {
	return keep(names, func(n string) bool {
		return slices.ContainsFunc(served, func(s string) bool { return coveredBy([]string{n}, s) })
	})
}

// keep rend les noms qui satisfont le critère, dans leur ordre.
func keep(names []string, ok func(string) bool) []string {
	var out []string
	for _, n := range names {
		if ok(n) {
			out = append(out, n)
		}
	}
	return out
}

// issueOrFallback émet la demande complète et, si elle échoue, se replie par
// paliers (fallbacks) : une commande ACME échoue tout entière sur un seul nom
// refusé, et sans repli, une redirection dont le DNS pointe ailleurs ferait
// expirer le certificat du site principal — ou empêcherait à jamais de couvrir
// l'alias qu'on vient d'ajouter. Le premier palier qui aboutit rend une
// *PartialError : le certificat est écrit, il ne couvre pas tout.
//
// Quand le certificat peut attendre — présent, loin de son échéance, sans
// renouvellement forcé —, un palier n'est tenté que s'il couvre un nom que le
// certificat en place ne couvre pas, sans retirer aucun de ceux qu'il sert.
// Réémettre à l'identique consommerait les quotas de l'autorité pour rien, et
// retirer un nom servi — l'ancien principal devenu redirection, qu'un HSTS garde
// en HTTPS chez tous ses visiteurs — le casserait des semaines avant l'heure.
// Près de l'échéance, le site passe avant tout le reste.
func (p *Provider) issueOrFallback(ctx context.Context, req Request, urgent bool) error {
	err := p.issue(ctx, req)
	if fallback := (*DNSFallbackError)(nil); err == nil || errors.As(err, &fallback) {
		return err
	}
	var inPlace *Info
	if info, ok, rerr := p.Store.Read(req.Name); rerr == nil && ok {
		inPlace = &info
	}
	var served []string
	if inPlace != nil && !inPlace.Expired(p.now()) {
		served = among(req.Domains, inPlace.Domains)
	}
	échecs := err
	var réservé *Request
	for _, fb := range fallbacks(req, inPlace) {
		if !urgent && inPlace != nil {
			if inPlace.Covers(fb.Domains) {
				continue
			}
			if !(Info{Domains: fb.Domains}).Covers(served) {
				if réservé == nil {
					réservé = &fb
				}
				continue
			}
		}
		// Un palier émis par le repli HTTP-01 d'un challenge DNS-01 venu de la zone
		// est un palier abouti.
		if ferr := p.issue(ctx, fb); ferr != nil && !Written(ferr) {
			échecs = fmt.Errorf("%w ; repli sur %s en échec : %v", échecs, strings.Join(fb.Domains, ", "), ferr)
			continue
		}
		partial := &PartialError{Name: req.Name, Covered: fb.Domains, Cause: err}
		for _, d := range req.Domains {
			if coveredBy(fb.Domains, d) {
				continue
			}
			partial.Missing = append(partial.Missing, d)
			if coveredBy(served, d) {
				partial.Dropped = append(partial.Dropped, d)
			}
		}
		return partial
	}
	if réservé != nil {
		// L'exploitant qui vient d'ajouter un nom doit apprendre pourquoi il n'est
		// pas couvert, et quand il le sera : sans cela, seul le refus de la demande
		// complète s'affichait, et l'attente semblait sans fin.
		retirés := keep(served, func(n string) bool { return !coveredBy(réservé.Domains, n) })
		échecs = fmt.Errorf("%w — repli sur %s réservé à l'échéance, à partir du %s : il retirerait %s, "+
			"que le certificat en place sert encore (un renouvellement forcé l'impose dès maintenant)",
			échecs, strings.Join(réservé.Domains, ", "),
			inPlace.NotAfter.Add(-RenewBefore).Format("02/01/2006"), strings.Join(retirés, ", "))
	}
	return échecs
}

// issue émet le certificat en tenant compte de la suspension éventuelle.
func (p *Provider) issue(ctx context.Context, req Request) error {
	key := strings.Join(req.Domains, ",")
	if p.DB != nil {
		if until, paused, err := p.DB.ACMEPause(key); err == nil && paused {
			return fmt.Errorf("tentatives suspendues jusqu'à %s après %d échecs consécutifs : "+
				"corriger la cause (résolution DNS, accessibilité du port 80) avant de réessayer",
				until.Format(time.RFC3339), store.ACMEFailureThreshold)
		}
	}
	issuer := p.Issuer
	if req.Challenge == ChallengeSelfSigned || issuer == nil {
		issuer = p.Fallback
	}
	if issuer == nil {
		return fmt.Errorf("aucun émetteur disponible pour %s", req.Name)
	}
	material, err := issuer.Issue(ctx, req)
	var dnsErr error
	if err != nil && req.AutoDNS {
		// Le challenge DNS-01 venait de la zone, non de la demande : HTTP-01, que la
		// demande aurait reçu sans elle, garde le site couvert si l'API du provider
		// est indisponible ou ses identifiants révoqués.
		http := req
		http.Challenge, http.AutoDNS, http.DNSProvider = ChallengeHTTP01, false, ""
		var herr error
		if material, herr = issuer.Issue(ctx, http); herr == nil {
			dnsErr, err = err, nil
		} else {
			err = fmt.Errorf("dns-01 : %v ; repli http-01 : %w", err, herr)
		}
	}
	if err != nil {
		if p.DB != nil && req.Challenge != ChallengeSelfSigned {
			count, until, rerr := p.DB.RecordACMEFailure(key, err.Error())
			if rerr == nil && !until.IsZero() {
				return fmt.Errorf("%w — %d échecs consécutifs, tentatives suspendues jusqu'à %s",
					err, count, until.Format(time.RFC3339))
			}
		}
		return err
	}
	if err := p.Store.Write(req.Name, material); err != nil {
		return err
	}
	if p.DB != nil {
		_ = p.DB.ClearACMEFailures(key)
	}
	if dnsErr != nil {
		return &DNSFallbackError{Name: req.Name, Cause: dnsErr}
	}
	return nil
}

// Issue émet ou renouvelle un certificat à la demande. Le repli suit la règle du
// plan : loin de l'échéance, seul un palier qui ajoute un nom sans en retirer
// aucun est tenté.
func (p *Provider) Issue(ctx context.Context, req Request) error {
	info, present, err := p.Store.Read(req.Name)
	if err != nil {
		return err
	}
	return p.issueOrFallback(ctx, req, !present || info.NeedsRenewal(p.now()))
}

// AwaitsCoverage dit si seule la couverture motive la réémission d'un certificat :
// présent, loin de son échéance, mais ne couvrant pas toute sa demande.
func (p *Provider) AwaitsCoverage(req Request) bool {
	info, ok, err := p.Store.Read(req.Name)
	return err == nil && ok && !info.NeedsRenewal(p.now()) && !info.Covers(req.Domains)
}

// Renew renouvelle les certificats arrivant à échéance, ou ne couvrant plus les
// domaines demandés, et renvoie ceux traités. Un renouvellement replié compte
// parmi les traités — le matériel a changé, le serveur doit le relire — et parmi
// les erreurs, les noms écartés restant à couvrir.
func (p *Provider) Renew(ctx context.Context, requests []Request, force bool) ([]string, []error) {
	var done []string
	var errs []error
	now := p.now()
	for _, req := range requests {
		info, present, err := p.Store.Read(req.Name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// Le renouvellement ne crée rien : un certificat absent relève de « cert
		// issue » ou du plan. Sans cela, le timer émettrait des certificats pour
		// des déclarations partagées inutilisées et consommerait les quotas.
		if !present && !force {
			continue
		}
		// Un certificat qui ne couvre plus la demande est réémis comme s'il
		// arrivait à échéance : c'est ce qui reprend seul une émission échouée à
		// l'apply — un alias dont le DNS ne pointait pas encore ici —, le vhost
		// servant déjà ce nom sur le certificat en place.
		due := !present || info.NeedsRenewal(now)
		if present && !force && !due && info.Covers(req.Domains) {
			continue
		}
		err = p.issueOrFallback(ctx, req, due || force)
		if Written(err) {
			done = append(done, req.Name)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s : %w", req.Name, err))
			continue
		}
		done = append(done, req.Name)
	}
	return done, errs
}

// Users énumère les vhosts qui référencent un certificat. Un certificat partagé
// ne doit pas pouvoir être supprimé tant qu'une configuration l'utilise.
func (p *Provider) Users(ref string, projects []*model.Project) []string {
	var out []string
	for _, proj := range projects {
		used, _ := p.CertificateFor(proj)
		if used == ref {
			out = append(out, proj.ID())
		}
	}
	sort.Strings(out)
	return out
}

// Delete supprime un certificat, en refusant de casser les vhosts qui s'en servent.
func (p *Provider) Delete(ref string, projects []*model.Project) error {
	if users := p.Users(ref, projects); len(users) > 0 {
		return fmt.Errorf("le certificat %s est référencé par %s : retirer d'abord ces vhosts",
			ref, strings.Join(users, ", "))
	}
	if _, ok, _ := p.Store.Read(ref); !ok {
		return fmt.Errorf("aucun certificat %s", ref)
	}
	return p.Store.Remove(ref)
}

// Warnings signale les certificats méritant attention. Le seuil d'un certificat
// partagé est plus précoce : son expiration casserait toutes les préproductions.
func (p *Provider) Warnings() ([]string, error) {
	list, err := p.Store.List()
	if err != nil {
		return nil, err
	}
	now := p.now()
	var out []string
	for _, info := range list {
		threshold := RenewBefore
		if info.Shared {
			threshold = SharedWarnBefore
		}
		switch {
		case info.Expired(now):
			out = append(out, fmt.Sprintf("certificat %s expiré depuis le %s",
				info.Name, info.NotAfter.Format("02/01/2006")))
		case now.Add(threshold).After(info.NotAfter):
			label := "certificat"
			if info.Shared {
				label = "certificat partagé"
			}
			out = append(out, fmt.Sprintf("%s %s expire dans %d jours",
				label, info.Name, info.DaysLeft(now)))
		}
	}
	return out, nil
}

// RenewUnitPlan produit les étapes d'installation de l'unité et du timer de
// renouvellement. Ce sont des ressources de portée serveur, régénérées comme le
// fichier de rotation des journaux.
func (p *Provider) RenewUnitPlan(templates *template.Set, binary, onCalendar string) ([]*engine.Step, error) {
	if onCalendar == "" {
		onCalendar = "*-*-* 03,15:00:00"
	}
	units := []struct {
		tmpl string
		file string
		data map[string]any
	}{
		{"systemd/ogs-renew.service", "ogs-renew.service", map[string]any{"Binary": binary}},
		{"systemd/ogs-renew.timer", "ogs-renew.timer", map[string]any{"OnCalendar": onCalendar}},
	}
	var steps []*engine.Step
	for _, u := range units {
		body, err := templates.Render(u.tmpl, u.data)
		if err != nil {
			return nil, err
		}
		content := fs.Render(fs.Header{
			Project:   "*",
			Resource:  "tls/renew/" + u.file,
			Generated: p.now(),
			Template:  templates.OverridePath(u.tmpl),
		}, body, "#")
		path := filepath.Join(p.Paths.SystemdDir(), u.file)
		state, err := fs.Inspect(path, content)
		if err != nil {
			return nil, err
		}
		resource := "tls/renew/" + u.file
		summary := "unité de renouvellement " + u.file
		forced := false
		switch state {
		case fs.StateCurrent:
			steps = append(steps, engine.NewStep(resource, engine.NoOp, u.file))
			continue
		case fs.StateHandEdited, fs.StateForeign:
			if !p.Force {
				steps = append(steps, refusStep(resource, summary, path, p.Paths.BackupsDir(), state))
				continue
			}
			// --force : l'étape redevient une mise à jour ordinaire, annoncée comme
			// telle par « plan » avant qu'« apply » ne l'exécute. La sauvegarde, elle,
			// n'a lieu qu'à l'application : « plan » et « diff » n'écrivent rien.
			forced = true
			state = fs.StateOutdated
			summary += " — écrasement forcé (sauvegarde dans " + p.Paths.BackupsDir() + ")"
		}
		action := engine.Create
		if state != fs.StateAbsent {
			action = engine.Update
		}
		s := engine.NewStep(resource, action, summary)
		target, data := path, content
		var previous []byte
		var hadPrevious bool
		s.Apply = func(context.Context) error {
			if body, err := os.ReadFile(target); err == nil {
				previous, hadPrevious = body, true
			}
			if forced {
				// Écraser le travail d'un exploitant sans en garder trace rend
				// l'opération irréversible : la sauvegarde précède l'écriture.
				if _, err := fs.BackupTo(target, p.Paths.BackupsDir(), p.now()); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			if err := fs.EnsureDir(filepath.Dir(target), 0o755, fs.KeepOwner); err != nil {
				return err
			}
			return fs.WriteAtomic(target, data, 0o644, fs.KeepOwner)
		}
		// La compensation rend l'unité qui était en place. La supprimer serait
		// juste pour une création, mais laisserait le parc sans renouvellement s'il
		// s'agissait d'une mise à jour.
		s.Compensate = func(context.Context) error {
			if hadPrevious {
				return fs.WriteAtomic(target, previous, 0o644, fs.KeepOwner)
			}
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		s.Reload = []string{"systemd:daemon-reload", "systemd:enable:ogs-renew.timer"}
		steps = append(steps, s)
	}
	return steps, nil
}
