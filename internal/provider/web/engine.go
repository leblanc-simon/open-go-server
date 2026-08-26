// Package web porte le provider des serveurs web : une abstraction commune et
// deux implémentations, Apache et Nginx.
package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"leblanc.io/open-go-server/internal/exec"
)

// Engine abstrait un serveur web. Les deux implémentations partagent le même
// modèle de données en entrée : seule la forme du fichier produit diffère.
type Engine interface {
	// Name est l'identifiant du moteur (« apache » ou « nginx »).
	Name() string
	// AvailableDir est le répertoire des configurations disponibles.
	AvailableDir() string
	// EnabledDir est le répertoire des configurations actives.
	EnabledDir() string
	// FileName traduit un nom de vhost en nom de fichier.
	FileName(name string) string
	// TemplateName traduit un template logique en template du moteur.
	TemplateName(template string) string
	// Unit est l'unité systemd à recharger.
	Unit() string
	// CommentPrefix est le préfixe de commentaire de ses fichiers.
	CommentPrefix() string
	// ValidateArgs est la commande de vérification de configuration.
	ValidateArgs() (string, []string)
	// FPMReference traduit un point d'écoute FPM en référence utilisable dans la
	// configuration du moteur.
	FPMReference(listen string, unix bool) string
	// Installed indique si le moteur est présent sur la machine.
	Installed() bool
	// ProxyReference traduit une cible applicative en référence de proxy.
	ProxyReference(target string, unix bool) string
	// SnippetPath est le fichier de réglages de portée serveur, vide si le moteur
	// n'en a pas besoin.
	SnippetPath() string
}

// apacheEngine implémente Engine pour Apache 2.4 sur Debian et Ubuntu.
type apacheEngine struct{ prefix string }

func (a apacheEngine) Name() string { return "apache" }
func (a apacheEngine) AvailableDir() string {
	return filepath.Join(a.prefix, "/etc/apache2/sites-available")
}
func (a apacheEngine) EnabledDir() string {
	return filepath.Join(a.prefix, "/etc/apache2/sites-enabled")
}
func (a apacheEngine) FileName(name string) string  { return name + ".conf" }
func (a apacheEngine) TemplateName(t string) string { return "apache/" + t + ".conf" }
func (a apacheEngine) Unit() string                 { return "apache2" }
func (a apacheEngine) CommentPrefix() string        { return "#" }

// SnippetPath : Apache n'a besoin d'aucun réglage de portée serveur, ses vhosts
// sont autonomes.
func (a apacheEngine) SnippetPath() string { return "" }

func (a apacheEngine) ValidateArgs() (string, []string) {
	return filepath.Join(a.prefix, "/usr/sbin/apachectl"), []string{"-t"}
}

// FPMReference produit la cible de mod_proxy_fcgi. Le socket Unix est référencé
// par la syntaxe unix:<chemin>|fcgi://localhost, supportée depuis Apache 2.4.7.
func (a apacheEngine) FPMReference(listen string, unix bool) string {
	if unix {
		return fmt.Sprintf("unix:%s|fcgi://localhost", listen)
	}
	return "fcgi://" + listen
}

// ProxyReference produit la cible de mod_proxy_http. La syntaxe unix: est
// supportée depuis Apache 2.4.7.
func (a apacheEngine) ProxyReference(target string, unix bool) string {
	if unix {
		return fmt.Sprintf("unix:%s|http://localhost/", target)
	}
	return "http://" + target + "/"
}

func (a apacheEngine) Installed() bool {
	_, err := os.Stat(a.AvailableDir())
	return err == nil
}

// nginxEngine implémente Engine pour Nginx.
type nginxEngine struct{ prefix string }

func (n nginxEngine) Name() string { return "nginx" }
func (n nginxEngine) AvailableDir() string {
	return filepath.Join(n.prefix, "/etc/nginx/sites-available")
}
func (n nginxEngine) EnabledDir() string           { return filepath.Join(n.prefix, "/etc/nginx/sites-enabled") }
func (n nginxEngine) FileName(name string) string  { return name + ".conf" }
func (n nginxEngine) TemplateName(t string) string { return "nginx/" + t + ".conf" }
func (n nginxEngine) Unit() string                 { return "nginx" }
func (n nginxEngine) CommentPrefix() string        { return "#" }

// SnippetPath : nginx exige que certains réglages vivent dans le contexte http,
// hors de tout server. Debian charge tout /etc/nginx/conf.d.
func (n nginxEngine) SnippetPath() string {
	return filepath.Join(n.prefix, "/etc/nginx/conf.d/ogs.conf")
}

func (n nginxEngine) ValidateArgs() (string, []string) {
	return filepath.Join(n.prefix, "/usr/sbin/nginx"), []string{"-t"}
}

// FPMReference produit la cible de fastcgi_pass.
func (n nginxEngine) FPMReference(listen string, unix bool) string {
	if unix {
		return "unix:" + listen
	}
	return listen
}

// ProxyReference produit la cible de proxy_pass.
func (n nginxEngine) ProxyReference(target string, unix bool) string {
	if unix {
		return "unix:" + target
	}
	return target
}

func (n nginxEngine) Installed() bool {
	_, err := os.Stat(n.AvailableDir())
	return err == nil
}

// NewEngine construit le moteur demandé.
func NewEngine(name, prefix string) (Engine, error) {
	switch name {
	case "apache", "apache2", "httpd":
		return apacheEngine{prefix: prefix}, nil
	case "nginx":
		return nginxEngine{prefix: prefix}, nil
	}
	return nil, fmt.Errorf("moteur web %q inconnu (apache ou nginx)", name)
}

// DetectEngine choisit le moteur : celui demandé, sinon celui qui est installé.
// Un seul moteur est piloté par serveur ; les deux peuvent être installés.
func DetectEngine(requested, prefix string) (Engine, error) {
	if requested != "" {
		e, err := NewEngine(requested, prefix)
		if err != nil {
			return nil, err
		}
		return e, nil
	}
	apache := apacheEngine{prefix: prefix}
	nginx := nginxEngine{prefix: prefix}
	switch {
	case apache.Installed() && nginx.Installed():
		return nil, fmt.Errorf("Apache et Nginx sont tous deux installés : préciser lequel piloter " +
			"(web.engine dans la configuration, ou spec.web.engine du projet)")
	case apache.Installed():
		return apache, nil
	case nginx.Installed():
		return nginx, nil
	}
	return nil, fmt.Errorf("aucun serveur web détecté : installer apache2 ou nginx, " +
		"ou préciser le moteur pour un calcul de plan hors ligne")
}

// validate exécute la vérification de configuration du moteur. L'absence du
// binaire n'est pas une erreur : le plan doit rester calculable sur une machine
// où le moteur n'est pas installé.
func validate(ctx context.Context, e Engine, ex exec.Executor) error {
	if ex.Simulated() {
		return nil
	}
	bin, args := e.ValidateArgs()
	if _, err := os.Stat(bin); err != nil {
		return nil
	}
	if _, err := ex.Run(ctx, exec.Command{Name: bin, Args: args}); err != nil {
		return fmt.Errorf("configuration refusée par %s %v : %w", bin, args, err)
	}
	return nil
}
