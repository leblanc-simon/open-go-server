package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// Une option HSTS non reconnue était une étape NoOp du plan web : ni « plan »
// ni « plan -o json » ne la montraient, Plan.String et le JSON ne retenant que
// les étapes qui changent quelque chose. C'est une note du plan (D118, D119) :
// affichée, portée par le champ notes, sans effet sur le code de retour.
func TestUneOptionHSTSNonReconnueEstUneNoteDuPlan(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.retoucher(t, "shopfr", "spec:\n", "spec:\n  web:\n    vhosts:\n      - name: shopfr\n"+
		"        domains: [shop.fr]\n        options: { hsts: \"off\" }\n")
	h.mustRunPartial("apply", "--yes")
	h.simulateSystemEffects("shopfr", "shop.fr", 2001)
	h.mustRun("apply", "--yes")

	out, code := h.run("plan")
	if code != CodeOK || !strings.Contains(out, `options.hsts "off"`) {
		t.Errorf("le plan doit nommer l'option non reconnue, en code 0 (code %d) :\n%s", code, out)
	}
	out, code = h.run("plan", "-o", "json")
	var env struct {
		Notes []string `json:"notes"`
		Items []struct {
			Resource string `json:"resource"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("sortie JSON illisible : %v\n%s", err, out)
	}
	if code != CodeOK || !strings.Contains(strings.Join(env.Notes, "\n"), `options.hsts "off"`) {
		t.Errorf("la note doit figurer dans le champ notes, en code 0 (code %d) :\n%s", code, out)
	}
	for _, it := range env.Items {
		if strings.HasSuffix(it.Resource, "/hsts") {
			t.Errorf("l'option HSTS n'est pas une étape : %s", it.Resource)
		}
	}
	if out, code := h.run("apply", "--yes"); code != CodeOK || !strings.Contains(out, `options.hsts "off"`) {
		t.Errorf("l'apply doit rendre 0 en affichant la note (code %d) :\n%s", code, out)
	}
	out, _ = h.run("doctor")
	if !strings.Contains(lignesDoctor(out, "hsts shopfr"), `"off"`) {
		t.Errorf("doctor doit toujours nommer l'option :\n%s", out)
	}
}
