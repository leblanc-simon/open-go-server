package cli

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"leblanc.io/open-go-server/internal/archive"
	"leblanc.io/open-go-server/internal/exec"
	appprov "leblanc.io/open-go-server/internal/provider/app"
	"leblanc.io/open-go-server/internal/provider/db"
	"leblanc.io/open-go-server/internal/provider/hardening"
	"leblanc.io/open-go-server/internal/provider/monitoring"
	"leblanc.io/open-go-server/internal/provider/system"
	"leblanc.io/open-go-server/internal/validate"
)

// check est le résultat d'un contrôle de diagnostic.
type check struct {
	Name   string `json:"name" yaml:"name"`
	Status string `json:"status" yaml:"status"` // ok | avertissement | échec
	Detail string `json:"detail" yaml:"detail"`
	Fix    string `json:"fix,omitempty" yaml:"fix,omitempty"`
}

func newDoctorCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnostiquer l'installation et l'environnement",
		Long: "Vérifie les prérequis, les permissions et la cohérence de l'installation.\n" +
			"Chaque anomalie est accompagnée de la commande qui la corrige. Aucun paquet\n" +
			"n'est installé : ogs diagnostique, il n'administre pas la distribution.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := app.runChecks(cmd.Context())
			failed, warned := 0, 0
			for _, c := range checks {
				switch c.Status {
				case "échec":
					failed++
				case "avertissement":
					warned++
				}
			}
			err := app.emit("DoctorReport", checks, nil, func(tw *tabwriter.Writer) {
				fmt.Fprintln(tw, "ÉTAT\tCONTRÔLE\tDÉTAIL")
				for _, c := range checks {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", symbolFor(c.Status), c.Name, c.Detail)
				}
			})
			if err != nil {
				return wrap(CodeError, err)
			}
			for _, c := range checks {
				if c.Fix != "" && c.Status != "ok" {
					app.printf("  → %s : %s\n", c.Name, c.Fix)
				}
			}
			if failed > 0 {
				return exitf(CodeDependency, "%d contrôle(s) en échec, %d avertissement(s)", failed, warned)
			}
			if warned > 0 {
				return exitf(CodePartial, "%d avertissement(s)", warned)
			}
			return nil
		},
	}
}

// listeners énumère les points d'écoute TCP de la machine.
func (a *App) listeners() ([]string, error) {
	res, err := a.Exec.Run(context.Background(), exec.Command{
		Name: "ss", Args: []string{"-tlnH"},
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		fields := strings.Fields(line)
		// ss -tlnH : State Recv-Q Send-Q Local:Port Peer:Port
		if len(fields) >= 4 {
			out = append(out, fields[3])
		}
	}
	return out, nil
}

func symbolFor(status string) string {
	switch status {
	case "ok":
		return "✓"
	case "avertissement":
		return "⚠"
	}
	return "✗"
}

// checkDefaultAuthorizedKeys contrôle le trousseau de clés commun du parc (D61).
// Un statut vide signifie « pas de trousseau » : il n'y a alors rien à dire.
//
// Sous StrictModes — le défaut d'OpenSSH —, sshd ignore **en silence** un
// authorized_keys qui n'appartient pas à root ou à l'utilisateur, ou qui est
// inscriptible au-delà de son propriétaire : la connexion est refusée sans que
// rien, du côté d'ogs, ne paraisse anormal. Le lien étant résolu avant ce
// contrôle, ce sont les droits du trousseau qui décident, non ceux du lien. Et
// sshd le lit sous l'identité de l'utilisateur du projet : illisible par lui, il
// ne sert à rien non plus.
func (a *App) checkDefaultAuthorizedKeys() (status, detail, fix string) {
	keys := a.Paths.DefaultAuthorizedKeys()
	info, err := os.Stat(keys)
	if err != nil {
		return "", "", ""
	}
	if !info.Mode().IsRegular() {
		return "échec", fmt.Sprintf("%s n'est pas un fichier régulier", keys), ""
	}
	var problems []string
	perm := info.Mode().Perm()
	if perm&0o004 == 0 {
		problems = append(problems,
			fmt.Sprintf("mode %04o : illisible par les utilisateurs des projets", perm))
	}
	// Tout ce qui se juge par rapport à root ne vaut qu'en production : sous
	// préfixe, l'arborescence appartient à qui fait tourner l'essai, et les modes
	// y sont ceux de son umask.
	if a.Prefix == "" {
		if uid, _, ok := system.OwnerOf(info); ok && uid != 0 {
			problems = append(problems, fmt.Sprintf("propriétaire uid %d, attendu root", uid))
		}
		if perm&0o022 != 0 {
			problems = append(problems,
				fmt.Sprintf("mode %04o : inscriptible au-delà de root, sshd l'ignorera", perm))
		}
		// Le répertoire traversé compte autant que le fichier : sshd remonte la chaîne.
		parent := filepath.Dir(keys)
		if pinfo, perr := os.Stat(parent); perr == nil && pinfo.Mode().Perm()&0o022 != 0 {
			problems = append(problems, fmt.Sprintf("%s (mode %04o) est inscriptible au-delà de root",
				parent, pinfo.Mode().Perm()))
		}
	}
	if len(problems) > 0 {
		return "échec", strings.Join(problems, " ; "),
			"chown root:root " + keys + " && chmod 0644 " + keys
	}
	return "ok", fmt.Sprintf("%s (mode %04o), lié dans le .ssh de chaque projet", keys, perm), ""
}

func (a *App) runChecks(ctx context.Context) []check {
	var out []check
	add := func(name, status, detail, fix string) {
		out = append(out, check{Name: name, Status: status, Detail: detail, Fix: fix})
	}

	if a.Prefix == "" && os.Getuid() != 0 {
		add("privilèges", "échec", "ogs n'est pas exécuté en tant que root", "utiliser sudo")
	} else {
		add("privilèges", "ok", fmt.Sprintf("uid %d", os.Getuid()), "")
	}

	// Arborescence de l'outil.
	for _, d := range a.Paths.Dirs() {
		info, err := os.Stat(d.Path)
		switch {
		case os.IsNotExist(err):
			add("répertoire "+d.Path, "échec", "absent", "ogs init")
		case err != nil:
			add("répertoire "+d.Path, "échec", err.Error(), "")
		case info.Mode().Perm() != d.Mode:
			add("répertoire "+d.Path, "avertissement",
				fmt.Sprintf("mode %04o, attendu %04o", info.Mode().Perm(), d.Mode),
				fmt.Sprintf("chmod %04o %s", d.Mode, d.Path))
		default:
			add("répertoire "+d.Path, "ok", fmt.Sprintf("mode %04o", info.Mode().Perm()), "")
		}
	}

	// Clé maître : sa divulgation expose tous les secrets.
	if info, err := os.Stat(a.Paths.MasterKey()); err != nil {
		add("clé maître", "échec", "absente", "ogs init")
	} else if perm := info.Mode().Perm(); perm&0o077 != 0 {
		add("clé maître", "échec", fmt.Sprintf("mode %04o : lisible au-delà de son propriétaire", perm),
			"chmod 0400 "+a.Paths.MasterKey())
	} else {
		add("clé maître", "ok", fmt.Sprintf("mode %04o", perm), "")
	}

	// Racine des homes : elle doit rester traversable par tous, sinon le serveur
	// web ne peut atteindre aucun docroot. Traversable, non énumérable : la cible
	// est 0711 (D60) — le correctif proposé ne rouvre donc pas la lecture. Et
	// « harden » ne desserre jamais ce qu'un administrateur a fermé : un 0700 ici
	// resterait en place, sans qu'aucun contrôle de configuration ne le dise, la
	// configuration étant par ailleurs valide.
	homeRoot := a.homeRoot()
	if info, err := os.Stat(homeRoot); err != nil {
		add("racine des homes", "échec", err.Error(), "")
	} else if info.Mode().Perm()&0o001 == 0 {
		add("racine des homes", "échec",
			fmt.Sprintf("%s (mode %04o) n'est pas traversable : aucun docroot ne sera servi", homeRoot, info.Mode().Perm()),
			"chmod 0711 "+homeRoot)
	} else {
		add("racine des homes", "ok",
			fmt.Sprintf("%s (mode %04o)", homeRoot, info.Mode().Perm()), "")
	}

	// Trousseau de clés commun : ogs le lie dans le .ssh de chaque projet, mais
	// c'est sshd qui décide s'il le lit. Le contrôle ne s'affiche que si le fichier
	// existe — la fonction est facultative, et un parc qui ne s'en sert pas n'a pas
	// à lire une ligne de plus.
	if status, detail, fix := a.checkDefaultAuthorizedKeys(); status != "" {
		add("trousseau de clés commun", status, detail, fix)
	}

	// Shell des comptes de projet : useradd accepte un chemin qui n'existe pas, et
	// le compte devient alors inutilisable sans le moindre message. Un shell fermé
	// à dessein — nologin — n'est pas un défaut : c'est son absence du disque qui
	// en est un.
	shell := a.Cfg.Shell()
	if _, err := os.Stat(shell); err != nil {
		add("shell des projets", "échec",
			fmt.Sprintf("%s est déclaré (user.shell) mais absent du système", shell),
			"corriger user.shell dans "+a.Paths.ConfigFile())
	} else {
		add("shell des projets", "ok", shell, "")
	}

	// Binaires attendus par le provider système.
	for _, bin := range []string{"useradd", "groupadd", "gpasswd", "usermod"} {
		if path, err := osexec.LookPath(bin); err != nil {
			add("binaire "+bin, "échec", "introuvable dans le PATH", "apt install passwd")
		} else {
			add("binaire "+bin, "ok", path, "")
		}
	}

	// Clusters PostgreSQL : l'accès d'administration passe par l'authentification
	// « peer », qui exige un rôle au nom de l'utilisateur système. Une installation
	// neuve n'en comporte pas, et le défaut ne se découvrirait qu'à la création
	// d'une base.
	if a.Prefix == "" && !a.Exec.Simulated() {
		clusters := db.DetectClusters("")
		role := db.PostgresAdminRole(os.Getenv("OGS_PG_ADMIN"))
		for _, cl := range clusters {
			client, err := db.NewPostgresClient(cl, role)
			if err != nil {
				add("cluster postgres "+cl.ID(), "échec", err.Error(), "")
				continue
			}
			if _, err := client.Endpoint(ctx); err != nil {
				add("cluster postgres "+cl.ID(), "échec", err.Error(),
					fmt.Sprintf("su - postgres -c 'createuser --superuser %s'", role))
			} else {
				add("cluster postgres "+cl.ID(), "ok",
					fmt.Sprintf("joignable en tant que %s", role), "")
			}
			client.Close()
		}
	}

	// Droits des répertoires sensibles — configuration et racine des homes : sur un
	// serveur mutualisé, les utilisateurs de projet ont un compte local et lisent
	// tout ce qui n'est pas fermé.
	if etats := a.hardeningProvider().Observe(); len(etats) > 0 {
		if n := hardening.Restrict(etats); n > 0 {
			add("droits des répertoires système", "avertissement",
				fmt.Sprintf("%d répertoire(s) énumérables au-delà de root", n),
				"ogs harden --apply")
		} else {
			add("droits des répertoires système", "ok",
				"configuration et racine des homes fermées", "")
		}
	}

	// Archives expirées : la purge a lieu à l'apply, mais un parc dont on
	// n'applique rien pendant des mois garderait des dumps indéfiniment.
	if retention := a.Cfg.Archives.RetentionDays; retention > 0 {
		expired, err := archive.Expired(a.Paths.ArchivesDir(),
			time.Duration(retention)*24*time.Hour, nowFunc())
		if err == nil && len(expired) > 0 {
			add("archives", "avertissement",
				fmt.Sprintf("%d archive(s) au-delà de %d jours attendent la purge",
					len(expired), retention),
				"ogs apply")
		} else if err == nil {
			add("archives", "ok", fmt.Sprintf("rétention %d jours", retention), "")
		}
	} else {
		add("archives", "ok", "conservées indéfiniment (rétention désactivée)", "")
	}

	// Modules Apache exigés par les vhosts produits. Leur absence ne se traduit
	// pas par un refus clair : Apache accepte la configuration puis rend une 500
	// avec un message obscur, ou ignore la directive en silence.
	if eng, err := a.webEngine(); err == nil && eng.Name() == "apache" {
		mods := filepath.Join(a.Paths.Prefix(), "/etc/apache2/mods-enabled")
		requis := []struct{ nom, pourquoi string }{
			{"ssl", "aucun site ne peut être servi en HTTPS"},
			{"proxy", "aucun relais vers PHP-FPM ni vers une application"},
			{"proxy_fcgi", "PHP ne peut pas être exécuté : le vhost rendra le source"},
			{"proxy_http", "les applications Go, Node, Ruby et Python ne peuvent pas être relayées"},
			{"rewrite", "les redirections vers HTTPS et les front controllers ne fonctionnent pas"},
			{"headers", "les en-têtes de sécurité (HSTS, X-Robots-Tag) sont ignorés"},
			{"status", "le vhost de supervision ne rend pas l'état du serveur : " +
				"SetHandler server-status n'a aucun effet sans ce module, et Apache " +
				"ne s'en plaint pas — il sert une page vide"},
		}
		var manquants []string
		for _, m := range requis {
			if _, err := os.Stat(filepath.Join(mods, m.nom+".load")); err != nil {
				add("mod_"+m.nom, "avertissement",
					"module Apache absent : "+m.pourquoi,
					"a2enmod "+m.nom+" && systemctl reload apache2")
				manquants = append(manquants, m.nom)
			}
		}
		if len(manquants) == 0 {
			add("modules apache", "ok", "tous les modules requis sont activés", "")
		}
	}

	// Journaux des vhosts : le serveur les ouvre tous à son (re)démarrage et
	// s'arrête si l'un résiste, emportant tous les sites. « apachectl -t » n'en dit
	// rien — il ne teste que la syntaxe — et l'échec ne se découvre qu'au
	// rechargement suivant, trop tard. Un /home remonté en lecture seule après une
	// erreur disque suffit à le provoquer.
	if provs, err := a.buildProviders(); err == nil {
		defer provs.close()
		if err := provs.Web.CheckLogs(context.Background()); err != nil {
			add("journaux des vhosts", "échec",
				"un rechargement du serveur web l'arrêterait : "+err.Error(),
				"rétablir l'accès en écriture (montage en lecture seule ? droits du répertoire ?)")
		} else {
			add("journaux des vhosts", "ok", "les journaux des vhosts gérés sont inscriptibles", "")
		}
		// Surcharges de vhost (D63) : un fichier lu en root par le maître, un
		// fichier déposé hors de tout contexte donc jamais chargé, un répertoire
		// qu'aucun vhost ne référence plus. Rien de tout cela ne se voit dans une
		// vérification de configuration — le serveur démarre et sert.
		problems := provs.Web.AuditIncludes()
		for _, p := range problems {
			status := "avertissement"
			if p.Severe {
				status = "échec"
			}
			add("surcharges de vhost", status, p.Detail, p.Fix)
		}
		if len(problems) == 0 {
			add("surcharges de vhost", "ok",
				"répertoires de surcharge conformes ("+provs.Web.Engine.IncludeRoot()+")", "")
		}
	}

	// Versions PHP en fin de support : signalées, jamais bloquantes.
	projects, err := a.loadProjects()
	if err != nil {
		add("projets", "avertissement", err.Error(), "")
		return out
	}
	add("projets", "ok", fmt.Sprintf("%d déclaré(s)", len(projects)), "")

	// Délégation sudo : un fichier au préfixe ogs- non généré par l'outil, ou
	// élargi à la main, mérite d'être signalé.
	if provs, err := a.buildProviders(); err == nil {
		if problems, err := provs.Sudoers.Audit(); err == nil {
			for _, p := range problems {
				add("sudoers", "échec", p, "vérifier ce fichier à la main")
			}
			if len(problems) == 0 {
				add("sudoers", "ok", "fichiers de délégation conformes", "")
			}
		}
		provs.close()
	}
	// « ogs my » identifie l'appelant par SUDO_UID, que sudo ne positionne de
	// façon fiable que si env_reset est active.
	if a.Prefix == "" {
		if data, err := os.ReadFile("/etc/sudoers"); err == nil {
			if strings.Contains(string(data), "!env_reset") {
				add("sudo env_reset", "échec",
					"env_reset est désactivée : l'appelant contrôle son environnement et « ogs my » "+
						"ne peut plus l'identifier de façon sûre",
					"réactiver Defaults env_reset dans /etc/sudoers")
			} else {
				add("sudo env_reset", "ok", "active", "")
			}
		}
	}

	// Applications : une écoute au-delà de la boucle locale contourne le vhost,
	// donc le TLS, les en-têtes de sécurité et l'authentification.
	if a.Prefix == "" && !a.Exec.Simulated() {
		if listeners, err := a.listeners(); err == nil {
			if exposed := appprov.ExposedPorts(listeners); len(exposed) > 0 {
				add("écoute applicative", "échec",
					"port(s) applicatif(s) exposé(s) hors de la boucle locale : "+strings.Join(exposed, ", "),
					"faire écouter l'application sur 127.0.0.1, ou employer un socket Unix")
			} else {
				add("écoute applicative", "ok", "aucun port exposé", "")
			}
		}
	}

	// Certificats : le seuil d'alerte d'un partagé est plus précoce, son
	// expiration cassant toutes les préproductions à la fois.
	if provs, err := a.buildProviders(); err == nil {
		defer provs.close()
		if warnings, err := provs.TLS.Warnings(); err == nil {
			for _, w := range warnings {
				add("tls", "avertissement", w, "ogs cert renew --all")
			}
		}
		if list, err := provs.TLS.Store.List(); err == nil && len(list) > 0 {
			add("certificats", "ok", fmt.Sprintf("%d présent(s)", len(list)), "")
		}
	}
	for _, p := range projects {
		for _, pool := range p.Spec.PHP.Pools {
			if date, eol := validate.EOLDate(pool.Version); eol {
				add(fmt.Sprintf("php %s (%s)", pool.Version, p.ID()), "avertissement",
					"support terminé depuis le "+date, "envisager « ogs php switch »")
			}
		}
	}
	a.monitoringChecks(add)
	return out
}

// monitoringChecks porte ce que ni fail2ban ni Telegraf ne disent d'eux-mêmes.
//
// Le premier de ces contrôles est le plus important du lot : **une prison dont
// le journal n'existe plus empêche fail2ban de démarrer**, et il s'arrête alors
// entièrement — les prisons valides comprises. Le fichier reste syntaxiquement
// correct, le démon reste « enabled », et rien ne le dit jusqu'au prochain
// redémarrage de la machine. C'est le cas d'un projet dont les journaux ont été
// effacés à la main entre deux apply.
func (a *App) monitoringChecks(add func(name, status, detail, fix string)) {
	mon := a.Cfg.Monitoring

	// fail2ban : journaux référencés. Le contrôle n'a de sens que sur un système
	// réel — en mode simulé, le fichier retient tous les chemins par construction,
	// et rien n'est jamais donné à lire à fail2ban.
	jailFile := a.Paths.Fail2banJailFile()
	data, err := os.ReadFile(jailFile)
	switch {
	case a.Exec.Simulated():
	case err == nil:
		// Un même journal est surveillé par plusieurs prisons : dédupliquer, sinon
		// le diagnostic répète le même chemin autant de fois qu'il est référencé.
		var manquants []string
		vus := map[string]bool{}
		for _, path := range jailLogPaths(string(data)) {
			if vus[path] {
				continue
			}
			vus[path] = true
			if _, err := os.Stat(path); err != nil {
				manquants = append(manquants, path)
			}
		}
		if len(manquants) > 0 {
			add("prisons fail2ban", "échec",
				fmt.Sprintf("%d journal(aux) référencé(s) et absent(s) : %s — fail2ban refusera de "+
					"démarrer, et toutes les prisons tomberont avec celle-ci",
					len(manquants), strings.Join(manquants, ", ")),
				"ogs apply (le fichier est recalculé depuis les journaux réellement présents)")
		} else {
			add("prisons fail2ban", "ok", "les journaux référencés existent tous", "")
		}
	case !os.IsNotExist(err):
		add("prisons fail2ban", "avertissement", err.Error(), "")
	}

	// Le contrôle de configuration de fail2ban, lorsqu'il est installé. Il couvre
	// ce qu'aucune inspection de fichier ne voit : un filtre absent, une expression
	// régulière refusée, un backend indisponible — la prison sshd en backend
	// systemd sans les liaisons python3-systemd, par exemple. Chacun de ces cas
	// empêche le démon de démarrer, toutes prisons confondues.
	if a.Prefix == "" && !a.Exec.Simulated() {
		if _, err := osexec.LookPath("fail2ban-client"); err == nil {
			res, err := a.Exec.Run(context.Background(), exec.Command{
				Name: "fail2ban-client", Args: []string{"-t"}})
			if err != nil {
				detail := strings.TrimSpace(res.Stderr)
				if detail == "" {
					detail = strings.TrimSpace(res.Stdout)
				}
				add("configuration fail2ban", "échec",
					"fail2ban refuse sa configuration : "+firstLine(detail)+
						" — le démon ne démarrera pas, et toutes les prisons tomberont ensemble",
					"fail2ban-client -t pour le détail")
			} else {
				add("configuration fail2ban", "ok", "acceptée par fail2ban-client -t", "")
			}
		}
	}

	// Telegraf : le fragment porte les mots de passe des comptes de supervision.
	// Il doit être lisible par l'agent, et par lui seul.
	fragment := a.Paths.TelegrafFile()
	if info, err := os.Stat(fragment); err == nil {
		perm := info.Mode().Perm()
		switch {
		case perm&0o007 != 0:
			add("entrées telegraf", "échec",
				fmt.Sprintf("mode %04o : lisible par tous, alors que le fichier porte les mots de "+
					"passe des comptes de supervision", perm),
				"chmod 0640 "+fragment+" && chgrp "+mon.TelegrafUser()+" "+fragment)
		case !readableBy(info, mon.TelegrafUser()):
			// Un fragment que l'agent ne peut pas lire ne produit aucune erreur
			// visible : Telegraf démarre, ne collecte rien, et se tait.
			add("entrées telegraf", "avertissement",
				"le fragment n'est pas lisible par "+mon.TelegrafUser()+
					" : l'agent démarrera sans collecter ces entrées, sans le dire",
				"chgrp "+mon.TelegrafUser()+" "+fragment+" && chmod 0640 "+fragment)
		default:
			add("entrées telegraf", "ok", fmt.Sprintf("mode %04o", perm), "")
		}
	}

	// Comptes de supervision : leur mot de passe vit dans le keystore. Un secret
	// absent alors que le fragment le référence signifie une base d'état
	// restaurée sans sa clé — l'agent s'authentifiera avec un mot de passe qui
	// n'est plus celui du moteur.
	if a.Store != nil {
		names, err := a.Store.SecretNames(monitoring.SecretScope)
		if err == nil && len(names) > 0 {
			add("comptes de supervision", "ok",
				fmt.Sprintf("%d secret(s) de parc scellé(s) : %s", len(names), strings.Join(names, ", ")), "")
		}
	}
}

// jailLogPaths extrait les chemins de journaux d'un fichier de prisons. La
// directive « logpath » accepte plusieurs chemins, un par ligne, les suivants
// étant indentés — d'où la reprise des lignes qui ne portent pas de directive.
func jailLogPaths(content string) []string {
	var out []string
	inLogpath := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "logpath"):
			inLogpath = true
			if _, value, ok := strings.Cut(trimmed, "="); ok {
				if v := strings.TrimSpace(value); v != "" {
					out = append(out, v)
				}
			}
		case inLogpath && trimmed != "" && !strings.Contains(trimmed, "=") &&
			!strings.HasPrefix(trimmed, "[") && !strings.HasPrefix(trimmed, "#"):
			out = append(out, trimmed)
		default:
			inLogpath = false
		}
	}
	return out
}

// readableBy indique si un fichier est lisible par un compte, en ne considérant
// que le groupe propriétaire : c'est le seul canal qu'ogs emploie pour l'agent.
func readableBy(info os.FileInfo, username string) bool {
	if info.Mode().Perm()&0o040 == 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	u, err := user.Lookup(username)
	if err != nil {
		// L'agent n'est pas installé : le contrôle n'a pas d'objet.
		return true
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return true
	}
	return uint32(gid) == stat.Gid
}

// firstLine borne un message d'erreur multiligne à sa première ligne utile : le
// tableau de « doctor » est fait pour être lu d'un coup d'œil.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			return l
		}
	}
	return s
}
