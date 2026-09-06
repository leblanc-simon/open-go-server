// Package archive crée et relit les archives de projet, produites avant toute
// suppression.
package archive

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
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
}

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
	return &Writer{file: f, zstd: enc, tar: tar.NewWriter(enc), manifest: manifest, path: path}, nil
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
func (w *Writer) AddTree(kind, prefix, root string) error {
	root = filepath.Clean(root)
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Un fichier disparu pendant l'archivage n'est pas une raison
			// d'interrompre : on le signale dans le manifeste et on continue.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(filepath.Join(prefix, rel))
		switch {
		case info.IsDir():
			return w.tar.WriteHeader(&tar.Header{
				Name: name + "/", Mode: int64(info.Mode().Perm()),
				ModTime: info.ModTime(), Typeflag: tar.TypeDir,
			})
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return w.tar.WriteHeader(&tar.Header{
				Name: name, Linkname: target, ModTime: info.ModTime(), Typeflag: tar.TypeSymlink,
			})
		case !info.Mode().IsRegular():
			// Sockets et fifos n'ont pas de sens dans une archive.
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := w.tar.WriteHeader(&tar.Header{
			Name: name, Mode: int64(info.Mode().Perm()), Size: info.Size(),
			ModTime: info.ModTime(), Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		written, err := io.Copy(w.tar, f)
		if err != nil {
			return err
		}
		w.manifest.Bytes += written
		return nil
	})
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
// répertoire. Les chemins sont vérifiés : une entrée pointant hors du répertoire
// cible est refusée, faille classique des extractions d'archives.
func Extract(path, prefix, dest string) (int, error) {
	f, err := os.Open(path)
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
	root, err := filepath.Abs(dest)
	if err != nil {
		return 0, err
	}
	count := 0
	tr := tar.NewReader(dec)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, err
		}
		if prefix != "" && !strings.HasPrefix(header.Name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(header.Name, prefix)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(rel))
		// Une entrée qui sortirait du répertoire cible est refusée : c'est la
		// faille classique des extractions d'archives.
		if !within(root, target) {
			return count, fmt.Errorf("entrée %q sortirait du répertoire de destination", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode).Perm()); err != nil {
				return count, err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return count, err
			}
			_ = os.Remove(target)
			if err := os.Symlink(header.Linkname, target); err != nil {
				return count, err
			}
			count++
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return count, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY,
				os.FileMode(header.Mode).Perm())
			if err != nil {
				return count, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return count, err
			}
			if err := out.Close(); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, nil
}

// ReadFile extrait un seul fichier de l'archive, en mémoire.
func ReadFile(path, name string) ([]byte, error) {
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
	tr := tar.NewReader(dec)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s absent de l'archive", name)
		}
		if err != nil {
			return nil, err
		}
		if header.Name == name {
			return io.ReadAll(tr)
		}
	}
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

func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

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
		if errors.Is(err, fs.ErrNotExist) {
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
