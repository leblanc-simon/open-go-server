package mail

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"leblanc.io/open-go-server/internal/store"
)

// États d'une clé DKIM. Une clé naît **active** à la création d'un projet — une
// signature invérifiable ne vaut pas moins qu'une absence de signature — et
// **préparée** par une rotation, où elle attend que son enregistrement soit
// publié. La clé qu'elle remplace est alors **retirée** : son fichier reste en
// place le temps de la rétention, son enregistrement devant survivre aux
// courriers encore en transit.
const (
	StateActive  = "active"
	StatePending = "pending"
	StateRetired = "retired"
)

// Key est une clé DKIM, telle qu'elle est scellée dans le keystore. L'état y est
// scellé avec la clé : un seul enregistrement, donc une seule chose à archiver
// et à restaurer avec le projet — une table d'état à côté se serait perdue à la
// restauration, laissant des clés sans statut.
type Key struct {
	Domain    string    `json:"domain"`
	Selector  string    `json:"selector"`
	State     string    `json:"state"`
	Created   time.Time `json:"created"`
	Activated time.Time `json:"activated,omitzero"`
	Retired   time.Time `json:"retired,omitzero"`
	// PEM est la clé privée, telle qu'elle est écrite pour OpenDKIM.
	PEM string `json:"pem"`
}

// Name est le nom DNS de l'enregistrement de la clé.
func (k Key) Name() string { return k.Selector + "._domainkey." + k.Domain }

// secretPrefix nomme les secrets DKIM d'un projet. Le domaine précède le
// sélecteur : c'est l'ordre dans lequel on les cherche.
const secretPrefix = "dkim/"

// SecretName nomme le secret d'une clé dans la portée de son projet.
func SecretName(domain, selector string) string {
	return secretPrefix + domain + "/" + selector
}

// GenerateKey engendre une clé RSA et la rend en PEM (PKCS#8). RSA et non
// ed25519 : la RFC 8463 est encore trop peu vérifiée par les destinataires pour
// servir seule.
func GenerateKey(bits int) (string, error) {
	priv, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// PublicKey rend la partie publique d'une clé, telle que l'enregistrement DKIM
// la porte : la structure SubjectPublicKeyInfo en base64, celle qu'emploie
// opendkim-genkey.
func PublicKey(pemData string) (string, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return "", errors.New("clé DKIM illisible : aucun bloc PEM")
	}
	var pub any
	switch block.Type {
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return "", err
		}
		signer, ok := k.(*rsa.PrivateKey)
		if !ok {
			return "", errors.New("clé DKIM : seules les clés RSA sont gérées")
		}
		pub = &signer.PublicKey
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return "", err
		}
		pub = &k.PublicKey
	default:
		return "", fmt.Errorf("clé DKIM : bloc PEM %q inattendu", block.Type)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// loadKeys lit les clés DKIM scellées d'un projet, triées par domaine puis par
// date de création.
//
// La clé maître n'est exigée que s'il y a une clé à ouvrir : un parc sans clé
// maître — un « ogs init » interrompu, une restauration en cours — ne doit pas
// voir échouer le plan de tous ses projets, signés ou non, dès qu'OpenDKIM est
// installé.
func (p *Provider) loadKeys(projectID string) ([]Key, error) {
	if p.Store == nil {
		return nil, errors.New("base d'état indisponible : impossible de gérer les clés DKIM")
	}
	names, err := p.Store.SecretNames(projectID)
	if err != nil {
		return nil, err
	}
	var out []Key
	for _, name := range names {
		if !strings.HasPrefix(name, secretPrefix) {
			continue
		}
		if p.Keys == nil {
			return nil, errors.New("keystore indisponible : impossible d'ouvrir les clés DKIM")
		}
		k, err := p.openKey(projectID, name)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Domain != out[j].Domain {
			return out[i].Domain < out[j].Domain
		}
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].Selector < out[j].Selector
	})
	return out, nil
}

func (p *Provider) openKey(projectID, name string) (Key, error) {
	nonce, ciphertext, err := p.Store.GetSecret(projectID, name)
	if err != nil {
		return Key{}, err
	}
	plain, err := p.Keys.Open(projectID, name, nonce, ciphertext)
	if err != nil {
		return Key{}, err
	}
	var k Key
	if err := json.Unmarshal(plain, &k); err != nil {
		return Key{}, fmt.Errorf("clé DKIM %s/%s illisible : %w", projectID, name, err)
	}
	return k, nil
}

// sealKey enregistre une clé. Il n'est appelé que depuis l'Apply d'une étape :
// « plan » et « diff » n'écrivent rien, pas même dans le keystore.
func (p *Provider) sealKey(projectID string, k Key) error {
	if p.Store == nil || p.Keys == nil {
		return errors.New("keystore indisponible : impossible de gérer les clés DKIM")
	}
	plain, err := json.Marshal(k)
	if err != nil {
		return err
	}
	name := SecretName(k.Domain, k.Selector)
	nonce, ciphertext, err := p.Keys.Seal(projectID, name, plain)
	if err != nil {
		return err
	}
	return p.Store.PutSecret(projectID, name, nonce, ciphertext)
}

// sealed est une copie brute d'un secret, pour le restaurer à l'identique lors
// d'une compensation.
type sealed struct {
	nonce, ciphertext []byte
}

func (p *Provider) snapshot(projectID string, k Key) (*sealed, error) {
	nonce, ciphertext, err := p.Store.GetSecret(projectID, SecretName(k.Domain, k.Selector))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sealed{nonce: nonce, ciphertext: ciphertext}, nil
}

func (p *Provider) restore(projectID string, k Key, s *sealed) error {
	name := SecretName(k.Domain, k.Selector)
	if s == nil {
		return p.Store.DeleteSecret(projectID, name)
	}
	return p.Store.PutSecret(projectID, name, s.nonce, s.ciphertext)
}

// byDomain regroupe des clés par domaine.
func byDomain(keys []Key) map[string][]Key {
	out := map[string][]Key{}
	for _, k := range keys {
		out[k.Domain] = append(out[k.Domain], k)
	}
	return out
}

// find rend la première clé d'un état donné.
func find(keys []Key, state string) (Key, bool) {
	for _, k := range keys {
		if k.State == state {
			return k, true
		}
	}
	return Key{}, false
}

// nextSelector choisit le sélecteur d'une nouvelle clé : le préfixe, l'année et
// le mois. Une seconde clé dans le même mois reçoit une lettre — un sélecteur
// déjà employé désignerait deux clés différentes pour le même enregistrement.
func nextSelector(prefix string, now time.Time, existing []Key) string {
	taken := map[string]bool{}
	for _, k := range existing {
		taken[k.Selector] = true
	}
	base := prefix + now.UTC().Format("200601")
	if !taken[base] {
		return base
	}
	for c := 'b'; c <= 'z'; c++ {
		if s := base + string(c); !taken[s] {
			return s
		}
	}
	return base + now.UTC().Format("021504")
}
