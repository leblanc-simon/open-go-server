package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/leviathan/ogs/internal/archive"
	"github.com/leviathan/ogs/internal/engine"
	"github.com/leviathan/ogs/internal/exec"
	"github.com/leviathan/ogs/internal/hook"
	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/provider/cron"
	"github.com/leviathan/ogs/internal/provider/system"
	"github.com/leviathan/ogs/internal/provider/unit"
	"github.com/leviathan/ogs/internal/spec"
	"github.com/leviathan/ogs/internal/store"
	"github.com/leviathan/ogs/internal/version"
)

// ArchiveSizeThreshold est la taille au-delà de laquelle l'archivage demande une
// confirmation supplémentaire : une archive de plusieurs gigaoctets prend du
// temps et de la place, et l'exploitant doit le savoir avant de commencer.
const ArchiveSizeThreshold = 5 << 30 // 5 Gio

func newDeleteCommand(app *App) *cobra.Command {
	var (
		purge    bool
		keepData bool
		noDump   bool
	)
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Supprimer un projet, en l'archivant au préalable",
		Long: "Par défaut, tout est archivé avant suppression : déclaration, configurations\n" +
			"générées, dumps de bases, secrets chiffrés et arborescence, avec un manifeste\n" +
			"décrivant ce qui a été retiré. L'ordre suit l'inverse des dépendances.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			if purge {
				// La purge ne laisse aucun recours : la confirmation exige de saisir
				// l'identifiant, ce qui rend l'erreur de frappe improbable.
				if err := app.confirmExact(p.ID(),
					fmt.Sprintf("SUPPRESSION DÉFINITIVE de %s, sans archive. Saisir l'identifiant pour confirmer",
						p.ID())); err != nil {
					return err
				}
			} else if err := app.confirm(fmt.Sprintf(
				"supprimer %s ? (une archive sera créée avant toute suppression)", p.ID())); err != nil {
				return err
			}

			if err := app.openStore(); err != nil {
				return err
			}
			lock, err := engine.Acquire(app.Paths.GlobalLock())
			if err != nil {
				return wrap(CodeLocked, err)
			}
			defer lock.Release()

			provs, err := app.buildProviders()
			if err != nil {
				return err
			}
			defer provs.close()

			// Le hook « preDelete » garde la porte : il peut refuser la suppression,
			// par exemple si une sauvegarde externe n'a pas abouti.
			if _, err := app.runHook(cmd.Context(), provs.Hooks, p, hook.PreDelete, hook.Context{}); err != nil {
				return err
			}

			// 1. Arrêt des services et retrait des tâches : rien ne doit plus
			// s'exécuter pendant que l'on démonte le site.
			app.printf("arrêt des services de %s…\n", p.ID())
			if err := provs.Unit.Control(cmd.Context(), p, "stop"); err != nil {
				app.printf("⚠ arrêt des services : %v\n", err)
			}

			var removed []string
			archived := ""
			if !purge {
				path, list, err := app.archiveProject(cmd, provs, p, noDump)
				if err != nil {
					return err
				}
				archived = path
				removed = list
			}

			// 2. Suppression des fichiers générés, dans l'ordre inverse.
			for _, file := range app.generatedFiles(provs, p) {
				if err := os.Remove(file); err == nil {
					removed = append(removed, file)
				} else if !os.IsNotExist(err) {
					app.printf("⚠ %s : %v\n", file, err)
				}
			}
			// Les instances activées disparaissent avec leur répertoire .wants.
			if err := os.RemoveAll(filepath.Join(app.Paths.SystemdDir(),
				unit.TargetName(p.ID())+".wants")); err != nil && !os.IsNotExist(err) {
				app.printf("⚠ instances : %v\n", err)
			}

			// 3. Ressources vivantes.
			if !keepData {
				app.dropResources(cmd, provs, p, &removed)
			} else {
				app.printf("= --keep-data : bases, files et contenu conservés\n")
			}

			// 4. Compte système et arborescence.
			if !keepData {
				if err := app.removeUser(cmd, provs, p); err != nil {
					app.printf("⚠ suppression du compte : %v\n", err)
				} else {
					removed = append(removed, "utilisateur "+p.Spec.User.Name)
				}
			}

			// 5. Le projet quitte l'inventaire actif.
			if purge {
				if err := os.Remove(app.Paths.ProjectFile(p.ID())); err != nil && !os.IsNotExist(err) {
					return wrap(CodeError, err)
				}
				if err := app.Store.DeleteProject(p.ID()); err != nil {
					app.printf("⚠ état : %v\n", err)
				}
			} else {
				archivedPath := filepath.Join(app.Paths.ArchivedDir(), p.ID()+".yaml")
				if err := os.MkdirAll(filepath.Dir(archivedPath), 0o755); err != nil {
					return wrap(CodeError, err)
				}
				if err := os.Rename(app.Paths.ProjectFile(p.ID()), archivedPath); err != nil && !os.IsNotExist(err) {
					return wrap(CodeError, err)
				}
				if err := app.Store.SetProjectStatus(p.ID(), "archived"); err != nil &&
					!strings.Contains(err.Error(), "absent") {
					app.printf("⚠ état : %v\n", err)
				}
			}

			// Les serveurs doivent oublier ce qui vient de disparaître : sans
			// rechargement, le maître PHP-FPM garde le pool du projet en mémoire —
			// ses workers continuent de tourner et le site reste servi — et le
			// serveur web conserve un vhost dont les fichiers n'existent plus.
			for _, pool := range p.Spec.PHP.Pools {
				if err := provs.PHP.Reload(cmd.Context(), pool.Version); err != nil {
					app.printf("⚠ rechargement de PHP %s : %v\n", pool.Version, err)
				}
			}
			if err := provs.Web.Reload(cmd.Context()); err != nil {
				app.printf("⚠ rechargement du serveur web : %v\n", err)
			}
			if err := app.systemctl(cmd.Context(), "daemon-reload"); err != nil {
				app.printf("⚠ daemon-reload : %v\n", err)
			}

			_, _ = app.runHook(cmd.Context(), provs.Hooks, p, hook.PostDelete,
				hook.Context{Resources: removed, Extra: map[string]string{"archive": archived}})
			_ = app.Audit.Record(p.ID(), "project", "project.delete", "ok", archived)
			app.printf("\n✓ %s supprimé (%d ressource(s))\n", p.ID(), len(removed))
			if archived != "" {
				app.printf("  archive : %s\n", archived)
				app.printf("  restaurer : ogs project restore %s\n", archived)
			} else {
				app.printf("  aucune archive : la suppression était définitive.\n")
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&purge, "purge", false, "supprimer sans archiver (irréversible)")
	f.BoolVar(&keepData, "keep-data", false, "conserver bases, files et contenu")
	f.BoolVar(&noDump, "no-dump", false, "ne pas inclure de sauvegarde de base dans l'archive")
	return cmd
}

// generatedFiles énumère les fichiers produits par ogs pour un projet, dans
// l'ordre inverse de leur création.
func (a *App) generatedFiles(provs *providers, p *model.Project) []string {
	files := []string{
		a.Paths.SudoersFile(p.ID()),
		filepath.Join(a.Paths.CronDir(), cron.FileName(p.ID())),
		// Le lien d'activation d'abord : laissé derrière, il fait subsister dans
		// systemd une référence à une unité introuvable.
		filepath.Join(a.Paths.SystemdDir(), "multi-user.target.wants", unit.TargetName(p.ID())),
		filepath.Join(a.Paths.SystemdDir(), unit.TargetName(p.ID())),
	}
	for _, svc := range p.Spec.Services {
		files = append(files, filepath.Join(a.Paths.SystemdDir(), unit.ServiceName(p.ID(), svc.Name)))
	}
	for _, vh := range provs.Web.VHostNames(p) {
		files = append(files,
			filepath.Join(provs.Engine.EnabledDir(), provs.Engine.FileName(vh)),
			filepath.Join(provs.Engine.AvailableDir(), provs.Engine.FileName(vh)))
	}
	for _, pool := range p.Spec.PHP.Pools {
		name := p.ID()
		if pool.Name != model.MainPoolName {
			name = p.ID() + "_" + pool.Name
		}
		files = append(files, a.Paths.PHPPoolFile(pool.Version, name))
	}
	return files
}

// dropResources supprime les bases et le vhost AMQP.
func (a *App) dropResources(cmd *cobra.Command, provs *providers, p *model.Project, removed *[]string) {
	for _, decl := range p.Spec.Databases {
		obs, err := provs.DB.Observe(cmd.Context(), p)
		if err != nil || len(obs.States) == 0 {
			continue
		}
		for _, state := range obs.States {
			if state.Unavailable != nil {
				a.printf("⚠ base %s : %v\n", decl.Name, state.Unavailable)
				continue
			}
			if err := provs.DB.Drop(cmd.Context(), state.Declaration, state); err != nil {
				a.printf("⚠ suppression de la base %s : %v\n", state.Declaration.Name, err)
				continue
			}
			*removed = append(*removed, fmt.Sprintf("base %s %s",
				state.Declaration.Engine, state.Declaration.Name))
		}
		break
	}
	if p.Spec.AMQP != nil && provs.AMQP.Client != nil {
		if err := provs.AMQP.Client.DeleteVhost(cmd.Context(), p.Spec.AMQP.Vhost); err != nil {
			a.printf("⚠ suppression du vhost amqp : %v\n", err)
		} else {
			*removed = append(*removed, "vhost amqp "+p.Spec.AMQP.Vhost)
		}
		if err := provs.AMQP.Client.DeleteUser(cmd.Context(), p.Spec.AMQP.User); err != nil {
			a.printf("⚠ suppression du compte amqp : %v\n", err)
		}
	}
}

// removeUser supprime le compte système et l'arborescence.
func (a *App) removeUser(cmd *cobra.Command, provs *providers, p *model.Project) error {
	u, err := provs.System.Identity.LookupUser(p.Spec.User.Name)
	if err != nil {
		return err
	}
	if u != nil && !u.Managed && !a.Force {
		return fmt.Errorf("l'utilisateur %s n'a pas été créé par ogs : le supprimer exige --force",
			p.Spec.User.Name)
	}
	if !a.Exec.Simulated() {
		if _, err := a.Exec.Run(cmd.Context(), exec.Command{
			Name: "userdel", Args: []string{p.Spec.User.Name},
		}); err != nil {
			return err
		}
		if _, err := a.Exec.Run(cmd.Context(), exec.Command{
			Name: "groupdel", Args: []string{p.Spec.User.Group},
		}); err != nil {
			a.printf("⚠ suppression du groupe : %v\n", err)
		}
	}
	// L'arborescence part en dernier : tout ce qui précède a pu avoir besoin de la lire.
	if err := os.RemoveAll(p.Spec.Layout.Root); err != nil {
		return err
	}
	return os.RemoveAll(a.Paths.ProjectRunDir(p.ID()))
}

// archiveProject construit l'archive complète du projet.
func (a *App) archiveProject(cmd *cobra.Command, provs *providers, p *model.Project, noDump bool) (string, []string, error) {
	// L'espace disponible est vérifié avant de commencer : échouer au milieu
	// d'une suppression laisserait le système à moitié démonté.
	size, err := treeSize(p.Spec.Layout.Root)
	if err != nil {
		return "", nil, wrap(CodeError, err)
	}
	free, err := archive.Available(a.Paths.ArchivesDir())
	if err == nil && uint64(size) > free {
		return "", nil, exitf(CodeError,
			"espace insuffisant : %s à archiver, %s disponible sous %s",
			humanBytes(size), humanBytes(int64(free)), a.Paths.ArchivesDir())
	}
	if size > ArchiveSizeThreshold {
		if err := a.confirm(fmt.Sprintf(
			"l'archive contiendra environ %s : continuer ?", humanBytes(size))); err != nil {
			return "", nil, err
		}
	}

	path := filepath.Join(a.Paths.ArchivesDir(), archive.FileName(p.ID(), time.Now()))
	w, err := archive.Create(path, archive.Manifest{
		Project: p.ID(), Host: p.Metadata.Host, Env: p.Metadata.Env,
		Reason: "suppression du projet", OGSVersion: version.Version,
	})
	if err != nil {
		return "", nil, wrap(CodeError, err)
	}
	ok := false
	defer func() {
		if !ok {
			w.Abort()
		}
	}()

	// La déclaration : c'est elle qui permettra de tout recréer.
	if err := w.AddFile("spec", "project.yaml", a.Paths.ProjectFile(p.ID())); err != nil {
		return "", nil, wrap(CodeError, err)
	}
	// Les configurations générées, pour comparaison ultérieure.
	var removed []string
	for _, file := range a.generatedFiles(provs, p) {
		if _, err := os.Stat(file); err != nil {
			continue
		}
		if err := w.AddFile("config", "config/"+filepath.Base(file), file); err != nil {
			return "", nil, wrap(CodeError, err)
		}
		removed = append(removed, file)
	}
	// Les dumps : sans eux, l'archive serait inexploitable pour une restauration.
	if !noDump {
		for _, decl := range p.Spec.Databases {
			tmp := filepath.Join(os.TempDir(), fmt.Sprintf("ogs-dump-%s-%s.sql", p.ID(), decl.Name))
			if _, err := provs.DB.Dump(cmd.Context(), p, decl, tmp); err != nil {
				a.printf("⚠ sauvegarde de %s impossible : %v\n", decl.Name, err)
				continue
			}
			if err := w.AddFile("dump", "dumps/"+decl.Name+".sql", tmp); err != nil {
				_ = os.Remove(tmp)
				return "", nil, wrap(CodeError, err)
			}
			_ = os.Remove(tmp)
			a.printf("✓ sauvegarde de %s incluse\n", decl.Name)
		}
	}
	// Les secrets restent chiffrés : l'archive n'est pas une porte de sortie.
	if names, err := a.Store.SecretNames(p.ID()); err == nil {
		for _, name := range names {
			nonce, ciphertext, err := a.Store.GetSecret(p.ID(), name)
			if err != nil {
				continue
			}
			if err := w.AddSecret(name, nonce, ciphertext); err != nil {
				return "", nil, wrap(CodeError, err)
			}
		}
	}
	// L'arborescence complète.
	if err := w.AddTree("tree", "tree", p.Spec.Layout.Root); err != nil {
		return "", nil, wrap(CodeError, err)
	}
	for _, r := range removed {
		w.Note(r)
	}
	manifest, err := w.Close()
	if err != nil {
		return "", nil, wrap(CodeError, err)
	}
	ok = true
	a.printf("✓ archive créée : %s (%s de données, %d secret(s))\n",
		path, humanBytes(manifest.Bytes), manifest.Secrets)
	return path, removed, nil
}

func newRestoreCommand(app *App) *cobra.Command {
	var withData bool
	cmd := &cobra.Command{
		Use:   "restore <archive>",
		Short: "Restaurer un projet depuis une archive",
		Long: "Recrée le projet depuis sa déclaration archivée, réinjecte les secrets scellés,\n" +
			"restaure l'arborescence puis, si demandé, recharge les sauvegardes de bases.\n" +
			"La clé maître doit être la même : sans elle, les secrets sont illisibles.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]
			manifest, err := archive.ReadManifest(path)
			if err != nil {
				return exitf(CodeUsage, "%v", err)
			}
			app.printf("archive de %s (%s, créée le %s)\n", manifest.Project, manifest.Env,
				manifest.CreatedAt.Format("02/01/2006 à 15:04"))
			app.printf("  %d entrée(s), %s de données, %d secret(s)\n",
				len(manifest.Entries), humanBytes(manifest.Bytes), manifest.Secrets)

			target := app.Paths.ProjectFile(manifest.Project)
			if _, err := os.Stat(target); err == nil {
				return exitf(CodeConflict, "le projet %s existe déjà : le supprimer d'abord", manifest.Project)
			}
			if err := app.confirm(fmt.Sprintf("restaurer %s ?", manifest.Project)); err != nil {
				return err
			}

			// La déclaration d'abord : tout le reste s'y adosse.
			data, err := archive.ReadFile(path, "project.yaml")
			if err != nil {
				return exitf(CodeError, "%v", err)
			}
			p, err := spec.Decode(data)
			if err != nil {
				return exitf(CodeValidation, "%v", err)
			}
			if err := app.prepare(p); err != nil {
				return err
			}
			if err := spec.Save(target, p); err != nil {
				return wrap(CodeError, err)
			}
			app.printf("✓ déclaration restaurée : %s\n", target)

			if err := app.openStore(); err != nil {
				return err
			}
			// Les secrets sont réinjectés tels quels : ils restent chiffrés par la
			// même clé maître.
			secrets, err := archive.Secrets(path)
			if err != nil {
				return wrap(CodeError, err)
			}
			for name, pair := range secrets {
				if err := app.Store.PutSecret(p.ID(), name, pair[0], pair[1]); err != nil {
					return wrap(CodeError, err)
				}
			}
			if len(secrets) > 0 {
				app.printf("✓ %d secret(s) réinjecté(s)\n", len(secrets))
				if app.Keys != nil {
					for name, pair := range secrets {
						if _, err := app.Keys.Open(p.ID(), name, pair[0], pair[1]); err != nil {
							app.printf("⚠ %s est illisible avec la clé maître actuelle : "+
								"l'archive vient-elle d'un autre serveur ?\n", name)
							break
						}
					}
				}
			}

			// Puis l'infrastructure, avant de remettre le contenu. Un succès partiel
			// — une base indisponible, par exemple — ne doit pas empêcher la
			// restauration des fichiers : le code est conservé et rendu à la fin.
			partial := error(nil)
			if err := app.applyProjects(cmd, []*model.Project{p}); err != nil {
				if ExitCode(err) != CodePartial {
					return err
				}
				partial = err
			}
			if count, err := archive.Extract(path, "tree", p.Spec.Layout.Root); err != nil {
				app.printf("⚠ restauration de l'arborescence : %v\n", err)
			} else {
				app.printf("✓ %d fichier(s) restauré(s) dans %s\n", count, p.Spec.Layout.Root)
				app.chownTree(cmd, p)
			}
			if withData {
				app.restoreDumps(cmd, path, p, manifest)
			}
			_ = app.Audit.Record(p.ID(), "project", "project.restore", "ok", path)
			app.printf("\n✓ %s restauré\n", p.ID())
			return partial
		},
	}
	cmd.Flags().BoolVar(&withData, "with-data", true, "recharger les sauvegardes de bases")
	return cmd
}

// chownTree rend l'arborescence restaurée à l'utilisateur du projet.
func (a *App) chownTree(cmd *cobra.Command, p *model.Project) {
	if a.Exec.Simulated() {
		return
	}
	if _, err := a.Exec.Run(cmd.Context(), exec.Command{
		Name: "/bin/chown",
		Args: []string{"-R", p.Spec.User.Name + ":" + p.Spec.User.Group,
			filepath.Join(p.Spec.Layout.Root, "www")},
	}); err != nil {
		a.printf("⚠ propriétaire de l'arborescence : %v\n", err)
	}
}

// restoreDumps recharge les sauvegardes présentes dans l'archive.
func (a *App) restoreDumps(cmd *cobra.Command, path string, p *model.Project, manifest archive.Manifest) {
	provs, err := a.buildProviders()
	if err != nil {
		a.printf("⚠ %v\n", err)
		return
	}
	defer provs.close()
	for _, entry := range manifest.Entries {
		if entry.Kind != "dump" {
			continue
		}
		data, err := archive.ReadFile(path, entry.Name)
		if err != nil {
			a.printf("⚠ %s : %v\n", entry.Name, err)
			continue
		}
		tmp := filepath.Join(os.TempDir(), filepath.Base(entry.Name))
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			a.printf("⚠ %s : %v\n", entry.Name, err)
			continue
		}
		name := strings.TrimSuffix(filepath.Base(entry.Name), ".sql")
		var decl *model.Database
		for i := range p.Spec.Databases {
			if p.Spec.Databases[i].Name == name {
				decl = &p.Spec.Databases[i]
			}
		}
		if decl == nil {
			a.printf("⚠ aucune base %s déclarée : sauvegarde ignorée\n", name)
			_ = os.Remove(tmp)
			continue
		}
		if err := provs.DB.Restore(cmd.Context(), p, *decl, tmp); err != nil {
			a.printf("⚠ restauration de %s : %v\n", name, err)
		} else {
			a.printf("✓ base %s restaurée\n", name)
		}
		_ = os.Remove(tmp)
	}
}

func newArchiveListCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "archives",
		Short: "Lister les archives disponibles",
		RunE: func(cmd *cobra.Command, _ []string) error {
			entries, err := os.ReadDir(app.Paths.ArchivesDir())
			if err != nil {
				if os.IsNotExist(err) {
					app.printf("aucune archive\n")
					return nil
				}
				return wrap(CodeError, err)
			}
			type row struct {
				Project string `json:"project" yaml:"project"`
				File    string `json:"file" yaml:"file"`
				Created string `json:"created" yaml:"created"`
				Size    string `json:"size" yaml:"size"`
				Secrets int    `json:"secrets" yaml:"secrets"`
				// Expire dit ce qu'il reste avant la purge : une archive qui va
				// disparaître doit se voir avant, pas après.
				Expire string `json:"expire" yaml:"expire"`
			}
			retention := app.Cfg.Archives.RetentionDays
			var rows []row
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.zst") {
					continue
				}
				full := filepath.Join(app.Paths.ArchivesDir(), e.Name())
				manifest, err := archive.ReadManifest(full)
				if err != nil {
					continue
				}
				info, _ := e.Info()
				size := int64(0)
				if info != nil {
					size = info.Size()
				}
				expire := "jamais"
				if retention > 0 {
					reste := int(manifest.CreatedAt.AddDate(0, 0, retention).Sub(nowFunc()).Hours() / 24)
					switch {
					case reste < 0:
						expire = "au prochain apply"
					case reste == 0:
						expire = "aujourd'hui"
					default:
						expire = fmt.Sprintf("dans %d jour(s)", reste)
					}
				}
				rows = append(rows, row{
					Project: manifest.Project, File: e.Name(),
					Created: manifest.CreatedAt.Format("02/01/2006 15:04"),
					Size:    humanBytes(size), Secrets: manifest.Secrets,
					Expire: expire,
				})
			}
			return wrap(CodeError, app.emit("ArchiveList", rows, nil, func(tw *tabwriter.Writer) {
				if len(rows) == 0 {
					fmt.Fprintln(app.Out, "aucune archive")
					return
				}
				fmt.Fprintln(tw, "PROJET\tARCHIVE\tCRÉÉE\tTAILLE\tSECRETS\tPURGE")
				for _, r := range rows {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
						r.Project, r.File, r.Created, r.Size, r.Secrets, r.Expire)
				}
			}))
		},
	}
}

// confirmExact exige la saisie exacte d'une valeur : une confirmation par oui ou
// non ne suffit pas pour une opération sans recours.
func (a *App) confirmExact(expected, question string) error {
	if a.NonInteractive || a.Yes {
		return exitf(CodeCancelled,
			"cette opération exige une confirmation interactive : elle est sans recours")
	}
	fmt.Fprintf(a.Out, "%s : ", question)
	var answer string
	if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil {
		return exitf(CodeCancelled, "opération annulée")
	}
	if strings.TrimSpace(answer) != expected {
		return exitf(CodeCancelled, "saisie incorrecte : opération annulée")
	}
	return nil
}

// treeSize mesure une arborescence.
func treeSize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	return total, err
}

// humanBytes rend une taille lisible.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d o", n)
	}
	value := float64(n)
	units := []string{"Kio", "Mio", "Gio", "Tio"}
	for _, u := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, u)
		}
	}
	return fmt.Sprintf("%.1f Pio", value)
}

// storeStatus expose le statut persistant, pour les messages.
func storeStatus(st *store.Store, id string) string {
	row, err := st.Project(id)
	if err != nil {
		return "inconnu"
	}
	return row.Status
}

var _ = context.Background
var _ = system.WebUser
