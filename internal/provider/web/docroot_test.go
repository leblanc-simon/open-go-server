package web

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	osexec "os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/template"
)

// Les vhosts qui servent un docroot, tels que chaque type d'application les
// rend : le docroot est la racine du dépôt pour php-plain, Prestashop et le
// statique, un sous-répertoire pour Symfony.
var casDeDocroot = []struct {
	nom          string
	kind         model.AppKind
	version, php string
}{
	{nom: "statique", kind: model.KindStatic},
	{nom: "php-plain", kind: model.KindPHPPlain, php: "8.5"},
	{nom: "symfony", kind: model.KindSymfony, version: "6.4", php: "8.5"},
	{nom: "symfony-3.4", kind: model.KindSymfony, version: "3.4", php: "7.4"},
	{nom: "prestashop", kind: model.KindPrestashop, php: "8.5"},
}

// pourChaqueVHostDeDocroot rend le vhost principal de chaque type, sur chaque
// moteur, en HTTP seul puis en TLS : le bloc qui sert le docroot n'est pas le
// même dans les deux cas. Le rendu ne modifie pas le provider : un seul par
// moteur suffit.
func pourChaqueVHostDeDocroot(t *testing.T, f func(t *testing.T, moteur string, p *model.Project, bp *VHostPlan)) {
	for _, moteur := range []string{"apache", "nginx"} {
		prov, _ := fixture(t, moteur)
		for _, c := range casDeDocroot {
			for _, tls := range []bool{false, true} {
				nom := moteur + "/" + c.nom + "/http"
				prov.Certificate = nil
				if tls {
					nom = moteur + "/" + c.nom + "/tls"
					prov.Certificate = certificatCouvrant("boutique.fr", "www.boutique.fr")
				}
				t.Run(nom, func(t *testing.T) {
					p := project(t, "boutique.fr", c.kind, c.version, c.php)
					f(t, moteur, p, buildMain(t, prov, p))
				})
			}
		}
	}
}

// --- Lecture des vhosts nginx ------------------------------------------------

// blocsNginx renvoie le contenu des blocs de premier niveau ouverts par
// « <mot> … { », dans l'ordre du fichier. Les commentaires sont écartés : ils ne
// portent aucune accolade dans nos templates, mais une surcharge pourrait.
func blocsNginx(conf, mot string) []string {
	var out []string
	var cur strings.Builder
	profondeur, dedans := 0, false
	for _, l := range strings.Split(conf, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#") {
			continue
		}
		if profondeur == 0 && (t == mot+" {" || strings.HasPrefix(t, mot+" ")) && strings.HasSuffix(t, "{") {
			dedans = true
			cur.Reset()
		}
		profondeur += strings.Count(t, "{") - strings.Count(t, "}")
		if dedans {
			cur.WriteString(l + "\n")
			if profondeur == 0 {
				out = append(out, cur.String())
				dedans = false
			}
		}
	}
	return out
}

// directivesNginx renvoie les directives de premier niveau d'un bloc, sans leur
// point-virgule : celles des location imbriqués n'y figurent pas.
func directivesNginx(bloc string) []string {
	var out []string
	profondeur := 0
	for _, l := range strings.Split(bloc, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if profondeur == 1 && strings.HasSuffix(t, ";") {
			out = append(out, strings.TrimSuffix(t, ";"))
		}
		profondeur += strings.Count(t, "{") - strings.Count(t, "}")
	}
	return out
}

type locationNginx struct{ mod, motif, corps string }

var reLocationNginx = regexp.MustCompile(`^location\s+(=|\^~|~\*|~)?\s*(\S+)\s*\{$`)

// locationsNginx énumère les location de premier niveau d'un server.
func locationsNginx(serveur string) []locationNginx {
	var out []locationNginx
	var cur *locationNginx
	profondeur := 0
	for _, l := range strings.Split(serveur, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if profondeur == 1 {
			if m := reLocationNginx.FindStringSubmatch(t); m != nil {
				cur = &locationNginx{mod: m[1], motif: m[2]}
			}
		} else if cur != nil && profondeur >= 2 {
			cur.corps += t + "\n"
		}
		profondeur += strings.Count(t, "{") - strings.Count(t, "}")
		if cur != nil && profondeur == 1 {
			out = append(out, *cur)
			cur = nil
		}
	}
	return out
}

// correspond évalue un motif PCRE de vhost avec le moteur de Go. RE2 refuse de
// compiler l'assertion négative « (?!…) » (« invalid or unsupported Perl
// syntax »), dont dépend l'exception de /.well-known/ : elle n'est admise qu'en
// fin de motif, seule forme qu'emploient les templates, et éprouvée à chaque
// position où le début du motif correspond.
func correspond(t *testing.T, motif, sujet string) bool {
	t.Helper()
	i := strings.LastIndex(motif, "(?!")
	if i < 0 {
		return compiler(motif).MatchString(sujet)
	}
	if !strings.HasSuffix(motif, ")") || strings.Contains(motif[:i], "|") {
		t.Fatalf("assertion négative hors de la forme « début(?!exclu) » : %s", motif)
	}
	debut := compiler(motif[:i])
	exclu := compiler("^(?:" + motif[i+3:len(motif)-1] + ")")
	for _, m := range debut.FindAllStringIndex(sujet, -1) {
		if !exclu.MatchString(sujet[m[1]:]) {
			return true
		}
	}
	return false
}

// motifsCompiles évite de recompiler les mêmes motifs pour chaque URL éprouvée.
var motifsCompiles sync.Map

func compiler(motif string) *regexp.Regexp {
	if re, ok := motifsCompiles.Load(motif); ok {
		return re.(*regexp.Regexp)
	}
	re := regexp.MustCompile(motif)
	motifsCompiles.Store(motif, re)
	return re
}

// choisirLocation applique l'algorithme de nginx : correspondance exacte, puis
// préfixe le plus long — qui clôt la recherche s'il porte « ^~ » —, puis la
// première regex dans l'ordre du fichier, à défaut le préfixe retenu.
func choisirLocation(t *testing.T, locs []locationNginx, uri string) locationNginx {
	t.Helper()
	for _, l := range locs {
		if l.mod == "=" && l.motif == uri {
			return l
		}
	}
	var prefixe *locationNginx
	for i, l := range locs {
		if (l.mod == "" || l.mod == "^~") && strings.HasPrefix(uri, l.motif) &&
			(prefixe == nil || len(l.motif) > len(prefixe.motif)) {
			prefixe = &locs[i]
		}
	}
	if prefixe != nil && prefixe.mod == "^~" {
		return *prefixe
	}
	for _, l := range locs {
		motif := l.motif
		if l.mod == "~*" {
			motif = "(?i)" + motif
		}
		if (l.mod == "~" || l.mod == "~*") && correspond(t, motif, uri) {
			return l
		}
	}
	if prefixe == nil {
		t.Fatalf("aucune location ne prend %s", uri)
	}
	return *prefixe
}

// refuse dit si une location refuse la requête, quelle qu'en soit la forme.
func (l locationNginx) refuse() bool {
	return strings.Contains(l.corps, "deny all;") || strings.Contains(l.corps, "return 403;") ||
		strings.Contains(l.corps, "return 404;")
}

// serveurDuDocroot renvoie le server qui sert le docroot : celui du port 443 en
// TLS, l'unique sinon.
func serveurDuDocroot(t *testing.T, conf, docroot string) string {
	t.Helper()
	var trouves []string
	for _, s := range blocsNginx(conf, "server") {
		for _, d := range directivesNginx(s) {
			if d == "root "+docroot {
				trouves = append(trouves, s)
			}
		}
	}
	if len(trouves) != 1 {
		t.Fatalf("%d server(s) servent %s, un seul attendu :\n%s", len(trouves), docroot, conf)
	}
	return trouves[0]
}

// --- Lecture des vhosts Apache -----------------------------------------------

type sectionApache struct{ balise, argument, corps string }

var reSectionApache = regexp.MustCompile(`^<(\w+)\s+([^>]*)>$`)

// sectionsApache énumère, dans l'ordre de leur fermeture, les sections des deux
// premiers niveaux : les VirtualHost d'un vhost rendu et leurs Directory ou
// Location, ou les sections d'un corps de VirtualHost et celles qu'elles
// contiennent.
func sectionsApache(conf string) []sectionApache {
	var out []sectionApache
	var pile []*sectionApache
	for _, l := range strings.Split(conf, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(t, "</") {
			if n := len(pile); n > 0 {
				fermee := pile[n-1]
				pile = pile[:n-1]
				if len(pile) <= 1 {
					out = append(out, *fermee)
				}
				for _, s := range pile {
					s.corps += t + "\n"
				}
			}
			continue
		}
		for _, s := range pile {
			s.corps += t + "\n"
		}
		if m := reSectionApache.FindStringSubmatch(t); m != nil {
			pile = append(pile, &sectionApache{balise: m[1], argument: strings.Trim(m[2], `"`)})
		}
	}
	return out
}

// vhostApacheDuDocroot renvoie le corps du VirtualHost qui sert le docroot :
// celui du port 443 en TLS, l'unique sinon.
func vhostApacheDuDocroot(t *testing.T, conf, docroot string) string {
	t.Helper()
	var out []string
	for _, s := range sectionsApache(conf) {
		if s.balise == "VirtualHost" && strings.Contains(s.corps, "<Directory "+docroot+">") {
			out = append(out, s.corps)
		}
	}
	if len(out) != 1 {
		t.Fatalf("%d VirtualHost servent %s, un seul attendu :\n%s", len(out), docroot, conf)
	}
	return out[0]
}

// optionsApache applique une ligne « Options » à un jeu d'options comme le fait
// Apache : des options relatives (+/-) modifient celles héritées, une liste
// absolue les remplace.
func optionsApache(herite map[string]bool, ligne string) map[string]bool {
	mots := strings.Fields(ligne)[1:]
	out := map[string]bool{}
	relatif := len(mots) > 0 && (strings.HasPrefix(mots[0], "+") || strings.HasPrefix(mots[0], "-"))
	if relatif {
		for k, v := range herite {
			out[k] = v
		}
	}
	for _, m := range mots {
		switch {
		case strings.HasPrefix(m, "-"):
			delete(out, strings.ToLower(m[1:]))
		case strings.HasPrefix(m, "+"):
			out[strings.ToLower(m[1:])] = true
		case strings.EqualFold(m, "None"):
		default:
			out[strings.ToLower(m)] = true
		}
	}
	return out
}

// optionsEnVigueur calcule les options d'un répertoire en fusionnant, du plus
// court au plus long, les <Directory> du vhost qui le contiennent, à partir du
// <Directory /> de Debian (« Options FollowSymLinks »).
func optionsEnVigueur(vhost, chemin string) map[string]bool {
	var dirs []sectionApache
	for _, s := range sectionsApache(vhost) {
		if s.balise == "Directory" && (chemin == s.argument || strings.HasPrefix(chemin, s.argument+"/")) {
			dirs = append(dirs, s)
		}
	}
	sort.SliceStable(dirs, func(i, j int) bool { return len(dirs[i].argument) < len(dirs[j].argument) })
	opts := map[string]bool{"followsymlinks": true}
	for _, d := range dirs {
		for _, l := range strings.Split(d.corps, "\n") {
			if strings.HasPrefix(l, "Options ") {
				opts = optionsApache(opts, l)
			}
		}
	}
	return opts
}

// optionsOuvertesAuHtaccess renvoie les options qu'un .htaccess peut régler
// sous une directive AllowOverride, comme Apache la lit : « All » et « Options »
// nu ouvrent toutes les options, « Options=liste » les restreint, dans l'ordre
// des mots.
func optionsOuvertesAuHtaccess(ligne string) map[string]bool {
	toutes := map[string]bool{"all": true}
	out := map[string]bool{}
	for _, mot := range strings.Fields(ligne)[1:] {
		cle, liste, avecListe := strings.Cut(mot, "=")
		switch {
		case strings.EqualFold(cle, "None"):
			out = map[string]bool{}
		case strings.EqualFold(cle, "All"):
			out = toutes
		case strings.EqualFold(cle, "Options") && !avecListe:
			out = toutes
		case strings.EqualFold(cle, "Options"):
			out = map[string]bool{}
			for _, o := range strings.Split(liste, ",") {
				out[strings.ToLower(o)] = true
			}
		}
	}
	return out
}

// directivesApache renvoie les directives de premier niveau d'un corps de
// section : celles des sections qu'il contient n'y figurent pas.
func directivesApache(corps string) []string {
	var out []string
	profondeur := 0
	for _, l := range strings.Split(corps, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case t == "" || strings.HasPrefix(t, "#"):
		case strings.HasPrefix(t, "</"):
			profondeur--
		case strings.HasPrefix(t, "<"):
			profondeur++
		case profondeur == 0:
			out = append(out, t)
		}
	}
	return out
}

var reRedirectMatch = regexp.MustCompile(`^RedirectMatch\s+(\d{3})\s+"([^"]+)"$`)

// refuseApache dit si l'URL est refusée par un vhost. Un RedirectMatch de
// premier niveau hors de la plage 3xx répond dès la traduction de l'URL, avant
// tout contrôle d'accès : rien ne le rouvre. À défaut, les <Location> et
// <LocationMatch> décident : fusionnés après les <Directory> et les .htaccess,
// dans l'ordre du fichier, le dernier qui règle l'accès l'emporte — un
// « Require » posé plus bas, dans une surcharge du contexte vhost, rouvre ce
// qu'un refus antérieur fermait.
func refuseApache(t *testing.T, vhost, uri string) bool {
	t.Helper()
	for _, d := range directivesApache(vhost) {
		if m := reRedirectMatch.FindStringSubmatch(d); m != nil && m[1][0] != '3' && correspond(t, m[2], uri) {
			return true
		}
	}
	refus := false
	for _, s := range sectionsApache(vhost) {
		var prend bool
		switch s.balise {
		case "Location":
			prend = strings.HasPrefix(uri, s.argument)
		case "LocationMatch":
			prend = correspond(t, s.argument, uri)
		default:
			continue
		}
		if !prend {
			continue
		}
		switch {
		case strings.Contains(s.corps, "Require all denied"):
			refus = true
		case strings.Contains(s.corps, "Require all granted"):
			refus = false
		}
	}
	return refus
}

// --- Liens symboliques (S1R-01) ------------------------------------------------

// www-data est membre du groupe de chaque projet (D25) et sert le statique sous
// cette identité partagée. Le vhost suivait tout lien du docroot : l'utilisateur
// d'alpha posait « x.txt → /home/beta.fr/www/config.php » et un tiers lisait le
// code de beta, secrets compris, que le pool d'alpha — sous son propre compte —
// ne peut pas lire. Un lien n'est plus suivi que vers ce qui appartient à son
// auteur, et le contrôle part de la racine du projet : le home appartient à
// l'utilisateur (D51), qui y remplace le docroot lui-même par un lien.
func TestLeServeurWebNeSuitUnLienQueVersCeQuiAppartientASonAuteur(t *testing.T) {
	pourChaqueVHostDeDocroot(t, func(t *testing.T, moteur string, p *model.Project, bp *VHostPlan) {
		c := string(bp.Content)
		racine, docroot := p.Spec.Layout.Root, p.Spec.Layout.Docroot
		if moteur == "nginx" {
			serveur := serveurDuDocroot(t, c, docroot)
			voulu := "disable_symlinks if_not_owner from=" + racine
			var vus []string
			for _, d := range directivesNginx(serveur) {
				if strings.HasPrefix(d, "disable_symlinks") {
					vus = append(vus, d)
				}
			}
			// Une seule : nginx refuse une directive dupliquée dans un même contexte.
			if len(vus) != 1 || vus[0] != voulu {
				t.Errorf("le server du docroot doit porter « %s; », il porte %q :\n%s", voulu, vus, serveur)
			}
			// from= borne le contrôle à la racine du projet, jamais au docroot : le
			// docroot lui-même se remplace par un lien.
			if !strings.HasPrefix(docroot, racine+"/") {
				t.Fatalf("le docroot %s n'est pas sous la racine %s", docroot, racine)
			}
			return
		}

		vhost := vhostApacheDuDocroot(t, c, docroot)
		if strings.Contains(vhost, "+FollowSymLinks") {
			t.Errorf("aucune section ne doit rouvrir FollowSymLinks :\n%s", vhost)
		}
		// Apache éprouve un composant avec les options du répertoire qui le
		// contient : celles de la racine gardent le docroot, celles du docroot
		// tout ce qu'il contient.
		for _, chemin := range []string{racine, docroot, docroot + "/sous/dossier"} {
			opts := optionsEnVigueur(vhost, chemin)
			if !opts["symlinksifownermatch"] || opts["followsymlinks"] {
				t.Errorf("options en vigueur sous %s : %v, attendu SymLinksIfOwnerMatch sans FollowSymLinks",
					chemin, opts)
			}
		}
		// Un .htaccess ne doit pas pouvoir rétablir FollowSymLinks : Prestashop lit
		// les siens (AllowOverride), et « All » ouvre toutes les options.
		for _, l := range strings.Split(vhost, "\n") {
			if !strings.HasPrefix(l, "AllowOverride ") {
				continue
			}
			// SymLinksIfOwnerMatch reste réglable : le retirer ne fait que cesser
			// de suivre les liens. ExecCGI et Includes feraient exécuter du code
			// sous www-data, membre de tous les groupes.
			ouvertes := optionsOuvertesAuHtaccess(l)
			for _, o := range []string{"all", "followsymlinks", "execcgi", "includes", "includesnoexec"} {
				if ouvertes[o] {
					t.Errorf("« %s » laisse un .htaccess régler %s (%v)", l, o, ouvertes)
				}
			}
			if p.Spec.App.Kind == model.KindPrestashop && !ouvertes["symlinksifownermatch"] {
				t.Errorf("« %s » fait répondre 500 à un .htaccess qui pose la forme sûre, "+
					"« Options +SymLinksIfOwnerMatch »", l)
			}
			if p.Spec.App.Kind == model.KindPrestashop && !strings.Contains(l, "All") {
				t.Errorf("Prestashop s'appuie sur ses .htaccess : %q", l)
			}
		}
	})
}

// --- L'accueil sous nginx --------------------------------------------------------

// fragmentNginx rend le fragment de portée serveur d'ogs, qui définit les
// variables propres aux vhosts.
func fragmentNginx(t *testing.T) string {
	t.Helper()
	b, err := template.New("").Render("nginx/http-snippet.conf", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var reMapNginx = regexp.MustCompile(`^map\s+(\$[a-z_]+)\s+(\$[a-z_]+)\s*\{$`)

// variableNginx évalue pour une URI une variable intégrée ou définie par une
// « map » du fragment. Seules les formes qu'emploient nos vhosts sont admises :
// une map sur $uri aux clés littérales, et la chaîne de requête vide.
func variableNginx(t *testing.T, fragment, nom, uri string) string {
	t.Helper()
	switch nom {
	case "$uri":
		return uri
	case "$is_args", "$args":
		return ""
	}
	var source, defaut string
	table, dedans := map[string]string{}, false
	for _, l := range strings.Split(fragment, "\n") {
		l = strings.TrimSpace(l)
		if m := reMapNginx.FindStringSubmatch(l); m != nil {
			dedans = m[2] == nom
			source = m[1]
			continue
		}
		if !dedans || l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if l == "}" {
			break
		}
		cle, valeur, _ := strings.Cut(strings.TrimSuffix(l, ";"), " ")
		valeur = strings.TrimSpace(valeur)
		switch {
		case cle == "volatile":
		case cle == "default":
			defaut = valeur
		case strings.HasPrefix(cle, "~"):
			t.Fatalf("map %s : clé régulière %q non prise en charge", nom, cle)
		default:
			table[cle] = valeur
		}
	}
	if source == "" {
		t.Fatalf("variable %s définie par aucune map du fragment :\n%s", nom, fragment)
	}
	if source != "$uri" {
		t.Fatalf("map %s sur %s, seule $uri est prise en charge", nom, source)
	}
	valeur, ok := table[uri]
	if !ok {
		valeur = defaut
	}
	return evaluerNginx(t, fragment, valeur, uri)
}

// evaluerNginx remplace les variables d'une valeur de directive.
func evaluerNginx(t *testing.T, fragment, valeur, uri string) string {
	t.Helper()
	return regexp.MustCompile(`\$[a-z_]+`).ReplaceAllStringFunc(valeur, func(v string) string {
		return variableNginx(t, fragment, v, uri)
	})
}

// essaisTryFiles renvoie les arguments de try_files d'une location, vides s'il
// n'y en a pas.
func essaisTryFiles(corps string) []string {
	for _, l := range strings.Split(corps, "\n") {
		if strings.HasPrefix(l, "try_files ") {
			return strings.Fields(strings.TrimSuffix(strings.TrimPrefix(l, "try_files "), ";"))
		}
	}
	return nil
}

// « / » désigne le docroot, que www-data traverse sans pouvoir le lire (0710,
// D51) ; sous disable_symlinks, nginx n'éprouve plus un répertoire par stat()
// mais l'ouvre en lecture. Confier « / » à une location dédiée, sans try_files,
// y perdait deux choses, mesurées sur nginx 1.26. Les surcharges du docroot
// (D63) : un auth_basic y protégeait /autre-page mais laissait servir l'accueil
// d'un site PHP, un « return 503 » de maintenance aussi. Et les méthodes : le
// module index, qui la servait, ne traite que GET, HEAD et POST — le pré-vol
// CORS d'une API servie à la racine recevait 405 au lieu d'atteindre le front
// controller. « / » reste dans location /, où try_files n'éprouve rien qui
// désigne le docroot.
func TestLAccueilNginxResteSousLesSurchargesDuDocroot(t *testing.T) {
	fragment := fragmentNginx(t)
	pourChaqueVHostDeDocroot(t, func(t *testing.T, moteur string, p *model.Project, bp *VHostPlan) {
		if moteur != "nginx" {
			return
		}
		c := string(bp.Content)
		serveur := serveurDuDocroot(t, c, p.Spec.Layout.Docroot)
		locs := locationsNginx(serveur)
		accueil := choisirLocation(t, locs, "/")
		if accueil.mod != "" || accueil.motif != "/" {
			t.Fatalf("« / » est pris par « location %s %s », non par location / : les surcharges "+
				"du docroot ne s'y appliquent pas :\n%s", accueil.mod, accueil.motif, serveur)
		}
		if !reIncludeDocrootNginx.MatchString(accueil.corps) {
			t.Errorf("location / n'inclut pas le contexte du docroot :\n%s", accueil.corps)
		}

		essais := essaisTryFiles(accueil.corps)
		at, err := p.AppType()
		if err != nil {
			t.Fatal(err)
		}
		if !at.NeedsPHP {
			// Le module index sert l'accueil sans ouvrir le docroot : try_files,
			// lui, l'ouvrirait et rendrait 404.
			if len(essais) > 0 {
				t.Errorf("try_files %v éprouve le docroot pour « / » : l'accueil rendrait 404", essais)
			}
			return
		}
		if len(essais) < 2 {
			t.Fatalf("un site PHP retombe sur son front controller par try_files :\n%s", accueil.corps)
		}
		// Le dernier argument est une redirection interne : elle atteint le front
		// controller quelle que soit la méthode, là où le module index ne traite
		// que GET, HEAD et POST.
		controleur := "/" + at.FrontController
		if at.FrontController == "" {
			controleur = "/index.php"
		}
		if dernier := evaluerNginx(t, fragment, essais[len(essais)-1], "/"); dernier != controleur {
			t.Errorf("« / » retombe sur %q, attendu le front controller %q", dernier, controleur)
		}
		if l := choisirLocation(t, locs, controleur); !strings.Contains(l.corps, "fastcgi_pass ") {
			t.Errorf("%s n'est pas confié à FPM : « location %s %s »", controleur, l.mod, l.motif)
		}
		for _, essai := range essais[:len(essais)-1] {
			v := evaluerNginx(t, fragment, essai, "/")
			if path.Clean(v) == "/" {
				t.Errorf("try_files éprouve %q pour « / » : c'est le docroot, que www-data ne peut "+
					"pas ouvrir — une ligne « crit » à chaque visite de l'accueil", v)
			}
			// Un essai qui ne finit pas par une barre cherche un fichier, que
			// try_files servirait ici même, en statique et hors de toute règle de
			// refus : pour « / », son nom doit être celui d'un répertoire, que la
			// barre finale impose.
			if !strings.HasSuffix(essai, "/") && !strings.HasSuffix(v, "/") {
				t.Errorf("try_files éprouve le fichier %q pour « / » : il le servirait en statique", v)
			}
		}
		// Ailleurs, le fichier demandé est éprouvé tel quel : le statique reste servi.
		if v := evaluerNginx(t, fragment, essais[0], "/assets/app.css"); v != "/assets/app.css" {
			t.Errorf("try_files éprouve %q pour /assets/app.css", v)
		}
	})
}

var reIncludeDocrootNginx = regexp.MustCompile(`(?m)^include \S+/` + IncludeDocroot + `/\*\.conf;$`)

// Le contexte du docroot est celui de location / (D63), où une surcharge peut
// imbriquer ses propres location. nginx refuse une location préfixe sous une
// regex ou sous une correspondance exacte (« location "/prive/" is outside
// location "^/$" ») : inclure ce contexte ailleurs rendrait fatale toute
// surcharge qui en déclare une, et le parc entier refuserait de recharger.
func TestLeContexteDuDocrootNginxAdmetDesLocationsImbriquees(t *testing.T) {
	pourChaqueVHostDeDocroot(t, func(t *testing.T, moteur string, p *model.Project, bp *VHostPlan) {
		if moteur != "nginx" {
			return
		}
		serveur := serveurDuDocroot(t, string(bp.Content), p.Spec.Layout.Docroot)
		inclus := 0
		for _, l := range locationsNginx(serveur) {
			if !reIncludeDocrootNginx.MatchString(l.corps) {
				continue
			}
			inclus++
			if l.mod != "" {
				t.Errorf("le contexte du docroot est inclus dans « location %s %s » : une location "+
					"imbriquée par une surcharge y serait fatale", l.mod, l.motif)
			}
		}
		if inclus != 1 {
			t.Errorf("le contexte du docroot est inclus par %d location, une seule attendue :\n%s", inclus, serveur)
		}
	})
}

// --- Fichiers cachés et métadonnées (S1R-14) -----------------------------------

// Ce qu'un « git clone » dans www/ dépose à la racine du docroot, et qu'aucun
// tiers ne doit lire : .git reconstitue le code et ses secrets, .env livre les
// identifiants applicatifs.
var urlsSensibles = []string{
	"/.git/config", "/.git/HEAD", "/.git/logs/HEAD", "/.env", "/.env.local",
	"/.htaccess", "/sous/dossier/.env", "/.well-known/.git/config",
	"/composer.json", "/composer.lock", "/vendor/paquet/composer.json",
}

// Ce qui doit rester servi : /.well-known/ (RFC 8615) porte security.txt et les
// routes d'un applicatif — openid-configuration —, et le reste du site.
var urlsLegitimes = []string{
	"/", "/page.html", "/assets/app.css", "/.well-known/security.txt",
	"/.well-known/openid-configuration", "/aide/composer.json.html",
}

// Le déploiement documenté tire le dépôt dans www/, qui est le docroot de
// php-plain, de Prestashop et du statique : .git/config, .env et composer.json
// étaient servis à quiconque les demandait, sous Apache comme sous nginx.
func TestLeDocrootNeSertNiFichiersCachesNiMetadonnees(t *testing.T) {
	pourChaqueVHostDeDocroot(t, func(t *testing.T, moteur string, p *model.Project, bp *VHostPlan) {
		c := string(bp.Content)
		docroot := p.Spec.Layout.Docroot
		if moteur == "nginx" {
			serveur := serveurDuDocroot(t, c, docroot)
			locs := locationsNginx(serveur)
			for _, re := range regexp.MustCompile(`(?m)^\s*rewrite\s+(\S+)\s`).FindAllStringSubmatch(serveur, -1) {
				for _, uri := range urlsSensibles {
					if compiler(re[1]).MatchString(uri) {
						t.Fatalf("la réécriture %s détourne %s avant le choix de la location", re[1], uri)
					}
				}
			}
			for _, uri := range urlsSensibles {
				if l := choisirLocation(t, locs, uri); !l.refuse() {
					t.Errorf("GET %s est servi par « location %s %s » :\n%s", uri, l.mod, l.motif, l.corps)
				}
			}
			for _, uri := range urlsLegitimes {
				if l := choisirLocation(t, locs, uri); l.refuse() {
					t.Errorf("GET %s est refusé par « location %s %s »", uri, l.mod, l.motif)
				}
			}
			// Le challenge ACME reste servi en clair, depuis son propre webroot.
			port80, _ := blocs(t, moteur, c)
			acme := choisirLocation(t, locationsNginx(blocsNginx(port80, "server")[0]),
				"/.well-known/acme-challenge/jeton")
			if acme.mod != "^~" || !strings.Contains(acme.corps, "root ") {
				t.Errorf("le challenge ACME n'est plus servi par son webroot : location %s %s", acme.mod, acme.motif)
			}
			return
		}

		vhost := vhostApacheDuDocroot(t, c, docroot)
		// Une restriction d'accès ordinaire, déposée dans le contexte du vhost
		// (D63) pour réserver le site au réseau du bureau ou à un mot de passe :
		// incluse après les refus, elle les rouvre à tous ceux qu'elle admet si
		// ceux-ci ne sont qu'un « Require » de <LocationMatch>. Pour le modèle,
		// elle admet le visiteur, comme le ferait « Require ip » à qui en vient.
		restreint := vhost + "<Location />\nRequire all granted\n</Location>\n"
		for _, uri := range urlsSensibles {
			if !refuseApache(t, vhost, uri) {
				t.Errorf("GET %s est servi :\n%s", uri, vhost)
			}
			if !refuseApache(t, restreint, uri) {
				t.Errorf("GET %s est servi dès qu'une surcharge du vhost pose un <Location /> :\n%s", uri, restreint)
			}
		}
		for _, uri := range urlsLegitimes {
			if refuseApache(t, vhost, uri) {
				t.Errorf("GET %s est refusé", uri)
			}
		}
		if !strings.Contains(c, "Alias /.well-known/acme-challenge ") {
			t.Errorf("le challenge ACME n'est plus aliasé :\n%s", c)
		}
	})
}

var reIfFichier = regexp.MustCompile(`^%\{REQUEST_FILENAME\} =~ m#(.+)#$`)

// refuseVarianteApache dit si un fichier est refusé à la sous-requête par
// laquelle MultiViews éprouve une variante. Elle ne passe pas par la traduction
// de l'URL, donc par aucun RedirectMatch, et Apache y fusionne les sections par
// étapes — <Files> et <FilesMatch> sur le nom du fichier, puis <Location> sur
// l'URL de la variante, puis <If> —, la dernière qui règle l'accès l'emportant.
func refuseVarianteApache(t *testing.T, vhost, docroot, rel string) bool {
	t.Helper()
	refus := false
	for _, etape := range []string{"Files", "Location", "If"} {
		for _, s := range sectionsApache(vhost) {
			var prend bool
			switch {
			case etape == "Files" && s.balise == "Files":
				prend = s.argument == path.Base(rel)
			case etape == "Files" && s.balise == "FilesMatch":
				prend = correspond(t, s.argument, path.Base(rel))
			case etape == "Location" && s.balise == "Location":
				prend = strings.HasPrefix(rel, s.argument)
			case etape == "Location" && s.balise == "LocationMatch":
				prend = correspond(t, s.argument, rel)
			case etape == "If" && s.balise == "If":
				m := reIfFichier.FindStringSubmatch(s.argument)
				if m == nil {
					t.Fatalf("condition <If> que le modèle ne sait pas évaluer : %s", s.argument)
				}
				prend = correspond(t, m[1], docroot+rel)
			}
			if !prend {
				continue
			}
			switch {
			case strings.Contains(s.corps, "Require all denied"):
				refus = true
			case strings.Contains(s.corps, "Require all granted"):
				refus = false
			}
		}
	}
	return refus
}

// MultiViews, qu'un .htaccess de Prestashop ou une surcharge du docroot ouvrent,
// négocie « /composer » vers composer.json par une sous-requête sur le fichier,
// que le RedirectMatch ne voit pas : le fichier était servi. Le refus porte sur
// son nom, dans une section qu'un <Location /> de surcharge ne rouvre pas — un
// <FilesMatch>, fusionné avant lui, n'y suffirait pas.
func TestUneVarianteNegocieeNeSertPasLesMetadonnees(t *testing.T) {
	pourChaqueVHostDeDocroot(t, func(t *testing.T, moteur string, p *model.Project, bp *VHostPlan) {
		if moteur != "apache" {
			return // nginx ne négocie aucune variante
		}
		docroot := p.Spec.Layout.Docroot
		vhost := vhostApacheDuDocroot(t, string(bp.Content), docroot)
		restreint := vhost + "<Location />\nRequire all granted\n</Location>\n"
		for _, rel := range []string{"/composer.json", "/composer.lock", "/negocie/composer.json", "/vendor/paquet/composer.lock"} {
			if !refuseVarianteApache(t, vhost, docroot, rel) {
				t.Errorf("la variante %s est servie :\n%s", rel, vhost)
			}
			if !refuseVarianteApache(t, restreint, docroot, rel) {
				t.Errorf("la variante %s est servie dès qu'une surcharge du vhost pose un <Location /> :\n%s", rel, restreint)
			}
		}
		for _, rel := range []string{"/page.html", "/aide/composer.json.html", "/composer.php"} {
			if refuseVarianteApache(t, vhost, docroot, rel) {
				t.Errorf("la variante %s est refusée", rel)
			}
		}
	})
}

// --- Sur le vrai Apache --------------------------------------------------------

// TestApacheReelNeSuitNiLiensEtrangersNiFichiersCaches sert le vhost rendu par
// ogs avec l'Apache de la machine, sans privilège, sur la boucle locale : c'est
// lui, et non une lecture du rendu, qui dit comment Apache fusionne les options
// héritées de Debian, ce qu'un .htaccess peut rouvrir et comment il évalue les
// motifs. Il dépend de l'Apache installé, d'où l'exécution sur demande :
//
//	OGS_TEST_APACHE=1 go test ./internal/provider/web/ -run TestApacheReel
func TestApacheReelNeSuitNiLiensEtrangersNiFichiersCaches(t *testing.T) {
	if os.Getenv("OGS_TEST_APACHE") == "" {
		t.Skip("test sur le vrai Apache désactivé : exporter OGS_TEST_APACHE=1 pour l'exécuter")
	}
	const apache, modules = "/usr/sbin/apache2", "/usr/lib/apache2/modules"
	for _, f := range []string{apache, modules, "/etc/mime.types"} {
		if _, err := os.Stat(f); err != nil {
			t.Skipf("%s absent", f)
		}
	}
	// La cible étrangère doit appartenir à un autre compte que le lien.
	etranger := "/etc/passwd"
	if os.Getuid() == 0 {
		t.Skip("sous root, le lien et sa cible ont le même propriétaire : le refus ne s'éprouve pas")
	}

	for _, c := range []struct {
		nom  string
		kind model.AppKind
	}{{"php-plain", model.KindPHPPlain}, {"prestashop", model.KindPrestashop}, {"statique", model.KindStatic}} {
		t.Run(c.nom, func(t *testing.T) {
			prov, paths := fixture(t, "apache")
			p := project(t, "boutique.fr", c.kind, "", "8.5")
			docroot := p.Spec.Layout.Docroot
			ecrire := func(rel, corps string) {
				t.Helper()
				f := filepath.Join(docroot, rel)
				if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f, []byte(corps), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			lier := func(cible, rel string) {
				t.Helper()
				if err := os.Symlink(cible, filepath.Join(docroot, rel)); err != nil {
					t.Fatal(err)
				}
			}
			for _, uri := range urlsSensibles {
				ecrire(uri, "secret\n")
			}
			ecrire("page.txt", "page\n")
			ecrire(".well-known/security.txt", "Contact: mailto:securite@boutique.fr\n")
			// MultiViews négocie « /negocie/composer » vers composer.json, et
			// « /negocie/page » vers page.txt : le témoin qu'elle est active.
			ecrire("negocie/composer.json", "secret\n")
			ecrire("negocie/page.txt", "page\n")
			lier("page.txt", "lien.txt")
			lier(etranger, "etranger.txt")
			lier(filepath.Dir(etranger), "repertoire-etranger")
			jetons := paths.ACMEChallengeTokens()
			if err := os.MkdirAll(jetons, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(jetons, "jeton"), []byte("jeton\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(p.Spec.Layout.LogDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if c.kind == model.KindPrestashop {
				// Ce que Prestashop écrit dans ses .htaccess reste admis ; un
				// « Require all granted » n'y rouvre pas les fichiers cachés.
				ecrire(".htaccess", "Options -MultiViews\nDirectoryIndex index.php\n"+
					"<IfModule mod_rewrite.c>\nRewriteEngine on\n</IfModule>\nRequire all granted\n")
				// Les .htaccess sont bien lus : celui de app/ ferme son répertoire,
				// comme Prestashop le fait pour app/, config/ ou var/.
				ecrire("app/.htaccess", "Require all denied\n")
				ecrire("app/parameters.txt", "secret\n")
				// Un .htaccess ne rétablit pas le suivi des liens : Apache refuse
				// la directive, et la requête avec elle.
				ecrire("rouvre/.htaccess", "Options +FollowSymLinks\n")
				lier(etranger, "rouvre/etranger.txt")
				// La forme sûre reste admise, dans un sens comme dans l'autre :
				// retirer SymLinksIfOwnerMatch ne fait que cesser de suivre les liens.
				ecrire("sur/.htaccess", "Options +SymLinksIfOwnerMatch\n")
				lier("../page.txt", "sur/lien.txt")
				ecrire("strict/.htaccess", "Options -SymLinksIfOwnerMatch\n")
				lier("../page.txt", "strict/lien.txt")
				// Toute autre option est refusée, retrait compris : Apache ne
				// distingue pas « -ExecCGI » de « +ExecCGI ».
				ecrire("cgi/.htaccess", "Options -ExecCGI\n")
				ecrire("cgi/page.txt", "page\n")
				// MultiViews reste admise au .htaccess : Prestashop la retire,
				// un projet peut l'ouvrir.
				ecrire("negocie/.htaccess", "Options +MultiViews\n")
			}

			bp := buildMain(t, prov, p)
			url := demarrerApache(t, apache, modules, string(bp.Content))
			obtenir := func(uri string) (int, string) {
				t.Helper()
				req, err := http.NewRequest("GET", url+uri, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Host = "boutique.fr"
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				corps, _ := io.ReadAll(res.Body)
				return res.StatusCode, string(corps)
			}
			attendre := func(uri string, code int) {
				t.Helper()
				if obtenu, corps := obtenir(uri); obtenu != code {
					t.Errorf("GET %s : %d, attendu %d (%.60q)", uri, obtenu, code, corps)
				}
			}
			// La réponse à une variante refusée dépend de ce qui prend la suite :
			// 404, ou le front controller de FallbackResource. Seul compte que le
			// fichier ne sorte pas.
			nonServi := func(uri string) {
				t.Helper()
				if obtenu, corps := obtenir(uri); obtenu == http.StatusOK || strings.Contains(corps, "secret") {
					t.Errorf("GET %s : %d, le fichier négocié est servi (%.60q)", uri, obtenu, corps)
				}
			}

			attendre("/page.txt", 200)
			attendre("/.well-known/security.txt", 200)
			attendre("/.well-known/acme-challenge/jeton", 200)
			for _, uri := range urlsSensibles {
				attendre(uri, 403)
			}
			// Un lien vers ses propres fichiers est servi ; vers ceux d'un autre, non.
			attendre("/lien.txt", 200)
			attendre("/etranger.txt", 403)
			attendre("/repertoire-etranger/"+filepath.Base(etranger), 403)
			if c.kind == model.KindPrestashop {
				attendre("/app/parameters.txt", 403)
				attendre("/rouvre/etranger.txt", 500)
				attendre("/sur/lien.txt", 200)
				attendre("/strict/lien.txt", 403)
				attendre("/cgi/page.txt", 500)
				// La variante est cherchée par une sous-requête sur le fichier,
				// qui ne passe pas par la traduction de l'URL : le RedirectMatch
				// ne la voit pas.
				attendre("/negocie/page", 200)
				nonServi("/negocie/composer")
			}

			// Une restriction d'accès ordinaire, déposée dans le contexte du vhost
			// (D63) pour réserver le site au réseau du bureau : Apache fusionne les
			// <Location> dans l'ordre du fichier, et celle-ci, incluse après les
			// refus, rouvre .git et .env à tous ceux qu'elle admet si ces refus ne
			// sont que des <LocationMatch>.
			surcharges := prov.IncludeDir(bp.Name, IncludeVHost)
			if err := os.MkdirAll(surcharges, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(surcharges, "10-bureau.conf"),
				[]byte("<Location />\n    Require expr \"%{REMOTE_ADDR} == '127.0.0.1'\"\n</Location>\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// MultiViews ouverte par une surcharge du docroot : le seul moyen
			// d'un type sans .htaccess. Le <Location /> ci-dessus, fusionné après
			// les <Files>, rouvrirait la variante si le refus n'était qu'un
			// <FilesMatch>.
			docrootSurcharges := prov.IncludeDir(bp.Name, IncludeDocroot)
			if err := os.MkdirAll(docrootSurcharges, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(docrootSurcharges, "10-negociation.conf"),
				[]byte("Options +MultiViews\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			url = demarrerApache(t, apache, modules, string(bp.Content))
			attendre("/page.txt", 200)
			for _, uri := range urlsSensibles {
				attendre(uri, 403)
			}
			attendre("/negocie/page", 200)
			nonServi("/negocie/composer")

			// Le docroot lui-même remplacé par un lien : le home appartient à
			// l'utilisateur, qui le renomme sans privilège.
			if err := os.Rename(docroot, docroot+".orig"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Dir(etranger), docroot); err != nil {
				t.Fatal(err)
			}
			attendre("/"+filepath.Base(etranger), 403)
		})
	}
}

// demarrerApache sert un vhost rendu par ogs avec l'Apache de la machine, en un
// seul processus, et renvoie son adresse. Seul le port change : la
// configuration qui l'entoure reprend les défauts de Debian, <Directory />
// compris, dont le vhost hérite.
func demarrerApache(t *testing.T, apache, modules, vhost string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	dir := t.TempDir()
	vhost = strings.ReplaceAll(vhost, "<VirtualHost *:80>", fmt.Sprintf("<VirtualHost *:%d>", port))
	if err := os.WriteFile(filepath.Join(dir, "vhost.conf"), []byte(vhost), 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ServerRoot %q\nServerName localhost\nListen 127.0.0.1:%d\n", dir, port)
	fmt.Fprintf(&b, "PidFile %q\nDefaultRuntimeDir %q\nMutex file:%s default\nErrorLog %q\nLogLevel warn\n",
		filepath.Join(dir, "httpd.pid"), dir, dir, filepath.Join(dir, "error.log"))
	// mod_negotiation est actif par défaut sous Debian : c'est lui qui sert
	// MultiViews.
	for _, m := range []string{"mpm_event", "authz_core", "alias", "dir", "mime", "negotiation", "rewrite", "headers", "proxy", "proxy_fcgi"} {
		fmt.Fprintf(&b, "LoadModule %s_module %s/mod_%s.so\n", m, modules, m)
	}
	b.WriteString("TypesConfig /etc/mime.types\nAccessFileName .htaccess\n")
	b.WriteString("<Directory />\n    Options FollowSymLinks\n    AllowOverride None\n    Require all denied\n</Directory>\n")
	fmt.Fprintf(&b, "Include %q\n", filepath.Join(dir, "vhost.conf"))
	conf := filepath.Join(dir, "httpd.conf")
	if err := os.WriteFile(conf, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := osexec.Command(apache, "-t", "-f", conf).CombinedOutput(); err != nil {
		t.Fatalf("apache2 -t refuse le vhost rendu : %v\n%s", err, out)
	}

	cmd := osexec.Command(apache, "-X", "-f", conf)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var sortie strings.Builder
	cmd.Stdout, cmd.Stderr = &sortie, &sortie
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			c.Close()
			return fmt.Sprintf("http://127.0.0.1:%d", port)
		}
		select {
		case <-ctx.Done():
			journal, _ := os.ReadFile(filepath.Join(dir, "error.log"))
			t.Fatalf("Apache ne répond pas :\n%s\n%s", sortie.String(), journal)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
