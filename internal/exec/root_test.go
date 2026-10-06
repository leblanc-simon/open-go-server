package exec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Avec les privilèges : le vrai setpriv, sous nobody. Les groupes du compte
// sont posés, le petit-fils meurt avec le groupe.
func TestAsUserRéel(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("changer d'identité exige root")
	}
	// t.TempDir pose son parent en 0700 : nobody n'y entrerait pas.
	dir, err := os.MkdirTemp("", "ogs-asuser-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MYSQL_PWD", "secret-de-root")
	l := &Local{}
	res, err := l.Run(context.Background(), Command{Name: "/bin/sh", AsUser: "nobody",
		Args: []string{"-c", "id -u; cat /proc/self/status | grep ^Groups; env"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Stdout, "65534\n") {
		t.Errorf("identité : %q", res.Stdout)
	}
	if strings.Contains(res.Stdout, "MYSQL_PWD") {
		t.Errorf("l'environnement de root est passé : %q", res.Stdout)
	}
	if strings.Contains(res.Stdout, "Groups:\t0") {
		t.Errorf("les groupes de root sont restés : %q", res.Stdout)
	}
	démarré, orphelin := filepath.Join(dir, "démarré"), filepath.Join(dir, "orphelin")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _ = l.Run(ctx, Command{Name: "/bin/sh", AsUser: "nobody", Args: []string{"-c",
		"touch " + démarré + "; /bin/sh -c 'sleep 1; touch " + orphelin + "' & sleep 30"}})
	if _, err := os.Stat(démarré); err != nil {
		t.Fatalf("la commande n'a pas tourné : %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(orphelin); err == nil {
		t.Error("le petit-fils a survécu à l'annulation")
	}
}

// B1 (D122) : une commande lancée sous l'identité d'un projet n'a pas de
// terminal de contrôle. Restée dans la session d'ogs, elle ouvrait /dev/tty et
// y poussait par ioctl(TIOCSTI) une ligne que le shell root exécutait au retour
// d'ogs. L'essai n'a de sens que si le processus de test a lui-même un terminal
// (docker run -t).
func TestAsUserSansTerminalDeControle(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("changer d'identité exige root")
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		t.Skip("le test n'a pas de terminal de contrôle (docker run -t)")
	}
	tty.Close()
	l := &Local{}
	res, err := l.Run(context.Background(), Command{Name: "/bin/sh", AsUser: "nobody",
		Args: []string{"-c", "if (exec 3<>/dev/tty) 2>/dev/null; then echo TERMINAL; else echo aucun; fi"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "aucun" {
		t.Errorf("DÉFAUT : la commande du projet ouvre le terminal de root : %q", res.Stdout)
	}
}
