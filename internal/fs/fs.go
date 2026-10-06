// Package fs porte l'écriture atomique des fichiers gérés, leur en-tête de
// gestion et la détection des modifications manuelles.
package fs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

// reProject lit l'identifiant que Render inscrit dans la seconde ligne d'en-tête.
var reProject = regexp.MustCompile(`^[#;]\s*projet: (\S+)  ressource: `)

// reResource lit la ressource inscrite à sa suite, sur la même ligne.
var reResource = regexp.MustCompile(`  ressource: (\S+)  `)

// ManagedProject rend l'identifiant du projet pour lequel ogs a écrit path, lu
// dans son en-tête, ou "" si le fichier est absent, n'est pas un fichier régulier
// ou ne porte pas la marque d'ogs.
//
// C'est le seul critère d'appartenance qui vaille avant de supprimer un fichier.
// Le nom ne prouve rien : « shop_api.conf » semble prolonger « shop » par un nom
// de pool, alors qu'il est le fichier du projet shop_api — l'attribution par
// préfixe faisait retirer au premier apply de shop le pool et le vhost de son
// voisin. Un lien n'est jamais suivi : un lien n'a pas d'en-tête, et sa cible
// n'est pas lui.
func ManagedProject(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", nil
	}
	// L'en-tête tient dans les premières lignes : lire le fichier entier pour
	// l'attribuer coûterait autant qu'un journal.
	buf := make([]byte, 4096)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	lines := strings.SplitN(string(buf[:n]), "\n", 3)
	if len(lines) < 2 || !strings.Contains(lines[0], Marker) {
		return "", nil
	}
	if m := reProject.FindStringSubmatch(lines[1]); m != nil {
		return m[1], nil
	}
	return "", nil
}

// readMark lit la marque d'ogs là où Inspect la cherche, dans les trois
// premières lignes, puis le projet et la ressource sur la ligne qui la suit. at
// vaut -1 sans marque — une marque qu'aucune ligne ne suit n'est pas un en-tête
// de Render —, et project est vide si cette ligne n'est pas la sienne.
func readMark(data []byte) (at int, project, resource string) {
	// Trois lignes, et le reste : la ligne qui suit la marque peut être la
	// quatrième.
	lines := strings.SplitN(string(data), "\n", 4)
	at = -1
	for i := 0; i < len(lines) && i < 3; i++ {
		if strings.Contains(lines[i], Marker) {
			at = i
			break
		}
	}
	if at < 0 || at+1 >= len(lines) {
		return -1, "", ""
	}
	next, _, _ := strings.Cut(lines[at+1], "\n")
	if m := reProject.FindStringSubmatch(next); m != nil {
		project = m[1]
		if r := reResource.FindStringSubmatch(next); r != nil {
			resource = r[1]
		}
	}
	return at, project, resource
}

// ManagedResource rend le projet et la ressource que l'en-tête d'ogs inscrit
// dans path (« web/vhost/shopfr », « php/pool/shopfr »), ou deux chaînes vides si
// le fichier est absent, n'est pas un fichier régulier ou ne porte pas la marque.
// Un lien n'est jamais suivi.
//
// Le projet seul ne range pas un fichier parmi ceux d'un provider : les fichiers
// de portée parc portent un libellé à sa place, et « parc » — celui du vhost de
// supervision, posé dans sites-available parmi les vhosts des projets — est aussi
// un identifiant valide. Un projet ainsi nommé retirait ogs-status.conf comme
// l'un de ses vhosts obsolètes, que le plan global recréait aussitôt : son plan
// n'était jamais vide, « diff » rendait toujours 6, et la supervision tombait le
// temps de chaque apply.
//
// C'est la lecture des providers, qui cherche la marque là où Inspect et
// InspectRemoval la cherchent : ce que l'un attribue, les autres le jugent.
// Exiger la marque en tête, comme ManagedProject, laissait sans maître le
// fichier d'un voisin dont l'exploitant avait retouché le haut : Inspect le
// tenait pour modifié à la main, et --force le réécrivait au compte du mauvais
// projet. ManagedProject garde cette exigence : il décide seul de ce que la
// suppression d'un projet retire, sans passer par les cinq états, et un fichier
// retouché au-dessus de son en-tête y reste en place.
func ManagedResource(path string) (project, resource string, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
			return "", "", nil
		}
		return "", "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", "", err
	}
	if !st.Mode().IsRegular() {
		return "", "", nil
	}
	// L'en-tête tient dans les premières lignes : lire le fichier entier pour
	// l'attribuer coûterait autant qu'un journal.
	buf := make([]byte, 4096)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", "", err
	}
	_, project, resource = readMark(buf[:n])
	return project, resource, nil
}

// OwnerError signale un fichier qu'ogs a écrit pour un autre projet que celui
// qui s'apprête à l'écrire.
type OwnerError struct {
	Path string
	// Owner est le projet que nomme l'en-tête, Project celui qui voulait écrire.
	Owner, Project string
	// What nomme ce qui voulait l'écrire (« pool « api » de shop »). Il est
	// porté par l'erreur plutôt que par un fmt.Errorf qui l'envelopperait : le
	// message d'un enveloppant est figé à sa création, et Gone ne se sait
	// qu'après.
	What string
	// Gone dit que le propriétaire n'est plus déclaré dans le parc. Seul
	// l'appelant le sait : CheckOwner ne voit qu'un fichier.
	Gone bool
}

// Error nomme les issues : le propriétaire ne le déclare plus, et son apply le
// retire ; ou les deux le déclarent, et l'un doit changer de nom. Un propriétaire
// disparu n'a plus d'apply : y renvoyer l'exploitant le ferait tourner en rond.
func (e *OwnerError) Error() string {
	var msg string
	if e.Gone {
		msg = fmt.Sprintf("%s est le fichier du projet %s, d'après son en-tête, et ce projet n'est plus "+
			"déclaré : %s ne le reprend pas, --force compris — c'est un reste, à retirer à la main s'il est "+
			"sans objet", e.Path, e.Owner, e.Project)
	} else {
		msg = fmt.Sprintf("%s est le fichier du projet %s, d'après son en-tête : %s ne l'écrit pas, --force compris "+
			"(si %s ne le déclare plus, « ogs apply %s » le retire ; sinon, l'un des deux doit changer de nom)",
			e.Path, e.Owner, e.Project, e.Owner, e.Owner)
	}
	if e.What != "" {
		return e.What + " : " + msg
	}
	return msg
}

// CheckOwner refuse d'écrire pour project un fichier qu'ogs a écrit pour un
// autre projet (*OwnerError, dont what nomme l'écrivain).
//
// Le pool « api » et le vhost « b2b » du projet shop s'écrivent dans
// shop_api.conf et shop_b2b.conf, les fichiers principaux des projets shop_api
// et shop_b2b. Le contrôle d'unicité ne voit la collision que sur le parc
// entier, or « apply <id> » ne valide que ses cibles — le chemin ordinaire après
// avoir édité une déclaration : Inspect, qui ignore le projet de l'en-tête,
// tenait le fichier du voisin pour « à mettre à jour », et shop le réécrivait à
// son compte, avec le code 0. --force n'y change rien : il reprend un fichier
// retouché ou jamais écrit par ogs, il ne donne pas à un projet celui d'un autre.
// Le propriétaire est lu comme Inspect lit la marque (ManagedResource).
func CheckOwner(path, project, what string) error {
	owner, _, err := ManagedResource(path)
	if err != nil {
		if what != "" {
			return fmt.Errorf("%s : %w", what, err)
		}
		return err
	}
	if owner != "" && owner != project {
		return &OwnerError{Path: path, Owner: owner, Project: project, What: what}
	}
	return nil
}

// InspectRemoval rend l'état d'un fichier qu'ogs s'apprête à retirer. Aucun
// rendu n'est attendu, et les cinq états s'y lisent donc à l'aune de ce qu'ogs a
// lui-même écrit :
//
//   - StateAbsent : rien à retirer ;
//   - StateCurrent : écrit par ogs pour project, et intact — il se retire ;
//   - StateHandEdited : écrit par ogs pour project, mais retouché depuis — son
//     retrait exige --force et une sauvegarde, comme son écrasement (§6.4) ;
//   - StateForeign : sans la marque d'ogs, marqué pour un autre projet, ou qui
//     n'est pas un fichier régulier — ogs ne le retire jamais.
//
// Un project vide accepte toute marque d'ogs : l'en-tête des fichiers de parc
// porte un libellé (« parc ») ou un décompte de projets, non un identifiant.
//
// La marque est cherchée dans les trois premières lignes, comme Inspect : une
// ligne ajoutée au-dessus de l'en-tête fait un fichier retouché, que --force
// retire après sauvegarde. Exiger la marque en tête l'aurait rendu « non géré »
// au retrait alors qu'Inspect le tient pour modifié à la main à l'écriture —
// réécrit sous --force, mais jamais retirable.
//
// Le retrait passait autrefois par le seul nom : un pool ou un vhost non géré,
// nommé selon la convention, et le fichier retouché d'un exploitant partaient
// sans --force ni copie, alors que les mêmes fichiers étaient refusés à
// l'écriture. Un lien n'est jamais suivi : ce n'est pas lui qu'ogs a écrit.
func InspectRemoval(path, project string) (State, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return StateAbsent, nil
		case errors.Is(err, syscall.ELOOP):
			return StateForeign, nil
		}
		return StateAbsent, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return StateAbsent, err
	}
	if !st.Mode().IsRegular() {
		return StateForeign, nil
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return StateAbsent, err
	}
	at, owner, _ := readMark(data)
	if at < 0 || (project != "" && owner != project) {
		return StateForeign, nil
	}
	if at > 0 {
		// L'empreinte ne couvre que ce qui suit l'en-tête : une ligne posée
		// au-dessus y échappe, et le fichier passerait pour intact.
		return StateHandEdited, nil
	}
	if announced, body := splitHeader(data); announced == "" || announced != Fingerprint(body) {
		return StateHandEdited, nil
	}
	return StateCurrent, nil
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
// l'horodatage se devine à la seconde près. Une copie déposée dans un répertoire
// qu'un non-root possède — le .ssh d'un projet, pour la rotation de la clé de
// déploiement — se ferait détourner par un lien posé d'avance à ce nom dès que
// l'écriture le suit, comme os.WriteFile, et root écrirait le contenu de
// l'ancienne clé où le projet l'aurait décidé. Aucun appelant ne choisit donc
// son répertoire : tous passent par config.Paths.BackupsDir(). Le mode est un
// filet — « ogs init » le pose déjà (config.Paths.Dirs), avec un mode explicite
// que le umask ambiant ne rabote pas, ce que MkdirAll ne garantit pas. Ses parents sont
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
	dst, err := writeBackup(dir, filepath.Base(path), data, info.Mode().Perm(), now)
	if err != nil {
		return "", err
	}
	return dst, nil
}

// BackupDataTo sauvegarde, sous le nom horodaté de base, un contenu déjà lu par
// l'appelant — dans les mêmes conditions que BackupTo, par writeBackup : jamais
// sur une autre sauvegarde, jamais à travers un lien. Il sert quand l'original
// vit dans un répertoire qu'un non-root possède : BackupTo le lirait par son nom,
// donc à travers un lien, là où l'appelant le lit par un descripteur (DirAt).
func BackupDataTo(base string, data []byte, perm os.FileMode, dir string, now time.Time) (string, error) {
	return writeBackup(dir, base, data, perm, now)
}

// backupName construit le nom horodaté d'une sauvegarde. Il n'est pas exposé :
// personne d'autre n'a de nom à composer, et le plan — seul endroit qui annonce
// une sauvegarde à l'exploitant — s'affiche avant que l'horodatage existe. Il
// nomme donc le répertoire, qui lui est connu d'avance. Le format ne s'écrit
// qu'ici : horodatage en UTC, et trié alphabétiquement donc chronologiquement.
func backupName(base string, now time.Time) string {
	return fmt.Sprintf("%s.ogs-bak-%s", base, now.UTC().Format("20060102-150405"))
}

// maxBackupSuffix borne la recherche d'un nom libre : au-delà, c'est le
// répertoire qui est en cause, non la coïncidence de deux sauvegardes.
const maxBackupSuffix = 1000

// writeBackup dépose data dans dir, créé au besoin, sous un nom qu'aucune autre
// sauvegarde n'occupe : le nom horodaté de base, puis, s'il est pris, ce nom
// suffixé de « .1 », « .2 »…
//
// Le nom de base ne désigne pas un fichier : le pool et le vhost d'un projet
// s'appellent tous deux shopfr.conf, son fichier cron et sa délégation sudo
// ogs-shopfr, les fragments Telegraf et fail2ban ogs.conf. Un même apply les
// sauvegarde dans la même seconde — « php switch » sous --force réécrit le vhost
// puis retire l'ancien pool —, et os.WriteFile écrasait sans un mot la première
// copie par la seconde : la retouche du vhost était perdue, alors que le plan
// l'annonçait sauvegardée. O_EXCL ne réutilise jamais un nom, et ne suit pas un
// lien posé à sa place.
func writeBackup(dir, base string, data []byte, perm os.FileMode, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := filepath.Join(dir, backupName(base, now))
	for i := 0; i <= maxBackupSuffix; i++ {
		dst := name
		if i > 0 {
			dst = fmt.Sprintf("%s.%d", name, i)
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm.Perm())
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			// Une copie tronquée passerait pour la sauvegarde : elle ne reste pas.
			_ = os.Remove(dst)
			return "", err
		}
		return dst, nil
	}
	return "", fmt.Errorf("aucun nom libre pour sauvegarder %s dans %s", base, dir)
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
//
// Les parents, eux, sont résolus par leur nom : EnsureDir ne convient qu'à un
// chemin dont les parents appartiennent à root — la racine d'un projet, sous la
// racine des homes. Les sous-répertoires du projet passent par EnsureDirAt, qui
// part de sa racine et ne suit de lien à aucun composant.
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

// EnsureSharedDir assure un répertoire système partagé — /etc/cron.d — où ogs
// dépose ses fichiers sans en être le juge : absent, il est créé au mode donné ;
// présent, il n'est **jamais desserré**. Son mode devient l'intersection du
// constaté et de mode, setgid et sticky gardés, et son propriétaire n'est pas
// touché. EnsureDir reposait mode tel quel : chaque écriture d'un fichier de
// tâche rouvrait en 0755 le /etc/cron.d qu'« ogs harden » venait de fermer en
// 0750 — deux juges du même chemin (D116, D85). Un lien à sa place est refusé.
func EnsureSharedDir(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s est un lien symbolique : ogs ne le suit pas", path)
	case err == nil && !info.IsDir():
		return fmt.Errorf("%s existe et n'est pas un répertoire", path)
	case errors.Is(err, fs.ErrNotExist):
		return EnsureDir(path, mode, KeepOwner)
	case err != nil:
		return err
	}
	d, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return fmt.Errorf("%s est un lien symbolique : ogs ne le suit pas", path)
		}
		return fmt.Errorf("ouverture de %s : %w", path, err)
	}
	defer d.Close()
	st, err := d.Stat()
	if err != nil {
		return err
	}
	current := st.Mode()
	want := current.Perm() & mode.Perm()
	if want == current.Perm() {
		return nil
	}
	return d.Chmod(want | current&(os.ModeSetgid|os.ModeSticky|os.ModeSetuid))
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
