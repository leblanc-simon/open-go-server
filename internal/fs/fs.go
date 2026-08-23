// Package fs porte l'écriture atomique des fichiers gérés, leur en-tête de
// gestion et la détection des modifications manuelles.
package fs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Marker identifie un fichier produit par ogs.
const Marker = "Généré par ogs"

// Header décrit l'en-tête de gestion d'un fichier généré.
type Header struct {
	Project     string
	Resource    string
	Fingerprint string // empreinte du corps, hors en-tête
	Generated   time.Time
	Template    string
}

var reFingerprint = regexp.MustCompile(`empreinte: sha256:([0-9a-f]{64})`)

// Fingerprint calcule l'empreinte d'un corps de fichier.
func Fingerprint(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Render assemble l'en-tête et le corps. L'empreinte porte sur le corps seul :
// elle permet de distinguer une modification manuelle d'une évolution du rendu.
func Render(h Header, body []byte, comment string) []byte {
	if comment == "" {
		comment = "#"
	}
	fp := Fingerprint(body)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s — NE PAS MODIFIER À LA MAIN\n", comment, Marker)
	fmt.Fprintf(&b, "%s projet: %s  ressource: %s  empreinte: sha256:%s  généré: %s\n",
		comment, h.Project, h.Resource, fp, h.Generated.UTC().Format(time.RFC3339))
	if h.Template != "" {
		fmt.Fprintf(&b, "%s Pour personnaliser : %s\n", comment, h.Template)
	}
	b.Write(body)
	return []byte(b.String())
}

// State décrit l'état d'un fichier sur le disque, par rapport au rendu attendu.
type State int

const (
	// StateAbsent : le fichier n'existe pas.
	StateAbsent State = iota
	// StateCurrent : le contenu correspond exactement au rendu attendu.
	StateCurrent
	// StateOutdated : le fichier est géré par ogs et son contenu diffère du rendu
	// attendu, mais il n'a pas été modifié à la main : il peut être réécrit.
	StateOutdated
	// StateHandEdited : le contenu ne correspond ni au rendu attendu ni à
	// l'empreinte annoncée dans son en-tête. Réécrire exigerait --force.
	StateHandEdited
	// StateForeign : le fichier existe mais ne porte pas la marque ogs.
	StateForeign
)

func (s State) String() string {
	switch s {
	case StateAbsent:
		return "absent"
	case StateCurrent:
		return "conforme"
	case StateOutdated:
		return "à mettre à jour"
	case StateHandEdited:
		return "modifié à la main"
	case StateForeign:
		return "non géré par ogs"
	}
	return "inconnu"
}

// Inspect compare le fichier présent au rendu attendu.
func Inspect(path string, expected []byte) (State, error) {
	current, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return StateAbsent, nil
		}
		return StateAbsent, err
	}
	if string(current) == string(expected) {
		return StateCurrent, nil
	}
	if !strings.Contains(firstLines(current, 3), Marker) {
		return StateForeign, nil
	}
	// Le fichier se déclare géré : son en-tête annonce l'empreinte du corps écrit
	// par ogs. Si le corps ne correspond plus, quelqu'un l'a édité.
	announced, body := splitHeader(current)
	if announced == "" || announced != Fingerprint(body) {
		return StateHandEdited, nil
	}
	return StateOutdated, nil
}

// splitHeader isole l'empreinte annoncée et le corps d'un fichier géré.
func splitHeader(data []byte) (fingerprint string, body []byte) {
	m := reFingerprint.FindSubmatch(data)
	if m != nil {
		fingerprint = string(m[1])
	}
	lines := strings.SplitAfter(string(data), "\n")
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		return fingerprint, []byte(strings.Join(lines[i:], ""))
	}
	return fingerprint, nil
}

func firstLines(data []byte, n int) string {
	lines := strings.SplitN(string(data), "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// Owner décrit le propriétaire cible d'un fichier ou d'un répertoire.
type Owner struct {
	UID int // -1 pour ne pas modifier
	GID int // -1 pour ne pas modifier
}

// KeepOwner laisse le propriétaire inchangé.
var KeepOwner = Owner{UID: -1, GID: -1}

// WriteAtomic écrit un fichier de façon atomique : temporaire dans le même
// répertoire, fsync, mode et propriétaire, puis renommage. Un renommage étant
// atomique sur le même système de fichiers, aucun lecteur ne peut observer un
// contenu partiel.
func WriteAtomic(path string, data []byte, mode os.FileMode, owner Owner) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".ogs-*")
	if err != nil {
		return fmt.Errorf("création du fichier temporaire dans %s : %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if owner.UID >= 0 || owner.GID >= 0 {
		if err := os.Chown(tmpName, owner.UID, owner.GID); err != nil {
			return fmt.Errorf("attribution du propriétaire de %s : %w", path, err)
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = ""
	return syncDir(dir)
}

// Backup copie un fichier existant avant écrasement, en horodatant la copie.
func Backup(path string, now time.Time) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	dst := fmt.Sprintf("%s.ogs-bak-%s", path, now.UTC().Format("20060102-150405"))
	if err := os.WriteFile(dst, data, info.Mode().Perm()); err != nil {
		return "", err
	}
	return dst, nil
}

// EnsureDir crée un répertoire avec le mode et le propriétaire attendus, et les
// corrige s'ils diffèrent. Les modes sont toujours explicites : ils ne dépendent
// jamais du umask ambiant.
func EnsureDir(path string, mode os.FileMode, owner Owner) error {
	info, err := os.Stat(path)
	switch {
	case err == nil && !info.IsDir():
		return fmt.Errorf("%s existe et n'est pas un répertoire", path)
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(path, mode); err != nil {
			return err
		}
	case err != nil:
		return err
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	if owner.UID >= 0 || owner.GID >= 0 {
		if err := os.Chown(path, owner.UID, owner.GID); err != nil {
			return fmt.Errorf("attribution du propriétaire de %s : %w", path, err)
		}
	}
	return nil
}

// ParseMode lit un mode octal écrit sous forme de chaîne (« 0750 »).
func ParseMode(s string) (os.FileMode, error) {
	var m uint32
	if _, err := fmt.Sscanf(s, "%o", &m); err != nil {
		return 0, fmt.Errorf("mode %q illisible", s)
	}
	if m > 0o7777 {
		return 0, fmt.Errorf("mode %q hors plage", s)
	}
	return os.FileMode(m), nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}

// UnifiedDiff produit un diff ligne à ligne, suffisant pour qu'un administrateur
// juge un changement de configuration avant de l'appliquer. Ce n'est pas un
// algorithme de plus longue sous-séquence commune : les fichiers comparés étant
// tous deux générés depuis le même template, les écarts sont locaux et un
// alignement séquentiel les rend correctement.
func UnifiedDiff(before, after, label string) string {
	if before == after {
		return ""
	}
	b := strings.Split(strings.TrimRight(before, "\n"), "\n")
	a := strings.Split(strings.TrimRight(after, "\n"), "\n")
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s (présent)\n+++ %s (attendu)\n", label, label)
	i, j := 0, 0
	for i < len(b) || j < len(a) {
		switch {
		case i < len(b) && j < len(a) && b[i] == a[j]:
			i++
			j++
		case j < len(a) && (i >= len(b) || !contains(b[i:], a[j])):
			fmt.Fprintf(&out, "+%s\n", a[j])
			j++
		case i < len(b):
			fmt.Fprintf(&out, "-%s\n", b[i])
			i++
		default:
			j++
		}
	}
	return out.String()
}

func contains(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}
