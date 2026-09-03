// Package config porte la configuration globale et la construction des chemins.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/provider/hardening"
)

// Config est la configuration globale, chargée depuis /etc/ogs/config.yaml.
type Config struct {
	// Prefix décale l'ensemble des chemins système. Vide en production ; renseigné
	// pour les tests et le développement, ce qui permet d'exécuter ogs sans être
	// root sur une arborescence jetable.
	Prefix string `yaml:"-"`

	Naming       Naming                 `yaml:"naming"`
	User         User                   `yaml:"user"`
	Environments map[string]Environment `yaml:"environments"`
	Logs         Logs                   `yaml:"logs"`
	Web          Web                    `yaml:"web"`
	Sudo         Sudo                   `yaml:"sudo"`
	Deployment   Deployment             `yaml:"deployment"`
	Certificates map[string]SharedCert  `yaml:"certificates"`
	ACME         ACME                   `yaml:"acme"`
	Archives     Archives               `yaml:"archives"`
	Hardening    Hardening              `yaml:"hardening"`
	Monitoring   Monitoring             `yaml:"monitoring"`
}

// Hardening paramètre la restriction des droits des répertoires sensibles :
// ceux de configuration, et la racine des homes (D60).
type Hardening struct {
	// Skip énumère les chemins que « ogs harden » doit laisser tranquilles.
	Skip []string `yaml:"skip,omitempty"`
	// Targets remplace la table livrée — la racine des homes comprise, qu'il faut
	// alors y réinscrire. Vide, celle d'ogs s'applique.
	Targets []hardening.Target `yaml:"targets,omitempty"`
}

// Monitoring paramètre la supervision du parc (D64) : l'agent de métriques et le
// garde-fou des tentatives d'intrusion. ogs n'installe ni Telegraf ni fail2ban —
// il écrit leur configuration à partir de ce qu'il sait du parc, et se tait
// lorsqu'ils sont absents.
type Monitoring struct {
	// Status est le point d'écoute local qui expose l'état des serveurs et des
	// pools. Il n'a de sens que sur la boucle locale (D66).
	Status Status `yaml:"status"`

	Telegraf Telegraf `yaml:"telegraf"`
	Fail2ban Fail2ban `yaml:"fail2ban"`
}

// Status décrit le vhost de supervision.
type Status struct {
	// Enabled force l'écriture du vhost, ou l'interdit. Nil laisse ogs décider :
	// il l'écrit dès que Telegraf est configuré.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Listen est l'adresse d'écoute. Une adresse hors boucle locale est refusée :
	// le vhost expose l'état de tous les pools du parc sans authentification.
	Listen string `yaml:"listen,omitempty"`
	// Path est le préfixe des chemins exposés.
	Path string `yaml:"path,omitempty"`
}

// Telegraf paramètre le fragment d'entrées écrit dans /etc/telegraf/telegraf.d.
// Seules les entrées sont générées : la destination des métriques — base, jeton,
// organisation — reste à l'administrateur, dans telegraf.conf (D65).
type Telegraf struct {
	// Enabled force la génération, ou l'interdit. Nil : ogs génère si le
	// répertoire telegraf.d existe.
	Enabled *bool `yaml:"enabled,omitempty"`
	// User est le compte système de l'agent. Il donne le groupe propriétaire du
	// fragment, qui porte les mots de passe de supervision.
	User string `yaml:"user,omitempty"`
	// Interval est la période de collecte, reprise dans chaque entrée.
	Interval string `yaml:"interval,omitempty"`
	// Account est le nom des comptes de supervision créés sur MySQL, PostgreSQL
	// et RabbitMQ. Un seul nom pour les trois : ils ont la même finalité, et un
	// nom par moteur ne ferait qu'obscurcir les journaux d'authentification.
	Account string `yaml:"account,omitempty"`
	// Inputs désactive une famille d'entrées : mysql, postgres, rabbitmq, phpfpm,
	// web, procstat, disk. Une famille absente de la table est activée.
	Inputs map[string]bool `yaml:"inputs,omitempty"`
}

// Fail2ban paramètre le fichier de prisons écrit dans /etc/fail2ban/jail.d.
type Fail2ban struct {
	// Enabled force la génération, ou l'interdit. Nil : ogs génère si le
	// répertoire jail.d existe.
	Enabled *bool `yaml:"enabled,omitempty"`
	// BanTime, FindTime et MaxRetry sont les réglages communs à toutes les
	// prisons générées ; une prison peut les redéfinir.
	BanTime  string `yaml:"banTime,omitempty"`
	FindTime string `yaml:"findTime,omitempty"`
	MaxRetry int    `yaml:"maxRetry,omitempty"`
	// IgnoreIP énumère les adresses jamais bannies. La boucle locale y est
	// toujours ajoutée : se bannir soi-même coupe la supervision.
	IgnoreIP []string `yaml:"ignoreIP,omitempty"`
	// Jails désactive une prison nommée (sshd, recidive, web-auth, web-badbots,
	// web-noscript). Une prison absente de la table est activée.
	Jails map[string]bool `yaml:"jails,omitempty"`
}

// TelegrafEnabled, Fail2banEnabled et StatusEnabled traduisent le tri-état des
// champs « enabled » : une valeur explicite l'emporte, l'absence laisse le
// provider décider d'après ce qui est installé.
func (m Monitoring) TelegrafEnabled() (bool, bool) { return deref(m.Telegraf.Enabled) }
func (m Monitoring) Fail2banEnabled() (bool, bool) { return deref(m.Fail2ban.Enabled) }
func (m Monitoring) StatusEnabled() (bool, bool)   { return deref(m.Status.Enabled) }

func deref(b *bool) (value, set bool) {
	if b == nil {
		return false, false
	}
	return *b, true
}

// TelegrafUser renvoie le compte système de l'agent, défaut appliqué.
func (m Monitoring) TelegrafUser() string {
	if m.Telegraf.User == "" {
		return DefaultTelegrafUser
	}
	return m.Telegraf.User
}

// TelegrafAccount renvoie le nom des comptes de supervision, défaut appliqué.
func (m Monitoring) TelegrafAccount() string {
	if m.Telegraf.Account == "" {
		return DefaultTelegrafAccount
	}
	return m.Telegraf.Account
}

// TelegrafInterval renvoie la période de collecte, défaut appliqué.
func (m Monitoring) TelegrafInterval() string {
	if m.Telegraf.Interval == "" {
		return DefaultTelegrafInterval
	}
	return m.Telegraf.Interval
}

// InputEnabled indique si une famille d'entrées est retenue.
func (m Monitoring) InputEnabled(name string) bool {
	if v, ok := m.Telegraf.Inputs[name]; ok {
		return v
	}
	return true
}

// JailEnabled indique si une prison est retenue.
func (m Monitoring) JailEnabled(name string) bool {
	if v, ok := m.Fail2ban.Jails[name]; ok {
		return v
	}
	return true
}

// StatusListen renvoie l'adresse d'écoute du vhost de supervision, défaut appliqué.
func (m Monitoring) StatusListen() string {
	if m.Status.Listen == "" {
		return DefaultStatusListen
	}
	return m.Status.Listen
}

// StatusPath renvoie le préfixe des chemins de supervision, défaut appliqué.
func (m Monitoring) StatusPath() string {
	if m.Status.Path == "" {
		return DefaultStatusPath
	}
	return strings.TrimSuffix(m.Status.Path, "/")
}

// Défauts de la supervision.
const (
	DefaultTelegrafUser     = "telegraf"
	DefaultTelegrafAccount  = "telegraf"
	DefaultTelegrafInterval = "30s"
	// DefaultStatusListen n'écoute que la boucle locale. Le port est au-dessus de
	// 8000 pour ne heurter aucun service courant, et le vhost refuse de s'écrire
	// sur une adresse routable.
	DefaultStatusListen = "127.0.0.1:8099"
	DefaultStatusPath   = "/ogs-status"
	// PoolStatusPath est le chemin de statut d'un pool PHP-FPM. Il n'est pas
	// atteignable depuis un vhost de projet : ceux-ci ne routent vers FPM que les
	// URI se terminant par « .php ».
	PoolStatusPath = "/ogs-fpm-status"
	// PoolPingPath répond « pong » sans charger l'application : il distingue un
	// pool vivant d'un pool saturé.
	PoolPingPath = "/ogs-fpm-ping"
)

// Archives paramètre le sort des archives de projets supprimés.
type Archives struct {
	// RetentionDays borne leur conservation. Une archive contient un dump de base,
	// donc des données de clients : la garder indéfiniment n'est pas neutre.
	// Zéro signifie « ne jamais purger », et doit être un choix explicite.
	RetentionDays int `yaml:"retentionDays"`
}

// ACME paramètre le dialogue avec l'autorité de certification.
type ACME struct {
	// Email est le contact déclaré : l'autorité y notifie les expirations.
	Email string `yaml:"email"`
	// Directory permet de viser une autre autorité (test, ACME privé).
	Directory string `yaml:"directory,omitempty"`
	// Staging bascule sur l'environnement de test de Let's Encrypt.
	Staging bool `yaml:"staging,omitempty"`
}

// Naming regroupe les réglages de nommage et d'emplacement des projets.
type Naming struct {
	HomeRoot string `yaml:"homeRoot"` // racine des homes, /home par défaut
}

// User porte les défauts des comptes de projet. Ce sont des défauts **à la
// création** : le fichier d'un projet existant porte déjà sa valeur, et c'est lui
// qui fait autorité ensuite — le changer ici ne rétroagit sur aucun projet, mais
// « spec.user.shell » édité puis appliqué, si.
type User struct {
	// Shell est le shell des comptes de projet (D62). Vide, le défaut du modèle
	// s'applique — /bin/bash. Le fermer se fait ici, en une ligne pour tout le parc.
	Shell string `yaml:"shell,omitempty"`
}

// Environment décrit un environnement (prod, prep).
type Environment struct {
	Domain          string            `yaml:"domain"`
	HostTemplate    string            `yaml:"hostTemplate"`
	IDSuffix        string            `yaml:"idSuffix"`
	Certificate     string            `yaml:"certificate"`
	RobotsNoindex   bool              `yaml:"robotsNoindex"`
	PHPEnv          map[string]string `yaml:"phpEnv"`
	PresetDowngrade bool              `yaml:"presetDowngrade"`
}

// Logs paramètre la rotation.
type Logs struct {
	Rotate   int    `yaml:"rotate"`
	MaxSize  string `yaml:"maxSize"`
	Compress bool   `yaml:"compress"`
}

// Web fixe le moteur préféré lorsque les deux sont installés.
type Web struct {
	Engine string `yaml:"engine"`
}

// Sudo paramètre la délégation à l'utilisateur du projet.
type Sudo struct {
	Style string   `yaml:"style"` // ogs | systemctl | none
	Verbs []string `yaml:"verbs"`
}

// Deployment paramètre la clé de déploiement.
type Deployment struct {
	KnownHosts []string `yaml:"knownHosts"`
}

// SharedCert est un certificat de portée serveur, référencé par @<nom>.
type SharedCert struct {
	Domains     []string `yaml:"domains"`
	Challenge   string   `yaml:"challenge"`
	DNSProvider string   `yaml:"dnsProvider"`
	KeyType     string   `yaml:"keyType"`
}

// Default renvoie la configuration par défaut, telle que décrite au cahier des charges.
func Default() *Config {
	return &Config{
		Naming: Naming{HomeRoot: "/home"},
		User:   User{Shell: model.DefaultShell},
		Environments: map[string]Environment{
			string(naming.EnvProd): {HostTemplate: "{{.Host}}"},
			string(naming.EnvPrep): {
				Domain:          "preprod-pp.net",
				HostTemplate:    "{{.PrepLabel}}.{{.Domain}}",
				IDSuffix:        naming.SuffixPrep,
				Certificate:     "@wildcard-prep",
				RobotsNoindex:   true,
				PHPEnv:          map[string]string{"APP_ENV": "dev"},
				PresetDowngrade: true,
			},
		},
		Logs:       Logs{Rotate: 30, MaxSize: "200M", Compress: true},
		Archives:   Archives{RetentionDays: 14},
		Sudo:       Sudo{Style: "ogs", Verbs: []string{"start", "stop", "restart", "status"}},
		Deployment: Deployment{KnownHosts: []string{"github.com", "gitlab.com"}},
		Certificates: map[string]SharedCert{
			"wildcard-prep": {
				Domains:     []string{"*.preprod-pp.net", "preprod-pp.net"},
				Challenge:   "dns-01",
				DNSProvider: "ovh",
				KeyType:     "ec256",
			},
		},
	}
}

// Shell renvoie le shell des comptes de projet, défaut du modèle appliqué.
func (c *Config) Shell() string {
	if c.User.Shell == "" {
		return model.DefaultShell
	}
	return c.User.Shell
}

// PrepDomain renvoie le domaine de préproduction configuré.
func (c *Config) PrepDomain() string {
	return c.Environments[string(naming.EnvPrep)].Domain
}

// Validate contrôle la cohérence de la configuration chargée.
func (c *Config) Validate() error {
	if c.Naming.HomeRoot == "" || !filepath.IsAbs(c.Naming.HomeRoot) {
		return fmt.Errorf("naming.homeRoot doit être un chemin absolu (valeur : %q)", c.Naming.HomeRoot)
	}
	// Un shell relatif serait pris tel quel par useradd et rendrait le compte
	// inutilisable sans un mot : le refuser au chargement, non à l'application.
	if c.User.Shell != "" && !filepath.IsAbs(c.User.Shell) {
		return fmt.Errorf("user.shell doit être un chemin absolu (valeur : %q)", c.User.Shell)
	}
	switch c.Sudo.Style {
	case "", "ogs", "systemctl", "none":
	default:
		return fmt.Errorf("sudo.style %q inconnu (attendu : ogs, systemctl ou none)", c.Sudo.Style)
	}
	if c.Web.Engine != "" && c.Web.Engine != "apache" && c.Web.Engine != "nginx" {
		return fmt.Errorf("web.engine %q inconnu (attendu : apache ou nginx)", c.Web.Engine)
	}
	for nom, cert := range c.Certificates {
		if cert.Challenge == "dns-01" && cert.DNSProvider != "" && cert.DNSProvider != "ovh" {
			return fmt.Errorf("certificates.%s.dnsProvider %q non géré : ogs ne gère qu'« ovh »",
				nom, cert.DNSProvider)
		}
	}
	if c.Archives.RetentionDays < 0 {
		return fmt.Errorf("archives.retentionDays ne peut pas être négatif (valeur : %d)",
			c.Archives.RetentionDays)
	}
	if err := validateStatusListen(c.Monitoring.StatusListen()); err != nil {
		return err
	}
	if p := c.Monitoring.StatusPath(); p != "" && !strings.HasPrefix(p, "/") {
		return fmt.Errorf("monitoring.status.path doit commencer par « / » (valeur : %q)", p)
	}
	return nil
}

// validateStatusListen refuse une écoute hors de la boucle locale. Le vhost de
// supervision expose, sans la moindre authentification, l'état de tous les pools
// du parc et l'adresse de chaque client en cours de traitement : l'ouvrir sur une
// interface routable publierait la carte du serveur. Le refus est au chargement
// de la configuration, non à l'écriture du vhost : une erreur au moment de
// l'apply laisserait le fichier écrit avant de se plaindre.
func validateStatusListen(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("monitoring.status.listen doit être de la forme adresse:port (valeur : %q) : %w",
			listen, err)
	}
	if port == "" {
		return fmt.Errorf("monitoring.status.listen : port manquant (valeur : %q)", listen)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("monitoring.status.listen : %q n'est pas une adresse IP — "+
			"un nom d'hôte se résoudrait différemment selon le moteur", host)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("monitoring.status.listen : %s n'est pas une adresse de boucle locale — "+
			"le vhost de supervision expose l'état de tous les pools sans authentification", host)
	}
	return nil
}

// Paths centralise tous les chemins système, préfixe appliqué.
type Paths struct{ prefix string }

// Chemins de référence, hors préfixe.
const (
	etcDir       = "/etc/ogs"
	libDir       = "/var/lib/ogs"
	logDir       = "/var/log/ogs"
	runDir       = "/run/ogs"
	sudoersDir   = "/etc/sudoers.d"
	logrotateDir = "/etc/logrotate.d"
	tmpfilesDir  = "/etc/tmpfiles.d"
	systemdDir   = "/etc/systemd/system"
	cronDir      = "/etc/cron.d"

	telegrafDir     = "/etc/telegraf/telegraf.d"
	fail2banJailDir = "/etc/fail2ban/jail.d"
	fail2banLog     = "/var/log/fail2ban.log"
	authLog         = "/var/log/auth.log"

	// defaultAuthorizedKeysFile est le trousseau commun du parc : s'il existe, le
	// .ssh/authorized_keys de chaque projet y est lié (D61). Il n'est pas un
	// fichier géré par ogs — c'est l'administrateur qui le pose et le tient à
	// jour ; ogs ne fait que le lier, et « doctor » en contrôle les droits.
	defaultAuthorizedKeysFile = "/opt/default_authorized_keys"
)

// NewPaths construit les chemins pour un préfixe donné.
func NewPaths(prefix string) Paths { return Paths{prefix: strings.TrimRight(prefix, "/")} }

func (p Paths) join(elem ...string) string {
	return filepath.Join(append([]string{p.prefix}, elem...)...)
}

// Prefix expose le préfixe actif (vide en production).
func (p Paths) Prefix() string { return p.prefix }

func (p Paths) Etc() string           { return p.join(etcDir) }
func (p Paths) ConfigFile() string    { return p.join(etcDir, "config.yaml") }
func (p Paths) MasterKey() string     { return p.join(etcDir, "master.key") }
func (p Paths) ProjectsDir() string   { return p.join(etcDir, "projects") }
func (p Paths) ArchivedDir() string   { return p.join(etcDir, "projects", "archived") }
func (p Paths) TemplatesDir() string  { return p.join(etcDir, "templates") }
func (p Paths) HooksDir() string      { return p.join(etcDir, "hooks") }
func (p Paths) DNSCredsDir() string   { return p.join(etcDir, "credentials", "dns") }
func (p Paths) Lib() string           { return p.join(libDir) }
func (p Paths) StateDB() string       { return p.join(libDir, "state.db") }
func (p Paths) ACMEDir() string       { return p.join(libDir, "acme") }
func (p Paths) ACMEChallenge() string { return p.join(libDir, "acme-challenge") }

// DefaultAuthorizedKeys renvoie le trousseau de clés commun du parc, lié dans le
// .ssh de chaque projet lorsqu'il est présent.
func (p Paths) DefaultAuthorizedKeys() string { return p.join(defaultAuthorizedKeysFile) }

// ACMEChallengeTokens est le répertoire où les jetons sont réellement déposés :
// le client ACME ajoute « .well-known/acme-challenge » au webroot qu'on lui
// confie. C'est donc là que l'alias d'Apache doit pointer, tandis que le « root »
// de nginx veut le webroot, puisqu'il y accole l'URI demandée.
func (p Paths) ACMEChallengeTokens() string {
	return filepath.Join(p.ACMEChallenge(), ".well-known", "acme-challenge")
}

// RobotsNoindexFile est le robots.txt servi aux préproductions. Il est hors de
// portée des projets : un site ne doit pas pouvoir se désindexer lui-même.
func (p Paths) RobotsNoindexFile() string { return p.join(libDir, "robots-noindex.txt") }
func (p Paths) ArchivesDir() string       { return p.join(libDir, "archives") }
func (p Paths) DumpsDir() string          { return p.join(libDir, "dumps") }
func (p Paths) LogDir() string            { return p.join(logDir) }
func (p Paths) AppLog() string            { return p.join(logDir, "ogs.log") }
func (p Paths) AuditLog() string          { return p.join(logDir, "audit.log") }
func (p Paths) RunDir() string            { return p.join(runDir) }
func (p Paths) GlobalLock() string        { return p.join(runDir, "ogs.lock") }
func (p Paths) SudoersDir() string        { return p.join(sudoersDir) }
func (p Paths) LogrotateFile() string     { return p.join(logrotateDir, "ogs") }
func (p Paths) TmpfilesFile() string      { return p.join(tmpfilesDir, "ogs.conf") }

// TelegrafDir est le répertoire de fragments de l'agent de métriques, et
// TelegrafFile le seul fichier qu'ogs y écrit. Un fichier unique, comme pour
// logrotate : cent projets ne doivent pas produire cent fragments à relire.
func (p Paths) TelegrafDir() string  { return p.join(telegrafDir) }
func (p Paths) TelegrafFile() string { return filepath.Join(p.TelegrafDir(), "ogs.conf") }

// Fail2banJailDir est le répertoire de prisons, et Fail2banJailFile le fichier
// géré. Il ne remplace pas jail.local : l'administrateur garde le sien, et les
// prisons d'ogs portent toutes le préfixe « ogs- » pour ne recouvrir aucune des
// siennes.
func (p Paths) Fail2banJailDir() string  { return p.join(fail2banJailDir) }
func (p Paths) Fail2banJailFile() string { return filepath.Join(p.Fail2banJailDir(), "ogs.conf") }

// Fail2banLog est le journal de fail2ban, que la prison de récidive relit.
func (p Paths) Fail2banLog() string { return p.join(fail2banLog) }

// AuthLog est le journal d'authentification du système. Il n'existe plus sur une
// Debian récente sans rsyslog : son absence décide du backend de la prison sshd.
func (p Paths) AuthLog() string { return p.join(authLog) }

// SystemdDir renvoie le répertoire des unités systemd générées.
func (p Paths) SystemdDir() string { return p.join(systemdDir) }

// CronDir renvoie le répertoire des fichiers cron générés.
func (p Paths) CronDir() string { return p.join(cronDir) }

// ProjectFile renvoie le chemin du fichier d'état désiré d'un projet.
func (p Paths) ProjectFile(id string) string { return filepath.Join(p.ProjectsDir(), id+".yaml") }

// SudoersFile renvoie le chemin du fichier sudoers d'un projet.
func (p Paths) SudoersFile(id string) string { return filepath.Join(p.SudoersDir(), "ogs-"+id) }

// ProjectRunDir renvoie le répertoire d'exécution d'un projet (verrous, sockets).
func (p Paths) ProjectRunDir(id string) string { return filepath.Join(p.RunDir(), id) }

// Home renvoie le home d'un projet à partir de son host effectif.
func (p Paths) Home(cfg *Config, effectiveHost string) string {
	return filepath.Join(p.prefix, cfg.Naming.HomeRoot, effectiveHost)
}

// Chemins PHP. Ils suivent le découpage Debian/Ubuntu (dépôt Sury pour le
// multi-version), seule famille gérée en v1.
const (
	phpEtc = "/etc/php"
	phpRun = "/run/php"
	phpCLI = "/usr/bin/php"
	phpFPM = "/usr/sbin/php-fpm"
)

// PHPRoot renvoie la racine des configurations PHP.
func (p Paths) PHPRoot() string { return p.join(phpEtc) }

// PHPPoolDir renvoie le répertoire des pools d'une version.
func (p Paths) PHPPoolDir(version string) string {
	return p.join(phpEtc, version, "fpm", "pool.d")
}

// PHPPoolFile renvoie le chemin du fichier de pool.
func (p Paths) PHPPoolFile(version, pool string) string {
	return filepath.Join(p.PHPPoolDir(version), pool+".conf")
}

// PHPBin renvoie l'interpréteur en ligne de commande d'une version.
func (p Paths) PHPBin(version string) string { return p.join(phpCLI + version) }

// PHPFpmBin renvoie le binaire FPM d'une version.
func (p Paths) PHPFpmBin(version string) string { return p.join(phpFPM + version) }

// PHPRunDir renvoie le répertoire des sockets FPM.
func (p Paths) PHPRunDir() string { return p.join(phpRun) }

// PHPSocket renvoie le chemin du socket d'un pool. Le numéro y figure, ce qui
// rend l'appartenance lisible dans un « ss -x » ou un « ls /run/php ».
func (p Paths) PHPSocket(version string, number int, pool string) string {
	return filepath.Join(p.PHPRunDir(), fmt.Sprintf("php%s-fpm-%d-%s.sock", version, number, pool))
}

// PHPUnit renvoie le nom de l'unité systemd du gestionnaire de processus.
func PHPUnit(version string) string { return "php" + version + "-fpm" }

// Dirs énumère les répertoires à créer par `ogs init`, avec leur mode.
func (p Paths) Dirs() []struct {
	Path string
	Mode os.FileMode
} {
	return []struct {
		Path string
		Mode os.FileMode
	}{
		// 0700 : les déclarations de projets dressent la carte du parc — hôtes,
		// utilisateurs, noms de bases, transports —, et le répertoire porte la clé
		// maître. Rien de tout cela ne concerne les utilisateurs du serveur, qui y
		// ont un compte local.
		{p.Etc(), 0o700},
		{p.ProjectsDir(), 0o700},
		{p.ArchivedDir(), 0o700},
		{p.TemplatesDir(), 0o750},
		// Les hooks s'exécutent en root : leur répertoire n'a pas à être lisible.
		{p.HooksDir(), 0o700},
		{p.DNSCredsDir(), 0o700},
		// 0751 et non 0750 : le serveur web doit pouvoir traverser ce répertoire
		// pour atteindre /var/lib/ogs/acme-challenge, sans quoi tout challenge
		// HTTP-01 se voit refuser par un 403. Le bit d'exécution seul n'autorise
		// pas l'énumération, et les contenus sensibles gardent leurs propres
		// droits — la base d'état en 0600, les certificats en 0700.
		{p.Lib(), 0o751},
		{p.ACMEDir(), 0o700},
		{p.ACMEChallenge(), 0o755},
		{p.ACMEChallengeTokens(), 0o755},
		{p.ArchivesDir(), 0o700},
		{p.DumpsDir(), 0o700},
		{p.LogDir(), 0o750},
		{p.RunDir(), 0o755},
	}
}
