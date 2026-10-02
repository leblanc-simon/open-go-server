// Package firewall réconcilie ufw avec /etc/ogs/ufw.yaml (D72) : profils
// d'application dérivés de ce qui est installé, règles d'accès par profil,
// adresses de confiance, bannissements, politiques par défaut.
//
// ogs n'installe pas ufw et ne touche à rien sans ufw.yaml. Il ne réécrit
// jamais user.rules : toute règle passe par la commande ufw, seule à tenir ce
// fichier et le jeu de règles chargé en cohérence. Une règle est à lui si son
// commentaire porte sa marque ; les autres appartiennent à l'administrateur.
package firewall

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// RollbackUnit est l'unité transitoire qui défait une première activation que
// personne n'a confirmée.
const RollbackUnit = "ogs-ufw-rollback"

// Provider réconcilie le pare-feu.
type Provider struct {
	Paths config.Paths
	Exec  exec.Executor
	// Cfg est la déclaration chargée ; nil lorsque ufw.yaml n'existe pas.
	Cfg *config.UFW
	// LoadErr porte l'erreur de chargement d'un ufw.yaml présent mais invalide.
	// Elle n'arrête pas le reste du parc : le plan global en fait un
	// avertissement, et seules les commandes du pare-feu la rendent en code 3.
	LoadErr error
	// WebUnit est l'unité du moteur web retenu, qui rend le profil « web »
	// disponible.
	WebUnit string
	// WebUndecided dit qu'un serveur web est installé sans qu'un moteur soit
	// retenu — Apache et nginx à la fois, sans web.engine. C'est une inconnue,
	// non une absence : le profil web reste disponible, ses ports ne dépendant
	// pas du moteur.
	WebUndecided bool
	// Force passe outre un fichier de profils modifié à la main (§6.4), et rien
	// d'autre : c'est le --force de toutes les commandes, qu'on donne pour un
	// fichier retouché sans penser au pare-feu.
	Force bool
	// AllowLockout passe outre les gardes contre la coupure d'accès. C'est une
	// décision sur le pare-feu, distincte de Force : seules les commandes
	// « ogs firewall » la prennent, le plan global jamais.
	AllowLockout bool
	Now          func() time.Time
	// Admin rend la session d'administration courante. Nil : elle est cherchée
	// dans l'environnement de la session SSH.
	Admin func() (Session, bool)

	// Armed est posé par l'étape d'activation lorsqu'elle a programmé le retour
	// automatique : l'appelant doit alors dire qu'il faut confirmer.
	Armed time.Duration
}

// Session est la session SSH d'administration : l'adresse du client, et
// l'adresse et le port du serveur où elle est arrivée — ceux que la session
// suivante empruntera aussi.
type Session struct {
	Addr netip.Addr
	// Local est l'adresse du serveur que le client a jointe ; invalide
	// lorsqu'elle n'est pas connue.
	Local netip.Addr
	// Port est vide lorsqu'il n'est pas connu.
	Port string
}

// Name identifie le provider.
func (p *Provider) Name() string { return "firewall" }

// Managed dit si ogs gère ufw : ufw.yaml existe, valide ou non.
func (p *Provider) Managed() bool { return p.Cfg != nil || p.LoadErr != nil }

func (p *Provider) cfg() *config.UFW {
	if p.Cfg == nil {
		return &config.UFW{}
	}
	return p.Cfg
}

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Provider) simulated() bool { return p.Exec == nil || p.Exec.Simulated() }

// Observed est l'état d'ufw sur la machine.
type Observed struct {
	// Installed dit si ufw est présent ; le critère est son répertoire de
	// profils, que ogs écrit.
	Installed bool
	Enabled   bool
	IPv6      bool
	// Incoming, Outgoing et Routed sont les politiques en vigueur (allow, deny,
	// reject) ; Logging le niveau de journalisation.
	Incoming, Outgoing, Routed string
	Logging                    string
	Rules                      []ObservedRule
	// RollbackPending dit qu'une activation attend sa confirmation : son retour
	// automatique est programmé.
	RollbackPending bool
	// Unconfirmed dit qu'une activation n'a jamais été confirmée : le témoin posé
	// à l'armement du retour automatique est toujours là.
	Unconfirmed bool
	// PreviousProfiles porte les ports du fichier de profils actuel.
	PreviousProfiles map[string]string
}

// RollbackLost dit qu'une activation jamais confirmée tient sans filet : ufw est
// actif, et plus aucun retour n'est programmé — un redémarrage dans le délai a
// effacé l'unité transitoire, qui vivait sous /run.
func (o Observed) RollbackLost() bool { return o.Unconfirmed && o.Enabled && !o.RollbackPending }

// RolledBack dit qu'ufw est inactif après une activation jamais confirmée : le
// retour automatique a joué.
func (o Observed) RolledBack() bool { return o.Unconfirmed && !o.Enabled && !o.RollbackPending }

// unconfirmedMark est le témoin d'une activation que personne n'a confirmée. Il
// vit sous /var/lib/ogs, là où l'unité transitoire du retour vit sous /run : un
// redémarrage dans le délai efface celle-ci, ufw repart actif sans filet, et
// sans le témoin ni confirm, ni status, ni doctor n'en disaient rien.
func (p *Provider) unconfirmedMark() string { return filepath.Join(p.Paths.Lib(), "ufw-unconfirmed") }

// Observe relit l'état d'ufw dans ses fichiers : ceux-là mêmes que la commande
// ufw écrit, et qu'elle relit à chaque démarrage.
func (p *Provider) Observe(ctx context.Context) (Observed, error) {
	var obs Observed
	info, err := os.Stat(p.Paths.UFWAppsDir())
	obs.Installed = err == nil && info.IsDir()
	if !obs.Installed {
		return obs, nil
	}
	conf := keyValues(p.Paths.UFWConf(), "=")
	obs.Enabled = strings.EqualFold(conf["ENABLED"], "yes")
	obs.Logging = strings.ToLower(conf["LOGLEVEL"])
	defaults := keyValues(p.Paths.UFWDefaults(), "=")
	obs.IPv6 = !strings.EqualFold(defaults["IPV6"], "no")
	obs.Incoming = policyName(defaults["DEFAULT_INPUT_POLICY"])
	obs.Outgoing = policyName(defaults["DEFAULT_OUTPUT_POLICY"])
	obs.Routed = policyName(defaults["DEFAULT_FORWARD_POLICY"])

	v4, err := readRules(p.Paths.UFWUserRules(), 4)
	if err != nil {
		return obs, fmt.Errorf("lecture des règles ufw : %w", err)
	}
	v6, err := readRules(p.Paths.UFWUser6Rules(), 6)
	if err != nil {
		return obs, fmt.Errorf("lecture des règles ufw (IPv6) : %w", err)
	}
	obs.Rules = append(v4, v6...)
	if data, err := os.ReadFile(p.Paths.UFWProfilesFile()); err == nil {
		obs.PreviousProfiles = parseProfiles(data)
	}
	if !p.simulated() {
		res, _ := p.Exec.Run(ctx, exec.Command{Name: "systemctl",
			Args: []string{"is-active", RollbackUnit + ".timer"}})
		obs.RollbackPending = strings.TrimSpace(res.Stdout) == "active"
	}
	if _, err := os.Lstat(p.unconfirmedMark()); err == nil {
		obs.Unconfirmed = true
	}
	return obs, nil
}

// policyName traduit la politique iptables d'/etc/default/ufw.
func policyName(v string) string {
	switch strings.ToUpper(v) {
	case "ACCEPT":
		return "allow"
	case "DROP":
		return "deny"
	case "REJECT":
		return "reject"
	}
	return strings.ToLower(v)
}

// ValidationError rassemble ce qui interdit d'appliquer la déclaration : elle
// se traduit par le code 3, et rien n'est tenté.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "pare-feu :\n  " + strings.Join(e.Problems, "\n  ")
}

// run partage l'état d'une exécution entre les étapes du pare-feu : une règle
// qui n'a pas pu être posée interdit tout durcissement ultérieur, retraits
// compris — retirer l'ancienne adresse quand la nouvelle n'a pas été autorisée
// couperait l'accès que le changement voulait déplacer.
type run struct {
	failed []string
	// keep porte les profils dont une règle n'a pas pu être retirée : ils restent
	// définis, faute de quoi ufw refuserait à jamais de la retirer — il refuse une
	// application inconnue jusque dans « ufw delete ».
	keep map[string]string
}

func (r *run) guard(step string) error {
	if len(r.failed) == 0 {
		return nil
	}
	return fmt.Errorf("%s non appliqué : %d règle(s) n'ont pas pu être posées (%s) — durcir le pare-feu "+
		"sans elles pourrait couper un accès", step, len(r.failed), strings.Join(r.failed, ", "))
}

// Plan calcule les étapes du pare-feu. Toutes sont non critiques : le pare-feu
// est une ressource de parc, et son échec ne doit pas dérouler le reste du plan
// global. L'ordre, lui, est une décision :
//
//  1. retrait des règles dont le profil disparaît, tant qu'ufw le connaît ;
//  2. fichier de profils, puis « ufw app update » des profils dont les ports
//     changent ;
//  3. refus, placés en tête ;
//  4. accès et sorties ;
//  5. politiques qui s'ouvrent (vers « allow ») ;
//  6. retrait des règles devenues sans objet — après les ajouts et
//     l'ouverture, pour qu'un changement n'ouvre jamais de fenêtre sans accès ;
//  7. journalisation, politiques qui se ferment ;
//  8. activation, avec retour automatique faute de confirmation.
//
// Ce qui retire un accès — retraits, politiques qui se ferment, activation —
// refuse de s'appliquer si une règle a échoué.
func (p *Provider) Plan(ctx context.Context, projects []*model.Project, obs Observed) ([]*engine.Step, error) {
	if !p.Managed() {
		return nil, nil
	}
	if p.LoadErr != nil {
		return nil, &ValidationError{Problems: []string{p.LoadErr.Error()}}
	}
	if !obs.Installed {
		s := engine.NewStep("firewall/ufw", engine.Update, "pare-feu : ufw n'est pas installé")
		s.Critical = false
		s.Apply = func(context.Context) error {
			return fmt.Errorf("%s déclare un pare-feu, mais ufw est absent (%s introuvable) : "+
				"installer ufw, ou retirer ufw.yaml", p.Paths.UFWConfig(), p.Paths.UFWAppsDir())
		}
		return []*engine.Step{s}, nil
	}
	cfg := p.cfg()
	profiles := p.Profiles()
	desired, err := desiredRules(cfg, profiles)
	if err != nil {
		return nil, &ValidationError{Problems: []string{err.Error()}}
	}

	st := &run{}
	var steps []*engine.Step
	desiredKeys := map[string]bool{}
	for _, r := range desired {
		desiredKeys[r.Key()] = true
	}
	profileNames := map[string]bool{}
	for _, prof := range profiles {
		profileNames[prof.UFWName] = true
	}

	// Règles relues, regroupées par clé : une règle « any » existe dans les deux
	// familles, et une seule commande la pose ou la retire.
	byKey := map[string][]ObservedRule{}
	var staleKeys []string
	for _, o := range obs.Rules {
		if !o.Recognized {
			continue
		}
		k := o.Key()
		if len(byKey[k]) == 0 && o.Managed && !desiredKeys[k] {
			staleKeys = append(staleKeys, k)
		}
		byKey[k] = append(byKey[k], o)
	}
	sort.Strings(staleKeys)

	// Profils qu'une règle restante emploie encore : ils restent définis.
	kept := map[string]string{}
	for _, o := range obs.Rules {
		if o.App == "" || profileNames[o.App] || !strings.HasPrefix(o.App, "ogs-") {
			continue
		}
		if o.Managed && o.Recognized && !desiredKeys[o.Key()] {
			continue
		}
		if ports, ok := obs.PreviousProfiles[o.App]; ok {
			kept[o.App] = ports
		}
	}

	stale := func(first bool) {
		for _, k := range staleKeys {
			rules := byKey[k]
			r := rules[0].Rule
			vanishing := r.App != "" && !profileNames[r.App]
			if vanishing == first {
				steps = append(steps, p.deleteStep(r, st, obs.PreviousProfiles[r.App], vanishing))
			}
		}
	}

	// Politiques : celle qui s'ouvre (vers « allow ») passe avant le retrait des
	// règles qu'elle rend inutiles — retirer d'abord les sorties permises sous
	// une politique encore fermée couperait DNS et ACME le temps de l'apply ;
	// celle qui se ferme passe après les ajouts, et se garde.
	var loosen, tighten []*engine.Step
	for _, pol := range []struct{ dir, want, have string }{
		{"routed", cfg.RoutedPolicy(), obs.Routed},
		{"outgoing", cfg.OutgoingPolicy(), obs.Outgoing},
		{"incoming", cfg.IncomingPolicy(), obs.Incoming},
	} {
		if pol.want == "" || pol.want == pol.have {
			continue
		}
		opening := pol.want == "allow"
		step := p.commandStep("firewall/policy/"+pol.dir, engine.Update,
			fmt.Sprintf("politique %s : %s → %s", policyLabel(pol.dir), orDash(pol.have), pol.want),
			[]string{"default", pol.want, pol.dir},
			restoreArgs(pol.have != "", "default", pol.have, pol.dir), st, "la politique "+policyLabel(pol.dir), !opening)
		if opening {
			loosen = append(loosen, step)
		} else {
			tighten = append(tighten, step)
		}
	}

	stale(true)
	profStep, portsChanged, err := p.profilesStep(profiles, kept, obs, st)
	if err != nil {
		return nil, err
	}
	if profStep != nil {
		steps = append(steps, profStep)
	}
	for _, r := range desired {
		if s := p.ruleStep(r, byKey[r.Key()], obs, st); s != nil {
			steps = append(steps, s)
		}
	}
	steps = append(steps, loosen...)
	stale(false)

	if lvl := cfg.Logging; lvl != "" {
		want := lvl
		if want == "on" {
			want = "low"
		}
		if obs.Logging != want {
			steps = append(steps, p.commandStep("firewall/logging", engine.Update,
				fmt.Sprintf("journalisation d'ufw : %s → %s", orDash(obs.Logging), want),
				[]string{"logging", want}, restoreArgs(obs.Logging != "", "logging", obs.Logging), st,
				"la journalisation", false))
		}
	}
	steps = append(steps, tighten...)
	if s := p.stateStep(obs, st); s != nil {
		steps = append(steps, s)
	}

	// Changer les ports d'un profil qu'une règle emploie ferme l'ancien port :
	// « ufw app update » y porte la règle entière. C'est un resserrement au même
	// titre qu'un retrait — un Port de sshd mal relu, ou des ports déclarés à
	// tort, fermaient ainsi le port où sshd écoute sans qu'aucune garde ne joue.
	tightening := len(portsChanged) > 0 || tightens(steps)
	if err := p.validate(ctx, projects, profiles, desired, obs, tightening); err != nil {
		return nil, err
	}
	return steps, nil
}

// RefusedStep fait d'une déclaration refusée une étape non bloquante en échec.
// C'est la forme qu'elle prend dans le plan global : un pare-feu qu'on refuse
// de poser ne doit pas priver le parc du reste — la rotation des journaux, les
// prisons recalculées à la suppression d'un projet (D68). Rien n'est tenté sur
// ufw, et l'apply rend le code 6 en nommant le motif.
func RefusedStep(err error) *engine.Step {
	s := engine.NewStep("firewall/declaration", engine.Update, "pare-feu : déclaration refusée, ufw laissé en l'état")
	s.Critical = false
	s.Apply = func(context.Context) error { return err }
	return s
}

func policyLabel(dir string) string {
	switch dir {
	case "incoming":
		return "entrante"
	case "outgoing":
		return "sortante"
	}
	return "routée"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func restoreArgs(ok bool, args ...string) []string {
	if !ok {
		return nil
	}
	return args
}

// ufw exécute une commande ufw.
func (p *Provider) ufw(ctx context.Context, args ...string) error {
	if p.Exec == nil {
		return nil
	}
	res, err := p.Exec.Run(ctx, exec.Command{Name: "ufw", Args: args})
	if err != nil {
		return err
	}
	// ufw rend 0 sur certaines impossibilités qu'il se contente d'écrire.
	if out := res.Stdout + res.Stderr; strings.Contains(out, "ERROR:") {
		return fmt.Errorf("ufw %s : %s", strings.Join(args, " "), strings.TrimSpace(out))
	}
	return nil
}

// commandStep est une étape faite d'une commande ufw et de son inverse. Gardée,
// elle refuse de s'appliquer après l'échec d'une règle.
func (p *Provider) commandStep(resource string, action engine.Action, summary string,
	args, undo []string, st *run, label string, guarded bool) *engine.Step {
	s := engine.NewStep(resource, action, summary)
	s.Critical = false
	s.Diff = func() (string, error) { return commandLine(args), nil }
	s.Apply = func(ctx context.Context) error {
		if guarded {
			if err := st.guard(label); err != nil {
				return err
			}
		}
		return p.ufw(ctx, args...)
	}
	if undo != nil {
		s.Compensate = func(ctx context.Context) error { return p.ufw(ctx, undo...) }
	}
	return s
}

// ruleStep pose une règle voulue, ou en corrige le commentaire, ou replace un
// refus en tête.
func (p *Provider) ruleStep(r Rule, observed []ObservedRule, obs Observed, st *run) *engine.Step {
	families := r.Families(obs.IPv6)
	present := map[int]ObservedRule{}
	for _, o := range observed {
		present[o.Family] = o
	}
	missing, commentDiffers, misplaced := false, false, false
	var oldComment string
	for _, f := range families {
		o, ok := present[f]
		if !ok {
			missing = true
			continue
		}
		// Une règle de l'administrateur de même définition satisfait la
		// déclaration, et reste la sienne : lui donner la marque d'ogs — ufw
		// remplace le commentaire en place — ferait qu'ogs la retire le jour où
		// ufw.yaml cesse de la demander.
		if !o.Managed {
			continue
		}
		if o.Comment != r.Comment {
			commentDiffers = true
			oldComment = o.Comment
		}
		if r.Kind == KindBan && precededByAccess(obs.Rules, o) {
			misplaced = true
		}
	}
	resource := "firewall/rule/" + r.Key()
	label := r.Kind + " « " + r.Describe() + " »"
	switch {
	case misplaced:
		s := engine.NewStep(resource, engine.Update, "pare-feu : "+label+" replacé en tête des règles")
		s.Critical = false
		s.Diff = func() (string, error) {
			return commandLine(r.deleteArgs()) + "\n" + commandLine(r.addArgs(true)), nil
		}
		s.Apply = func(ctx context.Context) error {
			if err := p.ufw(ctx, r.deleteArgs()...); err != nil {
				st.failed = append(st.failed, r.Describe())
				return err
			}
			if err := p.ufw(ctx, r.addArgs(true)...); err != nil {
				st.failed = append(st.failed, r.Describe())
				return err
			}
			return nil
		}
		return s
	case missing:
		s := engine.NewStep(resource, engine.Create, "pare-feu : "+label)
		s.Critical = false
		args := r.addArgs(r.Kind == KindBan)
		s.Diff = func() (string, error) { return commandLine(args), nil }
		s.Apply = func(ctx context.Context) error {
			if err := p.ufw(ctx, args...); err != nil {
				st.failed = append(st.failed, r.Describe())
				return err
			}
			return nil
		}
		// Seule une règle entièrement nouvelle se retire : celle qui existait dans
		// une famille appartenait déjà au pare-feu avant cette exécution.
		if len(present) == 0 {
			s.Compensate = func(ctx context.Context) error { return p.ufw(ctx, r.deleteArgs()...) }
		}
		return s
	case commentDiffers:
		// ufw remplace en place le commentaire d'une règle identique : la
		// commande d'ajout, sans « prepend » — qui, elle, ignore une règle
		// existante —, suffit.
		s := engine.NewStep(resource, engine.Update, "pare-feu : commentaire de "+label)
		s.Critical = false
		args := r.addArgs(false)
		s.Diff = func() (string, error) {
			return fmt.Sprintf("commentaire : %q → %q\n%s", oldComment, r.Comment, commandLine(args)), nil
		}
		s.Apply = func(ctx context.Context) error { return p.ufw(ctx, args...) }
		prev := r
		prev.Comment = oldComment
		s.Compensate = func(ctx context.Context) error { return p.ufw(ctx, prev.addArgs(false)...) }
		return s
	}
	return nil
}

// precededByAccess dit si une règle autre qu'un refus précède ce refus dans sa
// famille : ufw retient la première règle qui correspond.
func precededByAccess(all []ObservedRule, ban ObservedRule) bool {
	for _, o := range all {
		if o.Family != ban.Family || o.Index >= ban.Index {
			continue
		}
		if !strings.HasPrefix(o.Verb, "deny") && !strings.HasPrefix(o.Verb, "reject") {
			return true
		}
	}
	return false
}

// deleteStep retire une règle d'ogs qui n'est plus voulue. Le retrait se garde
// comme un durcissement : il retire un accès. Celui d'une règle dont le profil
// disparaît (vanishing) garde le profil défini s'il échoue.
func (p *Provider) deleteStep(r Rule, st *run, ports string, vanishing bool) *engine.Step {
	summary := "pare-feu : retrait de " + r.Kind + " « " + r.Describe() + " »"
	if r.Kind == "" {
		summary = "pare-feu : retrait de « " + r.Describe() + " »"
		if r.App != "" {
			summary = "pare-feu : retrait de « " + r.App + " depuis " + r.From + " »"
		}
	}
	s := engine.NewStep("firewall/rule/"+r.Key(), engine.Delete, summary)
	s.Critical = false
	s.Diff = func() (string, error) { return commandLine(r.deleteArgs()), nil }
	s.Apply = func(ctx context.Context) error {
		err := st.guard("le retrait de « " + r.Describe() + " »")
		if err == nil {
			err = p.ufw(ctx, r.deleteArgs()...)
		}
		if err != nil && vanishing && ports != "" {
			if st.keep == nil {
				st.keep = map[string]string{}
			}
			st.keep[r.App] = ports
		}
		return err
	}
	s.Compensate = func(ctx context.Context) error {
		return p.ufw(ctx, r.addArgs(r.Action == "deny" && r.Direction == "in")...)
	}
	return s
}

// stateStep active ou désactive ufw.
func (p *Provider) stateStep(obs Observed, st *run) *engine.Step {
	cfg := p.cfg()
	switch {
	case cfg.IsActive() && !obs.Enabled:
		delay := cfg.Rollback()
		summary := "activation d'ufw"
		if delay > 0 {
			summary += fmt.Sprintf(" — retour automatique dans %s sans « ogs firewall confirm »", delay)
		}
		s := engine.NewStep("firewall/state", engine.Create, summary)
		s.Critical = false
		s.Diff = func() (string, error) {
			d := "ufw --force enable"
			if delay > 0 {
				d = fmt.Sprintf("systemd-run --unit=%s --on-active=%ds ufw --force disable\n", RollbackUnit,
					int(delay.Seconds())) + d
			}
			return d, nil
		}
		s.Apply = func(ctx context.Context) error {
			if err := st.guard("l'activation d'ufw"); err != nil {
				return err
			}
			if delay > 0 {
				if err := p.armRollback(ctx, delay); err != nil {
					return fmt.Errorf("activation refusée : le retour automatique n'a pas pu être programmé (%w) — "+
						"« rollbackAfter: 0 » dans ufw.yaml active sans ce filet", err)
				}
			}
			if err := p.ufw(ctx, "--force", "enable"); err != nil {
				p.disarm(ctx)
				return err
			}
			if delay > 0 {
				p.Armed = delay
			}
			return nil
		}
		s.Compensate = func(ctx context.Context) error {
			p.disarm(ctx)
			p.Armed = 0
			return p.ufw(ctx, "disable")
		}
		return s
	case !cfg.IsActive() && obs.Enabled:
		s := engine.NewStep("firewall/state", engine.Delete, "désactivation d'ufw (active: false)")
		s.Critical = false
		s.Diff = func() (string, error) { return "ufw disable", nil }
		s.Apply = func(ctx context.Context) error {
			if err := p.ufw(ctx, "disable"); err != nil {
				return err
			}
			// Une activation en attente n'a plus rien à confirmer : son témoin
			// ferait croire, ufw inactif, que le retour automatique a joué.
			p.disarm(ctx)
			return nil
		}
		s.Compensate = func(ctx context.Context) error { return p.ufw(ctx, "--force", "enable") }
		return s
	}
	return nil
}

// armRollback programme la désactivation d'ufw. C'est systemd qui la porte, non
// ogs : si la session qui active le pare-feu est celle qu'il coupe, plus rien
// de ce qu'elle a lancé ne tourne pour réparer.
func (p *Provider) armRollback(ctx context.Context, delay time.Duration) error {
	if p.simulated() {
		return nil
	}
	p.disarm(ctx)
	// Le témoin d'abord : un retour programmé sans lui se perdrait en silence au
	// premier redémarrage.
	now := p.now()
	note := fmt.Sprintf("# Activation d'ufw du %s, jamais confirmée : retour automatique prévu le %s.\n"+
		"# « ogs firewall confirm » retire ce fichier.\n", now.Format(time.RFC3339), now.Add(delay).Format(time.RFC3339))
	if err := fs.WriteAtomic(p.unconfirmedMark(), []byte(note), 0o600, fs.KeepOwner); err != nil {
		return fmt.Errorf("témoin de l'activation : %w", err)
	}
	_, err := p.Exec.Run(ctx, exec.Command{Name: "systemd-run", Args: []string{
		"--unit=" + RollbackUnit,
		"--description=ogs : désactivation d'ufw faute de confirmation",
		fmt.Sprintf("--on-active=%ds", int(delay.Seconds())),
		"--timer-property=AccuracySec=1s",
		"ufw", "--force", "disable",
	}})
	if err != nil {
		_ = os.Remove(p.unconfirmedMark())
	}
	return err
}

// disarm annule un retour automatique en attente et retire son témoin, et
// oublie un précédent échec : une unité transitoire en échec empêcherait d'en
// recréer une du même nom.
func (p *Provider) disarm(ctx context.Context) {
	if p.simulated() {
		return
	}
	_, _ = p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{"stop", RollbackUnit + ".timer"}})
	_, _ = p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{"reset-failed",
		RollbackUnit + ".timer", RollbackUnit + ".service"}})
	_ = os.Remove(p.unconfirmedMark())
}

// Confirm annule le retour automatique d'une activation et retire son témoin.
// Le booléen dit s'il y avait une activation à confirmer : en attente, ou dont
// le retour a été perdu — ufw actif, le minuteur effacé par un redémarrage. La
// nouvelle session d'où l'on confirme prouve l'accès dans les deux cas. Un
// retour qui a joué laisse le témoin : il n'y a rien à confirmer, et status et
// doctor disent pourquoi ufw est inactif.
func (p *Provider) Confirm(ctx context.Context, obs Observed) (bool, error) {
	switch {
	case obs.RollbackPending:
		if _, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl",
			Args: []string{"stop", RollbackUnit + ".timer"}}); err != nil {
			return true, err
		}
	case !obs.RollbackLost():
		return false, nil
	}
	if err := os.Remove(p.unconfirmedMark()); err != nil && !os.IsNotExist(err) {
		return true, fmt.Errorf("témoin de l'activation : %w", err)
	}
	return true, nil
}

// profilesStep écrit le fichier de profils, puis fait relire par ufw ceux dont
// les ports changent : une règle d'application porte les ports que le profil
// avait lorsqu'elle a été posée, et ne suit pas un profil réécrit. Il rend aussi
// ces profils, dont le changement ferme l'ancien port.
func (p *Provider) profilesStep(profiles []Profile, kept map[string]string, obs Observed,
	st *run) (*engine.Step, []string, error) {
	path := p.Paths.UFWProfilesFile()
	body := renderProfiles(profiles, kept)
	content := fs.Render(fs.Header{Project: "parc", Resource: "firewall/profiles", Generated: p.now()}, body, "#")
	state, err := fs.Inspect(path, content)
	if err != nil {
		return nil, nil, err
	}
	summary := fmt.Sprintf("profils ufw (%d disponible(s) sur %d)", countAvailable(profiles), len(profiles))
	forced := false
	switch state {
	case fs.StateCurrent:
		return nil, nil, nil
	case fs.StateHandEdited, fs.StateForeign:
		if !p.Force {
			s := engine.NewStep("firewall/profiles", engine.Update, summary+" — fichier modifié hors ogs")
			s.Critical = false
			s.Apply = func(context.Context) error {
				st.failed = append(st.failed, "profils")
				return fmt.Errorf("%s a été modifié en dehors d'ogs : relancer avec --force pour l'écraser "+
					"(l'original sera sauvegardé dans %s)", path, p.Paths.BackupsDir())
			}
			return s, nil, nil
		}
		forced = true
		summary += " — écrasement forcé"
	}
	newPorts := parseProfiles(body)
	var changed []string
	for name, ports := range newPorts {
		if old, ok := obs.PreviousProfiles[name]; ok && old != ports && usedBy(obs.Rules, name) {
			changed = append(changed, name)
		}
	}
	sort.Strings(changed)
	if len(changed) > 0 {
		summary += " ; ports modifiés : " + strings.Join(changed, ", ")
	}
	action := engine.Update
	if state == fs.StateAbsent {
		action = engine.Create
	}
	s := engine.NewStep("firewall/profiles", action, summary)
	s.Critical = false
	s.Diff = func() (string, error) {
		current, err := os.ReadFile(path)
		if err != nil {
			return "nouveau fichier " + path, nil
		}
		return fs.UnifiedDiff(string(current), string(content), path), nil
	}
	var previous []byte
	s.Apply = func(ctx context.Context) error {
		fail := func(err error) error {
			st.failed = append(st.failed, "profils")
			return err
		}
		content := content
		if len(st.keep) > 0 {
			all := map[string]string{}
			for k, v := range kept {
				all[k] = v
			}
			for k, v := range st.keep {
				all[k] = v
			}
			content = fs.Render(fs.Header{Project: "parc", Resource: "firewall/profiles", Generated: p.now()},
				renderProfiles(profiles, all), "#")
		}
		if data, err := os.ReadFile(path); err == nil {
			previous = data
		}
		if forced {
			if _, err := fs.BackupTo(path, p.Paths.BackupsDir(), p.now()); err != nil && !os.IsNotExist(err) {
				return fail(err)
			}
		}
		if err := fs.EnsureDir(filepath.Dir(path), 0o755, fs.KeepOwner); err != nil {
			return fail(err)
		}
		if err := fs.WriteAtomic(path, content, 0o644, fs.KeepOwner); err != nil {
			return fail(err)
		}
		for i, name := range changed {
			if err := p.ufw(ctx, "app", "update", name); err != nil {
				// Le profil en échec est ramené lui aussi : ufw a pu réécrire
				// user.rules avant d'échouer — au rechargement, par exemple.
				return fail(p.unwriteProfiles(path, previous, changed[:i+1], err))
			}
		}
		return nil
	}
	s.Compensate = func(ctx context.Context) error {
		if previous == nil {
			return os.Remove(path)
		}
		if err := fs.WriteAtomic(path, previous, 0o644, fs.KeepOwner); err != nil {
			return err
		}
		for _, name := range changed {
			_ = p.ufw(ctx, "app", "update", name)
		}
		return nil
	}
	return s, changed, nil
}

// unwriteProfiles défait l'écriture du fichier de profils quand « ufw app
// update » a échoué : le moteur ne compense jamais l'étape qui échoue, et un
// fichier laissé aux nouveaux ports passait pour conforme au plan suivant — qui
// ne rejouait plus la commande, la règle gardant l'ancien port pour toujours
// (Q-06). Rendu tel qu'il était, le fichier redit ce que portent les règles, et
// le plan suivant recalcule les profils à mettre à jour, gardes comprises. Ceux
// qui l'ont déjà été sont ramenés au fichier rendu, au mieux : un fichier qui
// dirait l'ancien port d'une règle passée au nouveau tromperait à son tour le
// plan suivant.
func (p *Provider) unwriteProfiles(path string, previous []byte, updated []string, cause error) error {
	var err error
	if previous == nil {
		if err = os.Remove(path); os.IsNotExist(err) {
			err = nil
		}
	} else {
		err = fs.WriteAtomic(path, previous, 0o644, fs.KeepOwner)
	}
	if err != nil {
		return fmt.Errorf("%w ; le fichier de profils n'a pas pu être rendu (%v) : le plan suivant le "+
			"dira conforme alors que les règles portent encore les anciens ports", cause, err)
	}
	// Une interruption a pu annuler le contexte : la remise en place n'en dépend pas.
	for _, name := range updated {
		_ = p.ufw(context.Background(), "app", "update", name)
	}
	return cause
}

func usedBy(rules []ObservedRule, app string) bool {
	for _, r := range rules {
		if r.App == app {
			return true
		}
	}
	return false
}

func countAvailable(profiles []Profile) int {
	n := 0
	for _, prof := range profiles {
		if prof.Available {
			n++
		}
	}
	return n
}

// validate refuse ce qui couperait un accès ou ne fonctionnerait pas. Les
// gardes contre la coupure cèdent à AllowLockout : l'exploitant qui a la
// console sous la main peut vouloir fermer SSH ; les autres contrôles, non.
// Elles ne jouent que si le plan resserre le pare-feu (tightening) : sans
// resserrement, rien de ce qui passe aujourd'hui ne cesse de passer. Et elles
// jugent ce qui restera ouvert : les règles voulues, et celles que le plan
// laisse en place (keptOpens).
func (p *Provider) validate(ctx context.Context, projects []*model.Project, profiles []Profile, desired []Rule,
	obs Observed, tightening bool) error {
	cfg := p.cfg()
	var problems, lockout []string

	byName, byUFW := map[string]Profile{}, map[string]Profile{}
	for _, prof := range profiles {
		byName[prof.Name] = prof
		byUFW[prof.UFWName] = prof
	}
	rulesFor := func(profile string) []Rule {
		var out []Rule
		for _, r := range desired {
			if r.Profile == profile && r.Direction == "in" && r.Action == "allow" {
				out = append(out, r)
			}
		}
		return out
	}

	if !obs.IPv6 {
		for _, r := range desired {
			if r.From != config.UFWAny && r.Families(true)[0] == 6 {
				problems = append(problems, fmt.Sprintf("%s : adresse IPv6 alors qu'IPV6=no dans %s",
					r.Describe(), p.Paths.UFWDefaults()))
			}
		}
	}
	if ftp := byName["ftp"]; ftp.Available && ftp.PassiveUnknown && len(rulesFor("ftp")) > 0 {
		problems = append(problems, "ftp : plage de ports passifs inconnue — la connexion passerait et chaque "+
			"transfert resterait bloqué ; la déclarer dans profiles.ftp.passive (par exemple 40000:40100) "+
			"ou dans la configuration du serveur FTP — ou, si le FTP ne sert pas, profiles.ftp.available: false")
	}

	filtering := cfg.IsActive() && cfg.IncomingPolicy() != "allow"
	ssh, sshRules := byName["ssh"], rulesFor("ssh")
	// Les ports où sshd écoute réellement, lus une fois, et seulement si une
	// garde en a besoin : aucune lecture de sshd_config ne couvre toutes ses
	// syntaxes. Vide, c'est que « ss » se tait — mode simulé, ss absent.
	var listening []string
	listened := false
	sshdPorts := func() []string {
		if !listened {
			listening, listened = p.sshdListening(ctx), true
		}
		return listening
	}
	// admits dit si une règle ssh voulue laisse passer cette adresse.
	admits := func(addr netip.Addr) bool {
		for _, r := range sshRules {
			if r.From == config.UFWAny {
				return true
			}
			if pr, err := config.ParseUFWAddress(r.From); err == nil && pr.Contains(addr) {
				return true
			}
		}
		return false
	}
	// wanted dit si une règle voulue ouvre l'un de ces ports depuis addr — depuis
	// une source quelconque si addr est invalide —, quel que soit son profil : un
	// profil déclaré sur un port de sshd est un accès comme un autre, et ne
	// regarder que le profil « ssh » refusait ce qui ne fermait rien.
	wanted := func(ports []string, addr netip.Addr) bool {
		for _, r := range desired {
			if r.Direction != "in" || r.Action != "allow" || r.App == "" || !overlaps(byUFW[r.App].Ports, ports) {
				continue
			}
			if !addr.IsValid() || r.From == config.UFWAny {
				return true
			}
			if pr, err := config.ParseUFWAddress(r.From); err == nil && pr.Contains(addr) {
				return true
			}
		}
		return false
	}

	// Un port où sshd écoute, que plus rien n'ouvre une fois le plan appliqué,
	// est l'accès que la politique par défaut coupe. La garde vaut quelle que
	// soit la disponibilité du profil : un profil ssh écarté, ou qu'aucune unité
	// ne signale, voit ses règles retirées aussi sûrement qu'un port mal
	// déclaré — et sans session SSH à protéger, rien d'autre ne le voyait.
	if filtering && tightening {
		for _, port := range sshdPorts() {
			if (ssh.Available && opens(ssh.Ports, port)) || wanted(tcpSpecs(port), netip.Addr{}) ||
				keptOpens(obs.Rules, byUFW, tcpSpecs(port), Session{}) {
				continue
			}
			if ssh.Available {
				lockout = append(lockout, fmt.Sprintf("sshd écoute sur le port %s, que le profil ssh n'ouvre pas "+
					"(%s), ni aucune autre règle — le déclarer dans profiles.ssh.ports", port, ssh.PortsSpec()))
			} else {
				lockout = append(lockout, fmt.Sprintf("sshd écoute sur le port %s, que plus rien n'ouvrirait : le "+
					"profil ssh n'est pas disponible (%s), et aucune autre règle n'ouvre ce port", port, ssh.Reason))
			}
		}
	}
	if filtering {
		// Une règle de l'administrateur est une source comme une autre : ogs ne
		// la retire jamais. L'ignorer faisait refuser un « ufw limit OpenSSH »
		// posé à la main à chaque apply, et l'on prenait l'habitude de lever la
		// garde.
		if ssh.Available && len(sshRules) == 0 {
			target := tcpSpecs(sshdPorts()...)
			if len(target) == 0 {
				target = ssh.Ports
			}
			if !wanted(target, netip.Addr{}) && !keptOpens(obs.Rules, byUFW, target, Session{}) {
				lockout = append(lockout, "ssh : aucune source autorisée alors que la politique entrante est "+
					cfg.IncomingPolicy()+" — plus personne ne se connecterait ; ajouter une adresse de confiance "+
					"(trusted) ou profiles.ssh.allow")
			}
		}
		// La garde porte sur ce que le plan laisse ouvert, non sur la seule
		// disponibilité détectée : un profil web tenu pour absent voit ses règles
		// retirées, et ferme le web aux validations aussi sûrement qu'un accès
		// restreint.
		if web := byName["web"]; usesHTTP01(projects) && (web.Available || managedRulesFor(obs.Rules, web.UFWName)) {
			open := false
			for _, r := range rulesFor("web") {
				if r.From == config.UFWAny {
					open = true
				}
			}
			forced := cfg.Profiles["web"].Available
			switch {
			case open:
			case web.Available:
				problems = append(problems, "web : un projet émet ses certificats par HTTP-01, dont les "+
					"validations viennent d'adresses que nul ne connaît d'avance — profiles.web.allow doit "+
					"comprendre « any »")
			case forced != nil && !*forced:
				problems = append(problems, "web : un projet émet ses certificats par HTTP-01, et « profiles.web."+
					"available: false » retirerait l'accès public au web, dont ses validations ont besoin — lever "+
					"ce forçage, ou émettre ces certificats par DNS-01")
			default:
				problems = append(problems, fmt.Sprintf("web : un projet émet ses certificats par HTTP-01, et le "+
					"profil web n'est pas disponible (%s) : le plan retirerait l'accès public au web — retenir le "+
					"moteur (web.engine), ou « profiles.web.available: true »", web.Reason))
			}
		}
	}

	if admin, ok := p.adminSession(); ok {
		for _, r := range desired {
			if r.Kind != KindBan {
				continue
			}
			if pr, err := config.ParseUFWAddress(r.From); err == nil && pr.Contains(admin.Addr) {
				lockout = append(lockout, fmt.Sprintf("la session d'administration vient de %s, que %s bannirait",
					admin.Addr, r.From))
			}
		}
		if filtering && tightening {
			// La session suivante doit passer, adresse et port ensemble. Ses ports
			// sont ceux où sshd écoute, si ss les dit ; à défaut, celui où la
			// session en cours est arrivée ; à défaut, ceux du profil. Le port de
			// la session ne compte pas quand ss dit que sshd n'y écoute plus : la
			// suivante n'y arrivera de toute façon pas, et le compter refusait le
			// plan même qui ouvrait le port où sshd a été déplacé.
			basis, target := "profil", ssh.Ports
			switch {
			case len(sshdPorts()) > 0:
				basis, target = "ss", tcpSpecs(sshdPorts()...)
			case admin.Port != "":
				basis, target = "session", tcpSpecs(admin.Port)
			}
			if !wanted(target, admin.Addr) && !keptOpens(obs.Rules, byUFW, target, admin) {
				lockout = append(lockout, sessionLockout(admin, basis, ssh, sshdPorts(),
					admits(admin.Addr), overlaps(ssh.Ports, target), viaInterface(obs.Rules, byUFW, target, admin)))
			}
		}
	}

	if !p.AllowLockout {
		for _, l := range lockout {
			problems = append(problems, l+" (« ogs firewall apply --allow-lockout » passe outre)")
		}
	}
	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

// sessionLockout compose le refus de la garde de la session. Il nomme ce qui
// manque, et seulement cela : admitted dit si une règle ssh voulue admet
// l'adresse, reachable si le profil ssh ouvre l'un des ports visés. Déduire le
// remède d'une règle qui admettait l'adresse sur un port sans rapport — le 80 —
// faisait conseiller de déclarer un port que le profil ouvrait déjà.
func sessionLockout(admin Session, basis string, ssh Profile, listening []string,
	admitted, reachable, viaIface bool) string {
	var msg string
	switch {
	case !ssh.Available:
		msg = fmt.Sprintf("la session d'administration vient de %s, et le profil ssh n'est pas disponible (%s) : "+
			"ni une règle voulue ni une règle de l'administrateur ne la laisseraient plus passer — l'appliquer "+
			"couperait la suivante", admin.Addr, ssh.Reason)
	case admitted && basis == "session":
		msg = fmt.Sprintf("la session d'administration est arrivée sur le port %s, que le profil ssh n'ouvre pas "+
			"(%s) : l'appliquer couperait la suivante — le déclarer dans profiles.ssh.ports",
			admin.Port, ssh.PortsSpec())
	case admitted && basis == "ss":
		msg = fmt.Sprintf("la session d'administration vient de %s, qu'aucune règle n'admettrait plus sur les "+
			"ports où sshd écoute (%s) : l'appliquer couperait la suivante — les déclarer dans profiles.ssh.ports",
			admin.Addr, strings.Join(listening, ", "))
	case admitted:
		msg = fmt.Sprintf("la session d'administration vient de %s, et le profil ssh n'ouvre aucun port TCP "+
			"(%s) : l'appliquer couperait la suivante — corriger profiles.ssh.ports", admin.Addr, ssh.PortsSpec())
	default:
		msg = fmt.Sprintf("la session d'administration vient de %s, qu'aucune règle du profil ssh n'autorise : "+
			"l'appliquer la couperait — ajouter cette adresse à trusted ou à profiles.ssh.allow", admin.Addr)
		switch {
		case reachable:
		case basis == "session":
			msg += fmt.Sprintf(", et le port %s, où elle est arrivée, à profiles.ssh.ports (%s)",
				admin.Port, ssh.PortsSpec())
		case basis == "ss":
			msg += fmt.Sprintf(", et les ports où sshd écoute (%s) à profiles.ssh.ports",
				strings.Join(listening, ", "))
		}
	}
	if viaIface {
		msg += " ; seule une règle de l'administrateur liée à une interface l'admettrait peut-être, et rien ne " +
			"dit par laquelle la session est arrivée"
	}
	return msg
}

// managedRulesFor dit si une règle d'ogs relue emploie ce profil : c'est une
// règle que le plan retire si le profil cesse d'être disponible.
func managedRulesFor(rules []ObservedRule, app string) bool {
	for _, r := range rules {
		if r.Managed && r.Recognized && r.App == app {
			return true
		}
	}
	return false
}

// keptOpens dit si une règle que le plan laisse en place ouvre l'un de ces
// ports TCP — tout port, si ports est vide. Une telle règle est celle de
// l'administrateur, qu'ogs ne touche jamais, ou une règle d'ogs retouchée qu'il
// ne sait plus retirer : l'accès qu'elle porte survit au plan, et les gardes
// contre la coupure qui l'ignoraient refusaient ce qui ne coupe rien.
//
// Sans session (s.Addr invalide), la question est « ce port restera-t-il ouvert
// du dehors ? », et toute autorisation entrante y répond, liée ou non à une
// interface ou à une adresse du serveur : l'accès par un VPN (« in on wg0 ») est
// un accès, et le compter pour rien refusait, depuis la console, chaque
// bannissement de qui administre par là. Pour une session, la règle doit
// admettre son chemin : son adresse, et l'adresse du serveur où elle est
// arrivée si la règle en vise une. Une interface, en revanche, rien ne dit
// laquelle la session a empruntée : la garde refuse plutôt que de parier, et le
// dit (viaInterface).
func keptOpens(rules []ObservedRule, profiles map[string]Profile, ports []string, s Session) bool {
	return keptRule(rules, profiles, ports, s, !s.Addr.IsValid())
}

// viaInterface dit si seule une règle laissée en place liée à une interface
// admettrait la session : keptOpens ne la compte pas.
func viaInterface(rules []ObservedRule, profiles map[string]Profile, ports []string, s Session) bool {
	return keptRule(rules, profiles, ports, s, true) && !keptRule(rules, profiles, ports, s, false)
}

// keptRule porte keptOpens ; anyInterface y compte les règles liées à une
// interface. Seule compte une autorisation (« allow », « limit », journalisée
// ou non), entrante, depuis tout port d'origine — une règle qui exige un port
// d'origine n'admet pas un client ordinaire. Sans session, ce qui ne sert que
// la boucle locale, qu'ufw laisse passer de toute façon, n'ouvre rien du dehors.
func keptRule(rules []ObservedRule, profiles map[string]Profile, ports []string, s Session, anyInterface bool) bool {
	for _, o := range rules {
		// Une règle d'ogs reconnue est voulue — comptée comme telle —, ou retirée.
		if o.Managed && o.Recognized {
			continue
		}
		f := o.fields
		if len(f) != 7 && len(f) != 9 {
			continue
		}
		verb, _, _ := strings.Cut(f[0], "_")
		proto, dport, dst, sport, src, dir := f[1], f[2], f[3], f[4], f[5], f[len(f)-1]
		if (verb != "allow" && verb != "limit") || sport != "any" {
			continue
		}
		// ufw range l'interface dans le sens : « in_wg0 ».
		if iface, bound := strings.CutPrefix(dir, "in_"); dir != "in" &&
			(!bound || !anyInterface || iface == "lo" || strings.Contains(iface, "!")) {
			continue
		}
		if !isAnyAddress(dst) {
			pr, ok := observedPrefix(dst)
			switch {
			case !ok:
				continue
			case s.Addr.IsValid():
				if !s.Local.IsValid() || !pr.Contains(s.Local) {
					continue
				}
			case pr.Addr().IsLoopback():
				continue
			}
		}
		if dport == "any" {
			dport = "1:65535"
		}
		specs := []string{dport + "/" + proto}
		if len(f) == 9 {
			if f[7] != "-" {
				continue
			}
			// « ufw app update » donne à toute règle d'un profil réécrit ses
			// nouveaux ports, celles de l'administrateur comprises.
			if prof, ok := profiles[strings.ReplaceAll(f[6], "%20", " ")]; ok {
				specs = prof.Ports
			}
		}
		if len(ports) > 0 && !overlaps(specs, ports) {
			continue
		}
		// Une règle IPv4 n'admet pas un client IPv6, « any » compris : chaque
		// famille a son fichier.
		pr, ok := observedPrefix(src)
		switch {
		case !ok:
		case s.Addr.IsValid():
			if pr.Contains(s.Addr) {
				return true
			}
		case !pr.Addr().IsLoopback():
			return true
		}
	}
	return false
}

// observedPrefix lit une adresse d'un tuple d'ufw, réseau ou adresse seule.
func observedPrefix(s string) (netip.Prefix, bool) {
	if pr, err := netip.ParsePrefix(s); err == nil {
		return pr, true
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(a, a.BitLen()), true
}

// tcpSpecs écrit des ports nus au format d'un profil.
func tcpSpecs(ports ...string) []string {
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, p+"/tcp")
	}
	return out
}

// tcpRanges lit des ports au format d'ufw (« 22/tcp », « 22,2222/tcp »,
// « 2200:2300/tcp ») en plages TCP. Un port sans protocole, ou « any », vaut
// pour les deux.
func tcpRanges(specs []string) [][2]int {
	var out [][2]int
	for _, spec := range specs {
		list, proto, _ := strings.Cut(spec, "/")
		if proto != "" && proto != "tcp" && proto != "any" {
			continue
		}
		for _, part := range strings.Split(list, ",") {
			lo, hi, isRange := strings.Cut(part, ":")
			if !isRange {
				hi = lo
			}
			a, errA := strconv.Atoi(lo)
			b, errB := strconv.Atoi(hi)
			if errA == nil && errB == nil && a <= b {
				out = append(out, [2]int{a, b})
			}
		}
	}
	return out
}

// overlaps dit si deux jeux de ports ont un port TCP en commun.
func overlaps(a, b []string) bool {
	rb := tcpRanges(b)
	for _, x := range tcpRanges(a) {
		for _, y := range rb {
			if x[0] <= y[1] && y[0] <= x[1] {
				return true
			}
		}
	}
	return false
}

// opens dit si des ports de profil ouvrent ce port TCP.
func opens(specs []string, port string) bool {
	if _, err := strconv.Atoi(port); err != nil {
		return false
	}
	return overlaps(specs, tcpSpecs(port))
}

// sshdListening rend les ports TCP où sshd écoute, lus par « ss ». En mode
// simulé, ou si ss manque, rien : le contrôle se tait plutôt que d'inventer.
//
// Ubuntu démarre sshd par activation de socket (ssh.socket, depuis 22.10) : le
// port est alors tenu par systemd, et « ss » n'y montre sshd qu'une fois une
// première connexion reçue — avant, rien. Les ports d'une ssh.socket active sont
// donc lus chez systemd. Une socket inactive ne dit rien : sshd peut tourner
// seul, sur d'autres ports, et ses ListenStream n'écoutent pas.
func (p *Provider) sshdListening(ctx context.Context) []string {
	if p.simulated() {
		return nil
	}
	var ports []string
	if res, err := p.Exec.Run(ctx, exec.Command{Name: "ss", Args: []string{"-Htlnp"}}); err == nil {
		ports = sshdPortsFromSS(res.Stdout)
	}
	if _, err := p.Exec.Run(ctx, exec.Command{
		Name: "systemctl", Args: []string{"is-active", "--quiet", "ssh.socket"}}); err != nil {
		return ports
	}
	res, err := p.Exec.Run(ctx, exec.Command{
		Name: "systemctl", Args: []string{"show", "ssh.socket", "--property=Listen", "--value"}})
	if err != nil {
		return ports
	}
	for _, port := range socketStreamPorts(res.Stdout) {
		ports = appendUnique(ports, port)
	}
	return ports
}

// sshdPortsFromSS relève les ports d'écoute des processus sshd dans une sortie
// de « ss -Htlnp ». Seul le démon écoute ; « sshd-session » ne fait que servir
// une connexion. sshd est cherché dans toute la liste des processus : sous
// ssh.socket, systemd partage le descripteur et peut être nommé le premier.
func sshdPortsFromSS(out string) []string {
	var ports []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, `("sshd",`) {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		local := f[3]
		i := strings.LastIndexByte(local, ':')
		if i < 0 {
			continue
		}
		ports = appendUnique(ports, local[i+1:])
	}
	return ports
}

// reSocketStream reconnaît une écoute TCP dans la propriété Listen d'une socket
// systemd : « [::]:22 (Stream) », « 0.0.0.0:2222 (Stream) ». Un ListenStream
// réduit à un port y est rendu sous la première forme.
var reSocketStream = regexp.MustCompile(`:(\d+) \(Stream\)`)

// socketStreamPorts relève les ports TCP d'une sortie de « systemctl show
// --property=Listen --value ».
func socketStreamPorts(out string) []string {
	var ports []string
	for _, m := range reSocketStream.FindAllStringSubmatch(out, -1) {
		ports = appendUnique(ports, m[1])
	}
	return ports
}

// tightens dit si les étapes du plan peuvent retirer un accès : activation,
// politique entrante, retrait d'une règle, nouveau refus. Le changement des
// ports d'un profil employé, que l'étape des profils ne laisse pas voir, est
// compté par Plan.
func tightens(steps []*engine.Step) bool {
	for _, s := range steps {
		switch {
		case s.Resource == "firewall/state" && s.Action == engine.Create,
			s.Resource == "firewall/policy/incoming",
			strings.HasPrefix(s.Resource, "firewall/rule/") && s.Action == engine.Delete,
			strings.HasPrefix(s.Resource, "firewall/rule/deny|") && s.Action == engine.Create:
			return true
		}
	}
	return false
}

// usesHTTP01 dit si un projet émet un certificat par HTTP-01, le défaut.
func usesHTTP01(projects []*model.Project) bool {
	for _, proj := range projects {
		for _, c := range proj.Spec.Certificates {
			if c.Challenge == "" || c.Challenge == "http-01" {
				return true
			}
		}
	}
	return false
}

// adminSession rend la session d'administration, en mode réel seulement.
func (p *Provider) adminSession() (Session, bool) {
	if p.Admin != nil {
		return p.Admin()
	}
	if p.simulated() {
		return Session{}, false
	}
	return DetectAdmin()
}

// DetectAdmin cherche la session SSH courante : l'adresse du client, et le port
// du serveur où elle est arrivée. sudo efface SSH_CONNECTION de l'environnement
// (env_reset, que « doctor » exige) : on remonte donc la chaîne des processus
// parents, dont l'environnement la porte encore — root peut le lire.
func DetectAdmin() (Session, bool) {
	if s, ok := parseSSHConnection(os.Getenv("SSH_CONNECTION")); ok {
		return s, true
	}
	pid := os.Getppid()
	for i := 0; i < 32 && pid > 1; i++ {
		if env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); err == nil {
			for _, kv := range strings.Split(string(env), "\x00") {
				if v, ok := strings.CutPrefix(kv, "SSH_CONNECTION="); ok {
					if s, ok := parseSSHConnection(v); ok {
						return s, true
					}
				}
			}
		}
		next, ok := parentPID(pid)
		if !ok {
			break
		}
		pid = next
	}
	return Session{}, false
}

// parseSSHConnection lit « client port-client serveur port-serveur ». L'adresse
// et le port du serveur sont ceux que sshd a vus, après toute translation : ce
// sont aussi ceux que filtre ufw, dont la chaîne d'entrée passe après la
// translation de destination.
func parseSSHConnection(v string) (Session, bool) {
	f := strings.Fields(v)
	if len(f) == 0 {
		return Session{}, false
	}
	a, err := netip.ParseAddr(f[0])
	if err != nil {
		return Session{}, false
	}
	s := Session{Addr: a.Unmap()}
	if len(f) >= 4 {
		if l, err := netip.ParseAddr(f[2]); err == nil {
			s.Local = l.Unmap()
		}
		if _, err := strconv.Atoi(f[3]); err == nil {
			s.Port = f[3]
		}
	}
	return s, true
}

// parentPID lit le parent d'un processus dans /proc/<pid>/stat. Le nom de la
// commande, entre parenthèses, peut contenir des espaces : on coupe après la
// dernière parenthèse.
func parentPID(pid int) (int, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0, false
	}
	var ppid int
	if _, err := fmt.Sscanf(f[1], "%d", &ppid); err != nil {
		return 0, false
	}
	return ppid, true
}

// IgnoreIP rend les adresses de confiance, pour les prisons de fail2ban : une
// adresse qui a accès à tout ne doit pas être bannie par une autre voie.
func (p *Provider) IgnoreIP() []string {
	if p.Cfg == nil {
		return nil
	}
	cfg := p.cfg()
	var out []string
	for _, e := range cfg.Trusted {
		addrs, err := cfg.Resolve(e)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if !a.Any {
				out = append(out, a.String())
			}
		}
	}
	return out
}

// Desired rend les règles voulues, pour l'affichage.
func (p *Provider) Desired() ([]Rule, error) { return desiredRules(p.cfg(), p.Profiles()) }
