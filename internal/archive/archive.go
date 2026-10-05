// Package archive crée et relit les archives de projet, produites avant toute
// suppression.
package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"

	"leblanc.io/open-go-server/internal/fs"
)

// Extension est le suffixe des archives produites.
const Extension = ".tar.zst"

// ManifestName est le nom du manifeste dans l'archive. Il vient en premier, de
// sorte qu'un « tar -tf » ou un « zstdcat | head » le montre immédiatement.
const ManifestName = "manifest.json"

// Version du format d'archive : elle permet de relire les archives anciennes, et
// de refuser celles qu'on ne sait pas lire. La 2 porte le nom exact de chaque
// secret dans sa charge, le chemin ne suffisant pas à le restituer. Une archive
// de version supérieure est refusée plutôt que relue de travers : un secret
// réinjecté sous un nom faux ne se rouvre plus jamais, sa donnée associée le
// liant à son nom (secret.aad).
const Version = 2

// checkVersion refuse une archive écrite par un ogs plus récent. Le manifeste est
// le seul endroit qui dise le format ; une version absente est celle des archives
// antérieures au champ, que le repli sait relire.
func checkVersion(path string, version int) error {
	if version > Version {
		return fmt.Errorf("%s : archive au format %d, cet ogs ne relit que le format %d "+
			"— mettre ogs à jour avant de restaurer", path, version, Version)
	}
	return nil
}

// Manifest décrit ce que contient l'archive et ce qui a été supprimé.
type Manifest struct {
	Version    int       `json:"version"`
	Project    string    `json:"project"`
	Host       string    `json:"host"`
	Env        string    `json:"env"`
	CreatedAt  time.Time `json:"createdAt"`
	OGSVersion string    `json:"ogsVersion"`
	// Reason décrit l'opération à l'origine de l'archive.
	Reason string `json:"reason"`
	// Entries énumère le contenu, par catégorie.
	Entries []Entry `json:"entries"`
	// Removed énumère les ressources supprimées du système.
	Removed []string `json:"removed"`
	// Secrets est le nombre de secrets inclus ; ils restent chiffrés.
	Secrets int `json:"secrets"`
	// Bytes est la taille totale des données archivées, avant compression.
	Bytes int64 `json:"bytes"`
	// Owners donne, par préfixe, le propriétaire de la racine de chaque
	// arborescence archivée. Les en-têtes de ses entrées portent chacun le leur,
	// et c'est à celui de la racine qu'ils se comparent à la restauration : le
	// compte recréé n'a pas forcément les identifiants d'origine. Absent des
	// archives antérieures, dont les en-têtes ne portaient aucun propriétaire.
	Owners map[string]Owner `json:"owners,omitempty"`
}

// Owner est un propriétaire numérique, tel que l'archivage l'a relevé.
type Owner struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

// RootOwner rend le propriétaire de la racine archivée sous prefix, ou nil si
// l'archive ne le porte pas.
func (m Manifest) RootOwner(prefix string) *fs.Owner {
	o, ok := m.Owners[prefix]
	if !ok {
		return nil
	}
	return &fs.Owner{UID: o.UID, GID: o.GID}
}

// Entry décrit un élément archivé.
type Entry struct {
	// Kind vaut spec, config, dump, secret, tree.
	Kind string `json:"kind"`
	// Name est le chemin dans l'archive.
	Name string `json:"name"`
	// Origin est le chemin d'origine sur le système, s'il existe.
	Origin string `json:"origin,omitempty"`
	Bytes  int64  `json:"bytes"`
}

// Writer construit une archive.
type Writer struct {
	file     *os.File
	zstd     *zstd.Encoder
	tar      *tar.Writer
	manifest Manifest
	path     string
	// leases borne l'attente des baux sur toute l'archive (fs.LeaseBudget) : un
	// locataire qui en pose un par fichier ne tient pas le verrou global plus
	// longtemps qu'un seul.
	leases *fs.LeaseBudget
}

// leaseBudget est le budget d'attente des baux d'une archive ; les tests le
// raccourcissent.
var leaseBudget = fs.DefaultLeaseBudget

// Create ouvre une archive en écriture. Le fichier est en 0600 : il contient les
// données de l'application et ses secrets, même chiffrés.
func Create(path string, manifest Manifest) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("l'archive %s existe déjà : refus de l'écraser", path)
		}
		return nil, err
	}
	enc, err := zstd.NewWriter(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	manifest.Version = Version
	if manifest.CreatedAt.IsZero() {
		manifest.CreatedAt = time.Now()
	}
	return &Writer{file: f, zstd: enc, tar: tar.NewWriter(enc), manifest: manifest, path: path,
		leases: fs.NewLeaseBudget(leaseBudget)}, nil
}

// Path renvoie le chemin de l'archive.
func (w *Writer) Path() string { return w.path }

// AddBytes ajoute un contenu en mémoire.
func (w *Writer) AddBytes(kind, name, origin string, data []byte, mode os.FileMode) error {
	if err := w.tar.WriteHeader(&tar.Header{
		Name: name, Mode: int64(mode.Perm()), Size: int64(len(data)),
		ModTime: time.Now(), Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	if _, err := w.tar.Write(data); err != nil {
		return err
	}
	w.manifest.Entries = append(w.manifest.Entries,
		Entry{Kind: kind, Name: name, Origin: origin, Bytes: int64(len(data))})
	w.manifest.Bytes += int64(len(data))
	return nil
}

// AddFile ajoute un fichier du système.
func (w *Writer) AddFile(kind, name, origin string) error {
	data, err := os.ReadFile(origin)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(origin); err == nil {
		mode = info.Mode().Perm()
	}
	return w.AddBytes(kind, name, origin, data, mode)
}

// AddTree ajoute une arborescence. Les liens symboliques sont conservés tels
// quels : les suivre pourrait faire sortir de l'arborescence du projet, et
// dupliquerait des données déjà archivées.
//
// L'arborescence est le plus souvent un home, qui appartient à l'utilisateur du
// projet (D51) et où son code tourne encore pendant l'archivage — shell ouvert
// (D62), tâches, pool PHP. filepath.Walk atteignait chaque entrée par son chemin
// complet et ouvrait les fichiers par os.Open : un répertoire remplacé par un lien
// une fois sa liste lue, ou un fichier entre son Lstat et son ouverture, faisait
// archiver en root le contenu d'un fichier du système — /etc/ogs/master.key, le
// home d'un voisin —, que la restauration rendait ensuite à l'utilisateur. Le
// parcours se fait donc par descripteurs (fs.Dir) : chaque entrée est ouverte
// relativement au répertoire ouvert, sans suivre de lien, et décrite par le
// descripteur ouvert plutôt que par le constat qui l'a précédé.
//
// Chaque en-tête porte le propriétaire numérique de son entrée, et le manifeste
// celui de la racine (Manifest.Owners) : sans eux, la restauration ne savait pas
// ce qui était au projet, et rendait à l'utilisateur jusqu'au lien dur qu'il
// aurait posé vers un fichier du système.
//
// Le parcours suit ctx avant chaque entrée et chaque bloc lu, et pendant
// l'attente d'un bail : une interruption l'arrête là où elle le trouve, et rend
// ctx.Err() — l'archive, incomplète, est à abandonner. Il ne tenait aucun
// contexte : Ctrl+C restait sans effet des heures durant, le locataire réglant
// la durée de l'archivage à coups de baux (Q-86).
func (w *Writer) AddTree(ctx context.Context, kind, prefix, root string) error {
	dir, err := fs.OpenDir(filepath.Clean(root))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer dir.Close()
	st, err := dir.Stat()
	if err != nil {
		return err
	}
	if w.manifest.Owners == nil {
		w.manifest.Owners = map[string]Owner{}
	}
	uid, gid := ownerOf(st)
	w.manifest.Owners[prefix] = Owner{UID: uid, GID: gid}
	return w.addDir(ctx, dir, prefix)
}

// ownerOf rend le propriétaire numérique relevé par un stat, qu'il vienne d'un
// fs.Dir ou d'un *os.File.
func ownerOf(info os.FileInfo) (uid, gid int) {
	switch st := info.Sys().(type) {
	case *unix.Stat_t:
		return int(st.Uid), int(st.Gid)
	case *syscall.Stat_t:
		return int(st.Uid), int(st.Gid)
	}
	return 0, 0
}

// entryChecked est appelée entre le constat d'un fichier et son ouverture. Elle
// ne fait rien : les tests y substituent un lien au fichier constaté, au moment
// exact que viserait une course.
var entryChecked = func(name string) {}

// addDir archive le contenu d'un répertoire ouvert, sous le nom name.
func (w *Writer) addDir(ctx context.Context, dir *fs.Dir, name string) error {
	names, err := dir.Names()
	if err != nil {
		return err
	}
	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.addEntry(ctx, dir, n, path.Join(name, n)); err != nil {
			return err
		}
	}
	return nil
}

// addEntry archive l'entrée n du répertoire ouvert dir. Une entrée qui disparaît
// ou change de nature entre son constat et son ouverture est passée : son contenu
// n'est plus celui qu'on a constaté, et une entrée que l'utilisateur fait changer
// sous nos pieds n'est pas une raison d'interrompre la suppression.
func (w *Writer) addEntry(ctx context.Context, dir *fs.Dir, n, name string) error {
	info, err := dir.Lstat(n)
	if err != nil {
		return skipChanged(err)
	}
	switch {
	case info.IsDir():
		sub, err := dir.OpenDir(n)
		if err != nil {
			return skipChanged(err)
		}
		defer sub.Close()
		st, err := sub.Stat()
		if err != nil {
			return err
		}
		uid, gid := ownerOf(st)
		if err := w.tar.WriteHeader(&tar.Header{
			Name: name + "/", Mode: int64(st.Mode().Perm()), Uid: uid, Gid: gid,
			ModTime: st.ModTime(), Typeflag: tar.TypeDir,
		}); err != nil {
			return err
		}
		return w.addDir(ctx, sub, name)
	case info.Mode()&os.ModeSymlink != 0:
		target, err := dir.Readlink(n)
		if err != nil {
			return skipChanged(err)
		}
		uid, gid := ownerOf(info)
		return w.tar.WriteHeader(&tar.Header{
			Name: name, Linkname: target, Uid: uid, Gid: gid,
			ModTime: info.ModTime(), Typeflag: tar.TypeSymlink,
		})
	case !info.Mode().IsRegular():
		// Sockets et fifos n'ont pas de sens dans une archive.
		return nil
	}
	entryChecked(name)
	f, err := dir.OpenFileContext(ctx, n, w.leases)
	if err != nil {
		return skipChanged(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	uid, gid := ownerOf(st)
	if err := w.tar.WriteHeader(&tar.Header{
		Name: name, Mode: int64(st.Mode().Perm()), Size: st.Size(), Uid: uid, Gid: gid,
		ModTime: st.ModTime(), Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	// La taille annoncée est celle du descripteur : un fichier qui grandit pendant
	// la lecture n'en donne pas davantage — tar refuserait l'excédent et ferait
	// échouer toute l'archive —, et un fichier raccourci est complété de zéros,
	// comme le fait GNU tar, l'en-tête étant déjà écrit.
	written, err := io.CopyN(w.tar, ctxReader{ctx, f}, st.Size())
	if errors.Is(err, io.EOF) {
		_, err = io.CopyN(w.tar, zeros{}, st.Size()-written)
	}
	if err != nil {
		return err
	}
	w.manifest.Bytes += written
	return nil
}

// skipChanged écarte les erreurs d'une entrée disparue ou changée de nature
// pendant l'archivage — le constat ne vaut plus —, et rend les autres.
func skipChanged(err error) error {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, fs.ErrNotDirectory) ||
		errors.Is(err, fs.ErrNotRegular) || errors.Is(err, syscall.EINVAL) {
		return nil
	}
	return err
}

// ctxReader lit r tant que ctx n'est pas annulé : un fichier de plusieurs
// gigaoctets ne retient pas une interruption jusqu'à sa fin.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// zeros est une source inépuisable d'octets nuls.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// AddSecret ajoute un secret, qui reste chiffré : l'archive ne doit pas être une
// façon d'exfiltrer en clair ce que le keystore protège. Le nom exact voyage dans
// la charge, et le nom de fichier ne sert plus qu'à l'unicité : le déduire du
// chemin corrompait tout nom portant déjà un double souligné — « mysql/shopfr__ro »
// revenait « mysql/shopfr/ro », et un secret réinjecté sous ce nom faux ne
// s'ouvre plus jamais, sa donnée associée le liant à son nom (secret.aad).
func (w *Writer) AddSecret(name string, nonce, ciphertext []byte) error {
	payload, err := json.Marshal(map[string][]byte{
		"name": []byte(name), "nonce": nonce, "ciphertext": ciphertext,
	})
	if err != nil {
		return err
	}
	w.manifest.Secrets++
	return w.AddBytes("secret", "secrets/"+secretFileName(name)+".json", "", payload, 0o600)
}

// secretFileName choisit le codage du nom sous lequel un secret est rangé dans
// l'archive. La charge fait foi à la relecture ; ce nom de fichier ne sert qu'à
// l'unicité — et à ce qu'un ogs antérieur, qui ne lit que le chemin, retrouve
// malgré tout le bon nom après un retour arrière du paquet. D'où le codage
// historique tant qu'il est réversible, et l'échappement d'URL réservé à ce qu'il
// corrompait : un nom portant déjà un double souligné.
func secretFileName(name string) string {
	if !strings.Contains(name, "__") && !strings.Contains(name, "%") {
		return sanitize(name)
	}
	// Le « % » bascule ici lui aussi : sans quoi les deux codages ne se
	// distingueraient plus l'un de l'autre. Et url.PathEscape laissant « _ »
	// intact, on l'échappe à son tour — « mysql__shopfr » et « mysql/shopfr »
	// prétendraient sinon au même fichier.
	return strings.ReplaceAll(url.PathEscape(name), "_", "%5F")
}

// Note enregistre une ressource supprimée du système.
func (w *Writer) Note(removed string) {
	w.manifest.Removed = append(w.manifest.Removed, removed)
}

// Close écrit le manifeste et referme l'archive.
func (w *Writer) Close() (Manifest, error) {
	sort.Slice(w.manifest.Entries, func(i, j int) bool {
		return w.manifest.Entries[i].Name < w.manifest.Entries[j].Name
	})
	data, err := json.MarshalIndent(w.manifest, "", "  ")
	if err != nil {
		return w.manifest, err
	}
	if err := w.tar.WriteHeader(&tar.Header{
		Name: ManifestName, Mode: 0o600, Size: int64(len(data)),
		ModTime: time.Now(), Typeflag: tar.TypeReg,
	}); err != nil {
		return w.manifest, err
	}
	if _, err := w.tar.Write(data); err != nil {
		return w.manifest, err
	}
	if err := w.tar.Close(); err != nil {
		return w.manifest, err
	}
	if err := w.zstd.Close(); err != nil {
		return w.manifest, err
	}
	if err := w.file.Sync(); err != nil {
		return w.manifest, err
	}
	return w.manifest, w.file.Close()
}

// Abort referme et supprime une archive incomplète.
func (w *Writer) Abort() {
	_ = w.tar.Close()
	_ = w.zstd.Close()
	_ = w.file.Close()
	_ = os.Remove(w.path)
}

// ReadManifest lit le manifeste d'une archive sans extraire son contenu.
func ReadManifest(path string) (Manifest, error) {
	var manifest Manifest
	f, err := os.Open(path)
	if err != nil {
		return manifest, err
	}
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		return manifest, err
	}
	defer dec.Close()
	tr := tar.NewReader(dec)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return manifest, err
		}
		if header.Name != ManifestName {
			continue
		}
		if err := json.NewDecoder(tr).Decode(&manifest); err != nil {
			return manifest, err
		}
		return manifest, checkVersion(path, manifest.Version)
	}
	return manifest, fmt.Errorf("%s : manifeste absent, l'archive est-elle complète ?", path)
}

// Extract lit une archive et remet le contenu d'un préfixe donné dans un
// répertoire, et rend le nombre de fichiers et de liens posés. owner donne le
// propriétaire de chaque entrée créée d'après son chemin relatif à dest et celui
// que son en-tête porte — nul dans une archive antérieure à Manifest.Owners ; nil
// laisse celui du processus.
//
// dest est le plus souvent le home d'un projet, qui appartient à son utilisateur
// (D51), et root y écrit après que l'apply y a posé ses liens — vers le trousseau
// commun (D61), l'interpréteur PHP (D70), le répertoire root des journaux (D71).
// L'extraction écrivait par le nom, donc à travers eux : l'authorized_keys
// régulier d'un projet réécrivait le trousseau de tout le parc. Rien n'est donc
// résolu par son nom : chaque composant est ouvert relativement à son parent sans
// suivre de lien (fs.Dir), et une entrée n'est jamais écrite à travers ce qui
// occupe son nom — un fichier ou un lien est retiré, le lien lui-même et jamais
// sa cible, puis l'entrée créée en exclusivité et son propriétaire posé sur son
// descripteur. Aucun chown ne repasse après coup sur une arborescence que
// l'utilisateur modifie peut-être déjà. Un répertoire existant est gardé avec son
// mode et son propriétaire : model.defaultDirs en est le juge.
//
// Une entrée qui ne peut pas être posée ainsi — un répertoire occupe son nom, un
// composant de son chemin est un lien, fût-il posé par l'archive elle-même — est
// écartée sans interrompre les autres, et l'erreur rendue les énumère. Une entrée
// qui sortirait de dest, faille classique des extractions d'archives, interrompt
// tout : l'archive a été fabriquée.
func Extract(file, prefix, dest string, owner func(rel string, orig fs.Owner) fs.Owner) (int, error) {
	f, err := os.Open(file)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		return 0, err
	}
	defer dec.Close()
	if err := os.MkdirAll(dest, 0o750); err != nil {
		return 0, err
	}
	root, err := fs.OpenDir(dest)
	if err != nil {
		return 0, err
	}
	chain := &dirChain{root: root}
	defer chain.close()
	if owner == nil {
		owner = func(string, fs.Owner) fs.Owner { return fs.KeepOwner }
	}
	count := 0
	var refused []string
	tr := tar.NewReader(dec)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, err
		}
		if prefix != "" && header.Name != prefix && !strings.HasPrefix(header.Name, prefix+"/") {
			continue
		}
		rel := path.Clean(strings.TrimLeft(strings.TrimPrefix(header.Name, prefix), "/"))
		if rel == "." {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, "../") {
			return count, fmt.Errorf("entrée %q sortirait du répertoire de destination", header.Name)
		}
		comps := strings.Split(rel, "/")
		name := comps[len(comps)-1]
		mode := os.FileMode(header.Mode).Perm()
		o := owner(rel, fs.Owner{UID: header.Uid, GID: header.Gid})
		var placed bool
		parent, err := chain.open(comps[:len(comps)-1])
		if err == nil {
			switch header.Typeflag {
			case tar.TypeDir:
				err = placeDir(parent, name, mode, o)
			case tar.TypeSymlink:
				err = placeSymlink(parent, name, header.Linkname, o)
				placed = err == nil
			case tar.TypeReg:
				placed, err = placeFile(parent, name, mode, o, tr)
				if err != nil && placed {
					// Le contenu n'a pas pu être recopié : l'archive est illisible
					// ou le disque plein, et les entrées suivantes n'iraient pas
					// mieux.
					return count, err
				}
			}
		}
		if err != nil {
			refused = append(refused, rel+" : "+err.Error())
			continue
		}
		if placed {
			count++
		}
	}
	if len(refused) > 0 {
		shown := refused
		if len(shown) > 5 {
			shown = append(shown[:5:5], fmt.Sprintf("et %d autre(s)", len(refused)-5))
		}
		return count, fmt.Errorf("%d entrée(s) non restaurée(s) — %s", len(refused), strings.Join(shown, " ; "))
	}
	return count, nil
}

// dirChain garde ouverts les répertoires du chemin de la dernière entrée extraite.
// L'archive énumère l'arborescence en profondeur d'abord : les entrées successives
// partagent l'essentiel de leur chemin, et rouvrir chaque composant depuis la
// racine coûterait un openat par niveau et par entrée. Seule la branche courante
// est ouverte, ce qui borne les descripteurs à la profondeur de l'arborescence.
type dirChain struct {
	root  *fs.Dir
	names []string
	dirs  []*fs.Dir
}

// open rend le répertoire désigné par comps, chaque composant ouvert relativement
// au précédent sans suivre de lien. Un composant absent n'est pas créé : l'archive
// nomme chaque répertoire avant son contenu, et son absence dit qu'il a été
// écarté — son contenu l'est avec lui.
func (c *dirChain) open(comps []string) (*fs.Dir, error) {
	keep := 0
	for keep < len(c.names) && keep < len(comps) && c.names[keep] == comps[keep] {
		keep++
	}
	c.truncate(keep)
	for _, comp := range comps[keep:] {
		sub, err := c.current().OpenDir(comp)
		if err != nil {
			return nil, err
		}
		c.names = append(c.names, comp)
		c.dirs = append(c.dirs, sub)
	}
	return c.current(), nil
}

func (c *dirChain) current() *fs.Dir {
	if len(c.dirs) == 0 {
		return c.root
	}
	return c.dirs[len(c.dirs)-1]
}

func (c *dirChain) truncate(n int) {
	for len(c.dirs) > n {
		_ = c.dirs[len(c.dirs)-1].Close()
		c.dirs = c.dirs[:len(c.dirs)-1]
		c.names = c.names[:len(c.names)-1]
	}
}

func (c *dirChain) close() {
	c.truncate(0)
	_ = c.root.Close()
}

// placeDir pose un répertoire de l'archive. Un répertoire existant est gardé tel
// quel ; un fichier ou un lien à sa place est retiré — le lien, pas sa cible.
func placeDir(parent *fs.Dir, name string, mode os.FileMode, owner fs.Owner) error {
	info, err := parent.Lstat(name)
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		if err := parent.Remove(name); err != nil {
			return err
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	sub, err := parent.Mkdir(name, mode, owner)
	if err != nil {
		return err
	}
	return sub.Close()
}

// placeSymlink pose un lien de l'archive, en remplaçant atomiquement le fichier ou
// le lien qui occupe son nom. Un répertoire n'est jamais remplacé par un lien : la
// suite de l'extraction, ou l'apply, écrirait dedans.
func placeSymlink(parent *fs.Dir, name, target string, owner fs.Owner) error {
	if info, err := parent.Lstat(name); err == nil && info.IsDir() {
		return errors.New("un répertoire occupe ce nom")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return parent.Symlink(target, name, owner)
}

// placeFile pose un fichier de l'archive et y recopie son contenu. created dit si
// le fichier a été créé : une erreur rendue ensuite vient de la recopie, et le
// fichier incomplet est retiré.
func placeFile(parent *fs.Dir, name string, mode os.FileMode, owner fs.Owner, content io.Reader) (created bool, err error) {
	info, err := parent.Lstat(name)
	switch {
	case err == nil && info.IsDir():
		return false, errors.New("un répertoire occupe ce nom")
	case err == nil:
		if err := parent.Remove(name); err != nil {
			return false, err
		}
	case !errors.Is(err, os.ErrNotExist):
		return false, err
	}
	out, err := parent.Create(name, mode, owner)
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(out, content); err != nil {
		out.Close()
		_ = parent.Remove(name)
		return true, err
	}
	if err := out.Close(); err != nil {
		_ = parent.Remove(name)
		return true, err
	}
	return true, nil
}

// ReadFile extrait un seul fichier de l'archive, en mémoire.
func ReadFile(path, name string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadEntry(f, name)
}

// maxReadEntry borne ce que ReadEntry rend en mémoire : une déclaration, un
// manifeste. Une sauvegarde, qui peut peser des gigaoctets — ou être une bombe
// zstd fabriquée —, se recopie en flux (CopyEntry).
const maxReadEntry = 16 << 20

// ReadEntry relit une entrée d'une archive déjà ouverte — par un descripteur
// obtenu sans suivre de lien, quand le chemin vient de l'exploitant (D83). Le
// lecteur est consommé.
func ReadEntry(r io.Reader, name string) ([]byte, error) {
	var buf bytes.Buffer
	_, err := CopyEntry(r, name, &limitedWriter{w: &buf, left: maxReadEntry})
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// FindEntry dit si l'archive porte l'entrée, fichier ordinaire, sans en lire le
// contenu.
func FindEntry(r io.Reader, name string) error {
	return withEntry(r, name, func(io.Reader) error { return nil })
}

// CopyEntry recopie en flux une entrée de l'archive vers w.
func CopyEntry(r io.Reader, name string, w io.Writer) (int64, error) {
	var n int64
	err := withEntry(r, name, func(body io.Reader) error {
		var err error
		n, err = io.Copy(w, body)
		return err
	})
	return n, err
}

// CopyFile recopie en flux l'entrée d'une archive désignée par son chemin.
func CopyFile(path, name string, w io.Writer) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return CopyEntry(f, name, w)
}

// withEntry cherche l'entrée et passe son contenu à use. Seul un fichier
// ordinaire est une entrée : un lien symbolique ou un répertoire au même nom,
// dans une archive fabriquée, rendait un contenu vide qu'une restauration
// chargeait en annonçant « ✓ base restaurée ».
func withEntry(r io.Reader, name string, use func(io.Reader) error) error {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return err
	}
	defer dec.Close()
	tr := tar.NewReader(dec)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s absent de l'archive", name)
		}
		if err != nil {
			return err
		}
		if header.Name != name {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fmt.Errorf("%s n'est pas un fichier ordinaire dans l'archive", name)
		}
		return use(tr)
	}
}

// limitedWriter refuse d'écrire au-delà de left octets.
type limitedWriter struct {
	w    io.Writer
	left int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.left {
		return 0, fmt.Errorf("entrée de l'archive trop grande (plus de %d octets)", maxReadEntry)
	}
	l.left -= int64(len(p))
	return l.w.Write(p)
}

// Secrets relit les secrets scellés d'une archive.
func Secrets(path string) (map[string][2][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	out := map[string][2][]byte{}
	tr := tar.NewReader(dec)
	// Le manifeste est écrit en dernier : la version ne se contrôle qu'une fois
	// l'archive parcourue, mais rien n'est rendu avant ce contrôle.
	version := 0
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Name == ManifestName {
			var manifest Manifest
			if err := json.NewDecoder(tr).Decode(&manifest); err != nil {
				return nil, err
			}
			version = manifest.Version
			continue
		}
		if !strings.HasPrefix(header.Name, "secrets/") {
			continue
		}
		var payload map[string][]byte
		if err := json.NewDecoder(tr).Decode(&payload); err != nil {
			return nil, err
		}
		name := string(payload["name"])
		if name == "" {
			// Archive écrite avant que la charge ne porte le nom : le chemin est
			// tout ce qui reste, avec l'ambiguïté du double souligné.
			name = unsanitize(strings.TrimSuffix(strings.TrimPrefix(header.Name, "secrets/"), ".json"))
		}
		out[name] = [2][]byte{payload["nonce"], payload["ciphertext"]}
	}
	if err := checkVersion(path, version); err != nil {
		return nil, err
	}
	return out, nil
}

// FileName construit le nom d'une archive.
func FileName(id string, at time.Time) string {
	return fmt.Sprintf("%s-%s%s", id, at.UTC().Format("20060102-150405"), Extension)
}

// sanitize rend un nom de secret utilisable comme nom de fichier. Le codage n'est
// pas réversible — un nom contenant déjà un double souligné en ressort faussé —,
// d'où sa réserve aux noms qui n'en portent pas (voir secretFileName).
func sanitize(name string) string { return strings.ReplaceAll(name, "/", "__") }

// unsanitize relit le nom de fichier, avec l'ambiguïté du double souligné. Il ne
// sert qu'aux archives antérieures au nom porté dans la charge.
func unsanitize(name string) string { return strings.ReplaceAll(name, "__", "/") }

// Available renvoie l'espace disponible, en octets, sur le système de fichiers
// contenant le chemin donné.
func Available(path string) (uint64, error) { return available(path) }

// Expired énumère les archives dont l'âge dépasse la rétention. Une rétention
// nulle signifie « ne jamais purger » : c'est un choix explicite, pas un défaut.
//
// La date de référence est celle du manifeste, non celle du fichier : une copie
// ou une restauration de sauvegarde change l'horodatage du fichier, pas la date
// à laquelle le projet a été supprimé.
func Expired(dir string, retention time.Duration, now time.Time) ([]string, error) {
	if retention <= 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), Extension) {
			continue
		}
		full := filepath.Join(dir, e.Name())
		created := time.Time{}
		if m, err := ReadManifest(full); err == nil {
			created = m.CreatedAt
		} else if info, err := e.Info(); err == nil {
			// Manifeste illisible : l'archive est inexploitable, mais son âge se
			// déduit encore du fichier. La laisser indéfiniment serait pire.
			created = info.ModTime()
		} else {
			continue
		}
		if now.Sub(created) > retention {
			out = append(out, full)
		}
	}
	sort.Strings(out)
	return out, nil
}
