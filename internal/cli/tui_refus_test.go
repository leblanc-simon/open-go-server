package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// Sous la TUI, une déclaration refusée était écrite sur la sortie pendant que
// Bubble Tea pilotait l'écran : l'affichage en était brouillé (D120, Y11). Le
// backend la rend à l'interface, qui l'affiche, et n'écrit rien lui-même.
func TestLaTUIRecoitLesDeclarationsRefuseesSansRienEcrire(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("project", "create", "blog.fr", "--kind", "static")
	h.modifierDeclaration(t, "blogfr", func(s string) string {
		return strings.Replace(s, "webAccess: group", "webAccess: nimporte", 1)
	})
	déposer(t, h.path("etc", "ogs", "projects", "casse.yaml"), "metadata: [\n")

	app := h.ouvrirApp(t, true)
	var out, errOut bytes.Buffer
	app.Quiet, app.Out, app.Err = false, &out, &errOut
	app.tolerateFleet = true
	b := &tuiBackend{app: app}
	ps, refused, err := b.Projects()
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].ID() != "shopfr" {
		t.Fatalf("le parc lisible doit être rendu : %v", ps)
	}
	if _, err := b.Snapshot(context.Background(), ps[0]); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Errorf("sous la TUI, rien ne doit être écrit hors de l'écran :\nsortie : %q\nerreurs : %q", out.String(), errOut.String())
	}
	joint := strings.Join(refused, "\n")
	for _, nom := range []string{"blogfr", "casse.yaml"} {
		if !strings.Contains(joint, nom) {
			t.Errorf("la déclaration refusée %s doit être rendue à l'interface : %q", nom, refused)
		}
	}
}
