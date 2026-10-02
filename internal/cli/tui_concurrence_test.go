package cli

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
)

// Les appels de la TUI s'exécutent chacun dans sa goroutine et partagent l'App :
// Apply y détournait Out et posait Yes, qu'une autre action ou une observation
// lisaient en même temps (course relevée par -race), et Yes restait posé après
// lui (Q-101). « go test -race » le verrouille.
func TestLaTUISérialiseSesAppelsSurLApp(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	app := h.ouvrirApp(t, true)
	out := app.Out
	b := &tuiBackend{app: app}
	p, err := app.loadProject("shopfr")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = b.Apply(context.Background(), p)
		}()
		go func() {
			defer wg.Done()
			_, _ = b.Snapshot(context.Background(), p)
		}()
	}
	wg.Wait()
	if app.Yes {
		t.Error("Apply doit rendre Yes tel qu'il l'a trouvé")
	}
	if app.Out != out || app.Out != io.Discard {
		t.Error("Apply doit rendre Out tel qu'il l'a trouvé")
	}
}

// Dans la TUI, une étape non critique en échec s'affichait deux fois — son
// issue « ✗ », puis la ligne « ⚠ » d'OnWarning —, comme en ligne de commande
// avant D101. La règle est la même des deux côtés (annonceÉtape).
func TestLaTUINAnnonceQuUneFoisUneEtapeNonCritique(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	// Le mode préfixé n'a pas de moteur MySQL : l'étape de la base échoue, non
	// critique, à chaque apply.
	h.mustRunPartial("project", "create", "shop.fr", "--db", "mysql")

	app := h.ouvrirApp(t, true)
	b := &tuiBackend{app: app}
	p, err := app.loadProject("shopfr")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := b.Apply(context.Background(), p)
	if !strings.Contains(out, "⚠ ") {
		t.Fatalf("l'apply devait porter un avertissement :\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "✗ ") {
			t.Errorf("une étape non critique ne doit s'annoncer que par « ⚠ » :\n%s", out)
			break
		}
	}
}
