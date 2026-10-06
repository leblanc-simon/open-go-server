package web

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/fcgi"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

// Sous les deux moteurs, ogs porte dans le template les règles des .htaccess que
// livre Prestashop (D99) : Apache ne les lit plus (AllowOverride None), nginx ne
// les a jamais lus. Elles diffèrent d'une branche à l'autre : le template les
// rend d'après la version en place. Ces tests éprouvent les règles rendues sur
// des URL, non leur seule présence ; les fixtures (testdata/prestashop) sont les
// .htaccess livrés par les sources de 1.7.8.11, 8.1.7 et 9.0.0 — ceux de 8.2.1
// sont identiques à ceux de 8.1.7 —, le répertoire d'administration renommé, et
// les .htaccess racine engendrés par le vrai Tools::generateHtaccess de chaque
// version, pour une boutique ordinaire, en sous-répertoire (/sub/) et à URI
// virtuelle (shop2/).

// versionsDeReference sont les versions dont les fixtures sont tirées, et la
// branche que chacune doit recevoir.
var versionsDeReference = map[string]string{"1.7.8.11": "1.7", "8.1.7": "8", "9.0.0": "9"}

func fixturePrestashop(t *testing.T, rel string) []byte {
	t.Helper()
	c, err := os.ReadFile(filepath.Join("testdata", "prestashop", rel))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ecrireDansDocroot(t *testing.T, p *model.Project, rel string, contenu []byte) {
	t.Helper()
	f := filepath.Join(p.Spec.Layout.Docroot, rel)
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, contenu, 0o644); err != nil {
		t.Fatal(err)
	}
}

// preparerBoutique dépose dans le docroot le code minimal d'une boutique de la
// version donnée : son index.php et le fichier où elle porte sa version, tel que
// les sources l'écrivent. "" : code déployé, version introuvable.
func preparerBoutique(t *testing.T, p *model.Project, version string) {
	t.Helper()
	ecrireDansDocroot(t, p, "index.php", []byte("<?php\n"))
	noyau := []byte("<?php\nclass AppKernel extends Kernel\n{\n")
	switch version {
	case "":
	case "1.7.8.11":
		ecrireDansDocroot(t, p, "app/AppKernel.php", append(noyau, fixturePrestashop(t, version+"/AppKernel.php.extrait")...))
	default:
		ecrireDansDocroot(t, p, "src/Core/Version.php",
			append([]byte("<?php\nfinal class Version\n{\n"), fixturePrestashop(t, version+"/Version.php.extrait")...))
		if version == "8.1.7" {
			// En 8 et en 9, AppKernel reprend la constante sans la porter.
			ecrireDansDocroot(t, p, "app/AppKernel.php", append(noyau, fixturePrestashop(t, version+"/AppKernel.php.extrait")...))
		}
	}
}

// boutique rend le vhost principal d'un Prestashop de la version donnée.
func boutique(t *testing.T, prov *Provider, version string) (*model.Project, *VHostPlan) {
	t.Helper()
	p := project(t, "boutique.fr", model.KindPrestashop, "", "8.2")
	preparerBoutique(t, p, version)
	return p, buildMain(t, prov, p)
}

func TestLaVersionDePrestashopSeLitDansSesSources(t *testing.T) {
	for version, branche := range versionsDeReference {
		p := project(t, "boutique.fr", model.KindPrestashop, "", "8.2")
		preparerBoutique(t, p, version)
		if v := VersionPrestashop(p); v != version || BranchePrestashop(v) != branche {
			t.Errorf("%s : version lue %q, branche %q ; attendu %q", version, v, BranchePrestashop(v), branche)
		}
	}
	for v, b := range map[string]string{"1.6.1.24": "", "": "", "9.1.0": "9", "10.0.0": "9", "8.2.1": "8", "1.7.0.0": "1.7"} {
		if got := BranchePrestashop(v); got != b {
			t.Errorf("branche de %q : %q, attendu %q", v, got, b)
		}
	}
	// Un fichier de version atteint par un lien n'est pas lu (D82) : le compte du
	// projet choisirait sinon ce que root lit.
	p := project(t, "boutique.fr", model.KindPrestashop, "", "8.2")
	preparerBoutique(t, p, "")
	ailleurs := t.TempDir()
	dehors := filepath.Join(ailleurs, "Core", "Version.php")
	if err := os.MkdirAll(filepath.Dir(dehors), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dehors, []byte("<?php\n    public const VERSION = '9.0.0';\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p.Spec.Layout.Docroot, "src", "Core"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dehors, filepath.Join(p.Spec.Layout.Docroot, "src", "Core", "Version.php")); err != nil {
		t.Fatal(err)
	}
	if v := VersionPrestashop(p); v != "" {
		t.Errorf("un Version.php lié a été lu : %q", v)
	}
	// Un répertoire src lié non plus.
	if err := os.RemoveAll(filepath.Join(p.Spec.Layout.Docroot, "src")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ailleurs, filepath.Join(p.Spec.Layout.Docroot, "src")); err != nil {
		t.Fatal(err)
	}
	if v := VersionPrestashop(p); v != "" {
		t.Errorf("un src/ lié a été parcouru : %q", v)
	}
}

// Le vhost dit pour quelle branche il est rendu ; une version indéterminée reçoit
// les règles de la branche par défaut.
func TestLeVhostPrestashopSuitLaBrancheEnPlace(t *testing.T) {
	for _, moteur := range []string{"apache", "nginx"} {
		prov, _ := fixture(t, moteur)
		for version, branche := range map[string]string{"1.7.8.11": "1.7", "8.1.7": "8", "9.0.0": "9", "": BrancheParDefaut} {
			_, bp := boutique(t, prov, version)
			if !RenduPourBranche(bp.Content, branche) {
				t.Errorf("%s, version %q : le vhost ne porte pas « %s »", moteur, version, MarquePrestashop(branche))
			}
		}
		// Une version corrective ne change pas le rendu : le vhost n'est ni
		// réécrit ni rechargé à chaque mise à jour de la boutique.
		rendu := func(v string) string {
			p := project(t, "boutique.fr", model.KindPrestashop, "", "8.2")
			ecrireDansDocroot(t, p, "index.php", []byte("<?php\n"))
			ecrireDansDocroot(t, p, "src/Core/Version.php", []byte("<?php\n    public const VERSION = '"+v+"';\n"))
			c := string(buildMain(t, prov, p).Content)
			c = strings.ReplaceAll(c, p.Spec.Layout.Root, "RACINE")
			c = strings.ReplaceAll(c, p.Spec.Layout.LogDir, "JOURNAUX")
			// L'en-tête d'ogs porte un horodatage : seul le corps compte.
			lignes := strings.Split(c, "\n")
			for len(lignes) > 0 && strings.HasPrefix(lignes[0], "#") {
				lignes = lignes[1:]
			}
			return strings.Join(lignes, "\n")
		}
		if a, b := rendu("9.0.1"), rendu("9.0.2"); a != b {
			t.Errorf("%s : 9.0.1 et 9.0.2 rendent deux vhosts différents", moteur)
		}
		if a, b := rendu("9.1.0-beta.1"), rendu("9.0.2"); a != b {
			t.Errorf("%s : une préversion de la 9 doit recevoir les règles de la 9", moteur)
		}
	}
}

// reglesImages extrait, dans l'ordre, les réécritures d'images d'un vhost
// Prestashop : sous Apache, les RewriteRule du <Directory> du docroot, motifs
// relatifs au répertoire ; sous nginx, les rewrite du server, motifs absolus.
func reglesImages(t *testing.T, moteur, vhost string) [][2]string {
	t.Helper()
	var out [][2]string
	for _, l := range strings.Split(vhost, "\n") {
		champs := strings.Fields(strings.TrimSpace(l))
		if len(champs) < 3 {
			continue
		}
		if (moteur == "apache" && champs[0] == "RewriteRule" || moteur == "nginx" && champs[0] == "rewrite") &&
			strings.Contains(champs[2], "img/") {
			out = append(out, [2]string{champs[1], champs[2]})
		}
	}
	if len(out) == 0 {
		t.Fatalf("aucune réécriture d'image dans le vhost %s :\n%s", moteur, vhost)
	}
	return out
}

// reecrire applique la première règle qui prend l'URL, comme [L] et last.
func reecrire(regles [][2]string, sujet string) string {
	for _, r := range regles {
		re := regexp.MustCompile(r[0])
		if m := re.FindStringSubmatchIndex(sujet); m != nil {
			return string(re.ExpandString(nil, r[1], sujet, m))
		}
	}
	return ""
}

// Prestashop engendre une règle d'image de produit par longueur d'identifiant, de
// 1 à 7 chiffres : n'en reprendre que deux faisait perdre ses visuels à toute
// boutique de plus de 99 images. La 1.7 ne réécrit que le .jpg, la 8 admet
// .jpeg, .webp, .png et .avif, la 9 les vignettes de catégorie « _thumb ».
func TestLesImagesPrestashopSontReecritesQuelleQueSoitLaLongueurDeLIdentifiant(t *testing.T) {
	communs := map[string]string{
		"/1-home_default/vue.jpg":       "img/p/1/1-home_default.jpg",
		"/12/vue.jpg":                   "img/p/1/2/12.jpg",
		"/123-home_default/vue.jpg":     "img/p/1/2/3/123-home_default.jpg",
		"/123/vue.jpg":                  "img/p/1/2/3/123.jpg",
		"/1234567-home_default/vue.jpg": "img/p/1/2/3/4/5/6/7/1234567-home_default.jpg",
		"/c/3-category_default/vue.jpg": "img/c/3-category_default.jpg",
		"/c/fr-default/vue.jpg":         "img/c/fr-default.jpg",
		"/12345678/vue.jpg":             "", // Prestashop s'arrête à 7 chiffres
		"/produit.jpg":                  "",
	}
	parBranche := map[string]map[string]string{
		"1.7.8.11": {
			"/1234567-home_default/vue.jpeg": "",
			"/45-large_default/vue.webp":     "",
			"/c/3_thumb-small_default/v.jpg": "",
		},
		"8.1.7": {
			"/1234567-home_default/vue.jpeg": "img/p/1/2/3/4/5/6/7/1234567-home_default.jpeg",
			"/45-large_default/vue.webp":     "img/p/4/5/45-large_default.webp",
			"/c/fr/vue.png":                  "img/c/fr.png",
			"/c/3_thumb-small_default/v.jpg": "",
		},
		"9.0.0": {
			"/1234567-home_default/vue.jpeg": "img/p/1/2/3/4/5/6/7/1234567-home_default.jpeg",
			"/45-large_default/vue.avif":     "img/p/4/5/45-large_default.avif",
			"/c/3_thumb-small_default/v.jpg": "img/c/3_thumb-small_default.jpg",
		},
	}
	for _, moteur := range []string{"apache", "nginx"} {
		for version, propres := range parBranche {
			t.Run(moteur+"/"+version, func(t *testing.T) {
				prov, _ := fixture(t, moteur)
				_, bp := boutique(t, prov, version)
				regles := reglesImages(t, moteur, string(bp.Content))
				cas := map[string]string{}
				for k, v := range communs {
					cas[k] = v
				}
				for k, v := range propres {
					cas[k] = v
				}
				for uri, voulu := range cas {
					sujet := uri
					if moteur == "apache" {
						sujet = strings.TrimPrefix(uri, "/") // contexte <Directory>
					}
					if obtenu := strings.TrimPrefix(reecrire(regles, sujet), "/"); obtenu != voulu {
						t.Errorf("%s → %q, attendu %q", uri, obtenu, voulu)
					}
				}
			})
		}
	}
}

// Ce que refusent les .htaccess livrés — répertoires entiers, listes blanches
// par extension d'img/, themes/ et js/, modèles, déclarations et scripts de
// modules, sauvegardes du back-office — est refusé par le template, branche par
// branche, et ce qu'ils ouvrent reste servi. Les listes reprennent ce qu'a
// relevé la comparaison URL par URL sur un vrai Apache servant les .htaccess
// livrés de chaque version.
func TestLeVhostPrestashopRefuseCeQueSesHtaccessRefusent(t *testing.T) {
	refusesPartout := []string{
		"/app/config/parameters.php", "/var/logs/dev.log", "/download/produit.zip",
		"/mails/fr/order_conf.html", "/config/settings.inc.php", "/config/xml/themes.xml",
		"/classes/Cart.php", "/vendor/autoload.php", "/docs/readme_fr.txt", "/docs",
		"/translations/fr-FR/Shop.xlf", "/tools/profiling/Profiler.php", "/img/notes.txt",
		"/img/p/porte.php", "/modules/m/views/templates/hook/t.tpl",
		"/mails/themes/modern/core/order_conf.html.twig", "/bin/console", "/src/Kernel.php",
		"/controllers/x.php", "/override/x.php", "/localization/fr.xml", "/cache/class_index.php",
		"/pdf/invoice.tpl", "/templates/index.tpl", "/themes/classic/config/theme.yml",
	}
	servisPartout := []string{
		"/img/p/1/1-home_default.jpg", "/img/c/3.JPG", "/themes/classic/assets/css/theme.css",
		"/themes/classic/assets/img/logo.svg", "/js/jquery/jquery-3.7.1.min.js",
		"/docs/csv_import/products_import.csv", "/translations/cldr/datas/main/fr.json",
		"/tools/foundation/foundation.js", "/mails/themes/modern/assets/logo.png",
		"/modules/m/views/css/front.css", "/modules/m/logo.png", "/pdf/invoice.css",
		"/admin634xyz/themes/default/css/admin.css", "/fr/3-vetements",
	}
	// servisApache : ce qu'Apache exécute et que nginx refuse, faute d'être un
	// point d'entrée de la branche (D119) — un .php de js/ ou de themes/ en 1.7.
	type attendus struct{ refuses, servis, servisApache []string }
	parBranche := map[string]attendus{
		"1.7.8.11": {
			refuses: []string{"/themes/classic/_dev/js/theme.js", "/img/logo.webp"},
			servis: []string{"/js/vendor/bower.json", "/themes/classic/assets/js/theme.js.map",
				"/themes/classic/README.md", "/upload/perso.jpg", "/modules/m/config.xml", "/archive.git/HEAD",
				"/js/retro-compat.js.php", "/modules/m/ajax.php", "/webservice/dispatcher.php",
				"/css/retro-compat.css.php"},
			servisApache: []string{"/js/porte.php", "/themes/classic/porte.php"},
		},
		"8.1.7": {
			refuses: []string{"/modules/m/config.xml", "/modules/m/myconfig.xml", "/modules/m/config_fr.xml",
				"/upload/perso.jpg", "/upload/", "/js/porte.php", "/js/vendor/bower.json", "/themes/classic/porte.php",
				"/themes/classic/assets/js/theme.js.map", "/themes/classic/README.md", "/themes/classic/x.avif"},
			servis: []string{"/img/logo.webp", "/modules/m/readme.txt", "/modules/m/data.json", "/archive.git/HEAD",
				"/js/retro-compat.js.php", "/js/jquery/jquery.noConflict.php",
				"/modules/m/ajax.php", "/webservice/dispatcher.php"},
		},
		"9.0.0": {
			refuses: []string{"/modules/m/ajax.php", "/modules/m/logs/a.log", "/modules/m/readme.txt",
				"/modules/m/data.json", "/modules/m/services.yml", "/modules/m/config.xml",
				"/js/jquery/jquery.noConflict.php", "/js/retro-compat.js.php", "/upload/perso.jpg",
				"/archive.git/HEAD", "/themes/classic/porte.php"},
			servis: []string{"/img/logo.webp", "/themes/classic/x.avif", "/webservice/dispatcher.php"},
		},
	}
	for _, moteur := range []string{"apache", "nginx"} {
		for version, a := range parBranche {
			for _, tls := range []bool{false, true} {
				t.Run(moteur+"/"+version+map[bool]string{false: "/http", true: "/tls"}[tls], func(t *testing.T) {
					prov, _ := fixture(t, moteur)
					if tls {
						prov.Certificate = certificatCouvrant("boutique.fr", "www.boutique.fr")
					}
					p, bp := boutique(t, prov, version)
					c := string(bp.Content)
					refuse := func(uri string) bool {
						if moteur == "nginx" {
							return choisirLocation(t, locationsNginx(serveurDuDocroot(t, c, p.Spec.Layout.Docroot)), uri).refuse()
						}
						return refuseApache(t, vhostApacheDuDocroot(t, c, p.Spec.Layout.Docroot), uri)
					}
					for _, uri := range append(append([]string{}, refusesPartout...), a.refuses...) {
						if !refuse(uri) {
							t.Errorf("%s doit être refusé (un .htaccess de Prestashop %s le refuse)", uri, version)
						}
					}
					servis := append(append([]string{}, servisPartout...), a.servis...)
					if moteur == "apache" {
						servis = append(servis, a.servisApache...)
					} else {
						for _, uri := range a.servisApache {
							if !refuse(uri) {
								t.Errorf("%s n'est pas un point d'entrée de Prestashop %s : nginx le refuse (D119)", uri, version)
							}
						}
					}
					for _, uri := range servis {
						if refuse(uri) {
							t.Errorf("%s doit rester servi (Prestashop %s l'ouvre)", uri, version)
						}
					}
				})
			}
		}
	}
}

// Sauvegardes, exports et imports d'un répertoire d'administration ne se
// refusent que sous un répertoire réel (D99) : une URL propre de même forme
// (/fr/import/12-produit.html, /module/export/ajax) va au front controller. Le
// contrôle porte sur le disque, que les lectures de vhost de ces tests ne voient
// pas : on vérifie ici sa forme, le vrai Apache (TestApacheReelPrestashopParBranche),
// le scénario nginx et la comparaison URL par URL en éprouvent l'effet.
func TestLesSauvegardesDuBackOfficeNeSontRefuseesQueSousUnRepertoireReel(t *testing.T) {
	for version := range versionsDeReference {
		for moteur, voulus := range map[string][]string{
			"apache": {"RewriteCond %{DOCUMENT_ROOT}/$1 -d", "RewriteRule ^/([^/]+)/(backups|export|import)(/|$) - [F]"},
			"nginx": {"set $ogs_rep_prive /dev/null/ogs;", `if ($uri ~ "^/([^/]+)/(backups|export|import)(/|$)") {`,
				"set $ogs_rep_prive $document_root/$1;", "if (-d $ogs_rep_prive) {"},
		} {
			prov, _ := fixture(t, moteur)
			p, bp := boutique(t, prov, version)
			var c string
			if moteur == "nginx" {
				c = serveurDuDocroot(t, string(bp.Content), p.Spec.Layout.Docroot)
			} else {
				c = vhostApacheDuDocroot(t, string(bp.Content), p.Spec.Layout.Docroot)
				// De portée vhost, hors du <Directory> : elle précède ses réécritures.
				for _, s := range sectionsApache(c) {
					if strings.Contains(s.corps, "(backups|export|import)") {
						t.Errorf("apache %s : le refus doit être de portée vhost, non dans <%s>", version, s.balise)
					}
				}
			}
			for _, v := range voulus {
				if !strings.Contains(c, v) {
					t.Errorf("%s %s : %q absent du vhost", moteur, version, v)
				}
			}
			if strings.Contains(c, `"^/[^/]+/(backups|export|import)`) || strings.Contains(c, "location ~ ^/[^/]+/(backups") {
				t.Errorf("%s %s : refus sans condition de répertoire réel", moteur, version)
			}
		}
	}
}

func TestLeHtaccessRacineNeSignaleQueCeQueLeTemplateNeReprendPas(t *testing.T) {
	for version := range versionsDeReference {
		ordinaire := fixturePrestashop(t, version+"/racine-ordinaire.htaccess")
		if c := ConstatsHtaccessRacine(ordinaire, true); len(c) != 0 {
			t.Errorf("%s : une boutique à la racine, sans règle propre, ne doit rien signaler : %v", version, c)
		}
		c := strings.Join(ConstatsHtaccessRacine(fixturePrestashop(t, version+"/racine-sub.htaccess"), false), " ; ")
		if !strings.Contains(c, "sous-répertoire") || !strings.Contains(c, "/sub/") {
			t.Errorf("%s : REWRITEBASE /sub/ non signalé : %q", version, c)
		}
		c = strings.Join(ConstatsHtaccessRacine(fixturePrestashop(t, version+"/racine-virtuelle.htaccess"), false), " ; ")
		if !strings.Contains(c, "URI virtuelle") || !strings.Contains(c, "/shop2/") {
			t.Errorf("%s : URI virtuelle non signalée : %q", version, c)
		}
		propre := append(append([]byte{}, ordinaire...), "\n\nRedirect 301 /ancienne-page /nouvelle-page\n"...)
		if c := ConstatsHtaccessRacine(propre, false); len(c) != 0 {
			t.Errorf("%s : le plan ne signale que sous-répertoire et URI virtuelle : %v", version, c)
		}
		if c := strings.Join(ConstatsHtaccessRacine(propre, true), " ; "); !strings.Contains(c, "hors du bloc") {
			t.Errorf("%s : une règle hors du bloc ~~start~~/~~end~~ doit être signalée par doctor : %q", version, c)
		}
		// Toutes les options de generateHtaccess actives (cache, MultiViews,
		// mod_security, serveurs de médias, images « legacy », hôte CGI) et URI
		// virtuelle : rien d'inconnu dans le bloc.
		options := fixturePrestashop(t, version+"/racine-options.htaccess")
		for _, c := range ConstatsHtaccessRacine(options, true) {
			if !strings.Contains(c, "URI virtuelle") {
				t.Errorf("%s : options de generateHtaccess prises pour des règles propres : %q", version, c)
			}
		}
		// Une règle qu'un module glisse dans le bloc est signalée par doctor.
		injecte := strings.Replace(string(ordinaire), "RewriteEngine on\n",
			"RewriteEngine on\nRewriteRule ^promo$ /module/promo/landing [L]\n", 1)
		if c := strings.Join(ConstatsHtaccessRacine([]byte(injecte), true), " ; "); !strings.Contains(c, "dans le bloc") {
			t.Errorf("%s : une règle injectée dans le bloc doit être signalée : %q", version, c)
		}
	}
}

// Les .htaccess de sous-répertoires que livrent Prestashop 1.7, 8 et 9 sont
// couverts par le template — doctor les taisait en 8 seulement, et rendait le
// code 6 à toute 1.7 et toute 9 —; une directive qu'on y ajoute ne l'est pas.
func TestLesHtaccessLivresParPrestashopSontCouverts(t *testing.T) {
	vus := 0
	for version := range versionsDeReference {
		racine := filepath.Join("testdata", "prestashop", version, "arbre")
		err := filepath.WalkDir(racine, func(chemin string, d fs.DirEntry, err error) error {
			if err != nil || d.Name() != ".htaccess" {
				return err
			}
			vus++
			contenu, err := os.ReadFile(chemin)
			if err != nil {
				return err
			}
			if !HtaccessSousRepertoireCouvert(contenu) {
				t.Errorf("%s, livré par Prestashop, n'est pas reconnu comme couvert", chemin)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if vus < 90 {
		t.Fatalf("%d .htaccess de référence seulement : fixtures incomplètes", vus)
	}
	ajout := append(fixturePrestashop(t, "8.1.7/arbre/img/.htaccess"), "\nRewriteRule ^promo$ /fr/promotions [R=301,L]\n"...)
	if HtaccessSousRepertoireCouvert(ajout) {
		t.Error("une règle ajoutée à un .htaccess livré doit être signalée")
	}
	if HtaccessSignificatif([]byte("# rien\n\n   \n")) {
		t.Error("un .htaccess fait de commentaires n'a rien à signaler")
	}
}

// Le parcours ne suit aucun lien — le docroot appartient à l'utilisateur (D51) —
// et un docroot lui-même lié n'est pas lu.
func TestListerHtaccessNeSuitAucunLien(t *testing.T) {
	p := project(t, "boutique.fr", model.KindPrestashop, "", "8.2")
	docroot := p.Spec.Layout.Docroot
	ecrire := func(rel string) {
		t.Helper()
		ecrireDansDocroot(t, p, filepath.Join(rel, ".htaccess"), []byte("Require all denied\n"))
	}
	ecrire(".")
	ecrire("app")
	ecrire("modules/m")
	ecrire("vendor/lib")
	dehors := t.TempDir()
	if err := os.WriteFile(filepath.Join(dehors, ".htaccess"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dehors, filepath.Join(docroot, "lien")); err != nil {
		t.Fatal(err)
	}
	reps, err := ListerHtaccess(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(reps, ","); got != ".,app,modules/m" {
		t.Errorf("répertoires à .htaccess : %q, attendu « .,app,modules/m » (vendor ignoré, lien non suivi)", got)
	}
	if c, err := LireHtaccess(p, "lien"); err == nil && c != nil {
		t.Errorf("un .htaccess atteint par un lien a été lu : %q", c)
	}
	// Le docroot lui-même remplacé par un lien.
	if err := os.Rename(docroot, docroot+".orig"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dehors, docroot); err != nil {
		t.Fatal(err)
	}
	if reps, err := ListerHtaccess(p); err == nil {
		t.Errorf("un docroot lié a été parcouru : %v", reps)
	}
}

// etapePrestashop planifie le vhost d'un Prestashop dont le docroot porte
// htaccess à sa racine (rien si nil) et le code de la version donnée ("-" :
// docroot vide), et rend l'étape d'avertissement si le plan en porte une.
func etapePrestashop(t *testing.T, moteur string, htaccess []byte, version string) (string, bool, error) {
	t.Helper()
	prov, _ := fixture(t, moteur)
	p := project(t, "boutique.fr", model.KindPrestashop, "", "8.2")
	if err := os.MkdirAll(p.Spec.Layout.Docroot, 0o755); err != nil {
		t.Fatal(err)
	}
	if version != "-" {
		preparerBoutique(t, p, version)
	}
	if htaccess != nil {
		ecrireDansDocroot(t, p, ".htaccess", htaccess)
	}
	ctx := context.Background()
	obs, err := prov.Observe(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if strings.HasSuffix(s.Resource, "/htaccess") {
			if s.Critical {
				t.Errorf("l'avertissement ne doit pas défaire le vhost : étape critique")
			}
			return s.Summary, true, s.Apply(ctx)
		}
	}
	return "", false, nil
}

// Une boutique en sous-répertoire tombe en silence si ogs réécrit son vhost sans
// ses règles : le plan l'annonce, par une étape non critique qui nomme la sortie.
func TestLePlanAnnonceUneBoutiqueEnSousRepertoire(t *testing.T) {
	for version := range versionsDeReference {
		resume, vu, err := etapePrestashop(t, "apache", fixturePrestashop(t, version+"/racine-sub.htaccess"), version)
		if !vu || err == nil || !strings.Contains(resume, "/sub/") ||
			!strings.Contains(err.Error(), "docroot") || !strings.Contains(err.Error(), "D63") {
			t.Errorf("%s, sous-répertoire : étape %v, résumé %q, erreur %v", version, vu, resume, err)
		}
		if _, vu, _ := etapePrestashop(t, "apache", fixturePrestashop(t, version+"/racine-virtuelle.htaccess"), version); !vu {
			t.Errorf("%s : une URI virtuelle doit être annoncée au plan", version)
		}
		if r, vu, _ := etapePrestashop(t, "apache", fixturePrestashop(t, version+"/racine-ordinaire.htaccess"), version); vu {
			t.Errorf("%s : une boutique à la racine ne mérite aucun avertissement : %q", version, r)
		}
		if r, vu, _ := etapePrestashop(t, "nginx", fixturePrestashop(t, version+"/racine-ordinaire.htaccess"), version); vu {
			t.Errorf("%s : nginx, version connue, rien à annoncer : %q", version, r)
		}
	}
	if _, vu, _ := etapePrestashop(t, "apache", nil, "8.1.7"); vu {
		t.Error("sans .htaccess, rien à annoncer")
	}
	// La boutique installée dans www/sub/ — son .htaccess racine est celui de
	// sub/, que REWRITEBASE /sub/ désigne — est annoncée aussi.
	prov, _ := fixture(t, "apache")
	p := project(t, "boutique.fr", model.KindPrestashop, "", "8.2")
	ecrireDansDocroot(t, p, "sub/.htaccess", fixturePrestashop(t, "8.1.7/racine-sub.htaccess"))
	ecrireDansDocroot(t, p, "sub/img/.htaccess", fixturePrestashop(t, "8.1.7/arbre/img/.htaccess"))
	obs, err := prov.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), p, obs)
	if err != nil {
		t.Fatal(err)
	}
	vu := false
	for _, s := range steps {
		if strings.HasSuffix(s.Resource, "/htaccess") {
			vu = strings.Contains(s.Summary, "sub/ : boutique en sous-répertoire (REWRITEBASE /sub/)")
		}
	}
	if !vu {
		t.Error("une boutique installée dans un sous-répertoire du docroot doit être annoncée au plan")
	}
}

// Une version indéterminée, code en place, est servie avec les règles de la
// branche par défaut : le plan le dit sous les deux moteurs, d'une étape non
// critique. Un docroot encore vide — premier apply, code pas encore déployé — ne
// dit rien.
func TestLePlanAnnonceUneVersionDePrestashopIndeterminee(t *testing.T) {
	for _, moteur := range []string{"apache", "nginx"} {
		resume, vu, err := etapePrestashop(t, moteur, nil, "")
		if !vu || err == nil || !strings.Contains(resume, "indéterminée") ||
			!strings.Contains(err.Error(), "règles de la "+BrancheParDefaut) {
			t.Errorf("%s : étape %v, résumé %q, erreur %v", moteur, vu, resume, err)
		}
		if r, vu, _ := etapePrestashop(t, moteur, nil, "-"); vu {
			t.Errorf("%s : un docroot vide ne mérite aucun avertissement : %q", moteur, r)
		}
	}
}

// fauxFPM répond sur un socket Unix par le script que le serveur web lui demande
// d'exécuter, relatif au docroot : la réponse dit où une URL a été conduite.
func fauxFPM(t *testing.T, docroot string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fpm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go fcgi.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		script := fcgi.ProcessEnv(r)["SCRIPT_FILENAME"]
		if i := strings.Index(script, "fcgi://localhost"); i >= 0 {
			script = script[i+len("fcgi://localhost"):]
		}
		if st, err := os.Stat(script); err != nil || st.IsDir() {
			w.WriteHeader(http.StatusNotFound)
		}
		fmt.Fprintf(w, "FPM %s", strings.TrimPrefix(script, docroot))
	}))
	return sock
}

// TestApacheReelPrestashopParBranche sert, sur le vrai Apache, le vhost rendu
// pour chaque branche, avec un faux FPM, et relève où chaque URL est conduite :
// ce sont les écarts que la comparaison URL par URL avec les .htaccess livrés
// avait relevés (D99) — sauvegardes et scripts de modules de la 9, admin-api/,
// vignettes « _thumb », upload/ de la 1.7, listes blanches de la 1.7.
//
//	OGS_TEST_APACHE=1 go test ./internal/provider/web/ -run TestApacheReelPrestashop
func TestApacheReelPrestashopParBranche(t *testing.T) {
	if os.Getenv("OGS_TEST_APACHE") == "" {
		t.Skip("test sur le vrai Apache désactivé : exporter OGS_TEST_APACHE=1 pour l'exécuter")
	}
	const apache, modules = "/usr/sbin/apache2", "/usr/lib/apache2/modules"
	for _, f := range []string{apache, modules, "/etc/mime.types"} {
		if _, err := os.Stat(f); err != nil {
			t.Skipf("%s absent", f)
		}
	}
	fichiers := []string{
		"img/p/1/2/3/123-home_default.jpg", "img/p/1/2/3/123-home_default.webp",
		"img/c/3_thumb-category_default.jpg", "upload/perso.jpg",
		"admin634xyz/cron_currency_rates.php", "admin634xyz/index.php", "admin634xyz/backups/sauvegarde.sql.bz2",
		"admin634xyz/themes/default/css/admin.css", "admin-api/index.php",
		"modules/m/ajax.php", "modules/m/logs/a.log", "modules/m/views/css/front.css",
		"js/jquery/jquery.noConflict.php", "js/retro-compat.js.php", "js/vendor/bower.json",
		"themes/classic/_dev/js/theme.js", "themes/classic/assets/js/theme.js.map",
		"webservice/dispatcher.php", "img/porte.php", "themes/porte.php", "js/porte.php",
	}
	attendus := map[string]map[string]string{
		"1.7.8.11": {
			"/123-home_default/a.jpg":                   "200 img/p/1/2/3/123-home_default.jpg",
			"/123-home_default/a.webp":                  "200 FPM /index.php",
			"/upload/perso.jpg":                         "200 FPM /index.php",
			"/themes/classic/_dev/js/theme.js":          "403",
			"/themes/classic/assets/js/theme.js.map":    "200 themes/classic/assets/js/theme.js.map",
			"/js/vendor/bower.json":                     "200 js/vendor/bower.json",
			"/js/absent.css":                            "200 FPM /index.php",
			"/js/absent.js":                             "200 FPM /js/retro-compat.js.php",
			"/modules/m/ajax.php":                       "200 FPM /modules/m/ajax.php",
			"/admin634xyz/sell/catalog/products":        "200 FPM /admin634xyz/index.php",
			"/admin634xyz/backups/sauvegarde.sql.bz2":   "403",
			"/admin634xyz/themes/default/css/admin.css": "200 admin634xyz/themes/default/css/admin.css",
			"/fr/import/12-produit.html":                "200 FPM /index.php",
			"/module/export/ajax":                       "200 FPM /index.php",
			"/sansmarque/backups/x.sql":                 "403",
		},
		"8.1.7": {
			"/123-home_default/a.webp":                "200 img/p/1/2/3/123-home_default.webp",
			"/upload/perso.jpg":                       "403",
			"/js/vendor/bower.json":                   "403",
			"/js/jquery/jquery.noConflict.php":        "200 FPM /js/jquery/jquery.noConflict.php",
			"/js/absent.js":                           "200 FPM /js/retro-compat.js.php",
			"/modules/m/ajax.php":                     "200 FPM /modules/m/ajax.php",
			"/modules/m/ajax.php/suite":               "200 FPM /modules/m/ajax.php",
			"/img/porte.php/a.jpg":                    "403",
			"/themes/porte.php/a.css":                 "403",
			"/modules/m/logs/a.log":                   "200 modules/m/logs/a.log",
			"/c/3_thumb-category_default/x.jpg":       "200 FPM /index.php",
			"/admin634xyz/backups/sauvegarde.sql.bz2": "403",
			"/admin634xyz/get-file-admin.php":         "404 FPM /admin634xyz/get-file-admin.php",
			"/api/products":                           "200 FPM /webservice/dispatcher.php",
		},
		"9.0.0": {
			"/c/3_thumb-category_default/x.jpg":       "200 img/c/3_thumb-category_default.jpg",
			"/upload/perso.jpg":                       "403",
			"/modules/m/ajax.php":                     "403",
			"/modules/m/ajax.php/suite":               "403",
			"/img/porte.php/a.jpg":                    "403",
			"/themes/porte.php/a.css":                 "403",
			"/js/porte.php/a.js":                      "403",
			"/modules/m/logs/a.log":                   "403",
			"/modules/m/views/css/front.css":          "200 modules/m/views/css/front.css",
			"/js/jquery/jquery.noConflict.php":        "403",
			"/js/retro-compat.js.php":                 "403",
			"/js/absent.js":                           "200 FPM /index.php",
			"/admin-api/oauth/token":                  "200 FPM /admin-api/index.php",
			"/admin-api/":                             "200 FPM /admin-api/index.php",
			"/admin634xyz/":                           "200 FPM /admin634xyz/index.php",
			"/admin634xyz/backups/sauvegarde.sql.bz2": "403",
			"/admin634xyz/backups/absent.sql":         "403",
			"/rapports/export/public.txt":             "403",
			"/fr/export/12-produit.html":              "200 FPM /index.php",
			"/module/import/default":                  "200 FPM /index.php",
			"/en/backups":                             "200 FPM /index.php",
			"/sansmarque/backups/x.sql":               "403",
			"/archive.git/HEAD":                       "403",
		},
	}
	for version, cas := range attendus {
		t.Run(version, func(t *testing.T) {
			prov, _ := fixture(t, "apache")
			p := project(t, "boutique.fr", model.KindPrestashop, "", "8.2")
			preparerBoutique(t, p, version)
			// sansmarque/ : un répertoire d'administration privé de son
			// cron_currency_rates.php ; ses sauvegardes restent refusées.
			for _, f := range append(fichiers, "rapports/export/public.txt", "archive.git/HEAD", "sansmarque/backups/x.sql") {
				ecrireDansDocroot(t, p, f, []byte(f+"\n"))
			}
			if err := os.MkdirAll(p.Spec.Layout.LogDir, 0o755); err != nil {
				t.Fatal(err)
			}
			bp := buildMain(t, prov, p)
			vhost := string(bp.Content)
			m := regexp.MustCompile(`proxy:unix:([^|]+)\|`).FindStringSubmatch(vhost)
			if m == nil {
				t.Fatalf("aucun socket FPM dans le vhost :\n%s", vhost)
			}
			vhost = strings.ReplaceAll(vhost, m[1], fauxFPM(t, p.Spec.Layout.Docroot))
			url := demarrerApache(t, apache, modules, vhost)
			for uri, voulu := range cas {
				req, err := http.NewRequest("GET", url+uri, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Host = "boutique.fr"
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				corps, _ := io.ReadAll(res.Body)
				res.Body.Close()
				obtenu := fmt.Sprint(res.StatusCode)
				if res.StatusCode != http.StatusForbidden {
					obtenu += " " + strings.TrimSpace(string(corps))
				}
				if obtenu != voulu {
					t.Errorf("GET %s : %q, attendu %q", uri, obtenu, voulu)
				}
			}
		})
	}
}
