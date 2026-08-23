package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leviathan/ogs/internal/secret"
	"github.com/leviathan/ogs/internal/store"
)

// harness exécute la CLI en mode préfixé, ce qui permet d'éprouver le flux
// complet sans privilèges et sans toucher au système réel.
type harness struct {
	t      *testing.T
	prefix string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return &harness{t: t, prefix: t.TempDir()}
}

// run exécute une commande et renvoie sa sortie et son code de retour.
func (h *harness) run(args ...string) (string, int) {
	h.t.Helper()
	var out, errBuf bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(append([]string{"--prefix", h.prefix}, args...))
	err := root.Execute()
	// Le message d'erreur est produit par main ; on le joint à la sortie pour que
	// les tests puissent en vérifier le contenu.
	if err != nil {
		out.WriteString("\nogs : " + err.Error() + "\n")
	}
	return out.String(), ExitCode(err)
}

// mustRun exige un succès.
func (h *harness) mustRun(args ...string) string {
	h.t.Helper()
	out, code := h.run(args...)
	if code != CodeOK {
		h.t.Fatalf("« ogs %s » a échoué (code %d) :\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

// read lit un fichier produit sous le préfixe.
func (h *harness) read(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(h.path(rel))
	if err != nil {
		t.Fatalf("lecture de %s : %v", rel, err)
	}
	return string(data)
}

func (h *harness) path(elem ...string) string {
	return filepath.Join(append([]string{h.prefix}, elem...)...)
}

// simulateSystemEffects reproduit les effets que le mode préfixé n'exécute pas :
// création des comptes par useradd et de la paire de clés par ssh-keygen. Sans
// cela, l'idempotence ne pourrait pas être éprouvée sans privilèges.
func (h *harness) simulateSystemEffects(id, host string, uid int) {
	h.t.Helper()
	home := h.path("home", host)
	if err := os.MkdirAll(h.path("etc"), 0o755); err != nil {
		h.t.Fatal(err)
	}
	passwd := fmt.Sprintf("%s:x:%d:%d:ogs=%s:%s:/usr/sbin/nologin\n", id, uid, uid, id, home)
	if err := os.WriteFile(h.path("etc", "passwd"), []byte(passwd), 0o644); err != nil {
		h.t.Fatal(err)
	}
	group := fmt.Sprintf("%s:x:%d:www-data\n", id, uid)
	if err := os.WriteFile(h.path("etc", "group"), []byte(group), 0o644); err != nil {
		h.t.Fatal(err)
	}
	key := filepath.Join(home, ".ssh", "id_ed25519")
	for _, f := range []string{key, key + ".pub"} {
		if err := os.WriteFile(f, []byte("clé simulée\n"), 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
}

func TestInitIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	for _, f := range []string{
		filepath.Join("etc", "ogs", "config.yaml"),
		filepath.Join("etc", "ogs", "master.key"),
		filepath.Join("var", "lib", "ogs", "state.db"),
	} {
		if _, err := os.Stat(h.path(f)); err != nil {
			t.Errorf("%s absent après init : %v", f, err)
		}
	}
	info, err := os.Stat(h.path("etc", "ogs", "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o400 {
		t.Errorf("clé maître en mode %04o, attendu 0400", info.Mode().Perm())
	}
	// Rejouer init ne doit rien casser : le postinst du paquet l'appelle.
	before, err := os.ReadFile(h.path("etc", "ogs", "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	h.mustRun("init")
	after, _ := os.ReadFile(h.path("etc", "ogs", "master.key"))
	if string(before) != string(after) {
		t.Error("init a régénéré la clé maître : les secrets existants deviendraient illisibles")
	}
}

func TestFullLifecycle(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")

	// 1. Déclaration : rien n'est appliqué au système.
	out := h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4")
	if !strings.Contains(out, "shopfr") {
		t.Errorf("sortie de création inattendue :\n%s", out)
	}
	if _, err := os.Stat(h.path("home", "shop.fr")); !os.IsNotExist(err) {
		t.Error("« project create » ne doit rien créer sur le système")
	}

	// 2. Le plan énumère les créations.
	plan := h.mustRun("plan", "shopfr")
	for _, want := range []string{"groupe shopfr", "utilisateur shopfr", "www/public"} {
		if !strings.Contains(plan, want) {
			t.Errorf("le plan devrait mentionner %q :\n%s", want, plan)
		}
	}

	// 3. --dry-run n'écrit rien.
	if _, code := h.run("apply", "shopfr", "--dry-run", "--yes"); code != CodeOK {
		t.Fatalf("dry-run en échec (code %d)", code)
	}
	if _, err := os.Stat(h.path("home", "shop.fr")); !os.IsNotExist(err) {
		t.Error("--dry-run a écrit sur le disque")
	}

	// 4. Application.
	h.mustRun("apply", "shopfr", "--yes")
	modes := map[string]os.FileMode{
		"":           0o750,
		"www":        0o750,
		"www/public": 0o750,
		"var":        0o700,
		"logs":       0o750,
		".bin":       0o750,
		".ssh":       0o700,
	}
	for rel, want := range modes {
		info, err := os.Stat(h.path("home", "shop.fr", rel))
		if err != nil {
			t.Fatalf("%s absent : %v", rel, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s : mode %04o, attendu %04o", rel, got, want)
		}
	}

	// 5. Idempotence, une fois les comptes réputés créés.
	h.simulateSystemEffects("shopfr", "shop.fr", 1004)
	out = h.mustRun("apply", "shopfr", "--yes")
	if !strings.Contains(out, "aucun changement") {
		t.Errorf("un apply rejoué doit être vide :\n%s", out)
	}
	if out, code := h.run("diff", "shopfr"); code != CodeOK {
		t.Errorf("diff sur un système conforme : code %d\n%s", code, out)
	}

	// 6. Dérive : détectée par diff, corrigée par apply.
	logs := h.path("home", "shop.fr", "logs")
	if err := os.Chmod(logs, 0o777); err != nil {
		t.Fatal(err)
	}
	out, code := h.run("diff", "shopfr")
	if code != CodePartial {
		t.Errorf("une dérive doit donner le code %d, obtenu %d\n%s", CodePartial, code, out)
	}
	if !strings.Contains(out, "0777 → 0750") {
		t.Errorf("le diff doit expliciter l'écart :\n%s", out)
	}
	h.mustRun("apply", "shopfr", "--yes")
	info, _ := os.Stat(logs)
	if info.Mode().Perm() != 0o750 {
		t.Errorf("mode non corrigé : %04o", info.Mode().Perm())
	}
}

func TestCreateRejectsDuplicate(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr")
	out, code := h.run("project", "create", "shop.fr")
	if code != CodeConflict {
		t.Errorf("un projet déjà déclaré doit donner le code %d, obtenu %d\n%s", CodeConflict, code, out)
	}
}

func TestCreateRejectsConvergentIdentifiers(t *testing.T) {
	// mon-shop.fr et monshop.fr convergent vers le même identifiant : le conflit
	// doit être refusé au moment de la déclaration.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "mon-shop.fr")
	out, code := h.run("project", "create", "monshop.fr")
	if code != CodeConflict {
		t.Errorf("collision d'identifiant : code %d attendu %d\n%s", code, CodeConflict, out)
	}
}

func TestPrepEnvironmentUsesDedicatedDomain(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony")
	h.mustRun("project", "create", "shop.fr", "--env", "prep", "--kind", "php-symfony")
	out := h.mustRun("project", "show", "shopfrprep")
	for _, want := range []string{"shopfrprep", "shopfr.preprod-pp.net"} {
		if !strings.Contains(out, want) {
			t.Errorf("la préproduction devrait mentionner %q :\n%s", want, out)
		}
	}
	// Les deux environnements cohabitent sans partager de ressource.
	h.mustRun("apply", "--yes")
	for _, home := range []string{"shop.fr", "shopfr.preprod-pp.net"} {
		if _, err := os.Stat(h.path("home", home)); err != nil {
			t.Errorf("home %s absent : %v", home, err)
		}
	}
}

func TestSymfonyLegacyDocroot(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "ancien.fr", "--kind", "php-symfony", "--app-version", "3.4")
	out := h.mustRun("project", "show", "ancienfr")
	if !strings.Contains(out, filepath.Join("www", "web")) {
		t.Errorf("Symfony 3.4 doit servir www/web :\n%s", out)
	}
	h.mustRun("apply", "ancienfr", "--yes")
	if _, err := os.Stat(h.path("home", "ancien.fr", "www", "web")); err != nil {
		t.Errorf("le docroit www/web devrait exister : %v", err)
	}
}

func TestPrestashopDocroot(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "boutique.fr", "--kind", "php-prestashop", "--php", "7.4")
	out := h.mustRun("project", "show", "boutiquefr")
	if strings.Contains(out, filepath.Join("www", "public")) {
		t.Errorf("Prestashop doit servir www, pas www/public :\n%s", out)
	}
}

func TestJSONOutputIsWellFormed(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr")
	out := h.mustRun("project", "list", "-o", "json")
	var env struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Items      []struct {
			ID   string `json:"id"`
			Home string `json:"home"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("JSON invalide : %v\n%s", err, out)
	}
	if env.APIVersion != "ogs/v1" || env.Kind != "ProjectList" {
		t.Errorf("enveloppe : %+v", env)
	}
	if len(env.Items) != 1 || env.Items[0].ID != "shopfr" {
		t.Errorf("items : %+v", env.Items)
	}
}

func TestUnknownKindIsUsageError(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	out, code := h.run("project", "create", "shop.fr", "--kind", "php-drupal")
	if code != CodeUsage {
		t.Errorf("type inconnu : code %d attendu %d\n%s", code, CodeUsage, out)
	}
	if !strings.Contains(out, "php-prestashop") {
		t.Errorf("l'erreur devrait énumérer les types connus :\n%s", out)
	}
}

func TestNonInteractiveRequiresYes(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr")
	out, code := h.run("apply", "shopfr", "--non-interactive")
	if code != CodeCancelled {
		t.Errorf("sans --yes en mode non interactif : code %d attendu %d\n%s", code, CodeCancelled, out)
	}
}

func TestDoctorReportsMissingInit(t *testing.T) {
	h := newHarness(t)
	// Sans init, les répertoires manquent : doctor doit le dire et proposer la
	// correction, avec un code de retour non nul.
	out, code := h.run("doctor")
	if code == CodeOK {
		t.Errorf("doctor devrait échouer avant init :\n%s", out)
	}
	if !strings.Contains(out, "ogs init") {
		t.Errorf("doctor doit proposer la correction :\n%s", out)
	}
}

func TestVersionAndLicence(t *testing.T) {
	h := newHarness(t)
	if out := h.mustRun("version"); !strings.Contains(out, "ogs") {
		t.Errorf("version : %q", out)
	}
	out := h.mustRun("version", "--license")
	for _, want := range []string{"WTFPL", "ABSENCE DE GARANTIE"} {
		if !strings.Contains(out, want) {
			t.Errorf("la licence doit mentionner %q :\n%s", want, out)
		}
	}
}

// --- Lot L1 : PHP, serveur web, rotation ---

func TestPrestashopAndSymfonyCoexist(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "boutique.fr", "--kind", "php-prestashop", "--php", "7.4")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "8.0", "--php", "8.5")
	h.mustRun("apply", "--yes")

	// Deux pools, deux versions, deux numéros suivant la norme.
	boutique := h.read(t, filepath.Join("etc", "php", "7.4", "fpm", "pool.d", "boutiquefr.conf"))
	shop := h.read(t, filepath.Join("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf"))
	if !strings.Contains(boutique, "php7.4-fpm-17401-boutiquefr.sock") {
		t.Errorf("numéro attendu 17401 pour le premier pool en 7.4 :\n%s", boutique)
	}
	if !strings.Contains(shop, "php8.5-fpm-18501-shopfr.sock") {
		t.Errorf("numéro attendu 18501 pour le premier pool en 8.5 :\n%s", shop)
	}
	// Chaque pool tourne sous son propre utilisateur : c'est l'isolation.
	if !strings.Contains(boutique, "user = boutiquefr") || !strings.Contains(shop, "user = shopfr") {
		t.Error("chaque pool doit tourner sous l'utilisateur de son projet")
	}
	// Réglages propres à Prestashop, absents du projet Symfony.
	if !strings.Contains(boutique, "max_input_vars] = 5000") {
		t.Error("les réglages Prestashop doivent être appliqués")
	}
	if strings.Contains(shop, "max_input_vars] = 5000") {
		t.Error("les réglages Prestashop ne doivent pas fuir vers Symfony")
	}
	// Docroots distincts selon le type d'application.
	vhBoutique := h.read(t, filepath.Join("etc", "apache2", "sites-available", "boutiquefr.conf"))
	vhShop := h.read(t, filepath.Join("etc", "apache2", "sites-available", "shopfr.conf"))
	if !strings.Contains(vhBoutique, filepath.Join("boutique.fr", "www")+"\n") {
		t.Errorf("Prestashop sert www :\n%s", vhBoutique)
	}
	if !strings.Contains(vhShop, filepath.Join("shop.fr", "www", "public")) {
		t.Errorf("Symfony sert www/public :\n%s", vhShop)
	}
	if !strings.Contains(vhBoutique, "AllowOverride All") || !strings.Contains(vhShop, "AllowOverride None") {
		t.Error("AllowOverride doit suivre le type d'application")
	}
}

func TestLogrotateGathersProjectsInOneBlock(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "boutique.fr", "--kind", "php-prestashop", "--php", "7.4")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5")
	h.mustRun("apply", "--yes")

	rot := h.read(t, filepath.Join("etc", "logrotate.d", "ogs"))
	// Un seul rechargement pour tout le parc : sans sharedscripts, cent projets
	// provoqueraient cent rechargements.
	if n := strings.Count(rot, "postrotate"); n != 1 {
		t.Errorf("%d blocs postrotate, 1 attendu :\n%s", n, rot)
	}
	if !strings.Contains(rot, "sharedscripts") {
		t.Error("sharedscripts est indispensable")
	}
	// « create » sans propriétaire : chaque fichier est recréé avec le sien, ce qui
	// permet à un bloc unique de couvrir des projets appartenant à des
	// utilisateurs différents.
	for _, line := range strings.Split(rot, "\n") {
		if strings.TrimSpace(line) != "" && strings.HasPrefix(strings.TrimSpace(line), "create ") {
			t.Errorf("« create » ne doit pas fixer de propriétaire : %q", line)
		}
	}
	// L'unité rechargée est bien apache2, pas « apache ».
	if !strings.Contains(rot, "systemctl reload apache2 php7.4-fpm php8.5-fpm") {
		t.Errorf("unités de rechargement inattendues :\n%s", rot)
	}
	// Aucun glob sur /home : il embarquerait les répertoires d'utilisateurs humains.
	if strings.Contains(rot, "/home/*/") {
		t.Error("les chemins doivent être énumérés, pas globalisés")
	}
	for _, want := range []string{"boutique.fr/logs/php-error.log", "shop.fr/logs/apache-access.log"} {
		if !strings.Contains(rot, want) {
			t.Errorf("chemin absent de la rotation : %s", want)
		}
	}
}

func TestPHPSwitchRefusesIncompatibleVersion(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "ancien.fr", "--kind", "php-symfony", "--app-version", "3.4", "--php", "7.4")
	h.mustRun("apply", "ancienfr", "--yes")

	// Contrainte réelle d'un Symfony 3.4 : il ne fonctionne pas sur PHP 8.
	composer := filepath.Join(h.path("home", "ancien.fr", "www"), "composer.json")
	if err := os.WriteFile(composer, []byte(`{"require":{"php":"^5.5.9|>=7.0.8 <8.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := h.run("php", "switch", "ancienfr", "8.3")
	if code != CodeValidation {
		t.Fatalf("la bascule incompatible doit être refusée (code %d attendu %d) :\n%s", code, CodeValidation, out)
	}
	if !strings.Contains(out, "composer.json") {
		t.Errorf("le refus doit citer la source de la contrainte :\n%s", out)
	}
	// La version déclarée n'a pas changé.
	shown := h.mustRun("project", "show", "ancienfr")
	if !strings.Contains(shown, "7.4") {
		t.Errorf("le projet ne doit pas avoir été modifié :\n%s", shown)
	}
	// --force passe outre, en le consignant.
	out = h.mustRun("php", "switch", "ancienfr", "8.3", "--force")
	if !strings.Contains(out, "forcée") {
		t.Errorf("le forçage doit être signalé :\n%s", out)
	}
}

func TestPHPSwitchRewritesVHostAndRemovesOldPoolLast(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("apply", "shopfr", "--yes")

	h.mustRun("php", "switch", "shopfr", "8.3")
	// L'ordre du plan est structurant : le nouveau pool, puis le vhost, puis la
	// suppression de l'ancien pool. Supprimer d'abord exposerait un 502.
	plan := h.mustRun("plan", "shopfr")
	iNew := strings.Index(plan, "pool php 8.3")
	iVHost := strings.Index(plan, "vhost apache shopfr")
	iOld := strings.Index(plan, "pool php obsolète")
	if iNew < 0 || iVHost < 0 || iOld < 0 {
		t.Fatalf("plan de bascule incomplet :\n%s", plan)
	}
	if !(iNew < iVHost && iVHost < iOld) {
		t.Errorf("ordre incorrect (nouveau=%d vhost=%d ancien=%d) :\n%s", iNew, iVHost, iOld, plan)
	}

	h.mustRun("apply", "shopfr", "--yes")
	vhost := h.read(t, filepath.Join("etc", "apache2", "sites-available", "shopfr.conf"))
	if !strings.Contains(vhost, "php8.3-fpm-18301-shopfr.sock") {
		t.Errorf("le vhost doit pointer sur le nouveau socket :\n%s", vhost)
	}
	if strings.Contains(vhost, "php8.5") {
		t.Error("aucune référence à l'ancienne version ne doit subsister")
	}
	if _, err := os.Stat(h.path("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf")); !os.IsNotExist(err) {
		t.Error("l'ancien pool doit avoir été supprimé")
	}
	// Le lien d'interpréteur suit la version.
	target, err := os.Readlink(h.path("home", "shop.fr", ".bin", "php"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(target, "php8.3") {
		t.Errorf("lien .bin/php : %s", target)
	}
}

func TestPHPListAndPorts(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5")
	h.mustRun("apply", "shopfr", "--yes")

	ports := h.mustRun("php", "ports")
	if !strings.Contains(ports, "18501") || !strings.Contains(ports, "shopfr") {
		t.Errorf("les numéros attribués doivent être listés :\n%s", ports)
	}
	list := h.mustRun("php", "list")
	if !strings.Contains(list, "8.5") {
		t.Errorf("la version utilisée doit être détectée :\n%s", list)
	}
}

func TestTemplateExtractChangesRendering(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	out := h.mustRun("template", "extract", "php/pool.conf")
	if !strings.Contains(out, "templates") {
		t.Errorf("l'extraction doit indiquer le chemin :\n%s", out)
	}
	// La surcharge remplace la version embarquée.
	override := h.path("etc", "ogs", "templates", "php", "pool.conf.tmpl")
	if err := os.WriteFile(override, []byte("[{{ .Name }}]\n; surcharge locale\nuser = {{ .User }}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5")
	h.mustRun("apply", "shopfr", "--yes")
	pool := h.read(t, filepath.Join("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf"))
	if !strings.Contains(pool, "surcharge locale") {
		t.Errorf("la surcharge doit être employée :\n%s", pool)
	}
	// L'en-tête doit désigner le fichier à éditer.
	if !strings.Contains(pool, override) {
		t.Errorf("l'en-tête doit renvoyer vers la surcharge :\n%s", pool)
	}
}

func TestWebEngineReportsChoice(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	out := h.mustRun("web", "engine")
	if !strings.Contains(out, "apache") {
		t.Errorf("le moteur retenu doit être indiqué :\n%s", out)
	}
}

func TestUserKeyShowRequiresGeneratedKey(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	// En mode préfixé, ssh-keygen n'est pas exécuté : la commande doit le dire
	// clairement plutôt que d'échouer sur une erreur de fichier.
	out, code := h.run("user", "key", "show", "shopfr")
	if code != CodeUsage {
		t.Errorf("code %d attendu %d :\n%s", code, CodeUsage, out)
	}
	if !strings.Contains(out, "ogs apply") {
		t.Errorf("le message doit indiquer comment obtenir la clé :\n%s", out)
	}
	// Une fois la clé présente, elle est restituée.
	h.mustRun("apply", "shopfr", "--yes")
	key := h.path("home", "shop.fr", ".ssh", "id_ed25519.pub")
	if err := os.WriteFile(key, []byte("ssh-ed25519 AAAAC3Nza ogs=shopfr@test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = h.mustRun("user", "key", "show", "shopfr")
	if !strings.Contains(out, "ssh-ed25519") {
		t.Errorf("la clé publique doit être affichée :\n%s", out)
	}
	if strings.Contains(out, "PRIVATE") {
		t.Error("la clé privée ne doit jamais être exposée")
	}
}

// --- Lot L2 : bases de données et secrets ---

func TestDatabaseDeclarationAndUnavailableEngine(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5", "--db", "mysql")

	// La déclaration porte le nom du projet (D23).
	shown := h.mustRun("project", "show", "shopfr")
	if !strings.Contains(shown, "mysql shopfr") {
		t.Errorf("la base doit porter le nom du projet :\n%s", shown)
	}

	// Aucun serveur sous le préfixe : l'état doit le dire sans faire échouer la
	// commande de consultation.
	list := h.mustRun("db", "list")
	if !strings.Contains(list, "moteur indisponible") {
		t.Errorf("l'indisponibilité doit être rapportée :\n%s", list)
	}

	// Et surtout : un moteur injoignable ne doit pas empêcher la création du site.
	out, code := h.run("apply", "shopfr", "--yes")
	if code != CodePartial {
		t.Fatalf("code %d attendu %d (succès partiel) :\n%s", code, CodePartial, out)
	}
	if !strings.Contains(out, "⚠") {
		t.Errorf("l'échec non critique doit être signalé :\n%s", out)
	}
	// Le reste du site est bien en place.
	for _, rel := range []string{
		filepath.Join("home", "shop.fr", "www", "public"),
		filepath.Join("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf"),
		filepath.Join("etc", "apache2", "sites-available", "shopfr.conf"),
	} {
		if _, err := os.Stat(h.path(rel)); err != nil {
			t.Errorf("%s devrait exister malgré l'indisponibilité de la base : %v", rel, err)
		}
	}
}

func TestDatabaseAddRejectsDuplicateAndUnknownEngine(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("db", "add", "shopfr", "--engine", "postgres")
	if out, code := h.run("db", "add", "shopfr", "--engine", "postgres"); code != CodeConflict {
		t.Errorf("une base déjà déclarée doit donner le code %d, obtenu %d :\n%s", CodeConflict, code, out)
	}
	if out, code := h.run("db", "add", "shopfr", "--engine", "oracle"); code != CodeUsage {
		t.Errorf("moteur inconnu : code %d attendu %d :\n%s", code, CodeUsage, out)
	}
	// Une base additionnelle porte un suffixe de rôle.
	h.mustRun("db", "add", "shopfr", "--engine", "postgres", "--role", "analytics")
	shown := h.mustRun("project", "show", "shopfr")
	if !strings.Contains(shown, "shopfr_analytics") {
		t.Errorf("la base additionnelle doit être suffixée :\n%s", shown)
	}
}

func TestDatabaseRemoveKeepsData(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static", "--db", "mysql")
	out := h.mustRun("db", "remove", "shopfr", "shopfr")
	if !strings.Contains(out, "subsistent") {
		t.Errorf("le retrait d'une déclaration ne détruit rien, et doit le dire :\n%s", out)
	}
	if out, code := h.run("db", "remove", "shopfr", "shopfr"); code != CodeUsage {
		t.Errorf("retirer une base absente : code %d attendu %d :\n%s", code, CodeUsage, out)
	}
}

func TestSecretsListShowAndAudit(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static", "--db", "mysql")

	// Aucun secret avant application.
	out := h.mustRun("secrets", "list", "shopfr")
	if !strings.Contains(out, "aucun secret") {
		t.Errorf("liste initiale :\n%s", out)
	}

	// Un secret scellé à la main, comme le ferait l'application d'une base.
	st, err := store.Open(h.path("var", "lib", "ogs", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	ks, err := secret.Open(h.path("etc", "ogs", "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	nonce, ciphertext, err := ks.Seal("shopfr", "mysql/shopfr", []byte("mot-de-passe-scelle"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSecret("shopfr", "mysql/shopfr", nonce, ciphertext); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// La liste n'expose que les noms.
	out = h.mustRun("secrets", "list", "shopfr")
	if !strings.Contains(out, "mysql/shopfr") {
		t.Errorf("le nom du secret doit apparaître :\n%s", out)
	}
	if strings.Contains(out, "mot-de-passe-scelle") {
		t.Error("la liste ne doit jamais exposer une valeur")
	}

	// show est la seule voie de divulgation, et elle est journalisée.
	out = h.mustRun("secrets", "show", "shopfr", "mysql/shopfr")
	if !strings.Contains(out, "mot-de-passe-scelle") {
		t.Errorf("la valeur doit être restituée :\n%s", out)
	}
	audit := h.read(t, filepath.Join("var", "log", "ogs", "audit.log"))
	if !strings.Contains(audit, "secrets.show") {
		t.Errorf("la consultation doit être consignée :\n%s", audit)
	}
	if strings.Contains(audit, "mot-de-passe-scelle") {
		t.Error("l'audit ne doit jamais contenir la valeur du secret")
	}
	// Un secret inconnu produit une erreur d'usage, pas une trace de pile.
	if _, code := h.run("secrets", "show", "shopfr", "mysql/absent"); code != CodeUsage {
		t.Errorf("secret inconnu : code %d attendu %d", code, CodeUsage)
	}
}

func TestSecretsEnvNeedsAppliedDatabase(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	// Sans base ni transport déclarés, il n'y a rien à émettre.
	out := h.mustRun("secrets", "env", "shopfr")
	if !strings.Contains(out, "ni base ni transport") {
		t.Errorf("sortie inattendue :\n%s", out)
	}
	// Avec une base déclarée mais un moteur injoignable, le motif doit être clair.
	h.mustRun("db", "add", "shopfr", "--engine", "mysql")
	out, code := h.run("secrets", "env", "shopfr")
	if code != CodeDependency {
		t.Errorf("code %d attendu %d :\n%s", code, CodeDependency, out)
	}
}

func TestPlanShowsSQLWithoutSecrets(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static", "--db", "mysql")
	// Un écart existe (la base n'est pas créée), donc le code 6 est attendu : c'est
	// le contrat de « diff », utilisable en supervision.
	out, code := h.run("diff", "shopfr")
	if code != CodePartial {
		t.Fatalf("code %d attendu %d :\n%s", code, CodePartial, out)
	}
	if strings.Contains(out, "IDENTIFIED BY '") && !strings.Contains(out, "mot de passe généré") {
		t.Errorf("le plan ne doit pas exposer de mot de passe :\n%s", out)
	}
}
