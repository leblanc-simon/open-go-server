package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/spec"
)

// poserHooks dépose le hook de parc « postCertRenew » et un script que le
// projet shopfr déclare pour le même événement.
func (h *harness) poserHooksDeCertificat(t *testing.T) {
	t.Helper()
	dir := h.path("etc", "ogs", "hooks")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, nom := range []string{"post-cert-renew", "shop-cert.sh"} {
		if err := os.WriteFile(filepath.Join(dir, nom), []byte("#!/bin/sh\n"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, nom), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	path := h.path("etc", "ogs", "projects", "shopfr.yaml")
	p, err := spec.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p.Spec.Hooks = map[string]string{"postCertRenew": "shop-cert.sh"}
	if err := spec.Save(path, p); err != nil {
		t.Fatal(err)
	}
}

// avantDans vérifie que a paraît dans out, et avant b.
func avantDans(t *testing.T, out, a, b string) {
	t.Helper()
	ia, ib := strings.Index(out, a), strings.Index(out, b)
	if ia < 0 || ib < 0 || ia > ib {
		t.Errorf("%q doit précéder %q :\n%s", a, b, out)
	}
}

// R3-25 : « cert renew » lançait postCertRenew avant le rechargement du
// serveur web — le hook annonçait un certificat que le serveur ne présentait
// pas encore —, et jamais celui que déclare un projet.
func TestPostCertRenewSuitLeRechargementEtAtteintLeProjet(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "--yes")
	h.poserHooksDeCertificat(t)

	out := h.mustRun("cert", "renew", "shopfr", "--force")
	avantDans(t, out, "↻ rechargement", "↪ hook postCertRenew : "+h.path("etc", "ogs", "hooks", "post-cert-renew"))
	avantDans(t, out, "↻ rechargement", "↪ hook postCertRenew : "+h.path("etc", "ogs", "hooks", "shop-cert.sh"))
}

// R3-25 : un certificat émis par « apply » ou « cert issue » ne déclenchait
// aucun postCertRenew : une supervision qui copie le certificat vers un
// frontal ne l'apprenait qu'au renouvellement suivant, deux mois plus tard.
func TestPostCertRenewSuitApplyEtCertIssue(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.poserHooksDeCertificat(t)

	out := h.mustRun("apply", "--yes")
	avantDans(t, out, "↻ rechargement", "↪ hook postCertRenew : "+h.path("etc", "ogs", "hooks", "post-cert-renew"))
	if !strings.Contains(out, "↪ hook postCertRenew : "+h.path("etc", "ogs", "hooks", "shop-cert.sh")) {
		t.Errorf("apply doit déclencher le hook du projet :\n%s", out)
	}
	// Un apply sans certificat changé ne le déclenche pas.
	if out := h.mustRun("apply", "--yes"); strings.Contains(out, "postCertRenew") {
		t.Errorf("aucun certificat changé, aucun hook :\n%s", out)
	}

	out = h.mustRun("cert", "issue", "shopfr")
	avantDans(t, out, "↻ rechargement", "↪ hook postCertRenew : "+h.path("etc", "ogs", "hooks", "post-cert-renew"))
	if !strings.Contains(out, "shop-cert.sh") {
		t.Errorf("cert issue doit déclencher le hook du projet :\n%s", out)
	}
}

// La création d'un site émet son certificat dans le même plan qu'un apply :
// postCertRenew le suit de même.
func TestPostCertRenewSuitLaCreationDUnSite(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	dir := h.path("etc", "ogs", "hooks")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "post-cert-renew"), []byte("#!/bin/sh\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "post-cert-renew"), 0o750); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	avantDans(t, out, "↻ rechargement", "↪ hook postCertRenew")
}

// Mineur 3 de la relecture de D117 : un postCertRenew en échec se taisait sous
// « cert renew » et « cert issue ». Il compte dans le code 6, que le timer
// tolère.
func TestPostCertRenewEnEchecRendLeCode6(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "--yes")
	dir := h.path("etc", "ogs", "hooks")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Non exécutable : le hook est refusé, donc en échec.
	hook := filepath.Join(dir, "post-cert-renew")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hook, 0o640); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"cert", "renew", "shopfr", "--force"}, {"cert", "issue", "shopfr"}} {
		out, code := h.run(args...)
		if code != CodePartial || !strings.Contains(out, "postCertRenew") {
			t.Errorf("« ogs %s » : code %d attendu %d :\n%s", strings.Join(args, " "), code, CodePartial, out)
		}
	}
}

// Mineur 6 : « apply -f » applique une version que le disque ne porte pas
// encore ; c'est son hook déclaré qui doit être lancé.
func TestPostCertRenewSuitLaVersionAppliqueeParF(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	dir := h.path("etc", "ogs", "hooks")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "version-f.sh"), []byte("#!/bin/sh\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "version-f.sh"), 0o750); err != nil {
		t.Fatal(err)
	}
	p, err := spec.Load(h.path("etc", "ogs", "projects", "shopfr.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p.Spec.Hooks = map[string]string{"postCertRenew": "version-f.sh"}
	autre := filepath.Join(t.TempDir(), "shopfr.yaml")
	if err := spec.Save(autre, p); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("apply", "-f", autre, "--yes")
	if !strings.Contains(out, "version-f.sh") {
		t.Errorf("le hook de la version appliquée par -f doit être lancé :\n%s", out)
	}
}
