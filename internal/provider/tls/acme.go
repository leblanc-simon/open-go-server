package tls

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/dns/ovh"
	"github.com/go-acme/lego/v4/providers/http/webroot"
	"github.com/go-acme/lego/v4/registration"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/fs"
)

// DNSProviderOVH est le seul provider DNS géré (D27).
const DNSProviderOVH = "ovh"

// DirectoryStaging est l'environnement de test de Let's Encrypt. Il délivre des
// certificats non reconnus mais sans quota serré : c'est là qu'on éprouve une
// configuration avant de consommer les quotas de production.
const DirectoryStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"

// ACME émet des certificats via le protocole ACME, avec lego embarqué : aucune
// dépendance à certbot, donc contrôle complet du cycle de vie.
type ACME struct {
	Paths config.Paths
	Store Store
	// Email est le compte de contact déclaré à l'autorité de certification.
	Email string
	// Directory permet de pointer une autre autorité (test, ACME privé).
	Directory string
	// DNSCredentials fournit les identifiants d'un provider DNS.
	DNSCredentials func(provider string) (map[string]string, error)
	Now            func() time.Time
}

// Name identifie l'émetteur.
func (a *ACME) Name() string { return "ACME" }

func (a *ACME) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// account porte l'enregistrement ACME et sa clé privée.
type account struct {
	Email        string                 `json:"email"`
	Registration *registration.Resource `json:"registration"`
	key          crypto.PrivateKey
}

func (a *account) GetEmail() string                        { return a.Email }
func (a *account) GetRegistration() *registration.Resource { return a.Registration }
func (a *account) GetPrivateKey() crypto.PrivateKey        { return a.key }

// directory renvoie l'URL de l'autorité à interroger.
func (a *ACME) directory(staging bool) string {
	if a.Directory != "" {
		return a.Directory
	}
	if staging {
		return DirectoryStaging
	}
	return lego.LEDirectoryProduction
}

// accountDir isole les comptes par autorité et par courriel : un compte de test
// ne doit jamais servir en production.
func (a *ACME) accountDir(directory string) string {
	host := directory
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	return filepath.Join(a.Paths.ACMEDir(), "accounts", host, a.Email)
}

// loadOrCreateAccount charge le compte ACME ou l'enregistre au premier usage.
func (a *ACME) loadOrCreateAccount(directory string) (*lego.Client, error) {
	if a.Email == "" {
		return nil, fmt.Errorf("aucun courriel de contact configuré : " +
			"l'autorité de certification en exige un pour notifier les expirations")
	}
	dir := a.accountDir(directory)
	if err := fs.EnsureDir(dir, 0o700, fs.KeepOwner); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, "account.key")
	regPath := filepath.Join(dir, "account.json")

	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	acc := &account{Email: a.Email, key: key}
	if data, err := os.ReadFile(regPath); err == nil {
		if err := json.Unmarshal(data, acc); err != nil {
			return nil, fmt.Errorf("%s illisible : %w", regPath, err)
		}
	}

	cfg := lego.NewConfig(acc)
	cfg.CADirURL = directory
	cfg.Certificate.KeyType = certcrypto.EC256
	client, err := lego.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if acc.Registration == nil {
		reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
		if err != nil {
			return nil, fmt.Errorf("enregistrement du compte ACME : %w", err)
		}
		acc.Registration = reg
		data, err := json.MarshalIndent(acc, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := fs.WriteAtomic(regPath, data, 0o600, fs.KeepOwner); err != nil {
			return nil, err
		}
	}
	return client, nil
}

// loadOrCreateKey lit la clé du compte ou la génère. Elle est en 0600 : elle
// vaut le contrôle du compte ACME.
func loadOrCreateKey(path string) (crypto.PrivateKey, error) {
	if data, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("%s : clé PEM illisible", path)
		}
		return x509.ParseECPrivateKey(block.Bytes)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := fs.WriteAtomic(path, pemData, 0o600, fs.KeepOwner); err != nil {
		return nil, err
	}
	return key, nil
}

// keyType traduit le type de clé demandé.
func keyType(v string) certcrypto.KeyType {
	switch strings.ToLower(v) {
	case "rsa2048":
		return certcrypto.RSA2048
	case "rsa4096":
		return certcrypto.RSA4096
	case "ec384":
		return certcrypto.EC384
	default:
		return certcrypto.EC256
	}
}

// Issue obtient un certificat auprès de l'autorité.
func (a *ACME) Issue(ctx context.Context, req Request) (Material, error) {
	if err := req.Validate(); err != nil {
		return Material{}, err
	}
	directory := a.directory(req.Staging)
	client, err := a.loadOrCreateAccount(directory)
	if err != nil {
		return Material{}, err
	}
	if err := a.setupChallenge(client, req); err != nil {
		return Material{}, err
	}
	res, err := client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: req.Domains,
		Bundle:  true,
	})
	if err != nil {
		return Material{}, fmt.Errorf("émission refusée par l'autorité : %w", err)
	}
	cert, chain := splitBundle(res.Certificate)
	return Material{Certificate: cert, Chain: chain, PrivateKey: res.PrivateKey}, nil
}

// setupChallenge branche la méthode de validation.
func (a *ACME) setupChallenge(client *lego.Client, req Request) error {
	switch req.Challenge {
	case ChallengeHTTP01:
		// Webroot partagé : le vhost sert /.well-known/acme-challenge avant toute
		// redirection, donc aucun serveur temporaire n'est nécessaire et le site
		// reste en ligne pendant la validation.
		dir := a.Paths.ACMEChallenge()
		if err := fs.EnsureDir(dir, 0o755, fs.KeepOwner); err != nil {
			return err
		}
		provider, err := webroot.NewHTTPProvider(dir)
		if err != nil {
			return err
		}
		return client.Challenge.SetHTTP01Provider(provider)
	case ChallengeDNS01:
		provider, err := a.dnsProvider(req.DNSProvider)
		if err != nil {
			return err
		}
		return client.Challenge.SetDNS01Provider(provider)
	}
	return fmt.Errorf("challenge %q non pris en charge par l'émetteur ACME", req.Challenge)
}

// dnsProvider construit le provider DNS. OVH est le seul géré (D27) : la zone de
// préproduction y est hébergée, et un provider non éprouvé donnerait une émission
// qui échoue au moment le moins opportun.
func (a *ACME) dnsProvider(name string) (challenge.Provider, error) {
	if name != DNSProviderOVH {
		return nil, fmt.Errorf("provider DNS %q non géré : ogs ne gère qu'%q. "+
			"Un autre provider demande d'ajouter sa configuration et de l'éprouver",
			name, DNSProviderOVH)
	}
	if a.DNSCredentials == nil {
		return nil, fmt.Errorf("aucune source d'identifiants DNS configurée")
	}
	creds, err := a.DNSCredentials(name)
	if err != nil {
		return nil, err
	}
	cfg := ovh.NewDefaultConfig()
	cfg.APIEndpoint = orDefault(creds["endpoint"], "ovh-eu")
	// Deux modes d'authentification coexistent chez OVH : les clés applicatives
	// historiques et les identifiants OAuth2 récents.
	switch {
	case creds["clientId"] != "":
		cfg.OAuth2Config = &ovh.OAuth2Config{
			ClientID:     creds["clientId"],
			ClientSecret: creds["clientSecret"],
		}
	case creds["applicationKey"] != "":
		cfg.ApplicationKey = creds["applicationKey"]
		cfg.ApplicationSecret = creds["applicationSecret"]
		cfg.ConsumerKey = creds["consumerKey"]
	default:
		return nil, fmt.Errorf("identifiants OVH incomplets : fournir soit clientId et clientSecret, " +
			"soit applicationKey, applicationSecret et consumerKey")
	}
	return ovh.NewDNSProviderConfig(cfg)
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// Revoke révoque un certificat auprès de l'autorité.
func (a *ACME) Revoke(ctx context.Context, req Request, cert []byte) error {
	client, err := a.loadOrCreateAccount(a.directory(req.Staging))
	if err != nil {
		return err
	}
	return client.Certificate.Revoke(cert)
}

// splitBundle sépare le certificat de sa chaîne : lego renvoie l'ensemble
// concaténé, alors que les serveurs web attendent souvent les deux séparément.
func splitBundle(bundle []byte) (cert, chain []byte) {
	rest := bundle
	first := true
	for len(rest) > 0 {
		block, remaining := pem.Decode(rest)
		if block == nil {
			break
		}
		encoded := pem.EncodeToMemory(block)
		if first && block.Type == "CERTIFICATE" {
			cert = encoded
			first = false
		} else {
			chain = append(chain, encoded...)
		}
		rest = remaining
	}
	if cert == nil {
		return bundle, nil
	}
	return cert, chain
}
