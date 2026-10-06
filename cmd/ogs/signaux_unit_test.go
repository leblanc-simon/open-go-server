package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuffer est un tampon que le gestionnaire de signaux et le test se
// partagent.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func attendre(t *testing.T, cond func() bool, quoi string) {
	t.Helper()
	limite := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(limite) {
			t.Fatalf("délai dépassé : %s", quoi)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// R1-69 : SIGHUP annule le contexte comme SIGTERM, et un second signal est dit,
// non avalé.
func TestSIGHUPAnnuleLeContexteEtLeSuivantEstDit(t *testing.T) {
	var warn syncBuffer
	ctx, stop := watchSignals(context.Background(), &warn)
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP n'a pas annulé le contexte")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	attendre(t, func() bool { return strings.Contains(warn.String(), "SIGINT reçu") },
		"le second signal doit être dit")
}

// SIGPIPE ne tue pas ogs et n'annule rien : une écriture vers une sortie fermée
// échoue en EPIPE.
func TestSIGPIPENAnnulePasEtNeTuePas(t *testing.T) {
	ctx, stop := watchSignals(context.Background(), io.Discard)
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGPIPE); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if ctx.Err() != nil {
		t.Error("SIGPIPE ne doit pas annuler la commande")
	}
}
