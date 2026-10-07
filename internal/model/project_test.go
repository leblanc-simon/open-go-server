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

func TestNormalizeEffaceLeMoteurDeProjet(t *testing.T) {
	// Le moteur web est un choix de machine : un spec.web.engine hérité d'une
	// déclaration antérieure est relu sans erreur, puis effacé, pour ne plus
	// laisser croire qu'il décide de quoi que ce soit.
	p := New("shop.fr", naming.EnvProd, KindStatic)
	p.Spec.Web.Engine = "nginx"
	if err := p.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	if p.Spec.Web.Engine != "" {
		t.Errorf("spec.web.engine %q conservé : le moteur ne se choisit pas par projet", p.Spec.Web.Engine)
	}
}

func TestLAncienneEntreeBinEn0750EstRetireeAussi(t *testing.T) {
	// Avant D51, ogs déclarait .bin et logs en 0750 root : ces entrées, gardées
	// telles quelles par la normalisation, sont les siennes aussi.
	p := New("shop.fr", naming.EnvProd, KindStatic)
	p.Spec.Layout.Dirs = []Dir{{Path: ".bin", Mode: "0750", Owner: "root"}, {Path: "logs", Mode: "0750", Owner: "root"}}
	if err := p.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	for _, d := range p.Spec.Layout.Dirs {
		if d.Path == ".bin" || d.Path == "logs" {
			t.Errorf("l'ancienne entrée %+v doit disparaître", d)
		}
	}
}

func TestLesReferencesALAncienBin(t *testing.T) {
	p := New("shop.fr", naming.EnvProd, KindStatic)
	if err := p.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	root := p.Spec.Layout.Root
	p.Spec.Crons = []CronJob{
		{Name: "a", Command: root + "/.bin/php x.php && " + root + "/.bin/php y.php"},
		{Name: "b", Command: root + "/.bin/php8.1 x.php"}, // pas un lien d'ogs
		{Name: "c", Command: "/opt" + root + "/.bin/php x.php"},
		{Name: "d", Command: "cd .. && .bin/php x.php"},
		{Name: "e", Command: `sh -c "${HOME}/.bin/ruby x.rb"`},
	}
	refs := p.LegacyBinRefs()
	got := map[string]LegacyBinRef{}
	for _, r := range refs {
		got[r.Field] = r
	}
	if r := got["spec.crons[a].command"]; r.Rewritten != root+"/.local/bin/php x.php && "+root+"/.local/bin/php y.php" {
		t.Errorf("a : %q", r.Rewritten)
	}
	for _, f := range []string{"spec.crons[b].command", "spec.crons[c].command"} {
		if _, ok := got[f]; ok {
			t.Errorf("%s ne désigne pas l'ancien .bin d'ogs", f)
		}
	}
	if r, ok := got["spec.crons[d].command"]; !ok || r.Rewritten != "" {
		t.Errorf("d : forme relative nommée sans réécriture attendue, obtenu %+v", r)
	}
	if r := got["spec.crons[e].command"]; r.Rewritten != `sh -c "${HOME}/.local/bin/ruby x.rb"` {
		t.Errorf("e : %q", r.Rewritten)
	}
}

// Arbitrage du lot Z (D116 × D123) : dans un projet Python, ~/.local/bin/python
// désigne <venv>/bin/python, et Python n'active pas un venv atteint par un lien
// vers son interpréteur. Une commande .bin/python est réécrite vers
// ~/.venv/bin/python, lien de répertoire qui l'active ; les autres liens, et
// python hors d'un projet Python, gardent ~/.local/bin.
func TestLAncienPythonEstReecritVersLeVenv(t *testing.T) {
	p := New("py.fr", naming.EnvProd, KindPython)
	p.Spec.App.Runtime = "3.12.7"
	if err := p.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	root := p.Spec.Layout.Root
	p.Spec.Crons = []CronJob{
		{Name: "a", Command: root + "/.bin/python manage.py x && ~/.bin/node y.js"},
		{Name: "b", Command: "$HOME/.bin/python -V"},
	}
	got := map[string]LegacyBinRef{}
	for _, r := range p.LegacyBinRefs() {
		got[r.Field] = r
	}
	if r := got["spec.crons[a].command"]; r.Rewritten != root+"/.venv/bin/python manage.py x && ~/.local/bin/node y.js" {
		t.Errorf("a : %q", r.Rewritten)
	}
	if r := got["spec.crons[b].command"]; r.Rewritten != "$HOME/.venv/bin/python -V" {
		t.Errorf("b : %q", r.Rewritten)
	}
	if rel := p.LegacyReplacement("python"); rel != ".venv/bin/python" {
		t.Errorf("remplaçant de python : %q", rel)
	}
	if rel := p.LegacyReplacement("node"); rel != ".local/bin/node" {
		t.Errorf("remplaçant de node : %q", rel)
	}

	s := New("shop.fr", naming.EnvProd, KindStatic)
	if err := s.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	s.Spec.Crons = []CronJob{{Name: "a", Command: "~/.bin/python x.py"}}
	if r := s.LegacyBinRefs(); len(r) != 1 || r[0].Rewritten != "~/.local/bin/python x.py" {
		t.Errorf("hors d'un projet Python, python garde ~/.local/bin : %+v", r)
	}
}

// doctor nomme les commandes déclarées qui appellent ~/.local/bin/python : le
// venv n'y est pas actif (D123). Le relevé ne vaut que pour un projet Python.
func TestLesAppelsALInterpreteurSansVenv(t *testing.T) {
	p := New("py.fr", naming.EnvProd, KindPython)
	p.Spec.App.Runtime = "3.12.7"
	if err := p.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	root := p.Spec.Layout.Root
	p.Spec.Crons = []CronJob{
		{Name: "a", Command: root + "/.local/bin/python manage.py x"},
		{Name: "b", Command: "cd www && ../.local/bin/python -V"},
		{Name: "c", Command: root + "/.venv/bin/python ok.py"},
		{Name: "d", Command: "~/.local/bin/python3 x.py"},
	}
	p.Spec.App.Env = map[string]string{"PY": "$HOME/.local/bin/python"}
	want := []string{"spec.crons[a].command", "spec.crons[b].command", "spec.app.env.PY"}
	if got := p.LocalPythonRefs(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("relevé : %v, attendu %v", got, want)
	}
	s := New("shop.fr", naming.EnvProd, KindStatic)
	if err := s.Normalize("/home", "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	s.Spec.Crons = []CronJob{{Name: "a", Command: "~/.local/bin/python x.py"}}
	if got := s.LocalPythonRefs(); len(got) != 0 {
		t.Errorf("hors d'un projet Python : %v", got)
	}
}
