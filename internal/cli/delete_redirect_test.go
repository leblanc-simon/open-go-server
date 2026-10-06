package cli

import (
	"os"
	"strings"
	"testing"
)

// Le nom d'un vhost de redirection suit sa cible (D119). Une cible changée, ou
// une redirection retirée, sans apply, puis « project delete » : recalculés
// depuis la seule déclaration, les fichiers du projet ne nommaient plus le vhost
// écrit, qui restait actif en désignant les journaux et le certificat effacés —
// le rechargement suivant faisait tomber le parc (D77). La suppression retire
// aussi tout vhost que l'en-tête attribue au projet.
func TestSuppressionRetireLeVHostDUneCibleDeRedirectionAbandonnee(t *testing.T) {
	for nom, edition := range map[string]func(string) string{
		"cible changée": func(s string) string {
			return strings.Replace(s, "target: tiers.example", "target: autre.example", 1)
		},
		"redirection retirée": func(s string) string {
			return strings.Replace(s, "    - name: promo.fr\n      role: redirect\n      target: tiers.example\n", "", 1)
		},
	} {
		t.Run(nom, func(t *testing.T) {
			h := newHarness(t)
			h.mustRun("init")
			h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
			decl := h.path("etc", "ogs", "projects", "shopfr.yaml")
			ajouter := func(f func(string) string) {
				t.Helper()
				c, err := os.ReadFile(decl)
				if err != nil {
					t.Fatal(err)
				}
				n := f(string(c))
				if n == string(c) {
					t.Fatalf("déclaration inchangée :\n%s", c)
				}
				if err := os.WriteFile(decl, []byte(n), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ajouter(func(s string) string {
				return strings.Replace(s, "  domains:\n",
					"  domains:\n    - name: promo.fr\n      role: redirect\n      target: tiers.example\n", 1)
			})
			h.mustRun("apply", "shopfr", "--yes")
			ecrit := h.path("etc", "apache2", "sites-available", "shopfr_redirect-tiers.example.conf")
			actif := h.path("etc", "apache2", "sites-enabled", "shopfr_redirect-tiers.example.conf")
			for _, f := range []string{ecrit, actif} {
				if _, err := os.Lstat(f); err != nil {
					t.Fatalf("vhost de la cible attendu : %v", err)
				}
			}
			ajouter(edition)
			out, code := h.run("project", "delete", "shopfr", "--yes")
			if code != CodeOK {
				t.Fatalf("suppression : code %d\n%s", code, out)
			}
			for _, f := range []string{ecrit, actif} {
				if _, err := os.Lstat(f); !os.IsNotExist(err) {
					t.Errorf("%s doit partir avec le projet : %v", f, err)
				}
			}
		})
	}
}
