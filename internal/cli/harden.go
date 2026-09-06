package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/provider/hardening"
)

// newHardenCommand construit « ogs harden ». Le rapport est le comportement par
// défaut, et l'application se demande : restreindre /etc et la racine des homes
// dépasse le périmètre d'un projet, et une commande qui modifierait le système au
// premier essai n'inspirerait pas confiance.
func newHardenCommand(app *App) *cobra.Command {
	var (
		apply   bool
		verbose bool
	)
	cmd := &cobra.Command{
		Use:   "harden",
		Short: "Restreindre les droits des répertoires sensibles du système",
		Long: "Sur un serveur mutualisé, les utilisateurs de projet ont un compte local :\n" +
			"sans restriction ils lisent /etc/nginx, /etc/mysql ou /etc/telegraf, y trouvent\n" +
			"des chemins, des noms de bases et des jetons, et dressent la carte du serveur —\n" +
			"celle des sites de leurs voisins comprise. La racine des homes leur donne la\n" +
			"même carte d'un seul « ls /home » : elle passe en 0711, traversable sans être\n" +
			"énumérable, ce qui ne change rien à la façon dont le serveur web sert un\n" +
			"fichier. Le gain est partiel et il faut le savoir : /etc/passwd est lisible par\n" +
			"tous et énumère déjà les homes.\n\n" +
			"Sans option, la commande rapporte et ne touche à rien ; le code 6 signale qu'il\n" +
			"reste des droits à restreindre. « --apply » applique.\n\n" +
			"Deux règles la gouvernent : elle ne desserre jamais — un répertoire déjà plus\n" +
			"strict que la cible est laissé tel quel — et elle ne suit aucun lien symbolique.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			prov := app.hardeningProvider()
			states := prov.Observe()

			render := func(tw *tabwriter.Writer) {
				fmt.Fprintln(tw, "CHEMIN\tÉTAT\tCONSTATÉ\tCIBLE")
				for _, st := range states {
					constate := "—"
					switch {
					// Un refus s'explique : sinon il passe pour un défaut d'ogs.
					case st.Reason != "":
						constate = st.Reason
					case st.Present:
						constate = fmt.Sprintf("%04o %s:%s", st.Mode, st.Owner, st.Group)
						// Sans cette mention, un répertoire déjà conforme mais dont les
						// fichiers ne le sont pas paraîtrait signalé sans raison.
						if n := len(st.Loose); n > 0 {
							constate += fmt.Sprintf(" + %d fichier(s)", n)
						}
					}
					cible := fmt.Sprintf("%04o", st.Target.DirMode)
					if st.Target.Owner != "" || st.Target.Group != "" {
						cible += " " + hardening.OwnerLabel(st.Target)
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", st.Target.Path, st.Verdict, constate, cible)
				}
			}
			if err := app.emit("HardeningReport", states, nil, render); err != nil {
				return err
			}

			// Le pourquoi et le risque de chaque restriction : appliquer sans les
			// lire est le meilleur moyen d'interrompre un service.
			if verbose {
				for _, st := range states {
					if st.Target.Why == "" {
						continue
					}
					app.printf("\n%s\n  pourquoi : %s\n", st.Target.Path, st.Target.Why)
					if st.Target.Risk != "" {
						app.printf("  risque   : %s\n", st.Target.Risk)
					}
					for _, f := range st.Loose {
						// « à reprendre » et non « trop permissif » : la liste retient aussi
						// un fichier plus strict que la cible mais privé de son groupe, que
						// le démon ne peut plus lire. Le nommer trop permissif enverrait
						// l'exploitant chercher une permissivité qui n'existe pas.
						app.printf("  fichier à reprendre : %s\n", f)
					}
				}
			}

			reste := hardening.Restrict(states)
			if reste == 0 {
				app.printf("\n✓ tous les répertoires surveillés sont conformes\n")
				return nil
			}
			if !apply {
				app.printf("\n%d répertoire(s) à restreindre. Détail : ogs harden -v\n", reste)
				app.printf("Appliquer : ogs harden --apply\n")
				return exitf(CodePartial, "%d répertoire(s) à restreindre", reste)
			}

			if err := app.confirm(fmt.Sprintf(
				"restreindre %d répertoire(s) du système ?", reste)); err != nil {
				return err
			}
			if err := app.openStore(); err != nil {
				return err
			}
			plan := &engine.Plan{Project: "système"}
			for _, s := range prov.Plan(states) {
				plan.Add(s)
			}
			report, err := engine.Execute(cmd.Context(), plan, engine.Options{
				DryRun: app.DryRun,
				OnStep: func(s *engine.Step, stepErr error) {
					if stepErr == nil {
						app.printf("  ✓ %s\n", s.Summary)
					}
				},
				OnWarning: func(s *engine.Step, stepErr error) {
					app.printf("  ⚠ %s : %v\n", s.Summary, stepErr)
				},
				Audit: func(s *engine.Step, outcome, detail string) {
					_ = app.Audit.Record("", s.Resource, "harden", outcome, detail)
				},
			})
			if err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ %d répertoire(s) restreint(s) (audit %s)\n",
				report.Applied(), app.Audit.RunID())
			if len(report.Warnings) > 0 {
				return exitf(CodePartial, "terminé avec %d avertissement(s)", len(report.Warnings))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&apply, "apply", false, "appliquer les restrictions au lieu de seulement les rapporter")
	f.BoolVar(&verbose, "explain", false, "expliquer chaque restriction et ce qu'elle peut casser")
	// « -v » global compte les niveaux de détail : on l'accepte aussi ici.
	cmd.PreRun = func(*cobra.Command, []string) {
		if app.Verbose > 0 {
			verbose = true
		}
	}
	return cmd
}

// hardeningProvider construit le provider avec les réglages de configuration.
func (a *App) hardeningProvider() *hardening.Provider {
	return &hardening.Provider{
		Prefix: a.Paths.Prefix(),
		// La racine hors préfixe : c'est le provider qui décale les chemins.
		HomeRoot: a.Cfg.Naming.HomeRoot,
		Targets:  a.Cfg.Hardening.Targets,
		Skip:     a.Cfg.Hardening.Skip,
	}
}
