package monitoring

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// Jail est une prison générée : un filtre livré par fail2ban, appliqué aux
// journaux du parc.
type Jail struct {
	// Name est le nom de la prison, préfixé « ogs- ». Le préfixe n'est pas
	// décoratif : jail.d est un espace partagé avec l'administrateur, et une
	// prison nommée « sshd » recouvrirait silencieusement la sienne.
	Name string
	// Filter est le filtre de fail2ban, tel qu'il est livré par la distribution.
	// ogs n'écrit aucun filtre : un filtre est une expression régulière appliquée
	// à chaque ligne de journal, et en maintenir un jeu à côté de celui du projet
	// amont serait s'engager à suivre chaque changement de format de log.
	Filter string
	Port   string
	// LogPaths énumère les journaux surveillés. Une prison sans aucun journal
	// n'est pas écrite : fail2ban refuse de démarrer sur une prison dont il ne
	// trouve aucun fichier, et emporte alors toutes les autres.
	LogPaths []string
	MaxRetry int
	Backend  string
	// BanTime, FindTime et IgnoreIP sont portés par chaque prison, jamais par une
	// section [DEFAULT] : fail2ban fusionne jail.conf, jail.d/*.conf et jail.local
	// dans un même analyseur, de sorte qu'une section commune posée dans jail.d
	// n'est pas locale à son fichier — elle s'imposerait à **toutes** les prisons
	// de la machine, celles de l'administrateur comprises. Le plus grave est
	// « ignoreip », qui est remplacé et non complété : la liste blanche
	// d'administration disparaîtrait, et quelques échecs d'authentification
	// suffiraient à verrouiller l'administrateur hors de son propre serveur.
	BanTime  string
	FindTime string
	IgnoreIP []string
}

// Fail2banData est le contexte du fichier de prisons.
type Fail2banData struct {
	BanTime  string
	FindTime string
	MaxRetry int
	IgnoreIP []string
	Jails    []Jail
}

// Défauts des prisons générées.
const (
	defaultBanTime  = "1h"
	defaultFindTime = "10m"
	defaultMaxRetry = 5
	// recidiveBanTime punit la récidive : celui qui s'est fait bannir plusieurs
	// fois dans la semaine l'est pour une semaine.
	recidiveBanTime  = "1w"
	recidiveFindTime = "1d"
)

// BuildFail2ban compose les prisons à partir du parc.
//
// Deux règles gouvernent cette construction, et toutes deux viennent du même
// comportement de fail2ban : **une prison dont aucun journal n'existe empêche le
// démon de démarrer**, et il s'arrête alors entièrement — les prisons valides
// comprises. D'où l'énumération explicite des chemins, jamais un joker sur
// /home/*/logs, et le filtrage des fichiers absents.
func (p *Provider) BuildFail2ban(projects []*model.Project) Fail2banData {
	mon := p.Cfg.Monitoring
	data := Fail2banData{
		BanTime:  firstNonEmpty(mon.Fail2ban.BanTime, defaultBanTime),
		FindTime: firstNonEmpty(mon.Fail2ban.FindTime, defaultFindTime),
		MaxRetry: mon.Fail2ban.MaxRetry,
		IgnoreIP: withLoopback(mon.Fail2ban.IgnoreIP),
	}
	if data.MaxRetry == 0 {
		data.MaxRetry = defaultMaxRetry
	}

	var errorLogs, accessLogs []string
	for _, proj := range projects {
		logDir := proj.Spec.Layout.LogDir
		if logDir == "" || p.WebEngine == "" || len(proj.Spec.Domains) == 0 {
			continue
		}
		errorLogs = append(errorLogs, filepath.Join(logDir, p.WebEngine+"-error.log"))
		accessLogs = append(accessLogs, filepath.Join(logDir, p.WebEngine+"-access.log"))
	}
	sort.Strings(errorLogs)
	sort.Strings(accessLogs)

	// Les filtres portent le nom du moteur : ceux d'Apache et de nginx ne lisent
	// pas le même format de ligne.
	authFilter := "apache-auth"
	noscriptFilter := "apache-noscript"
	badbotsFilter := "apache-badbots"
	if p.WebEngine == "nginx" {
		authFilter = "nginx-http-auth"
		noscriptFilter = "nginx-botsearch"
		badbotsFilter = "nginx-bad-request"
	}

	add := func(name, filter, port string, logs []string, maxRetry int) {
		if !mon.JailEnabled(name) {
			return
		}
		existing := p.existing(logs)
		if len(existing) == 0 {
			return
		}
		data.Jails = append(data.Jails, Jail{
			Name: "ogs-" + name, Filter: filter, Port: port,
			LogPaths: existing, MaxRetry: maxRetry, Backend: "auto",
			BanTime: data.BanTime, FindTime: data.FindTime, IgnoreIP: data.IgnoreIP,
		})
	}

	add("web-auth", authFilter, "http,https", errorLogs, data.MaxRetry)
	add("web-noscript", noscriptFilter, "http,https", errorLogs, data.MaxRetry)
	add("web-badbots", badbotsFilter, "http,https", accessLogs, 2)

	if mon.JailEnabled("sshd") {
		// sshd est la seule prison qui ne dépende pas du parc, et la plus utile :
		// un serveur administré par ogs porte un compte local par projet, dont le
		// shell est ouvert (D62).
		//
		// Le backend n'est pas un détail. Une Debian récente n'installe plus
		// rsyslog par défaut : /var/log/auth.log peut ne pas exister, et il faut
		// alors lire le journal de systemd. Mais ce backend-là exige les liaisons
		// python3-systemd, qui ne sont qu'une recommandation du paquet : sans
		// elles, la prison échoue au démarrage — et **fail2ban s'arrête tout
		// entier**, emportant les prisons web qui, elles, fonctionnaient. On
		// choisit donc d'après ce qui existe, en préférant le fichier lorsqu'il est
		// là : il ne dépend d'aucune liaison.
		jail := Jail{Name: "ogs-sshd", Filter: "sshd", Port: "ssh", MaxRetry: data.MaxRetry,
			BanTime: data.BanTime, FindTime: data.FindTime, IgnoreIP: data.IgnoreIP}
		if logs := p.existing([]string{p.Paths.AuthLog()}); len(logs) > 0 {
			jail.Backend, jail.LogPaths = "auto", logs
		} else {
			jail.Backend = "systemd"
		}
		data.Jails = append(data.Jails, jail)
	}
	if mon.JailEnabled("recidive") && len(data.Jails) > 0 {
		// La récidive lit le journal de fail2ban lui-même : elle n'a de sens que
		// s'il existe au moins une prison pour l'alimenter — et que si ce journal
		// existe. Il peut manquer durablement : « logtarget » n'est pas toujours un
		// fichier, et un démon jamais démarré n'en a écrit aucun. Écrite sans
		// « logpath », la prison est acceptée par « fail2ban-client -t » et ne
		// surveille rien : la protection serait annoncée et absente, sans même un
		// chemin que « doctor » puisse contrôler. On ne bascule pas non plus sur le
		// backend systemd, qui exige les liaisons python3-systemd (D68).
		if logs := p.existing([]string{p.Paths.Fail2banLog()}); len(logs) > 0 {
			data.Jails = append(data.Jails, Jail{
				Name: "ogs-recidive", Filter: "recidive", Port: "all",
				LogPaths: logs, MaxRetry: 3, Backend: "auto",
				BanTime: recidiveBanTime, FindTime: recidiveFindTime, IgnoreIP: data.IgnoreIP,
			})
		}
	}
	return data
}

// existing ne retient que les journaux réellement présents. En mode simulé, tout
// est retenu : le mode d'essai doit rendre un fichier complet, et rien n'y est
// jamais donné à lire à fail2ban.
func (p *Provider) existing(paths []string) []string {
	if p.Exec != nil && p.Exec.Simulated() {
		return paths
	}
	var out []string
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			out = append(out, path)
		}
	}
	return out
}

// planFail2ban produit l'étape d'écriture du fichier de prisons.
func (p *Provider) planFail2ban(projects []*model.Project) (*engine.Step, error) {
	data := p.BuildFail2ban(projects)
	// Les réglages communs ne sont pas passés au gabarit : ils sont recopiés dans
	// chaque prison, faute de quoi ils déborderaient sur celles que l'administrateur
	// a écrites (voir le commentaire du champ Jail.IgnoreIP).
	body, err := p.Templates.Render("fail2ban/ogs.conf", map[string]any{
		"Jails": data.Jails,
	})
	if err != nil {
		return nil, err
	}
	content := fs.Render(p.header("monitoring/fail2ban", "fail2ban/ogs.conf", len(projects)), body, "#")
	summary := fmt.Sprintf("prisons fail2ban (%d prison(s), %d projet(s))", len(data.Jails), len(projects))
	// 0644 : le fichier ne porte aucun secret, et fail2ban le lit en root. Le
	// rendre illisible n'apporterait rien qu'« ogs harden » ne fasse déjà sur le
	// répertoire.
	return p.fileStep(managedFile{
		resource: "monitoring/fail2ban",
		summary:  summary,
		path:     p.Paths.Fail2banJailFile(),
		content:  content,
		mode:     0o644,
		owner:    fs.KeepOwner,
		reload:   "monitoring:fail2ban",
	})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// withLoopback garantit la présence de la boucle locale dans les adresses
// ignorées : se bannir soi-même couperait la supervision, les hooks et les
// requêtes de disponibilité qu'ogs émet après un démarrage.
func withLoopback(addrs []string) []string {
	out := append([]string{"127.0.0.1/8", "::1"}, addrs...)
	seen := map[string]bool{}
	var uniq []string
	for _, a := range out {
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		uniq = append(uniq, a)
	}
	return uniq
}
