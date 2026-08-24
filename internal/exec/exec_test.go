package exec

import (
	"context"
	"os"
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
