package unit

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
)

// poserBroker installe une unité rabbitmq-server factice sous le préfixe.
func poserBroker(t *testing.T, prov *Provider) string {
	t.Helper()
	path := filepath.Join(prov.Paths.Rooted("/lib/systemd/system"), BrokerUnit)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func exécuter(t *testing.T, steps []*engine.Step) {
	t.Helper()
	for _, s := range steps {
		if s.Apply == nil {
			continue
		}
		if err := s.Apply(context.Background()); err != nil {
			t.Fatalf("%s : %v", s.Summary, err)
		}
	}
}

// D137 : sans broker sur la machine — absent, ou distant —, rien n'est posé.
func TestSansBrokerLocalAucuneUnitéDeRattachement(t *testing.T) {
	prov, _ := fixture(t)
	steps, err := prov.BrokerHookPlan("/usr/bin/ogs")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Errorf("aucune étape attendue sans broker local : %v", steps)
	}
}

// D137 : le broker installé, l'unité est écrite, rattachée à lui par PartOf et
// WantedBy, ordonnée après lui, et activée ; puis conforme au plan suivant.
func TestLeBrokerLocalRattacheLesConsommateurs(t *testing.T) {
	prov, _ := fixture(t)
	poserBroker(t, prov)
	steps, err := prov.BrokerHookPlan("/usr/bin/ogs")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Action != engine.Create {
		t.Fatalf("une création attendue : %v", steps)
	}
	if !slices.Contains(steps[0].Reload, "systemd:enable:"+BrokerHookName) ||
		slices.ContainsFunc(steps[0].Reload, func(r string) bool { return strings.HasPrefix(r, "systemd:restart-target") }) {
		t.Errorf("l'unité s'active, sans redémarrer aucune cible : %v", steps[0].Reload)
	}
	exécuter(t, steps)
	data, err := os.ReadFile(filepath.Join(prov.Paths.SystemdDir(), BrokerHookName))
	if err != nil {
		t.Fatal(err)
	}
	for _, ligne := range []string{"After=rabbitmq-server.service", "PartOf=rabbitmq-server.service",
		"WantedBy=rabbitmq-server.service", "RemainAfterExit=yes",
		"ExecStart=/usr/bin/ogs amqp start --quiet", "ExecStop=/usr/bin/ogs amqp stop --quiet",
		"SuccessExitStatus=0 6"} {
		if !strings.Contains(string(data), ligne+"\n") {
			t.Errorf("%q absent de l'unité :\n%s", ligne, data)
		}
	}
	steps, err = prov.BrokerHookPlan("/usr/bin/ogs")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Errorf("l'unité écrite est conforme : %v", steps)
	}
}

// D137 : le broker désinstallé, l'unité part avec son lien d'activation ; celle
// d'un administrateur, sans la marque d'ogs, reste.
func TestLeBrokerRetiréEmporteLUnité(t *testing.T) {
	prov, _ := fixture(t)
	broker := poserBroker(t, prov)
	steps, _ := prov.BrokerHookPlan("/usr/bin/ogs")
	exécuter(t, steps)
	path := filepath.Join(prov.Paths.SystemdDir(), BrokerHookName)
	link := filepath.Join(prov.Paths.SystemdDir(), BrokerUnit+".wants", BrokerHookName)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(broker); err != nil {
		t.Fatal(err)
	}
	steps, err := prov.BrokerHookPlan("/usr/bin/ogs")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Action != engine.Delete {
		t.Fatalf("un retrait attendu : %v", steps)
	}
	exécuter(t, steps)
	for _, p := range []string{path, link, filepath.Dir(link)} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s doit partir : %v", p, err)
		}
	}

	if err := os.WriteFile(path, []byte("[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	steps, err = prov.BrokerHookPlan("/usr/bin/ogs")
	if err != nil || len(steps) != 0 {
		t.Errorf("l'unité d'un administrateur n'est jamais retirée : %v, %v", steps, err)
	}
}
