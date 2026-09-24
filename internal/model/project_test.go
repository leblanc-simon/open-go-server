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

func TestLAncienneEntreeBinEstRetireeALaNormalisation(t *testing.T) {
	// Les déclarations antérieures à D70 portent .bin/ en root : la normalisation
	// l'en retire, et déclare à la place le répertoire des liens de l'utilisateur.
	p := New("shop.fr", naming.EnvProd, KindStatic)
	p.Spec.Layout.Dirs = []Dir{{Path: ".bin", Mode: "0710", Owner: "root"}, {Path: "outils", Mode: "0700"}}
	if err := p.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	seen := map[string]Dir{}
	for _, d := range p.Spec.Layout.Dirs {
		seen[d.Path] = d
	}
	if _, ok := seen[".bin"]; ok {
		t.Error("l'ancienne entrée .bin doit disparaître")
	}
	if d, ok := seen[LinkDir]; !ok || d.Owner != "" || d.Mode != "0700" {
		t.Errorf("%s attendu au compte du projet en 0700, obtenu %+v", LinkDir, d)
	}
	if _, ok := seen["outils"]; !ok {
		t.Error("une entrée de l'exploitant doit être conservée")
	}

	// Un .bin/ déclaré autrement l'a été par l'exploitant : il reste.
	q := New("blog.fr", naming.EnvProd, KindStatic)
	q.Spec.Layout.Dirs = []Dir{{Path: ".bin", Mode: "0750"}}
	if err := q.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	if q.Spec.Layout.Dirs[0].Path != ".bin" {
		t.Error("un .bin/ déclaré par l'exploitant ne doit pas être retiré")
	}
}

func TestLesJournauxQuittentLeHome(t *testing.T) {
	// Une déclaration antérieure à D71 porte <home>/logs : elle est ramenée sous
	// la racine des journaux, et l'ancienne entrée root de logs/ disparaît.
	p := New("shop.fr", naming.EnvProd, KindStatic)
	p.Spec.Layout.LogDir = "/home/shop.fr/logs"
	p.Spec.Layout.Dirs = []Dir{{Path: "logs", Mode: "0710", Owner: "root"}}
	if err := p.Normalize("/home", "preprod-pp.net", WithLogsRoot("/srv/journaux")); err != nil {
		t.Fatal(err)
	}
	if p.Spec.Layout.LogDir != "/srv/journaux/shopfr" {
		t.Errorf("répertoire des journaux attendu sous la racine configurée, obtenu %s", p.Spec.Layout.LogDir)
	}
	for _, d := range p.Spec.Layout.Dirs {
		if d.Path == "logs" {
			t.Error("l'ancienne entrée logs/ doit être retirée")
		}
	}
	// Un répertoire choisi par l'exploitant est laissé : c'est la validation qui
	// dira s'il est acceptable.
	q := New("blog.fr", naming.EnvProd, KindStatic)
	q.Spec.Layout.LogDir = "/srv/ailleurs/blog"
	if err := q.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	if q.Spec.Layout.LogDir != "/srv/ailleurs/blog" {
		t.Errorf("un choix de l'exploitant doit être conservé, obtenu %s", q.Spec.Layout.LogDir)
	}
}
