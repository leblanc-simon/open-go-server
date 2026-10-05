package firewall

import (
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
)

// Une règle « any » existe dans les deux familles, et une seule commande la pose
// ou la retire. Quand l'administrateur la tient dans une seule famille, la
// commande commune lui donnait la marque d'ogs — ufw remplace le commentaire en
// place — et ogs la retirait le jour où ufw.yaml cessait de la demander. Chaque
// commande vise alors sa famille, et la règle de l'administrateur reste la
// sienne (R2-16, D109).

// La règle voulue manque en IPv6 ; l'administrateur la tient en IPv4.
func TestRègleDeLAdministrateurDansUneFamilleNeReçoitPasLaMarque(t *testing.T) {
	b := nouveauBanc(t, "profiles:\n  web: {allow: [any]}\npolicy: {incoming: allow}\n", "ssh")
	b.write("/etc/ufw/user.rules", "*filter\n"+
		tuple("allow any 80,443 0.0.0.0/0 any 0.0.0.0/0 ogs-web - in", "à moi"))
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	d := diffs(t, b.plan())
	if !strings.Contains(d, "ufw allow from ::/0 to any app ogs-web comment 'ogs: accès public à web'") {
		t.Errorf("la famille manquante doit être posée seule :\n%s", d)
	}
	if strings.Contains(d, "ufw allow from any to any app ogs-web") {
		t.Errorf("la commande commune marquerait la règle IPv4 de l'administrateur :\n%s", d)
	}

	// Posée, la règle d'ogs voisine celle de l'administrateur : plus rien à faire.
	b.write("/etc/ufw/user6.rules", "*filter\n"+
		tuple("allow any 80,443 ::/0 any ::/0 ogs-web - in", "ogs: accès public à web"))
	for _, s := range b.plan() {
		if strings.HasPrefix(s.Resource, "firewall/rule/") && s.Mutates() {
			t.Errorf("les deux familles sont tenues, rien à reprendre : %s", s.Summary)
		}
	}
}

// Une règle neuve, absente des deux familles, garde la commande commune.
func TestRègleNeuveDesDeuxFamillesPoséeDUneCommande(t *testing.T) {
	b := nouveauBanc(t, "profiles:\n  web: {allow: [any]}\npolicy: {incoming: allow}\n", "ssh")
	d := diffs(t, b.plan())
	if !strings.Contains(d, "ufw allow from any to any app ogs-web comment 'ogs: accès public à web'") ||
		strings.Contains(d, "from ::/0") || strings.Contains(d, "from 0.0.0.0/0") {
		t.Errorf("une règle neuve se pose d'une commande, dans les deux familles :\n%s", d)
	}
}

// La règle n'est plus voulue : seule la famille d'ogs part, quelle que soit la
// famille relue la première.
func TestRetraitDUneFamilleNEmportePasCelleDeLAdministrateur(t *testing.T) {
	for _, c := range []struct {
		nom, v4, v6, attendu string
	}{
		{"ogs en IPv4", "ogs: accès public à ssh", "", "ufw delete allow from 0.0.0.0/0 to any app ogs-ssh"},
		{"ogs en IPv6", "", "ogs: accès public à ssh", "ufw delete allow from ::/0 to any app ogs-ssh"},
	} {
		t.Run(c.nom, func(t *testing.T) {
			b := nouveauBanc(t, "policy: {incoming: allow}\n", "ssh")
			b.write("/etc/ufw/user.rules", "*filter\n"+tuple("allow any 22 0.0.0.0/0 any 0.0.0.0/0 ogs-ssh - in", c.v4))
			b.write("/etc/ufw/user6.rules", "*filter\n"+tuple("allow any 22 ::/0 any ::/0 ogs-ssh - in", c.v6))
			b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
			d := diffs(t, b.plan())
			if !strings.Contains(d, c.attendu) {
				t.Errorf("la règle d'ogs doit partir, seule :\n%s", d)
			}
			if strings.Contains(d, "ufw delete allow from any to any app ogs-ssh") {
				t.Errorf("le retrait commun emporterait la règle de l'administrateur :\n%s", d)
			}
		})
	}
}

// Le commentaire d'ogs a changé ; l'autre famille est à l'administrateur.
func TestCommentaireCorrigéDansSaSeuleFamille(t *testing.T) {
	b := nouveauBanc(t, "profiles:\n  web: {allow: [any]}\npolicy: {incoming: allow}\n", "ssh")
	b.write("/etc/ufw/user.rules", "*filter\n"+
		tuple("allow any 80,443 0.0.0.0/0 any 0.0.0.0/0 ogs-web - in", "ogs: ancien commentaire"))
	b.write("/etc/ufw/user6.rules", "*filter\n"+
		tuple("allow any 80,443 ::/0 any ::/0 ogs-web - in", "à moi"))
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	d := diffs(t, b.plan())
	if !strings.Contains(d, "ufw allow from 0.0.0.0/0 to any app ogs-web comment 'ogs: accès public à web'") {
		t.Errorf("le commentaire se corrige dans la famille d'ogs :\n%s", d)
	}
	if strings.Contains(d, "ufw allow from any to any app ogs-web") {
		t.Errorf("la commande commune marquerait la règle IPv6 de l'administrateur :\n%s", d)
	}
}

// Une sortie se restreint à sa famille par sa destination.
func TestSortieDUneFamillePoséeSeule(t *testing.T) {
	b := nouveauBanc(t, "policy: {incoming: allow, outgoing: deny}\noutgoing:\n  allow: [dns, https]\n", "ssh")
	b.write("/etc/ufw/user6.rules", "*filter\n"+tuple("allow any 53 ::/0 any ::/0 out", ""))
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	d := diffs(t, b.plan())
	if !strings.Contains(d, "ufw allow out to 0.0.0.0/0 port 53 comment 'ogs: sortie dns'") {
		t.Errorf("la sortie manquante en IPv4 doit être posée seule :\n%s", d)
	}
}

// Sans session, la garde comptait toute règle de l'administrateur comme
// ouverture pour tous : une règle ssh tenue en IPv4 seule laissait retirer la
// moitié IPv6 d'ogs, et ssh se fermait en IPv6 sans refus — un défaut antérieur
// que le retrait famille par famille rendait atteignable (D109). La garde juge
// désormais chaque famille ouverte avant le plan.
func TestGardeSansSessionJugeChaqueFamille(t *testing.T) {
	const déclaration = "profiles:\n  web: {allow: [any]}\n"
	b := nouveauBanc(t, déclaration, "ssh")
	b.write("/etc/ufw/user.rules", "*filter\n"+tuple("allow tcp 22 0.0.0.0/0 any 0.0.0.0/0 ogs-ssh - in", "à moi"))
	b.write("/etc/ufw/user6.rules", "*filter\n"+tuple("allow tcp 22 ::/0 any ::/0 ogs-ssh - in", "ogs: accès public à ssh"))
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	_, err := b.planErr()
	if err == nil || !strings.Contains(err.Error(), "en IPv6") {
		t.Errorf("retirer le seul accès ssh IPv6 doit être refusé, en le nommant : %v", err)
	}

	// L'administrateur tient les deux familles : le retrait ne ferme rien.
	b = nouveauBanc(t, déclaration, "ssh")
	b.write("/etc/ufw/user.rules", "*filter\n"+
		tuple("allow tcp 22 0.0.0.0/0 any 0.0.0.0/0 ogs-ssh - in", "à moi")+
		tuple("allow any any 0.0.0.0/0 any 198.51.100.7 in", "ogs: confiance ancienne"))
	b.write("/etc/ufw/user6.rules", "*filter\n"+tuple("allow tcp 22 ::/0 any ::/0 ogs-ssh - in", "à moi"))
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	if !planResserre(t, b) {
		t.Fatal("montage : le plan doit retirer la règle de confiance")
	}
	if _, err := b.planErr(); err != nil {
		t.Errorf("les deux familles restent ouvertes par l'administrateur : %v", err)
	}

	// Ouvert en IPv4 seulement avant le plan : rien à protéger en IPv6.
	b = nouveauBanc(t, déclaration, "ssh")
	b.write("/etc/ufw/user.rules", "*filter\n"+
		tuple("allow tcp 22 0.0.0.0/0 any 0.0.0.0/0 ogs-ssh - in", "à moi")+
		tuple("allow any any 0.0.0.0/0 any 198.51.100.7 in", "ogs: confiance ancienne"))
	b.write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
	if !planResserre(t, b) {
		t.Fatal("montage : le plan doit retirer la règle de confiance")
	}
	if _, err := b.planErr(); err != nil {
		t.Errorf("ssh n'était ouvert qu'en IPv4 : rien ne se ferme : %v", err)
	}
}

// planResserre dit si le plan retire quelque chose : sans resserrement, la garde
// ne se prononce pas, et un cas qui passe ne prouverait rien.
func planResserre(t *testing.T, b *banc) bool {
	t.Helper()
	b.prov.AllowLockout = true
	defer func() { b.prov.AllowLockout = false }()
	for _, s := range b.plan() {
		if s.Action == engine.Delete {
			return true
		}
	}
	return false
}
