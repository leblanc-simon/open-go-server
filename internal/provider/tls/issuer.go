package tls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// Challenge est la méthode de validation ACME.
type Challenge string

const (
	ChallengeHTTP01     Challenge = "http-01"
	ChallengeDNS01      Challenge = "dns-01"
	ChallengeSelfSigned Challenge = "selfsigned"
)

// ParseChallenge valide une méthode de validation.
func ParseChallenge(v string) (Challenge, error) {
	switch Challenge(strings.ToLower(strings.TrimSpace(v))) {
	case "", ChallengeHTTP01:
		return ChallengeHTTP01, nil
	case ChallengeDNS01:
		return ChallengeDNS01, nil
	case ChallengeSelfSigned:
		return ChallengeSelfSigned, nil
	}
	return "", fmt.Errorf("challenge %q inconnu (http-01, dns-01 ou selfsigned)", v)
}

// Request décrit une demande d'émission.
type Request struct {
	Name        string
	Domains     []string
	Challenge   Challenge
	DNSProvider string
	// DNSCredentials désigne le jeu d'identifiants DNS à employer pour tous les
	// noms ; vide, chaque nom reçoit celui de sa zone (D97).
	DNSCredentials string
	// AutoDNS signale un challenge DNS-01 que la demande n'a pas choisi : tous ses
	// noms relèvent d'une zone déclarée. Il se replie sur HTTP-01 en cas d'échec,
	// ce que le challenge par défaut aurait tenté.
	AutoDNS bool
	KeyType string
	Staging bool
	Email   string
	// Essential porte les noms dont l'émission ne doit dépendre d'aucun autre :
	// ceux du vhost principal. Quand la demande complète échoue, l'émission se
	// replie par paliers qui écartent d'abord les redirections, puis les noms que
	// le certificat en place ne couvre pas (fallbacks). Vide, la demande est d'un
	// seul tenant — un certificat partagé, par exemple.
	Essential []string
}

// Wildcard indique si la demande porte un joker, ce qui impose DNS-01 : aucun
// challenge HTTP ne peut prouver la maîtrise de *.exemple.net.
func (r Request) Wildcard() bool {
	for _, d := range r.Domains {
		if strings.HasPrefix(d, "*.") {
			return true
		}
	}
	return false
}

// Validate contrôle la cohérence de la demande.
func (r Request) Validate() error {
	if r.Name == "" {
		return fmt.Errorf("nom de certificat requis")
	}
	if len(r.Domains) == 0 {
		return fmt.Errorf("certificat %s : aucun domaine", r.Name)
	}
	for _, d := range r.Domains {
		if err := validateDomain(d); err != nil {
			return fmt.Errorf("certificat %s : %w", r.Name, err)
		}
	}
	if r.Wildcard() && r.Challenge != ChallengeDNS01 && r.Challenge != ChallengeSelfSigned {
		return fmt.Errorf("certificat %s : un joker impose le challenge dns-01 "+
			"(aucune requête HTTP ne peut valider *.domaine)", r.Name)
	}
	if r.Challenge == ChallengeDNS01 && r.DNSProvider == "" && r.DNSCredentials == "" {
		return fmt.Errorf("certificat %s : le challenge dns-01 exige un provider DNS", r.Name)
	}
	if r.DNSCredentials != "" {
		if err := CheckDNSCredentialsName(r.DNSCredentials); err != nil {
			return fmt.Errorf("certificat %s : %w", r.Name, err)
		}
		if r.Challenge == ChallengeHTTP01 {
			return fmt.Errorf("certificat %s : « dnsCredentials » n'a de sens qu'avec le challenge dns-01", r.Name)
		}
	}
	return nil
}

// Method décrit la méthode de validation, pour le plan et les messages.
func (r Request) Method() string {
	switch {
	case r.AutoDNS:
		return "dns-01 par la zone déclarée, repli http-01"
	case r.Challenge == ChallengeDNS01 && r.DNSCredentials != "":
		return "dns-01, identifiants " + r.DNSCredentials
	}
	return string(r.Challenge)
}

// servesChallenge dit si l'émission peut passer par le vhost — HTTP-01, ou son
// repli : le vhost doit alors être chargé, non seulement écrit.
func (r Request) servesChallenge() bool {
	return r.Challenge == ChallengeHTTP01 || r.AutoDNS
}

// validateDomain vérifie un nom, joker compris. Un joker ne couvre qu'un seul
// niveau de sous-domaine : accepter *.a.b.net alors que le certificat ne
// couvrirait pas c.d.a.b.net produirait des erreurs difficiles à comprendre.
func validateDomain(d string) error {
	name := d
	if strings.HasPrefix(d, "*.") {
		name = strings.TrimPrefix(d, "*.")
		if strings.Contains(name, "*") {
			return fmt.Errorf("domaine %q : un seul joker admis, en première position", d)
		}
	} else if strings.Contains(d, "*") {
		return fmt.Errorf("domaine %q : le joker doit être le premier label", d)
	}
	if name == "" || len(name) > 253 {
		return fmt.Errorf("domaine %q invalide", d)
	}
	if net.ParseIP(name) != nil {
		return fmt.Errorf("domaine %q : une adresse IP ne peut pas figurer dans un certificat ACME", d)
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("domaine %q : label invalide", d)
		}
	}
	if !strings.Contains(name, ".") {
		return fmt.Errorf("domaine %q : au moins deux labels attendus", d)
	}
	return nil
}

// Issuer émet un certificat.
type Issuer interface {
	Name() string
	Issue(ctx context.Context, req Request) (Material, error)
	Revoke(ctx context.Context, req Request, cert []byte) error
}

// SelfSigned émet des certificats auto-signés, pour le développement et pour
// disposer d'un certificat immédiat avant qu'ACME ne prenne le relais.
type SelfSigned struct {
	// Validity est la durée de vie du certificat produit.
	Validity time.Duration
	// Now est injectable pour rendre les tests déterministes.
	Now func() time.Time
}

// Name identifie l'émetteur.
func (s SelfSigned) Name() string { return "auto-signé" }

func (s SelfSigned) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Issue produit un certificat auto-signé couvrant les domaines demandés.
func (s SelfSigned) Issue(_ context.Context, req Request) (Material, error) {
	if err := req.Validate(); err != nil {
		return Material{}, err
	}
	validity := s.Validity
	if validity == 0 {
		validity = 90 * 24 * time.Hour
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Material{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Material{}, err
	}
	now := s.now()
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: req.Domains[0], Organization: []string{"ogs (auto-signé)"}},
		DNSNames:              req.Domains,
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return Material{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return Material{}, err
	}
	return Material{
		Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		PrivateKey:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// Revoke n'a pas de sens pour un certificat auto-signé.
func (s SelfSigned) Revoke(context.Context, Request, []byte) error { return nil }
