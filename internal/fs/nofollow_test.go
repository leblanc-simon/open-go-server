package fs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSymlinkAtCreeLesRepertoiresEtPoseLeLien(t *testing.T) {
	root := t.TempDir()
	if err := SymlinkAt(root, ".local/bin", "php", "/usr/bin/php8.4", KeepOwner, false); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(root, ".local", "bin", "php"))
	if err != nil || target != "/usr/bin/php8.4" {
		t.Fatalf("lien attendu vers /usr/bin/php8.4, obtenu %q (%v)", target, err)
	}
	info, _ := os.Stat(filepath.Join(root, ".local", "bin"))
	if info.Mode().Perm() != 0o700 {
		t.Errorf("répertoire créé en %04o, 0700 attendu malgré l'umask", info.Mode().Perm())
	}
	kind, got, err := LinkStateAt(root, ".local/bin", "php")
	if err != nil || kind != EntrySymlink || got != "/usr/bin/php8.4" {
		t.Errorf("état inattendu : %v %q %v", kind, got, err)
	}
	// Le remplacement est atomique et ne laisse aucun temporaire derrière lui.
	if err := SymlinkAt(root, ".local/bin", "php", "/usr/bin/php8.5", KeepOwner, false); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, ".local", "bin"))
	if len(entries) != 1 {
		t.Errorf("un seul lien attendu, obtenu %v", entries)
	}
}

func TestUnComposantRemplaceParUnLienNEstJamaisSuivi(t *testing.T) {
	// L'utilisateur possède ~/.local : il peut y substituer un lien vers un
	// répertoire de son choix, et faire créer par root un fichier où il veut.
	root := t.TempDir()
	ailleurs := t.TempDir()
	if err := os.Symlink(ailleurs, filepath.Join(root, ".local")); err != nil {
		t.Fatal(err)
	}
	err := SymlinkAt(root, ".local/bin", "php", "/usr/bin/php8.4", KeepOwner, false)
	if !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("un composant en lien doit être refusé, obtenu %v", err)
	}
	if entries, _ := os.ReadDir(ailleurs); len(entries) != 0 {
		t.Errorf("root a écrit dans le répertoire désigné par le lien : %v", entries)
	}
	if err := RemoveLinkAt(root, ".local/bin", "php"); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("la suppression doit refuser le même détour, obtenu %v", err)
	}
	if _, _, err := LinkStateAt(root, ".local/bin", "php"); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("l'observation doit refuser le même détour, obtenu %v", err)
	}
}

func TestUnFichierDeLUtilisateurNEstPasRemplaceSansAccord(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".local", "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "php"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if kind, _, _ := LinkStateAt(root, ".local/bin", "php"); kind != EntryOther {
		t.Fatalf("un fichier régulier doit être vu comme EntryOther, obtenu %v", kind)
	}
	err := SymlinkAt(root, ".local/bin", "php", "/usr/bin/php8.4", KeepOwner, false)
	if err == nil || !strings.Contains(err.Error(), "refus") {
		t.Fatalf("refus attendu, obtenu %v", err)
	}
	if err := RemoveLinkAt(root, ".local/bin", "php"); err == nil {
		t.Error("un fichier régulier ne doit pas être supprimé comme un lien")
	}
	if err := SymlinkAt(root, ".local/bin", "php", "/usr/bin/php8.4", KeepOwner, true); err != nil {
		t.Fatalf("avec accord, le remplacement doit passer : %v", err)
	}
}

func TestRemoveEmptyDirAtLaisseCeQuiNEstPasAOgs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".bin")
	if err := os.MkdirAll(dir, 0o710); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"php", "node"} {
		if err := os.Symlink("/nulle/part", filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "outil"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"php", "node", "absent"} {
		if err := RemoveLinkAt(root, ".bin", n); err != nil {
			t.Fatal(err)
		}
	}
	remaining, err := RemoveEmptyDirAt(root, ".bin")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(remaining)
	if strings.Join(remaining, ",") != "outil" {
		t.Errorf("seul « outil » doit rester, obtenu %v", remaining)
	}
	if err := os.Remove(filepath.Join(dir, "outil")); err != nil {
		t.Fatal(err)
	}
	if remaining, err := RemoveEmptyDirAt(root, ".bin"); err != nil || len(remaining) != 0 {
		t.Fatalf("un répertoire vide doit partir : %v %v", remaining, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("le répertoire vide est resté")
	}
}

func TestDirAtNeSuitAucunLien(t *testing.T) {
	root := t.TempDir()
	ailleurs := t.TempDir()
	temoin := filepath.Join(ailleurs, "temoin")
	if err := os.WriteFile(temoin, []byte("contenu de root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Un composant lié : rien n'est ouvert.
	if err := os.Symlink(ailleurs, filepath.Join(root, ".ssh")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDirAt(root, ".ssh"); !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("un répertoire lié doit être refusé, obtenu %v", err)
	}
	if err := os.Remove(filepath.Join(root, ".ssh")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDirAt(root, ".ssh")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Une entrée liée : ni lue, ni suivie à l'écriture.
	if err := os.Symlink(temoin, filepath.Join(root, ".ssh", "id")); err != nil {
		t.Fatal(err)
	}
	if e, present, err := d.Entry("id"); err != nil || !present || e.Regular {
		t.Errorf("le lien doit être vu comme tel : %+v %v %v", e, present, err)
	}
	if data, _, err := d.ReadFile("id", -1, 1<<10); err == nil {
		t.Errorf("un lien ne doit pas être lu : %q", string(data))
	}
	if err := d.WriteFile("id", []byte("neuf\n"), 0o640, KeepOwner); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(temoin); string(data) != "contenu de root\n" {
		t.Errorf("la cible du lien a été écrite : %q", string(data))
	}
	data, mode, err := d.ReadFile("id", -1, 1<<10)
	if err != nil || string(data) != "neuf\n" || mode != 0o640 {
		t.Errorf("relecture : %q %04o %v", string(data), mode, err)
	}

	// Un fichier spécial n'est pas lu : une FIFO bloquerait la lecture.
	if err := unix.Mkfifo(filepath.Join(root, ".ssh", "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.ReadFile("fifo", -1, 1<<10); err == nil {
		t.Error("une FIFO ne doit pas être lue")
	}
	if _, _, err := d.ReadFile("absent", -1, 1<<10); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("un fichier absent doit se reconnaître comme tel : %v", err)
	}
	for _, nom := range []string{"", ".", "..", "../temoin"} {
		if err := d.WriteFile(nom, nil, 0o600, KeepOwner); err == nil {
			t.Errorf("%q ne désigne pas une entrée du répertoire : il doit être refusé", nom)
		}
	}
}

func TestCopyTreeAtRecopieSansSuivreDeLien(t *testing.T) {
	src, dst, secret := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(secret, "shadow"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	www := filepath.Join(src, "www")
	for _, d := range []string{"www/public/css", "www/var"} {
		if err := os.MkdirAll(filepath.Join(src, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(www, "var"), 0o770|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(www, "public", "css"), 0o750|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	// Le noyau efface le setgid, sans erreur, d'un répertoire dont le groupe
	// n'est pas celui de l'essai : il n'y a alors rien à reporter.
	setgid := false
	if info, err := os.Stat(filepath.Join(www, "public", "css")); err == nil {
		setgid = info.Mode()&os.ModeSetgid != 0
	}
	css := filepath.Join(www, "public", "css", "site.css")
	if err := os.WriteFile(css, []byte("body{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	passe := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(css, passe, passe); err != nil {
		t.Fatal(err)
	}
	outil := filepath.Join(www, "outil")
	if err := os.WriteFile(outil, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outil, 0o755|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	liens := map[string]string{"fuite": filepath.Join(secret, "shadow"), "dossier": secret}
	for nom, cible := range liens {
		if err := os.Symlink(cible, filepath.Join(www, nom)); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mkfifo(filepath.Join(www, "tube"), 0o600); err != nil {
		t.Fatal(err)
	}
	// La destination porte déjà un sous-répertoire déclaré, à son propre mode.
	if err := os.MkdirAll(filepath.Join(dst, "www", "public"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"www", "www/public"} {
		if err := os.Chmod(filepath.Join(dst, d), 0o710); err != nil {
			t.Fatal(err)
		}
	}

	skipped, err := CopyTreeAt(context.Background(), src, "www", dst, "www", -1, KeepOwner)
	if err != nil {
		t.Fatal(err)
	}
	if skipped.Count != 1 || len(skipped.Named) != 1 || !strings.Contains(skipped.Named[0], "tube") {
		t.Errorf("seule la FIFO doit être écartée, obtenu %+v", skipped)
	}
	out := filepath.Join(dst, "www")
	data, err := os.ReadFile(filepath.Join(out, "public", "css", "site.css"))
	if err != nil || string(data) != "body{}" {
		t.Errorf("fichier ordinaire : %q, %v", string(data), err)
	}
	if info, err := os.Stat(filepath.Join(out, "public", "css", "site.css")); err != nil ||
		info.Mode().Perm() != 0o640 || !info.ModTime().Equal(passe) {
		t.Errorf("mode et date doivent être repris : %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Join(out, "outil")); err != nil || info.Mode()&os.ModeSetuid != 0 ||
		info.Mode().Perm() != 0o755 {
		t.Errorf("le bit setuid ne doit pas être reporté : %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Join(out, "var")); err != nil || info.Mode()&os.ModeSticky == 0 ||
		info.Mode().Perm() != 0o770 {
		t.Errorf("mode d'un répertoire créé, bit sticky compris : %v %v", info, err)
	}
	// Le setgid d'un répertoire, que « chown -R » laissait après « cp -a », l'est
	// aussi : il fait hériter le groupe du répertoire, celui de la destination.
	if info, err := os.Stat(filepath.Join(out, "public", "css")); err != nil || info.Mode()&os.ModeSetgid == 0 && setgid ||
		info.Mode().Perm() != 0o750 {
		t.Errorf("mode d'un répertoire créé, bit setgid compris : %v %v", info, err)
	}
	for _, d := range []string{"", "public"} {
		if info, err := os.Stat(filepath.Join(out, d)); err != nil || info.Mode().Perm() != 0o710 {
			t.Errorf("un répertoire existant garde son mode : %q %v %v", d, info, err)
		}
	}
	for nom, cible := range liens {
		got, err := os.Readlink(filepath.Join(out, nom))
		if err != nil || got != cible {
			t.Errorf("%s doit être recopié comme lien vers %s : %q, %v", nom, cible, got, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(out, "shadow")); !os.IsNotExist(err) {
		t.Errorf("le contenu d'une cible a été recopié : %v", err)
	}
}

func TestCopyTreeAtRefuseUneRacineLiee(t *testing.T) {
	src, dst, secret := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(secret, "shadow"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(src, "www")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dst, "www"), 0o710); err != nil {
		t.Fatal(err)
	}
	_, err := CopyTreeAt(context.Background(), src, "www", dst, "www", -1, KeepOwner)
	if !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("une source liée doit être refusée, obtenu %v", err)
	}
	// Le refus se dit à la racine : l'appelant sait que rien n'a été recopié.
	var re *RootError
	if !errors.As(err, &re) || !re.Source {
		t.Errorf("le refus d'une source liée doit se dire à la racine source : %#v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dst, "www")); len(entries) != 0 {
		t.Errorf("la cible du lien a été recopiée : %v", entries)
	}

	// La destination liée l'est de même : le compte qui la possède ne choisit pas
	// où root écrit.
	if err := os.Remove(filepath.Join(src, "www")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "www"), 0o710); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "www", "index.html"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dst, "www")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dst, "www")); err != nil {
		t.Fatal(err)
	}
	_, err = CopyTreeAt(context.Background(), src, "www", dst, "www", -1, KeepOwner)
	if !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("une destination liée doit être refusée, obtenu %v", err)
	}
	if !errors.As(err, &re) || re.Source {
		t.Errorf("le refus d'une destination liée doit se dire à la racine de destination : %#v", err)
	}
	if _, err := os.Lstat(filepath.Join(secret, "index.html")); !os.IsNotExist(err) {
		t.Errorf("root a écrit dans la cible du lien : %v", err)
	}
}

func TestDirAtNeLitPasUnFichierDUnAutreCompte(t *testing.T) {
	// Un lien physique ne se voit pas comme un lien : dans un répertoire qu'un
	// non-root possède, c'est le propriétaire du fichier ouvert qui trahit un
	// fichier d'un autre compte.
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".ssh", "id.pub"), []byte("clé\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDirAt(root, ".ssh")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if data, _, err := d.ReadFile("id.pub", os.Getuid()+1, 1<<10); err == nil || !strings.Contains(err.Error(), "appartient") {
		t.Errorf("un fichier d'un autre compte ne doit pas être lu : %q, %v", string(data), err)
	}
	if data, _, err := d.ReadFile("id.pub", os.Getuid(), 1<<10); err != nil || string(data) != "clé\n" {
		t.Errorf("le fichier du compte attendu doit être lu : %q, %v", string(data), err)
	}
}

func TestDirAtNeLitPasUnFichierDemesure(t *testing.T) {
	// Le fichier appartient au compte qui possède le répertoire, et ce compte lui
	// donne la taille qu'il veut : une clé creuse de 1 Gio ne lui coûte aucun
	// bloc, et root la lisait en entier en mémoire.
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	creux := filepath.Join(root, ".ssh", "id")
	if err := os.WriteFile(creux, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(creux, 1<<30); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".ssh", "id.pub"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDirAt(root, ".ssh")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if data, _, err := d.ReadFile("id", os.Getuid(), 16<<10); err == nil || !strings.Contains(err.Error(), "octets") {
		t.Errorf("un fichier au-delà de la borne ne doit pas être lu : %d octets, %v", len(data), err)
	}
	// La borne est incluse : un fichier qui l'atteint tout juste est lu.
	if data, _, err := d.ReadFile("id.pub", os.Getuid(), 10); err != nil || string(data) != "0123456789" {
		t.Errorf("un fichier à la borne doit être lu : %q, %v", string(data), err)
	}
	if _, _, err := d.ReadFile("id.pub", os.Getuid(), 9); err == nil {
		t.Error("un octet de trop doit suffire au refus")
	}
}

// arbreSource crée src/www et rend les deux chemins.
func arbreSource(t *testing.T) (src, www string) {
	t.Helper()
	src = t.TempDir()
	www = filepath.Join(src, "www")
	if err := os.Mkdir(www, 0o710); err != nil {
		t.Fatal(err)
	}
	return src, www
}

// arbreDestination crée dst/www, comme l'apply du projet cible l'aurait posé.
func arbreDestination(t *testing.T) (dst, www string) {
	t.Helper()
	return arbreSource(t)
}

func TestCopyTreeAtReposeLesLiensPhysiquesInternes(t *testing.T) {
	// Un fichier lié N fois dans la source était écrit N fois par root : le
	// locataire, qui ne paie qu'une fois, lui faisait remplir le disque.
	src, www := arbreSource(t)
	for _, d := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(www, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	premier := filepath.Join(www, "a", "lie.bin")
	contenu := strings.Repeat("0123456789abcdef", 4096)
	if err := os.WriteFile(premier, []byte(contenu), 0o640); err != nil {
		t.Fatal(err)
	}
	noms := []string{"a/lie.bin", "b/lie.bin"}
	for i := 0; i < 20; i++ {
		noms = append(noms, fmt.Sprintf("lie-%02d.bin", i))
	}
	for _, n := range noms[1:] {
		if err := os.Link(premier, filepath.Join(www, filepath.FromSlash(n))); err != nil {
			t.Fatal(err)
		}
	}
	dst, out := arbreDestination(t)

	skipped, err := CopyTreeAt(context.Background(), src, "www", dst, "www", -1, KeepOwner)
	if err != nil || skipped.Count != 0 {
		t.Fatalf("copie : %v, écartés %+v", err, skipped)
	}
	var ref unix.Stat_t
	if err := unix.Lstat(filepath.Join(out, "a", "lie.bin"), &ref); err != nil {
		t.Fatal(err)
	}
	if int(ref.Nlink) != len(noms) {
		t.Errorf("la copie porte %d lien(s), la source %d : chaque lien a été écrit à part", ref.Nlink, len(noms))
	}
	for _, n := range noms {
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(out, filepath.FromSlash(n)), &st); err != nil {
			t.Fatal(err)
		}
		if st.Ino != ref.Ino {
			t.Errorf("%s est un fichier distinct de la copie liée", n)
		}
	}
	if data, err := os.ReadFile(filepath.Join(out, "lie-19.bin")); err != nil || string(data) != contenu {
		t.Errorf("contenu du lien : %d octets, %v", len(data), err)
	}
	if info, err := os.Stat(filepath.Join(out, "b", "lie.bin")); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("mode repris par le lien : %v, %v", info, err)
	}
}

func TestCopyTreeAtGardeLesFichiersCreux(t *testing.T) {
	// Un fichier creux ne coûte rien à son propriétaire ; recopié plein, il fait
	// écrire à root jusqu'à sa taille apparente — seize tébioctets sur ext4.
	src, www := arbreSource(t)
	const taille = 64 << 20
	f, err := os.Create(filepath.Join(www, "creux.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("début"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("milieu"), taille/2); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(taille); err != nil {
		t.Fatal(err)
	}
	f.Close()
	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(www, "creux.bin"), &st); err != nil {
		t.Fatal(err)
	}
	if st.Blocks*512 >= taille/2 {
		t.Skip("le système de fichiers de l'essai ne fait pas de fichiers creux")
	}
	dst, out := arbreDestination(t)

	if _, err := CopyTreeAt(context.Background(), src, "www", dst, "www", -1, KeepOwner); err != nil {
		t.Fatal(err)
	}
	var cst unix.Stat_t
	if err := unix.Stat(filepath.Join(out, "creux.bin"), &cst); err != nil {
		t.Fatal(err)
	}
	if cst.Size != taille {
		t.Errorf("taille %d, attendue %d", cst.Size, taille)
	}
	if cst.Blocks*512 > 1<<20 {
		t.Errorf("la copie occupe %d Kio pour %d Kio dans la source : les trous ont été écrits",
			cst.Blocks/2, st.Blocks/2)
	}
	c, err := os.Open(filepath.Join(out, "creux.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for off, attendu := range map[int64]string{0: "début", taille / 2: "milieu", taille - 4: "\x00\x00\x00\x00"} {
		buf := make([]byte, len(attendu))
		if _, err := c.ReadAt(buf, off); err != nil || string(buf) != attendu {
			t.Errorf("à %d : %q, attendu %q (%v)", off, buf, attendu, err)
		}
	}
}

func TestLaCopieNEcritPasPlusQueLaSourceNOccupait(t *testing.T) {
	// Le compte source comble les trous devant la lecture et les perce derrière :
	// relues au fil de la copie, les étendues faisaient écrire à root toute la
	// taille apparente, la source n'occupant jamais qu'une fenêtre. La course se
	// rejoue ici en deux temps : la mesure, puis des données qui apparaissent.
	dir := t.TempDir()
	const taille = 64 << 20
	src := filepath.Join(dir, "creux.bin")
	if err := os.WriteFile(src, []byte("début"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(src, taille); err != nil {
		t.Fatal(err)
	}
	in, err := unix.Open(src, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(in)
	data, err := dataSize(context.Background(), in, taille)
	if err != nil {
		t.Fatal(err)
	}
	if data >= taille/2 {
		t.Skip("le système de fichiers de l'essai ne fait pas de fichiers creux")
	}
	f, err := os.OpenFile(src, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(strings.Repeat("x", 1<<20)), taille/2); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := unix.Open(filepath.Join(dir, "copie.bin"), unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(out)

	if err := copyData(context.Background(), out, in, taille, data); err == nil {
		t.Error("des données apparues pendant la copie doivent l'interrompre")
	}
	var st unix.Stat_t
	if err := unix.Fstat(out, &st); err != nil {
		t.Fatal(err)
	}
	if st.Blocks*512 > data+64<<10 {
		t.Errorf("root a écrit %d Kio pour %d octets de données mesurés avant la copie", st.Blocks/2, data)
	}
}

func TestCopyTreeAtEcarteLesFichiersDUnAutreCompte(t *testing.T) {
	// Un lien physique vers un fichier de root n'a pas toujours plusieurs liens :
	// quand l'original est remplacé par renommage — /etc/shadow, que le useradd
	// du clone réécrit —, celui du locataire reste seul. Seul le propriétaire le
	// trahit, et ce doit être le compte source, quel que soit Nlink.
	src, www := arbreSource(t)
	if err := os.Mkdir(filepath.Join(www, "sous"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"durshadow", "sous/autre"} {
		if err := os.WriteFile(filepath.Join(www, filepath.FromSlash(n)), []byte("secret\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("sous/autre", filepath.Join(www, "lien")); err != nil {
		t.Fatal(err)
	}
	dst, out := arbreDestination(t)

	// Le compte source n'est pas celui qui possède ces fichiers.
	skipped, err := CopyTreeAt(context.Background(), src, "www", dst, "www", os.Getuid()+1, KeepOwner)
	if err != nil {
		t.Fatal(err)
	}
	if skipped.Count != 2 || !strings.Contains(strings.Join(skipped.Named, "\n"), "fichier de l'uid") {
		t.Errorf("les deux fichiers étrangers au compte source doivent être écartés : %+v", skipped)
	}
	for _, n := range []string{"durshadow", "sous/autre"} {
		if _, err := os.Lstat(filepath.Join(out, filepath.FromSlash(n))); !os.IsNotExist(err) {
			t.Errorf("%s a été recopié : %v", n, err)
		}
	}
	// Ce qui ne porte aucun contenu reste recopié : le répertoire et le lien.
	if info, err := os.Lstat(filepath.Join(out, "sous")); err != nil || !info.IsDir() {
		t.Errorf("le répertoire doit être recopié : %v, %v", info, err)
	}
	if got, err := os.Readlink(filepath.Join(out, "lien")); err != nil || got != "sous/autre" {
		t.Errorf("le lien doit être recopié : %q, %v", got, err)
	}
}

func TestCopyTreeAtRendLesRepertoiresCreesMemeEnEchec(t *testing.T) {
	// Une copie qui s'arrête en chemin laissait ses répertoires en 0700 à root :
	// le compte de destination ne pouvait ni les lire ni les vider, et aucun
	// apply ne les reprenait.
	if os.Geteuid() == 0 {
		t.Skip("root lit un fichier en 0000 : l'échec ne se provoque pas ainsi")
	}
	src, www := arbreSource(t)
	if err := os.Mkdir(filepath.Join(www, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(www, "a", "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(www, "a", "z.txt"), []byte("z"), 0o000); err != nil {
		t.Fatal(err)
	}
	dst, out := arbreDestination(t)

	_, err := CopyTreeAt(context.Background(), src, "www", dst, "www", -1, KeepOwner)
	if err == nil {
		t.Fatal("un fichier illisible doit arrêter la copie")
	}
	// Une partie du contenu est en place : ce n'est pas un refus à la racine.
	if errors.As(err, new(*RootError)) {
		t.Errorf("une erreur en cours de copie ne doit pas se dire à la racine : %v", err)
	}
	if info, err := os.Stat(filepath.Join(out, "a")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("le répertoire créé doit recevoir son mode malgré l'échec : %v, %v", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(out, "a", "b.txt")); err != nil || string(data) != "b" {
		t.Errorf("ce qui a été recopié avant l'échec reste en place : %q, %v", string(data), err)
	}
}

func TestCopyTreeAtRetireUnFichierInterrompu(t *testing.T) {
	// Un fichier dont la copie échoue — disque plein, propriétaire refusé — ne
	// reste pas à moitié écrit, en 0600 à root, dans le répertoire du projet.
	if os.Geteuid() == 0 {
		t.Skip("root donne un fichier à n'importe quel compte : l'échec ne se provoque pas ainsi")
	}
	src, www := arbreSource(t)
	if err := os.WriteFile(filepath.Join(www, "index.html"), []byte("<p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst, out := arbreDestination(t)

	if _, err := CopyTreeAt(context.Background(), src, "www", dst, "www", -1, Owner{UID: os.Getuid() + 1, GID: -1}); err == nil {
		t.Fatal("un propriétaire refusé doit arrêter la copie")
	}
	if _, err := os.Lstat(filepath.Join(out, "index.html")); !os.IsNotExist(err) {
		t.Errorf("la copie interrompue est restée en place : %v", err)
	}
}

func TestLeLienPhysiqueNeSuitPasUneCopieDeplacee(t *testing.T) {
	// Entre la première copie d'un fichier lié et la suivante, le compte de
	// destination a pu renommer ce qui lui appartient : le lien se pose sur
	// l'inode de la copie, retrouvé sans suivre de lien, ou pas du tout.
	dst, out := arbreDestination(t)
	if err := os.Mkdir(filepath.Join(out, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "a", "f"), []byte("copie"), 0o600); err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Lstat(filepath.Join(out, "a", "f"), &st); err != nil {
		t.Fatal(err)
	}
	root, err := openDirAt(dst, "www", false, 0, KeepOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(root)
	racine := &dirNode{}
	c := &treeCopy{ctx: context.Background(), owner: KeepOwner, dstPath: out, links: map[inode]copied{},
		open: []openDir{{node: racine, fd: root}}}
	defer c.forget()
	first := copied{dir: racine.child("a"), name: "f", node: nodeOf(&st)}
	g := filepath.Join(out, "g")

	// « a » remplacé par un lien vers un répertoire qui porte un « f » : refusé.
	ailleurs := t.TempDir()
	if err := os.WriteFile(filepath.Join(ailleurs, "f"), []byte("de root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(out, "a"), filepath.Join(out, "a.orig")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ailleurs, filepath.Join(out, "a")); err != nil {
		t.Fatal(err)
	}
	if err := c.hardlink(root, "g", racine, first); err == nil {
		t.Error("un composant lié doit faire refuser le lien physique")
	}
	// « a » redevenu répertoire, mais « f » n'est plus la copie : refusé aussi.
	if err := os.Remove(filepath.Join(out, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(out, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "a", "f"), []byte("autre"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.hardlink(root, "g", racine, first); err == nil {
		t.Error("un fichier qui n'est plus la copie ne doit pas être lié")
	}
	if _, err := os.Lstat(g); !os.IsNotExist(err) {
		t.Errorf("un lien a été posé malgré le refus : %v", err)
	}
	// La copie remise en place : le lien la rejoint.
	if err := os.RemoveAll(filepath.Join(out, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(out, "a.orig"), filepath.Join(out, "a")); err != nil {
		t.Fatal(err)
	}
	if err := c.hardlink(root, "g", racine, first); err != nil {
		t.Fatal(err)
	}
	var got unix.Stat_t
	if err := unix.Lstat(g, &got); err != nil || got.Ino != st.Ino {
		t.Errorf("le lien doit rejoindre la copie : inode %d, attendu %d (%v)", got.Ino, st.Ino, err)
	}
}

// cheminProfond crée sous www une chaîne de profondeur répertoires, chacun nommé
// nom, et rend le descripteur du plus profond. La chaîne se crée par
// descripteurs : au-delà de PATH_MAX, elle ne se crée plus par son nom.
//
// Le profil AppArmor docker-default refuse pourtant tout chemin de plus de huit
// Kio environ, même atteint par descripteur (ENAMETOOLONG) : c'est alors l'essai
// que l'environnement empêche, non la copie qui échoue, et il est sauté.
func cheminProfond(t *testing.T, www string, profondeur int, nom string) int {
	t.Helper()
	fd, err := unix.Open(www, dirFlags, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < profondeur; i++ {
		err := unix.Mkdirat(fd, nom, 0o755)
		next := -1
		if err == nil {
			next, err = unix.Openat(fd, nom, dirFlags, 0)
		}
		unix.Close(fd)
		if errors.Is(err, unix.ENAMETOOLONG) {
			t.Skipf("niveau %d : %v — l'environnement borne la longueur des chemins", i+1, err)
		}
		if err != nil {
			t.Fatal(err)
		}
		fd = next
	}
	t.Cleanup(func() { unix.Close(fd) })
	return fd
}

func TestCopyTreeAtNeRetientPasUnCheminParNiveau(t *testing.T) {
	// La profondeur est aux mains du compte source. Chaque niveau retenait une
	// copie de la liste de ses composants et deux chemins complets, le temps de
	// toute la descente : la mémoire de root croissait comme le carré de la
	// profondeur — huit cents Mio pour huit mille niveaux, quelques Mio de disque
	// chez le locataire.
	const profondeur = 400
	src, www := arbreSource(t)
	cheminProfond(t, www, profondeur, strings.Repeat("n", 250))
	dst, _ := arbreDestination(t)

	var avant, apres runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&avant)
	_, err := CopyTreeAt(context.Background(), src, "www", dst, "www", -1, KeepOwner)
	runtime.ReadMemStats(&apres)
	if err != nil {
		t.Fatal(err)
	}
	// Un chemin de ce niveau pèse près de cent Kio : ce qu'un niveau retiendrait de
	// ses ancêtres crève ce budget, qu'une copie en mémoire linéaire tient de loin.
	const budget = profondeur * 32 << 10
	if alloue := apres.TotalAlloc - avant.TotalAlloc; alloue > budget {
		t.Errorf("%d Kio alloués pour %d niveaux, au-delà de %d Kio : la mémoire croît plus vite que la profondeur",
			alloue>>10, profondeur, budget>>10)
	}
}

func TestCopyTreeAtNeDescendPasAuDelaDeLaBorne(t *testing.T) {
	// Recopiée par descripteurs, une arborescence n'a plus la borne de fait de
	// « cp -a », PATH_MAX : la copie descendait aussi loin que le compte source
	// empilait de répertoires, deux descripteurs ouverts par niveau.
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil || lim.Cur < 2*maxCopyDepth+64 {
		t.Skipf("%d descripteurs au plus : la borne ne s'atteint pas ici (%v)", lim.Cur, err)
	}
	src, www := arbreSource(t)
	fond := cheminProfond(t, www, maxCopyDepth, "a")
	if err := unix.Mkdirat(fond, "trop", 0o755); err != nil {
		t.Fatal(err)
	}
	for _, nom := range []string{"dernier.txt", "trop/au-dela.txt"} {
		fd, err := unix.Openat(fond, nom, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		unix.Close(fd)
	}
	if err := os.WriteFile(filepath.Join(www, "index.html"), []byte("<p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst, out := arbreDestination(t)

	skipped, err := CopyTreeAt(context.Background(), src, "www", dst, "www", -1, KeepOwner)
	if err != nil {
		t.Fatalf("la borne écarte un sous-arbre, elle n'arrête pas la copie : %v", err)
	}
	if skipped.Count != 1 || len(skipped.Named) != 1 || !strings.Contains(skipped.Named[0], "trop") ||
		!strings.Contains(skipped.Named[0], fmt.Sprintf("au-delà de %d niveaux", maxCopyDepth)) {
		t.Errorf("le répertoire au-delà de la borne doit être écarté, et nommé : %+v", skipped)
	}
	profond := filepath.Join(out, strings.Repeat("a"+string(filepath.Separator), maxCopyDepth))
	if _, err := os.Lstat(filepath.Join(profond, "dernier.txt")); err != nil {
		t.Errorf("ce qui tient dans la borne est recopié : %v", err)
	}
	if _, err := os.Lstat(filepath.Join(profond, "trop")); !os.IsNotExist(err) {
		t.Errorf("le répertoire au-delà de la borne a été recopié : %v", err)
	}
	if _, err := os.Lstat(filepath.Join(out, "index.html")); err != nil {
		t.Errorf("le reste de l'arborescence est recopié : %v", err)
	}
}

func TestCopyTreeAtCompteLesEntreesEcarteesSansToutesLesNommer(t *testing.T) {
	// Chaque entrée écartée était retenue avec son chemin complet : leur nombre et
	// leur profondeur étant aux mains du compte source, la liste pesait l'un
	// multiplié par l'autre dans la mémoire de root.
	src, www := arbreSource(t)
	const tubes = maxSkippedNamed + 10
	for i := 0; i < tubes; i++ {
		if err := unix.Mkfifo(filepath.Join(www, fmt.Sprintf("tube-%03d", i)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dst, _ := arbreDestination(t)
	skipped, err := CopyTreeAt(context.Background(), src, "www", dst, "www", -1, KeepOwner)
	if err != nil {
		t.Fatal(err)
	}
	if skipped.Count != tubes || len(skipped.Named) != maxSkippedNamed {
		t.Errorf("%d entrées comptées et %d nommées, attendu %d et %d", skipped.Count, len(skipped.Named),
			tubes, maxSkippedNamed)
	}
}

func TestCopyTreeAtCiteLesNomsQuIlEcarte(t *testing.T) {
	// Le nom d'une entrée est aux mains du compte source : affiché tel quel, un
	// saut de ligne et une séquence d'échappement y forgeaient une ligne entière
	// de la sortie de « site clone ».
	src, www := arbreSource(t)
	forge := "x : fichier spécial\n✓ contenu copié : tout va bien\n\x1b[1A"
	if err := unix.Mkfifo(filepath.Join(www, forge), 0o600); err != nil {
		t.Fatal(err)
	}
	dst, _ := arbreDestination(t)
	skipped, err := CopyTreeAt(context.Background(), src, "www", dst, "www", -1, KeepOwner)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped.Named) != 1 {
		t.Fatalf("une entrée écartée attendue : %+v", skipped)
	}
	if nom := skipped.Named[0]; strings.ContainsAny(nom, "\n\x1b") || !strings.Contains(nom, `\n✓ contenu copié`) {
		t.Errorf("le nom doit être cité, sans saut de ligne ni échappement : %q", nom)
	}
}

func TestCopyTreeAtNommeLaLimiteDeDescripteurs(t *testing.T) {
	// Deux descripteurs par niveau : sous une limite RLIMIT_NOFILE de 1024, la
	// copie s'arrêtait vers cinq cents niveaux sur un « too many open files » qui
	// ne disait pas ce qui les consommait.
	src, www := arbreSource(t)
	cheminProfond(t, www, 200, "a")
	dst, _ := arbreDestination(t)
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	bas := lim
	bas.Cur = 128
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &bas); err != nil {
		t.Skipf("limite de descripteurs non réglable : %v", err)
	}
	_, err := CopyTreeAt(context.Background(), src, "www", dst, "www", -1, KeepOwner)
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(err, unix.EMFILE) || !strings.Contains(err.Error(), "deux descripteurs par niveau") ||
		!strings.Contains(err.Error(), "RLIMIT_NOFILE") {
		t.Errorf("l'arrêt doit nommer la limite de descripteurs : %v", err)
	}
}

func TestCopyTreeAtNeCiteQueLaFinDUnCheminProfond(t *testing.T) {
	// Chaque entrée écartée était nommée par son chemin complet : mille niveaux de
	// noms de 255 octets que la citation quadruple faisaient retenir à root
	// cinquante Mio pour cinquante tubes, puis les imprimaient dans le terminal de
	// l'exploitant.
	src, www := arbreSource(t)
	nom := strings.Repeat("\x01", 250)
	fond := cheminProfond(t, www, 12, nom)
	if err := unix.Mkfifoat(fond, "tube", 0o600); err != nil {
		t.Fatal(err)
	}
	dst, _ := arbreDestination(t)
	skipped, err := CopyTreeAt(context.Background(), src, "www", dst, "www", -1, KeepOwner)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped.Named) != 1 {
		t.Fatalf("une entrée écartée attendue : %d", len(skipped.Named))
	}
	nomme := skipped.Named[0]
	// La fin du chemin, citée : au pire quatre caractères par octet.
	if len(nomme) > 4*maxShownPath+64 {
		t.Errorf("%d octets pour nommer une entrée : le chemin n'est pas borné", len(nomme))
	}
	if !strings.HasPrefix(nomme, `…"`) || !strings.HasSuffix(nomme, `/tube" : fichier spécial`) ||
		!strings.Contains(nomme, `\x01`) {
		t.Errorf("la fin du chemin doit être citée, la coupure marquée hors des guillemets : %.200q", nomme)
	}
	// Un chemin court reste entier.
	if got := quotedPath("/home/shop.fr/www", (&dirNode{}).child("a"), "b"); got != `"/home/shop.fr/www/a/b"` {
		t.Errorf("chemin court : %s", got)
	}
}

// annuleApres est un contexte qui se dit annulé à partir de sa consultation
// n° reste+1, et qui appelle alors constat, une fois : de quoi arrêter une copie
// en chacun de ses points d'arrêt tour à tour, et relever ce qu'elle avait écrit
// à cet instant.
type annuleApres struct {
	context.Context
	reste   int
	constat func()
}

func (c *annuleApres) Err() error {
	if c.reste > 0 {
		c.reste--
		return nil
	}
	if c.constat != nil {
		constat := c.constat
		c.constat = nil
		constat()
	}
	return context.Canceled
}

// inventaire rend le type et le mode de chaque entrée sous root, sans la suivre.
func inventaire(t *testing.T, root string) map[string]os.FileMode {
	t.Helper()
	entrees := map[string]os.FileMode{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		entrees[rel] = info.Mode()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entrees
}

func TestCopyTreeAtSuitLAnnulation(t *testing.T) {
	// Faite dans le processus, la copie ne mourait plus avec le groupe que
	// l'annulation tue : main capte SIGINT et SIGTERM, et ni Ctrl-C ni l'arrêt
	// d'une unité ne l'arrêtaient plus — soixante-six secondes pour un www que le
	// locataire allonge à volonté. Seul un SIGKILL y mettait fin, en laissant à
	// root, en 0700, les répertoires créés. La copie est arrêtée ici en chacun de
	// ses points d'arrêt tour à tour.
	src, www := arbreSource(t)
	for _, d := range []struct {
		nom  string
		mode os.FileMode
	}{{"a", 0o750}, {"c", 0o755}, {"c/d", 0o710}} {
		p := filepath.Join(www, filepath.FromSlash(d.nom))
		if err := os.Mkdir(p, d.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, d.mode); err != nil {
			t.Fatal(err)
		}
	}
	gros := []byte(strings.Repeat("0123456789abcdef", 1<<16)) // 1 Mio : plusieurs blocs de copie
	for f, contenu := range map[string][]byte{"a/gros.bin": gros, "a/z.txt": []byte("z"), "b.txt": []byte("b"),
		"c/d/e.txt": []byte("e")} {
		if err := os.WriteFile(filepath.Join(www, filepath.FromSlash(f)), contenu, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	source := inventaire(t, www)

	interrompues, fichierRetire := 0, false
	for n := 0; ; n++ {
		if n > 1000 {
			t.Fatal("la copie n'aboutit jamais")
		}
		dst, out := arbreDestination(t)
		var constat map[string]os.FileMode
		ctx := &annuleApres{Context: context.Background(), reste: n,
			constat: func() { constat = inventaire(t, out) }}
		_, err := CopyTreeAt(ctx, src, "www", dst, "www", -1, KeepOwner)
		copie := inventaire(t, out)
		if err == nil {
			if len(copie) != len(source) {
				t.Errorf("copie menée à son terme : %d entrée(s), la source %d", len(copie), len(source))
			}
			break
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("arrêt n° %d : l'erreur doit être celle du contexte, obtenu %v", n, err)
		}
		interrompues++
		// Rien n'est écrit après l'annulation ; seul le fichier en cours est retiré.
		for rel := range copie {
			if _, vu := constat[rel]; !vu {
				t.Errorf("arrêt n° %d : %s écrit après l'annulation", n, rel)
			}
		}
		for rel, mode := range constat {
			if _, reste := copie[rel]; !reste {
				if !mode.IsRegular() {
					t.Errorf("arrêt n° %d : %s retiré, alors que seul un fichier interrompu doit l'être", n, rel)
				}
				fichierRetire = true
			}
		}
		// Ce qui reste est entier, et rendu : un répertoire créé ne reste pas en 0700.
		for rel, mode := range copie {
			if mode != source[rel] {
				t.Errorf("arrêt n° %d : %s en %v, la source en %v", n, rel, mode, source[rel])
			}
			if mode.IsRegular() {
				a, _ := os.ReadFile(filepath.Join(www, rel))
				b, _ := os.ReadFile(filepath.Join(out, rel))
				if !bytes.Equal(a, b) {
					t.Errorf("arrêt n° %d : %s recopié en partie (%d octets sur %d)", n, rel, len(b), len(a))
				}
			}
		}
	}
	t.Logf("%d points d'arrêt éprouvés", interrompues)
	if interrompues < len(source) || !fichierRetire {
		t.Errorf("%d arrêt(s), fichier interrompu retiré : %v — la copie doit s'arrêter entre les entrées et "+
			"entre les blocs d'un fichier", interrompues, fichierRetire)
	}

	// La mesure des étendues s'arrête aussi : leur nombre n'est borné que par la
	// taille apparente.
	fd, err := unix.Open(filepath.Join(www, "a", "gros.bin"), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	annule, stop := context.WithCancel(context.Background())
	stop()
	if _, err := dataSize(annule, fd, int64(len(gros))); !errors.Is(err, context.Canceled) {
		t.Errorf("la mesure doit suivre l'annulation : %v", err)
	}
}

func TestLeLienPhysiqueSeRetrouveParLesDescripteursTenus(t *testing.T) {
	// Chaque nom d'un fichier lié rouvrait la chaîne de ses répertoires depuis la
	// racine de destination : vingt mille liens au fond de mille niveaux tenaient
	// root soixante-sept secondes, un lien physique ne coûtant au compte source
	// qu'une entrée de répertoire. Le répertoire de la première copie s'atteint
	// désormais par le descripteur que la descente tient, ou par celui retenu la
	// fois précédente — ce que montre un renommage, qu'une réouverture par les
	// noms ne traverserait pas.
	dst, out := arbreDestination(t)
	for _, d := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(out, d), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, d, "f"), []byte("copie de "+d), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inodeDe := func(p string) inode {
		t.Helper()
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			t.Fatal(err)
		}
		return nodeOf(&st)
	}
	premierA := copied{name: "f", node: inodeDe(filepath.Join(out, "a", "f"))}
	premierB := copied{name: "f", node: inodeDe(filepath.Join(out, "b", "f"))}
	root, err := openDirAt(dst, "www", false, 0, KeepOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(root)
	fdA, err := openDirAt(dst, "www/a", false, 0, KeepOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fdA)
	racine := &dirNode{}
	premierA.dir, premierB.dir = racine.child("a"), racine.child("b")
	c := &treeCopy{ctx: context.Background(), owner: KeepOwner, dstPath: out, links: map[inode]copied{},
		open: []openDir{{node: racine, fd: root}, {node: premierA.dir, fd: fdA}}}
	defer c.forget()

	// Le répertoire en cours de copie : c'est celui que la descente tient.
	if err := os.Rename(filepath.Join(out, "a"), filepath.Join(out, "a-renomme")); err != nil {
		t.Fatal(err)
	}
	if err := c.hardlink(fdA, "g", premierA.dir, premierA); err != nil {
		t.Fatalf("un lien vers le répertoire en cours ne doit rien rouvrir : %v", err)
	}
	if inodeDe(filepath.Join(out, "a-renomme", "g")) != premierA.node {
		t.Error("le lien doit rejoindre la première copie")
	}
	// Un autre répertoire : rouvert une fois, puis retenu.
	if err := c.hardlink(fdA, "h", premierA.dir, premierB); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(out, "b"), filepath.Join(out, "b-renomme")); err != nil {
		t.Fatal(err)
	}
	if err := c.hardlink(fdA, "i", premierA.dir, premierB); err != nil {
		t.Fatalf("le répertoire où la copie a été retrouvée doit être retenu : %v", err)
	}
	if inodeDe(filepath.Join(out, "a-renomme", "i")) != premierB.node {
		t.Error("le lien doit rejoindre la première copie")
	}
}

func TestDirAtRetireUnFichierInterrompu(t *testing.T) {
	// Une écriture interrompue laissait le fichier à root, à moitié écrit, dans le
	// répertoire du projet : la tentative suivante le refusait comme étranger.
	if os.Geteuid() == 0 {
		t.Skip("root donne un fichier à n'importe quel compte : l'échec ne se provoque pas ainsi")
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDirAt(root, ".ssh")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.WriteFile("id", []byte("clé\n"), 0o600, Owner{UID: os.Getuid() + 1, GID: -1}); err == nil {
		t.Fatal("un propriétaire refusé doit faire échouer l'écriture")
	}
	if _, err := os.Lstat(filepath.Join(root, ".ssh", "id")); !os.IsNotExist(err) {
		t.Errorf("le fichier interrompu est resté en place : %v", err)
	}
}
