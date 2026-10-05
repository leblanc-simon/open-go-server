package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
)

// systemeFactice répond comme un vrai système, sans rien exécuter : Simulated
// rend faux, et chaque commande reçoit la réponse que l'essai lui donne.
type systemeFactice struct {
	appels   []exec.Command
	repondre func(exec.Command) (exec.Result, error)
}

func (s *systemeFactice) Simulated() bool { return false }

func (s *systemeFactice) Run(_ context.Context, c exec.Command) (exec.Result, error) {
	s.appels = append(s.appels, c)
	if s.repondre != nil {
		return s.repondre(c)
	}
	return exec.Result{}, nil
}

func (s *systemeFactice) lignes() []string {
	var out []string
	for _, c := range s.appels {
		out = append(out, c.String())
	}
	return out
}

func planDuGroupe(t *testing.T, prov *Provider, p *model.Project) ([]*engine.Step, error) {
	t.Helper()
	prov.Exec = &systemeFactice{}
	prov.AccountOfUID = func(int) (string, error) { return "", nil }
	obs, err := prov.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return prov.Plan(context.Background(), p, obs)
}

func TestUnGroupePreexistantTenuParDAutresComptesEstRefuse(t *testing.T) {
	// Le groupe existant était repris sans un regard (R2-43) : ses membres
	// recevaient le socket FPM du projet, donc l'exécution sous son identité,
	// puis la suppression retirait le groupe sous eux.
	p, prov, _, ident := fixture(t)
	ident.Groups["shopfr"] = &GroupInfo{Name: "shopfr", GID: 5000, Members: []string{"www-data", "alice"}}
	prov.AccountsOfGID = func(int) ([]string, error) { return nil, nil }
	_, err := planDuGroupe(t, prov, p)
	if err == nil {
		t.Fatal("un groupe dont alice est membre ne doit pas être repris par le compte à créer")
	}
	for _, want := range []string{"groupe shopfr", "alice", "ogs import"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("le refus doit contenir %q : %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "www-data") {
		t.Errorf("www-data, que le projet ajoute lui-même, n'est pas un étranger : %v", err)
	}
}

func TestUnGroupePrimaireDUnAutreCompteEstRefuse(t *testing.T) {
	p, prov, _, ident := fixture(t)
	ident.Groups["shopfr"] = &GroupInfo{Name: "shopfr", GID: 5000}
	prov.AccountsOfGID = func(gid int) ([]string, error) {
		if gid == 5000 {
			return []string{"bob"}, nil
		}
		return nil, nil
	}
	if _, err := planDuGroupe(t, prov, p); err == nil || !strings.Contains(err.Error(), "bob (groupe primaire)") {
		t.Fatalf("le groupe primaire de bob doit être refusé en le nommant : %v", err)
	}
}

func TestUnGroupeRepriseLegitimeNEstPasRefuse(t *testing.T) {
	// Le refus ne doit bloquer ni ce que laisse un compte supprimé à la main —
	// un groupe dont www-data seul est membre —, ni un projet adopté, ni un
	// compte déjà là, ni un projet que la base d'état connaît.
	cas := []struct {
		nom     string
		membres []string
		prépare func(*model.Project, *Provider, *FakeIdentity)
	}{
		{"www-data seul", []string{"www-data"}, nil},
		{"aucun membre", nil, nil},
		{"adopté", []string{"alice"}, func(p *model.Project, _ *Provider, _ *FakeIdentity) {
			p.Metadata.Labels = map[string]string{"adopted": "true"}
		}},
		{"compte déjà là", []string{"alice"}, func(p *model.Project, _ *Provider, id *FakeIdentity) {
			id.Users["shopfr"] = &UserInfo{Name: "shopfr", UID: 5000, GID: 5000, Home: p.Spec.User.Home,
				Shell: p.Spec.User.Shell, Managed: true}
		}},
		{"connu de la base d'état", []string{"alice"}, func(_ *model.Project, prov *Provider, _ *FakeIdentity) {
			prov.KnownHome = func(*model.Project) bool { return true }
		}},
	}
	for _, c := range cas {
		p, prov, _, ident := fixture(t)
		ident.Groups["shopfr"] = &GroupInfo{Name: "shopfr", GID: 5000, Members: c.membres}
		prov.AccountsOfGID = func(int) ([]string, error) { return nil, nil }
		if c.prépare != nil {
			c.prépare(p, prov, ident)
		}
		if _, err := planDuGroupe(t, prov, p); err != nil && strings.Contains(err.Error(), "groupe shopfr") {
			t.Errorf("%s : le groupe devait être repris : %v", c.nom, err)
		}
	}
}

func TestLesTachesDuCompteSontRelues(t *testing.T) {
	_, prov, _, _ := fixture(t)
	sys := &systemeFactice{repondre: func(c exec.Command) (exec.Result, error) {
		switch {
		case c.Name == "crontab":
			return exec.Result{Stdout: "*/5 * * * * /home/shop.fr/tache.sh\n"}, nil
		case c.Name == "atq":
			return exec.Result{Stdout: "7\tThu Oct  2 10:00:00 2026 a shopfr\n8\tThu Oct  2 11:00:00 2026 a autre\n" +
				"12\tFri Oct  3 09:00:00 2026 b shopfr\n"}, nil
		case c.Name == "at" && c.Args[1] == "7":
			return exec.Result{Stdout: "#!/bin/sh\necho sept\n"}, nil
		case c.Name == "at" && c.Args[1] == "12":
			return exec.Result{Stdout: "#!/bin/sh\necho douze\n"}, nil
		case c.Name == "atrm", c.Name == "loginctl", c.Name == "systemctl":
			return exec.Result{}, nil
		}
		return exec.Result{}, fmt.Errorf("inattendu : %s", c)
	}}
	prov.Exec = sys
	linger := filepath.Join(prov.Paths.Rooted(lingerDir), "shopfr")
	if err := os.MkdirAll(filepath.Dir(linger), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(linger, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	jobs := prov.ReadUserJobs(context.Background(), "shopfr")
	if string(jobs.Crontab) != "*/5 * * * * /home/shop.fr/tache.sh\n" || jobs.CrontabErr != nil {
		t.Errorf("crontab : %q (%v)", jobs.Crontab, jobs.CrontabErr)
	}
	if got := jobs.AtIDs(); strings.Join(got, ",") != "7,12" || jobs.AtErr != nil {
		t.Errorf("seules les tâches at de shopfr sont retenues : %v (%v)", got, jobs.AtErr)
	}
	if !strings.Contains(string(jobs.AtJobs["12"]), "douze") || !jobs.Linger {
		t.Errorf("script ou linger manquant : %+v", jobs)
	}

	// Le retrait suit la relecture : crontab, at, linger, puis le gestionnaire
	// de l'utilisateur, dont les processus tiendraient le compte.
	// La crontab est relue juste avant son retrait, et atq aussi : seul ce que
	// l'archive porte part.
	sys.appels = nil
	removed, err := prov.RemoveUserJobs(context.Background(), jobs, 5000)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"crontab -l -u shopfr", "crontab -r -u shopfr", "atq", "atrm 7 12",
		"loginctl disable-linger shopfr", "systemctl is-active --quiet user@5000.service",
		"systemctl stop user@5000.service"}
	if strings.Join(sys.lignes(), "|") != strings.Join(want, "|") {
		t.Errorf("commandes : %v, attendu %v", sys.lignes(), want)
	}
	if len(removed) != 3 {
		t.Errorf("trois retraits nommés attendus : %v", removed)
	}
}

func TestDesOutilsAbsentsNeDisentRien(t *testing.T) {
	// at n'est pas installé, cron non plus, et le compte n'a pas de crontab :
	// rien n'est à archiver, et la suppression ne doit pas le dire en échec.
	_, prov, _, _ := fixture(t)
	for _, cas := range []func(exec.Command) (exec.Result, error){
		func(c exec.Command) (exec.Result, error) {
			return exec.Result{}, fmt.Errorf("échec de « %s » : %w", c, osexec.ErrNotFound)
		},
		func(c exec.Command) (exec.Result, error) {
			if c.Name == "crontab" {
				return exec.Result{ExitCode: 1, Stderr: "no crontab for shopfr\n"},
					errors.New("échec de « crontab » : exit status 1 : no crontab for shopfr")
			}
			return exec.Result{}, fmt.Errorf("%w", osexec.ErrNotFound)
		},
	} {
		prov.Exec = &systemeFactice{repondre: cas}
		jobs := prov.ReadUserJobs(context.Background(), "shopfr")
		if !jobs.Empty() {
			t.Errorf("rien n'est attendu : %+v", jobs)
		}
		removed, err := prov.RemoveUserJobs(context.Background(), jobs, 5000)
		if err != nil || len(removed) > 0 {
			t.Errorf("rien à retirer : %v, %v", removed, err)
		}
	}
	// Une relecture en échec n'est ni archivée ni retirée : elle est nommée.
	prov.Exec = &systemeFactice{repondre: func(c exec.Command) (exec.Result, error) {
		if c.Name == "crontab" {
			return exec.Result{ExitCode: 1}, errors.New("permission refusée")
		}
		return exec.Result{}, nil
	}}
	jobs := prov.ReadUserJobs(context.Background(), "shopfr")
	sys := &systemeFactice{}
	prov.Exec = sys
	if _, err := prov.RemoveUserJobs(context.Background(), jobs, 5000); err == nil ||
		!strings.Contains(err.Error(), "non relue") {
		t.Errorf("la crontab illisible doit être nommée : %v", err)
	}
	for _, l := range sys.lignes() {
		if strings.HasPrefix(l, "crontab") || strings.HasPrefix(l, "atrm") {
			t.Errorf("rien ne doit être retiré de ce qui n'a pas été archivé : %v", sys.lignes())
		}
	}
}

func TestLaPurgeRetireLesFichiersDeLUIDSansSuivreDeLien(t *testing.T) {
	// useradd redonne l'UID libéré au compte suivant, qui recevait ce que
	// l'ancien laissait dans /tmp, /var/tmp et /dev/shm (R4-19).
	_, prov, _, _ := fixture(t)
	tmp, shm := t.TempDir(), t.TempDir()
	prov.TempDirs = []string{tmp, shm, filepath.Join(t.TempDir(), "absent")}
	ailleurs := filepath.Join(t.TempDir(), "cible")
	if err := os.WriteFile(ailleurs, []byte("à garder"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"sess_abc", "php/upload/x.tmp", "php/upload/y.tmp"} {
		path := filepath.Join(tmp, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("données"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(ailleurs, filepath.Join(shm, "lien")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(ailleurs), filepath.Join(tmp, "repertoire-lie")); err != nil {
		t.Fatal(err)
	}

	// Un autre UID ne perd rien.
	if removed, err := prov.PurgeUID(context.Background(), os.Getuid()+1); err != nil || len(removed) > 0 {
		t.Fatalf("rien n'est à un autre UID : %v, %v", removed, err)
	}
	removed, err := prov.PurgeUID(context.Background(), os.Getuid())
	if os.Getuid() == 0 {
		t.Skip("l'UID 0 n'est jamais purgé")
	}
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(shm, "lien"), filepath.Join(tmp, "php"), filepath.Join(tmp, "repertoire-lie"),
		filepath.Join(tmp, "sess_abc")}
	for _, w := range want {
		found := false
		for _, r := range removed {
			found = found || r == w
		}
		if !found {
			t.Errorf("%s doit être nommé parmi les retraits : %v", w, removed)
		}
	}
	if len(removed) != len(want) {
		t.Errorf("un répertoire retiré en entier est nommé seul : %v", removed)
	}
	entries, _ := os.ReadDir(tmp)
	if len(entries) > 0 {
		t.Errorf("%s doit être vide : %v", tmp, entries)
	}
	if data, err := os.ReadFile(ailleurs); err != nil || string(data) != "à garder" {
		t.Errorf("la cible d'un lien ne doit jamais être touchée : %q, %v", data, err)
	}
}

func TestLeLingerSeRetireParSonFichierQuandLogindEstInjoignable(t *testing.T) {
	// Sans D-Bus, loginctl ne joint pas logind : le linger n'est que son fichier.
	_, prov, _, _ := fixture(t)
	linger := filepath.Join(prov.Paths.Rooted(lingerDir), "shopfr")
	if err := os.MkdirAll(filepath.Dir(linger), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(linger, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	prov.Exec = &systemeFactice{repondre: func(c exec.Command) (exec.Result, error) {
		if c.Name == "loginctl" {
			return exec.Result{ExitCode: 1}, errors.New("Failed to connect to bus")
		}
		return exec.Result{}, nil
	}}
	removed, err := prov.RemoveUserJobs(context.Background(), UserJobs{Account: "shopfr", Linger: true}, 5000)
	if err != nil || len(removed) != 1 || !strings.Contains(removed[0], "linger de shopfr") {
		t.Fatalf("le linger doit être retiré par son fichier : %v, %v", removed, err)
	}
	if _, err := os.Lstat(linger); !os.IsNotExist(err) {
		t.Errorf("%s doit être retiré : %v", linger, err)
	}
}

// poserTable écrit une table des montages factice qui porte les points donnés.
func poserTable(t *testing.T, points ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n")
	for i, p := range points {
		fmt.Fprintf(&b, "%d 22 8:1 /srv/source %s rw,relatime shared:1 - ext4 /dev/sda1 rw\n",
			100+i, strings.ReplaceAll(p, " ", `\040`))
	}
	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	old := MountInfoPath
	MountInfoPath = path
	t.Cleanup(func() { MountInfoPath = old })
}

func TestLaPurgeNeFranchitPasUnMontageLieDuMemeSystemeDeFichiers(t *testing.T) {
	// Un montage lié issu du même système de fichiers garde le st_dev de sa
	// source : seule la table des montages le révèle. La purge le traversait et
	// effaçait la source (relecture de D107, B2). La table factice en tient lieu,
	// sans privilège : le répertoire « monté » n'est qu'un répertoire.
	if os.Getuid() == 0 {
		t.Skip("l'UID 0 n'est jamais purgé")
	}
	_, prov, _, _ := fixture(t)
	tmp := t.TempDir()
	prov.TempDirs = []string{tmp}
	monte := filepath.Join(tmp, "cache de l'uid", "monte")
	precieux := filepath.Join(monte, "precieux")
	if err := os.MkdirAll(monte, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(precieux, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "sess"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	poserTable(t, monte)
	removed, err := prov.PurgeUID(context.Background(), os.Getuid())
	if _, e := os.Stat(precieux); e != nil {
		t.Fatalf("la purge a franchi le montage lié et effacé sa source : %v", e)
	}
	if err == nil || !strings.Contains(err.Error(), "point de montage") || !strings.Contains(err.Error(), monte) {
		t.Errorf("le montage doit être nommé : %v", err)
	}
	if len(removed) != 1 || removed[0] != filepath.Join(tmp, "sess") {
		t.Errorf("le reste de l'UID part quand même : %v", removed)
	}
}

func TestLaPurgeRenonceSansTableDesMontages(t *testing.T) {
	_, prov, _, _ := fixture(t)
	tmp := t.TempDir()
	prov.TempDirs = []string{tmp}
	f := filepath.Join(tmp, "sess")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := MountInfoPath
	MountInfoPath = filepath.Join(t.TempDir(), "absente")
	t.Cleanup(func() { MountInfoPath = old })
	if _, err := prov.PurgeUID(context.Background(), os.Getuid()+1000); err == nil {
		t.Fatal("une table illisible doit faire renoncer à la purge, en le disant")
	}
	if _, err := os.Stat(f); err != nil {
		t.Errorf("rien ne doit être retiré : %v", err)
	}
}

func TestLaPurgeEstBornee(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("l'UID 0 n'est jamais purgé")
	}
	_, prov, _, _ := fixture(t)
	tmp := t.TempDir()
	prov.TempDirs = []string{tmp}
	for i := 0; i < 30; i++ {
		if err := os.WriteFile(filepath.Join(tmp, fmt.Sprintf("f%02d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldMax, oldKept, oldBatch := purgeMaxEntries, purgeKept, purgeBatch
	purgeMaxEntries, purgeKept, purgeBatch = 20, 5, 4
	t.Cleanup(func() { purgeMaxEntries, purgeKept, purgeBatch = oldMax, oldKept, oldBatch })
	removed, err := prov.PurgeUID(context.Background(), os.Getuid())
	if err == nil || !strings.Contains(err.Error(), "20 entrées") {
		t.Errorf("un parcours arrêté sur sa borne doit être nommé : %v", err)
	}
	if len(removed) != 6 || !strings.Contains(removed[5], "et 14 autre(s)") {
		t.Errorf("les retraits au-delà de la borne sont comptés : %v", removed)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 11 {
		t.Errorf("le parcours s'arrête à la borne : %d entrées restent", len(left))
	}
}

func TestCeQuiAChangeDepuisLArchivageResteNomme(t *testing.T) {
	// Le compte existe encore entre l'archive et le retrait : sa crontab modifiée
	// et une tâche at posée entre-temps ne sont pas dans l'archive, et restent.
	_, prov, _, _ := fixture(t)
	sys := &systemeFactice{repondre: func(c exec.Command) (exec.Result, error) {
		switch {
		case c.Name == "crontab" && c.Args[0] == "-l":
			return exec.Result{Stdout: "@reboot /home/shop.fr/nouveau.sh\n"}, nil
		case c.Name == "atq":
			return exec.Result{Stdout: "7\tMon Oct  5 10:00:00 2026 a shopfr\n9\tMon Oct  5 11:00:00 2026 a shopfr\n"}, nil
		case c.Name == "systemctl" && c.Args[0] == "stop":
			return exec.Result{ExitCode: 1}, errors.New("Job for user@5000.service canceled")
		}
		return exec.Result{}, nil
	}}
	prov.Exec = sys
	jobs := UserJobs{Account: "shopfr", Crontab: []byte("*/5 * * * * ancien\n"),
		AtJobs: map[string][]byte{"7": []byte("sept")}}
	removed, err := prov.RemoveUserJobs(context.Background(), jobs, 5000)
	for _, l := range sys.lignes() {
		if strings.HasPrefix(l, "crontab -r") || l == "atrm 7 9" || l == "atrm 9" {
			t.Errorf("seul ce que l'archive porte part : %v", sys.lignes())
		}
	}
	if len(removed) != 1 || !strings.Contains(removed[0], "7") {
		t.Errorf("la tâche 7, archivée, part : %v", removed)
	}
	for _, w := range []string{"modifiée depuis son archivage", "9", "non arrêté"} {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Errorf("« %s » doit être nommé : %v", w, err)
		}
	}
}

func TestUneTacheAtExecuteeEntreTempsNEstPasUnEchec(t *testing.T) {
	// atrm rend « Cannot find jobid » pour une tâche exécutée entre l'archivage
	// et le retrait : elle n'est plus là, ce n'est pas un échec.
	_, prov, _, _ := fixture(t)
	file := "7\tMon Oct  5 10:00:00 2026 a shopfr\n12\tMon Oct  5 11:00:00 2026 a shopfr\n"
	prov.Exec = &systemeFactice{repondre: func(c exec.Command) (exec.Result, error) {
		switch c.Name {
		case "atq":
			return exec.Result{Stdout: file}, nil
		case "atrm":
			file = ""
			return exec.Result{ExitCode: 1}, errors.New("Cannot find jobid 12")
		}
		return exec.Result{ExitCode: 1}, errors.New("inactif")
	}}
	jobs := UserJobs{Account: "shopfr", AtJobs: map[string][]byte{"7": nil, "12": nil}}
	removed, err := prov.RemoveUserJobs(context.Background(), jobs, 5000)
	if err != nil || len(removed) != 1 {
		t.Errorf("une tâche disparue est tolérée : %v, %v", removed, err)
	}
	// Une tâche archivée encore en file, elle, est un échec nommé.
	file = "7\tMon Oct  5 10:00:00 2026 a shopfr\n12\tMon Oct  5 11:00:00 2026 a shopfr\n"
	prov.Exec = &systemeFactice{repondre: func(c exec.Command) (exec.Result, error) {
		switch c.Name {
		case "atq":
			return exec.Result{Stdout: file}, nil
		case "atrm":
			return exec.Result{ExitCode: 1}, errors.New("permission refusée")
		}
		return exec.Result{ExitCode: 1}, errors.New("inactif")
	}}
	if _, err := prov.RemoveUserJobs(context.Background(), jobs, 5000); err == nil ||
		!strings.Contains(err.Error(), "encore en file : 7, 12") {
		t.Errorf("une tâche restée en file est nommée : %v", err)
	}
}

func TestLaPurgeJugeLeMontageSurLeDescripteurOuvert(t *testing.T) {
	// La table est lue une fois : un montage posé ou un parent renommé après n'y
	// figure pas, et un montage lié du même système de fichiers garde son
	// st_dev. Le descripteur ouvert dit sans course à quel montage il appartient
	// (statx) ; l'essai simule un montage sur « monte », absent de la table.
	if os.Getuid() == 0 {
		t.Skip("l'UID 0 n'est jamais purgé")
	}
	_, prov, _, _ := fixture(t)
	tmp := t.TempDir()
	prov.TempDirs = []string{tmp}
	monte := filepath.Join(tmp, "cache", "monte")
	precieux := filepath.Join(monte, "precieux")
	if err := os.MkdirAll(monte, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(precieux, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	poserTable(t)
	var st unix.Stat_t
	if err := unix.Stat(monte, &st); err != nil {
		t.Fatal(err)
	}
	réel := mountOfFD
	if _, _, ok := réel(int(mustOpen(t, tmp).Fd())); !ok {
		t.Log("statx ne donne pas le montage sur ce noyau : seule la simulation est éprouvée")
	}
	mountOfFD = func(fd int) (uint64, bool, bool) {
		var o unix.Stat_t
		if unix.Fstat(fd, &o) == nil && o.Ino == st.Ino && o.Dev == st.Dev {
			return 999, false, true
		}
		return 1, false, true
	}
	t.Cleanup(func() { mountOfFD = réel })
	_, err := prov.PurgeUID(context.Background(), os.Getuid())
	if _, e := os.Stat(precieux); e != nil {
		t.Fatalf("la purge a franchi un montage que la table ne montrait pas : %v", e)
	}
	if err == nil || !strings.Contains(err.Error(), monte+" : point de montage") {
		t.Errorf("le montage doit être nommé : %v", err)
	}
}

func TestMountOfFDSurUnVraiNoyau(t *testing.T) {
	// Sur le noyau de l'essai, un répertoire et son enfant sont du même montage.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "enfant"), 0o700); err != nil {
		t.Fatal(err)
	}
	parent, enfant := mustOpen(t, dir), mustOpen(t, filepath.Join(dir, "enfant"))
	if _, _, ok := mountOfFD(int(parent.Fd())); !ok {
		t.Skip("statx ne donne pas le montage sur ce noyau")
	}
	if crossesMount(int(parent.Fd()), int(enfant.Fd())) {
		t.Error("un sous-répertoire ordinaire n'est pas un point de montage")
	}
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
