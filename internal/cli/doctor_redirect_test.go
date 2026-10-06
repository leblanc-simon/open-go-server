package cli

import (
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
)

// doctor nomme une cible de redirection écartée (D119), que le chargement
// n'a pas refusée ; une cible honorée ne lui inspire rien.
func TestDoctorNommeUneCibleDeRedirectionEcartee(t *testing.T) {
	p := model.New("shop.fr", naming.EnvProd, model.KindStatic)
	if err := p.Normalize(t.TempDir(), "preprod-pp.net", model.WithLogsRoot(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	p.Spec.Domains = append(p.Spec.Domains,
		model.Domain{Name: "promo.fr", Role: "redirect", Target: "autre-site.com"})
	var constats []string
	add := func(name, status, detail, fix string) { constats = append(constats, name+" "+status+" "+detail) }
	redirectTargetChecks([]*model.Project{p}, add)
	if len(constats) > 0 {
		t.Fatalf("une cible honorée ne mérite aucun constat : %v", constats)
	}
	p.Spec.Domains = append(p.Spec.Domains,
		model.Domain{Name: "url.fr", Role: "redirect", Target: "https://autre.fr/page"})
	redirectTargetChecks([]*model.Project{p}, add)
	if len(constats) != 1 || !strings.Contains(constats[0], "redirections shopfr avertissement") ||
		!strings.Contains(constats[0], "url.fr") {
		t.Errorf("constat attendu sur url.fr : %v", constats)
	}
}

// Une option HSTS non reconnue d'un vhost déclaré est rappelée par doctor (D119).
func TestDoctorNommeUneOptionHSTSNonReconnue(t *testing.T) {
	p := model.New("shop.fr", naming.EnvProd, model.KindStatic)
	if err := p.Normalize(t.TempDir(), "preprod-pp.net", model.WithLogsRoot(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	p.Spec.Web.VHosts = p.VHosts()
	p.Spec.Web.VHosts[0].Options = map[string]any{"hsts": "off"}
	var constats []string
	redirectTargetChecks([]*model.Project{p}, func(name, status, detail, fix string) {
		constats = append(constats, name+" "+status+" "+detail)
	})
	if len(constats) != 1 || !strings.Contains(constats[0], `"off"`) {
		t.Errorf("constat attendu sur options.hsts : %v", constats)
	}
}
