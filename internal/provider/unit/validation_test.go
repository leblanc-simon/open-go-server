package unit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/provider/verify"
)

// systemdRefusant refuse toute unité que systemd-analyze verify éprouve, en
// localisant la plainte dans le fichier.
type systemdRefusant struct{ appels []exec.Command }

func (s *systemdRefusant) Simulated() bool { return false }
func (s *systemdRefusant) Run(_ context.Context, c exec.Command) (exec.Result, error) {
	s.appels = append(s.appels, c)
	if c.Name == "systemd-analyze" {
		path := c.Args[len(c.Args)-1]
		return exec.Result{ExitCode: 1, Stderr: path + ":9: Unknown key 'StartLimitBurst' in section [Service], ignoring.\n"},
			errors.New("code 1")
	}
	return exec.Result{}, nil
}

// R3-37 : « le rendu est validé avant activation », promettait « ogs template
// extract » ; aucune unité n'était éprouvée. Un template surchargé qui pose une
// clé que systemd ignore — StartLimitBurst sous [Service] — était écrit, puis
// les services redémarrés sur une définition amputée. L'unité refusée est
// rendue par l'étape même : le moteur ne compense jamais l'étape qui échoue.
func TestUneUniteRefuseeParSystemdEstRendue(t *testing.T) {
	prov, proj := fixture(t)
	path := filepath.Join(prov.Paths.SystemdDir(), "ogs-shopfr-essai@.service")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	ancien := []byte("ancienne unité\n")
	if err := os.WriteFile(path, ancien, 0o644); err != nil {
		t.Fatal(err)
	}
	// Le fichier en place est étranger : --force fait de l'étape une écriture.
	prov.Force = true
	ex := &systemdRefusant{}
	prov.Exec = ex
	step, err := prov.fileStepFor(proj.ID(), "unit/service/essai", path, []byte("[Service]\nStartLimitBurst=5\n"), "essai")
	if err != nil || step == nil {
		t.Fatalf("étape : %v", err)
	}
	err = step.Apply(context.Background())
	if !verify.IsRefused(err) || !strings.Contains(err.Error(), "StartLimitBurst") {
		t.Fatalf("l'unité doit être refusée en citant systemd : %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != string(ancien) {
		t.Errorf("l'unité refusée doit être rendue, contenu = %q", data)
	}
	// Une unité qui existait reste servie : le refus ne déroule pas le projet.
	if !errors.As(err, new(*engine.ToleratedError)) {
		t.Errorf("le refus de la mise à jour d'une unité en place doit être toléré : %v", err)
	}

	// Une unité neuve refusée ne reste pas sur le disque.
	neuve := filepath.Join(prov.Paths.SystemdDir(), "ogs-shopfr-neuve@.service")
	step, err = prov.fileStepFor(proj.ID(), "unit/service/neuve", neuve, []byte("[Service]\n"), "neuve")
	if err != nil {
		t.Fatal(err)
	}
	err = step.Apply(context.Background())
	if !verify.IsRefused(err) || errors.As(err, new(*engine.ToleratedError)) {
		t.Fatalf("une unité neuve refusée doit faire échouer l'étape critique : %v", err)
	}
	if _, err := os.Stat(neuve); !os.IsNotExist(err) {
		t.Errorf("l'unité neuve refusée doit être retirée : %v", err)
	}
}
