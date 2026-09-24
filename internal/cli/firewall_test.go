package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ufwInstalle pose sous le préfixe ce qu'un ufw installé laisse sur le disque.
func (h *harness) ufwInstalle(units ...string) {
	h.t.Helper()
	files := map[string]string{
		"etc/ufw/applications.d/openssh-server": "[OpenSSH]\ntitle=x\ndescription=x\nports=22/tcp\n",
		"etc/ufw/ufw.conf":                      "ENABLED=no\nLOGLEVEL=low\n",
		"etc/default/ufw":                       "IPV6=yes\nDEFAULT_INPUT_POLICY=\"DROP\"\nDEFAULT_OUTPUT_POLICY=\"ACCEPT\"\n",
	}
	for _, u := range units {
		files["lib/systemd/system/"+u+".service"] = ""
	}
	for rel, content := range files {
		if err := os.MkdirAll(filepath.Dir(h.path(rel)), 0o755); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(h.path(rel), []byte(content), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) ecrireUFW(content string) {
	h.t.Helper()
	if err := os.WriteFile(h.path("etc/ogs/ufw.yaml"), []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

const ufwEssai = `# pare-feu d'essai
sources:
  bureau:
    owner: "Agence de Nantes"
    addresses: [203.0.113.10]
trusted: [bureau]
profiles:
  web:
    allow: [any]
# bannissements tenus à la main
ban:
  - 192.0.2.66   # scan du 12/09
`

func TestPareFeuSansDeclarationNeTouchePasUFW(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh")
	if _, code := h.run("firewall", "plan"); code != CodeDependency {
		t.Errorf("sans ufw.yaml, « firewall plan » doit rendre %d, obtenu %d", CodeDependency, code)
	}
	if out := h.mustRun("plan"); strings.Contains(out, "ufw") {
		t.Errorf("sans ufw.yaml, le plan global ne doit pas parler d'ufw :\n%s", out)
	}
}

func TestPareFeuDeBoutEnBout(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh", "mariadb")
	h.ecrireUFW(ufwEssai)

	out := h.mustRun("firewall", "profiles")
	for _, want := range []string{"ssh", "mysql", "203.0.113.10"} {
		if !strings.Contains(out, want) {
			t.Errorf("profils sans %q :\n%s", want, out)
		}
	}
	out = h.mustRun("plan")
	for _, want := range []string{"parc", "confiance « ssh depuis 203.0.113.10 »", "activation d'ufw"} {
		if !strings.Contains(out, want) {
			t.Errorf("le plan global doit porter le pare-feu (%q) :\n%s", want, out)
		}
	}

	h.mustRun("apply", "--yes")
	profiles := h.read(t, "etc/ufw/applications.d/ogs")
	if !strings.Contains(profiles, "[ogs-mysql]") || !strings.Contains(profiles, "Généré par ogs") {
		t.Errorf("fichier de profils :\n%s", profiles)
	}
	// La confiance vaut aussi pour fail2ban : une adresse qui a accès à tout ne
	// doit pas être bannie par une autre voie.
	if jails := h.read(t, "etc/fail2ban/jail.d/ogs.conf"); !strings.Contains(jails, "203.0.113.10") {
		t.Errorf("les prisons doivent ignorer les adresses de confiance :\n%s", jails)
	}

	out = h.mustRun("firewall", "whois", "203.0.113.10")
	if !strings.Contains(out, "Agence de Nantes") {
		t.Errorf("whois doit nommer le propriétaire :\n%s", out)
	}
}

func TestPareFeuBanConserveLesCommentaires(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh")
	h.ecrireUFW(ufwEssai)

	h.mustRun("firewall", "ban", "198.51.100.9", "--comment", "robot", "--no-apply")
	got := h.read(t, "etc/ogs/ufw.yaml")
	for _, want := range []string{"# pare-feu d'essai", "# bannissements tenus à la main", "# scan du 12/09",
		"address: 198.51.100.9", "comment: robot"} {
		if !strings.Contains(got, want) {
			t.Errorf("ufw.yaml sans %q après ban :\n%s", want, got)
		}
	}
	h.mustRun("firewall", "unban", "192.0.2.66", "--no-apply")
	got = h.read(t, "etc/ogs/ufw.yaml")
	if strings.Contains(got, "192.0.2.66") || !strings.Contains(got, "198.51.100.9") {
		t.Errorf("unban ne doit retirer que l'adresse visée :\n%s", got)
	}

	// Une adresse de confiance ne se bannit pas : le document serait invalide.
	if _, code := h.run("firewall", "ban", "203.0.113.10", "--no-apply"); code != CodeValidation {
		t.Errorf("bannir une adresse de confiance doit rendre %d, obtenu %d", CodeValidation, code)
	}
	if strings.Contains(h.read(t, "etc/ogs/ufw.yaml"), "- 203.0.113.10") {
		t.Error("le document refusé ne doit pas avoir été écrit")
	}

	// Liste en ligne (« ban: [] ») : réécrite en bloc.
	h.ecrireUFW("trusted: [10.0.0.1]\nban: []\n")
	h.mustRun("firewall", "ban", "192.0.2.1", "--no-apply")
	if got := h.read(t, "etc/ogs/ufw.yaml"); !strings.Contains(got, "- 192.0.2.1") {
		t.Errorf("ban dans une liste vide :\n%s", got)
	}
}

func TestPareFeuDeclarationInvalideNePrivePasLeParc(t *testing.T) {
	// Un ufw.yaml invalide n'est jamais appliqué, mais il ne doit pas priver le
	// parc du reste : « project delete » recalcule les prisons et la rotation des
	// journaux par le plan global (D68), et une faute de frappe dans le pare-feu
	// ne doit pas l'en empêcher.
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.ecrireUFW("trusted: [inconnue]\n")

	out, code := h.run("apply", "--yes")
	if code != CodePartial || !strings.Contains(out, "source « inconnue »") {
		t.Errorf("apply doit rendre %d en nommant le motif, obtenu %d :\n%s", CodePartial, code, out)
	}
	if !strings.Contains(h.read(t, "etc/logrotate.d/ogs"), "shopfr") {
		t.Error("le reste du plan global doit avoir été appliqué")
	}
	if _, err := os.Stat(h.path("etc/ufw/applications.d/ogs")); err == nil {
		t.Error("rien ne doit avoir été tenté sur ufw")
	}
	if _, code := h.run("firewall", "apply", "--yes"); code != CodeValidation {
		t.Errorf("« firewall apply » doit rendre %d, obtenu %d", CodeValidation, code)
	}
	if _, code := h.run("firewall", "plan"); code != CodeValidation {
		t.Errorf("« firewall plan » doit rendre %d, obtenu %d", CodeValidation, code)
	}
	// La confirmation ne dépend pas d'ufw.yaml : sans activation en attente, elle
	// le dit, même sur un fichier cassé.
	if out := h.mustRun("firewall", "confirm"); !strings.Contains(out, "aucune activation") {
		t.Errorf("confirm :\n%s", out)
	}
	h.mustRunPartial("project", "delete", "shopfr", "--yes")
	if strings.Contains(h.read(t, "etc/logrotate.d/ogs"), "shopfr") {
		t.Error("la suppression doit recalculer la rotation malgré le pare-feu refusé")
	}
}

func TestPareFeuInitProposeLesProfilsDetectes(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh", "redis-server")
	h.mustRun("firewall", "init")
	got := h.read(t, "etc/ogs/ufw.yaml")
	for _, want := range []string{"redis", "disponible", "incoming: deny", "à restreindre"} {
		if !strings.Contains(got, want) {
			t.Errorf("ufw.yaml de départ sans %q :\n%s", want, got)
		}
	}
	if _, code := h.run("firewall", "init"); code != CodeConflict {
		t.Errorf("init ne doit pas écraser une déclaration existante (code %d)", code)
	}
	// Le fichier de départ doit être applicable tel quel.
	h.mustRun("firewall", "plan")
}

func TestPareFeuBanSurUneCleCommentee(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh")
	h.ecrireUFW("trusted: [10.0.0.1]\nban:   # liste noire\n  - 192.0.2.66\n")
	h.mustRun("firewall", "ban", "192.0.2.67", "--no-apply")
	got := h.read(t, "etc/ogs/ufw.yaml")
	if strings.Count(got, "ban:") != 1 || !strings.Contains(got, "192.0.2.67") || !strings.Contains(got, "# liste noire") {
		t.Errorf("ban sur une clé commentée :\n%s", got)
	}
	h.mustRun("firewall", "unban", "192.0.2.66", "--no-apply")
	h.mustRun("firewall", "unban", "192.0.2.67", "--no-apply")
	got = h.read(t, "etc/ogs/ufw.yaml")
	if strings.Contains(got, "192.0.2.6") || !strings.Contains(got, "[]") || !strings.Contains(got, "# liste noire") {
		t.Errorf("une liste vidée reste une liste, commentaire compris :\n%s", got)
	}
}

func TestPareFeuSansMoteurWeb(t *testing.T) {
	// Deux moteurs sans web.engine : l'attelage complet refuserait de choisir,
	// mais bannir une adresse ne doit pas en dépendre.
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh")
	h.ecrireUFW(ufwEssai)
	for _, d := range []string{"etc/apache2/sites-available", "etc/nginx/sites-available"} {
		if err := os.MkdirAll(h.path(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.mustRun("firewall", "plan")
}
