package php

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/provider/removal"
	"leblanc.io/open-go-server/internal/store"
	"leblanc.io/open-go-server/internal/template"
)

// WebUser est le propriétaire du socket : le serveur web doit pouvoir s'y
// connecter, l'utilisateur du projet doit pouvoir l'écouter.
const WebUser = "www-data"

// Provider réconcilie les pools PHP-FPM d'un projet.
type Provider struct {
	Paths     config.Paths
	Exec      exec.Executor
	Templates *template.Set
	Store     *store.Store
	// Now est injectable pour rendre les rendus reproductibles en test.
	Now func() time.Time
	// Versions est la liste des versions détectées ; recalculée si nulle.
	Versions []Version
	// Force autorise l'écrasement d'un fichier qu'ogs n'a pas écrit, après
	// sauvegarde horodatée (§6.4). Sans lui, le refus n'aurait aucune porte de
	// sortie et l'apply du projet resterait bloqué.
	Force bool
	// PortsInUse rend les ports TCP en écoute sur la machine ; un pool en écoute
	// TCP n'en reçoit jamais un (D94). Nil, rien n'est écarté.
	PortsInUse func() map[int]bool
	// Config porte les couches de la machine (« php.pool », D103) et la descente
	// du preset par environnement. Nil, la configuration livrée.
	Config *config.Config

	recovered   sync.Once
	recoveryErr error
}

// refusStep produit l'étape qui refuse d'écraser un fichier qu'ogs n'a pas
// écrit. Le message nomme la porte de sortie — elle existe désormais — et
// distingue les deux causes : un fichier « non géré » n'a jamais porté la marque
// d'ogs, c'est ce que rencontre un site adopté à son premier apply ; un fichier
// « modifié hors ogs » a été retouché après avoir été généré.
func refusStep(resource, summary, path, backups string, state fs.State) *engine.Step {
	libelle, motif := "modifié hors ogs", "a été modifié en dehors d'ogs"
	if state == fs.StateForeign {
		libelle, motif = "non géré par ogs", "existe déjà sans porter la marque d'ogs"
	}
	s := engine.NewStep(resource, engine.Update, summary+" — fichier "+libelle)
	s.Apply = func(context.Context) error {
		return fmt.Errorf("%s %s : relancer avec --force pour l'écraser "+
			"(l'original sera sauvegardé dans %s)", path, motif, backups)
	}
	return s
}

// Name identifie le provider.
func (p *Provider) Name() string { return "php" }

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Provider) versions() []Version {
	if p.Versions == nil {
		p.Versions = Detect(p.Paths)
	}
	return p.Versions
}

// PoolPlan est le résultat du calcul d'un pool : son nom effectif, son numéro,
// son point d'écoute et le contenu attendu de son fichier.
type PoolPlan struct {
	Pool    model.Pool
	Name    string
	Number  int
	Path    string
	Listen  string
	Unix    bool
	Content []byte
	State   fs.State
	Version string
	// Fixed dit que le numéro est imposé par la déclaration : l'étape l'inscrit
	// à la place de celui que le pool tenait (D100).
	Fixed bool
	// Project est l'identifiant du projet, pour l'inscription du numéro.
	Project string
	// Resolved porte les réglages rendus, toutes couches fusionnées, et ce qui
	// en a été écarté.
	Resolved Resolved
}

// Observed est l'état réel des pools d'un projet.
type Observed struct {
	// Files associe un chemin de pool à son état.
	Files map[string]fs.State
	// Stale énumère les fichiers de pool appartenant au projet mais devenus
	// inutiles : version abandonnée après un switch, pool supprimé du YAML.
	Stale []string
	// Leftovers énumère les fichiers qui portent le nom exact d'un pool du
	// projet dans une autre version que la sienne, sans qu'ogs les ait écrits
	// pour lui : le pool d'origine d'un projet adopté que --force n'a jamais
	// repris, resté dans l'ancienne version après un « php switch ». Ils ne
	// sont pas au projet, donc jamais retirés ; ils sont signalés.
	Leftovers []string
	// Units porte l'état de l'unité FPM de chaque version qu'emploie le projet
	// (D105) : un maître mort ou désactivé n'était relevé qu'à l'écriture d'un
	// pool, donc jamais tant que la déclaration ne changeait pas.
	Units map[string]UnitState
}

// Observe inspecte les pools existants du projet, toutes versions confondues.
//
// Un pool n'est obsolète que si son en-tête le dit écrit par ogs pour ce projet,
// et comme l'un de ses pools (fs.ManagedResource), jamais d'après son nom :
// « shop_api.conf » semble prolonger « shop » d'un rôle, et il est le pool
// principal du projet shop_api. L'attribution par préfixe faisait retirer au
// premier apply de shop le pool de son voisin, et tout pool posé à la main
// selon la convention.
func (p *Provider) Observe(ctx context.Context, proj *model.Project) (Observed, error) {
	obs := Observed{Files: map[string]fs.State{}, Units: map[string]UnitState{}}
	wanted := map[string]bool{}
	names := map[string]bool{}
	for _, pool := range proj.Spec.PHP.Pools {
		if _, seen := obs.Units[pool.Version]; !seen {
			obs.Units[pool.Version] = p.Unit(ctx, pool.Version)
		}
		name, err := p.poolName(proj, pool)
		if err != nil {
			return obs, err
		}
		path := p.Paths.PHPPoolFile(pool.Version, name)
		wanted[path] = true
		names[filepath.Base(path)] = true
	}
	for _, v := range p.versions() {
		entries, err := os.ReadDir(v.PoolDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".conf" {
				continue
			}
			path := filepath.Join(v.PoolDir, e.Name())
			if wanted[path] {
				continue
			}
			// Un fichier illisible ne peut pas être attribué : il n'est pas retiré.
			owner, resource, err := fs.ManagedResource(path)
			switch {
			case err != nil:
			case owner == proj.ID() && strings.HasPrefix(resource, "php/pool/"):
				obs.Stale = append(obs.Stale, path)
			case names[e.Name()]:
				// Le nom exact d'un pool du projet, ailleurs que dans sa version :
				// il tourne encore sous le compte du projet. Le taire laissait deux
				// pools actifs après un « php switch » sans que rien ne le dise.
				obs.Leftovers = append(obs.Leftovers, path)
			}
		}
	}
	return obs, nil
}

// poolName traduit le nom logique d'un pool en nom effectif : le pool principal
// porte le nom nu du projet, les autres reçoivent un suffixe de rôle.
func (p *Provider) poolName(proj *model.Project, pool model.Pool) (string, error) {
	if pool.Name == "" {
		return "", fmt.Errorf("pool sans nom dans le projet %s", proj.ID())
	}
	if pool.Name == model.MainPoolName {
		return proj.ID(), nil
	}
	return naming.RoleName(proj.ID(), pool.Name), nil
}

// number attribue ou retrouve le numéro d'un pool. La plage est propre à la
// version, de sorte qu'un pool garde un numéro lisible : 18501 pour le premier
// pool en PHP 8.5.
func (p *Provider) number(proj *model.Project, pool model.Pool, name string) (int, error) {
	explicit := pool.Number != "" && pool.Number != "auto"
	var fixed int
	if explicit {
		n, err := strconv.Atoi(pool.Number)
		if err != nil {
			return 0, fmt.Errorf("numéro de pool %q illisible", pool.Number)
		}
		fixed = n
	}
	min, err := naming.PoolNumber(pool.Version, 1)
	if err != nil {
		return 0, err
	}
	max, err := naming.PoolNumber(pool.Version, 99)
	if err != nil {
		return 0, err
	}
	if p.Store == nil {
		if explicit {
			return fixed, nil
		}
		// Sans base d'état (calcul de plan hors ligne), le premier numéro de la
		// plage est utilisé : le plan reste lisible, l'attribution définitive a
		// lieu à l'application.
		return min, nil
	}
	// Les numéros des pools écrits passent avant toute attribution : sur une
	// base perdue, le premier projet calculé prendrait sinon le port d'un voisin
	// (D94).
	p.recovered.Do(func() { p.recoveryErr = p.recoverNumbers() })
	if p.recoveryErr != nil {
		return 0, fmt.Errorf("reprise des numéros des pools écrits : %w", p.recoveryErr)
	}
	if explicit {
		// Le numéro imposé est inscrit par l'étape qui écrit le pool, non au
		// calcul du plan (poolStep) : un plan refusé ne retire rien.
		return fixed, nil
	}
	var inUse map[int]bool
	if pool.Listen == "tcp" && p.PortsInUse != nil {
		inUse = p.PortsInUse()
	}
	// Les numéros que d'autres pools du projet s'imposent ne sont pas encore
	// inscrits au calcul du plan : l'attribution automatique les évite.
	imposed := map[int]bool{}
	for _, other := range proj.Spec.PHP.Pools {
		if other.Version == pool.Version && other.Number != "" && other.Number != "auto" {
			if n, err := strconv.Atoi(other.Number); err == nil {
				imposed[n] = true
			}
		}
	}
	return p.Store.Allocate("php-"+pool.Version, proj.ID(), name, min, max,
		func(n int) bool { return inUse[n] || imposed[n] })
}

// reListenLine relit l'écoute d'un pool écrit : le port TCP, ou le numéro que
// porte le nom du socket (php<v>-fpm-<numéro>-<pool>.sock).
var reListenLine = regexp.MustCompile(`(?m)^listen\s*=\s*(?:127\.0\.0\.1:([0-9]+)|\S*/php[0-9.]+-fpm-([0-9]+)-[^/\s]+\.sock)\s*$`)

// recoverNumbers inscrit dans la base d'état les numéros que portent les pools
// écrits par ogs (D94). La base n'est pas seule à savoir quel numéro sert quel
// pool : en écoute TCP, le vhost d'un voisin relaie déjà vers le port de son
// pool. Le projet et le pool sont lus sur l'en-tête, jamais sur le nom du
// fichier (D76).
func (p *Provider) recoverNumbers() error {
	for _, version := range Supported {
		min, err := naming.PoolNumber(version, 1)
		if err != nil {
			return err
		}
		max, err := naming.PoolNumber(version, 99)
		if err != nil {
			return err
		}
		matches, err := filepath.Glob(filepath.Join(p.Paths.PHPPoolDir(version), "*.conf"))
		if err != nil {
			return err
		}
		for _, path := range matches {
			project, resource, err := fs.ManagedResource(path)
			name, ok := strings.CutPrefix(resource, "php/pool/")
			if err != nil || project == "" || !ok || name == "" {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			m := reListenLine.FindSubmatch(data)
			if m == nil {
				continue
			}
			digits := m[1]
			if len(digits) == 0 {
				digits = m[2]
			}
			n, err := strconv.Atoi(string(digits))
			if err != nil || n < min || n > max {
				continue
			}
			if _, err := p.Store.Claim("php-"+version, project, name, n); err != nil {
				return err
			}
		}
	}
	return nil
}

// Build calcule le rendu attendu d'un pool.
func (p *Provider) Build(proj *model.Project, pool model.Pool) (*PoolPlan, error) {
	if !IsSupported(pool.Version) {
		return nil, fmt.Errorf("version PHP %q hors périmètre (gérées : %v)", pool.Version, Supported)
	}
	name, err := p.poolName(proj, pool)
	if err != nil {
		return nil, err
	}
	num, err := p.number(proj, pool, name)
	if err != nil {
		return nil, err
	}
	at, err := proj.AppType()
	if err != nil {
		return nil, err
	}
	resolved := Resolve(p.Config, proj, pool, at)
	// php.ini l'emporte sur le pool : une fonction qu'il ferme reste fermée,
	// allowFunctions compris (éprouvé sous un vrai FPM, D103).
	if len(pool.AllowFunctions) > 0 {
		ini := iniDisabledFunctions(filepath.Join(p.Paths.PHPRoot(), pool.Version, "fpm", "php.ini"))
		for _, fn := range pool.AllowFunctions {
			if ini[strings.ToLower(fn)] {
				resolved.Problems = append(resolved.Problems, Problem{
					Field: fmt.Sprintf("spec.php.pools[%s].allowFunctions", pool.Name),
					Message: fmt.Sprintf("%s est fermée par le php.ini de FPM %s, qu'un pool ne rouvre pas",
						fn, pool.Version)})
			}
		}
	}

	unix := pool.Listen != "tcp"
	listen := p.Paths.PHPSocket(pool.Version, num, name)
	if !unix {
		listen = fmt.Sprintf("127.0.0.1:%d", num)
	}
	settings, env := resolved.Settings, resolved.Env

	body, err := p.Templates.Render("php/pool.conf", map[string]any{
		"Project":    proj.ID(),
		"Name":       name,
		"Version":    pool.Version,
		"Number":     num,
		"User":       proj.Spec.User.Name,
		"Group":      proj.Spec.User.Group,
		"WebUser":    WebUser,
		"Listen":     listen,
		"UnixSocket": unix,
		"Settings":   settings,
		"Env":        env,
	})
	if err != nil {
		return nil, err
	}
	content := fs.Render(fs.Header{
		Project:   proj.ID(),
		Resource:  "php/pool/" + name,
		Generated: p.now(),
		Template:  p.Templates.OverridePath("php/pool.conf"),
	}, body, ";")

	path := p.Paths.PHPPoolFile(pool.Version, name)
	state, err := fs.Inspect(path, content)
	if err != nil {
		return nil, err
	}
	return &PoolPlan{
		Pool: pool, Name: name, Number: num, Path: path, Listen: listen,
		Unix: unix, Content: content, State: state, Version: pool.Version,
		Fixed: pool.Number != "" && pool.Number != "auto", Project: proj.ID(),
		Resolved: resolved,
	}, nil
}

// Plan calcule les étapes relatives aux pools.
func (p *Provider) Plan(_ context.Context, proj *model.Project, obs Observed) ([]*engine.Step, error) {
	var steps []*engine.Step
	for _, pool := range proj.Spec.PHP.Pools {
		bp, err := p.Build(proj, pool)
		if err != nil {
			return nil, err
		}
		// Une déclaration qui injecterait une directive, ou qui nomme un preset
		// inconnu, est refusée au plan du projet — jamais au chargement, qui
		// arrêterait tout le parc (D103).
		if blocking := bp.Resolved.Blocking(); len(blocking) > 0 {
			return nil, &DeclarationError{Project: proj.ID(), Pool: pool.Name, Problems: blocking}
		}
		// Le pool d'un autre projet n'est jamais réécrit, --force compris : le pool
		// « api » de shop s'écrirait dans shop_api.conf, le pool principal du projet
		// shop_api, qui tournerait alors sous l'utilisateur de shop.
		if err := fs.CheckOwner(bp.Path, proj.ID(),
			fmt.Sprintf("pool « %s » de %s", pool.Name, proj.ID())); err != nil {
			return nil, err
		}
		// L'installation n'est vérifiée qu'en exécution réelle : un plan doit
		// rester calculable sur une machine qui n'a pas la version visée.
		if !p.Exec.Simulated() {
			v, ok := Find(p.versions(), pool.Version)
			switch {
			case !ok:
				return nil, fmt.Errorf("PHP %s n'est pas installé (versions détectées : %s)",
					pool.Version, describeVersions(p.versions()))
			case !v.Usable:
				return nil, fmt.Errorf("PHP %s est déclaré mais inutilisable : %s",
					pool.Version, strings.Join(v.Missing, ", "))
			}
		}
		steps = append(steps, p.poolStep(proj, bp))
		// Ce qui a été écarté du rendu est nommé par une étape non critique : le
		// pool est servi sans, et l'apply rend le code 6 tant que la déclaration —
		// ou config.yaml — n'est pas corrigée.
		if warnings := bp.Resolved.Warnings(); len(warnings) > 0 {
			steps = append(steps, discardedStep(bp, warnings))
		}
	}
	return append(steps, unitSteps(obs)...), nil
}

// unitSteps relève l'unité FPM d'une version employée par le projet quand elle
// est arrêtée ou désactivée (D105). L'étape n'écrit rien : elle porte
// l'intention « php:<v> », que Reload traduit en démarrage et activation — le
// même chemin que celui du premier pool, contrôles compris. Elle n'est pas
// bloquante : un FPM qui refuse de démarrer est un avertissement du
// rechargement, comme ailleurs.
func unitSteps(obs Observed) []*engine.Step {
	versions := make([]string, 0, len(obs.Units))
	for v := range obs.Units {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	var steps []*engine.Step
	for _, v := range versions {
		st := obs.Units[v]
		if st.Healthy() {
			continue
		}
		var etat string
		switch {
		case !st.Active && !st.Enabled:
			etat = "arrêtée et désactivée"
		case !st.Active:
			etat = "arrêtée"
		default:
			etat = "désactivée : elle ne repartirait pas au démarrage de la machine"
		}
		s := engine.NewStep("php/unit/"+v, engine.Update,
			fmt.Sprintf("unité %s %s — démarrage et activation", config.PHPUnit(v), etat))
		s.Critical = false
		s.Reload = []string{"php:" + v}
		steps = append(steps, s)
	}
	return steps
}

// DeclarationError est le refus d'une déclaration de pool que le plan ne peut
// pas rendre.
type DeclarationError struct {
	Project  string
	Pool     string
	Problems []Problem
}

func (e *DeclarationError) Error() string {
	lines := make([]string, len(e.Problems))
	for i, pb := range e.Problems {
		lines[i] = pb.String()
	}
	return fmt.Sprintf("pool « %s » de %s refusé :\n%s", e.Pool, e.Project, strings.Join(lines, "\n"))
}

// discardedStep nomme les réglages écartés du rendu d'un pool, ou sans effet.
// Elle échoue à l'application, sans rien écrire : c'est ce qui la fait compter
// dans le code 6. La liste n'est portée que par l'erreur : le résumé et l'erreur
// s'affichent sur la même ligne.
func discardedStep(bp *PoolPlan, warnings []Problem) *engine.Step {
	lines := make([]string, len(warnings))
	for i, pb := range warnings {
		lines[i] = pb.String()
	}
	s := engine.NewStep("php/pool/"+bp.Name+"/settings", engine.Update,
		fmt.Sprintf("pool php %s %s — %d réglage(s) à revoir : %s",
			bp.Version, bp.Name, len(warnings), strings.Join(lines, " ; ")))
	s.Critical = false
	s.Apply = func(context.Context) error {
		// Chaque réglage porte son effet dans le résumé — écarté, sans effet,
		// rien à retirer — : l'erreur ne fait que demander la correction.
		return fmt.Errorf("déclaration du pool %s à corriger", bp.Name)
	}
	return s
}

// iniDisabledFunctions lit la directive disable_functions d'un php.ini. Un
// fichier illisible ne ferme rien.
func iniDisabledFunctions(path string) map[string]bool {
	out := map[string]bool{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "disable_functions" {
			continue
		}
		if i := strings.Index(value, ";"); i >= 0 {
			value = value[:i]
		}
		for _, fn := range strings.Split(strings.Trim(strings.TrimSpace(value), `"'`), ",") {
			if fn = strings.TrimSpace(fn); fn != "" {
				out[strings.ToLower(fn)] = true
			}
		}
	}
	return out
}

// PlanStale renvoie les suppressions de pools devenus inutiles. Elles sont
// séparées du reste et appliquées **après** la réécriture des vhosts : supprimer
// l'ancien pool avant que le vhost ne pointe sur le nouveau socket exposerait une
// erreur 502 le temps de la bascule (§7.5).
//
// Les restes (Leftovers) y passent aussi, par la même porte : le régime des
// cinq états les refuse — ogs ne les a pas écrits pour le projet —, et ce refus
// non critique est ce qui les signale.
func (p *Provider) PlanStale(_ context.Context, proj *model.Project, obs Observed) ([]*engine.Step, error) {
	steps := make([]*engine.Step, 0, len(obs.Stale)+len(obs.Leftovers))
	for _, stale := range append(append([]string(nil), obs.Stale...), obs.Leftovers...) {
		s, err := p.removeStep(proj, stale)
		if err != nil {
			return nil, err
		}
		if s != nil {
			steps = append(steps, s)
		}
	}
	return steps, nil
}

func (p *Provider) poolStep(proj *model.Project, bp *PoolPlan) *engine.Step {
	resource := "php/pool/" + bp.Name
	summary := fmt.Sprintf("pool php %s %s (n° %d, %s)", bp.Version, bp.Name, bp.Number, describeListen(bp))
	state, forced := bp.State, false
	if (state == fs.StateHandEdited || state == fs.StateForeign) && p.Force {
		// --force : l'étape redevient une mise à jour ordinaire, annoncée comme
		// telle par « plan » avant qu'« apply » ne l'exécute. La sauvegarde, elle,
		// n'a lieu qu'à l'application : « plan » et « diff » n'écrivent rien.
		state, forced = fs.StateOutdated, true
		summary += " — écrasement forcé (sauvegarde dans " + p.Paths.BackupsDir() + ")"
	}
	switch state {
	case fs.StateCurrent:
		return engine.NewStep(resource, engine.NoOp, summary)
	case fs.StateHandEdited, fs.StateForeign:
		return refusStep(resource, summary, bp.Path, p.Paths.BackupsDir(), state)
	}
	action := engine.Create
	if state == fs.StateOutdated {
		action = engine.Update
	}
	s := engine.NewStep(resource, action, summary)
	s.Diff = func() (string, error) { return diffText(bp.Path, bp.Content) }
	// Le corps précédent est relevé à l'application et non au plan : l'état du
	// disque a pu changer entre les deux.
	var previous []byte
	var hadPrevious bool
	s.Apply = func(ctx context.Context) error {
		if data, err := os.ReadFile(bp.Path); err == nil {
			previous, hadPrevious = data, true
		}
		if forced {
			// Écraser le travail d'un exploitant sans en garder trace rend
			// l'opération irréversible : la sauvegarde précède l'écriture.
			if _, err := fs.BackupTo(bp.Path, p.Paths.BackupsDir(), p.now()); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := p.writePool(ctx, bp); err != nil {
			return err
		}
		// Le numéro imposé est inscrit à la place de celui que le pool tenait :
		// l'attribution automatique d'un autre pool le prenait sinon (D100). Tenu
		// par une autre ressource, il est employé sans être repris — en socket
		// Unix, le nom du pool distingue les deux sockets. Un apply déroulé rend
		// l'ancien numéro (Store.CancelAllocations).
		if bp.Fixed && p.Store != nil {
			if _, err := p.Store.Reserve("php-"+bp.Version, bp.Project, bp.Name, bp.Number); err != nil {
				return err
			}
		}
		return nil
	}
	// La compensation rend le pool qui était servi. Le supprimer serait juste
	// pour une création, destructeur pour une mise à jour — un écrasement forcé
	// en particulier, dont le fichier écrasé portait l'intention de quelqu'un.
	s.Compensate = func(context.Context) error {
		if hadPrevious {
			return fs.WriteAtomic(bp.Path, previous, 0o644, fs.KeepOwner)
		}
		if err := os.Remove(bp.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	// Le rechargement est groupé en fin d'application : une seule fois par version.
	s.Reload = []string{"php:" + bp.Version}
	return s
}

// removeStep retire un pool devenu inutile, sous le régime des cinq états : un
// pool retouché exige --force et part sauvegardé, un pool qu'ogs n'a pas écrit
// pour ce projet reste en place.
//
// Le numéro du pool retiré est libéré une fois le fichier parti (D100) : sans
// quoi chaque pool renommé, retiré ou passé à une autre version gardait le sien
// à jamais, et la plage de la version finissait saturée. Il ne l'est qu'alors —
// un pool refusé, resté en place, sert encore son numéro.
func (p *Provider) removeStep(proj *model.Project, path string) (*engine.Step, error) {
	version := versionFromPoolPath(path)
	s, err := removal.File{
		Resource: "php/pool/" + filepath.Base(path),
		Summary:  fmt.Sprintf("pool php obsolète %s", path),
		Path:     path,
		Project:  proj.ID(),
		Force:    p.Force,
		Backups:  p.Paths.BackupsDir(),
		Now:      p.now,
		Reload:   []string{"php:" + version},
	}.Step()
	if err != nil || s == nil || p.Store == nil {
		return s, err
	}
	owner, resource, _ := fs.ManagedResource(path)
	name, ok := strings.CutPrefix(resource, "php/pool/")
	if owner != proj.ID() || !ok || name == "" {
		return s, nil
	}
	scope := "php-" + version
	released := 0
	remove, restore := s.Apply, s.Compensate
	s.Apply = func(ctx context.Context) error {
		if err := remove(ctx); err != nil {
			return err
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return nil
		}
		released = allocatedNumber(p.Store, scope, proj.ID(), name)
		return p.Store.Release(scope, proj.ID(), name)
	}
	// Le pool rendu par une compensation reprend son numéro : il le sert de
	// nouveau.
	s.Compensate = func(ctx context.Context) error {
		if restore != nil {
			if err := restore(ctx); err != nil {
				return err
			}
		}
		if released > 0 {
			_, err := p.Store.Claim(scope, proj.ID(), name, released)
			return err
		}
		return nil
	}
	return s, nil
}

// allocatedNumber rend le numéro qu'une ressource tient en base, zéro sinon.
func allocatedNumber(st *store.Store, scope, project, name string) int {
	list, err := st.Allocations(scope)
	if err != nil {
		return 0
	}
	for _, a := range list {
		if a.Project == project && a.Name == name {
			return a.Number
		}
	}
	return 0
}

// writePool écrit le fichier de pool puis valide la configuration FPM. La
// validation a lieu après écriture et avant rechargement : un pool fautif est
// ainsi retiré avant d'atteindre le service.
func (p *Provider) writePool(ctx context.Context, bp *PoolPlan) error {
	dir := filepath.Dir(bp.Path)
	if _, err := os.Stat(dir); err != nil {
		if !p.Exec.Simulated() {
			// Le message doit désigner la cause réelle — une version PHP absente —
			// et non l'échec d'écriture qui n'en est que la conséquence.
			return fmt.Errorf("PHP %s n'est pas installé : le répertoire de pools %s est absent "+
				"(installer php%s-fpm, ou choisir une autre version)", bp.Version, dir, bp.Version)
		}
		// Mode simulé : l'arborescence est créée pour que l'essai produise des
		// fichiers inspectables.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	previous, hadPrevious := os.ReadFile(bp.Path)
	if err := fs.WriteAtomic(bp.Path, bp.Content, 0o644, fs.KeepOwner); err != nil {
		return err
	}
	if err := p.validate(ctx, bp.Version); err != nil {
		// Restauration immédiate : la configuration ne doit jamais rester dans un
		// état que FPM refuserait au prochain redémarrage. Une remise en place qui
		// échoue se dit : le pool refusé resterait sinon en place sans un mot, et
		// le prochain démarrage de FPM tomberait dessus.
		var restoreErr error
		if hadPrevious == nil {
			restoreErr = fs.WriteAtomic(bp.Path, previous, 0o644, fs.KeepOwner)
		} else if rmErr := os.Remove(bp.Path); rmErr != nil && !os.IsNotExist(rmErr) {
			restoreErr = rmErr
		}
		if restoreErr != nil {
			return fmt.Errorf("%w\n  la remise en place de %s a échoué : %v — "+
				"FPM refusera de redémarrer tant que ce fichier n'est pas corrigé", err, bp.Path, restoreErr)
		}
		return err
	}
	return nil
}

// validate lance « php-fpm -t ». L'absence du binaire n'est pas une erreur en
// mode simulé : le plan doit rester calculable sur une machine sans PHP.
func (p *Provider) validate(ctx context.Context, version string) error {
	if p.Exec.Simulated() {
		return nil
	}
	bin := p.Paths.PHPFpmBin(version)
	if _, err := os.Stat(bin); err != nil {
		return nil
	}
	if _, err := p.Exec.Run(ctx, exec.Command{Name: bin, Args: []string{"-t"}}); err != nil {
		return fmt.Errorf("configuration FPM refusée par %s -t : %w", bin, err)
	}
	return nil
}

// Reload met le gestionnaire de processus d'une version en accord avec ses
// pools : rechargé s'il tourne, démarré s'il est arrêté, arrêté s'il n'a plus
// aucun pool.
//
// Un simple « systemctl reload » ne suffit pas, parce que FPM refuse de tourner
// sans pool (« No pool defined ») :
//   - une version dont le pool de la distribution (www.conf) a été retiré reste
//     arrêtée tant qu'aucun projet ne l'emploie. Le rechargement qui suit
//     l'écriture du premier pool échouait alors sur une unité inactive, et le
//     site répondait 503 sur un simple avertissement ;
//   - recharger un maître dont le dernier pool vient de partir le tue : il relit
//     sa configuration, n'y trouve rien à servir et s'arrête — en échec, au
//     prochain démarrage comme à celui-ci.
//
// L'unité est donc activée au démarrage et désactivée à l'arrêt : une version
// sans pool relancée au redémarrage de la machine tomberait en échec. Une unité
// qui tourne sans être activée l'est au passage (D105) : elle ne repartirait
// pas au prochain démarrage de la machine.
//
// Le rechargement d'un maître qui tourne est gardé comme celui du serveur web
// (D105). « systemctl reload » n'envoie qu'un USR2 et rend 0 aussitôt : le
// maître se ré-exécute ensuite, relit toute sa configuration — pools de
// l'administrateur compris — et, sur le moindre refus, meurt pour toute la
// version, Restart=on-failure s'épuisant sur la même cause. Retirer un pool, qui
// ne passe par aucune écriture donc par aucun « -t », suffisait à le tuer sur un
// défaut latent d'un voisin, en code 0. D'où « php-fpm -t » avant tout
// rechargement — refusé, le maître garde la configuration qu'il sert —, puis le
// constat : un maître mort est relevé et le fait signalé dans tous les cas.
func (p *Provider) Reload(ctx context.Context, version string) error {
	if p.Exec.Simulated() {
		return nil
	}
	unit := config.PHPUnit(version)
	has, err := p.hasPools(version)
	if err != nil {
		return err
	}
	if !has {
		if !p.unitActive(ctx, unit) {
			return nil
		}
		if res, err := p.systemctl(ctx, "disable", "--now", unit); err != nil {
			return fmt.Errorf("arrêt de %s, qui n'a plus aucun pool : %s", unit, firstError(res, err))
		}
		return nil
	}
	if p.unitActive(ctx, unit) {
		if err := p.validate(ctx, version); err != nil {
			return &RefusedError{Unit: unit, Err: err}
		}
		restarts, known := p.restarts(ctx, unit)
		if res, err := p.systemctl(ctx, "reload", unit); err != nil {
			return fmt.Errorf("rechargement de %s : %s", unit, firstError(res, err))
		}
		if err := p.checkReloaded(ctx, unit, restarts, known); err != nil {
			return err
		}
		return p.ensureEnabled(ctx, unit)
	}
	// Arrêtée, jamais démarrée ou morte : la configuration est éprouvée d'abord,
	// pour nommer la cause plutôt que l'échec du démarrage qui en découlerait.
	if err := p.validate(ctx, version); err != nil {
		return fmt.Errorf("%w\n  démarrage de %s refusé", err, unit)
	}
	// Le compteur d'échecs est acquitté, sans quoi systemd refuse le démarrage
	// passé StartLimitBurst alors que la cause est levée.
	_, _ = p.systemctl(ctx, "reset-failed", unit)
	if res, err := p.systemctl(ctx, "enable", "--now", unit); err != nil {
		return fmt.Errorf("démarrage de %s : %s", unit, firstError(res, err))
	}
	return nil
}

// RefusedError est un rechargement refusé par « php-fpm -t » : le maître sert
// encore sa configuration précédente, pools retirés compris (D105).
type RefusedError struct {
	Unit string
	Err  error
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("%v\n  rechargement refusé : %s continue de servir sa configuration précédente",
		e.Err, e.Unit)
}

func (e *RefusedError) Unwrap() error { return e.Err }

// délaiConstat est la fenêtre pendant laquelle un maître FPM rechargé est
// observé. L'unité de la distribution recharge par « kill -USR2 » : systemctl
// rend la main avant même que le maître se ré-exécute, et c'est dans la seconde
// qui suit qu'il meurt sur une configuration refusée — puis que Restart=
// on-failure le relance, et le relance encore. Variable pour les tests.
var délaiConstat = 2 * time.Second

// checkReloaded constate qu'un maître rechargé vit encore. Il le relève s'il
// est mort, et le signale dans tous les cas : un FPM qui meurt au rechargement
// est un incident, même relevé aussitôt. Un redémarrage par systemd pendant la
// fenêtre (NRestarts) compte comme une mort : les requêtes en cours ont été
// coupées, et la cause reste à traiter.
func (p *Provider) checkReloaded(ctx context.Context, unit string, before int, known bool) error {
	settle := délaiConstat
	pas := settle / 8
	alive := true
	for fin := time.Now().Add(settle); ; {
		if !p.unitActive(ctx, unit) {
			alive = false
			break
		}
		if !time.Now().Before(fin) {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pas):
		}
	}
	if alive {
		after, ok := p.restarts(ctx, unit)
		if !known || !ok || after <= before {
			return nil
		}
		return fmt.Errorf("le rechargement a arrêté %s, que systemd a relancé ; "+
			"la cause reste à traiter (voir « journalctl -u %s »)", unit, unit)
	}
	_, _ = p.systemctl(ctx, "reset-failed", unit)
	if res, err := p.systemctl(ctx, "start", unit); err != nil {
		return fmt.Errorf("le rechargement a arrêté %s, qui refuse de redémarrer : %s\n"+
			"  les sites PHP de cette version ne sont plus servis — voir « journalctl -u %s »",
			unit, firstError(res, err), unit)
	}
	return fmt.Errorf("le rechargement a arrêté %s ; le service a été redémarré, "+
		"mais la cause reste à traiter (voir « journalctl -u %s »)", unit, unit)
}

// restarts rend le compteur de redémarrages automatiques de l'unité. Faux quand
// systemd ne le donne pas : le constat se borne alors à l'état de l'unité.
func (p *Provider) restarts(ctx context.Context, unit string) (int, bool) {
	res, err := p.systemctl(ctx, "show", unit, "-p", "NRestarts", "--value")
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if err != nil {
		return 0, false
	}
	return n, true
}

// ensureEnabled active une unité qui tourne sans être activée.
func (p *Provider) ensureEnabled(ctx context.Context, unit string) error {
	if p.unitEnabled(ctx, unit) {
		return nil
	}
	if res, err := p.systemctl(ctx, "enable", unit); err != nil {
		return fmt.Errorf("activation de %s, qui ne repartirait pas au redémarrage de la machine : %s",
			unit, firstError(res, err))
	}
	return nil
}

// UnitState est l'état d'une unité FPM tel que systemd le rend.
type UnitState struct {
	Active  bool
	Enabled bool
}

// Healthy dit que l'unité tourne et repartira au démarrage de la machine.
func (u UnitState) Healthy() bool { return u.Active && u.Enabled }

// Unit observe l'unité FPM d'une version. Un exécuteur simulé la dit saine : le
// mode préfixé n'a pas de systemd à interroger.
func (p *Provider) Unit(ctx context.Context, version string) UnitState {
	if p.Exec.Simulated() {
		return UnitState{Active: true, Enabled: true}
	}
	unit := config.PHPUnit(version)
	return UnitState{Active: p.unitActive(ctx, unit), Enabled: p.unitEnabled(ctx, unit)}
}

// Validate éprouve la configuration FPM d'une version (« php-fpm -t »).
func (p *Provider) Validate(ctx context.Context, version string) error {
	return p.validate(ctx, version)
}

// hasPools dit si FPM trouvera au moins un pool pour cette version : tout
// fichier .conf de pool.d, qu'il soit d'ogs ou de l'administrateur — c'est ce
// qu'inclut php-fpm.conf.
func (p *Provider) hasPools(version string) (bool, error) {
	entries, err := os.ReadDir(p.Paths.PHPPoolDir(version))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".conf" {
			return true, nil
		}
	}
	return false, nil
}

func (p *Provider) unitActive(ctx context.Context, unit string) bool {
	_, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{"is-active", "--quiet", unit}})
	return err == nil
}

func (p *Provider) unitEnabled(ctx context.Context, unit string) bool {
	_, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{"is-enabled", "--quiet", unit}})
	return err == nil
}

func (p *Provider) systemctl(ctx context.Context, args ...string) (exec.Result, error) {
	return p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: args})
}

// firstError privilégie la sortie d'erreur de la commande, plus parlante que le
// code de retour qu'elle a produit.
func firstError(res exec.Result, err error) string {
	if s := strings.TrimSpace(res.Stderr); s != "" {
		return s
	}
	return err.Error()
}

func describeListen(bp *PoolPlan) string {
	if bp.Unix {
		return "socket unix"
	}
	return "tcp " + bp.Listen
}

func describeVersions(versions []Version) string {
	if len(versions) == 0 {
		return "aucune"
	}
	out := ""
	for i, v := range versions {
		if i > 0 {
			out += ", "
		}
		out += v.Version
		if !v.Usable {
			out += " (inutilisable)"
		}
	}
	return out
}

// versionFromPoolPath retrouve la version depuis le chemin /etc/php/<v>/fpm/pool.d/x.conf.
func versionFromPoolPath(path string) string {
	dir := filepath.Dir(path)              // .../fpm/pool.d
	fpm := filepath.Dir(filepath.Dir(dir)) // .../<version>
	return filepath.Base(fpm)
}

// diffText produit un diff lisible entre le fichier présent et le rendu attendu.
func diffText(path string, expected []byte) (string, error) {
	current, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "nouveau fichier " + path, nil
		}
		return "", err
	}
	return fs.UnifiedDiff(string(current), string(expected), path), nil
}
