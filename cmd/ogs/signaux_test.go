package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// construireOgs compile ogs pour les essais de bout en bout.
// Il l'est une fois pour tous les essais du paquet.
func construireOgs(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("compilation d'ogs")
	}
	compilation.Do(func() {
		dir, err := os.MkdirTemp("", "ogs-signaux-")
		if err != nil {
			compilationErr = err
			return
		}
		binaire = filepath.Join(dir, "ogs")
		out, err := osexec.Command("go", "build", "-o", binaire, ".").CombinedOutput()
		if err != nil {
			compilationErr = fmt.Errorf("%w\n%s", err, out)
		}
	})
	if compilationErr != nil {
		t.Fatalf("compilation : %v", compilationErr)
	}
	return binaire
}

var (
	compilation    sync.Once
	binaire        string
	compilationErr error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if binaire != "" {
		os.RemoveAll(filepath.Dir(binaire))
	}
	os.Exit(code)
}

// parcPréfixé déclare un projet sous un préfixe, prêt à être appliqué.
func parcPréfixé(t *testing.T, bin string) string {
	t.Helper()
	prefix := t.TempDir()
	for _, args := range [][]string{
		{"--prefix", prefix, "init"},
		{"--prefix", prefix, "project", "create", "shop.fr", "--kind", "static"},
	} {
		if out, err := osexec.Command(bin, args...).CombinedOutput(); err != nil {
			t.Fatalf("ogs %s : %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return prefix
}

// lancerÀLaQuestion lance « ogs apply » sans --yes, l'entrée standard ouverte,
// et attend la question de confirmation.
func lancerÀLaQuestion(t *testing.T, cmd *osexec.Cmd) (io.WriteCloser, chan error) {
	t.Helper()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	question := make(chan struct{})
	go func() {
		r := bufio.NewReader(pr)
		var vu bytes.Buffer
		signalée := false
		for {
			b, err := r.ReadByte()
			if err != nil {
				return
			}
			vu.WriteByte(b)
			if !signalée && strings.Contains(vu.String(), "[o/N]") {
				signalée = true
				close(question)
			}
		}
	}()
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
		pw.Close()
	}()
	select {
	case <-question:
	case err := <-done:
		t.Fatalf("ogs est sorti avant la question : %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("la question n'est pas venue")
	}
	return stdin, done
}

// R1-69, de bout en bout : SIGHUP — la session ssh coupée — à la question de
// confirmation. ogs mourait du signal, sans rien dérouler ; il rend le code 9.
func TestOgsSIGHUPRendLeCode9(t *testing.T) {
	bin := construireOgs(t)
	prefix := parcPréfixé(t, bin)
	cmd := osexec.Command(bin, "--prefix", prefix, "apply", "shopfr")
	stdin, done := lancerÀLaQuestion(t, cmd)
	defer stdin.Close()
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var ee *osexec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 9 {
			t.Fatalf("SIGHUP doit rendre le code 9 : %v (état %v)", err, cmd.ProcessState)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("SIGHUP n'a pas interrompu la question")
	}
}

// Un SIGHUP ignoré au lancement — nohup — le reste : ogs ne s'y abonne pas.
func TestOgsSousNohupIgnoreSIGHUP(t *testing.T) {
	bin := construireOgs(t)
	prefix := parcPréfixé(t, bin)
	cmd := osexec.Command("/bin/sh", "-c", `trap "" HUP; exec "$0" "$@"`, bin, "--prefix", prefix, "apply", "shopfr")
	stdin, done := lancerÀLaQuestion(t, cmd)
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("un SIGHUP ignoré au lancement ne doit rien interrompre : %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	stdin.Close()
	select {
	case err := <-done:
		var ee *osexec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 9 {
			t.Fatalf("une entrée fermée vaut refus, code 9 : %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("ogs ne s'est pas arrêté sur l'entrée fermée")
	}
}

// Une sortie fermée — la session ssh sans terminal qui disparaît — ne tue plus
// ogs par SIGPIPE : il va au bout, et rend son propre code.
func TestOgsSortieFerméeNeLeTuePas(t *testing.T) {
	bin := construireOgs(t)
	prefix := parcPréfixé(t, bin)
	cmd := osexec.Command(bin, "--prefix", prefix, "plan")
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pr.Close()
	cmd.Stdout, cmd.Stderr = pw, pw
	err = cmd.Run()
	pw.Close()
	if st, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && st.Signaled() {
		t.Fatalf("ogs tué par %v sur une sortie fermée", st.Signal())
	}
	_ = err
}
