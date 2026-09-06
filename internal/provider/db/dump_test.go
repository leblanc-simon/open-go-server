package db

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
)

// dumpFixture installe l'outillage que Dump exige — un socket MySQL et un
// mysqldump — et fait rendre au Recorder le contenu de la sauvegarde.
func dumpFixture(t *testing.T) (*Provider, *model.Project, model.Database) {
	t.Helper()
	prov, proj := fixture(t)
	prefix := prov.Paths.Prefix()

	socket := filepath.Join(prefix, "/run/mysqld/mysqld.sock")
	tool := filepath.Join(prefix, "/usr/bin/mysqldump")
	for _, path := range []string{socket, tool} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rec, ok := prov.Exec.(*exec.Recorder)
	if !ok {
		t.Fatalf("exécuteur simulé attendu, %T", prov.Exec)
	}
	rec.Responses[tool+" --socket="+socket] = exec.Result{Stdout: dumpContent}
	return prov, proj, model.Database{Engine: "mysql", Name: "shopfr"}
}

const dumpContent = "-- sauvegarde de shopfr\n"

func TestSauvegardeVersUnFichierOrdinaireCreeUnInodeNeuf(t *testing.T) {
	// Écrire dans un fichier posé d'avance lui laisserait son propriétaire et son
	// mode : le 0600 annoncé ne s'applique qu'à la création. Le lien dur donne un
	// témoin de l'inode — s'il porte encore le dump, c'est qu'on a réécrit celui
	// de quelqu'un d'autre.
	prov, proj, decl := dumpFixture(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "dump.sql")
	if err := os.WriteFile(target, []byte("piège"), 0o666); err != nil {
		t.Fatal(err)
	}
	temoin := filepath.Join(dir, "temoin.sql")
	if err := os.Link(target, temoin); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.Dump(context.Background(), proj, decl, target); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(temoin); err != nil || string(data) != "piège" {
		t.Errorf("l'inode préexistant a reçu la sauvegarde : %q, %v", data, err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode %04o : une sauvegarde porte toutes les données de l'application", perm)
	}
}

func TestSauvegardeVersUnTubeNommeNeLeDeliePas(t *testing.T) {
	// « -O /dev/null » ou « -O /dev/stdout » : le retrait préalable n'assainit rien
	// sur un inode qui n'est pas un fichier ordinaire — il le détruit, et root
	// délie sans peine dans /dev. Le tube nommé est le même cas, éprouvable sans
	// privilèges.
	prov, proj, decl := dumpFixture(t)
	tube := filepath.Join(t.TempDir(), "tube")
	if err := syscall.Mkfifo(tube, 0o600); err != nil {
		t.Fatal(err)
	}
	avant, err := os.Lstat(tube)
	if err != nil {
		t.Fatal(err)
	}
	// Le lecteur est ouvert avant l'écriture : sans lui, l'ouverture en écriture
	// d'un tube attend indéfiniment.
	lecteur, err := os.OpenFile(tube, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lecteur.Close()

	if _, err := prov.Dump(context.Background(), proj, decl, tube); err != nil {
		t.Fatal(err)
	}
	apres, err := os.Lstat(tube)
	if err != nil {
		t.Fatalf("le tube a été délié : %v", err)
	}
	if apres.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("%s est devenu un %v : l'objet visé a été détruit et remplacé", tube, apres.Mode())
	}
	if !os.SameFile(avant, apres) {
		t.Error("le tube a été remplacé par un autre inode")
	}
	buf := make([]byte, len(dumpContent))
	if n, err := lecteur.Read(buf); err != nil || string(buf[:n]) != dumpContent {
		t.Errorf("le tube n'a pas reçu la sauvegarde : %q, %v", buf[:n], err)
	}
}

func TestSauvegardeATraversUnLienNeLeRemplacePas(t *testing.T) {
	prov, proj, decl := dumpFixture(t)
	dir := t.TempDir()
	cible := filepath.Join(dir, "cible.sql")
	if err := os.WriteFile(cible, []byte("un contenu bien plus long que la sauvegarde"), 0o644); err != nil {
		t.Fatal(err)
	}
	lien := filepath.Join(dir, "lien.sql")
	if err := os.Symlink(cible, lien); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.Dump(context.Background(), proj, decl, lien); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(lien)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("le lien a été délié : %v, %v", info, err)
	}
	// Écrire à travers sans tronquer laisserait la queue de l'ancien contenu.
	if data, err := os.ReadFile(cible); err != nil || string(data) != dumpContent {
		t.Errorf("contenu de la cible : %q, %v", data, err)
	}
}

func TestSauvegardeVersUnRepertoireNeLeSupprimePas(t *testing.T) {
	// « -O /var/backups/dumps » mal visé : l'ouverture doit rendre son erreur, et
	// le répertoire rester en place. Il est vide à dessein — un répertoire peuplé
	// résiste de lui-même au retrait, et le test ne prouverait rien.
	prov, proj, decl := dumpFixture(t)
	dir := filepath.Join(t.TempDir(), "sauvegardes")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.Dump(context.Background(), proj, decl, dir); err == nil {
		t.Error("écrire une sauvegarde dans un répertoire doit échouer")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("le répertoire visé a été supprimé : %v, %v", info, err)
	}
}
