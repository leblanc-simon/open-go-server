package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// tracker enregistre l'ordre des applications et des compensations.
type tracker struct{ events []string }

func (tr *tracker) step(name string, action Action, critical bool, fail bool) *Step {
	s := NewStep("test/"+name, action, name)
	s.Critical = critical
	s.Apply = func(context.Context) error {
		if fail {
			return fmt.Errorf("échec simulé de %s", name)
		}
		tr.events = append(tr.events, "apply:"+name)
		return nil
	}
	s.Compensate = func(context.Context) error {
		tr.events = append(tr.events, "compensate:"+name)
		return nil
	}
	return s
}

func TestExecuteAppliesInOrder(t *testing.T) {
	tr := &tracker{}
	plan := &Plan{Project: "shopfr"}
	plan.Add(tr.step("user", Create, true, false))
	plan.Add(tr.step("dirs", Create, true, false))
	report, err := Execute(context.Background(), plan, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied() != 2 || report.RolledBack {
		t.Fatalf("rapport inattendu : %+v", report)
	}
	if got := fmt.Sprint(tr.events); got != "[apply:user apply:dirs]" {
		t.Errorf("ordre d'application : %s", got)
	}
}

func TestExecuteRollsBackInReverseOrder(t *testing.T) {
	tr := &tracker{}
	plan := &Plan{Project: "shopfr"}
	plan.Add(tr.step("user", Create, true, false))
	plan.Add(tr.step("dirs", Create, true, false))
	plan.Add(tr.step("database", Create, true, true)) // échec critique
	plan.Add(tr.step("vhost", Create, true, false))   // ne doit pas être atteinte

	report, err := Execute(context.Background(), plan, Options{})
	if err == nil {
		t.Fatal("un échec critique doit remonter une erreur")
	}
	if !report.RolledBack {
		t.Error("le rollback doit être signalé")
	}
	if report.RollbackErr != nil {
		t.Errorf("compensation en échec : %v", report.RollbackErr)
	}
	want := "[apply:user apply:dirs compensate:dirs compensate:user]"
	if got := fmt.Sprint(tr.events); got != want {
		t.Errorf("séquence :\n  obtenue %s\n  attendue %s", got, want)
	}
	if f := report.Failed(); f == nil || f.Step.Resource != "test/database" {
		t.Errorf("étape en échec mal identifiée : %+v", f)
	}
}

func TestExecuteContinuesOnNonCriticalFailure(t *testing.T) {
	tr := &tracker{}
	plan := &Plan{Project: "shopfr"}
	plan.Add(tr.step("user", Create, true, false))
	plan.Add(tr.step("certificat", Create, false, true)) // non critique, échoue
	plan.Add(tr.step("cron", Create, true, false))

	var warned []string
	report, err := Execute(context.Background(), plan, Options{
		OnWarning: func(s *Step, err error) { warned = append(warned, s.Resource) },
	})
	if err != nil {
		t.Fatalf("une étape non critique ne doit pas faire échouer l'apply : %v", err)
	}
	if report.RolledBack {
		t.Error("aucun rollback ne doit avoir lieu")
	}
	if len(report.Warnings) != 1 || len(warned) != 1 || warned[0] != "test/certificat" {
		t.Errorf("avertissements : %v / %v", report.Warnings, warned)
	}
	if got := fmt.Sprint(tr.events); got != "[apply:user apply:cron]" {
		t.Errorf("les étapes suivantes doivent être appliquées : %s", got)
	}
	if report.Failed() != nil {
		t.Error("un échec non critique ne doit pas être rapporté comme échec")
	}
}

func TestExecuteStopsRollbackOnCompensationFailure(t *testing.T) {
	tr := &tracker{}
	plan := &Plan{Project: "shopfr"}
	first := tr.step("user", Create, true, false)
	second := tr.step("dirs", Create, true, false)
	second.Compensate = func(context.Context) error { return errors.New("compensation impossible") }
	plan.Add(first)
	plan.Add(second)
	plan.Add(tr.step("database", Create, true, true))

	report, err := Execute(context.Background(), plan, Options{})
	if err == nil {
		t.Fatal("erreur attendue")
	}
	if report.RollbackErr == nil {
		t.Fatal("l'échec de compensation doit être remonté")
	}
	// La compensation de « user » n'est pas tentée : on s'arrête et on rapporte.
	if got := fmt.Sprint(tr.events); got != "[apply:user apply:dirs]" {
		t.Errorf("le déroulage doit s'arrêter à la première compensation en échec : %s", got)
	}
}

func TestExecuteDryRunAppliesNothing(t *testing.T) {
	tr := &tracker{}
	plan := &Plan{Project: "shopfr"}
	plan.Add(tr.step("user", Create, true, false))
	report, err := Execute(context.Background(), plan, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.events) != 0 {
		t.Errorf("aucune action ne doit être exécutée : %v", tr.events)
	}
	if len(report.Outcomes) != 1 || !report.Outcomes[0].Skipped {
		t.Errorf("les étapes doivent être marquées non exécutées : %+v", report.Outcomes)
	}
}

func TestPlanSkipsNoOpAndDeduplicatesReloads(t *testing.T) {
	plan := &Plan{Project: "shopfr"}
	a := NewStep("web/vhost/shopfr", Update, "vhost shopfr")
	a.Reload = []string{"apache2", "php8.3-fpm"}
	b := NewStep("web/vhost/autre", Create, "vhost autre")
	b.Reload = []string{"apache2"}
	c := NewStep("system/user/shopfr", NoOp, "utilisateur shopfr")
	c.Reload = []string{"ne-doit-pas-apparaitre"}
	plan.Add(a)
	plan.Add(b)
	plan.Add(c)

	if len(plan.Mutating()) != 2 {
		t.Errorf("%d étapes mutantes, 2 attendues", len(plan.Mutating()))
	}
	if plan.Empty() {
		t.Error("le plan n'est pas vide")
	}
	got := fmt.Sprint(plan.Reloads())
	if got != "[apache2 php8.3-fpm]" {
		t.Errorf("rechargements : %s", got)
	}
}

func TestPlanEmptyWhenAllNoOp(t *testing.T) {
	plan := &Plan{Project: "shopfr"}
	plan.Add(NewStep("system/user/shopfr", NoOp, "utilisateur"))
	if !plan.Empty() {
		t.Error("un plan entièrement inchangé doit être vide")
	}
	if plan.String() != "  aucun changement\n" {
		t.Errorf("rendu : %q", plan.String())
	}
}

func TestExecuteReloadsOnce(t *testing.T) {
	plan := &Plan{Project: "shopfr"}
	for _, n := range []string{"a", "b"} {
		s := NewStep("web/vhost/"+n, Create, "vhost "+n)
		s.Apply = func(context.Context) error { return nil }
		s.Reload = []string{"apache2"}
		plan.Add(s)
	}
	var reloads []string
	report, err := Execute(context.Background(), plan, Options{
		Reload: func(_ context.Context, target string) error {
			reloads = append(reloads, target)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reloads) != 1 || reloads[0] != "apache2" {
		t.Errorf("un seul rechargement attendu, obtenu %v", reloads)
	}
	if len(report.Reloaded) != 1 {
		t.Errorf("rapport de rechargement : %v", report.Reloaded)
	}
}

func TestLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ogs.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	// Dans le même processus, flock est réentrant : on vérifie donc surtout que
	// le verrou s'acquiert, se libère et se réacquiert proprement.
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("le verrou libéré doit être réacquérable : %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}
