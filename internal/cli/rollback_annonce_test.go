package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/audit"
	"leblanc.io/open-go-server/internal/engine"
)

// D104 : « le système est revenu à son état initial » suivait une compensation
// en échec, ou l'échec d'une étape qui laisse un vhost désactivé — que rien ne
// compense. L'annonce ne dit l'état initial que si rien ne la dément.
func TestLAnnonceDuRollbackNeMentPas(t *testing.T) {
	for _, cas := range []struct {
		nom       string
		échec     error
		compenser error
		attendu   string
	}{
		{"déroulage complet", errors.New("refus"), nil, "revenu à son état initial"},
		{"étape en échec non défaite", engine.Residual(errors.New("vhost hors ligne")), nil,
			"sauf l'étape en échec"},
		{"compensation en échec", errors.New("refus"), errors.New("disque plein"), "rollback interrompu"},
	} {
		t.Run(cas.nom, func(t *testing.T) {
			avant := engine.NewStep("essai/avant", engine.Create, "étape appliquée")
			avant.Apply = func(context.Context) error { return nil }
			avant.Compensate = func(context.Context) error { return cas.compenser }
			échec := engine.NewStep("web/vhost/shopfr", engine.Delete, "vhost obsolète shopfr.conf")
			échec.Apply = func(context.Context) error { return cas.échec }
			plan := &engine.Plan{Project: "shopfr"}
			plan.Add(avant)
			plan.Add(échec)
			report, err := engine.Execute(context.Background(), plan, engine.Options{})
			if err == nil {
				t.Fatal("l'exécution devait échouer")
			}
			var out bytes.Buffer
			a := &App{Out: &out, Err: &out, Format: "table", Audit: &audit.Logger{}}
			a.reportRollback(report)
			if !strings.Contains(out.String(), cas.attendu) {
				t.Errorf("DÉFAUT : annonce attendue %q :\n%s", cas.attendu, out.String())
			}
			if cas.attendu != "revenu à son état initial" && strings.Contains(out.String(), "le système est revenu à son état initial") {
				t.Errorf("DÉFAUT : l'état initial est annoncé à tort :\n%s", out.String())
			}
		})
	}
}
