package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/tui"
)

// Une déclaration groupée, écrite à la main, se planifie sous le répertoire de
// son groupe, bases préfixées (D135).
func TestLePlanDUnProjetGroupeLeRangeSousSonGroupe(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	decl := filepath.Join(t.TempDir(), "blogfr.yaml")
	contenu := `apiVersion: ogs/v1
kind: Project
metadata:
  host: blog.fr
spec:
  group: acme
  app:
    kind: static
  databases:
    - engine: mysql
      name: acme_blogfr
      user: blogfr
`
	if err := os.WriteFile(decl, []byte(contenu), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("plan", "-f", decl)
	if !strings.Contains(out, filepath.Join("home", "acme", "blog.fr")) {
		t.Errorf("le plan doit poser le home sous le répertoire du groupe :\n%s", out)
	}
	if !strings.Contains(out, "acme_blogfr") {
		t.Errorf("le plan doit nommer la base préfixée :\n%s", out)
	}
}

// Un groupe ajouté après la création ne déplace pas le home : le plan du projet
// le refuse en code 3, sans arrêter celui d'un voisin (D135, D120).
func TestUnGroupeAjouteApresCoupNArretePasLesVoisins(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr")
	h.mustRun("project", "create", "blog.fr")
	rel := filepath.Join("etc", "ogs", "projects", "blogfr.yaml")
	avant := h.read(t, rel)
	apres := strings.Replace(avant, "\nspec:\n", "\nspec:\n  group: acme\n", 1)
	if apres == avant {
		t.Fatalf("aucune section spec dans %s :\n%s", rel, avant)
	}
	if err := os.WriteFile(h.path(rel), []byte(apres), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := h.run("plan", "blogfr")
	if code != CodeValidation || !strings.Contains(out, "spec.group") {
		t.Errorf("« plan blogfr » : code %d, attendu %d, avec spec.group :\n%s", code, CodeValidation, out)
	}
	h.mustRunPartial("plan", "shopfr")
}

// En mode préfixé, l'apply de deux membres d'un groupe crée un seul répertoire
// de groupe, en 0701, et les deux homes dessous (D135).
func TestLApplyDeDeuxMembresCreeLeRepertoireDuGroupe(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	for _, host := range []string{"shop.fr", "blog.fr"} {
		id := strings.ReplaceAll(host, ".", "")
		decl := h.path(filepath.Join("etc", "ogs", "projects", id+".yaml"))
		contenu := "apiVersion: ogs/v1\nkind: Project\nmetadata:\n  host: " + host +
			"\nspec:\n  group: acme\n  app:\n    kind: static\n"
		if err := os.WriteFile(decl, []byte(contenu), 0o644); err != nil {
			t.Fatal(err)
		}
		h.mustRunPartial("apply", id, "--yes")
	}
	info, err := os.Lstat(h.path(filepath.Join("home", "acme")))
	if err != nil || info.Mode().Perm() != 0o701 {
		t.Fatalf("répertoire du groupe : %v, %v", info, err)
	}
	for _, host := range []string{"shop.fr", "blog.fr"} {
		if _, err := os.Lstat(h.path(filepath.Join("home", "acme", host))); err != nil {
			t.Errorf("home de %s : %v", host, err)
		}
	}
}

// déclarerGroupé dépose la déclaration d'un membre du groupe acme et l'applique.
func (h *harness) déclarerGroupé(t *testing.T, host string) string {
	t.Helper()
	id := strings.ReplaceAll(host, ".", "")
	contenu := "apiVersion: ogs/v1\nkind: Project\nmetadata:\n  host: " + host +
		"\nspec:\n  group: acme\n  app:\n    kind: static\n"
	if err := os.WriteFile(h.path("etc", "ogs", "projects", id+".yaml"), []byte(contenu), 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustRunPartial("apply", id, "--yes")
	return id
}

func existe(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// Le répertoire du groupe part avec son dernier membre, jamais avant (D135).
func TestLeRepertoireDuGroupePartAvecSonDernierMembre(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.déclarerGroupé(t, "shop.fr")
	h.déclarerGroupé(t, "blog.fr")
	groupe := h.path("home", "acme")

	h.mustRunPartial("project", "delete", "shopfr", "--yes")
	if existe(t, filepath.Join(groupe, "shop.fr")) || !existe(t, filepath.Join(groupe, "blog.fr")) {
		t.Fatal("seul le home de shopfr doit partir")
	}
	out := h.mustRunPartial("project", "delete", "blogfr", "--yes")
	if existe(t, groupe) {
		t.Errorf("le répertoire du groupe doit partir avec son dernier membre :\n%s", out)
	}
}

// --keep-data laisse le home, donc le répertoire du groupe.
func TestKeepDataLaisseLeRepertoireDuGroupe(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.déclarerGroupé(t, "shop.fr")
	h.mustRunPartial("project", "delete", "shopfr", "--keep-data", "--yes")
	if !existe(t, h.path("home", "acme", "shop.fr")) {
		t.Error("--keep-data doit laisser le home et son répertoire de groupe")
	}
}

// Une restauration recrée le répertoire du groupe, en 0701, d'après la
// déclaration archivée.
func TestLaRestaurationRecreeLeRepertoireDuGroupe(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.déclarerGroupé(t, "shop.fr")
	h.mustRunPartial("project", "delete", "shopfr", "--yes")
	if existe(t, h.path("home", "acme")) {
		t.Fatal("le répertoire du groupe doit être parti")
	}
	h.mustRunPartial("project", "restore", h.derniereArchive(t), "--yes")
	info, err := os.Lstat(h.path("home", "acme"))
	if err != nil || info.Mode().Perm() != 0o701 {
		t.Fatalf("répertoire du groupe après restauration : %v, %v", info, err)
	}
	if !existe(t, h.path("home", "acme", "shop.fr")) {
		t.Error("home absent après restauration")
	}
	if decl := h.read(t, filepath.Join("etc", "ogs", "projects", "shopfr.yaml")); !strings.Contains(decl, "group: acme") {
		t.Errorf("la déclaration restaurée doit garder son groupe :\n%s", decl)
	}
}

// Un projet dont la racine est le répertoire d'un groupe emporterait tous ses
// membres : sa suppression est refusée.
func TestLaSuppressionRefuseUneRacineEgaleAuRepertoireDUnGroupe(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.déclarerGroupé(t, "shop.fr")
	h.mustRun("project", "create", "blog.fr")
	rel := filepath.Join("etc", "ogs", "projects", "blogfr.yaml")
	decl := h.read(t, rel)
	avant := h.path("home", "blog.fr")
	if !strings.Contains(decl, "root: "+avant) {
		t.Fatalf("racine introuvable dans la déclaration :\n%s", decl)
	}
	decl = strings.ReplaceAll(decl, avant, h.path("home", "acme"))
	if err := os.WriteFile(h.path(rel), []byte(decl), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := h.run("project", "delete", "blogfr", "--yes")
	if code != CodeConflict || !strings.Contains(out, "shopfr") {
		t.Errorf("la suppression doit être refusée en nommant le membre (code %d), obtenu %d :\n%s",
			CodeConflict, code, out)
	}
	if !existe(t, h.path("home", "acme", "shop.fr")) {
		t.Error("le home d'un membre du groupe a été effacé")
	}
}

// --group sur « project create » et « site create » pose le groupe, le home sous
// son répertoire et la base préfixée (D135).
func TestCreateAvecGroupe(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--group", "acme", "--db", "mysql")
	decl := h.read(t, filepath.Join("etc", "ogs", "projects", "shopfr.yaml"))
	for _, attendu := range []string{"group: acme", "root: " + h.path("home", "acme", "shop.fr"),
		"name: acme_shopfr", "user: shopfr"} {
		if !strings.Contains(decl, attendu) {
			t.Errorf("%q absent de la déclaration :\n%s", attendu, decl)
		}
	}
	h.mustRunPartial("site", "create", "blog.fr", "--kind", "static", "--group", "acme", "--tls=false", "--yes")
	if !existe(t, h.path("home", "acme", "blog.fr")) {
		t.Error("site create --group doit poser le home sous le répertoire du groupe")
	}
	out := h.mustRun("project", "list")
	if !strings.Contains(out, "GROUPE") || !strings.Contains(out, "acme") {
		t.Errorf("project list doit montrer le groupe :\n%s", out)
	}
	out = h.mustRun("project", "list", "-o", "json")
	if !strings.Contains(out, `"group": "acme"`) {
		t.Errorf("la sortie JSON doit porter le groupe :\n%s", out)
	}
}

func TestUnGroupeInvalideEstUneErreurDUsage(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	for _, args := range [][]string{
		{"project", "create", "shop.fr", "--group", "ac_me"},
		{"site", "create", "shop.fr", "--group", "..", "--yes"},
	} {
		if out, code := h.run(args...); code != CodeUsage {
			t.Errorf("« ogs %s » : code %d, attendu %d :\n%s", strings.Join(args, " "), code, CodeUsage, out)
		}
	}
	if existe(t, h.path("etc", "ogs", "projects", "shopfr.yaml")) {
		t.Error("aucune déclaration ne doit être écrite")
	}
}

// L'assistant de la TUI produit la même déclaration que « site create --group ».
func TestLAssistantRangeLeSiteDansSonGroupe(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--group", "acme", "--apply=false", "--yes")
	cli := h.read(t, filepath.Join("etc", "ogs", "projects", "shopfr.yaml"))
	if err := os.Remove(h.path("etc", "ogs", "projects", "shopfr.yaml")); err != nil {
		t.Fatal(err)
	}
	b := &tuiBackend{app: h.ouvrirApp(t, true)}
	if out, err := b.Create(context.Background(), tui.WizardAnswers{Host: "shop.fr", Kind: "static",
		Env: "prod", Group: "acme"}); err != nil {
		t.Fatalf("Create : %v\n%s", err, out)
	}
	if got := h.read(t, filepath.Join("etc", "ogs", "projects", "shopfr.yaml")); got != cli {
		t.Errorf("déclarations différentes :\n--- CLI\n%s\n--- TUI\n%s", cli, got)
	}
}

// La préproduction d'un projet groupé rejoint le groupe de la source, base
// préfixée comprise (D135).
func TestLeCloneHeriteDuGroupe(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static", "--group", "acme", "--db", "mysql")
	h.mustRunPartial("apply", "--yes")
	out, code := h.run("site", "clone", "shopfr", "--to", "prep", "--yes")
	if code != CodeOK && code != CodePartial {
		t.Fatalf("clone en échec (code %d) :\n%s", code, out)
	}
	decl := h.read(t, filepath.Join("etc", "ogs", "projects", "shopfrprep.yaml"))
	for _, attendu := range []string{"group: acme", "root: " + h.path("home", "acme", "shop."+config.Default().Environments[string(naming.EnvPrep)].Domain),
		"name: acme_shopfrprep"} {
		if !strings.Contains(decl, attendu) {
			t.Errorf("%q absent de la déclaration du clone :\n%s", attendu, decl)
		}
	}
}

// --group propose les groupes du parc, une fois chacun.
func TestLaCompletionDeGroupProposeLesGroupesExistants(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--group", "acme")
	h.mustRun("project", "create", "blog.fr", "--group", "acme")
	h.mustRun("project", "create", "news.fr", "--group", "zeta")
	h.mustRun("project", "create", "solo.fr")
	out, code := h.run("__complete", "site", "create", "x.fr", "--group", "")
	if code != CodeOK {
		t.Fatalf("__complete a échoué (code %d) :\n%s", code, out)
	}
	if strings.Count(out, "acme\n") != 1 || !strings.Contains(out, "zeta\n") {
		t.Errorf("groupes proposés :\n%s", out)
	}
}

// ligneDoctor rend les lignes de doctor qui portent name.
func ligneDoctor(out, name string) string {
	var lignes []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, name) {
			lignes = append(lignes, l)
		}
	}
	return strings.Join(lignes, "\n")
}

// doctor juge le répertoire du groupe comme l'apply : régime, traversée,
// contenu (D135).
func TestDoctorJugeLeRepertoireDuGroupe(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.déclarerGroupé(t, "shop.fr")
	groupe := h.path("home", "acme")
	out, _ := h.run("doctor")
	if l := ligneDoctor(out, "répertoire du groupe acme"); !strings.Contains(l, "1 membre(s)") {
		t.Fatalf("répertoire conforme :\n%s", out)
	}

	if err := os.Chmod(groupe, 0o755); err != nil {
		t.Fatal(err)
	}
	out, _ = h.run("doctor")
	if l := ligneDoctor(out, "répertoire du groupe acme"); !strings.Contains(l, "mode 0755") {
		t.Errorf("mode retouché :\n%s", out)
	}

	if err := os.Chmod(groupe, 0o700); err != nil {
		t.Fatal(err)
	}
	out, _ = h.run("doctor")
	if l := ligneDoctor(out, "répertoire du groupe acme"); !strings.Contains(l, "n'est pas traversable") {
		t.Errorf("répertoire fermé :\n%s", out)
	}
	if err := os.Chmod(groupe, 0o701); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(filepath.Join(groupe, "vieux.fr"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, _ = h.run("doctor")
	if l := ligneDoctor(out, "répertoire du groupe acme"); !strings.Contains(l, "vieux.fr") {
		t.Errorf("entrée étrangère :\n%s", out)
	}
}

// Un répertoire au régime d'un groupe que plus aucun projet ne déclare est nommé.
func TestDoctorNommeUnRepertoireDeGroupeOrphelin(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "solo.fr")
	h.mustRunPartial("apply", "--yes")
	zeta := h.path("home", "zeta")
	if err := os.Mkdir(zeta, 0o701); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(zeta, 0o701); err != nil {
		t.Fatal(err)
	}
	out, _ := h.run("doctor")
	if l := ligneDoctor(out, "répertoire du groupe zeta"); !strings.Contains(l, "n'est le groupe d'aucun projet") {
		t.Errorf("répertoire orphelin :\n%s", out)
	}
	if strings.Contains(out, "solo.fr, au régime") {
		t.Errorf("un home ne doit pas passer pour un répertoire de groupe :\n%s", out)
	}
}

// doctor dit le groupe ajouté après coup, que le plan du projet refuse.
func TestDoctorNommeUnGroupeAjouteApresCoup(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "blog.fr")
	rel := filepath.Join("etc", "ogs", "projects", "blogfr.yaml")
	decl := strings.Replace(h.read(t, rel), "\nspec:\n", "\nspec:\n  group: acme\n", 1)
	if err := os.WriteFile(h.path(rel), []byte(decl), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := h.run("doctor")
	if l := ligneDoctor(out, "groupe acme"); !strings.Contains(l, "blogfr") {
		t.Errorf("groupe ajouté après coup :\n%s", out)
	}
}

// Un site repris dont le home serait le répertoire d'un groupe emporterait ses
// membres à la suppression, et l'apply le donnerait à son compte : l'adoption est
// refusée comme un home imbriqué (D94, D135).
func TestLAdoptionRefuseUnHomeEgalAuRepertoireDUnGroupe(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.déclarerGroupé(t, "shop.fr")
	h.legacyServer(t)
	vhost := h.path("etc", "apache2", "sites-available", "boutique.conf")
	data, err := os.ReadFile(vhost)
	if err != nil {
		t.Fatal(err)
	}
	// Sous préfixe, une racine adoptée garde le chemin qu'elle lit : le serveur
	// simulé désigne donc le répertoire du groupe par son chemin préfixé, comme un
	// vrai serveur le ferait par /home/acme.
	groupe := h.path("home", "acme")
	if err := os.WriteFile(vhost, []byte(strings.ReplaceAll(string(data), "/var/www/boutique", groupe)),
		0o644); err != nil {
		t.Fatal(err)
	}
	passwd := "boutique:x:1001:1001::" + groupe + ":/bin/bash\n"
	if err := os.WriteFile(h.path("etc", "passwd"), []byte(passwd), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.path("home", "acme", "public"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := h.run("import", "adopt", "boutiquefr", "--yes")
	if code != CodeConflict || !strings.Contains(out, "shopfr") {
		t.Errorf("adoption d'un home égal au répertoire du groupe : code %d, attendu %d :\n%s",
			code, CodeConflict, out)
	}
	if existe(t, h.path("etc", "ogs", "projects", "boutiquefr.yaml")) {
		t.Error("aucune déclaration ne doit être écrite")
	}
}
