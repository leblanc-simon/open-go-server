package system

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
)

// fixture prépare un projet dont la racine est dans un répertoire temporaire,
// afin de pouvoir créer réellement l'arborescence sans être root.
func fixture(t *testing.T) (*model.Project, *Provider, *exec.Recorder, *FakeIdentity) {
	t.Helper()
	homeRoot := t.TempDir()
	p := model.New("shop.fr", naming.EnvProd, model.KindStatic)
	if err := p.Normalize(homeRoot, "preprod-pp.net", model.WithLogsRoot(filepath.Join(t.TempDir(), "ogs-projects"))); err != nil {
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
	if !rec.Contains("--shell " + model.DefaultShell) {
		t.Errorf("le compte doit être créé avec le shell déclaré : %v", rec.Calls())
	}
	// L'arborescence est réellement créée.
	for _, dir := range []string{"", "www", "var", "logs", ".local", ".local/bin", ".ssh"} {
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
		"www":        0o710,
		"var":        0o700, // hors de portée de www-data
		".local":     0o700,
		".local/bin": 0o700,
		".ssh":       0o700,
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
		Home: p.Spec.Layout.Root, Shell: p.Spec.User.Shell, Managed: true}
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

	logs := filepath.Join(p.Spec.Layout.Root, "var")
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
	if len(mutating) != 1 || !strings.Contains(mutating[0].Summary, "var") {
		t.Fatalf("la dérive de mode doit produire exactement une étape, obtenu :\n%s", plan.String())
	}
	if _, err := engine.Execute(ctx, plan, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(logs)
	if info.Mode().Perm() != 0o700 {
		t.Errorf("mode non corrigé : %04o", info.Mode().Perm())
	}
}

func TestPlanReconcilesShellDrift(t *testing.T) {
	// Le shell déclaré est un état désiré : l'éditer puis appliquer doit suffire à
	// ouvrir ou fermer l'accès d'un projet existant (D62). Sans cette
	// réconciliation, « spec.user.shell » serait une déclaration sans effet.
	p, prov, rec, ident := fixture(t)
	apply(t, prov, p)
	declareKey(t, p)
	ident.Users["shopfr"] = &UserInfo{Name: "shopfr", UID: 1004, GID: 1004,
		Home: p.Spec.Layout.Root, Shell: "/usr/sbin/nologin", Managed: true}
	ident.Groups["shopfr"] = &GroupInfo{Name: "shopfr", GID: 1004, Members: []string{WebUser}}

	report := apply(t, prov, p)
	if report.RolledBack {
		t.Fatal("la correction du shell ne doit pas dérouler la saga")
	}
	if !rec.Contains("usermod --shell " + model.DefaultShell + " shopfr") {
		t.Errorf("le shell doit être corrigé par usermod : %v", rec.Calls())
	}
}

func TestPlanCorrectsHomeAndShellInOneStep(t *testing.T) {
	// Deux étapes portant la même ressource se marcheraient dessus dans le plan
	// comme dans l'audit : un seul usermod pose les deux corrections.
	p, prov, rec, ident := fixture(t)
	apply(t, prov, p)
	declareKey(t, p)
	ident.Users["shopfr"] = &UserInfo{Name: "shopfr", UID: 1004, GID: 1004,
		Home: "/home/ancien", Shell: "/usr/sbin/nologin", Managed: true}
	ident.Groups["shopfr"] = &GroupInfo{Name: "shopfr", GID: 1004, Members: []string{WebUser}}

	ctx := context.Background()
	obs, err := prov.Observe(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range steps {
		if s.Resource == "system/user/shopfr" && s.Mutates() {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d étapes pour le compte, attendu une seule", n)
	}
	apply(t, prov, p)
	if !rec.Contains("usermod --home " + p.Spec.Layout.Root + " --shell " + model.DefaultShell + " shopfr") {
		t.Errorf("home et shell doivent être posés ensemble : %v", rec.Calls())
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
	if _, err := os.Stat(p.Spec.Layout.LogDir); !os.IsNotExist(err) {
		t.Errorf("le répertoire des journaux devrait avoir été retiré : %v", err)
	}
	if _, err := os.Lstat(filepath.Join(p.Spec.Layout.Root, "logs")); !os.IsNotExist(err) {
		t.Errorf("le lien ~/logs devrait avoir été retiré : %v", err)
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

func TestLAppartenanceAuGroupeRechargeLeMoteurRetenu(t *testing.T) {
	// L'intention nue « web », émise ici depuis le premier lot, n'était routée
	// nulle part : le rechargement n'avait pas lieu, et chaque création de projet
	// se soldait par un avertissement que rien n'affichait, et par le code 6.
	p, prov, _, _ := fixture(t)
	prov.WebReload = "web:nginx"
	ctx := context.Background()
	obs, err := prov.Observe(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if !strings.HasPrefix(s.Resource, "system/group-member/") {
			continue
		}
		if len(s.Reload) != 1 || s.Reload[0] != "web:nginx" {
			t.Errorf("intention « web:nginx » attendue, obtenu %v", s.Reload)
		}
		return
	}
	t.Fatal("aucune étape d'appartenance au groupe")
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

// declareCommonKeys pose le trousseau commun sous le préfixe du provider, comme
// l'administrateur le ferait dans /opt.
func declareCommonKeys(t *testing.T, prov *Provider) string {
	t.Helper()
	path := prov.Paths.DefaultAuthorizedKeys()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("ssh-ed25519 AAAA opérateur\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAuthorizedKeysLinkedToCommonKeyring(t *testing.T) {
	p, prov, _, _ := fixture(t)
	source := declareCommonKeys(t, prov)
	apply(t, prov, p)

	link := AuthorizedKeysPath(p.Spec.Layout.Root)
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("le lien des clés autorisées est absent : %v", err)
	}
	if target != source {
		t.Errorf("cible du lien %q, attendue %q", target, source)
	}
	// Un lien, non une copie : le trousseau doit rester la seule source.
	if info, lerr := os.Lstat(link); lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Error("les clés autorisées doivent être un lien, une copie figerait le trousseau")
	}

	// Idempotence : l'étape disparaît du plan une fois le lien posé.
	obs, _ := prov.Observe(context.Background(), p)
	steps, _ := prov.Plan(context.Background(), p, obs)
	for _, s := range steps {
		if s.Resource == "system/authorized-keys/shopfr" && s.Mutates() {
			t.Errorf("le lien déjà conforme ne doit pas être réécrit : %s", s)
		}
	}
}

func TestAuthorizedKeysAbsentWithoutCommonKeyring(t *testing.T) {
	// Sans trousseau commun, aucune étape : la fonction est facultative, et rien
	// ne doit apparaître dans le plan de tous les parcs qui ne s'en servent pas.
	p, prov, _, _ := fixture(t)
	apply(t, prov, p)

	if _, err := os.Lstat(AuthorizedKeysPath(p.Spec.Layout.Root)); err == nil {
		t.Error("aucun authorized_keys ne doit être créé sans trousseau commun")
	}
	obs, _ := prov.Observe(context.Background(), p)
	steps, _ := prov.Plan(context.Background(), p, obs)
	for _, s := range steps {
		if strings.HasPrefix(s.Resource, "system/authorized-keys/") {
			t.Errorf("étape inattendue : %s", s.Resource)
		}
	}
}

func TestAuthorizedKeysPreservesHandWrittenFile(t *testing.T) {
	// Un authorized_keys posé à la main porte l'accès de quelqu'un : le remplacer
	// par un lien le lui retirerait sans un mot. L'étape échoue donc sans rien
	// détruire, et n'étant pas critique, elle n'annule pas le reste du projet.
	p, prov, _, _ := fixture(t)
	declareCommonKeys(t, prov)
	link := AuthorizedKeysPath(p.Spec.Layout.Root)
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	const propre = "ssh-ed25519 BBBB clé du projet\n"
	if err := os.WriteFile(link, []byte(propre), 0o600); err != nil {
		t.Fatal(err)
	}

	report := apply(t, prov, p)
	if report.RolledBack {
		t.Fatal("l'échec du lien ne doit pas dérouler la saga")
	}
	data, err := os.ReadFile(link)
	if err != nil || string(data) != propre {
		t.Errorf("le fichier posé à la main a été altéré : %q, %v", string(data), err)
	}
	var warned bool
	for _, o := range report.Outcomes {
		if o.Step.Resource == "system/authorized-keys/shopfr" && o.Err != nil {
			warned = true
			if !strings.Contains(o.Err.Error(), "n'est pas un lien") {
				t.Errorf("message peu explicite : %v", o.Err)
			}
		}
	}
	if !warned {
		t.Error("un authorized_keys posé à la main doit être signalé, non ignoré")
	}
}

func TestAuthorizedKeysRedirectsForeignLink(t *testing.T) {
	// Un lien qui pointe ailleurs — trousseau déplacé, reprise d'un autre outil —
	// est ramené sur le trousseau commun : rien n'est perdu, la cible existe encore.
	p, prov, _, _ := fixture(t)
	source := declareCommonKeys(t, prov)
	link := AuthorizedKeysPath(p.Spec.Layout.Root)
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/opt/ancien_trousseau", link); err != nil {
		t.Fatal(err)
	}

	apply(t, prov, p)
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if target != source {
		t.Errorf("cible du lien %q, attendue %q", target, source)
	}
}

func TestBinPHPLink(t *testing.T) {
	homeRoot := t.TempDir()
	p := model.New("shop.fr", naming.EnvProd, model.KindSymfony)
	p.Spec.App.Version = "6.4"
	if err := p.Normalize(homeRoot, "preprod-pp.net", model.WithLogsRoot(filepath.Join(t.TempDir(), "ogs-projects"))); err != nil {
		t.Fatal(err)
	}
	p.Spec.PHP.Pools[0].Version = "8.3"
	prefix := t.TempDir()
	prov := &Provider{Paths: config.NewPaths(prefix), Exec: exec.NewRecorder(), Identity: NewFakeIdentity()}
	apply(t, prov, p)

	link := filepath.Join(p.Spec.Layout.Root, ".local", "bin", "php")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("lien .local/bin/php absent : %v", err)
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

func TestShellDuSystemeEstLuDansPasswd(t *testing.T) {
	// os/user ne rend pas le shell : sans lecture de /etc/passwd, le shell observé
	// reste vide sur un système réel et la réconciliation de D62 ne voit jamais de
	// dérive — alors qu'elle fonctionne en mode préfixé, où la base des comptes est
	// un fichier. Le défaut ne se voyait qu'en scénario d'intégration.
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	if err := os.WriteFile(passwd,
		[]byte("root:x:0:0:root:/root:/bin/bash\nshopfr:x:1001:1001:ogs=shopfr:/home/shop.fr:/usr/sbin/nologin\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	shell, ok := shellFromPasswd(passwd, "shopfr")
	if !ok || shell != "/usr/sbin/nologin" {
		t.Errorf("shell lu = %q (trouvé : %v), attendu /usr/sbin/nologin", shell, ok)
	}
	// Un compte servi par NSS sans exister dans /etc/passwd garde un shell vide :
	// ogs n'a pas à lancer un usermod sur un compte défini ailleurs.
	if _, ok := shellFromPasswd(passwd, "ldapuser"); ok {
		t.Error("un compte absent de /etc/passwd ne doit pas rendre de shell")
	}
	if _, ok := shellFromPasswd(filepath.Join(dir, "absent"), "shopfr"); ok {
		t.Error("une base de comptes absente ne doit pas rendre de shell")
	}
}

func TestPlanRefuseUnRepertoireRemplaceParUnLien(t *testing.T) {
	// www/ vit sous un home que l'utilisateur du projet écrit (0711) : il peut le
	// retirer et poser à sa place un lien vers n'importe quel répertoire du
	// système. Le suivre poserait le mode 0710 et le propriétaire du projet sur la
	// cible — l'escalade vers root que la propriété root de logs/ ferme par un
	// autre chemin (§7.1.2).
	p, prov, _, _ := fixture(t)
	apply(t, prov, p)

	temoin := t.TempDir()
	if err := os.Chmod(temoin, 0o755); err != nil {
		t.Fatal(err)
	}
	www := filepath.Join(p.Spec.Layout.Root, "www")
	if err := os.Remove(www); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(temoin, www); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	obs, err := prov.Observe(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Dirs[www].Symlink {
		t.Fatal("le lien n'est pas constaté : statDir suit encore les liens")
	}
	steps, err := prov.Plan(ctx, p, obs)
	if err != nil {
		t.Fatal(err)
	}
	plan := &engine.Plan{Project: p.ID()}
	for _, s := range steps {
		plan.Add(s)
	}
	// Le refus se lit dès le plan : annoncer « mode 0755 → 0710 » ferait croire à
	// une correction bénigne, alors que le mode constaté est celui de la cible.
	for _, s := range plan.Mutating() {
		if s.Resource != "system/dir/"+www {
			continue
		}
		if !strings.Contains(s.Summary, "lien symbolique") {
			t.Errorf("le plan doit annoncer le lien, non une correction de mode : %q", s.Summary)
		}
	}
	report, err := engine.Execute(ctx, plan, engine.Options{})
	if err == nil {
		t.Fatal("un répertoire remplacé par un lien doit faire échouer l'étape, non la corriger")
	}
	var signale bool
	for _, o := range report.Outcomes {
		if o.Step.Resource != "system/dir/"+www || o.Err == nil {
			continue
		}
		signale = true
		if !strings.Contains(o.Err.Error(), www) || !strings.Contains(o.Err.Error(), "lien symbolique") {
			t.Errorf("message peu explicite : %v", o.Err)
		}
	}
	if !signale {
		t.Error("le lien doit produire une étape en échec qui nomme le chemin")
	}
	info, err := os.Stat(temoin)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("la cible du lien est passée en %04o : ogs a suivi le lien", got)
	}
	// Le reste de l'arborescence continue d'être planifié : un lien ne rend pas
	// le plan aveugle au reste du projet.
	if len(steps) < len(prov.paths(p)) {
		t.Errorf("plan tronqué : %d étapes pour %d répertoires", len(steps), len(prov.paths(p)))
	}
}

// etatConforme fabrique l'état d'une arborescence au mode attendu, appartenant
// tout entière à un même couple : c'est ce que laisse un « chown -R ».
func etatConforme(t *testing.T, p *model.Project, uid, gid int) Observed {
	t.Helper()
	obs := Observed{
		User:  &UserInfo{Name: p.Spec.User.Name, UID: uid, GID: gid, Managed: true},
		Group: &GroupInfo{Name: p.Spec.User.Group, GID: gid},
		Dirs:  map[string]DirState{},
	}
	mode, err := fs.ParseMode(p.Spec.Layout.Mode)
	if err != nil {
		t.Fatal(err)
	}
	obs.Dirs[p.Spec.Layout.Root] = DirState{Exists: true, Mode: mode, UID: uid, GID: gid}
	for _, d := range p.Spec.Layout.Dirs {
		m, merr := fs.ParseMode(d.Mode)
		if merr != nil {
			t.Fatal(merr)
		}
		obs.Dirs[filepath.Join(p.Spec.Layout.Root, d.Path)] = DirState{
			Exists: true, Mode: m, UID: uid, GID: gid,
		}
	}
	return obs
}

func TestPlanCorrigeUneDeriveDeProprietaire(t *testing.T) {
	// Un « chown -R shopfr:shopfr /home/shop.fr » laisse les modes intacts : seul
	// le propriétaire change. Sans comparaison, un répertoire déclaré au nom de
	// root reste « conforme » à jamais. Les journaux ont quitté le home (D71) : le
	// mécanisme est éprouvé sur un répertoire root que l'exploitant déclare.
	p, prov, _, _ := fixture(t)
	p.Spec.Layout.Dirs = append(p.Spec.Layout.Dirs, model.Dir{Path: "outils", Mode: "0710", Owner: "root"})
	// L'exécuteur réel : sous préfixe, ensureDir laisse le propriétaire en place
	// (fs.KeepOwner), et comparer y ferait osciller le plan.
	prov.Exec = &exec.Local{}

	steps, err := prov.dirSteps(p, etatConforme(t, p, 4242, 4242))
	if err != nil {
		t.Fatal(err)
	}
	corriges := map[string]string{}
	for _, s := range steps {
		if s.Mutates() {
			corriges[s.Resource] = s.Summary
		}
	}
	for _, dir := range []string{"outils"} {
		res := "system/dir/" + filepath.Join(p.Spec.Layout.Root, dir)
		resume, ok := corriges[res]
		if !ok {
			t.Errorf("%s appartient à root : la dérive de propriétaire doit être corrigée", dir)
			continue
		}
		if !strings.Contains(resume, "propriétaire") {
			t.Errorf("%s : le libellé doit nommer la dérive de propriétaire (%q)", dir, resume)
		}
		delete(corriges, res)
	}
	if len(corriges) != 0 {
		t.Errorf("les répertoires du projet sont au bon propriétaire : %v", corriges)
	}
}

func TestDeriveDeProprietaireIgnoreeEnModeSimule(t *testing.T) {
	// Sous préfixe, l'arborescence appartient à qui fait tourner l'essai et
	// ensureDir ne pose aucun propriétaire : signaler un écart ferait osciller le
	// plan sur une correction qu'aucune étape n'applique.
	p, prov, _, _ := fixture(t)
	steps, err := prov.dirSteps(p, etatConforme(t, p, 4242, 4242))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Mutates() {
			t.Errorf("étape inattendue en mode simulé : %s", s)
		}
	}
}

func TestCleDeDeploiementRefuseUnLienDansSsh(t *testing.T) {
	// .ssh appartient à l'utilisateur du projet, qui en choisit les entrées. Un
	// os.Stat conclurait « clé absente » sur un lien pendant, et ssh-keygen —
	// lancé en root, sans O_NOFOLLOW — écrirait la clé dans la cible, que le chown
	// suivant lui donnerait : écriture root arbitraire, déclenchée par un apply de
	// routine (§7.1.2).
	for _, cas := range []struct{ nom, fichier string }{
		{"clé privée", KeyName},
		{"clé publique", KeyName + ".pub"},
	} {
		t.Run(cas.nom, func(t *testing.T) {
			p, prov, rec, _ := fixture(t)
			ssh := filepath.Dir(KeyPath(p.Spec.Layout.Root))
			if err := os.MkdirAll(ssh, 0o700); err != nil {
				t.Fatal(err)
			}
			cible := filepath.Join(t.TempDir(), "fichier-de-root")
			if err := os.Symlink(cible, filepath.Join(ssh, cas.fichier)); err != nil {
				t.Fatal(err)
			}

			report := apply(t, prov, p)
			if report.RolledBack {
				t.Fatal("le refus doit rester non bloquant : le reste du projet s'applique")
			}
			var signale bool
			for _, o := range report.Outcomes {
				if o.Step.Resource == "system/deploy-key/shopfr" && o.Err != nil {
					signale = true
					if !strings.Contains(o.Err.Error(), cas.fichier) {
						t.Errorf("message peu explicite : %v", o.Err)
					}
				}
			}
			if !signale {
				t.Error("un lien à la place de la clé doit être signalé, non contourné")
			}
			if _, err := os.Lstat(cible); !os.IsNotExist(err) {
				t.Errorf("ogs a écrit dans la cible du lien : %v", err)
			}
			if rec.Contains("ssh-keygen") {
				t.Errorf("ssh-keygen ne doit pas être lancé sur un chemin lié : %v", rec.Calls())
			}
		})
	}
}

func TestCleDeDeploiementEstGenereeHorsDeSsh(t *testing.T) {
	p, prov, rec, _ := fixture(t)
	apply(t, prov, p)
	// La paire est produite dans un répertoire de root, puis installée : laisser
	// ssh-keygen écrire directement dans .ssh donnerait à l'utilisateur du projet
	// une écriture root sur le chemin de son choix.
	ssh := filepath.Dir(KeyPath(p.Spec.Layout.Root))
	for _, appel := range rec.Calls() {
		if !strings.Contains(appel.String(), "ssh-keygen") {
			continue
		}
		if strings.Contains(appel.String(), ssh) {
			t.Errorf("ssh-keygen écrit dans le répertoire du projet : %s", appel.String())
		}
	}
}

func TestInstallKeyPairNeSuitPasUnLien(t *testing.T) {
	// La fenêtre que l'utilisateur du projet gagnerait sinon : reposer un lien
	// entre le contrôle du plan et l'écriture de l'apply. Le nom est délié puis
	// recréé en O_CREAT|O_EXCL|O_NOFOLLOW par le descripteur de .ssh, jamais suivi.
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	temoin := filepath.Join(t.TempDir(), "fichier-de-root")
	const contenu = "contenu de root\n"
	if err := os.WriteFile(temoin, []byte(contenu), 0o600); err != nil {
		t.Fatal(err)
	}
	lien := filepath.Join(root, ".ssh", KeyName)
	if err := os.Symlink(temoin, lien); err != nil {
		t.Fatal(err)
	}
	gen := filepath.Join(t.TempDir(), KeyName)
	for f, data := range map[string]string{gen: "clé privée\n", gen + ".pub": "clé publique\n"} {
		if err := os.WriteFile(f, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ssh, err := fs.OpenDirAt(root, ".ssh")
	if err != nil {
		t.Fatal(err)
	}
	defer ssh.Close()
	if err := installKeyPair(gen, ssh, fs.KeepOwner); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(temoin)
	if err != nil || string(data) != contenu {
		t.Errorf("la cible du lien a été écrasée : %q, %v", string(data), err)
	}
	for name, mode := range map[string]os.FileMode{KeyName: 0o600, KeyName + ".pub": 0o644} {
		info, err := os.Lstat(filepath.Join(root, ".ssh", name))
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Errorf("%s devrait être un fichier régulier (%v)", name, info.Mode())
		}
		if got := info.Mode().Perm(); got != mode {
			t.Errorf("%s : mode %04o, attendu %04o", name, got, mode)
		}
	}
}

// keygenFactice tient lieu de ssh-keygen sans être un exécuteur simulé : la
// rotation va alors jusqu'à l'installation de la paire, que l'on veut éprouver.
type keygenFactice struct{ appels []exec.Command }

func (k *keygenFactice) Simulated() bool { return false }

func (k *keygenFactice) Run(_ context.Context, c exec.Command) (exec.Result, error) {
	k.appels = append(k.appels, c)
	if c.Name == "ssh-keygen" {
		out := c.Args[len(c.Args)-1]
		if err := os.WriteFile(out, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nneuve\n"), 0o600); err != nil {
			return exec.Result{}, err
		}
		if err := os.WriteFile(out+".pub", []byte("ssh-ed25519 NEUVE ogs=shopfr@test\n"), 0o644); err != nil {
			return exec.Result{}, err
		}
	}
	return exec.Result{}, nil
}

// identiteDeLEssai donne au projet l'uid et le gid de qui fait tourner l'essai :
// les chown de l'installation aboutissent alors sans root.
func identiteDeLEssai(ident *FakeIdentity) {
	ident.Users["shopfr"] = &UserInfo{Name: "shopfr", UID: os.Getuid(), GID: os.Getgid(), Managed: true}
	ident.Groups["shopfr"] = &GroupInfo{Name: "shopfr", GID: os.Getgid()}
}

// sshLie remplace le .ssh du projet par un lien vers un répertoire hors du home,
// qui figure /root/.ssh et porte déjà une paire.
func sshLie(t *testing.T, root string) string {
	t.Helper()
	cible := filepath.Join(t.TempDir(), "root-ssh")
	if err := os.MkdirAll(cible, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{KeyName: "CLÉ PRIVÉE DE ROOT\n", KeyName + ".pub": "CLÉ PUBLIQUE DE ROOT\n"} {
		if err := os.WriteFile(filepath.Join(cible, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(root, 0o711); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Join(root, ".ssh"))
	if err := os.Symlink(cible, filepath.Join(root, ".ssh")); err != nil {
		t.Fatal(err)
	}
	return cible
}

func TestRotationRefuseUnSshRemplaceParUnLien(t *testing.T) {
	// « ogs user key rotate » tourne hors de tout plan : aucun contrôle de
	// l'arborescence ne le précède. Un Lstat des seules clés laissait résoudre
	// .ssh, et root supprimait puis recréait la paire dans le répertoire visé
	// — /root/.ssh, /etc/apt/apt.conf.d —, qu'il donnait ensuite au projet.
	p, _, _, ident := fixture(t)
	identiteDeLEssai(ident)
	cible := sshLie(t, p.Spec.Layout.Root)

	kg := &keygenFactice{}
	sauvegardes := filepath.Join(t.TempDir(), "backups")
	old, err := RotateKey(context.Background(), kg, p, ident, sauvegardes)
	if err == nil || !strings.Contains(err.Error(), "lien symbolique") {
		t.Fatalf("la rotation doit refuser un .ssh lié, obtenu %v", err)
	}
	if old != "" {
		t.Errorf("l'ancienne clé a été lue à travers le lien : %q", old)
	}
	for name, attendu := range map[string]string{KeyName: "CLÉ PRIVÉE DE ROOT\n", KeyName + ".pub": "CLÉ PUBLIQUE DE ROOT\n"} {
		data, err := os.ReadFile(filepath.Join(cible, name))
		if err != nil || string(data) != attendu {
			t.Errorf("%s a été touché dans la cible du lien : %q, %v", name, string(data), err)
		}
	}
	if copies, _ := os.ReadDir(sauvegardes); len(copies) != 0 {
		t.Errorf("%d copie(s) de la cible sauvegardée(s)", len(copies))
	}
	if len(kg.appels) != 0 {
		t.Errorf("le refus doit précéder toute génération : %v", kg.appels)
	}
}

func TestRotationRefuseUnSshQuiNAppartientPasAuProjet(t *testing.T) {
	// Un vrai répertoire, mais pas celui du projet : l'utilisateur peut renommer
	// en .ssh un répertoire de root resté dans son home.
	p, _, _, ident := fixture(t)
	ident.Users["shopfr"] = &UserInfo{Name: "shopfr", UID: os.Getuid() + 1, GID: os.Getgid(), Managed: true}
	ident.Groups["shopfr"] = &GroupInfo{Name: "shopfr", GID: os.Getgid()}
	ssh := filepath.Join(p.Spec.Layout.Root, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ssh, KeyName), []byte("CLÉ D'UN AUTRE\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	kg := &keygenFactice{}
	// C'est .ssh lui-même que le refus nomme, avant d'examiner la moindre clé — et
	// ce qui le répare : l'apply rend .ssh au projet.
	_, err := RotateKey(context.Background(), kg, p, ident, filepath.Join(t.TempDir(), "b"))
	if err == nil || !strings.Contains(err.Error(), ssh+" appartient à l'uid") {
		t.Fatalf("la rotation doit refuser un .ssh étranger au projet, obtenu %v", err)
	}
	if !strings.Contains(err.Error(), "« ogs apply shopfr »") {
		t.Errorf("le refus doit dire ce qui rend .ssh au projet : %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(ssh, KeyName)); string(data) != "CLÉ D'UN AUTRE\n" {
		t.Errorf("la clé a été remplacée : %q", string(data))
	}
	if len(kg.appels) != 0 {
		t.Errorf("le refus doit précéder toute génération : %v", kg.appels)
	}
}

func TestUnSshEtrangerSansCleEstRefuse(t *testing.T) {
	// Sans clé à examiner, seul le contrôle du répertoire parle : root créerait
	// sinon la paire dans un .ssh qui n'est pas celui du projet — un répertoire
	// de root que l'utilisateur a renommé dans son home —, et « show » dirait
	// l'absence de clé au lieu du refus.
	p, _, _, ident := fixture(t)
	ident.Users["shopfr"] = &UserInfo{Name: "shopfr", UID: os.Getuid() + 1, GID: os.Getgid(), Managed: true}
	ident.Groups["shopfr"] = &GroupInfo{Name: "shopfr", GID: os.Getgid()}
	ssh := filepath.Join(p.Spec.Layout.Root, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}

	kg := &keygenFactice{}
	if _, err := RotateKey(context.Background(), kg, p, ident, filepath.Join(t.TempDir(), "b")); err == nil ||
		!strings.Contains(err.Error(), ssh+" appartient à l'uid") {
		t.Errorf("la rotation doit refuser un .ssh étranger, même vide : %v", err)
	}
	if len(kg.appels) != 0 {
		t.Errorf("le refus doit précéder toute génération : %v", kg.appels)
	}
	if entries, _ := os.ReadDir(ssh); len(entries) != 0 {
		t.Errorf("rien ne doit être écrit dans un .ssh étranger : %v", entries)
	}
	if _, err := PublicKey(kg, p, ident); err == nil || !strings.Contains(err.Error(), ssh+" appartient à l'uid") {
		t.Errorf("« user key show » doit refuser un .ssh étranger, non dire la clé absente : %v", err)
	}
}

// cleCreuse crée, dans le .ssh du projet, un fichier creux de size octets : il
// n'occupe aucun bloc, et ne coûte donc rien à son propriétaire.
func cleCreuse(t *testing.T, root, name string, size int64) {
	t.Helper()
	ssh := filepath.Join(root, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(ssh, name)
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(f, size); err != nil {
		t.Fatal(err)
	}
}

func TestLaRotationNeLitPasUneCleDemesuree(t *testing.T) {
	// Une clé creuse au compte du projet passe tous les autres contrôles : elle
	// est à lui. La rotation la lisait en mémoire puis la réécrivait pleine dans
	// les sauvegardes — de quoi épuiser la mémoire de root, ou remplir /var à
	// chaque rotation.
	p, _, _, ident := fixture(t)
	identiteDeLEssai(ident)
	cleCreuse(t, p.Spec.Layout.Root, KeyName, 8<<20)

	kg := &keygenFactice{}
	sauvegardes := filepath.Join(t.TempDir(), "backups")
	if _, err := RotateKey(context.Background(), kg, p, ident, sauvegardes); err == nil ||
		!strings.Contains(err.Error(), "octets") {
		t.Fatalf("la rotation doit refuser une clé démesurée : %v", err)
	}
	if copies, _ := os.ReadDir(sauvegardes); len(copies) != 0 {
		t.Errorf("une clé démesurée ne doit pas être sauvegardée : %v", copies)
	}
	if len(kg.appels) != 0 {
		t.Errorf("le refus doit précéder toute génération : %v", kg.appels)
	}
}

func TestLaClePubliqueDemesureeNEstPasLue(t *testing.T) {
	p, _, _, ident := fixture(t)
	identiteDeLEssai(ident)
	cleCreuse(t, p.Spec.Layout.Root, KeyName+".pub", 8<<20)
	if pub, err := PublicKey(&keygenFactice{}, p, ident); err == nil || len(pub) != 0 {
		t.Errorf("« user key show » ne doit pas rendre une clé démesurée : %d octets, %v", len(pub), err)
	}
	// La rotation la refuse de même, sans rien lire ni générer.
	kg := &keygenFactice{}
	if _, err := RotateKey(context.Background(), kg, p, ident, filepath.Join(t.TempDir(), "b")); err == nil ||
		len(kg.appels) != 0 {
		t.Errorf("la rotation doit refuser une clé publique démesurée : %v, %v", err, kg.appels)
	}
}

func TestLaClePubliqueNeSAfficheQueSiElleEstImprimable(t *testing.T) {
	// La clé publique appartient au projet, et « show » comme « rotate » la
	// rendent brute dans le terminal de l'exploitant : un saut de ligne et une
	// séquence d'échappement y forgeaient des lignes, et un terminal en 8 bits lit
	// 0x9b comme l'introducteur d'une séquence.
	p, _, _, ident := fixture(t)
	identiteDeLEssai(ident)
	ssh := filepath.Join(p.Spec.Layout.Root, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, forge := range []string{
		"ssh-ed25519 AAAA ogs\n✓ clé vérifiée\n",
		"ssh-ed25519 AAAA ogs\x1b[2J\n",
		"ssh-ed25519 AAAA ogs\x9b2J\n",
	} {
		if err := os.WriteFile(filepath.Join(ssh, KeyName+".pub"), []byte(forge), 0o644); err != nil {
			t.Fatal(err)
		}
		if pub, err := PublicKey(&keygenFactice{}, p, ident); err == nil || pub != "" {
			t.Errorf("%q ne doit pas s'afficher : %q, %v", forge, pub, err)
		}
		kg := &keygenFactice{}
		if old, err := RotateKey(context.Background(), kg, p, ident, filepath.Join(t.TempDir(), "b")); err == nil ||
			old != "" || len(kg.appels) != 0 {
			t.Errorf("la rotation doit refuser %q avant tout : %q, %v, %v", forge, old, err, kg.appels)
		}
	}
	// Une clé ordinaire, commentaire accentué compris, s'affiche.
	cle := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI ogs=shopfr@hôte\n"
	if err := os.WriteFile(filepath.Join(ssh, KeyName+".pub"), []byte(cle), 0o644); err != nil {
		t.Fatal(err)
	}
	if pub, err := PublicKey(&keygenFactice{}, p, ident); err != nil || pub != cle {
		t.Errorf("une clé imprimable doit s'afficher telle quelle : %q, %v", pub, err)
	}
}

func TestRotationRemplaceLaPaireDansSsh(t *testing.T) {
	p, _, _, ident := fixture(t)
	identiteDeLEssai(ident)
	ssh := filepath.Join(p.Spec.Layout.Root, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{KeyName: "ancienne privée\n", KeyName + ".pub": "ssh-ed25519 ANCIENNE\n"} {
		if err := os.WriteFile(filepath.Join(ssh, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	sauvegardes := filepath.Join(t.TempDir(), "backups")
	old, err := RotateKey(context.Background(), &keygenFactice{}, p, ident, sauvegardes)
	if err != nil {
		t.Fatal(err)
	}
	if old != "ssh-ed25519 ANCIENNE\n" {
		t.Errorf("ancienne clé publique : %q", old)
	}
	for name, mode := range map[string]os.FileMode{KeyName: 0o600, KeyName + ".pub": 0o644} {
		info, err := os.Lstat(filepath.Join(ssh, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); !info.Mode().IsRegular() || got != mode {
			t.Errorf("%s : %v, attendu un fichier régulier en %04o", name, info.Mode(), mode)
		}
	}
	pub, err := PublicKey(&keygenFactice{}, p, ident)
	if err != nil || !strings.Contains(pub, "NEUVE") {
		t.Errorf("la nouvelle clé publique doit être relue : %q, %v", pub, err)
	}
	copies, _ := os.ReadDir(sauvegardes)
	if len(copies) != 1 {
		t.Fatalf("une sauvegarde de l'ancienne clé privée attendue, obtenu %v", copies)
	}
	data, _ := os.ReadFile(filepath.Join(sauvegardes, copies[0].Name()))
	if string(data) != "ancienne privée\n" {
		t.Errorf("sauvegarde : %q", string(data))
	}
}

func TestDeuxRotationsDansLaMemeSecondeGardentLesDeuxCles(t *testing.T) {
	// La sauvegarde porte un nom horodaté à la seconde : la seconde rotation
	// écrasait celle de la première, et la clé privée d'origine était perdue.
	p, _, _, ident := fixture(t)
	identiteDeLEssai(ident)
	fige := time.Date(2026, 8, 23, 3, 4, 5, 0, time.UTC)
	avant := nowFunc
	nowFunc = func() time.Time { return fige }
	t.Cleanup(func() { nowFunc = avant })
	ssh := filepath.Join(p.Spec.Layout.Root, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ssh, KeyName), []byte("clé d'origine\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sauvegardes := filepath.Join(t.TempDir(), "backups")
	for i := 0; i < 2; i++ {
		if _, err := RotateKey(context.Background(), &keygenFactice{}, p, ident, sauvegardes); err != nil {
			t.Fatal(err)
		}
	}
	copies, _ := os.ReadDir(sauvegardes)
	var contenus []string
	for _, c := range copies {
		data, _ := os.ReadFile(filepath.Join(sauvegardes, c.Name()))
		contenus = append(contenus, string(data))
	}
	if len(copies) != 2 || !slices.Contains(contenus, "clé d'origine\n") {
		t.Errorf("chaque rotation doit garder sa sauvegarde, la clé d'origine comprise : %q", contenus)
	}
}

func TestLaClePubliqueDUnProjetJamaisAppliqueSeDitAbsente(t *testing.T) {
	// Un projet déclaré mais jamais appliqué n'a ni compte ni clé : « user key
	// show » doit dire l'absence de la clé et ce qui la crée, non celle du compte.
	p, _, _, ident := fixture(t)
	_, err := PublicKey(&keygenFactice{}, p, ident)
	if err == nil || !strings.Contains(err.Error(), "aucune clé de déploiement") {
		t.Errorf("l'absence de clé doit être dite : %v", err)
	}
	// Une clé présente sans compte connu reste, elle, un refus : rien n'est lu
	// sans que son propriétaire ait été contrôlé.
	ssh := filepath.Join(p.Spec.Layout.Root, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ssh, KeyName+".pub"), []byte("ssh-ed25519 ORPHELINE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if pub, err := PublicKey(&keygenFactice{}, p, ident); err == nil || strings.Contains(pub, "ORPHELINE") {
		t.Errorf("sans compte, la clé ne doit pas être lue : %q, %v", pub, err)
	}
}

func TestLaClePubliqueNeSeLitPasATraversUnLien(t *testing.T) {
	// « ogs user key show » tourne en root : un os.ReadFile par le nom affichait
	// à l'exploitant n'importe quel fichier que root peut lire.
	secret := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(secret, []byte("root:$6$HASH-SECRET:19000:0:99999:7:::\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("clé publique liée", func(t *testing.T) {
		p, _, _, ident := fixture(t)
		identiteDeLEssai(ident)
		ssh := filepath.Join(p.Spec.Layout.Root, ".ssh")
		if err := os.MkdirAll(ssh, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(secret, filepath.Join(ssh, KeyName+".pub")); err != nil {
			t.Fatal(err)
		}
		pub, err := PublicKey(&keygenFactice{}, p, ident)
		if err == nil || strings.Contains(pub, "HASH-SECRET") {
			t.Errorf("la clé publique liée doit être refusée : %q, %v", pub, err)
		}
	})
	t.Run(".ssh lié", func(t *testing.T) {
		p, _, _, ident := fixture(t)
		identiteDeLEssai(ident)
		sshLie(t, p.Spec.Layout.Root)
		pub, err := PublicKey(&keygenFactice{}, p, ident)
		if err == nil || strings.Contains(pub, "DE ROOT") {
			t.Errorf("un .ssh lié doit être refusé : %q, %v", pub, err)
		}
	})
}

func TestLaCompensationDeLaCleNeSuitPasUnSshLie(t *testing.T) {
	// Entre l'apply et le rollback, l'utilisateur remplace .ssh par un lien : un
	// os.Remove par le nom supprimait alors la paire du répertoire visé.
	p, prov, _, _ := fixture(t)
	root := p.Spec.Layout.Root
	if err := os.MkdirAll(filepath.Join(root, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := prov.keyStep(p)
	if s.Compensate == nil {
		t.Fatal("la génération de la clé doit être compensable")
	}
	cible := sshLie(t, root)
	if err := s.Compensate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{KeyName, KeyName + ".pub"} {
		if _, err := os.Lstat(filepath.Join(cible, name)); err != nil {
			t.Errorf("%s a été supprimé dans la cible du lien : %v", name, err)
		}
	}
}

func TestLaCompensationDesClesAutoriseesNeSuitPasUnSshLie(t *testing.T) {
	p, prov, _, _ := fixture(t)
	declareCommonKeys(t, prov)
	root := p.Spec.Layout.Root
	if err := os.MkdirAll(filepath.Join(root, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := prov.authorizedKeysStep(p)
	if s == nil || s.Compensate == nil {
		t.Fatal("la pose du lien doit être compensable")
	}
	cible := sshLie(t, root)
	if err := os.Symlink("/opt/clés-de-root", filepath.Join(cible, "authorized_keys")); err != nil {
		t.Fatal(err)
	}
	if err := s.Compensate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(cible, "authorized_keys")); err != nil {
		t.Errorf("l'authorized_keys de la cible du lien a été supprimé : %v", err)
	}
}

func TestRotationRefuseUnLien(t *testing.T) {
	// fs.Backup lit par os.ReadFile, donc à travers le lien : une rotation lancée
	// sur un lien vers un fichier de root en recopierait le contenu dans .ssh, où
	// le projet — qui possède le répertoire — le lit.
	p, _, rec, ident := fixture(t)
	ident.Users["shopfr"] = &UserInfo{Name: "shopfr", UID: 1004, GID: 1004, Managed: true}
	ident.Groups["shopfr"] = &GroupInfo{Name: "shopfr", GID: 1004}
	ssh := filepath.Dir(KeyPath(p.Spec.Layout.Root))
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	temoin := filepath.Join(t.TempDir(), "fichier-de-root")
	if err := os.WriteFile(temoin, []byte("secret de root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(temoin, KeyPath(p.Spec.Layout.Root)); err != nil {
		t.Fatal(err)
	}

	sauvegardes := filepath.Join(t.TempDir(), "backups")
	if _, err := RotateKey(context.Background(), rec, p, ident, sauvegardes); err == nil {
		t.Fatal("la rotation doit refuser un chemin qui n'est pas un fichier régulier")
	}
	entries, err := os.ReadDir(ssh)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".ogs-bak-") {
			t.Errorf("le contenu de la cible a été recopié dans .ssh : %s", e.Name())
		}
	}
	// Le refus précède toute lecture : rien n'a pu partir non plus dans le
	// répertoire des sauvegardes, où la copie irait désormais.
	if copies, _ := os.ReadDir(sauvegardes); len(copies) != 0 {
		t.Errorf("%d copie(s) écrite(s) malgré le refus", len(copies))
	}
}

func TestLaCompensationNeSuitPasUnLien(t *testing.T) {
	// Le plan refuse un répertoire lié, mais la compensation intervient après
	// coup : entre l'apply et le rollback, l'utilisateur du projet — qui écrit
	// dans son home — a le temps de substituer un lien. Un os.Chmod suivi d'un
	// os.Chown sur le chemin rendraient alors à la cible le mode et le
	// propriétaire d'origine de www/, c'est-à-dire ceux du projet (§7.1.2).
	p, prov, _, _ := fixture(t)
	apply(t, prov, p)

	www := filepath.Join(p.Spec.Layout.Root, "www")
	if err := os.Chmod(www, 0o777); err != nil {
		t.Fatal(err)
	}
	obs, err := prov.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.dirSteps(p, obs)
	if err != nil {
		t.Fatal(err)
	}
	var compense func(context.Context) error
	for _, s := range steps {
		if s.Resource == "system/dir/"+www && s.Compensate != nil {
			compense = s.Compensate
		}
	}
	if compense == nil {
		t.Fatal("un mode divergent doit produire une étape compensable")
	}

	temoin := t.TempDir()
	if err := os.Chmod(temoin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(www); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(temoin, www); err != nil {
		t.Fatal(err)
	}
	if err := compense(context.Background()); err == nil {
		t.Error("la compensation doit refuser un chemin devenu un lien")
	}
	info, err := os.Stat(temoin)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("la cible du lien est passée en %04o : la compensation a suivi le lien", got)
	}
}

func TestLAncienBinEstRetireApresLaMigration(t *testing.T) {
	p, prov, _, _ := fixture(t)
	legacy := filepath.Join(p.Spec.Layout.Root, ".bin")
	if err := os.MkdirAll(legacy, 0o710); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/php8.3", filepath.Join(legacy, "php")); err != nil {
		t.Fatal(err)
	}
	s := prov.PlanLegacyBin(p)
	if s == nil || s.Action != engine.Delete {
		t.Fatalf("retrait de l'ancien .bin attendu, obtenu %v", s)
	}
	// Les unités réécrites doivent avoir été relues avant que le lien disparaisse.
	if !s.FlushReloads || s.Critical {
		t.Error("l'étape doit vider les rechargements en attente et rester non critique")
	}
	if err := s.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(legacy); !os.IsNotExist(err) {
		t.Error("l'ancien .bin doit disparaître")
	}
	if prov.PlanLegacyBin(p) != nil {
		t.Error("plus rien à retirer une fois la migration faite")
	}
}

func TestLAncienBinGardeCeQuOgsNAPasPose(t *testing.T) {
	p, prov, _, _ := fixture(t)
	legacy := filepath.Join(p.Spec.Layout.Root, ".bin")
	if err := os.MkdirAll(legacy, 0o710); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/php8.3", filepath.Join(legacy, "php")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "composer"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := prov.PlanLegacyBin(p).Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "composer") {
		t.Fatalf("le répertoire doit rester, en nommant ce qu'il contient : %v", err)
	}
	if _, err := os.Lstat(filepath.Join(legacy, "php")); !os.IsNotExist(err) {
		t.Error("le lien géré doit être retiré même quand le répertoire reste")
	}
}

func TestLeLienPhpRefuseUnFichierDeLUtilisateur(t *testing.T) {
	// ~/.local/bin appartient à l'utilisateur : un fichier qu'il y a posé sous le
	// nom « php » porte son travail, et n'est remplacé que sous --force.
	p, prov, _, _ := fixture(t)
	p.Spec.PHP.Pools = []model.Pool{{Name: model.MainPoolName, Version: "8.3"}}
	dir := filepath.Join(p.Spec.Layout.Root, ".local", "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "php"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := prov.binStep(p)
	if s.Critical || !strings.Contains(s.Summary, "un fichier occupe déjà ce nom") {
		t.Fatalf("refus non critique attendu, obtenu %q (critique=%v)", s.Summary, s.Critical)
	}
	if err := s.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("le refus doit renvoyer à --force, obtenu %v", err)
	}
	prov.Force = true
	if err := prov.binStep(p).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "php")); err != nil || !strings.HasSuffix(target, "php8.3") {
		t.Errorf("--force doit poser le lien géré, obtenu %q (%v)", target, err)
	}
}

func TestLeLienPhpNeSuitPasUnRepertoireSubstitue(t *testing.T) {
	// L'utilisateur remplace ~/.local par un lien : root ne doit pas écrire là où
	// il l'envoie.
	p, prov, _, _ := fixture(t)
	p.Spec.PHP.Pools = []model.Pool{{Name: model.MainPoolName, Version: "8.3"}}
	ailleurs := t.TempDir()
	if err := os.MkdirAll(p.Spec.Layout.Root, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ailleurs, filepath.Join(p.Spec.Layout.Root, ".local")); err != nil {
		t.Fatal(err)
	}
	s := prov.binStep(p)
	if err := s.Apply(context.Background()); err == nil {
		t.Fatal("un répertoire substitué par un lien doit être refusé")
	}
	if entries, _ := os.ReadDir(ailleurs); len(entries) != 0 {
		t.Errorf("root a écrit dans le répertoire désigné par l'utilisateur : %v", entries)
	}
}

func TestLesJournauxSontPreCreesHorsDuHome(t *testing.T) {
	p, prov, _, _ := fixture(t)
	prov.WebEngine = "apache"
	p.Spec.PHP.Pools = []model.Pool{{Name: model.MainPoolName, Version: "8.3"}}
	apply(t, prov, p)

	dir := p.Spec.Layout.LogDir
	if strings.HasPrefix(dir, p.Spec.Layout.Root) {
		t.Fatalf("les journaux ne doivent pas vivre dans le home : %s", dir)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("répertoire des journaux attendu en 0750 : %v", err)
	}
	if info, err := os.Stat(filepath.Dir(dir)); err != nil || info.Mode().Perm() != 0o711 {
		t.Fatalf("racine des journaux attendue en 0711 : %v", err)
	}
	// php-error.log surtout : sans lui, le worker ne peut rien créer dans un
	// répertoire root, et PHP écrit dans le journal global de FPM.
	for _, name := range []string{"apache-access.log", "apache-error.log", "php-error.log", "php-slow.log"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0o640 {
			t.Errorf("%s attendu pré-créé en 0640 : %v", name, err)
		}
	}
	if target, err := os.Readlink(filepath.Join(p.Spec.Layout.Root, "logs")); err != nil || target != dir {
		t.Errorf("~/logs doit désigner %s, obtenu %q (%v)", dir, target, err)
	}
}

// legacyLayout reconstitue l'ancien ~/logs d'un projet d'avant D71.
func legacyLayout(t *testing.T, p *model.Project) string {
	t.Helper()
	legacy := filepath.Join(p.Spec.Layout.Root, "logs")
	if err := os.MkdirAll(legacy, 0o710); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"apache-error.log":      "ligne 1\n",
		"apache-error.log.1":    "roté\n",
		"apache-error.log.2.gz": "compressé",
	} {
		if err := os.WriteFile(filepath.Join(legacy, name), []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return legacy
}

func TestLaMigrationReprendLesJournauxSansPerte(t *testing.T) {
	p, prov, _, _ := fixture(t)
	if err := os.MkdirAll(p.Spec.Layout.Root, 0o711); err != nil {
		t.Fatal(err)
	}
	legacy := legacyLayout(t, p)
	if err := os.MkdirAll(p.Spec.Layout.LogDir, 0o750); err != nil {
		t.Fatal(err)
	}
	obs, _ := prov.Observe(context.Background(), p)
	prepare, finish := prov.PlanLogMigration(p, obs)
	if prepare == nil || finish == nil || !finish.FlushReloads {
		t.Fatalf("deux étapes attendues, la seconde après les rechargements : %v %v", prepare, finish)
	}
	// Même sans vhost ni pool modifié, les serveurs doivent rouvrir leurs journaux
	// au nouvel emplacement : sinon ils écrivent dans l'ancien fichier, que la
	// seconde étape retire.
	p.Spec.PHP.Pools = []model.Pool{{Name: model.MainPoolName, Version: "8.3"}}
	prov.WebReload = "web:apache"
	withReloads, _ := prov.PlanLogMigration(p, obs)
	if strings.Join(withReloads.Reload, ",") != "web:apache,php:8.3" {
		t.Errorf("rechargements attendus du serveur web et de FPM, obtenu %v", withReloads.Reload)
	}
	if err := prepare.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Entre la recopie et le rechargement, l'ancien fichier reçoit encore des
	// lignes, et le nouveau commence à en recevoir.
	appendTo := func(path, line string) {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(line)
		f.Close()
	}
	appendTo(filepath.Join(legacy, "apache-error.log"), "ligne 2\n")
	if err := finish.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := p.Spec.Layout.LogDir
	got, _ := os.ReadFile(filepath.Join(dir, "apache-error.log"))
	if string(got) != "ligne 1\nligne 2\n" {
		t.Errorf("aucune ligne ne doit être perdue ni dupliquée, obtenu %q", got)
	}
	for _, name := range []string{"apache-error.log.1", "apache-error.log.2.gz"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s doit suivre, sous son nom, pour que logrotate poursuive la numérotation", name)
		}
	}
	info, err := os.Lstat(legacy)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("l'ancien répertoire doit avoir laissé place au lien : %v", err)
	}
	if target, _ := os.Readlink(legacy); target != dir {
		t.Errorf("~/logs doit désigner %s, obtenu %s", dir, target)
	}
}

func TestLaMigrationNeLitPasUnRepertoireSubstitue(t *testing.T) {
	// Hors du mode simulé, un ancien logs/ qui n'appartient pas à root a été
	// substitué par l'utilisateur : son contenu est choisi par lui — un
	// « apache-error.log → /etc/shadow » —, et root ne doit rien en recopier dans
	// un répertoire que ce même utilisateur lit.
	if os.Geteuid() == 0 {
		t.Skip("le répertoire de test appartient à root")
	}
	p, prov, _, _ := fixture(t)
	prov.Exec = &exec.Local{}
	if err := os.MkdirAll(p.Spec.Layout.Root, 0o711); err != nil {
		t.Fatal(err)
	}
	legacy := legacyLayout(t, p)
	secret := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(legacy, "php-error.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.Spec.Layout.LogDir, 0o750); err != nil {
		t.Fatal(err)
	}
	prepare, _ := prov.PlanLogMigration(p, Observed{})
	err := prepare.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "n'appartient pas à root") {
		t.Fatalf("un répertoire qui n'appartient pas à root doit être refusé, obtenu %v", err)
	}
	if entries, _ := os.ReadDir(p.Spec.Layout.LogDir); len(entries) != 0 {
		t.Errorf("rien ne doit avoir été recopié : %v", entries)
	}
}

func TestLaMigrationNeSuitPasUnLienPoseAuLieuDUnJournal(t *testing.T) {
	p, prov, _, _ := fixture(t)
	if err := os.MkdirAll(p.Spec.Layout.Root, 0o711); err != nil {
		t.Fatal(err)
	}
	legacy := legacyLayout(t, p)
	secret := filepath.Join(t.TempDir(), "shadow")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(legacy, "php-error.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.Spec.Layout.LogDir, 0o750); err != nil {
		t.Fatal(err)
	}
	obs, _ := prov.Observe(context.Background(), p)
	prepare, finish := prov.PlanLogMigration(p, obs)
	if err := prepare.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := finish.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "php-error.log") {
		t.Fatalf("le lien doit rester, nommé, et l'ancien répertoire avec lui : %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(p.Spec.Layout.LogDir, "php-error.log")); strings.Contains(string(data), "secret") {
		t.Error("root a recopié la cible du lien dans le répertoire des journaux")
	}
}
