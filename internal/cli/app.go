package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"leblanc.io/open-go-server/internal/audit"
	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/hook"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/provider/amqp"
	appprov "leblanc.io/open-go-server/internal/provider/app"
	"leblanc.io/open-go-server/internal/provider/cron"
	"leblanc.io/open-go-server/internal/provider/db"
	"leblanc.io/open-go-server/internal/provider/firewall"
	"leblanc.io/open-go-server/internal/provider/logrotate"
	"leblanc.io/open-go-server/internal/provider/mail"
	"leblanc.io/open-go-server/internal/provider/monitoring"
	"leblanc.io/open-go-server/internal/provider/php"
	"leblanc.io/open-go-server/internal/provider/runtime"
	"leblanc.io/open-go-server/internal/provider/sudoers"
	"leblanc.io/open-go-server/internal/provider/system"
	tlsprov "leblanc.io/open-go-server/internal/provider/tls"
	"leblanc.io/open-go-server/internal/provider/unit"
	"leblanc.io/open-go-server/internal/provider/web"
	"leblanc.io/open-go-server/internal/secret"
	"leblanc.io/open-go-server/internal/spec"
	"leblanc.io/open-go-server/internal/store"
	"leblanc.io/open-go-server/internal/template"
	"leblanc.io/open-go-server/internal/validate"
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
	// RecreateQueues autorise la recréation d'une queue dont les arguments
	// immuables ont changé. Cette opération détruit les messages en attente.
	RecreateQueues bool
	// allowNonRoot lève l'exigence de root pour la surface déléguée « ogs my ».
	allowNonRoot bool
	// cfgUnknown et cfgRetired sont les clés de config.yaml que Config ne
	// connaît pas, et celles qu'une version antérieure écrivait (D101).
	cfgUnknown, cfgRetired []string
	// amqpWarned retient que l'avertissement sur amqp.yaml a été donné.
	amqpWarned bool
	// holdServices fait différer par le plan d'un projet ce qui exécute son
	// code — démarrage des instances, attente de disponibilité, tâches
	// planifiées — et par applyProjects le hook « postCreate » : la restauration
	// remet le home et les bases avant de les lancer (D113).
	holdServices bool
	// postCreateRan dit, pour un projet, que son « postCreate » a déjà été lancé
	// — une restauration reprise ne le relance pas —, et onPostCreate le fait
	// noter dès qu'il l'est (D113).
	postCreateRan func(id string) bool
	onPostCreate  func(id string)
}

// nowFunc est isolé pour rester substituable en test.
var nowFunc = time.Now

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

// newPrefixRecorder fournit l'exécuteur simulé du mode préfixé. Les essais le
// remplacent pour donner aux commandes simulées la sortie d'un vrai système.
var newPrefixRecorder = exec.NewRecorder

// setup applique les options globales : préfixe, configuration, root-strict.
func (a *App) setup() error {
	a.Paths = config.NewPaths(a.Prefix)
	if err := a.loadConfig(); err != nil {
		return err
	}
	if a.Prefix == "" {
		// Root strict (D21) : toute opération d'administration exige root. Deux
		// dérogations seulement : le mode préfixé, réservé aux tests, et la surface
		// déléguée « ogs my », dont la portée est bornée à un projet (D28).
		if os.Getuid() != 0 && !a.allowNonRoot {
			return exitf(CodePermission,
				"ogs doit être exécuté en tant que root ; pour un essai sans privilèges, "+
					"utiliser --prefix <répertoire>")
		}
	} else {
		// Mode préfixé : aucune commande système n'est exécutée. Les comptes ne sont
		// pas créés, les répertoires le sont sous le préfixe. C'est un mode d'essai
		// et de développement, pas un mode d'exploitation.
		a.Exec = newPrefixRecorder()
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
	// Les clés inconnues ne sont pas jugées ici : un refus au chargement
	// arrêterait tout le parc, le renouvellement des certificats compris.
	// configKeysPolicy décide, commande par commande (D101).
	if unknown, retired, err := config.UnknownKeys(data); err == nil {
		a.cfgUnknown, a.cfgRetired = unknown, retired
	}
	if err := cfg.Validate(); err != nil {
		return exitf(CodeValidation, "%s : %v", a.Paths.ConfigFile(), err)
	}
	a.Cfg = cfg
	return nil
}

// configTolerant énumère les commandes qui écrivent et que des clés inconnues
// de config.yaml n'arrêtent pas, au-delà des consultations : le renouvellement
// des certificats, que le timer lance sans personne pour lire un refus ;
// « init », que le paquet relance à chaque mise à jour pour migrer la base
// d'état ; et ce qui ne fait que démarrer ou arrêter des services, sans rien
// calculer de la configuration.
var configTolerant = map[string]bool{
	"ogs cert renew":      true,
	"ogs init":            true,
	"ogs my restart":      true,
	"ogs my start":        true,
	"ogs my stop":         true,
	"ogs service restart": true,
	"ogs service start":   true,
	"ogs service stop":    true,
}

// configKeysPolicy décide du sort d'une commande quand config.yaml porte des
// clés inconnues (D101) : elles ne se taisent jamais. Une commande qui écrit
// est refusée en code 3 — elle appliquerait un réglage que l'administrateur
// croit avoir posé ; une consultation, et ce que configTolerant énumère,
// l'annoncent sur la sortie d'erreur — hors du fil de la commande, qu'un
// script lit peut-être — et continuent ; doctor la compte en échec
// (configChecks). Une clé retirée depuis la version qui l'écrivait est nommée,
// jamais refusée.
func (a *App) configKeysPolicy(cmd *cobra.Command) error {
	path := a.Paths.ConfigFile()
	doctor := cmd.CommandPath() == "ogs doctor"
	if len(a.cfgRetired) > 0 && !doctor {
		fmt.Fprintf(a.Err, "⚠ %s : clé(s) retirée(s), sans effet, à supprimer : %s\n", path,
			strings.Join(a.cfgRetired, ", "))
	}
	err := config.UnknownKeysError(path, a.cfgUnknown)
	switch {
	case err == nil || doctor:
		return nil
	case dryRunConductOf(cmd) == dryRunReadOnly || configTolerant[cmd.CommandPath()],
		// « ogs » seul hors d'un terminal n'affiche que l'aide ; sur un terminal,
		// il ouvre l'inspecteur, où l'on peut appliquer — refusé comme « ogs tui ».
		!cmd.HasParent() && !term.IsTerminal(int(os.Stdout.Fd())):
		fmt.Fprintf(a.Err, "⚠ %v — les commandes qui écrivent la refusent (D101)\n", err)
		return nil
	}
	return exitf(CodeValidation, "%v — corriger le fichier : une commande qui écrit ne s'exécute pas "+
		"sur un réglage perdu (D101)", err)
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

// hookRunner construit l'exécuteur de hooks. Il est isolé pour les commandes qui
// n'ont pas besoin de l'attelage complet des providers.
func (a *App) hookRunner() *hook.Runner {
	return &hook.Runner{
		Exec: a.Exec, Dir: a.Paths.HooksDir(), Disabled: a.NoHooks, DryRun: a.DryRun,
		// Hors root, il n'y a rien à escalader : l'exiger interdirait tout essai.
		RequireRootOwner: os.Getuid() == 0 && a.Prefix == "",
		Log:              a.printf,
	}
}

// runHook exécute un hook et applique la politique de l'événement, tenue en un
// seul endroit : l'échec d'un hook « pre » interrompt l'opération, celui d'un
// hook « post » n'est qu'un avertissement — ce qui est fait est fait. Le booléen
// dit si un avertissement a été émis, pour que l'appelant le comptabilise.
func (a *App) runHook(ctx context.Context, r *hook.Runner, p *model.Project,
	event string, hc hook.Context) (bool, error) {
	if err := r.Run(ctx, p, event, hc); err != nil {
		if hook.Critical(event) {
			return false, exitf(CodeError, "%v", err)
		}
		a.warnf("⚠ %v\n", err)
		return true, nil
	}
	return false, nil
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
	// Le shell configuré est un défaut de création : il ne s'applique qu'à un
	// projet qui n'en déclare pas. Le fichier d'un projet existant porte déjà sa
	// valeur, et c'est lui qui fait autorité — sans quoi un changement de
	// configuration réécrirait le shell de tout le parc au prochain apply.
	if p.Spec.User.Shell == "" {
		p.Spec.User.Shell = a.Cfg.Shell()
	}
	logsRoot := a.logsRoot()
	if err := p.Normalize(homeRoot, a.Cfg.PrepDomain(), model.WithLogsRoot(logsRoot)); err != nil {
		return exitf(CodeValidation, "%s", err)
	}
	if err := validate.Project(p, homeRoot); err != nil {
		return exitf(CodeValidation, "projet %s :\n%v", p.ID(), err)
	}
	// Le répertoire des journaux doit vivre sous logs.root, dont « doctor »
	// contrôle la chaîne de propriété : ailleurs, rien ne garantit qu'aucun de ses
	// composants n'appartienne à l'utilisateur (D71).
	if dir := filepath.Clean(p.Spec.Layout.LogDir); !strings.HasPrefix(dir, logsRoot+string(filepath.Separator)) {
		return exitf(CodeValidation, "projet %s :\nspec.layout.logDir : %s doit vivre sous %s (logs.root, D71)",
			p.ID(), dir, logsRoot)
	}
	return nil
}

// reservedDBAccounts énumère les comptes SGBD que la configuration désigne et
// qu'aucun projet ne doit déclarer (D91) : ceux de la supervision — le compte
// MySQL et le rôle PostgreSQL, qui porte le nom de l'utilisateur système de
// l'agent (D67) — et le rôle d'administration PostgreSQL (D44). Les noms fixes,
// root et postgres en tête, sont ceux de validate.ReservedDBAccount.
func (a *App) reservedDBAccounts() []string {
	return []string{
		a.Cfg.Monitoring.TelegrafAccount(),
		a.Cfg.Monitoring.TelegrafUser(),
		db.PostgresAdminRole(os.Getenv("OGS_PG_ADMIN")),
	}
}

// logsRoot renvoie la racine des journaux des projets, préfixe appliqué.
func (a *App) logsRoot() string { return filepath.Clean(a.Paths.LogsRoot(a.Cfg)) }

// homeRoot renvoie la racine des homes, préfixe appliqué.
func (a *App) homeRoot() string {
	if a.Prefix == "" {
		return a.Cfg.Naming.HomeRoot
	}
	return a.Paths.Home(a.Cfg, "")
}

// providers assemble les providers actifs pour cette exécution.
type providers struct {
	System     *system.Provider
	TLS        *tlsprov.Provider
	DB         *db.Provider
	AMQP       *amqp.Provider
	App        *appprov.Provider
	Cron       *cron.Provider
	Unit       *unit.Provider
	Sudoers    *sudoers.Provider
	Runtime    *runtime.Provider
	Hooks      *hook.Runner
	PHP        *php.Provider
	Web        *web.Provider
	Logrotate  *logrotate.Provider
	Monitoring *monitoring.Provider
	Mail       *mail.Provider
	Firewall   *firewall.Provider
	Engine     web.Engine
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
	sys.Force = a.Force
	php := &php.Provider{
		Paths:     a.Paths,
		Exec:      a.Exec,
		Templates: tmpl,
		Store:     a.Store,
		Force:     a.Force,
		Config:    a.Cfg,
	}
	eng, err := a.webEngine()
	if err != nil {
		return nil, err
	}
	sys.WebReload = "web:" + eng.Name()
	sys.WebEngine = eng.Name()
	appProv := &appprov.Provider{Paths: a.Paths, Exec: a.Exec, Templates: tmpl, Store: a.Store,
		Installer: &appprov.Installer{Exec: a.Exec}, Force: a.Force,
		Owner: func(proj *model.Project) (int, int, error) { return sys.ResolveOwner(proj, "") }}
	// Un pool en écoute TCP écarte, comme une application, les ports que la
	// machine tient déjà (D94).
	php.PortsInUse = appProv.PortsInUse
	amqpProv := a.amqpProvider()
	amqpPort := amqpProv.Port
	dbProv := &db.Provider{Paths: a.Paths, Exec: a.Exec, Store: a.Store, Keys: a.Keys, AMQPPort: amqpPort, Force: a.Force,
		Reserved: a.reservedDBAccounts()}
	certStore := tlsprov.Store{Paths: a.Paths}
	certs := &tlsprov.Provider{
		Paths: a.Paths, Cfg: a.Cfg, Store: certStore, DB: a.Store,
		Force: a.Force, Exec: a.Exec,
		Issuer: &tlsprov.ACME{
			Paths: a.Paths, Store: certStore, Email: a.Cfg.ACME.Email,
			Directory: a.Cfg.ACME.Directory,
		},
		// En mode simulé, l'émetteur de repli évite tout appel réseau : le mode
		// d'essai doit produire un site complet, certificat compris.
		Fallback: tlsprov.SelfSigned{},
	}
	if a.Exec.Simulated() {
		certs.Issuer = tlsprov.SelfSigned{}
	}
	certs.WebReload = sys.WebReload
	fw, err := a.firewallProvider(eng.Unit())
	if err != nil {
		return nil, err
	}
	// Sans déclaration lisible, les adresses de confiance sont inconnues : les
	// prisons gardent celles qu'elles ignorent déjà (D109).
	trusted, trustedErr := fw.IgnoreIP()
	webProv := &web.Provider{
		Paths: a.Paths, Exec: a.Exec, Templates: tmpl, Engine: eng, PHP: php,
		Certificate: certs.VHostCoverage,
		CertDir:     certStore.Dir,
		ProxyTarget: appProv.ProxyTarget,
		Force:       a.Force,
		LogsRoot:    a.logsRoot(),
	}
	// Un certificat ne quitte le disque — suppression, compensation d'une
	// émission — que si aucun vhost écrit ne le désigne (D110).
	certs.ServedBy = webProv.CertificateReferences
	// Avant tout rechargement, toute validation et tout redémarrage du serveur
	// web, les paires que désignent les vhosts activés sont vérifiées (D110).
	webProv.CertificatePair = func(ctx context.Context, dir string, repair bool) (web.PairCheck, error) {
		name, paired, repaired, err := certs.CheckPair(ctx, dir, repair)
		return web.PairCheck{Name: name, Paired: paired, Repaired: repaired}, err
	}
	return &providers{
		System: sys,
		TLS:    certs,
		DB:     dbProv,
		AMQP:   amqpProv,
		App:    appProv,
		Cron: &cron.Provider{Paths: a.Paths, Templates: tmpl, Force: a.Force, Simulated: a.Exec.Simulated(),
			Owner: func(proj *model.Project) (int, int, error) { return sys.ResolveOwner(proj, "") }},
		Unit:      &unit.Provider{Paths: a.Paths, Exec: a.Exec, Templates: tmpl, Force: a.Force},
		Sudoers:   &sudoers.Provider{Paths: a.Paths, Exec: a.Exec, Force: a.Force},
		Runtime:   &runtime.Provider{Paths: a.Paths},
		Hooks:     a.hookRunner(),
		PHP:       php,
		Web:       webProv,
		Logrotate: &logrotate.Provider{Paths: a.Paths, Templates: tmpl, Config: a.Cfg, Force: a.Force},
		Monitoring: &monitoring.Provider{
			Paths: a.Paths, Cfg: a.Cfg, Exec: a.Exec, Templates: tmpl,
			Store: a.Store, Keys: a.Keys,
			DB: dbProv, AMQP: amqpProv, PHP: php,
			WebEngine: eng.Name(), WebUnit: eng.Unit(),
			TrustedIP:   trusted,
			KeepTrusted: trustedErr != nil,
			Force:       a.Force,
		},
		Mail: &mail.Provider{
			Paths: a.Paths, Cfg: a.Cfg, Exec: a.Exec,
			Store: a.Store, Keys: a.Keys, Force: a.Force,
		},
		Firewall: fw,
		Engine:   eng,
	}, nil
}

// firewallProvider construit le provider du pare-feu (D72). ufw.yaml est lu
// ici, et non au démarrage, et une déclaration invalide n'est pas une erreur de
// construction : elle arrête les commandes du pare-feu, pas « ogs apply » ni
// « ogs project delete », dont le plan global la rend en avertissement. Absent,
// le provider ne gère rien.
func (a *App) firewallProvider(webUnit string) (*firewall.Provider, error) {
	prov := &firewall.Provider{Paths: a.Paths, Exec: a.Exec, WebUnit: webUnit, Force: a.Force}
	cfg, _, err := config.LoadUFW(a.Paths.UFWConfig())
	if err != nil {
		prov.LoadErr = fmt.Errorf("%s :\n%v", a.Paths.UFWConfig(), err)
		return prov, nil
	}
	prov.Cfg = cfg
	return prov, nil
}

// amqpProvider construit le provider RabbitMQ. Le client n'est branché que si
// des identifiants d'administration existent : sans eux, l'indisponibilité est
// rapportée par le plan plutôt qu'en erreur d'initialisation.
func (a *App) amqpProvider() *amqp.Provider {
	prov := &amqp.Provider{Paths: a.Paths, Store: a.Store, Keys: a.Keys,
		RecreateQueues: a.RecreateQueues, Fleet: a.loadProjects}
	creds, err := a.amqpCredentials()
	if err != nil {
		return prov
	}
	if client, err := amqp.NewHTTPClient(creds); err == nil {
		prov.Client = client
		prov.Host = "127.0.0.1"
		prov.Port = creds.AMQPPort
		prov.ManagementURL = creds.URL
		if prov.ManagementURL == "" {
			prov.ManagementURL = amqp.DefaultManagementURL
		}
	}
	return prov
}

// amqpCredentials lit les identifiants d'administration du broker.
func (a *App) amqpCredentials() (amqp.Credentials, error) {
	path := filepath.Join(a.Paths.Etc(), "credentials", "amqp.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return amqp.Credentials{}, fmt.Errorf("identifiants RabbitMQ absents : "+
				"les enregistrer avec « ogs amqp credentials set » (%s)", path)
		}
		return amqp.Credentials{}, err
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
		return amqp.Credentials{}, fmt.Errorf("%s est lisible au-delà de son propriétaire (mode %04o) : "+
			"corriger avec chmod 0600", path, info.Mode().Perm())
	}
	var creds amqp.Credentials
	if err := yaml.Unmarshal(data, &creds); err != nil {
		return amqp.Credentials{}, fmt.Errorf("%s : %w", path, err)
	}
	// Une clé inconnue — « amqpport » — perdait son réglage en silence (Q-34).
	// Le fichier reste lu : le refuser priverait de broker toutes les commandes
	// pour une faute que doctor nomme aussi (D101).
	if err := yaml.UnmarshalWithOptions(data, &amqp.Credentials{}, yaml.Strict()); err != nil && !a.amqpWarned {
		a.amqpWarned = true
		fmt.Fprintf(a.Err, "⚠ %s : %s — réglage ignoré\n", path, strictError(err))
	}
	return creds, nil
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
	prov := &system.Provider{Exec: a.Exec, Identity: ident, Paths: a.Paths}
	// Par la variable du paquet, que les essais substituent.
	prov.AccountOfUID = func(uid int) (string, error) { return accountOfUID(uid) }
	prov.KnownHome = a.knownHome
	if a.Prefix != "" {
		prov.AccountsOfGID = system.AccountsOfGIDIn(ident.PasswdPath)
	}
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

// knownHome dit si la base d'état porte ce projet, **actif**, avec cette racine :
// son home est alors le sien, et un compte supprimé à la main se recrée dessus
// (D95). Une ligne archivée ne vaut rien : « project delete » sans --purge la
// garde, home compris, et le home a pu passer depuis à un autre compte — une
// restauration, un clone ou une déclaration redéposée le reprenaient.
func (a *App) knownHome(p *model.Project) bool {
	if a.Store == nil {
		return false
	}
	row, err := a.Store.Project(p.ID())
	return err == nil && row.Status == "active" && row.Home != "" &&
		filepath.Clean(row.Home) == filepath.Clean(p.Spec.Layout.Root)
}
