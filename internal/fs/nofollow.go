package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Opérations de root dans un répertoire qu'un autre compte contrôle.
//
// Les liens gérés d'un projet vivent dans ~/.local/bin, qui appartient à
// l'utilisateur du projet (D70). Root y écrit donc dans un chemin dont chaque
// composant, sous le home, peut être remplacé à tout instant par un lien
// symbolique : suivre le chemin par son nom, c'est laisser l'utilisateur choisir
// où root crée, renomme ou supprime. Ces fonctions ouvrent chaque composant par
// un descripteur, sans jamais suivre de lien (O_NOFOLLOW), puis n'opèrent que
// relativement à ce descripteur : le répertoire atteint est celui que l'on a
// ouvert, même si son nom change ensuite sous nos pieds.
//
// La racine elle-même — le home du projet — est ouverte sans suivre de lien
// elle non plus ; elle vit sous la racine des homes, qui appartient à root, et
// l'utilisateur ne peut donc pas la substituer.

// EntryKind décrit ce qu'on trouve à la place d'un lien géré.
type EntryKind int

const (
	// EntryAbsent : rien sous ce nom.
	EntryAbsent EntryKind = iota
	// EntrySymlink : un lien symbolique, quelle que soit sa cible.
	EntrySymlink
	// EntryOther : un fichier, un répertoire — quelque chose qu'ogs n'a pas posé.
	EntryOther
)

// ErrNotDirectory signale un composant du chemin qui n'est pas un répertoire
// véritable — le plus souvent, un lien symbolique posé à sa place.
var ErrNotDirectory = errors.New("composant qui n'est pas un répertoire (lien symbolique ?)")

const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// openDirAt ouvre root puis chacun des composants de rel sans suivre de lien.
// Avec create, un composant absent est créé en mode dirMode et donné à owner.
func openDirAt(root, rel string, create bool, dirMode uint32, owner Owner) (int, error) {
	fd, err := unix.Open(root, dirFlags, 0)
	if err != nil {
		return -1, wrapNoFollow(root, err)
	}
	if rel == "" || rel == "." {
		return fd, nil
	}
	path := root
	for _, comp := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		if comp == "" || comp == "." {
			continue
		}
		if comp == ".." {
			unix.Close(fd)
			return -1, fmt.Errorf("%s : « .. » refusé dans un chemin relatif au projet", rel)
		}
		path = filepath.Join(path, comp)
		next, err := unix.Openat(fd, comp, dirFlags, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if err := unix.Mkdirat(fd, comp, dirMode); err != nil && !errors.Is(err, unix.EEXIST) {
				unix.Close(fd)
				return -1, fmt.Errorf("création de %s : %w", path, err)
			}
			// Le mode passé à mkdir est rogné par l'umask : il est reposé, sur le
			// répertoire que l'on vient d'ouvrir et non sur son nom.
			next, err = unix.Openat(fd, comp, dirFlags, 0)
			if err == nil {
				if err := unix.Fchmod(next, dirMode); err != nil {
					unix.Close(next)
					unix.Close(fd)
					return -1, err
				}
				if owner.UID >= 0 || owner.GID >= 0 {
					if err := unix.Fchown(next, owner.UID, owner.GID); err != nil {
						unix.Close(next)
						unix.Close(fd)
						return -1, err
					}
				}
			}
		}
		unix.Close(fd)
		if err != nil {
			return -1, wrapNoFollow(path, err)
		}
		fd = next
	}
	return fd, nil
}

func wrapNoFollow(path string, err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return fmt.Errorf("%s : %w", path, ErrNotDirectory)
	}
	return fmt.Errorf("%s : %w", path, err)
}

// LinkStateAt dit ce qui occupe le nom name du répertoire root/rel, et la cible
// si c'est un lien. Un répertoire absent rend EntryAbsent.
func LinkStateAt(root, rel, name string) (EntryKind, string, error) {
	fd, err := openDirAt(root, rel, false, 0, KeepOwner)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return EntryAbsent, "", nil
		}
		return EntryAbsent, "", err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return EntryAbsent, "", nil
		}
		return EntryAbsent, "", err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		return EntryOther, "", nil
	}
	buf := make([]byte, 4096)
	n, err := unix.Readlinkat(fd, name, buf)
	if err != nil {
		return EntrySymlink, "", err
	}
	return EntrySymlink, string(buf[:n]), nil
}

// SymlinkAt pose, de façon atomique, le lien name → target dans root/rel. Les
// composants absents de rel sont créés en 0700 au nom d'owner, le lien lui est
// donné (lchown : le lien, jamais sa cible). Un nom occupé par autre chose qu'un
// lien n'est remplacé que si replace l'autorise : il porte alors le travail de
// quelqu'un.
func SymlinkAt(root, rel, name, target string, owner Owner, replace bool) error {
	fd, err := openDirAt(root, rel, true, 0o700, owner)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if !replace {
		var st unix.Stat_t
		if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil &&
			st.Mode&unix.S_IFMT != unix.S_IFLNK {
			return fmt.Errorf("%s existe et n'est pas un lien : refus de le remplacer",
				filepath.Join(root, rel, name))
		}
	}
	tmp := "." + name + ".ogs-tmp"
	_ = unix.Unlinkat(fd, tmp, 0)
	if err := unix.Symlinkat(target, fd, tmp); err != nil {
		return fmt.Errorf("lien %s : %w", filepath.Join(root, rel, name), err)
	}
	if owner.UID >= 0 || owner.GID >= 0 {
		if err := unix.Fchownat(fd, tmp, owner.UID, owner.GID, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			_ = unix.Unlinkat(fd, tmp, 0)
			return err
		}
	}
	if err := unix.Renameat(fd, tmp, fd, name); err != nil {
		_ = unix.Unlinkat(fd, tmp, 0)
		return fmt.Errorf("lien %s : %w", filepath.Join(root, rel, name), err)
	}
	return nil
}

// RemoveLinkAt retire le lien name de root/rel — et seulement si c'est un lien.
func RemoveLinkAt(root, rel, name string) error {
	fd, err := openDirAt(root, rel, false, 0, KeepOwner)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		return fmt.Errorf("%s n'est pas un lien : refus de le supprimer", filepath.Join(root, rel, name))
	}
	return unix.Unlinkat(fd, name, 0)
}

// RemoveEmptyDirAt retire le répertoire root/rel s'il est vide, sans suivre de
// lien. Un répertoire qui n'est pas vide est laissé en place et ses entrées
// rendues : elles portent le travail de quelqu'un.
func RemoveEmptyDirAt(root, rel string) (remaining []string, err error) {
	parent, name := filepath.Split(filepath.Clean(rel))
	fd, err := openDirAt(root, parent, false, 0, KeepOwner)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, nil
		}
		return nil, err
	}
	defer unix.Close(fd)
	err = unix.Unlinkat(fd, name, unix.AT_REMOVEDIR)
	switch {
	case err == nil, errors.Is(err, unix.ENOENT):
		return nil, nil
	case errors.Is(err, unix.ENOTEMPTY), errors.Is(err, unix.EEXIST):
		dir, derr := openDirAt(root, rel, false, 0, KeepOwner)
		if derr != nil {
			return nil, derr
		}
		defer unix.Close(dir)
		entries, derr := readDirNames(dir)
		if derr != nil {
			return nil, derr
		}
		return entries, nil
	}
	return nil, err
}

func readDirNames(fd int) ([]string, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	var names []string
	buf := make([]byte, 8192)
	for {
		n, err := unix.Getdents(dup, buf)
		if err != nil {
			unix.Close(dup)
			return nil, err
		}
		if n <= 0 {
			break
		}
		_, _, found := unix.ParseDirent(buf[:n], -1, nil)
		names = append(names, found...)
	}
	unix.Close(dup)
	return names, nil
}

// EnsureFile crée un fichier vide s'il manque, sans jamais suivre de lien, et
// recale son mode et son propriétaire. Un fichier existant n'est ni tronqué ni
// réécrit : les journaux pré-créés gardent leur contenu. Il rend vrai si quelque
// chose a changé. Il ne sert que dans un répertoire qui appartient à root — celui
// des journaux d'un projet (D71) — : ailleurs, le nom lui-même serait à la merci
// de l'utilisateur.
func EnsureFile(path string, mode os.FileMode, owner Owner) (bool, error) {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK,
		uint32(mode.Perm()))
	if err != nil {
		return false, wrapNoFollow(path, err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return false, fmt.Errorf("%s n'est pas un fichier régulier", path)
	}
	changed := false
	if os.FileMode(st.Mode).Perm() != mode.Perm() {
		if err := unix.Fchmod(fd, uint32(mode.Perm())); err != nil {
			return false, err
		}
		changed = true
	}
	uid, gid := owner.UID, owner.GID
	if (uid >= 0 && uint32(uid) != st.Uid) || (gid >= 0 && uint32(gid) != st.Gid) {
		if err := unix.Fchown(fd, uid, gid); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

// DirEntryAt décrit une entrée lue par ListDirAt.
type DirEntryAt struct {
	Name    string
	Regular bool
	UID     uint32
	Size    int64
}

// ListDirAt ouvre root/rel sans suivre de lien et en énumère les entrées, avec le
// propriétaire du répertoire lui-même. Aucune entrée n'est suivie : un lien y
// est rapporté comme non régulier.
func ListDirAt(root, rel string) (dirUID uint32, entries []DirEntryAt, err error) {
	fd, err := openDirAt(root, rel, false, 0, KeepOwner)
	if err != nil {
		return 0, nil, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0, nil, err
	}
	names, err := readDirNames(fd)
	if err != nil {
		return 0, nil, err
	}
	sort.Strings(names)
	for _, n := range names {
		var est unix.Stat_t
		if err := unix.Fstatat(fd, n, &est, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			continue
		}
		entries = append(entries, DirEntryAt{
			Name: n, Regular: est.Mode&unix.S_IFMT == unix.S_IFREG, UID: est.Uid, Size: est.Size,
		})
	}
	return st.Uid, entries, nil
}

// OpenFileAt ouvre en lecture le fichier régulier name de root/rel, sans suivre
// de lien ni à l'entrée ni en chemin. Le propriétaire est rendu pour que
// l'appelant juge de ce qu'il accepte de lire.
func OpenFileAt(root, rel, name string) (*os.File, uint32, error) {
	dir, err := openDirAt(root, rel, false, 0, KeepOwner)
	if err != nil {
		return nil, 0, err
	}
	defer unix.Close(dir)
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, wrapNoFollow(filepath.Join(root, rel, name), err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return nil, 0, fmt.Errorf("%s n'est pas un fichier régulier", filepath.Join(root, rel, name))
	}
	return os.NewFile(uintptr(fd), filepath.Join(root, rel, name)), st.Uid, nil
}

// RemoveFileAt retire le fichier régulier name de root/rel, sans suivre de lien.
func RemoveFileAt(root, rel, name string) error {
	fd, err := openDirAt(root, rel, false, 0, KeepOwner)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%s n'est pas un fichier régulier : refus de le supprimer", filepath.Join(root, rel, name))
	}
	return unix.Unlinkat(fd, name, 0)
}

// DirAt est un répertoire ouvert sans suivre de lien, ni en chemin ni au dernier
// composant. Ses opérations portent sur le descripteur, jamais sur le nom : ce
// qui est lu, écrit ou supprimé l'est dans le répertoire constaté à l'ouverture,
// même si l'utilisateur qui possède le chemin le remplace l'instant d'après par
// un lien. Une suite d'opérations — contrôler, sauvegarder, remplacer, relire —
// reste ainsi dans un seul et même répertoire, là où des appels par le nom
// pourraient chacun en viser un différent.
type DirAt struct {
	fd   int
	path string
}

// OpenDirAt ouvre root/rel sans suivre de lien. Un composant absent rend une
// erreur que errors.Is reconnaît comme fs.ErrNotExist ; un lien, ErrNotDirectory.
func OpenDirAt(root, rel string) (*DirAt, error) {
	fd, err := openDirAt(root, rel, false, 0, KeepOwner)
	if err != nil {
		return nil, err
	}
	return &DirAt{fd: fd, path: filepath.Join(root, rel)}, nil
}

// Close libère le descripteur.
func (d *DirAt) Close() error { return unix.Close(d.fd) }

// Path rend le chemin par lequel le répertoire a été ouvert. Il ne sert qu'aux
// messages : le nom a pu changer de sens depuis.
func (d *DirAt) Path() string { return d.path }

// Owner rend le propriétaire du répertoire ouvert.
func (d *DirAt) Owner() (uid, gid uint32, err error) {
	var st unix.Stat_t
	if err := unix.Fstat(d.fd, &st); err != nil {
		return 0, 0, err
	}
	return st.Uid, st.Gid, nil
}

// Entry décrit ce qui occupe name, sans le suivre ; present est faux si rien ne
// porte ce nom.
func (d *DirAt) Entry(name string) (e DirEntryAt, present bool, err error) {
	if err := checkName(name); err != nil {
		return DirEntryAt{}, false, err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(d.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return DirEntryAt{}, false, nil
		}
		return DirEntryAt{}, false, fmt.Errorf("%s : %w", filepath.Join(d.path, name), err)
	}
	return DirEntryAt{Name: name, Regular: st.Mode&unix.S_IFMT == unix.S_IFREG, UID: st.Uid, Size: st.Size},
		true, nil
}

// ReadFile lit le fichier régulier name et rend son mode. Un lien n'est pas
// suivi, et un fichier spécial n'est pas lu : une FIFO bloquerait la lecture, un
// périphérique la rendrait sans fin. Un fichier absent rend une erreur que
// errors.Is reconnaît comme fs.ErrNotExist.
//
// Avec uid ≥ 0, un fichier qui n'appartient pas à ce compte n'est pas lu : dans
// un répertoire qu'un non-root possède, un lien physique est le seul moyen, sans
// lien symbolique, d'y faire figurer le contenu d'un fichier qu'il ne possède
// pas. fs.protected_hardlinks l'interdit d'ordinaire, mais c'est un réglage de la
// machine, non une garantie. Le contrôle porte sur le descripteur ouvert, non sur
// le nom, que l'utilisateur peut faire changer de fichier entre les deux.
//
// Au-delà de limit octets, le fichier n'est pas lu. Il appartient au compte qui
// possède le répertoire, et ce compte lui donne la taille qu'il veut : un fichier
// creux de plusieurs Gio ne lui coûte aucun bloc, et root le lisait en entier en
// mémoire — puis, pour une clé, le réécrivait plein dans une sauvegarde. La
// taille est jugée sur le descripteur avant toute lecture, puis la lecture
// elle-même est bornée : le fichier peut grandir entre les deux.
func (d *DirAt) ReadFile(name string, uid int, limit int64) ([]byte, os.FileMode, error) {
	if err := checkName(name); err != nil {
		return nil, 0, err
	}
	path := filepath.Join(d.path, name)
	fd, err := unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, 0, fmt.Errorf("%s est un lien symbolique : ogs ne le suit pas", path)
		}
		return nil, 0, fmt.Errorf("%s : %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, 0, fmt.Errorf("%s n'est pas un fichier régulier", path)
	}
	if uid >= 0 && st.Uid != uint32(uid) {
		return nil, 0, fmt.Errorf("%s appartient à l'uid %d, non à l'uid %d : ogs ne le lit pas", path, st.Uid, uid)
	}
	if st.Size > limit {
		return nil, 0, fmt.Errorf("%s fait %d octets, au-delà des %d qu'ogs accepte d'y lire : il n'est pas lu",
			path, st.Size, limit)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, 0, fmt.Errorf("%s : %w", path, err)
	}
	if int64(len(data)) > limit {
		return nil, 0, fmt.Errorf("%s a dépassé %d octets pendant sa lecture : il n'est pas lu", path, limit)
	}
	return data, os.FileMode(st.Mode).Perm(), nil
}

// WriteFile remplace name par un fichier neuf portant data, en mode mode, donné
// à owner. Le nom est délié d'abord, puis créé en O_CREAT|O_EXCL|O_NOFOLLOW :
// l'ouverture échoue si quoi que ce soit l'occupe entre les deux — un lien
// reposé par l'utilisateur compris. Mode et propriétaire sont posés sur le
// descripteur, jamais sur le nom.
//
// Un fichier interrompu — disque plein, propriétaire refusé — est retiré : il
// resterait sinon à root, à moitié écrit, dans le répertoire d'un autre compte,
// et le contrôle du propriétaire ferait refuser la tentative suivante sans que
// rien n'en dise la cause.
func (d *DirAt) WriteFile(name string, data []byte, mode os.FileMode, owner Owner) (err error) {
	if err := checkName(name); err != nil {
		return err
	}
	path := filepath.Join(d.path, name)
	if err := unix.Unlinkat(d.fd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remplacement de %s : %w", path, err)
	}
	fd, err := unix.Openat(d.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		uint32(mode.Perm()))
	if err != nil {
		return fmt.Errorf("écriture de %s : %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("%s : %w", path, err)
	}
	defer func() {
		if err != nil {
			removeIfSame(d.fd, name, &st)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	// Le mode passé à la création est rogné par l'umask : il est reposé.
	if err := unix.Fchmod(fd, uint32(mode.Perm())); err != nil {
		return fmt.Errorf("%s : %w", path, err)
	}
	if owner.UID >= 0 || owner.GID >= 0 {
		if err := unix.Fchown(fd, owner.UID, owner.GID); err != nil {
			return fmt.Errorf("%s : %w", path, err)
		}
	}
	return f.Close()
}

// checkName refuse un nom qui ne désigne pas une entrée du répertoire lui-même :
// un « / » ferait traverser d'autres composants, par leur nom.
func checkName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return fmt.Errorf("%q n'est pas un nom d'entrée de répertoire", name)
	}
	return nil
}

// CopyTreeAt recopie le contenu de srcRoot/srcRel dans dstRoot/dstRel sans
// suivre aucun lien, et rend compte des entrées écartées, chacune avec sa raison.
//
// La source appartient à l'utilisateur d'un projet, qui peut remplacer à tout
// instant n'importe lequel de ses composants par un lien. Une copie faite par
// root en suivant les chemins — « cp -a src/. dst » : le « . » résout un src
// devenu lien — lui ferait recopier ce que root seul peut lire, puis donner la
// copie à un autre compte. Ici, chaque répertoire est ouvert par un descripteur
// relatif à son parent. Un lien rencontré est recopié comme lien : sa cible est
// son seul contenu, et qui l'emprunte ensuite le fait sous sa propre identité.
// Il n'est jamais traversé, et un répertoire devenu lien entre l'énumération et
// l'ouverture fait échouer la copie au lieu de la détourner.
//
// Seuls sont recopiés les fichiers qui appartiennent au compte source, srcUID —
// à défaut (srcUID < 0), au propriétaire de la racine source. Un lien physique
// est le seul moyen, sans lien symbolique, de faire figurer dans son répertoire
// le contenu d'un fichier qu'on ne possède pas, et son compte de liens ne le
// trahit pas toujours : quand l'original est remplacé par renommage —
// /etc/shadow, que le useradd du clone réécrit juste avant la copie —, le lien
// posé par l'utilisateur reste seul, et « plusieurs liens et un autre
// propriétaire » le laissait passer. fs.protected_hardlinks interdit d'ordinaire
// de tels liens, mais c'est un réglage de la machine, non une garantie. Les
// fichiers spéciaux (FIFO, socket, périphérique) sont écartés de même.
//
// Root n'écrit pas plus que la source n'occupe : l'utilisateur ne paie que ce que
// son répertoire consomme, et une copie qui le démultiplierait lui laisserait
// remplir le disque, blocs réservés à root compris. Un fichier que la source
// porte sous plusieurs noms n'est écrit qu'une fois, ses autres noms reposés en
// liens physiques vers cette copie ; un fichier creux reste creux — seules ses
// étendues de données sont recopiées (SEEK_DATA, SEEK_HOLE), jusqu'à la taille
// constatée à l'ouverture, et jamais plus de données qu'il n'en portait avant la
// copie : combler ses trous pendant qu'elle avance l'interrompt (copyData).
//
// La mémoire de root suit la même règle : elle ne croît qu'avec la profondeur,
// et la profondeur est bornée. Aucun niveau ne retient de chemin — la chaîne des
// répertoires parcourus en tient lieu (dirNode), un chemin n'étant composé que
// pour un message, et seulement sa fin (maxShownPath) —, et un répertoire
// au-delà de maxCopyDepth niveaux est écarté comme une entrée refusée, nommé et
// compté. Les entrées écartées sont toutes comptées, les maxSkippedNamed
// premières seules nommées (Skipped).
//
// Le temps aussi est aux mains du compte source, et la copie suit donc ctx : il
// est consulté avant chaque entrée et entre deux blocs de données. Une copie par
// commande — « cp -a » — mourait avec le groupe de processus que l'annulation
// tue ; faite dans le processus, elle ne s'arrête que si elle consulte le
// contexte elle-même. main capte SIGINT et SIGTERM pour l'annuler : sans cela,
// ni Ctrl-C ni l'arrêt d'une unité n'y feraient rien, et seul un SIGKILL
// l'arrêterait — sans qu'aucun defer ne rende les répertoires créés. Annulée, la
// copie rend l'erreur du contexte (errors.Is(err, context.Canceled)) après avoir,
// comme tout arrêt en chemin, retiré le fichier en cours et rendu les
// répertoires créés. Aucune entrée n'est écrite après la consultation qui l'a
// constatée.
//
// La destination appartient à un autre compte et s'écrit de même : créations en
// O_EXCL|O_NOFOLLOW, propriétaire, mode et dates posés sur le descripteur. Un
// répertoire créé reste en 0700 à root tant que son contenu n'est pas en place,
// et lui est rendu même si la copie s'arrête en chemin : resté à root, il serait
// illisible et impossible à vider pour le projet, et aucun apply ne le
// reprendrait — model.defaultDirs ne le connaît pas. Un fichier interrompu est
// retiré. Un répertoire qui existait déjà — la racine, les sous-répertoires
// déclarés — garde son mode et son propriétaire : ils relèvent de l'arborescence
// du projet, dont un seul juge décide (model.defaultDirs). Les bits setuid et
// setgid des fichiers ne sont pas reportés, que le « chown -R » suivant « cp -a »
// effaçait déjà ; le setgid d'un répertoire, que chown laisse, l'est — le groupe
// qu'il fait hériter est celui du compte de destination. Ni les attributs étendus
// ni les ACL, que « cp -a » reportait, ne le sont : le pool du projet de
// destination tourne sous son identité, à laquelle propriétaire et mode
// suffisent, et une ACL recopiée laisserait le compte source décider qui lit la
// copie.
//
// Une racine qui ne s'ouvre pas rend une *RootError : rien n'a été écrit. Toute
// autre erreur survient en cours de copie, une partie du contenu étant en place.
// Les messages citent leurs chemins (%q) : les noms sont aux mains du compte
// source, et un saut de ligne ou une séquence d'échappement y forgerait sinon une
// ligne de la sortie.
func CopyTreeAt(ctx context.Context, srcRoot, srcRel, dstRoot, dstRel string, srcUID int,
	owner Owner) (Skipped, error) {
	return CopyTreeGuardedAt(ctx, srcRoot, srcRel, dstRoot, dstRel, srcUID, owner, Guard{})
}

// CopyTreeGuardedAt est CopyTreeAt sous la garde g : les entrées que désignent
// ses chemins ne sont pas recopiées, et ses valeurs sont masquées dans tout
// fichier recopié qui les porte (Guard). Les unes et les autres sont comptées, et
// les premières nommées avec leur raison (Skipped.Withheld, Skipped.Masked). Le
// masquage a lieu sur les morceaux lus, avant leur écriture : la copie d'un
// fichier reste à root, en 0600, tant qu'elle n'est pas achevée, et le compte de
// destination ne voit jamais le secret. Une garde invalide rend une *RootError :
// rien n'est recopié.
func CopyTreeGuardedAt(ctx context.Context, srcRoot, srcRel, dstRoot, dstRel string, srcUID int,
	owner Owner, g Guard) (Skipped, error) {
	if err := ctx.Err(); err != nil {
		return Skipped{}, err
	}
	gd, err := newGuard(g)
	if err != nil {
		return Skipped{}, &RootError{Source: true, Err: err}
	}
	src, err := openDirAt(srcRoot, srcRel, false, 0, KeepOwner)
	if err != nil {
		return Skipped{}, &RootError{Source: true, Err: err}
	}
	defer unix.Close(src)
	dst, err := openDirAt(dstRoot, dstRel, false, 0, KeepOwner)
	if err != nil {
		return Skipped{}, &RootError{Err: err}
	}
	defer unix.Close(dst)
	uid := uint32(srcUID)
	if srcUID < 0 {
		var st unix.Stat_t
		if err := unix.Fstat(src, &st); err != nil {
			return Skipped{}, &RootError{Source: true, Err: err}
		}
		uid = st.Uid
	}
	root := &dirNode{}
	c := &treeCopy{ctx: ctx, owner: owner, srcUID: uid, srcPath: filepath.Join(srcRoot, srcRel),
		dstPath: filepath.Join(dstRoot, dstRel), links: map[inode]copied{},
		open: []openDir{{node: root, fd: dst}}, guard: gd,
		srcRoot: filepath.Clean(srcRoot), dstRoot: filepath.Clean(dstRoot),
		rootDepth: len(strings.Split(filepath.Clean(dstRel), "/"))}
	if gd != nil && len(gd.values) > 0 {
		c.masker = newMasker(gd)
	}
	defer c.forget()
	err = c.dir(src, dst, root)
	if errors.Is(err, unix.EMFILE) {
		var lim unix.Rlimit
		_ = unix.Getrlimit(unix.RLIMIT_NOFILE, &lim)
		err = fmt.Errorf("%w — la copie tient deux descripteurs par niveau, et le processus n'en a que %d "+
			"(RLIMIT_NOFILE)", err, lim.Cur)
	}
	return c.skipped, err
}

// maxCopyDepth borne la profondeur à laquelle CopyTreeAt descend sous la racine
// source. La profondeur est aux mains du compte source — une boucle « mkdir a;
// cd a » de quelques secondes empile des milliers de niveaux pour quelques Mio de
// disque —, et chaque niveau tient deux descripteurs ouverts le temps de sa
// copie. « cp -a », qui travaillait par chemins complets, s'arrêtait de lui-même
// vers deux mille niveaux, sur « Nom de fichier trop long » ; la copie par
// descripteurs n'a plus cette borne de fait, d'où celle-ci. Aucune application ne
// s'en approche : à mille niveaux, un chemin dépasse PATH_MAX (4096 octets) dès
// que ses noms ont plus de trois caractères, et ne s'ouvre plus par son nom.
//
// Deux descripteurs par niveau, c'est aussi une seconde borne, celle du
// processus : sous une limite RLIMIT_NOFILE de 1024 — que Go relève jusqu'à la
// limite dure, mais pas au-delà —, la copie s'arrête vers cinq cents niveaux, en
// chemin et en le disant (EMFILE, nommé par CopyTreeAt), avant d'atteindre
// celle-ci.
const maxCopyDepth = 1024

// maxShownPath borne, en octets, la part d'un chemin que CopyTreeAt reprend dans
// un message : sa fin, où sont l'entrée et ses parents proches. Un chemin
// complet est aux mains du compte source — mille niveaux de noms de 255 octets,
// que la citation quadruple s'ils ne s'impriment pas —, et chaque entrée écartée
// en retenait un dans la mémoire de root avant de l'imprimer dans le terminal de
// l'exploitant : cinquante Mio pour quatre Mio de répertoires chez le locataire.
const maxShownPath = 256

// maxSkippedNamed borne les entrées écartées que CopyTreeAt nomme ; au-delà, il
// les compte seulement. Leur nombre est aux mains du compte source, et le nom de
// chacune porte un chemin que la profondeur allonge : les retenir toutes ferait
// croître la mémoire de root comme leur nombre multiplié par la profondeur.
const maxSkippedNamed = 50

// Skipped rend compte de ce que CopyTreeAt n'a pas recopié tel quel : les
// entrées qu'il a écartées, et celles que la garde d'une copie a retenues ou
// expurgées (CopyTreeGuardedAt).
type Skipped struct {
	// Count est le nombre d'entrées écartées.
	Count int
	// Named nomme les premières d'entre elles, maxSkippedNamed au plus, chacune
	// avec sa raison.
	Named []string
	// Withheld est le nombre d'entrées que les chemins de la garde ont retenues ;
	// WithheldNamed nomme les premières, avec leur raison.
	Withheld      int
	WithheldNamed []string
	// Masked est le nombre de fichiers recopiés dont la garde a masqué une
	// valeur ; MaskedNamed nomme les premiers, avec ce qui y a été masqué.
	Masked      int
	MaskedNamed []string
	// Outside est le nombre de liens recopiés tels quels dont la cible sort de
	// la destination : absolue, elle désigne depuis la copie ce qu'elle désignait
	// depuis la source — le projet source lui-même, s'il y vit —, relative, elle
	// remonte au-delà de la racine de destination. OutsideNamed nomme les
	// premiers, avec leur cible.
	Outside      int
	OutsideNamed []string
	// Exposed est le nombre de fichiers recopiés sous un nom que la garde ne
	// désigne pas, avant qu'un autre de leurs noms, qu'elle désigne, ne soit
	// rencontré : liés physiquement, ils portent le contenu d'un fichier retenu.
	// ExposedNamed nomme les premiers. Rencontré avant, le nom retenu fait
	// retenir les autres (Withheld).
	Exposed      int
	ExposedNamed []string
}

// RootError est l'échec d'ouverture d'une racine de CopyTreeAt — source, ou
// destination. Il sépare « rien n'a été recopié » de « la copie s'est arrêtée en
// chemin » : une même cause, un lien ou une entrée absente, n'a pas le même sens
// à la racine et plus bas, où une partie du contenu est déjà en place.
type RootError struct {
	// Source est vrai pour la racine source, faux pour la destination.
	Source bool
	Err    error
}

func (e *RootError) Error() string { return e.Err.Error() }

func (e *RootError) Unwrap() error { return e.Err }

// inode identifie un fichier, quel que soit le nom par lequel on l'atteint.
type inode struct{ dev, ino uint64 }

// nodeOf rend l'inode que décrit st. Les conversions ne sont pas superflues :
// Dev n'a pas le même type sur toutes les architectures.
func nodeOf(st *unix.Stat_t) inode {
	return inode{dev: uint64(st.Dev), ino: uint64(st.Ino)}
}

// dirNode repère un répertoire de la copie par son nom et son parent : la chaîne
// des parents tient lieu de chemin, la racine de la copie n'ayant ni l'un ni
// l'autre. Chaque niveau n'ajoute ainsi qu'un nœud, là où un chemin complet et
// la liste de ses composants, recopiés à chaque niveau et retenus pendant toute
// la descente, faisaient croître la mémoire de root comme le carré de la
// profondeur — huit cents Mio pour huit mille niveaux. Un nœud ne survit à sa
// copie que si la première copie d'un fichier lié y vit (copied).
type dirNode struct {
	parent *dirNode
	name   string
	depth  int
}

// child rend le nœud du sous-répertoire name de n.
func (n *dirNode) child(name string) *dirNode {
	return &dirNode{parent: n, name: name, depth: n.depth + 1}
}

// copied est la première copie d'un fichier lié plusieurs fois dans la source :
// son répertoire et son nom dans la destination, et l'inode qu'elle y occupe.
type copied struct {
	dir  *dirNode
	name string
	node inode
}

// openDir est un répertoire de la destination et le descripteur qui le tient.
type openDir struct {
	node *dirNode
	fd   int
}

type treeCopy struct {
	ctx     context.Context
	owner   Owner
	srcUID  uint32
	srcPath string // racine source, pour les messages
	dstPath string // racine de destination, pour les messages
	links   map[inode]copied
	skipped Skipped
	// open tient les répertoires de destination ouverts le long de la descente,
	// la racine d'abord : open[d] est l'ancêtre de profondeur d du répertoire en
	// cours de copie. Il n'est jamais vide.
	open []openDir
	// last est le dernier répertoire rouvert où la première copie d'un fichier
	// lié a été retrouvée ; son descripteur appartient à treeCopy (forget).
	last openDir
	// guard est la garde de la copie, nil sans garde ; masker en masque les
	// valeurs, nil si elle n'en a pas.
	guard  *guard
	masker *masker
	// held associe aux fichiers retenus par la garde qui ont plusieurs noms la
	// raison de retenir les autres.
	held map[inode]string
	// srcRoot et dstRoot sont les répertoires sous lesquels la copie a lieu — les
	// racines des deux projets —, rootDepth le nombre de niveaux qui séparent
	// dstRoot de la racine de la copie : ce qu'une cible relative peut remonter
	// sans sortir de la destination (outside).
	srcRoot, dstRoot string
	rootDepth        int
}

// from et to composent, pour un message, le chemin cité de l'entrée name du
// répertoire at, côté source et côté destination — celui de at lui-même si name
// est vide.
func (c *treeCopy) from(at *dirNode, name string) string { return quotedPath(c.srcPath, at, name) }

func (c *treeCopy) to(at *dirNode, name string) string { return quotedPath(c.dstPath, at, name) }

// quotedPath cite le chemin de name dans at, sous root — ses maxShownPath
// derniers octets seulement, précédés de « … » hors des guillemets. Le chemin
// est composé depuis la feuille et s'arrête à la borne : un message ne coûte
// jamais la profondeur entière. Les points de suspension restent hors de la
// citation, où le compte source ne peut pas les écrire : ce qui est cité est
// toujours une fin exacte du chemin, jamais un nom qui imiterait une coupure.
func quotedPath(root string, at *dirNode, name string) string {
	var parts []string
	size := 0
	if name != "" {
		parts, size = append(parts, name), len(name)
	}
	cut := false
	for n := at; ; n = n.parent {
		if size > maxShownPath {
			cut = true
			break
		}
		if n.parent == nil {
			parts = append(parts, root)
			break
		}
		parts, size = append(parts, n.name), size+len(n.name)+1
	}
	slices.Reverse(parts)
	return quoteTail(filepath.Join(parts...), cut)
}

// quoteTail cite p, ou ses maxShownPath derniers octets précédés de « … » s'il
// est plus long ou déjà coupé (cut).
func quoteTail(p string, cut bool) string {
	if !cut && len(p) <= maxShownPath {
		return strconv.Quote(p)
	}
	// Coupé à une frontière de caractère : la fin d'un nom UTF-8 reste lisible.
	i := max(len(p)-maxShownPath, 0)
	for i < len(p) && !utf8.RuneStart(p[i]) {
		i++
	}
	return "…" + strconv.Quote(p[i:])
}

// skip écarte l'entrée name du répertoire at : elle est comptée, et nommée tant
// que maxSkippedNamed n'est pas atteint.
func (c *treeCopy) skip(at *dirNode, name, reason string) {
	c.skipped.Count++
	if len(c.skipped.Named) < maxSkippedNamed {
		c.skipped.Named = append(c.skipped.Named, c.from(at, name)+" : "+reason)
	}
}

// withhold retient l'entrée name du répertoire at, que la garde désigne. Les
// retenues sont bornées comme les entrées écartées, mais à part : celles-ci ne
// doivent pas taire celles-là.
func (c *treeCopy) withhold(at *dirNode, name, reason string) {
	c.skipped.Withheld++
	if len(c.skipped.WithheldNamed) < maxSkippedNamed {
		c.skipped.WithheldNamed = append(c.skipped.WithheldNamed, c.from(at, name)+" : "+reason)
	}
}

// masked note que la copie de l'entrée name du répertoire at a été expurgée.
func (c *treeCopy) masked(at *dirNode, name string, reasons []string) {
	c.skipped.Masked++
	if len(c.skipped.MaskedNamed) < maxSkippedNamed {
		c.skipped.MaskedNamed = append(c.skipped.MaskedNamed, c.from(at, name)+" : "+strings.Join(reasons, ", "))
	}
}

// hold retient les autres noms du fichier que la garde retient sous le nom name
// du répertoire at : un lien physique ne doit pas faire passer sous un autre nom
// un contenu retenu. Un autre nom déjà recopié — les noms se visitent dans
// l'ordre, et « .env.backup » précède le « .env.local » qu'il double — l'a été à
// tort, et il est nommé.
func (c *treeCopy) hold(at *dirNode, name string, st *unix.Stat_t) {
	node := nodeOf(st)
	withheld := c.from(at, name)
	why := "autre nom de " + withheld + ", que la garde retient"
	if first, ok := c.links[node]; ok {
		c.skipped.Exposed++
		if len(c.skipped.ExposedNamed) < maxSkippedNamed {
			c.skipped.ExposedNamed = append(c.skipped.ExposedNamed, c.from(first.dir, first.name)+
				" : même fichier que "+withheld+", que la garde retient mais qu'elle a rencontré après lui — "+
				"son contenu est recopié")
		}
	}
	if c.held == nil {
		c.held = map[inode]string{}
	}
	c.held[node] = why
}

// outside dit si la cible d'un lien posé dans le répertoire at sort de la
// destination, et comment. La copie ne suit aucun lien, mais le projet de
// destination les suivra : une cible absolue lui désigne ce qu'elle désignait
// au projet source — ses propres fichiers, s'ils y vivent : la configuration
// d'une application déployée avec un répertoire partagé hors de www —, une
// cible relative qui remonte au-delà de sa racine désigne un autre projet. Une
// cible relative qui reste sous la racine désigne, depuis la copie, les fichiers
// du projet de destination : elle est à lui. L'analyse est lexicale : un lien
// intermédiaire qui sortirait de la destination est lui-même nommé.
func (c *treeCopy) outside(at *dirNode, target string) (string, bool) {
	if filepath.IsAbs(target) {
		clean := filepath.Clean(target)
		if clean == c.srcRoot || strings.HasPrefix(clean, c.srcRoot+"/") {
			return "cible absolue, dans le projet source " + strconv.Quote(c.srcRoot), true
		}
		return "cible absolue, hors de la copie", true
	}
	level := c.rootDepth + at.depth
	for _, comp := range strings.Split(target, "/") {
		switch comp {
		case "", ".":
		case "..":
			if level--; level < 0 {
				return "cible relative qui sort de " + strconv.Quote(c.dstRoot), true
			}
		default:
			level++
		}
	}
	return "", false
}

func (c *treeCopy) dir(src, dst int, at *dirNode) error {
	names, err := readDirNames(src)
	if err != nil {
		return fmt.Errorf("%s : %w", c.from(at, ""), err)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		var st unix.Stat_t
		if err := unix.Fstatat(src, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue // retiré depuis l'énumération
			}
			return fmt.Errorf("%s : %w", c.from(at, name), err)
		}
		if c.guard != nil {
			if reason, ok := c.guard.withheld(at, name); ok {
				c.withhold(at, name, reason)
				if st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink > 1 {
					c.hold(at, name, &st)
				}
				continue
			}
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if at.depth >= maxCopyDepth {
				c.skip(at, name, fmt.Sprintf("répertoire au-delà de %d niveaux : ogs n'y descend pas", maxCopyDepth))
				continue
			}
			err = c.subdir(src, dst, name, at)
		case unix.S_IFREG:
			err = c.file(src, dst, name, at)
		case unix.S_IFLNK:
			err = c.link(src, dst, name, at, &st)
		default:
			c.skip(at, name, "fichier spécial")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *treeCopy) subdir(src, dst int, name string, at *dirNode) (err error) {
	in, err := unix.Openat(src, name, dirFlags, 0)
	if err != nil {
		return wrapNoFollow(c.from(at, name), err)
	}
	defer unix.Close(in)
	var st unix.Stat_t
	if err := unix.Fstat(in, &st); err != nil {
		return err
	}
	created := true
	if err := unix.Mkdirat(dst, name, 0o700); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("création de %s : %w", c.to(at, name), err)
		}
		created = false
	}
	out, err := unix.Openat(dst, name, dirFlags, 0)
	if err != nil {
		return wrapNoFollow(c.to(at, name), err)
	}
	defer unix.Close(out)
	if created {
		// Après le contenu : chaque entrée créée change la date du répertoire. Et
		// en échec aussi — annulation comprise : le répertoire revient à son
		// compte, qui peut le vider.
		defer func() {
			if ferr := c.finish(out, &st, true); ferr != nil && err == nil {
				err = fmt.Errorf("%s : %w", c.to(at, name), ferr)
			}
		}()
	}
	child := at.child(name)
	c.open = append(c.open, openDir{node: child, fd: out})
	defer func() { c.open = c.open[:len(c.open)-1] }()
	return c.dir(in, out, child)
}

func (c *treeCopy) file(src, dst int, name string, at *dirNode) (err error) {
	in, err := unix.Openat(src, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return wrapNoFollow(c.from(at, name), err)
	}
	defer unix.Close(in)
	// Type et propriétaire sont relus sur le descripteur : entre l'énumération et
	// l'ouverture, le nom a pu passer à une FIFO, ou à un lien physique étranger.
	var st unix.Stat_t
	if err := unix.Fstat(in, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		c.skip(at, name, "fichier spécial")
		return nil
	}
	if st.Uid != c.srcUID {
		c.skip(at, name, fmt.Sprintf("fichier de l'uid %d, non du compte source (uid %d)", st.Uid, c.srcUID))
		return nil
	}
	node := nodeOf(&st)
	if st.Nlink > 1 {
		if why, ok := c.held[node]; ok {
			c.withhold(at, name, why)
			return nil
		}
		if first, ok := c.links[node]; ok {
			return c.hardlink(dst, name, at, first)
		}
	}
	data, err := dataSize(c.ctx, in, st.Size)
	if err != nil {
		return fmt.Errorf("%s : %w", c.from(at, name), err)
	}
	out, err := c.create(dst, name, at, func() (int, error) {
		return unix.Openat(dst, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	})
	if err != nil {
		return err
	}
	defer unix.Close(out)
	var ost unix.Stat_t
	if err := unix.Fstat(out, &ost); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			removeIfSame(dst, name, &ost)
		}
	}()
	if c.masker != nil {
		c.masker.reset()
	}
	if err := copyData(c.ctx, out, in, st.Size, data, c.masker); err != nil {
		return fmt.Errorf("copie de %s : %w", c.from(at, name), err)
	}
	if err := c.finish(out, &st, false); err != nil {
		return fmt.Errorf("%s : %w", c.to(at, name), err)
	}
	if c.masker != nil {
		if reasons := c.masker.reasons(); len(reasons) > 0 {
			c.masked(at, name, reasons)
		}
	}
	if st.Nlink > 1 {
		c.links[node] = copied{dir: at, name: name, node: nodeOf(&ost)}
	}
	return nil
}

// hardlink repose name, dans le répertoire at de la destination, comme lien
// physique vers la première copie d'un fichier déjà rencontré. Cette copie est
// retrouvée dans son répertoire, atteint par descripteur (reach), puis reconnue
// à son inode avant d'être liée par son descripteur : le compte de destination a
// pu entre-temps renommer ce qui lui appartient, et un lien posé par le nom le
// ferait porter sur ce qu'il y aurait mis.
func (c *treeCopy) hardlink(dst int, name string, at *dirNode, first copied) error {
	dir, fresh, err := c.reach(first.dir)
	if err != nil {
		return fmt.Errorf("%s : lien physique vers %s : %w", c.to(at, name), c.to(first.dir, first.name), err)
	}
	kept := false
	if fresh {
		defer func() {
			if !kept {
				unix.Close(dir)
			}
		}()
	}
	fd, err := unix.Openat(dir, first.name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%s : lien physique vers %s : %w", c.to(at, name), c.to(first.dir, first.name), err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if nodeOf(&st) != first.node {
		return fmt.Errorf("%s : %s a changé pendant la copie — le lien physique n'est pas reposé",
			c.to(at, name), c.to(first.dir, first.name))
	}
	if fresh {
		// Retenu seulement une fois la copie reconnue : un répertoire que le compte
		// de destination aurait substitué ne sert jamais deux fois.
		c.forget()
		c.last, kept = openDir{node: first.dir, fd: dir}, true
	}
	_, err = c.create(dst, name, at, func() (int, error) { return -1, linkFd(fd, dst, name) })
	return err
}

// reach rend un descripteur du répertoire de destination n, sans suivre de
// lien : celui de la descente quand n en est un ancêtre — le répertoire en cours
// compris —, celui retenu (last) quand n y a déjà été retrouvé, et sinon un
// descripteur neuf (fresh), rouvert depuis leur plus proche ancêtre commun, que
// l'appelant ferme ou retient.
//
// Rouvrir chaque fois depuis la racine coûtait autant d'ouvertures que la
// profondeur, pour chaque nom d'un fichier lié : vingt mille liens au fond de
// mille niveaux, soixante-sept secondes de root, un lien physique ne coûtant au
// compte source qu'une entrée de répertoire. Les liens vers une copie du
// répertoire en cours ou d'un de ses ancêtres n'ouvrent plus rien, ceux qui
// visent un même autre répertoire une seule fois. Au pire — des liens qui
// alternent entre deux répertoires, sur des branches séparées très haut —, un nom
// coûte les niveaux qui séparent les branches, maxCopyDepth au plus : la durée
// reste proportionnelle aux entrées de la source (quatre mille liens alternés au
// fond de mille niveaux, une quinzaine de secondes), et la copie interruptible
// (ctx).
func (c *treeCopy) reach(n *dirNode) (fd int, fresh bool, err error) {
	if n == c.last.node {
		return c.last.fd, false, nil
	}
	var comps []string
	for n.depth >= len(c.open) || c.open[n.depth].node != n {
		comps = append(comps, n.name)
		n = n.parent
	}
	base := c.open[n.depth].fd
	if len(comps) == 0 {
		return base, false, nil
	}
	slices.Reverse(comps)
	fd, err = openRelDir(base, comps)
	return fd, err == nil, err
}

// forget ferme le répertoire retenu par hardlink.
func (c *treeCopy) forget() {
	if c.last.node != nil {
		unix.Close(c.last.fd)
		c.last = openDir{}
	}
}

// openRelDir ouvre, depuis le répertoire base, celui que désignent les
// composants comps, sans suivre de lien. base reste ouvert.
func openRelDir(base int, comps []string) (int, error) {
	fd, err := unix.Openat(base, ".", dirFlags, 0)
	if err != nil {
		return -1, err
	}
	for _, comp := range comps {
		next, err := unix.Openat(fd, comp, dirFlags, 0)
		unix.Close(fd)
		if err != nil {
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
				return -1, ErrNotDirectory
			}
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

// linkFd pose name dans dir, lien physique vers le fichier qu'ouvre fd — ce
// fichier-là, quel que soit le nom qui le désigne désormais.
func linkFd(fd, dir int, name string) error {
	err := unix.Linkat(fd, "", dir, name, unix.AT_EMPTY_PATH)
	if errors.Is(err, unix.ENOENT) {
		// AT_EMPTY_PATH exige CAP_DAC_READ_SEARCH, que root a et qu'un essai n'a
		// pas : le lien magique de /proc désigne le même fichier, par le même
		// descripteur.
		err = unix.Linkat(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", fd), dir, name, unix.AT_SYMLINK_FOLLOW)
	}
	return err
}

// dataSize mesure les étendues de données de fd comprises dans [0, size) : ce
// que la source occupe, trous exclus. Un système de fichiers qui ne sait pas les
// décrire (EINVAL) la voit pleine. La mesure ne lit rien, et ne compte que les
// étendues logiques : une compression, qui réduit st_blocks sans rien changer au
// contenu, n'y joue aucun rôle — une borne tirée de st_blocks interromprait, sur
// ZFS, la copie de tout fichier qui se compresse bien.
//
// ctx est consulté à chaque étendue : leur nombre n'est borné que par la taille
// apparente, et un compte source qui pose ses données juste devant la mesure le
// porte à des milliards.
func dataSize(ctx context.Context, fd int, size int64) (int64, error) {
	var total int64
	for off := int64(0); off < size; {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		start, err := unix.Seek(fd, off, unix.SEEK_DATA)
		switch {
		case errors.Is(err, unix.ENXIO):
			return total, nil // plus aucune donnée d'ici la fin
		case errors.Is(err, unix.EINVAL):
			return size, nil
		case err != nil:
			return 0, err
		}
		if start >= size {
			break
		}
		end, err := unix.Seek(fd, start, unix.SEEK_HOLE)
		if err != nil {
			return 0, err
		}
		// Une étendue percée entre les deux appels ne doit pas arrêter la mesure.
		end = min(max(end, start+1), size)
		total += end - start
		off = end
	}
	return total, nil
}

// copyData recopie dans out les étendues de données de in comprises dans
// [0, size), puis fixe la taille de out à size : les trous restent des trous.
// Un système de fichiers qui ne sait pas les décrire (EINVAL) voit le fichier
// recopié en entier.
//
// Les étendues sont relues au fil de la copie, et le compte source les déplace à
// sa guise : combler les trous devant la lecture et les percer derrière
// (fallocate) lui faisait écrire par root toute la taille apparente — seize Tio
// sur ext4 — en n'occupant jamais qu'une fenêtre. Root n'écrit donc jamais plus
// que data, les données que dataSize a mesurées avant la copie : un fichier dont
// les données grandissent en chemin interrompt la copie. ctx est consulté avant
// chaque bloc, et m, s'il n'est pas nil, masque chaque bloc avant son écriture.
func copyData(ctx context.Context, out, in int, size, data int64, m *masker) error {
	buf := make([]byte, 128<<10)
	var written int64
	for off := int64(0); off < size; {
		start, end := off, size
		if s, err := unix.Seek(in, off, unix.SEEK_DATA); err == nil {
			start = s
			if e, err := unix.Seek(in, s, unix.SEEK_HOLE); err == nil && e < end {
				end = max(e, s+1) // une étendue percée entre les deux appels : on avance
			}
		} else if errors.Is(err, unix.ENXIO) {
			break // plus aucune donnée d'ici la fin : le reste est un trou
		} else if !errors.Is(err, unix.EINVAL) {
			return err
		}
		if start >= size {
			break
		}
		for start < end {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, err := unix.Pread(in, buf[:min(int64(len(buf)), end-start)], start)
			if err != nil {
				return err
			}
			if n == 0 {
				return unix.Ftruncate(out, size) // le fichier a rétréci : la suite est un trou
			}
			if written += int64(n); written > data {
				return fmt.Errorf("ses données ont dépassé pendant la copie les %d octets qu'elles occupaient "+
					"avant elle", data)
			}
			if m != nil {
				if err := m.mask(out, buf[:n], start); err != nil {
					return err
				}
			}
			for p := buf[:n]; len(p) > 0; {
				w, err := unix.Pwrite(out, p, start)
				if err != nil {
					return err
				}
				p, start = p[w:], start+int64(w)
			}
		}
		off = end
	}
	return unix.Ftruncate(out, size)
}

// removeIfSame retire name de dir s'il désigne encore l'inode st : une copie
// interrompue ne reste pas en place, et ce que le compte de destination aurait
// mis à sa place n'est pas touché.
func removeIfSame(dir int, name string, st *unix.Stat_t) {
	var cur unix.Stat_t
	if err := unix.Fstatat(dir, name, &cur, unix.AT_SYMLINK_NOFOLLOW); err == nil &&
		cur.Dev == st.Dev && cur.Ino == st.Ino {
		_ = unix.Unlinkat(dir, name, 0)
	}
}

func (c *treeCopy) link(src, dst int, name string, at *dirNode, st *unix.Stat_t) error {
	buf := make([]byte, unix.PathMax)
	n, err := unix.Readlinkat(src, name, buf)
	if err != nil {
		return fmt.Errorf("%s : %w", c.from(at, name), err)
	}
	target := string(buf[:n])
	if _, err := c.create(dst, name, at, func() (int, error) {
		return -1, unix.Symlinkat(target, dst, name)
	}); err != nil {
		return err
	}
	// Lchown et dates du lien lui-même : AT_SYMLINK_NOFOLLOW, jamais sa cible.
	if c.owner.UID >= 0 || c.owner.GID >= 0 {
		if err := unix.Fchownat(dst, name, c.owner.UID, c.owner.GID, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("%s : %w", c.to(at, name), err)
		}
	}
	if err := unix.UtimesNanoAt(dst, name, []unix.Timespec{st.Atim, st.Mtim}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("%s : %w", c.to(at, name), err)
	}
	if reason, ok := c.outside(at, target); ok {
		c.skipped.Outside++
		if len(c.skipped.OutsideNamed) < maxSkippedNamed {
			c.skipped.OutsideNamed = append(c.skipped.OutsideNamed,
				c.from(at, name)+" → "+quoteTail(target, false)+" : "+reason)
		}
	}
	return nil
}

// create crée l'entrée name du répertoire at de la destination. Un nom déjà pris
// par autre chose qu'un répertoire est délié puis recréé, une fois et sans jamais
// être suivi.
func (c *treeCopy) create(dst int, name string, at *dirNode, mk func() (int, error)) (int, error) {
	fd, err := mk()
	if errors.Is(err, unix.EEXIST) {
		if err := unix.Unlinkat(dst, name, 0); err != nil {
			return -1, fmt.Errorf("remplacement de %s : %w", c.to(at, name), err)
		}
		fd, err = mk()
	}
	if err != nil {
		return -1, fmt.Errorf("création de %s : %w", c.to(at, name), err)
	}
	return fd, nil
}

// finish pose propriétaire, mode et dates sur une entrée créée, par son
// descripteur ; l'appelant nomme l'entrée dans l'erreur. Le propriétaire d'abord :
// un chown efface les bits setuid et setgid d'un fichier, qui ne sont de toute
// façon pas reportés — un fichier setuid au compte de destination n'a rien à
// faire dans un docroot. Ceux d'un répertoire le sont, comme « cp -a » puis
// « chown -R » les laissaient : le bit sticky protège les entrées des autres, le
// setgid fait hériter aux entrées futures le groupe du répertoire, qui est
// désormais celui du compte de destination.
func (c *treeCopy) finish(fd int, st *unix.Stat_t, dir bool) error {
	if c.owner.UID >= 0 || c.owner.GID >= 0 {
		if err := unix.Fchown(fd, c.owner.UID, c.owner.GID); err != nil {
			return err
		}
	}
	mode := st.Mode & 0o777
	if dir {
		mode |= st.Mode & (unix.S_ISVTX | unix.S_ISGID)
	}
	if err := unix.Fchmod(fd, mode); err != nil {
		return err
	}
	tv := []unix.Timeval{
		unix.NsecToTimeval(unix.TimespecToNsec(st.Atim)),
		unix.NsecToTimeval(unix.TimespecToNsec(st.Mtim)),
	}
	return unix.Futimes(fd, tv)
}
