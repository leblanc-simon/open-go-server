package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/archive"
	"leblanc.io/open-go-server/internal/model"
)

// siteServi prépare un site PHP avec une tâche planifiée, et rend les chemins
// qui le servent : vhost, son lien d'activation, pool et fichier cron.
func siteServi(t *testing.T, h *harness) []string {
	t.Helper()
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "php-plain", "--php", "8.5", "--tls=false", "--yes")
	h.mustRun("cron", "add", "shopfr", "horodate", "--schedule", "* * * * *", "--command", "/bin/date")
	h.mustRun("apply", "shopfr", "--yes")
	chemins := []string{
		h.path("etc", "apache2", "sites-available", "shopfr.conf"),
		h.path("etc", "apache2", "sites-enabled", "shopfr.conf"),
		h.path("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf"),
		h.path("etc", "cron.d", "ogs-shopfr"),
	}
	for _, c := range chemins {
		if _, err := os.Lstat(c); err != nil {
			t.Fatalf("le site doit être servi avant la suppression : %v", err)
		}
	}
	return chemins
}

// Le site restait servi pendant l'archive : vhost, pool et cron ne partaient
// qu'après elle, et ce que le site écrivait après le dump manquait à l'archive
// (R1-17). Il est coupé avant l'archive et le dump (D114).
func TestSuppressionCoupeLeSiteAvantLArchive(t *testing.T) {
	h := newHarness(t)
	chemins := siteServi(t, h)

	var servis []string
	out, code := h.runEpie(context.Background(), "archive créée", func() {
		for _, c := range chemins {
			if _, err := os.Lstat(c); err == nil {
				servis = append(servis, c)
			}
		}
	}, "project", "delete", "shopfr", "--yes")
	if code != CodeOK {
		t.Fatalf("la suppression doit aboutir, code %d :\n%s", code, out)
	}
	if len(servis) > 0 {
		t.Errorf("le site doit être coupé avant l'archive, encore en place : %v\n%s", servis, out)
	}
	coupure, créée := strings.Index(out, "coupure du site"), strings.Index(out, "archive créée")
	if coupure < 0 || coupure > créée {
		t.Errorf("la coupure doit être annoncée avant l'archive :\n%s", out)
	}
	for _, c := range chemins {
		if _, err := os.Lstat(c); !os.IsNotExist(err) {
			t.Errorf("%s doit être parti : %v", c, err)
		}
	}
	// L'archive garde les configurations coupées, relues avant le retrait.
	archives, _ := filepath.Glob(h.path("var", "lib", "ogs", "archives", "shopfr-*"))
	if len(archives) != 1 {
		t.Fatalf("une archive attendue : %v", archives)
	}
	for _, nom := range []string{"config/shopfr.conf", "config/ogs-shopfr"} {
		if data, err := archive.ReadFile(archives[0], nom); err != nil || len(data) == 0 {
			t.Errorf("l'archive doit porter %s : %q (%v)", nom, data, err)
		}
	}
}

// Un abandon avant le point de non-retour rend le site tel qu'il était : une
// interruption pendant l'archive, ou un rempart qu'elle a laissé naître.
func TestSuppressionAbandonneeRetablitLeSite(t *testing.T) {
	for _, cas := range []struct {
		nom    string
		geste  func(t *testing.T, h *harness, interrompre context.CancelFunc)
		code   int
		archiv bool
	}{
		{"interruption", func(_ *testing.T, _ *harness, interrompre context.CancelFunc) { interrompre() },
			CodeCancelled, false},
		{"partage monté pendant l'archive", func(t *testing.T, h *harness, _ context.CancelFunc) {
			monterFactice(t, h.path("home", "shop.fr", "www", "medias"))
		}, CodeConflict, false},
	} {
		t.Run(cas.nom, func(t *testing.T) {
			h := newHarness(t)
			chemins := siteServi(t, h)
			monterFactice(t)
			avant := map[string]string{}
			for _, c := range chemins {
				avant[c] = lire(t, c)
			}
			ctx, interrompre := context.WithCancel(context.Background())
			defer interrompre()
			out, code := h.runEpie(ctx, "archive créée", func() { cas.geste(t, h, interrompre) },
				"project", "delete", "shopfr", "--yes")
			if code != cas.code {
				t.Fatalf("code %d, attendu %d :\n%s", code, cas.code, out)
			}
			if !strings.Contains(out, "rétablissement du site") {
				t.Errorf("le rétablissement doit être annoncé :\n%s", out)
			}
			for _, c := range chemins {
				if got := lire(t, c); got != avant[c] {
					t.Errorf("%s doit revenir tel qu'il était :\n%q\nau lieu de\n%q", c, got, avant[c])
				}
			}
			h.rienNAEteRetire(t, "shopfr")
			// Le site rétabli est conforme : le plan suivant n'a rien à réécrire.
			if plan := h.mustRun("plan", "shopfr"); strings.Contains(plan, "shopfr.conf") ||
				strings.Contains(plan, "ogs-shopfr ") {
				t.Errorf("le site rétabli doit être conforme :\n%s", plan)
			}
		})
	}
}

// lire rend le contenu d'un fichier, ou la cible d'un lien.
func lire(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("%s : %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		cible, err := os.Readlink(path)
		if err != nil {
			t.Fatal(err)
		}
		return "-> " + cible
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Les journaux partent avec le projet par un effacement récursif (D71) : un
// logDir recopié d'un voisin faisait effacer les siens (R1-26). Le conflit est
// refusé au plan du projet visé et à la suppression, nommé par doctor, et jamais
// au chargement — le parc entier ne doit pas s'arrêter pour une déclaration.
func TestUnLogDirRecopieDUnVoisinEstRefuse(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	h.mustRun("site", "create", "blog.fr", "--kind", "static", "--tls=false", "--yes")
	journaux := h.path("var", "log", "ogs-projects", "shopfr")
	témoin := filepath.Join(journaux, "a-garder.log")
	déposer(t, témoin, "journal de shopfr\n")
	h.modifierDeclaration(t, "blogfr", func(s string) string {
		return strings.ReplaceAll(s, h.path("var", "log", "ogs-projects", "blogfr"), journaux)
	})

	for _, args := range [][]string{{"plan", "blogfr"}, {"apply", "blogfr", "--yes"}} {
		out, code := h.run(args...)
		if code != CodeConflict || !strings.Contains(out, "répertoire de journaux") {
			t.Errorf("« ogs %s » : le logDir d'un voisin doit être refusé (code %d), obtenu %d :\n%s",
				strings.Join(args, " "), CodeConflict, code, out)
		}
	}
	out, code := h.run("project", "delete", "blogfr", "--yes")
	if code != CodeConflict || !strings.Contains(out, journaux) || !strings.Contains(out, "shopfr") {
		t.Errorf("la suppression doit refuser d'effacer les journaux du voisin (code %d), obtenu %d :\n%s",
			CodeConflict, code, out)
	}
	intact(t, témoin, "journal de shopfr\n")
	h.rienNAEteDemonte(t, "blogfr", out)

	// Jamais au chargement : les commandes de parc restent ouvertes.
	if out, code := h.run("status"); code == CodeValidation || code == CodeConflict {
		t.Errorf("le conflit ne doit pas arrêter le parc au chargement, code %d :\n%s", code, out)
	}
	if out, _ := h.run("doctor"); !strings.Contains(out, "répertoire de journaux") {
		t.Errorf("doctor doit nommer le conflit :\n%s", out)
	}
}

// Un projet « ufw » côtoyait le retour automatique du pare-feu sous le préfixe
// ogs-ufw- (D114) : l'identifiant est refusé à la création, jamais au chargement.
func TestLesIdentifiantsDesUnitésDeParcSontRéservésALaCréation(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	for _, args := range [][]string{
		{"project", "create", "ufw.test", "--id", "ufw"},
		{"site", "create", "restauration.test", "--id", "restauration", "--kind", "static", "--tls=false", "--yes"},
		{"project", "create", "renew.test", "--id", "renew"},
	} {
		out, code := h.run(args...)
		if code != CodeValidation || !strings.Contains(out, "réservé") {
			t.Errorf("« ogs %s » : identifiant réservé attendu (code %d), obtenu %d :\n%s",
				strings.Join(args, " "), CodeValidation, code, out)
		}
	}
	if decl, _ := filepath.Glob(h.path("etc", "ogs", "projects", "*.yaml")); len(decl) > 0 {
		t.Errorf("rien ne doit être déclaré : %v", decl)
	}
	// Un projet « ufw » déclaré avant la règle reste chargé et appliqué.
	h.mustRun("project", "create", "ufw.test", "--kind", "static")
	h.modifierDeclaration(t, "ufwtest", func(s string) string { return strings.ReplaceAll(s, "ufwtest", "ufw") })
	if err := os.Rename(h.path("etc", "ogs", "projects", "ufwtest.yaml"),
		h.path("etc", "ogs", "projects", "ufw.yaml")); err != nil {
		t.Fatal(err)
	}
	h.mustRun("apply", "ufw", "--yes")
	if out, code := h.run("status"); code == CodeValidation || code == CodeConflict {
		t.Errorf("un projet « ufw » déjà déclaré doit rester chargé, code %d :\n%s", code, out)
	}
	// La restauration crée le projet : elle refuse l'identifiant réservé.
	h.mustRun("project", "delete", "ufw", "--yes")
	out, code := h.run("project", "restore", h.derniereArchive(t), "--yes")
	if code != CodeValidation || !strings.Contains(out, "réservé") {
		t.Errorf("project restore d'un projet « ufw » : code %d attendu, obtenu %d :\n%s", CodeValidation, code, out)
	}
}

// Un logDir logé sous celui d'un voisin n'emporte rien de lui à la
// suppression : seul celui qui contient l'autre est refusé.
func TestLaSuppressionNeRefuseQueLeLogDirQuiContientCeluiDUnVoisin(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	h.mustRun("site", "create", "blog.fr", "--kind", "static", "--tls=false", "--yes")
	journaux := h.path("var", "log", "ogs-projects", "shopfr")
	témoin := filepath.Join(journaux, "a-garder.log")
	déposer(t, témoin, "journal de shopfr\n")
	h.modifierDeclaration(t, "blogfr", func(s string) string {
		return strings.ReplaceAll(s, h.path("var", "log", "ogs-projects", "blogfr"), filepath.Join(journaux, "blog"))
	})
	if out, code := h.run("project", "delete", "shopfr", "--yes"); code != CodeConflict ||
		!strings.Contains(out, "blogfr") {
		t.Errorf("shopfr contient les journaux de blogfr : refus attendu (code %d), obtenu %d :\n%s",
			CodeConflict, code, out)
	}
	intact(t, témoin, "journal de shopfr\n")
	if out, code := h.run("project", "delete", "blogfr", "--yes"); code != CodeOK {
		t.Errorf("blogfr, logé sous shopfr, n'efface rien du voisin : code %d :\n%s", code, out)
	}
	intact(t, témoin, "journal de shopfr\n")
}

// Un pool que la déclaration ne nomme plus part avec le projet (D115) : il est
// coupé avec les autres, avant l'archive, et non après elle.
func TestLaCoupureRetireAussiLePoolQueLaDeclarationNeNommePlus(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("apply", "--yes")
	pool := h.path("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf")
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) {
		p.Spec.App.Kind = model.KindStatic
		p.Spec.App.Version = ""
		p.Spec.PHP.Pools = nil
	})
	présent := true
	out, _ := h.runEpie(context.Background(), "archive créée", func() {
		_, err := os.Stat(pool)
		présent = err == nil
	}, "project", "delete", "shopfr", "--yes")
	if présent {
		t.Errorf("le pool non déclaré doit être coupé avant l'archive :\n%s", out)
	}
	if _, err := os.Stat(pool); !os.IsNotExist(err) {
		t.Errorf("le pool doit être parti : %v", err)
	}
}
