package fs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// confianceDEssai tient pour de confiance, le temps d'un essai, tout répertoire
// que ni le groupe ni les autres ne modifient, ou collant comme /tmp : un essai
// ne tourne pas en root, et rien de ce qu'il crée n'appartient à root. Un
// répertoire en 0777 reste hors de confiance.
func confianceDEssai(t *testing.T) {
	t.Helper()
	orig := trustedDir
	trustedDir = func(st *unix.Stat_t) bool { return st.Mode&0o022 == 0 || st.Mode&unix.S_ISVTX != 0 }
	t.Cleanup(func() { trustedDir = orig })
}

// garde rend un répertoire en 0700, que seul son propriétaire modifie — quel que
// soit l'umask de l'essai.
func garde(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ouvert rend un répertoire en 0777 : n'importe quel compte peut y poser un lien.
func ouvert(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestOpenParentDirNeSuitPasUnLienHorsDeConfiance(t *testing.T) {
	// Un répertoire que son propriétaire n'est pas seul à pouvoir modifier — un
	// home, du point de vue de root — : ni un lien en chemin ni un lien à
	// l'entrée n'y sont suivis.
	home, ailleurs := t.TempDir(), t.TempDir()
	if err := os.Symlink(ailleurs, filepath.Join(home, "sauvegardes")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenParentDir(filepath.Join(home, "sauvegardes", "x.sql"), true, 0o750); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("un répertoire lié doit être refusé, obtenu %v", err)
	}
	if entries, _ := os.ReadDir(ailleurs); len(entries) != 0 {
		t.Errorf("la cible du lien a été touchée : %v", entries)
	}
	if err := os.Symlink(filepath.Join(ailleurs, "cible"), filepath.Join(home, "x.sql")); err != nil {
		t.Fatal(err)
	}
	d, name, err := OpenParentDir(filepath.Join(home, "x.sql"), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if info, err := d.Lstat(name); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("le lien à l'entrée doit être rendu tel quel, non suivi : %v %v", info, err)
	}
}

func TestOpenParentDirSuitUnLienQueSeulRootAPuPoser(t *testing.T) {
	// /var/run vers /run, un /home ou un /var/lib/ogs déplacé sur un autre
	// disque : l'administrateur a posé ces liens, dans des répertoires que lui
	// seul modifie, et les refuser casserait « project delete » sur une telle
	// machine.
	confianceDEssai(t)
	racine, disque := garde(t), garde(t)
	if err := os.Mkdir(filepath.Join(disque, "dumps"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(disque, filepath.Join(racine, "ogs")); err != nil {
		t.Fatal(err)
	}
	d, name, err := OpenParentDir(filepath.Join(racine, "ogs", "dumps", "x.sql"), false, 0)
	if err != nil {
		t.Fatalf("un lien de confiance doit être suivi : %v", err)
	}
	if name != "x.sql" || d.Path() != filepath.Join(disque, "dumps") {
		t.Errorf("atteint %s/%s, attendu %s/x.sql", d.Path(), name, filepath.Join(disque, "dumps"))
	}
	d.Close()
	// À l'entrée aussi, et une cible relative se résout depuis le répertoire du
	// lien, « .. » compris.
	if err := os.Symlink("../"+filepath.Base(disque)+"/dumps/y.sql", filepath.Join(racine, "dernier.sql")); err != nil {
		t.Fatal(err)
	}
	d, name, err = OpenParentDir(filepath.Join(racine, "dernier.sql"), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if name != "y.sql" || d.Path() != filepath.Join(disque, "dumps") {
		t.Errorf("atteint %s/%s, attendu %s/y.sql", d.Path(), name, filepath.Join(disque, "dumps"))
	}
	d.Close()
}

func TestOpenParentDirRefuseUnLienPoseSousUnRepertoireHorsDeConfiance(t *testing.T) {
	// La confiance tient à toute la chaîne : un répertoire bien gardé, posé dans
	// un répertoire que d'autres modifient, se renomme et se remplace par eux.
	confianceDEssai(t)
	partage := ouvert(t)
	garde := filepath.Join(partage, "garde")
	if err := os.Mkdir(garde, 0o700); err != nil {
		t.Fatal(err)
	}
	ailleurs := t.TempDir()
	if err := os.Symlink(ailleurs, filepath.Join(garde, "lien")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenParentDir(filepath.Join(garde, "lien", "x.sql"), false, 0); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("un lien sous une chaîne hors de confiance doit être refusé, obtenu %v", err)
	}
	if err := os.Symlink(ailleurs, filepath.Join(partage, "direct")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenParentDir(filepath.Join(partage, "direct", "x.sql"), false, 0); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("un lien dans un répertoire en 0777 doit être refusé, obtenu %v", err)
	}
}

func TestOpenParentDirBorneLesLiensEtCreeLesRepertoires(t *testing.T) {
	confianceDEssai(t)
	racine := garde(t)
	if err := os.Symlink("b", filepath.Join(racine, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", filepath.Join(racine, "b")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenParentDir(filepath.Join(racine, "a", "x.sql"), false, 0); !errors.Is(err, unix.ELOOP) {
		t.Errorf("une boucle de liens doit rendre ELOOP, obtenu %v", err)
	}
	if _, _, err := OpenParentDir("/", false, 0); err == nil {
		t.Error("la racine du système ne désigne aucune entrée")
	}
	// Un chemin relatif part du répertoire courant ; les répertoires absents sont
	// créés au mode demandé.
	t.Chdir(racine)
	d, name, err := OpenParentDir(filepath.Join("c", "d", "x.sql"), true, 0o750)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	info, err := os.Lstat(filepath.Join(racine, "c", "d"))
	if err != nil || !info.IsDir() || name != "x.sql" {
		t.Fatalf("répertoire attendu : %v %v %q", info, err, name)
	}
	if info.Mode().Perm() != 0o750 {
		t.Errorf("créé en %04o, 0750 attendu", info.Mode().Perm())
	}
}
