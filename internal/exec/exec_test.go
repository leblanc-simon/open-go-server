package exec

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalRun(t *testing.T) {
	l := &Local{}
	res, err := l.Run(context.Background(), Command{Name: "echo", Args: []string{"bonjour"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "bonjour" {
		t.Errorf("stdout %q", res.Stdout)
	}
	if l.Simulated() {
		t.Error("l'exécuteur local n'est pas simulé")
	}
}

func TestLocalRunFailureCarriesStderr(t *testing.T) {
	l := &Local{}
	_, err := l.Run(context.Background(), Command{Name: "sh", Args: []string{"-c", "echo souci >&2; exit 3"}})
	if err == nil {
		t.Fatal("un code de retour non nul doit produire une erreur")
	}
	if !strings.Contains(err.Error(), "souci") {
		t.Errorf("l'erreur doit reprendre stderr : %v", err)
	}
}

func TestLocalRunUsesStdin(t *testing.T) {
	l := &Local{}
	res, err := l.Run(context.Background(), Command{Name: "cat", Stdin: []byte("entrée")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "entrée" {
		t.Errorf("stdout %q", res.Stdout)
	}
}

func TestRecorder(t *testing.T) {
	r := NewRecorder()
	if !r.Simulated() {
		t.Error("l'enregistreur est simulé")
	}
	r.Responses["id -u"] = Result{Stdout: "1004\n"}
	res, err := r.Run(context.Background(), Command{Name: "id", Args: []string{"-u", "shopfr"}})
	if err != nil || strings.TrimSpace(res.Stdout) != "1004" {
		t.Fatalf("réponse simulée non appliquée : %q, %v", res.Stdout, err)
	}
	_, _ = r.Run(context.Background(), Command{Name: "useradd", Args: []string{"--gid", "shopfr", "shopfr"}})
	if len(r.Calls()) != 2 {
		t.Errorf("%d appels enregistrés, 2 attendus", len(r.Calls()))
	}
	if !r.Contains("useradd") {
		t.Error("Contains devrait retrouver useradd")
	}
}

func TestCommandStringMentionsUser(t *testing.T) {
	c := Command{Name: "npm", Args: []string{"install"}, AsUser: "shopfr"}
	if !strings.Contains(c.String(), "en tant que shopfr") {
		t.Errorf("rendu %q", c.String())
	}
}

func TestLocalRunHonoursContextCancellation(t *testing.T) {
	// Un script lent doit rendre la main à l'annulation. Sans tuer le groupe de
	// processus, tuer le shell laisserait « sleep » en vie et Wait resterait
	// bloqué sur les tuyaux hérités.
	l := &Local{}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := l.Run(ctx, Command{Name: "sh", Args: []string{"-c", "sleep 30"}})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("l'annulation doit produire une erreur")
	}
	if elapsed > 5*time.Second {
		t.Errorf("l'annulation a pris %s : les enfants n'ont pas été tués", elapsed)
	}
}

func TestLocalRunKillsChildProcesses(t *testing.T) {
	// Le petit-fils doit mourir avec le groupe : un processus orphelin
	// continuerait de tourner après l'interruption de l'outil.
	l := &Local{}
	marker := filepath.Join(t.TempDir(), "orphelin")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _ = l.Run(ctx, Command{Name: "sh", Args: []string{"-c",
		"sh -c 'sleep 1; touch " + marker + "' &  sleep 30"}})
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("le processus petit-fils a survécu à l'annulation")
	}
}

// faussesIdentités remplace la résolution des comptes et les outils de
// changement d'identité : setpriv est un script qui note ses arguments puis
// exécute la commande à sa place, comme le vrai — sans changer d'identité, ce
// qu'un test sans privilège ne peut pas.
func faussesIdentités(t *testing.T, outil string) (trace string) {
	t.Helper()
	dir := t.TempDir()
	trace = filepath.Join(dir, "arguments")
	script := filepath.Join(dir, outil)
	corps := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + trace + "\n" +
		"while [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(script, []byte(corps), 0o755); err != nil {
		t.Fatal(err)
	}
	anciensSetpriv, anciensRunuser, ancienLookup := setprivPaths, runuserPaths, lookupAccount
	t.Cleanup(func() { setprivPaths, runuserPaths, lookupAccount = anciensSetpriv, anciensRunuser, ancienLookup })
	setprivPaths, runuserPaths = []string{filepath.Join(dir, "absent")}, []string{filepath.Join(dir, "absent")}
	if outil == "setpriv" {
		setprivPaths = []string{script}
	} else {
		runuserPaths = []string{script}
	}
	lookupAccount = func(name string) (account, error) {
		if name != "shopfr" {
			return account{}, errors.New("inconnu")
		}
		return account{name: "shopfr", uid: "1004", gid: "1005", home: "/home/shop.fr"}, nil
	}
	return trace
}

// R4-27 : sous « su -c », la commande d'un compte vivait dans une nouvelle
// session, hors du groupe de processus que l'annulation tue : une installation
// de runtime ou « ogs cron run » interrompus continuaient sans ogs. setpriv
// exécute la commande à sa place, et le petit-fils meurt avec le groupe.
func TestAsUserMeurtAvecLeGroupe(t *testing.T) {
	faussesIdentités(t, "setpriv")
	dir := t.TempDir()
	démarré, orphelin := filepath.Join(dir, "démarré"), filepath.Join(dir, "orphelin")
	l := &Local{}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := l.Run(ctx, Command{Name: "/bin/sh", AsUser: "shopfr", Args: []string{"-c",
		"touch " + démarré + "; /bin/sh -c 'sleep 1; touch " + orphelin + "' & sleep 30"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("l'annulation doit se reconnaître : %v", err)
	}
	if _, err := os.Stat(démarré); err != nil {
		t.Fatalf("la commande n'a pas tourné : %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(orphelin); err == nil {
		t.Error("le petit-fils a survécu à l'annulation")
	}
}

// R4-27 : setpriv reçoit l'uid, le gid et les groupes du compte, et l'argv tel
// quel — aucun shell ne le redécoupe, ni ne s'impose (D62).
func TestAsUserPasseParSetpriv(t *testing.T) {
	trace := faussesIdentités(t, "setpriv")
	l := &Local{}
	res, err := l.Run(context.Background(), Command{Name: "/bin/sh", AsUser: "shopfr",
		Args: []string{"-c", "/bin/sh -c 'echo imbriqué'"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "imbriqué" {
		t.Errorf("sortie %q", res.Stdout)
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	want := "--reuid=1004\n--regid=1005\n--init-groups\n--\n/bin/sh\n-c\n/bin/sh -c 'echo imbriqué'\n"
	if string(data) != want {
		t.Errorf("arguments de setpriv :\n%s\nattendu :\n%s", data, want)
	}
}

// Sans setpriv, runuser -u, qui n'emploie pas de shell non plus.
func TestAsUserSeReplieSurRunuser(t *testing.T) {
	trace := faussesIdentités(t, "runuser")
	l := &Local{}
	if _, err := l.Run(context.Background(), Command{Name: "/bin/true", AsUser: "shopfr"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(trace)
	if string(data) != "-u\nshopfr\n--\n/bin/true\n" {
		t.Errorf("arguments de runuser : %q", data)
	}
}

// Ni l'un ni l'autre : refus nommé, jamais su.
func TestAsUserSansOutilRefuse(t *testing.T) {
	faussesIdentités(t, "setpriv")
	setprivPaths = []string{"/inexistant/setpriv"}
	l := &Local{}
	_, err := l.Run(context.Background(), Command{Name: "/bin/true", AsUser: "shopfr"})
	if err == nil || !strings.Contains(err.Error(), "util-linux") {
		t.Errorf("refus attendu : %v", err)
	}
	if Missing(err) {
		t.Error("l'absence de l'outil ne doit pas passer pour celle de la commande, que certains appelants taisent")
	}
	if _, err := l.Run(context.Background(), Command{Name: "/bin/true", AsUser: "inconnu"}); err == nil {
		t.Error("un compte introuvable doit être refusé")
	}
}

// R4-27, D92 : l'environnement d'une commande lancée sous un compte est construit
// de toutes pièces. Sous « su » sans « -l », tout celui de root passait — un
// MYSQL_PWD, un PGPASSWORD, SSH_AUTH_SOCK.
func TestAsUserNHeritePasDeRoot(t *testing.T) {
	faussesIdentités(t, "setpriv")
	t.Setenv("MYSQL_PWD", "secret-de-root")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent")
	t.Setenv("LANG", "fr_FR.UTF-8")
	// Le mandataire ne passe pas : ses identifiants seraient lisibles du projet.
	// Seule l'installation d'un runtime le demande (ProxyEnv).
	t.Setenv("https_proxy", "http://id:secret@mandataire:3128")
	t.Setenv("NO_PROXY", "localhost")
	l := &Local{}
	res, err := l.Run(context.Background(), Command{Name: "/bin/sh", AsUser: "shopfr",
		Args: []string{"-c", "env; pwd"}, Env: []string{"PGPASSWORD=voulu", "PATH=/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"MYSQL_PWD", "SSH_AUTH_SOCK", "secret-de-root", "mandataire", "NO_PROXY"} {
		if strings.Contains(res.Stdout, absent) {
			t.Errorf("%s hérité de root :\n%s", absent, res.Stdout)
		}
	}
	for _, présent := range []string{"HOME=/home/shop.fr\n", "USER=shopfr\n", "LOGNAME=shopfr\n",
		"SHELL=/bin/sh\n", "LANG=fr_FR.UTF-8\n",
		"PGPASSWORD=voulu\n", "PATH=/usr/bin:/bin\n"} {
		if !strings.Contains(res.Stdout, présent) {
			t.Errorf("%q absent :\n%s", présent, res.Stdout)
		}
	}
	if strings.Contains(res.Stdout, "PATH="+UserPath) {
		t.Error("le PATH fourni par l'appelant doit l'emporter")
	}
	if !strings.HasSuffix(res.Stdout, "\n/\n") {
		t.Errorf("le répertoire courant doit être « / » à défaut : %q", res.Stdout)
	}
}

func TestShellQuoteProtectsMetacharacters(t *testing.T) {
	got := shellQuote([]string{"/bin/sh", "-c", "echo a > b && rm -rf /"})
	want := `'/bin/sh' '-c' 'echo a > b && rm -rf /'`
	if got != want {
		t.Errorf("shellQuote = %s, attendu %s", got, want)
	}
	// Un guillemet simple dans la valeur ne doit pas rompre la citation.
	if got := shellQuote([]string{"date 'x'"}); got != `'date '\''x'\'''` {
		t.Errorf("citation d'un guillemet simple : %s", got)
	}
}

// Q-66 : une commande dont la sortie est un flux — journalctl -f — l'écrit au
// fil de l'eau dans les destinations fournies, sans l'accumuler dans Result.
func TestLocalRunEcritAuFilDeLEau(t *testing.T) {
	l := &Local{}
	var out, errOut strings.Builder
	res, err := l.Run(context.Background(), Command{Name: "sh",
		Args:   []string{"-c", "echo ligne; echo souci >&2"},
		Stdout: &out, Stderr: &errOut})
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "ligne\n" || errOut.String() != "souci\n" {
		t.Errorf("flux : stdout %q, stderr %q", out.String(), errOut.String())
	}
	if res.Stdout != "" || res.Stderr != "" {
		t.Errorf("un flux ne doit pas être retenu : %+v", res)
	}
	// La sortie arrive avant la fin de la commande, qui ne finit qu'annulée.
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := l.Run(ctx, Command{Name: "sh", Args: []string{"-c", "echo début; sleep 30"}, Stdout: pw})
		pw.Close()
		done <- err
	}()
	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil || line != "début\n" {
		t.Fatalf("première ligne %q (%v)", line, err)
	}
	cancel()
	go io.Copy(io.Discard, pr)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("l'annulation doit se reconnaître : %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("l'annulation n'a pas arrêté la commande")
	}
}

// Q-66 : le Recorder écrit la sortie simulée dans le flux demandé.
func TestRecorderEcritDansLeFlux(t *testing.T) {
	r := NewRecorder()
	r.Responses["journalctl -u"] = Result{Stdout: "ligne simulée\n"}
	var out strings.Builder
	if _, err := r.Run(context.Background(), Command{Name: "journalctl", Args: []string{"-u", "x"}, Stdout: &out}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "ligne simulée\n" {
		t.Errorf("flux simulé : %q", out.String())
	}
}

// Après une annulation, le code de sortie et l'erreur d'origine restent lisibles.
func TestLAnnulationGardeLeCodeDeSortie(t *testing.T) {
	l := &Local{}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res, err := l.Run(ctx, Command{Name: "sh", Args: []string{"-c", "trap 'exit 7' TERM; sleep 30 & wait"}})
	var ee *osexec.ExitError
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &ee) {
		t.Fatalf("les deux causes doivent rester accessibles : %v", err)
	}
	if res.ExitCode != ee.ExitCode() || res.ExitCode == 0 {
		t.Errorf("code de sortie %d, attendu %d", res.ExitCode, ee.ExitCode())
	}
}

// ShellLine cite ce qui le demande, et seulement cela : un nom d'archive ou de
// vhost fabriqué ne doit rien faire exécuter de plus à qui copie le remède.
func TestShellLineCiteLesMetacaracteres(t *testing.T) {
	got := ShellLine("ogs", "db", "restore", "shopfr", "x;rm -rf /", "/var/lib/ogs/a b.tar.zst", "l'x", "")
	want := `ogs db restore shopfr 'x;rm -rf /' '/var/lib/ogs/a b.tar.zst' 'l'\''x' ''`
	if got != want {
		t.Errorf("ShellLine : %s, attendu %s", got, want)
	}
}

// R1-57 : une entrée fournie en flux est lue au fil de l'eau par la commande ;
// le Recorder la consomme et la retient dans Stdin pour les assertions.
func TestEntréeEnFlux(t *testing.T) {
	l := &Local{}
	res, err := l.Run(context.Background(), Command{Name: "wc", Args: []string{"-c"},
		StdinStream: io.LimitReader(zeros{}, 64<<20)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "67108864" {
		t.Errorf("octets lus : %q", res.Stdout)
	}
	r := NewRecorder()
	if _, err := r.Run(context.Background(), Command{Name: "mysql", StdinStream: strings.NewReader("CREATE")}); err != nil {
		t.Fatal(err)
	}
	if c := r.Calls()[0]; string(c.Stdin) != "CREATE" || c.StdinStream != nil {
		t.Errorf("entrée retenue : %+v", c)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// B1 (D122) : sous AsUser, une nouvelle session — pas de terminal de contrôle,
// et le groupe de processus a le pid de la commande, que l'annulation tue — ;
// une commande root garde la session d'ogs.
func TestUneCommandeDeProjetOuvreSaSession(t *testing.T) {
	if a := procAttr(true); !a.Setsid || a.Setpgid {
		t.Errorf("sous AsUser : %+v", a)
	}
	if a := procAttr(false); a.Setsid || !a.Setpgid {
		t.Errorf("commande root : %+v", a)
	}
	faussesIdentités(t, "setpriv")
	l := &Local{}
	res, err := l.Run(context.Background(), Command{Name: "/bin/sh", AsUser: "shopfr",
		Args: []string{"-c", `set -- $(cut -d' ' -f1,6 /proc/$$/stat); [ "$1" = "$2" ] && echo chef`}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "chef" {
		t.Errorf("la commande doit être chef de sa session : %q", res.Stdout)
	}
}

// B1 (D122) : un fichier de root — le terminal de l'exploitant — n'est jamais
// passé tel quel à une commande de projet : elle reçoit un tube.
func TestUneCommandeDeProjetNeRecoitPasLesDescripteursDeRoot(t *testing.T) {
	faussesIdentités(t, "setpriv")
	f, err := os.CreateTemp(t.TempDir(), "sortie")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	l := &Local{}
	if _, err := l.Run(context.Background(), Command{Name: "/bin/sh", AsUser: "shopfr",
		Args: []string{"-c", "readlink /proc/self/fd/1"}, Stdout: f}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(f.Name())
	if !strings.HasPrefix(string(data), "pipe:") {
		t.Errorf("la sortie de la commande doit être un tube : %q", data)
	}
}

// Le mandataire n'est rendu qu'à qui le demande.
func TestProxyEnv(t *testing.T) {
	t.Setenv("https_proxy", "http://id:secret@mandataire:3128")
	t.Setenv("NO_PROXY", "localhost")
	t.Setenv("MYSQL_PWD", "x")
	got := strings.Join(ProxyEnv(), " ")
	if !strings.Contains(got, "https_proxy=http://id:secret@mandataire:3128") ||
		!strings.Contains(got, "NO_PROXY=localhost") || strings.Contains(got, "MYSQL_PWD") {
		t.Errorf("ProxyEnv : %q", got)
	}
}

// Sous AsUser, une entrée qui est un fichier ouvert — un terminal, un tube
// jamais fermé — bloquait Wait au-delà de WaitDelay : elle est refusée.
func TestAsUserRefuseUnFichierEnEntrée(t *testing.T) {
	faussesIdentités(t, "setpriv")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	l := &Local{}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	début := time.Now()
	_, err = l.Run(ctx, Command{Name: "/bin/cat", AsUser: "shopfr", StdinStream: r})
	if err == nil || !strings.Contains(err.Error(), "fichier ouvert en entrée") {
		t.Errorf("refus attendu : %v", err)
	}
	if time.Since(début) > 2*time.Second {
		t.Error("le refus doit précéder toute exécution")
	}
	// Un lecteur qui se termine passe.
	if _, err := l.Run(context.Background(), Command{Name: "/bin/cat", AsUser: "shopfr",
		StdinStream: strings.NewReader("x")}); err != nil {
		t.Errorf("un lecteur ordinaire doit passer : %v", err)
	}
}
