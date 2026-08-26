package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/importer"
	"leblanc.io/open-go-server/internal/provider/db"
	"leblanc.io/open-go-server/internal/spec"
)

func newImportCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Reprendre en main un serveur déjà en production",
	}
	cmd.AddCommand(newImportScanCommand(app), newImportAdoptCommand(app), newImportUnmanagedCommand(app))
	return cmd
}

// scanner construit l'inspecteur, avec les clients de bases s'ils sont joignables.
func (a *App) scanner() (*importer.Scanner, error) {
	eng, err := a.webEngine()
	if err != nil {
		// Sans serveur web, le scan reste utile pour les comptes et les bases.
		eng = nil
	}
	clients := map[string]db.Client{}
	if socket, ok := db.FindMySQLSocket(a.Paths.Prefix()); ok {
		if client, err := db.NewMySQLClient(socket); err == nil {
			clients["mysql"] = client
		}
	}
	if clusters := db.DetectClusters(a.Paths.Prefix()); len(clusters) > 0 {
		if client, err := db.NewPostgresClient(clusters[0], os.Getenv("OGS_PG_ADMIN")); err == nil {
			clients["postgres"] = client
		}
	}
	return &importer.Scanner{
		Paths: a.Paths, Engine: eng, DBClients: clients,
		HomeRoots: []string{a.Cfg.Naming.HomeRoot, "/var/www", "/srv"},
	}, nil
}

func newImportScanCommand(app *App) *cobra.Command {
	var showAll bool
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Inspecter le serveur et proposer des projets",
		Long: "Lecture seule : rien n'est modifié. Les ressources déjà gérées par ogs sont\n" +
			"ignorées, et celles qui ne peuvent être rattachées restent non gérées — elles\n" +
			"ne seront jamais touchées.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			scanner, err := app.scanner()
			if err != nil {
				return err
			}
			resources, err := scanner.Scan(cmd.Context())
			if err != nil {
				return wrap(CodeError, err)
			}
			candidates, unmanaged := importer.Correlate(resources,
				app.homeRoot(), app.Cfg.PrepDomain())

			if app.Format != "table" {
				return wrap(CodeError, app.emit("ImportScan", map[string]any{
					"candidates": candidates, "unmanaged": unmanaged,
					"summary": importer.Summary(resources),
				}, nil, nil))
			}

			counts := importer.Summary(resources)
			app.printf("%d ressource(s) inspectée(s) :", len(resources))
			for _, kind := range []importer.Kind{importer.KindVHost, importer.KindPool,
				importer.KindUser, importer.KindDatabase, importer.KindCron,
				importer.KindUnit, importer.KindCertificate} {
				if counts[kind] > 0 {
					app.printf(" %d %s", counts[kind], kind)
				}
			}
			app.printf("\n\n")

			if len(candidates) == 0 {
				app.printf("aucun projet à adopter : tout est déjà géré, ou rien n'a pu être rattaché.\n")
			}
			for _, c := range candidates {
				app.printf("Projet candidat : %s  (confiance %s)\n", orValue(c.ID, "?"), c.Confidence)
				app.printf("  host        %s\n", strings.Join(c.Domains, ", "))
				if c.User != "" {
					app.printf("  utilisateur %s\n", c.User)
				}
				if c.Home != "" {
					app.printf("  racine      %s\n", c.Home)
				}
				if c.Docroot != "" {
					app.printf("  docroot     %s\n", c.Docroot)
				}
				if c.PHPVersion != "" {
					app.printf("  php         %s\n", c.PHPVersion)
				}
				app.printf("  type deviné %s\n", c.Kind)
				for _, r := range c.Resources {
					app.printf("    · %s\n", r.String())
				}
				for _, u := range c.Uncertain {
					app.printf("    ⚠ %s\n", u)
				}
				app.printf("  adopter : ogs import adopt %s\n\n", c.ID)
			}

			if len(unmanaged) > 0 {
				app.printf("Non géré (laissé strictement intact) :\n")
				limit := len(unmanaged)
				if !showAll && limit > 10 {
					limit = 10
				}
				for _, r := range unmanaged[:limit] {
					app.printf("  · %s\n", r.String())
				}
				if limit < len(unmanaged) {
					app.printf("  … %d de plus (--all pour tout voir)\n", len(unmanaged)-limit)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&showAll, "all", false, "afficher toutes les ressources non gérées")
	return cmd
}

func newImportAdoptCommand(app *App) *cobra.Command {
	var host string
	cmd := &cobra.Command{
		Use:   "adopt <id>",
		Short: "Adopter un projet candidat",
		Long: "Écrit la déclaration correspondant à l'existant, sans rien modifier sur le\n" +
			"système. Les ressources gardent leurs noms réels : l'adoption décrit, elle ne\n" +
			"normalise pas. Les écarts à la convention sont affichés, ainsi que ce qu'un\n" +
			"premier « apply » changerait.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			scanner, err := app.scanner()
			if err != nil {
				return err
			}
			resources, err := scanner.Scan(cmd.Context())
			if err != nil {
				return wrap(CodeError, err)
			}
			candidates, _ := importer.Correlate(resources, app.homeRoot(), app.Cfg.PrepDomain())
			var chosen *importer.Candidate
			for i := range candidates {
				if candidates[i].ID == args[0] || candidates[i].Host == args[0] ||
					(host != "" && candidates[i].Host == host) {
					chosen = &candidates[i]
				}
			}
			if chosen == nil {
				available := make([]string, 0, len(candidates))
				for _, c := range candidates {
					available = append(available, c.ID)
				}
				return exitf(CodeUsage, "aucun candidat %q (proposés : %s)",
					args[0], orValue(strings.Join(available, ", "), "aucun"))
			}

			p, notes, err := importer.Adopt(*chosen, app.homeRoot(), app.Cfg.PrepDomain())
			if err != nil {
				return exitf(CodeValidation, "%v", err)
			}
			path := app.Paths.ProjectFile(p.ID())
			if _, err := os.Stat(path); err == nil {
				return exitf(CodeConflict, "le projet %s est déjà déclaré (%s)", p.ID(), path)
			}

			app.printf("adoption de %s\n", p.ID())
			app.printf("  utilisateur %s\n", p.Spec.User.Name)
			app.printf("  racine      %s\n", p.Spec.Layout.Root)
			if diffs := importer.Diverges(p, app.homeRoot(), app.Cfg.PrepDomain()); len(diffs) > 0 {
				// L'écart est annoncé maintenant : un « apply » normaliserait sans
				// prévenir, et c'est exactement ce qu'il faut éviter de découvrir après.
				app.printf("\nécarts à la convention ogs (conservés tels quels) :\n")
				for _, d := range diffs {
					app.printf("  · %s\n", d)
				}
			}
			for _, n := range notes {
				app.printf("  ⚠ %s\n", n)
			}
			if app.DryRun {
				data, err := spec.Encode(p)
				if err != nil {
					return wrap(CodeError, err)
				}
				app.printf("\n--dry-run : %s serait écrit.\n\n", path)
				fmt.Fprint(app.Out, string(data))
				return nil
			}
			if err := app.confirm(fmt.Sprintf("écrire la déclaration de %s ?", p.ID())); err != nil {
				return err
			}
			if err := spec.Save(path, p); err != nil {
				return wrap(CodeError, err)
			}
			if err := app.openStore(); err == nil {
				_ = app.Audit.Record(p.ID(), "project", "import.adopt", "ok", path)
				// Les ressources non rattachées sont mémorisées : le moteur ne les
				// planifiera jamais, et les détections de conflit les prendront en compte.
				_, unmanaged := importer.Correlate(resources, app.homeRoot(), app.Cfg.PrepDomain())
				for _, r := range unmanaged {
					_ = app.Store.RecordUnmanaged(string(r.Kind), r.Name, r.Path)
				}
			}
			app.printf("\n✓ %s adopté : %s\n", p.ID(), path)
			app.printf("  vérifier avant d'appliquer :\n    ogs diff %s\n", p.ID())
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "désigner le candidat par son host")
	return cmd
}

func newImportUnmanagedCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "unmanaged",
		Short: "Lister les ressources laissées hors gestion",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := app.openStore(); err != nil {
				return err
			}
			rows, err := app.Store.Unmanaged()
			if err != nil {
				return wrap(CodeError, err)
			}
			return wrap(CodeError, app.emit("UnmanagedList", rows, nil, func(tw *tabwriter.Writer) {
				if len(rows) == 0 {
					fmt.Fprintln(app.Out, "aucune ressource hors gestion enregistrée "+
						"(lancer « ogs import scan » pour inspecter le serveur)")
					return
				}
				fmt.Fprintln(tw, "NATURE\tNOM\tDÉTAIL")
				for _, r := range rows {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Kind, r.Name, r.Detail)
				}
			}))
		},
	}
}
