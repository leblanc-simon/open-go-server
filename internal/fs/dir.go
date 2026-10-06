package fs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Parcours d'une arborescence qu'un autre compte contrôle.
//
// L'archivage et la restauration d'un projet lisent et peuplent son home en root,
// alors que chaque répertoire en dessous appartient à l'utilisateur (D51) et que
// son code peut tourner pendant ce temps : shell ouvert (D62), tâches, pool PHP.
// Résoudre une entrée par son chemin complet, c'est laisser l'utilisateur
// substituer un lien à n'importe quel composant entre deux appels — un Lstat ne
// protège que le dernier. Dir porte donc les opérations de nofollow.go à l'échelle
// d'un parcours : un répertoire ouvert sans suivre de lien, dont chaque opération
// est relative au descripteur, sur un nom d'un seul composant, sans jamais suivre
// de lien à l'entrée visée. Le répertoire atteint reste celui que l'on a ouvert,
// même renommé ensuite sous nos pieds.

// ErrNotRegular signale une entrée qui n'est pas un fichier régulier — le plus
// souvent un lien symbolique posé à sa place, qui n'a pas été suivi.
var ErrNotRegular = errors.New("entrée qui n'est pas un fichier régulier (lien symbolique ?)")

// Dir est un répertoire ouvert sans suivre de lien.
type Dir struct {
	fd   int
	path string
}

// OpenDir ouvre le répertoire path. Son dernier composant n'est pas suivi : un
// lien à sa place rend ErrNotDirectory. Les composants antérieurs le sont — ils
// désignent la racine des homes ou celle des journaux, qui appartiennent à root.
func OpenDir(path string) (*Dir, error) {
	fd, err := unix.Open(path, dirFlags, 0)
	if err != nil {
		return nil, wrapNoFollow(path, err)
	}
	return &Dir{fd: fd, path: path}, nil
}

// Path rend le chemin sous lequel le répertoire a été ouvert. Il ne sert qu'aux
// messages : le répertoire a pu être renommé depuis.
func (d *Dir) Path() string { return d.path }

// Close referme le répertoire.
func (d *Dir) Close() error { return unix.Close(d.fd) }

// child nomme une entrée dans les messages, après avoir vérifié qu'elle tient en
// un seul composant : un nom portant « / » serait résolu par le noyau composant
// par composant, en suivant les liens intermédiaires, et « .. » sortirait du
// répertoire ouvert.
func (d *Dir) child(name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return "", fmt.Errorf("%s : nom d'entrée %q refusé, un seul composant attendu", d.path, name)
	}
	return filepath.Join(d.path, name), nil
}

// Stat décrit le répertoire lui-même, par son descripteur.
func (d *Dir) Stat() (os.FileInfo, error) {
	var st unix.Stat_t
	if err := unix.Fstat(d.fd, &st); err != nil {
		return nil, fmt.Errorf("%s : %w", d.path, err)
	}
	return &statInfo{name: filepath.Base(d.path), st: st}, nil
}

// Lstat décrit l'entrée name sans la suivre : un lien est décrit comme tel.
func (d *Dir) Lstat(name string) (os.FileInfo, error) {
	p, err := d.child(name)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(d.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, fmt.Errorf("%s : %w", p, err)
	}
	return &statInfo{name: name, st: st}, nil
}

// Names énumère les entrées du répertoire, triées.
func (d *Dir) Names() ([]string, error) {
	// Le descripteur dupliqué par readDirNames partage la position de lecture :
	// sans ce retour au début, une seconde énumération ne rendrait rien.
	if _, err := unix.Seek(d.fd, 0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("%s : %w", d.path, err)
	}
	names, err := readDirNames(d.fd)
	if err != nil {
		return nil, fmt.Errorf("%s : %w", d.path, err)
	}
	sort.Strings(names)
	return names, nil
}

// OpenDir ouvre le sous-répertoire name, sans le suivre s'il est un lien.
func (d *Dir) OpenDir(name string) (*Dir, error) {
	p, err := d.child(name)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(d.fd, name, dirFlags, 0)
	if err != nil {
		return nil, wrapNoFollow(p, err)
	}
	return &Dir{fd: fd, path: p}, nil
}

// leaseWait borne l'attente d'un bail en écriture (F_SETLEASE), que le
// propriétaire d'un fichier pose sans privilège. L'ouverture non bloquante échoue
// alors en EWOULDBLOCK, mais demande quand même au noyau de rompre le bail : un
// détenteur coopératif — les oplocks de Samba — le rend aussitôt, les autres le
// perdent au terme de fs.lease-break-time, 45 s par défaut. Passé ce délai, le
// bail a été repris exprès, et l'ouverture échoue au lieu de laisser un
// locataire tenir indéfiniment le verrou global d'ogs.
var leaseWait, leasePoll = 50 * time.Second, 50 * time.Millisecond

// DefaultLeaseBudget borne le temps qu'une opération entière — l'archivage d'un
// projet — passe à attendre des baux, tous fichiers confondus. La borne de
// leaseWait, fichier par fichier, ne tenait pas sa promesse : chaque bail coûtait
// 45 s, les attentes s'additionnaient, et un locataire qui en posait un de plus
// à chaque répertoire tenait le verrou global d'ogs des heures durant (Q-86).
// Deux baux rompus par le noyau y tiennent ; au-delà, l'opération échoue en le
// disant.
const DefaultLeaseBudget = 2 * time.Minute

// LeaseBudget est le temps d'attente des baux qui reste à une opération. Nil, il
// n'y a que la borne de chaque fichier.
type LeaseBudget struct {
	mu    sync.Mutex
	total time.Duration
	left  time.Duration
}

// NewLeaseBudget rend un budget de d pour toute une opération.
func NewLeaseBudget(d time.Duration) *LeaseBudget { return &LeaseBudget{total: d, left: d} }

func (b *LeaseBudget) remaining() time.Duration {
	if b == nil {
		return leaseWait
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.left
}

func (b *LeaseBudget) spend(d time.Duration) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.left = max(b.left-d, 0)
}

// OpenFile ouvre en lecture le fichier régulier name, sans contexte ni budget
// d'opération : l'attente d'un bail n'y est bornée que par leaseWait. Il ne
// convient qu'à l'ouverture d'un fichier isolé ; un parcours passe par
// OpenFileContext.
func (d *Dir) OpenFile(name string) (*os.File, error) {
	return d.OpenFileContext(context.Background(), name, nil)
}

// OpenFileContext ouvre en lecture le fichier régulier name. Le type est constaté
// sur le descripteur ouvert, non sur le nom : ce que l'on lit est ce que l'on a
// vérifié. Une fifo n'y bloque pas l'appelant, l'ouverture étant non bloquante ;
// un bail ne la fait pas échouer d'emblée, l'ouverture étant retentée le temps
// que le noyau le rompe — dans la limite de leaseWait pour ce fichier, et du
// budget de l'opération. Une interruption (ctx) arrête l'attente aussitôt.
func (d *Dir) OpenFileContext(ctx context.Context, name string, budget *LeaseBudget) (*os.File, error) {
	p, err := d.child(name)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s : %w", p, err)
	}
	const flags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	fd, err := unix.Openat(d.fd, name, flags, 0)
	exhausted := false
	if errors.Is(err, unix.EWOULDBLOCK) {
		deadline := time.Now().Add(leaseWait)
		poll := time.NewTicker(leasePoll)
		for errors.Is(err, unix.EWOULDBLOCK) && time.Now().Before(deadline) {
			if budget.remaining() <= 0 {
				exhausted = true
				break
			}
			start := time.Now()
			select {
			case <-ctx.Done():
				poll.Stop()
				return nil, fmt.Errorf("%s : attente d'un bail interrompue : %w", p, ctx.Err())
			case <-poll.C:
			}
			budget.spend(time.Since(start))
			fd, err = unix.Openat(d.fd, name, flags, 0)
		}
		poll.Stop()
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%s : %w", p, ErrNotRegular)
		}
		if errors.Is(err, unix.EWOULDBLOCK) {
			if exhausted {
				return nil, fmt.Errorf("%s : des baux (F_SETLEASE) tenus par un processus ont épuisé les %v "+
					"d'attente de l'opération — arrêter les processus du projet, puis relancer : %w",
					p, budget.total, err)
			}
			return nil, fmt.Errorf("%s : un bail (F_SETLEASE) tenu par un processus en interdit la lecture — "+
				"arrêter les processus du projet, puis relancer : %w", p, err)
		}
		return nil, fmt.Errorf("%s : %w", p, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("%s : %w", p, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return nil, fmt.Errorf("%s : %w", p, ErrNotRegular)
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("%s : %w", p, err)
	}
	return os.NewFile(uintptr(fd), p), nil
}

// Readlink rend la cible du lien name, sans la résoudre.
func (d *Dir) Readlink(name string) (string, error) {
	p, err := d.child(name)
	if err != nil {
		return "", err
	}
	for size := 256; ; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(d.fd, name, buf)
		if err != nil {
			return "", fmt.Errorf("%s : %w", p, err)
		}
		if n < size {
			return string(buf[:n]), nil
		}
	}
}

// Mkdir crée le sous-répertoire name, qui ne doit pas exister, et rend son
// descripteur. Le mode, que l'umask a rogné, et le propriétaire sont posés sur le
// répertoire ouvert, non sur son nom : un lien substitué entre la création et
// l'ouverture fait échouer l'ouverture au lieu de recevoir le chmod.
func (d *Dir) Mkdir(name string, mode os.FileMode, owner Owner) (*Dir, error) {
	p, err := d.child(name)
	if err != nil {
		return nil, err
	}
	if err := unix.Mkdirat(d.fd, name, uint32(mode.Perm())); err != nil {
		return nil, fmt.Errorf("création de %s : %w", p, err)
	}
	sub, err := d.OpenDir(name)
	if err != nil {
		return nil, err
	}
	if err := setOwnerMode(sub.fd, p, mode, owner); err != nil {
		sub.Close()
		return nil, err
	}
	return sub, nil
}

// Create crée le fichier name en exclusivité et le rend ouvert en écriture. Rien
// ne doit occuper le nom : O_EXCL refuse aussi bien un fichier qu'un lien, reposé
// entre-temps ou non, si bien que l'inode écrit est toujours celui qu'on vient de
// créer. Propriétaire et mode sont posés sur son descripteur.
func (d *Dir) Create(name string, mode os.FileMode, owner Owner) (*os.File, error) {
	p, err := d.child(name)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(d.fd, name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return nil, fmt.Errorf("création de %s : %w", p, err)
	}
	if err := setOwnerMode(fd, p, mode, owner); err != nil {
		unix.Close(fd)
		_ = unix.Unlinkat(d.fd, name, 0)
		return nil, err
	}
	return os.NewFile(uintptr(fd), p), nil
}

// Symlink pose, de façon atomique, le lien name → target, au nom d'owner (lchown :
// le lien, jamais sa cible). Un fichier ou un lien qui occupait le nom est
// remplacé — lui, et non ce qu'il désigne ; un répertoire ne l'est jamais.
func (d *Dir) Symlink(target, name string, owner Owner) error {
	p, err := d.child(name)
	if err != nil {
		return err
	}
	var tmp string
	for attempt := 0; ; attempt++ {
		// Un nom temporaire tiré au hasard : un nom fixe, qu'il faudrait libérer
		// d'abord, emporterait une entrée légitime qui le porterait — une entrée
		// extraite de l'archive, par exemple.
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		tmp = ".ogs-lien-" + hex.EncodeToString(b[:])
		err := unix.Symlinkat(target, d.fd, tmp)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EEXIST) || attempt >= 3 {
			return fmt.Errorf("lien %s : %w", p, err)
		}
	}
	if owner.UID >= 0 || owner.GID >= 0 {
		if err := d.lchown(tmp, owner); err != nil {
			_ = unix.Unlinkat(d.fd, tmp, 0)
			return fmt.Errorf("propriétaire du lien %s : %w", p, err)
		}
	}
	if err := unix.Renameat(d.fd, tmp, d.fd, name); err != nil {
		_ = unix.Unlinkat(d.fd, tmp, 0)
		return fmt.Errorf("lien %s : %w", p, err)
	}
	return nil
}

// lchown pose le propriétaire du lien name par un descripteur ouvert sur le lien
// lui-même (O_PATH et O_NOFOLLOW), une fois sa nature vérifiée sur ce descripteur.
// Un fchownat par le nom laissait une fenêtre : le temporaire vit dans un
// répertoire de l'utilisateur, qui le voit apparaître par inotify, le retire et
// glisse à sa place un lien dur vers un fichier du système — fs.protected_hardlinks
// désactivé, le chown portait sur ce fichier, et une archive peut compter des
// milliers de liens, donc autant de tentatives. Par le descripteur, c'est l'inode
// vérifié qui change de propriétaire, quoi que devienne le nom.
func (d *Dir) lchown(name string, owner Owner) error {
	fd, err := unix.Openat(d.fd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		return fmt.Errorf("%s n'est plus un lien symbolique : son propriétaire n'est pas posé", filepath.Join(d.path, name))
	}
	return unix.Fchownat(fd, "", owner.UID, owner.GID, unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW)
}

// Remove retire l'entrée name si c'est un fichier ou un lien — le lien lui-même,
// jamais sa cible. Un répertoire est refusé.
func (d *Dir) Remove(name string) error {
	p, err := d.child(name)
	if err != nil {
		return err
	}
	if err := unix.Unlinkat(d.fd, name, 0); err != nil {
		return fmt.Errorf("suppression de %s : %w", p, err)
	}
	return nil
}

// Rename remplace l'entrée newname par oldname, toutes deux dans ce répertoire,
// sans suivre de lien : un lien qui occuperait newname est remplacé, lui, et non
// ce qu'il désigne ; un répertoire ne l'est jamais.
func (d *Dir) Rename(oldname, newname string) error {
	if _, err := d.child(oldname); err != nil {
		return err
	}
	p, err := d.child(newname)
	if err != nil {
		return err
	}
	if err := unix.Renameat(d.fd, oldname, d.fd, newname); err != nil {
		return fmt.Errorf("renommage vers %s : %w", p, err)
	}
	return nil
}

// setOwnerMode pose le propriétaire puis le mode d'un inode par son descripteur.
// Le propriétaire d'abord : un chown efface les bits setuid et setgid, et c'est
// au mode demandé d'avoir le dernier mot.
func setOwnerMode(fd int, path string, mode os.FileMode, owner Owner) error {
	if owner.UID >= 0 || owner.GID >= 0 {
		if err := unix.Fchown(fd, owner.UID, owner.GID); err != nil {
			return fmt.Errorf("attribution du propriétaire de %s : %w", path, err)
		}
	}
	if err := unix.Fchmod(fd, uint32(mode.Perm())); err != nil {
		return fmt.Errorf("mode de %s : %w", path, err)
	}
	return nil
}

// statInfo présente un unix.Stat_t sous la forme d'un os.FileInfo. Sys rend le
// *unix.Stat_t.
type statInfo struct {
	name string
	st   unix.Stat_t
}

func (s *statInfo) Name() string       { return s.name }
func (s *statInfo) Size() int64        { return s.st.Size }
func (s *statInfo) ModTime() time.Time { return time.Unix(s.st.Mtim.Unix()) }
func (s *statInfo) IsDir() bool        { return s.st.Mode&unix.S_IFMT == unix.S_IFDIR }
func (s *statInfo) Sys() any           { return &s.st }

func (s *statInfo) Mode() os.FileMode {
	mode := os.FileMode(s.st.Mode & 0o777)
	switch s.st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		mode |= os.ModeDir
	case unix.S_IFLNK:
		mode |= os.ModeSymlink
	case unix.S_IFIFO:
		mode |= os.ModeNamedPipe
	case unix.S_IFSOCK:
		mode |= os.ModeSocket
	case unix.S_IFBLK:
		mode |= os.ModeDevice
	case unix.S_IFCHR:
		mode |= os.ModeDevice | os.ModeCharDevice
	}
	if s.st.Mode&unix.S_ISUID != 0 {
		mode |= os.ModeSetuid
	}
	if s.st.Mode&unix.S_ISGID != 0 {
		mode |= os.ModeSetgid
	}
	if s.st.Mode&unix.S_ISVTX != 0 {
		mode |= os.ModeSticky
	}
	return mode
}
