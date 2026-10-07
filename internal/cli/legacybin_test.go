package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ancienBin reconstitue l'ancien ~/.bin d'ogs (D70) : à root — ce que le mode
// préfixé ne contrôle pas —, en 0710, son lien php vers l'interpréteur.
func ancienBin(t *testing.T, h *harness, home string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".bin"), 0o710); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(h.path("usr", "bin", "php8.3"), filepath.Join(home, ".bin", "php")); err != nil {
		t.Fatal(err)
	}
}

func TestLaMigrationReecritLesCommandesQuiDesignentLAncienBin(t *testing.T) {
	// R3N-08 : l'ancien §7.5 invitait à passer par .bin/php. Une tâche et un
	// worker qui l'invoquent en chemin absolu doivent suivre le lien dans
	// ~/.local/bin — fichiers générés et déclaration —, avant que .bin parte.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4")
	home := h.path("home", "shop.fr")
	h.mustRun("cron", "add", "shopfr", "export", "--schedule", "0 3 * * *",
		"--command", home+"/.bin/php www/scripts/export.php")
	h.mustRun("service", "add", "shopfr", "indexer", "--template", "worker",
		"--command", home+"/.bin/php "+home+"/www/bin/indexer.php")

	// L'exploitant commente sa déclaration : la réécriture doit le garder.
	decl := h.path("etc", "ogs", "projects", "shopfr.yaml")
	data, err := os.ReadFile(decl)
	if err != nil {
		t.Fatal(err)
	}
	commented := "# Boutique — tenue par l'équipe web\n" + strings.Replace(string(data),
		"  crons:\n", "  crons:  # tâches de nuit\n", 1)
	if err := os.WriteFile(decl, []byte(commented), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(decl, 0o640); err != nil {
		t.Fatal(err)
	}
	ancienBin(t, h, home)

	plan := h.mustRun("plan", "shopfr")
	if !strings.Contains(plan, "spec.crons[export].command") || !strings.Contains(plan, "spec.services[indexer].params.command") {
		t.Errorf("le plan doit annoncer la réécriture de la déclaration :\n%s", plan)
	}
	if got, _ := os.ReadFile(decl); string(got) != commented {
		t.Fatal("le plan ne doit rien écrire")
	}

	h.mustRun("apply", "shopfr", "--yes")
	cron := h.read(t, "etc/cron.d/ogs-shopfr")
	if strings.Contains(cron, "/.bin/") || !strings.Contains(cron, home+"/.local/bin/php www/scripts/export.php") {
		t.Errorf("la tâche doit désigner ~/.local/bin/php :\n%s", cron)
	}
	unit := h.read(t, "etc/systemd/system/ogs-shopfr-indexer@.service")
	if strings.Contains(unit, "/.bin/") || !strings.Contains(unit, home+"/.local/bin/php "+home+"/www/bin/indexer.php") {
		t.Errorf("le worker doit désigner ~/.local/bin/php :\n%s", unit)
	}
	got, err := os.ReadFile(decl)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(commented, home+"/.bin/php", home+"/.local/bin/php")
	if string(got) != want {
		t.Errorf("seules les commandes doivent changer dans la déclaration :\n--- obtenu\n%s\n--- attendu\n%s", got, want)
	}
	if info, err := os.Stat(decl); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("la déclaration doit garder son mode : %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".bin")); !os.IsNotExist(err) {
		t.Error("l'ancien .bin doit être retiré une fois les commandes réécrites")
	}
	h.simulateSystemEffects("shopfr", "shop.fr", 1004)
	if out := h.mustRun("apply", "shopfr", "--yes"); !strings.Contains(out, "aucun changement") {
		t.Errorf("la migration faite, un apply rejoué doit être vide :\n%s", out)
	}
}

func TestUnBinDeLUtilisateurNEstNiRetireNiReecrit(t *testing.T) {
	// R3N-07 : un ~/.bin qui n'est pas l'ancien répertoire d'ogs — mode 0755,
	// liens vers les binaires de l'utilisateur — porte son travail.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4")
	home := h.path("home", "shop.fr")
	bin := filepath.Join(home, ".bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"php": "/opt/php-8.1-custom/bin/php", "node": "/usr/local/bin/node18", "python": "/srv/tools/python3.11",
	} {
		if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	h.mustRun("cron", "add", "shopfr", "export", "--schedule", "0 3 * * *",
		"--command", home+"/.bin/php www/scripts/export.php")
	decl := h.path("etc", "ogs", "projects", "shopfr.yaml")
	before, _ := os.ReadFile(decl)
	out := h.mustRun("apply", "shopfr", "--yes")
	h.simulateSystemEffects("shopfr", "shop.fr", 1004)
	out += h.mustRun("apply", "shopfr", "--yes")
	if strings.Contains(out, "retrait de l'ancien") || strings.Contains(out, "déclaration") {
		t.Errorf("un .bin de l'utilisateur ne doit être ni retiré ni réécrit :\n%s", out)
	}
	for _, name := range []string{"php", "node", "python"} {
		if _, err := os.Lstat(filepath.Join(bin, name)); err != nil {
			t.Errorf("le lien %s de l'utilisateur doit rester : %v", name, err)
		}
	}
	if !strings.Contains(h.read(t, "etc/cron.d/ogs-shopfr"), home+"/.bin/php") {
		t.Errorf("la tâche doit garder le lien de l'utilisateur :\n%s", h.read(t, "etc/cron.d/ogs-shopfr"))
	}
	if after, _ := os.ReadFile(decl); string(after) != string(before) {
		t.Error("la déclaration ne doit pas être réécrite")
	}
}

func TestUnBinDeclareParLExploitantNOscillePas(t *testing.T) {
	// R3N-07, variante : spec.layout.dirs déclare .bin sous une autre forme que
	// l'ancien défaut. L'arborescence le crée ; la migration ne doit pas le
	// retirer à l'apply suivant.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4")
	decl := h.path("etc", "ogs", "projects", "shopfr.yaml")
	data, _ := os.ReadFile(decl)
	declared := strings.Replace(string(data), "path: .ssh", "path: .bin\n        mode: \"0750\"\n      - path: .ssh", 1)
	if declared == string(data) {
		t.Fatalf("déclaration inattendue :\n%s", data)
	}
	if err := os.WriteFile(decl, []byte(declared), 0o600); err != nil {
		t.Fatal(err)
	}
	h.mustRun("apply", "shopfr", "--yes")
	h.simulateSystemEffects("shopfr", "shop.fr", 1004)
	for i := 0; i < 2; i++ {
		out := h.mustRun("apply", "shopfr", "--yes")
		if !strings.Contains(out, "aucun changement") {
			t.Fatalf("apply %d : le .bin déclaré ne doit être ni retiré ni recréé :\n%s", i+2, out)
		}
	}
	if info, err := os.Lstat(filepath.Join(h.path("home", "shop.fr"), ".bin")); err != nil || !info.IsDir() {
		t.Errorf("le .bin déclaré doit rester : %v", err)
	}
}

// tacheSurAncienBin prépare un projet dont la tâche désigne l'ancien .bin
// d'ogs, présent, et dont la déclaration n'est pas encore réécrite.
func tacheSurAncienBin(t *testing.T) (*harness, string, string) {
	t.Helper()
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4")
	home := h.path("home", "shop.fr")
	h.mustRun("cron", "add", "shopfr", "export", "--schedule", "0 3 * * *",
		"--command", home+"/.bin/php www/scripts/export.php")
	h.mustRun("apply", "shopfr", "--yes")
	h.simulateSystemEffects("shopfr", "shop.fr", 1004)
	ancienBin(t, h, home)
	h.modifierDeclaration(t, "shopfr", func(s string) string {
		return strings.ReplaceAll(s, "/.local/bin/php", "/.bin/php")
	})
	return h, home, h.path("etc", "ogs", "projects", "shopfr.yaml")
}

func TestLePlanCalculeDeuxFoisGardeLaReecriture(t *testing.T) {
	// B1 de la relecture : le plan modifiait le projet reçu ; un second calcul
	// (la TUI affiche puis applique) perdait l'étape de réécriture, et .bin
	// partait quand même.
	h, _, _ := tacheSurAncienBin(t)
	app := h.ouvrirApp(t, true)
	if err := app.openStore(); err != nil {
		t.Fatal(err)
	}
	app.Store.PreviewAllocations(true)
	provs, err := app.buildProviders()
	if err != nil {
		t.Fatal(err)
	}
	defer provs.close()
	p, err := app.loadProject("shopfr")
	if err != nil {
		t.Fatal(err)
	}
	var first string
	for i := 1; i <= 2; i++ {
		plan, err := app.buildPlan(context.Background(), provs, p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan.String(), "spec.crons[export].command") {
			t.Fatalf("calcul %d : la réécriture de la déclaration doit figurer au plan :\n%s", i, plan.String())
		}
		if i == 1 {
			first = plan.String()
		} else if plan.String() != first {
			t.Errorf("deux calculs doivent rendre le même plan :\n%s\n---\n%s", first, plan.String())
		}
	}
	if strings.Contains(p.Spec.Crons[0].Command, ".local/bin") {
		t.Error("le plan ne doit pas modifier le projet chargé")
	}
}

func TestLInterfaceAffichepuisAppliqueReecritLaDeclaration(t *testing.T) {
	h, home, decl := tacheSurAncienBin(t)
	b := &tuiBackend{app: h.ouvrirApp(t, true)}
	ps, _, err := b.Projects()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Snapshot(context.Background(), ps[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply(context.Background(), ps[0]); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(decl)
	if strings.Contains(string(got), "/.bin/php") {
		t.Errorf("déclaration non réécrite après l'affichage puis l'application :\n%s", got)
	}
	if _, err := os.Lstat(filepath.Join(home, ".bin")); !os.IsNotExist(err) {
		t.Error("l'ancien .bin doit être retiré une fois la déclaration réécrite")
	}
}

func TestUneReecritureRefuseeRetientLAncienBin(t *testing.T) {
	// Mineur 1 de la relecture : une valeur repliée n'est pas réécrite ; .bin
	// doit rester, « cron run » relisant la déclaration.
	h, home, _ := tacheSurAncienBin(t)
	h.modifierDeclaration(t, "shopfr", func(s string) string {
		return strings.Replace(s, "command: "+home+"/.bin/php www/scripts/export.php",
			"command: >-\n        "+home+"/.bin/php\n        www/scripts/export.php", 1)
	})
	out, code := h.run("apply", "shopfr", "--yes")
	if code != 6 {
		t.Fatalf("code 6 attendu, obtenu %d :\n%s", code, out)
	}
	if _, err := os.Lstat(filepath.Join(home, ".bin", "php")); err != nil {
		t.Fatalf("l'ancien lien doit rester tant que la déclaration le désigne :\n%s", out)
	}
	if !strings.Contains(out, "spec.crons[export].command") {
		t.Errorf("la valeur à reprendre doit être nommée :\n%s", out)
	}
}
