package tls

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leviathan/ogs/internal/config"
)

func newStore(t *testing.T) Store {
	t.Helper()
	return Store{Paths: config.NewPaths(t.TempDir())}
}

func issueSelfSigned(t *testing.T, s Store, name string, domains []string, validity time.Duration) Info {
	t.Helper()
	issuer := SelfSigned{Validity: validity}
	m, err := issuer.Issue(context.Background(), Request{
		Name: name, Domains: domains, Challenge: ChallengeSelfSigned,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(name, m); err != nil {
		t.Fatal(err)
	}
	info, ok, err := s.Read(name)
	if err != nil || !ok {
		t.Fatalf("certificat illisible : %v", err)
	}
	return info
}

func TestSelfSignedRoundTrip(t *testing.T) {
	s := newStore(t)
	info := issueSelfSigned(t, s, "shopfr", []string{"shop.fr", "www.shop.fr"}, 90*24*time.Hour)
	if len(info.Domains) != 2 || info.Domains[0] != "shop.fr" {
		t.Errorf("domaines : %v", info.Domains)
	}
	if !info.SelfSign {
		t.Error("le certificat doit être reconnu comme auto-signé")
	}
	if days := info.DaysLeft(time.Now()); days < 88 || days > 90 {
		t.Errorf("durée restante %d jours", days)
	}
}

func TestWritePermissions(t *testing.T) {
	s := newStore(t)
	issueSelfSigned(t, s, "shopfr", []string{"shop.fr"}, time.Hour)
	cases := map[string]os.FileMode{
		FileKey:       0o600, // la clé privée n'est lisible que par root
		FileCert:      0o644,
		FileFullchain: 0o644,
	}
	for file, want := range cases {
		info, err := os.Stat(s.Path("shopfr", file))
		if err != nil {
			t.Fatalf("%s : %v", file, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s : mode %04o, attendu %04o", file, got, want)
		}
	}
	// Un auto-signé n'a pas de chaîne : le fichier doit être absent plutôt que
	// vide, un chain.pem vide faisant échouer le chargement côté serveur web.
	if _, err := os.Stat(s.Path("shopfr", FileChain)); !os.IsNotExist(err) {
		t.Errorf("chain.pem ne devrait pas exister pour un auto-signé : %v", err)
	}
}

func TestSharedCertificateDirectoryIsDistinct(t *testing.T) {
	s := newStore(t)
	if !IsShared("@wildcard-prep") {
		t.Error("la référence @ désigne un certificat partagé")
	}
	if IsShared("shopfr") {
		t.Error("un certificat de projet n'est pas partagé")
	}
	dir := s.Dir("@wildcard-prep")
	if filepath.Base(dir) != "_shared-wildcard-prep" {
		t.Errorf("répertoire d'un partagé : %s", dir)
	}
	// Un projet nommé « wildcard-prep » ne doit pas entrer en collision.
	if s.Dir("wildcard-prep") == dir {
		t.Error("les espaces de noms doivent être distincts")
	}
}

func TestReadAbsentCertificate(t *testing.T) {
	s := newStore(t)
	if _, ok, err := s.Read("absent"); ok || err != nil {
		t.Errorf("un certificat absent doit être signalé sans erreur : %v, %v", ok, err)
	}
}

func TestListSortsAndIncludesShared(t *testing.T) {
	s := newStore(t)
	issueSelfSigned(t, s, "shopfr", []string{"shop.fr"}, time.Hour)
	issueSelfSigned(t, s, "@wildcard-prep", []string{"*.preprod-pp.net", "preprod-pp.net"}, time.Hour)
	issueSelfSigned(t, s, "autrefr", []string{"autre.fr"}, time.Hour)
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("%d certificats : %+v", len(list), list)
	}
	if list[0].Name != "@wildcard-prep" || !list[0].Shared {
		t.Errorf("le partagé doit être listé et marqué : %+v", list[0])
	}
	if list[1].Name != "autrefr" || list[2].Name != "shopfr" {
		t.Errorf("tri instable : %s, %s", list[1].Name, list[2].Name)
	}
}

func TestNeedsRenewal(t *testing.T) {
	s := newStore(t)
	// Un certificat de 90 jours n'a pas besoin d'être renouvelé aujourd'hui.
	fresh := issueSelfSigned(t, s, "frais", []string{"frais.fr"}, 90*24*time.Hour)
	if fresh.NeedsRenewal(time.Now()) {
		t.Error("un certificat de 90 jours ne doit pas être renouvelé")
	}
	// À 20 jours de l'échéance, si.
	soon := issueSelfSigned(t, s, "bientot", []string{"bientot.fr"}, 20*24*time.Hour)
	if !soon.NeedsRenewal(time.Now()) {
		t.Error("un certificat expirant dans 20 jours doit être renouvelé")
	}
	if soon.Expired(time.Now()) {
		t.Error("il n'est pas encore expiré")
	}
	// Le seuil est bien 30 jours.
	if !fresh.NeedsRenewal(fresh.NotAfter.Add(-RenewBefore + time.Hour)) {
		t.Error("le seuil de renouvellement doit être de 30 jours")
	}
}

func TestCoversWildcardRules(t *testing.T) {
	s := newStore(t)
	info := issueSelfSigned(t, s, "@wildcard-prep",
		[]string{"*.preprod-pp.net", "preprod-pp.net"}, time.Hour)
	cases := map[string]bool{
		"shopfr.preprod-pp.net":     true,
		"autre.preprod-pp.net":      true,
		"preprod-pp.net":            true,
		"a.shopfr.preprod-pp.net":   false, // un joker ne couvre qu'un niveau
		"preprod-pp.net.exemple.fr": false,
		"shop.fr":                   false,
	}
	for domain, want := range cases {
		if got := info.Covers([]string{domain}); got != want {
			t.Errorf("Covers(%q) = %v, attendu %v", domain, got, want)
		}
	}
	// Plusieurs domaines : tous doivent être couverts.
	if info.Covers([]string{"a.preprod-pp.net", "shop.fr"}) {
		t.Error("un domaine non couvert doit invalider l'ensemble")
	}
}

func TestRequestValidation(t *testing.T) {
	valid := []Request{
		{Name: "shopfr", Domains: []string{"shop.fr", "www.shop.fr"}, Challenge: ChallengeHTTP01},
		{Name: "prep", Domains: []string{"*.preprod-pp.net"}, Challenge: ChallengeDNS01, DNSProvider: "ovh"},
	}
	for _, r := range valid {
		if err := r.Validate(); err != nil {
			t.Errorf("demande valide refusée (%s) : %v", r.Name, err)
		}
	}
	invalid := []struct {
		req    Request
		reason string
	}{
		{Request{Domains: []string{"shop.fr"}}, "nom manquant"},
		{Request{Name: "x"}, "aucun domaine"},
		{Request{Name: "x", Domains: []string{"*.preprod-pp.net"}, Challenge: ChallengeHTTP01}, "joker en HTTP-01"},
		{Request{Name: "x", Domains: []string{"shop.fr"}, Challenge: ChallengeDNS01}, "provider DNS manquant"},
		{Request{Name: "x", Domains: []string{"a.*.fr"}, Challenge: ChallengeDNS01, DNSProvider: "ovh"}, "joker mal placé"},
		{Request{Name: "x", Domains: []string{"192.168.1.1"}, Challenge: ChallengeHTTP01}, "adresse IP"},
		{Request{Name: "x", Domains: []string{"localhost"}, Challenge: ChallengeHTTP01}, "un seul label"},
	}
	for _, c := range invalid {
		if err := c.req.Validate(); err == nil {
			t.Errorf("demande invalide acceptée (%s)", c.reason)
		}
	}
}

func TestParseChallenge(t *testing.T) {
	if c, err := ParseChallenge(""); err != nil || c != ChallengeHTTP01 {
		t.Errorf("défaut = %q, %v", c, err)
	}
	if c, _ := ParseChallenge("DNS-01"); c != ChallengeDNS01 {
		t.Errorf("casse non normalisée : %q", c)
	}
	if _, err := ParseChallenge("tls-alpn-01"); err == nil {
		t.Error("challenge non géré accepté")
	}
}

func TestRemove(t *testing.T) {
	s := newStore(t)
	issueSelfSigned(t, s, "shopfr", []string{"shop.fr"}, time.Hour)
	if err := s.Remove("shopfr"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Read("shopfr"); ok {
		t.Error("le certificat devrait avoir été supprimé")
	}
}
