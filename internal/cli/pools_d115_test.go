package cli

import (
	"os"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/model"
)

// R1-28 (D115) : un projet passé d'un type PHP à un type sans PHP gardait ses
// pools — le plan ne les observait que sous at.NeedsPHP —, qui tournaient
// toujours sous son compte et survivaient même à sa suppression.
func TestUnTypeSansPHPRetireLesPoolsDuProjet(t *testing.T) {
	for _, garderDeclaration := range []bool{false, true} {
		h := newHarness(t)
		h.mustRun("init")
		h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
		h.mustRun("project", "create", "blog.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
		h.mustRun("apply", "--yes")
		pool := h.path("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf")
		voisin := h.path("etc", "php", "8.5", "fpm", "pool.d", "blogfr.conf")
		if _, err := os.Stat(pool); err != nil {
			t.Fatal(err)
		}
		h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
			p.Spec.App.Kind = model.KindStatic
			p.Spec.App.Version = ""
			if !garderDeclaration {
				p.Spec.PHP.Pools = nil
			}
		})
		plan := h.mustRun("plan", "shopfr")
		if !strings.Contains(plan, "pool php obsolète") {
			t.Errorf("(pools déclarés : %v) le plan doit retirer le pool :\n%s", garderDeclaration, plan)
		}
		h.mustRun("apply", "shopfr", "--yes")
		if _, err := os.Stat(pool); !os.IsNotExist(err) {
			t.Errorf("(pools déclarés : %v) le pool d'un projet sans PHP doit être retiré", garderDeclaration)
		}
		if _, err := os.Stat(voisin); err != nil {
			t.Errorf("le pool du voisin ne doit jamais être touché : %v", err)
		}
	}
}

// La suppression retrouve les pools par leur en-tête (D76), comme les unités
// (Unit.StaleFiles) : un pool que la déclaration ne nomme plus — type changé,
// pool retiré sans apply — survivait au projet, sous un compte supprimé.
func TestLaSuppressionRetireUnPoolQueLaDeclarationNeNommePlus(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("project", "create", "shop.de", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("apply", "--yes")
	pool := h.path("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf")
	voisin := h.path("etc", "php", "8.5", "fpm", "pool.d", "shopde.conf")
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.App.Kind = model.KindStatic
		p.Spec.App.Version = ""
		p.Spec.PHP.Pools = nil
	})
	h.mustRunPartial("project", "delete", "shopfr", "--yes")
	if _, err := os.Stat(pool); !os.IsNotExist(err) {
		t.Error("le pool écrit pour le projet doit partir avec lui, déclaré ou non")
	}
	if _, err := os.Stat(voisin); err != nil {
		t.Errorf("le pool du voisin ne doit jamais être touché : %v", err)
	}
}

// Les sessions PHP partent avec le projet, --keep-data compris (D115) : le
// fichier tmpfiles recalculé ne les purgerait plus, et un projet recréé sous
// cet identifiant hériterait des sessions ouvertes.
func TestLesSessionsPartentMemeSousKeepData(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("apply", "shopfr", "--yes")
	dir := h.path("var", "lib", "ogs", "sessions", "shopfr")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("répertoire des sessions absent après l'apply : %v", err)
	}
	h.mustRunPartial("project", "delete", "shopfr", "--keep-data", "--yes")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("les sessions du projet supprimé restent : %v", err)
	}
}

// Un php_value masqué par le php_admin_value du socle (D115) ne compte pas au
// code de retour : une déclaration ancienne qui le porte rendait 6 à chaque
// apply. doctor le nomme, avec son remède.
func TestUnPHPValueMasqueNeCompteQuADoctor(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.PHP.Pools[0].Settings = model.PoolSettings{
			"php_value[session.save_path]": model.Value("/srv/s"),
			"php_value[open_basedir]":      model.Value("/srv")}
	})
	h.mustRun("apply", "shopfr", "--yes")
	h.mustRun("apply", "shopfr", "--yes")
	out, _ := h.run("doctor")
	for _, k := range []string{"php_value[session.save_path]", "php_value[open_basedir]"} {
		if !strings.Contains(out, k) || !strings.Contains(out, "l'emporte") {
			t.Errorf("doctor doit nommer %s sans effet :\n%s", k, out)
		}
	}
}
