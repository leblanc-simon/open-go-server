package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/engine"
)

// Le chemin unique d'apply (D112). Chaque test reproduit un constat de la revue
// sur la boucle « projets puis plan du parc » qu'« ogs apply », site create,
// site clone, project restore et l'interface recopiaient chacun.

// poserPoolEtranger pose, à l'emplacement du pool d'un projet, un fichier qui ne
// porte pas la marque d'ogs : l'étape du pool, critique, échoue à l'application
// et la saga déroule ce qui précède.
func (h *harness) poserPoolEtranger(t *testing.T, version, id string) {
	t.Helper()
	dir := h.path("etc", "php", version, "fpm", "pool.d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".conf"), []byte("[pool-de-l-exploitant]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// poserArchiveExpiree dépose une archive plus vieille que la rétention : le plan
// du parc en porte la purge (D52).
func (h *harness) poserArchiveExpiree(t *testing.T) string {
	t.Helper()
	dir := h.path("var", "lib", "ogs", "archives")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ancien-20200101-0000.tar.zst")
	if err := os.WriteFile(path, []byte("archive illisible\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vieux := time.Now().AddDate(-3, 0, 0)
	if err := os.Chtimes(path, vieux, vieux); err != nil {
		t.Fatal(err)
	}
	return path
}

// R1-38 : un projet en échec privait tout le parc du plan global, y compris les
// projets appliqués avant lui — « ogs apply » rendait la main avant le plan du
// parc, et le projet appliqué n'entrait ni dans tmpfiles.d ni dans la rotation.
func TestUnProjetEnEchecNePrivePasLeParcDuPlanGlobal(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "aaa.fr", "--kind", "static")
	h.mustRun("project", "create", "zzz.fr", "--kind", "php-symfony", "--php", "8.5")
	h.poserPoolEtranger(t, "8.5", "zzzfr")

	out, code := h.run("apply", "--yes")
	if code != CodeError {
		t.Fatalf("le projet en échec doit rendre le code %d, obtenu %d :\n%s", CodeError, code, out)
	}
	tmpfiles, err := os.ReadFile(h.path("etc", "tmpfiles.d", "ogs.conf"))
	if err != nil {
		t.Fatalf("DÉFAUT : le plan du parc n'a pas été exécuté après l'échec de zzzfr : %v\n%s", err, out)
	}
	if !strings.Contains(string(tmpfiles), "/run/ogs/aaafr") {
		t.Errorf("DÉFAUT : aaafr, appliqué, manque aux répertoires d'exécution :\n%s", tmpfiles)
	}
	// Le projet en échec y figure par la déclaration du parc : elle reste, c'est
	// celle de l'exploitant (project create).
	if !strings.Contains(out, "✓ aaafr") {
		t.Errorf("aaafr devait être appliqué :\n%s", out)
	}
}

// Constat neuf rattaché à R1-38 : une création refusée et défaite laissait sa
// déclaration ; le plan du parc l'énumérait ensuite — répertoire d'exécution
// d'un compte inexistant — et l'apply suivant d'un voisin rendait 6
// (« Failed to resolve user »), confirmé sur un vrai système.
func TestUneCreationDefaiteRetireSaDeclaration(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "aaa.fr", "--kind", "static")
	h.mustRun("apply", "--yes")
	h.poserPoolEtranger(t, "8.5", "zzzfr")

	out, code := h.run("site", "create", "zzz.fr", "--kind", "php-symfony", "--php", "8.5",
		"--tls=false", "--yes")
	if code != CodeError {
		t.Fatalf("la création devait échouer (code %d), obtenu %d :\n%s", CodeError, code, out)
	}
	if !strings.Contains(out, "rollback effectué") {
		t.Fatalf("la saga devait dérouler :\n%s", out)
	}
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "zzzfr.yaml")); !os.IsNotExist(err) {
		t.Errorf("DÉFAUT : la déclaration d'une création défaite reste au parc (%v) :\n%s", err, out)
	}
	tmpfiles := h.read(t, filepath.Join("etc", "tmpfiles.d", "ogs.conf"))
	if strings.Contains(tmpfiles, "zzzfr") {
		t.Errorf("DÉFAUT : la création défaite figure aux répertoires d'exécution :\n%s", tmpfiles)
	}
	// Elle se relance telle quelle une fois la cause levée.
	if err := os.Remove(h.path("etc", "php", "8.5", "fpm", "pool.d", "zzzfr.conf")); err != nil {
		t.Fatal(err)
	}
	h.mustRunPartial("site", "create", "zzz.fr", "--kind", "php-symfony", "--php", "8.5", "--tls=false", "--yes")
}

// R2-12 : le plan du parc s'exécutait sans être affiché ni confirmé, et
// --dry-run l'omettait : une purge d'archives (D52), l'activation d'ufw, un
// retrait d'accès passaient hors de toute confirmation.
func TestLePlanDuParcEstAfficheEtConfirme(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "--yes")
	archive := h.poserArchiveExpiree(t)

	// --dry-run montre la purge, et ne la fait pas.
	out, code := h.run("apply", "shopfr", "--dry-run")
	if code != CodeOK {
		t.Fatalf("--dry-run : code %d\n%s", code, out)
	}
	if !strings.Contains(out, "purge de 1 archive(s)") {
		t.Errorf("DÉFAUT : --dry-run doit afficher le plan du parc :\n%s", out)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("--dry-run a purgé l'archive : %v", err)
	}

	// Sans confirmation, rien du parc ne s'applique.
	out, code = h.run("apply", "shopfr", "--non-interactive")
	if code != CodeCancelled {
		t.Errorf("DÉFAUT : le plan du parc doit être confirmé (code %d attendu), obtenu %d :\n%s",
			CodeCancelled, code, out)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("DÉFAUT : l'archive a été purgée sans confirmation :\n%s", out)
	}
	if !strings.Contains(out, "parc\n") || !strings.Contains(out, "purge de 1 archive(s)") {
		t.Errorf("le plan du parc doit être affiché avant la question :\n%s", out)
	}

	// Une seule confirmation, projet et parc ensemble.
	out = h.mustRun("apply", "shopfr", "--yes")
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Errorf("l'archive expirée devait être purgée une fois confirmée :\n%s", out)
	}
}

// R2-12 aussi : « project delete » recalculait et exécutait le plan du parc sans
// l'avoir montré ; il l'est avant la confirmation de la suppression, qui le
// couvre.
func TestLaSuppressionMontreLePlanDuParcAvantDeConfirmer(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "--yes")
	archive := h.poserArchiveExpiree(t)

	out, code := h.run("project", "delete", "shopfr", "--non-interactive")
	if code != CodeCancelled {
		t.Fatalf("la suppression devait attendre sa confirmation, code %d :\n%s", code, out)
	}
	if !strings.Contains(out, "parc, sans shopfr") || !strings.Contains(out, "purge de 1 archive(s)") {
		t.Errorf("DÉFAUT : le plan du parc doit être montré avant la confirmation :\n%s", out)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("rien ne devait être purgé : %v", err)
	}
}

// R3-24 : preApply ne gardait pas le plan du parc — sans changement de projet,
// rien n'était soumis au hook, et la purge des archives passait.
func TestPreApplyGardeLePlanDuParc(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "--yes")
	h.simulateSystemEffects("shopfr", "shop.fr", 1004)
	h.mustRun("apply", "--yes")
	h.déposerHooks(t)
	h.poserArchiveExpiree(t)

	out := h.mustRun("apply", "shopfr", "--yes")
	if !strings.Contains(out, "aucun changement") {
		t.Fatalf("le projet devait être conforme :\n%s", out)
	}
	pre := strings.Index(out, "↪ hook preApplyFleet :")
	purge := strings.Index(out, "✓ purge de 1 archive(s)")
	if pre < 0 || purge < 0 || pre > purge {
		t.Errorf("DÉFAUT : le hook preApplyFleet doit précéder le plan du parc :\n%s", out)
	}
	if !strings.Contains(out, "↪ hook postApplyFleet :") {
		t.Errorf("le hook postApplyFleet doit suivre le plan du parc :\n%s", out)
	}
	// preApply et postApply restent ceux d'un projet : rien à changer au projet,
	// ils ne sont pas appelés — jamais sans projet.
	if strings.Contains(out, "↪ hook preApply :") || strings.Contains(out, "↪ hook postApply :") {
		t.Errorf("DÉFAUT : preApply/postApply appelés sans projet :\n%s", out)
	}
}

// R3-23 : postCreate manquait les créations du cycle documenté — « project
// create » puis « ogs apply » —, tournait avant le contenu pour un clone et une
// restauration, et la restauration sautait preCreate.
func TestPostCreateAuPremierApplyReussi(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.déposerHooks(t)
	h.mustRun("project", "create", "shop.fr", "--kind", "static")

	out := h.mustRun("apply", "shopfr", "--yes")
	if !strings.Contains(out, "↪ hook postCreate") {
		t.Errorf("DÉFAUT : postCreate doit suivre le premier apply réussi d'un projet déclaré :\n%s", out)
	}
	// Une seule fois.
	h.simulateSystemEffects("shopfr", "shop.fr", 1004)
	if err := os.Chmod(h.path("home", "shop.fr", "var"), 0o777); err != nil {
		t.Fatal(err)
	}
	out = h.mustRun("apply", "shopfr", "--yes")
	if strings.Contains(out, "↪ hook postCreate") {
		t.Errorf("postCreate ne vaut que pour le premier apply :\n%s", out)
	}
}

func TestPostCreateDUnCloneSuitLeContenu(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "--yes")
	h.déposerHooks(t)
	out, code := h.run("site", "clone", "shopfr", "--to", "prep", "--yes")
	if code != CodeOK && code != CodePartial {
		t.Fatalf("clone en échec (code %d) :\n%s", code, out)
	}
	post := strings.LastIndex(out, "↪ hook postCreate")
	contenu := strings.Index(out, "contenu copié")
	if post < 0 || contenu < 0 || post < contenu {
		t.Errorf("DÉFAUT : postCreate doit suivre la copie du contenu :\n%s", out)
	}
	if strings.Count(out, "↪ hook postCreate") != 1 {
		t.Errorf("postCreate une seule fois :\n%s", out)
	}
}

// R1-42 : « apply -f » d'un fichier hors inventaire mettait le projet en service,
// puis l'apply d'un voisin l'effaçait des fichiers de parc.
func TestApplyFichierHorsInventaireRefuse(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	decl := h.path("etc", "ogs", "projects", "shopfr.yaml")
	ailleurs := filepath.Join(t.TempDir(), "shopfr.yaml")
	data, err := os.ReadFile(decl)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ailleurs, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(decl); err != nil {
		t.Fatal(err)
	}
	out, code := h.run("apply", "-f", ailleurs, "--yes")
	if code != CodeValidation {
		t.Errorf("DÉFAUT : apply -f hors inventaire doit être refusé (code %d), obtenu %d :\n%s",
			CodeValidation, code, out)
	}
	if !strings.Contains(out, "ogs project create") {
		t.Errorf("le refus doit nommer son remède :\n%s", out)
	}
	if _, err := os.Stat(h.path("home", "shop.fr")); !os.IsNotExist(err) {
		t.Errorf("rien ne devait être appliqué")
	}
	// Dans l'inventaire, -f reste admis.
	if err := os.WriteFile(decl, data, 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustRun("apply", "-f", ailleurs, "--yes")
}

// R1-39 : l'interface appliquait sans plan du parc, sans hook, et sans afficher
// un rechargement en échec ; elle déclarait sans preCreate ni contrôle du home.
// Elle passe par le chemin d'« ogs apply ».
func TestLaTUIPasseParLeCheminDApply(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.déposerHooks(t)
	app := h.ouvrirApp(t, true)
	app.Quiet = false
	b := &tuiBackend{app: app}
	p, err := app.loadProject("shopfr")
	if err != nil {
		t.Fatal(err)
	}
	lines, err := b.Plan(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "parc") {
		t.Errorf("DÉFAUT : le plan montré par l'interface doit porter celui du parc :\n%s",
			strings.Join(lines, "\n"))
	}
	// « d » montre aussi le plan du parc (relecture, mineur 4).
	diff, err := b.Diff(context.Background(), p)
	if err != nil || !strings.Contains(strings.Join(diff, "\n"), "parc") {
		t.Errorf("DÉFAUT : le diff de l'interface doit porter le parc (%v) :\n%s", err, strings.Join(diff, "\n"))
	}
	out, err := b.Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("apply de l'interface : %v\n%s", err, out)
	}
	for _, attendu := range []string{"↪ hook preApply :", "↪ hook postApply :", "↪ hook preApplyFleet :",
		"↪ hook postApplyFleet :", "↪ hook postCreate :"} {
		if !strings.Contains(out, attendu) {
			t.Errorf("DÉFAUT : l'interface doit lancer %q :\n%s", attendu, out)
		}
	}
	tmpfiles, err := os.ReadFile(h.path("etc", "tmpfiles.d", "ogs.conf"))
	if err != nil || !strings.Contains(string(tmpfiles), "/run/ogs/shopfr") {
		t.Errorf("DÉFAUT : l'apply de l'interface doit exécuter le plan du parc (%v) :\n%s", err, out)
	}
}

// R3-23 : la restauration sautait preCreate et lançait postCreate avant de
// rendre l'arborescence et les bases.
func TestLaRestaurationEncadreeParPreCreateEtPostCreate(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "shopfr", "--yes")
	h.mustRun("project", "delete", "shopfr", "--yes")
	h.déposerHooks(t)
	out := h.mustRunPartial("project", "restore", h.derniereArchive(t), "--yes")
	pre := strings.Index(out, "↪ hook preCreate")
	decl := strings.Index(out, "déclaration restaurée")
	if pre < 0 || decl < 0 || pre > decl {
		t.Errorf("DÉFAUT : preCreate doit précéder le dépôt de la déclaration :\n%s", out)
	}
	post := strings.LastIndex(out, "↪ hook postCreate")
	parc := strings.LastIndex(out, "↪ hook postApply")
	if post < 0 || post < parc {
		t.Errorf("DÉFAUT : postCreate doit suivre l'apply et la remise du contenu :\n%s", out)
	}
	if strings.Count(out, "↪ hook postCreate") != 1 {
		t.Errorf("postCreate une seule fois :\n%s", out)
	}
}

// Arbitrage de D112 : le timer de renouvellement n'exécute aucun plan du parc,
// et rien de la confirmation neuve ne l'atteint — « cert renew » reste non
// interactif, sans --yes.
func TestCertRenewNEstPasBloqueParLaConfirmationDuParc(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "--yes")
	archive := h.poserArchiveExpiree(t)
	out, code := h.run("cert", "renew", "--all", "--non-interactive")
	if code == CodeCancelled {
		t.Errorf("« cert renew » ne doit jamais attendre une confirmation :\n%s", out)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Errorf("« cert renew » n'exécute pas le plan du parc : %v", err)
	}
}

// Relecture de D112 (B1) : preApply et postApply étaient appelés aussi sans
// projet pour le plan du parc ; un hook « rm -rf "$OGS_ROOT"/var/cache/* »
// vidait alors /var/cache de la machine. Ils ne valent que pour un projet, une
// fois chacun ; le parc a ses événements à lui.
func TestPreApplyEtPostApplyNeSontJamaisAppelesSansProjet(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.déposerHooks(t)
	out := h.mustRunPartial("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	for _, ev := range []string{"preApply", "postApply", "preApplyFleet", "postApplyFleet", "postCreate"} {
		if n := strings.Count(out, "↪ hook "+ev+" :"); n != 1 {
			t.Errorf("DÉFAUT : %s appelé %d fois, attendu une (projet ou parc) :\n%s", ev, n, out)
		}
	}
}

// Relecture de D112 (M1) : une création refusée avant le premier geste — ici à
// la confirmation — laissait sa déclaration, et l'apply d'un voisin rendait 6.
func TestUneCreationRefuseeALaConfirmationRetireSaDeclaration(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	out, code := h.run("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--non-interactive")
	if code != CodeCancelled {
		t.Fatalf("la confirmation devait manquer (code %d), obtenu %d :\n%s", CodeCancelled, code, out)
	}
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "shopfr.yaml")); !os.IsNotExist(err) {
		t.Errorf("DÉFAUT : la déclaration d'une création refusée reste au parc (%v) :\n%s", err, out)
	}
}

// D112 sous D113 : le premier apply d'une restauration neuve est une création.
// Défait entier, il retire la déclaration et le témoin de reprise — sans quoi
// la déclaration restait au parc et le message renvoyait à une reprise —, et
// la restauration se relance telle quelle.
func TestUneRestaurationDefaiteRetireDeclarationEtTemoin(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5")
	h.mustRunPartial("apply", "shopfr", "--yes")
	h.mustRunPartial("project", "delete", "shopfr", "--yes")
	archive := h.derniereArchive(t)
	h.poserPoolEtranger(t, "8.5", "shopfr")

	out, code := h.run("project", "restore", archive, "--yes")
	if code != CodeError {
		t.Fatalf("la restauration devait échouer (code %d), obtenu %d :\n%s", CodeError, code, out)
	}
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "shopfr.yaml")); !os.IsNotExist(err) {
		t.Errorf("DÉFAUT : la déclaration d'une restauration défaite reste au parc :\n%s", out)
	}
	if strings.Contains(out, "--resume") {
		t.Errorf("une restauration défaite ne se reprend pas, elle se relance :\n%s", out)
	}
	if err := os.Remove(h.path("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf")); err != nil {
		t.Fatal(err)
	}
	h.mustRunPartial("project", "restore", archive, "--yes")
}

// D117 sous D112 : « postCertRenew » suit le plan du projet dans applyTarget,
// donc tout chemin d'apply — l'interface comprise, qui ne le lançait pas — et
// une seule fois par projet pour un même certificat.
func TestPostCertRenewSuitToutApplyUneFoisParProjet(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.poserHooksDeCertificat(t)
	app := h.ouvrirApp(t, true)
	app.Quiet = false
	b := &tuiBackend{app: app}
	p, err := app.loadProject("shopfr")
	if err != nil {
		t.Fatal(err)
	}
	out, err := b.Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("apply de l'interface : %v\n%s", err, out)
	}
	parc := "↪ hook postCertRenew : " + h.path("etc", "ogs", "hooks", "post-cert-renew")
	projet := "↪ hook postCertRenew : " + h.path("etc", "ogs", "hooks", "shop-cert.sh")
	if strings.Count(out, parc) != 1 || strings.Count(out, projet) != 1 {
		t.Errorf("DÉFAUT : postCertRenew attendu une fois au parc et une fois au projet, sous l'interface :\n%s", out)
	}
	avantDans(t, out, "↻ rechargement", projet)
}

// R1-54 sous D112 : une restauration dont le premier apply n'a pas été
// entièrement déroulé — une compensation en échec — garde déclaration et
// témoin : ce qu'elle a laissé est nommé, la reprise et l'abandon aussi.
func TestUneRestaurationMalDerouleeResteReprenable(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5")
	h.mustRunPartial("apply", "shopfr", "--yes")
	h.mustRunPartial("project", "delete", "shopfr", "--yes")
	archive := h.derniereArchive(t)
	h.poserPoolEtranger(t, "8.5", "shopfr")

	précédent := executeProjectPlan
	t.Cleanup(func() { executeProjectPlan = précédent })
	executeProjectPlan = func(ctx context.Context, plan *engine.Plan, o engine.Options) (*engine.Report, error) {
		report, err := précédent(ctx, plan, o)
		if err != nil {
			report.RollbackErr = errors.New("compensation refusée")
		}
		return report, err
	}
	out, code := h.run("project", "restore", archive, "--yes")
	if code == CodeOK || code == CodePartial {
		t.Fatalf("la restauration devait échouer (code %d) :\n%s", code, out)
	}
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "shopfr.yaml")); err != nil {
		t.Errorf("DÉFAUT : un déroulage incomplet doit laisser la déclaration : %v\n%s", err, out)
	}
	if _, err := os.Stat(h.path("var", "lib", "ogs", "restores", "shopfr.json")); err != nil {
		t.Errorf("DÉFAUT : un déroulage incomplet doit laisser le témoin : %v", err)
	}
	if !strings.Contains(out, "--resume") || !strings.Contains(out, "ogs project delete shopfr") {
		t.Errorf("DÉFAUT : la sortie doit nommer la reprise et l'abandon :\n%s", out)
	}
}

// Ajustement d'intégration de D112 : « postCertRenew » partait d'applyTarget
// même quand le serveur web n'avait pas relu le certificat émis, là où
// « cert renew » et « cert issue » le taisent en le disant — le hook annonçait
// un certificat que le serveur ne présentait pas.
func TestApplyTaitPostCertRenewSiLeServeurWebNARienRelu(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.poserHooksDeCertificat(t)
	app := h.ouvrirApp(t, true)
	var out bytes.Buffer
	app.Quiet, app.Out, app.Err = false, &out, &out
	provs, err := app.buildProviders()
	if err != nil {
		t.Fatal(err)
	}
	defer provs.close()
	p, err := app.loadProject("shopfr")
	if err != nil {
		t.Fatal(err)
	}
	app.Store.PreviewAllocations(true)
	plan, err := app.buildPlan(context.Background(), provs, p)
	if err != nil {
		t.Fatal(err)
	}
	app.Store.PreviewAllocations(false)
	target := &fleetTarget{p: p, shown: keysOf(plan)}
	normal := app.reloadFunc(provs)
	webTente := false
	reload := func(ctx context.Context, cible string) error {
		if strings.HasPrefix(cible, "web:") {
			webTente = true
			return errors.New("configuration refusée")
		}
		return normal(ctx, cible)
	}
	warnings, err := app.applyTarget(context.Background(), provs, target, applyRun{audit: "apply."}, reload)
	if err != nil {
		t.Fatalf("apply : %v\n%s", err, out.String())
	}
	if !webTente {
		t.Fatalf("le plan devait recharger le serveur web :\n%s", out.String())
	}
	if strings.Contains(out.String(), "↪ hook postCertRenew") {
		t.Errorf("DÉFAUT : postCertRenew lancé alors que le serveur web n'a pas relu le certificat :\n%s", out.String())
	}
	if !strings.Contains(out.String(), "hook postCertRenew non lancé") {
		t.Errorf("le silence du hook doit être dit :\n%s", out.String())
	}
	// Le rechargement en échec et le hook tu comptent chacun.
	if warnings < 2 {
		t.Errorf("avertissements : %d, au moins 2 attendus :\n%s", warnings, out.String())
	}
}
