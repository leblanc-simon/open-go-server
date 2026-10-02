package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/mail"
	"leblanc.io/open-go-server/internal/spec"
)

func newMailCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{Use: "mail", Short: "Courrier sortant des projets : signature DKIM"}
	dkim := &cobra.Command{
		Use:   "dkim",
		Short: "Clés DKIM des projets et enregistrements à publier",
		Long: "ogs tient une clé par domaine d'envoi et les tables d'OpenDKIM qui les désignent\n" +
			"(D69). Il ne publie rien dans le DNS : « records » émet l'enregistrement à poser,\n" +
			"et « doctor » vérifie ce qui l'a été.",
	}
	dkim.AddCommand(
		newDKIMEnableCommand(app),
		newDKIMDisableCommand(app),
		newDKIMShowCommand(app),
		newDKIMRecordsCommand(app),
		newDKIMRotateCommand(app),
		newDKIMActivateCommand(app),
	)
	cmd.AddCommand(dkim)
	return cmd
}

func newDKIMEnableCommand(app *App) *cobra.Command {
	var domains []string
	cmd := &cobra.Command{
		Use:   "enable <id>",
		Short: "Demander la signature DKIM du courrier d'un projet",
		Long: "Sans --domain, les domaines primaires du projet sont signés. La clé est créée par\n" +
			"« ogs apply », qui signe aussitôt : une signature invérifiable ne vaut pas moins\n" +
			"qu'une absence de signature. Refusé en préproduction.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			p.Spec.Mail = &model.Mail{DKIM: &model.DKIM{Enabled: true, Domains: domains}}
			if err := app.prepare(p); err != nil {
				return err
			}
			if app.DryRun {
				app.printf("--dry-run : la signature DKIM serait demandée pour %s\n",
					strings.Join(p.DKIMDomains(), ", "))
				return nil
			}
			if err := spec.Save(app.Paths.ProjectFile(p.ID()), p); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ signature DKIM demandée pour %s\n", strings.Join(p.DKIMDomains(), ", "))
			app.printf("\nAppliquer, puis publier les enregistrements :\n  ogs apply %s\n  ogs mail dkim records %s\n",
				p.ID(), p.ID())
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&domains, "domain", nil, "domaine à signer (répétable) ; défaut : les domaines primaires")
	return cmd
}

func newDKIMDisableCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "disable <id>",
		Short: "Cesser de signer le courrier d'un projet",
		Long: "Les tables d'OpenDKIM ne désignent plus les domaines du projet au prochain apply,\n" +
			"et leurs fichiers de clés sont retirés. Les clés restent scellées : réactiver la\n" +
			"signature retrouve l'enregistrement déjà publié.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			if len(p.DKIMDomains()) == 0 {
				app.printf("= %s ne demande pas de signature DKIM\n", p.ID())
				return nil
			}
			p.Spec.Mail = nil
			if app.DryRun {
				app.printf("--dry-run : la signature DKIM de %s serait retirée\n", p.ID())
				return nil
			}
			if err := spec.Save(app.Paths.ProjectFile(p.ID()), p); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ signature DKIM retirée de %s\n\nAppliquer :\n  ogs apply %s\n", p.ID(), p.ID())
			return nil
		},
	}
}

// dkimKeyView est la vue machine d'une clé : jamais sa partie privée.
type dkimKeyView struct {
	Domain    string `json:"domain" yaml:"domain"`
	Selector  string `json:"selector" yaml:"selector"`
	State     string `json:"state" yaml:"state"`
	Created   string `json:"created" yaml:"created"`
	Activated string `json:"activated,omitempty" yaml:"activated,omitempty"`
	Retired   string `json:"retired,omitempty" yaml:"retired,omitempty"`
	File      string `json:"file" yaml:"file"`
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func newDKIMShowCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Lister les clés DKIM d'un projet et leur état",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, provs, err := app.mailContext(args[0])
			if err != nil {
				return err
			}
			defer provs.close()
			keys, err := provs.Mail.ProjectKeys(p)
			if err != nil {
				return wrap(CodeError, err)
			}
			views := make([]dkimKeyView, 0, len(keys))
			for _, k := range keys {
				views = append(views, dkimKeyView{
					Domain: k.Domain, Selector: k.Selector, State: k.State,
					Created: formatTime(k.Created), Activated: formatTime(k.Activated),
					Retired: formatTime(k.Retired),
					File:    app.Paths.OpenDKIMKeyFile(p.ID(), k.Domain, k.Selector),
				})
			}
			return wrap(CodeError, app.emit("DKIMKeyList", views, nil, func(tw *tabwriter.Writer) {
				if len(views) == 0 {
					fmt.Fprintf(app.Out, "aucune clé DKIM pour %s\n", p.ID())
					return
				}
				fmt.Fprintln(tw, "DOMAINE\tSÉLECTEUR\tÉTAT\tCRÉÉE\tFICHIER")
				for _, v := range views {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", v.Domain, v.Selector, v.State, v.Created, v.File)
				}
			}))
		},
	}
}

func newDKIMRecordsCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "records <id>",
		Short: "Émettre les enregistrements DNS à publier",
		Long: "Au format d'un fichier de zone, ou en JSON avec --output json. Une clé RSA-2048\n" +
			"dépasse les 255 octets d'une chaîne TXT : l'enregistrement est découpé en plusieurs\n" +
			"chaînes, que le résolveur recolle. ogs ne publie rien lui-même (D69).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, provs, err := app.mailContext(args[0])
			if err != nil {
				return err
			}
			defer provs.close()
			records, err := provs.Mail.Records(p)
			if err != nil {
				return wrap(CodeError, err)
			}
			if app.Format != "table" {
				return wrap(CodeError, app.emit("DKIMRecordList", records, nil, nil))
			}
			if len(records) == 0 {
				app.printf("aucune clé DKIM pour %s : « ogs mail dkim enable %s » puis « ogs apply %s »\n",
					p.ID(), p.ID(), p.ID())
				return nil
			}
			for _, r := range records {
				fmt.Fprintf(app.Out, "; %s — clé %s\n%s\n", r.Domain, recordStateLabel(r.State), r.Zone())
			}
			return nil
		},
	}
}

func recordStateLabel(state string) string {
	switch state {
	case mail.StateActive:
		return "active"
	case mail.StatePending:
		return "préparée, à publier avant « ogs mail dkim activate »"
	case mail.StateRetired:
		return "remplacée, à retirer une fois la rétention écoulée"
	}
	return state
}

func newDKIMRotateCommand(app *App) *cobra.Command {
	var domain string
	cmd := &cobra.Command{
		Use:   "rotate <id>",
		Short: "Préparer une nouvelle clé, sans rien changer à la signature",
		Long: "Première moitié de la rotation. La clé est engendrée sous un nouveau sélecteur et\n" +
			"écrite, mais les tables désignent toujours l'ancienne : basculer vers une clé non\n" +
			"publiée casserait la signature de tout envoi. Publier ensuite l'enregistrement\n" +
			"(« ogs mail dkim records »), puis « ogs mail dkim activate ».",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, provs, err := app.mailContext(args[0])
			if err != nil {
				return err
			}
			defer provs.close()
			obs, err := provs.Mail.Observe(cmd.Context())
			if err != nil {
				return wrap(CodeError, err)
			}
			steps, err := provs.Mail.Rotate(cmd.Context(), p, domain, obs)
			if err != nil {
				if errors.Is(err, mail.ErrAlreadyPending) {
					return exitf(CodeConflict, "%v", err)
				}
				return exitf(CodeValidation, "%v", err)
			}
			lock, err := app.lock()
			if err != nil {
				return err
			}
			defer lock.Release()
			if err := app.runSteps(cmd.Context(), provs, p.ID(), steps, true); err != nil {
				return err
			}
			app.printf("\nPublier l'enregistrement, puis basculer :\n  ogs mail dkim records %s\n"+
				"  ogs mail dkim activate %s\n", p.ID(), p.ID())
			return nil
		},
	}
	cmd.Flags().StringVar(&domain, "domain", "", "ne préparer que la clé de ce domaine")
	return cmd
}

func newDKIMActivateCommand(app *App) *cobra.Command {
	var domain string
	cmd := &cobra.Command{
		Use:   "activate <id>",
		Short: "Basculer la signature vers la clé préparée",
		Long: "Seconde moitié de la rotation. L'enregistrement publié doit correspondre à la clé\n" +
			"préparée : sans quoi la commande refuse (code 5), --force passant outre. La clé\n" +
			"remplacée reste en place pendant mail.dkim.retention, le temps que les courriers\n" +
			"en transit soient vérifiés ; son enregistrement est à retirer ensuite.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, provs, err := app.mailContext(args[0])
			if err != nil {
				return err
			}
			defer provs.close()
			pending, err := provs.Mail.Pending(p, domain)
			if err != nil {
				if errors.Is(err, mail.ErrNothingPending) {
					return exitf(CodeConflict, "%v", err)
				}
				return exitf(CodeValidation, "%v", err)
			}
			var refused []string
			for _, k := range pending {
				verdict, detail := provs.Mail.Verify(cmd.Context(), k)
				app.printf("  %s : enregistrement %s\n", k.Name(), verdict)
				if verdict != mail.DNSMatch {
					refused = append(refused, k.Name()+" ("+detail+")")
				}
			}
			if len(refused) > 0 && !app.Force {
				return exitf(CodeDependency, "enregistrement non publié ou différent : %s — publier "+
					"l'enregistrement (ogs mail dkim records %s) ou passer outre avec --force",
					strings.Join(refused, " ; "), p.ID())
			}
			lock, err := app.lock()
			if err != nil {
				return err
			}
			defer lock.Release()
			if err := app.runSteps(cmd.Context(), provs, p.ID(),
				[]*engine.Step{provs.Mail.Activate(p, pending)}, false); err != nil {
				return err
			}
			// Les tables sont de portée parc : recalculées en entier, jamais depuis le
			// seul projet basculé.
			if err := app.refreshGlobal(cmd.Context(), provs); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ signature basculée ; la clé remplacée est conservée %s, "+
				"son enregistrement est à retirer ensuite\n", humanDuration(app.Cfg.Mail.DKIMRetention()))
			return nil
		},
	}
	cmd.Flags().StringVar(&domain, "domain", "", "ne basculer que la clé de ce domaine")
	return cmd
}

// humanDuration rend une durée en jours quand elle en compte un nombre entier :
// « 168h0m0s » ne dit rien à qui attend une rétention d'une semaine.
func humanDuration(d time.Duration) string {
	if d > 0 && d%(24*time.Hour) == 0 {
		days := int(d / (24 * time.Hour))
		if days == 1 {
			return "1 jour"
		}
		return fmt.Sprintf("%d jours", days)
	}
	return d.String()
}

// mailContext charge le projet et les providers d'une commande DKIM.
func (a *App) mailContext(id string) (*model.Project, *providers, error) {
	p, err := a.loadProject(id)
	if err != nil {
		return nil, nil, err
	}
	if err := a.openStore(); err != nil {
		return nil, nil, err
	}
	if a.Keys == nil {
		return nil, nil, exitf(CodeDependency, "keystore indisponible : vérifier %s", a.Paths.MasterKey())
	}
	provs, err := a.buildProviders()
	if err != nil {
		return nil, nil, err
	}
	return p, provs, nil
}

// lock prend le verrou global d'exécution.
func (a *App) lock() (*engine.Lock, error) {
	lock, err := engine.Acquire(a.Paths.GlobalLock())
	if err != nil {
		var locked engine.ErrLocked
		if errors.As(err, &locked) {
			return nil, wrap(CodeLocked, err)
		}
		return nil, wrap(CodeError, err)
	}
	return lock, nil
}

// runSteps exécute une poignée d'étapes hors d'un apply, avec l'audit et les
// rechargements de l'apply. Une étape non critique en échec est ici une erreur :
// la commande n'a pas d'autre objet que cette étape.
func (a *App) runSteps(ctx context.Context, provs *providers, projectID string,
	steps []*engine.Step, strict bool) error {
	plan := &engine.Plan{Project: projectID}
	for _, s := range steps {
		plan.Add(s)
	}
	var warned []string
	report, err := engine.Execute(ctx, plan, engine.Options{
		OnStep: func(s *engine.Step, stepErr error) { a.stepf("  ", s, stepErr) },
		OnWarning: func(s *engine.Step, stepErr error) {
			warned = append(warned, fmt.Sprintf("%s : %v", s.Summary, stepErr))
			a.warnf("  ⚠ %s : %v\n", s.Summary, stepErr)
		},
		Audit: func(s *engine.Step, outcome, detail string) {
			_ = a.Audit.Record(projectID, s.Resource, "mail."+s.Action.String(), outcome, detail)
		},
		Reload:          a.reloadFunc(provs),
		OnReloadWarning: a.reloadWarning,
	})
	if err != nil {
		if report != nil && report.RolledBack {
			a.warnf("↶ rollback effectué\n")
		}
		return wrap(CodeError, err)
	}
	if strict && len(warned) > 0 {
		return exitf(CodeError, "%s", strings.Join(warned, " ; "))
	}
	if len(report.Warnings) > 0 {
		return exitf(CodePartial, "terminé avec %d avertissement(s)", len(report.Warnings))
	}
	return nil
}
