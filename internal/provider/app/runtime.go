// Package app porte les applications non-PHP : runtimes, processus supervisés et
// point d'écoute.
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// Runtimes gérés. Go n'en a pas : un binaire compilé est autonome. « custom »
// n'en a pas non plus, ni de lien géré : la commande déclarée est lancée telle
// quelle (D75).
const (
	RuntimeGo     = "go"
	RuntimeNode   = "node"
	RuntimePython = "python"
	RuntimeRuby   = "ruby"
	RuntimeCustom = "custom"
)

// managed indique si ogs pose un lien vers le binaire du runtime.
func managed(runtime string) bool { return runtime != RuntimeCustom }

// Runtimes énumère les runtimes connus.
func Runtimes() []string { return []string{RuntimeGo, RuntimeNode, RuntimePython, RuntimeRuby} }

// Installer installe un runtime dans le home d'un projet (D35).
type Installer struct {
	Exec exec.Executor
}

// managerDir renvoie le répertoire du gestionnaire de versions dans le home.
func managerDir(root, runtime string) string {
	switch runtime {
	case RuntimeNode:
		return filepath.Join(root, ".nvm")
	case RuntimeRuby:
		return filepath.Join(root, ".rbenv")
	case RuntimePython:
		return filepath.Join(root, ".pyenv")
	}
	return ""
}

// BinaryPath renvoie le chemin du binaire d'une version installée.
func BinaryPath(root, runtime, version string) string {
	switch runtime {
	case RuntimeNode:
		return filepath.Join(root, ".nvm", "versions", "node", "v"+version, "bin", "node")
	case RuntimeRuby:
		return filepath.Join(root, ".rbenv", "versions", version, "bin", "ruby")
	case RuntimePython:
		// Le lien pointe vers le venv du projet, pas vers l'interpréteur nu : les
		// dépendances doivent suivre. Le venv porte la version (D123).
		return filepath.Join(PythonVenv(root, version), "bin", "python")
	case RuntimeGo:
		// Aucun gestionnaire : le déploiement dépose un binaire, et le lien
		// « app » du répertoire des liens permet de le remplacer atomiquement.
		return filepath.Join(root, "www", "app")
	}
	return ""
}

// LinkBundle est le nom du lien vers le lanceur de Ruby. Une unité n'invoque que
// des liens gérés (D30) : le CDC prévoit déjà ce pluriel pour Node (npm, npx).
const LinkBundle = "bundle"

// BundlePath renvoie le chemin de « bundle » d'une version de Ruby installée.
// bundler est une gem par défaut depuis Ruby 2.6 : rbenv le pose dans le même
// bin que l'interpréteur.
func BundlePath(root, version string) string {
	return filepath.Join(filepath.Dir(BinaryPath(root, RuntimeRuby, version)), LinkBundle)
}

// LinkName renvoie le nom du lien géré d'un runtime.
func LinkName(runtime string) string {
	if runtime == RuntimeGo {
		return "app"
	}
	return runtime
}

// LinkPath renvoie le chemin du lien géré.
func LinkPath(root, runtime string) string {
	return filepath.Join(model.BinDir(root), LinkName(runtime))
}

// Installed indique si la version demandée est présente.
func Installed(root, runtime, version string) bool {
	if CheckVersion(version) != nil {
		return false
	}
	if runtime == RuntimePython {
		venv := PythonVenv(root, version)
		rel, err := filepath.Rel(root, venv)
		return err == nil && venvUsable(root, rel)
	}
	path := BinaryPath(root, runtime, version)
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// CheckVersion refuse une version de runtime qui ne serait pas un simple nom :
// elle compose un chemin du home — ~/.venv-<version>, versions/<version> — et
// le script d'installation. « ../ » en sortirait, un blanc ou un « ; » y
// glisserait une commande. « + » reste admis : ruby-build nomme ainsi ses
// distributions (« truffleruby+graalvm-24.1.1 »). Le refus porte sur le plan et
// l'installation du projet visé, jamais sur le chargement.
func CheckVersion(version string) error {
	if version == "" || len(version) > 64 || !isVersionStart(version[0]) {
		return fmt.Errorf("version de runtime %q : un nom de version est attendu (« 22.11.0 », « 3.12.7 »)", version)
	}
	for i := 0; i < len(version); i++ {
		if c := version[i]; !isVersionStart(c) && c != '.' && c != '-' && c != '_' && c != '+' {
			return fmt.Errorf("version de runtime %q : caractère %q refusé (lettres, chiffres, « . », « - », "+
				"« _ », « + »)", version, c)
		}
	}
	return nil
}

func isVersionStart(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// venvPrefix nomme les venvs du projet, un par version de Python, à la racine
// du home (D123) : hors de www/, la copie d'un clone ne les emporte pas, et un
// changement de version ne réécrit jamais le venv en service.
const venvPrefix = ".venv-"

// legacyVenv est le venv d'avant D123, sans version dans son nom.
func legacyVenv(root string) string { return filepath.Join(root, "www", ".venv") }

// maxPyvenvCfg borne la relecture de pyvenv.cfg : il appartient au projet.
const maxPyvenvCfg = 64 << 10

// PythonVenv rend le venv qui sert une version de Python : ~/.venv-<version>
// s'il est utilisable — un répertoire réel, non un lien, qui porte son
// interpréteur — ; sinon l'ancien www/.venv s'il a été créé par l'interpréteur
// de cette version installé dans ce home — le recréer vide couperait
// l'application de ses dépendances (D123) ; sinon ~/.venv-<version>, à créer.
// Un ~/.venv-<version> inachevé ne passe donc jamais avant un venv qui
// fonctionne, et un www/.venv d'une autre version, ou recopié du home d'un autre
// projet par un clone d'avant D123, ne sert jamais.
func PythonVenv(root, version string) string {
	if venvUsable(root, venvPrefix+version) {
		return filepath.Join(root, venvPrefix+version)
	}
	if legacyVenvVersion(root) == version {
		return legacyVenv(root)
	}
	return filepath.Join(root, venvPrefix+version)
}

// venvUsable dit si root/rel est un venv utilisable : un répertoire réel,
// atteint sans suivre de lien, qui porte bin/python. Un lien à sa place n'en est
// pas un : la règle est celle de RetiredVenvs.
func venvUsable(root, rel string) bool {
	info, err := fs.LstatAt(root, rel)
	if err != nil || !info.IsDir() {
		return false
	}
	_, err = fs.LstatAt(root, filepath.Join(rel, "bin", "python"))
	return err == nil
}

// VenvLinkName est le lien de répertoire, à la racine du home, qui désigne le
// venv de la version déclarée (D123). L'unité d'une application Python lance
// <racine>/.venv/bin/python : Python cherche pyvenv.cfg à côté du chemin
// invoqué, sans résoudre un lien vers l'interpréteur — ~/.local/bin/python lance
// l'interpréteur de base, venv inactif, ses modules introuvables —, mais en
// traversant un lien de répertoire.
const VenvLinkName = model.VenvLinkName

// VenvPython rend l'interpréteur qu'invoque l'unité d'une application Python.
func VenvPython(root string) string { return filepath.Join(root, VenvLinkName, "bin", "python") }

// AppModule est le module que l'unité d'une application Python lance par
// « python -m » : un venv qui ne le porte pas ne peut pas la servir.
const AppModule = "gunicorn"

// VenvServes dit si le venv porte le module que lance l'unité — son lanceur
// bin/gunicorn, que pip pose avec lui. Un venv neuf est vide : ogs n'en installe
// pas les dépendances (§20).
func VenvServes(venv string) bool {
	_, err := os.Stat(filepath.Join(venv, "bin", AppModule))
	return err == nil
}

// ServingVenvs nomme les venvs du projet, autres que venv, qui peuvent servir
// l'application : un répertoire réel, atteint sans suivre de lien, qui porte son
// interpréteur et le module que lance l'unité — www/.venv quelle que soit sa
// version, et chaque ~/.venv-<v>. Un premier lien n'est posé vers un venv vide
// que si cette liste est vide : sinon le projet passerait d'un venv qui a ses
// dépendances — celui que désignait l'ancien .bin/python, ou le www/.venv d'une
// autre version — à un venv neuf qui n'a rien (D123 × D116).
func ServingVenvs(root, venv string) []string {
	var out []string
	consider := func(rel string) {
		path := filepath.Join(root, rel)
		if filepath.Clean(path) == filepath.Clean(venv) || !venvUsable(root, rel) {
			return
		}
		if _, err := fs.LstatAt(root, filepath.Join(rel, "bin", AppModule)); err != nil {
			return
		}
		// L'interpréteur doit se lancer : bin/python est un lien vers celui de
		// base, et un lien pendant — version retirée de pyenv, home d'un autre
		// projet disparu — ne sert plus rien. Seule son existence est lue.
		if _, err := os.Stat(filepath.Join(path, "bin", "python")); err != nil {
			return
		}
		out = append(out, path)
	}
	consider(filepath.Join("www", ".venv"))
	if dir, err := fs.OpenDir(root); err == nil {
		names, _ := dir.Names()
		_ = dir.Close()
		sort.Strings(names)
		for _, name := range names {
			if strings.HasPrefix(name, venvPrefix) {
				consider(name)
			}
		}
	}
	return out
}

// LinkNotReady dit ce qu'attend la réécriture d'une commande de l'ancien
// .bin/name vers son remplaçant (D116), "" si elle peut avoir lieu. Seul python
// d'un projet Python est concerné : son remplaçant est ~/.venv/bin/python
// (model.LegacyReplacement), et une tâche réécrite vers un venv qui n'a pas les
// dépendances de l'application — celui d'une version neuve, créé vide —
// perdrait l'ancien qui les avait. La réécriture attend donc que ~/.venv
// désigne un venv qui porte le module de l'unité, ou le venv même que
// désignait l'ancien .bin/python. Un ~/.venv absent n'est pas jugé ici : la
// migration attend déjà qu'il soit posé. Sous préfixe, rien n'est installé :
// l'assemblage ne câble pas ce contrôle, comme les contrôles des liens.
func LinkNotReady(proj *model.Project, name string) string {
	if proj.LegacyReplacement(name) != model.VenvPython {
		return ""
	}
	root := proj.Spec.Layout.Root
	kind, target, err := fs.LinkStateAt(root, ".", VenvLinkName)
	if err != nil || kind != fs.EntrySymlink {
		return ""
	}
	venv := filepath.Clean(target)
	if k, old, err := fs.LinkStateAt(root, model.LegacyLinkDir, name); err == nil && k == fs.EntrySymlink &&
		filepath.Dir(filepath.Dir(filepath.Clean(old))) == venv {
		return ""
	}
	if VenvServes(venv) {
		return ""
	}
	return fmt.Sprintf("les dépendances de l'application installées dans %s, que désigne %s : %s",
		venv, filepath.Join(root, VenvLinkName), PipInstall(proj, venv))
}

// ForeignLegacyVenv dit si www/.venv est un venv dont l'interpréteur vit hors
// de ce home : celui qu'un clone d'avant D123 a recopié de la production
// (R4-25). Il sert encore — son interpréteur se lance —, mais ce n'est pas celui
// du projet.
func ForeignLegacyVenv(root string) bool {
	return venvUsable(root, filepath.Join("www", ".venv")) && legacyVenvVersion(root) == ""
}

// PipInstall rend la commande qui installe les dépendances d'une application
// Python dans venv, sous le compte du projet : celle de requirements.txt s'il
// existe dans www/, sinon le module que lance l'unité.
func PipInstall(proj *model.Project, venv string) string {
	cmd := fmt.Sprintf("runuser -u %s -- %s -m pip install", proj.Spec.User.Name,
		filepath.Join(venv, "bin", "python"))
	if _, err := fs.LstatAt(proj.Spec.Layout.Root, filepath.Join("www", "requirements.txt")); err == nil {
		return cmd + " -r " + filepath.Join(proj.Spec.Layout.Root, "www", "requirements.txt")
	}
	return cmd + " " + AppModule + " (et les dépendances de l'application)"
}

// legacyVenvVersion rend la version de pyenv qui a créé www/.venv dans ce home,
// lue dans son pyvenv.cfg (« home = <root>/.pyenv/versions/<v>/bin »), vide si
// le venv manque, n'a pas d'interpréteur ou vient d'ailleurs. Tout est lu par
// descripteur, sans suivre de lien : le home appartient au projet (D82).
func legacyVenvVersion(root string) string {
	dir, err := fs.OpenDirAt(root, filepath.Join("www", ".venv"))
	if err != nil {
		return ""
	}
	defer dir.Close()
	data, _, err := dir.ReadFile("pyvenv.cfg", -1, maxPyvenvCfg)
	if err != nil {
		return ""
	}
	if _, err := fs.LstatAt(root, filepath.Join("www", ".venv", "bin", "python")); err != nil {
		return ""
	}
	versions := filepath.Join(managerDir(root, RuntimePython), "versions") + string(filepath.Separator)
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "home" {
			continue
		}
		home := filepath.Clean(strings.TrimSpace(value))
		rest, found := strings.CutPrefix(home, versions)
		if !found {
			return ""
		}
		version, bin, _ := strings.Cut(rest, string(filepath.Separator))
		if bin != "bin" || CheckVersion(version) != nil {
			return ""
		}
		return version
	}
	return ""
}

// RetiredVenvs nomme les venvs de Python que la déclaration ne désigne plus :
// ogs ne les retire pas — ils permettent de revenir à la version précédente,
// dépendances comprises —, doctor les signale (D123).
func RetiredVenvs(proj *model.Project) []string {
	at, err := proj.AppType()
	if err != nil || at.Runtime != RuntimePython {
		return nil
	}
	root := proj.Spec.Layout.Root
	// En service : celui de la version déclarée, et celui que désigne encore
	// ~/.venv — un changement de version retenu faute de dépendances (D123).
	current := map[string]bool{}
	if CheckVersion(proj.Spec.App.Runtime) == nil {
		current[PythonVenv(root, proj.Spec.App.Runtime)] = true
	}
	if kind, target, err := fs.LinkStateAt(root, ".", VenvLinkName); err == nil && kind == fs.EntrySymlink {
		current[filepath.Clean(target)] = true
	}
	var out []string
	if dir, err := fs.OpenDir(root); err == nil {
		names, _ := dir.Names()
		for _, name := range names {
			if !strings.HasPrefix(name, venvPrefix) {
				continue
			}
			if info, err := dir.Lstat(name); err != nil || !info.IsDir() {
				continue
			}
			if path := filepath.Join(root, name); !current[path] {
				out = append(out, path)
			}
		}
		_ = dir.Close()
	}
	if legacyVenvVersion(root) != "" && !current[legacyVenv(root)] {
		out = append(out, legacyVenv(root))
	}
	return out
}

// Install pose le runtime dans le home du projet. Les gestionnaires de versions
// étant des scripts shell conçus pour un shell interactif, ils sont invoqués par
// « /bin/bash -c » sous l'identité du projet, et jamais depuis une unité
// systemd (§7.11.2).
func (i *Installer) Install(ctx context.Context, proj *model.Project, runtime, version string) error {
	root := proj.Spec.Layout.Root
	if runtime != RuntimeGo && runtime != RuntimeCustom {
		if err := CheckVersion(version); err != nil {
			return err
		}
	}
	switch runtime {
	case RuntimeGo:
		return fmt.Errorf("le runtime go n'a pas de gestionnaire de versions : " +
			"le déploiement dépose un binaire, et « ~/.local/bin/app » pointe dessus")
	case RuntimeCustom:
		return fmt.Errorf("une application custom n'a pas de runtime géré : " +
			"ogs lance la commande déclarée dans spec.app.command")
	case RuntimeNode:
		script := fmt.Sprintf(
			`export NVM_DIR=%q; [ -s "$NVM_DIR/nvm.sh" ] || `+
				`{ curl -fsSL https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.1/install.sh | bash; }; `+
				`. "$NVM_DIR/nvm.sh"; nvm install %s`,
			managerDir(root, runtime), version)
		return i.runAs(ctx, proj, script)
	case RuntimeRuby:
		script := fmt.Sprintf(
			`export RBENV_ROOT=%q; [ -d "$RBENV_ROOT" ] || `+
				`git clone --depth 1 https://github.com/rbenv/rbenv.git "$RBENV_ROOT"; `+
				`[ -d "$RBENV_ROOT/plugins/ruby-build" ] || `+
				`git clone --depth 1 https://github.com/rbenv/ruby-build.git "$RBENV_ROOT/plugins/ruby-build"; `+
				`"$RBENV_ROOT/bin/rbenv" install -s %s`,
			managerDir(root, runtime), version)
		return i.runAs(ctx, proj, script)
	case RuntimePython:
		// pyenv fournit l'interpréteur, ogs crée le venv du projet : c'est lui que
		// le lien désigne, afin que les dépendances suivent. Un venv par version
		// (D123) : celui d'une autre version reste en place, pour un retour
		// arrière, et celui de cette version n'est jamais recréé par-dessus.
		venv := PythonVenv(root, version)
		script := fmt.Sprintf(
			`export PYENV_ROOT=%q; [ -d "$PYENV_ROOT" ] || `+
				`git clone --depth 1 https://github.com/pyenv/pyenv.git "$PYENV_ROOT"; `+
				`"$PYENV_ROOT/bin/pyenv" install -s %s && `+
				`{ [ ! -L %q ] || { echo "%s est un lien : ogs n'y crée pas de venv" >&2; exit 1; }; } && `+
				`{ [ -x %q ] || "$PYENV_ROOT/versions/%s/bin/python" -m venv %q; }`,
			managerDir(root, runtime), version, venv, venv, filepath.Join(venv, "bin", "python"), version, venv)
		return i.runAs(ctx, proj, script)
	}
	return fmt.Errorf("runtime %q inconnu (gérés : %s)", runtime, strings.Join(Runtimes(), ", "))
}

// runAs exécute un script sous l'identité du projet, par bash nommé
// explicitement : le compte peut avoir un shell fermé (D62), et
// exec.Command.AsUser n'en emploie aucun (D122). Le répertoire courant est la
// racine du projet, comme pour ses tâches.
func (i *Installer) runAs(ctx context.Context, proj *model.Project, script string) error {
	if i.Exec == nil || i.Exec.Simulated() {
		return nil
	}
	_, err := i.Exec.Run(ctx, exec.Command{
		Name: "/bin/bash", Args: []string{"-c", script},
		AsUser: proj.Spec.User.Name, Dir: proj.Spec.Layout.Root,
		// Le téléchargement suit le mandataire d'ogs ; lui seul le reçoit (D122).
		Env: exec.ProxyEnv(),
	})
	return err
}

// Version interroge le binaire lié pour connaître la version réellement en place.
// Le lien vit dans ~/.local/bin, qui appartient à l'utilisateur (D70) : il en
// change la cible à volonté, et la version effective doit donc être vérifiée, non
// supposée (§7.11.2). Pour la même raison, le binaire est lancé sous l'identité
// du projet, jamais en root : exécuter en root ce que désigne un lien de
// l'utilisateur, c'est lui laisser choisir ce que root exécute.
func (i *Installer) Version(ctx context.Context, proj *model.Project, runtime string) (string, error) {
	link := LinkPath(proj.Spec.Layout.Root, runtime)
	if _, err := os.Lstat(link); err != nil {
		return "", fmt.Errorf("aucun lien %s", link)
	}
	if i.Exec == nil || i.Exec.Simulated() {
		return "", nil
	}
	res, err := i.Exec.Run(ctx, exec.Command{Name: link, Args: []string{"--version"}, AsUser: proj.Spec.User.Name})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.Fields(res.Stdout + " ")[0], "v")), nil
}
