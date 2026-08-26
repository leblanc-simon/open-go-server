package cli

import (
	"fmt"
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
				app.printf("  il remplace désormais la version embarquée ; le rendu est validé avant activation.\n")
				return nil
			},
		},
	)
	return cmd
}
