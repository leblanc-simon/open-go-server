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
	// planifiées : la restauration remet le home et les bases avant de les lancer
	// (D113).
	holdServices bool
	// declaredBefore garde la ligne d'état qu'une création a remplacée, pour la
	// rendre si la création est défaite (D112).
	declaredBefore map[string]*store.ProjectRow
	// undone nomme les créations défaites, déclaration retirée (D112).
	undone map[string]bool
	// command est le chemin de la commande en cours (« ogs cert renew ») : la
	// base d'état disparue n'est tolérée que par le renouvellement (D120).
	command string
	// tolerateFleet laisse une consultation (status, la TUI) calculer sur le parc
	// lisible, les déclarations refusées nommées à part (D120). Jamais posé par
	// une commande qui écrit.
	tolerateFleet bool
	// stateLost compte les déclarations d'un parc dont state.db a disparu,
	// quand la commande tourne sur une base éphémère (D120).
	stateLost int
	// ctx est le contexte de la commande, qu'une interruption annule (D122) :
	// une question posée à l'exploitant rend la main dès qu'il l'est.
	ctx context.Context
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

// stateTolerant énumère les commandes qui tournent sans state.db quand elle a
// disparu d'un parc qui compte des projets, sur une base éphémère en mémoire
// (D120). Le renouvellement seul : le timer le lance sans personne pour lire un
// refus, il ne réserve aucun numéro, et un certificat ne doit pas périmer
// pendant que l'exploitant n'a pas encore vu le message.
//
// S'y ajoutent les commandes d'incident, qu'un exploitant doit pouvoir lancer
// pendant qu'il cherche la sauvegarde : le serveur web (« web reload », « web
// test »), les modèles (« template extract »), le pare-feu (« firewall
// status », « confirm », « apply », « ban », « unban ») et « harden ». Celles qui
// ouvrent la base — pour l'audit seulement — reçoivent la base éphémère, et le
// disent sur la sortie d'erreur.
var stateTolerant = map[string]bool{
	"ogs cert renew":       true,
	"ogs web reload":       true,
	"ogs web test":         true,
	"ogs template extract": true,
	"ogs firewall status":  true,
	"ogs firewall confirm": true,
	"ogs firewall apply":   true,
	"ogs firewall ban":     true,
	"ogs firewall unban":   true,
	"ogs harden":           true,
}

// lostState dit combien de déclarations porte un parc dont la base d'état a
// disparu : zéro si elle est là, si le parc est vide, ou si la machine n'a
// jamais été initialisée — sans clé maître, que seul « ogs init » engendre, il
// n'y a ni secret scellé ni preuve à perdre, et la première commande qui ouvre
// la base la crée, comme avant D120 ; « ogs init » reste la première étape
// documentée, et sans clé maître aucun secret ne se scelle.
func (a *App) lostState() int {
	if _, err := os.Lstat(a.Paths.StateDB()); !os.IsNotExist(err) {
		return 0
	}
	if _, err := os.Lstat(a.Paths.MasterKey()); os.IsNotExist(err) {
		return 0
	}
	entries, _ := os.ReadDir(a.Paths.ProjectsDir())
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			n++
		}
	}
	return n
}

// stateGuard refuse en code 5, avant toute écriture, une commande qui écrit
// quand la base d'état a disparu d'un parc initialisé (D120) : « site create »
// déposait sinon sa déclaration avant que l'apply ne refuse. Les consultations
// passent — celles qui ouvrent la base refusent à ce moment-là —, ainsi que
// « init », qui la recrée, « doctor », qui la nomme, ce que stateTolerant
// énumère, et ce qui ne fait que démarrer ou arrêter des services
// (configTolerant) : ils passent la garde, mais « service » et « my » ouvrent la
// base et refusent alors en code 5. L'inspecteur, « ogs » nu comme « ogs tui »,
// passe aussi la garde et refuse en code 5 à l'ouverture de la base.
func (a *App) stateGuard(cmd *cobra.Command) error {
	path := cmd.CommandPath()
	if path == "ogs doctor" || path == "ogs tui" || stateTolerant[path] || configTolerant[path] || !cmd.HasParent() ||
		dryRunConductOf(cmd) == dryRunReadOnly || !cmd.Runnable() {
		return nil
	}
	if n := a.lostState(); n > 0 {
		return exitf(CodeDependency, "%s", a.lostStateMessage(n))
	}
	return nil
}

// lostStateMessage décrit la base d'état disparue et son remède (D120).
func (a *App) lostStateMessage(n int) string {
	return fmt.Sprintf("base d'état %s absente alors que %d projet(s) sont déclarés : elle porte les mots de "+
		"passe scellés, les preuves de propriété des bases et des comptes, les numéros attribués et "+
		"l'empreinte du dernier apply, que rien ne recrée. La restaurer depuis sa sauvegarde ; à défaut, "+
		"« ogs init » la recrée vide — les numéros écrits dans les fichiers sont repris au prochain apply "+
		"(D94), les secrets non (D120)", a.Paths.StateDB(), n)
}

// openStore ouvre la base d'état et le keystore.
//
// La base n'est jamais recréée ici quand elle a disparu d'un parc qui compte
// des projets (D120) : store.Open la recréait vide, sans un mot, et ce qu'elle
// portait — secrets scellés, preuves de propriété, empreintes — passait pour
// absent, donc à refaire. La commande refuse en code 5 jusqu'à « ogs init » ;
// le renouvellement des certificats tourne sur une base éphémère, et le dit.
func (a *App) openStore() error {
	if a.Store != nil {
		return nil
	}
	var st *store.Store
	var err error
	if n := a.lostState(); n > 0 {
		if !stateTolerant[a.command] {
			return exitf(CodeDependency, "%s", a.lostStateMessage(n))
		}
		st, err = store.OpenMemory()
		a.stateLost = n
		// Le renouvellement le compte lui-même dans son code 6 ; les autres le
		// disent ici, une fois.
		if a.command != "ogs cert renew" {
			a.warnf("⚠ %s — cette commande tourne sur une base éphémère, oubliée à sa sortie\n",
				a.lostStateMessage(n))
		}
	} else {
		st, err = store.Open(a.Paths.StateDB())
	}
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

// loadProjects charge tous les projets actifs, et refuse en code 3 si une
// seule déclaration l'est : c'est le chargement de ce qui calcule pour tout le
// parc — plan global, unicité, pare-feu —, dont un fichier recalculé sans un
// projet effacerait ses entrées. Toutes les déclarations refusées sont nommées
// d'un coup : un refus qui en masque un autre fait corriger, relancer, et
// découvrir le suivant. Ce qui peut avancer sans le projet fautif — le
// renouvellement des certificats, doctor — passe par loadFleet (D120).
func (a *App) loadProjects() ([]*model.Project, error) {
	list, refused, err := a.loadFleet()
	if err != nil {
		return nil, err
	}
	if len(refused) > 0 {
		return nil, fleetRefusedError(refused)
	}
	return list, nil
}

// loadFleet charge le parc sans s'arrêter à la première déclaration refusée :
// illisible, ou refusée par prepare. Elle est rendue à part, nommée, et le reste
// du parc est chargé (R1-41, D120). Seule une lecture impossible du répertoire
// des projets est une erreur.
func (a *App) loadFleet() ([]*model.Project, []error, error) {
	dir := a.Paths.ProjectsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, wrap(CodeError, err)
	}
	var list []*model.Project
	var refused []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		p, err := spec.Load(filepath.Join(dir, e.Name()))
		if err != nil {
			refused = append(refused, fmt.Errorf("déclaration illisible : %w", err))
			continue
		}
		if err := a.prepare(p); err != nil {
			refused = append(refused, err)
			continue
		}
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID() < list[j].ID() })
	return list, refused, nil
}

// loadFleetNamed charge le parc pour une consultation : chaque déclaration
// refusée est nommée sur la sortie d'erreur, et le reste du parc est rendu avec
// leur nombre, que l'appelant compte dans son code de retour (D120).
func (a *App) loadFleetNamed() ([]*model.Project, int, error) {
	list, refused, err := a.loadFleet()
	if err != nil {
		return nil, 0, err
	}
	for _, err := range refused {
		a.warnf("⚠ déclaration refusée, ignorée ici : %v\n", err)
	}
	return list, len(refused), nil
}

// fleetRefusedError réunit les déclarations refusées du parc en un refus de
// code 3.
func fleetRefusedError(refused []error) error {
	parts := make([]string, len(refused))
	for i, err := range refused {
		parts[i] = err.Error()
	}
	return exitf(CodeValidation, "%d déclaration(s) refusée(s) ; ce calcul porte sur tout le parc, "+
		"il ne se fait pas sans elles :\n%s", len(refused), strings.Join(parts, "\n"))
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
	// L'appartenance de la racine à naming.homeRoot et des journaux à logs.root
	// n'est pas jugée ici, mais au plan et à l'apply (checkPlacement) et par
	// doctor : changer l'une des racines dans config.yaml rendait sinon tout le
	// parc invalide au chargement, renouvellement des certificats compris (D120).
	return nil
}

// placementProblem est un écart entre les racines que portent les déclarations
// et celles de config.yaml (D120).
type placementProblem struct {
	what, fix string
}

// placementProblems compare chaque déclaration aux racines de la configuration
// (validate.Placement). La déclaration porte les chemins reçus à sa création —
// Normalize ne remplit que les vides —, et c'est d'elle qu'ogs tire l'ancienne
// valeur d'une racine changée : le répertoire parent de ces chemins. ogs ne
// déplace ni un home, ni un compte, ni des journaux : changer la racine alors
// que des projets existent est refusé, et le remède est de remettre l'ancienne
// valeur. Un seul projet hors de la racine peut aussi être une déclaration
// retouchée : le remède nomme les deux.
func (a *App) placementProblems(fleet []*model.Project) []placementProblem {
	homeRoot, logsRoot := a.homeRoot(), a.logsRoot()
	var roots, logs []*model.Project
	for _, p := range fleet {
		err := validate.Placement(p, homeRoot, logsRoot)
		if err == nil {
			continue
		}
		for _, e := range err.(validate.Errors) {
			switch e.Field {
			case "spec.layout.root":
				roots = append(roots, p)
			case "spec.layout.logDir":
				logs = append(logs, p)
			}
		}
	}
	var out []placementProblem
	describe := func(key, field, current, reason string, list []*model.Project, path func(*model.Project) string) {
		if len(list) == 0 {
			return
		}
		var names []string
		parents := map[string]bool{}
		for _, p := range list {
			names = append(names, fmt.Sprintf("%s (%s)", p.ID(), path(p)))
			parents[a.unprefixed(filepath.Dir(filepath.Clean(path(p))))] = true
		}
		olds := make([]string, 0, len(parents))
		for d := range parents {
			olds = append(olds, d)
		}
		sort.Strings(olds)
		out = append(out, placementProblem{
			what: fmt.Sprintf("%s vaut %s, mais %s de %d projet(s) vit ailleurs : %s — %s ; ogs ne déplace pas "+
				"un projet existant (D120)", key, a.unprefixed(current), field, len(list), strings.Join(names, ", "),
				reason),
			fix: fmt.Sprintf("si %s a changé, lui rendre son ancienne valeur, %s, dans %s ; sinon, corriger "+
				"%s de la déclaration", key, strings.Join(olds, " ou "), a.Paths.ConfigFile(), field),
		})
	}
	describe("naming.homeRoot", "spec.layout.root", homeRoot, "un projet neuf se poserait sous la nouvelle "+
		"racine, le parc sous l'ancienne", roots, func(p *model.Project) string { return p.Spec.Layout.Root })
	describe("logs.root", "spec.layout.logDir", logsRoot, "seule la chaîne de logs.root est contrôlée (D71)",
		logs, func(p *model.Project) string { return p.Spec.Layout.LogDir })
	return out
}

// checkPlacement refuse en code 3 un parc dont une déclaration vit hors des
// racines de la configuration (D120). Il vaut au plan et à l'apply, du projet
// visé comme du parc, et à toute écriture d'une déclaration : un projet neuf se
// poserait sous la nouvelle racine pendant que le parc reste sous l'ancienne.
func (a *App) checkPlacement(fleet []*model.Project) error {
	problems := a.placementProblems(fleet)
	if len(problems) == 0 {
		return nil
	}
	parts := make([]string, len(problems))
	for i, p := range problems {
		parts[i] = p.what + "\n  → " + p.fix
	}
	return exitf(CodeValidation, "%s", strings.Join(parts, "\n"))
}

// unprefixed retire le préfixe du mode d'essai d'un chemin : le remède cite la
// valeur à écrire dans config.yaml, qui ne le porte pas.
func (a *App) unprefixed(path string) string {
	if a.Prefix == "" {
		return path
	}
	if rel, ok := strings.CutPrefix(path, filepath.Clean(a.Prefix)); ok && strings.HasPrefix(rel, "/") {
		return rel
	}
	return path
}

// creationSudo pose dans la déclaration d'un projet neuf la délégation que
// sudo.style et sudo.verbs de config.yaml demandent (D121). C'est un défaut de
// création, comme user.shell (D62) : appelé aux seules créations — project et
// site create, import adopt —, jamais au chargement, il ne retire ni n'ajoute de
// délégation aux projets existants. Une configuration qui demande la délégation
// historique n'écrit rien : la déclaration reste celle d'avant D121.
func (a *App) creationSudo(p *model.Project) {
	if p.Spec.Sudo == nil {
		p.Spec.Sudo = sudoers.CreationSettings(a.Cfg.Sudo.Style, a.Cfg.Sudo.Verbs)
	}
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
		Reserved: a.reservedDBAccounts(), PHPWorkers: a.phpWorkers}
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
			PHPVersions: a.Cfg.PHP.PHPVersions(),
			Owner:       func(proj *model.Project) (int, int, error) { return sys.ResolveOwner(proj, "") }},
		Unit:      &unit.Provider{Paths: a.Paths, Exec: a.Exec, Templates: tmpl, Force: a.Force},
		Sudoers:   &sudoers.Provider{Paths: a.Paths, Exec: a.Exec, Force: a.Force},
		Runtime:   &runtime.Provider{Paths: a.Paths, Config: a.Cfg},
		Hooks:     a.hookRunner(),
		PHP:       php,
		Web:       webProv,
		Logrotate: &logrotate.Provider{Paths: a.Paths, Exec: a.Exec, Templates: tmpl, Config: a.Cfg, Force: a.Force},
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
		prov.Admin = creds.User
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
	// La réécriture d'une commande vers ~/.venv/bin/python attend un venv
	// pourvu de ses dépendances (D116, D123). Sous préfixe, rien n'est installé :
	// ce contrôle se tait, comme ceux des liens du provider applicatif.
	if a.Prefix == "" {
		prov.LinkNotReady = appprov.LinkNotReady
	}
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
