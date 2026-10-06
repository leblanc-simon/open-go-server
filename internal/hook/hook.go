// Package hook exécute les scripts utilisateur aux points prévus du cycle de vie.
package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
)

// Événements du cycle de vie.
const (
	PreCreate     = "preCreate"
	PostCreate    = "postCreate"
	PreDelete     = "preDelete"
	PostDelete    = "postDelete"
	PreApply      = "preApply"
	PostApply     = "postApply"
	PostCertRenew = "postCertRenew"
	PostPHPSwitch = "postPhpSwitch"
)

// Events énumère les événements connus.
func Events() []string {
	return []string{PreCreate, PostCreate, PreDelete, PostDelete,
		PreApply, PostApply, PostCertRenew, PostPHPSwitch}
}

// Critical indique si l'échec d'un hook doit interrompre l'opération. Les hooks
// « pre » gardent la porte : leur refus doit arrêter ce qui suit. Les hooks
// « post » constatent, et leur échec est un avertissement.
func Critical(event string) bool {
	switch event {
	case PreCreate, PreDelete, PreApply:
		return true
	}
	return false
}

// DefaultTimeout borne l'exécution : un hook qui ne rend pas la main bloquerait
// l'outil, verrou compris.
const DefaultTimeout = 5 * time.Minute

// Runner exécute les hooks.
type Runner struct {
	Exec exec.Executor
	// Dir est le répertoire autorisé : /etc/ogs/hooks.
	Dir string
	// Timeout borne chaque exécution.
	Timeout time.Duration
	// Disabled coupe tous les hooks (--no-hooks).
	Disabled bool
	// DryRun résout le hook et l'annonce sans l'exécuter : aucun hook en
	// --dry-run (D40). Les commandes qui simulent s'arrêtent avant leurs hooks, et
	// les autres refusent l'option ; ce court-circuit tient la règle même pour un
	// appelant qui l'oublierait.
	DryRun bool
	// RequireRootOwner exige que le script appartienne à root. Le contrôle protège
	// contre une escalade vers root : hors root, il n'y a rien à escalader, et
	// l'imposer empêcherait tout essai en mode préfixé.
	RequireRootOwner bool
	// Log reçoit la sortie du hook.
	Log func(format string, args ...any)
}

// Context est ce que le hook reçoit sur son entrée standard.
type Context struct {
	Event     string            `json:"event"`
	Project   string            `json:"project"`
	Host      string            `json:"host"`
	Env       string            `json:"env"`
	Kind      string            `json:"kind,omitempty"`
	User      string            `json:"user,omitempty"`
	PHP       string            `json:"php,omitempty"`
	Root      string            `json:"root"`
	Docroot   string            `json:"docroot,omitempty"`
	Resources []string          `json:"resources,omitempty"`
	Extra     map[string]string `json:"extra,omitempty"`
}

// environ traduit le contexte en variables d'environnement, pour qu'un script
// court n'ait pas à analyser du JSON. Les valeurs supplémentaires deviennent des
// OGS_<CLÉ>, ce qui évite d'avoir à étendre cette liste à chaque événement.
func (c Context) environ() []string {
	env := []string{
		"OGS_EVENT=" + c.Event,
		"OGS_PROJECT=" + c.Project,
		"OGS_HOST=" + c.Host,
		"OGS_ENV=" + c.Env,
		"OGS_KIND=" + c.Kind,
		"OGS_USER=" + c.User,
		"OGS_PHP=" + c.PHP,
		"OGS_ROOT=" + c.Root,
		"OGS_DOCROOT=" + c.Docroot,
		"OGS_RESOURCES=" + strings.Join(c.Resources, "\n"),
	}
	for k, v := range c.Extra {
		env = append(env, "OGS_"+strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(k))+"="+v)
	}
	sort.Strings(env)
	return env
}

// kebab traduit « postCertRenew » en « post-cert-renew ».
func kebab(event string) string {
	var b strings.Builder
	for i, r := range event {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('-')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Resolve renvoie le script associé à un événement, ou une chaîne vide s'il n'y
// en a pas. Le chemin est validé : un hook ne peut être désigné qu'au sein du
// répertoire prévu, et doit appartenir à root sans être inscriptible par autrui.
//
// Deux origines, par ordre de priorité : le script déclaré par le projet, puis
// celui de portée serveur — un fichier du répertoire des hooks portant le nom de
// l'événement. Un projet peut donc raccorder son propre traitement sans priver
// le parc du sien, mais aussi le remplacer.
//
// Le chemin rendu est le chemin réel, liens résolus ; le hook est lancé, lui,
// par le nom désigné (Resolution.Path).
func (r *Runner) Resolve(proj *model.Project, event string) (string, error) {
	res, err := r.inspect(proj, event, true)
	return res.Real, err
}

// Resolution décrit un hook résolu : le chemin désigné sous le répertoire des
// hooks, et le fichier réel qu'il désigne, liens suivis.
type Resolution struct {
	Path string
	Real string
}

// Outside dit si le fichier réel vit hors du répertoire des hooks : un lien qui
// en sort (D117).
func (r Resolution) Outside(dir string) bool {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return r.Real != "" && !under(dir, r.Real)
}

// Inspect résout le hook d'un événement comme Resolve, en rendant aussi le
// chemin désigné : « doctor » signale ainsi les liens qui sortent du répertoire.
func (r *Runner) Inspect(proj *model.Project, event string) (Resolution, error) {
	return r.inspect(proj, event, true)
}

// declared rend le script que déclare le projet, sans repli sur celui du parc.
func declared(proj *model.Project, event string) string {
	if proj != nil && proj.Spec.Hooks != nil {
		return proj.Spec.Hooks[event]
	}
	return ""
}

func under(dir, path string) bool {
	if dir == "" {
		return true
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (r *Runner) inspect(proj *model.Project, event string, fleet bool) (Resolution, error) {
	if r.Disabled {
		return Resolution{}, nil
	}
	script := declared(proj, event)
	if script == "" && fleet {
		// Le hook de parc n'est pas déclaré : sa seule présence vaut déclaration,
		// et son absence n'est pas une erreur. Les deux orthographes sont admises,
		// celle de la clé YAML et celle qui se lit mieux sur un disque.
		for _, name := range []string{event, kebab(event)} {
			candidate := filepath.Join(r.Dir, name)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				script = candidate
				break
			}
		}
	}
	if script == "" {
		return Resolution{}, nil
	}
	if !filepath.IsAbs(script) {
		script = filepath.Join(r.Dir, script)
	}
	clean := filepath.Clean(script)
	if !under(r.Dir, clean) {
		return Resolution{}, fmt.Errorf("hook %s : le script doit se trouver sous %s (obtenu %q)",
			event, r.Dir, script)
	}
	res := Resolution{Path: clean, Real: clean}
	if r.RequireRootOwner {
		// Un lien posé sous le répertoire des hooks vers un fichier de root hors
		// de lui fonctionnait, et des parcs en dépendent : il reste admis tant que
		// seul root peut changer ce qui est exécuté — chaque répertoire traversé,
		// lien compris, à root et fermé en écriture au groupe et aux autres
		// (D117). Le contrôle portait sur le seul chemin nettoyé, et os.Stat
		// suivait le lien jusque dans un répertoire qu'un projet possède.
		real, err := trustedPath(clean)
		if err != nil {
			return Resolution{}, fmt.Errorf("hook %s : %s — %w", event, clean, err)
		}
		res.Real = real
	} else if real, err := filepath.EvalSymlinks(clean); err == nil {
		// Hors root, aucune chaîne à éprouver : le chemin réel sert seulement à
		// dire si le hook sort du répertoire.
		res.Real = real
	}
	info, err := os.Stat(res.Real)
	if err != nil {
		return Resolution{}, fmt.Errorf("hook %s : %w", event, err)
	}
	if info.IsDir() {
		return Resolution{}, fmt.Errorf("hook %s : %s est un répertoire", event, res.Real)
	}
	// Un hook s'exécute en root : s'il est inscriptible par un autre compte, ce
	// compte obtient une exécution privilégiée.
	if info.Mode().Perm()&0o022 != 0 {
		return Resolution{}, fmt.Errorf("hook %s : %s est inscriptible au-delà de son propriétaire "+
			"(mode %04o) — corriger avec chmod 0750", event, res.Real, info.Mode().Perm())
	}
	if r.RequireRootOwner {
		if err := ownedByRoot(info); err != nil {
			return Resolution{}, fmt.Errorf("hook %s : %s %w", event, res.Real, err)
		}
	}
	if info.Mode().Perm()&0o100 == 0 {
		return Resolution{}, fmt.Errorf("hook %s : %s n'est pas exécutable", event, res.Real)
	}
	return res, nil
}

// Run exécute le hook associé à un événement. Le contexte est passé en JSON sur
// l'entrée standard, et les principales valeurs aussi par l'environnement, pour
// qu'un script court n'ait pas à analyser du JSON.
func (r *Runner) Run(ctx context.Context, proj *model.Project, event string, hookCtx Context) error {
	return r.run(ctx, proj, event, hookCtx, true)
}

// RunDeclared exécute le seul hook que déclare le projet, sans repli sur celui
// du parc. « postCertRenew » en a besoin (D117) : le hook de parc y est appelé
// une fois pour tous les certificats changés, et celui d'un projet en plus,
// pour les siens.
func (r *Runner) RunDeclared(ctx context.Context, proj *model.Project, event string, hookCtx Context) error {
	return r.run(ctx, proj, event, hookCtx, false)
}

func (r *Runner) run(ctx context.Context, proj *model.Project, event string, hookCtx Context, fleet bool) error {
	resolved, err := r.inspect(proj, event, fleet)
	if err != nil {
		return err
	}
	if resolved.Real == "" {
		return nil
	}
	// Le hook est lancé par le nom désigné, non par son chemin réel : un
	// répartiteur lié sous plusieurs noms d'événement lit « basename "$0" ».
	// Le contrôle ne se périme pas pour autant : chaque maillon de la chaîne
	// n'est modifiable que par root (D117).
	script := resolved.Path
	if r.DryRun {
		if r.Log != nil {
			r.Log("  ↪ hook %s : %s — non exécuté (--dry-run)\n", event, script)
		}
		return nil
	}
	hookCtx.Event = event
	if proj != nil {
		hookCtx.Project = proj.ID()
		hookCtx.Host = proj.Metadata.Host
		hookCtx.Env = proj.Metadata.Env
		hookCtx.Root = proj.Spec.Layout.Root
		hookCtx.Docroot = proj.Spec.Layout.Docroot
		hookCtx.Kind = string(proj.Spec.App.Kind)
		hookCtx.User = proj.Spec.User.Name
		if len(proj.Spec.PHP.Pools) > 0 {
			hookCtx.PHP = proj.Spec.PHP.Pools[0].Version
		}
	}
	payload, err := json.Marshal(hookCtx)
	if err != nil {
		return err
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	env := append(os.Environ(), hookCtx.environ()...)
	if r.Log != nil {
		r.Log("  ↪ hook %s : %s\n", event, script)
	}
	res, err := r.Exec.Run(runCtx, exec.Command{Name: script, Env: env, Stdin: payload})
	if r.Log != nil {
		for _, line := range strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n") {
			if line != "" {
				r.Log("     %s\n", line)
			}
		}
	}
	if err != nil {
		if runCtx.Err() != nil {
			return fmt.Errorf("hook %s : interrompu après %s (le script doit rendre la main)",
				event, timeout)
		}
		return fmt.Errorf("hook %s : %w", event, err)
	}
	return nil
}
