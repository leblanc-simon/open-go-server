package web

import (
	"context"
	"fmt"
	"io"
	iofs "io/fs"
	"net"
	"net/http"
	"net/http/fcgi"
	"os"
	osexec "os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/model"
)

// fauxFPMDetaille répond, sur un socket Unix, par le script que nginx lui confie
// — relatif au docroot —, ses arguments et son PATH_INFO : la réponse dit où une
// URL a été conduite, et avec quoi.
func fauxFPMDetaille(t *testing.T, docroot string) string {
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
	// Sous root, nginx sert sous www-data (poserModesReels) : le socket doit lui
	// être joignable.
	if os.Geteuid() == 0 {
		_ = os.Chmod(dir, 0o755)
		_ = os.Chmod(sock, 0o777)
	}
	go fcgi.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := fcgi.ProcessEnv(r)
		script := env["SCRIPT_FILENAME"]
		if st, err := os.Stat(script); err != nil || st.IsDir() {
			w.WriteHeader(http.StatusNotFound)
		}
		reponse := "FPM " + strings.TrimPrefix(script, docroot)
		// net/http/fcgi garde pour lui QUERY_STRING et PATH_INFO : demarrerNginx les
		// recopie sous un autre nom.
		if q := env["X_QUERY_STRING"]; q != "" {
			reponse += "?" + q
		}
		if pi := env["X_PATH_INFO"]; pi != "" {
			reponse += " " + pi
		}
		fmt.Fprint(w, reponse)
	}))
	return sock
}

// demarrerNginx sert un vhost rendu par ogs avec le nginx de la machine, en un
// seul processus et sans privilège, précédé du fragment de portée serveur
// d'ogs, et renvoie son adresse.
func demarrerNginx(t *testing.T, nginx string, prov *Provider, vhost, utilisateur string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	dir := t.TempDir()
	vhost = strings.ReplaceAll(vhost, "listen 80;", fmt.Sprintf("listen 127.0.0.1:%d;", port))
	vhost = strings.ReplaceAll(vhost, "listen [::]:80;", "")
	vhost = strings.ReplaceAll(vhost, "include fastcgi_params;",
		"include /etc/nginx/fastcgi_params;\n        fastcgi_param X_QUERY_STRING $query_string;")
	vhost = strings.ReplaceAll(vhost, "fastcgi_param PATH_INFO ", "fastcgi_param X_PATH_INFO ")
	fragment, err := prov.Templates.Render(prov.Engine.TemplateName("http-snippet"), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	for nom, contenu := range map[string][]byte{"vhost.conf": []byte(vhost), "fragment.conf": fragment} {
		if err := os.WriteFile(filepath.Join(dir, nom), contenu, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%spid %s/nginx.pid;\nerror_log %s/error.log warn;\nevents {}\nhttp {\n", utilisateur, dir, dir)
	b.WriteString("    include /etc/nginx/mime.types;\n    default_type application/octet-stream;\n    access_log off;\n")
	for _, tmp := range []string{"client_body", "proxy", "fastcgi", "uwsgi", "scgi"} {
		fmt.Fprintf(&b, "    %s_temp_path %s/%s;\n", tmp, dir, tmp)
	}
	fmt.Fprintf(&b, "    include %s/fragment.conf;\n    include %s/vhost.conf;\n}\n", dir, dir)
	conf := filepath.Join(dir, "nginx.conf")
	if err := os.WriteFile(conf, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-p", dir, "-e", filepath.Join(dir, "error.log"), "-c", conf}
	if out, err := osexec.Command(nginx, append(args, "-t")...).CombinedOutput(); err != nil {
		t.Fatalf("nginx -t refuse le vhost rendu : %v\n%s\n%s", err, out, vhost)
	}
	// Un utilisateur désigné exige des workers : le maître seul tournerait en root.
	global := "daemon off; master_process off;"
	if utilisateur != "" {
		global = "daemon off;"
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		traversable(t, dir)
	}
	cmd := osexec.Command(nginx, append(args, "-g", global)...)
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
			t.Fatalf("nginx ne répond pas :\n%s\n%s", sortie.String(), journal)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// interroger rend « code », suivi du corps pour une réponse 200 ou une réponse
// du faux FPM.
func interroger(t *testing.T, url, host, uri string) string {
	t.Helper()
	req, err := http.NewRequest("GET", url+uri, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	corps, _ := io.ReadAll(res.Body)
	res.Body.Close()
	obtenu := fmt.Sprint(res.StatusCode)
	if c := strings.TrimSpace(string(corps)); res.StatusCode == http.StatusOK || strings.HasPrefix(c, "FPM ") {
		obtenu += " " + c
	}
	return obtenu
}

// TestNginxReelPointsDEntreePHP sert, sur le vrai nginx, le vhost PHP rendu pour
// chaque type, avec un faux FPM, et relève où chaque URL est conduite (R4-04,
// D119). Pour Prestashop, ce sont les URL où la comparaison avec Apache (D99)
// relevait un 404 sous nginx : back-office — reconnu à son
// cron_currency_rates.php, comme sous Apache —, webservice, scripts de modules,
// retro-compat, admin-api/ de la 9. Le vhost est servi sans TLS : le bloc 443
// porte le même corps.
//
//	OGS_TEST_NGINX=1 go test ./internal/provider/web/ -run TestNginxReel
//
// nginx n'est pas toujours installé là où l'on développe ; l'image
// d'intégration le porte :
//
//	CGO_ENABLED=0 go test -c -o /tmp/web.test ./internal/provider/web/
//	docker run --rm -u "$(id -u)" -e OGS_TEST_NGINX=1 -v /tmp/web.test:/web.test:ro \
//	    -v "$PWD/internal/provider/web/testdata:/w/testdata:ro" -w /w \
//	    --entrypoint /web.test ogs-integration -test.run TestNginxReel -test.v
func TestNginxReelPointsDEntreePHP(t *testing.T) {
	if os.Getenv("OGS_TEST_NGINX") == "" {
		t.Skip("test sur le vrai nginx désactivé : exporter OGS_TEST_NGINX=1 pour l'exécuter")
	}
	const nginx = "/usr/sbin/nginx"
	for _, f := range []string{nginx, "/etc/nginx/mime.types", "/etc/nginx/fastcgi_params"} {
		if _, err := os.Stat(f); err != nil {
			t.Skipf("%s absent", f)
		}
	}
	type cas struct {
		kind              model.AppKind
		version, boutique string
		fichiers          []string
		attendus          map[string]string
	}
	boutiqueFichiers := []string{
		"init.php", "webservice/dispatcher.php", "js/porte.php", "js/retro-compat.js.php",
		"js/jquery/jquery.noConflict.php", "css/retro-compat.css.php",
		"admin634xyz/cron_currency_rates.php", "admin634xyz/index.php", "admin634xyz/ajax.php",
		"admin634xyz/filemanager/dialog.php", "admin634xyz/themes/default/css/admin.css",
		"admin634xyz/backups/sauvegarde.sql.bz2", "admin-api/index.php", "modules/m/ajax.php",
		"modules/m/views/css/front.css", "sansmarque/outil.php", "upload/perso.jpg",
	}
	commun := map[string]string{
		"/":                                   "200 FPM /index.php",
		"/index.php":                          "200 FPM /index.php",
		"/index.php?controller=404":           "200 FPM /index.php?controller=404",
		"/fr/3-vetements?q=1":                 "200 FPM /index.php?q=1",
		"/api/products":                       "200 FPM /webservice/dispatcher.php?url=products",
		"/webservice/dispatcher.php":          "200 FPM /webservice/dispatcher.php",
		"/admin634xyz/":                       "200 FPM /admin634xyz/index.php",
		"/admin634xyz/index.php":              "200 FPM /admin634xyz/index.php",
		"/admin634xyz/?controller=AdminLogin": "200 FPM /admin634xyz/index.php?controller=AdminLogin",
		"/admin634xyz/sell/catalog/products":  "200 FPM /admin634xyz/index.php",
		"/admin634xyz/index.php/sell/catalog/products": "200 FPM /admin634xyz/index.php /sell/catalog/products",
		"/admin634xyz/ajax.php":                        "200 FPM /admin634xyz/ajax.php",
		"/admin634xyz/filemanager/dialog.php":          "200 FPM /admin634xyz/filemanager/dialog.php",
		"/admin634xyz/cron_currency_rates.php":         "200 FPM /admin634xyz/cron_currency_rates.php",
		"/admin634xyz/absent.php":                      "404",
		"/admin634xyz/themes/default/css/admin.css":    "200 admin634xyz/themes/default/css/admin.css",
		"/admin634xyz/backups/sauvegarde.sql.bz2":      "403",
		"/init.php":                      "404",
		"/sansmarque/outil.php":          "404",
		"/modules/m/views/css/front.css": "200 modules/m/views/css/front.css",
	}
	fusion := func(propres map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range commun {
			out[k] = v
		}
		for k, v := range propres {
			out[k] = v
		}
		return out
	}
	tous := map[string]cas{
		"php-plain": {kind: model.KindPHPPlain, fichiers: []string{"index.php", "outil.php", "admin/console.php",
			"sous/script.php", "uploads/depose.php", "page.txt"},
			attendus: map[string]string{
				"/":                            "200 FPM /index.php",
				"/index.php?x=1":               "200 FPM /index.php?x=1",
				"/outil.php":                   "200 FPM /outil.php",
				"/admin/console.php":           "200 FPM /admin/console.php",
				"/sous/script.php/chemin/info": "200 FPM /sous/script.php /chemin/info",
				"/uploads/depose.php":          "200 FPM /uploads/depose.php",
				"/absent.php":                  "404",
				"/page-inconnue":               "200 FPM /index.php",
				"/page.txt":                    "200 page.txt",
				"/.git/config":                 "403",
			}},
		"symfony": {kind: model.KindSymfony, version: "6.4", fichiers: []string{"index.php", "autre.php"},
			attendus: map[string]string{
				"/":            "200 FPM /index.php",
				"/route/x?a=b": "200 FPM /index.php?a=b",
				"/index.php":   "404",
				"/autre.php":   "404",
			}},
		"prestashop-1.7": {kind: model.KindPrestashop, boutique: "1.7.8.11", fichiers: boutiqueFichiers,
			attendus: fusion(map[string]string{
				"/modules/m/ajax.php":              "200 FPM /modules/m/ajax.php",
				"/js/retro-compat.js.php":          "200 FPM /js/retro-compat.js.php",
				"/js/jquery/jquery.noConflict.php": "200 FPM /js/jquery/jquery.noConflict.php",
				"/js/absent.js":                    "200 FPM /js/retro-compat.js.php?file=absent.js",
				"/js/sous/absent.js?v=2":           "200 FPM /js/retro-compat.js.php?file=absent.js&v=2",
				"/js/absent.css":                   "200 FPM /css/retro-compat.css.php?file=absent.css",
				"/css/retro-compat.css.php":        "200 FPM /css/retro-compat.css.php",
				"/js/porte.php":                    "404",
				"/upload/perso.jpg":                "200 FPM /index.php",
				"/admin-api/oauth/token":           "200 FPM /index.php",
			})},
		"prestashop-8": {kind: model.KindPrestashop, boutique: "8.1.7", fichiers: boutiqueFichiers,
			attendus: fusion(map[string]string{
				"/modules/m/ajax.php":              "200 FPM /modules/m/ajax.php",
				"/js/jquery/jquery.noConflict.php": "200 FPM /js/jquery/jquery.noConflict.php",
				"/js/absent.js":                    "200 FPM /js/retro-compat.js.php?file=absent.js",
				"/js/porte.php":                    "403",
				"/upload/perso.jpg":                "403",
				"/admin634xyz/get-file-admin.php":  "404",
			})},
		"prestashop-9": {kind: model.KindPrestashop, boutique: "9.0.0", fichiers: boutiqueFichiers,
			attendus: fusion(map[string]string{
				"/modules/m/ajax.php":       "403",
				"/modules/m/ajax.php/suite": "403",
				"/js/retro-compat.js.php":   "403",
				"/js/absent.js":             "200 FPM /index.php",
				"/admin-api/":               "200 FPM /admin-api/index.php",
				"/admin-api/index.php":      "200 FPM /admin-api/index.php",
				"/admin-api/oauth/token":    "200 FPM /admin-api/index.php",
				"/admin-api/x.css":          "200 FPM /admin-api/index.php",
			})},
	}
	for nom, c := range tous {
		t.Run(nom, func(t *testing.T) {
			prov, _ := fixture(t, "nginx")
			p := project(t, "shop.fr", c.kind, c.version, "8.2")
			if c.boutique != "" {
				preparerBoutique(t, p, c.boutique)
			}
			for _, f := range c.fichiers {
				ecrireDansDocroot(t, p, f, []byte(f+"\n"))
			}
			if err := os.MkdirAll(p.Spec.Layout.LogDir, 0o755); err != nil {
				t.Fatal(err)
			}
			bp := buildMain(t, prov, p)
			vhost := string(bp.Content)
			m := regexp.MustCompile(`fastcgi_pass unix:([^;]+);`).FindStringSubmatch(vhost)
			if m == nil {
				t.Fatalf("aucun socket FPM dans le vhost :\n%s", vhost)
			}
			vhost = strings.ReplaceAll(vhost, m[1], fauxFPMDetaille(t, p.Spec.Layout.Docroot))
			url := demarrerNginx(t, nginx, prov, vhost, poserModesReels(t, p))
			for uri, voulu := range c.attendus {
				if obtenu := interroger(t, url, "shop.fr", uri); obtenu != voulu {
					t.Errorf("GET %s : %q, attendu %q", uri, obtenu, voulu)
				}
			}
		})
	}
}

// poserModesReels donne à l'arborescence du projet les modes d'un vrai parc —
// racine en 0711, répertoires en 0710, fichiers en 0640 (D51), à un compte de
// projet et à son groupe — et rend la directive qui fait servir nginx sous
// www-data, membre de ce groupe (D25) : traverser sans énumérer, sous
// disable_symlinks, c'est ce qui a fait manquer l'accueil (D87). Hors root, rien
// n'est posé : nginx tourne alors sous le propriétaire des fichiers.
func poserModesReels(t *testing.T, p *model.Project) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Log("hors root : modes réels non posés, nginx sert sous le propriétaire des fichiers")
		return ""
	}
	if _, err := user.Lookup("www-data"); err != nil {
		t.Skip("www-data absent")
	}
	groupe, err := user.LookupGroup("nogroup")
	if err != nil {
		t.Skip("groupe nogroup absent")
	}
	gid, _ := strconv.Atoi(groupe.Gid)
	const uid = 4242 // un compte de projet, distinct de root et de www-data
	racine := p.Spec.Layout.Root
	err = filepath.WalkDir(racine, func(chemin string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := os.Lchown(chemin, uid, gid); err != nil {
			return err
		}
		switch {
		case chemin == racine:
			return os.Chmod(chemin, 0o711)
		case d.IsDir():
			return os.Chmod(chemin, 0o710)
		case d.Type().IsRegular():
			return os.Chmod(chemin, 0o640)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	traversable(t, filepath.Dir(racine))
	for _, d := range []string{p.Spec.Layout.LogDir} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
		traversable(t, filepath.Dir(d))
	}
	return "user www-data nogroup;\n"
}

// traversable rend un répertoire et ses parents traversables par tous, comme
// le sont / et /home : les répertoires temporaires des tests naissent en 0700.
func traversable(t *testing.T, dir string) {
	t.Helper()
	for d := dir; d != "/" && d != "."; d = filepath.Dir(d) {
		st, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if m := st.Mode().Perm(); m&0o011 != 0o011 {
			if err := os.Chmod(d, m|0o011); err != nil {
				t.Fatal(err)
			}
		}
	}
}
