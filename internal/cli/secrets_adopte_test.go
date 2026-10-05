package cli

import (
	"os"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/secret"
	"leblanc.io/open-go-server/internal/store"
)

func TestSecretsShowDUnAdopteNeTouchePasLaValeur(t *testing.T) {
	// Le mot de passe scellé d'un compte adopté peut n'avoir jamais été posé
	// (R2-21, D111) : « secrets show » le dit — sur la sortie d'erreur, la sortie
	// standard ne portant que la valeur, qu'un script capture.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static", "--db", "mysql")
	yaml := h.path("etc", "ogs", "projects", "shopfr.yaml")
	data, err := os.ReadFile(yaml)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "metadata:\n", "metadata:\n  labels:\n    adopted: \"true\"\n", 1))
	if err := os.WriteFile(yaml, data, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(h.path("var", "lib", "ogs", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	ks, err := secret.Open(h.path("etc", "ogs", "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	nonce, ciphertext, err := ks.Seal("shopfr", "mysql/shopfr", []byte("scelle"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSecret("shopfr", "mysql/shopfr", nonce, ciphertext); err != nil {
		t.Fatal(err)
	}
	st.Close()

	out := h.mustRun("secrets", "show", "shopfr", "mysql/shopfr")
	if strings.TrimSpace(out) != "scelle" {
		t.Errorf("la sortie standard ne doit porter que la valeur :\n%q", out)
	}
}
