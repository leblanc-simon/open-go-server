package logrotate

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/provider/verify"
)

type logrotateRefusant struct{}

func (logrotateRefusant) Simulated() bool { return false }
func (logrotateRefusant) Run(_ context.Context, c exec.Command) (exec.Result, error) {
	if c.Name == "logrotate" {
		path := c.Args[len(c.Args)-1]
		return exec.Result{ExitCode: 1, Stderr: "error: " + path + ":14 duplicate log entry for /var/log/x.log\n"},
			errors.New("code 1")
	}
	return exec.Result{}, nil
}

// R3-37 : le fichier de rotation n'était éprouvé par personne. Refusé par
// logrotate, il l'est chaque nuit pour tout le parc, et passait pour conforme à
// chaque plan. Il est désormais éprouvé par « logrotate -d », et rendu par
// l'étape même ; l'étape est non critique.
func TestUnFichierDeRotationRefuseEstRendu(t *testing.T) {
	prov, projects := fixture(t)
	ctx := context.Background()
	step, err := prov.Plan(projects, "apache", "apache2.service")
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	path := prov.Paths.LogrotateFile()
	ancien, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	prov.Exec = logrotateRefusant{}
	step, err = prov.Plan(projects, "nginx", "nginx.service")
	if err != nil {
		t.Fatal(err)
	}
	err = step.Apply(ctx)
	if !errors.As(err, new(*engine.ToleratedError)) {
		t.Errorf("le refus ne doit pas arrêter le plan global : %v", err)
	}
	if !verify.IsRefused(err) || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("le fichier doit être refusé en citant logrotate : %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != string(ancien) {
		t.Errorf("le fichier refusé doit être rendu :\n%s", data)
	}

	// Premier fichier refusé : retiré.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	step, err = prov.Plan(projects, "nginx", "nginx.service")
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Apply(ctx); !verify.IsRefused(err) {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("un premier fichier refusé doit être retiré : %v", err)
	}
}
