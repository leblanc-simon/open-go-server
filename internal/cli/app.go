package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/goccy/go-yaml"

	"github.com/leviathan/ogs/internal/audit"
	"github.com/leviathan/ogs/internal/config"
	"github.com/leviathan/ogs/internal/exec"
	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/naming"
	"github.com/leviathan/ogs/internal/provider/db"
	"github.com/leviathan/ogs/internal/provider/logrotate"
	"github.com/leviathan/ogs/internal/provider/php"
	"github.com/leviathan/ogs/internal/provider/system"
	"github.com/leviathan/ogs/internal/provider/web"
	"github.com/leviathan/ogs/internal/secret"
	"github.com/leviathan/ogs/internal/spec"
	"github.com/leviathan/ogs/internal/store"
	"github.com/leviathan/ogs/internal/template"
	"github.com/leviathan/ogs/internal/validate"
)

// App porte le contexte d'exécution partagé par les commandes.
type App struct {
	Cfg   *config.Config
	Paths config.Paths
	Exec  exec.Executor
	Store *store.Store
	Keys  *secret.Keystore
	Audit *audit.Logger

	Out io.Writer
	Err io.Writer

	// Options globales.
	Prefix         string
	Format         string
	Yes            bool
	DryRun         bool
	Force          bool
	NonInteractive bool
	Quiet          bool
	Verbose        int
	NoHooks        bool
}

// newApp construit le contexte, sans ouvrir la base : les commandes qui en ont
// besoin appellent openStore.
func newApp() *App {
	return &App{
		Cfg:    config.Default(),
		Paths:  config.NewPaths(""),
		Exec:   &exec.Local{},
		Out:    os.Stdout,
		Err:    os.Stderr,
		Format: "table",
	}
}

// setup applique les options globales : préfixe, configuration, root-strict.
func (a *App) setup() error {
	a.Paths = config.NewPaths(a.Prefix)
	if err := a.loadConfig(); err != nil {
		return err
	}
	if a.Prefix == "" {
		// Root strict (D21) : toute opération d'administration exige root. Le mode
		// préfixé est la seule dérogation, réservée aux tests et au développement.
		if os.Getuid() != 0 {
			return exitf(CodePermission,
				"ogs doit être exécuté en tant que root ; pour un essai sans privilèges, "+
					"utiliser --prefix <répertoire>")
		}
	} else {
		// Mode préfixé : aucune commande système n'est exécutée. Les comptes ne sont
		// pas créés, les répertoires le sont sous le préfixe. C'est un mode d'essai
		// et de développement, pas un mode d'exploitation.
		a.Exec = exec.NewRecorder()
		if !a.Quiet && a.Format == "table" {
			fmt.Fprintf(a.Err, "mode préfixé %s : les commandes système sont simulées\n", a.Prefix)
		}
	}
	switch a.Format {
	case "table", "json", "yaml":
	default:
		return exitf(CodeUsage, "format de sortie %q inconnu (table, json ou yaml)", a.Format)
	}
	return nil
}

func (a *App) loadConfig() error {
	data, err := os.ReadFile(a.Paths.ConfigFile())
	if err != nil {
		if os.IsNotExist(err) {
			a.Cfg = config.Default()
			return nil
		}
		return wrap(CodeError, err)
	}
	cfg := config.Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return exitf(CodeValidation, "%s : %v", a.Paths.ConfigFile(), err)
	}
	if err := cfg.Validate(); err != nil {
		return exitf(CodeValidation, "%s : %v", a.Paths.ConfigFile(), err)
	}
	a.Cfg = cfg
	return nil
}

// openStore ouvre la base d'état et le keystore.
func (a *App) openStore() error {
	if a.Store != nil {
		return nil
	}
	st, err := store.Open(a.Paths.StateDB())
	if err != nil {
		return wrap(CodeError, err)
	}
	a.Store = st
	if ks, err := secret.Open(a.Paths.MasterKey()); err == nil {
		a.Keys = ks
	}
	lg, err := audit.New(st, a.Paths.AuditLog(), "", "")
	if err != nil {
		return wrap(CodeError, err)
	}
	a.Audit = lg
	return nil
}

// close libère les ressources ouvertes.
func (a *App) close() {
	if a.Audit != nil {
		_ = a.Audit.Close()
	}
	if a.Store != nil {
		_ = a.Store.Close()
	}
}

// loadProject charge un projet par identifiant, normalise et valide.
func (a *App) loadProject(id string) (*model.Project, error) {
	p, err := spec.Load(a.Paths.ProjectFile(id))
	if err != nil {
		return nil, wrap(CodeUsage, err)
	}
	if err := a.prepare(p); err != nil {
		return nil, err
	}
	return p, nil
}

// loadProjects charge tous les projets actifs.
func (a *App) loadProjects() ([]*model.Project, error) {
	list, err := spec.LoadAll(a.Paths.ProjectsDir())
	if err != nil {
		return nil, wrap(CodeError, err)
	}
	for _, p := range list {
		if err := a.prepare(p); err != nil {
			return nil, err
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID() < list[j].ID() })
	return list, nil
}

// prepare normalise puis valide un projet.
func (a *App) prepare(p *model.Project) error {
	homeRoot := a.homeRoot()
	if err := p.Normalize(homeRoot, a.Cfg.PrepDomain()); err != nil {
		return exitf(CodeValidation, "%s", err)
	}
	if err := validate.Project(p, homeRoot); err != nil {
		return exitf(CodeValidation, "projet %s :\n%v", p.ID(), err)
	}
	return nil
}

// homeRoot renvoie la racine des homes, préfixe appliqué.
func (a *App) homeRoot() string {
	if a.Prefix == "" {
		return a.Cfg.Naming.HomeRoot
	}
	return a.Paths.Home(a.Cfg, "")
}

// providers assemble les providers actifs pour cette exécution.
type providers struct {
	System    *system.Provider
	DB        *db.Provider
	PHP       *php.Provider
	Web       *web.Provider
	Logrotate *logrotate.Provider
	Engine    web.Engine
}

// close libère les connexions ouvertes par les providers.
func (p *providers) close() {
	if p != nil && p.DB != nil {
		p.DB.Close()
	}
}

// buildProviders construit les providers une fois par exécution : la détection
// des versions PHP et du moteur web ne doit pas être refaite à chaque projet.
func (a *App) buildProviders() (*providers, error) {
	tmpl := template.New(a.Paths.TemplatesDir())
	sys := a.systemProvider()
	sys.Paths = a.Paths
	php := &php.Provider{
		Paths:     a.Paths,
		Exec:      a.Exec,
		Templates: tmpl,
		Store:     a.Store,
	}
	eng, err := a.webEngine()
	if err != nil {
		return nil, err
	}
	return &providers{
		System: sys,
		DB:     &db.Provider{Paths: a.Paths, Exec: a.Exec, Store: a.Store, Keys: a.Keys},
		PHP:    php,
		Web: &web.Provider{
			Paths: a.Paths, Exec: a.Exec, Templates: tmpl, Engine: eng, PHP: php,
		},
		Logrotate: &logrotate.Provider{Paths: a.Paths, Templates: tmpl, Config: a.Cfg},
		Engine:    eng,
	}, nil
}

// webEngine choisit le moteur web. En mode préfixé, l'arborescence du moteur est
// créée à la demande : le mode d'essai doit fonctionner sur une machine où aucun
// serveur web n'est installé.
func (a *App) webEngine() (web.Engine, error) {
	requested := a.Cfg.Web.Engine
	eng, err := web.DetectEngine(requested, a.Prefix)
	if err == nil {
		return eng, nil
	}
	if a.Prefix == "" {
		return nil, exitf(CodeDependency, "%s", err)
	}
	if requested == "" {
		requested = "apache"
	}
	eng, nerr := web.NewEngine(requested, a.Prefix)
	if nerr != nil {
		return nil, exitf(CodeUsage, "%s", nerr)
	}
	for _, d := range []string{eng.AvailableDir(), eng.EnabledDir()} {
		if mkErr := os.MkdirAll(d, 0o755); mkErr != nil {
			return nil, wrap(CodeError, mkErr)
		}
	}
	return eng, nil
}

// systemProvider construit le provider système.
func (a *App) systemProvider() *system.Provider {
	ident := &system.SystemIdentity{}
	if a.Prefix != "" {
		// En mode préfixé, la base des comptes est lue sous le préfixe : elle peut
		// être peuplée à la main pour éprouver l'idempotence sans root.
		ident.PasswdPath = filepath.Join(a.Prefix, "etc", "passwd")
		ident.GroupPath = filepath.Join(a.Prefix, "etc", "group")
	}
	prov := &system.Provider{Exec: a.Exec, Identity: ident}
	if a.Prefix != "" {
		prov.SkipWebUser = true
	} else if u, err := ident.LookupUser(system.WebUser); err == nil && u == nil {
		// Sans serveur web installé, l'appartenance au groupe n'a pas d'objet.
		prov.SkipWebUser = true
	}
	return prov
}

// projectEnv renvoie l'environnement demandé, validé.
func parseEnv(v string) (naming.Env, error) {
	env, err := naming.ParseEnv(v)
	if err != nil {
		return "", exitf(CodeUsage, "%s", err)
	}
	return env, nil
}
