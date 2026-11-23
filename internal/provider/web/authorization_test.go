package web

import (
	"io"
	"net"
	"net/http"
	"net/http/fcgi"
	"os"
	"regexp"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

// Tout vhost PHP d'Apache relaie l'en-tête Authorization à FPM : mod_proxy_fcgi
// le tait sans CGIPassAuth, et le .htaccess qui le relaie d'ordinaire n'est pas
// lu (D99).
func TestLeVhostApacheRelaieAuthorizationAFPM(t *testing.T) {
	for _, kind := range []model.AppKind{model.KindPHPPlain, model.KindSymfony, model.KindPrestashop} {
		prov, _ := fixture(t, "apache")
		p := project(t, "boutique.fr", kind, "", "8.5")
		vhost := string(buildMain(t, prov, p).Content)
		if n := strings.Count(vhost, "CGIPassAuth On"); n != 1 {
			t.Errorf("%s : %d CGIPassAuth On, attendu 1 :\n%s", kind, n, vhost)
		}
		// Le relais par réécriture ferait un doublon de la variable.
		if strings.Contains(vhost, "E=HTTP_AUTHORIZATION") {
			t.Errorf("%s : relais par réécriture en plus de CGIPassAuth", kind)
		}
	}
}

// fauxFPMAuthorization répond par l'en-tête Authorization que FPM a reçu.
func fauxFPMAuthorization(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fpm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := dir + "/s.sock"
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go fcgi.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "auth="+r.Header.Get("Authorization"))
	}))
	return sock
}

// TestApacheReelRelaieAuthorization éprouve, sur le vrai Apache, qu'un jeton
// Bearer atteint FPM — script nommé comme route réécrite vers le front
// controller.
//
//	OGS_TEST_APACHE=1 go test ./internal/provider/web/ -run TestApacheReelRelaieAuthorization
func TestApacheReelRelaieAuthorization(t *testing.T) {
	if os.Getenv("OGS_TEST_APACHE") == "" {
		t.Skip("test sur le vrai Apache désactivé : exporter OGS_TEST_APACHE=1 pour l'exécuter")
	}
	const apache, modules = "/usr/sbin/apache2", "/usr/lib/apache2/modules"
	for _, f := range []string{apache, modules, "/etc/mime.types"} {
		if _, err := os.Stat(f); err != nil {
			t.Skipf("%s absent", f)
		}
	}
	for _, c := range []struct {
		kind model.AppKind
		uris []string
	}{
		{model.KindPHPPlain, []string{"/index.php", "/script.php"}},
		{model.KindSymfony, []string{"/index.php", "/api/commandes"}},
		{model.KindPrestashop, []string{"/index.php", "/api/products"}},
	} {
		t.Run(string(c.kind), func(t *testing.T) {
			prov, _ := fixture(t, "apache")
			p := project(t, "boutique.fr", c.kind, "", "8.5")
			if c.kind == model.KindPrestashop {
				preparerBoutique(t, p, "8.1.7")
				ecrireDansDocroot(t, p, "webservice/dispatcher.php", []byte("<?php\n"))
			} else {
				ecrireDansDocroot(t, p, "index.php", []byte("<?php\n"))
				ecrireDansDocroot(t, p, "script.php", []byte("<?php\n"))
			}
			if err := os.MkdirAll(p.Spec.Layout.LogDir, 0o755); err != nil {
				t.Fatal(err)
			}
			vhost := string(buildMain(t, prov, p).Content)
			m := regexp.MustCompile(`proxy:unix:([^|]+)\|`).FindStringSubmatch(vhost)
			if m == nil {
				t.Fatalf("aucun socket FPM dans le vhost :\n%s", vhost)
			}
			vhost = strings.ReplaceAll(vhost, m[1], fauxFPMAuthorization(t))
			url := demarrerApache(t, apache, modules, vhost)
			for _, uri := range c.uris {
				req, err := http.NewRequest("GET", url+uri, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Host = "boutique.fr"
				req.Header.Set("Authorization", "Bearer jeton-ogs")
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				corps, _ := io.ReadAll(res.Body)
				res.Body.Close()
				if got := strings.TrimSpace(string(corps)); res.StatusCode != http.StatusOK || got != "auth=Bearer jeton-ogs" {
					t.Errorf("GET %s : %d %q, attendu 200 « auth=Bearer jeton-ogs »", uri, res.StatusCode, got)
				}
			}
		})
	}
}
