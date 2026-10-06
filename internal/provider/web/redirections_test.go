package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

// R4-07 (D119) : chaque domaine redirigé part vers sa cible déclarée, par un
// vhost par cible distincte. Le certificat du projet couvre toutes ses
// redirections (D80) : chaque vhost de redirection qu'il couvre est servi en
// HTTPS (D79), et son port 80 sert le challenge ACME avant toute redirection.
func TestChaqueRedirectionVaASaCibleDeclaree(t *testing.T) {
	for _, moteur := range []string{"apache", "nginx"} {
		t.Run(moteur, func(t *testing.T) {
			prov, _ := fixture(t, moteur)
			prov.Certificate = certificatCouvrant("shop.fr", "www.shop.fr", "ancien.fr", "shop.com", "promo.fr")
			p := project(t, "shop.fr", model.KindStatic, "", "")
			p.Spec.Domains = append(p.Spec.Domains,
				model.Domain{Name: "ancien.fr", Role: "redirect", Target: "shop.fr"},
				model.Domain{Name: "shop.com", Role: "redirect", Target: "www.shop.fr"},
				model.Domain{Name: "promo.fr", Role: "redirect", Target: "autre-site.com"})
			voulu := map[string]struct{ nom, cible string }{
				"ancien.fr": {"shopfr_redirect", "https://shop.fr"},
				"shop.com":  {"shopfr_redirect-www.shop.fr", "https://www.shop.fr"},
				"promo.fr":  {"shopfr_redirect-autre-site.com", "https://autre-site.com"},
			}
			vus := 0
			for _, vh := range prov.vhosts(p)[1:] {
				bp, err := prov.Build(p, vh)
				if err != nil {
					t.Fatal(err)
				}
				for _, d := range vh.Domains {
					v, ok := voulu[d]
					if !ok {
						t.Fatalf("domaine inattendu %s dans %s", d, bp.Name)
					}
					vus++
					if bp.Name != v.nom {
						t.Errorf("%s : vhost %s, attendu %s", d, bp.Name, v.nom)
					}
					port80, port443 := blocs(t, moteur, string(bp.Content))
					if port443 == "" {
						t.Errorf("%s est couvert : son bloc 443 doit être rendu", d)
					}
					for nom, bloc := range map[string]string{"80": port80, "443": port443} {
						if cible := redirigeVers(moteur, bloc); cible != v.cible {
							t.Errorf("%s, port %s : cible %q, attendu %q", d, nom, cible, v.cible)
						}
					}
					if !strings.Contains(port80, "acme-challenge") {
						t.Errorf("%s : le port 80 doit servir le challenge ACME avant la redirection", d)
					}
				}
			}
			if vus != len(voulu) {
				t.Errorf("%d domaines redirigés rendus, %d attendus", vus, len(voulu))
			}
		})
	}
}

// Une cible écartée (D119) n'arrête rien : les vhosts sont écrits, le domaine
// vise le premier domaine servi, et le plan le dit par une étape non critique
// (code 6) qui nomme la cible.
func TestUneCibleDeRedirectionEcarteeEstAnnonceeParLePlan(t *testing.T) {
	prov, _ := fixture(t, "nginx")
	p := project(t, "shop.fr", model.KindStatic, "", "")
	p.Spec.Domains = append(p.Spec.Domains,
		model.Domain{Name: "url.fr", Role: "redirect", Target: "https://autre.fr/page"})
	if err := os.MkdirAll(p.Spec.Layout.Docroot, 0o755); err != nil {
		t.Fatal(err)
	}
	plan, report, err := appliquerVHosts(t, prov, p)
	if err != nil {
		t.Fatalf("une cible écartée ne doit pas faire échouer l'apply : %v", err)
	}
	var etape bool
	for _, s := range plan.Steps {
		if s.Resource == "web/redirect/"+p.ID() {
			etape = true
			if s.Critical {
				t.Error("l'étape qui annonce une cible écartée doit être non critique")
			}
		}
	}
	if !etape {
		t.Fatalf("aucune étape n'annonce la cible écartée : %v", plan.Steps)
	}
	if len(report.Warnings) == 0 {
		t.Error("une cible écartée rend un avertissement, donc le code 6")
	}
	c, err := os.ReadFile(filepath.Join(prov.Engine.AvailableDir(), "shopfr_redirect.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if cible := redirigeVers("nginx", string(c)); cible != "http://shop.fr" {
		t.Errorf("la redirection vise %q, attendu le premier domaine servi", cible)
	}
}
