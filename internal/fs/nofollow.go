package fs

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

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
