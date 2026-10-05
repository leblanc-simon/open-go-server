package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/archive"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/hook"
	"leblanc.io/open-go-server/internal/model"
	appprov "leblanc.io/open-go-server/internal/provider/app"
	"leblanc.io/open-go-server/internal/provider/cron"
	"leblanc.io/open-go-server/internal/provider/php"
	"leblanc.io/open-go-server/internal/provider/system"
	"leblanc.io/open-go-server/internal/provider/unit"
	"leblanc.io/open-go-server/internal/spec"
	"leblanc.io/open-go-server/internal/store"
	"leblanc.io/open-go-server/internal/validate"
	"leblanc.io/open-go-server/internal/version"
)

// ArchiveSizeThreshold est la taille au-delà de laquelle l'archivage demande une
// confirmation supplémentaire : une archive de plusieurs gigaoctets prend du
// temps et de la place, et l'exploitant doit le savoir avant de commencer. C'est
// une variable pour que les tests puissent l'abaisser.
var ArchiveSizeThreshold int64 = 5 << 30 // 5 Gio

// deleteOptions porte les options de « project delete » que les gardes
// consultent : ce qui sera effacé décide de ce qu'il faut vérifier.
type deleteOptions struct {
	purge, keepData, noDump bool
}

// dumpsRequired dit si les sauvegardes conditionnent la suppression : une
// archive est faite, elle doit porter les données, et les bases vont être
// supprimées. Sous --keep-data les bases restent, et une sauvegarde manquée ne
// coûte qu'un avertissement.
func (o deleteOptions) dumpsRequired() bool { return !o.purge && !o.noDump && !o.keepData }

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
			"décrivant ce qui a été retiré. L'ordre suit l'inverse des dépendances.\n\n" +
			"Avant tout geste, la suppression refuse une racine qu'un autre projet partage ou\n" +
			"englobe, un point de montage sous l'arborescence, une base qu'elle ne peut pas\n" +
			"sauvegarder (sauf --no-dump), un fichier de configuration qu'ogs n'a pas écrit et\n" +
			"qui désigne peut-être le compte ou l'arborescence, et, sans --force, le compte ou\n" +
			"les bases d'un projet qu'ogs n'a pas créés. Ces refus sont énumérés ensemble, chacun\n" +
			"avec ce qui le lève. Seuls partent les fichiers qu'ogs a écrits pour ce projet ;\n" +
			"sous --keep-data, l'archive ne recopie pas une arborescence qui n'est pas au seul\n" +
			"projet.\n\n" +
			"Le compte emporte sa crontab personnelle et ses tâches at, recopiées dans l'archive,\n" +
			"son linger, et les fichiers de son UID dans /tmp, /var/tmp et /dev/shm ; son groupe\n" +
			"ne part que si personne d'autre ne le tient.\n\n" +
			"Une interruption reçue avant le premier retrait abandonne la suppression et relance\n" +
			"les services arrêtés ; reçue après, elle laisse la suppression aller à son terme.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			opts := deleteOptions{purge: purge, keepData: keepData, noDump: noDump}

			// Le verrou est pris d'abord, comme pour apply : ce que les remparts
			// constatent ne doit pas changer sous eux avant le geste.
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

			// Les remparts sont éprouvés AVANT tout le reste, et avant la
			// confirmation : un « oui » ne doit jamais précéder un refus. La
			// suppression franchit très vite le point de non-retour — archive,
			// fichiers générés, bases, vhosts, compte système —, si bien qu'un refus
			// prononcé au moment d'effacer l'arborescence ne sauvait plus que
			// celle-ci et conseillait de corriger une déclaration qui venait de
			// partir aux archives. Le compte non créé par ogs en était le jumeau :
			// son refus tombait à l'étape 5, tout le reste démonté, et le « --force »
			// qu'il conseillait échouait faute de déclaration. Tout ce qui peut
			// refuser le geste le refuse donc ici ; « --keep-data », qui ne touche
			// ni au compte, ni aux bases, ni à l'arborescence, reste ouvert.
			//
			// Et tout est nommé d'un seul coup : un refus qui en masque un autre
			// fait suivre à l'exploitant un remède qui coûte — retirer le vhost
			// d'un site adopté, c'est couper le site — avant qu'il n'apprenne que
			// le compte, les bases ou un partage monté refusent encore. Une base
			// que l'on ne pourra pas sauvegarder en est.
			refused := app.deletionRefusals(provs, p, opts)
			var dumps []model.Database
			if !purge && !noDump {
				var r *refusal
				if dumps, r = app.databasesToDump(cmd, provs, p, opts); r != nil {
					refused = append(refused, r)
				}
			}
			if err := refused.err(p.ID()); err != nil {
				return err
			}
			// Ce que --force lève est nommé avant la confirmation : sans quoi le
			// drapeau donné pour le compte emportait aussi, sans un mot, les bases
			// qu'un import avait rattachées à tort.
			if app.Force && !keepData {
				if forced, _ := app.forcedRemovals(p); len(forced) > 0 {
					app.printf("= --force : la suppression efface aussi ce qu'ogs n'a pas créé — %s\n",
						strings.Join(forced, " ; "))
				}
			}
			scope := archiveScope{dumps: dumps, strict: opts.dumpsRequired()}
			if keepData && !purge {
				scope.skipTree, scope.skipLogs = app.archiveExclusions(p)
			}
			// Ce que le compte a posé hors d'ogs — crontab, tâches at, linger — est
			// relu avant l'archive, qui en garde la copie, puis retiré avec le compte
			// (D107). Sous --keep-data, le compte reste, et tout cela avec lui.
			jobs := app.userJobs(cmd, provs, p, opts)
			scope.jobs = jobs

			if purge {
				// Ce qui part sans copie est nommé avant la saisie : le fichier
				// d'environnement est le seul contenu de l'exploitant qu'ogs efface
				// hors du home, et la purge n'en garde rien (D100).
				if _, err := os.Lstat(app.Paths.EnvFile(p.ID())); err == nil && !keepData {
					app.printf("= --purge : %s (contenu de l'exploitant) sera effacé sans copie\n",
						app.Paths.EnvFile(p.ID()))
				}
				if names := describeJobs(jobs); len(names) > 0 {
					app.printf("= --purge : %s seront retirés sans copie\n", strings.Join(names, ", "))
				}
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
			// La taille de l'archive passe elle aussi avant l'arrêt des services :
			// refuser une archive trop grosse, ou manquer de place, laissait derrière
			// soi des workers arrêtés que rien ne relançait.
			if !purge {
				if err := app.checkArchiveSize(p, scope); err != nil {
					return err
				}
			}

			// Une interruption reçue pendant les questions n'a encore rien arrêté.
			if err := interrupted(cmd.Context()); err != nil {
				return err
			}

			// Le hook « preDelete » garde la porte : il peut refuser la suppression,
			// par exemple si une sauvegarde externe n'a pas abouti.
			if _, err := app.runHook(cmd.Context(), provs.Hooks, p, hook.PreDelete, hook.Context{}); err != nil {
				return err
			}

			// Les avertissements sont comptés : une suppression qui laisse une
			// ressource derrière elle n'est pas un succès, et le code 6 le dit sans
			// pour autant faire échouer ce qui a bien été démonté.
			warnings := 0
			warn := func(format string, args ...any) {
				warnings++
				app.warnf("⚠ "+format+"\n", args...)
			}

			// 1. Arrêt des services et retrait des tâches : rien ne doit plus
			// s'exécuter pendant que l'on démonte le site. Une cible que systemd n'a
			// pas chargée n'a rien à arrêter : l'annoncer comme un échec ferait
			// passer pour un défaut le cas d'un projet sans service.
			stopped := false
			if app.unitActive(cmd.Context(), unit.TargetName(p.ID())) {
				app.printf("arrêt des services de %s…\n", p.ID())
				// Un arrêt en échec a pu en arrêter une partie : la cible était active,
				// c'est elle qu'un abandon relance.
				stopped = true
				if err := provs.Unit.Control(cmd.Context(), p, "stop"); err != nil {
					warn("arrêt des services : %v", err)
				}
			}
			// Jusqu'au point de non-retour, renoncer doit rendre le système tel
			// qu'on l'a trouvé : un dump ou une archive en échec laissait les workers
			// arrêtés — commandes, courriels et files en souffrance, sans un mot —
			// jusqu'au prochain redémarrage de la machine. Une commande tuée par une
			// interruption échoue sur « context canceled » : c'est l'annulation
			// demandée, qui rend le code 9, non une panne.
			abandon := func(err error) error {
				if stopped {
					app.restartServices(cmd.Context(), provs, p)
				}
				if cmd.Context().Err() != nil && ExitCode(err) != CodeCancelled {
					return exitf(CodeCancelled, "interruption reçue : suppression abandonnée avant tout retrait (%v)", err)
				}
				return err
			}

			// Le sort des fichiers générés est tranché avant l'archive : elle ne
			// recueille, et son manifeste ne déclare retiré, que ce qui part vraiment.
			files, kept := app.projectFiles(provs, p)
			scope.files = files
			// Les certificats du projet aussi (D100) : ils partaient nulle part, et
			// doctor avertissait à vie de leur expiration.
			certs, keptCerts := app.projectCertificates(provs, p)
			scope.certs = certs

			var removed []string
			archived := ""
			if !purge {
				path, list, err := app.archiveProject(cmd, provs, p, scope, warn)
				if err != nil {
					return abandon(err)
				}
				archived = path
				removed = list
			}
			// Le point de non-retour. Une interruption (Ctrl+C, SIGTERM) reçue
			// pendant l'archive est un abandon comme un autre : main n'arrête pas le
			// processus, il annule le contexte, que rien ici ne consultait —
			// l'archive allait à son terme, puis la suppression continuait, chaque
			// commande échouant sur « context canceled » : home effacé sous un
			// compte et des workers FPM toujours vivants, fichiers de parc jamais
			// recalculés.
			//
			// L'archive a pu prendre des minutes, pendant lesquelles un partage a pu
			// être monté sous la racine, une déclaration voisine ou un vhost posés à
			// la main : les remparts sont éprouvés une dernière fois, tant qu'y
			// renoncer ne coûte que l'archive, qui n'a plus d'objet.
			err = interrupted(cmd.Context())
			if err == nil {
				err = app.deletionRefusals(provs, p, opts).err(p.ID())
			}
			if err != nil {
				if archived != "" {
					_ = os.Remove(archived)
				}
				return abandon(err)
			}
			// Passé ce point, la suppression va à son terme : y renoncer en chemin
			// ne rendrait rien, et laisserait le pire des états — un home effacé
			// sous un compte vivant, une prison fail2ban désignant des journaux
			// disparus, qui empêche le démon de redémarrer. Une interruption ne
			// coupe donc plus rien ; elle est seulement signalée, dès qu'une étape
			// s'achève.
			interruption := cmd.Context()
			cmd.SetContext(context.WithoutCancel(interruption))
			signaled := false
			signalInterruption := func() {
				if !signaled && interruption.Err() != nil {
					signaled = true
					app.printf("= interruption reçue après le point de non-retour : la suppression va à son terme\n")
				}
			}

			// 2. Suppression des fichiers générés, dans l'ordre inverse. Un fichier
			// qu'ogs n'a pas écrit pour ce projet reste en place : son nom ne prouve
			// rien — le pool d'un site adopté porte celui de l'identifiant alors qu'il
			// sert aussi un vhost voisin resté hors d'ogs, qui tombait en 502 sous
			// « --keep-data » même. Il est nommé, et la suppression rend le code 6 :
			// ce qu'il configure est toujours actif. Hors --keep-data, seul celui
			// d'un autre projet arrive jusqu'ici — keptFilesRefusal a refusé les autres.
			for _, k := range kept {
				warn("%s conservé : %s", k.path, k.reason)
			}
			for _, file := range files {
				if err := os.Remove(file); err == nil {
					removed = append(removed, file)
				} else if !os.IsNotExist(err) {
					warn("%s : %v", file, err)
				}
			}
			// Le fichier d'environnement porte des secrets : il ne survit pas au
			// projet, et l'archive en garde une copie scellée. Mais son contenu est
			// celui de l'exploitant, qu'ogs n'a jamais écrit (D75) : sous
			// --keep-data, qui garde données et contenu, il reste (D100) — il
			// partait sans que rien ne puisse le reproduire.
			if envFile := app.Paths.EnvFile(p.ID()); keepData {
				if _, err := os.Lstat(envFile); err == nil {
					app.printf("= --keep-data : %s conservé\n", envFile)
				}
			} else if err := os.Remove(envFile); err == nil {
				removed = append(removed, envFile)
			} else if !os.IsNotExist(err) {
				warn("%s : %v", envFile, err)
			}
			// Les instances activées disparaissent avec leur répertoire .wants, qui
			// suit le sort de la cible : une cible conservée garde ses instances.
			if target := filepath.Join(app.Paths.SystemdDir(), unit.TargetName(p.ID())); !keptPath(kept, target) {
				if err := os.RemoveAll(target + ".wants"); err != nil && !os.IsNotExist(err) {
					warn("instances : %v", err)
				}
			}
			// Les répertoires de surcharge des vhosts (D63) partent avec eux, mais
			// seulement s'ils sont vides : un fichier déposé là porte le travail de
			// quelqu'un, et ogs ne l'a pas écrit. Ce qui reste est annoncé — sinon
			// il ne resterait qu'un répertoire orphelin que personne ne relie plus
			// à un projet disparu.
			for _, vh := range provs.Web.VHostNames(p) {
				for _, dir := range provs.Web.RemoveIncludes(vh) {
					app.printf("= surcharges conservées : %s (non écrites par ogs)\n", dir)
				}
			}

			// Les enregistrements DKIM publiés (D69) sont hors de portée : ogs ne les
			// a pas posés, et c'est le seul moment où l'exploitant apprend qu'ils
			// sont à retirer. Les fichiers de clés, eux, partent après le recalcul
			// des tables — voir plus bas.
			dkimRecords, _ := provs.Mail.Records(p)
			for _, r := range dkimRecords {
				app.printf("= enregistrement DKIM à retirer du DNS : %s\n", r.Name)
			}

			// 3. Ressources vivantes.
			signalInterruption()
			if !keepData {
				app.dropResources(cmd, provs, p, &removed, warn)
			} else {
				app.printf("= --keep-data : bases, files et contenu conservés\n")
			}

			// 4. Les serveurs doivent oublier ce qui vient de disparaître : sans
			// rechargement, le maître PHP-FPM garde le pool du projet en mémoire —
			// ses workers continuent de tourner et le site reste servi — et le
			// serveur web conserve un vhost dont les fichiers n'existent plus.
			//
			// Ce rechargement précède impérativement la suppression du compte : ces
			// workers appartiennent à l'utilisateur du projet, et userdel refuse de
			// supprimer un compte dont un processus tourne encore (« user is
			// currently used by process »). L'ordre inverse laissait le compte, le
			// groupe et le home en place tout en annonçant un succès.
			for _, pool := range p.Spec.PHP.Pools {
				if err := provs.PHP.Reload(cmd.Context(), pool.Version); err != nil {
					// Refusé, le maître sert encore le pool supprimé : son socket sous
					// un UID que userdel libère et que useradd redonnera (D105).
					if refus := (*php.RefusedError)(nil); errors.As(err, &refus) {
						warn("rechargement de PHP %s refusé : le pool de %s reste servi jusqu'à correction de "+
							"la cause, puis « systemctl reload %s » :\n  %v", pool.Version, p.ID(), refus.Unit, refus.Err)
						continue
					}
					warn("rechargement de PHP %s : %v", pool.Version, err)
				}
			}
			// Un serveur web à l'arrêt n'a rien à recharger : le signaler comme un
			// échec accuserait ogs de ce qui est un état légitime du système.
			if app.unitActive(cmd.Context(), provs.Web.Engine.Unit()) {
				if err := provs.Web.Reload(cmd.Context()); err != nil {
					warn("rechargement du serveur web : %v", err)
				}
			}
			if err := app.systemctl(cmd.Context(), "daemon-reload"); err != nil {
				warn("daemon-reload : %v", err)
			}
			// Les certificats partent une fois le serveur web rechargé sans les
			// vhosts qui les désignaient : retirés avant, le rechargement échouait
			// sur un fichier introuvable. L'archive en garde une copie scellée.
			for _, k := range keptCerts {
				warn("certificat %s conservé : %s", k.name, k.reason)
			}
			app.removeCertificates(provs, certs, &removed, warn)

			// 5. Compte système et arborescence, une fois que plus rien ne tourne
			// sous cette identité.
			signalInterruption()
			if !keepData {
				if err := app.removeUser(cmd, provs, p, jobs, &removed, warn); err != nil {
					warn("suppression du compte : %v", err)
				} else {
					removed = append(removed, "utilisateur "+p.Spec.User.Name)
				}
			} else {
				// Le compte reste, et l'UID avec lui : ce qu'il a posé hors d'ogs et ce
				// qu'il tient dans les répertoires temporaires restent à lui (D107).
				app.printf("= --keep-data : compte %s conservé, avec sa crontab, ses tâches at, son linger "+
					"et ses fichiers temporaires\n", p.Spec.User.Name)
			}

			// 6. Le projet quitte l'inventaire actif, et ses numéros de pool et de
			// port reviennent à la plage — seulement ceux dont le fichier est parti.
			app.releaseAllocations(provs, p, warn)
			if purge {
				if err := os.Remove(app.Paths.ProjectFile(p.ID())); err != nil && !os.IsNotExist(err) {
					return wrap(CodeError, err)
				}
				if err := app.Store.DeleteProject(p.ID()); err != nil {
					warn("état : %v", err)
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
					warn("état : %v", err)
				}
			}

			// 7. Les fichiers de portée parc énumèrent les projets, et celui-ci n'en
			// fait plus partie. Le recalcul n'est pas cosmétique : une prison fail2ban
			// dont le journal a disparu **empêche le démon de démarrer** — toutes les
			// autres prisons tombent avec elle —, et le vhost de supervision
			// proxifierait un socket FPM qui n'existe plus.
			signalInterruption()
			if err := app.refreshGlobal(cmd.Context(), provs); err != nil {
				warn("mise à jour des fichiers de parc : %v", err)
			}
			// Les clés DKIM partent une fois les tables d'OpenDKIM rechargées sans
			// elles : retirées avant, le démon les désignerait encore, et retiendrait
			// en file le courrier qui s'en réclame. Elles restent scellées et suivent
			// le projet dans l'archive.
			dkimKept := false
			if len(dkimRecords) > 0 {
				if err := provs.Mail.RemoveProject(p.ID()); err != nil {
					dkimKept = true
					warn("clés DKIM : %v", err)
				} else {
					removed = append(removed, "clés DKIM "+p.ID())
				}
			}
			// Les secrets qui ne prouvent plus rien quittent le keystore (D100) ;
			// l'archive en garde la copie. Sous --keep-data, ils restent avec ce
			// qu'ils servent.
			if !keepData {
				app.purgeSecrets(p, dkimKept, &removed, warn)
			} else if names, err := app.Store.SecretNames(p.ID()); err == nil && len(names) > 0 {
				app.printf("= --keep-data : %d secret(s) conservé(s) au keystore, avec ce qu'ils servent ; "+
					"un projet recréé sous l'identifiant %s les reprend\n", len(names), p.ID())
			}

			_, _ = app.runHook(cmd.Context(), provs.Hooks, p, hook.PostDelete,
				hook.Context{Resources: removed, Extra: map[string]string{"archive": archived}})
			signalInterruption()
			status, mark := "ok", "✓"
			if warnings > 0 {
				status, mark = "avertissement", "⚠"
			}
			_ = app.Audit.Record(p.ID(), "project", "project.delete", status, archived)
			app.printf("\n%s %s supprimé (%d ressource(s))\n", mark, p.ID(), len(removed))
			if archived != "" {
				app.printf("  archive : %s\n", archived)
				app.printf("  restaurer : ogs project restore %s\n", archived)
			} else {
				app.printf("  aucune archive : la suppression était définitive.\n")
			}
			if warnings > 0 {
				return exitf(CodePartial, "terminé avec %d avertissement(s) : le projet a été retiré, "+
					"mais tout n'a pas pu être démonté", warnings)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&purge, "purge", false, "supprimer sans archiver (irréversible)")
	f.BoolVar(&keepData, "keep-data", false, "conserver bases, files et contenu")
	f.BoolVar(&noDump, "no-dump", false, "ne pas sauvegarder les bases dans l'archive (elles sont supprimées quand même)")
	return cmd
}

// refreshGlobal recalcule et applique les fichiers de portée parc après le
// retrait d'un projet. Le plan est recalculé depuis les fichiers restants : le
// projet supprimé a déjà quitté /etc/ogs/projects.
func (a *App) refreshGlobal(ctx context.Context, provs *providers) error {
	plan, err := a.buildGlobalPlan(ctx, provs, nil)
	if err != nil {
		return err
	}
	if plan.Empty() {
		return nil
	}
	_, err = engine.Execute(ctx, plan, engine.Options{
		OnStep: func(s *engine.Step, stepErr error) {
			if stepErr == nil {
				a.printf("  ✓ %s\n", s.Summary)
			}
		},
		OnWarning: func(s *engine.Step, stepErr error) {
			a.warnf("  ⚠ %s : %v\n", s.Summary, stepErr)
		},
		Reload:          a.reloadFunc(provs),
		OnReloadWarning: a.reloadWarning,
	})
	return err
}

// generatedFiles énumère les chemins où ogs écrit les fichiers d'un projet, dans
// l'ordre inverse de leur création. Ce ne sont que des chemins, calculés depuis
// l'identifiant : ce qui s'y trouve n'est pas forcément d'ogs, et c'est
// projectFiles qui en décide.
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
	// L'unité d'une application supervisée : oubliée, elle survivait au projet,
	// invisible de « systemctl list-units » puisqu'aucune instance ne la charge.
	if appprov.Applies(p) {
		files = append(files, provs.App.UnitPath(p))
	}
	// Et toute unité que l'en-tête attribue au projet (D100) : le modèle d'un
	// service retiré, l'unité d'une application quittée pour un type PHP, que la
	// déclaration ne nomme plus.
	if stale, err := provs.Unit.StaleFiles(p.ID()); err == nil {
		for _, path := range stale {
			if !slices.Contains(files, path) {
				files = append(files, path)
			}
		}
	}
	// Les noms réels des fichiers : celui d'un vhost adopté ne porte pas toujours
	// l'extension du moteur (« shop.fr » sur nginx), et le composer depuis le nom
	// du vhost laissait le fichier et son lien derrière le projet.
	for _, name := range provs.Web.VHostFiles(p) {
		files = append(files,
			filepath.Join(provs.Engine.EnabledDir(), name),
			filepath.Join(provs.Engine.AvailableDir(), name))
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

// keptFile est un fichier que la suppression laisse en place, et pourquoi.
// foreign dit qu'ogs ne sait pas ce qu'il configure : sans en-tête, illisible, ou
// lien vers un fichier qui n'est pas du projet. Celui qu'ogs a écrit pour un
// autre projet ne l'est pas — il désigne le compte et la racine de ce projet-là,
// que le parc interdit de faire recouvrir les nôtres —, ni le lien vers un
// fichier conservé, dont c'est la cible qui décide.
type keptFile struct {
	path, reason string
	foreign      bool
}

// keptPath dit si un chemin fait partie des fichiers conservés.
func keptPath(kept []keptFile, path string) bool {
	for _, k := range kept {
		if k.path == path {
			return true
		}
	}
	return false
}

// projectFiles partage les chemins de generatedFiles entre ce que la suppression
// retire et ce qu'elle laisse. Un fichier ne part que si son en-tête le déclare
// écrit par ogs pour ce projet (fs.ManagedProject) : sans en-tête, c'est celui de
// l'exploitant — le pool ou le vhost d'un site adopté qu'aucun apply n'a encore
// réécrit —, et avec l'en-tête d'un autre projet, celui d'un voisin dont le nom
// prolonge l'identifiant. Un lien n'a pas d'en-tête : il suit le sort de sa
// cible, qu'il désigne un fichier retiré — ou déjà disparu — parmi ceux du
// projet ; tout autre lien est laissé, sa cible n'étant pas du projet.
func (a *App) projectFiles(provs *providers, p *model.Project) (remove []string, kept []keptFile) {
	candidates := a.generatedFiles(provs, p)
	known := map[string]bool{}
	for _, path := range candidates {
		known[filepath.Clean(path)] = true
	}
	removing := map[string]bool{}
	var links []string
	for _, path := range candidates {
		info, err := os.Lstat(path)
		switch {
		case os.IsNotExist(err):
			continue
		case err != nil:
			kept = append(kept, keptFile{path, err.Error(), true})
			continue
		case info.Mode()&os.ModeSymlink != 0:
			links = append(links, path)
			continue
		}
		owner, err := fs.ManagedProject(path)
		switch {
		case err != nil:
			kept = append(kept, keptFile{path, err.Error(), true})
		case owner == p.ID():
			removing[path] = true
		case owner == "":
			kept = append(kept, keptFile{path, "ogs ne l'a pas écrit — il configure peut-être encore " +
				"autre chose que ce projet ; le retirer à la main s'il n'a plus d'objet", true})
		default:
			kept = append(kept, keptFile{path, fmt.Sprintf("ogs l'a écrit pour le projet %s, non pour %s",
				owner, p.ID()), false})
		}
	}
	for _, path := range links {
		target, err := os.Readlink(path)
		if err != nil {
			kept = append(kept, keptFile{path, err.Error(), true})
			continue
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		target = filepath.Clean(target)
		_, statErr := os.Lstat(target)
		switch {
		case removing[target], known[target] && os.IsNotExist(statErr):
			removing[path] = true
		case known[target]:
			kept = append(kept, keptFile{path, "lien vers " + target + ", conservé avec lui", false})
		default:
			kept = append(kept, keptFile{path, "lien vers " + target + ", qui n'est pas un fichier du projet", true})
		}
	}
	for _, path := range candidates {
		if removing[path] {
			remove = append(remove, path)
		}
	}
	return remove, kept
}

// dropResources supprime les bases et le vhost AMQP. Ce qui résiste passe par
// warn, comme partout dans la suppression : une base injoignable reste en place,
// et le code 6 doit le dire.
func (a *App) dropResources(cmd *cobra.Command, provs *providers, p *model.Project, removed *[]string,
	warn func(format string, args ...any)) {
	// La garde d'entrée (forcedRemovals) l'a déjà refusé ; elle est
	// refaite ici pour que le seul appel qui supprime n'en dépende pas.
	dbs := len(p.Spec.Databases) > 0
	if dbs && adopted(p) && !a.Force {
		warn("bases conservées : %s est adopté, et les supprimer exige --force", p.ID())
	} else {
		obs, err := provs.DB.Observe(cmd.Context(), p)
		if err != nil {
			warn("bases : %v", err)
		}
		for _, state := range obs.States {
			if state.Unavailable != nil {
				warn("base %s conservée : %v", state.Declaration.Name, state.Unavailable)
				continue
			}
			// Seul part ce qui est à ogs (D91) : un homonyme qu'il n'a pas créé
			// reste, et le code 6 le dit.
			kept, err := provs.DB.Drop(cmd.Context(), state.Declaration, state)
			if len(kept) > 0 {
				warn("base %s %s : conservé, faute d'avoir été créé par ogs — %s", state.Declaration.Engine,
					state.Declaration.Name, strings.Join(kept, ", "))
			}
			if err != nil {
				warn("suppression de la base %s : %v", state.Declaration.Name, err)
				continue
			}
			if !slices.Contains(kept, "base "+state.Declaration.Name) {
				*removed = append(*removed, fmt.Sprintf("base %s %s",
					state.Declaration.Engine, state.Declaration.Name))
			}
		}
		// Puis les comptes que la déclaration ne nomme plus (D100) : un compte
		// additionnel retiré, l'ancien compte d'une base renommée. Seuls partent
		// ceux dont le mot de passe est scellé dans la portée du projet, avec lui ;
		// ce qui reste garde son secret, qui en prouve l'appartenance.
		for _, r := range provs.DB.DropOrphans(cmd.Context(), p) {
			o := r.Orphan
			switch {
			case r.Dropped && r.Kept == nil:
				*removed = append(*removed, fmt.Sprintf("compte %s %s", o.Engine, o.User))
			case r.Unreachable:
				a.printf("= compte %s %s : moteur injoignable, et plus aucune base %s déclarée — son mot de "+
					"passe reste scellé (%v)\n", o.Engine, o.User, o.Engine, r.Kept)
			case r.Dropped:
				warn("compte %s %s supprimé, mais son mot de passe est resté au keystore : %v", o.Engine, o.User, r.Kept)
			case r.Kept != nil:
				warn("compte %s %s conservé, avec son mot de passe scellé : %v", o.Engine, o.User, r.Kept)
			}
		}
	}
	// Ce qu'ogs n'a pas créé reste en place (D93) : un vhost homonyme de
	// l'administrateur partait avec ses files et leurs messages. Vhost et compte
	// se jugent chacun sur sa propre preuve — le mot de passe du compte ne dit
	// rien du vhost, que spec.amqp.vhost a pu faire désigner après coup. Ceux
	// qu'une déclaration changée ou retirée a laissés derrière elle partent aussi
	// (D100) : leurs preuves restaient au keystore, et un projet recréé sous le
	// même identifiant les aurait repris. Broker injoignable, rien n'est tenté :
	// la suppression n'aurait pas mieux réussi, et les preuves restent.
	report := provs.AMQP.DropProject(cmd.Context(), p)
	for _, w := range report.Warnings {
		warn("%s", w)
	}
	*removed = append(*removed, report.Removed...)
}

// adopted dit si le projet a été repris d'un serveur existant (« ogs import ») :
// ses ressources existaient avant ogs, qui n'en a pas la propriété.
func adopted(p *model.Project) bool { return p.Metadata.Labels["adopted"] == "true" }

// keepDataHint est l'issue que laisse tout refus de toucher au compte, aux bases
// ou à l'arborescence.
const keepDataHint = "relancer avec --keep-data, qui retire le projet sans toucher ni au compte, ni aux " +
	"bases, ni à l'arborescence"

// refusal est un refus d'effacer le compte, les bases ou l'arborescence, en deux
// pièces : le constat, qui nomme ce qui est en cause, et le geste qui le lève —
// vide quand seul --keep-data le lève. Sous --keep-data, qui laisse l'arborescence
// en place, le constat seul dit pourquoi l'archive ne la recopie pas ; au moment
// d'effacer, quand plus rien ne se relance, il dit ce qui est resté.
type refusal struct {
	code   int
	what   string
	remedy string
}

// refusals est l'ensemble des refus d'une suppression. Ils sont toujours
// rassemblés, jamais rendus au premier : chacun appelle un geste différent, et
// certains coûtent — retirer le vhost d'un site adopté, c'est couper le site.
// L'exploitant qui suivait le remède du premier refus apprenait au passage
// suivant que le compte ou les bases exigeaient encore --force, puis, au
// troisième, qu'une base était injoignable.
type refusals []*refusal

// err assemble les refus en un seul message, ou rend nil s'il n'y en a aucun.
// Le code est celui du premier : l'ordre va de ce que rien ne lève à ce que lève
// un drapeau (deletionRefusals), et c'est le plus grave qui décide.
func (rs refusals) err(id string) error {
	switch len(rs) {
	case 0:
		return nil
	case 1:
		if rs[0].remedy == "" {
			return exitf(rs[0].code, "%s — suppression refusée ; %s", rs[0].what, keepDataHint)
		}
		return exitf(rs[0].code, "%s — suppression refusée ; %s, ou %s", rs[0].what, rs[0].remedy, keepDataHint)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "suppression de %s refusée, pour %d raisons, chacune avec ce qui la lève :", id, len(rs))
	for _, r := range rs {
		b.WriteString("\n  • " + r.what)
		if r.remedy != "" {
			b.WriteString(" — " + r.remedy)
		}
	}
	b.WriteString("\n  ou, pour toutes à la fois, " + keepDataHint)
	return exitf(rs[0].code, "%s", b.String())
}

// constat rend les seuls constats, sans remède.
func (rs refusals) constat() string {
	whats := make([]string, len(rs))
	for i, r := range rs {
		whats[i] = r.what
	}
	return strings.Join(whats, " ; ")
}

// deletionRefusals rassemble les refus qu'une suppression doit prononcer tant
// qu'elle est encore refusable. Il est appelé à l'entrée, avant la confirmation
// — où s'y ajoute la base qu'on ne pourra pas sauvegarder (databasesToDump) —,
// puis au point de non-retour, après l'archive. Viennent d'abord ceux que rien ne
// lève — la racine, les montages, les fichiers laissés en place —, parce que ce
// qu'ils protègent n'appartient pas au projet ; puis, en un seul refus, tout ce
// que --force lève. Sous --keep-data, ni le compte, ni les bases, ni
// l'arborescence ne sont touchés : seule l'archive s'en garde (archiveExclusions).
func (a *App) deletionRefusals(provs *providers, p *model.Project, o deleteOptions) refusals {
	if o.keepData {
		return nil
	}
	// Les journaux partent avec le projet (D71), par le même effacement récursif :
	// leurs points de montage sont cherchés avec ceux de la racine, et nommés dans
	// le même refus — un seul passage d'umount.
	rs := a.rootRefusals(p, p.Spec.Layout.LogDir)
	if r := a.keptFilesRefusal(provs, p); r != nil {
		rs = append(rs, r)
	}
	forced, r := a.forcedRemovals(p)
	switch {
	case r != nil:
		rs = append(rs, r)
	case len(forced) > 0 && !a.Force:
		rs = append(rs, &refusal{code: CodeConflict,
			what: fmt.Sprintf("projet %s : la suppression effacerait ce qu'ogs n'a pas créé — %s", p.ID(),
				strings.Join(forced, " ; ")),
			remedy: "relancer avec --force, qui efface aussi ce qu'ogs n'a pas créé"})
	}
	return rs
}

// forcedRemovals énumère ce que la suppression n'efface que sous --force, parce
// qu'ogs ne l'a pas créé (§7.1.6) : le compte système qui ne porte pas sa marque —
// celui d'un projet adopté ne la porte jamais —, et les bases d'un projet adopté,
// avec leurs comptes. Elles existaient avant ogs, et l'import les rattache sur des
// indices qu'il ne peut pas toujours vérifier : la base d'un site voisin resté hors
// d'ogs, un compte dont personne ne connaît plus le mot de passe. Un compte
// illisible est un refus : rien ne dit alors qu'ogs l'a créé.
//
// Tout est nommé d'un coup. Éprouvés l'un après l'autre, le refus du compte
// masquait celui des bases, et le --force qu'il réclamait les emportait sans
// qu'elles aient jamais été nommées. removeUser et dropResources portent encore
// chacun le leur : le seul appel qui supprime ne dépend pas de qui l'appelle.
func (a *App) forcedRemovals(p *model.Project) ([]string, *refusal) {
	u, err := a.systemProvider().Identity.LookupUser(p.Spec.User.Name)
	if err != nil {
		return nil, &refusal{code: CodeError, what: fmt.Sprintf("compte %s illisible (%v) : impossible de "+
			"vérifier qu'ogs l'a créé", p.Spec.User.Name, err)}
	}
	var out []string
	if u != nil && !u.Managed {
		out = append(out, fmt.Sprintf("le compte %s, qui n'a pas été créé par ogs, avec son arborescence %s",
			p.Spec.User.Name, p.Spec.Layout.Root))
	}
	if adopted(p) {
		for _, d := range p.Spec.Databases {
			out = append(out, fmt.Sprintf("la base %s, qui existait avant ogs", databaseLabel(d, a.reservedDBAccounts())))
		}
		// Les comptes que la déclaration ne nomme plus et dont le mot de passe est
		// scellé — par une rotation, par exemple : --force les emporte aussi (D100).
		if orphans := a.undeclaredDBAccounts(p); len(orphans) > 0 {
			out = append(out, "les comptes SGBD que la déclaration ne nomme plus : "+strings.Join(orphans, ", "))
		}
	}
	return out, nil
}

// undeclaredDBAccounts nomme les comptes SGBD scellés pour le projet qu'aucune
// de ses déclarations ne nomme.
func (a *App) undeclaredDBAccounts(p *model.Project) []string {
	if a.Store == nil {
		return nil
	}
	names, err := a.Store.SecretNames(p.ID())
	if err != nil {
		return nil
	}
	declared := map[string]bool{}
	for _, d := range p.Spec.Databases {
		for _, u := range append([]string{d.User}, extraUserNames(d)...) {
			declared[u] = true
		}
	}
	var out []string
	for _, n := range names {
		for _, prefix := range []string{"mysql/", "mariadb/", "postgres/"} {
			if user, ok := strings.CutPrefix(n, prefix); ok && !declared[user] {
				out = append(out, strings.TrimSuffix(prefix, "/")+" "+user)
			}
		}
	}
	return out
}

// databaseLabel nomme une base et les comptes que sa suppression emporte — et
// ceux-là seuls : un compte réservé ne part jamais (D91), et la confirmation qui
// l'annonçait promettait une suppression que Drop ne fait pas. Il n'est appelé
// que pour un projet adopté, dont Drop retire tout le reste de ce qu'il déclare.
func databaseLabel(d model.Database, reserved []string) string {
	host := d.Host
	if host == "" {
		host = "localhost"
	}
	var removed, kept []string
	for _, user := range append([]string{d.User}, extraUserNames(d)...) {
		account := user
		if d.Engine != "postgres" {
			account += "@" + host
		}
		if validate.ReservedDBAccount(d.Engine, user, reserved...) != "" {
			kept = append(kept, account)
			continue
		}
		removed = append(removed, account)
	}
	label := fmt.Sprintf("%s %s", d.Engine, d.Name)
	if len(removed) > 0 {
		label += " (compte " + strings.Join(removed, ", ") + ")"
	}
	if len(kept) > 0 {
		label += ", sans " + strings.Join(kept, ", ") + ", réservé et conservé"
	}
	return label
}

func extraUserNames(d model.Database) []string {
	out := make([]string, 0, len(d.ExtraUsers))
	for _, u := range d.ExtraUsers {
		out = append(out, u.User)
	}
	return out
}

// keptFilesRefusal refuse d'effacer le compte et l'arborescence tant que la
// suppression laisse en place un fichier dont ogs ne sait pas ce qu'il configure.
// Le vhost d'origine d'un projet adopté, qu'aucun apply n'a réécrit, journalise
// souvent sous la racine, et son pool tourne sous le compte : les effacer quand
// même rendait la configuration du serveur invalide (« Cannot access directory »),
// plus aucun rechargement ne passait, et le redémarrage suivant faisait tomber
// tous les sites. ogs ne retire pas ces fichiers à la place de l'exploitant — ils
// servent peut-être autre chose, un voisin resté hors d'ogs en tête —, et --force
// n'y change rien, pour la même raison. Le remède qui ne coupe rien vient en
// premier : les confier à ogs, qui les réécrit au nom du projet ; les retirer à la
// main coupe le site sur-le-champ.
func (a *App) keptFilesRefusal(provs *providers, p *model.Project) *refusal {
	_, kept := a.projectFiles(provs, p)
	var foreign []string
	for _, k := range kept {
		if k.foreign {
			foreign = append(foreign, k.path)
		}
	}
	if len(foreign) == 0 {
		return nil
	}
	return &refusal{code: CodeConflict,
		what: fmt.Sprintf("projet %s : fichier(s) qu'ogs n'a pas écrit(s) pour lui, qu'il laisserait donc "+
			"en place (%s), et qui désignent peut-être encore son compte ou son arborescence : un vhost qui "+
			"journalise sous une racine effacée, un pool qui tourne sous un compte disparu rendent la "+
			"configuration du serveur invalide", p.ID(), strings.Join(foreign, ", ")),
		remedy: fmt.Sprintf("s'ils ne servent que ce projet, les confier à ogs (ogs apply %s --force, qui les "+
			"réécrit à son nom et en sauvegarde l'original dans %s) ; sinon, les retirer ou les désactiver "+
			"d'abord, s'ils n'ont plus d'objet", p.ID(), a.Paths.BackupsDir())}
}

// restartServices relance ce que l'étape 1 a arrêté, quand la suppression renonce
// avant le point de non-retour. Le contexte est détaché : une interruption
// (Ctrl+C) est précisément l'un des abandons à réparer, et un contexte annulé ne
// relancerait rien.
func (a *App) restartServices(ctx context.Context, provs *providers, p *model.Project) {
	a.printf("redémarrage des services de %s : rien n'a été supprimé\n", p.ID())
	if err := provs.Unit.Control(context.WithoutCancel(ctx), p, "start"); err != nil {
		a.warnf("⚠ redémarrage des services : %v — les relancer : systemctl start %s\n",
			err, unit.TargetName(p.ID()))
	}
}

// interrupted rend l'abandon qu'impose une interruption (Ctrl+C, SIGTERM) reçue
// avant le point de non-retour, ou nil. main n'arrête pas le processus : il
// annule le contexte, et c'est à la suppression de le consulter entre deux étapes.
func interrupted(ctx context.Context) error {
	if ctx.Err() == nil {
		return nil
	}
	return exitf(CodeCancelled, "interruption reçue : suppression abandonnée avant tout retrait")
}

// databasesToDump rend les bases à sauvegarder dans l'archive. Quand les bases
// vont être supprimées, chacune doit pouvoir être sauvegardée : une base
// injoignable ne l'est pas, et la suppression continuait vers une archive sans
// ses données. Le refus tombe ici, avant que rien ne s'arrête, nommé avec les
// autres ; l'échec du dump lui-même, qui ne se sait qu'en le tentant, abandonne
// dans archiveProject. Une base que le moteur ne connaît pas n'a rien à
// sauvegarder.
func (a *App) databasesToDump(cmd *cobra.Command, provs *providers, p *model.Project,
	o deleteOptions) ([]model.Database, *refusal) {
	if len(p.Spec.Databases) == 0 {
		return nil, nil
	}
	if !o.dumpsRequired() {
		// Sous --keep-data, la sauvegarde n'est qu'un complément de l'archive :
		// chaque base est tentée, et un échec ne coûte qu'un avertissement.
		return p.Spec.Databases, nil
	}
	const remedy = "relancer avec --no-dump pour supprimer le projet sans sauvegarde de ses bases"
	obs, err := provs.DB.Observe(cmd.Context(), p)
	if err != nil {
		return nil, &refusal{code: CodeDependency, what: fmt.Sprintf("bases de %s inobservables (%v) : elles "+
			"ne peuvent être ni sauvegardées ni supprimées", p.ID(), err), remedy: remedy}
	}
	var (
		dumps       []model.Database
		unavailable []string
	)
	for _, state := range obs.States {
		decl := state.Declaration
		switch {
		case state.Unavailable != nil:
			unavailable = append(unavailable, fmt.Sprintf("%s %s (%v)", decl.Engine, decl.Name, state.Unavailable))
		case !state.DBExists:
			a.printf("= base %s absente du moteur : rien à sauvegarder\n", decl.Name)
		default:
			dumps = append(dumps, decl)
		}
	}
	if len(unavailable) > 0 {
		return nil, &refusal{code: CodeDependency, what: fmt.Sprintf("base(s) inobservable(s), qui ne peuvent "+
			"être ni sauvegardées ni supprimées : %s", strings.Join(unavailable, ", ")), remedy: remedy}
	}
	return dumps, nil
}

// noDumpHint conclut tout refus lié aux sauvegardes : les deux issues, et ce
// que chacune coûte.
const noDumpHint = "suppression abandonnée avant tout retrait. Relancer avec --no-dump pour supprimer " +
	"le projet sans sauvegarde de ses bases, ou avec --keep-data pour le retirer en les conservant"

// archiveScope dit ce que l'archive d'une suppression recueille. files sont les
// fichiers générés que la suppression retire (projectFiles), dumps les bases à
// sauvegarder (databasesToDump). strict dit que les bases vont être supprimées :
// une sauvegarde en échec abandonne alors l'archive et la suppression. skipTree et
// skipLogs écartent, sous --keep-data, ce qui n'est pas au seul projet
// (archiveExclusions).
type archiveScope struct {
	files []string
	// certs nomme les certificats que la suppression retire, scellés dans
	// l'archive (D100).
	certs []string
	// jobs est ce que le compte a posé hors d'ogs (D107), recopié en clair sous
	// user/ : rien n'y est scellé, comme l'arborescence.
	jobs               system.UserJobs
	dumps              []model.Database
	strict             bool
	skipTree, skipLogs bool
}

// archiveExclusions décide, sous --keep-data, de ce que l'archive ne recopie pas.
// Rien n'est effacé, mais l'archive recopiait tout ce qui se trouvait sous la
// racine : le home d'un voisin que déclarait un clone raté, rangé sous le nom du
// clone, un partage de médias monté dessous, /var/www tout entier pour une racine
// adoptée trop large. L'arborescence n'est archivée que si la suppression aurait
// pu l'effacer — les mêmes remparts en décident — ; sinon, elle reste en place, et
// l'exploitant apprend pourquoi l'archive ne la porte pas. Refuser le geste, comme
// sans --keep-data, n'aurait rien protégé de plus, et aurait fermé la seule issue
// à qui retire un site en laissant le partage monté pour les autres.
func (a *App) archiveExclusions(p *model.Project) (skipTree, skipLogs bool) {
	if rs := a.rootRefusals(p); len(rs) > 0 {
		a.printf("= arborescence non archivée, laissée en place : %s\n", rs.constat())
		skipTree = true
	}
	if r := mountRefusal(p, p.Spec.Layout.LogDir); r != nil {
		a.printf("= journaux non archivés, laissés en place : %s\n", r.what)
		skipLogs = true
	}
	return skipTree, skipLogs
}

// checkArchiveSize vérifie l'espace disponible et demande confirmation au-delà du
// seuil (§13, garde-fous), avant que rien ne soit arrêté : un refus ici laissait
// jusque-là les services du projet à l'arrêt, sans rien pour les relancer. Une
// arborescence que l'archive ne recopie pas ne compte pas : un partage monté
// dessous faisait refuser l'archive « faute de place ».
func (a *App) checkArchiveSize(p *model.Project, scope archiveScope) error {
	var size int64
	if !scope.skipTree {
		var err error
		if size, err = treeSize(p.Spec.Layout.Root); err != nil {
			return wrap(CodeError, err)
		}
	}
	free, err := archive.Available(a.Paths.ArchivesDir())
	if err == nil && uint64(size) > free {
		return exitf(CodeError,
			"espace insuffisant : %s à archiver, %s disponible sous %s",
			humanBytes(size), humanBytes(int64(free)), a.Paths.ArchivesDir())
	}
	if size > ArchiveSizeThreshold {
		if err := a.confirm(fmt.Sprintf(
			"l'archive contiendra environ %s : continuer ?", humanBytes(size))); err != nil {
			return err
		}
	}
	return nil
}

// removeUser supprime le compte système et l'arborescence.
func (a *App) removeUser(cmd *cobra.Command, provs *providers, p *model.Project, jobs system.UserJobs,
	removed *[]string, warn func(format string, args ...any)) error {
	u, err := provs.System.Identity.LookupUser(p.Spec.User.Name)
	if err != nil {
		return err
	}
	if u != nil && !u.Managed && !a.Force {
		return fmt.Errorf("l'utilisateur %s n'a pas été créé par ogs : le supprimer exige --force",
			p.Spec.User.Name)
	}
	// Le groupe est observé avant userdel, qui l'emporte parfois : c'est le
	// compte encore présent qui dit s'il est le sien (D107).
	group, groupErr := provs.System.Identity.LookupGroup(p.Spec.User.Group)
	// Le rempart de l'arborescence est éprouvé avant userdel, et non après : il
	// compare le propriétaire de la racine au compte du projet, qui n'existera
	// plus ensuite — un home orphelin et celui d'un voisin ne se distingueraient
	// alors plus.
	treeErr := a.checkRemovableRoot(p)
	// L'échec du userdel n'interrompt plus la suppression : l'arborescence part
	// quand même. Laisser le home d'un projet dont la suppression vient d'être
	// annoncée — définitive, qui plus est — serait le pire des deux états.
	var accountErr error
	if u != nil {
		// Ce que le compte a posé hors d'ogs s'exécute sous son nom, et cron comme
		// atd le relisent par le nom : un compte recréé sous ce nom le reprenait
		// (D107, R4-18). L'archive en a gardé la copie ; ce qui n'a pas pu être
		// relu n'y est pas, et reste.
		list, err := provs.System.RemoveUserJobs(cmd.Context(), jobs, u.UID)
		*removed = append(*removed, list...)
		for _, r := range list {
			a.printf("✓ retiré : %s\n", r)
		}
		if err != nil {
			warn("%v", err)
		}
		// useradd redonne l'UID libéré au compte suivant, qui recevait ce que
		// l'ancien laissait dans les répertoires temporaires communs (D107, R4-19).
		purged, err := provs.System.PurgeUID(cmd.Context(), u.UID)
		if len(purged) > 0 {
			a.printf("✓ fichiers de l'UID %d retirés : %s\n", u.UID, strings.Join(purged, ", "))
			*removed = append(*removed, fmt.Sprintf("fichiers temporaires de l'UID %d", u.UID))
		}
		if err != nil {
			warn("fichiers de l'UID %d laissés en place, que le prochain compte de cet UID recevra : %v",
				u.UID, err)
		}
		if _, err := a.Exec.Run(cmd.Context(), exec.Command{
			Name: "userdel", Args: []string{p.Spec.User.Name},
		}); err != nil {
			accountErr = err
			// userdel ne dit pas ce qui tient le compte, et « currently used by
			// process 3418 » sans le nom du processus n'apprend rien : la liste est
			// lue directement dans /proc, d'où la commande à passer pour finir.
			if procs := processesOfUID(u.UID); len(procs) > 0 {
				accountErr = fmt.Errorf("%w\n  processus encore actifs sous %s : %s\n"+
					"  les arrêter puis : userdel %s && groupdel %s",
					err, p.Spec.User.Name, strings.Join(procs, ", "),
					p.Spec.User.Name, p.Spec.User.Group)
			}
		} else {
			a.removeGroup(cmd, provs, p, u, group, groupErr, removed, warn)
		}
	} else if group != nil {
		// Sans compte, rien ne dit que le groupe homonyme est celui du projet :
		// il reste, nommé, et un projet recréé sous ce nom le verrait (D107).
		warn("groupe %s conservé : le compte %s n'existe plus, rien ne dit que ce groupe est le sien ; "+
			"s'il l'est : groupdel %s", p.Spec.User.Group, p.Spec.User.Name, p.Spec.User.Group)
	}
	// L'arborescence part en dernier : tout ce qui précède a pu avoir besoin de la lire.
	if treeErr == nil {
		treeErr = os.RemoveAll(p.Spec.Layout.Root)
	}
	// Les journaux vivent sous logs.root (D71), que la validation impose : ils
	// partent avec le projet, après l'archive qui les a recueillis. Le contrôle est
	// refait ici, la suppression étant récursive — et os.RemoveAll ne s'arrête pas
	// plus à un point de montage sous les journaux que sous la racine. Comme pour
	// l'arborescence, seul le constat est rendu : la déclaration part aux archives
	// juste après, et aucun remède ne se relance plus.
	var logsErr error
	if dir := filepath.Clean(p.Spec.Layout.LogDir); strings.HasPrefix(dir, a.logsRoot()+string(filepath.Separator)) {
		if r := mountRefusal(p, dir); r != nil {
			logsErr = fmt.Errorf("%s — journaux laissés en place", r.what)
		} else {
			logsErr = os.RemoveAll(dir)
		}
	}
	// Le répertoire d'exécution est dérivé du préfixe et de l'identifiant, non du
	// fichier de projet : il part quoi qu'il arrive à l'arborescence.
	if err := os.RemoveAll(a.Paths.ProjectRunDir(p.ID())); err != nil {
		return errors.Join(accountErr, treeErr, logsErr, err)
	}
	return errors.Join(accountErr, treeErr, logsErr)
}

// removeGroup retire le groupe du projet après son compte, s'il est à ogs et à
// personne d'autre (D107). groupdel partait sans condition : le groupe homonyme
// d'un compte adopté, que d'autres comptes partageaient, disparaissait sous eux
// (R1-25), et son échec n'était qu'affiché — « ✓ supprimé », code 0 (R1-18).
//
// Le groupe est à ogs s'il était le groupe primaire du compte qu'ogs vient de
// supprimer : il l'a créé avec lui (§7.1.1), ou, pour un compte adopté que
// --force a supprimé, c'est son groupe privé, que userdel emporte de lui-même
// quand il est seul. Il est à quelqu'un d'autre s'il a un membre autre que
// www-data, ou s'il est le groupe primaire d'un autre compte : il reste alors,
// nommé, code 6 — un projet recréé sous ce nom se le verrait refuser (D107).
// Le groupe observé l'a été avant userdel ; il est relu après, userdel l'ayant
// peut-être emporté.
func (a *App) removeGroup(cmd *cobra.Command, provs *providers, p *model.Project, u *system.UserInfo,
	g *system.GroupInfo, lookupErr error, removed *[]string, warn func(format string, args ...any)) {
	name := p.Spec.User.Group
	switch {
	case lookupErr != nil:
		warn("groupe %s illisible, laissé en place : %v", name, lookupErr)
		return
	case g == nil:
		return
	case g.GID != u.GID:
		warn("groupe %s conservé : ce n'est pas le groupe primaire du compte %s, ogs ne l'a pas créé avec lui",
			name, u.Name)
		return
	}
	strangers, err := provs.System.GroupStrangers(g, u.Name)
	if err != nil {
		warn("groupe %s laissé en place, ses membres sont illisibles : %v", name, err)
		return
	}
	if len(strangers) > 0 {
		warn("groupe %s conservé : d'autres comptes le tiennent — %s ; une fois qu'ils n'en ont plus "+
			"besoin : gpasswd -d <compte> %s, puis groupdel %s", name, strings.Join(strangers, ", "), name, name)
		return
	}
	after, err := provs.System.Identity.LookupGroup(name)
	if err != nil {
		warn("groupe %s illisible, laissé en place : %v", name, err)
		return
	}
	if after == nil {
		// userdel l'a emporté : il était seul.
		*removed = append(*removed, "groupe "+name)
		return
	}
	// Le serveur web est membre du groupe quand webAccess vaut « group » : sans
	// l'en retirer, groupdel refuse — « group has other members ».
	if slices.Contains(after.Members, system.WebUser) {
		if _, err := a.Exec.Run(cmd.Context(), exec.Command{
			Name: "gpasswd", Args: []string{"-d", system.WebUser, name},
		}); err != nil {
			warn("groupe %s laissé en place, %s n'a pas pu en être retiré : %v", name, system.WebUser, err)
			return
		}
	}
	// Le code 6 de groupdel dit que le groupe n'existe plus : un autre l'a retiré
	// entre-temps, c'est le résultat voulu.
	const groupdelNoSuchGroup = 6
	if res, err := a.Exec.Run(cmd.Context(), exec.Command{
		Name: "groupdel", Args: []string{name},
	}); err != nil && res.ExitCode != groupdelNoSuchGroup {
		warn("suppression du groupe %s : %v", name, err)
		return
	}
	*removed = append(*removed, "groupe "+name)
}

// checkRemovableRoot est le rempart devant un geste sans retour. La racine vient
// du fichier YAML du projet : la validation la contrôle désormais à l'entrée,
// mais une déclaration écrite avant ce durcissement — ou à la main — reste sur le
// disque, et os.RemoveAll ne demande rien à personne. Le contrôle est donc refait
// ici, en nommant le chemin : l'exploitant corrige layout.root puis recommence.
//
// Ses refus (rootRefusals) sont prononcés à l'entrée de « project delete », tant
// que le geste est encore refusable, et de nouveau au point de non-retour,
// l'archive faite (deletionRefusals). checkRemovableRoot les rend une dernière
// fois juste avant os.RemoveAll, où il garde le seul appel qui efface — removeUser
// ne doit pas dépendre de qui l'appelle. Là, seul le constat est rendu : la
// déclaration part aux archives juste après, et un « corriger puis relancer »
// n'aurait plus d'objet.
//
// Un projet adopté échappe à la racine des homes, et c'est voulu : l'import
// reprend le home réel d'un compte du système (/var/www/boutique, par exemple).
// Pour lui, seuls tiennent les emplacements interdits à tout le monde. La liste
// est celle de la validation, recopiée — le rempart doit valoir même quand l'amont
// n'a rien vu — et élargie aux racines que plusieurs sites se partagent : un
// /etc/passwd dont le home vaut /var/www ferait effacer les voisins.
//
// La forme du chemin ne suffit pas : un clone de préproduction reprenant le home
// de son homologue, deux docroots imbriqués adoptés tels quels, un home de compte
// englobant le site d'un autre, et la suppression effaçait un voisin avec une
// racine d'apparence irréprochable. Sont donc éprouvés le parc (aucun autre
// projet ne déclare la même racine, ni une racine au-dessus ou au-dessous), puis
// ce qui est sur le disque : un vrai répertoire, sans point de montage dessous,
// au propriétaire attendu. Aucun de ces refus ne cède à --force : ce qu'ils
// protègent n'appartient pas au projet.
func (a *App) checkRemovableRoot(p *model.Project) error {
	if rs := a.rootRefusals(p); len(rs) > 0 {
		return fmt.Errorf("%s — arborescence laissée en place", rs.constat())
	}
	return nil
}

// rootRefusals rend, en pièces, les refus d'effacer la racine — aucun si rien ne
// s'y oppose. archiveExclusions en tire le seul constat. alsoMounts sont des
// chemins effacés avec elle, dont les points de montage sont cherchés dans le
// même passage.
//
// Tous sont rassemblés — le voisin qui partage la racine ne masque pas le partage
// monté dessous —, sauf quand la forme même du chemin est refusée : l'exploitant
// en désignera un autre, et le parc ou le disque vus à celui-ci n'apprendraient
// rien — pour « / », tous les projets et tous les montages de la machine.
func (a *App) rootRefusals(p *model.Project, alsoMounts ...string) refusals {
	clean := filepath.Clean(p.Spec.Layout.Root)
	home := filepath.Clean(a.homeRoot())
	var raison string
	switch {
	// filepath.Clean rend « . » pour une racine vide : un chemin relatif
	// effacerait le répertoire de travail de qui a lancé la commande.
	case clean == "/" || !filepath.IsAbs(clean):
		raison = "ni la racine du système ni un chemin relatif ne sont effaçables"
	case systemRoot(clean):
		raison = "emplacement système"
	// L'égalité est refusée au même titre que le débordement : effacer la racine
	// des homes emporterait tous les autres projets du parc (D60).
	case clean == home:
		raison = "c'est la racine des homes elle-même, partagée par tout le parc"
	case !strings.HasPrefix(clean, home+string(filepath.Separator)) && !adopted(p):
		raison = "hors de " + home + ", alors que le projet n'est pas adopté"
	}
	if raison != "" {
		rs := refusals{{code: CodeValidation, what: fmt.Sprintf("racine de projet %q : %s", p.Spec.Layout.Root,
			raison), remedy: "corriger spec.layout.root dans la déclaration du projet"}}
		if r := mountRefusal(p, alsoMounts...); r != nil {
			rs = append(rs, r)
		}
		return rs
	}
	return append(a.fleetRefusals(p, clean), a.diskRefusals(p, clean, alsoMounts...)...)
}

// fleetRefusals refuse une racine qu'un autre projet du parc déclare aussi, ou
// dont il déclare un sous-répertoire ou un ancêtre : l'effacer emporterait tout
// ou partie du voisin, toujours déclaré et servi — un clone de préproduction
// raté qui reprenait le home de son homologue, deux docroots imbriqués adoptés
// tels quels. Chaque voisin est nommé. Chaque déclaration est relue ici, et non
// tirée de loadProjects : une seule déclaration invalide y ferait échouer tout le
// parc, alors qu'il suffit ici d'en connaître la racine. Une déclaration dont on
// ne tire pas de racine fait refuser, en la nommant : dans le doute, rien n'est
// effacé. projects/archived/ n'est pas lu : un projet supprimé puis recréé au
// même home y laisse une racine qui ne protège plus rien.
func (a *App) fleetRefusals(p *model.Project, root string) refusals {
	dir := a.Paths.ProjectsDir()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return refusals{{code: CodeError, what: fmt.Sprintf("racine de projet %q : inventaire du parc "+
			"illisible (%v), impossible de vérifier qu'aucun autre projet ne la partage", root, err)}}
	}
	var rs refusals
	own := filepath.Clean(a.Paths.ProjectFile(p.ID()))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if path == own {
			continue
		}
		other, err := spec.Load(path)
		if err == nil {
			err = other.Normalize(a.homeRoot(), a.Cfg.PrepDomain(), model.WithLogsRoot(a.logsRoot()))
		}
		if err == nil && !filepath.IsAbs(other.Spec.Layout.Root) {
			err = fmt.Errorf("spec.layout.root %q n'est pas un chemin absolu", other.Spec.Layout.Root)
		}
		if err != nil {
			rs = append(rs, &refusal{code: CodeConflict, what: fmt.Sprintf("racine de projet %q : la "+
				"déclaration %s est illisible (%v), impossible de vérifier qu'elle ne partage pas cette racine",
				root, path, err), remedy: "la corriger"})
			continue
		}
		for _, decl := range []struct{ what, path string }{
			{"la racine", other.Spec.Layout.Root}, {"le docroot", other.Spec.Layout.Docroot},
		} {
			theirs := filepath.Clean(decl.path)
			if decl.path == "" || !validate.Overlap(root, theirs) {
				continue
			}
			relation := "est aussi " + decl.what
			switch {
			case theirs == root:
			case strings.HasPrefix(theirs, root+string(filepath.Separator)):
				relation = "contient " + decl.what + " " + theirs
			default:
				relation = "est sous " + decl.what + " " + theirs
			}
			rs = append(rs, &refusal{code: CodeConflict, what: fmt.Sprintf("racine de projet %q : elle %s "+
				"du projet %s (%s), et effacer l'une emporterait l'autre", root, relation, other.ID(), path),
				remedy: "corriger spec.layout.root de l'une des deux déclarations"})
			// Un voisin se nomme une fois : son docroot est d'ordinaire sous sa racine.
			break
		}
	}
	return rs
}

// diskRefusals éprouve ce que la déclaration ne dit pas : ce qui se trouve
// réellement à l'emplacement de la racine. Une racine absente n'a rien à effacer,
// mais les chemins de alsoMounts, eux, restent à éprouver — quoi qu'on trouve à
// l'emplacement de la racine. Sous un vrai répertoire, points de montage et
// propriétaire sont éprouvés l'un et l'autre, et nommés ensemble.
func (a *App) diskRefusals(p *model.Project, root string, alsoMounts ...string) refusals {
	var rs refusals
	add := func(r *refusal) {
		if r != nil {
			rs = append(rs, r)
		}
	}
	info, err := os.Lstat(root)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		add(&refusal{code: CodeError, what: fmt.Sprintf("racine de projet %q : %v", root, err)})
	case info.Mode()&os.ModeSymlink != 0:
		// os.RemoveAll n'effacerait que le lien, mais tout ce qui le précède —
		// l'archive, la mesure de l'espace, le propriétaire — lirait le lien ou sa
		// cible selon qu'il le suit ou non : ogs n'efface pas ce qu'il ne voit pas
		// en entier.
		target, _ := os.Readlink(root)
		add(&refusal{code: CodeConflict, what: fmt.Sprintf("racine de projet %q : c'est un lien "+
			"symbolique (vers %s), non un répertoire", root, target), remedy: "retirer le lien à la main"})
	case !info.IsDir():
		add(&refusal{code: CodeConflict, what: fmt.Sprintf("racine de projet %q : ce n'est pas un "+
			"répertoire", root)})
	default:
		add(mountRefusal(p, append([]string{root}, alsoMounts...)...))
		add(a.rootOwnerRefusal(p, root, info))
		add(a.foreignEntriesRefusal(p, root))
		return rs
	}
	add(mountRefusal(p, alsoMounts...))
	return rs
}

// rootOwnerRefusal compare le propriétaire de la racine au compte du projet. Le
// compte existe : la racine doit être à lui. Il n'existe pas — un clone qui n'a
// jamais été appliqué, un compte déjà supprimé à la main — : la racine ne doit
// alors appartenir à aucun autre compte existant, sans quoi c'est le home de
// quelqu'un d'autre que la déclaration désigne. Un UID orphelin, lui, est
// précisément ce que laisse un compte supprimé.
//
// En mode préfixé, le contrôle n'a pas d'objet : les comptes sont simulés, et
// tout ce que l'essai crée appartient à qui l'a lancé — comme pour les
// répertoires du projet, dont le provider système ne compare pas non plus le
// propriétaire sous préfixe.
func (a *App) rootOwnerRefusal(p *model.Project, root string, info os.FileInfo) *refusal {
	if a.Exec.Simulated() {
		return nil
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	u, err := a.systemProvider().Identity.LookupUser(p.Spec.User.Name)
	if err != nil {
		return &refusal{code: CodeError, what: fmt.Sprintf("racine de projet %q : compte %s illisible (%v)",
			root, p.Spec.User.Name, err)}
	}
	reason, err := ownerRefusal(st.Uid, p.Spec.User.Name, u,
		func(uid uint32) (string, error) { return accountOfUID(int(uid)) })
	if err != nil {
		return &refusal{code: CodeError, what: fmt.Sprintf("racine de projet %q : propriétaire illisible (%v)",
			root, err)}
	}
	if reason != "" {
		return &refusal{code: CodeConflict, what: fmt.Sprintf("racine de projet %q : %s", root, reason),
			remedy: "corriger spec.layout.root ou le propriétaire"}
	}
	return nil
}

// foreignEntriesRefusal refuse une racine dont une entrée de premier niveau
// appartient à un compte ordinaire existant autre que celui du projet
// (system.ForeignEntries : ni root, ni un UID orphelin, ni www-data ou un compte
// système). C'est la trace d'un home repris : avant D95, la création d'un projet
// sur le home laissé par un autre compte lui donnait la racine, et
// rootOwnerRefusal y voyait alors le propriétaire attendu — tout ce que l'autre
// compte y avait laissé partait avec le projet. Le premier niveau suffit à le
// voir, et reste bon marché ; au-delà, ce qu'ogs ne pose pas est le contenu du
// projet. --force ne le lève pas : ce qu'il protège n'est pas au projet.
func (a *App) foreignEntriesRefusal(p *model.Project, root string) *refusal {
	if a.Exec.Simulated() {
		return nil
	}
	u, err := a.systemProvider().Identity.LookupUser(p.Spec.User.Name)
	if err != nil {
		return &refusal{code: CodeError, what: fmt.Sprintf("racine de projet %q : compte %s illisible (%v)",
			root, p.Spec.User.Name, err)}
	}
	uid := -1
	if u != nil {
		uid = u.UID
	}
	found, err := system.ForeignEntries(root, uid, accountOfUID)
	if err != nil {
		return &refusal{code: CodeError, what: fmt.Sprintf("racine de projet %q : entrées illisibles (%v)",
			root, err)}
	}
	if len(found) == 0 {
		return nil
	}
	return &refusal{code: CodeConflict, what: fmt.Sprintf("racine de projet %q : %s appartien%s à un autre "+
		"compte que celui du projet — le home a pu être repris d'un autre compte", root,
		strings.Join(found, ", "), map[bool]string{true: "t", false: "nent"}[len(found) == 1]),
		remedy: "rendre ces entrées au projet ou les déplacer hors de la racine"}
}

// ownerRefusal rend la raison de refuser l'effacement d'une racine appartenant à
// l'UID owner, ou "" si rien ne s'y oppose. account est le compte du projet, nil
// s'il n'existe pas ; nameOf nomme le compte d'un UID, "" s'il n'en a aucun.
func ownerRefusal(owner uint32, user string, account *system.UserInfo,
	nameOf func(uid uint32) (string, error)) (string, error) {
	if account != nil && uint32(account.UID) == owner {
		return "", nil
	}
	name, err := nameOf(owner)
	if err != nil {
		return "", err
	}
	if account != nil {
		if name == "" {
			name = "un UID sans compte"
		}
		return fmt.Sprintf("elle appartient à %s (UID %d), non à l'utilisateur du projet %s (UID %d)",
			name, owner, user, account.UID), nil
	}
	if name != "" {
		return fmt.Sprintf("elle appartient au compte %s (UID %d), alors que l'utilisateur du projet %s "+
			"n'existe pas : c'est le home de quelqu'un d'autre", name, owner, user), nil
	}
	return "", nil
}

// accountOfUID nomme le compte système d'un UID, "" s'il n'en a aucun. Une
// variable, pour que les tests nomment les comptes sans toucher à la machine.
var accountOfUID = system.AccountOfUID

// mountInfoPath est la table des montages du processus. C'est elle qui fait foi,
// non la comparaison des st_dev : un bind mount issu du même système de fichiers
// garde le périphérique de sa source, et ne se distingue de l'arborescence qui
// l'accueille que par cette table. Une variable, pour que les tests en posent une.
var mountInfoPath = "/proc/self/mountinfo"

// mountRefusal refuse d'effacer — ou d'archiver — une arborescence sous laquelle
// un système de fichiers est monté : os.RemoveAll y descend comme ailleurs, et un
// partage de médias monté sous le home — bind mount, NFS commun à deux frontaux —
// était vidé pour tous ceux qui s'en servent, seul le rmdir final du point de
// montage échouant, après coup. L'archive, elle, le recopiait tout entier. ogs ne
// démonte rien : l'exploitant sait ce que le montage sert encore, et
// « --keep-data » lui laisse retirer le site sans y toucher.
func mountRefusal(p *model.Project, paths ...string) *refusal {
	var found []string
	for _, path := range paths {
		if path == "" {
			continue
		}
		points, err := mountPointsUnder(path)
		if err != nil {
			return &refusal{code: CodeError, what: fmt.Sprintf("projet %s : table des montages illisible "+
				"(%v), impossible de vérifier qu'aucun partage n'est monté sous %s", p.ID(), err, path)}
		}
		found = append(found, points...)
	}
	if len(found) == 0 {
		return nil
	}
	return &refusal{code: CodeConflict, what: fmt.Sprintf("projet %s : système de fichiers monté sous "+
		"l'arborescence (%s), qui sert peut-être encore d'autres sites ou d'autres serveurs", p.ID(),
		strings.Join(found, ", ")),
		remedy: "la suppression ne franchit jamais un point de montage : le démonter d'abord (umount)"}
}

// mountPointsUnder énumère les points de montage situés sous path, path compris :
// une racine qui est elle-même un point de montage verrait vider le système de
// fichiers qu'elle accueille. Le chemin est aussi comparé résolu, la table
// portant des chemins sans lien (un /home lié à /data/home y figure en
// /data/home).
func mountPointsUnder(path string) ([]string, error) {
	data, err := os.ReadFile(mountInfoPath)
	if err != nil {
		return nil, err
	}
	roots := []string{filepath.Clean(path)}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != roots[0] {
		roots = append(roots, resolved)
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		// « 36 35 98:0 /racine /point options… - type source options » : le
		// point de montage est le cinquième champ.
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		point := unescapeMountPath(fields[4])
		for _, root := range roots {
			if (point == root || strings.HasPrefix(point, root+"/")) && !seen[point] {
				seen[point] = true
				out = append(out, point)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// unescapeMountPath décode les séquences octales dont le noyau protège les
// blancs et la barre oblique inverse dans la table des montages (« \040 » pour
// une espace) : sans quoi un point de montage au nom espacé ne serait jamais
// reconnu.
func unescapeMountPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// systemRoot énumère les emplacements qu'aucune racine de projet ne peut valoir,
// adoptée ou non.
func systemRoot(clean string) bool {
	for _, f := range []string{"/etc", "/usr", "/bin", "/sbin", "/lib", "/boot",
		"/dev", "/proc", "/sys", "/run", "/var", "/var/lib", "/var/log", "/var/www",
		"/root", "/tmp", "/home", "/srv", "/opt"} {
		if clean == f {
			return true
		}
	}
	return false
}

// unitActive dit si systemd a l'unité active. Une unité inconnue, non chargée ou
// à l'arrêt répond non : il n'y a alors rien à arrêter ni à recharger, ce qui
// n'est pas un échec. En mode simulé, tout est réputé actif — c'est ce qui permet
// aux tests d'observer les commandes qui auraient été passées.
func (a *App) unitActive(ctx context.Context, name string) bool {
	if a.Exec.Simulated() {
		return true
	}
	_, err := a.Exec.Run(ctx, exec.Command{
		Name: "systemctl", Args: []string{"is-active", "--quiet", name},
	})
	return err == nil
}

// processesOfUID énumère les processus tournant sous un UID, « pid nom ». La
// lecture passe par /proc et non par pgrep : ogs ne dépend d'aucun binaire qui ne
// soit pas indispensable, et procps peut manquer sur une installation minimale.
func processesOfUID(uid int) []string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		status, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
		if err != nil {
			// Le processus s'est terminé entre-temps : rien d'anormal.
			continue
		}
		if !uidMatches(string(status), uid) {
			continue
		}
		name := "?"
		if comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm")); err == nil {
			name = strings.TrimSpace(string(comm))
		}
		out = append(out, fmt.Sprintf("%d %s", pid, name))
		if len(out) == maxListedProcesses {
			out = append(out, "…")
			break
		}
	}
	return out
}

// maxListedProcesses borne l'énumération : un pool FPM chargé peut avoir des
// centaines de workers, et les lister tous noierait le message.
const maxListedProcesses = 10

// uidMatches lit l'UID réel dans le champ « Uid: » de /proc/<pid>/status, dont le
// premier des quatre nombres est l'UID réel.
func uidMatches(status string, uid int) bool {
	for _, line := range strings.Split(status, "\n") {
		rest, ok := strings.CutPrefix(line, "Uid:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return false
		}
		real, err := strconv.Atoi(fields[0])
		return err == nil && real == uid
	}
	return false
}

// dumpWorkDir crée un répertoire de travail privé pour les sauvegardes de
// passage — celles qui entrent dans une archive, en sortent, ou transitent d'un
// projet à l'autre lors d'un clonage.
//
// Il ne peut pas être /tmp : le nom du fichier y serait déductible, le
// répertoire est inscriptible par tous, et chaque projet a un compte local
// dont le shell est ouvert (D62). Un fichier planté d'avance serait ouvert tel
// quel — le mode 0600 du dump ne vaut qu'à la création —, et un attaquant
// n'aurait même pas à le lire pour nuire : sur les plateformes visées le noyau
// refuse l'ouverture (fs.protected_regular), la sauvegarde échoue, et la
// suppression continuait alors vers une archive sans données. Sous /var/lib/ogs, que
// seul root traverse, aucun utilisateur de projet n'y crée de nom.
func (a *App) dumpWorkDir() (string, error) {
	root := a.Paths.DumpsDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	// MkdirTemp crée en 0700 : rien à corriger après coup.
	return os.MkdirTemp(root, ".travail-")
}

// archiveProject construit l'archive complète du projet. Les avertissements
// passent par le compteur de la commande et non par un simple affichage : une
// archive à laquelle il manque un dump n'est pas une archive dont on peut
// restaurer le site, et rendre 0 après l'avoir produite ferait croire le
// contraire à qui enchaîne sur la suppression.
//
// scope dit ce qu'elle recueille (archiveScope). Quand scope.strict est vrai,
// les bases vont être supprimées : une sauvegarde en échec abandonne alors
// l'archive et la suppression, avant tout retrait — elle partait sinon sans que
// l'archive en garde rien, au mode par défaut qui promet l'inverse. L'espace et
// le seuil de taille ont été vérifiés avant l'arrêt des services
// (checkArchiveSize).
func (a *App) archiveProject(cmd *cobra.Command, provs *providers, p *model.Project, scope archiveScope,
	warn func(format string, args ...any)) (string, []string, error) {
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
	// Les configurations générées, pour comparaison ultérieure. Un lien n'a pas
	// de contenu propre : il est noté parmi les retraits, sa cible est recueillie
	// pour elle-même.
	var removed []string
	for _, file := range scope.files {
		info, err := os.Lstat(file)
		if err != nil {
			continue
		}
		if info.Mode().IsRegular() {
			if err := w.AddFile("config", "config/"+filepath.Base(file), file); err != nil {
				return "", nil, wrap(CodeError, err)
			}
		}
		removed = append(removed, file)
	}
	// Une interruption abandonne l'archive entre deux phases : les dumps et les
	// arborescences peuvent prendre des minutes, qu'il serait vain de passer à
	// recueillir ce qu'on ne supprimera pas. Pendant une phase, les dumps et le
	// parcours des arborescences suivent le contexte (Q-86), et le point de
	// non-retour la voit de toute façon.
	if err := interrupted(cmd.Context()); err != nil {
		return "", nil, err
	}
	// Les dumps : sans eux, l'archive serait inexploitable pour une restauration.
	if len(scope.dumps) > 0 {
		dir, err := a.dumpWorkDir()
		if err != nil {
			return "", nil, wrap(CodeError, err)
		}
		defer os.RemoveAll(dir)
		for _, decl := range scope.dumps {
			tmp := filepath.Join(dir, decl.Name+".sql")
			if _, err := provs.DB.Dump(cmd.Context(), p, decl, tmp); err != nil {
				if scope.strict {
					return "", nil, exitf(CodeError, "sauvegarde de %s impossible : %v — sans elle, la "+
						"base serait supprimée sans recours ; %s", decl.Name, err, noDumpHint)
				}
				warn("sauvegarde de %s impossible : %v — l'archive ne contiendra pas ses données, "+
					"et « ogs project restore » ne les rendra pas", decl.Name, err)
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
			// Le fichier d'environnement qu'une restauration n'a pas pu rendre,
			// resté scellé au keystore, est archivé plus bas avec le fichier en
			// place (archiveEnvFile).
			if name == appprov.EnvFileSecret {
				continue
			}
			nonce, ciphertext, err := a.Store.GetSecret(p.ID(), name)
			if err != nil {
				continue
			}
			if err := w.AddSecret(name, nonce, ciphertext); err != nil {
				return "", nil, wrap(CodeError, err)
			}
		}
	}
	// Les certificats que la suppression retire, scellés eux aussi : la clé
	// privée n'a pas à voyager en clair (D100).
	if err := a.archiveCertificates(w, provs, p, scope.certs); err != nil {
		return "", nil, wrap(CodeError, err)
	}
	// La crontab personnelle et les tâches at du compte (D107) : la suppression
	// les retire, l'archive en garde la seule copie.
	if err := archiveUserJobs(w, scope.jobs); err != nil {
		return "", nil, wrap(CodeError, err)
	}
	if err := a.archiveEnvFile(w, p); err != nil {
		return "", nil, err
	}
	// L'arborescence complète, quand elle est au seul projet.
	if err := interrupted(cmd.Context()); err != nil {
		return "", nil, err
	}
	if !scope.skipTree {
		if err := w.AddTree(cmd.Context(), "tree", "tree", p.Spec.Layout.Root); err != nil {
			return "", nil, wrap(CodeError, err)
		}
	}
	// Les journaux, qui ne vivent plus dans le home (D71) : ils partent avec le
	// projet, comme lorsqu'ils étaient dans son arborescence.
	if err := interrupted(cmd.Context()); err != nil {
		return "", nil, err
	}
	if info, err := os.Stat(p.Spec.Layout.LogDir); err == nil && info.IsDir() && !scope.skipLogs {
		if err := w.AddTree(cmd.Context(), "logs", "logs", p.Spec.Layout.LogDir); err != nil {
			return "", nil, wrap(CodeError, err)
		}
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

// envFileKeptSecret porte dans l'archive la copie scellée du fichier
// d'environnement qu'une restauration n'avait pas pu écrire, quand un fichier
// écrit depuis l'emporte : elle est scellée sous le nom appprov.EnvFileSecret,
// sa donnée associée, et retourne au keystore sous ce nom (D104).
const envFileKeptSecret = "app/envfile.gardé"

// archiveEnvFile scelle le fichier d'environnement dans l'archive : il porte des
// secrets, et le recopier en clair ferait de l'archive leur porte de sortie.
//
// Une restauration qui n'a pas pu l'écrire en a laissé la copie scellée au
// keystore, et l'apply qui la suivait a posé à sa place le modèle vide d'ogs.
// Ce modèle ne l'emporte jamais sur la copie : archivé à sa place, il perdait
// les secrets de l'application à la suppression suivante, sans un mot (D104).
// Un fichier écrit depuis par l'exploitant l'emporte, et la copie, si elle en
// diffère, voyage à côté.
func (a *App) archiveEnvFile(w *archive.Writer, p *model.Project) error {
	path := a.Paths.EnvFile(p.ID())
	data, err := os.ReadFile(path)
	present := err == nil
	if err != nil && !os.IsNotExist(err) {
		return wrap(CodeError, err)
	}
	var kept [2][]byte
	if a.Store != nil {
		if nonce, ciphertext, err := a.Store.GetSecret(p.ID(), appprov.EnvFileSecret); err == nil {
			kept = [2][]byte{nonce, ciphertext}
		}
	}
	if kept[1] != nil && (!present || bytes.Equal(data, appprov.EnvFileTemplate(p.ID()))) {
		a.printf("✓ fichier d'environnement : la copie scellée qu'une restauration n'avait pas pu écrire est " +
			"archivée, le modèle vide d'ogs écarté\n")
		return wrap(CodeError, w.AddSecret(appprov.EnvFileSecret, kept[0], kept[1]))
	}
	if !present {
		return nil
	}
	if a.Keys == nil {
		return exitf(CodeError, "clé maître illisible : %s ne peut pas être archivé chiffré", path)
	}
	nonce, ciphertext, err := a.Keys.Seal(p.ID(), appprov.EnvFileSecret, data)
	if err != nil {
		return wrap(CodeError, err)
	}
	if err := w.AddSecret(appprov.EnvFileSecret, nonce, ciphertext); err != nil {
		return wrap(CodeError, err)
	}
	if kept[1] != nil {
		if plain, err := a.Keys.Open(p.ID(), appprov.EnvFileSecret, kept[0], kept[1]); err == nil &&
			bytes.Equal(plain, data) {
			return nil
		}
		a.printf("= fichier d'environnement : %s, écrit depuis la restauration, est archivé ; la copie scellée "+
			"qui en diffère voyage à côté, et retournera au keystore\n", path)
		return wrap(CodeError, w.AddSecret(envFileKeptSecret, kept[0], kept[1]))
	}
	return nil
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
			// Les répertoires tels que l'archive les déclare : la normalisation
			// retire les anciennes entrées logs/ et .bin/, qui disent pourtant, dans
			// une archive antérieure aux propriétaires portés, que ces répertoires
			// étaient à root (RestoredOwner).
			archived := append([]model.Dir(nil), p.Spec.Layout.Dirs...)
			if err := app.prepare(p); err != nil {
				return err
			}
			// Une archive qui déclare « webAccess: acl » serait refusée par l'apply,
			// après le dépôt de la déclaration : le refus vient avant tout (D107).
			if p.Spec.User.WebAccess == webAccessACL {
				return exitf(CodeValidation, "l'archive de %s déclare spec.user.webAccess « acl », qu'ogs "+
					"n'implémente pas : rien n'a été restauré.\n  remède : %s", p.ID(), webAccessArchiveFix)
			}
			// Une archive d'avant D91 peut déclarer un compte réservé : la restaurer
			// l'écrirait de nouveau au parc.
			if err := app.refuseReservedDBAccounts(p); err != nil {
				return err
			}
			// L'archive dit ce que le projet tenait à sa suppression, non ce que le
			// parc a déclaré depuis : un domaine, une base ou un home repris entre-temps
			// seraient servis deux fois.
			if err := app.checkFleetUniqueness(p); err != nil {
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
			skipped := 0
			for name, pair := range secrets {
				if !restorableSecret(p, name) {
					if keystoreSecret(name) {
						skipped++
					}
					continue
				}
				if err := app.Store.PutSecret(p.ID(), name, pair[0], pair[1]); err != nil {
					return wrap(CodeError, err)
				}
			}
			if skipped > 0 {
				app.printf("= %d preuve(s) de ressources que la déclaration ne nomme plus, non réinjectée(s) : "+
					"la suppression les avait retirées (D100)\n", skipped)
			}
			// Ce que la restauration n'a pas pu rendre, chacun avec son remède : le
			// projet est en place, il lui manque une partie de ce que l'archive
			// porte — succès partiel (code 6), jamais « ✓ restauré » ni un audit
			// « ok » (Q-01).
			var manques []restoreGap
			if m := app.restoreEnvFile(p, path, secrets); m != nil {
				manques = append(manques, *m)
			}
			manques = append(manques, app.restoreCertificates(p, path, secrets)...)
			if len(secrets) > 0 {
				app.printf("✓ %d secret(s) réinjecté(s)\n", len(secrets))
				if app.Keys != nil {
					for name, pair := range secrets {
						if name == envFileKeptSecret {
							// Scellée sous le nom du fichier d'environnement.
							name = appprov.EnvFileSecret
						}
						if _, err := app.Keys.Open(p.ID(), name, pair[0], pair[1]); err != nil {
							app.warnf("⚠ %s est illisible avec la clé maître actuelle : "+
								"l'archive vient-elle d'un autre serveur ?\n", name)
							manques = append(manques, restoreGap{
								fmt.Sprintf("secrets illisibles avec la clé maître actuelle (%s le premier)", name),
								"rétablir la clé maître d'origine dans " + app.Paths.MasterKey()})
							break
						}
					}
				}
			}

			// Puis l'infrastructure, avant de remettre le contenu. Un succès partiel
			// — une base indisponible, par exemple — ne doit pas empêcher la
			// restauration des fichiers : le code est conservé et rendu à la fin.
			if err := app.applyProjects(cmd, []*model.Project{p}); err != nil {
				if ExitCode(err) != CodePartial {
					return err
				}
				manques = append(manques, restoreGap{fmt.Sprintf("apply partiel (%v)", err),
					"reprendre les étapes en échec : ogs apply " + p.ID()})
			}
			// L'arborescence vient après l'apply, et c'est voulu : le compte existe
			// alors, et chaque entrée reçoit son propriétaire sur le descripteur de
			// ce qui vient d'être créé. Extraire avant, dans un home vierge, l'aurait
			// laissé à root jusqu'à une passe de chown faite après la création du
			// compte — ses services déjà démarrés, son code libre de glisser sous
			// un nom un lien dur vers un fichier du système, que chown lui aurait
			// donné. Ce que l'apply vient de poser dans le home — le lien du
			// trousseau commun, celui de l'interpréteur, ~/logs — n'est jamais
			// traversé : l'extraction le remplace, en tant que lien, par ce que
			// l'archive porte, soit le home tel que l'utilisateur l'avait laissé ;
			// l'apply suivant le traite selon ses règles (D61, D70, D71) — c'est
			// lui qui migre l'ancien ~/logs d'une archive antérieure à D71, rendu à
			// root comme il l'était.
			if err := app.restoreTree(path, p, archived, manifest); err != nil {
				app.warnf("⚠ restauration de l'arborescence : %v\n", err)
				manques = append(manques, restoreGap{
					fmt.Sprintf("arborescence incomplète dans %s (%v)", p.Spec.Layout.Root, err),
					fmt.Sprintf("l'archive %s en garde la copie, sous tree/", path)})
			}
			// Une base non rechargée est un succès partiel : le projet est en place,
			// sa base vide (D92).
			if withData {
				missing, err := app.restoreDumps(cmd, path, p, manifest)
				if err != nil {
					return err
				}
				manques = append(manques, missing...)
			}
			// La crontab et les tâches at du compte ne sont pas reposées (D107) :
			// l'archive les porte, la restauration ne les rend pas — un manque
			// comme un autre (D104).
			manques = append(manques, restoreUserJobs(path, p, manifest)...)
			if len(manques) > 0 {
				// La sortie d'erreur porte chaque manque avec son remède ; le motif
				// du code 6 et l'audit les nomment.
				what := make([]string, len(manques))
				app.warnf("\n⚠ %s restauré en partie :\n", p.ID())
				for i, m := range manques {
					what[i] = m.What
					app.warnf("  - %s — %s\n", m.What, m.Remedy)
				}
				_ = app.Audit.Record(p.ID(), "project", "project.restore", "avertissement",
					path+" : "+strings.Join(what, " ; "))
				return exitf(CodePartial, "%s restauré en partie : %s (remèdes ci-dessus)", p.ID(),
					strings.Join(what, " ; "))
			}
			_ = app.Audit.Record(p.ID(), "project", "project.restore", "ok", path)
			app.printf("\n✓ %s restauré\n", p.ID())
			return nil
		},
	}
	cmd.Flags().BoolVar(&withData, "with-data", true, "recharger les sauvegardes de bases")
	return cmd
}

// restoreEnvFile réécrit le fichier d'environnement archivé, et rend ce qui
// manque s'il n'a pas pu l'être. Il voyage parmi les secrets, et n'a pas à
// rester dans le keystore une fois rendu à son fichier ; non rendu, il y est
// gardé, scellé tel que l'archive le porte : sans quoi il ne serait plus nulle
// part hors de l'archive, qu'aucune commande ne relit en partie, et le remède
// ne serait qu'une nouvelle suppression suivie d'une restauration.
func (a *App) restoreEnvFile(p *model.Project, archivePath string, secrets map[string][2][]byte) *restoreGap {
	pair, ok := secrets[appprov.EnvFileSecret]
	if !ok {
		a.keepEnvFileCopy(p, secrets)
		return nil
	}
	path := a.Paths.EnvFile(p.ID())
	// Le remède écrit le fichier en 0600 : une redirection sous l'umask de root
	// le créerait lisible de tous.
	show := "(umask 077 && " + exec.ShellLine("ogs", "secrets", "show", p.ID(), appprov.EnvFileSecret) +
		" > " + exec.ShellLine(path) + ")"
	keep := func(motif, remède string) *restoreGap {
		a.warnf("⚠ %s : %s n'est pas restauré\n", motif, path)
		err := errors.New("keystore indisponible")
		if a.Store != nil {
			err = a.Store.PutSecret(p.ID(), appprov.EnvFileSecret, pair[0], pair[1])
		}
		if err != nil {
			a.warnf("⚠ %s : contenu non gardé au keystore (%v)\n", path, err)
			return &restoreGap{fmt.Sprintf("fichier d'environnement %s non restauré (%s)", path, motif),
				fmt.Sprintf("l'archive %s seule le garde, scellé", archivePath)}
		}
		if _, ok := secrets[envFileKeptSecret]; ok {
			a.warnf("⚠ %s : la copie plus ancienne que l'archive portait aussi n'est pas gardée au keystore — "+
				"l'archive %s la garde\n", path, archivePath)
		}
		return &restoreGap{fmt.Sprintf("fichier d'environnement %s non restauré (%s)", path, motif),
			"son contenu reste scellé au keystore ; " + remède}
	}
	if a.Keys == nil {
		return keep("clé maître illisible", fmt.Sprintf("rétablir la clé maître %s, puis %s",
			a.Paths.MasterKey(), show))
	}
	data, err := a.Keys.Open(p.ID(), appprov.EnvFileSecret, pair[0], pair[1])
	if err != nil {
		return keep("illisible avec la clé maître actuelle", fmt.Sprintf("rétablir la clé maître "+
			"d'origine dans %s, puis %s", a.Paths.MasterKey(), show))
	}
	// Le modèle vide d'ogs ne porte rien de l'exploitant : la copie archivée le
	// remplace (D104). Tout autre fichier en place est conservé.
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
		if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, appprov.EnvFileTemplate(p.ID())) {
			_ = os.Remove(path)
		}
	}
	if err := appprov.CreateEnvFile(path, data); err != nil {
		return keep(err.Error(), fmt.Sprintf("le fichier en place est conservé ; le comparer au "+
			"contenu archivé (%s), puis le remplacer s'il y a lieu : %s",
			exec.ShellLine("ogs", "secrets", "show", p.ID(), appprov.EnvFileSecret), show))
	}
	_ = a.Store.DeleteSecret(p.ID(), appprov.EnvFileSecret)
	a.printf("✓ fichier d'environnement restauré : %s\n", path)
	a.keepEnvFileCopy(p, secrets)
	return nil
}

// keepEnvFileCopy rend au keystore la copie scellée qu'une restauration
// antérieure n'avait pas pu écrire, et qu'un fichier écrit depuis a supplantée
// (archiveEnvFile) : rien de ce qu'elle porte n'est perdu.
func (a *App) keepEnvFileCopy(p *model.Project, secrets map[string][2][]byte) {
	pair, ok := secrets[envFileKeptSecret]
	if !ok || a.Store == nil {
		return
	}
	if err := a.Store.PutSecret(p.ID(), appprov.EnvFileSecret, pair[0], pair[1]); err != nil {
		a.warnf("⚠ copie antérieure du fichier d'environnement non gardée au keystore : %v\n", err)
		return
	}
	a.printf("= une copie antérieure du fichier d'environnement reste scellée au keystore : %s\n",
		exec.ShellLine("ogs", "secrets", "show", p.ID(), appprov.EnvFileSecret))
}

// restoreGap est ce qu'une restauration n'a pas pu rendre, et le remède.
type restoreGap struct{ What, Remedy string }

// restoreTree remet l'arborescence archivée dans le home, chaque entrée au nom
// que system.Provider.RestoredOwner lui donne — à l'utilisateur ce qui était à
// lui, à root le reste, d'après les propriétaires que l'archive porte, ou à
// défaut d'après sa déclaration archivée. Un « chown -R » après coup ne rendait
// que www/, et n'importe quel chown récursif passerait sur un home que
// l'utilisateur modifie déjà : le propriétaire est posé par l'extraction, sur
// chaque descripteur.
func (a *App) restoreTree(path string, p *model.Project, archived []model.Dir, manifest archive.Manifest) error {
	owner, err := a.systemProvider().RestoredOwner(p, archived, manifest.RootOwner("tree"))
	if err != nil {
		return fmt.Errorf("%w — rien n'a été extrait", err)
	}
	count, err := archive.Extract(path, "tree", p.Spec.Layout.Root, owner)
	if count > 0 || err == nil {
		a.printf("✓ %d fichier(s) restauré(s) dans %s\n", count, p.Spec.Layout.Root)
	}
	// L'apply de la restauration a précédé l'extraction : l'ancien répertoire des
	// journaux qu'elle vient de rendre attend l'apply suivant pour être migré. La
	// déclaration archivée dit s'il en est un ; un répertoire que l'utilisateur
	// aurait posé à la place du lien n'en est pas un, et la migration le refusera.
	legacy := model.LegacyLogDir(p.Spec.Layout.Root)
	for _, d := range archived {
		if filepath.Clean(d.Path) != model.LogLink || d.Owner != "root" {
			continue
		}
		if info, lerr := os.Lstat(legacy); lerr == nil && info.IsDir() {
			a.printf("→ l'archive rend l'ancien %s d'un projet antérieur à D71 : « ogs apply %s » en reprendra "+
				"les journaux sous %s\n", legacy, p.ID(), p.Spec.Layout.LogDir)
		}
		break
	}
	return err
}

// restoreDumps recharge les sauvegardes présentes dans l'archive, et rend, pour
// chacune qui n'a pas pu l'être, ce qui manque et le remède. Une interruption
// arrête tout et remonte en code 9 : les bases suivantes ne sont pas rechargées.
func (a *App) restoreDumps(cmd *cobra.Command, path string, p *model.Project, manifest archive.Manifest) ([]restoreGap, error) {
	var missing []restoreGap
	// Chaque sauvegarde est d'abord rapportée à une base déclarée : le nom vient
	// du manifeste, que rien n'a validé, et il ne compose de remède — une ligne
	// que l'exploitant copiera en root — que s'il désigne une base de la
	// déclaration. Le remède recharge la sauvegarde que l'archive garde, par ogs
	// lui-même, sous le même confinement (D92) : ni tar ni zstd, que Debian
	// n'installe pas.
	type dump struct {
		entry string
		decl  *model.Database
	}
	var dumps []dump
	for _, entry := range manifest.Entries {
		if entry.Kind != "dump" {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(entry.Name), ".sql")
		var decl *model.Database
		for i := range p.Spec.Databases {
			if p.Spec.Databases[i].Name == name && entry.Name == "dumps/"+name+".sql" {
				decl = &p.Spec.Databases[i]
			}
		}
		if decl == nil {
			a.warnf("⚠ aucune base déclarée pour la sauvegarde %q : ignorée\n", entry.Name)
			missing = append(missing, restoreGap{
				fmt.Sprintf("sauvegarde %q ignorée : la déclaration ne nomme pas cette base", entry.Name),
				"la déclarer, puis : " + exec.ShellLine("ogs", "db", "restore", p.ID(), name, "--from-archive", path)})
			continue
		}
		dumps = append(dumps, dump{entry.Name, decl})
	}
	manque := func(d dump, motif string) {
		missing = append(missing, restoreGap{fmt.Sprintf("base %s non rechargée (%s)", d.decl.Name, motif),
			"la recharger depuis l'archive : " + exec.ShellLine("ogs", "db", "restore", p.ID(), d.decl.Name,
				"--from-archive", path)})
	}
	if len(dumps) == 0 {
		return missing, nil
	}
	tous := func(motif string) []restoreGap {
		for _, d := range dumps {
			manque(d, motif)
		}
		return missing
	}
	provs, err := a.buildProviders()
	if err != nil {
		a.warnf("⚠ %v\n", err)
		return tous(err.Error()), nil
	}
	defer provs.close()
	dir, err := a.dumpWorkDir()
	if err != nil {
		a.warnf("⚠ %v\n", err)
		return tous(err.Error()), nil
	}
	defer os.RemoveAll(dir)
	for _, d := range dumps {
		name := d.decl.Name
		// En flux, sans passer en entier par la mémoire : une sauvegarde pèse ce
		// que pèse la base.
		tmp := filepath.Join(dir, name+".sql")
		if err := copyEntryTo(tmp, func(w io.Writer) (int64, error) {
			return archive.CopyFile(path, d.entry, w)
		}); err != nil {
			a.warnf("⚠ %s : %v\n", d.entry, err)
			manque(d, err.Error())
			continue
		}
		if err := provs.DB.Restore(cmd.Context(), p, *d.decl, tmp); errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) {
			_ = os.Remove(tmp)
			return missing, exitf(CodeCancelled, "restauration interrompue à la demande — la base %s n'est que "+
				"partiellement chargée : %v", name, err)
		} else if err != nil {
			a.warnf("⚠ restauration de %s : %v\n", name, err)
			manque(d, err.Error())
		} else {
			a.printf("✓ base %s restaurée\n", name)
		}
		_ = os.Remove(tmp)
	}
	return missing, nil
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

// userJobs relit ce que le compte du projet a posé hors d'ogs (D107) : rien
// sous --keep-data, qui garde le compte, ni sans compte.
func (a *App) userJobs(cmd *cobra.Command, provs *providers, p *model.Project, o deleteOptions) system.UserJobs {
	if o.keepData {
		return system.UserJobs{}
	}
	u, err := provs.System.Identity.LookupUser(p.Spec.User.Name)
	if err != nil || u == nil {
		return system.UserJobs{}
	}
	return provs.System.ReadUserJobs(cmd.Context(), u.Name)
}

// describeJobs nomme ce que la suppression retirera du compte.
func describeJobs(j system.UserJobs) []string {
	var out []string
	if len(j.Crontab) > 0 {
		out = append(out, "la crontab personnelle de "+j.Account)
	}
	if ids := j.AtIDs(); len(ids) > 0 {
		out = append(out, fmt.Sprintf("les tâches at %s de %s", strings.Join(ids, ", "), j.Account))
	}
	if j.Linger {
		out = append(out, "le linger de "+j.Account)
	}
	return out
}

// archiveUserJobs recopie la crontab et les tâches at du compte sous user/.
func archiveUserJobs(w *archive.Writer, j system.UserJobs) error {
	if len(j.Crontab) > 0 {
		if err := w.AddBytes("user", "user/crontab", "crontab -l -u "+j.Account, j.Crontab, 0o600); err != nil {
			return err
		}
	}
	for _, id := range j.AtIDs() {
		if err := w.AddBytes("user", "user/at/"+id, "at -c "+id, j.AtJobs[id], 0o600); err != nil {
			return err
		}
	}
	return nil
}

// restoreUserJobs nomme ce que l'archive porte sous user/ et que la
// restauration ne repose pas (D107) : la crontab personnelle et les tâches at
// du compte. Les reposer d'office rendrait au compte recréé des tâches que
// personne n'a relues ; ne pas les nommer ferait annoncer « ✓ restauré » d'un
// projet dont les tâches ne tournent plus (D104). Les noms viennent du
// manifeste, que rien n'a validé : seul un numéro de tâche est repris.
func restoreUserJobs(path string, p *model.Project, manifest archive.Manifest) []restoreGap {
	var gaps []restoreGap
	var ids []string
	account := p.Spec.User.Name
	for _, e := range manifest.Entries {
		if e.Kind != "user" {
			continue
		}
		if e.Name == "user/crontab" {
			gaps = append(gaps, restoreGap{
				fmt.Sprintf("crontab personnelle de %s non reposée", account),
				fmt.Sprintf("l'archive %s en garde la copie, sous user/crontab ; la relire, puis : %s", path,
					exec.ShellLine("crontab", "-u", account)+" <fichier>")})
			continue
		}
		if id, ok := strings.CutPrefix(e.Name, "user/at/"); ok && id != "" &&
			strings.Trim(id, "0123456789") == "" {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		gaps = append(gaps, restoreGap{
			fmt.Sprintf("tâches at %s de %s non reposées", strings.Join(ids, ", "), account),
			fmt.Sprintf("l'archive %s en garde les scripts, sous user/at/ ; les reprogrammer à la main", path)})
	}
	return gaps
}
