package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/provider/db"
	"github.com/leviathan/ogs/internal/spec"
)

func newDBCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "db",
		Aliases: []string{"database"},
		Short:   "Gérer les bases de données",
	}
	user := &cobra.Command{Use: "user", Short: "Gérer les utilisateurs de base"}
	user.AddCommand(newDBUserAddCommand(app), newDBUserRotateCommand(app))
	cmd.AddCommand(
		newDBListCommand(app),
		newDBAddCommand(app),
		newDBRemoveCommand(app),
		newDBDumpCommand(app),
		newDBRestoreCommand(app),
		user,
	)
	return cmd
}

// dbRow est la vue machine d'une base déclarée.
type dbRow struct {
	Project    string `json:"project" yaml:"project"`
	Engine     string `json:"engine" yaml:"engine"`
	Name       string `json:"name" yaml:"name"`
	User       string `json:"user" yaml:"user"`
	Privileges string `json:"privileges" yaml:"privileges"`
	Cluster    string `json:"cluster,omitempty" yaml:"cluster,omitempty"`
	State      string `json:"state" yaml:"state"`
}

func newDBListCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "list [id...]",
		Aliases: []string{"ls"},
		Short:   "Lister les bases déclarées et leur état réel",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := app.targets(args, "")
			if err != nil {
				return err
			}
			if err := app.openStore(); err != nil {
				return err
			}
			provs, err := app.buildProviders()
			if err != nil {
				return err
			}
			defer provs.close()

			var rows []dbRow
			var warnings []string
			for _, p := range projects {
				if len(p.Spec.Databases) == 0 {
					continue
				}
				obs, err := provs.DB.Observe(cmd.Context(), p)
				if err != nil {
					return wrap(CodeError, err)
				}
				for _, st := range obs.States {
					state := "absente"
					switch {
					case st.Unavailable != nil:
						state = "moteur indisponible"
						warnings = append(warnings, fmt.Sprintf("%s : %v", p.ID(), st.Unavailable))
					case st.DBExists && st.UserExists:
						state = "en place"
					case st.DBExists:
						state = "base présente, utilisateur absent"
					}
					rows = append(rows, dbRow{
						Project: p.ID(), Engine: st.Declaration.Engine, Name: st.Declaration.Name,
						User: st.Declaration.User, Privileges: orValue(st.Declaration.Privileges, "all"),
						Cluster: st.Declaration.Cluster, State: state,
					})
				}
			}
			return wrap(CodeError, app.emit("DatabaseList", rows, warnings, func(tw *tabwriter.Writer) {
				if len(rows) == 0 {
					fmt.Fprintln(app.Out, "aucune base déclarée")
					return
				}
				fmt.Fprintln(tw, "PROJET\tMOTEUR\tBASE\tUTILISATEUR\tPRIVILÈGES\tÉTAT")
				for _, r := range rows {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
						r.Project, r.Engine, r.Name, r.User, r.Privileges, r.State)
				}
			}))
		},
	}
}

func newDBAddCommand(app *App) *cobra.Command {
	var (
		engine     string
		role       string
		privileges string
		cluster    string
		extensions []string
	)
	cmd := &cobra.Command{
		Use:   "add <id>",
		Short: "Déclarer une base pour un projet",
		Long: "Ajoute la déclaration au fichier du projet. La base est créée par « ogs apply ».\n" +
			"Sans --role, c'est la base principale : elle porte le nom du projet.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			switch engine {
			case "mysql", "mariadb", "postgres":
			default:
				return exitf(CodeUsage, "moteur %q inconnu (mysql, mariadb ou postgres)", engine)
			}
			if _, err := db.ParsePrivilege(privileges); err != nil {
				return exitf(CodeUsage, "%s", err)
			}
			decl := db.DefaultDatabase(p, engine)
			if role != "" {
				decl = db.RoleDatabase(p, engine, role)
			}
			decl.Privileges = privileges
			decl.Cluster = cluster
			decl.Extensions = extensions
			for _, existing := range p.Spec.Databases {
				if existing.Engine == decl.Engine && existing.Name == decl.Name {
					return exitf(CodeConflict, "la base %s %s est déjà déclarée", decl.Engine, decl.Name)
				}
			}
			p.Spec.Databases = append(p.Spec.Databases, decl)
			if app.DryRun {
				app.printf("--dry-run : la base %s %s serait déclarée\n", decl.Engine, decl.Name)
				return nil
			}
			if err := spec.Save(app.Paths.ProjectFile(p.ID()), p); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ base %s %s déclarée (utilisateur %s, privilèges %s)\n",
				decl.Engine, decl.Name, decl.User, orValue(decl.Privileges, "all"))
			app.printf("\nAppliquer :\n  ogs apply %s\n", p.ID())
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&engine, "engine", "mysql", "moteur : mysql, mariadb ou postgres")
	f.StringVar(&role, "role", "", "rôle d'une base additionnelle (suffixe du nom)")
	f.StringVar(&privileges, "privileges", "all", "jeu de privilèges : all, crud ou readonly")
	f.StringVar(&cluster, "cluster", "", "cluster PostgreSQL, par exemple 16/main")
	f.StringSliceVar(&extensions, "extension", nil, "extension PostgreSQL à activer (répétable)")
	return cmd
}

func newDBRemoveCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <id> <base>",
		Short: "Retirer la déclaration d'une base",
		Long: "Retire la déclaration du fichier du projet. La base et son utilisateur ne sont\n" +
			"pas supprimés du serveur : cette opération ne détruit aucune donnée.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			kept := make([]model.Database, 0, len(p.Spec.Databases))
			found := false
			for _, d := range p.Spec.Databases {
				if d.Name == args[1] {
					found = true
					continue
				}
				kept = append(kept, d)
			}
			if !found {
				return exitf(CodeUsage, "aucune base %q déclarée dans %s", args[1], p.ID())
			}
			p.Spec.Databases = kept
			if err := spec.Save(app.Paths.ProjectFile(p.ID()), p); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ déclaration retirée : %s\n", args[1])
			app.printf("  la base et son utilisateur subsistent sur le serveur ; les supprimer manuellement si besoin.\n")
			return nil
		},
	}
}

func newDBDumpCommand(app *App) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "dump <id> [base]",
		Short: "Sauvegarder une base",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			decl, err := pickDatabase(p, args)
			if err != nil {
				return err
			}
			if output == "" {
				// Un emplacement défini, à côté des archives, plutôt que le répertoire
				// courant : un dump contient les données des clients, et il n'a rien à
				// faire à la racine du système parce que la commande a été lancée de là.
				dir := app.Paths.DumpsDir()
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return wrap(CodeError, err)
				}
				output = filepath.Join(dir, fmt.Sprintf("%s-%s-%s.sql",
					p.ID(), decl.Name, nowFunc().UTC().Format("20060102-150405")))
			}
			if err := app.openStore(); err != nil {
				return err
			}
			provs, err := app.buildProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			path, err := provs.DB.Dump(cmd.Context(), p, decl, output)
			if err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ sauvegarde écrite : %s\n", path)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "O", "", "fichier de destination")
	return cmd
}

func newDBRestoreCommand(app *App) *cobra.Command {
	var input string
	cmd := &cobra.Command{
		Use:   "restore <id> [base]",
		Short: "Restaurer une base depuis une sauvegarde",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if input == "" {
				return exitf(CodeUsage, "préciser le fichier à restaurer avec --input")
			}
			if _, err := os.Stat(input); err != nil {
				return exitf(CodeUsage, "%v", err)
			}
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			decl, err := pickDatabase(p, args)
			if err != nil {
				return err
			}
			if err := app.confirm(fmt.Sprintf(
				"restaurer %s dans la base %s (le contenu actuel peut être écrasé) ?", input, decl.Name)); err != nil {
				return err
			}
			if err := app.openStore(); err != nil {
				return err
			}
			provs, err := app.buildProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			if err := provs.DB.Restore(cmd.Context(), p, decl, input); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ base %s restaurée depuis %s\n", decl.Name, input)
			return nil
		},
	}
	cmd.Flags().StringVarP(&input, "input", "i", "", "fichier de sauvegarde à restaurer")
	return cmd
}

func newDBUserAddCommand(app *App) *cobra.Command {
	var privileges string
	cmd := &cobra.Command{
		Use:   "add <id> <utilisateur>",
		Short: "Déclarer un utilisateur de base supplémentaire",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			if len(p.Spec.Databases) == 0 {
				return exitf(CodeUsage, "le projet %s n'a aucune base déclarée", p.ID())
			}
			if _, err := db.ParsePrivilege(privileges); err != nil {
				return exitf(CodeUsage, "%s", err)
			}
			name := args[1]
			for _, u := range p.Spec.Databases[0].ExtraUsers {
				if u.User == name {
					return exitf(CodeConflict, "l'utilisateur %s est déjà déclaré", name)
				}
			}
			p.Spec.Databases[0].ExtraUsers = append(p.Spec.Databases[0].ExtraUsers,
				model.DBUser{User: name, Privileges: privileges})
			if err := spec.Save(app.Paths.ProjectFile(p.ID()), p); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ utilisateur %s déclaré sur %s (privilèges %s)\n",
				name, p.Spec.Databases[0].Name, privileges)
			return nil
		},
	}
	cmd.Flags().StringVar(&privileges, "privileges", "readonly", "jeu de privilèges : all, crud ou readonly")
	return cmd
}

func newDBUserRotateCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "rotate <id> <utilisateur>",
		Short: "Régénérer le mot de passe d'un utilisateur de base",
		Long: "Applique un nouveau mot de passe au moteur et le scelle dans le keystore.\n" +
			"L'application doit ensuite reprendre sa chaîne de connexion : « ogs secrets env ».",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			user := args[1]
			var decl *model.Database
			for i := range p.Spec.Databases {
				if p.Spec.Databases[i].User == user {
					decl = &p.Spec.Databases[i]
				}
				for _, u := range p.Spec.Databases[i].ExtraUsers {
					if u.User == user {
						decl = &p.Spec.Databases[i]
					}
				}
			}
			if decl == nil {
				return exitf(CodeUsage, "aucun utilisateur %q déclaré dans %s", user, p.ID())
			}
			if err := app.confirm(fmt.Sprintf("régénérer le mot de passe de %s ?", user)); err != nil {
				return err
			}
			if err := app.openStore(); err != nil {
				return err
			}
			provs, err := app.buildProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			if _, err := provs.DB.RotatePassword(cmd.Context(), p, *decl, user); err != nil {
				return wrap(CodeError, err)
			}
			_ = app.Audit.Record(p.ID(), "db/"+decl.Engine+"/"+user, "db.user.rotate", "ok", "")
			app.printf("✓ mot de passe de %s régénéré\n", user)
			app.printf("  reprendre la chaîne de connexion applicative :\n    ogs secrets env %s\n", p.ID())
			return nil
		},
	}
}

// pickDatabase retrouve la base visée : celle nommée, ou l'unique déclarée.
func pickDatabase(p *model.Project, args []string) (model.Database, error) {
	if len(p.Spec.Databases) == 0 {
		return model.Database{}, exitf(CodeUsage, "le projet %s n'a aucune base déclarée", p.ID())
	}
	if len(args) < 2 {
		if len(p.Spec.Databases) > 1 {
			names := make([]string, 0, len(p.Spec.Databases))
			for _, d := range p.Spec.Databases {
				names = append(names, d.Name)
			}
			return model.Database{}, exitf(CodeUsage,
				"plusieurs bases déclarées, préciser laquelle : %s", strings.Join(names, ", "))
		}
		return p.Spec.Databases[0], nil
	}
	for _, d := range p.Spec.Databases {
		if d.Name == args[1] {
			return d, nil
		}
	}
	return model.Database{}, exitf(CodeUsage, "aucune base %q déclarée dans %s", args[1], p.ID())
}
