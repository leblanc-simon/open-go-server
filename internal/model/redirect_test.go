package model

import (
	"reflect"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/naming"
)

func projetAvecRedirections(t *testing.T, domaines ...Domain) *Project {
	t.Helper()
	p := New("shop.fr", naming.EnvProd, KindStatic)
	if err := p.Normalize(t.TempDir(), "preprod-pp.net", WithLogsRoot(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	p.Spec.Domains = append(p.Spec.Domains, Domain{Name: "www.shop.fr", Role: "alias"})
	p.Spec.Domains = append(p.Spec.Domains, domaines...)
	return p
}

// R4-07 (D119) : la cible déclarée d'une redirection était ignorée, et tous les
// domaines redirigés partaient en 301 vers le premier domaine du projet. Chaque
// cible distincte a désormais son vhost ; celui du premier domaine servi garde
// le nom « redirect », donc le fichier qu'il avait.
func TestLaCibleDeclareeDUneRedirectionEstHonoree(t *testing.T) {
	p := projetAvecRedirections(t,
		Domain{Name: "ancien.fr", Role: "redirect", Target: "shop.fr"},
		Domain{Name: "shop.com", Role: "redirect", Target: "www.shop.fr"},
		Domain{Name: "promo.fr", Role: "redirect", Target: "Autre-Site.com."},
		Domain{Name: "legacy.fr", Role: "redirect"},
		// Une cible qui est elle-même une redirection est suivie : un saut de moins.
		Domain{Name: "chaine.fr", Role: "redirect", Target: "shop.com"},
	)
	type vu struct {
		nom, cible string
		domaines   []string
	}
	var obtenus []vu
	for _, vh := range p.VHosts()[1:] {
		if vh.Template != "redirect" {
			t.Errorf("vhost %s : template %q", vh.Name, vh.Template)
		}
		obtenus = append(obtenus, vu{vh.Name, vh.Options["target"].(string), vh.Domains})
	}
	voulus := []vu{
		{"redirect", "shop.fr", []string{"ancien.fr", "legacy.fr"}},
		{"redirect-www.shop.fr", "www.shop.fr", []string{"shop.com", "chaine.fr"}},
		{"redirect-autre-site.com", "autre-site.com", []string{"promo.fr"}},
	}
	if !reflect.DeepEqual(obtenus, voulus) {
		t.Errorf("vhosts de redirection :\n obtenus %+v\n voulus  %+v", obtenus, voulus)
	}
	if problems := p.RedirectProblems(); len(problems) > 0 {
		t.Errorf("aucune cible écartée attendue : %v", problems)
	}
	// Les fichiers restent distincts, et celui de la cible principale inchangé.
	fichiers := map[string]bool{}
	for _, vh := range p.VHosts() {
		fichiers[p.VHostFile(vh)] = true
	}
	for _, f := range []string{"shopfr.conf", "shopfr_redirect.conf", "shopfr_redirect-www.shop.fr.conf",
		"shopfr_redirect-autre-site.com.conf"} {
		if !fichiers[f] {
			t.Errorf("fichier %s attendu parmi %v", f, fichiers)
		}
	}
}

// Une cible qui n'est pas un nom d'hôte — elle atteindrait la configuration du
// serveur telle quelle —, ou une boucle de redirections, est écartée : le domaine
// vise le premier domaine servi, comme avant D119, et RedirectProblems le dit.
// Jamais de refus au chargement : la cible était ignorée, et une déclaration
// servie jusque-là ne doit pas arrêter le parc.
func TestUneCibleDeRedirectionInvalideRetombeSurLePremierDomaine(t *testing.T) {
	p := projetAvecRedirections(t,
		Domain{Name: "url.fr", Role: "redirect", Target: "https://autre.fr/page"},
		Domain{Name: "injecte.fr", Role: "redirect", Target: "autre.fr; return 200 x"},
		Domain{Name: "a.fr", Role: "redirect", Target: "b.fr"},
		Domain{Name: "b.fr", Role: "redirect", Target: "a.fr"},
	)
	vhosts := p.VHosts()
	if len(vhosts) != 2 || vhosts[1].Name != "redirect" || vhosts[1].Options["target"] != "shop.fr" {
		t.Fatalf("un seul vhost de redirection vers shop.fr attendu : %+v", vhosts)
	}
	if got := strings.Join(vhosts[1].Domains, " "); got != "url.fr injecte.fr a.fr b.fr" {
		t.Errorf("domaines redirigés : %s", got)
	}
	problems := strings.Join(p.RedirectProblems(), "\n")
	for _, voulu := range []string{"url.fr : ce n'est pas un nom d'hôte", "injecte.fr : ce n'est pas un nom d'hôte",
		"boucle de redirections"} {
		if !strings.Contains(problems, voulu) {
			t.Errorf("%q attendu dans :\n%s", voulu, problems)
		}
	}
}

// Une cible très longue ne donne pas un nom de fichier que le système refuserait.
func TestLeNomDUnVHostDeRedirectionResteBorne(t *testing.T) {
	long := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + ".fr"
	p := projetAvecRedirections(t, Domain{Name: "x.fr", Role: "redirect", Target: long})
	vhosts := p.VHosts()
	if len(vhosts) != 2 {
		t.Fatalf("deux vhosts attendus : %+v", vhosts)
	}
	if f := p.VHostFile(vhosts[1]); len(f) > 150 {
		t.Errorf("nom de fichier de %d octets : %s", len(f), f)
	}
	if vhosts[1].Options["target"] != long {
		t.Errorf("la cible elle-même n'est pas tronquée : %v", vhosts[1].Options["target"])
	}
}
