package php

import (
	"os"
	"path/filepath"
	"testing"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/store"
)

// Une base d'état perdue redistribuait les numéros de pool dans l'ordre
// alphabétique : en écoute TCP, aa.fr prenait le port que le pool de bb.fr
// écoute, et le vhost de chacun relayait vers le PHP de l'autre (R2-18).
func TestUneBaseDEtatPerdueRendLesNumerosDePoolEnService(t *testing.T) {
	prov, paths, _ := fixture(t, "8.5")
	a := project(t, "aa.fr", model.KindPHPPlain, "", "8.5")
	b := project(t, "bb.fr", model.KindPHPPlain, "", "8.5")
	a.Spec.PHP.Pools[0].Listen = "tcp"
	b.Spec.PHP.Pools[0].Listen = "tcp"
	for _, proj := range []*model.Project{b, a} {
		plan, err := prov.Build(proj, proj.Spec.PHP.Pools[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(plan.Path, plan.Content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	fresh, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fresh.Close() })
	again := &Provider{Paths: paths, Exec: prov.Exec, Templates: prov.Templates, Store: fresh, Now: prov.Now}
	for _, c := range []struct {
		proj *model.Project
		want string
	}{{a, "127.0.0.1:18502"}, {b, "127.0.0.1:18501"}} {
		plan, err := again.Build(c.proj, c.proj.Spec.PHP.Pools[0])
		if err != nil {
			t.Fatal(err)
		}
		if plan.Listen != c.want {
			t.Errorf("%s : %s après la perte de la base, attendu %s (le pool écrit)", c.proj.ID(), plan.Listen, c.want)
		}
	}
}

// Un pool en écoute TCP ne reçoit jamais un port déjà tenu sur la machine : un
// autre compte y recevrait les requêtes FastCGI du vhost, et le maître FPM, au
// bind refusé, tomberait avec tous les pools de sa version (S2R-02).
func TestUnPoolTCPEviteUnPortDejaEnEcoute(t *testing.T) {
	prov, _, _ := fixture(t, "8.5")
	prov.PortsInUse = func() map[int]bool { return map[int]bool{18501: true} }
	p := project(t, "shop.fr", model.KindPHPPlain, "", "8.5")
	p.Spec.PHP.Pools[0].Listen = "tcp"
	plan, err := prov.Build(p, p.Spec.PHP.Pools[0])
	if err != nil {
		t.Fatal(err)
	}
	if plan.Listen != "127.0.0.1:18502" {
		t.Errorf("écoute %s : 18501 est déjà tenu", plan.Listen)
	}
}
