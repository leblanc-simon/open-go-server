package cli

import (
	"fmt"
	osexec "os/exec"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/template"
)

// execCommand est un raccourci de construction de commande.
func execCommand(name string, args []string) exec.Command {
	return exec.Command{Name: name, Args: args}
}

func newTemplateCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "template",
		Short: "Consulter et surcharger les templates de configuration",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:     "list",
			Aliases: []string{"ls"},
			Short:   "Lister les templates disponibles",
			RunE: func(c *cobra.Command, _ []string) error {
				set := template.New(app.Paths.TemplatesDir())
				names := set.Names()
				type row struct {
					Name   string `json:"name" yaml:"name"`
					Source string `json:"source" yaml:"source"`
				}
				items := make([]row, 0, len(names))
				for _, n := range names {
					items = append(items, row{Name: n, Source: set.Source(n)})
				}
				return wrap(CodeError, app.emit("TemplateList", items, nil, func(tw *tabwriter.Writer) {
					fmt.Fprintln(tw, "TEMPLATE\tSOURCE")
					for _, it := range items {
						fmt.Fprintf(tw, "%s\t%s\n", it.Name, it.Source)
					}
				}))
			},
		},
		&cobra.Command{
			Use:   "show <nom>",
			Short: "Afficher la source d'un template",
			Args:  cobra.ExactArgs(1),
			RunE: func(c *cobra.Command, args []string) error {
				data, err := template.New(app.Paths.TemplatesDir()).Read(args[0])
				if err != nil {
					return exitf(CodeUsage, "%s", err)
				}
				_, err = app.Out.Write(data)
				return wrap(CodeError, err)
			},
		},
		&cobra.Command{
			Use:   "extract <nom>",
			Short: "Copier un template embarqué dans /etc/ogs/templates pour le personnaliser",
			Args:  cobra.ExactArgs(1),
			RunE: func(c *cobra.Command, args []string) error {
				path, err := template.New(app.Paths.TemplatesDir()).Extract(args[0])
				if err != nil {
					return exitf(CodeUsage, "%s", err)
				}
				app.printf("✓ template extrait : %s\n", path)
				msg, tool := templateValidation(args[0])
				switch {
				case tool == "":
				case app.Exec.Simulated():
					msg += " En mode simulé, aucun outil n'est lancé."
				default:
					if _, err := osexec.LookPath(tool); err != nil {
						msg += " " + tool + " est absent de cette machine : rien n'y est éprouvé."
					}
				}
				app.printf("  il remplace désormais la version embarquée ; %s\n", msg)
				return nil
			},
		},
	)
	return cmd
}

// templateValidation dit ce qui éprouve le rendu d'un template avant que l'outil
// qui le lit ne le relise. « Le rendu est validé avant activation » valait pour
// tous : c'était faux des unités, du cron, de la rotation et de Telegraf, et
// fail2ban n'était éprouvé qu'après coup, le fichier refusé laissé en place
// (D117). Un fichier refusé est rendu à son état précédent ; un outil absent
// n'éprouve rien.
func templateValidation(name string) (msg, tool string) {
	dir, base, _ := strings.Cut(name, "/")
	switch {
	case dir == "apache":
		return "le rendu est éprouvé par « apachectl -t » à l'écriture et avant chaque rechargement.", "apachectl"
	case dir == "nginx":
		return "le rendu est éprouvé par « nginx -t » à l'écriture et avant chaque rechargement.", "nginx"
	case dir == "php":
		return "le rendu est éprouvé par « php-fpm -t » à l'écriture.", ""
	case dir == "logrotate":
		return "le rendu est éprouvé par « logrotate -d » à l'écriture, et rendu s'il est refusé.", "logrotate"
	case dir == "fail2ban":
		return "le rendu est éprouvé par « fail2ban-client -t » à l'écriture, et rendu s'il est refusé.", "fail2ban-client"
	case dir == "systemd" && strings.HasPrefix(base, "ogs-renew"):
		return "son rendu n'est éprouvé par aucun outil avant activation : le vérifier avec « systemd-analyze verify ».", ""
	case dir == "systemd":
		return "le rendu est éprouvé par « systemd-analyze verify » à l'écriture, et rendu s'il est refusé.", "systemd-analyze"
	}
	// cron, telegraf : aucun validateur. L'option de test de Telegraf exécute une
	// collecte réelle, et cron n'en a pas.
	return "son rendu n'est éprouvé par aucun outil avant activation : le relire avec soin.", ""
}
