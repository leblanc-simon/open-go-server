package cli

import (
	"net/netip"
	"os"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/provider/firewall"
)

// « firewall ban » écrivait ufw.yaml avant de calculer le plan : un
// bannissement que la garde refusait restait écrit, et le prochain apply le
// retrouvait sans un mot (R2-14). Le plan se calcule et se garde d'abord, sur le
// document modifié ; un refus laisse ufw.yaml intact (D109).
func TestBannissementRefuséNÉcritPasUFWYaml(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh")
	// Aucune source ssh : toute exécution du pare-feu est refusée par la garde
	// « ssh : aucune source », que seul --allow-lockout lève.
	const declaration = "profiles:\n  web:\n    allow: [any]\nban: []\n"
	h.ecrireUFW(declaration)

	// Sous --no-apply, un refus déjà porté par ufw.yaml n'empêche plus d'écrire :
	// voir TestBannissementNoApplyMalgréUnRefusDeGardePréexistant.
	for _, args := range [][]string{
		{"firewall", "ban", "198.51.100.9", "--yes"},
	} {
		out, code := h.run(args...)
		if code != CodeValidation || !strings.Contains(out, "ssh : aucune source") {
			t.Errorf("« ogs %s » : la garde doit refuser en code %d, obtenu %d :\n%s",
				strings.Join(args, " "), CodeValidation, code, out)
		}
		if got := h.read(t, "etc/ogs/ufw.yaml"); got != declaration {
			t.Errorf("« ogs %s » : un bannissement refusé ne doit pas être écrit :\n%s", strings.Join(args, " "), got)
		}
	}

	// --allow-lockout lève la garde : le bannissement est écrit et appliqué.
	out := h.mustRun("firewall", "ban", "198.51.100.9", "--yes", "--allow-lockout")
	if !strings.Contains(h.read(t, "etc/ogs/ufw.yaml"), "198.51.100.9") || !strings.Contains(out, "198.51.100.9") {
		t.Errorf("--allow-lockout : le bannissement doit être écrit et posé :\n%s", out)
	}
	// Le retrait se garde de même.
	before := h.read(t, "etc/ogs/ufw.yaml")
	if _, code := h.run("firewall", "unban", "198.51.100.9", "--yes"); code != CodeValidation {
		t.Errorf("unban refusé par la garde : code %d attendu, obtenu %d", CodeValidation, code)
	}
	if got := h.read(t, "etc/ogs/ufw.yaml"); got != before {
		t.Errorf("un retrait refusé ne doit pas être écrit :\n%s", got)
	}
}

// Une confirmation déclinée n'écrit rien non plus : rien n'a été fait.
func TestBannissementDéclinéNÉcritPasUFWYaml(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh")
	h.ecrireUFW(ufwEssai)
	before := h.read(t, "etc/ogs/ufw.yaml")
	if _, code := h.run("firewall", "ban", "198.51.100.9"); code != CodeCancelled {
		t.Errorf("confirmation déclinée : code %d attendu, obtenu %d", CodeCancelled, code)
	}
	if got := h.read(t, "etc/ogs/ufw.yaml"); got != before {
		t.Errorf("un bannissement décliné ne doit pas être écrit :\n%s", got)
	}
}

// Un ufw.yaml invalide vidait l'ignoreip des prisons fail2ban des adresses de
// confiance : le plan global les recalcule à chaque apply, et le provider du
// pare-feu, sans déclaration lisible, n'en rendait plus aucune — l'adresse
// d'administration redevenait bannissable alors que le plan annonçait « ufw
// laissé en l'état » (R2-15). Les adresses du fichier en place sont gardées
// tant que la déclaration ne se lit pas (D109).
func TestUFWYamlInvalideGardeLesAdressesDeConfianceDeFail2ban(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh")
	h.ecrireUFW(ufwEssai)
	h.mustRun("apply", "--yes")
	const prisons = "etc/fail2ban/jail.d/ogs.conf"
	if !strings.Contains(h.read(t, prisons), "203.0.113.10") {
		t.Fatalf("montage : la confiance doit rejoindre l'ignoreip :\n%s", h.read(t, prisons))
	}

	h.ecrireUFW(ufwEssai + "trusted: [inconnue]\n")
	// Le plan dit qu'une adresse retirée d'ufw.yaml resterait de confiance.
	if out, _ := h.run("plan"); !strings.Contains(out, "reste de confiance tant qu'il ne se relit pas") {
		t.Errorf("le plan doit dire que les adresses de confiance sont reprises :\n%s", out)
	}
	out, code := h.run("apply", "--yes")
	if code != CodePartial || !strings.Contains(out, "déclaration refusée") {
		t.Errorf("ufw.yaml invalide : apply doit rendre %d en le nommant, obtenu %d :\n%s", CodePartial, code, out)
	}
	jails := h.read(t, prisons)
	if !strings.Contains(jails, "203.0.113.10") {
		t.Errorf("ufw.yaml invalide : l'adresse de confiance doit rester dans l'ignoreip :\n%s", jails)
	}

	// La déclaration redevenue lisible fait foi de nouveau : la confiance retirée
	// quitte l'ignoreip.
	h.ecrireUFW("profiles:\n  ssh:\n    allow: [any]\n  web:\n    allow: [any]\n")
	h.mustRun("apply", "--yes")
	if jails := h.read(t, prisons); strings.Contains(jails, "203.0.113.10") {
		t.Errorf("ufw.yaml lisible : la confiance retirée doit quitter l'ignoreip :\n%s", jails)
	}
}

// Le plan gardé avant l'écriture refusait, sous --no-apply, tout bannissement
// dès qu'ufw.yaml portait un problème sans rapport — un profil ftp à plage
// passive inconnue —, là où main l'écrivait. Seuls les refus des gardes, et les
// problèmes que le bannissement fait naître, empêchent d'écrire (D109).
func TestBannissementMalgréUnProblèmeSansRapport(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh", "vsftpd")
	h.ecrireUFW(strings.Replace(ufwEssai, "    allow: [any]\n", "    allow: [any]\n  ftp:\n    allow: [any]\n", 1))
	if out, code := h.run("firewall", "plan"); code != CodeValidation || !strings.Contains(out, "ftp") {
		t.Fatalf("montage : le profil ftp doit être refusé, code %d :\n%s", code, out)
	}

	out, code := h.run("firewall", "ban", "198.51.100.9", "--no-apply")
	if code != CodePartial || !strings.Contains(out, "ftp") {
		t.Errorf("--no-apply : écrit, le problème dit, code %d attendu, obtenu %d :\n%s", CodePartial, code, out)
	}
	if !strings.Contains(h.read(t, "etc/ogs/ufw.yaml"), "198.51.100.9") {
		t.Error("--no-apply : le bannissement doit être écrit")
	}
	out, code = h.run("firewall", "ban", "198.51.100.10", "--yes")
	if code != CodeValidation || !strings.Contains(h.read(t, "etc/ogs/ufw.yaml"), "198.51.100.10") {
		t.Errorf("sans --no-apply : écrit, pare-feu non appliqué, code %d attendu, obtenu %d :\n%s",
			CodeValidation, code, out)
	}

	// Un problème que le bannissement fait naître refuse, et n'écrit rien.
	if err := os.WriteFile(h.path("etc/default/ufw"),
		[]byte("IPV6=no\nDEFAULT_INPUT_POLICY=\"DROP\"\nDEFAULT_OUTPUT_POLICY=\"ACCEPT\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := h.read(t, "etc/ogs/ufw.yaml")
	out, code = h.run("firewall", "ban", "2001:db8::66", "--no-apply")
	if code != CodeValidation || !strings.Contains(out, "IPV6=no") {
		t.Errorf("bannissement IPv6 sous IPV6=no : refus en code %d attendu, obtenu %d :\n%s", CodeValidation, code, out)
	}
	if got := h.read(t, "etc/ogs/ufw.yaml"); got != before {
		t.Errorf("un bannissement refusé ne doit pas être écrit :\n%s", got)
	}
}

// Sous --no-apply, un refus des gardes qu'ufw.yaml portait déjà n'empêche pas
// d'écrire un bannissement sans rapport, comme main ; un refus que le
// bannissement fait naître, si (D109).
func TestBannissementNoApplyMalgréUnRefusDeGardePréexistant(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh")
	// Aucune source ssh : refus « ssh : aucune source », déjà présent.
	h.ecrireUFW("profiles:\n  web:\n    allow: [any]\nban: []\n")
	out, code := h.run("firewall", "ban", "198.51.100.9", "--no-apply")
	if code != CodePartial || !strings.Contains(out, "ssh : aucune source") || !strings.Contains(out, "--allow-lockout") {
		t.Errorf("--no-apply, refus préexistant : code %d attendu, obtenu %d :\n%s", CodePartial, code, out)
	}
	if !strings.Contains(h.read(t, "etc/ogs/ufw.yaml"), "198.51.100.9") {
		t.Error("--no-apply, refus préexistant : le bannissement doit être écrit")
	}

	// Un refus neuf — la session bannie — reste un refus, sans écriture.
	firewallAdmin = func() (firewall.Session, bool) {
		return firewall.Session{Addr: netip.MustParseAddr("203.0.113.10")}, true
	}
	t.Cleanup(func() { firewallAdmin = nil })
	before := h.read(t, "etc/ogs/ufw.yaml")
	out, code = h.run("firewall", "ban", "203.0.113.0/24", "--no-apply")
	if code != CodeValidation || !strings.Contains(out, "bannirait") {
		t.Errorf("refus neuf : code %d attendu, obtenu %d :\n%s", CodeValidation, code, out)
	}
	if got := h.read(t, "etc/ogs/ufw.yaml"); got != before {
		t.Errorf("un refus neuf ne doit pas être écrit :\n%s", got)
	}
}
