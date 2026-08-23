// Package config porte la configuration globale et la construction des chemins.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/leviathan/ogs/internal/naming"
)

// Config est la configuration globale, chargée depuis /etc/ogs/config.yaml.
type Config struct {
	// Prefix décale l'ensemble des chemins système. Vide en production ; renseigné
	// pour les tests et le développement, ce qui permet d'exécuter ogs sans être
	// root sur une arborescence jetable.
	Prefix string `yaml:"-"`

	Naming       Naming                 `yaml:"naming"`
	Environments map[string]Environment `yaml:"environments"`
	Logs         Logs                   `yaml:"logs"`
	Web          Web                    `yaml:"web"`
	Sudo         Sudo                   `yaml:"sudo"`
	Deployment   Deployment             `yaml:"deployment"`
	Certificates map[string]SharedCert  `yaml:"certificates"`
}

// Naming regroupe les réglages de nommage et d'emplacement des projets.
type Naming struct {
	HomeRoot string `yaml:"homeRoot"` // racine des homes, /home par défaut
}

// Environment décrit un environnement (prod, prep).
type Environment struct {
	Domain          string            `yaml:"domain"`
	HostTemplate    string            `yaml:"hostTemplate"`
	IDSuffix        string            `yaml:"idSuffix"`
	Certificate     string            `yaml:"certificate"`
	RobotsNoindex   bool              `yaml:"robotsNoindex"`
	BasicAuth       bool              `yaml:"basicAuth"`
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
		Environments: map[string]Environment{
			string(naming.EnvProd): {HostTemplate: "{{.Host}}"},
			string(naming.EnvPrep): {
				Domain:          "preprod-pp.net",
				HostTemplate:    "{{.ID}}.{{.Domain}}",
				IDSuffix:        naming.SuffixPrep,
				Certificate:     "@wildcard-prep",
				RobotsNoindex:   true,
				PHPEnv:          map[string]string{"APP_ENV": "dev"},
				PresetDowngrade: true,
			},
		},
		Logs:       Logs{Rotate: 30, MaxSize: "200M", Compress: true},
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

// PrepDomain renvoie le domaine de préproduction configuré.
func (c *Config) PrepDomain() string {
	return c.Environments[string(naming.EnvPrep)].Domain
}

// Validate contrôle la cohérence de la configuration chargée.
func (c *Config) Validate() error {
	if c.Naming.HomeRoot == "" || !filepath.IsAbs(c.Naming.HomeRoot) {
		return fmt.Errorf("naming.homeRoot doit être un chemin absolu (valeur : %q)", c.Naming.HomeRoot)
	}
	switch c.Sudo.Style {
	case "", "ogs", "systemctl", "none":
	default:
		return fmt.Errorf("sudo.style %q inconnu (attendu : ogs, systemctl ou none)", c.Sudo.Style)
	}
	if c.Web.Engine != "" && c.Web.Engine != "apache" && c.Web.Engine != "nginx" {
		return fmt.Errorf("web.engine %q inconnu (attendu : apache ou nginx)", c.Web.Engine)
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
func (p Paths) HtpasswdDir() string   { return p.join(etcDir, "htpasswd") }
func (p Paths) DNSCredsDir() string   { return p.join(etcDir, "credentials", "dns") }
func (p Paths) Lib() string           { return p.join(libDir) }
func (p Paths) StateDB() string       { return p.join(libDir, "state.db") }
func (p Paths) ACMEDir() string       { return p.join(libDir, "acme") }
func (p Paths) ACMEChallenge() string { return p.join(libDir, "acme-challenge") }
func (p Paths) ArchivesDir() string   { return p.join(libDir, "archives") }
func (p Paths) LogDir() string        { return p.join(logDir) }
func (p Paths) AppLog() string        { return p.join(logDir, "ogs.log") }
func (p Paths) AuditLog() string      { return p.join(logDir, "audit.log") }
func (p Paths) RunDir() string        { return p.join(runDir) }
func (p Paths) GlobalLock() string    { return p.join(runDir, "ogs.lock") }
func (p Paths) SudoersDir() string    { return p.join(sudoersDir) }
func (p Paths) LogrotateFile() string { return p.join(logrotateDir, "ogs") }
func (p Paths) TmpfilesFile() string  { return p.join(tmpfilesDir, "ogs.conf") }

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
		{p.Etc(), 0o755},
		{p.ProjectsDir(), 0o755},
		{p.ArchivedDir(), 0o755},
		{p.TemplatesDir(), 0o755},
		{p.HooksDir(), 0o750},
		{p.HtpasswdDir(), 0o750},
		{p.DNSCredsDir(), 0o700},
		{p.Lib(), 0o750},
		{p.ACMEDir(), 0o700},
		{p.ACMEChallenge(), 0o755},
		{p.ArchivesDir(), 0o700},
		{p.LogDir(), 0o750},
		{p.RunDir(), 0o755},
	}
}
