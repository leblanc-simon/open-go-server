package migration

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

// certificatSource pose sur la source, à la manière du live/ de certbot, une
// paire pour domains signée par une autorité d'essai, et rend le répertoire.
// cléÉtrangère y met la clé d'une autre paire.
func (b *banc) certificatSource(cléÉtrangère bool, domains ...string) string {
	b.t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Autorite essai"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		b.t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: domains[0]}, DNSNames: domains,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(80 * 24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		b.t.Fatal(err)
	}
	if cléÉtrangère {
		key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	dir := filepath.Join(b.source, "etc/letsencrypt/live", domains[0])
	b.ecrire(filepath.Join(dir, "fullchain.pem"), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))+
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})))
	b.ecrire(filepath.Join(dir, "privkey.pem"), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	return dir
}

func (b *banc) projetShopAvecCertificat(dir string) M {
	p := b.projetShop()
	p["source"].(M)["certificat"] = dir
	return p
}

// D130 : le certificat de la source est importé avant le premier apply, qui sert
// alors le site en HTTPS sans tenter d'émission — vouée à l'échec tant que le
// DNS désigne la source. Rejouée, la déclaration laisse en place le certificat
// déjà importé.
func TestDeclarerImporteLeCertificatDeLaSourceAvantLePremierApply(t *testing.T) {
	b := nouveauBanc(t)
	dir := b.certificatSource(false, "shop.fr", "www.shop.fr")
	b.ecrireManifeste(b.manifeste(b.projetShopAvecCertificat(dir)))

	// En simulation, l'import est annoncé, rien n'est lu ni écrit.
	m := b.marque()
	sortie := b.reussir(nil, "declarer", "shopfr")
	contient(t, "simulation", sortie, "cert import shopfr --cert")
	exclut(t, "simulation", b.depuis(m), "privkey.pem", "[ogs] cert import")

	m = b.marque()
	sortie = b.reussir(nil, phase("declarer", "shopfr")...)
	journal := b.depuis(m)
	imp, app := strings.Index(journal, "[ogs] cert import shopfr"), strings.Index(journal, "[ogs] apply shopfr")
	if imp < 0 || app < 0 || imp > app {
		t.Fatalf("l'import doit précéder le premier apply\n%s\n%s", journal, sortie)
	}
	contient(t, "lecture de la source", journal, "[source] cat "+dir+"/privkey.pem")
	// La clé passe par la sortie de ssh, jamais par une ligne de commande.
	clé := strings.Split(b.lire(filepath.Join(dir, "privkey.pem")), "\n")[1]
	exclut(t, "journal des appels", b.journal(), clé)
	attendu := b.lire(filepath.Join(dir, "fullchain.pem"))
	if got := b.lire(filepath.Join(b.prefixe, "var/lib/ogs/acme/certificates/shopfr/fullchain.pem")); got != attendu {
		t.Error("le magasin de la cible doit porter la chaîne de la source")
	}
	contient(t, "vhost", b.lire(filepath.Join(b.prefixe, "etc/apache2/sites-available/shopfr.conf")),
		"acme/certificates/shopfr/fullchain.pem")
	// La clé n'a laissé aucune copie dans le répertoire temporaire.
	if entrees, _ := os.ReadDir(filepath.Join(b.dir, "tmp")); len(entrees) != 0 {
		t.Errorf("répertoire de l'exécution laissé derrière elle : %v", entrees)
	}

	sortie = b.reussir(nil, phase("declarer", "shopfr")...)
	contient(t, "déclaration rejouée", sortie, "déjà présent sur la cible : laissé tel quel")
}

// Une paire refusée arrête la déclaration avant l'apply, en nommant l'issue.
func TestDeclarerRefuseUnCertificatDepareilleAvantLApply(t *testing.T) {
	b := nouveauBanc(t)
	dir := b.certificatSource(true, "shop.fr")
	b.ecrireManifeste(b.manifeste(b.projetShopAvecCertificat(dir)))

	m := b.marque()
	sortie := b.echouer(nil, phase("declarer", "shopfr")...)
	contient(t, "refus", sortie, "ne correspond pas", "retirer source.certificat")
	exclut(t, "refus", b.depuis(m), "[ogs] apply shopfr")
}

func TestLeControleExigeUnCheminAbsoluPourLeCertificat(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrireManifeste(b.manifeste(b.projetShopAvecCertificat("etc/letsencrypt/live/shop.fr")))
	sortie := b.echouer(nil, "controle", "shopfr")
	contient(t, "contrôle", sortie, "source.certificat : chemin absolu")
}

// prealables vérifie que la paire désignée est lisible sur la source.
func TestLesPrealablesVerifientLeCertificatDeLaSource(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrireManifeste(b.manifeste(b.projetShopAvecCertificat(filepath.Join(b.source, "etc/letsencrypt/live/absent.fr"))))
	sortie := b.echouer([]string{dnsSource}, "prealables", "shopfr")
	contient(t, "préalables", sortie, "privkey.pem illisible sur la source")
}
