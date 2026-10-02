package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// localize traduit l'ossature d'aide fournie par cobra. Sans cela, une interface
// française présente des rubriques anglaises et un « help for … » par commande.
func localize(root *cobra.Command) {
	root.SetUsageTemplate(usageTemplate)
	root.SetHelpCommand(&cobra.Command{
		Use:         "help [commande]",
		Short:       "Afficher l'aide d'une commande",
		Annotations: map[string]string{"skip-setup": "true"},
		Args:        cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Ce que Find ne reconnaît pas lui reste : « help projct » n'est pas
			// l'aide de la racine.
			target, rest, err := root.Find(args)
			if err != nil || target == nil || len(rest) > 0 {
				return exitf(CodeUsage, "commande inconnue : %s", strings.Join(args, " "))
			}
			return target.Help()
		},
	})
	// pflag écrit « (default …) » en dur ; on traduit la rubrique entière.
	cobra.AddTemplateFunc("options", func(usages string) string {
		return strings.ReplaceAll(strings.TrimRight(usages, " \t\n"), "(default ", "(par défaut ")
	})
	for _, cmd := range walk(root) {
		// La ligne d'utilisation n'annonce pas « [flags] » : la rubrique des
		// options, juste en dessous, les énumère déjà.
		cmd.DisableFlagsInUseLine = true
		// Déclarer nous-mêmes l'option évite celle que cobra ajouterait en anglais.
		if cmd.Flags().Lookup("help") == nil {
			cmd.Flags().BoolP("help", "h", false, "afficher l'aide de cette commande")
		}
		if cmd.Name() == "completion" {
			localizeCompletionCommand(cmd)
		}
		guardArgs(cmd)
	}
}

// guardArgs donne à chaque commande un validateur d'arguments qui rend le code 2
// (Q-69, D101). Un groupe — une commande qui n'existe que par ses
// sous-commandes — n'était pas exécutable : cobra affichait son aide et rendait
// 0 pour une sous-commande mal orthographiée, si bien que « ogs db dmup shopfr >
// dump.sql » écrivait l'aide dans la sauvegarde et rendait 0. Il devient
// exécutable, affiche son aide sans argument et refuse tout autre. Une feuille
// sans validateur ignorait ses arguments en trop : « project list extra »
// listait tout le parc. Elle refuse désormais tout argument, sauf si sa ligne
// d'utilisation en annonce un nombre libre (« [id...] »).
func guardArgs(cmd *cobra.Command) {
	if cmd.Hidden || strings.HasPrefix(cmd.Name(), "__") {
		return
	}
	switch {
	case cmd.HasSubCommands() && !cmd.Runnable():
		cmd.Args = unknownSubcommand
		cmd.RunE = func(c *cobra.Command, _ []string) error { return c.Help() }
		// L'aide d'un groupe ne demande ni root ni configuration, comme celle
		// qu'affichait cobra.
		if cmd.Annotations == nil {
			cmd.Annotations = map[string]string{}
		}
		cmd.Annotations["skip-setup"] = "true"
		cmd.Annotations[groupAnnotation] = "true"
		// Devenu exécutable, le groupe reçoit un synopsis des pages de manuel :
		// qu'il dise ce qui le suit.
		if !strings.Contains(cmd.Use, " ") {
			cmd.Use += " <commande>"
		}
		return
	case cmd.HasSubCommands() && cmd.Args == nil:
		// La racine : cobra refusait une commande inconnue en anglais, code 1.
		cmd.Args = unknownSubcommand
		return
	case cmd.Args == nil && strings.Contains(cmd.Use, "..."):
		cmd.Args = cobra.ArbitraryArgs
	case cmd.Args == nil:
		cmd.Args = cobra.NoArgs
	}
	validate := cmd.Args
	cmd.Args = func(c *cobra.Command, args []string) error {
		err := validate(c, args)
		if err == nil || ExitCode(err) != CodeError {
			return err
		}
		// Les messages de cobra sont anglais et rendent le code 1.
		return exitf(CodeUsage, "« %s » : %d argument(s) reçu(s), utilisation : %s",
			c.CommandPath(), len(args), c.UseLine())
	}
}

// groupAnnotation marque un groupe rendu exécutable par guardArgs : sa ligne
// d'utilisation n'annonce que ses sous-commandes.
const groupAnnotation = "ogs-groupe"

// unknownSubcommand refuse, en code 2, ce qui suit un groupe sans en être une
// sous-commande, avec les suggestions de cobra.
func unknownSubcommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	msg := fmt.Sprintf("sous-commande inconnue « %s » pour « %s »", args[0], cmd.CommandPath())
	// Le défaut de cobra (2) n'est posé que sur son propre chemin d'erreur.
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}
	if s := cmd.SuggestionsFor(args[0]); len(s) > 0 {
		msg += " ; voulez-vous dire : " + strings.Join(s, ", ") + " ?"
	}
	return exitf(CodeUsage, "%s — « %s --help » énumère les commandes", msg, cmd.CommandPath())
}

// localizeCompletionCommand traduit la commande engendrée par cobra, dont les
// libellés ne sont pas paramétrables.
func localizeCompletionCommand(cmd *cobra.Command) {
	cmd.Short = "Engendrer le script de complétion d'un shell"
	cmd.Long = "Écrit sur la sortie standard le script de complétion du shell demandé.\n\n" +
		"Pour l'installer durablement sous bash :\n" +
		"  ogs completion bash > /etc/bash_completion.d/ogs\n\n" +
		"Le paquet Debian installe ces scripts pour bash, zsh et fish."
	for _, sub := range cmd.Commands() {
		sub.Short = "Script de complétion pour " + sub.Name()
		sub.Long = sub.Short + ".\n\nÀ évaluer par le shell, par exemple :\n  source <(ogs completion " + sub.Name() + ")"
		if f := sub.Flags().Lookup("no-descriptions"); f != nil {
			f.Usage = "omettre les descriptions dans les propositions"
		}
	}
}

// usageTemplate reprend celui de cobra, rubriques traduites.
const usageTemplate = `Utilisation :{{if and .Runnable (ne (index .Annotations "ogs-groupe") "true")}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} <commande>{{end}}{{if gt (len .Aliases) 0}}

Alias :
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Exemples :
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Commandes :{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Commandes diverses :{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Options :
{{.LocalFlags.FlagUsages | options}}{{end}}{{if .HasAvailableInheritedFlags}}

Options héritées :
{{.InheritedFlags.FlagUsages | options}}{{end}}{{if .HasHelpSubCommands}}

Rubriques d'aide :{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

« {{.CommandPath}} <commande> --help » détaille une commande.{{end}}
`
