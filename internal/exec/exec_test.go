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

func TestAsUserPreservesQuotedArguments(t *testing.T) {
	// su attend un script, non un argv : recoller les arguments avec de simples
	// espaces ferait redécouper par le shell tout argument qui en contient. Le cas
	// réel est « ogs cron run », qui exécute « sh -c "flock … sh -c 'date …'" ».
	if os.Getuid() != 0 {
		t.Skip("su exige root")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	cible := filepath.Join(dir, "trace")
	l := &Local{}
	_, err := l.Run(context.Background(), Command{
		Name:   "/bin/sh",
		Args:   []string{"-c", "/bin/sh -c 'echo imbriqué > " + cible + "'"},
		AsUser: "nobody",
	})
	if err != nil {
		t.Fatalf("exécution : %v", err)
	}
	data, err := os.ReadFile(cible)
	if err != nil {
		t.Fatalf("la commande imbriquée n'a rien écrit : %v", err)
	}
	if strings.TrimSpace(string(data)) != "imbriqué" {
		t.Errorf("contenu = %q", data)
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
