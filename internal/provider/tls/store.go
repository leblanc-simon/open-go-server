// Package tls porte la gestion des certificats : stockage, décision de
// renouvellement et émission (ACME ou auto-signée).
package tls

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/fs"
)

// Noms des fichiers d'un certificat, alignés sur les conventions de lego et de
// certbot pour qu'un administrateur retrouve ses repères.
const (
	FileCert      = "cert.pem"
	FileChain     = "chain.pem"
	FileFullchain = "fullchain.pem"
	FileKey       = "privkey.pem"
)

// SharedPrefix marque une référence à un certificat de portée serveur.
const SharedPrefix = "@"

// RenewBefore est le délai avant expiration à partir duquel un certificat est
// renouvelé. Let's Encrypt émet pour 90 jours : 30 jours laissent le temps de
// diagnostiquer un renouvellement en échec avant la coupure.
const RenewBefore = 30 * 24 * time.Hour

// SharedWarnBefore est le seuil d'alerte d'un certificat partagé. Il est plus
// précoce que le seuil de renouvellement : son expiration casserait toutes les
// préproductions à la fois.
const SharedWarnBefore = 21 * 24 * time.Hour

// Store gère l'arborescence des certificats.
type Store struct {
	Paths config.Paths
}

// Dir renvoie le répertoire d'un certificat. Un certificat partagé est stocké
// sous un nom explicite, distinct de l'espace des projets.
func (s Store) Dir(name string) string {
	return filepath.Join(s.Paths.ACMEDir(), "certificates", DirName(name))
}

// DirName traduit un nom de certificat en nom de répertoire lisible.
func DirName(name string) string {
	if strings.HasPrefix(name, SharedPrefix) {
		return "_shared-" + strings.TrimPrefix(name, SharedPrefix)
	}
	return name
}

// IsShared indique si la référence désigne un certificat de portée serveur.
func IsShared(name string) bool { return strings.HasPrefix(name, SharedPrefix) }

// Path renvoie le chemin d'un fichier du certificat.
func (s Store) Path(name, file string) string { return filepath.Join(s.Dir(name), file) }

// Info décrit un certificat présent sur le disque.
type Info struct {
	Name      string    `json:"name" yaml:"name"`
	Shared    bool      `json:"shared" yaml:"shared"`
	Domains   []string  `json:"domains" yaml:"domains"`
	NotBefore time.Time `json:"notBefore" yaml:"notBefore"`
	NotAfter  time.Time `json:"notAfter" yaml:"notAfter"`
	Issuer    string    `json:"issuer" yaml:"issuer"`
	SelfSign  bool      `json:"selfSigned" yaml:"selfSigned"`
	Path      string    `json:"path" yaml:"path"`
}

// DaysLeft renvoie le nombre de jours avant expiration.
func (i Info) DaysLeft(now time.Time) int {
	return int(i.NotAfter.Sub(now).Hours() / 24)
}

// Expired indique si le certificat est arrivé à échéance.
func (i Info) Expired(now time.Time) bool { return !now.Before(i.NotAfter) }

// NeedsRenewal indique si le certificat doit être renouvelé.
func (i Info) NeedsRenewal(now time.Time) bool {
	return now.Add(RenewBefore).After(i.NotAfter)
}

// Covers indique si le certificat couvre l'ensemble des domaines demandés, en
// tenant compte des jokers.
func (i Info) Covers(domains []string) bool {
	for _, want := range domains {
		if !coveredBy(i.Domains, want) {
			return false
		}
	}
	return true
}

// coveredBy applique la règle des wildcards : *.exemple.net couvre a.exemple.net
// mais pas a.b.exemple.net ni exemple.net lui-même.
func coveredBy(names []string, want string) bool {
	want = strings.ToLower(strings.TrimSuffix(want, "."))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSuffix(name, "."))
		if name == want {
			return true
		}
		if !strings.HasPrefix(name, "*.") {
			continue
		}
		suffix := name[1:] // « .exemple.net »
		if !strings.HasSuffix(want, suffix) {
			continue
		}
		label := strings.TrimSuffix(want, suffix)
		if label != "" && !strings.Contains(label, ".") {
			return true
		}
	}
	return false
}

// Read lit le certificat présent, s'il existe.
func (s Store) Read(name string) (Info, bool, error) {
	path := s.Path(name, FileFullchain)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Info{}, false, nil
		}
		return Info{}, false, err
	}
	cert, err := parseFirst(data)
	if err != nil {
		return Info{}, false, fmt.Errorf("%s : %w", path, err)
	}
	info := Info{
		Name:      name,
		Shared:    IsShared(name),
		Domains:   cert.DNSNames,
		NotBefore: cert.NotBefore,
		NotAfter:  cert.NotAfter,
		Issuer:    cert.Issuer.CommonName,
		SelfSign:  cert.Issuer.CommonName == cert.Subject.CommonName,
		Path:      s.Dir(name),
	}
	if len(info.Domains) == 0 && cert.Subject.CommonName != "" {
		info.Domains = []string{cert.Subject.CommonName}
	}
	return info, true, nil
}

// List énumère les certificats présents.
func (s Store) List() ([]Info, error) {
	root := filepath.Join(s.Paths.ACMEDir(), "certificates")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "_shared-") {
			name = SharedPrefix + strings.TrimPrefix(name, "_shared-")
		}
		info, ok, err := s.Read(name)
		if err != nil || !ok {
			continue
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Material est le résultat d'une émission.
type Material struct {
	Certificate []byte
	Chain       []byte
	PrivateKey  []byte
}

// Write enregistre un certificat. La clé privée est en 0600 et le certificat en
// 0644 : le serveur web lit la clé en tant que root, avant d'abandonner ses
// privilèges, et n'a donc pas besoin qu'elle soit lisible plus largement.
func (s Store) Write(name string, m Material) error {
	dir := s.Dir(name)
	if err := fs.EnsureDir(dir, 0o750, fs.KeepOwner); err != nil {
		return err
	}
	full := append(append([]byte{}, m.Certificate...), m.Chain...)
	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{FileCert, m.Certificate, 0o644},
		{FileChain, m.Chain, 0o644},
		{FileFullchain, full, 0o644},
		{FileKey, m.PrivateKey, 0o600},
	}
	for _, f := range files {
		if len(f.data) == 0 && f.name == FileChain {
			// Un certificat auto-signé n'a pas de chaîne : le fichier reste absent
			// plutôt que vide, pour ne pas faire échouer le serveur web au chargement.
			continue
		}
		if err := fs.WriteAtomic(filepath.Join(dir, f.name), f.data, f.mode, fs.KeepOwner); err != nil {
			return err
		}
	}
	return nil
}

// Remove supprime un certificat du disque.
func (s Store) Remove(name string) error {
	return os.RemoveAll(s.Dir(name))
}

// parseFirst décode le premier certificat d'un fichier PEM.
func parseFirst(data []byte) (*x509.Certificate, error) {
	for len(data) > 0 {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		return x509.ParseCertificate(block.Bytes)
	}
	return nil, fmt.Errorf("aucun certificat PEM trouvé")
}
