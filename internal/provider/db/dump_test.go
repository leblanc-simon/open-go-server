package db

import (
	"context"
	"errors"
	"fmt"
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
	"leblanc.io/open-go-server/internal/naming"
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
	// La restauration se fait sous le compte du projet (D92) : son mot de passe
	// doit être scellé.
	if _, err := prov.ensurePassword(proj.ID(), "mysql", "shopfr"); err != nil {
		t.Fatal(err)
	}
	return prov, proj, model.Database{Engine: "mysql", Name: "shopfr", User: "shopfr", Privileges: "all"}
}

const dumpContent = "-- sauvegarde de shopfr\n"

func TestLesOutilsPostgreSQLSePresententSousLeRoleDuClient(t *testing.T) {
	// Avec OGS_PG_ADMIN, le client d'administration se connecte sous ce rôle ;
	// pg_dump, laissé au rôle du compte système, échouait là où lui joignait la
	// base — et la suppression la supprimait sans sauvegarde. psql, lui, restaure
	// sous le rôle du projet (D92).
	t.Setenv("OGS_PG_ADMIN", "postgres")
	prov, proj := fixture(t)
	prefix := prov.Paths.Prefix()
	conf := filepath.Join(prefix, "etc", "postgresql", "16", "main", "postgresql.conf")
	for path, content := range map[string]string{
		conf: "port = 5433\n",
		filepath.Join(prefix, "var", "run", "postgresql", ".s.PGSQL.5433"): "",
		filepath.Join(prefix, "usr", "bin", "pg_dump"):                     "",
		filepath.Join(prefix, "usr", "bin", "psql"):                        "",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	decl := model.Database{Engine: "postgres", Name: "shopfr", User: "shopfr"}
	if _, err := prov.ensurePassword(proj.ID(), "postgres", "shopfr"); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(t.TempDir(), "shopfr.sql")
	if _, err := prov.Dump(context.Background(), proj, decl, dump); err != nil {
		t.Fatal(err)
	}
	if err := prov.Restore(context.Background(), proj, decl, dump); err != nil {
		t.Fatal(err)
	}
	rec := prov.Exec.(*exec.Recorder)
	for tool, role := range map[string]string{"pg_dump": "postgres", "psql": "shopfr"} {
		found := false
		for _, c := range rec.Calls() {
			if filepath.Base(c.Name) != tool {
				continue
			}
			found = true
			if !slices.Contains(c.Args, "--username="+role) {
				t.Errorf("%s doit se présenter sous le rôle %s : %q", tool, role, c.Args)
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

// appelDe rend le dernier appel de l'outil nommé.
func appelDe(t *testing.T, prov *Provider, tool string) exec.Command {
	t.Helper()
	var found *exec.Command
	for _, c := range prov.Exec.(*exec.Recorder).Calls() {
		if filepath.Base(c.Name) == tool {
			c := c
			found = &c
		}
	}
	if found == nil {
		t.Fatalf("%s n'a pas été lancé : %v", tool, prov.Exec.(*exec.Recorder).Calls())
	}
	return *found
}

func TestLaRestaurationMySQLEstConfineeAuCompteDuProjet(t *testing.T) {
	// Le fichier restauré est un script que le client exécute. Chargé par
	// « mysql --user=root » sous root, il écrivait dans toutes les bases du parc,
	// et « \! » y lançait une commande shell en root (R1-14). Il doit l'être sous
	// le compte du projet, par un client lancé sous l'utilisateur du projet.
	prov, proj, decl := dumpFixture(t)
	mysql := filepath.Join(prov.Paths.Prefix(), "usr", "bin", "mysql")
	if err := os.WriteFile(mysql, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	entree := filepath.Join(t.TempDir(), "shopfr.sql")
	if err := os.WriteFile(entree, []byte("\\! id > /root/preuve\nUSE mysql;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prov.Restore(context.Background(), proj, decl, entree); err != nil {
		t.Fatal(err)
	}
	c := appelDe(t, prov, "mysql")
	if c.AsUser != proj.Spec.User.Name || c.AsUser == "" {
		t.Errorf("DÉFAUT : le client tourne sous %q, et « \\! » avec lui — attendu l'utilisateur du projet %q",
			c.AsUser, proj.Spec.User.Name)
	}
	if slices.Contains(c.Args, "--user=root") || !slices.Contains(c.Args, "--user=shopfr") {
		t.Errorf("DÉFAUT : la restauration doit se connecter sous le compte du projet : %q", c.Args)
	}
	if len(c.Args) == 0 || c.Args[0] != "--no-defaults" {
		t.Errorf("le ~/.my.cnf de l'utilisateur ne doit pas être lu : %q", c.Args)
	}
	password, err := prov.Password(proj.ID(), "mysql", "shopfr")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.Env, "MYSQL_PWD="+password) {
		t.Errorf("le mot de passe du compte doit passer par l'environnement : %q", c.Env)
	}
	if strings.Contains(strings.Join(c.Args, " "), password) {
		t.Error("le mot de passe ne doit jamais figurer dans les arguments, lisibles par tous")
	}
	for _, v := range c.Env {
		if !strings.HasPrefix(v, "PATH=") && !strings.HasPrefix(v, "MYSQL_PWD=") {
			t.Errorf("variable transmise à l'utilisateur du projet : %q", v)
		}
	}
}

func TestLaRestaurationPostgreSQLEstConfineeAuRoleDuProjet(t *testing.T) {
	prov, proj := fixture(t)
	prefix := prov.Paths.Prefix()
	for path, content := range map[string]string{
		filepath.Join(prefix, "etc", "postgresql", "16", "main", "postgresql.conf"): "port = 5433\n",
		filepath.Join(prefix, "var", "run", "postgresql", ".s.PGSQL.5433"):          "",
		filepath.Join(prefix, "usr", "bin", "psql"):                                 "",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	password, err := prov.ensurePassword(proj.ID(), "postgres", "shopfr")
	if err != nil {
		t.Fatal(err)
	}
	entree := filepath.Join(t.TempDir(), "shopfr.sql")
	if err := os.WriteFile(entree, []byte("\\! id\nCREATE ROLE pirate SUPERUSER;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	decl := model.Database{Engine: "postgres", Name: "shopfr", User: "shopfr"}
	if err := prov.Restore(context.Background(), proj, decl, entree); err != nil {
		t.Fatal(err)
	}
	c := appelDe(t, prov, "psql")
	if c.AsUser != proj.Spec.User.Name {
		t.Errorf("DÉFAUT : psql tourne sous %q — attendu %q", c.AsUser, proj.Spec.User.Name)
	}
	for _, want := range []string{"--no-psqlrc", "--username=shopfr", "--port=5433", "--dbname=shopfr",
		"--set=ON_ERROR_STOP=1"} {
		if !slices.Contains(c.Args, want) {
			t.Errorf("argument %s absent : %q", want, c.Args)
		}
	}
	if !slices.Contains(c.Env, "PGPASSWORD="+password) {
		t.Errorf("le mot de passe du rôle doit passer par l'environnement : %q", c.Env)
	}
	if !slices.Contains(c.Env, "PGREQUIREAUTH=scram-sha-256") {
		t.Errorf("le mot de passe ne doit être livré qu'à un échange SCRAM : %q", c.Env)
	}
}

func TestLaRestaurationSousUnCompteAdditionnelPasseParUneIdentiteJetable(t *testing.T) {
	// Principal « readonly », compte additionnel « all » pour les migrations : un
	// durcissement raisonnable, que la restauration doit servir. Mais pas sous
	// l'utilisateur du projet, qui lirait dans /proc/<pid>/environ du client le
	// mot de passe du compte « all » : le client est délégué à systemd, sous
	// DynamicUser, le mot de passe dans un fichier que systemd lit en root.
	prov, proj, decl := dumpFixture(t)
	prefix := prov.Paths.Prefix()
	if err := os.WriteFile(filepath.Join(prefix, "usr", "bin", "mysql"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	entree := filepath.Join(t.TempDir(), "shopfr.sql")
	if err := os.WriteFile(entree, []byte(dumpContent), 0o600); err != nil {
		t.Fatal(err)
	}
	decl.Privileges = "readonly"
	decl.ExtraUsers = []model.DBUser{{User: "lecture", Privileges: "readonly"}, {User: "migrations", Privileges: "all"}}
	password, err := prov.ensurePassword(proj.ID(), "mysql", "migrations")
	if err != nil {
		t.Fatal(err)
	}
	// Sans systemd, pas d'identité jetable : refus, qui nomme la sortie.
	if err := os.MkdirAll(filepath.Join(prefix, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prov.Restore(context.Background(), proj, decl, entree); err == nil ||
		!strings.Contains(err.Error(), "privileges: all") {
		t.Errorf("sans systemd, la sortie doit être nommée : %v", err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "run", "systemd", "system"), 0o755); err != nil {
		t.Fatal(err)
	}
	// systemd sans son bus : systemd-run échouerait, la sortie est nommée.
	if err := prov.Restore(context.Background(), proj, decl, entree); err == nil ||
		!strings.Contains(err.Error(), "system_bus_socket") {
		t.Errorf("sans D-Bus, la sortie doit être nommée : %v", err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "run", "dbus"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "run", "dbus", "system_bus_socket"), nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := prov.Restore(context.Background(), proj, decl, entree); err != nil {
		t.Fatal(err)
	}
	calls := prov.Exec.(*exec.Recorder).Calls()
	c := calls[len(calls)-1]
	if c.Name != "systemd-run" || c.AsUser != "" {
		t.Fatalf("DÉFAUT : le client d'un compte additionnel doit être délégué à systemd : %v", c)
	}
	joined := strings.Join(c.Args, " ")
	for _, want := range []string{"--pipe", "--wait", "--property=DynamicUser=yes", "--user=migrations",
		"--property=EnvironmentFile=" + filepath.Join(prefix, "run", "ogs-restauration-")} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s absent : %q", want, c.Args)
		}
	}
	if strings.Contains(joined, password) || len(c.Env) > 0 {
		t.Errorf("DÉFAUT : le mot de passe ne doit figurer ni dans les arguments ni dans l'environnement de "+
			"systemd-run : %q, %q", c.Args, c.Env)
	}
	if string(c.Stdin) != dumpContent {
		t.Errorf("la sauvegarde doit passer par l'entrée : %q", c.Stdin)
	}
	restes, _ := filepath.Glob(filepath.Join(prefix, "run", "ogs-restauration-*"))
	if len(restes) > 0 {
		t.Errorf("fichier d'environnement laissé derrière : %v", restes)
	}
	// Aucun compte « all » : refus, qui nomme les deux sorties.
	decl.ExtraUsers = decl.ExtraUsers[:1]
	if err := prov.Restore(context.Background(), proj, decl, entree); err == nil ||
		!strings.Contains(err.Error(), "ogs db user add shopfr") {
		t.Errorf("sans compte « all », la sortie doit être nommée : %v", err)
	}
}

func TestUneRestaurationDelegueeInterrompueArreteSonUnite(t *testing.T) {
	// Ctrl+C tue systemd-run, pas l'unité transitoire : elle continuait de charger
	// la base après qu'ogs avait rendu la main, en code 1. L'unité est arrêtée
	// avant, et l'annulation rendue.
	prov, proj, decl := dumpFixture(t)
	prefix := prov.Paths.Prefix()
	if err := os.WriteFile(filepath.Join(prefix, "usr", "bin", "mysql"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"systemd/system", "dbus/system_bus_socket"} {
		if err := os.MkdirAll(filepath.Join(prefix, "run", dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	decl.Privileges = "crud"
	decl.ExtraUsers = []model.DBUser{{User: "migr", Privileges: "all"}}
	if _, err := prov.ensurePassword(proj.ID(), "mysql", "migr"); err != nil {
		t.Fatal(err)
	}
	entree := filepath.Join(t.TempDir(), "shopfr.sql")
	if err := os.WriteFile(entree, []byte(dumpContent), 0o600); err != nil {
		t.Fatal(err)
	}
	// Le Ctrl+C arrive pendant que systemd-run tourne.
	ctx, cancel := context.WithCancel(context.Background())
	rec := prov.Exec.(*exec.Recorder)
	prov.Exec = interrompant{Recorder: rec, cancel: cancel}
	err := prov.Restore(ctx, proj, decl, entree)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("l'annulation doit être rendue : %v", err)
	}
	calls := rec.Calls()
	if len(calls) != 2 || calls[0].Name != "systemd-run" {
		t.Fatalf("systemd-run puis l'arrêt de l'unité attendus : %v", calls)
	}
	var unit string
	for _, a := range calls[0].Args {
		if v, ok := strings.CutPrefix(a, "--unit="); ok {
			unit = v
		}
	}
	if stop := calls[1]; unit == "" || stop.Name != "systemctl" || !slices.Equal(stop.Args, []string{"stop", unit}) {
		t.Errorf("DÉFAUT : l'unité %q n'est pas arrêtée : %v", unit, stop)
	}
}

func TestUneUniteDejaCollecteeEstTenuePourArretee(t *testing.T) {
	// Le client a fini au moment de l'interruption : --collect a retiré l'unité,
	// et « systemctl stop » la dit introuvable. Rien n'est plus en cours.
	prov, _ := fixture(t)
	rec := prov.Exec.(*exec.Recorder)
	rec.Errors["systemctl stop"] = errors.New("Failed to stop ogs-restauration-1.service: Unit " +
		"ogs-restauration-1.service not loaded.")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := prov.stopUnit(ctx, "ogs-restauration-1.service")
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "en cours de chargement") {
		t.Errorf("unité introuvable : arrêtée, annulation rendue : %v", err)
	}
	// Un autre échec de l'arrêt, lui, est annoncé.
	rec.Errors["systemctl stop"] = errors.New("Failed to stop: Access denied")
	if err := prov.stopUnit(ctx, "ogs-restauration-1.service"); !errors.Is(err, context.Canceled) ||
		!strings.Contains(err.Error(), "en cours de chargement") {
		t.Errorf("un arrêt en échec doit être annoncé : %v", err)
	}
}

func TestLeFichierDEnvironnementDelegueNePorteQueLeMotDePasse(t *testing.T) {
	prov, _ := fixture(t)
	for _, dir := range []string{"systemd/system", "dbus/system_bus_socket"} {
		if err := os.MkdirAll(filepath.Join(prov.Paths.Prefix(), "run", dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command{Name: "/usr/bin/mysql", Args: []string{"--no-defaults"},
		Env: []string{restorePath, "MYSQL_PWD=abc.DEF+9="}}
	name, unit, err := prov.delegate(&cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(name)
	if !strings.HasPrefix(unit, "ogs-restauration-") || !slices.Contains(cmd.Args, "--unit="+unit) {
		t.Errorf("l'unité doit être nommée, pour pouvoir l'arrêter : %q, %q", unit, cmd.Args)
	}
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "MYSQL_PWD=abc.DEF+9=\n" {
		t.Errorf("contenu : %q", data)
	}
	if info, err := os.Stat(name); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode du fichier : %v, %v", info, err)
	}
	// Un caractère qui changerait la lecture du fichier est refusé.
	bad := exec.Command{Name: "/usr/bin/mysql", Env: []string{"MYSQL_PWD=a b"}}
	if _, _, err := prov.delegate(&bad); err == nil {
		t.Error("un mot de passe à espace doit être refusé")
	}
}

func TestLaRestaurationControleLEcouteAvantDePresenterLeMotDePasse(t *testing.T) {
	// Le contrôle de l'écoute TCP est court-circuité en mode simulé : il est
	// branché ici, pour prouver qu'il garde les deux chemins TCP.
	var vus []string
	listenerCheck = func(port int, user, target string) error {
		vus = append(vus, fmt.Sprintf("%s:%d:%s", user, port, target))
		return fmt.Errorf("%w : écoute étrangère", ErrUnavailable)
	}
	t.Cleanup(func() { listenerCheck = nil })
	prov, proj, decl := dumpFixture(t)
	prefix := prov.Paths.Prefix()
	for path, content := range map[string]string{
		filepath.Join(prefix, "etc", "postgresql", "16", "main", "postgresql.conf"): "port = 5433\n",
		filepath.Join(prefix, "var", "run", "postgresql", ".s.PGSQL.5433"):          "",
		filepath.Join(prefix, "usr", "bin", "psql"):                                 "",
		filepath.Join(prefix, "usr", "bin", "mysql"):                                "",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := prov.ensurePassword(proj.ID(), "postgres", "shopfr"); err != nil {
		t.Fatal(err)
	}
	entree := filepath.Join(t.TempDir(), "shopfr.sql")
	if err := os.WriteFile(entree, []byte(dumpContent), 0o600); err != nil {
		t.Fatal(err)
	}
	pg := model.Database{Engine: "postgres", Name: "shopfr", User: "shopfr"}
	if err := prov.Restore(context.Background(), proj, pg, entree); !errors.Is(err, ErrUnavailable) {
		t.Errorf("DÉFAUT : PostgreSQL restauré malgré une écoute étrangère : %v", err)
	}
	decl.Host = "::1"
	if err := prov.Restore(context.Background(), proj, decl, entree); !errors.Is(err, ErrUnavailable) {
		t.Errorf("DÉFAUT : MySQL par TCP restauré malgré une écoute étrangère : %v", err)
	}
	// Par le socket, MySQL ne passe pas par TCP : aucun contrôle d'écoute.
	decl.Host = "localhost"
	if err := prov.Restore(context.Background(), proj, decl, entree); err != nil {
		t.Errorf("MySQL par le socket : %v", err)
	}
	if want := []string{"postgres:5433:127.0.0.1", "mysql:3306:::1"}; !slices.Equal(vus, want) {
		t.Errorf("contrôles : %q, attendu %q", vus, want)
	}
	for _, c := range prov.Exec.(*exec.Recorder).Calls() {
		if filepath.Base(c.Name) == "psql" || slices.Contains(c.Args, "--protocol=TCP") {
			t.Errorf("DÉFAUT : client lancé malgré le refus : %v", c)
		}
	}
}

func TestUnMotDePasseMd5RenvoieALaRotation(t *testing.T) {
	// Cluster mis à niveau, mot de passe haché en md5 : PGREQUIREAUTH fait échouer
	// libpq, et le message doit nommer la sortie.
	prov, proj := fixture(t)
	prefix := prov.Paths.Prefix()
	for path, content := range map[string]string{
		filepath.Join(prefix, "etc", "postgresql", "16", "main", "postgresql.conf"): "port = 5432\n",
		filepath.Join(prefix, "var", "run", "postgresql", ".s.PGSQL.5432"):          "",
		filepath.Join(prefix, "usr", "bin", "psql"):                                 "",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := prov.ensurePassword(proj.ID(), "postgres", "shopfr"); err != nil {
		t.Fatal(err)
	}
	prov.Exec.(*exec.Recorder).Errors[filepath.Join(prefix, "usr", "bin", "psql")+" --no-psqlrc"] = errors.New(
		`psql: error: connection to server at "127.0.0.1", port 5432 failed: authentication method ` +
			`requirement "scram-sha-256" failed: server requested a hashed password`)
	entree := filepath.Join(t.TempDir(), "shopfr.sql")
	if err := os.WriteFile(entree, []byte(dumpContent), 0o600); err != nil {
		t.Fatal(err)
	}
	err := prov.Restore(context.Background(), proj, model.Database{Engine: "postgres", Name: "shopfr", User: "shopfr"}, entree)
	if err == nil || !strings.Contains(err.Error(), "ogs db user rotate shopfr shopfr") || !strings.Contains(err.Error(), "md5") {
		t.Errorf("la sortie doit être nommée : %v", err)
	}
}

func TestLaRestaurationDUneBaseAdopteeNommeLaSortie(t *testing.T) {
	// L'import ne connaît pas le mot de passe d'une base adoptée : la
	// restauration, qui s'exécute sous ce compte, doit dire comment le reprendre.
	prov, proj, decl := dumpFixture(t)
	if err := os.WriteFile(filepath.Join(prov.Paths.Prefix(), "usr", "bin", "mysql"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	entree := filepath.Join(t.TempDir(), "shopfr.sql")
	if err := os.WriteFile(entree, []byte(dumpContent), 0o600); err != nil {
		t.Fatal(err)
	}
	decl.User = "ancien"
	err := prov.Restore(context.Background(), proj, decl, entree)
	if err == nil || !strings.Contains(err.Error(), "ogs db user rotate shopfr ancien") {
		t.Errorf("la sortie doit être nommée : %v", err)
	}
	// Un mot de passe scellé que le moteur ne connaît pas : même consigne.
	rec := prov.Exec.(*exec.Recorder)
	rec.Errors[filepath.Join(prov.Paths.Prefix(), "usr", "bin", "mysql")+" --no-defaults"] =
		errors.New("ERROR 1045 (28000): Access denied for user 'shopfr'@'localhost'")
	decl.User = "shopfr"
	if err := prov.Restore(context.Background(), proj, decl, entree); err == nil ||
		!strings.Contains(err.Error(), "ogs db user rotate shopfr shopfr") {
		t.Errorf("un refus d'authentification doit nommer la sortie : %v", err)
	}
}

func TestLaRestaurationRefuseUnMoteurSansSocketAdmis(t *testing.T) {
	// Moteur arrêté : son port TCP est libre, et un compte local qui s'y lie
	// recevrait le mot de passe scellé puis le script. Sans socket admis, rien
	// n'est présenté (S1R-02 par la restauration).
	prov, proj := fixture(t)
	prefix := prov.Paths.Prefix()
	postgresConf(t, prefix)
	for _, tool := range []string{"psql", "mysql"} {
		path := filepath.Join(prefix, "usr", "bin", tool)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	entree := filepath.Join(t.TempDir(), "shopfr.sql")
	if err := os.WriteFile(entree, []byte(dumpContent), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, engine := range []string{"postgres", "mysql"} {
		if _, err := prov.ensurePassword(proj.ID(), engine, "shopfr"); err != nil {
			t.Fatal(err)
		}
		decl := model.Database{Engine: engine, Name: "shopfr", User: "shopfr", Host: "127.0.0.1", Privileges: "all"}
		if err := prov.Restore(context.Background(), proj, decl, entree); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s sans socket : restauration attendue refusée, obtenu %v", engine, err)
		}
	}
	if calls := prov.Exec.(*exec.Recorder).Calls(); len(calls) > 0 {
		t.Errorf("DÉFAUT : client lancé sans moteur garanti : %v", calls)
	}
}

func TestLaRestaurationMySQLRetireLesDefinisseurs(t *testing.T) {
	// Un clone charge des vues et des déclencheurs qui nomment le compte de la
	// source : poser un autre définisseur exige SET_USER_ID, que le compte du
	// projet n'a pas. Les données restent intactes — y compris au format de
	// mariadb-dump, qui écrit chaque tuple sur sa ligne.
	dump := "/*!50001 CREATE ALGORITHM=UNDEFINED */\n" +
		"/*!50013 DEFINER=`shopfr`@`localhost` SQL SECURITY DEFINER */\n" +
		"/*!50001 VIEW `v` AS select 1 */;\n" +
		"/*!50003 CREATE*/ /*!50017 DEFINER=`shopfr`@`localhost`*/ /*!50003 TRIGGER t BEFORE INSERT ON a FOR EACH ROW SET @x=1 */;;\n" +
		"/*!50106 CREATE*/ /*!50117 DEFINER=`root`@`localhost`*/ /*!50106 EVENT `e` ON SCHEDULE EVERY 1 DAY DO SELECT 1 */ ;;\n" +
		"/*!50003 CREATE*/ /*!50020 DEFINER=`root`@`localhost`*/ /*!50003 PROCEDURE `q`() SELECT 1 */;;\n" +
		"CREATE DEFINER=`root`@`%` PROCEDURE `p`()\n" +
		"INSERT INTO `a` VALUES ('DEFINER=`shopfr`@`localhost`');\n" +
		"INSERT INTO `b` VALUES\n" +
		"(1,'ligne1\\nDEFINER=`root`@`localhost` x'),\n" +
		"(2,'/*!50013 DEFINER=`root`@`localhost` */');\n"
	got := string(confineMySQLDump([]byte(dump)))
	want := "/*!50001 CREATE ALGORITHM=UNDEFINED */\n" +
		"/*!50013  SQL SECURITY DEFINER */\n" +
		"/*!50001 VIEW `v` AS select 1 */;\n" +
		"/*!50003 CREATE*/ /*!50017 */ /*!50003 TRIGGER t BEFORE INSERT ON a FOR EACH ROW SET @x=1 */;;\n" +
		"/*!50106 CREATE*/ /*!50117 */ /*!50106 EVENT `e` ON SCHEDULE EVERY 1 DAY DO SELECT 1 */ ;;\n" +
		"/*!50003 CREATE*/ /*!50020 */ /*!50003 PROCEDURE `q`() SELECT 1 */;;\n" +
		"CREATE  PROCEDURE `p`()\n" +
		"INSERT INTO `a` VALUES ('DEFINER=`shopfr`@`localhost`');\n" +
		"INSERT INTO `b` VALUES\n" +
		"(1,'ligne1\\nDEFINER=`root`@`localhost` x'),\n" +
		"(2,'/*!50013 DEFINER=`root`@`localhost` */');\n"
	if got != want {
		t.Errorf("obtenu :\n%s\nattendu :\n%s", got, want)
	}
}

func TestLaRestaurationPostgreSQLRetireLesCommentairesDExtension(t *testing.T) {
	// COMMENT ON EXTENSION exige d'être propriétaire de l'extension, créée sous
	// le rôle d'administration : sous ON_ERROR_STOP, la restauration s'arrêtait.
	// Seule l'instruction que pg_dump pose sous son en-tête part : la même ligne
	// dans le corps d'une fonction ou dans des données reste.
	dump := "--\n" +
		"-- Name: EXTENSION pg_trgm; Type: COMMENT; Schema: -; Owner: \n" +
		"--\n" +
		"\n" +
		"COMMENT ON EXTENSION pg_trgm IS 'text similarity, l''outil';\n" +
		"\n" +
		"CREATE FUNCTION public.f() RETURNS void\n" +
		"    LANGUAGE plpgsql\n" +
		"    AS $$\n" +
		"COMMENT ON EXTENSION pg_trgm IS 'dans une fonction';\n" +
		"$$;\n" +
		"COPY public.notes (texte) FROM stdin;\n" +
		"COMMENT ON EXTENSION pg_trgm IS 'une donnée';\n" +
		"\\.\n" +
		"COMMENT ON TABLE public.notes IS 'garde';\n"
	got := string(confinePostgresDump([]byte(dump)))
	want := strings.Replace(dump, "COMMENT ON EXTENSION pg_trgm IS 'text similarity, l''outil';\n", "", 1)
	if got != want {
		t.Errorf("obtenu :\n%s\nattendu :\n%s", got, want)
	}
}

func TestLeCloneRestaureUneSauvegardePortableSousLaCible(t *testing.T) {
	// Le clone restaure sous le rôle de la cible : « ALTER … OWNER TO » et les
	// GRANT de la source y échoueraient, ou donneraient à ses comptes des droits
	// sur la copie. Et c'est l'utilisateur de la cible qui lance le client.
	prov, source := fixture(t)
	prefix := prov.Paths.Prefix()
	for path, content := range map[string]string{
		filepath.Join(prefix, "etc", "postgresql", "16", "main", "postgresql.conf"): "port = 5432\n",
		filepath.Join(prefix, "var", "run", "postgresql", ".s.PGSQL.5432"):          "",
		filepath.Join(prefix, "usr", "bin", "psql"):                                 "",
		filepath.Join(prefix, "usr", "bin", "pg_dump"):                              "",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := model.New("shop.fr", naming.EnvPrep, model.KindSymfony)
	target.Spec.App.Version = "6.4"
	if err := target.Normalize(filepath.Join(prefix, "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	from := model.Database{Engine: "postgres", Name: "shopfr", User: "shopfr"}
	to := model.Database{Engine: "postgres", Name: target.ID(), User: target.ID()}
	if _, err := prov.ensurePassword(target.ID(), "postgres", to.User); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(t.TempDir(), "shopfr.sql")
	if err := prov.CloneData(context.Background(), source, from, target, to, tmp); err != nil {
		t.Fatal(err)
	}
	dump := appelDe(t, prov, "pg_dump")
	if !slices.Contains(dump.Args, "--no-owner") || !slices.Contains(dump.Args, "--no-privileges") {
		t.Errorf("DÉFAUT : sauvegarde du clone non portable : %q", dump.Args)
	}
	psql := appelDe(t, prov, "psql")
	if psql.AsUser != target.Spec.User.Name || !slices.Contains(psql.Args, "--username="+to.User) {
		t.Errorf("la copie doit être chargée sous la cible : %q, %q", psql.AsUser, psql.Args)
	}
	// La sauvegarde ordinaire, elle, garde les privilèges des comptes
	// additionnels, mais aucun propriétaire.
	_, args, err := prov.dumpCommand(from, false)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(args, "--no-privileges") || !slices.Contains(args, "--no-owner") {
		t.Errorf("sauvegarde ordinaire : %q", args)
	}
}

// interrompant annule le contexte de l'appelant quand systemd-run est lancé :
// un Ctrl+C reçu pendant la restauration.
type interrompant struct {
	*exec.Recorder
	cancel context.CancelFunc
}

func (i interrompant) Run(ctx context.Context, cmd exec.Command) (exec.Result, error) {
	res, err := i.Recorder.Run(ctx, cmd)
	if cmd.Name == "systemd-run" {
		i.cancel()
		return res, errors.New("signal: killed")
	}
	return res, err
}
