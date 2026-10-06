package engine

import (
	"context"
	"errors"
	"testing"
)

// Une étape critique dont l'échec est toléré (D117) ne déroule pas le plan :
// elle compte comme un avertissement, et ses intentions ne sont pas retenues.
func TestUnEchecTolereNeDeroulePasLePlan(t *testing.T) {
	compensé := false
	a := NewStep("a", Create, "a")
	a.Apply = func(context.Context) error { return nil }
	a.Compensate = func(context.Context) error { compensé = true; return nil }
	b := NewStep("b", Update, "b")
	b.Apply = func(context.Context) error { return Tolerated(errors.New("refusé")) }
	b.Reload = []string{"r"}
	c := NewStep("c", Create, "c")
	ranC := false
	c.Apply = func(context.Context) error { ranC = true; return nil }
	plan := &Plan{}
	plan.Add(a)
	plan.Add(b)
	plan.Add(c)
	var rechargés []string
	report, err := Execute(context.Background(), plan, Options{Reload: func(_ context.Context, r string) error {
		rechargés = append(rechargés, r)
		return nil
	}})
	if err != nil || compensé || !ranC {
		t.Fatalf("err=%v compensé=%v c=%v", err, compensé, ranC)
	}
	if len(report.Warnings) != 1 || len(rechargés) != 0 {
		t.Errorf("avertissements=%v rechargements=%v", report.Warnings, rechargés)
	}
}
