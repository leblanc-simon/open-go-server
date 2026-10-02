package web

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// Sous Apache, ogs n'active plus les .htaccess (AllowOverride None, D99) : les
// règles des .htaccess que livre Prestashop sont portées par le template. Ce qui
// suit dit ce que le template ne reprend pas, pour le plan — qui l'annonce là où
// il réécrit le vhost — et pour doctor, qui le rappelle à chaque passage.

// tailleMaxHtaccess borne la lecture : le fichier appartient au compte du
// projet, qui lui donne la taille qu'il veut (D82).
const tailleMaxHtaccess = 1 << 20

var (
	reRewriteBase = regexp.MustCompile(`\[E=REWRITEBASE:([^\]]+)\]`)
	// « RewriteRule ^shop2/(.*) /$1 [L] » : l'URI virtuelle d'une boutique du
	// multi-boutique, telle que Tools::generateHtaccess l'écrit.
	reURIVirtuelle = regexp.MustCompile(`(?i)^RewriteRule\s+\^(\S+?)\(\.\*\)\s+\S*\$1\s+\[L\]$`)
	// Les directives d'accès et les conteneurs qu'emploient les .htaccess de
	// sous-répertoires livrés : le template en reprend l'effet.
	reAccesHtaccess = regexp.MustCompile(`(?i)^(</?(IfModule|Files|FilesMatch)\b.*|(Order|Deny|Allow|Require|Satisfy)\b.*|Options\s+-(Indexes|MultiViews)|DirectoryIndex\s+index\.php|SetEnv\s+HTTP_MOD_REWRITE\s+On)$`)
)

// reLignesEngendrees reconnaît, une à une, les lignes que Tools::generateHtaccess
// écrit entre ~~start~~ et ~~end~~, de la 1.7 à la 9 et quelles que soient ses
// options (cache, MultiViews, mod_security, serveurs de médias, images
// « legacy », hôte CGI) : ce qui n'y ressemble pas a été ajouté par un module ou
// à la main, et le template ne le reprend pas.
var reLignesEngendrees = func() []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, m := range []string{
		`(<IfModule !?mod_[a-z_]+\.c>|</IfModule>)`, `</?Files(Match)?( .*)?>`, `SetEnv HTTP_MOD_REWRITE On`,
		`Options -(Indexes|Multiviews)`, `RewriteEngine on`, `SecFilter(Engine|ScanPOST) Off`,
		`RewriteRule \\\.git - \[F,L\]`,
		`RewriteCond %\{HTTP:Authorization\} \^\(\.\*\)`, `RewriteRule \. - \[E=HTTP_AUTHORIZATION:%1\]`,
		`RewriteCond %\{HTTP_HOST\} \^\S+\$( \[OR\])?`, `RewriteRule \. - \[E=REWRITEBASE:\S+\]`,
		`RewriteRule \^api\(\?:/\(\.\*\)\)\?\$ %\{ENV:REWRITEBASE\}webservice/dispatcher\.php\?url=\$1 \[QSA,L\]`,
		`RewriteRule \^upload/\.\+\$ %\{ENV:REWRITEBASE\}index\.php \[QSA,L\]`,
		// URI virtuelle du multi-boutique.
		`RewriteRule \^\S+ /\S*( \[L,R\]|\$1 \[L\])`,
		`RewriteRule \^\S+ \S*index\.php \[L,R\]`,
		// Images de produit et de catégorie, « legacy » comprises.
		`RewriteRule \^\S+ %\{ENV:REWRITEBASE\}img/[pc]/\S+ \[L\]`,
		`RewriteRule \^images_ie/\?\(\[\^/\]\+\)\\\.\(jpe\?g\|png\|gif\)\$ (%\{ENV:REWRITEBASE\})?js/jquery/plugins/fancybox/images/\$1\.\$2 \[L\]`,
		`RewriteCond %\{REQUEST_FILENAME\} -[sld]( \[OR\])?`,
		`RewriteRule \^\.\*\$ (-|%\{ENV:REWRITEBASE\}index\.php) \[NC,L\]`,
		`AddType \S+ \.[a-z0-9]+`, `Header set Access-Control-Allow-Origin "\*"`,
		`Header set Content-Disposition "Attachment"`, `Header set X-Content-Type-Options "nosniff"`,
		`Order deny,allow`, `Deny from all`, `Require all denied`,
		`ExpiresActive On`, `ExpiresByType \S+ "access plus 1 (week|month|year)"`, `Header unset Etag`,
		`FileETag none`, `AddOutputFilterByType DEFLATE [a-z0-9/+. -]+`,
		`ErrorDocument 404 \S*index\.php\?controller=404`,
	} {
		out = append(out, regexp.MustCompile(`(?i)^`+m+`$`))
	}
	return out
}()

func ligneEngendree(l string) bool {
	l = strings.Join(strings.Fields(l), " ")
	for _, re := range reLignesEngendrees {
		if re.MatchString(l) {
			return true
		}
	}
	return false
}

// lignesLivrees sont les autres lignes des .htaccess de sous-répertoires que
// livre Prestashop de la 1.7 à la 9 (js/, tools/, img/, admin-api/, répertoire
// d'administration, sandbox de l'installeur) : leur effet est repris par le
// template.
var lignesLivrees = map[string]bool{}

func init() {
	for _, l := range []string{
		`RewriteEngine on`,
		`RewriteCond %{REQUEST_FILENAME} !-f`,
		`RewriteRule "([^/]*)\.js$" retro-compat.js.php?file=$1.js [QSA,L]`,
		`RewriteCond %{REQUEST_FILENAME} -s [OR]`,
		`RewriteCond %{REQUEST_FILENAME} -l [OR]`,
		`RewriteCond %{REQUEST_FILENAME} -d`,
		`RewriteRule ^.*$ - [NC,L]`,
		`RewriteRule ^.*$ ../index.php [NC,L]`,
		`RewriteCond %{REQUEST_URI}::$1 ^(/.+)/(.*)::\2$`,
		`RewriteRule ^(.*) - [E=BASE:%1]`,
		`RewriteCond %{HTTP:Authorization} .`,
		`RewriteRule .* - [E=HTTP_AUTHORIZATION:%{HTTP:Authorization}]`,
		`RewriteRule ^(cron_currency_rates|get-file-admin)\.php - [P]`,
		`RewriteCond %{QUERY_STRING} (^|&)controller=|(^|&)tab=`,
		`RewriteRule .* - [P]`,
		`RewriteCond %{REQUEST_FILENAME} -f`,
		`RewriteRule .? - [L]`,
		`RewriteRule .? %{ENV:BASE}/index.php [L]`,
		`RedirectMatch 302 ^/$ /index.php/`,
		`RewriteRule ^.*$ test.php`,
		// 1.7 : répertoire d'administration, js/, img/.
		`RewriteRule ^(ajax|ajax_products_list|ajax-tab|backup|cron_currency_rates)\.php - [P]`,
		`RewriteRule ^(displayImage|drawer|footer\.inc|functions|get-file-admin)\.php - [P]`,
		`RewriteRule ^(grider|header\.inc|init|login|password|pdf|searchcron)\.php - [P]`,
		`RewriteRule "([^/]*)\.css$" ../css/retro-compat.css.php?file=$1.css [QSA,L]`,
		`php_flag engine off`,
		// 9 : répertoire d'administration et admin-api/.
		`SSLOptions +StdEnvVars`,
	} {
		lignesLivrees[normaliserLigne(l)] = true
	}
}

// normaliserLigne ramène une ligne à une forme comparable : blancs réduits,
// casse ignorée. Rend "" pour une ligne vide ou un commentaire.
func normaliserLigne(l string) string {
	l = strings.TrimSpace(l)
	if l == "" || strings.HasPrefix(l, "#") {
		return ""
	}
	return strings.ToLower(strings.Join(strings.Fields(l), " "))
}

// lignesUtiles rend les lignes significatives d'un .htaccess, telles quelles.
func lignesUtiles(contenu []byte) []string {
	var out []string
	for _, l := range strings.Split(string(contenu), "\n") {
		if normaliserLigne(l) != "" {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}

// ConstatsHtaccessRacine dit ce que le template d'ogs ne reprend pas du
// .htaccess racine d'un Prestashop : boutique en sous-répertoire (REWRITEBASE
// autre que « / »), URI virtuelles du multi-boutique, et, si horsBloc, les
// directives posées hors du bloc ~~start~~/~~end~~ qu'engendre Prestashop — règles
// propres à la boutique ou écrites par un module — et celles qu'un module a
// glissées dans le bloc.
func ConstatsHtaccessRacine(contenu []byte, horsBloc bool) []string {
	var constats []string
	bases, virtuelles := map[string]bool{}, map[string]bool{}
	dedans, vuDebut, dehors, inconnues := false, false, 0, 0
	for _, brute := range strings.Split(string(contenu), "\n") {
		l := strings.TrimSpace(brute)
		switch {
		case strings.Contains(l, "~~start~~"):
			dedans, vuDebut = true, true
			continue
		case strings.Contains(l, "~~end~~"):
			dedans = false
			continue
		}
		if normaliserLigne(l) == "" {
			continue
		}
		if !dedans {
			dehors++
			continue
		}
		if !ligneEngendree(l) {
			inconnues++
		}
		for _, m := range reRewriteBase.FindAllStringSubmatch(l, -1) {
			if m[1] != "/" {
				bases[m[1]] = true
			}
		}
		if m := reURIVirtuelle.FindStringSubmatch(l); m != nil {
			virtuelles["/"+m[1]] = true
		}
	}
	if len(bases) > 0 {
		constats = append(constats, "boutique en sous-répertoire (REWRITEBASE "+joindre(bases)+")")
	}
	if len(virtuelles) > 0 {
		constats = append(constats, "multi-boutique à URI virtuelle ("+joindre(virtuelles)+")")
	}
	if horsBloc && inconnues > 0 {
		constats = append(constats, fmt.Sprintf("%d directive(s) dans le bloc ~~start~~/~~end~~ que "+
			"Tools::generateHtaccess n'écrit pas (règles de modules)", inconnues))
	}
	if horsBloc && dehors > 0 {
		if vuDebut {
			constats = append(constats, fmt.Sprintf("%d directive(s) hors du bloc ~~start~~/~~end~~ "+
				"(règles propres ou de modules)", dehors))
		} else {
			constats = append(constats, fmt.Sprintf("%d directive(s) dans un .htaccess racine "+
				"que Prestashop n'a pas engendré", dehors))
		}
	}
	return constats
}

// EngendreParPrestashop dit si un .htaccess porte le bloc ~~start~~/~~end~~
// qu'écrit Tools::generateHtaccess : celui de la racine d'une boutique, fût-elle
// installée dans un sous-répertoire du docroot.
func EngendreParPrestashop(contenu []byte) bool {
	return strings.Contains(string(contenu), "~~start~~")
}

// ConstatsBoutiques rend ce que le template ne reprend pas des .htaccess racine
// de boutique que porte le docroot d'un projet — le sien, et celui d'une
// boutique installée dans un sous-répertoire, que REWRITEBASE désigne —, chaque
// constat préfixé du répertoire hors la racine. Le parcours est celui de
// ListerHtaccess : sans lien suivi, borné.
func ConstatsBoutiques(proj *model.Project, horsBloc bool) []string {
	reps, err := ListerHtaccess(proj)
	if err != nil {
		return nil
	}
	var out []string
	for _, rel := range reps {
		contenu, err := LireHtaccess(proj, rel)
		if err != nil || contenu == nil || (rel != "." && !EngendreParPrestashop(contenu)) {
			continue
		}
		for _, c := range ConstatsHtaccessRacine(contenu, horsBloc) {
			if rel != "." {
				c = rel + "/ : " + c
			}
			out = append(out, c)
		}
	}
	return out
}

// HtaccessSousRepertoireCouvert dit si un .htaccess de sous-répertoire ne porte
// que ce que le template reprend : les directives d'accès et les lignes que
// Prestashop livre telles quelles.
func HtaccessSousRepertoireCouvert(contenu []byte) bool {
	for _, l := range lignesUtiles(contenu) {
		if !reAccesHtaccess.MatchString(l) && !lignesLivrees[normaliserLigne(l)] {
			return false
		}
	}
	return true
}

func joindre(m map[string]bool) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// docrootRelatif rend le docroot d'un projet relativement à sa racine, pour un
// parcours par descripteur (fs.OpenDirAt) : la racine appartient à l'utilisateur
// (D51), qui en substitue sans privilège n'importe quel composant.
func docrootRelatif(proj *model.Project) (string, bool) {
	rel, err := filepath.Rel(proj.Spec.Layout.Root, proj.Spec.Layout.Docroot)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// lireDansDocroot lit le fichier nom du répertoire rel (relatif au docroot) d'un
// projet sans suivre de lien, ni en chemin ni au dernier composant, et seulement
// s'il appartient au propriétaire du docroot, borné à max octets. Absent : rend
// nil, sans erreur.
func lireDansDocroot(proj *model.Project, rel, nom string, max int) ([]byte, error) {
	base, ok := docrootRelatif(proj)
	if !ok {
		return nil, fmt.Errorf("docroot %s hors de la racine du projet", proj.Spec.Layout.Docroot)
	}
	proprio, err := fs.OpenDirAt(proj.Spec.Layout.Root, base)
	if err != nil {
		return nil, err
	}
	uid, _, err := proprio.Owner()
	proprio.Close()
	if err != nil {
		return nil, err
	}
	d, err := fs.OpenDirAt(proj.Spec.Layout.Root, filepath.Join(base, rel))
	if errors.Is(err, iofs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer d.Close()
	contenu, _, err := d.ReadFile(nom, int(uid), int64(max))
	if errors.Is(err, iofs.ErrNotExist) {
		return nil, nil
	}
	return contenu, err
}

// LireHtaccess lit le .htaccess du répertoire rel (relatif au docroot) d'un
// projet, comme lireDansDocroot. Absent : rend nil, sans erreur.
func LireHtaccess(proj *model.Project, rel string) ([]byte, error) {
	return lireDansDocroot(proj, rel, ".htaccess", tailleMaxHtaccess)
}

// ListerHtaccess énumère, relatifs au docroot, les répertoires qui portent un
// .htaccess, sans jamais suivre de lien, en profondeur et en nombre bornés :
// un diagnostic n'a pas à lire tout un dépôt. vendor, node_modules et .git sont
// ignorés. Un docroot lié rend une erreur.
func ListerHtaccess(proj *model.Project) ([]string, error) {
	base, ok := docrootRelatif(proj)
	if !ok {
		return nil, fmt.Errorf("docroot %s hors de la racine du projet", proj.Spec.Layout.Docroot)
	}
	const maxProfondeur, maxRepertoires = 6, 2000
	ignore := map[string]bool{"vendor": true, "node_modules": true, ".git": true}
	var out []string
	vus := 0
	var descendre func(rel string, prof int) error
	descendre = func(rel string, prof int) error {
		vus++
		_, entrees, err := fs.ListDirAt(proj.Spec.Layout.Root, filepath.Join(base, rel))
		if err != nil {
			return err
		}
		for _, e := range entrees {
			switch {
			case e.Regular && e.Name == ".htaccess":
				out = append(out, rel)
			case !e.Regular && !ignore[e.Name] && prof < maxProfondeur && vus < maxRepertoires:
				// Un lien, ou tout ce qui n'est pas un répertoire, refuse de
				// s'ouvrir : ListDirAt ne suit rien.
				_ = descendre(filepath.Join(rel, e.Name), prof+1)
			}
		}
		return nil
	}
	if err := descendre(".", 0); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// HtaccessSignificatif dit si un .htaccess porte au moins une directive : un
// fichier vide ou fait de commentaires ne mérite aucun signalement.
func HtaccessSignificatif(contenu []byte) bool { return len(lignesUtiles(contenu)) > 0 }
