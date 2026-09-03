package model

import (
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/naming"
)

func TestNormalizeDefaultShellIsOpen(t *testing.T) {
	// Le shell des comptes de projet est ouvert par défaut (D62) : la connexion
	// entrante reste décidée par sshd et par les clés autorisées, non par ogs.
	p := New("shop.fr", naming.EnvProd, KindStatic)
	if err := p.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	if p.Spec.User.Shell != DefaultShell {
		t.Errorf("shell %q, attendu %q", p.Spec.User.Shell, DefaultShell)
	}
}

func TestNormalizeKeepsDeclaredShell(t *testing.T) {
	// Un projet qui ferme son shell doit le garder : le défaut ne s'applique qu'à
	// ce qui n'est pas déclaré, sinon fermer l'accès d'un projet serait impossible.
	p := New("shop.fr", naming.EnvProd, KindStatic)
	p.Spec.User.Shell = "/usr/sbin/nologin"
	if err := p.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	if p.Spec.User.Shell != "/usr/sbin/nologin" {
		t.Errorf("shell %q : le shell déclaré doit être respecté", p.Spec.User.Shell)
	}
}

func TestNormalizeRefusesRelativeShell(t *testing.T) {
	// useradd prendrait un chemin relatif tel quel et rendrait le compte
	// inutilisable sans un mot : le refus vaut mieux au chargement.
	p := New("shop.fr", naming.EnvProd, KindStatic)
	p.Spec.User.Shell = "bash"
	err := p.Normalize("/home", "preprod-pp.net")
	if err == nil {
		t.Fatal("un shell relatif doit être refusé")
	}
	if !strings.Contains(err.Error(), "spec.user.shell") {
		t.Errorf("le message doit nommer le champ fautif : %v", err)
	}
}
