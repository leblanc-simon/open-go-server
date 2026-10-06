package cli

import (
	"bytes"
	"testing"

	"leblanc.io/open-go-server/internal/audit"
	"leblanc.io/open-go-server/internal/engine"
)

// Relecture de D112 (mineurs 1 et 2) : une purge d'archives se compare aussi
// par sa quantité, et un ajout refusé au plan du parc n'écarte que lui.
func TestLaReconfirmationDuParcComparLaQuantiteEtNEcarteQueLesAjouts(t *testing.T) {
	montre := &engine.Plan{Project: "*"}
	montre.Add(engine.NewStep("archive/purge", engine.Delete, "purge de 1 archive(s) de plus de 14 jours"))
	montre.Add(engine.NewStep("runtime/tmpfiles", engine.Update, "répertoires d'exécution (1 projet(s))"))

	recalcule := &engine.Plan{Project: "*"}
	recalcule.Add(engine.NewStep("archive/purge", engine.Delete, "purge de 5 archive(s) de plus de 14 jours"))
	recalcule.Add(engine.NewStep("runtime/tmpfiles", engine.Update, "répertoires d'exécution (2 projet(s))"))

	extra := beyond(recalcule, keysOf(montre), retraitDuParc)
	if len(extra) != 1 || extra[0].Resource != "archive/purge" {
		t.Fatalf("DÉFAUT : une purge de 5 archives n'est pas celle d'une archive montrée : %v", extra)
	}
	var out bytes.Buffer
	a := &App{Out: &out, Err: &out, Format: "table", NonInteractive: true, Audit: &audit.Logger{}}
	reste, écarts := a.settleExtras(recalcule, extra, false)
	if écarts != 1 || len(reste.Mutating()) != 1 || reste.Mutating()[0].Resource != "runtime/tmpfiles" {
		t.Errorf("DÉFAUT : le refus doit n'écarter que l'ajout, le reste du parc s'applique : %v\n%s",
			reste.Mutating(), out.String())
	}
	// Sous l'interface, l'ajout est écarté sans tout refuser.
	reste, _ = a.settleExtras(recalcule, extra, true)
	if len(reste.Mutating()) != 1 {
		t.Errorf("l'interface doit appliquer le reste du plan du parc : %v", reste.Mutating())
	}
}
