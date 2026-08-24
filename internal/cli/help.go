package cli

import (
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
		RunE: func(cmd *cobra.Command, args []string) error {
			target, _, err := root.Find(args)
			if err != nil || target == nil {
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
	}
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
const usageTemplate = `Utilisation :{{if .Runnable}}
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
