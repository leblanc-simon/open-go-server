package config

import (
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"

	"leblanc.io/open-go-server/internal/model"
)

func TestDefaultShellIsOpen(t *testing.T) {
	if got := Default().Shell(); got != model.DefaultShell {
		t.Errorf("shell par défaut %q, attendu %q", got, model.DefaultShell)
	}
}

func TestShellFallsBackWhenUnset(t *testing.T) {
	// Une configuration ancienne ne porte pas la clé : le défaut du modèle prend
	// le relais plutôt que de rendre une chaîne vide, qui ferait échouer useradd.
	c := &Config{}
	if got := c.Shell(); got != model.DefaultShell {
		t.Errorf("shell %q, attendu %q", got, model.DefaultShell)
	}
}

func TestShellIsConfigurable(t *testing.T) {
	c := &Config{User: User{Shell: "/usr/sbin/nologin"}}
	if got := c.Shell(); got != "/usr/sbin/nologin" {
		t.Errorf("shell %q : la configuration doit primer", got)
	}
}

func TestValidateRefusesRelativeShell(t *testing.T) {
	c := Default()
	c.User.Shell = "bash"
	err := c.Validate()
	if err == nil {
		t.Fatal("un shell relatif doit être refusé au chargement")
	}
	if !strings.Contains(err.Error(), "user.shell") {
		t.Errorf("le message doit nommer la clé fautive : %v", err)
	}
}

func TestEcouteDeSupervisionRefuseeHorsBoucleLocale(t *testing.T) {
	// Le vhost de supervision expose l'état de tous les pools du parc sans la
	// moindre authentification : l'ouvrir sur une interface routable publierait la
	// carte du serveur. Le refus est au chargement, non à l'écriture du vhost — une
	// erreur à l'apply laisserait le fichier écrit avant de se plaindre.
	for _, listen := range []string{"0.0.0.0:8099", "203.0.113.4:8099", "[::]:8099"} {
		c := Default()
		c.Monitoring.Status.Listen = listen
		err := c.Validate()
		if err == nil {
			t.Errorf("%q accepté : une écoute hors boucle locale doit être refusée", listen)
			continue
		}
		if !strings.Contains(err.Error(), "boucle locale") {
			t.Errorf("%q : message peu explicite : %v", listen, err)
		}
	}
}

func TestEcouteDeSupervisionAccepteLaBoucleLocale(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:8099", "127.0.0.2:9000", "[::1]:8099"} {
		c := Default()
		c.Monitoring.Status.Listen = listen
		if err := c.Validate(); err != nil {
			t.Errorf("%q refusé alors qu'il est local : %v", listen, err)
		}
	}
}

func TestEcouteDeSupervisionRefuseUnNomDHote(t *testing.T) {
	// Un nom d'hôte se résoudrait différemment selon le moteur — et « localhost »
	// vise tantôt 127.0.0.1, tantôt ::1 : Apache écouterait ailleurs que là où
	// l'agent interroge.
	c := Default()
	c.Monitoring.Status.Listen = "localhost:8099"
	if err := c.Validate(); err == nil {
		t.Error("un nom d'hôte doit être refusé au profit d'une adresse IP")
	}
}

func TestSupervisionDefautsAppliques(t *testing.T) {
	m := Monitoring{}
	if got := m.StatusListen(); got != DefaultStatusListen {
		t.Errorf("écoute par défaut %q, attendu %q", got, DefaultStatusListen)
	}
	if got := m.TelegrafUser(); got != DefaultTelegrafUser {
		t.Errorf("compte de l'agent %q, attendu %q", got, DefaultTelegrafUser)
	}
	if !m.InputEnabled("mysql") || !m.JailEnabled("sshd") {
		t.Error("une entrée ou une prison absente de la table est activée")
	}
	m.Telegraf.Inputs = map[string]bool{"mysql": false}
	if m.InputEnabled("mysql") {
		t.Error("une entrée explicitement désactivée doit le rester")
	}
}

func TestSupervisionTriEtat(t *testing.T) {
	// Nil n'est pas « faux » : il signifie « qu'ogs décide d'après ce qui est
	// installé ». Les confondre activerait la supervision sur une machine où rien
	// n'est installé, ou l'inverse.
	m := Monitoring{}
	if _, set := m.TelegrafEnabled(); set {
		t.Error("un champ absent ne doit pas compter comme une décision")
	}
	no := false
	m.Telegraf.Enabled = &no
	value, set := m.TelegrafEnabled()
	if !set || value {
		t.Error("un « false » explicite doit être retenu comme une décision")
	}
}

func TestFusionConserveLesDefautsDesTables(t *testing.T) {
	// Le décodeur remplace une table entière dès qu'elle est déclarée : un fichier
	// n'annonçant que le domaine de préproduction vidait la référence au
	// certificat partagé (@wildcard-prep) et faisait disparaître « prod ». La
	// mutualisation des certificats de préproduction (D26) tombait en silence.
	c := Default()
	doc := "environments:\n  prep:\n    domain: preprod.mycorp.net\n"
	if err := yaml.Unmarshal([]byte(doc), c); err != nil {
		t.Fatal(err)
	}
	prep := c.Environments["prep"]
	if prep.Domain != "preprod.mycorp.net" {
		t.Errorf("le domaine déclaré doit primer : %q", prep.Domain)
	}
	if prep.Certificate != "@wildcard-prep" {
		t.Errorf("certificat partagé effacé : %q", prep.Certificate)
	}
	if prep.HostTemplate != "{{.PrepLabel}}.{{.Domain}}" || prep.IDSuffix != "prep" {
		t.Errorf("champ tu écrasé : %+v", prep)
	}
	if !prep.RobotsNoindex || !prep.PresetDowngrade {
		t.Errorf("champ tu écrasé : %+v", prep)
	}
	if _, ok := c.Environments["prod"]; !ok {
		t.Errorf("l'environnement prod a disparu : %v", c.Environments)
	}
	if _, ok := c.Certificates["wildcard-prep"]; !ok {
		t.Errorf("le certificat partagé livré a disparu : %v", c.Certificates)
	}
}

func TestFusionRespecteUneValeurExpliciteFausse(t *testing.T) {
	// Un « false » écrit l'emporte ; c'est le « false » par omission que la fusion
	// ne doit pas confondre avec une décision.
	c := Default()
	doc := "environments:\n  prep:\n    robotsNoindex: false\n"
	if err := yaml.Unmarshal([]byte(doc), c); err != nil {
		t.Fatal(err)
	}
	prep := c.Environments["prep"]
	if prep.RobotsNoindex {
		t.Error("un robotsNoindex explicitement faux doit être retenu")
	}
	if prep.Domain != "preprod-pp.net" {
		t.Errorf("le reste de l'entrée doit survivre : %+v", prep)
	}
}

func TestFusionLaisseRetirerUnCertificatPartage(t *testing.T) {
	// Le catalogue des certificats partagés appartient à l'administrateur :
	// réinsérer d'office wildcard-prep ferait demander à l'autorité un domaine qui
	// n'est pas le sien. La référence devenue orpheline est en revanche refusée au
	// chargement, plutôt qu'avalée par SharedRef.
	c := Default()
	doc := "certificates:\n  mycorp:\n    domains: [\"*.pp.mycorp.net\"]\n" +
		"    challenge: dns-01\n    dnsProvider: ovh\n"
	if err := yaml.Unmarshal([]byte(doc), c); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Certificates["wildcard-prep"]; ok {
		t.Error("un certificat non repris doit rester retiré")
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("une référence partagée orpheline doit être refusée au chargement")
	}
	if !strings.Contains(err.Error(), "wildcard-prep") || !strings.Contains(err.Error(), "mycorp") {
		t.Errorf("le message doit nommer la référence morte et ce qui reste : %v", err)
	}

	// L'environnement repointé sur le certificat maison, la configuration passe.
	prep := c.Environments["prep"]
	prep.Certificate = "@mycorp"
	c.Environments["prep"] = prep
	if err := c.Validate(); err != nil {
		t.Errorf("référence résolue refusée : %v", err)
	}
}

func TestFusionCompleteUneEntreeDeCertificatRedeclaree(t *testing.T) {
	// Redéclarer wildcard-prep pour ses seuls domaines ne doit pas lui ôter son
	// challenge ni son provider : le certificat deviendrait indemandable.
	c := Default()
	doc := "certificates:\n  wildcard-prep:\n    domains: [\"*.pp.mycorp.net\", \"pp.mycorp.net\"]\n"
	if err := yaml.Unmarshal([]byte(doc), c); err != nil {
		t.Fatal(err)
	}
	cert := c.Certificates["wildcard-prep"]
	if len(cert.Domains) != 2 || cert.Domains[0] != "*.pp.mycorp.net" {
		t.Errorf("domaines déclarés non retenus : %+v", cert)
	}
	if cert.Challenge != "dns-01" || cert.DNSProvider != "ovh" || cert.KeyType != "ec256" {
		t.Errorf("champs tus écrasés : %+v", cert)
	}
}

func TestFusionLaisseLesChampsDeStructureIntacts(t *testing.T) {
	// La fusion des tables ne doit pas défaire ce que le décodeur faisait déjà
	// bien : un fichier partiel garde les défauts des champs de structure.
	c := Default()
	doc := "web:\n  engine: nginx\n"
	if err := yaml.Unmarshal([]byte(doc), c); err != nil {
		t.Fatal(err)
	}
	if c.Web.Engine != "nginx" {
		t.Errorf("moteur %q", c.Web.Engine)
	}
	if c.Logs.Rotate != 30 || c.Naming.HomeRoot != "/home" || c.Sudo.Style != "ogs" {
		t.Errorf("défauts perdus : %+v", c)
	}
	if len(c.Environments) != 2 || len(c.Certificates) != 1 {
		t.Errorf("tables perdues : %v / %v", c.Environments, c.Certificates)
	}
}

func TestLeRepertoireDesSauvegardesEstCreeParInit(t *testing.T) {
	// Toute sauvegarde d'un écrasement forcé y atterrit : s'il n'est pas dans la
	// table des répertoires, personne ne le crée avec un mode explicite et le
	// premier apply forcé le poserait au gré du umask ambiant — un répertoire
	// portant des configurations système que leur original n'exposait qu'à root.
	// « ogs init » et « doctor » ne connaissent que cette table.
	p := NewPaths("/tmp/ogs-essai")
	if got, want := p.BackupsDir(), "/tmp/ogs-essai/var/lib/ogs/backups"; got != want {
		t.Errorf("répertoire de sauvegarde %q, attendu %q", got, want)
	}
	for _, d := range p.Dirs() {
		if d.Path != p.BackupsDir() {
			continue
		}
		if d.Mode != 0o700 {
			t.Errorf("mode %04o, attendu 0700", d.Mode)
		}
		return
	}
	t.Errorf("%s ne figure pas dans les répertoires créés par « ogs init »", p.BackupsDir())
}

func TestSignatureDKIMDefauts(t *testing.T) {
	m := Default().Mail
	if m.DKIMUser() != "opendkim" || m.DKIMKeyBits() != 2048 || m.DKIMSelectorPrefix() != "ogs" ||
		m.DKIMRetention() != 7*24*time.Hour {
		t.Errorf("défauts inattendus : %s %d %s %s", m.DKIMUser(), m.DKIMKeyBits(),
			m.DKIMSelectorPrefix(), m.DKIMRetention())
	}
	if _, set := m.DKIMEnabled(); set {
		t.Error("sans déclaration, la présence d'OpenDKIM doit décider")
	}
}

func TestSignatureDKIMRefusDuChargement(t *testing.T) {
	cases := []struct {
		nom    string
		regler func(*Config)
		motif  string
	}{
		// Une clé de 1024 bits publiée chez le client ne se rattrape pas sans une
		// seconde publication : le refus est au chargement.
		{"clé trop courte", func(c *Config) { c.Mail.DKIM.KeyBits = 1024 }, "keyBits"},
		{"préfixe hors label DNS", func(c *Config) { c.Mail.DKIM.SelectorPrefix = "Ogs_" }, "selectorPrefix"},
		{"rétention illisible", func(c *Config) { c.Mail.DKIM.Retention = "une semaine" }, "retention"},
	}
	for _, c := range cases {
		t.Run(c.nom, func(t *testing.T) {
			cfg := Default()
			c.regler(cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), c.motif) {
				t.Errorf("refus portant sur %s attendu, obtenu %v", c.motif, err)
			}
		})
	}
}

func TestLaRacineDesJournauxNePeutPasVivreSousLesHomes(t *testing.T) {
	if Default().LogsRoot() != "/var/log/ogs-projects" {
		t.Errorf("racine par défaut inattendue : %s", Default().LogsRoot())
	}
	for _, root := range []string{"/home/journaux", "journaux", "/"} {
		c := Default()
		c.Logs.Root = root
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "logs.root") {
			t.Errorf("logs.root %q doit être refusé, obtenu %v", root, err)
		}
	}
	c := Default()
	c.Logs.Root = "/srv/journaux"
	if err := c.Validate(); err != nil {
		t.Errorf("une racine hors des homes doit être acceptée : %v", err)
	}
}
