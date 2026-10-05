package tls

import (
	"context"
	"errors"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/template"
)

// systemdFactice est un systemd non simulé dont « is-enabled » et « is-active »
// répondent selon l'état voulu.
type systemdFactice struct {
	*exec.Recorder
	activé, actif bool
}

func (s *systemdFactice) Simulated() bool { return false }

func (s *systemdFactice) Run(ctx context.Context, cmd exec.Command) (exec.Result, error) {
	res, err := s.Recorder.Run(ctx, cmd)
	if cmd.Name == "systemctl" && len(cmd.Args) > 0 {
		switch {
		case cmd.Args[0] == "is-enabled" && !s.activé, cmd.Args[0] == "is-active" && !s.actif:
			return exec.Result{ExitCode: 1}, errors.New("non")
		}
	}
	return res, err
}

func intentionsDActivation(steps []*engine.Step) []*engine.Step {
	var out []*engine.Step
	for _, s := range steps {
		if !s.Mutates() {
			continue
		}
		for _, r := range s.Reload {
			if r == "systemd:enable:ogs-renew.timer" {
				out = append(out, s)
			}
		}
	}
	return out
}

// R1-49 : l'activation du timer de renouvellement n'était tentée qu'à
// l'écriture de son unité, et jamais observée : arrêté ou désactivé, il le
// restait sans que rien ne le dise.
func TestTimerDeRenouvellementObservé(t *testing.T) {
	avant := systemdBooted
	systemdBooted = func() bool { return true }
	t.Cleanup(func() { systemdBooted = avant })

	p := newProvider(t)
	tmpl := template.New(p.Paths.TemplatesDir())
	ctx := context.Background()
	steps, err := p.RenewUnitPlan(tmpl, "/usr/bin/ogs", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if err := s.Apply(ctx); err != nil {
			t.Fatal(err)
		}
	}

	sd := &systemdFactice{Recorder: exec.NewRecorder(), activé: true, actif: true}
	p.Exec = sd
	steps, err = p.RenewUnitPlan(tmpl, "/usr/bin/ogs", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := intentionsDActivation(steps); len(got) != 0 {
		t.Fatalf("un timer conforme, activé et actif ne doit rien produire : %v", got)
	}

	// Activé mais arrêté : démarré, l'activation disant ce que l'administrateur
	// veut.
	sd.activé, sd.actif = true, false
	steps, err = p.RenewUnitPlan(tmpl, "/usr/bin/ogs", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := intentionsDActivation(steps); len(got) != 1 || !strings.Contains(got[0].Summary, "arrêté") {
		t.Errorf("un timer arrêté doit être démarré : %v", got)
	}

	// Désactivé : D89 respecte ce choix, apply ne passe pas outre — mais il le
	// dit, à chaque apply, en avertissement.
	sd.activé, sd.actif = false, false
	steps, err = p.RenewUnitPlan(tmpl, "/usr/bin/ogs", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := intentionsDActivation(steps); len(got) != 0 {
		t.Errorf("un timer désactivé n'est pas réactivé : %v", got)
	}
	var signal *engine.Step
	for _, s := range steps {
		if s.Resource == "tls/renew/activation" {
			signal = s
		}
	}
	if signal == nil || signal.Critical || !strings.Contains(signal.Summary, "désactivé") {
		t.Fatalf("un timer désactivé doit être signalé par une étape non bloquante : %+v", signal)
	}
	if err := signal.Apply(ctx); err == nil || !strings.Contains(err.Error(), "systemctl enable --now ogs-renew.timer") {
		t.Errorf("l'avertissement nomme la commande qui le lève : %v", err)
	}

	// Sans systemd en PID 1, « actif » n'a pas de sens : seul l'activation
	// compte, sans quoi le plan oscillerait dans les conteneurs d'essai.
	systemdBooted = func() bool { return false }
	sd.activé, sd.actif = true, false
	steps, err = p.RenewUnitPlan(tmpl, "/usr/bin/ogs", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := intentionsDActivation(steps); len(got) != 0 {
		t.Errorf("sans systemd, un timer activé ne produit rien : %v", got)
	}
}
