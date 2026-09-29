package fs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// cibleHorsDuHome prépare, hors de la racine du projet, un répertoire portant un
// bin/ en 0755 — le /usr/local d'un vrai système —, et pose à la place de
// root/.local un lien vers lui.
func cibleHorsDuHome(t *testing.T, root string) (cible, bin string) {
	t.Helper()
	cible = t.TempDir()
	bin = filepath.Join(cible, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cible, filepath.Join(root, ".local")); err != nil {
		t.Fatal(err)
	}
	return cible, bin
}

func TestEnsureDirAtNeSuitPasUnParentSubstitue(t *testing.T) {
	// L'utilisateur possède son home (D51) : il remplace ~/.local par un lien
	// vers /usr/local sans aucun privilège. EnsureDir, qui résout les parents par
	// leur nom, posait alors le mode et le propriétaire du projet sur
	// /usr/local/bin — ou créait un répertoire à lui ailleurs dans le système.
	root := t.TempDir()
	cible, bin := cibleHorsDuHome(t, root)

	err := EnsureDirAt(root, ".local/bin", 0o700, KeepOwner)
	if !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("un parent en lien doit être refusé, obtenu %v", err)
	}
	info, err := os.Lstat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("le bin/ désigné par le lien est passé en %04o : root a traversé le parent substitué", got)
	}
	// Sans bin/ au bout du lien, rien n'est créé hors du home.
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirAt(root, ".local/bin", 0o700, KeepOwner); !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("un parent en lien doit être refusé, obtenu %v", err)
	}
	if entries, _ := os.ReadDir(cible); len(entries) != 0 {
		t.Errorf("root a créé %v hors du home, par le parent substitué", entries)
	}
}

func TestEnsureDirAtRefuseUnLienALaPlaceDuRepertoire(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".local"), 0o700); err != nil {
		t.Fatal(err)
	}
	cible := t.TempDir()
	if err := os.Chmod(cible, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cible, filepath.Join(root, ".local", "bin")); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirAt(root, ".local/bin", 0o700, KeepOwner); !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("un lien à la place du répertoire doit être refusé, obtenu %v", err)
	}
	if info, _ := os.Stat(cible); info.Mode().Perm() != 0o755 {
		t.Errorf("la cible du lien est passée en %04o", info.Mode().Perm())
	}
}

func TestEnsureDirAtCreeEtCorrigeSansUmask(t *testing.T) {
	// Un parent absent est créé comme l'entrée : laissé à root, il serait
	// infranchissable pour le projet. Les modes ne dépendent pas de l'umask.
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	root := t.TempDir()
	if err := EnsureDirAt(root, "donnees/televersements", 0o710, KeepOwner); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"donnees", "donnees/televersements"} {
		info, err := os.Lstat(filepath.Join(root, rel))
		if err != nil || !info.IsDir() {
			t.Fatalf("%s : %v %v", rel, info, err)
		}
		if got := info.Mode().Perm(); got != 0o710 {
			t.Errorf("%s créé en %04o, 0710 attendu malgré l'umask", rel, got)
		}
	}
	// Un répertoire existant est remis au mode demandé.
	if err := os.Chmod(filepath.Join(root, "donnees", "televersements"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirAt(root, "donnees/televersements", 0o700, KeepOwner); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Lstat(filepath.Join(root, "donnees", "televersements")); info.Mode().Perm() != 0o700 {
		t.Errorf("mode non corrigé : %04o", info.Mode().Perm())
	}
	// Le parent existant garde le sien : il a son propre juge.
	if info, _ := os.Lstat(filepath.Join(root, "donnees")); info.Mode().Perm() != 0o710 {
		t.Errorf("le parent a changé de mode : %04o", info.Mode().Perm())
	}
	if err := EnsureDirAt(root, "../ailleurs", 0o700, KeepOwner); err == nil {
		t.Error("« .. » doit être refusé")
	}
}

func TestSetDirAtEtLstatAtNeSuiventPasUnParentSubstitue(t *testing.T) {
	root := t.TempDir()
	_, bin := cibleHorsDuHome(t, root)
	if err := SetDirAt(root, ".local/bin", 0o700, KeepOwner); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("la compensation doit refuser un parent en lien, obtenu %v", err)
	}
	if info, _ := os.Lstat(bin); info.Mode().Perm() != 0o755 {
		t.Errorf("la compensation a traversé le parent substitué : %04o", info.Mode().Perm())
	}
	if _, err := LstatAt(root, ".local/bin"); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("l'observation doit refuser un parent en lien, obtenu %v", err)
	}
	// Le lien lui-même est décrit comme tel, et un absent comme absent.
	if info, err := LstatAt(root, ".local"); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("le lien doit être décrit comme lien : %v %v", info, err)
	}
	if _, err := LstatAt(root, "var/tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("un chemin absent doit rendre ErrNotExist, obtenu %v", err)
	}
	// La racine elle-même : SetDirAt la corrige par son descripteur.
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SetDirAt(root, "", 0o711, KeepOwner); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Lstat(root); info.Mode().Perm() != 0o711 {
		t.Errorf("racine en %04o, 0711 attendu", info.Mode().Perm())
	}
}
