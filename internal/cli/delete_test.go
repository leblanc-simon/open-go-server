package cli

import (
	"os"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/model"
)

// projetDeRacine fabrique le strict nécessaire au dernier rempart : une racine et
// le marqueur d'adoption. Le projet n'est pas validé — c'est précisément le cas
// qu'on éprouve, celui d'une déclaration écrite avant que la validation ne se
// durcisse, ou à la main.
func projetDeRacine(root string, adopté bool) *model.Project {
	p := &model.Project{}
	p.Spec.Layout.Root = root
	if adopté {
		p.Metadata.Labels = map[string]string{"adopted": "true"}
	}
	return p
}

func TestSuppressionRefuseUneRacineDangereuse(t *testing.T) {
	// os.RemoveAll ne demande rien à personne : le contrôle est refait au moment
	// du geste, la validation d'entrée pouvant n'avoir jamais vu ce fichier.
	a := &App{Cfg: config.Default(), Paths: config.NewPaths("")}
	cas := []struct {
		racine string
		adopté bool
		motif  string
	}{
		{"/", false, "la racine du système"},
		{"", false, "une racine vide, donc un chemin relatif"},
		{"var/www/shop", false, "un chemin relatif"},
		{"/home", false, "la racine des homes elle-même"},
		{"/home/", true, "la racine des homes, adoption ou non"},
		{"/var/www", true, "un docroot partagé, adopté ou non"},
		{"/etc", true, "un emplacement système"},
		{"/srv/../var/log", true, "un emplacement système, une fois le chemin réduit"},
		{"/opt/appli/../..", true, "la racine du système, une fois le chemin réduit"},
		{"/var/www/boutique", false, "hors de la racine des homes sans adoption"},
	}
	for _, c := range cas {
		err := a.checkRemovableRoot(projetDeRacine(c.racine, c.adopté))
		if err == nil {
			t.Errorf("%q devait être refusée (%s)", c.racine, c.motif)
			continue
		}
		// Le chemin est nommé : l'exploitant doit savoir quoi corriger.
		if !strings.Contains(err.Error(), c.racine) && c.racine != "" {
			t.Errorf("le message doit nommer %q : %v", c.racine, err)
		}
	}
}

func TestSuppressionAccepteLesRacinesLegitimes(t *testing.T) {
	// Le rempart ne doit rien empêcher de légitime : un projet du parc, et un
	// projet adopté qui garde le home réel de son compte (« ogs import »).
	a := &App{Cfg: config.Default(), Paths: config.NewPaths("")}
	for _, c := range []struct {
		racine string
		adopté bool
	}{
		{"/home/shop.fr", false},
		{"/home/shop.fr/", false},
		{"/var/www/boutique", true},
		{"/srv/legacy/site", true},
	} {
		if err := a.checkRemovableRoot(projetDeRacine(c.racine, c.adopté)); err != nil {
			t.Errorf("%q devait être acceptée : %v", c.racine, err)
		}
	}
}

func TestSuppressionEffaceBienLArborescenceDUnProjetConforme(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")

	h.mustRun("project", "delete", "shopfr", "--yes")
	if _, err := os.Stat(h.path("home", "shop.fr")); !os.IsNotExist(err) {
		t.Errorf("l'arborescence du projet devait disparaître : %v", err)
	}
}

func TestSuppressionRefuseAvantLePointDeNonRetour(t *testing.T) {
	// Un rempart qui ne se dresse qu'à la dernière étape n'en est pas un : le
	// compte, les bases, les vhosts et la déclaration sont déjà partis quand
	// l'arborescence est refusée. La racine est donc éprouvée à l'entrée, et rien
	// n'est démonté.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")

	// Une racine que la validation d'alors laissait passer, écrite à la main dans
	// la déclaration : /var/www est partagé par tous les sites d'un serveur repris.
	decl := h.path("etc", "ogs", "projects", "shopfr.yaml")
	data, err := os.ReadFile(decl)
	if err != nil {
		t.Fatal(err)
	}
	patché := strings.ReplaceAll(string(data), h.path("home", "shop.fr"), "/var/www")
	if patché == string(data) {
		t.Fatalf("la racine du projet est attendue dans la déclaration :\n%s", data)
	}
	// Le label d'adoption : sans lui, c'est la validation d'entrée qui refuse une
	// racine hors de la racine des homes, et le rempart de la suppression ne serait
	// jamais atteint.
	patché = strings.Replace(patché, "  env: prod\n", "  env: prod\n  labels:\n    adopted: \"true\"\n", 1)
	if err := os.WriteFile(decl, []byte(patché), 0o600); err != nil {
		t.Fatal(err)
	}

	out, code := h.run("project", "delete", "shopfr", "--yes")
	if code != CodeValidation {
		t.Fatalf("une racine ineffaçable doit refuser le geste (code %d), obtenu %d :\n%s",
			CodeValidation, code, out)
	}
	if !strings.Contains(out, "--keep-data") {
		t.Errorf("le refus doit nommer la seule issue applicable :\n%s", out)
	}
	// Rien n'a été démonté : la déclaration est toujours à l'inventaire actif, et
	// le vhost du site n'a pas bougé.
	if _, err := os.Stat(decl); err != nil {
		t.Errorf("la déclaration doit rester à l'inventaire actif : %v", err)
	}
	if _, err := os.Stat(h.path("etc", "apache2", "sites-available", "shopfr.conf")); err != nil {
		t.Errorf("le vhost ne doit pas avoir été démonté : %v", err)
	}
}

func TestSuppressionSignaleUneArchiveSansDonnees(t *testing.T) {
	// Une archive à laquelle il manque un dump ne permet pas de restaurer le
	// site : rendre 0 ferait croire le contraire à qui enchaîne.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5",
		"--db", "mysql", "--tls=false", "--yes")

	out, code := h.run("project", "delete", "shopfr", "--yes")
	if !strings.Contains(out, "sauvegarde de") {
		t.Fatalf("un dump en échec doit être annoncé :\n%s", out)
	}
	if code != CodePartial {
		t.Errorf("une archive sans les données est un succès partiel, code %d :\n%s", code, out)
	}
}
