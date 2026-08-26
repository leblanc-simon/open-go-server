package tls

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/store"
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
	// Une redirection n'a pas à être chiffrée : l'inclure élargirait le certificat
	// et ferait échouer l'émission si son DNS ne pointe pas encore ici.
	if strings.Join(reqs[0].Domains, ",") != "shop.fr,www.shop.fr" {
		t.Errorf("domaines couverts : %v", reqs[0].Domains)
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
