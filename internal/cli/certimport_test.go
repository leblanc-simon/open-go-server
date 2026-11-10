package cli

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// autoritéDEssai signe les certificats importés des essais ; importRoots la
// désigne, pour que la chaîne se vérifie comme celle de Let's Encrypt contre
// les autorités du système.
type autoritéDEssai struct {
	cert *x509.Certificate
	der  []byte
	key  *ecdsa.PrivateKey
}

func nouvelleAutorité(t *testing.T) *autoritéDEssai {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Autorité d'essai R1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(5 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &autoritéDEssai{cert: cert, der: der, key: key}
}

// faireConfiance fait de l'autorité la seule reconnue pour la durée de l'essai.
func (a *autoritéDEssai) faireConfiance(t *testing.T) {
	pool := x509.NewCertPool()
	pool.AddCert(a.cert)
	importRoots = pool
	t.Cleanup(func() { importRoots = nil })
}

// paire écrit dans dir la chaîne complète et la clé d'un certificat pour
// domains, valable jusqu'à now+validité, et rend leurs chemins.
func (a *autoritéDEssai) paire(t *testing.T, dir string, validité time.Duration, domains ...string) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: domains[0]},
		DNSNames: domains, NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: time.Now().Add(validité),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	full := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.der})...)
	if err := os.WriteFile(certPath, full, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

const quatreVingtJours = 80 * 24 * time.Hour

// D130 : un site qui migre est créé avec le certificat de l'ancien serveur. Le
// premier apply le trouve en place et sert le site en HTTPS sans rien émettre —
// une émission HTTP-01 échouerait, le DNS désignant encore l'ancien serveur.
func TestSiteCreateImporteLeCertificatAvantLePremierApply(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	ca := nouvelleAutorité(t)
	ca.faireConfiance(t)
	certPath, keyPath := ca.paire(t, t.TempDir(), quatreVingtJours, "shop.fr", "www.shop.fr")

	out := h.mustRun("site", "create", "shop.fr", "--kind", "static", "--alias", "www.shop.fr",
		"--cert", certPath, "--key", keyPath, "--yes")
	if !strings.Contains(out, "certificat shopfr importé") {
		t.Errorf("l'import doit être annoncé :\n%s", out)
	}
	want, _ := os.ReadFile(certPath)
	got, err := os.ReadFile(h.path("var", "lib", "ogs", "acme", "certificates", "shopfr", "fullchain.pem"))
	if err != nil || string(got) != string(want) {
		t.Fatalf("le magasin doit porter la chaîne importée, non un certificat émis : %v", err)
	}
	vhost := h.read(t, "etc/apache2/sites-available/shopfr.conf")
	if !strings.Contains(vhost, "acme/certificates/shopfr/fullchain.pem") {
		t.Errorf("le vhost doit servir le certificat importé dès le premier apply :\n%s", vhost)
	}
	plan := h.mustRun("plan", "shopfr")
	if strings.Contains(plan, "renouvellement") || strings.Contains(plan, "pour shop.fr") {
		t.Errorf("le plan suivant n'a rien à émettre :\n%s", plan)
	}
}

// Une paire refusée ne laisse aucune déclaration : rien n'est écrit avant que
// l'import n'ait été contrôlé.
func TestSiteCreateRefuseUnePaireDepareilleeAvantToutEcrit(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	ca := nouvelleAutorité(t)
	ca.faireConfiance(t)
	certPath, _ := ca.paire(t, t.TempDir(), quatreVingtJours, "shop.fr")
	_, autreClé := ca.paire(t, t.TempDir(), quatreVingtJours, "shop.fr")

	out, code := h.run("site", "create", "shop.fr", "--kind", "static", "--cert", certPath, "--key", autreClé, "--yes")
	if code != CodeValidation || !strings.Contains(out, "ne correspond pas") {
		t.Fatalf("code %d, attendu %d :\n%s", code, CodeValidation, out)
	}
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "shopfr.yaml")); !os.IsNotExist(err) {
		t.Errorf("aucune déclaration ne doit rester : %v", err)
	}
	if _, err := os.Stat(h.path("var", "lib", "ogs", "acme", "certificates", "shopfr")); !os.IsNotExist(err) {
		t.Errorf("aucun certificat ne doit être écrit : %v", err)
	}
}

func TestCertImportRemplaceSousForceApresSauvegarde(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	ca := nouvelleAutorité(t)
	ca.faireConfiance(t)
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	certPath, keyPath := ca.paire(t, t.TempDir(), quatreVingtJours, "shop.fr")

	// Le site a déjà son certificat (auto-signé en mode préfixé) : sans --force,
	// l'import refuse.
	out, code := h.run("cert", "import", "shopfr", "--cert", certPath, "--key", keyPath)
	if code != CodeConflict || !strings.Contains(out, "--force") {
		t.Fatalf("code %d, attendu %d :\n%s", code, CodeConflict, out)
	}
	out = h.mustRun("cert", "import", "shopfr", "--cert", certPath, "--key", keyPath, "--force")
	if !strings.Contains(out, "certificat shopfr remplacé") || !strings.Contains(out, "rechargement") {
		t.Errorf("le remplacement d'un certificat servi recharge le serveur web :\n%s", out)
	}
	backups, _ := filepath.Glob(h.path("var", "lib", "ogs", "backups", "shopfr-*"))
	if len(backups) != 2 {
		t.Errorf("la chaîne et la clé remplacées doivent être sauvegardées : %v", backups)
	}
	for _, b := range backups {
		if st, err := os.Stat(b); err == nil && strings.Contains(b, "privkey") && st.Mode().Perm() != 0o600 {
			t.Errorf("la clé sauvegardée doit rester en 0600 : %v", st.Mode())
		}
	}
	want, _ := os.ReadFile(certPath)
	got, _ := os.ReadFile(h.path("var", "lib", "ogs", "acme", "certificates", "shopfr", "fullchain.pem"))
	if string(got) != string(want) {
		t.Error("le magasin doit porter la chaîne importée")
	}
}

func TestCertImportRefuseCeQueLeServeurWebRefuserait(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	ca := nouvelleAutorité(t)
	ca.faireConfiance(t)
	h.mustRun("project", "create", "shop.fr", "--kind", "static")

	échu, échuClé := ca.paire(t, t.TempDir(), -time.Hour, "shop.fr")
	ailleurs, ailleursClé := ca.paire(t, t.TempDir(), quatreVingtJours, "autre.fr")
	chiffrée := filepath.Join(t.TempDir(), "chiffree.pem")
	_ = os.WriteFile(chiffrée, []byte("-----BEGIN ENCRYPTED PRIVATE KEY-----\nAAAA\n-----END ENCRYPTED PRIVATE KEY-----\n"), 0o600)
	bon, _ := ca.paire(t, t.TempDir(), quatreVingtJours, "shop.fr")

	for _, c := range []struct {
		nom, cert, key, motif string
		code                  int
	}{
		{"échu", échu, échuClé, "échu", CodeValidation},
		{"autre domaine", ailleurs, ailleursClé, "ne couvre aucun nom", CodeValidation},
		{"clé chiffrée", bon, chiffrée, "chiffrée", CodeValidation},
		{"fichier absent", filepath.Join(t.TempDir(), "absent.pem"), chiffrée, "--cert", CodeUsage},
	} {
		t.Run(c.nom, func(t *testing.T) {
			out, code := h.run("cert", "import", "shopfr", "--cert", c.cert, "--key", c.key)
			if code != c.code || !strings.Contains(out, c.motif) {
				t.Errorf("code %d, attendu %d avec « %s » :\n%s", code, c.code, c.motif, out)
			}
		})
	}
	if _, err := os.Stat(h.path("var", "lib", "ogs", "acme", "certificates", "shopfr")); !os.IsNotExist(err) {
		t.Errorf("aucun refus ne doit écrire dans le magasin : %v", err)
	}
}

// Avertir sans refuser : une échéance proche, une chaîne qu'aucune autorité du
// système ne reconnaît, un nom non couvert. Le certificat est posé, code 6.
func TestCertImportAvertitEtRendSixSansRefuser(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	ca := nouvelleAutorité(t)
	ca.faireConfiance(t)
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--alias", "www.shop.fr", "--apply=false")

	certPath, keyPath := ca.paire(t, t.TempDir(), 10*24*time.Hour, "shop.fr")
	out, code := h.run("cert", "import", "shopfr", "--cert", certPath, "--key", keyPath)
	if code != CodePartial {
		t.Fatalf("code %d, attendu %d :\n%s", code, CodePartial, out)
	}
	for _, motif := range []string{"expire dans", "ne couvre pas www.shop.fr", "ogs apply shopfr"} {
		if !strings.Contains(out, motif) {
			t.Errorf("« %s » attendu :\n%s", motif, out)
		}
	}
	if _, err := os.Stat(h.path("var", "lib", "ogs", "acme", "certificates", "shopfr", "privkey.pem")); err != nil {
		t.Errorf("le certificat doit être posé malgré les avertissements : %v", err)
	}

	// Sans l'autorité d'essai parmi les autorités reconnues, la chaîne ne se
	// vérifie pas : un avertissement, jamais un refus.
	importRoots = x509.NewCertPool()
	other, otherKey := ca.paire(t, t.TempDir(), quatreVingtJours, "shop.fr", "www.shop.fr")
	out, code = h.run("cert", "import", "shopfr", "--cert", other, "--key", otherKey, "--force")
	if code != CodePartial || !strings.Contains(out, "chaîne non vérifiée") {
		t.Errorf("code %d, attendu %d avec « chaîne non vérifiée » :\n%s", code, CodePartial, out)
	}
}
