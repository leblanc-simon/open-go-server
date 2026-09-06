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
	"syscall"
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

// customizeLabel ouvre la troisième ligne d'en-tête, écrite seulement lorsque le
// template est surchargeable. Le libellé sert à la fois à l'écrire et à la
// reconnaître à la relecture : deux littéraux se seraient désaccordés.
const customizeLabel = "Pour personnaliser :"

// reCustomize reconnaît cette troisième ligne. Les seuls préfixes de commentaire
// rendus sont « # » (web, systemd, cron, sudoers, logrotate, Telegraf, fail2ban)
// et « ; » (pool PHP).
var reCustomize = regexp.MustCompile(`^[#;]\s*` + regexp.QuoteMeta(customizeLabel))

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
		fmt.Fprintf(&b, "%s %s %s\n", comment, customizeLabel, h.Template)
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
	// Le corps est intact et c'est bien celui qu'ogs a écrit. Reste à savoir si le
	// rendu attendu porte le même corps : l'en-tête contient un horodatage, qui
	// diffère à chaque calcul. Sans cette comparaison d'empreintes, tout fichier
	// serait déclaré en écart une seconde après avoir été écrit, et chaque apply
	// le réécrirait — en rechargeant le serveur web au passage.
	if expectedFP, _ := splitHeader(expected); expectedFP != "" && expectedFP == announced {
		return StateCurrent, nil
	}
	return StateOutdated, nil
}

// splitHeader isole l'empreinte annoncée et le corps d'un fichier géré. Il ne
// consomme que l'en-tête qu'écrit Render : la ligne d'empreinte, puis la ligne
// « Pour personnaliser » quand elle est là.
//
// Sauter au contraire toute ligne de commentaire de tête amputait le corps des
// fichiers qui commencent eux-mêmes par un commentaire — vhost de supervision,
// fragments Telegraf et fail2ban, extrait nginx, et toute surcharge de template
// dont l'auteur a documenté la première ligne. L'empreinte recalculée ne valait
// alors jamais celle annoncée : le fichier passait pour modifié à la main à
// chaque relecture, donc réécrit et rechargé à chaque apply, et une vraie
// modification manuelle y devenait indiscernable.
//
// L'empreinte est cherchée ligne à ligne, et non dans tout le fichier : un corps
// contenant « empreinte: sha256:… » ne doit pas pouvoir se faire passer pour
// l'en-tête.
func splitHeader(data []byte) (fingerprint string, body []byte) {
	lines := strings.SplitAfter(string(data), "\n")
	idx := -1
	for i, l := range lines {
		if m := reFingerprint.FindStringSubmatch(l); m != nil {
			fingerprint, idx = m[1], i
			break
		}
	}
	if idx < 0 {
		return "", nil
	}
	start := idx + 1
	if start < len(lines) && reCustomize.MatchString(strings.TrimSpace(lines[start])) {
		start++
	}
	if start >= len(lines) {
		return fingerprint, nil
	}
	// SplitAfter conserve les fins de ligne : un corps vide et un corps réduit à
	// « \n » (logrotate sur un parc vide) sont rendus à l'identique.
	return fingerprint, []byte(strings.Join(lines[start:], ""))
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

// BackupTo dépose la sauvegarde dans un répertoire choisi, créé au besoin.
// C'est par là que passe tout écrasement forcé (§6.4), vers
// config.Paths.BackupsDir().
//
// Un répertoire de configuration est toujours lu en entier par quelqu'un, et la
// copie qu'on y laisse ment à ce lecteur. Par le démon d'abord : /etc/logrotate.d
// est chargé tout entier, à l'exception d'une liste de suffixes tabous
// (« .dpkg-old », « .rpmsave », « .disabled »…) où le nôtre ne figure pas — la
// sauvegarde y serait une deuxième rotation portant sur les mêmes journaux, et
// logrotate tient deux directives pour un même chemin pour une erreur : toute la
// rotation du parc s'arrête. Par ogs ensuite, ce que le seul raisonnement sur les
// démons manquait : « doctor » énumère /etc/sudoers.d sur le préfixe « ogs- » et
// accuserait sans fin la copie, l'importateur lit sites-available et proposerait
// d'adopter une seconde fois un site déjà géré. Sortir du répertoire plutôt que
// choisir un suffixe ignoré : la liste des tabous appartient à chaque démon et
// peut changer sous nos pieds, l'emplacement ne dépend de personne.
//
// Le répertoire est créé en 0700, et c'est aussi ce qui rend l'écriture sûre :
// os.WriteFile suit les liens, or l'horodatage se devine à la seconde près. Une
// copie déposée dans un répertoire qu'un non-root possède — le .ssh d'un projet,
// pour la rotation de la clé de déploiement — se ferait détourner par un lien
// posé d'avance à ce nom, et root écrirait le contenu de l'ancienne clé où le
// projet l'aurait décidé. Aucun appelant ne choisit donc son répertoire : tous
// passent par config.Paths.BackupsDir(). Le mode est un filet —
// « ogs init » le pose déjà (config.Paths.Dirs), avec un mode explicite que le
// umask ambiant ne rabote pas, ce que MkdirAll ne garantit pas. Ses parents sont
// supposés déjà posés — /var/lib/ogs l'est en 0751 par « ogs init », et le
// serveur web doit pouvoir le traverser : MkdirAll leur appliquerait sinon le
// 0700 du dernier composant.
func BackupTo(path, dir string, now time.Time) (string, error) {
	// La lecture précède la création du répertoire : un original absent doit
	// rendre une erreur os.IsNotExist — ce dont les appelants se servent pour
	// passer outre — sans laisser derrière lui un répertoire vide.
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, backupName(filepath.Base(path), now))
	if err := os.WriteFile(dst, data, info.Mode().Perm()); err != nil {
		return "", err
	}
	return dst, nil
}

// backupName construit le nom horodaté d'une sauvegarde. Il n'est pas exposé :
// personne d'autre n'a de nom à composer, et le plan — seul endroit qui annonce
// une sauvegarde à l'exploitant — s'affiche avant que l'horodatage existe. Il
// nomme donc le répertoire, qui lui est connu d'avance. Le format ne s'écrit
// qu'ici : horodatage en UTC, et trié alphabétiquement donc chronologiquement.
func backupName(base string, now time.Time) string {
	return fmt.Sprintf("%s.ogs-bak-%s", base, now.UTC().Format("20060102-150405"))
}

// EnsureDir crée un répertoire avec le mode et le propriétaire attendus, et les
// corrige s'ils diffèrent. Les modes sont toujours explicites : ils ne dépendent
// jamais du umask ambiant.
//
// Le dernier composant n'est jamais suivi. Les répertoires d'un projet (www/,
// var/, .ssh/) vivent sous un home que son utilisateur écrit — 0711, à lui
// (§7.1.2) : il peut donc en retirer un et poser à sa place un lien vers
// n'importe quel répertoire du système. os.Stat, os.Chmod et os.Chown résolvent
// les liens, si bien que le mode et le propriétaire du projet auraient été posés
// sur la cible — /etc/cron.d, /etc/systemd/system — soit une escalade vers root
// depuis un compte dont le shell est ouvert (D62). D'où l'ouverture en
// O_NOFOLLOW|O_DIRECTORY puis le mode et le propriétaire posés sur le
// descripteur : ce qui est corrigé est le répertoire constaté, jamais la cible
// d'un lien posé entre l'observation et l'écriture. Même règle que « harden »,
// qui ne suit aucun lien (D56).
func EnsureDir(path string, mode os.FileMode, owner Owner) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s est un lien symbolique : ogs ne le suit pas", path)
	case err == nil && !info.IsDir():
		return fmt.Errorf("%s existe et n'est pas un répertoire", path)
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(path, mode); err != nil {
			return err
		}
	case err != nil:
		return err
	}
	d, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		// ELOOP : le chemin est devenu un lien entre le Lstat et l'ouverture.
		if errors.Is(err, syscall.ELOOP) {
			return fmt.Errorf("%s est un lien symbolique : ogs ne le suit pas", path)
		}
		return fmt.Errorf("ouverture de %s : %w", path, err)
	}
	defer d.Close()
	if err := d.Chmod(mode); err != nil {
		return err
	}
	if owner.UID >= 0 || owner.GID >= 0 {
		if err := d.Chown(owner.UID, owner.GID); err != nil {
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
