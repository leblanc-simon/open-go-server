package cli

import (
	"context"
	"io"
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
