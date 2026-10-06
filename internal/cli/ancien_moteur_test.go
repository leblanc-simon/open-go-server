package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// R2-27 : à la bascule de web.engine, l'ancien moteur gardait les vhosts d'ogs,
// son vhost de supervision et son complément d'unité ; rien ne le disait, et
// « project delete » n'y retirait plus rien. Arbitrage (D118) : le plan et doctor
// le signalent — le plan d'une ligne d'information, sans effet sur le code de
// retour, doctor d'un avertissement ; ogs ne retire rien et n'arrête pas
// l'ancien moteur.
func TestLaBasculeDuMoteurWebSignaleLaConfigurationRestee(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRunPartial("apply", "--yes")
	h.simulateSystemEffects("shopfr", "shop.fr", 2001)
	ancien := h.path("etc", "apache2", "sites-available", "shopfr.conf")
	if _, err := os.Stat(ancien); err != nil {
		t.Fatalf("le vhost Apache devait être écrit : %v", err)
	}

	h.choisirNginx(t)
	// En mode préfixé, un moteur désigné n'a pas son arborescence créée d'office.
	for _, d := range []string{"sites-available", "sites-enabled"} {
		if err := os.MkdirAll(h.path("etc", "nginx", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	out, _ := h.run("plan")
	if !strings.Contains(out, "restée sous apache") {
		t.Errorf("le plan doit signaler la configuration restée sous l'ancien moteur :\n%s", out)
	}
	// Une bascule voulue n'est pas un écart : l'apply informe sans rendre 6, et
	// le parc revient à 0 (relecture de D118).
	for i := 0; i < 3; i++ {
		out, code := h.run("apply", "--yes")
		if code != CodeOK || !strings.Contains(out, ancien) {
			t.Errorf("apply %d : code 0 et information nommant le vhost resté attendus (code %d) :\n%s", i+1, code, out)
		}
	}
	if out, code := h.run("diff"); code != CodeOK {
		t.Errorf("diff ne doit pas compter la configuration restée comme un écart (code %d) :\n%s", code, out)
	}
	if _, err := os.Stat(ancien); err != nil {
		t.Errorf("le vhost de l'ancien moteur ne doit jamais être retiré : %v", err)
	}
	if !strings.Contains(h.read(t, "etc/nginx/sites-available/shopfr.conf"), "server_name") {
		t.Error("le vhost nginx doit être écrit")
	}
	out, _ = h.run("doctor")
	if l := lignesDoctor(out, "ancien moteur web"); !strings.Contains(l, "shopfr.conf") || !strings.Contains(l, "n'arrête pas apache2") {
		t.Errorf("doctor doit nommer la configuration restée sous l'ancien moteur :\n%s", out)
	}

	// Le complément d'unité d'Apache est nommé aussi.
	complement := h.path("etc", "systemd", "system", "apache2.service.d", "ogs.conf")
	if !strings.Contains(out, complement) {
		t.Errorf("doctor doit nommer le complément d'unité d'Apache :\n%s", out)
	}

	// Retirée à la main, plus rien n'est signalé.
	for _, chemin := range []string{h.path("etc", "apache2"), complement} {
		if err := os.RemoveAll(chemin); err != nil {
			t.Fatal(err)
		}
	}
	out, _ = h.run("plan")
	if strings.Contains(out, "restée sous apache") {
		t.Errorf("plus rien à signaler une fois la configuration retirée :\n%s", out)
	}
}

// Relecture de D118 (v3) : une déclaration étrangère morte d'un domaine du
// projet — chargée après son vhost, ou pas du tout — est une note du plan, que
// « plan -o json » porte dans son champ, et l'apply rend 0.
func TestUneDeclarationMorteEstUneNoteDuPlan(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRunPartial("apply", "--yes")
	h.simulateSystemEffects("shopfr", "shop.fr", 2001)
	h.mustRun("apply", "--yes")
	mort := h.path("etc", "apache2", "sites-enabled", "zz-redirect.conf")
	if err := os.WriteFile(mort, []byte("<VirtualHost *:80>\n  ServerName shop.fr\n</VirtualHost>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("plan", "-o", "json")
	var env struct {
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("sortie JSON illisible : %v\n%s", err, out)
	}
	if len(env.Notes) != 1 || !strings.Contains(env.Notes[0], mort) {
		t.Errorf("la note doit figurer dans le champ notes : %v\n%s", env.Notes, out)
	}
	if out, code := h.run("apply", "--yes"); code != CodeOK || !strings.Contains(out, mort) {
		t.Errorf("l'apply doit rendre 0 en affichant la note (code %d) :\n%s", code, out)
	}
	out, _ = h.run("doctor")
	if l := lignesDoctor(out, "domaines servis shopfr"); !strings.Contains(l, mort) {
		t.Errorf("doctor doit rappeler la déclaration morte :\n%s", out)
	}
}
