package engine

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestLockIsExclusiveAcrossProcesses éprouve ce que flock garantit réellement :
// l'exclusion entre processus distincts. Dans un même processus, flock est
// réentrant, donc un test intra-processus ne prouverait rien.
func TestLockIsExclusiveAcrossProcesses(t *testing.T) {
	if path := os.Getenv("OGS_LOCK_HELPER"); path != "" {
		// Rôle d'assistant : prendre le verrou, l'annoncer, puis le tenir.
		lock, err := Acquire(path)
		if err != nil {
			fmt.Println("échec:", err)
			os.Exit(3)
		}
		fmt.Println("verrou-pris")
		time.Sleep(10 * time.Second)
		_ = lock.Release()
		return
	}

	path := filepath.Join(t.TempDir(), "ogs.lock")
	helper := osexec.Command(os.Args[0], "-test.run=TestLockIsExclusiveAcrossProcesses", "-test.timeout=30s")
	helper.Env = append(os.Environ(), "OGS_LOCK_HELPER="+path)
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = helper.Process.Kill()
		_, _ = helper.Process.Wait()
	}()

	sc := bufio.NewScanner(stdout)
	if !sc.Scan() || sc.Text() != "verrou-pris" {
		t.Fatalf("l'assistant n'a pas pris le verrou : %q", sc.Text())
	}

	_, err = Acquire(path)
	if err == nil {
		t.Fatal("le verrou est détenu par un autre processus : l'acquisition doit échouer")
	}
	var locked ErrLocked
	if !errors.As(err, &locked) {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if locked.PID == "" {
		t.Error("le message doit nommer le processus détenteur pour être exploitable")
	}
	if locked.Path != path {
		t.Errorf("chemin de verrou %q", locked.Path)
	}
}
