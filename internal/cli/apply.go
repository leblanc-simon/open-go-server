package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/archive"
	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	appprov "leblanc.io/open-go-server/internal/provider/app"
	"leblanc.io/open-go-server/internal/provider/firewall"
	"leblanc.io/open-go-server/internal/provider/mail"
	"leblanc.io/open-go-server/internal/provider/php"
	"leblanc.io/open-go-server/internal/provider/removal"
	"leblanc.io/open-go-server/internal/provider/unit"
	"leblanc.io/open-go-server/internal/provider/web"
	"leblanc.io/open-go-server/internal/spec"
	"leblanc.io/open-go-server/internal/store"
	"leblanc.io/open-go-server/internal/template"
	"leblanc.io/open-go-server/internal/validate"
)

// buildPlan observe le système et calcule le plan d'un projet. L'ordre des
// providers suit celui des dépendances : utilisateur et arborescence, puis pools
// PHP, puis vhosts — un vhost référence un socket, qui référence un pool, qui
// tourne sous un utilisateur.
func (a *App) buildPlan(ctx context.Context, provs *providers, p *model.Project) (*engine.Plan, error) {
	plan := &engine.Plan{Project: p.ID()}

	sysObs, err := provs.System.Observe(ctx, p)
	if err != nil {
		return nil, wrap(CodeError, err)
	}
	sysSteps, err := provs.System.Plan(ctx, p, sysObs)
	if err != nil {
		return nil, wrap(CodeConflict, err)
	}
	for _, s := range sysSteps {
		plan.Add(s)
	}
	// Reprise de l'ancien ~/logs (D71) : la recopie précède la réécriture des
	// vhosts et du pool, la fin vient en dernier, une fois les serveurs rechargés
	// sur le nouvel emplacement.
	logsPrepare, logsFinish := provs.System.PlanLogMigration(p, sysObs)
	if logsPrepare != nil {
		plan.Add(logsPrepare)
	}

	// Les bases viennent après l'utilisateur et avant les pools : une application
	// n'a pas besoin de sa base pour que son pool existe, mais l'ordre du cahier
	// des charges place les données avant l'exécution.
	// Un projet qui ne déclare plus aucune base peut encore tenir des comptes
	// d'ogs, que le plan retire (D100).
	if len(p.Spec.Databases) > 0 || provs.DB.HasAccounts(p) {
		dbObs, err := provs.DB.Observe(ctx, p)
		if err != nil {
			return nil, wrap(CodeError, err)
		}
		dbSteps, err := provs.DB.Plan(ctx, p, dbObs)
		if err != nil {
			return nil, wrap(CodeValidation, err)
		}
		for _, s := range dbSteps {
			plan.Add(s)
		}
	}

	// Sans spec.amqp, le projet peut encore porter les preuves d'un vhost et d'un
	// compte qu'ogs a créés pour lui : la déclaration ne les demandant plus, le
	// plan les retire (D100).
	if p.Spec.AMQP != nil || provs.AMQP.HasLeftovers(p) {
		amqpObs, err := provs.AMQP.Observe(ctx, p)
		if err != nil {
			return nil, wrap(CodeError, err)
		}
		amqpSteps, err := provs.AMQP.Plan(ctx, p, amqpObs)
		if err != nil {
			return nil, wrap(CodeValidation, err)
		}
		for _, s := range amqpSteps {
			plan.Add(s)
		}
	}

	at, err := p.AppType()
	if err != nil {
		return nil, exitf(CodeValidation, "%s", err)
	}
	var stale []*engine.Step
	if at.NeedsPHP {
		phpObs, err := provs.PHP.Observe(ctx, p)
		if err != nil {
			return nil, wrap(CodeError, err)
		}
		phpSteps, err := provs.PHP.Plan(ctx, p, phpObs)
		if err != nil {
			// Le pool d'un autre projet est un conflit, comme un domaine déjà servi
			// — non une dépendance absente.
			var owned *fs.OwnerError
			if errors.As(err, &owned) {
				return nil, wrap(CodeConflict, a.ownerGone(err))
			}
			// Une déclaration de pool refusée (D103) est une erreur de validation.
			var refused *php.DeclarationError
			if errors.As(err, &refused) {
				return nil, wrap(CodeValidation, err)
			}
			return nil, wrap(CodeDependency, err)
		}
		for _, s := range phpSteps {
			plan.Add(s)
		}
		// Les suppressions de pools sont différées après la réécriture des vhosts.
		stale, err = provs.PHP.PlanStale(ctx, p, phpObs)
		if err != nil {
			return nil, wrap(CodeError, err)
		}
	} else if stale, err = provs.PHP.PlanStale(ctx, p, provs.PHP.Retired(p)); err != nil {
		// Un type sans PHP retire les pools qu'ogs a écrits pour le projet (D115).
		return nil, wrap(CodeError, err)
	}

	// Application non-PHP : runtime, unité et attente de disponibilité. Elle
	// précède le vhost de proxy, qui exposerait sinon une erreur 502.
	var appObs appprov.Observed
	if appprov.Applies(p) {
		var err error
		appObs, err = provs.App.Observe(ctx, p)
		if err != nil {
			return nil, wrap(CodeError, err)
		}
		appSteps, err := provs.App.Plan(ctx, p, appObs)
		if err != nil {
			return nil, wrap(CodeValidation, err)
		}
		for _, s := range appSteps {
			plan.Add(s)
		}
	}

	// Tâches planifiées, services et application : ils viennent après
	// l'arborescence et l'interpréteur dont ils dépendent, mais avant le serveur
	// web — un vhost de proxy branché sur une application qui n'a pas encore
	// démarré exposerait une 502.
	cronSteps, err := provs.Cron.Plan(ctx, p)
	if err != nil {
		return nil, wrap(CodeValidation, err)
	}
	for _, s := range cronSteps {
		plan.Add(s)
	}

	unitObs, err := provs.Unit.Observe(ctx, p)
	if err != nil {
		return nil, wrap(CodeError, err)
	}
	unitSteps, err := provs.Unit.Plan(ctx, p, unitObs)
	if err != nil {
		return nil, wrap(CodeValidation, err)
	}
	for _, s := range unitSteps {
		plan.Add(s)
	}
	// L'unité applicative d'un projet qui n'en a plus part après l'arrêt de ses
	// instances, que les étapes d'unités viennent de porter (D100).
	appStale, err := provs.App.PlanStale(p)
	if err != nil {
		return nil, wrap(CodeError, err)
	}
	if appStale != nil {
		plan.Add(appStale)
	}

	// L'attente de disponibilité vient après le démarrage des instances : attendre
	// avant d'avoir lancé quoi que ce soit ne pourrait qu'échouer, en accusant
	// l'application à tort.
	if appprov.Applies(p) {
		healthStep, err := provs.App.PlanHealth(ctx, p, appObs)
		if err != nil {
			return nil, wrap(CodeValidation, err)
		}
		plan.Add(healthStep)
	}

	if len(p.Spec.Domains) > 0 {
		// Une paire clé/certificat dépareillée se rétablit avant tout ce qui écrit
		// la configuration du serveur web (D110) : nginx refuse la configuration
		// entière dès « nginx -t », donc l'écriture du premier vhost tant qu'un
		// autre — la redirection du même certificat, un voisin pour un partagé —
		// désigne encore la paire, et l'étape d'émission, qui suit les vhosts,
		// n'était jamais atteinte.
		pairSteps, err := provs.TLS.PlanPairs(ctx, p)
		if err != nil {
			return nil, wrap(CodeValidation, err)
		}
		for _, s := range pairSteps {
			plan.Add(s)
		}

		// Le fragment de portée serveur précède les vhosts, et non le plan global
		// qui s'exécute après les projets : un vhost de proxy emploie
		// $connection_upgrade, que seule la « map » de ce fragment définit — sans
		// lui, nginx refuse toute la configuration à l'activation du vhost, la saga
		// déroule le projet et le plan global qui l'aurait écrit n'est jamais
		// atteint. C'est désormais le seul endroit qui le porte : le garder aussi au
		// plan global faisait annoncer par « plan » et « diff » — qui calculent tout
		// sans rien écrire — N+1 écritures d'un fichier unique, là où « apply » n'en
		// fait qu'une.
		snippet, err := provs.Web.PlanSnippet()
		if err != nil {
			return nil, wrap(CodeError, err)
		}
		plan.Add(snippet)

		// Le complément d'unité du moteur web précède les vhosts, et non le plan
		// global qui s'exécute après les projets : un vhost journalisant dans un
		// répertoire que le moteur ne peut pas atteindre arrête le serveur au
		// rechargement, donc avant que le plan global n'ait eu son tour. Il énumère
		// les journaux de tout le parc, l'unité étant partagée.
		// Une consultation (status, la TUI) le calcule sur le parc lisible : les
		// déclarations refusées y sont nommées à part, et rien n'est écrit. Un
		// apply, lui, a déjà refusé le parc par checkFleetUniqueness (D120).
		var all []*model.Project
		if a.tolerateFleet {
			all, _, err = a.loadFleet()
		} else {
			all, err = a.loadProjects()
		}
		if err != nil {
			return nil, err
		}
		dropIn, err := provs.Web.PlanDropIn(ctx, withProject(all, p))
		if err != nil {
			return nil, wrap(CodeError, err)
		}
		for _, s := range dropIn {
			plan.Add(s)
		}

		webObs, err := provs.Web.Observe(ctx, p)
		if err != nil {
			return nil, wrap(CodeError, err)
		}
		webSteps, err := provs.Web.Plan(ctx, p, webObs)
		if err != nil {
			return nil, a.webPlanErr(err)
		}
		for _, s := range webSteps {
			plan.Add(s)
		}

		// Les certificats viennent après le vhost HTTP : le challenge HTTP-01
		// exige que /.well-known/acme-challenge réponde déjà. Puis le vhost est
		// basculé en TLS (§6.2, étapes 7 à 9).
		certSteps, err := provs.TLS.Plan(ctx, p)
		if err != nil {
			return nil, wrap(CodeValidation, err)
		}
		for _, s := range certSteps {
			plan.Add(s)
		}
		for _, s := range provs.Web.PlanTLS(ctx, p) {
			plan.Add(s)
		}
	}
	for _, s := range stale {
		plan.Add(s)
	}

	// Clés DKIM (D69). Elles ne dépendent de rien de ce qui précède, et rien n'en
	// dépend dans le projet : les tables qui les désignent sont au plan global,
	// qui s'exécute après — une table ne référence ainsi jamais une clé que
	// l'apply n'a pas encore posée.
	mailObs, err := provs.Mail.Observe(ctx)
	if err != nil {
		return nil, wrap(CodeError, err)
	}
	mailSteps, err := provs.Mail.Plan(ctx, p, mailObs)
	if err != nil {
		return nil, wrap(CodeError, err)
	}
	for _, s := range mailSteps {
		plan.Add(s)
	}

	// La délégation est écrite en dernier : en style systemctl, elle énumère les
	// instances, dont le nombre vient d'être arrêté. Une application supervisée y
	// donne droit autant qu'un service déclaré : son propriétaire doit pouvoir la
	// redémarrer après un déploiement, c'est même le cas d'usage principal.
	if len(p.Spec.Services) > 0 || p.Spec.Sudo != nil || appprov.Applies(p) {
		sudoStep, err := provs.Sudoers.Plan(ctx, p, unitObs.Instances)
		if err != nil {
			return nil, wrap(CodeValidation, err)
		}
		plan.Add(sudoStep)
	}

	// L'ancien répertoire des liens (D70) part en tout dernier : les crons et les
	// unités qui le désignaient viennent d'être réécrits, et l'étape attend que
	// systemd les ait relus.
	if s := provs.System.PlanLegacyBin(p); s != nil {
		plan.Add(s)
	}
	if logsFinish != nil {
		plan.Add(logsFinish)
	}
	if a.holdServices {
		plan.Steps = heldBack(p, plan.Steps)
	}
	return plan, nil
}

// webPlanErr donne son code à une erreur du plan web. Un domaine déjà servi et
// le fichier d'un autre projet sont des conflits (4) ; un nom de fichier que le
// moteur ne chargerait pas est une faute de déclaration (3), et un fichier dont
// l'état ne se lit pas avant son retrait, une erreur ordinaire (1).
func (a *App) webPlanErr(err error) error {
	var file *web.FileError
	var inspect *removal.InspectError
	switch {
	case errors.As(err, &file):
		return wrap(CodeValidation, err)
	case errors.As(err, &inspect):
		return wrap(CodeError, err)
	}
	return wrap(CodeConflict, a.ownerGone(err))
}

// ownerGone marque le propriétaire d'un fichier refusé (fs.OwnerError) comme
// disparu quand sa déclaration n'existe plus : le message renverrait sinon à
// « ogs apply » d'un projet qui n'est plus là — le cas d'un fichier laissé par
// une suppression. Seule la présence du fichier de déclaration compte : charger
// le parc entier ferait échouer le message sur le premier voisin invalide.
func (a *App) ownerGone(err error) error {
	var owned *fs.OwnerError
	if errors.As(err, &owned) {
		if _, statErr := os.Stat(a.Paths.ProjectFile(owned.Owner)); os.IsNotExist(statErr) {
			owned.Gone = true
		}
	}
	return err
}

// buildGlobalPlan calcule les étapes qui portent sur l'ensemble du parc.
//
// Les fichiers de portée serveur — logrotate, répertoires d'exécution, vhost de
// supervision, entrées Telegraf, prisons fail2ban — sont recalculés EN ENTIER
// (D64), jamais complétés : les établir sur la seule liste visée par un « apply
// <id> », ou sur le seul projet que « site create » vient d'écrire, effacerait
// tous les autres du fichier. Rien ne le dirait — l'en-tête réécrit reste
// conforme, et systemd-tmpfiles ne supprime pas : la panne n'apparaîtrait qu'au
// redémarrage suivant, /run étant un tmpfs, quand plus aucun /run/ogs/<id> ne
// serait recréé et que tous les verrous de tâches échoueraient. Le parc fait
// donc foi ici, et la liste reçue ne sert qu'à y substituer la version en
// mémoire des projets visés, qui peut n'être pas encore sur le disque (« -f »,
// création).
func (a *App) buildGlobalPlan(ctx context.Context, provs *providers, targeted []*model.Project) (*engine.Plan, error) {
	return a.buildGlobalPlanWithout(ctx, provs, targeted, "")
}

// buildGlobalPlanWithout calcule le plan du parc sans le projet nommé : celui
// que « project delete » s'apprête à retirer, pour en montrer le plan avant la
// confirmation (D112).
func (a *App) buildGlobalPlanWithout(ctx context.Context, provs *providers, targeted []*model.Project,
	without string) (*engine.Plan, error) {
	projects, err := a.loadProjects()
	if err != nil {
		return nil, err
	}
	if without != "" {
		kept := projects[:0]
		for _, p := range projects {
			if p.ID() != without {
				kept = append(kept, p)
			}
		}
		projects = kept
	}
	for _, p := range targeted {
		projects = withProject(projects, p)
	}
	// Tout recalcul du parc passe ici — apply, création, suppression d'un voisin,
	// « mail dkim activate » : la rotation et les prisons y décrivent les journaux
	// de chaque projet, et un répertoire hors de logs.root n'y entre jamais (D71,
	// D120). Le refus est le même qu'au plan et à l'apply.
	if err := a.checkPlacement(projects); err != nil {
		return nil, err
	}
	// Le plan global lit les pools de tout le parc (vhost d'état, Telegraf) : il
	// n'en réserve aucun numéro. Celui d'un projet qu'aucun apply n'a encore visé
	// l'était sinon par l'apply d'un voisin (D100). L'aperçu reste en vigueur
	// jusqu'à la fin du processus, car certaines étapes ne rendent leur contenu
	// qu'à l'application : le plan global vient toujours après les projets.
	a.Store.PreviewAllocations(true)

	plan := &engine.Plan{Project: "*"}

	// Purge des archives expirées : une archive porte un dump de base, donc des
	// données de clients. Elle est une étape du plan, non un effet de bord, pour
	// être annoncée avant d'être faite.
	if step, err := a.purgeArchivesStep(); err != nil {
		return nil, err
	} else {
		plan.Add(step)
	}

	// Répertoires d'exécution : verrous de tâches et sockets applicatifs.
	runtimeStep, err := provs.Runtime.Plan(projects)
	if err != nil {
		return nil, wrap(CodeError, err)
	}
	plan.Add(runtimeStep)

	// Timer de renouvellement : ressource de portée serveur, installée dès qu'un
	// certificat est en jeu.
	binary, err := os.Executable()
	if err != nil || a.Prefix != "" {
		binary = "/usr/bin/ogs"
	}
	units, err := provs.TLS.RenewUnitPlan(template.New(a.Paths.TemplatesDir()), binary, "")
	if err != nil {
		return nil, wrap(CodeError, err)
	}
	parcNonBloquante(units...)
	for _, s := range units {
		plan.Add(s)
	}

	// Supervision. Le vhost d'état précède les entrées de l'agent, qui s'y
	// adressent, et le fichier de prisons ferme la marche : fail2ban refuse de
	// démarrer sur une prison dont aucun journal n'existe, et les journaux des
	// projets viennent d'être créés par le rechargement du moteur web à la fin de
	// leurs propres plans.
	monObs, err := provs.Monitoring.Observe(ctx)
	if err != nil {
		return nil, planErr(err)
	}
	statusOn, set := a.Cfg.Monitoring.StatusEnabled()
	if !set {
		statusOn = monObs.TelegrafPresent
	}
	statusSteps, err := provs.Web.PlanStatus(ctx, projects,
		a.Cfg.Monitoring.StatusListen(), a.Cfg.Monitoring.StatusPath(), statusOn)
	if err != nil {
		return nil, planErr(err)
	}
	for _, s := range statusSteps {
		plan.Add(s)
	}

	monSteps, err := provs.Monitoring.Plan(ctx, projects, monObs)
	if err != nil {
		return nil, planErr(err)
	}
	for _, s := range monSteps {
		plan.Add(s)
	}

	// Tables d'OpenDKIM : recalculées depuis le parc entier, comme les fichiers de
	// supervision, et après les plans de projets qui ont posé les clés.
	mailObs, err := provs.Mail.Observe(ctx)
	if err != nil {
		return nil, planErr(err)
	}
	mailSteps, err := provs.Mail.PlanTables(projects, mailObs)
	if err != nil {
		return nil, planErr(err)
	}
	for _, s := range mailSteps {
		plan.Add(s)
	}

	// Pare-feu (D72) : après tout ce qui fait naître un service, pour que ses
	// règles existent le jour où il écoute, et avant la rotation des journaux,
	// dont le refus éventuel ne doit pas le priver de son tour. Ses gardes contre
	// la coupure n'y cèdent jamais : le --force qu'on donne ici pour un fichier
	// retouché ne dit rien du pare-feu, et les lever est une décision que seules
	// les commandes « ogs firewall » prennent (--allow-lockout).
	fwSteps, err := a.firewallSteps(ctx, provs.Firewall, projects, false)
	if err != nil {
		return nil, err
	}
	for _, s := range fwSteps {
		plan.Add(s)
	}

	// La rotation des journaux ferme la marche, et c'est un ordre, non une
	// commodité : c'est le fichier de parc qu'un exploitant retouche le plus
	// volontiers — un « compress », une taille —, donc l'étape qui a le plus de
	// chances de refuser (§6.4). Rien de ce qui précède ne dépend d'elle, et
	// l'infrastructure du parc — répertoires d'exécution, timer de renouvellement,
	// supervision — est ainsi posée avant. L'ordre ne suffirait pas à lui seul, une
	// saga déroulant ce qu'elle a appliqué avant un échec critique : voir
	// parcNonBloquante, qui ôte à ce refus le pouvoir d'emporter le reste.
	step, err := provs.Logrotate.Plan(projects, provs.Engine.Name(), provs.Engine.Unit())
	if err != nil {
		return nil, wrap(CodeError, err)
	}
	parcNonBloquante(step)
	plan.Add(step)
	return plan, nil
}

// firewallSteps calcule les étapes du pare-feu. Une déclaration invalide, ou qui
// couperait un accès, n'est jamais tentée. Pour les commandes du pare-feu
// (strict), c'est une erreur de validation, code 3 ; dans le plan global, une
// étape non bloquante en échec : le reste du parc s'applique, et l'apply rend
// le code 6 en nommant le motif.
func (a *App) firewallSteps(ctx context.Context, fw *firewall.Provider, projects []*model.Project,
	strict bool) ([]*engine.Step, error) {
	if !fw.Managed() {
		return nil, nil
	}
	obs, err := fw.Observe(ctx)
	if err != nil {
		if strict {
			return nil, planErr(err)
		}
		return []*engine.Step{firewall.RefusedStep(err)}, nil
	}
	steps, err := fw.Plan(ctx, projects, obs)
	if err != nil {
		var ve *firewall.ValidationError
		switch {
		case !errors.As(err, &ve):
			return nil, planErr(err)
		case strict:
			return nil, wrap(CodeValidation, err)
		}
		refused := firewall.RefusedStep(err)
		// Les prisons gardent alors les adresses de confiance de leur fichier
		// (D109) : le plan le dit, car le pas de fail2ban, inchangé, n'y figure pas.
		if _, terr := fw.IgnoreIP(); terr != nil {
			refused.Summary += " — les prisons fail2ban gardent leurs adresses de confiance : une adresse " +
				"retirée d'ufw.yaml y reste de confiance tant qu'il ne se relit pas"
		}
		return []*engine.Step{refused}, nil
	}
	return steps, nil
}

// announceRollback dit, après une activation d'ufw, qu'il faut la confirmer.
// C'est la seule chose de l'apply que l'exploitant ne peut pas manquer : faute
// de confirmation, le pare-feu retombe de lui-même.
func (a *App) announceRollback(fw *firewall.Provider) {
	if fw == nil || fw.Armed == 0 {
		return
	}
	a.warnf("\n⚠ ufw est actif. Depuis une NOUVELLE session SSH, confirmer dans les %s :\n"+
		"    ogs firewall confirm\n"+
		"  Sans confirmation, ufw se désactivera de lui-même : une session coupée par le pare-feu\n"+
		"  ne pourrait pas le faire.\n", fw.Armed)
}

// parcNonBloquante rend non critiques les étapes de fichiers de portée parc que
// l'exploitant retouche — la rotation des journaux et les unités de
// renouvellement —, et c'est le plan qui en décide, non le provider qui les
// fabrique.
//
// Le refus d'un fichier modifié à la main (§6.4) est légitime, et « --force »
// reste sa porte de sortie ; ce qui ne l'est pas, c'est ce qu'un refus critique
// emporte ici. La saga compense en ordre inverse tout ce qu'elle a appliqué avant
// l'échec : un « compress » ajouté à /etc/logrotate.d/ogs déroulerait les
// répertoires d'exécution — donc tous les verrous de tâches au redémarrage
// suivant, /run étant un tmpfs —, le timer de renouvellement des certificats et
// la supervision. Aucun ordre ne protège de cela : ce qui précède est déroulé, ce
// qui suit n'est pas tenté. Un fichier de projet en écart n'engage que son
// projet ; un fichier de parc en écart ne doit pas priver le parc de tout le
// reste — d'où l'avertissement et le code 6, « succès partiel ou écart détecté »,
// qui nomme le fichier et son remède sans rien démonter.
func parcNonBloquante(steps ...*engine.Step) {
	for _, s := range steps {
		if s != nil {
			s.Critical = false
		}
	}
}

// planErr distingue l'interruption de la panne. Les observations de supervision
// atteignent le réseau et les moteurs, et portent désormais le contexte de la
// commande : un Ctrl-C les fait échouer, ce qui n'est pas une erreur du système
// mais l'annulation demandée par l'exploitant — code 9, pas code 1.
func planErr(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return wrap(CodeCancelled, err)
	}
	return wrap(CodeError, err)
}

// purgeArchivesStep supprime les archives dont l'âge dépasse la rétention.
func (a *App) purgeArchivesStep() (*engine.Step, error) {
	retention := time.Duration(a.Cfg.Archives.RetentionDays) * 24 * time.Hour
	expired, err := archive.Expired(a.Paths.ArchivesDir(), retention, nowFunc())
	if err != nil {
		return nil, wrap(CodeError, err)
	}
	if len(expired) == 0 {
		return nil, nil
	}
	s := engine.NewStep("archive/purge", engine.Delete,
		fmt.Sprintf("purge de %d archive(s) de plus de %d jours",
			len(expired), a.Cfg.Archives.RetentionDays))
	// Non critique : une archive qu'on n'arrive pas à supprimer ne doit pas
	// interrompre un déploiement.
	s.Critical = false
	s.Diff = func() (string, error) { return strings.Join(expired, "\n"), nil }
	s.Apply = func(context.Context) error {
		for _, f := range expired {
			if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	}
	return s, nil
}

// reloadFunc route les intentions de rechargement vers le bon provider. Elles
// sont dédupliquées par le moteur : un seul rechargement par cible, en fin
// d'application.
func (a *App) reloadFunc(provs *providers) func(context.Context, string) error {
	return func(ctx context.Context, target string) error {
		switch {
		case strings.HasPrefix(target, "php:"):
			version := strings.TrimPrefix(target, "php:")
			a.printf("  ↻ rechargement de %s\n", config.PHPUnit(version))
			return provs.PHP.Reload(ctx, version)
		case strings.HasPrefix(target, "web:"):
			a.printf("  ↻ rechargement de %s\n", provs.Engine.Unit())
			return provs.Web.Reload(ctx)
		case target == "systemd:daemon-reload":
			a.printf("  ↻ systemctl daemon-reload\n")
			return a.systemctl(ctx, "daemon-reload")
		case target == "systemd:tmpfiles":
			// Le seul fichier d'ogs, jamais toute la configuration : sans argument,
			// systemd-tmpfiles traite chaque fichier de la machine, crée ce qu'ils
			// déclarent — ce n'est pas à ogs de le décider — et rend 65 sur la
			// moindre ligne en double d'un fichier de la distribution. Cet échec,
			// longtemps compté sans être affiché, accompagnait chaque apply.
			a.printf("  ↻ systemd-tmpfiles --create %s\n", a.Paths.TmpfilesFile())
			return a.systemctlBin(ctx, "systemd-tmpfiles", "--create", a.Paths.TmpfilesFile())
		case strings.HasPrefix(target, "systemd:restart-target:"):
			id := strings.TrimPrefix(target, "systemd:restart-target:")
			// Une cible dont l'unité n'existe plus n'a rien à redémarrer : c'est ce
			// que rencontre l'intention rejouée par le déroulage d'une création
			// (D105), dont la compensation vient de retirer la cible.
			if !a.Exec.Simulated() {
				if _, err := os.Lstat(filepath.Join(a.Paths.SystemdDir(), unit.TargetName(id))); os.IsNotExist(err) {
					return nil
				}
			}
			a.printf("  ↻ redémarrage des services de %s\n", id)
			// La cible et chaque instance, nommées (D114) : une cible jamais
			// démarrée ne propage pas le redémarrage, et l'unité modifiée restait
			// sur son ancienne définition.
			return provs.Unit.ControlID(ctx, id, "restart")
		case target == "monitoring:telegraf":
			a.printf("  ↻ rechargement de telegraf\n")
			return provs.Monitoring.ReloadTelegraf(ctx)
		case target == "monitoring:fail2ban":
			a.printf("  ↻ rechargement de fail2ban\n")
			return provs.Monitoring.ReloadFail2ban(ctx)
		case target == mail.ReloadTarget:
			a.printf("  ↻ rechargement d'opendkim\n")
			return provs.Mail.Reload(ctx)
		case strings.HasPrefix(target, "systemd:enable:"):
			unit := strings.TrimPrefix(target, "systemd:enable:")
			a.printf("  ↻ activation de %s\n", unit)
			return a.systemctl(ctx, "enable", "--now", unit)
		}
		return fmt.Errorf("cible de rechargement inconnue : %s", target)
	}
}

// checkFleetUniqueness éprouve l'unicité des projets donnés contre le parc
// entier, jamais contre eux seuls (D94). Une déclaration passée par -f, ou
// modifiée sur le disque, remplace celle de même identifiant.
//
// Éprouver la seule liste visée comparait « apply blogfr » ou « plan -f » à
// lui-même : un domaine, une base ou un home déjà tenus par un voisin entraient
// sans un mot, et l'apply suivant réécrivait les droits de la base du voisin ou
// posait un second vhost pour le même nom. C'est le raisonnement du plan global,
// qui lit lui aussi le parc entier : un sous-ensemble ne dit rien de ce qu'il
// laisse de côté.
//
// Seuls comptent les conflits qui touchent un projet visé, détenteur ou
// demandeur : une collision entre deux voisins ne doit pas arrêter l'apply d'un
// troisième. Les noms de certificat des projets visés s'éprouvent ici, et non
// au chargement du parc (validate.CertificateNames).
func (a *App) checkFleetUniqueness(projects ...*model.Project) error {
	for _, p := range projects {
		if err := validate.CertificateNames(p); err != nil {
			return exitf(CodeValidation, "projet %s :\n%v", p.ID(), err)
		}
		if err := a.checkPoolDeclarations(p); err != nil {
			return err
		}
		if err := checkWebAccess(p); err != nil {
			return err
		}
	}
	fleet, err := a.loadProjects()
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		fleet = withProject(fleet, p)
		ids = append(ids, p.ID())
	}
	// Les racines se jugent sur le parc entier, projets visés compris (D120) :
	// une racine changée dans config.yaml laisse tout le parc sous l'ancienne,
	// et le projet neuf se poserait seul sous la nouvelle.
	if err := a.checkPlacement(fleet); err != nil {
		return err
	}
	if err := validate.UniquenessFor(fleet, a.Cfg.PrepDomain(), ids...); err != nil {
		return exitf(CodeConflict, "conflit avec le parc existant :\n%v", err)
	}
	return nil
}

// targets résout les projets visés : ceux nommés, ou tous à défaut.
func (a *App) targets(args []string, file string) ([]*model.Project, error) {
	if file != "" {
		p, err := spec.Load(file)
		if err != nil {
			return nil, wrap(CodeUsage, err)
		}
		if err := a.prepare(p); err != nil {
			return nil, err
		}
		return []*model.Project{p}, nil
	}
	if len(args) == 0 {
		return a.loadProjects()
	}
	out := make([]*model.Project, 0, len(args))
	for _, id := range args {
		p, err := a.loadProject(id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// planStep est la vue machine d'une étape.
type planStep struct {
	Project  string `json:"project" yaml:"project"`
	Action   string `json:"action" yaml:"action"`
	Resource string `json:"resource" yaml:"resource"`
	Summary  string `json:"summary" yaml:"summary"`
	Critical bool   `json:"critical" yaml:"critical"`
}

func newPlanCommand(app *App) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "plan [id...]",
		Short: "Calculer les changements nécessaires, sans rien appliquer",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := app.targets(args, file)
			if err != nil {
				return err
			}
			if err := app.checkFleetUniqueness(projects...); err != nil {
				return err
			}
			if err := app.openStore(); err != nil {
				return err
			}
			// Un plan ne réserve aucun numéro (D100).
			app.Store.PreviewAllocations(true)
			provs, err := app.buildProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			var items []planStep
			total := 0
			for _, p := range projects {
				plan, err := app.buildPlan(cmd.Context(), provs, p)
				if err != nil {
					return err
				}
				for _, s := range plan.Mutating() {
					items = append(items, planStep{
						Project: p.ID(), Action: s.Action.String(), Resource: s.Resource,
						Summary: s.Summary, Critical: s.Critical,
					})
				}
				total += len(plan.Mutating())
				if app.Format == "table" {
					app.printf("projet %s\n%s", p.ID(), plan.String())
				}
			}
			global, err := app.buildGlobalPlan(cmd.Context(), provs, projects)
			if err != nil {
				return err
			}
			for _, s := range global.Mutating() {
				items = append(items, planStep{
					Project: "*", Action: s.Action.String(), Resource: s.Resource,
					Summary: s.Summary, Critical: s.Critical,
				})
			}
			if len(global.Mutating()) > 0 {
				total += len(global.Mutating())
				if app.Format == "table" {
					app.printf("parc\n%s", global.String())
				}
			}
			if app.Format != "table" {
				return wrap(CodeError, app.emit("Plan", items, nil, nil))
			}
			app.printf("\n%d changement(s) au total.\n", total)
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "appliquer un fichier de projet précis")
	return cmd
}

func newDiffCommand(app *App) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "diff [id...]",
		Short: "Afficher l'écart entre l'état désiré et le système",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := app.targets(args, file)
			if err != nil {
				return err
			}
			if err := app.openStore(); err != nil {
				return err
			}
			app.Store.PreviewAllocations(true)
			provs, err := app.buildProviders()
			if err != nil {
				return err
			}
			defer provs.close()
			drifted := 0
			for _, p := range projects {
				plan, err := app.buildPlan(cmd.Context(), provs, p)
				if err != nil {
					return err
				}
				// La tenue de l'état d'ogs n'est pas un écart du système (D100).
				mutating := plan.Drift()
				if len(mutating) == 0 {
					app.printf("= %s : conforme\n", p.ID())
					continue
				}
				drifted++
				app.printf("~ %s : %d écart(s)\n", p.ID(), len(mutating))
				for _, s := range mutating {
					app.printf("    %s\n", s.String())
					if s.Diff == nil {
						continue
					}
					d, err := s.Diff()
					if err != nil {
						app.printf("      (diff indisponible : %v)\n", err)
						continue
					}
					if d != "" {
						app.printf("%s\n", indent(d, "      "))
					}
				}
			}
			if drifted > 0 {
				// Code distinct : « diff » est utilisable en supervision, où l'écart
				// doit se lire sans analyser la sortie.
				return exitf(CodePartial, "%d projet(s) en écart", drifted)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "comparer un fichier de projet précis")
	return cmd
}

func newApplyCommand(app *App) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "apply [id...]",
		Short: "Appliquer l'état désiré au système",
		Long: "Calcule le plan de chaque projet visé et celui du parc — fichiers communs à tous\n" +
			"les projets : rotation, répertoires d'exécution, supervision, pare-feu, purge des\n" +
			"archives —, les affiche, demande une seule confirmation pour le tout, puis applique\n" +
			"chaque projet en saga : une étape critique en échec déclenche le déroulage des\n" +
			"étapes déjà appliquées de ce projet, qui seul échoue. Une étape non critique en\n" +
			"échec produit un avertissement et l'exécution continue. Le plan du parc vient après\n" +
			"les projets, même si l'un a échoué. Sous --dry-run, les plans sont affichés seulement.\n" +
			"Avec -f, le fichier doit déclarer un projet de l'inventaire (/etc/ogs/projects).",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := app.targets(args, file)
			if err != nil {
				return err
			}
			// Une déclaration hors de l'inventaire n'est pas un projet du parc :
			// appliquée, elle était en service, mais le plan global de l'apply
			// suivant d'un voisin, qui lit /etc/ogs/projects, l'effaçait des
			// fichiers de parc — rotation, répertoires d'exécution, supervision
			// (R1-42, D112).
			if file != "" {
				for _, p := range projects {
					if _, err := os.Stat(app.Paths.ProjectFile(p.ID())); os.IsNotExist(err) {
						return exitf(CodeValidation, "%s déclare le projet %s, absent de l'inventaire (%s) : "+
							"un projet appliqué hors de l'inventaire serait effacé des fichiers de parc au "+
							"prochain apply d'un voisin.\n  remède : « ogs project create » pour le déclarer, ou "+
							"déposer la déclaration dans %s, puis « ogs apply %s »",
							file, p.ID(), app.Paths.ProjectsDir(), app.Paths.ProjectFile(p.ID()), p.ID())
					}
				}
			}
			app.warnUnfinishedRestores(projects)
			// Le chemin unique (D112) : unicité contre le parc sous le verrou,
			// projets et parc affichés, une seule confirmation, hooks, puis chaque
			// projet et le parc.
			return app.applyFleet(cmd.Context(), projects, applyRun{})
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "appliquer un fichier de projet précis")
	return cmd
}

// systemctl pilote systemd. En mode simulé, aucune commande n'est exécutée.
func (a *App) systemctl(ctx context.Context, args ...string) error {
	return a.systemctlBin(ctx, "systemctl", args...)
}

// systemctlBin exécute un binaire systemd.
func (a *App) systemctlBin(ctx context.Context, bin string, args ...string) error {
	if a.Exec.Simulated() {
		return nil
	}
	_, err := a.Exec.Run(ctx, exec.Command{Name: bin, Args: args})
	return err
}

// recordState met à jour l'état observé après une application réussie.
func (a *App) recordState(p *model.Project) error {
	hash, err := specHash(p)
	if err != nil {
		return wrap(CodeError, err)
	}
	row := store.ProjectRow{
		ID: p.ID(), Host: p.Metadata.Host, Env: p.Metadata.Env,
		EffectiveHost: effectiveHostOf(p), Home: p.Spec.Layout.Root,
		AppKind: string(p.Spec.App.Kind), AppVersion: p.Spec.App.Version,
		SpecHash: hash,
	}
	return wrap(CodeError, a.Store.UpsertProject(row))
}

// specHash est l'empreinte de la déclaration d'un projet, telle qu'enregistrée
// à chaque apply.
func specHash(p *model.Project) (string, error) {
	data, err := spec.Encode(p)
	if err != nil {
		return "", err
	}
	return fs.Fingerprint(data), nil
}

// applied dit si la déclaration d'un projet est celle de son dernier apply : ce
// que servent ses vhosts est alors ce qu'elle déclare. Faux au moindre doute —
// base fermée, projet jamais appliqué.
func (a *App) applied(p *model.Project) bool {
	if a.Store == nil {
		return false
	}
	row, err := a.Store.Project(p.ID())
	if err != nil {
		return false
	}
	hash, err := specHash(p)
	return err == nil && row.SpecHash == hash
}

// withProject fond le projet courant dans la liste du parc : lors d'une
// création, sa déclaration peut n'être pas encore sur le disque, et son
// répertoire de journaux serait alors le seul absent du complément d'unité.
// S'il y figure déjà, c'est la version en mémoire qui l'emporte — celle d'un
// « --file » fait autorité sur la copie enregistrée, puisque c'est elle qu'on
// applique.
func withProject(all []*model.Project, p *model.Project) []*model.Project {
	for i, other := range all {
		if other.ID() == p.ID() {
			all[i] = p
			return all
		}
	}
	return append(all, p)
}

func effectiveHostOf(p *model.Project) string {
	if d := p.PrimaryDomain(); d != "" {
		return d
	}
	return p.Metadata.Host
}

func indent(s, prefix string) string {
	out := ""
	for _, line := range splitLines(s) {
		out += prefix + line + "\n"
	}
	return trimTrailingNewline(out)
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func trimTrailingNewline(s string) string {
	for len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	return s
}

var _ = tabwriter.NewWriter

// reportRollback dit ce que la saga a défait après un échec critique, et
// surtout ce qu'elle n'a pas pu défaire : jamais tu, même sous --quiet.
func (a *App) reportRollback(report *engine.Report) {
	if report == nil {
		return
	}
	// « État initial » seulement quand rien ne le dément : une compensation en
	// échec, ou l'étape en échec elle-même, que rien ne compense, laissant un
	// changement qu'elle n'a pas pu défaire (D104).
	residue := report.Residue()
	switch {
	case report.RollbackErr != nil:
		a.warnf("↶ rollback interrompu : le système n'est pas revenu à son état initial.\n")
	case residue != nil:
		a.warnf("↶ rollback effectué, sauf l'étape en échec, qui n'a pas pu être défaite : %s — voir son "+
			"erreur ci-dessus.\n", residue.Step.Summary)
	case report.RolledBack:
		a.warnf("↶ rollback effectué : le système est revenu à son état initial.\n")
	}
	// Les démons avaient chargé ce que le déroulage a défait (D105).
	if len(report.Replayed) > 0 {
		a.warnf("  rechargements rejoués : %s\n", strings.Join(report.Replayed, ", "))
	}
	if report.RollbackErr != nil {
		a.warnf("✗ le rollback a échoué : %v\n", report.RollbackErr)
		a.warnf("  état à vérifier manuellement (audit %s)\n", a.Audit.RunID())
	}
}

// reloadWarning affiche un rechargement en échec. Il n'appartient à aucune
// étape : sans lui, seul le compte des avertissements en gardait la trace.
func (a *App) reloadWarning(target string, err error) {
	a.warnf("  ⚠ rechargement %s : %v\n", target, err)
}

// issueDÉtape rend la ligne qui annonce l'issue d'une étape. Une étape aboutie en
// partie (engine.PartialError) — un certificat émis pour les seuls noms que
// l'autorité a acceptés — a produit son effet : le certificat est écrit, le
// serveur le rechargera. La marquer « ✗ » ferait croire à un échec, et
// l'avertissement qui la suit dit déjà ce qui manque.
// annonceÉtape dit si l'issue d'une étape s'annonce à sa fin. Une étape non
// critique en échec ne s'y annonce pas : le moteur la remonte aussi par
// OnWarning, dont la ligne « ⚠ » est la seule — sans quoi chaque avertissement
// s'affichait deux fois, « ✗ » puis « ⚠ » du même message. La ligne de commande
// et la TUI suivent la même règle.
func annonceÉtape(s *engine.Step, stepErr error) bool {
	partial := (*engine.PartialError)(nil)
	return stepErr == nil || errors.As(stepErr, &partial) || s.Critical
}

func issueDÉtape(s *engine.Step, stepErr error) string {
	partial := (*engine.PartialError)(nil)
	switch {
	case stepErr == nil:
		return "✓ " + s.Summary
	case errors.As(stepErr, &partial):
		return "✓ " + s.Summary + " (en partie)"
	}
	return fmt.Sprintf("✗ %s : %v", s.Summary, stepErr)
}
