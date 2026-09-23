package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/model"
	appprov "leblanc.io/open-go-server/internal/provider/app"
	"leblanc.io/open-go-server/internal/spec"
)

func newRuntimeCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runtime",
		Short: "Gérer les runtimes applicatifs (Node, Ruby, Python)",
		Long: "Les runtimes sont installés dans le home de chaque projet : une copie par site,\n" +
			"des mises à jour à faire autant de fois qu'il y a de projets, mais une maîtrise\n" +
			"complète des versions par l'exploitant du site.",
	}
	cmd.AddCommand(newRuntimeListCommand(app), newRuntimeInstallCommand(app))
	return cmd
}

// runtimeRow est la vue machine d'un runtime.
type runtimeRow struct {
	Project  string `json:"project" yaml:"project"`
	Runtime  string `json:"runtime" yaml:"runtime"`
	Declared string `json:"declared" yaml:"declared"`
	Present  bool   `json:"present" yaml:"present"`
	Link     string `json:"link,omitempty" yaml:"link,omitempty"`
	Listen   string `json:"listen,omitempty" yaml:"listen,omitempty"`
}

func newRuntimeListCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list [id...]",
		Aliases: []string{"ls"},
		Short:   "Lister les runtimes des projets",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := app.targets(args, "")
			if err != nil {
				return err
			}
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			var rows []runtimeRow
			for _, p := range projects {
				if !appprov.Applies(p) {
					continue
				}
				at, err := p.AppType()
				if err != nil {
					continue
				}
				obs, err := provs.App.Observe(cmd.Context(), p)
				if err != nil {
					return wrap(CodeError, err)
				}
				row := runtimeRow{
					Project: p.ID(), Runtime: at.Runtime, Declared: p.Spec.App.Runtime,
					Present: obs.RuntimeInstalled, Link: obs.LinkTarget,
				}
				if obs.Endpoint.Unix() {
					row.Listen = "socket"
				} else {
					row.Listen = obs.Endpoint.Address
				}
				rows = append(rows, row)
			}
			return wrap(CodeError, app.emit("RuntimeList", rows, nil, func(tw *tabwriter.Writer) {
				if len(rows) == 0 {
					fmt.Fprintln(app.Out, "aucun projet à runtime applicatif")
					return
				}
				fmt.Fprintln(tw, "PROJET\tRUNTIME\tVERSION\tPRÉSENT\tÉCOUTE")
				for _, r := range rows {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\n",
						r.Project, r.Runtime, orValue(r.Declared, "—"), r.Present, r.Listen)
				}
			}))
		},
	}
}

func newRuntimeInstallCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "install <id> <version>",
		Short: "Installer une version de runtime dans le home d'un projet",
		Long: "L'installation télécharge depuis Internet et s'exécute sous l'identité du\n" +
			"projet. Le lien de ~/.local/bin est mis à jour pour désigner la version demandée.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			at, err := p.AppType()
			if err != nil {
				return exitf(CodeValidation, "%s", err)
			}
			if at.Runtime == "" {
				return exitf(CodeUsage, "le projet %s n'a pas de runtime applicatif "+
					"(types concernés : go, node, python, ruby)", p.ID())
			}
			provs, err := app.certProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			app.printf("installation de %s %s dans %s…\n", at.Runtime, args[1], p.Spec.Layout.Root)
			if err := provs.App.Installer.Install(cmd.Context(), p, at.Runtime, args[1]); err != nil {
				return wrap(CodeError, err)
			}
			// La version déclarée suit : sans cela, le prochain apply remettrait
			// l'ancienne.
			p.Spec.App.Runtime = args[1]
			if err := spec.Save(app.Paths.ProjectFile(p.ID()), p); err != nil {
				return wrap(CodeError, err)
			}
			_ = app.Audit.Record(p.ID(), "app/runtime/"+at.Runtime, "runtime.install", "ok", args[1])
			app.printf("✓ %s %s installé, déclaration mise à jour\n", at.Runtime, args[1])
			app.printf("\nAppliquer pour mettre à jour le lien et redémarrer :\n  ogs apply %s\n", p.ID())
			return nil
		},
	}
}

func newAppCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "app",
		Short: "Gérer les types d'application",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "types",
			Short: "Lister les types d'application connus",
			RunE: func(c *cobra.Command, _ []string) error {
				type row struct {
					Kind     string `json:"kind" yaml:"kind"`
					Docroot  string `json:"docroot" yaml:"docroot"`
					Front    string `json:"frontController,omitempty" yaml:"frontController,omitempty"`
					Runtime  string `json:"runtime,omitempty" yaml:"runtime,omitempty"`
					Template string `json:"template" yaml:"template"`
				}
				var rows []row
				for _, kind := range model.Kinds() {
					at, err := model.Resolve(kind, "")
					if err != nil {
						continue
					}
					rows = append(rows, row{
						Kind: string(kind), Docroot: orValue(at.Docroot, "— (proxy)"),
						Front: at.FrontController, Runtime: at.Runtime, Template: at.VHostTemplate,
					})
				}
				return wrap(CodeError, app.emit("AppTypeList", rows, nil, func(tw *tabwriter.Writer) {
					fmt.Fprintln(tw, "TYPE\tDOCROOT\tCONTRÔLEUR\tRUNTIME\tTEMPLATE")
					for _, r := range rows {
						fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
							r.Kind, r.Docroot, orValue(r.Front, "—"), orValue(r.Runtime, "—"), r.Template)
					}
				}))
			},
		},
		newAppSetCommand(app),
	)
	return cmd
}

func newAppSetCommand(app *App) *cobra.Command {
	var kind string
	cmd := &cobra.Command{
		Use:   "set <id> --kind <type>",
		Short: "Changer le type d'application d'un projet",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			{
				if kind == "" {
					return exitf(CodeUsage, "préciser le type avec --kind (%s)", kindsList())
				}
			}
			if _, err := model.Resolve(model.AppKind(kind), ""); err != nil {
				return exitf(CodeUsage, "%s (types connus : %s)", err, kindsList())
			}
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			previous := p.Spec.App.Kind
			p.Spec.App.Kind = model.AppKind(kind)
			// Le docroot dérive du type : le laisser figé servirait le mauvais
			// répertoire après la bascule.
			p.Spec.Layout.Docroot = ""
			if err := app.prepare(p); err != nil {
				return err
			}
			if err := spec.Save(app.Paths.ProjectFile(p.ID()), p); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ %s : type %s → %s\n", p.ID(), previous, kind)
			app.printf("  docroot   %s\n", p.Spec.Layout.Docroot)
			app.printf("\nAppliquer :\n  ogs apply %s\n", p.ID())
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "nouveau type d'application")
	return cmd
}
