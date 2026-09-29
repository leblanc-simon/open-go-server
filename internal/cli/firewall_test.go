package cli

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/provider/web"
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
	// le dit, même sur un fichier cassé — et, ufw ne filtrant rien, en écart.
	if out, code := h.run("firewall", "confirm"); code != CodePartial || !strings.Contains(out, "aucune activation") {
		t.Errorf("confirm, code %d :\n%s", code, out)
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

func (h *harness) ecrire(rel, content string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(h.path(rel)), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(h.path(rel), []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// tupleUFW écrit une ligne de user.rules telle qu'ufw la range.
func tupleUFW(fields, comment string) string {
	line := "### tuple ### " + fields
	if comment != "" {
		line += " comment=" + hex.EncodeToString([]byte(comment))
	}
	return line + "\n-A ufw-user-input -j ACCEPT\n\n"
}

func TestPareFeuSansMoteurWeb(t *testing.T) {
	// Deux moteurs sans web.engine : l'attelage complet refuserait de choisir,
	// mais bannir une adresse ne doit pas en dépendre — ni retirer l'accès public
	// au web : un moteur qu'on ne sait pas nommer n'est pas l'absence de serveur
	// web. Le mode préfixé se rabat sur Apache et ne rend jamais cet échec : celui
	// du mode réel lui est substitué.
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh")
	h.ecrireUFW(ufwEssai)
	for _, d := range []string{"etc/apache2/sites-available", "etc/nginx/sites-available"} {
		if err := os.MkdirAll(h.path(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, derr := web.DetectEngine("", h.prefix)
	if derr == nil {
		t.Fatal("deux moteurs sans web.engine : la détection doit refuser de choisir")
	}
	orig := firewallWebEngine
	firewallWebEngine = func(*App) (web.Engine, error) { return nil, exitf(CodeDependency, "%s", derr) }
	t.Cleanup(func() { firewallWebEngine = orig })

	// Pare-feu actif, l'accès public au web posé.
	h.ecrire("etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	h.ecrire("etc/ufw/user.rules", "*filter\n"+
		tupleUFW("allow any 80,443 0.0.0.0/0 any 0.0.0.0/0 ogs-web - in", "ogs: accès public à web")+"COMMIT\n")
	h.ecrire("etc/ufw/user6.rules", "*filter\n"+
		tupleUFW("allow any 80,443 ::/0 any ::/0 ogs-web - in", "ogs: accès public à web")+"COMMIT\n")

	h.mustRun("firewall", "plan")
	out := h.mustRun("firewall", "ban", "198.51.100.9", "--yes")
	if strings.Contains(out, "retrait") {
		t.Errorf("bannir une adresse ne doit retirer aucun accès au web :\n%s", out)
	}
	if !strings.Contains(out, "198.51.100.9") {
		t.Errorf("le bannissement doit être posé :\n%s", out)
	}
	if out := h.mustRun("firewall", "profiles"); !strings.Contains(out, "moteur non retenu") {
		t.Errorf("le profil web reste disponible, faute de moteur retenu :\n%s", out)
	}
}

// Le --force de toutes les commandes écrase les fichiers retouchés (§6.4) : on
// le donne pour un vhost, sans penser au pare-feu. Il levait pourtant les gardes
// contre la coupure dans tout plan global — « apply shopfr --force » retirait la
// dernière règle ssh qu'un « apply » sans lui refusait de retirer.
func TestPareFeuForceNeLevePasLaGardeContreLaCoupure(t *testing.T) {
	const avecConfiance = "sources:\n  bureau:\n    owner: \"Bureau\"\n    addresses: [10.200.0.2]\n" +
		"trusted: [bureau]\nprofiles:\n  web:\n    allow: [any]\n"
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.ecrireUFW(avecConfiance)
	h.mustRun("apply", "--yes")
	// Pare-feu actif et réconcilié : ssh ne passe que par la confiance.
	h.ecrire("etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	h.ecrire("etc/ufw/user.rules", "*filter\n"+
		tupleUFW("allow any 22 0.0.0.0/0 any 10.200.0.2 ogs-ssh - in", "ogs: confiance — bureau (Bureau)")+
		tupleUFW("allow any 80,443 0.0.0.0/0 any 0.0.0.0/0 ogs-web - in", "ogs: accès public à web")+
		tupleUFW("allow any 80,443 0.0.0.0/0 any 10.200.0.2 ogs-web - in", "ogs: confiance — bureau (Bureau)")+
		"COMMIT\n")
	// trusted retiré par erreur : plus aucune source ssh.
	h.ecrireUFW("sources:\n  bureau:\n    addresses: [10.200.0.2]\nprofiles:\n  web:\n    allow: [any]\n")

	for _, args := range [][]string{
		{"apply", "--yes"},
		{"apply", "shopfr", "--yes", "--force"},
		// Par le recalcul du parc, où --force sert à supprimer un compte adopté.
		{"project", "delete", "shopfr", "--yes", "--force"},
	} {
		out, code := h.run(args...)
		if (code != CodePartial && args[0] == "apply") || (code != CodeOK && code != CodePartial) ||
			!strings.Contains(out, "ssh : aucune source") {
			t.Errorf("« ogs %s » : le pare-feu doit être refusé en avertissement, obtenu le code %d :\n%s",
				strings.Join(args, " "), code, out)
		}
		if strings.Contains(out, "retrait de « ogs-ssh") {
			t.Errorf("« ogs %s » a retiré la dernière règle ssh :\n%s", strings.Join(args, " "), out)
		}
		if !strings.Contains(out, "--allow-lockout") {
			t.Errorf("« ogs %s » : le refus doit nommer la seule option qui le lève :\n%s", strings.Join(args, " "), out)
		}
	}
	if _, code := h.run("firewall", "apply", "--yes", "--force"); code != CodeValidation {
		t.Errorf("« firewall apply --force » ne lève pas la garde : code %d attendu, obtenu %d", CodeValidation, code)
	}
	out := h.mustRun("firewall", "apply", "--yes", "--allow-lockout")
	if !strings.Contains(out, "retrait de « ogs-ssh depuis 10.200.0.2 »") {
		t.Errorf("--allow-lockout lève la garde :\n%s", out)
	}
}

// Un « ufw limit OpenSSH » posé à la main est une source ssh : ogs ne le retire
// jamais. Ignoré, il faisait refuser « ssh : aucune source » à chaque apply — et
// l'on prenait l'habitude de lever la garde.
func TestPareFeuRegleSSHDeLAdministrateurSuffit(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh")
	h.ecrire("etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	h.ecrire("etc/ufw/user.rules", "*filter\n"+
		tupleUFW("limit tcp 22 0.0.0.0/0 any 0.0.0.0/0 OpenSSH - in", "")+"COMMIT\n")
	h.ecrireUFW("profiles:\n  web:\n    allow: [any]\n")
	if out, code := h.run("apply", "--yes"); code != CodeOK || strings.Contains(out, "aucune source") {
		t.Errorf("la règle ssh de l'administrateur est une source, obtenu le code %d :\n%s", code, out)
	}
}

// Un « confirm » lancé après l'échéance répondait « aucune activation en
// attente », alors que le retour automatique venait de désactiver ufw :
// l'exploitant croyait son pare-feu en place, et un script ne distinguait pas
// ce cas d'une confirmation.
func TestPareFeuConfirmSurUFWInactif(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh")
	// Sans ufw.yaml, ogs ne gère pas ufw : son inactivité ne le regarde pas.
	if out := h.mustRun("firewall", "confirm"); strings.Contains(out, "inactif") {
		t.Errorf("sans ufw.yaml, rien à signaler :\n%s", out)
	}
	h.ecrireUFW("sources:\n  bureau:\n    addresses: [10.200.0.2]\ntrusted: [bureau]\n")
	out, code := h.run("firewall", "confirm")
	if code != CodePartial || !strings.Contains(out, "ufw est inactif") {
		t.Errorf("confirm doit dire qu'ufw ne filtre plus rien, en code %d : code %d\n%s", CodePartial, code, out)
	}
	// Voulu inactif, il n'y a là rien d'anormal — ni conseil de le réactiver.
	h.ecrireUFW("active: false\n")
	if out := h.mustRun("firewall", "confirm"); !strings.Contains(out, "comme ufw.yaml le demande") ||
		strings.Contains(out, "firewall apply") {
		t.Errorf("ufw inactif comme ufw.yaml le demande :\n%s", out)
	}
	// Cassé, ufw.yaml ne décide rien : la confirmation ne l'exige pas valide.
	h.ecrireUFW("active: peut-être\n")
	if _, code := h.run("firewall", "confirm"); code != CodePartial {
		t.Errorf("ufw.yaml illisible : l'inactivité reste signalée, code %d attendu, obtenu %d", CodePartial, code)
	}
	h.ecrire("etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	if out := h.mustRun("firewall", "confirm"); strings.Contains(out, "inactif") ||
		!strings.Contains(out, "aucune activation en attente") {
		t.Errorf("ufw actif, sans minuteur : rien à confirmer :\n%s", out)
	}
}

// Un redémarrage dans le délai efface le retour automatique, qui vit sous /run,
// et ufw repart actif : confirm répondait « aucune activation en attente », et
// status comme doctor se taisaient. Le témoin de /var/lib/ogs le fait savoir.
func TestPareFeuActivationJamaisConfirmée(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.ufwInstalle("ssh")
	h.ecrireUFW(ufwEssai)
	h.mustRun("apply", "--yes")
	témoin := "var/lib/ogs/ufw-unconfirmed"
	if _, err := os.Stat(h.path(témoin)); !os.IsNotExist(err) {
		t.Fatalf("en mode simulé, rien n'est armé, et nul témoin n'est posé : %v", err)
	}

	// Redémarrage : ufw actif, le témoin là, le minuteur disparu.
	h.ecrire(témoin, "# activation jamais confirmée\n")
	h.ecrire("etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	if out := h.mustRun("firewall", "status"); !strings.Contains(out, "retour automatique perdu") {
		t.Errorf("status doit dire l'activation jamais confirmée :\n%s", out)
	}
	// doctor échoue sous préfixe pour d'autres raisons (modules d'Apache) : seul
	// le constat compte ici.
	if out, _ := h.run("doctor"); !strings.Contains(out, "retour automatique a disparu") {
		t.Errorf("doctor doit signaler le retour perdu :\n%s", out)
	}
	if out := h.mustRun("firewall", "confirm"); !strings.Contains(out, "confirmée") ||
		!strings.Contains(out, "avait déjà disparu") {
		t.Errorf("confirm doit confirmer l'activation au retour perdu :\n%s", out)
	}
	if _, err := os.Stat(h.path(témoin)); !os.IsNotExist(err) {
		t.Errorf("confirm doit retirer le témoin : %v", err)
	}

	// Le retour a joué : ufw inactif, le témoin là.
	h.ecrire(témoin, "# activation jamais confirmée\n")
	h.ecrire("etc/ufw/ufw.conf", "ENABLED=no\nLOGLEVEL=low\n")
	if out := h.mustRun("firewall", "status"); !strings.Contains(out, "défaite par le retour automatique") {
		t.Errorf("status doit dire le retour joué :\n%s", out)
	}
	out, code := h.run("firewall", "confirm")
	if code != CodePartial || !strings.Contains(out, "le retour automatique a défait l'activation") {
		t.Errorf("confirm après le retour, code %d :\n%s", code, out)
	}
}
