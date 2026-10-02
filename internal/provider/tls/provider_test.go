package tls

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/store"
	"leblanc.io/open-go-server/internal/template"
)

func newProvider(t *testing.T) *Provider {
	t.Helper()
	paths := config.NewPaths(t.TempDir())
	db, err := store.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := func() time.Time { return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) }
	return &Provider{
		Paths: paths, Cfg: config.Default(), Store: Store{Paths: paths}, DB: db,
		// Aucun réseau en test : l'émetteur auto-signé tient le rôle de l'ACME.
		Issuer:   SelfSigned{Validity: 90 * 24 * time.Hour, Now: now},
		Fallback: SelfSigned{Validity: 90 * 24 * time.Hour, Now: now},
		Now:      now,
	}
}

func newProject(t *testing.T, host string, env naming.Env) *model.Project {
	t.Helper()
	p := model.New(host, env, model.KindSymfony)
	p.Spec.App.Version = "6.4"
	if err := p.Normalize(filepath.Join(t.TempDir(), "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRequestsDerivedFromDomains(t *testing.T) {
	p := newProvider(t)
	proj := newProject(t, "shop.fr", naming.EnvProd)
	proj.Spec.Domains = append(proj.Spec.Domains,
		model.Domain{Name: "www.shop.fr", Role: "alias"},
		model.Domain{Name: "ancien-shop.fr", Role: "redirect", Target: "shop.fr"})
	reqs, err := p.Requests(proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 1 {
		t.Fatalf("%d demandes, 1 attendue : %+v", len(reqs), reqs)
	}
	if reqs[0].Name != "shopfr" {
		t.Errorf("nom du certificat : %q", reqs[0].Name)
	}
	// Une redirection a autant besoin d'être chiffrée (D43) : l'en exclure servait
	// son vhost 443 avec un certificat qui ne la couvrait pas. Qu'elle fasse
	// échouer la commande ACME ne doit pas coûter le certificat du site : les noms
	// du vhost principal sont ceux sur lesquels l'émission se replie.
	if strings.Join(reqs[0].Domains, ",") != "shop.fr,www.shop.fr,ancien-shop.fr" {
		t.Errorf("domaines couverts : %v", reqs[0].Domains)
	}
	if strings.Join(reqs[0].Essential, ",") != "shop.fr,www.shop.fr" {
		t.Errorf("noms essentiels : %v", reqs[0].Essential)
	}
	if reqs[0].Challenge != ChallengeHTTP01 {
		t.Errorf("challenge par défaut : %q", reqs[0].Challenge)
	}
}

func TestPrepUsesSharedWildcardAndAsksNothing(t *testing.T) {
	p := newProvider(t)
	prep := newProject(t, "shop.fr", naming.EnvPrep)
	// Une préproduction ne demande aucun certificat : le wildcard partagé la
	// couvre, ce qui supprime toute émission à la création (D26).
	reqs, err := p.Requests(prep)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 0 {
		t.Errorf("aucune demande propre attendue : %+v", reqs)
	}
	if ref := p.SharedRef(prep); ref != "@wildcard-prep" {
		t.Errorf("référence partagée : %q", ref)
	}
	// Le plan porte cependant l'émission du wildcard lui-même.
	steps, err := p.Plan(context.Background(), prep)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || !strings.Contains(steps[0].Resource, "@wildcard-prep") {
		t.Fatalf("plan inattendu : %+v", steps)
	}
}

func TestPreproductionGardeLeWildcardMalgreUneRedirection(t *testing.T) {
	// Couvrir les redirections (D43) ne doit pas priver une préproduction du
	// wildcard partagé (D26) : une redirection qu'il ne couvre pas ne vaut pas
	// une émission propre, avec l'appel ACME que D26 écarte à la création.
	p := newProvider(t)
	prep := newProject(t, "shop.fr", naming.EnvPrep)
	prep.Spec.Domains = append(prep.Spec.Domains,
		model.Domain{Name: "ancien-shop.fr", Role: "redirect", Target: prep.Spec.Domains[0].Name})
	if ref := p.SharedRef(prep); ref != "@wildcard-prep" {
		t.Errorf("le wildcard couvre les domaines servis, la référence doit rester partagée : %q", ref)
	}
	if reqs, err := p.Requests(prep); err != nil || len(reqs) != 0 {
		t.Errorf("aucune demande propre attendue : %+v, %v", reqs, err)
	}
}

func TestCouvertureDistingueLeMaterielPresentDeLaDemande(t *testing.T) {
	// Le rendu d'un vhost suit ce que couvre le certificat présent, non ce que
	// le plan demande : un alias ajouté ne doit pas retirer le HTTPS des noms que
	// le certificat en place couvre toujours.
	p := newProvider(t)
	proj := newProject(t, "shop.fr", naming.EnvProd)
	reqs, _ := p.Requests(proj)
	if err := p.issue(context.Background(), reqs[0]); err != nil {
		t.Fatal(err)
	}
	if cov := p.Coverage(proj); !cov.Complete() || !cov.Serves("shop.fr") {
		t.Errorf("certificat émis pour la demande : %+v", cov)
	}

	proj.Spec.Domains = append(proj.Spec.Domains, model.Domain{Name: "boutique.fr", Role: "alias"})
	cov := p.Coverage(proj)
	if cov.Ref != "shopfr" {
		t.Errorf("référence : %q", cov.Ref)
	}
	if !cov.Serves("shop.fr") || cov.Serves("boutique.fr") {
		t.Errorf("le matériel présent couvre shop.fr et lui seul : %+v", cov)
	}
	if !cov.WillServe("boutique.fr") || cov.Complete() {
		t.Errorf("la demande couvre boutique.fr, que le présent ne couvre pas : %+v", cov)
	}
	// Le rapport d'état, lui, dit bien que le certificat n'est pas complet.
	if _, ready := p.CertificateFor(proj); ready {
		t.Error("un certificat qui ne couvre pas toute la demande n'est pas complet")
	}

	// Un certificat expiré n'est plus présenté : il ne couvre rien.
	p.Now = func() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) }
	if cov := p.Coverage(proj); cov.Serves("shop.fr") || len(cov.Present) != 0 {
		t.Errorf("un certificat expiré ne doit rien couvrir : %+v", cov)
	}
}

func TestEmissionPorteLeRechargementDuServeurWeb(t *testing.T) {
	// Un certificat réémis remplace un matériel que le serveur garde en mémoire.
	// Quand le vhost n'a pas à changer — un alias ajouté, un renouvellement —,
	// rien d'autre ne le rechargerait, et il présenterait l'ancien certificat.
	p := newProvider(t)
	p.WebReload = "web:apache"
	proj := newProject(t, "shop.fr", naming.EnvProd)
	ctx := context.Background()
	rechargeLeWeb := func(s *engine.Step) bool {
		for _, r := range s.Reload {
			if r == "web:apache" {
				return true
			}
		}
		return false
	}

	steps, _ := p.Plan(ctx, proj)
	if !rechargeLeWeb(steps[0]) {
		t.Errorf("première émission sans rechargement : %+v", steps[0].Reload)
	}
	if err := steps[0].Apply(ctx); err != nil {
		t.Fatal(err)
	}

	proj.Spec.Domains = append(proj.Spec.Domains, model.Domain{Name: "boutique.fr", Role: "alias"})
	steps, _ = p.Plan(ctx, proj)
	if !strings.Contains(steps[0].Summary, "domaines à couvrir") || !rechargeLeWeb(steps[0]) {
		t.Errorf("la réémission d'un certificat en service doit recharger le serveur : %s %v",
			steps[0].Summary, steps[0].Reload)
	}
	// Le vidage préalable reste nécessaire : le port 80 doit servir le challenge
	// du nouvel alias.
	if !steps[0].FlushReloads {
		t.Error("le challenge HTTP-01 de l'alias exige un vhost rechargé avant l'émission")
	}
	if err := steps[0].Apply(ctx); err != nil {
		t.Fatal(err)
	}

	p.Now = func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) }
	steps, _ = p.Plan(ctx, proj)
	if !strings.Contains(steps[0].Summary, "renouvellement") || !rechargeLeWeb(steps[0]) {
		t.Errorf("le renouvellement doit recharger le serveur : %s %v", steps[0].Summary, steps[0].Reload)
	}
}

func TestRenouvellementReemetUnCertificatQuiNeCouvrePlusLaDemande(t *testing.T) {
	// Une émission échouée à l'apply — un alias dont le DNS ne pointait pas
	// encore ici — doit être reprise par le timer, sans attendre l'échéance ni un
	// nouvel apply.
	p := newProvider(t)
	proj := newProject(t, "shop.fr", naming.EnvProd)
	reqs, _ := p.Requests(proj)
	if err := p.issue(context.Background(), reqs[0]); err != nil {
		t.Fatal(err)
	}
	proj.Spec.Domains = append(proj.Spec.Domains, model.Domain{Name: "boutique.fr", Role: "alias"})
	reqs, _ = p.Requests(proj)
	done, errs := p.Renew(context.Background(), reqs, false)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if len(done) != 1 {
		t.Fatalf("le certificat qui ne couvre plus la demande doit être réémis : %v", done)
	}
	if info, _, _ := p.Store.Read("shopfr"); !info.Covers([]string{"shop.fr", "boutique.fr"}) {
		t.Errorf("le certificat réémis doit couvrir l'alias : %v", info.Domains)
	}
	// Couvert et frais : plus rien à faire.
	if done, _ := p.Renew(context.Background(), reqs, false); len(done) != 0 {
		t.Errorf("aucune réémission attendue : %v", done)
	}
}

// autoritéQuiRefuse délivre comme l'émetteur auto-signé, sauf pour un nom qu'elle
// refuse : la commande ACME échoue alors tout entière, comme chez Let's Encrypt
// quand un seul challenge n'aboutit pas.
type autoritéQuiRefuse struct {
	refuse   string
	inner    Issuer
	demandes []string
}

func (a *autoritéQuiRefuse) Name() string { return "autorité qui refuse" }
func (a *autoritéQuiRefuse) Issue(ctx context.Context, req Request) (Material, error) {
	a.demandes = append(a.demandes, strings.Join(req.Domains, ","))
	for _, d := range req.Domains {
		if d == a.refuse {
			return Material{}, fmt.Errorf("challenge http-01 refusé pour %s", d)
		}
	}
	return a.inner.Issue(ctx, req)
}
func (a *autoritéQuiRefuse) Revoke(context.Context, Request, []byte) error { return nil }

// refuser installe une autorité qui refuse un nom, et dont l'horloge suit celle
// du provider : un certificat renouvelé plus tard expire plus tard.
func refuser(p *Provider, nom string) *autoritéQuiRefuse {
	a := &autoritéQuiRefuse{refuse: nom, inner: SelfSigned{Validity: 90 * 24 * time.Hour,
		Now: func() time.Time { return p.Now() }}}
	p.Issuer = a
	return a
}

func projetAvecRedirection(t *testing.T) *model.Project {
	t.Helper()
	proj := newProject(t, "shop.fr", naming.EnvProd)
	proj.Spec.Domains = append(proj.Spec.Domains,
		model.Domain{Name: "www.shop.fr", Role: "alias"},
		model.Domain{Name: "ancien-shop.fr", Role: "redirect", Target: "shop.fr"})
	return proj
}

func TestUneRedirectionRefuseeNeFaitPasExpirerLeCertificatDuSite(t *testing.T) {
	// Une commande ACME échoue tout entière sur un seul nom refusé. Avec les
	// redirections dans la demande, une redirection dont le DNS pointe ailleurs —
	// ou dont l'enregistrement a expiré — faisait échouer le renouvellement du
	// site principal, qui tombait à l'échéance, HSTS compris. Deux parcs : l'un
	// dont le certificat a été émis sans la redirection, l'autre dont la
	// redirection, validée autrefois, est refusée aujourd'hui.
	for nom, initial := range map[string][]string{
		"émis sans la redirection":       {"shop.fr", "www.shop.fr"},
		"redirection refusée après coup": {"shop.fr", "www.shop.fr", "ancien-shop.fr"},
	} {
		t.Run(nom, func(t *testing.T) {
			p := newProvider(t)
			proj := projetAvecRedirection(t)
			reqs, _ := p.Requests(proj)
			avant := reqs[0]
			avant.Domains = initial
			if err := p.issue(context.Background(), avant); err != nil {
				t.Fatal(err)
			}
			autorité := refuser(p, "ancien-shop.fr")

			// À dix jours de l'échéance, le timer renouvelle.
			p.Now = func() time.Time { return time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC) }
			done, errs := p.Renew(context.Background(), reqs, false)
			if len(done) != 1 {
				t.Fatalf("le certificat du site doit être renouvelé malgré la redirection : %v %v", done, errs)
			}
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), "ancien-shop.fr non couvert") {
				t.Fatalf("la redirection écartée doit être signalée : %v", errs)
			}
			// Couverte hier, son vhost est encore écrit en HTTPS : l'exploitant doit
			// apprendre que l'apply le repassera en HTTP seul.
			retirée := strings.Contains(errs[0].Error(), "ancien-shop.fr n'est plus couvert : « ogs apply »")
			if retirée != (len(initial) == 3) {
				t.Errorf("le retrait d'un nom couvert jusque-là doit être annoncé, lui seul : %v", errs[0])
			}
			// Passé l'échéance de l'ancien certificat, le site est toujours couvert.
			p.Now = func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) }
			cov := p.Coverage(proj)
			if !cov.Serves("shop.fr") || !cov.Serves("www.shop.fr") || cov.Serves("ancien-shop.fr") {
				t.Errorf("le site doit rester couvert, la redirection non : %+v", cov)
			}
			if got := strings.Join(autorité.demandes, " | "); got !=
				"shop.fr,www.shop.fr,ancien-shop.fr | shop.fr,www.shop.fr" {
				t.Errorf("demandes à l'autorité : %s", got)
			}
		})
	}
}

func TestUnAliasEnAttenteNeFaitPasExpirerLeCertificatDuSite(t *testing.T) {
	// Un alias dont le DNS ne pointera jamais ici garde le site en HTTPS sur le
	// certificat en place — jusqu'à l'échéance de celui-ci, si son renouvellement
	// exige l'alias. Le repli ne retient que ce que le certificat couvrait déjà.
	p := newProvider(t)
	proj := newProject(t, "shop.fr", naming.EnvProd)
	reqs, _ := p.Requests(proj)
	if err := p.issue(context.Background(), reqs[0]); err != nil {
		t.Fatal(err)
	}
	proj.Spec.Domains = append(proj.Spec.Domains, model.Domain{Name: "boutique.fr", Role: "alias"})
	reqs, _ = p.Requests(proj)
	refuser(p, "boutique.fr")
	p.Now = func() time.Time { return time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC) }
	done, _ := p.Renew(context.Background(), reqs, false)
	if len(done) != 1 {
		t.Fatalf("le certificat doit être renouvelé pour ce qu'il couvrait : %v", done)
	}
	if info, _, _ := p.Store.Read("shopfr"); strings.Join(info.Domains, ",") != "shop.fr" ||
		info.NotAfter.Before(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("renouvellement attendu pour shop.fr seul : %v jusqu'au %s", info.Domains, info.NotAfter)
	}
}

func TestLoinDeLEcheanceUnRepliQuiNAjouteRienNEstPasTente(t *testing.T) {
	// Un certificat frais sert déjà ce qu'il couvre : le réémettre à l'identique
	// sur un refus consommerait les quotas de l'autorité pour rien.
	p := newProvider(t)
	proj := newProject(t, "shop.fr", naming.EnvProd)
	reqs, _ := p.Requests(proj)
	if err := p.issue(context.Background(), reqs[0]); err != nil {
		t.Fatal(err)
	}
	proj.Spec.Domains = append(proj.Spec.Domains, model.Domain{Name: "boutique.fr", Role: "alias"})
	reqs, _ = p.Requests(proj)
	autorité := refuser(p, "boutique.fr")
	done, errs := p.Renew(context.Background(), reqs, false)
	if len(done) != 0 || len(errs) != 1 {
		t.Errorf("échec sans repli attendu : %v %v", done, errs)
	}
	steps, _ := p.Plan(context.Background(), proj)
	err := steps[0].Apply(context.Background())
	if partial := (*engine.PartialError)(nil); err == nil || errors.As(err, &partial) {
		t.Errorf("l'étape doit échouer sans repli : %v", err)
	}
	if len(autorité.demandes) != 2 {
		t.Errorf("une seule demande par tentative, la complète : %v", autorité.demandes)
	}
}

func TestPremiereEmissionSansLaRedirectionRefusee(t *testing.T) {
	// Un site neuf dont la redirection ne pointe pas encore ici doit tout de même
	// recevoir son certificat : l'étape aboutit en partie, et garde le
	// rechargement qui fera présenter le matériel neuf.
	p := newProvider(t)
	p.WebReload = "web:apache"
	proj := projetAvecRedirection(t)
	refuser(p, "ancien-shop.fr")
	steps, _ := p.Plan(context.Background(), proj)
	if len(steps) != 1 || steps[0].Action != engine.Create {
		t.Fatalf("une émission attendue : %v", steps)
	}
	err := steps[0].Apply(context.Background())
	if partial := (*engine.PartialError)(nil); !errors.As(err, &partial) {
		t.Fatalf("l'émission doit aboutir en partie : %v", err)
	}
	if !strings.Contains(err.Error(), "émis pour shop.fr, www.shop.fr seulement") {
		t.Errorf("message : %v", err)
	}
	cov := p.Coverage(proj)
	if !cov.Serves("shop.fr") || cov.Serves("ancien-shop.fr") || cov.Complete() {
		t.Errorf("le site couvert, la redirection en attente : %+v", cov)
	}
}

// émisPourShop pose le certificat shopfr en place pour shop.fr, puis les domaines
// déclarés ensuite par l'exploitant.
func émisPourShop(t *testing.T, p *Provider, noms []string, domaines ...model.Domain) *model.Project {
	t.Helper()
	proj := newProject(t, "shop.fr", naming.EnvProd)
	if err := p.issue(context.Background(), Request{Name: "shopfr", Domains: noms,
		Challenge: ChallengeHTTP01}); err != nil {
		t.Fatal(err)
	}
	proj.Spec.Domains = domaines
	return proj
}

func TestUnNomAjouteAuSiteEstCouvertMalgreUneRedirectionRefusee(t *testing.T) {
	// Un site a une redirection que l'autorité refuse — son DNS pointe ailleurs,
	// son domaine a expiré —, et l'exploitant ajoute un alias joignable ou change
	// de domaine principal. Le repli ne retenait que les noms déjà couverts, et
	// seulement près de l'échéance : le nouveau nom n'était jamais couvert tant
	// que la redirection restait refusée — ni à l'apply, ni au timer, ni à
	// l'échéance —, alors qu'avant que les redirections n'entrent dans la demande,
	// il l'était.
	redirection := model.Domain{Name: "ancien-shop.fr", Role: "redirect", Target: "shop.fr"}
	cas := map[string][]model.Domain{
		"alias ajouté": {{Name: "shop.fr", Role: "primary"}, {Name: "boutique.fr", Role: "alias"}, redirection},
		"nouveau principal": {{Name: "boutique.fr", Role: "primary"}, {Name: "shop.fr", Role: "alias"},
			{Name: "ancien-shop.fr", Role: "redirect", Target: "boutique.fr"}},
	}
	moments := map[string]time.Time{
		"loin de l'échéance": time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
		"près de l'échéance": time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC),
	}
	for nom, domaines := range cas {
		for moment, quand := range moments {
			for _, voie := range []string{"apply", "timer"} {
				t.Run(nom+"/"+moment+"/"+voie, func(t *testing.T) {
					p := newProvider(t)
					proj := émisPourShop(t, p, []string{"shop.fr"}, domaines...)
					autorité := refuser(p, "ancien-shop.fr")
					p.Now = func() time.Time { return quand }
					var err error
					if voie == "apply" {
						steps, perr := p.Plan(context.Background(), proj)
						if perr != nil {
							t.Fatal(perr)
						}
						err = steps[0].Apply(context.Background())
						if partial := (*engine.PartialError)(nil); !errors.As(err, &partial) {
							t.Fatalf("l'émission doit aboutir en partie : %v", err)
						}
					} else {
						reqs, _ := p.Requests(proj)
						done, errs := p.Renew(context.Background(), reqs, false)
						if len(done) != 1 || len(errs) != 1 {
							t.Fatalf("le timer doit réémettre pour le nouveau nom : %v %v", done, errs)
						}
						err = errs[0]
					}
					info, _, _ := p.Store.Read("shopfr")
					if !info.Covers([]string{"shop.fr", "boutique.fr"}) || info.Covers([]string{"ancien-shop.fr"}) {
						t.Errorf("le vhost principal doit être couvert, la redirection non : %v", info.Domains)
					}
					if !strings.Contains(err.Error(), "— ancien-shop.fr non couvert(s)") {
						t.Errorf("seule la redirection doit manquer : %v", err)
					}
					if n := len(autorité.demandes); n != 2 {
						t.Errorf("la demande complète puis le vhost principal, obtenu %d : %v", n, autorité.demandes)
					}
				})
			}
		}
	}
}

func TestLeRepliGardeLesRedirectionsDejaServies(t *testing.T) {
	// www.shop.fr redirige vers shop.fr, et le HSTS « includeSubDomains » du site
	// oblige tout navigateur qui l'a visité à le joindre en HTTPS : un repli qui
	// l'écarterait le rendrait injoignable. Près de l'échéance, le repli se fait
	// sans lui seulement quand rien d'autre n'aboutit.
	www := model.Domain{Name: "www.shop.fr", Role: "redirect", Target: "shop.fr"}
	cas := []struct {
		nom      string
		domaines []model.Domain
		refusé   string
		demandes string
	}{
		{"alias en attente", []model.Domain{{Name: "shop.fr", Role: "primary"},
			{Name: "boutique.fr", Role: "alias"}, www}, "boutique.fr",
			"shop.fr,boutique.fr,www.shop.fr | shop.fr,boutique.fr | shop.fr,www.shop.fr"},
		{"redirection en attente", []model.Domain{{Name: "shop.fr", Role: "primary"}, www,
			{Name: "ancien-shop.fr", Role: "redirect", Target: "shop.fr"}}, "ancien-shop.fr",
			"shop.fr,www.shop.fr,ancien-shop.fr | shop.fr,www.shop.fr"},
	}
	for _, c := range cas {
		t.Run(c.nom, func(t *testing.T) {
			p := newProvider(t)
			proj := émisPourShop(t, p, []string{"shop.fr", "www.shop.fr"}, c.domaines...)
			autorité := refuser(p, c.refusé)
			p.Now = func() time.Time { return time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC) }
			reqs, _ := p.Requests(proj)
			done, errs := p.Renew(context.Background(), reqs, false)
			if len(done) != 1 || len(errs) != 1 {
				t.Fatalf("renouvellement replié attendu : %v %v", done, errs)
			}
			info, _, _ := p.Store.Read("shopfr")
			if strings.Join(info.Domains, ",") != "shop.fr,www.shop.fr" {
				t.Errorf("la redirection servie doit rester couverte : %v", info.Domains)
			}
			if strings.Contains(errs[0].Error(), "n'est plus couvert") {
				t.Errorf("aucun nom servi n'est retiré : %v", errs[0])
			}
			if got := strings.Join(autorité.demandes, " | "); got != c.demandes {
				t.Errorf("paliers : %s", got)
			}
		})
	}
}

func TestLoinDeLEcheanceUnRepliNeRetireAucunNomServi(t *testing.T) {
	// Le domaine principal passe de shop.fr à boutique.fr, et shop.fr devient
	// une redirection — qu'un AAAA pointé ailleurs fait refuser par l'autorité,
	// alors que ses visiteurs en IPv4 arrivent toujours ici, en HTTPS sous HSTS.
	// Loin de l'échéance, réémettre sans lui casserait l'ancien principal des
	// semaines avant l'heure : le certificat en place attend. Près d'elle, le
	// nouveau principal passe avant un nom que l'autorité refuse.
	domaines := []model.Domain{{Name: "boutique.fr", Role: "primary"},
		{Name: "shop.fr", Role: "redirect", Target: "boutique.fr"}}
	p := newProvider(t)
	proj := émisPourShop(t, p, []string{"shop.fr"}, domaines...)
	autorité := refuser(p, "shop.fr")
	p.Now = func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) }
	steps, _ := p.Plan(context.Background(), proj)
	err := steps[0].Apply(context.Background())
	if partial := (*engine.PartialError)(nil); err == nil || errors.As(err, &partial) {
		t.Fatalf("aucun repli ne doit retirer un nom servi : %v", err)
	}
	info, _, _ := p.Store.Read("shopfr")
	if strings.Join(info.Domains, ",") != "shop.fr" {
		t.Errorf("le certificat en place doit rester : %v", info.Domains)
	}
	// Seul le refus de la demande complète s'affichait : l'exploitant qui venait
	// de changer de domaine principal ne pouvait savoir ni pourquoi le nouveau
	// n'était pas couvert, ni quand il le serait.
	attendu := "repli sur boutique.fr réservé à l'échéance, à partir du " +
		info.NotAfter.Add(-RenewBefore).Format("02/01/2006") + " : il retirerait shop.fr"
	if !strings.Contains(err.Error(), attendu) || !strings.Contains(err.Error(), "renouvellement forcé") {
		t.Errorf("l'attente doit être expliquée, avec sa date et son recours : %v", err)
	}
	if got := strings.Join(autorité.demandes, " | "); got != "boutique.fr,shop.fr" {
		t.Errorf("seule la demande complète est tentée : %s", got)
	}

	p.Now = func() time.Time { return time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC) }
	reqs, _ := p.Requests(proj)
	_, errs := p.Renew(context.Background(), reqs, false)
	if info, _, _ := p.Store.Read("shopfr"); strings.Join(info.Domains, ",") != "boutique.fr" {
		t.Errorf("près de l'échéance, le nouveau principal doit être couvert : %v", info.Domains)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "shop.fr n'est plus couvert") {
		t.Errorf("le retrait de l'ancien principal doit être annoncé : %v", errs)
	}
}

func TestUneRedirectionDepuisUneAdresseIPNeBloquePasLeCertificat(t *testing.T) {
	// La validation admet une adresse IP comme domaine, et rediriger
	// http://203.0.113.10 vers le site a du sens. Mise dans la demande, elle la
	// faisait refuser entière : le projet n'était plus appliquable, et le
	// renouvellement du parc passait dessus sans un mot.
	p := newProvider(t)
	proj := newProject(t, "shop.fr", naming.EnvProd)
	proj.Spec.Domains = append(proj.Spec.Domains,
		model.Domain{Name: "203.0.113.10", Role: "redirect", Target: "shop.fr"},
		model.Domain{Name: "ancien-shop.fr", Role: "redirect", Target: "shop.fr"})
	reqs, err := p.Requests(proj)
	if err != nil {
		t.Fatalf("une redirection depuis une IP ne doit pas invalider la demande : %v", err)
	}
	if len(reqs) != 1 || strings.Join(reqs[0].Domains, ",") != "shop.fr,ancien-shop.fr" {
		t.Errorf("l'adresse IP n'a rien à faire dans un certificat ACME : %+v", reqs)
	}
	if _, err := p.Plan(context.Background(), proj); err != nil {
		t.Errorf("le plan doit aboutir : %v", err)
	}
}

func TestUnJokerDeclareEstEssentiel(t *testing.T) {
	// Un certificat déclaré [*.shop.fr, shop.fr] pour shop.fr et www.shop.fr : le
	// joker couvre www.shop.fr sans l'écrire. Comparé nom à nom, il n'était pas
	// essentiel, et un repli réussi réémettait pour shop.fr seul.
	p := newProvider(t)
	proj := newProject(t, "shop.fr", naming.EnvProd)
	proj.Spec.Domains = append(proj.Spec.Domains, model.Domain{Name: "www.shop.fr", Role: "alias"})
	proj.Spec.Certificates = []model.Certificate{{Name: "shopfr", Challenge: "dns-01", DNSProvider: "ovh",
		Domains: []string{"*.shop.fr", "shop.fr", "ancien-shop.fr"}}}
	reqs, err := p.Requests(proj)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(reqs[0].Essential, ","); got != "*.shop.fr,shop.fr" {
		t.Errorf("noms essentiels : %s", got)
	}
	refuser(p, "ancien-shop.fr")
	if err := p.Issue(context.Background(), reqs[0]); err == nil {
		t.Fatal("la redirection refusée doit être signalée")
	}
	if info, _, _ := p.Store.Read("shopfr"); !info.Covers([]string{"shop.fr", "www.shop.fr"}) {
		t.Errorf("le repli doit garder le joker : %v", info.Domains)
	}
}

func TestUnCertificatPartageNeSeReplieJamais(t *testing.T) {
	// Un certificat partagé n'a pas de noms essentiels : ses domaines sont ceux
	// que l'administrateur a déclarés d'un seul tenant, et aucun palier ne doit
	// les réémettre à moitié.
	p := newProvider(t)
	avant := Request{Name: "@wildcard-prep", Domains: []string{"*.preprod-pp.net"},
		Challenge: ChallengeDNS01, DNSProvider: "ovh"}
	if err := p.issue(context.Background(), avant); err != nil {
		t.Fatal(err)
	}
	demande := avant
	demande.Domains = []string{"*.preprod-pp.net", "preprod-pp.net"}
	autorité := refuser(p, "preprod-pp.net")
	p.Now = func() time.Time { return time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC) }
	done, errs := p.Renew(context.Background(), []Request{demande}, false)
	if len(done) != 0 || len(errs) != 1 {
		t.Errorf("échec sans repli attendu : %v %v", done, errs)
	}
	if len(autorité.demandes) != 1 {
		t.Errorf("une seule demande, la déclarée : %v", autorité.demandes)
	}
}

func TestCertificatPartageExpireNEstPlusDisponible(t *testing.T) {
	// Un wildcard expiré n'est plus présenté par les vhosts (Coverage) : l'état
	// ne doit pas le dire disponible, sans quoi « status » annoncerait saine une
	// préproduction repassée en HTTP seul.
	p := newProvider(t)
	prep := newProject(t, "shop.fr", naming.EnvPrep)
	req, _ := p.SharedRequest("@wildcard-prep")
	if err := p.issue(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if ref, ok := p.CertificateFor(prep); ref != "@wildcard-prep" || !ok {
		t.Fatalf("wildcard frais : %q %v", ref, ok)
	}
	p.Now = func() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) }
	if _, ok := p.CertificateFor(prep); ok {
		t.Error("un wildcard expiré ne doit pas être annoncé disponible")
	}
	if cov := p.Coverage(prep); len(cov.Present) != 0 {
		t.Errorf("un wildcard expiré ne couvre rien : %+v", cov)
	}
}

func TestPrepWithCustomHostFallsBackToOwnCertificate(t *testing.T) {
	// Un host de préproduction hors convention n'est pas couvert par le wildcard :
	// il lui faut son propre certificat, sinon le navigateur alerterait.
	p := newProvider(t)
	prep := newProject(t, "shop.fr", naming.EnvPrep)
	prep.Spec.Domains = []model.Domain{{Name: "staging.shop.fr", Role: "primary"}}
	if ref := p.SharedRef(prep); ref != "" {
		t.Errorf("le wildinard ne couvre pas staging.shop.fr, or la référence est %q", ref)
	}
	reqs, err := p.Requests(prep)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 1 || reqs[0].Domains[0] != "staging.shop.fr" {
		t.Errorf("demande propre attendue : %+v", reqs)
	}
}

func TestSharedWildcardRequiresDNS01(t *testing.T) {
	p := newProvider(t)
	req, ok := p.SharedRequest("@wildcard-prep")
	if !ok {
		t.Fatal("le certificat partagé par défaut doit être défini")
	}
	if req.Challenge != ChallengeDNS01 {
		t.Errorf("un wildcard impose dns-01, obtenu %q", req.Challenge)
	}
	if req.DNSProvider != "ovh" {
		t.Errorf("provider DNS : %q", req.DNSProvider)
	}
	if err := req.Validate(); err != nil {
		t.Errorf("la demande partagée doit être valide : %v", err)
	}
}

func TestCertificateStepLifecycle(t *testing.T) {
	p := newProvider(t)
	proj := newProject(t, "shop.fr", naming.EnvProd)
	ctx := context.Background()

	// 1. Absent : émission, non critique.
	steps, err := p.Plan(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Action != engine.Create {
		t.Fatalf("plan initial : %+v", steps)
	}
	if steps[0].Critical {
		t.Error("l'émission doit être non critique : un site en HTTP vaut mieux qu'un site absent")
	}
	if err := steps[0].Apply(ctx); err != nil {
		t.Fatal(err)
	}

	// 2. Présent et frais : rien à faire.
	steps, _ = p.Plan(ctx, proj)
	if steps[0].Mutates() {
		t.Errorf("un certificat frais ne doit pas être réémis : %s", steps[0].Summary)
	}

	// 3. Domaines élargis : réémission.
	proj.Spec.Domains = append(proj.Spec.Domains, model.Domain{Name: "www.shop.fr", Role: "alias"})
	steps, _ = p.Plan(ctx, proj)
	if !steps[0].Mutates() || !strings.Contains(steps[0].Summary, "domaines à couvrir") {
		t.Errorf("l'ajout d'un domaine doit déclencher une réémission : %s", steps[0].Summary)
	}
}

func TestRenewalThreshold(t *testing.T) {
	p := newProvider(t)
	proj := newProject(t, "shop.fr", naming.EnvProd)
	reqs, _ := p.Requests(proj)

	// Un certificat de 10 jours doit être renouvelé.
	p.Issuer = SelfSigned{Validity: 10 * 24 * time.Hour, Now: p.Now}
	if err := p.issue(context.Background(), reqs[0]); err != nil {
		t.Fatal(err)
	}
	done, errs := p.Renew(context.Background(), reqs, false)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if len(done) != 1 {
		t.Errorf("le certificat proche de l'échéance doit être renouvelé : %v", done)
	}

	// Après renouvellement sur 90 jours, plus rien à faire.
	p.Issuer = SelfSigned{Validity: 90 * 24 * time.Hour, Now: p.Now}
	if _, errs := p.Renew(context.Background(), reqs, false); len(errs) != 0 {
		t.Fatal(errs)
	}
	done, _ = p.Renew(context.Background(), reqs, false)
	if len(done) != 0 {
		t.Errorf("aucun renouvellement attendu : %v", done)
	}
	// Sauf si on le force.
	done, _ = p.Renew(context.Background(), reqs, true)
	if len(done) != 1 {
		t.Errorf("--force doit renouveler quand même : %v", done)
	}
}

// failingIssuer simule une autorité qui refuse systématiquement.
type failingIssuer struct{ calls int }

func (f *failingIssuer) Name() string { return "en échec" }
func (f *failingIssuer) Issue(context.Context, Request) (Material, error) {
	f.calls++
	return Material{}, errRefused
}
func (f *failingIssuer) Revoke(context.Context, Request, []byte) error { return nil }

var errRefused = &refusedError{}

type refusedError struct{}

func (e *refusedError) Error() string { return "challenge http-01 refusé" }

func TestFailuresEventuallySuspendAttempts(t *testing.T) {
	// Les autorités limitent les échecs de validation : insister ferait perdre le
	// droit d'émettre pour le domaine.
	p := newProvider(t)
	issuer := &failingIssuer{}
	p.Issuer = issuer
	proj := newProject(t, "shop.fr", naming.EnvProd)
	reqs, _ := p.Requests(proj)
	ctx := context.Background()

	var lastErr error
	for i := 0; i < store.ACMEFailureThreshold; i++ {
		lastErr = p.issue(ctx, reqs[0])
		if lastErr == nil {
			t.Fatal("l'émission devait échouer")
		}
	}
	if !strings.Contains(lastErr.Error(), "suspendues") {
		t.Errorf("le seuil doit être annoncé : %v", lastErr)
	}
	// La tentative suivante n'atteint plus l'autorité.
	before := issuer.calls
	err := p.issue(ctx, reqs[0])
	if err == nil || !strings.Contains(err.Error(), "suspendues") {
		t.Errorf("les tentatives doivent être bloquées : %v", err)
	}
	if issuer.calls != before {
		t.Error("aucune requête ne doit plus être envoyée à l'autorité")
	}
	// Le message doit orienter vers la cause probable.
	if !strings.Contains(err.Error(), "port 80") {
		t.Errorf("le message doit être actionnable : %v", err)
	}
}

func TestSelfSignedChallengeBypassesACME(t *testing.T) {
	p := newProvider(t)
	issuer := &failingIssuer{}
	p.Issuer = issuer
	proj := newProject(t, "shop.fr", naming.EnvProd)
	proj.Spec.Certificates = []model.Certificate{{
		Name: "shopfr", Domains: []string{"shop.fr"}, Challenge: "selfsigned",
	}}
	reqs, err := p.Requests(proj)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.issue(context.Background(), reqs[0]); err != nil {
		t.Fatalf("l'émetteur de repli doit être employé : %v", err)
	}
	if issuer.calls != 0 {
		t.Error("un certificat auto-signé ne doit pas solliciter l'autorité")
	}
}

func TestCertificateForReportsAvailability(t *testing.T) {
	p := newProvider(t)
	proj := newProject(t, "shop.fr", naming.EnvProd)
	ref, available := p.CertificateFor(proj)
	if ref != "shopfr" || available {
		t.Errorf("avant émission : %q, %v", ref, available)
	}
	reqs, _ := p.Requests(proj)
	if err := p.issue(context.Background(), reqs[0]); err != nil {
		t.Fatal(err)
	}
	ref, available = p.CertificateFor(proj)
	if ref != "shopfr" || !available {
		t.Errorf("après émission : %q, %v", ref, available)
	}
}

func TestDeleteRefusesReferencedCertificate(t *testing.T) {
	p := newProvider(t)
	prep := newProject(t, "shop.fr", naming.EnvPrep)
	req, _ := p.SharedRequest("@wildcard-prep")
	if err := p.issue(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// Le wildcard est référencé par la préproduction : le supprimer casserait son
	// vhost.
	err := p.Delete("@wildcard-prep", []*model.Project{prep})
	if err == nil || !strings.Contains(err.Error(), "shopfrprep") {
		t.Fatalf("la suppression doit être refusée en nommant les projets : %v", err)
	}
	// Sans référence, elle est acceptée.
	if err := p.Delete("@wildcard-prep", nil); err != nil {
		t.Fatalf("suppression refusée à tort : %v", err)
	}
}

func TestWarningsUseEarlierThresholdForShared(t *testing.T) {
	p := newProvider(t)
	// Un partagé à 25 jours de l'échéance : sous le seuil de renouvellement (30)
	// mais au-dessus du seuil d'alerte spécifique (21).
	p.Issuer = SelfSigned{Validity: 25 * 24 * time.Hour, Now: p.Now}
	req, _ := p.SharedRequest("@wildcard-prep")
	if err := p.issue(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	warnings, err := p.Warnings()
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("à 25 jours, aucun avertissement de partagé attendu : %v", warnings)
	}
	// À 15 jours, l'alerte doit tomber et signaler qu'il est partagé.
	p.Issuer = SelfSigned{Validity: 15 * 24 * time.Hour, Now: p.Now}
	if err := p.issue(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	warnings, _ = p.Warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "partagé") {
		t.Errorf("avertissements : %v", warnings)
	}
}

func TestRenewDoesNotCreateMissingCertificates(t *testing.T) {
	// Le timer de renouvellement ne doit émettre que ce qui existe déjà : sinon il
	// créerait des certificats pour des déclarations partagées inutilisées, en
	// consommant les quotas de l'autorité.
	p := newProvider(t)
	req, _ := p.SharedRequest("@wildcard-prep")
	done, errs := p.Renew(context.Background(), []Request{req}, false)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if len(done) != 0 {
		t.Errorf("aucune émission attendue : %v", done)
	}
	if _, ok, _ := p.Store.Read(req.Name); ok {
		t.Error("aucun certificat ne doit avoir été créé")
	}
	// Avec --force, l'émission est explicitement demandée.
	done, errs = p.Renew(context.Background(), []Request{req}, true)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if len(done) != 1 {
		t.Errorf("--force doit émettre : %v", done)
	}
}

func TestUniteDeRenouvellementModifieeALaMain(t *testing.T) {
	// Le timer de renouvellement est la seule chose qui renouvelle les
	// certificats du parc : une périodicité changée à la main ne doit pas
	// disparaître sans un mot (§6.4).
	p := newProvider(t)
	tmpl := template.New(p.Paths.TemplatesDir())
	ctx := context.Background()
	steps, err := p.RenewUnitPlan(tmpl, "/usr/bin/ogs", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if err := s.Apply(ctx); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(p.Paths.SystemdDir(), "ogs-renew.timer")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("X-Ajout-Manuel=vrai\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	steps, err = p.RenewUnitPlan(tmpl, "/usr/bin/ogs", "")
	if err != nil {
		t.Fatal(err)
	}
	var refus *engine.Step
	for _, s := range steps {
		if strings.Contains(s.Summary, "modifié hors ogs") {
			refus = s
		}
	}
	if refus == nil {
		t.Fatalf("la modification manuelle doit être signalée :\n%v", steps)
	}
	if err := refus.Apply(ctx); err == nil {
		t.Error("l'écrasement doit être refusé sans --force")
	}
	if intact, _ := os.ReadFile(path); !strings.Contains(string(intact), "X-Ajout-Manuel") {
		t.Error("le refus ne doit rien écrire")
	}

	// --force : écrasement, précédé de la sauvegarde.
	p.Force = true
	steps, err = p.RenewUnitPlan(tmpl, "/usr/bin/ogs", "")
	if err != nil {
		t.Fatal(err)
	}
	var forcée *engine.Step
	for _, s := range steps {
		if strings.Contains(s.Summary, "écrasement forcé") {
			forcée = s
		}
	}
	if forcée == nil {
		t.Fatalf("le plan doit annoncer l'écrasement forcé :\n%v", steps)
	}
	if err := forcée.Apply(ctx); err != nil {
		t.Fatalf("l'écrasement forcé doit réussir : %v", err)
	}
	if after, _ := os.ReadFile(path); strings.Contains(string(after), "X-Ajout-Manuel") {
		t.Error("l'unité doit avoir été réécrite")
	}
	if restes, _ := filepath.Glob(path + ".ogs-bak-*"); len(restes) != 0 {
		t.Errorf("aucune sauvegarde ne doit rester dans %s : %v", filepath.Dir(path), restes)
	}
	baks, _ := filepath.Glob(filepath.Join(p.Paths.BackupsDir(), filepath.Base(path)+".ogs-bak-*"))
	if len(baks) != 1 {
		t.Fatalf("une sauvegarde attendue dans %s, %d trouvée(s)", p.Paths.BackupsDir(), len(baks))
	}
	if saved, _ := os.ReadFile(baks[0]); !strings.Contains(string(saved), "X-Ajout-Manuel") {
		t.Error("la sauvegarde doit contenir l'original")
	}
}

func TestCompensationRendLUniteDeRenouvellementPrecedente(t *testing.T) {
	// La compensation supprimait le fichier : sur une mise à jour, le rollback
	// laissait donc le parc sans rien pour renouveler ses certificats.
	p := newProvider(t)
	tmpl := template.New(p.Paths.TemplatesDir())
	ctx := context.Background()
	steps, err := p.RenewUnitPlan(tmpl, "/usr/bin/ogs", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if err := s.Apply(ctx); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(p.Paths.SystemdDir(), "ogs-renew.timer")
	avant, _ := os.ReadFile(path)

	steps, err = p.RenewUnitPlan(tmpl, "/usr/bin/ogs", "*-*-* 04:00:00")
	if err != nil {
		t.Fatal(err)
	}
	var maj *engine.Step
	for _, s := range steps {
		if s.Action == engine.Update && strings.Contains(s.Summary, "timer") {
			maj = s
		}
	}
	if maj == nil {
		t.Fatalf("une mise à jour du timer était attendue :\n%v", steps)
	}
	if err := maj.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if err := maj.Compensate(ctx); err != nil {
		t.Fatal(err)
	}
	après, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("la compensation ne doit pas supprimer un timer en place : %v", err)
	}
	if string(après) != string(avant) {
		t.Error("la compensation doit rendre le timer précédent")
	}
}

// Un vhost qui désigne un certificat de portée serveur par « @nom » le sert, et
// le plan du projet l'émet ; supprimer ce certificat est refusé (D102).
func TestVhostDésigneUnCertificatPartagé(t *testing.T) {
	p := newProvider(t)
	p.Cfg.Certificates["commun"] = config.SharedCert{Domains: []string{"*.groupe.fr"}, Challenge: "selfsigned"}
	proj := newProject(t, "shop.fr", naming.EnvProd)
	proj.Spec.Domains = append(proj.Spec.Domains, model.Domain{Name: "blog.groupe.fr", Role: "alias"})
	principal := model.VHost{Name: "shopfr", Domains: []string{"shop.fr"}}
	blog := model.VHost{Name: "blog", Domains: []string{"blog.groupe.fr"},
		TLS: &model.TLS{Enabled: true, Certificate: "@commun"}}
	proj.Spec.Web.VHosts = []model.VHost{principal, blog}

	if cov := p.VHostCoverage(proj, blog); cov.Ref != "@commun" {
		t.Errorf("le vhost du blog doit servir @commun : %q", cov.Ref)
	}
	if cov := p.VHostCoverage(proj, principal); cov.Ref != "shopfr" {
		t.Errorf("le vhost principal garde le certificat du projet : %q", cov.Ref)
	}
	steps, err := p.Plan(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	var res []string
	for _, s := range steps {
		res = append(res, s.Resource)
	}
	if !slices.Contains(res, "tls/certificate/@commun") {
		t.Errorf("le plan doit émettre le certificat partagé désigné : %v", res)
	}
	for _, s := range steps {
		if err := s.Apply(context.Background()); err != nil {
			t.Fatalf("%s : %v", s.Resource, err)
		}
	}
	if err := p.Delete("@commun", []*model.Project{proj}); err == nil {
		t.Error("supprimer un certificat qu'un vhost sert doit être refusé")
	}
	if problems := p.ReferenceProblems(proj); len(problems) != 0 {
		t.Errorf("aucune référence sans objet : %v", problems)
	}
}

// La préproduction recopie spec.web.tls de la production (site clone) : la
// référence au certificat de la production n'y désigne rien, le wildcard reste
// servi, et rien n'est signalé — un code 6 à chaque apply serait faux.
func TestPréproductionIgnoreLaRéférenceDeLaProduction(t *testing.T) {
	p := newProvider(t)
	prep := newProject(t, "shop.fr", naming.EnvPrep)
	prep.Spec.Web.TLS = &model.TLS{Enabled: true, Certificate: "shopfr"}
	vh := model.VHost{Name: "shopfrprep", Domains: []string{prep.Spec.Domains[0].Name}}
	if cov := p.VHostCoverage(prep, vh); cov.Ref != "@wildcard-prep" {
		t.Errorf("la préproduction doit servir le wildcard : %q", cov.Ref)
	}
	if problems := p.ReferenceProblems(prep); len(problems) != 0 {
		t.Errorf("rien à signaler en préproduction : %v", problems)
	}
	steps, err := p.Plan(context.Background(), prep)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Errorf("seul le wildcard est planifié : %d étape(s)", len(steps))
	}
}

// Relecture de D102 : la préproduction hérite (site clone) de spec.web.tls de la
// production, qui désigne un partagé ; il ne la couvre pas, ou pas mieux que le
// wildcard. Le wildcard reste servi, rien n'est signalé, et le partagé n'est pas
// émis avec la préproduction.
func TestPréproductionGardeLeWildcardMalgréUnPartagéHérité(t *testing.T) {
	p := newProvider(t)
	p.Cfg.Certificates["commun"] = config.SharedCert{Domains: []string{"*.groupe.fr"}, Challenge: "selfsigned"}
	prep := newProject(t, "shop.fr", naming.EnvPrep)
	prep.Spec.Web.TLS = &model.TLS{Enabled: true, Certificate: "@commun"}
	vh := model.VHost{Name: "shopfrprep", Domains: []string{prep.Spec.Domains[0].Name}}
	if cov := p.VHostCoverage(prep, vh); cov.Ref != "@wildcard-prep" {
		t.Errorf("la préproduction doit servir le wildcard : %q", cov.Ref)
	}
	if problems := p.ReferenceProblems(prep); len(problems) != 0 {
		t.Errorf("rien à signaler en préproduction : %v", problems)
	}
	steps, err := p.Plan(context.Background(), prep)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if strings.Contains(s.Resource, "@commun") {
			t.Errorf("le partagé hérité ne doit pas être émis avec la préproduction : %s", s.Resource)
		}
	}
}

// Une référence dont la demande ne couvre aucun nom du vhost ne lui retire pas
// le certificat par défaut, et elle est signalée.
func TestUneRéférenceQuiNeCouvreRienEstSignalée(t *testing.T) {
	p := newProvider(t)
	proj := newProject(t, "shop.fr", naming.EnvProd)
	proj.Spec.Certificates = []model.Certificate{
		{Name: "principal", Domains: []string{"shop.fr"}, Challenge: "selfsigned"},
		{Name: "autre", Domains: []string{"autre.shop.fr"}, Challenge: "selfsigned"},
	}
	proj.Spec.Web.TLS = &model.TLS{Enabled: true, Certificate: "autre"}
	vh := model.VHost{Name: "shopfr", Domains: []string{"shop.fr"}}
	if cov := p.VHostCoverage(proj, vh); cov.Ref != "principal" {
		t.Errorf("le vhost doit garder le certificat qui le couvre : %q", cov.Ref)
	}
	if problems := p.ReferenceProblems(proj); len(problems) != 1 || !strings.Contains(problems[0], "ne couvre aucun") {
		t.Errorf("la référence sans objet doit être signalée : %v", problems)
	}
}

// Un certificat désigné, pas encore émis, laisse servir le certificat présent
// qui couvre le vhost, et se dit en attente (D79) ; émis, il est servi.
func TestUnCertificatDésignéEnAttenteLaisseServirLePrésent(t *testing.T) {
	p := newProvider(t)
	p.Cfg.Certificates["commun"] = config.SharedCert{Domains: []string{"*.shop.fr", "shop.fr"}, Challenge: "selfsigned"}
	proj := newProject(t, "shop.fr", naming.EnvProd)
	vh := model.VHost{Name: "shopfr", Domains: []string{"shop.fr"}}
	reqs, err := p.Requests(proj)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.issue(context.Background(), reqs[0]); err != nil {
		t.Fatal(err)
	}
	vh.TLS = &model.TLS{Enabled: true, Certificate: "@commun"}
	cov := p.VHostCoverage(proj, vh)
	if cov.Ref != proj.ID() || cov.Pending != "@commun" || !cov.Serves("shop.fr") {
		t.Fatalf("le présent doit rester servi, le désigné en attente : %+v", cov)
	}
	shared, _ := p.SharedRequest("@commun")
	if err := p.issue(context.Background(), shared); err != nil {
		t.Fatal(err)
	}
	if cov := p.VHostCoverage(proj, vh); cov.Ref != "@commun" || cov.Pending != "" {
		t.Errorf("émis, le désigné doit être servi : %+v", cov)
	}
}
