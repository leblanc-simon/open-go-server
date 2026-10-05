package tls

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
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
	// Exec observe l'activation du timer de renouvellement (D105). Nil, rien
	// n'est observé.
	Exec      exec.Executor
	DNSSets   func() []DNSCredentials
	dnsSets   []DNSCredentials
	dnsLoaded bool
	// ServedBy énumère les vhosts écrits qui désignent un répertoire de
	// certificat (web.Provider.CertificateReferences). C'est ce que le serveur
	// charge, non ce que déclarent les projets, qui décide si un certificat peut
	// quitter le disque (D110) : un vhost resté en place après une déclaration
	// retirée, un site hors d'ogs, un déroulage incomplet le désignent encore.
	ServedBy func(dir string) []string
}

// servedBy rend les vhosts écrits qui désignent un certificat.
func (p *Provider) servedBy(name string) []string {
	if p.ServedBy == nil {
		return nil
	}
	return p.ServedBy(p.Store.Dir(name))
}

// staging dit si les émissions doivent viser l'environnement de test de
// l'autorité : acme.staging de config.yaml vaut pour l'apply et le
// renouvellement comme pour « cert issue » (D110).
func (p *Provider) staging() bool { return p.Cfg != nil && p.Cfg.ACME.Staging }

// replacesStaging dit si un certificat présent, émis par l'environnement de test,
// doit être remplacé : la demande vise la production, et l'autorité configurée
// n'est pas elle-même celle de test — sans quoi chaque apply réémettrait un
// certificat de test à la place d'un autre.
func (p *Provider) replacesStaging(req Request, info Info) bool {
	if !info.Staging || req.Staging || req.Challenge == ChallengeSelfSigned {
		return false
	}
	return p.Cfg == nil || !SameDirectory(p.Cfg.ACME.Directory, DirectoryStaging)
}

// SameDirectory compare deux URL d'autorité ACME comme l'autorité les sert : le
// schéma et l'hôte sans casse, le port par défaut omis, le chemin sans « / »
// final. Une comparaison à la chaîne près faisait d'un « …/directory/ » une
// autorité de production, et le certificat de test était réémis à chaque apply
// et à chaque passage du timer, jusqu'au quota.
func SameDirectory(a, b string) bool {
	norm := func(raw string) string {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" {
			return strings.TrimRight(strings.TrimSpace(raw), "/")
		}
		scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname())
		if port := u.Port(); port != "" && !(scheme == "https" && port == "443") && !(scheme == "http" && port == "80") {
			host += ":" + port
		}
		return scheme + "://" + host + strings.TrimRight(u.EscapedPath(), "/")
	}
	return a != "" && b != "" && norm(a) == norm(b)
}

// keepsProduction dit si le renouvellement d'un certificat de production en
// place doit rester à l'autorité de production malgré acme.staging (D110) :
// l'option, oubliée après un essai, ferait sinon remplacer par le timer chaque
// certificat reconnu par un certificat qu'aucun navigateur n'accepte — HSTS
// compris, le site devient injoignable. Seule une demande explicite (« staging »
// déclaré, « cert issue --staging ») le remplace.
func keepsProduction(req Request, info Info, present bool) bool {
	return present && req.Staging && req.ConfigStaging && !info.Staging && !info.SelfSign &&
		req.Challenge != ChallengeSelfSigned
}

// productionKept rend la demande ramenée à l'autorité de production.
func productionKept(req Request) Request {
	req.Staging, req.ConfigStaging = false, false
	return req
}

// ProductionKeptError signale un renouvellement fait auprès de l'autorité de
// production alors qu'acme.staging est actif (D110) : le certificat est
// renouvelé, et l'exploitant doit l'apprendre — code 6.
type ProductionKeptError struct{ Name string }

func (e *ProductionKeptError) Error() string {
	return fmt.Sprintf("certificat %s renouvelé auprès de l'autorité de production malgré acme.staging : "+
		"un certificat reconnu n'est jamais remplacé par un certificat de test sans le demander "+
		"(« ogs cert issue %s --staging ») — retirer acme.staging de config.yaml si l'essai est fini (D110)",
		e.Name, e.Name)
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
			DNSProvider: c.DNSProvider, DNSCredentials: c.DNSCredentials, KeyType: c.KeyType,
			Staging: c.Staging || p.staging(), ConfigStaging: !c.Staging && p.staging(),
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
		Essential: servedDomains(proj), Staging: p.staging(), ConfigStaging: p.staging()}
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
		Staging: p.staging(), ConfigStaging: p.staging(),
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
	// Pending est le certificat que le vhost désigne, servi dès que son matériel
	// le couvrira : vide, rien n'est en attente (D102). Le certificat présent
	// reste servi jusque-là, pour ne jamais retirer le HTTPS d'un vhost (D79).
	Pending string
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
// certificat principal : celui du premier vhost, à défaut de vhost désigné.
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
	return p.present(cov)
}

// present complète une couverture par les noms du matériel présent et non
// expiré : c'est lui, jamais la demande, qui décide du rendu (D79).
func (p *Provider) present(cov Coverage) Coverage {
	if info, ok, err := p.Store.Read(cov.Ref); err == nil && ok && !info.Expired(p.now()) {
		cov.Present = info.Domains
	}
	return cov
}

// VHostCoverage renvoie le certificat que sert un vhost (D102) : celui qu'il
// désigne (tls.certificate du vhost, sinon spec.web.tls.certificate), un
// certificat du projet par son nom ou un certificat de portée serveur par
// « @nom », pourvu que sa demande couvre l'un des noms du vhost ; à défaut, le
// choix par défaut — le partagé de la préproduction, puis le premier
// certificat du projet dont la demande couvre l'un de ses noms, puis le
// premier tout court. Chaque vhost recevait le premier certificat déclaré, la
// référence lue puis ignorée : le vhost d'un second certificat restait en HTTP
// seul, et son certificat était renouvelé à vie sans servir.
//
// Trois règles empêchent une référence de retirer le HTTPS d'un vhost qui le
// servait (D79) :
//   - une référence que rien ne porte, ou dont la demande ne couvre aucun nom
//     du vhost, retombe sur le choix par défaut, et le plan du projet le
//     signale (ReferenceProblems) ;
//   - une préproduction couverte par le wildcard le sert, quelle que soit la
//     référence que « site clone » a recopiée de la production ;
//   - tant que le certificat désigné ne couvre pas encore réellement le vhost
//     — jamais émis, expiré —, le certificat présent qui le couvre reste
//     servi : la bascule suit l'émission (Coverage.Pending, PlanTLS).
func (p *Provider) VHostCoverage(proj *model.Project, vh model.VHost) Coverage {
	def := p.defaultCoverage(proj, vh)
	req, ok := p.designatedFor(proj, vh)
	if !ok || req.Name == def.Ref {
		return def
	}
	cov := p.present(Coverage{Ref: req.Name, Requested: req.Domains})
	if servesAny(cov.Present, vh.Domains) || !servesAny(def.Present, vh.Domains) {
		return cov
	}
	def.Pending = req.Name
	return def
}

// defaultCoverage est le choix par défaut d'un vhost qui ne désigne rien qui le
// couvre.
func (p *Provider) defaultCoverage(proj *model.Project, vh model.VHost) Coverage {
	if p.SharedRef(proj) != "" {
		return p.Coverage(proj)
	}
	reqs, err := p.Requests(proj)
	if err != nil || len(reqs) == 0 {
		return Coverage{}
	}
	// Le matériel présent qui couvre le vhost passe avant la demande : un
	// certificat ajouté en tête de spec.certificates, pas encore émis, ne retire
	// pas le HTTPS que sert celui d'après (D79).
	for _, req := range reqs {
		if cov := p.present(Coverage{Ref: req.Name, Requested: req.Domains}); servesAny(cov.Present, vh.Domains) {
			return cov
		}
	}
	chosen := reqs[0]
	for _, req := range reqs {
		if servesAny(req.Domains, vh.Domains) {
			chosen = req
			break
		}
	}
	return p.present(Coverage{Ref: chosen.Name, Requested: chosen.Domains})
}

// servesAny dit si des noms de certificat couvrent l'un des noms voulus.
func servesAny(names, wanted []string) bool {
	return len(names) > 0 && slices.ContainsFunc(wanted, func(d string) bool { return coveredBy(names, d) })
}

// certificateRef rend la référence de certificat que désigne un vhost, vide
// s'il n'en désigne aucune.
func certificateRef(proj *model.Project, vh model.VHost) string {
	if vh.TLS != nil && vh.TLS.Certificate != "" {
		return vh.TLS.Certificate
	}
	if t := proj.Spec.Web.TLS; t != nil {
		return t.Certificate
	}
	return ""
}

// resolve rend la demande que désigne une référence, et faux si rien ne la
// porte.
func (p *Provider) resolve(proj *model.Project, ref string) (Request, bool) {
	if strings.HasPrefix(ref, SharedPrefix) {
		return p.SharedRequest(ref)
	}
	reqs, err := p.Requests(proj)
	if err != nil {
		return Request{}, false
	}
	for _, req := range reqs {
		if req.Name == ref {
			return req, true
		}
	}
	return Request{}, false
}

// designatedFor rend le certificat que désigne un vhost, s'il en désigne un
// que sa demande fait couvrir l'un de ses noms. Une préproduction couverte par
// le wildcard n'en désigne jamais (D26).
func (p *Provider) designatedFor(proj *model.Project, vh model.VHost) (Request, bool) {
	ref := certificateRef(proj, vh)
	if ref == "" || p.SharedRef(proj) != "" {
		return Request{}, false
	}
	req, ok := p.resolve(proj, ref)
	if !ok || !servesAny(req.Domains, vh.Domains) {
		return Request{}, false
	}
	return req, true
}

// refUse est l'emploi d'une référence de certificat : où elle est écrite, et
// les noms qu'elle doit couvrir.
type refUse struct {
	where, ref string
	domains    []string
}

// refUses énumère les références de certificat d'un projet qui veut le TLS,
// vhost par vhost — spec.web.tls vaut pour chaque vhost qui ne désigne rien, et
// pour le vhost déduit d'un projet qui n'en déclare aucun. Une préproduction
// couverte par le wildcard n'en a aucune : la référence que « site clone » a
// recopiée de la production n'y désigne rien.
func (p *Provider) refUses(proj *model.Project) []refUse {
	if proj.Spec.Web.TLSDisabled() || p.SharedRef(proj) != "" {
		return nil
	}
	var out []refUse
	if len(proj.Spec.Web.VHosts) == 0 {
		if t := proj.Spec.Web.TLS; t != nil && t.Certificate != "" {
			out = append(out, refUse{"spec.web.tls", t.Certificate, servedDomains(proj)})
		}
		return out
	}
	for _, vh := range proj.Spec.Web.VHosts {
		if vh.TLS != nil && !vh.TLS.Enabled {
			continue
		}
		where := "le vhost " + vh.Name
		if vh.TLS == nil || vh.TLS.Certificate == "" {
			where += " (par spec.web.tls)"
		}
		if ref := certificateRef(proj, vh); ref != "" {
			out = append(out, refUse{where, ref, vh.Domains})
		}
	}
	return out
}

// DesignatedShared rend les demandes des certificats de portée serveur que
// désignent les vhosts d'un projet et qui couvrent leurs noms : elles s'émettent
// et se renouvellent avec le projet, comme le wildcard de la préproduction.
func (p *Provider) DesignatedShared(proj *model.Project) []Request {
	var out []Request
	for _, u := range p.refUses(proj) {
		if !strings.HasPrefix(u.ref, SharedPrefix) ||
			slices.ContainsFunc(out, func(r Request) bool { return r.Name == u.ref }) {
			continue
		}
		if req, ok := p.SharedRequest(u.ref); ok && servesAny(req.Domains, u.domains) {
			out = append(out, req)
		}
	}
	return out
}

// ReferenceProblems nomme les références de certificat sans objet : ni un
// certificat du projet ni un partagé déclaré ne portent ce nom, ou sa demande
// ne couvre aucun nom du vhost. Le vhost sert alors le certificat par défaut.
// Le plan du projet en fait une étape non critique en échec, doctor un
// avertissement ; le chargement du parc, jamais.
func (p *Provider) ReferenceProblems(proj *model.Project) []string {
	var out []string
	for _, u := range p.refUses(proj) {
		req, ok := p.resolve(proj, u.ref)
		var what string
		switch {
		case !ok && strings.HasPrefix(u.ref, SharedPrefix):
			what = "aucun certificat de portée serveur ne porte ce nom (certificates de config.yaml)"
		case !ok:
			what = "aucun certificat du projet ne porte ce nom (spec.certificates[].name)"
		case !servesAny(req.Domains, u.domains):
			what = fmt.Sprintf("il ne couvre aucun de ses noms (%s)", strings.Join(u.domains, ", "))
		default:
			continue
		}
		out = append(out, fmt.Sprintf("%s désigne le certificat « %s » : %s — le certificat par défaut est servi",
			u.where, u.ref, what))
	}
	return out
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
	reqs, err := p.planRequests(proj)
	if err != nil {
		return nil, err
	}
	var steps []*engine.Step
	for _, req := range reqs {
		step, err := p.certificateStep(req)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	if problems := p.ReferenceProblems(proj); len(problems) > 0 {
		s := engine.NewStep("tls/reference/"+proj.ID(), engine.Update,
			"référence de certificat sans objet : le certificat par défaut est servi")
		s.Critical = false
		s.Apply = func(context.Context) error {
			return errors.New(strings.Join(problems, " ; "))
		}
		steps = append(steps, s)
	}
	return steps, nil
}

// planRequests rend les demandes que le plan d'un projet examine : les siennes,
// le partagé de sa préproduction, et ceux que désignent ses vhosts.
func (p *Provider) planRequests(proj *model.Project) ([]Request, error) {
	reqs, err := p.Requests(proj)
	if err != nil {
		return nil, err
	}
	if shared := p.SharedRef(proj); shared != "" {
		if req, ok := p.SharedRequest(shared); ok {
			reqs = append(reqs, req)
		}
	}
	// Un certificat de portée serveur que désigne un vhost s'émet avec le projet
	// qui le sert, comme le wildcard de la préproduction : sans quoi il attendait
	// « cert issue », et le vhost restait en HTTP seul.
	for _, req := range p.DesignatedShared(proj) {
		if !slices.ContainsFunc(reqs, func(r Request) bool { return r.Name == req.Name }) {
			reqs = append(reqs, req)
		}
	}
	return reqs, nil
}

// PlanPairs rétablit, avant tout ce qui écrit la configuration du serveur web, les
// paires clé/certificat dépareillées du projet (D110). Le moteur refuse une
// paire dépareillée avec la configuration entière : nginx dès « nginx -t », donc
// à l'écriture du premier vhost tant qu'un autre la désigne encore — le vhost de
// redirection du même certificat, ou celui d'un projet voisin pour un partagé —,
// Apache au rechargement seulement, dont il meurt, « apachectl -t » répondant
// « Syntax OK ». L'étape d'émission, qui suit les vhosts pour que le challenge
// HTTP-01 trouve le sien chargé, venait trop tard : l'apply échouait en code 1
// sur le vhost, et seul « cert renew » réparait.
//
// Une écriture interrompue s'achève sans émission (Store.Repair) — étape
// critique, le rendu des vhosts l'ayant tenue pour achevée (Store.Read). Une
// paire qu'aucun fichier en attente n'achève est réémise ici, sans vidage des
// rechargements — le serveur sert le challenge avec la configuration qu'il a
// chargée, un rechargement échouerait ; l'étape est non critique, et son échec
// dit ce qui suivra et le remède.
func (p *Provider) PlanPairs(_ context.Context, proj *model.Project) ([]*engine.Step, error) {
	reqs, err := p.planRequests(proj)
	if err != nil {
		return nil, err
	}
	var reload []string
	if p.WebReload != "" {
		reload = []string{p.WebReload}
	}
	var steps []*engine.Step
	for _, req := range reqs {
		info, present, paired, err := p.Store.ReadPair(req.Name)
		if err != nil {
			return nil, err
		}
		if !present || paired {
			continue
		}
		resource := "tls/certificate/" + req.Name + "/paire"
		if info.Repairable {
			s := engine.NewStep(resource, engine.Update, fmt.Sprintf(
				"certificat %s : écriture interrompue, clé et certificat dépareillés — achevée avant les vhosts, "+
					"sans émission", req.Name))
			s.Apply = func(ctx context.Context) error {
				ok, err := p.Store.Repair(ctx, req.Name)
				if err == nil && !ok {
					err = fmt.Errorf("certificat %s : l'écriture interrompue n'a pas pu être achevée — %s",
						req.Name, p.unpairedConsequence())
				}
				return err
			}
			s.Reload = reload
			steps = append(steps, s)
			continue
		}
		kept := keepsProduction(req, info, present)
		if kept {
			req = productionKept(req)
		}
		s := engine.NewStep(resource, engine.Update, fmt.Sprintf(
			"certificat %s : clé et certificat dépareillés — réémis avant les vhosts%s", req.Name, keptNote(kept)))
		s.Critical = false
		s.Apply = func(ctx context.Context) error {
			if ok, err := p.Store.Repair(ctx, req.Name); err != nil || ok {
				return err
			}
			err := p.issueOrFallback(ctx, req, true)
			if err != nil && !Written(err) {
				return fmt.Errorf("%w — la paire reste dépareillée : %s ; remède : « ogs cert renew %s » une fois "+
					"la cause levée, ou la clé d'origine remise en place", err, p.unpairedConsequence(), req.Name)
			}
			return stepError(withKept(err, kept, req.Name))
		}
		s.Reload = reload
		steps = append(steps, s)
	}
	return steps, nil
}

// CheckPair vérifie la paire d'un répertoire de certificat que désigne un vhost
// activé (web.Provider.CheckCertificates), avant tout rechargement, toute
// validation et tout redémarrage du serveur web (D110). Avec repair, une
// écriture interrompue est achevée : c'est ce qui couvre, hors de l'apply du
// projet, une paire que Store.Read lit déjà comme achevée. Un répertoire hors du
// magasin, ou sans certificat, ne relève pas de ce contrôle.
func (p *Provider) CheckPair(ctx context.Context, dir string, repair bool) (name string, paired, repaired bool, err error) {
	base := filepath.Base(filepath.Clean(dir))
	name = base
	if strings.HasPrefix(base, sharedDirPrefix) {
		name = SharedPrefix + strings.TrimPrefix(base, sharedDirPrefix)
	}
	if filepath.Clean(p.Store.Dir(name)) != filepath.Clean(dir) {
		return name, true, false, nil
	}
	info, present, ok, err := p.Store.ReadPair(name)
	if err != nil || !present || ok {
		return name, true, false, err
	}
	if !repair || !info.Repairable {
		return name, false, false, nil
	}
	ok, err = p.Store.Repair(ctx, name)
	return name, ok, ok, err
}

// unpairedConsequence dit ce que fait le moteur retenu d'une paire dépareillée
// que des vhosts désignent encore.
func (p *Provider) unpairedConsequence() string {
	switch strings.TrimPrefix(p.WebReload, "web:") {
	case "nginx":
		return "nginx refuse toute configuration dont un vhost la désigne (« key values mismatch ») — " +
			"l'écriture des vhosts échoue (code 1) tant qu'un autre vhost la désigne encore, et aucun " +
			"rechargement ne passe"
	case "apache":
		return "Apache s'arrête au rechargement de toute configuration dont un vhost la désigne " +
			"(AH02565), « apachectl -t » répondant « Syntax OK » — les vhosts du projet passent en HTTP seul, " +
			"ceux d'un autre projet la désignent encore"
	}
	return "le serveur web refuse toute configuration dont un vhost la désigne"
}

// keptNote complète un résumé d'émission ramenée à l'autorité de production.
func keptNote(kept bool) string {
	if kept {
		return " — autorité de production conservée malgré acme.staging"
	}
	return ""
}

// withKept signale, au terme d'un renouvellement réussi, que l'autorité de
// production a été conservée malgré acme.staging : code 6, le rechargement
// restant acquis.
func withKept(err error, kept bool, name string) error {
	if kept && err == nil {
		return engine.Partial(&ProductionKeptError{Name: name})
	}
	return err
}

// certificateStep produit l'étape d'un certificat. Elle est toujours non
// critique : un certificat qui échoue laisse le site en ligne en HTTP, avec un
// avertissement et une commande de reprise (D16).
func (p *Provider) certificateStep(req Request) (*engine.Step, error) {
	resource := "tls/certificate/" + req.Name
	info, present, paired, err := p.Store.ReadPair(req.Name)
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
	if present && !paired {
		if pending, ok, err := p.Store.Pending(req.Name); err != nil {
			return nil, err
		} else if ok {
			// PlanPairs l'achève avant les vhosts : le plan en juge achevée.
			info, paired = pending, true
		}
	}
	kept := keepsProduction(req, info, present)
	if kept {
		req = productionKept(req)
	}
	issue := func(urgent bool) func(context.Context) error {
		return func(ctx context.Context) error {
			return stepError(withKept(p.issueOrFallback(ctx, req, urgent), kept, req.Name))
		}
	}
	switch {
	case present && !paired:
		// Une clé recopiée à la main : PlanPairs l'a réémise avant les vhosts. Si
		// cette émission a échoué, une seconde tentative a lieu ici, les vhosts du
		// projet écrits — en HTTP seul — et rechargés : sous Apache, un serveur
		// arrêté par la paire a pu être relevé entre-temps.
		s := engine.NewStep(resource, engine.Update, fmt.Sprintf(
			"certificat %s : clé et certificat dépareillés — nouvelle tentative si la réémission d'avant les "+
				"vhosts a échoué%s", req.Name, keptNote(kept)))
		s.Critical = false
		s.Apply = func(ctx context.Context) error {
			if _, _, ok, err := p.Store.ReadPair(req.Name); err != nil || ok {
				return err
			}
			if ok, err := p.Store.Repair(ctx, req.Name); err != nil || ok {
				return err
			}
			return issue(true)(ctx)
		}
		s.FlushReloads = req.servesChallenge()
		s.Reload = reload
		return s, nil
	case present && p.replacesStaging(req, info):
		// acme.staging retiré : le certificat de test, qu'aucun navigateur ne
		// reconnaît, est remplacé d'office. Il reste servi jusqu'à l'émission
		// (D79) — un échec laisse le site tel qu'il était.
		s := engine.NewStep(resource, engine.Update,
			fmt.Sprintf("certificat %s émis par l'environnement de test de l'autorité (%s) : remplacé "+
				"par un certificat reconnu, l'actuel restant servi jusqu'à l'émission", req.Name, info.Issuer))
		s.Critical = false
		s.Apply = issue(true)
		s.FlushReloads = req.servesChallenge()
		s.Reload = reload
		return s, nil
	case present && info.Covers(req.Domains) && !info.NeedsRenewal(now):
		return engine.NewStep(resource, engine.NoOp,
			fmt.Sprintf("certificat %s (expire dans %d jours)", req.Name, info.DaysLeft(now))), nil
	case present && !info.Covers(req.Domains):
		s := engine.NewStep(resource, engine.Update,
			fmt.Sprintf("certificat %s : domaines à couvrir modifiés (%s)%s",
				req.Name, strings.Join(req.Domains, ", "), keptNote(kept)))
		s.Critical = false
		// Loin de l'échéance, le certificat en place reste servi : le repli n'a lieu
		// que s'il couvre un nom de plus sans en retirer aucun. Près d'elle, il garde
		// le site quoi qu'il en coûte aux autres noms.
		s.Apply = issue(info.NeedsRenewal(now))
		// Le vidage recharge un vhost qui garde son bloc 443 sur le certificat en
		// place : il n'expose que les nouveaux noms au challenge du port 80.
		s.FlushReloads = req.servesChallenge()
		s.Reload = reload
		return s, nil
	case present:
		s := engine.NewStep(resource, engine.Update,
			fmt.Sprintf("renouvellement du certificat %s (expire dans %d jours)%s",
				req.Name, info.DaysLeft(now), keptNote(kept)))
		s.Critical = false
		s.Apply = issue(true)
		s.FlushReloads = req.servesChallenge()
		s.Reload = reload
		return s, nil
	}
	s := engine.NewStep(resource, engine.Create,
		fmt.Sprintf("certificat %s pour %s (%s%s)", req.Name, strings.Join(req.Domains, ", "), req.Method(),
			stagingNote(req)))
	s.Critical = false
	s.Apply = issue(true)
	s.Reload = reload
	// Le challenge HTTP-01 se fait servir par le vhost : il doit être chargé, non
	// seulement écrit. Sans ce vidage, la première émission d'un site neuf échoue
	// en 404, le serveur n'ayant pas encore relu sa configuration.
	s.FlushReloads = req.servesChallenge()
	// La compensation ne retire que ce que cette exécution a créé, et jamais un
	// certificat qu'un vhost écrit désigne encore (D110) : la bascule TLS d'un
	// déroulage incomplet — aboutie en partie, donc non compensée — laisserait
	// sinon un bloc 443 sur un matériel absent, que le moteur refuse au
	// rechargement suivant, le parc entier avec lui. Un certificat valide de
	// trop ne coûte rien.
	s.Compensate = func(context.Context) error {
		if len(p.servedBy(req.Name)) > 0 {
			return nil
		}
		return p.Store.Remove(req.Name)
	}
	return s, nil
}

// stagingNote complète un résumé d'émission vers l'environnement de test.
func stagingNote(req Request) string {
	switch {
	case req.ConfigStaging:
		return ", environnement de test (acme.staging)"
	case req.Staging:
		return ", environnement de test"
	}
	return ""
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
	if err := p.Store.WriteContext(ctx, req.Name, material); err != nil {
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
		info, present, paired, err := p.Store.ReadPair(req.Name)
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
		// Une paire dépareillée (D110) : l'écriture interrompue s'achève sans
		// émission, puis le certificat achevé est jugé comme les autres — il peut
		// être lui-même à échéance, ou ne plus couvrir la demande. À défaut, la
		// paire est réémise comme à l'échéance.
		repaired := false
		if present && !paired {
			ok, rerr := p.Store.Repair(ctx, req.Name)
			if rerr != nil {
				errs = append(errs, fmt.Errorf("%s : %w", req.Name, rerr))
				continue
			}
			if ok {
				repaired = true
				if info, present, paired, err = p.Store.ReadPair(req.Name); err != nil {
					errs = append(errs, err)
					continue
				}
			}
		}
		// Un certificat qui ne couvre plus la demande est réémis comme s'il
		// arrivait à échéance : c'est ce qui reprend seul une émission échouée à
		// l'apply — un alias dont le DNS ne pointait pas encore ici —, le vhost
		// servant déjà ce nom sur le certificat en place. Un certificat de
		// l'environnement de test l'est aussi, acme.staging retiré (D110).
		due := !present || !paired || info.NeedsRenewal(now) || p.replacesStaging(req, info)
		if present && !force && !due && info.Covers(req.Domains) {
			if repaired {
				// Le matériel a changé sur le disque : le serveur doit le relire.
				done = append(done, req.Name)
			}
			continue
		}
		// Un certificat de production se renouvelle en production, acme.staging
		// ou non (D110), et le renouvellement le dit.
		kept := keepsProduction(req, info, present)
		if kept {
			req = productionKept(req)
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
		if kept {
			errs = append(errs, &ProductionKeptError{Name: req.Name})
		}
	}
	return done, errs
}

// Users énumère les vhosts qui référencent un certificat. Un certificat partagé
// ne doit pas pouvoir être supprimé tant qu'une configuration l'utilise.
func (p *Provider) Users(ref string, projects []*model.Project) []string {
	var out []string
	for _, proj := range projects {
		used, _ := p.CertificateFor(proj)
		// Un vhost qui désigne son certificat le sert (D102) : le supprimer
		// casserait ce vhost-là, non le premier du projet.
		for _, vh := range proj.Spec.Web.VHosts {
			if used != ref && !proj.Spec.Web.TLSDisabled() && (vh.TLS == nil || vh.TLS.Enabled) {
				if cov := p.VHostCoverage(proj, vh); cov.Ref == ref || cov.Pending == ref {
					used = ref
				}
			}
		}
		if used == ref {
			out = append(out, proj.ID())
		}
	}
	sort.Strings(out)
	return out
}

// CheckRef refuse une référence de certificat qui ne désigne pas un certificat
// du magasin par son nom : un nom de projet (CheckProjectName), ou « @nom » pour
// un partagé. « _shared-x » est le répertoire de « @x » — le désigner ainsi
// échappait au contrôle des projets qui l'emploient —, et « . », « .. » ou un
// « / » mèneraient hors du magasin, où la suppression est un RemoveAll.
func CheckRef(ref string) error {
	if IsShared(ref) {
		if err := CheckProjectName(strings.TrimPrefix(ref, SharedPrefix)); err != nil {
			return fmt.Errorf("référence de certificat partagé %q invalide", ref)
		}
		return nil
	}
	if strings.HasPrefix(ref, sharedDirPrefix) {
		return fmt.Errorf("%q est le répertoire d'un certificat partagé : le désigner par « @%s »",
			ref, strings.TrimPrefix(ref, sharedDirPrefix))
	}
	if ref == ".." {
		return fmt.Errorf("nom de certificat %q invalide", ref)
	}
	return CheckProjectName(ref)
}

// DeleteRefusal dit pourquoi un certificat ne peut pas quitter le disque : une
// déclaration l'emploie, ou un vhost écrit le désigne (D110). Les deux sont
// nommés d'un coup : retirer l'un pour apprendre l'autre coûterait une coupure.
func (p *Provider) DeleteRefusal(ref string, projects []*model.Project) error {
	users := p.Users(ref, projects)
	files := p.servedBy(ref)
	var parts []string
	if len(users) > 0 {
		parts = append(parts, "employé par "+strings.Join(users, ", "))
	}
	if len(files) > 0 {
		parts = append(parts, "désigné par les vhosts écrits "+strings.Join(files, ", "))
	}
	if len(parts) > 0 {
		return fmt.Errorf("le certificat %s est %s : le retirer arrêterait le serveur web au "+
			"rechargement suivant — retirer d'abord ces vhosts", ref, strings.Join(parts, " et "))
	}
	return nil
}

// Delete supprime un certificat, en refusant de casser les vhosts qui s'en servent.
func (p *Provider) Delete(ref string, projects []*model.Project) error {
	if err := CheckRef(ref); err != nil {
		return err
	}
	if err := p.DeleteRefusal(ref, projects); err != nil {
		return err
	}
	if _, present, _, _ := p.Store.ReadPair(ref); !present {
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
		if info.Unpaired {
			out = append(out, p.unpairedWarning(info))
			continue
		}
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

// unpairedWarning nomme une paire dépareillée pour « doctor », « cert list » et
// les alertes : ce que le moteur en fait, et le remède (D110).
func (p *Provider) unpairedWarning(info Info) string {
	if info.Repairable {
		return fmt.Sprintf("certificat %s : écriture interrompue, clé et certificat dépareillés — %s ; "+
			"« ogs apply » ou « ogs cert renew %s » l'achève sans émission",
			info.Name, p.unpairedConsequence(), info.Name)
	}
	return fmt.Sprintf("certificat %s : clé et certificat dépareillés (clé recopiée à la main ?) — %s ; "+
		"« ogs cert renew %s » le réémet, « ogs apply » aussi, avant d'écrire les vhosts",
		info.Name, p.unpairedConsequence(), info.Name)
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
	if s := p.renewTimerStep(steps); s != nil {
		steps = append(steps, s)
	}
	return steps, nil
}

// RenewTimerProblem rend l'état d'un timer de renouvellement installé qui ne
// tourne pas — « désactivé » ou « arrêté » —, vide s'il tourne, s'il est absent
// ou si rien n'est observable (exécuteur simulé ou nul). doctor le porte aussi.
func (p *Provider) RenewTimerProblem(ctx context.Context) string {
	const timer = "ogs-renew.timer"
	if p.Exec == nil || p.Exec.Simulated() {
		return ""
	}
	if _, err := os.Stat(filepath.Join(p.Paths.SystemdDir(), timer)); err != nil {
		return ""
	}
	quiet := func(verb string) bool {
		_, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{verb, "--quiet", timer}})
		return err == nil
	}
	switch {
	case !quiet("is-enabled"):
		return "désactivé"
	case systemdBooted() && !quiet("is-active"):
		return "arrêté"
	}
	return ""
}

// systemdBooted dit si systemd est le gestionnaire du système (sd_booted) :
// sans lui, aucun timer ne tourne, et l'état « actif » n'a pas de sens — c'est
// le cas des conteneurs d'essai, où un substitut tient lieu de systemctl.
var systemdBooted = func() bool {
	info, err := os.Stat("/run/systemd/system")
	return err == nil && info.IsDir()
}

// renewTimerStep observe un timer de renouvellement présent mais qui ne tourne
// pas (R1-49, D105). Son activation n'était tentée qu'à l'écriture de l'unité, et
// rien ne disait qu'il ne tournait plus : plus aucun certificat ne se
// renouvelait, sans un mot. Rien n'est observé quand une écriture porte déjà
// l'activation, ni sur un exécuteur simulé.
//
// Désactivé, il n'est pas réactivé : c'est un choix que D89 respecte jusqu'à la
// réinstallation du paquet, et apply ne passe pas outre. L'étape non bloquante
// le dit à chaque apply (code 6), avec la commande qui le lève. Activé mais
// arrêté — un « systemctl stop », qui ne survit pas au redémarrage —, il est
// démarré : l'activation dit ce que l'administrateur veut.
func (p *Provider) renewTimerStep(steps []*engine.Step) *engine.Step {
	const timer = "ogs-renew.timer"
	for _, s := range steps {
		if s.Mutates() && slices.Contains(s.Reload, "systemd:enable:"+timer) {
			return nil
		}
	}
	switch p.RenewTimerProblem(context.Background()) {
	case "":
		return nil
	case "désactivé":
		s := engine.NewStep("tls/renew/activation", engine.Update,
			"timer de renouvellement désactivé — aucun certificat ne se renouvelle")
		s.Critical = false
		s.Apply = func(context.Context) error {
			return fmt.Errorf("%s est désactivé et ogs ne le réactive pas : « systemctl enable --now %s » "+
				"s'il ne l'a pas été volontairement", timer, timer)
		}
		return s
	}
	s := engine.NewStep("tls/renew/activation", engine.Update,
		"timer de renouvellement arrêté — démarrage")
	s.Reload = []string{"systemd:enable:" + timer}
	return s
}
