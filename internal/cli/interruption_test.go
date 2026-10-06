package cli

import (
	"context"
	"io"
	"os"
	"testing"
	"time"
)

// D122 : une interruption reçue pendant une question y répond « non » sur-le-
// champ. Captés par ogs, SIGINT, SIGTERM et SIGHUP ne coupaient plus la lecture,
// qui attendait Entrée ; la réponse tapée ensuite faisait poursuivre une
// commande annulée.
func TestUneQuestionRendLaMainALInterruption(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ancienne := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = ancienne; _ = w.Close(); _ = r.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := newApp()
	a.Out = io.Discard
	a.ctx = ctx
	for _, question := range []func() error{
		func() error { return a.confirm("appliquer ?") },
		func() error { return a.confirmExact("shopfr", "identifiant") },
	} {
		done := make(chan error, 1)
		go func() { done <- question() }()
		time.Sleep(50 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if ExitCode(err) != CodeCancelled {
				t.Errorf("code %d, attendu 9 : %v", ExitCode(err), err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("la question attend encore une réponse après l'interruption")
		}
	}
	// Une réponse tapée après l'interruption ne fait rien poursuivre.
	_, _ = w.WriteString("o\n")
	if err := a.confirm("appliquer ?"); ExitCode(err) != CodeCancelled {
		t.Errorf("une commande interrompue ne se confirme plus : %v", err)
	}
}
