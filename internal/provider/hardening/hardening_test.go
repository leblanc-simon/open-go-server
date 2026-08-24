package hardening

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/leviathan/ogs/internal/engine"
)

func fixture(t *testing.T, cibles ...Target) (*Provider, string) {
	t.Helper()
	prefix := t.TempDir()
	return &Provider{
		Prefix:  prefix,
		Targets: cibles,
		// Les comptes système n'existent pas en test : la résolution échoue, et le
		// provider doit s'en tenir au mode.
		LookupIDs: func(string, string) (int, int, error) { return -1, -1, os.ErrNotExist },
	}, prefix
}

func creer(t *testing.T, chemin string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(chemin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(chemin, mode); err != nil {
		t.Fatal(err)
	}
}

func appliquer(t *testing.T, p *Provider) []State {
	t.Helper()
	states := p.Observe()
	plan := &engine.Plan{Project: "système"}
	for _, s := range p.Plan(states) {
		plan.Add(s)
	}
	if _, err := engine.Execute(context.Background(), plan, engine.Options{}); err != nil {
		t.Fatalf("application : %v", err)
	}
	return states
}

func TestRestreintUnRepertoireTropOuvert(t *testing.T) {
	p, prefix := fixture(t, Target{Path: "/etc/nginx", DirMode: 0o750})
	creer(t, filepath.Join(prefix, "etc/nginx"), 0o755)

	states := appliquer(t, p)
	if states[0].Verdict != VerdictRestrict {
		t.Errorf("verdict = %s", states[0].Verdict)
	}
	info, err := os.Stat(filepath.Join(prefix, "etc/nginx"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Errorf("mode = %04o, attendu 0750", got)
	}
	// Rejoué, plus rien à faire.
	for _, st := range p.Observe() {
		if st.Verdict != VerdictOK {
			t.Errorf("second passage : %s pour %s", st.Verdict, st.Target.Path)
		}
	}
}

func TestNeDesserreJamais(t *testing.T) {
	// Un administrateur qui a fermé plus que la cible avait une raison : la
	// desserrer serait une régression de sécurité commise par un outil de
	// sécurité.
	p, prefix := fixture(t, Target{Path: "/etc/nginx", DirMode: 0o750})
	creer(t, filepath.Join(prefix, "etc/nginx"), 0o700)

	states := p.Observe()
	if states[0].Verdict != VerdictStricter {
		t.Fatalf("verdict = %s, attendu %s", states[0].Verdict, VerdictStricter)
	}
	appliquer(t, p)
	info, _ := os.Stat(filepath.Join(prefix, "etc/nginx"))
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("mode = %04o : le répertoire a été desserré", got)
	}
}

func TestIgnoreCeQuiNexistePas(t *testing.T) {
	p, _ := fixture(t, Target{Path: "/etc/absent", DirMode: 0o750})
	states := p.Observe()
	if states[0].Verdict != VerdictAbsent {
		t.Errorf("verdict = %s, attendu %s", states[0].Verdict, VerdictAbsent)
	}
	if Restrict(states) != 0 {
		t.Error("un service non installé ne doit rien réclamer")
	}
}

func TestRefuseUnLienSymbolique(t *testing.T) {
	// Suivre un lien reviendrait à modifier une cible inconnue, éventuellement
	// hors d'/etc.
	p, prefix := fixture(t, Target{Path: "/etc/nginx", DirMode: 0o750})
	ailleurs := filepath.Join(prefix, "ailleurs")
	creer(t, ailleurs, 0o777)
	if err := os.MkdirAll(filepath.Join(prefix, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ailleurs, filepath.Join(prefix, "etc/nginx")); err != nil {
		t.Fatal(err)
	}
	states := appliquer(t, p)
	if states[0].Verdict != VerdictRefused {
		t.Errorf("verdict = %s, attendu %s", states[0].Verdict, VerdictRefused)
	}
	info, _ := os.Stat(ailleurs)
	if got := info.Mode().Perm(); got != 0o777 {
		t.Errorf("la cible du lien a été modifiée (%04o)", got)
	}
}

func TestPreserveLeBitDExecutionDesFichiers(t *testing.T) {
	// Les hooks vivent sous /etc/ogs : les priver du bit d'exécution les ferait
	// refuser par ogs lui-même, qui exige un script exécutable.
	p, prefix := fixture(t, Target{
		Path: "/etc/ogs", DirMode: 0o700, FileMode: 0o600, Recursive: true,
	})
	creer(t, filepath.Join(prefix, "etc/ogs/hooks"), 0o700)
	hook := filepath.Join(prefix, "etc/ogs/hooks/post-create")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(prefix, "etc/ogs/config.yaml")
	if err := os.WriteFile(conf, []byte("a: b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	appliquer(t, p)

	info, _ := os.Stat(hook)
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("le hook (%04o) n'est plus exécutable", info.Mode().Perm())
	}
	if info.Mode().Perm()&0o044 != 0 {
		t.Errorf("le hook (%04o) reste lisible au-delà de son propriétaire", info.Mode().Perm())
	}
	info, _ = os.Stat(conf)
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("la configuration = %04o, attendu 0600", got)
	}
}

func TestSkipLaisseTranquille(t *testing.T) {
	p, prefix := fixture(t, Target{Path: "/etc/mysql", DirMode: 0o750})
	p.Skip = []string{"/etc/mysql"}
	creer(t, filepath.Join(prefix, "etc/mysql"), 0o755)

	if states := p.Observe(); len(states) != 0 {
		t.Errorf("un chemin exclu ne doit pas être rapporté : %v", states)
	}
}

func TestLaTableParDefautNeFermePasEtcPHP(t *testing.T) {
	// Le PHP en ligne de commande, exécuté sous l'identité du projet par les
	// crons, lit son php.ini dans /etc/php : le fermer changerait silencieusement
	// le comportement de PHP.
	for _, t2 := range Defaults() {
		if t2.Path != "/etc/php" {
			continue
		}
		if t2.DirMode&0o005 != 0o005 {
			t.Errorf("/etc/php doit rester traversable et lisible (%04o)", t2.DirMode)
		}
		return
	}
	t.Error("/etc/php doit figurer dans la table, ne serait-ce que pour dire qu'on n'y touche pas")
}
