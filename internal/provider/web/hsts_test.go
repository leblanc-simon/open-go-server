package web

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

var reHSTS = regexp.MustCompile(`Strict-Transport-Security "([^"]*)"`)

// R4-01 (D119) : HSTS « includeSubDomains » était posé sur tout vhost en TLS,
// sans dérogation : un sous-domaine servi en HTTP seul, dans le parc ou
// ailleurs, devenait injoignable un an durant pour qui avait visité le domaine.
// Et « hsts: false » retirait l'en-tête, ce qui n'efface rien : le navigateur
// garde la politique jusqu'à son échéance. includeSubDomains reste le défaut,
// « hstsSubdomains: false » le retire, « hsts: false » pose « max-age=0 », qui
// efface la politique au passage suivant, et « hsts: none » n'envoie plus rien.
func TestHSTSIncludeSubDomainsDesactivableEtRetraitParMaxAgeZero(t *testing.T) {
	cas := []struct {
		nom     string
		options map[string]any
		voulu   string // "" : aucun en-tête
	}{
		{"défaut", nil, "max-age=31536000; includeSubDomains"},
		{"sans sous-domaines", map[string]any{"hstsSubdomains": false}, "max-age=31536000"},
		{"retrait", map[string]any{"hsts": false}, "max-age=0"},
		{"retrait sans sous-domaines", map[string]any{"hsts": false, "hstsSubdomains": false}, "max-age=0"},
		{"aucun en-tête", map[string]any{"hsts": "none"}, ""},
		{"actif explicite", map[string]any{"hsts": true, "hstsSubdomains": true}, "max-age=31536000; includeSubDomains"},
	}
	for _, moteur := range []string{"apache", "nginx"} {
		for _, kind := range []model.AppKind{model.KindStatic, model.KindPHPPlain, model.KindSymfony, model.KindPrestashop, model.KindGo} {
			for _, c := range cas {
				t.Run(moteur+"/"+string(kind)+"/"+c.nom, func(t *testing.T) {
					prov, _ := fixture(t, moteur)
					prov.Certificate = certificatCouvrant("shop.fr", "www.shop.fr")
					p := project(t, "shop.fr", kind, "", "8.5")
					p.Spec.Web.VHosts = prov.vhosts(p)
					opts := map[string]any{}
					for k, v := range c.options {
						opts[k] = v
					}
					if kind == model.KindGo {
						opts["proxyTarget"] = "127.0.0.1:20101"
					}
					p.Spec.Web.VHosts[0].Options = opts
					bp, err := prov.Build(p, p.Spec.Web.VHosts[0])
					if err != nil {
						t.Fatal(err)
					}
					port80, port443 := blocs(t, moteur, string(bp.Content))
					if port443 == "" {
						t.Fatalf("bloc 443 attendu :\n%s", bp.Content)
					}
					if reHSTS.MatchString(port80) {
						t.Errorf("HSTS n'a de sens qu'en HTTPS : le port 80 le porte :\n%s", port80)
					}
					trouves := reHSTS.FindAllStringSubmatch(port443, -1)
					switch {
					case c.voulu == "" && len(trouves) > 0:
						t.Errorf("aucun en-tête attendu, obtenu %q", trouves[0][1])
					case c.voulu != "" && len(trouves) != 1:
						t.Errorf("un en-tête %q attendu, %d trouvé(s) :\n%s", c.voulu, len(trouves), port443)
					case c.voulu != "" && trouves[0][1] != c.voulu:
						t.Errorf("en-tête %q, attendu %q", trouves[0][1], c.voulu)
					}
				})
			}
		}
	}
}

// Un template surchargé (/etc/ogs/templates) qui écrit lui-même l'en-tête sous
// « if .HSTS » garde son sens : HSTS est vrai tant que la politique est active,
// faux pour un retrait comme pour « none ».
func TestHSTSResteUnBooleenPourLesTemplatesSurcharges(t *testing.T) {
	for _, c := range []struct {
		options map[string]any
		voulu   bool
	}{
		{nil, true},
		{map[string]any{"hstsSubdomains": false}, true},
		{map[string]any{"hsts": false}, false},
		{map[string]any{"hsts": "none"}, false},
		{map[string]any{"hsts": "valeur-inconnue"}, true},
	} {
		if got := hstsActive(c.options); got != c.voulu {
			t.Errorf("hstsActive(%v) = %v, attendu %v", c.options, got, c.voulu)
		}
	}
}

// Une valeur non reconnue (« off », « false » entre guillemets) laissait la
// politique complète en silence : le plan l'annonce, en information seulement —
// rien n'échoue, le vhost est écrit (D119).
func TestUneOptionHSTSNonReconnueEstAnnonceeParLePlan(t *testing.T) {
	prov, _ := fixture(t, "nginx")
	p := project(t, "shop.fr", model.KindStatic, "", "")
	p.Spec.Web.VHosts = prov.vhosts(p)
	p.Spec.Web.VHosts[0].Options = map[string]any{"hsts": "off", "hstsSubdomains": "false"}
	steps, err := prov.Plan(context.Background(), p, Observed{})
	if err != nil {
		t.Fatal(err)
	}
	// Une note du plan, jamais une étape : une étape NoOp n'était montrée nulle
	// part (D118).
	for _, s := range steps {
		if strings.HasSuffix(s.Resource, "/hsts") {
			t.Errorf("l'option HSTS n'est pas une étape : %+v", s)
		}
	}
	notes := prov.HSTSNotes(p)
	if len(notes) != 1 || !strings.Contains(notes[0], `"off"`) || !strings.Contains(notes[0], "hstsSubdomains") {
		t.Errorf("une note attendue, nommant les deux options : %v", notes)
	}
	for _, o := range []map[string]any{nil, {"hsts": false}, {"hsts": "None"}, {"hstsSubdomains": false}} {
		if pb := HSTSOptionProblems(o); len(pb) > 0 {
			t.Errorf("%v est reconnue : %v", o, pb)
		}
	}
}
