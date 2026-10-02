package cli

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/provider/firewall"
	"leblanc.io/open-go-server/internal/provider/web"
)

// newFirewallCommand construit « ogs firewall » (D72).
func newFirewallCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "firewall",
		Short: "Gérer le pare-feu ufw depuis /etc/ogs/ufw.yaml",
		Long: "Le pare-feu est décrit par /etc/ogs/ufw.yaml : politiques par défaut, profils\n" +
			"d'application détectés sur la machine, sources nommées (à qui appartient une\n" +
			"adresse), adresses de confiance, bannissements. « ogs apply » le réconcilie avec\n" +
			"le reste du parc ; « ogs firewall apply » ne fait que lui.\n\n" +
			"Sans ufw.yaml, ogs ne touche pas à ufw. Une règle est à ogs si son commentaire\n" +
			"commence par « ogs: » ; les autres appartiennent à l'administrateur et restent\n" +
			"intactes. La première activation se défait d'elle-même faute de\n" +
			"« ogs firewall confirm » dans le délai (rollbackAfter, 5 minutes par défaut).\n\n" +
			"Une déclaration qui couperait l'accès SSH est refusée. --allow-lockout, propre à\n" +
			"« plan », « apply », « ban » et « unban », passe outre. --force garde son sens\n" +
			"ordinaire — écraser un fichier retouché : le fichier de profils, ou ufw.yaml pour\n" +
			"« init » — et ne lève jamais cette garde.",
	}
	cmd.AddCommand(
		newFirewallInitCommand(app),
		newFirewallStatusCommand(app),
		newFirewallProfilesCommand(app),
		newFirewallRulesCommand(app),
		newFirewallSourcesCommand(app),
		newFirewallWhoisCommand(app),
		newFirewallPlanCommand(app),
		newFirewallApplyCommand(app),
		newFirewallConfirmCommand(app),
		newFirewallBanCommand(app, true),
		newFirewallBanCommand(app, false),
	)
	return cmd
}

// firewallWebEngine choisit le moteur web pour le pare-feu. Isolé pour rester
// substituable en test : le mode préfixé se rabat sur Apache, et ne rend jamais
// l'échec du mode réel devant deux moteurs sans web.engine.
var firewallWebEngine = (*App).webEngine

// firewall construit le provider pour les commandes du pare-feu. Le moteur web
// n'est cherché que pour le profil « web » : son absence ne doit pas empêcher
// de lire l'état d'ufw. Un moteur introuvable n'est pas pour autant l'absence
// de serveur web : deux moteurs sans web.engine, c'est un serveur web qu'on ne
// sait pas nommer, et le profil web reste disponible — le tenir pour absent
// retirait l'accès public de tous les sites au premier bannissement.
func (a *App) firewall() (*firewall.Provider, error) {
	unit, undecided := "", false
	if eng, err := firewallWebEngine(a); err == nil {
		unit = eng.Unit()
	} else {
		undecided = webServerInstalled(a.Prefix)
	}
	fw, err := a.firewallProvider(unit)
	if err != nil {
		return nil, err
	}
	fw.WebUndecided = undecided
	if fw.LoadErr != nil {
		return nil, wrap(CodeValidation, fw.LoadErr)
	}
	return fw, nil
}

// webServerInstalled dit si l'un des moteurs est présent, au critère même de
// leur détection.
func webServerInstalled(prefix string) bool {
	for _, name := range []string{"apache", "nginx"} {
		if eng, err := web.NewEngine(name, prefix); err == nil && eng.Installed() {
			return true
		}
	}
	return false
}

// managedFirewall exige ufw.yaml.
func (a *App) managedFirewall() (*firewall.Provider, error) {
	fw, err := a.firewall()
	if err != nil {
		return nil, err
	}
	if !fw.Managed() {
		return nil, exitf(CodeDependency, "%s absent : ogs ne gère pas le pare-feu — "+
			"« ogs firewall init » en propose un", a.Paths.UFWConfig())
	}
	return fw, nil
}

func newFirewallInitCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Écrire un ufw.yaml de départ d'après ce qui est installé",
		Long: "Écrit /etc/ogs/ufw.yaml à partir des profils détectés. L'adresse de la session\n" +
			"d'administration, si elle est connue, y devient une source de confiance ; sinon\n" +
			"SSH reste ouvert à tous, à restreindre. Rien n'est appliqué.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := app.Paths.UFWConfig()
			if _, err := os.Stat(path); err == nil && !app.Force {
				return exitf(CodeConflict, "%s existe déjà (--force pour le remplacer)", path)
			}
			fw, err := app.firewallProviderWith(&config.UFW{})
			if err != nil {
				return err
			}
			admin, known := netip.Addr{}, false
			if app.Prefix == "" {
				var s firewall.Session
				s, known = firewall.DetectAdmin()
				admin = s.Addr
			}
			content := starterUFW(fw.Profiles(), admin, known, nowFunc())
			if err := fs.EnsureDir(app.Paths.Etc(), 0o700, fs.KeepOwner); err != nil {
				return wrap(CodeError, err)
			}
			if err := fs.WriteAtomic(path, content, 0o600, fs.KeepOwner); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ %s écrit. Le relire, puis : ogs firewall plan\n", path)
			if !known {
				app.warnf("⚠ adresse de la session inconnue : SSH y est ouvert à tous, à restreindre\n")
			}
			return nil
		},
	}
}

// firewallProviderWith construit un provider sur une déclaration donnée.
func (a *App) firewallProviderWith(cfg *config.UFW) (*firewall.Provider, error) {
	fw, err := a.firewall()
	if err != nil {
		return nil, err
	}
	fw.Cfg = cfg
	return fw, nil
}

// starterUFW compose un ufw.yaml de départ.
func starterUFW(profiles []firewall.Profile, admin netip.Addr, known bool, now time.Time) []byte {
	var b strings.Builder
	b.WriteString("# Pare-feu géré par ogs (D72) : « ogs firewall plan » montre ce qui serait fait.\n")
	b.WriteString("#\n# Profils détectés sur cette machine :\n")
	for _, p := range profiles {
		state := "absent    "
		if p.Available {
			state = "disponible"
		}
		fmt.Fprintf(&b, "#   %-20s %s  %-24s %s\n", p.Name, state, p.PortsSpec(), p.Reason)
	}
	b.WriteString("\npolicy:\n  incoming: deny\n  outgoing: allow\nlogging: low\n\n")
	if known {
		fmt.Fprintf(&b, "sources:\n  admin:\n    owner: \"poste d'administration (relevé le %s)\"\n    addresses: [%s]\n\n",
			now.Format("02/01/2006"), admin)
		b.WriteString("# Accès à tous les profils disponibles.\ntrusted:\n  - admin\n\n")
	} else {
		b.WriteString("# sources:\n#   bureau:\n#     owner: \"Agence de Nantes\"\n#     addresses: [203.0.113.10]\n\n")
		b.WriteString("# trusted:\n#   - bureau\n\n")
	}
	b.WriteString("profiles:\n")
	for _, p := range profiles {
		if !p.Available {
			continue
		}
		switch p.Name {
		case "web":
			b.WriteString("  web:\n    allow: [any]\n")
		case "ssh":
			if known {
				b.WriteString("  ssh:\n    allow: []          # trusted suffit ; « any » l'ouvrirait à tous\n")
			} else {
				b.WriteString("  ssh:\n    allow:\n      - address: any\n        comment: \"à restreindre dès que possible\"\n")
			}
		case "ftp":
			if p.PassiveUnknown {
				b.WriteString("  ftp:\n    passive: \"40000:40100\"   # à accorder à la configuration du serveur FTP\n")
			}
		}
	}
	b.WriteString("\nban: []\n")
	return []byte(b.String())
}

func newFirewallStatusCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Afficher l'état d'ufw et l'écart avec ufw.yaml",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fw, err := app.firewall()
			if err != nil {
				return err
			}
			obs, err := fw.Observe(cmd.Context())
			if err != nil {
				return wrap(CodeError, err)
			}
			type ligne struct {
				Item     string `json:"item" yaml:"item"`
				Observed string `json:"observed" yaml:"observed"`
				Desired  string `json:"desired" yaml:"desired"`
			}
			cfg := &config.UFW{}
			if fw.Managed() {
				cfg = fw.Cfg
			}
			desired := func(v string) string {
				if !fw.Managed() {
					return "non géré"
				}
				return orTiret(v)
			}
			active := "oui"
			if !cfg.IsActive() {
				active = "non"
			}
			enabled := "non"
			if obs.Enabled {
				enabled = "oui"
			}
			if !obs.Installed {
				enabled = "ufw absent"
			}
			lignes := []ligne{
				{"actif", enabled, desired(active)},
				{"entrée", orTiret(obs.Incoming), desired(cfg.IncomingPolicy())},
				{"sortie", orTiret(obs.Outgoing), desired(cfg.OutgoingPolicy())},
				{"routage", orTiret(obs.Routed), desired(cfg.RoutedPolicy())},
				{"journalisation", orTiret(obs.Logging), desired(cfg.Logging)},
			}
			managed, foreign := 0, 0
			for _, r := range obs.Rules {
				if r.Managed {
					managed++
				} else {
					foreign++
				}
			}
			lignes = append(lignes, ligne{"règles", fmt.Sprintf("%d d'ogs, %d de l'administrateur", managed, foreign), ""})
			switch {
			case obs.RollbackPending:
				lignes = append(lignes, ligne{"confirmation", "en attente", "ogs firewall confirm"})
			case obs.RollbackLost():
				lignes = append(lignes, ligne{"confirmation", "jamais confirmée, retour automatique perdu",
					"ogs firewall confirm"})
			case obs.RolledBack():
				lignes = append(lignes, ligne{"confirmation", "défaite par le retour automatique", "ogs firewall apply"})
			}
			return app.emit("FirewallStatus", lignes, nil, func(tw *tabwriter.Writer) {
				fmt.Fprintln(tw, "\tCONSTATÉ\tVOULU")
				for _, l := range lignes {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", l.Item, l.Observed, l.Desired)
				}
			})
		},
	}
}

func orTiret(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func newFirewallProfilesCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "profiles",
		Short: "Lister les profils d'application, leur disponibilité et leurs sources",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fw, err := app.firewall()
			if err != nil {
				return err
			}
			rules, err := fw.Desired()
			if err != nil {
				return exitf(CodeValidation, "%v", err)
			}
			type ligne struct {
				Name      string   `json:"name" yaml:"name"`
				UFWName   string   `json:"ufwName" yaml:"ufwName"`
				Available bool     `json:"available" yaml:"available"`
				Ports     string   `json:"ports" yaml:"ports"`
				Sources   []string `json:"sources" yaml:"sources"`
				Reason    string   `json:"reason" yaml:"reason"`
			}
			var lignes []ligne
			for _, p := range fw.Profiles() {
				l := ligne{Name: p.Name, UFWName: p.UFWName, Available: p.Available, Ports: p.PortsSpec(),
					Reason: p.Reason, Sources: []string{}}
				for _, r := range rules {
					if r.Profile == p.Name {
						l.Sources = append(l.Sources, r.From)
					}
				}
				lignes = append(lignes, l)
			}
			return app.emit("FirewallProfiles", lignes, nil, func(tw *tabwriter.Writer) {
				fmt.Fprintln(tw, "PROFIL\tDISPONIBLE\tPORTS\tSOURCES\tRAISON")
				for _, l := range lignes {
					dispo := "non"
					if l.Available {
						dispo = "oui"
					}
					sources := strings.Join(l.Sources, ", ")
					if sources == "" {
						sources = "—"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", l.Name, dispo, l.Ports, sources, l.Reason)
				}
			})
		},
	}
}

func newFirewallRulesCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "rules",
		Short: "Lister les règles d'ufw, celles d'ogs et celles de l'administrateur",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fw, err := app.firewall()
			if err != nil {
				return err
			}
			obs, err := fw.Observe(cmd.Context())
			if err != nil {
				return wrap(CodeError, err)
			}
			type ligne struct {
				Family  string `json:"family" yaml:"family"`
				Owner   string `json:"owner" yaml:"owner"`
				Rule    string `json:"rule" yaml:"rule"`
				Comment string `json:"comment" yaml:"comment"`
			}
			var lignes []ligne
			for _, r := range obs.Rules {
				owner := "administrateur"
				switch {
				case r.Managed && r.Recognized:
					owner = "ogs"
				case r.Managed:
					owner = "ogs (retouchée)"
				}
				desc := r.Raw
				if r.Recognized {
					desc = r.Action + " " + r.Direction + " " + describeObserved(r)
				}
				if i := strings.Index(desc, " comment="); i >= 0 {
					desc = desc[:i]
				}
				lignes = append(lignes, ligne{Family: fmt.Sprintf("IPv%d", r.Family), Owner: owner,
					Rule: desc, Comment: r.Comment})
			}
			return app.emit("FirewallRules", lignes, nil, func(tw *tabwriter.Writer) {
				fmt.Fprintln(tw, "FAMILLE\tPROPRIÉTAIRE\tRÈGLE\tCOMMENTAIRE")
				for _, l := range lignes {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", l.Family, l.Owner, l.Rule, l.Comment)
				}
			})
		},
	}
}

func describeObserved(r firewall.ObservedRule) string {
	switch {
	case r.App != "":
		return r.App + " depuis " + r.From
	case r.Direction == "in":
		return "depuis " + r.From
	}
	if r.Proto == "any" {
		return "vers " + r.Port
	}
	return "vers " + r.Port + "/" + r.Proto
}

func newFirewallSourcesCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "sources",
		Short: "Lister les sources nommées : à qui appartiennent les adresses",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fw, err := app.managedFirewall()
			if err != nil {
				return err
			}
			type ligne struct {
				Name        string   `json:"name" yaml:"name"`
				Owner       string   `json:"owner" yaml:"owner"`
				Description string   `json:"description,omitempty" yaml:"description,omitempty"`
				Addresses   []string `json:"addresses" yaml:"addresses"`
			}
			var lignes []ligne
			for _, name := range fw.Cfg.SortedSources() {
				src := fw.Cfg.Sources[name]
				lignes = append(lignes, ligne{name, src.Owner, src.Description, src.Addresses})
			}
			return app.emit("FirewallSources", lignes, nil, func(tw *tabwriter.Writer) {
				fmt.Fprintln(tw, "SOURCE\tPROPRIÉTAIRE\tADRESSES\tDESCRIPTION")
				for _, l := range lignes {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", l.Name, orTiret(l.Owner),
						strings.Join(l.Addresses, ", "), l.Description)
				}
			})
		},
	}
}

func newFirewallWhoisCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "whois <adresse>",
		Short: "Dire à qui appartient une adresse et ce que le pare-feu lui permet",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fw, err := app.managedFirewall()
			if err != nil {
				return err
			}
			addr, err := netip.ParseAddr(strings.TrimSpace(args[0]))
			if err != nil {
				return exitf(CodeUsage, "%q n'est pas une adresse IP", args[0])
			}
			addr = addr.Unmap()
			type ligne struct {
				Kind   string `json:"kind" yaml:"kind"`
				Detail string `json:"detail" yaml:"detail"`
			}
			var lignes []ligne
			for _, name := range fw.Cfg.SortedSources() {
				src := fw.Cfg.Sources[name]
				for _, a := range src.Addresses {
					if p, err := config.ParseUFWAddress(a); err == nil && p.Contains(addr) {
						detail := name + " (" + a + ")"
						if src.Owner != "" {
							detail += " — " + src.Owner
						}
						if src.Description != "" {
							detail += " : " + src.Description
						}
						lignes = append(lignes, ligne{"source", detail})
					}
				}
			}
			rules, err := fw.Desired()
			if err != nil {
				return exitf(CodeValidation, "%v", err)
			}
			for _, r := range rules {
				if r.Direction != "in" || r.From == config.UFWAny {
					continue
				}
				if p, err := config.ParseUFWAddress(r.From); err == nil && p.Contains(addr) {
					lignes = append(lignes, ligne{r.Kind, r.Describe() + " — " + r.Comment})
				}
			}
			if len(lignes) == 0 {
				lignes = append(lignes, ligne{"—", "adresse inconnue d'ufw.yaml : seules les règles ouvertes à tous la concernent"})
			}
			return app.emit("FirewallWhois", lignes, nil, func(tw *tabwriter.Writer) {
				fmt.Fprintf(tw, "%s\n", addr)
				for _, l := range lignes {
					fmt.Fprintf(tw, "  %s\t%s\n", l.Kind, l.Detail)
				}
			})
		},
	}
}

// firewallPlan calcule le plan du pare-feu seul. Il se passe de l'attelage
// complet des providers : un moteur web absent, ou deux moteurs sans
// « web.engine », ne doivent pas empêcher de bannir une adresse. allowLockout
// lève les gardes contre la coupure : c'est la seule voie qui le fasse, le
// --force des fichiers retouchés n'en ayant pas le pouvoir.
func (a *App) firewallPlan(cmd *cobra.Command, allowLockout bool) (*engine.Plan, *firewall.Provider, error) {
	projects, err := a.loadProjects()
	if err != nil {
		return nil, nil, err
	}
	fw, err := a.managedFirewall()
	if err != nil {
		return nil, nil, err
	}
	fw.AllowLockout = allowLockout
	steps, err := a.firewallSteps(cmd.Context(), fw, projects, true)
	if err != nil {
		return nil, nil, err
	}
	plan := &engine.Plan{Project: "pare-feu"}
	for _, s := range steps {
		plan.Add(s)
	}
	return plan, fw, nil
}

// allowLockoutFlag déclare --allow-lockout sur une commande du pare-feu.
func allowLockoutFlag(cmd *cobra.Command, v *bool) {
	cmd.Flags().BoolVar(v, "allow-lockout", false,
		"passer outre les gardes contre la coupure d'accès (session SSH, ports de sshd, SSH fermé à tous)")
}

func newFirewallPlanCommand(app *App) *cobra.Command {
	var allowLockout bool
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Calculer les changements du pare-feu, sans rien appliquer",
		RunE: func(cmd *cobra.Command, _ []string) error {
			plan, _, err := app.firewallPlan(cmd, allowLockout)
			if err != nil {
				return err
			}
			var items []planStep
			for _, s := range plan.Mutating() {
				items = append(items, planStep{Project: "pare-feu", Action: s.Action.String(),
					Resource: s.Resource, Summary: s.Summary, Critical: s.Critical})
			}
			if app.Format != "table" {
				return wrap(CodeError, app.emit("Plan", items, nil, nil))
			}
			app.printf("pare-feu\n%s", plan.String())
			if app.Verbose > 0 {
				for _, s := range plan.Mutating() {
					if s.Diff == nil {
						continue
					}
					if d, err := s.Diff(); err == nil && d != "" {
						app.printf("\n%s\n%s\n", s.Summary, indent(d, "    "))
					}
				}
			}
			app.printf("\n%d changement(s).\n", len(plan.Mutating()))
			return nil
		},
	}
	allowLockoutFlag(cmd, &allowLockout)
	return cmd
}

func newFirewallApplyCommand(app *App) *cobra.Command {
	var allowLockout bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Appliquer ufw.yaml au pare-feu, et à lui seul",
		Long: "Réconcilie le seul pare-feu, sans le reste du parc. Les prisons fail2ban, dont\n" +
			"l'« ignoreip » reprend les adresses de confiance, suivent au prochain « ogs apply ».",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lock, err := engine.Acquire(app.Paths.GlobalLock())
			if err != nil {
				var locked engine.ErrLocked
				if errors.As(err, &locked) {
					return wrap(CodeLocked, err)
				}
				return wrap(CodeError, err)
			}
			defer lock.Release()
			if err := app.openStore(); err != nil {
				return err
			}
			return app.applyFirewall(cmd, allowLockout)
		},
	}
	allowLockoutFlag(cmd, &allowLockout)
	return cmd
}

// applyFirewall applique le plan du pare-feu. Le verrou global et la base
// d'état sont à la charge de l'appelant.
func (a *App) applyFirewall(cmd *cobra.Command, allowLockout bool) error {
	plan, fw, err := a.firewallPlan(cmd, allowLockout)
	if err != nil {
		return err
	}
	if plan.Empty() {
		a.printf("= pare-feu : aucun changement\n")
		return nil
	}
	a.printf("pare-feu\n%s", plan.String())
	if a.DryRun {
		a.printf("--dry-run : rien n'a été appliqué.\n")
		return nil
	}
	if err := a.confirm(fmt.Sprintf("appliquer %d changement(s) au pare-feu ?", len(plan.Mutating()))); err != nil {
		return err
	}
	report, err := engine.Execute(cmd.Context(), plan, engine.Options{
		OnStep: func(s *engine.Step, stepErr error) {
			if stepErr == nil {
				a.printf("  ✓ %s\n", s.Summary)
			}
		},
		OnWarning: func(s *engine.Step, stepErr error) {
			a.warnf("  ⚠ %s : %v\n", s.Summary, stepErr)
		},
		Audit: func(s *engine.Step, outcome, detail string) {
			_ = a.Audit.Record("", s.Resource, "firewall."+s.Action.String(), outcome, detail)
		},
	})
	if err != nil {
		return wrap(CodeError, err)
	}
	a.announceRollback(fw)
	if len(report.Warnings) > 0 {
		return exitf(CodePartial, "terminé avec %d avertissement(s)", len(report.Warnings))
	}
	a.printf("✓ pare-feu : %d étape(s) appliquée(s) (audit %s)\n", report.Applied(), a.Audit.RunID())
	return nil
}

func newFirewallConfirmCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "confirm",
		Short: "Confirmer l'activation d'ufw et annuler son retour automatique",
		Long: "À lancer depuis une NOUVELLE session SSH : c'est elle qui prouve que le pare-feu\n" +
			"laisse passer l'administration. Une session ouverte avant l'activation survit à\n" +
			"des règles qui refuseraient la suivante.\n\n" +
			"Un redémarrage dans le délai efface le retour automatique, et ufw repart actif :\n" +
			"l'activation reste alors à confirmer, ce que « status » et « doctor » signalent.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// La confirmation ne lit pas ufw.yaml : un fichier retouché, voire cassé,
			// depuis l'activation ne doit pas empêcher de garder un pare-feu qui
			// fonctionne — faute de quoi il retomberait de lui-même.
			fw := &firewall.Provider{Paths: app.Paths, Exec: app.Exec}
			obs, err := fw.Observe(cmd.Context())
			if err != nil {
				return wrap(CodeError, err)
			}
			pending, err := fw.Confirm(cmd.Context(), obs)
			if err != nil {
				return wrap(CodeError, err)
			}
			if !pending {
				// ufw inactif alors qu'ufw.yaml le veut actif, c'est d'ordinaire un
				// retour automatique qui vient de jouer : répondre « rien à
				// confirmer » laissait croire le pare-feu en place, alors qu'il ne
				// filtre plus rien — et un script ne pouvait pas le distinguer
				// d'une confirmation. ufw.yaml n'est lu que pour dire si l'inactivité
				// est voulue : cassé, il ne décide rien.
				if obs.Installed && !obs.Enabled {
					cfg, present, lerr := config.LoadUFW(app.Paths.UFWConfig())
					switch {
					case lerr == nil && !present:
					case lerr == nil && !cfg.IsActive():
						app.printf("aucune activation en attente de confirmation : ufw est inactif, comme " +
							"ufw.yaml le demande (active: false)\n")
						return nil
					case obs.RolledBack():
						app.warnf("⚠ ufw est inactif : le retour automatique a défait l'activation, faute de " +
							"confirmation dans le délai — « ogs firewall apply » la refait, à confirmer depuis une " +
							"nouvelle session\n")
						return exitf(CodePartial, "ufw est inactif : rien à confirmer")
					default:
						app.warnf("⚠ ufw est inactif : aucune activation à confirmer — le retour automatique " +
							"l'a défaite si le délai est échu, ou ufw n'a pas encore été activé ; « ogs firewall " +
							"apply » l'active\n")
						return exitf(CodePartial, "ufw est inactif : rien à confirmer")
					}
				}
				app.printf("aucune activation en attente de confirmation\n")
				return nil
			}
			if app.Audit != nil {
				_ = app.Audit.Record("", "firewall/state", "firewall.confirm", "ok", "")
			}
			if obs.RollbackLost() {
				app.printf("✓ activation d'ufw confirmée — son retour automatique avait déjà disparu (un " +
					"redémarrage l'efface) : ufw tenait sans filet\n")
				return nil
			}
			app.printf("✓ activation d'ufw confirmée : le retour automatique est annulé\n")
			return nil
		},
	}
}

// newFirewallBanCommand construit « ban » et « unban ». Elles écrivent ufw.yaml
// — il reste la source de vérité (D1) — puis appliquent le pare-feu, sauf
// --no-apply. L'écriture passe par l'arbre du document : les commentaires de
// l'exploitant, qui disent souvent pourquoi une adresse est là, survivent.
func newFirewallBanCommand(app *App, ban bool) *cobra.Command {
	var (
		comment      string
		noApply      bool
		allowLockout bool
	)
	use, short := "ban <adresse|source>", "Bannir une adresse, un réseau ou une source nommée"
	if !ban {
		use, short = "unban <adresse|source>", "Lever un bannissement"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := app.Paths.UFWConfig()
			if _, err := os.Stat(path); err != nil {
				return exitf(CodeDependency, "%s absent : ogs ne gère pas le pare-feu — "+
					"« ogs firewall init » en propose un", path)
			}
			entry := config.ParseUFWEntry(args[0])
			entry.Comment = comment
			if entry.Address != "" {
				p, err := config.ParseUFWAddress(entry.Address)
				if err != nil {
					return exitf(CodeUsage, "%v", err)
				}
				entry.Address = config.FormatUFWPrefix(p)
			}
			lock, err := engine.Acquire(app.Paths.GlobalLock())
			if err != nil {
				var locked engine.ErrLocked
				if errors.As(err, &locked) {
					return wrap(CodeLocked, err)
				}
				return wrap(CodeError, err)
			}
			defer lock.Release()

			changed, err := editBanList(path, entry, ban)
			if err != nil {
				return err
			}
			if !changed {
				if ban {
					app.printf("= %s est déjà bannie\n", entry.Label())
				} else {
					app.printf("= %s n'est pas bannie dans %s\n", entry.Label(), path)
				}
			} else if ban {
				app.printf("✓ %s ajoutée aux bannissements de %s\n", entry.Label(), path)
			} else {
				app.printf("✓ %s retirée des bannissements de %s\n", entry.Label(), path)
			}
			if noApply {
				return nil
			}
			if err := app.openStore(); err != nil {
				return err
			}
			return app.applyFirewall(cmd, allowLockout)
		},
	}
	if ban {
		cmd.Flags().StringVar(&comment, "comment", "", "commentaire de la règle (sinon engendré)")
	}
	cmd.Flags().BoolVar(&noApply, "no-apply", false, "écrire ufw.yaml sans appliquer")
	allowLockoutFlag(cmd, &allowLockout)
	return cmd
}

// editBanList ajoute ou retire une entrée de la liste « ban » d'ufw.yaml. Le
// document modifié est validé avant d'être écrit : une adresse bannie qui en
// recouvre une autorisée est refusée ici, non découverte à l'apply.
func editBanList(path string, entry config.UFWEntry, add bool) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, wrap(CodeError, err)
	}
	file, err := parser.ParseBytes(data, parser.ParseComments)
	if err != nil {
		return false, exitf(CodeValidation, "%s : %v", path, err)
	}
	if len(file.Docs) == 0 || file.Docs[0].Body == nil {
		return false, exitf(CodeValidation, "%s est vide", path)
	}
	root, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		return false, exitf(CodeValidation, "%s : un document de clés est attendu", path)
	}
	var seq *ast.SequenceNode
	var holder *ast.MappingValueNode
	for _, mv := range root.Values {
		// La valeur du jeton, non le texte du nœud : « ban:   # liste noire »
		// rendrait la clé avec son commentaire.
		if tok := mv.Key.GetToken(); tok != nil && tok.Value == "ban" {
			holder = mv
			if s, ok := mv.Value.(*ast.SequenceNode); ok {
				seq = s
			}
		}
	}

	same := func(n ast.Node) bool {
		var e config.UFWEntry
		if err := yaml.NodeToValue(n, &e); err != nil {
			return false
		}
		if e.Source != "" || entry.Source != "" {
			return e.Source == entry.Source && e.Address == entry.Address
		}
		a, err1 := config.ParseUFWAddress(e.Address)
		b, err2 := config.ParseUFWAddress(entry.Address)
		return err1 == nil && err2 == nil && a == b
	}

	changed := false
	if add {
		if seq != nil {
			for _, n := range seq.Values {
				if same(n) {
					return false, nil
				}
			}
		}
		node, err := yaml.ValueToNode(entry.YAMLValue())
		if err != nil {
			return false, wrap(CodeError, err)
		}
		switch {
		case seq != nil && !seq.IsFlowStyle:
			seq.Values = append(seq.Values, node)
		default:
			// Liste absente, vide ou en ligne (« ban: [] ») : elle est réécrite en
			// bloc, ses éléments repris un à un.
			var values []any
			if seq != nil {
				for _, n := range seq.Values {
					var v any
					if err := yaml.NodeToValue(n, &v); err == nil {
						values = append(values, v)
					}
				}
			}
			values = append(values, entry.YAMLValue())
			fresh, err := yaml.ValueToNode(map[string]any{"ban": values})
			if err != nil {
				return false, wrap(CodeError, err)
			}
			newMV := fresh.(*ast.MappingNode).Values[0]
			if holder != nil {
				holder.Value = newMV.Value
			} else {
				root.Values = append(root.Values, newMV)
			}
		}
		changed = true
	} else if seq != nil {
		kept := seq.Values[:0]
		for _, n := range seq.Values {
			if same(n) {
				changed = true
				continue
			}
			kept = append(kept, n)
		}
		seq.Values = kept
		if len(seq.Values) == 0 {
			// Une liste vidée passe en ligne (« [] ») : vide en bloc, elle se
			// relirait nulle. La séquence est gardée, non remplacée — le
			// commentaire d'une clé « ban:   # liste noire » y est attaché, et un
			// nœud neuf le ferait écrire au mauvais endroit.
			seq.IsFlowStyle = true
		}
	}
	if !changed {
		return false, nil
	}

	out := []byte(file.String())
	if !strings.HasSuffix(string(out), "\n") {
		out = append(out, '\n')
	}
	tmp := &config.UFW{}
	if err := yaml.UnmarshalWithOptions(out, tmp, yaml.Strict()); err != nil {
		return false, exitf(CodeError, "réécriture de %s illisible : %v", path, err)
	}
	if err := tmp.Validate(); err != nil {
		return false, exitf(CodeValidation, "%s refusé :\n%v", path, err)
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := fs.WriteAtomic(path, out, mode, fs.KeepOwner); err != nil {
		return false, wrap(CodeError, err)
	}
	return true, nil
}
