package system

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leviathan/ogs/internal/config"
	"github.com/leviathan/ogs/internal/engine"
	"github.com/leviathan/ogs/internal/exec"
	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/naming"
)

// fixture prépare un projet dont la racine est dans un répertoire temporaire,
// afin de pouvoir créer réellement l'arborescence sans être root.
func fixture(t *testing.T) (*model.Project, *Provider, *exec.Recorder, *FakeIdentity) {
	t.Helper()
	homeRoot := t.TempDir()
	p := model.New("shop.fr", naming.EnvProd, model.KindStatic)
	if err := p.Normalize(homeRoot, "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	rec := exec.NewRecorder()
	ident := NewFakeIdentity()
	prov := &Provider{
		Paths:    config.NewPaths(t.TempDir()),
		Exec:     rec,
		Identity: ident,
	}
	return p, prov, rec, ident
}

// declareKey simule le fichier que ssh-keygen aurait produit : l'enregistreur
// n'exécute pas la commande, mais l'idempotence se juge sur la présence du fichier.
func declareKey(t *testing.T, p *model.Project) {
	t.Helper()
	path := KeyPath(p.Spec.Layout.Root)
	for _, f := range []string{path, path + ".pub"} {
		if err := os.WriteFile(f, []byte("clé simulée\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func apply(t *testing.T, prov *Provider, p *model.Project) *engine.Report {
	t.Helper()
	ctx := context.Background()
	obs, err := prov.Observe(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	plan := &engine.Plan{Project: p.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	report, err := engine.Execute(ctx, plan, engine.Options{})
	if err != nil {
		t.Fatalf("apply : %v", err)
	}
	return report
}

func TestPlanCreatesGroupUserAndDirs(t *testing.T) {
	p, prov, rec, _ := fixture(t)
	report := apply(t, prov, p)
	if report.RolledBack {
		t.Fatal("aucun rollback attendu")
	}
	for _, want := range []string{"groupadd shopfr", "useradd", "gpasswd -a www-data shopfr"} {
		if !rec.Contains(want) {
			t.Errorf("commande manquante : %s\nappels : %v", want, rec.Calls())
		}
	}
	// Le compte est marqué comme géré par ogs, ce qui permettra de le distinguer
	// plus tard d'un compte adopté.
	if !rec.Contains("--comment ogs=shopfr") {
		t.Error("le compte doit porter la marque ogs dans son commentaire")
	}
	if !rec.Contains("--shell /usr/sbin/nologin") {
		t.Error("le compte doit être créé sans shell interactif")
	}
	// L'arborescence est réellement créée.
	for _, dir := range []string{"", "www", "var", "logs", ".bin", ".ssh"} {
		path := filepath.Join(p.Spec.Layout.Root, dir)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("répertoire absent : %s (%v)", path, err)
		}
		if !info.IsDir() {
			t.Errorf("%s n'est pas un répertoire", path)
		}
	}
}

func TestDirectoryModes(t *testing.T) {
	p, prov, _, _ := fixture(t)
	apply(t, prov, p)
	cases := map[string]os.FileMode{
		// La racine se traverse sans s'énumérer : le serveur web doit atteindre le
		// docroot, personne n'a besoin de lister le home.
		"": 0o711,
		// Les répertoires ne donnent au groupe qu'un droit de passage : servir un
		// fichier n'exige pas de pouvoir lister ce qui l'entoure.
		"www":  0o710,
		"var":  0o700, // hors de portée de www-data
		"logs": 0o710,
		".bin": 0o710,
		".ssh": 0o700,
	}
	for dir, want := range cases {
		path := filepath.Join(p.Spec.Layout.Root, dir)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s : mode %04o, attendu %04o", path, got, want)
		}
	}
}

func TestPlanIsIdempotent(t *testing.T) {
	p, prov, rec, ident := fixture(t)
	apply(t, prov, p)

	// Le système reflète maintenant l'état désiré.
	declareKey(t, p)
	ident.Users["shopfr"] = &UserInfo{Name: "shopfr", UID: 1004, GID: 1004,
		Home: p.Spec.Layout.Root, Shell: "/usr/sbin/nologin", Managed: true}
	ident.Groups["shopfr"] = &GroupInfo{Name: "shopfr", GID: 1004, Members: []string{WebUser}}

	before := len(rec.Calls())
	ctx := context.Background()
	obs, err := prov.Observe(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	plan := &engine.Plan{Project: p.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	if !plan.Empty() {
		t.Errorf("un second plan doit être vide, obtenu :\n%s", plan.String())
	}
	if len(rec.Calls()) != before {
		t.Error("aucune commande ne doit être exécutée lors du calcul du plan")
	}
}

func TestPlanCorrectsDriftedMode(t *testing.T) {
	p, prov, _, ident := fixture(t)
	apply(t, prov, p)
	declareKey(t, p)
	ident.Users["shopfr"] = &UserInfo{Name: "shopfr", UID: 1004, GID: 1004,
		Home: p.Spec.Layout.Root, Managed: true}
	ident.Groups["shopfr"] = &GroupInfo{Name: "shopfr", GID: 1004, Members: []string{WebUser}}

	logs := filepath.Join(p.Spec.Layout.Root, "logs")
	if err := os.Chmod(logs, 0o777); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	obs, _ := prov.Observe(ctx, p)
	steps, err := prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	plan := &engine.Plan{Project: p.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	mutating := plan.Mutating()
	if len(mutating) != 1 || !strings.Contains(mutating[0].Summary, "logs") {
		t.Fatalf("la dérive de mode doit produire exactement une étape, obtenu :\n%s", plan.String())
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(logs)
	if info.Mode().Perm() != 0o710 {
		t.Errorf("mode non corrigé : %04o", info.Mode().Perm())
	}
}

func TestPlanRefusesForeignUser(t *testing.T) {
	p, prov, _, ident := fixture(t)
	// Un compte homonyme non géré par ogs : refus explicite plutôt que
	// modification silencieuse.
	ident.Users["shopfr"] = &UserInfo{Name: "shopfr", UID: 1500, GID: 1500,
		Home: "/home/ancien", Managed: false}
	ctx := context.Background()
	obs, _ := prov.Observe(ctx, p)
	_, err := prov.Plan(ctx, p, obs)
	if err == nil || !strings.Contains(err.Error(), "n'est pas géré par ogs") {
		t.Fatalf("erreur attendue, obtenu : %v", err)
	}
}

func TestRollbackRemovesCreatedUserAndGroup(t *testing.T) {
	p, prov, rec, _ := fixture(t)
	ctx := context.Background()
	obs, _ := prov.Observe(ctx, p)
	steps, err := prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	plan := &engine.Plan{Project: p.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	// Une étape critique en échec est ajoutée à la fin, comme le ferait un
	// provider ultérieur (base de données, vhost).
	boom := engine.NewStep("test/boom", engine.Create, "étape en échec")
	boom.Apply = func(context.Context) error { return os.ErrPermission }
	plan.Add(boom)

	report, err := engine.Execute(ctx, plan, engine.Options{})
	if err == nil {
		t.Fatal("erreur attendue")
	}
	if !report.RolledBack || report.RollbackErr != nil {
		t.Fatalf("rollback : %+v", report)
	}
	for _, want := range []string{"gpasswd -d www-data shopfr", "userdel shopfr", "groupdel shopfr"} {
		if !rec.Contains(want) {
			t.Errorf("compensation manquante : %s", want)
		}
	}
	// Les répertoires créés et restés vides sont retirés ; la racine contenant
	// les sous-répertoires, elle peut subsister sans que ce soit une erreur.
	if _, err := os.Stat(filepath.Join(p.Spec.Layout.Root, "logs")); !os.IsNotExist(err) {
		t.Errorf("le répertoire logs devrait avoir été retiré : %v", err)
	}
}

func TestWebAccessNoneSkipsGroupMembership(t *testing.T) {
	p, prov, rec, _ := fixture(t)
	p.Spec.User.WebAccess = "none"
	apply(t, prov, p)
	if rec.Contains("gpasswd") {
		t.Error("webAccess none ne doit pas ajouter www-data au groupe")
	}
}

func TestDeployKeyIsGeneratedOnceAndNonCritical(t *testing.T) {
	p, prov, rec, _ := fixture(t)
	apply(t, prov, p)
	if !rec.Contains("ssh-keygen -t ed25519 -N  -C ogs=shopfr@") {
		t.Errorf("la clé de déploiement doit être générée sans passphrase : %v", rec.Calls())
	}
	// L'étape est non bloquante : une image sans ssh-keygen ne doit pas empêcher
	// la création du site.
	obs, _ := prov.Observe(context.Background(), p)
	steps, _ := prov.Plan(context.Background(), p, obs)
	for _, s := range steps {
		if s.Resource == "system/deploy-key/shopfr" && s.Critical {
			t.Error("la génération de clé doit être non critique")
		}
	}
	// Une fois la clé présente, l'étape disparaît du plan.
	declareKey(t, p)
	obs, _ = prov.Observe(context.Background(), p)
	steps, _ = prov.Plan(context.Background(), p, obs)
	for _, s := range steps {
		if s.Resource == "system/deploy-key/shopfr" && s.Mutates() {
			t.Error("la clé existante ne doit pas être régénérée")
		}
	}
}

func TestBinPHPLink(t *testing.T) {
	homeRoot := t.TempDir()
	p := model.New("shop.fr", naming.EnvProd, model.KindSymfony)
	p.Spec.App.Version = "6.4"
	if err := p.Normalize(homeRoot, "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	p.Spec.PHP.Pools[0].Version = "8.3"
	prefix := t.TempDir()
	prov := &Provider{Paths: config.NewPaths(prefix), Exec: exec.NewRecorder(), Identity: NewFakeIdentity()}
	apply(t, prov, p)

	link := filepath.Join(p.Spec.Layout.Root, ".bin", "php")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("lien .bin/php absent : %v", err)
	}
	if want := filepath.Join(prefix, "usr", "bin", "php8.3"); target != want {
		t.Errorf("cible du lien %q, attendue %q", target, want)
	}

	// Bascule de version : seul le lien change.
	p.Spec.PHP.Pools[0].Version = "8.5"
	apply(t, prov, p)
	target, _ = os.Readlink(link)
	if !strings.HasSuffix(target, "php8.5") {
		t.Errorf("le lien doit suivre la version : %q", target)
	}
}

func TestHomeGrantsGroupTraversal(t *testing.T) {
	// Le bit de groupe du home ne peut pas être ôté. Unix retient la première
	// classe applicable, sans repli : www-data étant membre du groupe du projet, un
	// home en 0701 lui appliquerait les droits de groupe — nuls — et lui refuserait
	// la traversée que les bits « autres » semblaient accorder. Ajouter www-data au
	// groupe lui retirerait donc l'accès, et le site cesserait d'être servi.
	p, prov, _, _ := fixture(t)
	apply(t, prov, p)

	info, err := os.Stat(p.Spec.Layout.Root)
	if err != nil {
		t.Fatal(err)
	}
	mode := info.Mode().Perm()
	if mode&0o010 == 0 {
		t.Errorf("le home (%04o) doit être traversable par son groupe, sans quoi "+
			"l'appartenance de www-data lui retirerait l'accès", mode)
	}
	if mode&0o040 != 0 {
		t.Errorf("le home (%04o) ne doit pas être énumérable par son groupe", mode)
	}
	if mode&0o004 != 0 {
		t.Errorf("le home (%04o) ne doit pas être énumérable par les autres", mode)
	}
}
