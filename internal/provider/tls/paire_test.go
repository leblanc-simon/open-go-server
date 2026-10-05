package tls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	cryptotls "crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/naming"
)

// R1-73 : clé et certificat étaient écrits par quatre renommages indépendants.
// Deux écritures concurrentes — l'apply et le timer, deux « cert issue » —
// entrelaçaient leurs fichiers : la clé de l'une avec le certificat de l'autre,
// que le serveur web refuse au chargement (« key values mismatch »), ce qui
// l'arrête entier. L'écriture se fait sous verrou (D110).
func TestDeuxEcrituresConcurrentesLaissentUnePaireAppariee(t *testing.T) {
	s := newStore(t)
	issuer := SelfSigned{Validity: 90 * 24 * time.Hour}
	req := Request{Name: "shopfr", Domains: []string{"shop.fr"}, Challenge: ChallengeSelfSigned}
	for tour := 0; tour < 12; tour++ {
		var wg sync.WaitGroup
		for range 4 {
			m, err := issuer.Issue(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := s.Write("shopfr", m); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		full, _ := os.ReadFile(s.Path("shopfr", FileFullchain))
		key, _ := os.ReadFile(s.Path("shopfr", FileKey))
		if _, err := cryptotls.X509KeyPair(full, key); err != nil {
			t.Fatalf("tour %d : paire dépareillée après des écritures concurrentes : %v", tour, err)
		}
	}
}

// Une paire dépareillée — clé recopiée à la main — ne se lit pas comme
// présente : un vhost rendu sur elle ferait refuser la configuration entière au
// moteur. Elle est tenue pour absente du rendu, et réémise AVANT les vhosts
// (PlanPairs) : sous nginx, l'écriture du premier vhost échouait sinon sur le
// second qui la désignait encore (B1 de la relecture de D110).
func TestUnePaireDepareilleeEstTenuePourAbsenteEtReemise(t *testing.T) {
	p := newProvider(t)
	p.WebReload = "web:nginx"
	proj := newProject(t, "shop.fr", naming.EnvProd)
	reqs, err := p.Requests(proj)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.issue(context.Background(), reqs[0]); err != nil {
		t.Fatal(err)
	}
	// La clé d'une autre émission remplace celle du certificat.
	autre, err := SelfSigned{}.Issue(context.Background(), reqs[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Store.Path("shopfr", FileKey), autre.PrivateKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := p.Store.Read("shopfr"); ok {
		t.Fatal("une paire dépareillée ne doit pas se lire comme présente")
	}
	if cov := p.Coverage(proj); len(cov.Present) != 0 {
		t.Errorf("le rendu ne doit pas servir une paire dépareillée : %+v", cov)
	}
	// Liste, alertes : nommée, jamais tue.
	list, _ := p.Store.List()
	if len(list) != 1 || !list[0].Unpaired || list[0].Repairable {
		t.Errorf("la liste doit nommer la paire dépareillée : %+v", list)
	}
	if w, _ := p.Warnings(); len(w) != 1 || !strings.Contains(w[0], "dépareill") ||
		!strings.Contains(w[0], "nginx refuse") || !strings.Contains(w[0], "ogs cert renew shopfr") {
		t.Errorf("l'alerte doit nommer la paire, la conséquence sous nginx et le remède : %v", w)
	}

	pairs, err := p.PlanPairs(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0].Critical || pairs[0].FlushReloads ||
		!strings.Contains(pairs[0].Summary, "réémis avant les vhosts") {
		t.Fatalf("la réémission doit précéder les vhosts, sans vidage des rechargements : %v", pairs)
	}
	steps, err := p.Plan(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) == 0 || steps[0].Action == engine.NoOp || !strings.Contains(steps[0].Summary, "dépareill") {
		t.Fatalf("l'étape d'émission doit annoncer la seconde tentative : %v", steps)
	}
	if err := pairs[0].Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := p.Store.Read("shopfr"); !ok {
		t.Error("après l'étape d'avant les vhosts, la paire doit être appariée")
	}
	// La seconde tentative n'émet rien : la paire est rétablie.
	p.Issuer, p.Fallback = refusant{}, refusant{}
	if err := steps[0].Apply(context.Background()); err != nil {
		t.Errorf("une paire rétablie avant les vhosts ne se réémet pas : %v", err)
	}

	// Le timer la reprend aussi.
	p.Issuer = SelfSigned{Validity: 90 * 24 * time.Hour, Now: p.Now}
	if err := os.WriteFile(p.Store.Path("shopfr", FileKey), autre.PrivateKey, 0o600); err != nil {
		t.Fatal(err)
	}
	done, errs := p.Renew(context.Background(), reqs, false)
	if len(errs) != 0 || len(done) != 1 {
		t.Fatalf("le renouvellement doit rétablir la paire : %v %v", done, errs)
	}
	if _, ok, _ := p.Store.Read("shopfr"); !ok {
		t.Error("après le renouvellement, la paire doit être appariée")
	}
	if w, _ := p.Warnings(); len(w) != 0 {
		t.Errorf("aucun avertissement attendu une fois la paire rétablie : %v", w)
	}
}

// Une réémission d'avant les vhosts qui échoue dit ce qui suivra sous le moteur
// retenu, et le remède.
func TestUneReemissionDepareilleeEnEchecNommeLaSuiteEtLeRemede(t *testing.T) {
	for engineName, want := range map[string]string{
		"nginx":  "l'écriture des vhosts échoue",
		"apache": "« apachectl -t » répondant « Syntax OK »",
	} {
		p := newProvider(t)
		p.WebReload = "web:" + engineName
		proj := newProject(t, "shop.fr", naming.EnvProd)
		reqs, _ := p.Requests(proj)
		if err := p.issue(context.Background(), reqs[0]); err != nil {
			t.Fatal(err)
		}
		autre, _ := SelfSigned{}.Issue(context.Background(), reqs[0])
		if err := os.WriteFile(p.Store.Path("shopfr", FileKey), autre.PrivateKey, 0o600); err != nil {
			t.Fatal(err)
		}
		p.Issuer, p.Fallback = refusant{}, refusant{}
		pairs, _ := p.PlanPairs(context.Background(), proj)
		err := pairs[0].Apply(context.Background())
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "ogs cert renew shopfr") {
			t.Errorf("%s : l'échec doit nommer la suite (%q) et le remède : %v", engineName, want, err)
		}
	}
}

// Une écriture interrompue entre deux renommages laisse la clé neuve en place
// et le certificat neuf en attente : elle s'achève avant les vhosts, sans
// challenge ni quota consommé, et le rendu la tient pour achevée — les vhosts
// gardent leur bloc 443, aucun n'est réécrit en HTTP seul.
func TestUneEcritureInterrompueEstAchevee(t *testing.T) {
	p := newProvider(t)
	proj := newProject(t, "shop.fr", naming.EnvProd)
	reqs, _ := p.Requests(proj)
	if err := p.issue(context.Background(), reqs[0]); err != nil {
		t.Fatal(err)
	}
	neuf, err := SelfSigned{Validity: 90 * 24 * time.Hour, Now: p.Now}.Issue(context.Background(), reqs[0])
	if err != nil {
		t.Fatal(err)
	}
	// Ce qu'une écriture coupée après le premier renommage laisse sur le disque.
	full := append(append([]byte{}, neuf.Certificate...), neuf.Chain...)
	for name, data := range map[string][]byte{
		FileKey:                      neuf.PrivateKey,
		FileFullchain + stagedSuffix: full,
		FileCert + stagedSuffix:      neuf.Certificate,
	} {
		if err := os.WriteFile(p.Store.Path("shopfr", name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if w, _ := p.Warnings(); len(w) != 1 || !strings.Contains(w[0], "écriture interrompue") {
		t.Errorf("doctor doit nommer l'écriture interrompue : %v", w)
	}
	if cov := p.Coverage(proj); len(cov.Present) == 0 {
		t.Error("une écriture que l'étape d'avant les vhosts achève doit rester servie au rendu")
	}
	p.Issuer, p.Fallback = refusant{}, refusant{}
	pairs, err := p.PlanPairs(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || !pairs[0].Critical || !strings.Contains(pairs[0].Summary, "sans émission") {
		t.Fatalf("l'achèvement doit précéder les vhosts, en étape critique : %v", pairs)
	}
	steps, _ := p.Plan(context.Background(), proj)
	if steps[0].Action != engine.NoOp {
		t.Errorf("le certificat achevé, frais, ne se réémet pas : %s", steps[0].Summary)
	}
	if err := pairs[0].Apply(context.Background()); err != nil {
		t.Fatalf("l'écriture interrompue doit être achevée sans émission : %v", err)
	}
	got, _ := os.ReadFile(p.Store.Path("shopfr", FileFullchain))
	if string(got) != string(full) {
		t.Error("le certificat en attente doit avoir rejoint sa clé")
	}
	if _, err := os.Stat(p.Store.Path("shopfr", FileCert+stagedSuffix)); !os.IsNotExist(err) {
		t.Errorf("aucun fichier en attente ne doit rester : %v", err)
	}
}

// Le renouvellement qui achève une écriture interrompue juge ensuite le
// certificat achevé : en attente depuis longtemps, il peut être lui-même à
// échéance. Il était compté « renouvelé » sans émission, et le timer le laissait
// expirer.
func TestLeRenouvellementJugeLaPaireAchevee(t *testing.T) {
	p := newProvider(t)
	proj := newProject(t, "shop.fr", naming.EnvProd)
	reqs, _ := p.Requests(proj)
	if err := p.issue(context.Background(), reqs[0]); err != nil {
		t.Fatal(err)
	}
	// Une écriture interrompue d'un certificat qui n'a plus que deux jours.
	vieux, _ := SelfSigned{Validity: 2 * 24 * time.Hour, Now: p.Now}.Issue(context.Background(), reqs[0])
	full := append(append([]byte{}, vieux.Certificate...), vieux.Chain...)
	_ = os.WriteFile(p.Store.Path("shopfr", FileKey), vieux.PrivateKey, 0o600)
	_ = os.WriteFile(p.Store.Path("shopfr", FileFullchain+stagedSuffix), full, 0o644)
	done, errs := p.Renew(context.Background(), reqs, false)
	if len(errs) != 0 || len(done) != 1 {
		t.Fatalf("%v %v", done, errs)
	}
	info, ok, _ := p.Store.Read("shopfr")
	if !ok || info.DaysLeft(p.now()) < 80 {
		t.Errorf("la paire achevée, à échéance, doit être réémise : %+v", info)
	}
}

// R1-73 : l'attente du verrou d'un certificat était bloquante sans borne ni
// interruption. Elle se borne sur l'opération et suit le contexte.
func TestLeVerrouDuCertificatEstBorneEtInterruptible(t *testing.T) {
	s := newStore(t)
	issueSelfSigned(t, s, "shopfr", []string{"shop.fr"}, time.Hour)
	f, err := os.Open(s.Dir("shopfr"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	old := lockBudget
	lockBudget = 300 * time.Millisecond
	defer func() { lockBudget = old }()
	m, _ := SelfSigned{}.Issue(context.Background(), Request{Name: "shopfr", Domains: []string{"shop.fr"}})
	début := time.Now()
	if err := s.Write("shopfr", m); err == nil || !strings.Contains(err.Error(), "tenu depuis plus de") {
		t.Errorf("un verrou tenu doit faire échouer l'écriture en le nommant : %v", err)
	}
	if d := time.Since(début); d > 3*time.Second {
		t.Errorf("l'attente doit être bornée : %s", d)
	}
	lockBudget = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	début = time.Now()
	if err := s.WriteContext(ctx, "shopfr", m); !errors.Is(err, context.Canceled) {
		t.Errorf("une interruption doit abandonner l'attente : %v", err)
	}
	if d := time.Since(début); d > 3*time.Second {
		t.Errorf("l'interruption doit être prise en compte sans délai : %s", d)
	}
}

// Une URL d'autorité se compare comme l'autorité la sert : un « / » final, une
// casse ou un port par défaut ne font pas d'une autorité de test une autorité de
// production — le certificat de test était sinon réémis à chaque apply.
func TestSameDirectory(t *testing.T) {
	for _, c := range []struct {
		a    string
		same bool
	}{
		{DirectoryStaging, true},
		{DirectoryStaging + "/", true},
		{"HTTPS://Acme-Staging-v02.api.letsencrypt.org:443/directory", true},
		{" " + DirectoryStaging + " ", true},
		{"https://acme-v02.api.letsencrypt.org/directory", false},
		{"https://acme-staging-v02.api.letsencrypt.org:14000/directory", false},
		{"", false},
	} {
		if got := SameDirectory(c.a, DirectoryStaging); got != c.same {
			t.Errorf("SameDirectory(%q) = %v, attendu %v", c.a, got, c.same)
		}
	}
	p := newProvider(t)
	p.Cfg.ACME.Directory = DirectoryStaging + "/"
	if p.replacesStaging(Request{Name: "x", Challenge: ChallengeHTTP01}, Info{Staging: true}) {
		t.Error("une autorité de test désignée avec un « / » final ne doit pas faire remplacer son certificat")
	}
}

// refusant est un émetteur qui échoue toujours.
type refusant struct{}

func (refusant) Name() string { return "refusant" }
func (refusant) Issue(context.Context, Request) (Material, error) {
	return Material{}, os.ErrPermission
}
func (refusant) Revoke(context.Context, Request, []byte) error { return nil }

// Le magasin refuse d'écrire une paire que l'émetteur aurait mal assemblée :
// mieux vaut garder l'ancienne, servie, qu'en poser une que le moteur refuse.
func TestLeMagasinRefuseDEcrireUnePaireDepareillee(t *testing.T) {
	s := newStore(t)
	req := Request{Name: "shopfr", Domains: []string{"shop.fr"}, Challenge: ChallengeSelfSigned}
	issueSelfSigned(t, s, "shopfr", req.Domains, time.Hour)
	avant, _ := os.ReadFile(s.Path("shopfr", FileFullchain))
	a, _ := SelfSigned{}.Issue(context.Background(), req)
	b, _ := SelfSigned{}.Issue(context.Background(), req)
	a.PrivateKey = b.PrivateKey
	if err := s.Write("shopfr", a); err == nil {
		t.Fatal("une paire dépareillée doit être refusée à l'écriture")
	}
	après, _ := os.ReadFile(s.Path("shopfr", FileFullchain))
	if string(avant) != string(après) {
		t.Error("le refus doit laisser l'ancienne paire en place")
	}
}

// stagingIssuer imite Let's Encrypt : une intermédiaire « (STAGING) » qu'aucun
// navigateur ne reconnaît dans l'environnement de test, une intermédiaire de
// production sinon.
type stagingIssuer struct {
	now  func() time.Time
	seen *[]bool
}

func (stagingIssuer) Name() string                                    { return "staging" }
func (s stagingIssuer) Revoke(context.Context, Request, []byte) error { return nil }
func (s stagingIssuer) Issue(_ context.Context, req Request) (Material, error) {
	if s.seen != nil {
		*s.seen = append(*s.seen, req.Staging)
	}
	caName := "R10"
	if req.Staging {
		caName = "(STAGING) Ersatz Edamame E1"
	}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: caName},
		NotBefore: s.now().Add(-time.Hour), NotAfter: s.now().Add(365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return Material{}, err
	}
	ca, _ = x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: req.Domains[0]}, DNSNames: req.Domains,
		NotBefore: s.now().Add(-time.Hour), NotAfter: s.now().Add(90 * 24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		return Material{}, err
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return Material{
		Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Chain:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		PrivateKey:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// R2-29 : acme.staging n'était lu que par « cert issue ». L'apply et le timer
// émettaient en production une machine qu'on éprouvait, et un certificat de
// l'environnement de test restait servi à vie une fois l'option retirée : frais,
// il couvrait sa demande, et le plan n'y voyait rien à faire. Il est remplacé
// d'office, l'ancien restant servi jusqu'à l'émission (D79, D110).
func TestAcmeStagingSuitLApplyEtSonCertificatEstRemplaceQuandIlEstRetire(t *testing.T) {
	p := newProvider(t)
	var seen []bool
	p.Issuer = stagingIssuer{now: p.Now, seen: &seen}
	p.Cfg.ACME.Staging = true
	proj := newProject(t, "shop.fr", naming.EnvProd)
	steps, err := p.Plan(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	if err := steps[0].Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || !seen[0] {
		t.Fatalf("acme.staging doit atteindre l'émission de l'apply : %v", seen)
	}
	info, ok, _ := p.Store.Read("shopfr")
	if !ok || !info.Staging {
		t.Fatalf("le certificat de test doit être reconnu comme tel : %+v", info)
	}
	// Tant que staging reste demandé, rien à refaire.
	steps, _ = p.Plan(context.Background(), proj)
	if steps[0].Action != engine.NoOp {
		t.Errorf("un certificat de test frais, staging demandé : aucune réémission attendue : %s", steps[0].Summary)
	}

	// L'option est retirée : le plan remplace le certificat, et le dit.
	p.Cfg.ACME.Staging = false
	steps, _ = p.Plan(context.Background(), proj)
	if steps[0].Action == engine.NoOp || !strings.Contains(steps[0].Summary, "environnement de test") {
		t.Fatalf("le plan doit annoncer le remplacement du certificat de test : %s", steps[0].Summary)
	}
	// Jusqu'à l'émission, l'ancien reste servi.
	if cov := p.Coverage(proj); len(cov.Present) == 0 {
		t.Error("le certificat de test doit rester servi jusqu'à son remplacement")
	}
	if err := steps[0].Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, _, _ = p.Store.Read("shopfr")
	if info.Staging {
		t.Error("le certificat doit avoir été remplacé par un certificat de production")
	}

	// Le timer le remplace de même — un certificat de test demandé
	// explicitement.
	reqs, _ := p.Requests(proj)
	explicite := reqs[0]
	explicite.Staging = true
	if err := p.issue(context.Background(), explicite); err != nil {
		t.Fatal(err)
	}
	done, errs := p.Renew(context.Background(), reqs, false)
	if len(errs) != 0 || len(done) != 1 {
		t.Fatalf("le renouvellement doit remplacer le certificat de test : %v %v", done, errs)
	}
	if info, _, _ := p.Store.Read("shopfr"); info.Staging {
		t.Error("le timer doit avoir remplacé le certificat de test")
	}
	// Un partagé suit la même option.
	p.Cfg.ACME.Staging = true
	if req, _ := p.SharedRequest("@wildcard-prep"); !req.Staging {
		t.Error("acme.staging doit valoir pour les certificats partagés")
	}
}

// Relecture de D110 : acme.staging resté actif faisait remplacer par le timer
// chaque certificat de production arrivé à échéance par un certificat de test,
// qu'aucun navigateur n'accepte — HSTS compris, le site devenait injoignable,
// sans annonce. Le renouvellement garde l'autorité de production et le dit
// (code 6) ; une émission neuve suit l'option, une demande explicite aussi.
func TestAcmeStagingNeRemplacePasUnCertificatDeProduction(t *testing.T) {
	p := newProvider(t)
	var seen []bool
	p.Issuer = stagingIssuer{now: p.Now, seen: &seen}
	proj := newProject(t, "shop.fr", naming.EnvProd)
	reqs, _ := p.Requests(proj)
	// Un certificat de production à dix jours de l'échéance.
	jeune := stagingIssuer{now: func() time.Time { return p.now().Add(-80 * 24 * time.Hour) }}
	m, _ := jeune.Issue(context.Background(), reqs[0])
	if err := p.Store.Write("shopfr", m); err != nil {
		t.Fatal(err)
	}
	p.Cfg.ACME.Staging = true

	steps, _ := p.Plan(context.Background(), proj)
	if !strings.Contains(steps[0].Summary, "autorité de production conservée") {
		t.Errorf("le plan doit annoncer que l'autorité de production est conservée : %s", steps[0].Summary)
	}
	err := steps[0].Apply(context.Background())
	var partial *engine.PartialError
	if !errors.As(err, &partial) || !strings.Contains(err.Error(), "malgré acme.staging") {
		t.Errorf("l'apply doit le dire en succès partiel (code 6) : %v", err)
	}
	if info, _, _ := p.Store.Read("shopfr"); info.Staging || info.DaysLeft(p.now()) < 80 {
		t.Fatalf("le certificat doit être renouvelé en production : %+v", info)
	}
	if len(seen) != 1 || seen[0] {
		t.Errorf("l'émission doit viser la production : %v", seen)
	}

	// Le timer, de même.
	m, _ = jeune.Issue(context.Background(), reqs[0])
	_ = p.Store.Write("shopfr", m)
	reqs, _ = p.Requests(proj)
	done, errs := p.Renew(context.Background(), reqs, false)
	var kept *ProductionKeptError
	if len(done) != 1 || len(errs) != 1 || !errors.As(errs[0], &kept) {
		t.Fatalf("le renouvellement doit garder la production et le dire : %v %v", done, errs)
	}
	if info, _, _ := p.Store.Read("shopfr"); info.Staging {
		t.Error("le timer ne doit pas poser un certificat de test")
	}

	// Une déclaration explicite le remplace.
	m, _ = jeune.Issue(context.Background(), reqs[0])
	_ = p.Store.Write("shopfr", m)
	explicite := reqs[0]
	explicite.ConfigStaging = false
	done, errs = p.Renew(context.Background(), []Request{explicite}, false)
	if len(done) != 1 || len(errs) != 0 {
		t.Fatalf("%v %v", done, errs)
	}
	if info, _, _ := p.Store.Read("shopfr"); !info.Staging {
		t.Error("une demande explicite de l'environnement de test doit être suivie")
	}
}

// Le contrôle d'avant rechargement (B2) : une écriture interrompue s'achève —
// Store.Read la lit déjà comme achevée, hors de l'apply du projet aussi —, une
// clé recopiée est nommée, et rien n'est écrit sans repair (doctor).
func TestCheckPairAcheveOuNomme(t *testing.T) {
	p := newProvider(t)
	proj := newProject(t, "shop.fr", naming.EnvProd)
	reqs, _ := p.Requests(proj)
	if err := p.issue(context.Background(), reqs[0]); err != nil {
		t.Fatal(err)
	}
	dir := p.Store.Dir("shopfr")
	if name, paired, _, err := p.CheckPair(context.Background(), dir, true); err != nil || !paired || name != "shopfr" {
		t.Fatalf("une paire appariée : %s %v %v", name, paired, err)
	}
	neuf, _ := SelfSigned{}.Issue(context.Background(), reqs[0])
	full := append(append([]byte{}, neuf.Certificate...), neuf.Chain...)
	_ = os.WriteFile(p.Store.Path("shopfr", FileKey), neuf.PrivateKey, 0o600)
	_ = os.WriteFile(p.Store.Path("shopfr", FileFullchain+stagedSuffix), full, 0o644)
	if _, paired, _, _ := p.CheckPair(context.Background(), dir, false); paired {
		t.Error("sans repair, la paire est nommée dépareillée")
	}
	if _, err := os.Stat(p.Store.Path("shopfr", FileFullchain+stagedSuffix)); err != nil {
		t.Error("sans repair, rien n'est écrit")
	}
	if _, paired, repaired, err := p.CheckPair(context.Background(), dir, true); !paired || !repaired || err != nil {
		t.Errorf("avec repair, l'écriture interrompue s'achève : %v %v %v", paired, repaired, err)
	}
	autre, _ := SelfSigned{}.Issue(context.Background(), reqs[0])
	_ = os.WriteFile(p.Store.Path("shopfr", FileKey), autre.PrivateKey, 0o600)
	if _, paired, _, _ := p.CheckPair(context.Background(), dir, true); paired {
		t.Error("une clé recopiée sans fichier en attente reste dépareillée")
	}
	// Hors du magasin : pas ce contrôle.
	if _, paired, _, _ := p.CheckPair(context.Background(), "/etc/letsencrypt/live/x", true); !paired {
		t.Error("un répertoire hors du magasin ne relève pas de ce contrôle")
	}
}
