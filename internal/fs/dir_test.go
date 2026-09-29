package fs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// dirAvecLiens prépare un répertoire ouvert par OpenDir, qui contient un lien
// vers un fichier et un lien vers un répertoire, tous deux hors de lui.
func dirAvecLiens(t *testing.T) (d *Dir, fichier, ailleurs string) {
	t.Helper()
	root, dehors := t.TempDir(), t.TempDir()
	fichier = filepath.Join(dehors, "trousseau")
	if err := os.WriteFile(fichier, []byte("cle-admin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ailleurs = filepath.Join(dehors, "repertoire")
	if err := os.Mkdir(ailleurs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fichier, filepath.Join(root, "lien-fichier")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ailleurs, filepath.Join(root, "lien-rep")); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, fichier, ailleurs
}

func TestDirNeSuitJamaisUnLien(t *testing.T) {
	// Un nom de plusieurs composants serait résolu par le noyau en suivant les
	// liens intermédiaires : il est refusé, comme « .. ».
	d, fichier, ailleurs := dirAvecLiens(t)
	for _, name := range []string{"lien-rep/x", "..", "", "."} {
		if f, err := d.Create(name, 0o644, KeepOwner); err == nil {
			f.Close()
			t.Errorf("Create(%q) devait être refusé", name)
		}
	}
	if _, err := d.OpenDir("lien-rep"); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("OpenDir sur un lien : %v, attendu ErrNotDirectory", err)
	}
	if _, err := d.OpenFile("lien-fichier"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("OpenFile sur un lien : %v, attendu ErrNotRegular", err)
	}
	// La création exclusive refuse un nom occupé par un lien, au lieu d'écrire
	// dans sa cible.
	if f, err := d.Create("lien-fichier", 0o644, KeepOwner); err == nil {
		f.Close()
		t.Error("Create sur un lien devait être refusé")
	}
	if info, err := d.Lstat("lien-fichier"); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("Lstat doit décrire le lien lui-même : %v %v", info, err)
	}
	// Remove et Symlink retirent ou remplacent le lien, jamais sa cible.
	if err := d.Symlink("/ailleurs", "lien-fichier", KeepOwner); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Readlink("lien-fichier"); err != nil || got != "/ailleurs" {
		t.Errorf("lien remplacé : %q %v", got, err)
	}
	if err := d.Remove("lien-rep"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(fichier); string(data) != "cle-admin\n" {
		t.Errorf("la cible du lien a été touchée : %q", data)
	}
	if _, err := os.Stat(ailleurs); err != nil {
		t.Errorf("le répertoire désigné par le lien a été touché : %v", err)
	}
	names, err := d.Names()
	if err != nil {
		t.Fatal(err)
	}
	// Aucun temporaire ne reste, et une seconde énumération relit depuis le début.
	if again, _ := d.Names(); strings.Join(names, ",") != "lien-fichier" || strings.Join(again, ",") != "lien-fichier" {
		t.Errorf("entrées : %v puis %v, attendu le seul lien remplacé", names, again)
	}
}

func TestDirSymlinkNeRemplaceJamaisUnRepertoire(t *testing.T) {
	d, _, _ := dirAvecLiens(t)
	sub, err := d.Mkdir("www", 0o710, KeepOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if info, err := sub.Stat(); err != nil || info.Mode().Perm() != 0o710 {
		t.Errorf("répertoire créé : %v %v, 0710 attendu malgré l'umask", info, err)
	}
	if err := d.Symlink("releases/r1", "www", KeepOwner); err == nil {
		t.Error("un répertoire ne doit jamais être remplacé par un lien")
	}
	if info, err := d.Lstat("www"); err != nil || !info.IsDir() {
		t.Errorf("www doit rester un répertoire : %v %v", info, err)
	}
}

// proprietaireDEssai rend un propriétaire que le test peut poser et qui se
// distingue de celui du processus : n'importe lequel en root, un groupe dont le
// processus est membre sans que ce soit le sien sinon.
func proprietaireDEssai(t *testing.T) Owner {
	t.Helper()
	if os.Geteuid() == 0 {
		return Owner{UID: 4242, GID: 4242}
	}
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g != os.Getgid() {
			return Owner{UID: -1, GID: g}
		}
	}
	t.Skip("ni root ni groupe secondaire : le propriétaire posé ne se distinguerait pas")
	return KeepOwner
}

func gidDe(t *testing.T, path string) int {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return int(info.Sys().(*syscall.Stat_t).Gid)
}

func TestDirSymlinkPoseLeProprietaireSurLeLienSeul(t *testing.T) {
	essai := proprietaireDEssai(t)
	d, fichier, _ := dirAvecLiens(t)
	avant := gidDe(t, fichier)
	if err := d.Symlink(fichier, "cle", essai); err != nil {
		t.Fatal(err)
	}
	if got := gidDe(t, filepath.Join(d.Path(), "cle")); got != essai.GID {
		t.Errorf("lien posé au groupe %d, attendu %d", got, essai.GID)
	}
	if got := gidDe(t, fichier); got != avant {
		t.Errorf("la cible du lien a changé de propriétaire : groupe %d, attendu %d", got, avant)
	}
}

func TestDirLchownNeChangeJamaisUnInodeQuiNEstPlusUnLien(t *testing.T) {
	// Le propriétaire d'un lien posé dans un répertoire de l'utilisateur l'était
	// par un fchownat sur le nom temporaire. Ce nom se voit par inotify :
	// l'utilisateur le retire et glisse à sa place un lien dur vers un fichier du
	// système — fs.protected_hardlinks désactivé —, qui recevait le chown. Le
	// propriétaire est posé sur un descripteur du lien lui-même, et un nom qui ne
	// désigne plus un lien est refusé sans que rien ne change.
	essai := proprietaireDEssai(t)
	d, fichier, _ := dirAvecLiens(t)
	if err := os.Link(fichier, filepath.Join(d.Path(), "lien-dur")); err != nil {
		t.Fatal(err)
	}
	avant := gidDe(t, fichier)
	if err := d.lchown("lien-dur", essai); err == nil {
		t.Error("un nom qui désigne un fichier régulier doit être refusé")
	}
	if got := gidDe(t, fichier); got != avant {
		t.Errorf("le fichier désigné par le lien dur a reçu le propriétaire : groupe %d, attendu %d", got, avant)
	}
	if err := d.lchown("lien-fichier", essai); err != nil {
		t.Fatal(err)
	}
	if got := gidDe(t, filepath.Join(d.Path(), "lien-fichier")); got != essai.GID {
		t.Errorf("lien au groupe %d, attendu %d", got, essai.GID)
	}
	if got := gidDe(t, fichier); got != avant {
		t.Errorf("lchown a suivi le lien : cible au groupe %d, attendu %d", got, avant)
	}
}

// bail pose un bail en écriture sur path, comme le propriétaire d'un fichier peut
// le faire sans privilège, et le rend par la fonction retournée.
func bail(t *testing.T, path string) func() {
	t.Helper()
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETLEASE, unix.F_WRLCK); err != nil {
		unix.Close(fd)
		t.Skipf("bail refusé par ce système de fichiers : %v", err)
	}
	var once sync.Once
	rendre := func() {
		once.Do(func() {
			_, _ = unix.FcntlInt(uintptr(fd), unix.F_SETLEASE, unix.F_UNLCK)
			unix.Close(fd)
		})
	}
	t.Cleanup(rendre)
	return rendre
}

func TestDirOpenFileAttendLaRuptureDUnBail(t *testing.T) {
	// L'ouverture non bloquante, qui protège d'une fifo, échoue en EWOULDBLOCK sur
	// un fichier dont le propriétaire tient un bail : l'archivage était abandonné,
	// et la suppression refusée, pour un fichier que Samba tenait le temps d'un
	// oplock. Le noyau engage la rupture dès la première tentative : l'ouverture
	// attend qu'elle aboutisse.
	d, _, _ := dirAvecLiens(t)
	path := filepath.Join(d.Path(), "tenu")
	if err := os.WriteFile(path, []byte("contenu"), 0o644); err != nil {
		t.Fatal(err)
	}
	rendre := bail(t, path)
	go func() {
		time.Sleep(150 * time.Millisecond)
		rendre()
	}()
	f, err := d.OpenFile("tenu")
	if err != nil {
		t.Fatalf("un bail rendu doit laisser ouvrir le fichier : %v", err)
	}
	defer f.Close()
	if data, _ := io.ReadAll(f); string(data) != "contenu" {
		t.Errorf("contenu lu : %q", data)
	}
}

func TestDirOpenFileRenonceAUnBailRepris(t *testing.T) {
	// Un bail que son détenteur ne rend pas ne tient pas indéfiniment le verrou
	// global : passé le délai, l'ouverture échoue et le dit.
	defer func(prev time.Duration) { leaseWait = prev }(leaseWait)
	leaseWait = 200 * time.Millisecond
	d, _, _ := dirAvecLiens(t)
	path := filepath.Join(d.Path(), "tenu")
	if err := os.WriteFile(path, []byte("contenu"), 0o644); err != nil {
		t.Fatal(err)
	}
	bail(t, path)
	debut := time.Now()
	f, err := d.OpenFile("tenu")
	if err == nil {
		f.Close()
		t.Fatal("un bail jamais rendu doit faire échouer l'ouverture")
	}
	if !strings.Contains(err.Error(), "bail") || time.Since(debut) < leaseWait {
		t.Errorf("échec après %v : %v — attendu après %v, en nommant le bail", time.Since(debut), err, leaseWait)
	}
}
