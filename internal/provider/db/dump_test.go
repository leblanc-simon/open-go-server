package db

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
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

func TestLesOutilsPostgreSQLSePresententSousLeRoleDuClient(t *testing.T) {
	// Avec OGS_PG_ADMIN, le client d'administration se connecte sous ce rôle ;
	// pg_dump et psql, laissés au rôle du compte système, échouaient là où lui
	// joignait la base — et la suppression la supprimait sans sauvegarde.
	t.Setenv("OGS_PG_ADMIN", "postgres")
	prov, proj := fixture(t)
	prefix := prov.Paths.Prefix()
	conf := filepath.Join(prefix, "etc", "postgresql", "16", "main", "postgresql.conf")
	for path, content := range map[string]string{
		conf: "port = 5433\n",
		filepath.Join(prefix, "usr", "bin", "pg_dump"): "",
		filepath.Join(prefix, "usr", "bin", "psql"):    "",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	decl := model.Database{Engine: "postgres", Name: "shopfr"}
	dump := filepath.Join(t.TempDir(), "shopfr.sql")
	if _, err := prov.Dump(context.Background(), proj, decl, dump); err != nil {
		t.Fatal(err)
	}
	if err := prov.Restore(context.Background(), proj, decl, dump); err != nil {
		t.Fatal(err)
	}
	rec := prov.Exec.(*exec.Recorder)
	for _, tool := range []string{"pg_dump", "psql"} {
		found := false
		for _, c := range rec.Calls() {
			if filepath.Base(c.Name) != tool {
				continue
			}
			found = true
			if !slices.Contains(c.Args, "--username=postgres") {
				t.Errorf("%s doit se présenter sous le rôle du client : %q", tool, c.Args)
			}
		}
		if !found {
			t.Errorf("%s n'a pas été lancé : %v", tool, rec.Calls())
		}
	}
}

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

func TestSauvegardeVersUnTubeNommeLeRefuseSansLeDelier(t *testing.T) {
	// Le retrait préalable n'assainit rien sur un inode qui n'est pas un fichier
	// ordinaire — il le détruit —, et écrire à travers livrerait les données à qui
	// a posé le tube : un utilisateur de projet, dans son home. La destination est
	// refusée, et le tube reste en place. /dev/null et les flux standard sont
	// ouverts par leur nom (outputStreams).
	prov, proj, decl := dumpFixture(t)
	tube := filepath.Join(t.TempDir(), "tube")
	if err := syscall.Mkfifo(tube, 0o600); err != nil {
		t.Fatal(err)
	}
	avant, err := os.Lstat(tube)
	if err != nil {
		t.Fatal(err)
	}
	// Sans lecteur, l'ouverture d'un tube en écriture attend indéfiniment : le
	// refus doit venir avant.
	fini := make(chan error, 1)
	go func() {
		_, err := prov.Dump(context.Background(), proj, decl, tube)
		fini <- err
	}()
	select {
	case err := <-fini:
		if err == nil {
			t.Error("une sauvegarde vers un tube nommé doit être refusée")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("la sauvegarde attend sur un tube nommé")
	}
	apres, err := os.Lstat(tube)
	if err != nil {
		t.Fatalf("le tube a été délié : %v", err)
	}
	if apres.Mode()&os.ModeNamedPipe == 0 || !os.SameFile(avant, apres) {
		t.Errorf("%s est devenu un %v : l'objet visé a été détruit et remplacé", tube, apres.Mode())
	}
	// Le refus précède la sauvegarde : la base n'a pas été lue pour rien.
	for _, c := range prov.Exec.(*exec.Recorder).Calls() {
		if filepath.Base(c.Name) == "mysqldump" {
			t.Errorf("la sauvegarde a tourné vers une destination refusée : %s", c)
		}
	}
	if _, err := prov.Dump(context.Background(), proj, decl, "/dev/null"); err != nil {
		t.Errorf("/dev/null doit rester une destination : %v", err)
	}
}

func TestSauvegardeRefuseUnLienALaPlaceDeLaDestination(t *testing.T) {
	// Un lien posé à la place de la destination n'est ni suivi ni délié : root
	// tronquait puis réécrivait sa cible. Il vit ici dans un répertoire qu'un autre
	// compte que root peut modifier — le cas d'un home de projet.
	prov, proj, decl := dumpFixture(t)
	dir := t.TempDir()
	cible := filepath.Join(dir, "cible.sql")
	const contenu = "un contenu bien plus long que la sauvegarde"
	if err := os.WriteFile(cible, []byte(contenu), 0o644); err != nil {
		t.Fatal(err)
	}
	lien := filepath.Join(dir, "lien.sql")
	if err := os.Symlink(cible, lien); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.Dump(context.Background(), proj, decl, lien); err == nil {
		t.Error("une sauvegarde à travers un lien doit être refusée")
	}
	info, err := os.Lstat(lien)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("le lien a été délié : %v, %v", info, err)
	}
	if data, err := os.ReadFile(cible); err != nil || string(data) != contenu {
		t.Errorf("la cible du lien a été réécrite : %q, %v", data, err)
	}
}

// homeDuProjet crée le home du projet de la fixture : tout ce qui s'y trouve,
// liens compris, est sous le contrôle de son utilisateur (D51).
func homeDuProjet(t *testing.T, proj *model.Project) string {
	t.Helper()
	home := proj.Spec.User.Home
	if err := os.MkdirAll(home, 0o711); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestSauvegardeVersLeHomeNeSuitPasUnLienPoseParLeProjet(t *testing.T) {
	// « ogs db dump shopfr -O /home/shop.fr/sauvegarde.sql », où l'utilisateur a
	// posé un lien vers un fichier de root : root le tronquait puis y écrivait le
	// dump, dont le projet dicte en partie le contenu.
	prov, proj, decl := dumpFixture(t)
	prefix := prov.Paths.Prefix()
	home := homeDuProjet(t, proj)
	victime := filepath.Join(prefix, "etc", "sudoers.d", "ogs-autrefr")
	if err := os.MkdirAll(filepath.Dir(victime), 0o750); err != nil {
		t.Fatal(err)
	}
	const regle = "autrefr ALL=(root) NOPASSWD: /usr/sbin/ogs my *\n"
	if err := os.WriteFile(victime, []byte(regle), 0o640); err != nil {
		t.Fatal(err)
	}
	lien := filepath.Join(home, "sauvegarde.sql")
	if err := os.Symlink(victime, lien); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.Dump(context.Background(), proj, decl, lien); err == nil {
		t.Error("la destination liée doit être refusée")
	}
	if data, _ := os.ReadFile(victime); string(data) != regle {
		t.Errorf("DÉFAUT : root a réécrit %s à travers le lien du home : %q", victime, data)
	}
}

func TestSauvegardeVersLeHomeNeSuitPasUnRepertoireLie(t *testing.T) {
	// « -O /home/shop.fr/sauvegardes/shopfr », où sauvegardes est un lien vers
	// /etc/cron.d : root y créait un fichier que cron lit.
	prov, proj, decl := dumpFixture(t)
	home := homeDuProjet(t, proj)
	cron := filepath.Join(prov.Paths.Prefix(), "etc", "cron.d")
	if err := os.MkdirAll(cron, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cron, filepath.Join(home, "sauvegardes")); err != nil {
		t.Fatal(err)
	}
	_, err := prov.Dump(context.Background(), proj, decl, filepath.Join(home, "sauvegardes", "shopfr"))
	if !errors.Is(err, fs.ErrNotDirectory) {
		t.Errorf("un répertoire lié doit être refusé, obtenu %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cron, "shopfr")); err == nil {
		t.Error("DÉFAUT : root a créé un fichier hors du home, par un répertoire lié")
	}
}

func TestSauvegardeCreeLesRepertoiresManquantsSansSuivreDeLien(t *testing.T) {
	// Un chemin relatif part du répertoire courant ; les répertoires absents sont
	// créés, et la sauvegarde l'est en 0600 quel que soit l'umask.
	prov, proj, decl := dumpFixture(t)
	t.Chdir(t.TempDir())
	if _, err := prov.Dump(context.Background(), proj, decl, filepath.Join("a", "b", "shopfr.sql")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join("a", "b", "shopfr.sql"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("sauvegarde attendue en 0600 : %v %v", info, err)
	}
	if data, _ := os.ReadFile(filepath.Join("a", "b", "shopfr.sql")); string(data) != dumpContent {
		t.Errorf("contenu : %q", data)
	}
}

func TestRestaurationDepuisLeHomeNeLitPasUnLienVersLaSauvegardeDUnVoisin(t *testing.T) {
	// « ogs db restore shopfr -i /home/shop.fr/a-restaurer.sql », un lien vers la
	// sauvegarde d'un autre projet sous /var/lib/ogs/dumps : root la lisait et la
	// chargeait dans la base de shopfr, que le projet lit.
	prov, proj, decl := dumpFixture(t)
	prefix := prov.Paths.Prefix()
	mysql := filepath.Join(prefix, "usr", "bin", "mysql")
	if err := os.WriteFile(mysql, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	voisin := filepath.Join(prov.Paths.DumpsDir(), "autrefr-autrefr-20260928-120000.sql")
	if err := os.MkdirAll(filepath.Dir(voisin), 0o700); err != nil {
		t.Fatal(err)
	}
	const secret = "INSERT INTO clients VALUES ('carte du voisin');\n"
	if err := os.WriteFile(voisin, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	home := homeDuProjet(t, proj)
	entree := filepath.Join(home, "a-restaurer.sql")
	if err := os.Symlink(voisin, entree); err != nil {
		t.Fatal(err)
	}
	if err := prov.Restore(context.Background(), proj, decl, entree); err == nil {
		t.Error("une entrée liée doit être refusée")
	}
	// Un répertoire lié en chemin, de même.
	if err := os.Symlink(filepath.Dir(voisin), filepath.Join(home, "dumps")); err != nil {
		t.Fatal(err)
	}
	if err := prov.Restore(context.Background(), proj, decl,
		filepath.Join(home, "dumps", filepath.Base(voisin))); !errors.Is(err, fs.ErrNotDirectory) {
		t.Errorf("un répertoire lié doit être refusé, obtenu %v", err)
	}
	for _, c := range prov.Exec.(*exec.Recorder).Calls() {
		if c.Name == mysql && strings.Contains(string(c.Stdin), "carte du voisin") {
			t.Errorf("DÉFAUT : la sauvegarde de autrefr, lue à travers un lien, est chargée dans %s", decl.Name)
		}
	}
}

func TestRestaurationRefuseUnTubeNommeSansAttendre(t *testing.T) {
	// Un tube nommé à la place de l'entrée bloquait la lecture — et root avec lui,
	// verrou d'ogs compris — jusqu'à ce que quelqu'un y écrive. Il est refusé sur
	// son descripteur, ouvert sans attendre.
	prov, proj, decl := dumpFixture(t)
	if err := os.WriteFile(filepath.Join(prov.Paths.Prefix(), "usr", "bin", "mysql"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	tube := filepath.Join(t.TempDir(), "tube.sql")
	if err := syscall.Mkfifo(tube, 0o600); err != nil {
		t.Fatal(err)
	}
	fini := make(chan error, 1)
	go func() { fini <- prov.Restore(context.Background(), proj, decl, tube) }()
	select {
	case err := <-fini:
		if err == nil {
			t.Error("un tube nommé doit être refusé")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("la restauration attend sur un tube nommé")
	}
	// Un fichier ordinaire, lui, est restauré.
	ordinaire := filepath.Join(t.TempDir(), "shopfr.sql")
	if err := os.WriteFile(ordinaire, []byte(dumpContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prov.Restore(context.Background(), proj, decl, ordinaire); err != nil {
		t.Fatal(err)
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
