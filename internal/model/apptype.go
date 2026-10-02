package model

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// AppKind identifie un type d'application. Seuls Prestashop et Symfony sont
// livrés en v1 (D37) ; les autres frameworks s'ajoutent par apptypes.yaml.
type AppKind string

const (
	KindStatic     AppKind = "static"
	KindPHPPlain   AppKind = "php-plain"
	KindPrestashop AppKind = "php-prestashop"
	KindSymfony    AppKind = "php-symfony"
	KindGo         AppKind = "go"
	KindNode       AppKind = "node"
	KindPython     AppKind = "python"
	KindRuby       AppKind = "ruby"
	// KindCustom est un processus supervisé quelconque : ogs n'y installe aucun
	// runtime, et la commande à lancer est déclarée par spec.app.command (D75).
	KindCustom AppKind = "custom"
)

// AppType décrit le comportement d'un type d'application.
type AppType struct {
	Kind AppKind
	// Docroot est relatif à la racine du projet ; vide pour les applications
	// servies par un reverse-proxy.
	Docroot string
	// FrontController est le fichier d'entrée, vide s'il n'y en a pas.
	FrontController string
	// VHostTemplate est le template de vhost à employer.
	VHostTemplate string
	// NeedsPHP indique qu'un pool PHP-FPM est requis.
	NeedsPHP bool
	// Runtime est le runtime applicatif à installer, vide pour PHP et le statique.
	Runtime string
	// EnvVar est la variable d'environnement pilotant l'environnement applicatif.
	EnvVar string
	// PHPSettings porte les réglages caractéristiques du type, appliqués par
	// défaut et surchargeables. Prestashop, par exemple, ne fonctionne pas avec
	// les valeurs par défaut de PHP.
	PHPSettings map[string]string
	// Config énumère, relatifs à www, les fichiers où le type range par
	// convention la configuration d'un environnement : les identifiants et les
	// adresses de ses bases et de son transport, mais aussi ses secrets — clé
	// de l'application, clés d'API, clé du coffre. « site clone » ne les recopie
	// pas : ceux de la production la désignent, et portent des secrets qu'ogs ne
	// saurait ni reconnaître ni remplacer. Chaque composant est un motif au sens
	// de path.Match.
	Config []string
	// Cache énumère, relatifs à www, les caches que le type compile depuis cette
	// configuration, secrets compris, et qu'il reconstruit à la demande : ils ne
	// sont pas recopiés non plus.
	Cache []string
	// Installer énumère, relatifs à www, les répertoires d'installation vers
	// lesquels l'application renvoie tout visiteur quand elle ne trouve aucun de
	// ses fichiers de configuration (Config). Un clone, qui ne recopie pas ces
	// derniers, offre alors l'installeur à qui visite la préproduction, publique
	// (D57), si l'un de ces répertoires a survécu à la mise en production.
	Installer []string
}

// dotenvConfig est la configuration par variables d'environnement des
// applications servies par un runtime : les bibliothèques dotenv de Node, de
// Python, de Ruby et de Go lisent .env et ses variantes, qui y sont le lieu des
// secrets — non celui des valeurs par défaut, comme chez Symfony.
var dotenvConfig = []string{".env", ".env.local", ".env.*.local", ".env.production"}

// appTypes est la table par défaut, surchargeable par /etc/ogs/apptypes.yaml.
var appTypes = map[AppKind]AppType{
	KindStatic:   {Kind: KindStatic, Docroot: "www", VHostTemplate: "static"},
	KindPHPPlain: {Kind: KindPHPPlain, Docroot: "www", VHostTemplate: "php-fpm", NeedsPHP: true},
	KindPrestashop: {
		Kind: KindPrestashop, Docroot: "www", FrontController: "index.php",
		VHostTemplate: "php-fpm", NeedsPHP: true,
		// parameters.php depuis la 1.7.4, parameters.yml avant elle,
		// settings.inc.php pour la 1.6 : base, clés des cookies, secret.
		Config: []string{"app/config/parameters.php", "app/config/parameters.yml", "config/settings.inc.php"},
		Cache:  []string{"var/cache"},
		// Sans configuration, config/config.inc.php redirige vers install/, ou
		// install-dev/ dans une copie du dépôt, que Prestashop demande de retirer
		// après l'installation.
		Installer: []string{"install", "install-dev"},
		// Prérequis Prestashop : les valeurs par défaut de PHP ne suffisent pas.
		PHPSettings: map[string]string{
			"php_admin_value[memory_limit]":        "512M",
			"php_admin_value[max_input_vars]":      "5000",
			"php_admin_value[upload_max_filesize]": "64M",
			"php_admin_value[post_max_size]":       "64M",
			"php_admin_value[max_execution_time]":  "300",
		},
	},
	KindSymfony: {
		Kind: KindSymfony, Docroot: "www/public", FrontController: "index.php", VHostTemplate: "php-fpm",
		NeedsPHP: true, EnvVar: "APP_ENV",
		// .env et .env.<environnement> sont versionnés et ne portent que des
		// valeurs par défaut ; celles d'une machine vont dans les variantes
		// « .local » ou dans leur compilation, .env.local.php, et la clé qui ouvre
		// le coffre des secrets dans config/secrets. parameters.yml est la
		// configuration de Symfony 3.4.
		Config: []string{".env.local", ".env.local.php", ".env.*.local",
			"config/secrets/*/*.decrypt.private.php", "app/config/parameters.yml"},
		Cache: []string{"var/cache"},
	},
	KindGo:     {Kind: KindGo, VHostTemplate: "proxy", Runtime: "go", Config: dotenvConfig},
	KindNode:   {Kind: KindNode, VHostTemplate: "proxy", Runtime: "node", Config: dotenvConfig},
	KindPython: {Kind: KindPython, VHostTemplate: "proxy", Runtime: "python", Config: dotenvConfig},
	KindRuby: {Kind: KindRuby, VHostTemplate: "proxy", Runtime: "ruby",
		// master.key et les clés par environnement ouvrent les credentials de Rails.
		Config: append(slices.Clone(dotenvConfig), "config/master.key", "config/credentials/*.key")},
	KindCustom: {Kind: KindCustom, VHostTemplate: "proxy", Runtime: "custom", Config: dotenvConfig},
}

// Kinds énumère les types connus, triés pour un affichage stable.
func Kinds() []AppKind {
	return []AppKind{
		KindPrestashop, KindSymfony, KindPHPPlain, KindStatic,
		KindGo, KindNode, KindPython, KindRuby, KindCustom,
	}
}

// symfonyLegacyMajor est la dernière version majeure de Symfony servant depuis
// web/app.php ; à partir de la 4.0, c'est public/index.php.
const symfonyLegacyMajor = 3

// Resolve renvoie le type d'application, ajusté selon la variante de version.
// Symfony 3.4 impose un docroot et un front controller différents des versions
// suivantes, ainsi qu'une autre variable d'environnement (§7.10.1).
func Resolve(kind AppKind, version string) (AppType, error) {
	t, ok := appTypes[kind]
	if !ok {
		return AppType{}, fmt.Errorf("type d'application %q inconnu", kind)
	}
	if kind != KindSymfony || version == "" || version == "auto" {
		return t, nil
	}
	major, err := majorVersion(version)
	if err != nil {
		return AppType{}, fmt.Errorf("version Symfony %q : %w", version, err)
	}
	if major <= symfonyLegacyMajor {
		t.Docroot = "www/web"
		t.FrontController = "app.php"
		t.EnvVar = "SYMFONY_ENV"
	}
	return t, nil
}

// SupportsMessenger indique si le composant Messenger est disponible : introduit
// en Symfony 4.1, il est absent en 3.4, où le worker passe par rabbitmq-consumer.
func SupportsMessenger(kind AppKind, version string) bool {
	if kind != KindSymfony {
		return false
	}
	if version == "" || version == "auto" {
		return true // hypothèse par défaut, corrigée dès la détection
	}
	major, minor, err := parseVersion(version)
	if err != nil {
		return true
	}
	if major > 4 {
		return true
	}
	return major == 4 && minor >= 1
}

func majorVersion(v string) (int, error) {
	major, _, err := parseVersion(v)
	return major, err
}

func parseVersion(v string) (int, int, error) {
	parts := strings.SplitN(strings.TrimSpace(v), ".", 3)
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("numéro de version majeure illisible")
	}
	minor := 0
	if len(parts) > 1 {
		if minor, err = strconv.Atoi(parts[1]); err != nil {
			return 0, 0, fmt.Errorf("numéro de version mineure illisible")
		}
	}
	return major, minor, nil
}
