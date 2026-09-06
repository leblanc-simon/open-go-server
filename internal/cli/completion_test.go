package cli

import (
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/naming"
)

func TestCompletionEnvNeProposeQueDesEnvironnementsValides(t *testing.T) {
	// La complétion est là pour éviter la faute de frappe : proposer une valeur
	// que ParseEnv refuse la suggérerait au contraire. « staging » avait survécu
	// à D24 par une recopie à la main.
	vals := envValues()
	if len(vals) == 0 {
		t.Fatal("aucune valeur proposée pour --env")
	}
	for _, v := range vals {
		if _, err := naming.ParseEnv(v); err != nil {
			t.Errorf("la complétion propose %q, que ParseEnv refuse : %v", v, err)
		}
	}
	var vuPrep bool
	for _, v := range vals {
		if v == string(naming.EnvPrep) {
			vuPrep = true
		}
	}
	if !vuPrep {
		t.Errorf("prep est la seule valeur qu'il vaille la peine de proposer : %v", vals)
	}
}

func TestCompletionEnvProposeeParLeShell(t *testing.T) {
	// Le chemin réel : le shell interroge « __complete », que cobra sert depuis
	// la fonction enregistrée pour l'option.
	h := newHarness(t)
	out, code := h.run("__complete", "project", "create", "--env", "")
	if code != CodeOK {
		t.Fatalf("__complete a échoué (code %d) :\n%s", code, out)
	}
	if !strings.Contains(out, string(naming.EnvPrep)) {
		t.Errorf("prep doit être proposé :\n%s", out)
	}
	if strings.Contains(out, "staging") {
		t.Errorf("staging n'est pas un environnement de projet (D24) :\n%s", out)
	}
}

func TestCompletionEnvSuitLaListeDeNaming(t *testing.T) {
	// La complétion ne tient plus sa propre liste : elle lit celle que
	// naming.ParseEnv parcourt pour valider. Les deux ne peuvent donc plus
	// diverger — c'est par une recopie que « staging » avait survécu.
	envs := naming.Envs()
	vals := envValues()
	if len(vals) != len(envs) {
		t.Fatalf("la complétion propose %v là où naming publie %v", vals, envs)
	}
	for i, e := range envs {
		if vals[i] != string(e) {
			t.Errorf("position %d : la complétion propose %q, naming publie %q", i, vals[i], e)
		}
	}
}
