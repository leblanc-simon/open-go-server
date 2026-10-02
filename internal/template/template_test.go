package template

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamesListsEmbedded(t *testing.T) {
	names := New("").Names()
	if len(names) == 0 {
		t.Fatal("aucun template embarqué")
	}
	found := false
	for _, n := range names {
		if n == "php/pool.conf" {
			found = true
		}
	}
	if !found {
		t.Errorf("php/pool.conf absent de %v", names)
	}
}

func TestRenderEmbedded(t *testing.T) {
	out, err := New("").Render("php/pool.conf", map[string]any{
		"Project": "shopfr", "Name": "shopfr", "Version": "8.3", "Number": 18301,
		"User": "shopfr", "Group": "shopfr", "WebUser": "www-data",
		"Listen": "/run/php/php8.3-fpm-18301-shopfr.sock", "UnixSocket": true,
		"Settings": map[string]string{"pm": "dynamic"}, "Env": map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[shopfr]", "user = shopfr", "listen.mode = 0660", "pm = dynamic"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("rendu incomplet, %q absent :\n%s", want, out)
		}
	}
}

func TestOverrideTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	set := New(dir)
	path := set.OverridePath("php/pool.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("; surcharge de {{ .Project }}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := set.Render("php/pool.conf", map[string]any{"Project": "shopfr"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "; surcharge de shopfr" {
		t.Errorf("la surcharge doit remplacer la version embarquée : %q", out)
	}
	if set.Source("php/pool.conf") != path {
		t.Errorf("la source doit désigner la surcharge : %s", set.Source("php/pool.conf"))
	}
}

func TestBrokenOverrideNamesItsFile(t *testing.T) {
	// Une surcharge fautive doit désigner le fichier que l'administrateur a écrit.
	dir := t.TempDir()
	set := New(dir)
	path := set.OverridePath("php/pool.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{{ .Project "), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := set.Render("php/pool.conf", map[string]any{"Project": "shopfr"})
	if err == nil {
		t.Fatal("une surcharge syntaxiquement fautive doit échouer")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("l'erreur doit nommer le fichier fautif : %v", err)
	}
}

func TestMissingKeyIsAnError(t *testing.T) {
	// Un champ absent du contexte ne doit pas produire silencieusement « <no value> »
	// dans un fichier de configuration.
	_, err := New("").Render("php/pool.conf", map[string]any{"Project": "shopfr"})
	if err == nil {
		t.Fatal("un contexte incomplet doit échouer")
	}
}

func TestExtract(t *testing.T) {
	dir := t.TempDir()
	set := New(dir)
	path, err := set.Extract("php/pool.conf")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "{{ .Name }}") {
		t.Errorf("le template extrait doit être la source, pas un rendu :\n%s", data)
	}
	if _, err := set.Extract("php/pool.conf"); err == nil {
		t.Error("extraire par-dessus une surcharge existante doit être refusé")
	}
}

func TestUnknownTemplate(t *testing.T) {
	if _, err := New("").Render("php/absent", nil); err == nil {
		t.Error("un template inconnu doit être signalé")
	}
}

func TestSortedIsDeterministic(t *testing.T) {
	keys := Funcs()["sorted"].(func(map[string]string) []string)
	got := keys(map[string]string{"b": "1", "a": "2", "c": "3"})
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("ordre %v", got)
	}
}

func TestHelpers(t *testing.T) {
	f := Funcs()
	if got := f["snake"].(func(string) string)("shop-fr.test"); got != "shop_fr_test" {
		t.Errorf("snake : %q", got)
	}
	if got := f["indent"].(func(int, string) string)(2, "a\nb"); got != "  a\n  b" {
		t.Errorf("indent : %q", got)
	}
	if got := f["hash"].(func(string) string)("shop.fr"); len(got) != 6 {
		t.Errorf("hash : %q", got)
	}
	if got := f["default"].(func(string, string) string)("medium", ""); got != "medium" {
		t.Errorf("default : %q", got)
	}
}

// Tout template embarqué qui sert un docroot ne suit un lien que vers ce qui
// appartient à son auteur, et refuse les fichiers cachés et les métadonnées de
// dépendances : www-data est membre du groupe de chaque projet (D25), et le
// dépôt tiré dans www/ en est souvent la racine. Le contrôle porte sur les
// sources, pour qu'un template ajouté demain n'y échappe pas ; l'effet du rendu
// est éprouvé par les tests du provider web.
func TestLesTemplatesQuiServentUnDocrootLeProtegent(t *testing.T) {
	set := New("")
	voulus := map[string][]string{
		"apache/": {
			"<Directory {{ .Root }}>\n        Options -FollowSymLinks +SymLinksIfOwnerMatch\n",
			"Options -Indexes -FollowSymLinks +SymLinksIfOwnerMatch\n",
			// Un refus rendu dès la traduction de l'URL : un <Location /> déposé
			// dans le contexte du vhost (D63) rouvrirait un <LocationMatch>.
			"RedirectMatch 403 \"/\\.(?!well-known/)\"\n",
			"RedirectMatch 403 \"/composer\\.(json|lock)$\"\n",
			// La variante que MultiViews négocie ne passe pas par la traduction
			// de l'URL : le refus porte aussi sur le fichier, dans un <If>, que
			// ce <Location /> ne rouvre pas.
			"<If \"%{REQUEST_FILENAME} =~ m#/composer\\.(json|lock)$#\">\n        Require all denied\n    </If>\n",
		},
		"nginx/": {
			"disable_symlinks if_not_owner from={{ .Root }};\n",
			"location ~ /\\.(?!well-known/) {\n        deny all;\n",
			"location ~ /composer\\.(json|lock)$ {\n        deny all;\n",
		},
	}
	examines := map[string]bool{}
	for _, nom := range set.Names() {
		src, err := set.Read(nom)
		if err != nil {
			t.Fatal(err)
		}
		s := string(src)
		moteur := nom[:strings.Index(nom, "/")+1]
		sert := (moteur == "apache/" && strings.Contains(s, "DocumentRoot ")) ||
			(moteur == "nginx/" && strings.Contains(s, "root {{ .Docroot }};"))
		if !sert {
			continue
		}
		examines[nom] = true
		for _, v := range voulus[moteur] {
			if !strings.Contains(s, v) {
				t.Errorf("%s sert un docroot sans %q", nom, v)
			}
		}
		if strings.Contains(s, "+FollowSymLinks") {
			t.Errorf("%s rouvre FollowSymLinks", nom)
		}
		if strings.Contains(s, "<LocationMatch") && strings.Contains(s, "Require all denied") {
			t.Errorf("%s refuse par <LocationMatch> : un <Location /> postérieur le rouvre", nom)
		}
		// Pour « / », try_files éprouverait le docroot lui-même, que www-data
		// traverse sans pouvoir le lire (D51) : disable_symlinks le lui fait
		// ouvrir en lecture. $ogs_fichier, que définit le fragment, l'épargne.
		for _, l := range strings.Split(s, "\n") {
			l = strings.TrimSpace(l)
			if moteur == "nginx/" && strings.HasPrefix(l, "try_files ") && strings.Contains(l, "$uri") {
				t.Errorf("%s : « %s » éprouve $uri, donc le docroot pour « / »", nom, l)
			}
		}
		// ogs n'active jamais les .htaccess sous Apache (AllowOverride None, D99) :
		// un .htaccess servi par www-data pouvait détourner le serveur (socket FPM
		// d'un voisin, server-status) d'un simple RewriteRule. Aucun « AllowOverride
		// All » ni « AllowOverrideList », et tout vhost Apache qui sert un docroot
		// pose « AllowOverride None ». (nginx n'a pas de directive AllowOverride.)
		if moteur == "apache/" {
			if strings.Contains(s, "AllowOverride All") || strings.Contains(s, "AllowOverrideList") {
				t.Errorf("%s : le .htaccess ne doit pas être honoré (AllowOverride None)", nom)
			}
			if !strings.Contains(s, "AllowOverride None") {
				t.Errorf("%s : un vhost qui sert un docroot doit poser AllowOverride None", nom)
			}
		}
	}
	for _, nom := range []string{"apache/php-fpm.conf", "apache/static.conf", "nginx/php-fpm.conf", "nginx/static.conf"} {
		if !examines[nom] {
			t.Errorf("%s n'est plus reconnu comme servant un docroot : le contrôle ne le voit pas", nom)
		}
	}
}

func TestSystemdEnvQuotesOnlyWhenNeeded(t *testing.T) {
	cases := map[[2]string]string{
		// Une valeur simple garde son rendu : les unités existantes ne changent pas.
		{"APP_ENV", "prod"}:              `APP_ENV=prod`,
		{"DSN", "amqp://u:p@h:5672/%2f"}: `DSN=amqp://u:p@h:5672/%2f`,
		{"VIDE", ""}:                     `VIDE=`,
		{"TITRE", "deux mots"}:           `"TITRE=deux mots"`,
		{"GUILLEMET", `dit "oui"`}:       `"GUILLEMET=dit \"oui\""`,
		{"CHEMIN", `C:\temp`}:            `"CHEMIN=C:\\temp"`,
		{"APOSTROPHE", "l'appli"}:        `"APOSTROPHE=l'appli"`,
		{"TAB", "a\tb"}:                  "\"TAB=a\tb\"",
	}
	for in, want := range cases {
		if got := systemdEnv(in[0], in[1]); got != want {
			t.Errorf("systemdEnv(%q, %q) = %s, attendu %s", in[0], in[1], got, want)
		}
	}
}
