package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R1-27 : « cert delete » jugeait l'usage d'un certificat d'après les seules
// déclarations. Un vhost écrit qui le désigne encore — déclaration retirée à la
// main, vhost d'un site resté hors d'ogs, référence d'un projet qui ne charge
// plus — perdait son matériel, et le rechargement suivant arrêtait le serveur,
// le parc entier avec lui (D110).
func TestCertDeleteRefuseUnCertificatQueDesVhostsEcritsDesignent(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "--yes")
	if _, err := os.Stat(h.path("var", "lib", "ogs", "acme", "certificates", "shopfr", "fullchain.pem")); err != nil {
		t.Fatalf("le certificat du projet doit être émis : %v", err)
	}
	// La déclaration disparaît, le vhost reste écrit.
	if err := os.Remove(h.path("etc", "ogs", "projects", "shopfr.yaml")); err != nil {
		t.Fatal(err)
	}
	out, code := h.run("cert", "delete", "shopfr", "--yes")
	if code != CodeConflict {
		t.Fatalf("code %d attendu %d :\n%s", code, CodeConflict, out)
	}
	if !strings.Contains(out, "shopfr.conf") {
		t.Errorf("le refus doit nommer le vhost qui désigne le certificat :\n%s", out)
	}
	if _, err := os.Stat(h.path("var", "lib", "ogs", "acme", "certificates", "shopfr", "fullchain.pem")); err != nil {
		t.Errorf("le certificat désigné par un vhost ne doit pas être retiré : %v", err)
	}
}

// Le répertoire « _shared-x » est celui du partagé « @x » : le désigner sous ce
// nom contournait le contrôle des projets qui l'emploient, et RemoveAll retirait
// le wildcard de toutes les préproductions.
func TestCertDeleteRefuseLeNomDeRepertoireDUnPartage(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--env", "prep", "--kind", "static")
	h.mustRun("apply", "--yes")
	dir := h.path("var", "lib", "ogs", "acme", "certificates", "_shared-wildcard-prep")
	for _, nom := range []string{"_shared-wildcard-prep", "../certificates/_shared-wildcard-prep", ".", ""} {
		out, code := h.run("cert", "delete", nom, "--yes")
		if code != CodeUsage {
			t.Errorf("« cert delete %s » : code %d attendu %d :\n%s", nom, code, CodeUsage, out)
		}
		if _, err := os.Stat(filepath.Join(dir, "fullchain.pem")); err != nil {
			t.Fatalf("« cert delete %s » a retiré le wildcard partagé : %v", nom, err)
		}
	}
}

// Une suppression est sans recours : le certificat d'un projet se réémet, mais
// au prix d'un challenge, et un partagé en DNS-01 au prix des identifiants.
func TestCertDeleteDemandeConfirmation(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	// Un partagé émis que nul vhost n'emploie : rien ne retient sa suppression.
	h.mustRun("cert", "issue", "@wildcard-prep")
	cert := h.path("var", "lib", "ogs", "acme", "certificates", "_shared-wildcard-prep", "fullchain.pem")
	if _, err := os.Stat(cert); err != nil {
		t.Fatalf("le partagé doit être émis : %v", err)
	}
	out, code := h.run("--non-interactive", "cert", "delete", "@wildcard-prep")
	if code != CodeCancelled {
		t.Fatalf("sans --yes en mode non interactif : code %d attendu %d :\n%s", code, CodeCancelled, out)
	}
	if _, err := os.Stat(cert); err != nil {
		t.Fatalf("un refus de confirmation ne doit rien retirer : %v", err)
	}
	h.mustRun("cert", "delete", "@wildcard-prep", "--yes")
	if _, err := os.Stat(cert); !os.IsNotExist(err) {
		t.Errorf("le certificat doit être retiré après confirmation : %v", err)
	}
}
