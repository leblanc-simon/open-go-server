// Package tls porte la gestion des certificats : stockage, décision de
// renouvellement et émission (ACME ou auto-signée).
package tls

import (
	"context"
	cryptotls "crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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

// sharedDirPrefix préfixe le répertoire d'un certificat partagé.
const sharedDirPrefix = "_shared-"

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
		return sharedDirPrefix + strings.TrimPrefix(name, SharedPrefix)
	}
	return name
}

// CheckProjectName refuse un nom de certificat de projet qui sortirait de
// l'espace des projets (D94) : « @… » désigne un certificat partagé,
// « _shared-… » est son répertoire, et « / » ou un « . » en tête mèneraient hors
// du magasin. Le nom devient un répertoire où l'émission écrit une clé privée,
// et que la compensation d'une émission ratée supprime en entier. C'est la
// règle de validate.CertificateNames, reprise ici pour que le magasin ne
// dépende pas d'une validation faite ailleurs ; tout autre nom, « *.cc.fr »
// compris, reste accepté.
func CheckProjectName(name string) error {
	switch {
	case name == "", strings.ContainsAny(name, "/\x00"), strings.HasPrefix(name, "."),
		IsShared(name), strings.HasPrefix(name, sharedDirPrefix):
		return fmt.Errorf("nom de certificat %q invalide : il sortirait du répertoire des certificats "+
			"du projet — un projet ne déclare pas de certificat partagé", name)
	}
	return nil
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
	// Staging signale un certificat de l'environnement de test de Let's Encrypt,
	// qu'aucun navigateur ne reconnaît : il est remplacé dès qu'acme.staging est
	// retiré (D110).
	Staging bool `json:"staging,omitempty" yaml:"staging,omitempty"`
	// Unpaired signale une clé qui ne correspond pas au certificat en place
	// (D110) : le rendu la tient pour absente, la liste et les alertes la
	// nomment. Repairable, quand des fichiers en attente achèvent l'écriture.
	Unpaired   bool   `json:"unpaired,omitempty" yaml:"unpaired,omitempty"`
	Repairable bool   `json:"repairable,omitempty" yaml:"repairable,omitempty"`
	Path       string `json:"path" yaml:"path"`
}

// isStagingIssuer reconnaît un émetteur de l'environnement de test de Let's
// Encrypt d'après son nom : « (STAGING) Ersatz Edamame E1 », « (STAGING)
// Counterfeit Cashew R10 », et, avant 2020, « Fake LE Intermediate X1 ». Le nom
// est celui que l'autorité publie pour cet usage ; aucun certificat de
// production ne le porte.
func isStagingIssuer(cn string) bool {
	return strings.HasPrefix(cn, "(STAGING)") || strings.HasPrefix(cn, "Fake LE ")
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

// Read lit le certificat que sert un vhost rendu maintenant. Une paire dont la
// clé ne correspond pas au certificat est tenue pour absente (D110) : un vhost
// rendu sur elle ferait refuser au moteur la configuration entière — « key
// values mismatch » sous nginx, un maître Apache qui meurt au rechargement alors
// qu'« apachectl -t » répond « Syntax OK » —, et c'est le parc qui tomberait, non
// ce seul site. Une écriture interrompue que les fichiers en attente achèvent se
// lit, elle, comme achevée (Pending) : l'étape qui l'achève précède les vhosts
// (Provider.PlanPairs), et rendre ces derniers en HTTP seul le temps de l'apply
// ferait refuser par nginx l'écriture du premier, tant qu'un second vhost
// désigne encore la paire.
func (s Store) Read(name string) (Info, bool, error) {
	st, err := s.state(context.Background(), name)
	if err != nil {
		return Info{}, false, err
	}
	switch {
	case st.present && st.paired:
		return st.info, true, nil
	case st.pending != nil:
		return *st.pending, true, nil
	}
	return Info{}, false, nil
}

// ReadPair lit le certificat en place et dit si sa clé lui correspond. La
// lecture se fait sous le verrou partagé du certificat : une écriture en cours
// n'y laisse jamais voir une paire en transit. Elle ne crée rien — « plan » et
// « diff » n'écrivent pas. Une paire dépareillée porte Unpaired, et Repairable
// quand des fichiers en attente l'achèvent.
func (s Store) ReadPair(name string) (info Info, present, paired bool, err error) {
	st, err := s.state(context.Background(), name)
	if err != nil {
		return Info{}, false, false, err
	}
	return st.info, st.present, st.paired, nil
}

// Pending rend le certificat qu'achève une écriture interrompue — clé neuve en
// place, chaîne neuve en attente —, faux si la paire est appariée ou si rien ne
// l'achève sans émission.
func (s Store) Pending(name string) (Info, bool, error) {
	st, err := s.state(context.Background(), name)
	if err != nil || st.pending == nil {
		return Info{}, false, err
	}
	return *st.pending, true, nil
}

// pairState est l'état d'une paire relu sous verrou.
type pairState struct {
	info            Info
	present, paired bool
	// pending est le certificat qu'achèveraient les fichiers en attente.
	pending *Info
}

func (s Store) state(ctx context.Context, name string) (pairState, error) {
	unlock, err := s.lock(ctx, name, syscall.LOCK_SH, false)
	if err != nil {
		return pairState{}, err
	}
	defer unlock()
	return s.readState(name)
}

func (s Store) readState(name string) (pairState, error) {
	info, present, paired, err := s.readPair(name)
	st := pairState{info: info, present: present, paired: paired}
	if err != nil || !present || paired {
		return st, err
	}
	st.info.Unpaired = true
	if pending, ok := s.pendingPair(name); ok {
		st.pending = &pending
		st.info.Repairable = true
	}
	return st, nil
}

// pendingPair lit la chaîne en attente et la clé qui l'accompagne — en attente
// elle aussi, ou déjà renommée — et rend le certificat qu'elles forment, s'il
// s'appareillent.
func (s Store) pendingPair(name string) (Info, bool) {
	dir := s.Dir(name)
	key, err := os.ReadFile(filepath.Join(dir, FileKey+stagedSuffix))
	if err != nil {
		key, _ = os.ReadFile(filepath.Join(dir, FileKey))
	}
	full, err := os.ReadFile(filepath.Join(dir, FileFullchain+stagedSuffix))
	if err != nil || !pairs(full, key) {
		return Info{}, false
	}
	info, err := s.parseInfo(name, full)
	if err != nil {
		return Info{}, false
	}
	return info, true
}

func (s Store) readPair(name string) (Info, bool, bool, error) {
	path := s.Path(name, FileFullchain)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Info{}, false, false, nil
		}
		return Info{}, false, false, err
	}
	info, err := s.parseInfo(name, data)
	if err != nil {
		return Info{}, false, false, fmt.Errorf("%s : %w", path, err)
	}
	key, err := os.ReadFile(s.Path(name, FileKey))
	if err != nil && !os.IsNotExist(err) {
		return Info{}, false, false, err
	}
	return info, true, err == nil && pairs(data, key), nil
}

// parseInfo décrit le premier certificat d'une chaîne.
func (s Store) parseInfo(name string, data []byte) (Info, error) {
	cert, err := parseFirst(data)
	if err != nil {
		return Info{}, err
	}
	info := Info{
		Name:      name,
		Shared:    IsShared(name),
		Domains:   cert.DNSNames,
		NotBefore: cert.NotBefore,
		NotAfter:  cert.NotAfter,
		Issuer:    cert.Issuer.CommonName,
		SelfSign:  cert.Issuer.CommonName == cert.Subject.CommonName,
		Staging:   isStagingIssuer(cert.Issuer.CommonName),
		Path:      s.Dir(name),
	}
	if len(info.Domains) == 0 && cert.Subject.CommonName != "" {
		info.Domains = []string{cert.Subject.CommonName}
	}
	return info, nil
}

// pairs dit si une clé privée correspond au premier certificat d'une chaîne :
// c'est le contrôle que les moteurs font au chargement.
func pairs(fullchain, key []byte) bool {
	_, err := cryptotls.X509KeyPair(fullchain, key)
	return err == nil
}

// lockBudget borne l'attente d'un verrou de certificat sur l'opération entière.
// Une écriture ne le tient que le temps de quatre fichiers ; au-delà, quelqu'un
// le garde — un processus bloqué, un « flock » posé à la main —, et attendre sans
// borne figerait l'apply, le timer et le verrou global avec eux.
var lockBudget = 30 * time.Second

// lockPoll est l'intervalle entre deux tentatives.
const lockPoll = 20 * time.Millisecond

// lock pose le verrou d'un certificat sur son répertoire même : aucun fichier de
// verrou à créer, donc une lecture qui n'écrit rien. Un répertoire absent n'a
// rien à protéger en lecture ; l'écriture le crée d'abord (create). L'attente est
// bornée (lockBudget) et suit le contexte : une interruption l'abandonne.
func (s Store) lock(ctx context.Context, name string, how int, create bool) (func(), error) {
	dir := s.Dir(name)
	if create {
		if err := fs.EnsureDir(dir, 0o750, fs.KeepOwner); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if !create && (os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR)) {
			return func() {}, nil
		}
		return nil, err
	}
	deadline := time.Now().Add(lockBudget)
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("verrou du certificat %s : %w", name, err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("verrou du certificat %s tenu depuis plus de %s par un autre processus "+
				"(émission en cours, ou verrou posé à la main sur %s)", name, lockBudget, dir)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("verrou du certificat %s : %w", name, ctx.Err())
		case <-time.After(lockPoll):
		}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// List énumère les certificats présents, une paire dépareillée comprise
// (Info.Unpaired) : « cert list », les alertes d'échéance et « doctor » la
// nomment, là où le rendu la tient pour absente.
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
		info, present, _, err := s.ReadPair(name)
		if err != nil || !present {
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

// stagedSuffix marque un fichier écrit, en attente de son renommage : une
// écriture interrompue entre deux renommages se reconnaît à lui, et s'achève
// (Repair).
const stagedSuffix = ".ogs-new"

// pairOrder est l'ordre des renommages : la clé et la chaîne complète — la paire
// que lisent les vhosts — d'abord, puis le certificat seul et l'intermédiaire.
var pairOrder = []string{FileKey, FileFullchain, FileCert, FileChain}

// Write enregistre un certificat. La clé privée est en 0600 et le certificat en
// 0644 : le serveur web lit la clé en tant que root, avant d'abandonner ses
// privilèges, et n'a donc pas besoin qu'elle soit lisible plus largement.
//
// Les chemins que lisent les vhosts restent ceux du répertoire du certificat
// (D110) : les changer réécrirait tout le parc. L'écriture se fait donc sous le
// verrou exclusif du certificat — deux émissions concurrentes, l'apply et le
// timer, entrelaçaient sinon leurs fichiers, la clé de l'une avec le certificat
// de l'autre —, refuse une paire dépareillée avant toute écriture, pose chaque
// fichier à côté sous un nom d'attente, puis les renomme. Une interruption ne
// peut plus laisser qu'une écriture inachevée, que Repair achève.
func (s Store) Write(name string, m Material) error {
	return s.WriteContext(context.Background(), name, m)
}

// WriteContext est Write, l'attente du verrou suivant le contexte.
func (s Store) WriteContext(ctx context.Context, name string, m Material) error {
	full := append(append([]byte{}, m.Certificate...), m.Chain...)
	if !pairs(full, m.PrivateKey) {
		return fmt.Errorf("certificat %s : la clé privée émise ne correspond pas au certificat — "+
			"le certificat en place est conservé", name)
	}
	unlock, err := s.lock(ctx, name, syscall.LOCK_EX, true)
	if err != nil {
		return err
	}
	defer unlock()
	dir := s.Dir(name)
	files := map[string]struct {
		data []byte
		mode os.FileMode
	}{
		FileCert:      {m.Certificate, 0o644},
		FileChain:     {m.Chain, 0o644},
		FileFullchain: {full, 0o644},
		FileKey:       {m.PrivateKey, 0o600},
	}
	var staged []string
	for _, f := range pairOrder {
		if len(files[f].data) == 0 && f == FileChain {
			// Un certificat auto-signé n'a pas de chaîne : le fichier reste absent
			// plutôt que vide, pour ne pas faire échouer le serveur web au chargement.
			continue
		}
		path := filepath.Join(dir, f)
		if err := fs.WriteAtomic(path+stagedSuffix, files[f].data, files[f].mode, fs.KeepOwner); err != nil {
			for _, p := range staged {
				_ = os.Remove(p + stagedSuffix)
			}
			return err
		}
		staged = append(staged, path)
	}
	for _, path := range staged {
		if err := os.Rename(path+stagedSuffix, path); err != nil {
			return err
		}
	}
	return syncDir(dir)
}

// Repair achève une écriture interrompue entre deux renommages : la clé neuve en
// place, le certificat neuf encore en attente. Il rend vrai si la paire est
// appariée au retour. Une paire dépareillée sans fichier en attente qui
// l'achève — recopiée à la main — reste à réémettre.
func (s Store) Repair(ctx context.Context, name string) (bool, error) {
	unlock, err := s.lock(ctx, name, syscall.LOCK_EX, false)
	if err != nil {
		return false, err
	}
	defer unlock()
	st, err := s.readState(name)
	if err != nil || !st.present || st.paired {
		return st.present && st.paired, err
	}
	dir := s.Dir(name)
	if st.pending == nil {
		// Les fichiers en attente n'achèvent pas la paire en place : les renommer
		// remplacerait une paire par une autre, aussi dépareillée.
		return false, nil
	}
	for _, f := range pairOrder {
		path := filepath.Join(dir, f)
		if err := os.Rename(path+stagedSuffix, path); err != nil && !os.IsNotExist(err) {
			return false, err
		}
	}
	if err := syncDir(dir); err != nil {
		return false, err
	}
	_, present, paired, err := s.readPair(name)
	return present && paired, err
}

// syncDir rend durables les renommages d'un répertoire.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}

// Remove supprime un certificat du disque, sous son verrou : une émission en
// cours ne le recrée pas à demi.
func (s Store) Remove(name string) error {
	unlock, err := s.lock(context.Background(), name, syscall.LOCK_EX, false)
	if err != nil {
		return err
	}
	defer unlock()
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
