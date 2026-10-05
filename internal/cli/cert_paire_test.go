package cli

import (
	"context"
	cryptotls "crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
	tlsprov "leblanc.io/open-go-server/internal/provider/tls"
)

// B1 de la relecture de D110 : une paire clé/certificat dépareillée sur un
// projet à deux vhosts — le site et sa redirection, servis par le même
// certificat. Le plan l'annonçait, mais l'étape web réécrivait d'abord le
// premier vhost en HTTP seul, et « nginx -t » lisait le second, qui désignait
// toujours la paire : « key values mismatch », étape critique, saga déroulée,
// et l'étape TLS, placée après le web, n'était jamais atteinte. La paire se
// rétablit désormais avant tout ce qui écrit la configuration du serveur web.
func TestUnePaireDepareilleeSeRetablitAvantLesVhosts(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.choisirNginx(t)
	for _, d := range []string{"sites-available", "sites-enabled"} {
		if err := os.MkdirAll(h.path("etc", "nginx", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.modifierDomaines(t, "shopfr", func(d []model.Domain) []model.Domain {
		return append(d, model.Domain{Name: "ancien-shop.fr", Role: "redirect", Target: "shop.fr"})
	})
	h.mustRunPartial("apply", "shopfr", "--yes")
	certDir := h.path("var", "lib", "ogs", "acme", "certificates", "shopfr")

	// Une autre clé remplace celle du certificat.
	autre, err := tlsprov.SelfSigned{}.Issue(context.Background(),
		tlsprov.Request{Name: "shopfr", Domains: []string{"shop.fr"}, Challenge: tlsprov.ChallengeSelfSigned})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certDir, tlsprov.FileKey), autre.PrivateKey, 0o600); err != nil {
		t.Fatal(err)
	}

	out := h.mustRunPartial("plan", "shopfr")
	paire := strings.Index(out, "réémis avant les vhosts")
	vhost := strings.Index(out, "vhost nginx")
	fragment := strings.Index(out, "ogs.conf")
	if paire < 0 || vhost < 0 || paire > vhost || (fragment >= 0 && paire > fragment) {
		t.Fatalf("la réémission doit précéder toute écriture de la configuration du serveur web :\n%s", out)
	}
	if out, _ := h.run("cert", "list"); !strings.Contains(out, "DÉPAREILLÉS") {
		t.Errorf("« cert list » doit nommer la paire dépareillée :\n%s", out)
	}
	if out, _ := h.run("status", "shopfr"); !strings.Contains(out, "dépareill") {
		t.Errorf("« status » doit nommer la paire dépareillée :\n%s", out)
	}

	h.mustRunPartial("apply", "shopfr", "--yes")
	full, _ := os.ReadFile(filepath.Join(certDir, tlsprov.FileFullchain))
	key, _ := os.ReadFile(filepath.Join(certDir, tlsprov.FileKey))
	if !pairsForTest(full, key) {
		t.Fatal("l'apply doit avoir rétabli la paire")
	}
	vhostRel, bloc443 := vhostEtBloc443("nginx")
	if !strings.Contains(h.read(t, vhostRel), bloc443) {
		t.Error("le vhost doit servir de nouveau le HTTPS")
	}
}

// Une écriture interrompue se lit comme achevée : les vhosts gardent leur bloc
// 443, et le plan n'annonce aucune réécriture qui les ferait passer en HTTP seul.
func TestUneEcritureInterrompueNeRetireRienAuxVhosts(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRunPartial("apply", "shopfr", "--yes")
	certDir := h.path("var", "lib", "ogs", "acme", "certificates", "shopfr")
	neuf, err := tlsprov.SelfSigned{}.Issue(context.Background(),
		tlsprov.Request{Name: "shopfr", Domains: []string{"shop.fr"}, Challenge: tlsprov.ChallengeSelfSigned})
	if err != nil {
		t.Fatal(err)
	}
	full := append(append([]byte{}, neuf.Certificate...), neuf.Chain...)
	_ = os.WriteFile(filepath.Join(certDir, tlsprov.FileKey), neuf.PrivateKey, 0o600)
	_ = os.WriteFile(filepath.Join(certDir, tlsprov.FileFullchain+".ogs-new"), full, 0o644)

	diff, _ := h.run("diff", "shopfr")
	for _, l := range strings.Split(diff, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "-") && strings.Contains(l, "VirtualHost *:443") {
			t.Errorf("le diff retire le bloc 443 : %q\n%s", l, diff)
		}
	}
	h.mustRunPartial("apply", "shopfr", "--yes")
	got, _ := os.ReadFile(filepath.Join(certDir, tlsprov.FileFullchain))
	if string(got) != string(full) {
		t.Error("l'écriture interrompue doit avoir été achevée")
	}
}

// pairsForTest dit si une clé correspond au premier certificat d'une chaîne.
func pairsForTest(full, key []byte) bool {
	_, err := cryptotls.X509KeyPair(full, key)
	return err == nil
}
