// Package app porte les applications non-PHP : runtimes, processus supervisés et
// point d'écoute.
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/leviathan/ogs/internal/exec"
	"github.com/leviathan/ogs/internal/model"
)

// Runtimes gérés. Go n'en a pas : un binaire compilé est autonome.
const (
	RuntimeGo     = "go"
	RuntimeNode   = "node"
	RuntimePython = "python"
	RuntimeRuby   = "ruby"
)

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
		// dépendances doivent suivre.
		return filepath.Join(root, "www", ".venv", "bin", "python")
	case RuntimeGo:
		// Aucun gestionnaire : le déploiement dépose un binaire, et le lien
		// .bin/app permet de le remplacer atomiquement.
		return filepath.Join(root, "www", "app")
	}
	return ""
}

// LinkName renvoie le nom du lien dans .bin pour un runtime.
func LinkName(runtime string) string {
	if runtime == RuntimeGo {
		return "app"
	}
	return runtime
}

// LinkPath renvoie le chemin du lien géré.
func LinkPath(root, runtime string) string {
	return filepath.Join(root, ".bin", LinkName(runtime))
}

// Installed indique si la version demandée est présente.
func Installed(root, runtime, version string) bool {
	path := BinaryPath(root, runtime, version)
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// Install pose le runtime dans le home du projet. Les gestionnaires de versions
// étant des scripts shell conçus pour un shell interactif, ils sont invoqués par
// « su -s /bin/bash », et jamais depuis une unité systemd (§7.11.2).
func (i *Installer) Install(ctx context.Context, proj *model.Project, runtime, version string) error {
	root := proj.Spec.Layout.Root
	user := proj.Spec.User.Name
	switch runtime {
	case RuntimeGo:
		return fmt.Errorf("le runtime go n'a pas de gestionnaire de versions : " +
			"le déploiement dépose un binaire, et « .bin/app » pointe dessus")
	case RuntimeNode:
		script := fmt.Sprintf(
			`export NVM_DIR=%q; [ -s "$NVM_DIR/nvm.sh" ] || `+
				`{ curl -fsSL https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.1/install.sh | bash; }; `+
				`. "$NVM_DIR/nvm.sh"; nvm install %s`,
			managerDir(root, runtime), version)
		return i.runAs(ctx, user, script)
	case RuntimeRuby:
		script := fmt.Sprintf(
			`export RBENV_ROOT=%q; [ -d "$RBENV_ROOT" ] || `+
				`git clone --depth 1 https://github.com/rbenv/rbenv.git "$RBENV_ROOT"; `+
				`[ -d "$RBENV_ROOT/plugins/ruby-build" ] || `+
				`git clone --depth 1 https://github.com/rbenv/ruby-build.git "$RBENV_ROOT/plugins/ruby-build"; `+
				`"$RBENV_ROOT/bin/rbenv" install -s %s`,
			managerDir(root, runtime), version)
		return i.runAs(ctx, user, script)
	case RuntimePython:
		// pyenv fournit l'interpréteur, ogs crée le venv du projet : c'est lui que
		// le lien désigne, afin que les dépendances suivent.
		script := fmt.Sprintf(
			`export PYENV_ROOT=%q; [ -d "$PYENV_ROOT" ] || `+
				`git clone --depth 1 https://github.com/pyenv/pyenv.git "$PYENV_ROOT"; `+
				`"$PYENV_ROOT/bin/pyenv" install -s %s; `+
				`"$PYENV_ROOT/versions/%s/bin/python" -m venv %q`,
			managerDir(root, runtime), version, version, filepath.Join(root, "www", ".venv"))
		return i.runAs(ctx, user, script)
	}
	return fmt.Errorf("runtime %q inconnu (gérés : %s)", runtime, strings.Join(Runtimes(), ", "))
}

// runAs exécute un script sous l'identité du projet. « su -s » force le shell :
// l'utilisateur du projet a nologin, et un su ordinaire échouerait.
func (i *Installer) runAs(ctx context.Context, user, script string) error {
	if i.Exec == nil || i.Exec.Simulated() {
		return nil
	}
	_, err := i.Exec.Run(ctx, exec.Command{
		Name: "su", Args: []string{"-s", "/bin/bash", "-c", script, user},
	})
	return err
}

// Version interroge le binaire lié pour connaître la version réellement en place.
// Le lien appartenant à root mais pointant dans le home de l'utilisateur, celui-ci
// peut en changer la cible : la version effective doit donc être vérifiée, non
// supposée (§7.11.2).
func (i *Installer) Version(ctx context.Context, proj *model.Project, runtime string) (string, error) {
	link := LinkPath(proj.Spec.Layout.Root, runtime)
	if _, err := os.Lstat(link); err != nil {
		return "", fmt.Errorf("aucun lien %s", link)
	}
	if i.Exec == nil || i.Exec.Simulated() {
		return "", nil
	}
	res, err := i.Exec.Run(ctx, exec.Command{Name: link, Args: []string{"--version"}})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.Fields(res.Stdout + " ")[0], "v")), nil
}
