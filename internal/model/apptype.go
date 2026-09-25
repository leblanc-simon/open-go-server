package model

import (
	"fmt"
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
	// AllowOverride vaut vrai pour les applications s'appuyant sur .htaccess.
	AllowOverride bool
}

// appTypes est la table par défaut, surchargeable par /etc/ogs/apptypes.yaml.
var appTypes = map[AppKind]AppType{
	KindStatic:   {Kind: KindStatic, Docroot: "www", VHostTemplate: "static"},
	KindPHPPlain: {Kind: KindPHPPlain, Docroot: "www", VHostTemplate: "php-fpm", NeedsPHP: true},
	KindPrestashop: {
		Kind: KindPrestashop, Docroot: "www", FrontController: "index.php",
		VHostTemplate: "php-fpm", NeedsPHP: true, AllowOverride: true,
		// Prérequis Prestashop : les valeurs par défaut de PHP ne suffisent pas.
		PHPSettings: map[string]string{
			"php_admin_value[memory_limit]":        "512M",
			"php_admin_value[max_input_vars]":      "5000",
			"php_admin_value[upload_max_filesize]": "64M",
			"php_admin_value[post_max_size]":       "64M",
			"php_admin_value[max_execution_time]":  "300",
		},
	},
	KindSymfony: {Kind: KindSymfony, Docroot: "www/public", FrontController: "index.php", VHostTemplate: "php-fpm", NeedsPHP: true, EnvVar: "APP_ENV"},
	KindGo:      {Kind: KindGo, VHostTemplate: "proxy", Runtime: "go"},
	KindNode:    {Kind: KindNode, VHostTemplate: "proxy", Runtime: "node"},
	KindPython:  {Kind: KindPython, VHostTemplate: "proxy", Runtime: "python"},
	KindRuby:    {Kind: KindRuby, VHostTemplate: "proxy", Runtime: "ruby"},
	KindCustom:  {Kind: KindCustom, VHostTemplate: "proxy", Runtime: "custom"},
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
