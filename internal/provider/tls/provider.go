package tls

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/leviathan/ogs/internal/config"
	"github.com/leviathan/ogs/internal/engine"
	"github.com/leviathan/ogs/internal/fs"
	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/store"
	"github.com/leviathan/ogs/internal/template"
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
			DNSProvider: c.DNSProvider, KeyType: c.KeyType, Staging: c.Staging,
		}
		if req.Name == "" {
			req.Name = proj.ID()
		}
		if len(req.Domains) == 0 {
			req.Domains = servedDomains(proj)
		}
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
	domains := servedDomains(proj)
	if len(domains) == 0 {
		return nil, nil
	}
	req := Request{Name: proj.ID(), Domains: domains, Challenge: ChallengeHTTP01}
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
	return Request{
		Name: SharedPrefix + name, Domains: decl.Domains, Challenge: ch,
		DNSProvider: decl.DNSProvider, KeyType: decl.KeyType,
	}, true
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
		_, ok, _ := p.Store.Read(shared)
		return shared, ok
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
	// Un certificat présent mais ne couvrant pas tous les domaines servis n'est
	// pas utilisable : l'activer produirait un avertissement de sécurité.
	return ref, info.Covers(reqs[0].Domains) && !info.Expired(p.now())
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

// servedDomains renvoie les domaines à couvrir : les domaines principaux et les
// alias, à l'exclusion des redirections, qui n'ont pas à être chiffrées.
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
	switch {
	case present && info.Covers(req.Domains) && !info.NeedsRenewal(now):
		return engine.NewStep(resource, engine.NoOp,
			fmt.Sprintf("certificat %s (expire dans %d jours)", req.Name, info.DaysLeft(now))), nil
	case present && !info.Covers(req.Domains):
		s := engine.NewStep(resource, engine.Update,
			fmt.Sprintf("certificat %s : domaines à couvrir modifiés (%s)",
				req.Name, strings.Join(req.Domains, ", ")))
		s.Critical = false
		s.Apply = func(ctx context.Context) error { return p.issue(ctx, req) }
		s.FlushReloads = req.Challenge == "http-01"
		return s, nil
	case present:
		s := engine.NewStep(resource, engine.Update,
			fmt.Sprintf("renouvellement du certificat %s (expire dans %d jours)",
				req.Name, info.DaysLeft(now)))
		s.Critical = false
		s.Apply = func(ctx context.Context) error { return p.issue(ctx, req) }
		s.FlushReloads = req.Challenge == "http-01"
		return s, nil
	}
	s := engine.NewStep(resource, engine.Create,
		fmt.Sprintf("certificat %s pour %s (%s)", req.Name, strings.Join(req.Domains, ", "), req.Challenge))
	s.Critical = false
	s.Apply = func(ctx context.Context) error { return p.issue(ctx, req) }
	// Le challenge HTTP-01 se fait servir par le vhost : il doit être chargé, non
	// seulement écrit. Sans ce vidage, la première émission d'un site neuf échoue
	// en 404, le serveur n'ayant pas encore relu sa configuration.
	s.FlushReloads = req.Challenge == "http-01"
	// La compensation ne retire que ce que cette exécution a créé.
	s.Compensate = func(context.Context) error { return p.Store.Remove(req.Name) }
	return s, nil
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
	return nil
}

// Issue émet ou renouvelle un certificat à la demande.
func (p *Provider) Issue(ctx context.Context, req Request) error { return p.issue(ctx, req) }

// Renew renouvelle les certificats arrivant à échéance et renvoie ceux traités.
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
		if present && !force && !info.NeedsRenewal(now) {
			continue
		}
		if err := p.issue(ctx, req); err != nil {
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

// DNSCredentialsFile renvoie le chemin des identifiants d'un provider DNS.
func (p *Provider) DNSCredentialsFile(provider string) string {
	return p.Paths.DNSCredsDir() + string(os.PathSeparator) + provider + ".yaml"
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
		if state == fs.StateCurrent {
			steps = append(steps, engine.NewStep("tls/renew/"+u.file, engine.NoOp, u.file))
			continue
		}
		action := engine.Create
		if state != fs.StateAbsent {
			action = engine.Update
		}
		s := engine.NewStep("tls/renew/"+u.file, action, "unité de renouvellement "+u.file)
		target, data := path, content
		s.Apply = func(context.Context) error {
			if err := fs.EnsureDir(filepath.Dir(target), 0o755, fs.KeepOwner); err != nil {
				return err
			}
			return fs.WriteAtomic(target, data, 0o644, fs.KeepOwner)
		}
		s.Compensate = func(context.Context) error { return os.Remove(target) }
		s.Reload = []string{"systemd:daemon-reload", "systemd:enable:ogs-renew.timer"}
		steps = append(steps, s)
	}
	return steps, nil
}
