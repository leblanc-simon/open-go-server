package web

import (
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

// R4-04 (D119) : sous nginx, le vhost PHP n'exécutait que le front controller —
// en redirection interne seulement — et répondait 404 à tout autre script :
// back-office et webservice de Prestashop, scripts de ses modules, /index.php
// appelé directement, tout script d'un site php-plain autre que index.php.
// Chaque type a désormais sa liste de points d'entrée, comme les .htaccess
// livrés le décident sous Apache (D99). Ce test lit le rendu par l'algorithme de
// choix des location ; TestNginxReelPointsDEntreePHP en éprouve l'effet sur le
// vrai nginx, répertoire d'administration compris.
func TestNginxPointsDEntreePHPParType(t *testing.T) {
	// juger dit si une URL est confiée à FPM, et si c'est sous la condition
	// du répertoire d'administration.
	type sort int
	const (
		execute sort = iota
		administration
		interne
		refuse
	)
	juger := func(t *testing.T, locs []locationNginx, uri string) sort {
		t.Helper()
		l := choisirLocation(t, locs, uri)
		switch {
		case strings.Contains(l.corps, "fastcgi_pass ") && strings.Contains(l.corps, "internal;"):
			return interne
		case strings.Contains(l.corps, "fastcgi_pass ") && strings.Contains(l.corps, `if ($ogs_admin = "")`):
			return administration
		case strings.Contains(l.corps, "fastcgi_pass ") && !l.refuse():
			return execute
		}
		return refuse
	}
	noms := map[sort]string{execute: "exécuté", administration: "exécuté sous le répertoire d'administration",
		interne: "interne", refuse: "refusé"}
	cas := []struct {
		nom          string
		kind         model.AppKind
		version, php string
		boutique     string
		voulus       map[string]sort
	}{
		{nom: "php-plain", kind: model.KindPHPPlain, php: "8.5", voulus: map[string]sort{
			"/index.php": execute, "/outil.php": execute, "/admin/console.php": execute,
			"/sous/script.php/chemin/info": execute, "/.git/hook.php": refuse,
		}},
		{nom: "symfony", kind: model.KindSymfony, version: "6.4", php: "8.5", voulus: map[string]sort{
			"/index.php": interne, "/index.php/route": interne, "/autre.php": refuse, "/bundles/x/porte.php": refuse,
		}},
		{nom: "prestashop-1.7", kind: model.KindPrestashop, php: "8.2", boutique: "1.7.8.11", voulus: map[string]sort{
			"/index.php": execute, "/webservice/dispatcher.php": execute, "/modules/m/ajax.php": execute,
			"/js/retro-compat.js.php": execute, "/js/jquery/jquery.noConflict.php": execute,
			"/css/retro-compat.css.php": execute, "/admin634xyz/ajax.php": administration,
			"/admin634xyz/index.php/sell/catalog": administration, "/init.php": administration,
			"/js/porte.php": administration, "/img/porte.php": refuse, "/classes/Cart.php": refuse,
		}},
		{nom: "prestashop-8", kind: model.KindPrestashop, php: "8.2", boutique: "8.1.7", voulus: map[string]sort{
			"/index.php": execute, "/webservice/dispatcher.php": execute, "/modules/m/ajax.php": execute,
			"/js/jquery/jquery.noConflict.php": execute, "/css/retro-compat.css.php": administration,
			"/admin634xyz/get-file-admin.php": administration, "/js/porte.php": refuse, "/upload/x.php": refuse,
			"/themes/classic/porte.php": refuse,
		}},
		{nom: "prestashop-9", kind: model.KindPrestashop, php: "8.2", boutique: "9.0.0", voulus: map[string]sort{
			"/index.php": execute, "/webservice/dispatcher.php": execute, "/modules/m/ajax.php": refuse,
			"/js/retro-compat.js.php": refuse, "/admin-api/index.php": administration,
			"/admin634xyz/index.php": administration,
		}},
	}
	for _, c := range cas {
		for _, tls := range []bool{false, true} {
			t.Run(c.nom+map[bool]string{false: "/http", true: "/tls"}[tls], func(t *testing.T) {
				prov, _ := fixture(t, "nginx")
				if tls {
					prov.Certificate = certificatCouvrant("shop.fr", "www.shop.fr")
				}
				p := project(t, "shop.fr", c.kind, c.version, c.php)
				if c.boutique != "" {
					preparerBoutique(t, p, c.boutique)
				}
				bp := buildMain(t, prov, p)
				locs := locationsNginx(serveurDuDocroot(t, string(bp.Content), p.Spec.Layout.Docroot))
				for uri, voulu := range c.voulus {
					if obtenu := juger(t, locs, uri); obtenu != voulu {
						t.Errorf("%s : %s, attendu %s", uri, noms[obtenu], noms[voulu])
					}
				}
			})
		}
	}
}
