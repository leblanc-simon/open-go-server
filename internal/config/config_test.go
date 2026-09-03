package config

import (
	"strings"
	"testing"

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
