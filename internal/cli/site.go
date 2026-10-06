package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/hook"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/provider/amqp"
	"leblanc.io/open-go-server/internal/provider/db"
	tlsprov "leblanc.io/open-go-server/internal/provider/tls"
	"leblanc.io/open-go-server/internal/provider/unit"
	"leblanc.io/open-go-server/internal/spec"
	"leblanc.io/open-go-server/internal/validate"
)

func newSiteCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "site",
		Aliases: []string{"sites"},
		Short:   "Créer et dupliquer des sites complets",
	}
	cmd.AddCommand(newSiteCreateCommand(app), newSiteCloneCommand(app))
	return cmd
}

// siteOptions rassemble les choix d'une création composite.
type siteOptions struct {
	kind       string
	appVersion string
	runtime    string
	command    string
	envFile    bool
	phpVersion string
	env        string
	id         string
	aliases    []string
	redirects  []string
	database   string
	messenger  []string
	tls        bool
	selfSigned bool
	services   []string
	crons      []string
	preset     string
	apply      bool
}

func newSiteCreateCommand(app *App) *cobra.Command {
	var opts siteOptions
	cmd := &cobra.Command{
		Use:   "create <host>",
		Short: "Créer un site complet en une opération",
		Long: "Assemble l'ensemble des ressources d'un site — utilisateur, arborescence, base,\n" +
			"transport, pool PHP, vhost, certificat, tâches et services — puis applique le\n" +
			"tout en une saga : si une étape critique échoue, les précédentes sont défaites.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.buildSite(args[0], opts)
			if err != nil {
				return err
			}
			path := app.Paths.ProjectFile(p.ID())
			if _, err := os.Stat(path); err == nil {
				return exitf(CodeConflict, "le projet %s existe déjà (%s)", p.ID(), path)
			}
			if err := naming.CheckCreatable(p.ID()); err != nil {
				return exitf(CodeValidation, "%v", err)
			}
			// Seuls comptent les conflits qui touchent le nouveau projet : une collision
			// entre deux voisins ne bloque pas une création sans rapport (D94).
			if err := app.checkFleetUniqueness(p); err != nil {
				return err
			}
			if err := app.refuseReservedDBAccounts(p); err != nil {
				return err
			}

			app.printf("site %s\n", p.ID())
			app.summarizeSite(p)
			// Le home est éprouvé avant que la déclaration ne soit déposée (D95) : le
			// plan le refuserait, et la déclaration restée arrêterait tout apply du parc.
			if err := app.preexistingHome(cmd.Context(), p); err != nil {
				return err
			}
			if app.DryRun {
				data, err := spec.Encode(p)
				if err != nil {
					return wrap(CodeError, err)
				}
				app.printf("\n--dry-run : %s serait écrit avec le contenu suivant.\n\n", path)
				fmt.Fprint(app.Out, string(data))
				return nil
			}
			// Le hook « preCreate » garde la porte : il peut refuser la création
			// avant que rien n'ait été écrit.
			if _, err := app.runHook(cmd.Context(), app.hookRunner(), p, hook.PreCreate, hook.Context{}); err != nil {
				return err
			}
			if err := spec.Save(path, p); err != nil {
				return wrap(CodeError, err)
			}
			if err := app.openStore(); err == nil {
				_ = app.Audit.Record(p.ID(), "site", "site.create", "ok", path)
				// postCreate attend le premier apply réussi, ici ou plus tard (D112).
				app.markDeclared(p)
			}
			app.printf("\n✓ déclaration écrite : %s\n", path)
			if !opts.apply {
				app.printf("\nAppliquer :\n  ogs apply %s\n", p.ID())
				return nil
			}
			return app.applyProjects(cmd, []*model.Project{p})
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.kind, "kind", string(model.KindSymfony), "type d'application ("+kindsList()+")")
	f.StringVar(&opts.appVersion, "app-version", "auto", "version applicative")
	f.StringVar(&opts.runtime, "runtime", "", "version du runtime pour Node, Ruby ou Python (exemple 22.14.0)")
	f.StringVar(&opts.command, "command", "", "commande lancée par le service d'un projet custom (exemple '/home/x/www/bin/serveur --port ${PORT}')")
	f.BoolVar(&opts.envFile, "env-file", false, "brancher /etc/ogs/env/<id>.env sur le service applicatif")
	f.StringVar(&opts.phpVersion, "php", "", "version PHP (défaut "+model.DefaultPHPVersion+")")
	f.StringVar(&opts.env, "env", "prod", "environnement : prod ou prep")
	f.StringVar(&opts.id, "id", "", "forcer l'identifiant")
	f.StringSliceVar(&opts.aliases, "alias", nil, "domaine servi en plus du principal (répétable)")
	f.StringSliceVar(&opts.redirects, "redirect", nil, "domaine redirigé vers le principal (répétable)")
	f.StringVar(&opts.database, "db", "", "déclarer une base : mysql, mariadb ou postgres")
	f.StringSliceVar(&opts.messenger, "messenger", nil, "transports Messenger à déclarer (répétable)")
	f.BoolVar(&opts.tls, "tls", true, "demander un certificat TLS")
	f.BoolVar(&opts.selfSigned, "self-signed", false, "certificat auto-signé (développement)")
	f.StringSliceVar(&opts.services, "service", nil,
		"service à déclarer, sous la forme template:transport:instances (répétable)")
	f.StringSliceVar(&opts.crons, "cron", nil,
		"tâche à déclarer, sous la forme « expression:commande » (répétable)")
	f.StringVar(&opts.preset, "preset", "", "dimensionnement du pool PHP")
	f.BoolVar(&opts.apply, "apply", true, "appliquer immédiatement")
	return cmd
}

// buildSite assemble un projet complet à partir des options.
func (a *App) buildSite(host string, opts siteOptions) (*model.Project, error) {
	env, err := parseEnv(opts.env)
	if err != nil {
		return nil, err
	}
	kind := model.AppKind(opts.kind)
	if _, err := model.Resolve(kind, opts.appVersion); err != nil {
		return nil, exitf(CodeUsage, "%s (types connus : %s)", err, kindsList())
	}
	p := model.New(host, env, kind)
	if opts.runtime != "" {
		p.Spec.App.Runtime = opts.runtime
	}
	p.Spec.App.Command = opts.command
	p.Spec.App.EnvFile = opts.envFile
	if opts.appVersion != "" {
		p.Spec.App.Version = opts.appVersion
	}
	if opts.id != "" {
		if err := naming.ValidateIdentifier(opts.id); err != nil {
			return nil, exitf(CodeUsage, "%s", err)
		}
		p.Metadata.Name = opts.id
	}
	if opts.phpVersion != "" {
		p.Spec.PHP.Pools = []model.Pool{{Name: model.MainPoolName, Version: opts.phpVersion}}
	}
	a.creationSudo(p)
	// Première normalisation : l'identifiant et le host effectif en découlent, et
	// tout le reste s'y adosse.
	if err := a.prepare(p); err != nil {
		return nil, err
	}
	if opts.preset != "" && len(p.Spec.PHP.Pools) > 0 {
		p.Spec.PHP.Pools[0].Preset = opts.preset
	}

	// Domaines : le principal vient de la normalisation, les alias et redirections
	// s'y ajoutent.
	for _, alias := range opts.aliases {
		p.Spec.Domains = append(p.Spec.Domains, model.Domain{Name: alias, Role: "alias"})
	}
	primary := p.Spec.Domains[0].Name
	for _, redirect := range opts.redirects {
		p.Spec.Domains = append(p.Spec.Domains,
			model.Domain{Name: redirect, Role: "redirect", Target: primary})
	}

	if opts.database != "" {
		switch opts.database {
		case "mysql", "mariadb", "postgres":
			p.Spec.Databases = []model.Database{db.DefaultDatabase(p, opts.database)}
		default:
			return nil, exitf(CodeUsage, "moteur %q inconnu (mysql, mariadb ou postgres)", opts.database)
		}
	}
	if len(opts.messenger) > 0 {
		if kind == model.KindSymfony && !model.SupportsMessenger(kind, p.Spec.App.Version) {
			return nil, exitf(CodeUsage, "Symfony %s n'a pas le composant Messenger "+
				"(introduit en 4.1) : employer un service rabbitmq-consumer", p.Spec.App.Version)
		}
		p.Spec.AMQP = amqp.Preset(p, opts.messenger, true)
	}
	if !opts.tls {
		// Le certificat doit disparaître avec le TLS : le laisser déclaré ferait
		// solliciter l'autorité — et consommer un quota — pour un site qui reste
		// délibérément en HTTP.
		p.Spec.Certificates = nil
		// Le réglage est porté par le projet, non par les vhosts : ceux-ci sont le
		// plus souvent implicites, et la liste est vide à ce stade.
		p.Spec.Web.TLS = &model.TLS{Enabled: false}
		for i := range p.Spec.Web.VHosts {
			p.Spec.Web.VHosts[i].TLS = &model.TLS{Enabled: false}
		}
	}
	if opts.selfSigned {
		p.Spec.Certificates = []model.Certificate{{
			Name: p.ID(), Domains: tlsprov.CertifiedDomains(p), Challenge: "selfsigned",
		}}
	}
	for _, raw := range opts.services {
		svc, err := parseServiceOption(raw)
		if err != nil {
			return nil, exitf(CodeUsage, "%s", err)
		}
		p.Spec.Services = append(p.Spec.Services, svc)
	}
	for _, raw := range opts.crons {
		job, err := parseCronOption(raw)
		if err != nil {
			return nil, exitf(CodeUsage, "%s", err)
		}
		p.Spec.Crons = append(p.Spec.Crons, job)
	}
	// Seconde normalisation et validation, une fois toutes les ressources posées.
	if err := a.prepare(p); err != nil {
		return nil, err
	}
	return p, nil
}

// parseServiceOption lit « template:transport:instances ».
func parseServiceOption(raw string) (model.Service, error) {
	parts := strings.Split(raw, ":")
	if parts[0] == "" {
		return model.Service{}, fmt.Errorf("service %q : template manquant "+
			"(attendu template:transport:instances)", raw)
	}
	svc := model.Service{Template: parts[0], Instances: 1, Params: map[string]string{}}
	transport := ""
	if len(parts) > 1 && parts[1] != "" {
		transport = parts[1]
		svc.Params["transport"] = transport
	}
	if len(parts) > 2 && parts[2] != "" {
		n, err := strconv.Atoi(parts[2])
		if err != nil || n < 1 {
			return model.Service{}, fmt.Errorf("service %q : nombre d'instances invalide", raw)
		}
		svc.Instances = n
	}
	svc.Name = parts[0]
	if transport != "" {
		svc.Name = strings.TrimPrefix(parts[0], "symfony-") + "-" + transport
	}
	return svc, nil
}

// parseCronOption lit « expression:commande ». La découpe se fait au premier
// deux-points : une expression cron n'en contient jamais, alors qu'une commande
// en contient souvent (« bin/console schedule:run »).
func parseCronOption(raw string) (model.CronJob, error) {
	idx := strings.Index(raw, ":")
	if idx <= 0 || idx == len(raw)-1 {
		return model.CronJob{}, fmt.Errorf("tâche %q : format attendu « expression:commande »", raw)
	}
	schedule := strings.TrimSpace(raw[:idx])
	command := strings.TrimSpace(raw[idx+1:])
	if err := validate.CronExpression(schedule); err != nil {
		return model.CronJob{}, fmt.Errorf("tâche %q : %w", raw, err)
	}
	name := "tache"
	if fields := strings.Fields(command); len(fields) > 0 {
		base := filepath.Base(fields[len(fields)-1])
		name = strings.NewReplacer(":", "-", ".", "-", "/", "-").Replace(base)
	}
	return model.CronJob{Name: name, Schedule: schedule, Command: command}, nil
}

// summarizeSite affiche ce qui va être créé.
func (a *App) summarizeSite(p *model.Project) {
	at, _ := p.AppType()
	a.printf("  host         %s\n", p.Spec.Domains[0].Name)
	a.printf("  utilisateur  %s:%s\n", p.Spec.User.Name, p.Spec.User.Group)
	a.printf("  home         %s\n", p.Spec.Layout.Root)
	if at.Docroot != "" {
		a.printf("  docroot      %s\n", p.Spec.Layout.Docroot)
	}
	for _, pool := range p.Spec.PHP.Pools {
		a.printf("  php          %s (pool %s, %s)\n", pool.Version, pool.Name, a.poolPreset(p, pool))
	}
	for _, d := range p.Spec.Databases {
		a.printf("  base         %s %s (utilisateur %s)\n", d.Engine, d.Name, d.User)
	}
	if p.Spec.AMQP != nil {
		a.printf("  transport    vhost %s, files %s\n", p.Spec.AMQP.Vhost,
			strings.Join(queueNames(p.Spec.AMQP), ", "))
	}
	for _, svc := range p.Spec.Services {
		state := ""
		if !isEnabled(svc.Enabled) {
			state = ", désactivé"
		}
		a.printf("  service      %s (%s, %d instance(s)%s)\n", svc.Name, svc.Template, svc.Instances, state)
	}
	for _, c := range p.Spec.Crons {
		state := ""
		if !isEnabled(c.Enabled) {
			state = ", désactivée"
		}
		a.printf("  tâche        %s (%s%s)\n", c.Name, c.Schedule, state)
	}
}

// isEnabled lit le champ « enabled » d'une tâche ou d'un service : absent, il
// vaut activé.
func isEnabled(enabled *bool) bool { return enabled == nil || *enabled }

// disabled rend une valeur « enabled: false » propre à son champ.
func disabled() *bool {
	off := false
	return &off
}

func newSiteCloneCommand(app *App) *cobra.Command {
	var (
		to       string
		withData bool
	)
	cmd := &cobra.Command{
		Use:   "clone <id>",
		Short: "Dupliquer un site vers un autre environnement",
		Long: "Crée le projet de destination avec la même configuration, recopie le contenu de\n" +
			"www/ sans suivre aucun lien et n'en reprend que les fichiers du compte source, puis,\n" +
			"si demandé, restaure un dump de la base source dans la base cible.\n\n" +
			"Un lien est recopié comme lien, et un www source remplacé par un lien fait refuser\n" +
			"la copie. Les fichiers d'un autre compte (root, www-data), les fichiers spéciaux et\n" +
			"ce qui se trouve au-delà de 1024 niveaux sont écartés et nommés, et la commande rend\n" +
			"alors le code 6. Ni les ACL ni les attributs étendus ne sont reportés. Une\n" +
			"interruption (Ctrl-C) arrête la copie et rend le code 9 : le projet de destination\n" +
			"est créé, avec une partie du contenu seulement.\n\n" +
			"La copie ne retire que ce qu'ogs connaît de la source. Les fichiers où le type\n" +
			"d'application range la configuration d'un environnement — .env.local de Symfony,\n" +
			"parameters.php de Prestashop, .env d'une application Node — et ses caches compilés ne\n" +
			"sont pas recopiés ; les mots de passe qu'ogs a générés pour les bases et le transport\n" +
			"de la source, et les valeurs de son fichier d'environnement, sont masqués partout\n" +
			"ailleurs, déclaration du projet comprise ; la racine de la source y est remplacée par\n" +
			"celle de la destination. Le reste passe tel quel, et la sortie le dit : tout autre\n" +
			"fichier de www (le .env versionné de Symfony compris), le reste d'une chaîne dont le\n" +
			"mot de passe est masqué, le contenu de la base. Chaque fichier retenu ou expurgé,\n" +
			"chaque variable déclarée reprise, chaque lien dont la cible sort de la destination,\n" +
			"chaque secret qu'ogs n'a pas pu chercher est nommé, et la commande rend alors le code\n" +
			"6 : la destination est à revoir, « ogs secrets env » en donne les chaînes de\n" +
			"connexion. Les tâches et les services y sont repris désactivés. --dry-run annonce ce\n" +
			"que la copie retiendra, masquera et reprendra.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			source, err := app.loadProject(args[0])
			if err != nil {
				return err
			}
			env, err := parseEnv(to)
			if err != nil {
				return err
			}
			if env == source.Env() {
				return exitf(CodeUsage, "le projet %s est déjà en environnement %s", source.ID(), to)
			}
			target, err := app.cloneProject(source, env)
			if err != nil {
				return err
			}
			path := app.Paths.ProjectFile(target.ID())
			if _, err := os.Stat(path); err == nil {
				return exitf(CodeConflict, "le projet %s existe déjà (%s)", target.ID(), path)
			}
			if err := app.checkFleetUniqueness(target); err != nil {
				return err
			}
			app.printf("clone de %s vers %s (%s)\n", source.ID(), target.ID(), to)
			app.summarizeSite(target)
			// Ce que la source tient d'ogs ne passe pas à la destination, ni par sa
			// déclaration — une variable d'environnement du pool, qu'elle recevrait
			// de FPM avant toute configuration applicative —, ni par la copie de son
			// contenu ; ce qui passe est nommé. --dry-run l'annonce : la garde ne
			// fait que lire le keystore et le fichier d'environnement de la source.
			if err := app.openStore(); err != nil {
				return err
			}
			guard := app.cloneGuard(source)
			declared := reviewDeclared(target, source, guard.Values)
			withData = withData && len(source.Spec.Databases) > 0 && len(target.Spec.Databases) > 0
			// Le home est éprouvé avant que la déclaration ne soit déposée (D95) : le
			// plan le refuserait, et la déclaration restée arrêterait tout apply du parc.
			if err := app.preexistingHome(cmd.Context(), target); err != nil {
				return err
			}
			if app.DryRun {
				app.describeCloneGuard(guard, declared, withData)
				app.printf("\n--dry-run : rien n'a été écrit.\n")
				return nil
			}
			for _, d := range declared.lines {
				app.warnf("⚠ %s\n", d)
			}
			// Le hook « preCreate » garde la porte : il peut refuser la création
			// avant que rien n'ait été écrit.
			if _, err := app.runHook(cmd.Context(), app.hookRunner(), target, hook.PreCreate, hook.Context{}); err != nil {
				return err
			}
			if err := spec.Save(path, target); err != nil {
				return wrap(CodeError, err)
			}
			// Un succès partiel n'interrompt pas la copie du contenu : le code est
			// conservé et rendu à la fin.
			partial := error(nil)
			if err := app.applyProjects(cmd, []*model.Project{target}); err != nil {
				if ExitCode(err) != CodePartial {
					return err
				}
				partial = err
			}

			// Copie du contenu applicatif, une fois l'arborescence en place. Une copie
			// refusée laisse la préproduction sans contenu, une copie interrompue ou
			// une entrée écartée la laisse incomplète : c'est un succès partiel, que le
			// code de retour doit dire autant que l'avertissement.
			srcWWW := filepath.Join(source.Spec.Layout.Root, "www")
			dstWWW := filepath.Join(target.Spec.Layout.Root, "www")
			skipped, err := app.copyTree(cmd.Context(), source, target, guard.Guard)
			for _, s := range skipped.Named {
				app.warnf("⚠ non recopié : %s\n", s)
			}
			if n := skipped.Count - len(skipped.Named); n > 0 {
				app.warnf("⚠ … et %d autre(s) entrée(s) non recopiée(s)\n", n)
			}
			copied := !errors.Is(err, errNotCopied)
			guarded := app.reportCloneCopy(skipped, guard, copied)
			switch {
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				// Une interruption demandée n'est pas un succès partiel : rien de ce
				// qui suit — la copie des données comprise — ne doit se faire, et le
				// projet créé ne doit pas s'annoncer comme un clone achevé.
				_ = app.Audit.Record(target.ID(), "site", "site.clone", "interrompu",
					"depuis "+source.ID()+" : copie du contenu interrompue à la demande")
				return exitf(CodeCancelled, "copie interrompue à la demande — %s n'en porte qu'une partie ; "+
					"le projet %s est créé : « ogs project delete %s » avant un nouveau clone",
					dstWWW, target.ID(), target.ID())
			case errors.Is(err, errNotCopied):
				app.warnf("⚠ copie du contenu : %v\n", err)
				if partial == nil {
					partial = exitf(CodePartial, "le contenu de %s n'a pas été recopié", srcWWW)
				}
			case err != nil:
				app.warnf("⚠ copie du contenu interrompue : %v — %s n'en porte qu'une partie\n", err, dstWWW)
				if partial == nil {
					partial = exitf(CodePartial, "le contenu de %s n'a été recopié qu'en partie", srcWWW)
				}
			case skipped.Count > 0:
				app.warnf("⚠ contenu copié en partie : %s → %s, %d entrée(s) écartée(s)\n",
					srcWWW, dstWWW, skipped.Count)
				if partial == nil {
					partial = exitf(CodePartial, "%d entrée(s) de %s non recopiée(s)", skipped.Count, srcWWW)
				}
			case len(guarded) > 0:
				// Une copie expurgée, ou qui laisse passer ce qu'ogs sait désigner,
				// n'est pas celle d'un contenu neutre : elle ne s'annonce pas comme
				// faite, et le code de retour le dit (plus bas).
				app.warnf("⚠ contenu copié avec les réserves ci-dessus : %s → %s\n", srcWWW, dstWWW)
			default:
				app.printf("✓ contenu copié : %s → %s\n", srcWWW, dstWWW)
			}
			if err == nil {
				if at, aerr := target.AppType(); aerr == nil {
					if d := exposedInstaller(at, dstWWW); d != "" {
						public := ""
						if target.IsPrep() {
							public = ", et une préproduction est publique (D57)"
						}
						app.warnf("⚠ installeur exposé : %s/ est recopié, et %s n'a aucun fichier de configuration — "+
							"une application %s y renvoie tout visiteur%s : écrire sa configuration avant de "+
							"l'ouvrir, ou retirer %s/\n", filepath.Join(dstWWW, d), target.ID(), at.Kind, public, d)
						guarded = append(guarded, "installeur exposé")
					}
				}
			}

			dataCopied := false
			if withData {
				if err := app.cloneData(cmd, source, target); ExitCode(err) == CodeCancelled {
					_ = app.Audit.Record(target.ID(), "site", "site.clone", "interrompu",
						"depuis "+source.ID()+" : copie des données interrompue à la demande")
					return err
				} else if err != nil {
					// Le projet est créé et sa base existe, vide : un succès partiel
					// que le code de retour doit dire autant que l'avertissement, comme
					// un contenu non recopié (Q-04).
					app.warnf("⚠ copie des données : %v\n", err)
					guarded = append(guarded, fmt.Sprintf("données de la base %s non copiées",
						target.Spec.Databases[0].Name))
				} else {
					dataCopied = true
				}
			}
			if n := declared.review; n > 0 {
				guarded = append(guarded, fmt.Sprintf("%d champ(s) de la déclaration à revoir", n))
			}
			app.adviseCloneConfig(source, target, guard, skipped, declared, copied, dataCopied)
			app.adviseDisabledTasks(source, target)
			// Ce qui n'a pas été recopié tel quel, ce qui l'a été et qu'ogs sait
			// désigner, ce qu'il n'a pas pu chercher : la destination est à revoir,
			// un succès partiel, jamais un succès muet.
			if len(guarded) > 0 {
				review := fmt.Sprintf("%s à revoir avant usage : %s", target.ID(), strings.Join(guarded, ", "))
				if partial != nil {
					// Un avertissement de l'apply ne doit pas taire la garde.
					review = partial.Error() + " ; " + review
				}
				partial = exitf(CodePartial, "%s", review)
			}
			// postCreate une fois le contenu en place, non avant (D112) : c'est le
			// site cloné qu'il trouve, pas une arborescence vide.
			if warned, _ := app.runHook(cmd.Context(), app.hookRunner(), target, hook.PostCreate,
				hook.Context{}); warned && partial == nil {
				partial = exitf(CodePartial, "hook postCreate en échec")
			}
			outcome, detail := "ok", "depuis "+source.ID()
			if partial != nil {
				outcome, detail = "avertissement", detail+" : "+partial.Error()
			}
			_ = app.Audit.Record(target.ID(), "site", "site.clone", outcome, detail)
			if partial != nil {
				app.warnf("\n⚠ %s créé depuis %s — succès partiel\n", target.ID(), source.ID())
			} else {
				app.printf("\n✓ %s créé depuis %s\n", target.ID(), source.ID())
			}
			return partial
		},
	}
	cmd.Flags().StringVar(&to, "to", "prep", "environnement de destination")
	cmd.Flags().BoolVar(&withData, "with-data", true, "copier aussi le contenu de la base")
	return cmd
}

// cloneProject construit le projet de destination depuis la source.
func (a *App) cloneProject(source *model.Project, env naming.Env) (*model.Project, error) {
	clone := *source
	clone.Metadata.Name = ""
	clone.Metadata.Env = string(env)
	clone.Metadata.Description = source.Metadata.Description
	// La préproduction est créée par ogs, jamais adoptée : l'étiquette héritée la
	// faisait échapper au refus des homonymes, et sa suppression sous --force les
	// emportait (D91). Les étiquettes sont dupliquées, pas partagées avec la source.
	clone.Metadata.Labels = maps.Clone(source.Metadata.Labels)
	delete(clone.Metadata.Labels, "adopted")
	// Tout ce qui porte l'identifiant doit être recalculé : l'oublier ferait
	// pointer la préproduction sur les ressources de la production.
	clone.Spec.User = model.User{}
	clone.Spec.Layout = model.Layout{}
	clone.Spec.Domains = nil
	clone.Spec.Web.VHosts = nil
	clone.Spec.Certificates = nil
	clone.Spec.Databases = nil
	clone.Spec.AMQP = nil
	// Ce que la copie de la structure partagerait avec la source est dupliqué :
	// la déclaration du clone est expurgée des secrets de la production
	// (reviewDeclared), et ses tâches et services désactivés, sans toucher à ceux
	// de la source.
	clone.Spec.App.Env = maps.Clone(source.Spec.App.Env)
	clone.Spec.PHP.Pools = slices.Clone(source.Spec.PHP.Pools)
	for i := range clone.Spec.PHP.Pools {
		clone.Spec.PHP.Pools[i].Env = maps.Clone(clone.Spec.PHP.Pools[i].Env)
		// Copie profonde : les valeurs sont pointées (D103), et le masquage des
		// secrets les réécrit en place — il réécrirait celles de la source.
		clone.Spec.PHP.Pools[i].Settings = clone.Spec.PHP.Pools[i].Settings.Clone()
		clone.Spec.PHP.Pools[i].AllowFunctions = slices.Clone(clone.Spec.PHP.Pools[i].AllowFunctions)
	}
	// Tâches et services partent désactivés : sur une copie des données de la
	// production, une relance, un envoi ou un import agiraient comme elle — sur
	// ses vrais clients —, et ils tourneraient avant que la préproduction ait sa
	// propre configuration. Ils se réactivent un à un (adviseDisabledTasks).
	clone.Spec.Crons = slices.Clone(source.Spec.Crons)
	for i := range clone.Spec.Crons {
		clone.Spec.Crons[i].Enabled = disabled()
	}
	clone.Spec.Services = slices.Clone(source.Spec.Services)
	for i := range clone.Spec.Services {
		clone.Spec.Services[i].Params = maps.Clone(clone.Spec.Services[i].Params)
		clone.Spec.Services[i].Enabled = disabled()
	}
	if err := a.prepare(&clone); err != nil {
		return nil, err
	}
	for _, d := range source.Spec.Databases {
		decl := db.DefaultDatabase(&clone, d.Engine)
		decl.Cluster = d.Cluster
		decl.Extensions = d.Extensions
		decl.Privileges = d.Privileges
		clone.Spec.Databases = append(clone.Spec.Databases, decl)
	}
	if source.Spec.AMQP != nil {
		transports := make([]string, 0, len(source.Spec.AMQP.Queues))
		for _, q := range source.Spec.AMQP.Queues {
			if !strings.HasSuffix(q.Name, amqp.FailedSuffix) {
				transports = append(transports, q.Name)
			}
		}
		clone.Spec.AMQP = amqp.Preset(&clone, transports, true)
	}
	if err := a.prepare(&clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

// errNotCopied marque une copie refusée avant d'avoir rien écrit : la
// préproduction est sans contenu, et non avec une partie.
var errNotCopied = errors.New("rien n'a été recopié")

// copyTree recopie le www du projet source dans celui du projet cible, donne la
// copie au projet cible, et rend compte des entrées écartées — toutes comptées,
// les premières nommées : leur nombre est aux mains de l'utilisateur de la
// production.
//
// Le www source appartient à l'utilisateur de la production, qui peut le
// remplacer — lui ou n'importe lequel de ses sous-répertoires — par un lien vers
// ce que root seul peut lire. « cp -a src/. dst » lancé en root suivait ce lien,
// et le « chown -R » qui suivait donnait la copie au compte de préproduction,
// dans un docroot servi. fs.CopyTreeAt ouvre chaque composant par un descripteur
// sans suivre de lien : un lien est recopié comme lien, jamais traversé, et un
// www source qui en est un fait refuser la copie. Il ne recopie que les fichiers
// du compte source, résolu par l'identité et non déduit du propriétaire de www :
// un lien physique vers un fichier d'un autre compte n'en est pas un. Les
// propriétaires sont posés entrée par entrée, sur les descripteurs : il n'y a
// plus de « chown -R » qui repasserait par les noms.
//
// La copie est une opération de fichiers, non une commande : sous préfixe, elle
// a lieu comme les autres écritures, les propriétaires en moins — le compte
// source est alors celui qui possède son www, l'essai lui-même.
//
// Une erreur rendue avant toute écriture porte errNotCopied. Toute autre arrête
// une copie commencée : un lien ou une entrée absente rencontrés plus bas n'ont
// pas le sens qu'ils ont à la racine, et « rien n'est recopié » ou « source
// absente » y seraient faux.
//
// La copie suit ctx, que main annule sur SIGINT et SIGTERM : « cp -a » mourait
// avec son groupe de processus, une copie faite dans le processus doit consulter
// le contexte elle-même. Annulée, elle rend l'erreur du contexte, telle quelle.
//
// La garde g retient de la copie la configuration et les secrets de la source
// (cloneGuard) : fs.CopyTreeGuardedAt en rend compte avec le reste.
func (a *App) copyTree(ctx context.Context, source, target *model.Project, g fs.Guard) (fs.Skipped, error) {
	srcUID, owner := -1, fs.KeepOwner
	if !a.Exec.Simulated() {
		uid, _, err := a.systemProvider().ResolveOwner(source, "")
		if err != nil {
			return fs.Skipped{}, fmt.Errorf("%w : %w", errNotCopied, err)
		}
		srcUID = uid
		uid, gid, err := a.systemProvider().ResolveOwner(target, "")
		if err != nil {
			return fs.Skipped{}, fmt.Errorf("%w : %w", errNotCopied, err)
		}
		owner = fs.Owner{UID: uid, GID: gid}
	}
	skipped, err := fs.CopyTreeGuardedAt(ctx, source.Spec.Layout.Root, "www", target.Spec.Layout.Root, "www",
		srcUID, owner, g)
	var root *fs.RootError
	switch {
	case !errors.As(err, &root):
		return skipped, err
	case errors.Is(err, fs.ErrNotDirectory):
		return skipped, fmt.Errorf("%w, ogs ne suit aucun lien : %w", errNotCopied, err)
	case root.Source && errors.Is(err, os.ErrNotExist):
		return skipped, fmt.Errorf("%w, source absente : %w", errNotCopied, err)
	}
	return skipped, fmt.Errorf("%w : %w", errNotCopied, err)
}

// cloneData transfère le contenu de la base source vers la base cible.
func (a *App) cloneData(cmd *cobra.Command, source, target *model.Project) error {
	provs, err := a.buildProviders()
	if err != nil {
		return err
	}
	defer provs.close()
	// Le dump de la source ne transite pas par /tmp : son nom y serait déductible
	// et un tiers projet se ferait livrer par root la base d'un site dont il n'est
	// pas propriétaire.
	dir, err := a.dumpWorkDir()
	if err != nil {
		return wrap(CodeError, err)
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, source.Spec.Databases[0].Name+".sql")
	if err := provs.DB.CloneData(cmd.Context(), source, source.Spec.Databases[0],
		target, target.Spec.Databases[0], tmp); err != nil {
		// Une interruption demandée n'est pas un succès partiel, comme pour la
		// copie des fichiers.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return exitf(CodeCancelled, "copie des données interrompue à la demande — la base %s n'en porte "+
				"qu'une partie : %v", target.Spec.Databases[0].Name, err)
		}
		return err
	}
	a.printf("✓ données copiées : %s → %s\n", source.Spec.Databases[0].Name, target.Spec.Databases[0].Name)
	return nil
}

// applyProjects applique les projets qu'une commande de création vient de
// déclarer — site create, site clone —, par le chemin unique
// d'« ogs apply » (D112) : plans du projet et du parc affichés et confirmés
// ensemble, hooks preApply et postApply, plan du parc exécuté même si le projet
// échoue. Une création que la saga a défaite entière retire sa déclaration.
func (a *App) applyProjects(cmd *cobra.Command, projects []*model.Project) error {
	return a.applyFleet(cmd.Context(), projects, applyRun{creation: true, recap: true})
}

// statusRow est la vue consolidée d'un projet.
type statusRow struct {
	Project   string   `json:"project" yaml:"project"`
	Env       string   `json:"env" yaml:"env"`
	Host      string   `json:"host" yaml:"host"`
	PHP       string   `json:"php,omitempty" yaml:"php,omitempty"`
	Drift     int      `json:"drift" yaml:"drift"`
	Services  string   `json:"services,omitempty" yaml:"services,omitempty"`
	Certifate string   `json:"certificate,omitempty" yaml:"certificate,omitempty"`
	Health    string   `json:"health" yaml:"health"`
	Problems  []string `json:"problems,omitempty" yaml:"problems,omitempty"`
}

func newStatusCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status [id...]",
		Short: "Vue consolidée de l'état des projets",
		Long: "Rassemble dérive de configuration, services actifs et échéance des certificats.\n" +
			"Le code de retour est non nul dès qu'un problème est détecté, ce qui rend la\n" +
			"commande utilisable en supervision.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Sans argument, le parc lisible : une déclaration refusée est nommée
			// et comptée parmi les problèmes, sans cacher l'état des autres (D120).
			var projects []*model.Project
			var refused int
			var err error
			app.tolerateFleet = true
			if len(args) == 0 {
				projects, refused, err = app.loadFleetNamed()
			} else {
				projects, err = app.targets(args, "")
			}
			if err != nil {
				return err
			}
			if err := app.openStore(); err != nil {
				return err
			}
			// L'état se calcule sans rien réserver (D100).
			app.Store.PreviewAllocations(true)
			provs, err := app.buildProviders()
			if err != nil {
				return err
			}
			defer provs.close()

			var rows []statusRow
			problems := refused
			restores := app.unfinishedRestores()
			for _, p := range projects {
				row := statusRow{Project: p.ID(), Env: p.Metadata.Env, Host: p.Spec.Domains[0].Name,
					Health: "sain"}
				// Une restauration inachevée (D113) : le projet est déclaré, son
				// contenu peut-être pas rendu.
				if s, ok := restores[p.ID()]; ok {
					row.Health = "restauration inachevée"
					row.Problems = append(row.Problems, "restauration inachevée : "+app.restoreRemedy(s))
					problems++
				}
				if len(p.Spec.PHP.Pools) > 0 {
					row.PHP = p.Spec.PHP.Pools[0].Version
				}
				plan, err := app.buildPlan(cmd.Context(), provs, p)
				if err != nil {
					row.Health = "erreur"
					row.Problems = append(row.Problems, err.Error())
					problems++
					rows = append(rows, row)
					continue
				}
				row.Drift = len(plan.Drift())
				if row.Drift > 0 {
					row.Health = "dérive"
					problems++
				}
				if len(p.Spec.Services) > 0 {
					obs, err := provs.Unit.Observe(cmd.Context(), p)
					if err == nil {
						active, declared := 0, 0
						for _, svc := range p.Spec.Services {
							want := svc.Instances
							if want <= 0 {
								want = 1
							}
							declared += want
							active += obs.Instances[svc.Name]
						}
						row.Services = fmt.Sprintf("%d/%d", active, declared)
						if active < declared {
							row.Health = "services incomplets"
							row.Problems = append(row.Problems,
								fmt.Sprintf("%d instance(s) sur %d actives", active, declared))
							problems++
						}
					}
				}
				if ref, ready := provs.TLS.CertificateFor(p); ref != "" {
					if !ready {
						état, problème := étatDuCertificat(provs, p, ref)
						row.Certifate = état
						row.Problems = append(row.Problems, problème)
						if row.Health == "sain" {
							row.Health = "TLS " + état
						}
						problems++
					} else if info, ok, _ := provs.TLS.Store.Read(ref); ok {
						days := info.DaysLeft(time.Now())
						row.Certifate = fmt.Sprintf("%d j", days)
						if info.NeedsRenewal(time.Now()) {
							row.Problems = append(row.Problems,
								fmt.Sprintf("certificat %s à renouveler (%d jours)", ref, days))
							problems++
						}
					}
				}
				rows = append(rows, row)
			}

			var warnings []string
			for _, r := range rows {
				warnings = append(warnings, r.Problems...)
			}
			if err := app.emit("StatusList", rows, warnings, func(tw *tabwriter.Writer) {
				if len(rows) == 0 {
					fmt.Fprintln(app.Out, "aucun projet déclaré")
					return
				}
				fmt.Fprintln(tw, "PROJET\tENV\tHOST\tPHP\tÉCARTS\tSERVICES\tTLS\tÉTAT")
				for _, r := range rows {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
						r.Project, r.Env, r.Host, orValue(r.PHP, "—"), r.Drift,
						orValue(r.Services, "—"), orValue(r.Certifate, "—"), r.Health)
				}
			}); err != nil {
				return wrap(CodeError, err)
			}
			if problems > 0 {
				return exitf(CodePartial, "%d problème(s) détecté(s)", problems)
			}
			return nil
		},
	}
}

// unitTarget expose le nom de cible pour les messages.
func unitTarget(id string) string { return unit.TargetName(id) }
