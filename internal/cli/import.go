package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/importer"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/db"
	"leblanc.io/open-go-server/internal/provider/mail"
	"leblanc.io/open-go-server/internal/spec"
	"leblanc.io/open-go-server/internal/validate"
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
	// Un socket refusé (D92) n'est pas une absence : les bases du moteur ne sont
	// pas inventoriées, et il faut le dire.
	if socket, err := db.MySQLSocket(a.Paths.Prefix()); err != nil {
		a.printf("⚠ bases MySQL non inventoriées : %v\n", err)
	} else if socket != "" {
		if client, err := db.NewMySQLClient(socket); err == nil {
			clients["mysql"] = client
		}
	}
	if clusters := db.DetectClusters(a.Paths.Prefix()); len(clusters) > 0 {
		if client, err := db.NewPostgresClient(clusters[0], os.Getenv("OGS_PG_ADMIN")); err == nil {
			clients["postgres"] = client
		} else {
			a.printf("⚠ bases PostgreSQL non inventoriées : %v\n", err)
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

			// Des tables DKIM tenues à la main sont signalées, non adoptées (D69) :
			// reprendre une clé exigerait de la sceller telle quelle sous son
			// sélecteur — le seul moyen de ne pas republier chez le client —, et
			// OpenDKIM n'accepte qu'une KeyTable, que les projets adoptés puis
			// signés par ogs lui disputeraient.
			var warnings []string
			dkim := &mail.Provider{Paths: app.Paths, Cfg: app.Cfg}
			if table, foreign := dkim.ForeignTables(); foreign {
				warnings = append(warnings, fmt.Sprintf("opendkim.conf désigne la KeyTable %s, qui "+
					"n'est pas celle d'ogs : ses clés ne sont pas adoptées, et un projet signé par "+
					"ogs ne sera signé qu'une fois opendkim.conf pointé vers %s", table,
					app.Paths.OpenDKIMKeyTable()))
			}

			if app.Format != "table" {
				return wrap(CodeError, app.emit("ImportScan", map[string]any{
					"candidates": candidates, "unmanaged": unmanaged,
					"summary": importer.Summary(resources),
				}, warnings, nil))
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
			for _, w := range warnings {
				app.printf("\n⚠ %s\n", w)
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

			p, notes, err := importer.Adopt(*chosen, app.homeRoot(), app.Cfg.PrepDomain(), model.WithLogsRoot(app.logsRoot()))
			if err != nil {
				return exitf(CodeValidation, "%v", err)
			}
			notes = withoutReservedDatabases(p, notes, app.reservedDBAccounts())
			path := app.Paths.ProjectFile(p.ID())
			if _, err := os.Stat(path); err == nil {
				return exitf(CodeConflict, "le projet %s est déjà déclaré (%s)", p.ID(), path)
			}
			// L'adoption est une porte d'entrée du parc au même titre que « site
			// create » et « project create » : sans ce contrôle, deux adoptions
			// revendiquant le même host, le même home ou la même base sont toutes
			// deux écrites, et le conflit n'apparaît qu'au premier apply portant sur
			// tout le parc — bien après que l'exploitant a cru le serveur repris.
			// Seuls comptent les conflits qui touchent le nouveau projet : une collision
			// entre deux voisins ne bloque pas une création sans rapport (D94).
			if err := app.checkFleetUniqueness(p); err != nil {
				return err
			}

			app.printf("adoption de %s\n", p.ID())
			app.printf("  utilisateur %s\n", p.Spec.User.Name)
			app.printf("  racine      %s\n", p.Spec.Layout.Root)
			// Une base déclarée voit son compte recevoir tous les droits à l'apply,
			// et « project delete » les supprime l'une et l'autre : l'exploitant doit
			// les voir avant de confirmer, compte et hôte compris, comme ce qui a
			// été laissé de côté et pourquoi. Le scan seul le disait.
			for _, d := range p.Spec.Databases {
				compte := d.User
				if d.Engine != "postgres" {
					compte += "@" + d.Host
				}
				app.printf("  base        %s %s (compte %s)\n", d.Engine, d.Name, compte)
			}
			for _, u := range chosen.Uncertain {
				app.printf("  ⚠ %s\n", u)
			}
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
			// Rien n'est mémorisé des ressources laissées de côté (D95) : aucune
			// déclaration ne les porte, donc le moteur ne les planifie pas, et les
			// conflits se jugent sur le système à chaque plan, non sur un relevé
			// fait ici — qui ne voyait ni les autres candidats, ni ce qui change
			// après l'import, et que rien ne relisait.
			if err := app.openStore(); err == nil {
				_ = app.Audit.Record(p.ID(), "project", "import.adopt", "ok", path)
			}
			app.printf("\n✓ %s adopté : %s\n", p.ID(), path)
			app.printf("  vérifier avant d'appliquer :\n    ogs diff %s\n", p.ID())
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "désigner le candidat par son host")
	return cmd
}

// unmanagedRow est une ressource que l'inspection n'a rattachée à aucun candidat.
type unmanagedRow struct {
	Kind   string `json:"kind" yaml:"kind"`
	Name   string `json:"name" yaml:"name"`
	Detail string `json:"detail" yaml:"detail"`
}

func newImportUnmanagedCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "unmanaged",
		Short: "Lister les ressources laissées hors gestion",
		Long: "Lecture seule : inspecte le serveur, comme « import scan », et n'affiche que ce\n" +
			"qui n'est rattaché à aucun projet candidat. La liste est celle du moment : ogs\n" +
			"ne tient pas de registre de ce qu'il laisse de côté, les conflits se jugeant\n" +
			"sur le système à chaque plan.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			scanner, err := app.scanner()
			if err != nil {
				return err
			}
			resources, err := scanner.Scan(cmd.Context())
			if err != nil {
				return wrap(CodeError, err)
			}
			_, unmanaged := importer.Correlate(resources, app.homeRoot(), app.Cfg.PrepDomain())
			rows := make([]unmanagedRow, 0, len(unmanaged))
			for _, r := range unmanaged {
				rows = append(rows, unmanagedRow{Kind: string(r.Kind), Name: r.Name, Detail: r.Path})
			}
			sort.Slice(rows, func(i, j int) bool {
				if rows[i].Kind != rows[j].Kind {
					return rows[i].Kind < rows[j].Kind
				}
				return rows[i].Name < rows[j].Name
			})
			return wrap(CodeError, app.emit("UnmanagedList", rows, nil, func(tw *tabwriter.Writer) {
				if len(rows) == 0 {
					fmt.Fprintln(app.Out, "aucune ressource hors gestion : tout ce que l'inspection voit "+
						"est géré par ogs ou rattaché à un projet candidat (« ogs import scan »)")
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

// withoutReservedDatabases retire de l'adoption les bases dont le compte est
// réservé — root, celui de la supervision (D91) : le projet en recevrait des
// droits, « db user rotate », que la note d'Adopt conseille, changerait le mot de
// passe d'administration du moteur, et la déclaration serait refusée dès la
// commande suivante. Chaque base écartée est dite, et le conseil de rotation de
// son compte tombe avec elle.
func withoutReservedDatabases(p *model.Project, notes, reserved []string) []string {
	kept := p.Spec.Databases[:0]
	for _, d := range p.Spec.Databases {
		why := validate.ReservedDBAccount(d.Engine, d.User, reserved...)
		if why == "" {
			kept = append(kept, d)
			continue
		}
		rotate := "« ogs db user rotate " + p.ID() + " " + d.User + " »"
		filtered := notes[:0]
		for _, n := range notes {
			if !strings.Contains(n, rotate) {
				filtered = append(filtered, n)
			}
		}
		notes = append(filtered, fmt.Sprintf("base %s non déclarée : son compte %s est réservé (%s) — "+
			"lui donner un compte propre avant de l'adopter", d.Name, d.User, why))
	}
	p.Spec.Databases = kept
	return notes
}
