package cli

import (
	"os"
	"slices"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/spec"
)

// dkimDomaines relit les domaines signés que porte la déclaration d'un projet.
func (h *harness) dkimDomaines(t *testing.T, id string) []string {
	t.Helper()
	p, err := spec.Load(h.path("etc", "ogs", "projects", id+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return p.DKIMDomains()
}

// R4-13 : « mail dkim enable --domain x » remplaçait la liste des domaines
// signés. Un domaine déjà signé cessait de l'être, et rien ne le disait (D108).
func TestDKIMEnableAjouteSansRetirer(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.Domains = append(p.Spec.Domains,
			model.Domain{Name: "shop.com", Role: "alias"}, model.Domain{Name: "shop.de", Role: "alias"})
	})

	// Depuis le défaut — les domaines primaires —, un domaine ajouté ne retire
	// pas le primaire.
	h.mustRun("mail", "dkim", "enable", "shopfr")
	out := h.mustRun("mail", "dkim", "enable", "shopfr", "--domain", "shop.com")
	if got := h.dkimDomaines(t, "shopfr"); !slices.Equal(got, []string{"shop.fr", "shop.com"}) {
		t.Errorf("shop.fr et shop.com attendus, obtenu %v\n%s", got, out)
	}
	h.mustRun("mail", "dkim", "enable", "shopfr", "--domain", "Shop.DE.")
	if got := h.dkimDomaines(t, "shopfr"); !slices.Equal(got, []string{"shop.fr", "shop.com", "shop.de"}) {
		t.Errorf("les trois domaines attendus, obtenu %v", got)
	}
	// Sans --domain, une liste explicite reste ce qu'elle est.
	out = h.mustRun("mail", "dkim", "enable", "shopfr")
	if got := h.dkimDomaines(t, "shopfr"); len(got) != 3 {
		t.Errorf("la liste explicite doit rester entière, obtenu %v\n%s", got, out)
	}
}

// R4-12 : « activate » scellait la bascule avant de recalculer les tables. Un
// recalcul en échec laissait la bascule faite et les tables sur l'ancienne clé,
// et « activate » relancé répondait qu'aucune clé n'était préparée (code 4) :
// rien ne permettait de rejouer la seconde moitié.
func TestDKIMActivateSeRejoue(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("mail", "dkim", "enable", "shopfr")
	h.mustRun("apply", "shopfr", "--yes")
	h.mustRun("mail", "dkim", "rotate", "shopfr")
	signing := h.read(t, "etc/opendkim/ogs/SigningTable")

	// La SigningTable remplacée par un répertoire : le recalcul échoue, --force
	// compris.
	table := h.path("etc", "opendkim", "ogs", "SigningTable")
	if err := os.Remove(table); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(table+"/x", 0o750); err != nil {
		t.Fatal(err)
	}
	out, code := h.run("mail", "dkim", "activate", "shopfr", "--force", "--yes")
	if code == CodeOK || !strings.Contains(out, "mail dkim activate shopfr") {
		t.Errorf("un recalcul en échec ne doit pas rendre 0, et doit dire comment rejouer (code %d) :\n%s", code, out)
	}

	if err := os.RemoveAll(table); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(table, []byte(signing), 0o640); err != nil {
		t.Fatal(err)
	}
	out, code = h.run("mail", "dkim", "activate", "shopfr", "--yes")
	if code != CodeOK {
		t.Fatalf("la seconde moitié de la bascule doit se rejouer (code %d) :\n%s", code, out)
	}
	after := h.read(t, "etc/opendkim/ogs/SigningTable")
	if !strings.Contains(after, "ogs202") || after == signing {
		t.Errorf("les tables doivent désigner la nouvelle clé :\n%s", after)
	}
	// Rejouée, la bascule n'a plus rien à faire : le code 4 redevient la réponse.
	if _, code := h.run("mail", "dkim", "activate", "shopfr", "--yes"); code != CodeConflict {
		t.Errorf("sans clé préparée ni table en retard, code %d attendu, obtenu %d", CodeConflict, code)
	}
}
