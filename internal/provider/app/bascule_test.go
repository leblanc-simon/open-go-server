package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
)

// Lot Y17 (D123) : le lien d'un runtime ne bascule que vers un binaire présent,
// un changement de lien redémarre l'application, et la version de Python est
// dans le chemin du venv.

// poserBinaire crée un exécutable factice à path.
func poserBinaire(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// étape rend l'étape de ressource resource, ou échoue.
func étape(t *testing.T, steps []*engine.Step, resource string) *engine.Step {
	t.Helper()
	for _, s := range steps {
		if s.Resource == resource {
			return s
		}
	}
	t.Fatalf("aucune étape %s", resource)
	return nil
}

// projetNodeEnService rend un projet Node dont le lien désigne la version
// 20.18.0, installée, et dont la déclaration demande 22.11.0, non installée :
// le cas d'une montée de version dont l'installation va échouer.
func projetNodeEnService(t *testing.T) (*Provider, *model.Project, string) {
	t.Helper()
	prov, proj := fixture(t, model.KindNode)
	prov.Exec = &realExecutor{exec.NewRecorder()}
	prov.Installer = &Installer{Exec: prov.Exec}
	root := proj.Spec.Layout.Root
	ancien := BinaryPath(root, RuntimeNode, "20.18.0")
	poserBinaire(t, ancien)
	if err := prov.setLink(proj, LinkName(RuntimeNode), ancien); err != nil {
		t.Fatal(err)
	}
	return prov, proj, ancien
}

// R4-22 : une installation en échec — réseau, version inconnue — est non
// critique ; le lien était pourtant basculé vers le binaire absent, à la place
// d'un lien qui fonctionnait, et l'application redémarrait sur rien.
func TestLeLienNeBasculeJamaisVersUnBinaireAbsent(t *testing.T) {
	prov, proj, ancien := projetNodeEnService(t)
	ctx := context.Background()
	obs, err := prov.Observe(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(ctx, proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	lien := étape(t, steps, "app/link/node")
	if lien.Action != engine.Update {
		t.Fatalf("le plan doit annoncer la bascule du lien : %s", lien.Action)
	}
	err = lien.Apply(ctx)
	if err == nil {
		t.Fatal("la bascule vers un binaire absent doit échouer")
	}
	if !strings.Contains(err.Error(), "v22.11.0") {
		t.Errorf("le refus doit nommer le binaire attendu : %v", err)
	}
	// Ni critique — l'apply d'un site qui tournait ne doit pas être déroulé —, ni
	// muet : un avertissement, code 6.
	if !errorsAsTolerated(err) {
		t.Errorf("l'échec doit être toléré (non critique) : %T %v", err, err)
	}
	if got := readLink(proj, LinkName(RuntimeNode)); got != ancien {
		t.Errorf("le lien en service a bougé : %q, attendu %q", got, ancien)
	}

	// L'installation a réussi entre le plan et l'étape du lien : la bascule a
	// lieu, sans exiger un second apply.
	poserBinaire(t, BinaryPath(proj.Spec.Layout.Root, RuntimeNode, "22.11.0"))
	if err := lien.Apply(ctx); err != nil {
		t.Fatalf("binaire présent à l'application : %v", err)
	}
	if got := readLink(proj, LinkName(RuntimeNode)); got != BinaryPath(proj.Spec.Layout.Root, RuntimeNode, "22.11.0") {
		t.Errorf("le lien n'a pas suivi l'installation : %q", got)
	}
}

func errorsAsTolerated(err error) bool {
	var tol *engine.ToleratedError
	return err != nil && errors.As(err, &tol)
}

// R4-22 : le lanceur de Ruby suit le même régime que l'interpréteur.
func TestLeLienBundleNeBasculeJamaisVersUnLanceurAbsent(t *testing.T) {
	prov, proj := fixture(t, model.KindRuby)
	prov.Exec = &realExecutor{exec.NewRecorder()}
	proj.Spec.App.Runtime = "3.3.6"
	root := proj.Spec.Layout.Root
	ancien := BundlePath(root, "3.2.0")
	poserBinaire(t, ancien)
	if err := prov.setLink(proj, LinkBundle, ancien); err != nil {
		t.Fatal(err)
	}
	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if err := étape(t, steps, "app/link/bundle").Apply(context.Background()); !errorsAsTolerated(err) {
		t.Errorf("lanceur absent : échec toléré attendu, obtenu %v", err)
	}
	if got := readLink(proj, LinkBundle); got != ancien {
		t.Errorf("le lien bundle a bougé : %q", got)
	}
}

// Go n'a pas de gestionnaire : le binaire arrive par le déploiement, souvent
// après le premier apply. Son lien se pose sans lui.
func TestLeLienGoSePoseAvantLeDeploiement(t *testing.T) {
	prov, proj := fixture(t, model.KindGo)
	prov.Exec = &realExecutor{exec.NewRecorder()}
	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if err := étape(t, steps, "app/link/go").Apply(context.Background()); err != nil {
		t.Errorf("le lien de Go doit se poser avant le dépôt du binaire : %v", err)
	}
}

// R1-40 : un lien changé ne redémarrait rien — l'application continuait sur
// l'ancienne version, que l'apply annonçait remplacée. Un lien inchangé, lui,
// ne redémarre rien.
func TestUnLienChangeRedemarreLApplication(t *testing.T) {
	prov, proj, _ := projetNodeEnService(t)
	ecritUnite(t, prov, proj)
	poserBinaire(t, BinaryPath(proj.Spec.Layout.Root, RuntimeNode, "22.11.0"))
	ctx := context.Background()
	obs, err := prov.Observe(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(ctx, proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(étape(t, steps, "app/link/node").Reload, RestartTargetIntent(proj.ID())) {
		t.Errorf("le changement de lien doit redémarrer l'application : %v", étape(t, steps, "app/link/node").Reload)
	}
	if err := étape(t, steps, "app/link/node").Apply(ctx); err != nil {
		t.Fatal(err)
	}
	obs, err = prov.Observe(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err = prov.Plan(ctx, proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if s := étape(t, steps, "app/link/node"); s.Action != engine.NoOp || len(s.Reload) > 0 {
		t.Errorf("un lien conforme ne redémarre rien : %s %v", s.Action, s.Reload)
	}
}

// R1-40 : une application à l'écoute au moment du plan n'était jamais
// revérifiée, même quand l'apply la redémarrait — une unité réécrite, un lien
// changé. Elle l'est dès qu'un redémarrage est prévu.
func TestUneApplicationRedemarreeEstAttendue(t *testing.T) {
	prov, proj := fixture(t, model.KindNode)
	ctx := context.Background()
	obs, err := prov.Observe(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	obs.Listening = true
	health, err := prov.PlanHealth(ctx, proj, obs, false)
	if err != nil {
		t.Fatal(err)
	}
	if health.Action != engine.NoOp {
		t.Errorf("sans redémarrage, une application à l'écoute n'est pas attendue : %s", health.Action)
	}
	health, err = prov.PlanHealth(ctx, proj, obs, true)
	if err != nil {
		t.Fatal(err)
	}
	if health.Action == engine.NoOp || !health.FlushReloads || health.Critical {
		t.Errorf("redémarrée, l'application doit être attendue après le redémarrage, en non critique : "+
			"%s flush=%v critique=%v", health.Action, health.FlushReloads, health.Critical)
	}
}

// pyvenv écrit le pyvenv.cfg d'un venv dans dir, créé par l'interpréteur home.
func pyvenv(t *testing.T, dir, home, version string) {
	t.Helper()
	poserBinaire(t, filepath.Join(dir, "bin", "python"))
	cfg := "home = " + home + "\ninclude-system-site-packages = false\nversion = " + version + "\n"
	if err := os.WriteFile(filepath.Join(dir, "pyvenv.cfg"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// R4-23 : le venv portait un nom sans version. L'apply le tenait pour conforme
// dès qu'il existait — la version déclarée n'était jamais appliquée — et
// « runtime install » laissait l'ancien interpréteur dans le venv. Le venv
// porte désormais sa version : changer de version en crée un autre, à côté.
func TestLeVenvPorteLaVersionDePython(t *testing.T) {
	prov, proj := fixture(t, model.KindPython)
	root := proj.Spec.Layout.Root
	proj.Spec.App.Runtime = "3.12.7"
	want := filepath.Join(root, ".venv-3.12.7", "bin", "python")
	if got := BinaryPath(root, RuntimePython, "3.12.7"); got != want {
		t.Errorf("venv de 3.12.7 : %q, attendu %q", got, want)
	}
	poserBinaire(t, want)
	if !Installed(root, RuntimePython, "3.12.7") {
		t.Error("le venv de 3.12.7 est en place")
	}
	// La déclaration passe à 3.13.1 : le venv de 3.12.7 ne la satisfait pas.
	if Installed(root, RuntimePython, "3.13.1") {
		t.Error("le venv d'une autre version ne satisfait pas la déclaration")
	}
	proj.Spec.App.Runtime = "3.13.1"
	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	if obs.RuntimeInstalled {
		t.Error("la version déclarée n'est pas installée : l'apply doit l'installer")
	}
}

// Un venv d'avant D123, www/.venv, reste celui de la version qui l'a créé dans
// ce home : le recréer vide couperait l'application de ses dépendances. Celui
// d'une autre version, ou recopié du home d'un autre projet par un clone, ne
// l'est pas.
func TestUnAncienVenvResteCeluiDeSaVersion(t *testing.T) {
	_, proj := fixture(t, model.KindPython)
	root := proj.Spec.Layout.Root
	legacy := filepath.Join(root, "www", ".venv")
	pyvenv(t, legacy, filepath.Join(root, ".pyenv", "versions", "3.12.7", "bin"), "3.12.7")
	if got := BinaryPath(root, RuntimePython, "3.12.7"); got != filepath.Join(legacy, "bin", "python") {
		t.Errorf("l'ancien venv de 3.12.7 doit rester celui de 3.12.7 : %q", got)
	}
	if !Installed(root, RuntimePython, "3.12.7") {
		t.Error("l'ancien venv satisfait sa version")
	}
	if got := BinaryPath(root, RuntimePython, "3.13.1"); got != filepath.Join(root, ".venv-3.13.1", "bin", "python") {
		t.Errorf("une autre version a son propre venv : %q", got)
	}

	// Recopié d'une production par un clone d'avant D123 : son interpréteur est
	// dans un autre home.
	_, prep := fixture(t, model.KindPython)
	proot := prep.Spec.Layout.Root
	pyvenv(t, filepath.Join(proot, "www", ".venv"), filepath.Join(root, ".pyenv", "versions", "3.12.7", "bin"), "3.12.7")
	if got := BinaryPath(proot, RuntimePython, "3.12.7"); got != filepath.Join(proot, ".venv-3.12.7", "bin", "python") {
		t.Errorf("un venv dont l'interpréteur vit dans un autre home n'est pas repris : %q", got)
	}
}

// L'installation crée le venv de la version, jamais par-dessus un autre.
func TestLInstallationCreeLeVenvDeLaVersion(t *testing.T) {
	_, proj := fixture(t, model.KindPython)
	rec := exec.NewRecorder()
	installer := &Installer{Exec: &realExecutor{rec}}
	if err := installer.Install(context.Background(), proj, RuntimePython, "3.13.1"); err != nil {
		t.Fatal(err)
	}
	calls := rec.Calls()
	script := strings.Join(calls[len(calls)-1].Args, " ")
	venv := filepath.Join(proj.Spec.Layout.Root, ".venv-3.13.1")
	if !strings.Contains(script, "-m venv "+`"`+venv+`"`) {
		t.Errorf("le venv doit être créé dans %s :\n%s", venv, script)
	}
	if strings.Contains(script, filepath.Join("www", ".venv")) {
		t.Errorf("l'ancien venv ne doit pas être réécrit :\n%s", script)
	}
}

// Une version porte un nom de répertoire : elle ne peut ni sortir du home ni
// glisser une commande dans le script d'installation.
func TestUneVersionDeRuntimeEstUnNomSimple(t *testing.T) {
	for _, v := range []string{"../../etc", "3.12;id", "3 12", "", "-x"} {
		if err := CheckVersion(v); err == nil {
			t.Errorf("version %q acceptée", v)
		}
	}
	for _, v := range []string{"3.12.7", "22.11.0", "3.13-dev", "pypy3.10-7.3.12", "lts_iron",
		"truffleruby+graalvm-24.1.1", "jruby-9.4.8.0", "3.13t", "system"} {
		if err := CheckVersion(v); err != nil {
			t.Errorf("version %q refusée : %v", v, err)
		}
	}
}

// Les venvs que la déclaration ne désigne plus sont gardés pour un retour
// arrière, et nommés.
func TestLesVenvsQuittesSontNommes(t *testing.T) {
	_, proj := fixture(t, model.KindPython)
	root := proj.Spec.Layout.Root
	pyvenv(t, filepath.Join(root, "www", ".venv"), filepath.Join(root, ".pyenv", "versions", "3.11.9", "bin"), "3.11.9")
	pyvenv(t, filepath.Join(root, ".venv-3.12.7"), filepath.Join(root, ".pyenv", "versions", "3.12.7", "bin"), "3.12.7")
	pyvenv(t, filepath.Join(root, ".venv-3.13.1"), filepath.Join(root, ".pyenv", "versions", "3.13.1", "bin"), "3.13.1")
	proj.Spec.App.Runtime = "3.13.1"
	got := RetiredVenvs(proj)
	want := []string{filepath.Join(root, ".venv-3.12.7"), filepath.Join(root, "www", ".venv")}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("venvs quittés : %v, attendu %v", got, want)
	}
	proj.Spec.App.Runtime = "3.11.9"
	if got := RetiredVenvs(proj); slices.Contains(got, filepath.Join(root, "www", ".venv")) {
		t.Errorf("l'ancien venv de la version déclarée sert encore : %v", got)
	}
}

// Relecture Y17 (v2) : le lien ~/.local/bin/python n'active pas le venv — Python
// cherche pyvenv.cfg à côté du chemin invoqué, sans résoudre le lien —, et un
// changement de version basculait l'application sur un venv vide.

// projetPythonAvantD123 rend un projet Python tel qu'ogs le laissait avant
// D123 : www/.venv créé par le pyenv du home, lien .local/bin/python vers lui,
// unité qui lance .local/bin/python ; avecGunicorn dit si le venv porte le
// module que lance l'unité.
func projetPythonAvantD123(t *testing.T, avecGunicorn bool) (*Provider, *model.Project, string) {
	t.Helper()
	prov, proj := fixture(t, model.KindPython)
	prov.Exec = &realExecutor{exec.NewRecorder()}
	prov.Installer = &Installer{Exec: prov.Exec}
	root := proj.Spec.Layout.Root
	proj.Spec.App.Runtime = "3.12.7"
	legacy := filepath.Join(root, "www", ".venv")
	pyvenv(t, legacy, filepath.Join(root, ".pyenv", "versions", "3.12.7", "bin"), "3.12.7")
	if avecGunicorn {
		poserBinaire(t, filepath.Join(legacy, "bin", "gunicorn"))
	}
	if err := prov.setLink(proj, LinkName(RuntimePython), filepath.Join(legacy, "bin", "python")); err != nil {
		t.Fatal(err)
	}
	endpoint, err := prov.Endpoint(proj)
	if err != nil {
		t.Fatal(err)
	}
	content, err := prov.build(proj, endpoint, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(prov.unitPath(proj)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prov.unitPath(proj), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(prov.Paths.ProjectRunDir(proj.ID()), 0o710); err != nil {
		t.Fatal(err)
	}
	return prov, proj, legacy
}

func planDe(t *testing.T, prov *Provider, proj *model.Project) []*engine.Step {
	t.Helper()
	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	return steps
}

// B2 : l'unité d'une application Python lance l'interpréteur du venv par le
// lien de répertoire ~/.venv, jamais par ~/.local/bin/python.
func TestLUniteDUneApplicationPythonPasseParLeLienDuVenv(t *testing.T) {
	prov, proj := fixture(t, model.KindPython)
	proj.Spec.App.Runtime = "3.12.7"
	endpoint, err := prov.Endpoint(proj)
	if err != nil {
		t.Fatal(err)
	}
	content, err := prov.Build(proj, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	want := "ExecStart=" + filepath.Join(proj.Spec.Layout.Root, ".venv", "bin", "python") + " -m gunicorn"
	if !strings.Contains(string(content), want) {
		t.Errorf("%q absent de l'unité :\n%s", want, content)
	}
	// Premier apply : ~/.venv est posé vers le venv de la version, sans exiger de
	// dépendances — il n'y a rien à garder.
	prov.Exec = &realExecutor{exec.NewRecorder()}
	venv := filepath.Join(proj.Spec.Layout.Root, ".venv-3.12.7")
	pyvenv(t, venv, filepath.Join(proj.Spec.Layout.Root, ".pyenv", "versions", "3.12.7", "bin"), "3.12.7")
	lien := étape(t, planDe(t, prov, proj), "app/link/.venv")
	if err := lien.Apply(context.Background()); err != nil {
		t.Fatalf("premier lien : %v", err)
	}
	kind, target, err := fsLinkState(proj.Spec.Layout.Root, VenvLinkName)
	if err != nil || kind != "lien" || target != venv {
		t.Errorf("~/.venv : %s %q %v, attendu un lien vers %s", kind, target, err, venv)
	}
}

// B1 : changer de version ne bascule jamais une application en service sur un
// venv qui ne porte pas ce que lance son unité.
func TestUnChangementDeVersionNeBasculePasSurUnVenvVide(t *testing.T) {
	prov, proj, _ := projetPythonAvantD123(t, true)
	ctx := context.Background()
	// Transition : ~/.venv posé vers l'ancien venv, qui sert.
	for _, s := range planDe(t, prov, proj) {
		if s.Action != engine.NoOp && s.Apply != nil {
			if err := s.Apply(ctx); err != nil {
				t.Fatalf("%s : %v", s.Resource, err)
			}
		}
	}
	root := proj.Spec.Layout.Root
	legacy := filepath.Join(root, "www", ".venv")
	proj.Spec.App.Runtime = "3.12.8"
	steps := planDe(t, prov, proj)
	neuf := filepath.Join(root, ".venv-3.12.8")
	for _, r := range []string{"app/link/python", "app/link/.venv"} {
		if s := étape(t, steps, r); !strings.Contains(s.Summary, "pip install") {
			t.Errorf("le plan doit annoncer les dépendances attendues : %s", s.Summary)
		}
	}
	// L'installation pose un venv vide.
	pyvenv(t, neuf, filepath.Join(root, ".pyenv", "versions", "3.12.8", "bin"), "3.12.8")
	for _, r := range []string{"app/link/python", "app/link/.venv"} {
		err := étape(t, steps, r).Apply(ctx)
		if !errorsAsTolerated(err) || !strings.Contains(err.Error(), "-m pip install") {
			t.Errorf("%s : échec toléré nommant pip attendu, obtenu %v", r, err)
		}
	}
	if got := readLink(proj, LinkName(RuntimePython)); got != filepath.Join(legacy, "bin", "python") {
		t.Errorf("le lien de l'interpréteur a bougé : %s", got)
	}
	if _, target, _ := fsLinkState(root, VenvLinkName); target != legacy {
		t.Errorf("~/.venv a bougé : %s", target)
	}
	if retired := RetiredVenvs(proj); slices.Contains(retired, legacy) {
		t.Errorf("le venv en service n'est pas un venv quitté : %v", retired)
	}
	// Dépendances installées : la bascule a lieu, et redémarre l'application.
	poserBinaire(t, filepath.Join(neuf, "bin", "gunicorn"))
	steps = planDe(t, prov, proj)
	for _, r := range []string{"app/link/python", "app/link/.venv"} {
		s := étape(t, steps, r)
		if err := s.Apply(ctx); err != nil {
			t.Errorf("%s : %v", r, err)
		}
		if !slices.Contains(s.Reload, RestartTargetIntent(proj.ID())) {
			t.Errorf("%s doit redémarrer l'application", r)
		}
	}
	if _, target, _ := fsLinkState(root, VenvLinkName); target != neuf {
		t.Errorf("~/.venv doit désigner %s : %s", neuf, target)
	}
}

// Transition d'un projet d'avant D123 : si l'ancien venv porte le module de
// l'unité, ~/.venv est posé et l'unité réécrite ; sinon rien de ce qui tourne
// ne change, et l'étape nomme la commande à lancer.
func TestLaTransitionDUnAncienVenvNeCoupeRien(t *testing.T) {
	prov, proj, legacy := projetPythonAvantD123(t, true)
	steps := planDe(t, prov, proj)
	if s := étape(t, steps, "app/link/python"); s.Action != engine.NoOp {
		t.Errorf("même version : le lien de l'interpréteur ne bouge pas : %s", s.Action)
	}
	venvLink := étape(t, steps, "app/link/.venv")
	if venvLink.Action != engine.Create {
		t.Errorf("~/.venv doit être posé : %s", venvLink.Action)
	}
	if err := venvLink.Apply(context.Background()); err != nil {
		t.Errorf("l'ancien venv porte gunicorn : %v", err)
	}
	if u := étape(t, steps, "app/unit/shopfr"); u.Action != engine.Update ||
		!slices.Contains(u.Reload, RestartTargetIntent(proj.ID())) {
		t.Errorf("l'unité passe à ~/.venv et redémarre l'application : %s %v", u.Action, u.Reload)
	}
	if _, target, _ := fsLinkState(proj.Spec.Layout.Root, VenvLinkName); target != legacy {
		t.Errorf("~/.venv doit désigner l'ancien venv : %s", target)
	}

	// Sans gunicorn dans l'ancien venv, l'application tournait sur
	// l'interpréteur de base : on n'y touche pas.
	prov, proj, _ = projetPythonAvantD123(t, false)
	steps = planDe(t, prov, proj)
	if u := étape(t, steps, "app/unit/shopfr"); u.Action != engine.NoOp {
		t.Errorf("l'unité qui tourne ne doit pas changer : %s %s", u.Action, u.Summary)
	}
	err := étape(t, steps, "app/link/.venv").Apply(context.Background())
	if !errorsAsTolerated(err) || !strings.Contains(err.Error(), "-m pip install") {
		t.Errorf("le lien du venv doit attendre les dépendances : %v", err)
	}
}

// Un clone d'avant D123 a laissé dans la préproduction le www/.venv de la
// production, désigné par son lien : la même garde vaut.
func TestUnVenvRecopieDeLaProductionResteJusquAuxDependances(t *testing.T) {
	prov, proj := fixture(t, model.KindPython)
	prov.Exec = &realExecutor{exec.NewRecorder()}
	root := proj.Spec.Layout.Root
	proj.Spec.App.Runtime = "3.12.7"
	copie := filepath.Join(root, "www", ".venv")
	pyvenv(t, copie, "/home/production/.pyenv/versions/3.12.7/bin", "3.12.7")
	poserBinaire(t, filepath.Join(copie, "bin", "gunicorn"))
	if err := prov.setLink(proj, LinkName(RuntimePython), filepath.Join(copie, "bin", "python")); err != nil {
		t.Fatal(err)
	}
	ecritUnite(t, prov, proj)
	pyvenv(t, filepath.Join(root, ".venv-3.12.7"), filepath.Join(root, ".pyenv", "versions", "3.12.7", "bin"), "3.12.7")
	err := étape(t, planDe(t, prov, proj), "app/link/python").Apply(context.Background())
	if !errorsAsTolerated(err) {
		t.Errorf("le venv neuf ne porte pas gunicorn : échec toléré attendu, obtenu %v", err)
	}
}

// Un ~/.venv-<v> inachevé, ou posé en lien, ne passe pas avant un venv qui
// fonctionne, et n'est pas un venv quitté.
func TestUnVenvInacheveOuLieNePassePasAvantCeluiQuiSert(t *testing.T) {
	_, proj := fixture(t, model.KindPython)
	root := proj.Spec.Layout.Root
	proj.Spec.App.Runtime = "3.12.7"
	legacy := filepath.Join(root, "www", ".venv")
	pyvenv(t, legacy, filepath.Join(root, ".pyenv", "versions", "3.12.7", "bin"), "3.12.7")
	if err := os.MkdirAll(filepath.Join(root, ".venv-3.12.7"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := PythonVenv(root, "3.12.7"); got != legacy {
		t.Errorf("un venv inachevé passe avant celui qui sert : %s", got)
	}
	ailleurs := t.TempDir()
	poserBinaire(t, filepath.Join(ailleurs, "bin", "python"))
	if err := os.Symlink(ailleurs, filepath.Join(root, ".venv-3.13.1")); err != nil {
		t.Fatal(err)
	}
	if got := PythonVenv(root, "3.13.1"); Installed(root, RuntimePython, "3.13.1") {
		t.Errorf("un lien n'est pas un venv : %s", got)
	}
	if retired := RetiredVenvs(proj); slices.Contains(retired, filepath.Join(root, ".venv-3.13.1")) {
		t.Errorf("un lien n'est pas un venv quitté : %v", retired)
	}
}

// fsLinkState décrit root/name : « lien » et sa cible, « autre » ou « absent ».
func fsLinkState(root, name string) (string, string, error) {
	info, err := os.Lstat(filepath.Join(root, name))
	if err != nil {
		return "absent", "", nil
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "autre", "", nil
	}
	target, err := os.Readlink(filepath.Join(root, name))
	return "lien", target, err
}

// v3 : une unité déjà passée à ~/.venv n'est jamais ramenée vers
// ~/.local/bin/python, qui n'active aucun venv — même si ~/.venv devient un
// vrai répertoire.
func TestUneUniteSurLeVenvNyRenonceJamais(t *testing.T) {
	prov, proj, _ := projetPythonAvantD123(t, true)
	ctx := context.Background()
	for _, s := range planDe(t, prov, proj) {
		if s.Action != engine.NoOp && s.Apply != nil {
			if err := s.Apply(ctx); err != nil {
				t.Fatalf("%s : %v", s.Resource, err)
			}
		}
	}
	unit, err := os.ReadFile(prov.unitPath(proj))
	if err != nil || !strings.Contains(string(unit), VenvPython(proj.Spec.Layout.Root)) {
		t.Fatalf("l'unité doit être passée à ~/.venv : %v\n%s", err, unit)
	}
	venvLink := filepath.Join(proj.Spec.Layout.Root, VenvLinkName)
	if err := os.Remove(venvLink); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(venvLink, 0o755); err != nil {
		t.Fatal(err)
	}
	steps := planDe(t, prov, proj)
	if u := étape(t, steps, "app/unit/shopfr"); u.Action != engine.NoOp {
		t.Errorf("l'unité ne doit pas revenir à ~/.local/bin/python : %s %s", u.Action, u.Summary)
	}
	err = étape(t, steps, "app/link/.venv").Apply(ctx)
	if !errorsAsTolerated(err) || !strings.Contains(err.Error(), venvLink) {
		t.Errorf("l'étape du lien doit nommer le répertoire, en échec toléré : %v", err)
	}
}

// v3 : l'étape de ~/.venv a échoué entre le plan et l'unité — l'unité en place
// n'est pas réécrite vers un ~/.venv absent.
func TestLUniteNeSuitPasUnLienDuVenvAbsent(t *testing.T) {
	prov, proj, _ := projetPythonAvantD123(t, true)
	steps := planDe(t, prov, proj)
	before, _ := os.ReadFile(prov.unitPath(proj))
	u := étape(t, steps, "app/unit/shopfr")
	if u.Action != engine.Update {
		t.Fatalf("la transition réécrit l'unité : %s", u.Action)
	}
	// L'étape du lien n'est pas appliquée : elle a échoué.
	if err := u.Apply(context.Background()); !errorsAsTolerated(err) {
		t.Errorf("unité vers un ~/.venv absent : échec toléré attendu, obtenu %v", err)
	}
	if after, _ := os.ReadFile(prov.unitPath(proj)); string(after) != string(before) {
		t.Error("l'unité en place ne doit pas être réécrite")
	}
}
