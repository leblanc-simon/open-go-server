package tls

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-acme/lego/v4/challenge"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
)

func écrireJeu(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestJeuxDIdentifiantsDNS(t *testing.T) {
	dir := t.TempDir()
	// L'ancien fichier, sans provider ni zones, reste le jeu par défaut d'OVH.
	écrireJeu(t, dir, "ovh", "# en-tête\nendpoint: ovh-eu\napplicationKey: ak\nconsumerKey: 12345\n", 0o600)
	écrireJeu(t, dir, "client-a", "provider: ovh\nzones: [Client-A.fr., client-a.com]\nclientId: ci\n", 0o600)
	écrireJeu(t, dir, "ouvert", "zones: [ouvert.fr]\n", 0o644)
	écrireJeu(t, dir, "autre", "provider: cloudflare\n", 0o600)

	sets, errs := LoadDNSCredentials(dir)
	if len(sets) != 2 || sets[0].Name != "client-a" || sets[1].Name != "ovh" {
		t.Fatalf("jeux lus : %+v", sets)
	}
	if strings.Join(sets[0].Zones, ",") != "client-a.com,client-a.fr" {
		t.Errorf("zones normalisées : %v", sets[0].Zones)
	}
	if sets[1].Provider != DNSProviderOVH || len(sets[1].Zones) != 0 || sets[1].Values["consumerKey"] != "12345" {
		t.Errorf("ancien fichier : %+v", sets[1])
	}
	// Un fichier lisible de tous, ou d'un provider non géré, est écarté et nommé,
	// sans priver les autres.
	joined := errors.Join(errs...).Error()
	if len(errs) != 2 || !strings.Contains(joined, "lisible au-delà") || !strings.Contains(joined, "cloudflare") {
		t.Errorf("erreurs : %v", errs)
	}
}

func TestNomDeJeuDIdentifiantsDNS(t *testing.T) {
	for _, bad := range []string{"", "../x", "a/b", ".cache", "-x", "Ovh"} {
		if CheckDNSCredentialsName(bad) == nil {
			t.Errorf("%q devrait être refusé", bad)
		}
	}
	for _, good := range []string{"ovh", "client-a", "client_a.2"} {
		if err := CheckDNSCredentialsName(good); err != nil {
			t.Errorf("%q : %v", good, err)
		}
	}
}

func jeux() []DNSCredentials {
	return []DNSCredentials{
		{Name: "ovh", Provider: DNSProviderOVH},
		{Name: "client-a", Provider: DNSProviderOVH, Zones: []string{"client-a.fr"}},
		{Name: "boutique", Provider: DNSProviderOVH, Zones: []string{"shop.client-a.fr"}},
	}
}

func TestChoixDuJeuParLaZone(t *testing.T) {
	cas := []struct {
		domain, explicit, want, err string
	}{
		{domain: "client-a.fr", want: "client-a"},
		{domain: "*.client-a.fr", want: "client-a"},
		{domain: "WWW.client-a.fr", want: "client-a"},
		// La zone la plus longue l'emporte : un jeton restreint à une sous-zone.
		{domain: "x.shop.client-a.fr", want: "boutique"},
		// Hors de toute zone : le jeu sans zone, seul de son provider.
		{domain: "preprod-pp.net", want: "ovh"},
		// Un nom qui ne fait que finir comme une zone n'en relève pas.
		{domain: "autreclient-a.fr", want: "ovh"},
		{domain: "x.client-a.fr", explicit: "ovh", want: "ovh"},
		{domain: "autre.fr", explicit: "client-a", err: "ne couvre pas"},
		{domain: "autre.fr", explicit: "inconnu", err: "absent"},
	}
	for _, c := range cas {
		got, err := ResolveDNSCredentials(jeux(), c.domain, c.explicit, "")
		switch {
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s (%s) : erreur %v, attendu « %s »", c.domain, c.explicit, err, c.err)
		case c.err == "" && (err != nil || got.Name != c.want):
			t.Errorf("%s (%s) : %q, %v — attendu %q", c.domain, c.explicit, got.Name, err, c.want)
		}
	}
	// Deux jeux sans zone, ou deux jeux d'une même zone : aucun n'est plus
	// légitime que l'autre.
	sets := append(jeux(), DNSCredentials{Name: "ovh-2", Provider: DNSProviderOVH})
	if _, err := ResolveDNSCredentials(sets, "autre.fr", "", ""); err == nil ||
		!strings.Contains(err.Error(), "plusieurs jeux") {
		t.Errorf("deux jeux sans zone : %v", err)
	}
	sets = append(jeux(), DNSCredentials{Name: "doublon", Provider: DNSProviderOVH, Zones: []string{"client-a.fr"}})
	if _, err := ResolveDNSCredentials(sets, "client-a.fr", "", ""); err == nil ||
		!strings.Contains(err.Error(), "revendiquée") {
		t.Errorf("zone revendiquée deux fois : %v", err)
	}
	if got := DNSZoneConflicts(sets); len(got) != 1 || !strings.Contains(got[0], "client-a.fr") {
		t.Errorf("conflits : %v", got)
	}
	if _, err := ResolveDNSCredentials(jeux()[1:], "autre.fr", "", ""); err == nil ||
		!strings.Contains(err.Error(), "--zone") {
		t.Errorf("aucun jeu : %v", err)
	}
}

func TestDNS01AutomatiqueSeulementSurDesZonesDeclarees(t *testing.T) {
	if _, ok := AutoDNS(jeux(), []string{"client-a.fr", "*.client-a.fr", "x.shop.client-a.fr"}); !ok {
		t.Error("tous les noms relèvent d'une zone déclarée")
	}
	// Une commande ACME ne choisit pas son challenge nom à nom : un seul nom hors
	// zone laisse la demande en HTTP-01.
	if _, ok := AutoDNS(jeux(), []string{"client-a.fr", "ailleurs.fr"}); ok {
		t.Error("un nom hors zone doit laisser la demande en http-01")
	}
	// Le jeu sans zone du wildcard de préproduction ne fait pas passer le parc à
	// DNS-01 : son compte ne porte pas les zones des clients.
	if _, ok := AutoDNS(jeux()[:1], []string{"shop.fr"}); ok {
		t.Error("un jeu sans zone ne suffit jamais")
	}
}

type faussePresentation struct {
	nom    string
	appels *[]string
}

func (f faussePresentation) Present(domain, _, _ string) error {
	*f.appels = append(*f.appels, f.nom+":"+domain)
	return nil
}
func (f faussePresentation) CleanUp(string, string, string) error { return nil }

func TestChallengesAiguillesVersLeJeuDeLeurZone(t *testing.T) {
	var appels []string
	construits := 0
	build := func(s DNSCredentials) (challenge.Provider, error) {
		construits++
		return faussePresentation{nom: s.Name, appels: &appels}, nil
	}
	req := Request{Name: "multi", Challenge: ChallengeDNS01, DNSProvider: DNSProviderOVH,
		Domains: []string{"client-a.fr", "*.client-a.fr", "preprod-pp.net"}}
	prov, err := routeDNS(jeux(), req, build)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"client-a.fr", "preprod-pp.net"} {
		if err := prov.Present(d, "t", "k"); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(appels, ",") != "client-a:client-a.fr,ovh:preprod-pp.net" || construits != 2 {
		t.Errorf("aiguillage : %v (%d construits)", appels, construits)
	}
	if err := prov.Present("inattendu.fr", "t", "k"); err == nil {
		t.Error("un nom hors de la demande ne doit être présenté à aucun jeu")
	}
	// Un seul jeu : son provider tel quel.
	req.Domains = []string{"client-a.fr"}
	if prov, err = routeDNS(jeux(), req, build); err != nil {
		t.Fatal(err)
	}
	if _, ok := prov.(faussePresentation); !ok {
		t.Errorf("un seul jeu doit rendre son provider : %T", prov)
	}
	// Un nom sans jeu fait échouer la demande avant tout appel à l'autorité.
	req.Domains = []string{"client-a.fr", "ailleurs.fr"}
	if _, err := routeDNS(jeux()[1:], req, build); err == nil {
		t.Error("un nom sans jeu doit être refusé")
	}
}

func TestLaZoneDeclareeFaitPasserLeCertificatADNS01(t *testing.T) {
	p := newProvider(t)
	p.DNSSets = func() []DNSCredentials { return jeux() }
	proj := newProject(t, "client-a.fr", naming.EnvProd)
	proj.Spec.Domains = append(proj.Spec.Domains, model.Domain{Name: "www.client-a.fr", Role: "alias"})
	reqs, err := p.Requests(proj)
	if err != nil {
		t.Fatal(err)
	}
	if reqs[0].Challenge != ChallengeDNS01 || !reqs[0].AutoDNS || reqs[0].DNSCredentials != "" {
		t.Fatalf("demande : %+v", reqs[0])
	}
	// Le repli HTTP-01 exige un vhost chargé : l'étape vide les rechargements.
	step, err := p.certificateStep(reqs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !step.FlushReloads || !strings.Contains(step.Summary, "repli http-01") {
		t.Errorf("étape : %q, vidage %v", step.Summary, step.FlushReloads)
	}

	// Un challenge déclaré est respecté ; des identifiants nommés imposent DNS-01,
	// sans repli.
	proj.Spec.Certificates = []model.Certificate{{Challenge: "http-01"}}
	if reqs, err = p.Requests(proj); err != nil || reqs[0].Challenge != ChallengeHTTP01 || reqs[0].AutoDNS {
		t.Errorf("http-01 déclaré : %+v, %v", reqs, err)
	}
	proj.Spec.Certificates = []model.Certificate{{DNSCredentials: "ovh"}}
	if reqs, err = p.Requests(proj); err != nil || reqs[0].Challenge != ChallengeDNS01 || reqs[0].AutoDNS {
		t.Errorf("identifiants nommés : %+v, %v", reqs, err)
	}
	proj.Spec.Certificates = []model.Certificate{{Challenge: "http-01", DNSCredentials: "ovh"}}
	if _, err = p.Requests(proj); err == nil {
		t.Error("« dnsCredentials » avec http-01 doit être refusé")
	}
	proj.Spec.Certificates = []model.Certificate{{DNSCredentials: "../master"}}
	if _, err = p.Requests(proj); err == nil {
		t.Error("un nom de jeu qui sort du répertoire doit être refusé")
	}

	// Un nom hors zone : la demande reste en HTTP-01.
	proj.Spec.Certificates = nil
	proj.Spec.Domains = append(proj.Spec.Domains, model.Domain{Name: "ailleurs.fr", Role: "alias"})
	if reqs, err = p.Requests(proj); err != nil || reqs[0].Challenge != ChallengeHTTP01 {
		t.Errorf("nom hors zone : %+v, %v", reqs, err)
	}
}

// autoritéSansDNS refuse le challenge DNS-01 et, au choix, le HTTP-01.
type autoritéSansDNS struct {
	SelfSigned
	httpRefusé bool
	challenges []Challenge
}

func (a *autoritéSansDNS) Issue(ctx context.Context, req Request) (Material, error) {
	a.challenges = append(a.challenges, req.Challenge)
	if req.Challenge == ChallengeDNS01 || a.httpRefusé {
		return Material{}, errors.New("challenge " + string(req.Challenge) + " refusé")
	}
	return a.SelfSigned.Issue(ctx, req)
}

func TestUnDNS01AutomatiqueSeReplieSurHTTP01(t *testing.T) {
	p := newProvider(t)
	autorité := &autoritéSansDNS{SelfSigned: SelfSigned{}}
	p.Issuer = autorité
	p.DNSSets = func() []DNSCredentials { return jeux() }
	proj := newProject(t, "client-a.fr", naming.EnvProd)
	reqs, err := p.Requests(proj)
	if err != nil {
		t.Fatal(err)
	}
	err = p.Issue(context.Background(), reqs[0])
	fallback := (*DNSFallbackError)(nil)
	if !errors.As(err, &fallback) || !Written(err) {
		t.Fatalf("repli http-01 attendu : %v", err)
	}
	if _, ok, _ := p.Store.Read(reqs[0].Name); !ok {
		t.Error("le certificat émis par http-01 doit être écrit")
	}
	if partial := (*engine.PartialError)(nil); !errors.As(stepError(err), &partial) {
		t.Errorf("le repli est un succès partiel : %v", stepError(err))
	}
	if len(autorité.challenges) != 2 || autorité.challenges[0] != ChallengeDNS01 ||
		autorité.challenges[1] != ChallengeHTTP01 {
		t.Errorf("challenges tentés : %v", autorité.challenges)
	}
	// Les deux en échec : l'erreur nomme les deux, et rien n'est écrit de plus.
	autorité.httpRefusé = true
	p.Store.Remove(reqs[0].Name)
	err = p.Issue(context.Background(), reqs[0])
	if err == nil || Written(err) || !strings.Contains(err.Error(), "dns-01") ||
		!strings.Contains(err.Error(), "repli http-01") {
		t.Errorf("double échec : %v", err)
	}
	// Un DNS-01 déclaré n'a pas de repli : l'exploitant l'a choisi.
	autorité.challenges = nil
	declared := reqs[0]
	declared.AutoDNS = false
	_ = p.Issue(context.Background(), declared)
	for _, c := range autorité.challenges {
		if c != ChallengeDNS01 {
			t.Errorf("un dns-01 déclaré ne doit pas se replier : %v", autorité.challenges)
		}
	}
}
