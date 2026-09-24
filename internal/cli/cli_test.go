package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/model"
	runtimeprov "leblanc.io/open-go-server/internal/provider/runtime"
	"leblanc.io/open-go-server/internal/secret"
	"leblanc.io/open-go-server/internal/store"
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

// mustRunPartial accepte un succès partiel : sur cette machine, MySQL n'est pas
// installé, donc la création d'une base produit un avertissement légitime.
func (h *harness) mustRunPartial(args ...string) string {
	h.t.Helper()
	out, code := h.run(args...)
	if code != CodeOK && code != CodePartial {
		h.t.Fatalf("« ogs %s » a échoué (code %d) :\n%s", strings.Join(args, " "), code, out)
	}
	return out
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
	passwd := fmt.Sprintf("%s:x:%d:%d:ogs=%s:%s:%s\n", id, uid, uid, id, home, model.DefaultShell)
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

func TestProjectShellComesFromConfig(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")

	// Le défaut du parc : un shell ouvert (D62).
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	if spec := h.read(t, filepath.Join("etc", "ogs", "projects", "shopfr.yaml")); !strings.Contains(
		spec, "shell: "+model.DefaultShell) {
		t.Errorf("le projet devrait porter le shell par défaut :\n%s", spec)
	}

	// Fermé dans la configuration, le suivant le reçoit fermé — et le premier ne
	// bouge pas : le fichier d'un projet existant fait autorité, sans quoi un
	// changement de configuration réécrirait le shell de tout le parc.
	cfg := h.path("etc", "ogs", "config.yaml")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// « init » écrit la clé : la fermeture du shell se fait en éditant sa valeur,
	// ce qui vaut mieux qu'un réglage à devenir.
	setShell := func(shell string) {
		out := strings.Replace(string(data), "shell: "+model.DefaultShell, "shell: "+shell, 1)
		if out == string(data) {
			t.Fatalf("la clé user.shell devrait figurer dans la configuration générée :\n%s", data)
		}
		if err := os.WriteFile(cfg, []byte(out), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setShell("/usr/sbin/nologin")
	h.mustRun("project", "create", "autre.fr", "--kind", "static")
	if spec := h.read(t, filepath.Join("etc", "ogs", "projects", "autrefr.yaml")); !strings.Contains(
		spec, "shell: /usr/sbin/nologin") {
		t.Errorf("le shell configuré devrait s'appliquer :\n%s", spec)
	}
	if spec := h.read(t, filepath.Join("etc", "ogs", "projects", "shopfr.yaml")); !strings.Contains(
		spec, "shell: "+model.DefaultShell) {
		t.Errorf("le projet existant ne doit pas suivre le changement de configuration :\n%s", spec)
	}

	// Un shell relatif est refusé au chargement, non à l'application.
	setShell("bash")
	out, code := h.run("plan", "shopfr")
	if code == CodeOK {
		t.Errorf("un shell relatif doit être refusé :\n%s", out)
	}
	if !strings.Contains(out, "user.shell") {
		t.Errorf("le message doit nommer la clé fautive :\n%s", out)
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
	// Le home ne se traverse que ; les répertoires ne s'énumèrent pas. Le groupe
	// n'obtient qu'un droit de passage, ce qui suffit à servir un fichier.
	modes := map[string]os.FileMode{
		"":           0o711,
		"www":        0o710,
		"www/public": 0o710,
		"var":        0o700,
		".local":     0o700,
		".local/bin": 0o700,
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
	// Les journaux vivent hors du home (D71) : ~/logs n'en est qu'un lien.
	logDir := h.path("var", "log", "ogs-projects", "shopfr")
	if info, err := os.Stat(logDir); err != nil || info.Mode().Perm() != 0o750 {
		t.Errorf("répertoire des journaux attendu en 0750 : %v", err)
	}
	if target, err := os.Readlink(h.path("home", "shop.fr", "logs")); err != nil || target != logDir {
		t.Errorf("~/logs doit désigner %s, obtenu %q (%v)", logDir, target, err)
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
	logs := h.path("home", "shop.fr", "var")
	if err := os.Chmod(logs, 0o777); err != nil {
		t.Fatal(err)
	}
	out, code := h.run("diff", "shopfr")
	if code != CodePartial {
		t.Errorf("une dérive doit donner le code %d, obtenu %d\n%s", CodePartial, code, out)
	}
	if !strings.Contains(out, "0777 → 0700") {
		t.Errorf("le diff doit expliciter l'écart :\n%s", out)
	}
	h.mustRun("apply", "shopfr", "--yes")
	info, _ := os.Stat(logs)
	if info.Mode().Perm() != 0o700 {
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
	for _, want := range []string{"shopfrprep", "shop.preprod-pp.net"} {
		if !strings.Contains(out, want) {
			t.Errorf("la préproduction devrait mentionner %q :\n%s", want, out)
		}
	}
	// Les deux environnements cohabitent sans partager de ressource.
	h.mustRun("apply", "--yes")
	for _, home := range []string{"shop.fr", "shop.preprod-pp.net"} {
		if _, err := os.Stat(h.path("home", home)); err != nil {
			t.Errorf("home %s absent : %v", home, err)
		}
	}
}

// Un host déjà posé sur le domaine de préproduction est pris tel quel : le
// construire une seconde fois donnait le host jardindecopreprodppnet.preprod-pp.net,
// le home du même nom et l'utilisateur jardindecopreprodppnetprep.
func TestPrepHostAlreadyOnPrepDomainIsTakenAsIs(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "jardindeco.preprod-pp.net", "--env", "prep", "--kind", "php-symfony")
	out := h.mustRun("project", "show", "jardindecoprep")
	for _, want := range []string{"jardindecoprep", "jardindeco.preprod-pp.net"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q attendu :\n%s", want, out)
		}
	}
	if strings.Contains(out, "jardindecopreprodppnet") {
		t.Errorf("le domaine de préproduction ne doit pas être dérivé une seconde fois :\n%s", out)
	}
	h.mustRun("apply", "--yes")
	if _, err := os.Stat(h.path("home", "jardindeco.preprod-pp.net")); err != nil {
		t.Errorf("home absent : %v", err)
	}
}

// En production, le domaine de préproduction est réservé (§3.3.6).
func TestProdHostOnPrepDomainRefused(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	out, code := h.run("project", "create", "jardindeco.preprod-pp.net")
	if code == CodeOK {
		t.Errorf("un host de production sur le domaine de préproduction doit être refusé :\n%s", out)
	}
	if !strings.Contains(out, "réservé") {
		t.Errorf("le message doit dire pourquoi :\n%s", out)
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
	for _, want := range []string{"ogs-projects/boutiquefr/php-error.log", "ogs-projects/shopfr/apache-access.log"} {
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
	target, err := os.Readlink(h.path("home", "shop.fr", ".local", "bin", "php"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(target, "php8.3") {
		t.Errorf("lien .local/bin/php : %s", target)
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

// --- Lot L3 : certificats TLS ---

func TestCertificateIssuedThenVHostSwitchesToTLS(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5")

	// L'ordre du plan est structurant : le vhost HTTP doit répondre avant le
	// challenge, et la bascule TLS ne peut venir qu'après l'émission.
	plan := h.mustRun("plan", "shopfr")
	iVHost := strings.Index(plan, "vhost apache shopfr")
	iCert := strings.Index(plan, "certificat shopfr")
	iTLS := strings.Index(plan, "activation TLS")
	if iVHost < 0 || iCert < 0 || iTLS < 0 {
		t.Fatalf("plan incomplet :\n%s", plan)
	}
	if !(iVHost < iCert && iCert < iTLS) {
		t.Errorf("ordre incorrect (vhost=%d cert=%d tls=%d) :\n%s", iVHost, iCert, iTLS, plan)
	}
	// L'émission est non bloquante : un site en HTTP vaut mieux qu'un site absent.
	if !strings.Contains(plan, "certificat shopfr pour shop.fr (http-01)  (non bloquant)") {
		t.Errorf("l'émission doit être annoncée non bloquante :\n%s", plan)
	}

	h.mustRun("apply", "shopfr", "--yes")
	vhost := h.read(t, filepath.Join("etc", "apache2", "sites-available", "shopfr.conf"))
	if !strings.Contains(vhost, "VirtualHost *:443") {
		t.Errorf("le vhost doit avoir basculé en TLS :\n%s", vhost)
	}
	if !strings.Contains(vhost, "Strict-Transport-Security") {
		t.Error("HSTS doit être posé sur le vhost chiffré")
	}
	// Le challenge reste servi avant la redirection, sinon aucun renouvellement
	// ne pourrait aboutir.
	iChallenge := strings.Index(vhost, "acme-challenge")
	iRedirect := strings.Index(vhost, "RewriteRule")
	if iChallenge < 0 || iRedirect < 0 || iChallenge > iRedirect {
		t.Errorf("le challenge doit précéder la redirection :\n%s", vhost)
	}
	// Et le certificat existe réellement, avec une clé privée protégée.
	key := h.path("var", "lib", "ogs", "acme", "certificates", "shopfr", "privkey.pem")
	info, err := os.Stat(key)
	if err != nil {
		t.Fatalf("clé privée absente : %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("clé privée en %04o, attendu 0600", info.Mode().Perm())
	}
}

func TestPrepSharesWildcardCertificate(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--env", "prep", "--kind", "php-symfony", "--php", "8.5")
	h.mustRun("apply", "--yes")

	list := h.mustRun("cert", "list")
	if !strings.Contains(list, "@wildcard-prep") {
		t.Errorf("le wildcard partagé doit être émis :\n%s", list)
	}
	// Aucun certificat propre à la préproduction : c'est tout l'intérêt du
	// partage, une création de préprod ne déclenche aucune émission.
	if strings.Contains(list, "shopfrprep") {
		t.Errorf("la préproduction ne doit pas avoir son propre certificat :\n%s", list)
	}
	vhost := h.read(t, filepath.Join("etc", "apache2", "sites-available", "shopfrprep.conf"))
	if !strings.Contains(vhost, "_shared-wildcard-prep") {
		t.Errorf("le vhost doit référencer le certificat partagé :\n%s", vhost)
	}
	// Et l'interdiction d'indexation reste en place.
	if !strings.Contains(vhost, "noindex") {
		t.Error("une préproduction ne doit pas être indexable")
	}
}

func TestCertDeleteRefusesReferencedShared(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--env", "prep", "--kind", "static")
	h.mustRun("apply", "--yes")
	out, code := h.run("cert", "delete", "@wildcard-prep")
	if code != CodeConflict {
		t.Fatalf("code %d attendu %d :\n%s", code, CodeConflict, out)
	}
	if !strings.Contains(out, "shopfrprep") {
		t.Errorf("le refus doit nommer les projets concernés :\n%s", out)
	}
}

func TestCertRenewSaysNothingToDo(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "--yes")
	out := h.mustRun("cert", "renew", "--all")
	if !strings.Contains(out, "aucun certificat n'arrive à échéance") {
		t.Errorf("un certificat frais ne doit pas être renouvelé :\n%s", out)
	}
	// --force renouvelle malgré tout.
	out = h.mustRun("cert", "renew", "--all", "--force")
	if !strings.Contains(out, "renouvelé") {
		t.Errorf("--force doit renouveler :\n%s", out)
	}
}

func TestCertIssueSelfSignedAndAudit(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	out := h.mustRun("cert", "issue", "shopfr", "--self-signed")
	if !strings.Contains(out, "valable jusqu'au") {
		t.Errorf("sortie inattendue :\n%s", out)
	}
	audit := h.read(t, filepath.Join("var", "log", "ogs", "audit.log"))
	if !strings.Contains(audit, "cert.issue") {
		t.Errorf("l'émission doit être consignée :\n%s", audit)
	}
	// Un certificat auto-signé n'a rien à révoquer auprès d'une autorité.
	if out, code := h.run("cert", "revoke", "shopfr"); code != CodeUsage {
		t.Errorf("code %d attendu %d :\n%s", code, CodeUsage, out)
	}
}

func TestCertIssueOnPrepRedirectsToSharedCertificate(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--env", "prep", "--kind", "static")
	out, code := h.run("cert", "issue", "shopfrprep")
	if code != CodeUsage {
		t.Fatalf("code %d attendu %d :\n%s", code, CodeUsage, out)
	}
	if !strings.Contains(out, "@wildcard-prep") {
		t.Errorf("le message doit orienter vers le certificat partagé :\n%s", out)
	}
}

func TestCertDNSProviderSet(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	// Identifiants incomplets : refus explicite plutôt qu'un échec au premier
	// renouvellement.
	if out, code := h.run("cert", "dns-provider", "set", "ovh", "--application-key", "x"); code != CodeUsage {
		t.Errorf("code %d attendu %d :\n%s", code, CodeUsage, out)
	}
	if out, code := h.run("cert", "dns-provider", "set", "cloudflare"); code != CodeUsage {
		t.Errorf("provider non géré : code %d attendu %d :\n%s", code, CodeUsage, out)
	}
	h.mustRun("cert", "dns-provider", "set", "ovh",
		"--application-key", "ak", "--application-secret", "as", "--consumer-key", "ck")
	path := h.path("etc", "ogs", "credentials", "dns", "ovh.yaml")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Ces identifiants donnent un droit d'écriture sur la zone entière.
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %04o, attendu 0600", info.Mode().Perm())
	}
	content := h.read(t, filepath.Join("etc", "ogs", "credentials", "dns", "ovh.yaml"))
	if !strings.Contains(content, "applicationKey: ak") || !strings.Contains(content, "endpoint: ovh-eu") {
		t.Errorf("contenu inattendu :\n%s", content)
	}
}

func TestRenewTimerIsInstalled(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "--yes")
	timer := h.read(t, filepath.Join("etc", "systemd", "system", "ogs-renew.timer"))
	if !strings.Contains(timer, "OnCalendar=*-*-* 03,15:00:00") {
		t.Errorf("cadence inattendue :\n%s", timer)
	}
	// L'étalement évite que cent serveurs sollicitent l'autorité à la même seconde.
	if !strings.Contains(timer, "RandomizedDelaySec=3600") {
		t.Error("l'étalement aléatoire est nécessaire")
	}
	if !strings.Contains(timer, "Persistent=true") {
		t.Error("une fenêtre manquée doit être rattrapée")
	}
	service := h.read(t, filepath.Join("etc", "systemd", "system", "ogs-renew.service"))
	if !strings.Contains(service, "cert renew --all") {
		t.Errorf("commande inattendue :\n%s", service)
	}
	// Le code 6 (succès partiel) ne doit pas marquer l'unité en échec.
	if !strings.Contains(service, "SuccessExitStatus=0 6") {
		t.Error("un renouvellement partiel ne doit pas faire échouer l'unité")
	}
}

func TestDoctorWarnsOnExpiringCertificate(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "--yes")
	// Un certificat frais ne doit pas produire d'avertissement TLS.
	out, _ := h.run("doctor")
	if strings.Contains(out, "expire dans") {
		t.Errorf("aucun avertissement attendu pour un certificat neuf :\n%s", out)
	}
	if !strings.Contains(out, "certificats") {
		t.Errorf("doctor doit rendre compte des certificats :\n%s", out)
	}
}

// --- Lot L4 : RabbitMQ ---

// fakeBroker démarre un serveur imitant l'API management et enregistre les
// identifiants sous le préfixe. Cela permet d'éprouver le câblage complet, de la
// commande jusqu'aux requêtes HTTP réellement émises.
func (h *harness) fakeBroker(t *testing.T, state *brokerState) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(state.serve))
	t.Cleanup(srv.Close)
	dir := h.path("etc", "ogs", "credentials")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	creds := fmt.Sprintf("url: %s\nuser: admin\npassword: s3cr3t\namqpPort: 5672\n", srv.URL)
	if err := os.WriteFile(filepath.Join(dir, "amqp.yaml"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
}

// brokerState porte l'état du faux broker.
type brokerState struct {
	vhosts    map[string]bool
	users     map[string]bool
	queues    map[string]map[string]any
	writes    []string
	queueList []map[string]any
}

func newBrokerState() *brokerState {
	return &brokerState{vhosts: map[string]bool{}, users: map[string]bool{},
		queues: map[string]map[string]any{}}
}

func (b *brokerState) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if r.Method != http.MethodGet {
		b.writes = append(b.writes, r.Method+" "+path)
	}
	switch {
	case path == "/api/overview":
		_ = json.NewEncoder(w).Encode(map[string]any{"rabbitmq_version": "3.13.7"})
	case path == "/api/vhosts" && r.Method == http.MethodGet:
		var out []map[string]any
		for name := range b.vhosts {
			out = append(out, map[string]any{"name": name})
		}
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(path, "/api/vhosts/") && r.Method == http.MethodPut:
		b.vhosts[strings.TrimPrefix(path, "/api/vhosts/")] = true
		w.WriteHeader(http.StatusNoContent)
	case path == "/api/users" && r.Method == http.MethodGet:
		var out []map[string]any
		for name := range b.users {
			out = append(out, map[string]any{"name": name})
		}
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(path, "/api/users/") && r.Method == http.MethodPut:
		b.users[strings.TrimPrefix(path, "/api/users/")] = true
		w.WriteHeader(http.StatusNoContent)
	case path == "/api/permissions":
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	case strings.HasPrefix(path, "/api/queues/") && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(b.queueList)
	case strings.HasPrefix(path, "/api/queues/") && r.Method == http.MethodPut:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		name := path[strings.LastIndex(path, "/")+1:]
		b.queues[name] = body
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "/api/exchanges/") && r.Method == http.MethodGet,
		strings.HasPrefix(path, "/api/bindings/") && r.Method == http.MethodGet,
		strings.HasPrefix(path, "/api/policies/") && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func TestAMQPMessengerPresetAndApply(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	state := newBrokerState()
	h.fakeBroker(t, state)
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	out := h.mustRun("amqp", "messenger", "shopfr", "--transport", "async")
	if !strings.Contains(out, "async_failed") {
		t.Errorf("la file d'échec doit être déclarée :\n%s", out)
	}

	h.mustRun("apply", "shopfr", "--yes")
	if !state.vhosts["shopfr"] {
		t.Errorf("vhost non créé, requêtes émises : %v", state.writes)
	}
	if !state.users["shopfr"] {
		t.Error("utilisateur non créé")
	}
	// Le type quorum se déclare par argument : c'est la seule forme acceptée par
	// toutes les versions du broker.
	async, ok := state.queues["async"]
	if !ok {
		t.Fatalf("file async non déclarée : %v", state.writes)
	}
	args, _ := async["arguments"].(map[string]any)
	if args["x-queue-type"] != "quorum" {
		t.Errorf("type de file : %+v", args)
	}
	if args["x-dead-letter-exchange"] != "dlx" {
		t.Errorf("exchange de rejet absent : %+v", args)
	}
}

func TestAMQPRefusesMessengerOnSymfony34(t *testing.T) {
	// Le composant Messenger n'existe qu'à partir de Symfony 4.1 : le proposer
	// sur un projet en 3.4 produirait une configuration inutilisable.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "ancien.fr", "--kind", "php-symfony", "--app-version", "3.4", "--php", "7.4")
	out, code := h.run("amqp", "messenger", "ancienfr")
	if code != CodeUsage {
		t.Fatalf("code %d attendu %d :\n%s", code, CodeUsage, out)
	}
	if !strings.Contains(out, "rabbitmq-consumer") {
		t.Errorf("le message doit indiquer la solution de remplacement :\n%s", out)
	}
}

func TestAMQPImmutableArgumentsRequireExplicitRecreation(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	state := newBrokerState()
	h.fakeBroker(t, state)
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("amqp", "messenger", "shopfr")
	h.mustRun("apply", "shopfr", "--yes")

	// Le broker rapporte désormais une file existante, avec des messages en
	// attente et un TTL différent de celui déclaré.
	state.vhosts["shopfr"] = true
	state.users["shopfr"] = true
	state.queueList = []map[string]any{{
		"name": "async", "vhost": "shopfr", "durable": true, "messages": 1240, "consumers": 3,
		"arguments": map[string]any{
			"x-queue-type": "quorum", "x-dead-letter-exchange": "dlx",
			"x-dead-letter-routing-key": "async_failed", "x-message-ttl": 3600000,
		},
	}, {
		"name": "async_failed", "vhost": "shopfr", "durable": true,
		"arguments": map[string]any{},
	}}
	// La déclaration ne porte pas de TTL : l'écart doit être signalé, jamais
	// appliqué en silence.
	out, code := h.run("apply", "shopfr", "--yes")
	if code != CodePartial {
		t.Fatalf("code %d attendu %d :\n%s", code, CodePartial, out)
	}
	for _, want := range []string{"recréation requise", "1240 message", "--recreate-queues"} {
		if !strings.Contains(out, want) {
			t.Errorf("la sortie doit contenir %q :\n%s", want, out)
		}
	}
	// Aucune suppression n'a été tentée.
	for _, w := range state.writes {
		if strings.HasPrefix(w, "DELETE /api/queues") {
			t.Errorf("aucune file ne devait être supprimée : %v", state.writes)
		}
	}
}

func TestAMQPStatusShowsCounters(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	state := newBrokerState()
	state.vhosts["shopfr"] = true
	state.users["shopfr"] = true
	state.queueList = []map[string]any{{
		"name": "async", "vhost": "shopfr", "durable": true, "type": "quorum",
		"messages": 42, "consumers": 2, "arguments": map[string]any{},
	}}
	h.fakeBroker(t, state)
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("amqp", "messenger", "shopfr")
	out := h.mustRun("amqp", "status", "shopfr")
	if !strings.Contains(out, "async") || !strings.Contains(out, "42") {
		t.Errorf("les compteurs doivent être affichés :\n%s", out)
	}
}

func TestAMQPUnavailableBrokerDoesNotBlockSite(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	// Aucun identifiant enregistré : le broker est injoignable.
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("amqp", "messenger", "shopfr")
	out, code := h.run("apply", "shopfr", "--yes")
	if code != CodePartial {
		t.Fatalf("code %d attendu %d :\n%s", code, CodePartial, out)
	}
	// Le site est déployé malgré tout.
	for _, rel := range []string{
		filepath.Join("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf"),
		filepath.Join("etc", "apache2", "sites-available", "shopfr.conf"),
	} {
		if _, err := os.Stat(h.path(rel)); err != nil {
			t.Errorf("%s devrait exister malgré l'indisponibilité du broker : %v", rel, err)
		}
	}
}

func TestAMQPCredentialsPermissions(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	if out, code := h.run("amqp", "credentials", "set", "--user", "admin"); code != CodeUsage {
		t.Errorf("le mot de passe est requis : code %d\n%s", code, out)
	}
	out := h.mustRun("amqp", "credentials", "set", "--user", "guest", "--password", "guest")
	if !strings.Contains(out, "compte par défaut connu") {
		t.Errorf("l'emploi de guest doit être signalé :\n%s", out)
	}
	info, err := os.Stat(h.path("etc", "ogs", "credentials", "amqp.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %04o, attendu 0600", info.Mode().Perm())
	}
}

// --- Lot L5 : tâches planifiées, services et délégation ---

func TestCronAndServiceLifecycle(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	out := h.mustRun("cron", "add", "shopfr", "scheduler",
		"--schedule", "0 3 * * *", "--command", "bin/console schedule:run")
	// Les prochaines occurrences confirment que l'expression dit ce qu'on croit.
	if !strings.Contains(out, "prochaines exécutions") || !strings.Contains(out, "03:00") {
		t.Errorf("les occurrences doivent être affichées :\n%s", out)
	}
	h.mustRun("service", "add", "shopfr", "messenger-async", "--transport", "async", "--instances", "3")
	h.mustRun("apply", "shopfr", "--yes")

	// Le fichier cron : verrou, identité, interpréteur par le lien, journal.
	cronFile := h.read(t, filepath.Join("etc", "cron.d", "ogs-shopfr"))
	for _, want := range []string{
		"0 3 * * * shopfr /usr/bin/flock -n",
		filepath.Join("run", "ogs", "shopfr", "scheduler.lock"),
		filepath.Join(".local", "bin", "php"),
		filepath.Join("www", "bin", "console"),
		"cron-scheduler.log 2>&1",
		`MAILTO=""`,
	} {
		if !strings.Contains(cronFile, want) {
			t.Errorf("fichier cron incomplet, %q absent :\n%s", want, cronFile)
		}
	}
	if strings.Contains(cronFile, "/usr/bin/php8") {
		t.Error("aucune version de PHP ne doit être figée dans un cron")
	}
	// Le journal est pré-créé : logs/ appartient à root, la tâche ne pourrait pas
	// le créer elle-même.
	if _, err := os.Stat(h.path("home", "shop.fr", "logs", "cron-scheduler.log")); err != nil {
		t.Errorf("journal de tâche absent : %v", err)
	}

	// Les unités : un modèle, une cible, trois instances activées.
	if _, err := os.Stat(h.path("etc", "systemd", "system", "ogs-shopfr-messenger-async@.service")); err != nil {
		t.Errorf("modèle d'unité absent : %v", err)
	}
	list := h.mustRun("service", "list", "shopfr")
	if !strings.Contains(list, "messenger-async") || !strings.Contains(list, "3") {
		t.Errorf("les instances doivent être comptées :\n%s", list)
	}

	// Idempotence de l'ensemble.
	h.simulateSystemEffects("shopfr", "shop.fr", 1004)
	out = h.mustRun("apply", "shopfr", "--yes")
	if !strings.Contains(out, "aucun changement") {
		t.Errorf("un apply rejoué doit être vide :\n%s", out)
	}
}

func TestCronDisableKeepsTraceInFile(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("cron", "add", "shopfr", "nettoyage",
		"--schedule", "0 4 * * *", "--command", "/usr/bin/find /tmp -delete")
	h.mustRun("apply", "shopfr", "--yes")
	h.mustRun("cron", "disable", "shopfr", "nettoyage")
	h.mustRun("apply", "shopfr", "--yes")
	cronFile := h.read(t, filepath.Join("etc", "cron.d", "ogs-shopfr"))
	if !strings.Contains(cronFile, "# nettoyage (désactivée)") {
		t.Errorf("la désactivation doit rester lisible :\n%s", cronFile)
	}
	if !strings.Contains(cronFile, "# 0 4 * * * shopfr") {
		t.Errorf("la ligne doit être commentée, pas supprimée :\n%s", cronFile)
	}
	list := h.mustRun("cron", "list", "shopfr")
	if !strings.Contains(list, "désactivée") {
		t.Errorf("l'état doit apparaître dans la liste :\n%s", list)
	}
}

func TestCronRejectsInvalidExpression(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	for _, expr := range []string{"* * * *", "60 * * * *", "0 0 31 2 *"} {
		out, code := h.run("cron", "add", "shopfr", "x", "--schedule", expr, "--command", "/bin/true")
		if code != CodeValidation {
			t.Errorf("expression %q : code %d attendu %d\n%s", expr, code, CodeValidation, out)
		}
	}
}

func TestServiceScaleActsWithoutTouchingDeclaration(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("service", "add", "shopfr", "messenger-async", "--transport", "async", "--instances", "2")
	h.mustRun("apply", "shopfr", "--yes")

	out := h.mustRun("service", "scale", "shopfr", "messenger-async", "5")
	if !strings.Contains(out, "5 instance(s)") {
		t.Errorf("sortie inattendue :\n%s", out)
	}
	if !strings.Contains(out, "la déclaration du projet reste inchangée") {
		t.Error("la nature temporaire de l'opération doit être annoncée")
	}
	wants := h.path("etc", "systemd", "system", "ogs-shopfr.target.wants")
	entries, err := os.ReadDir(wants)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Errorf("%d instances actives, 5 attendues", len(entries))
	}
	// Le prochain apply rétablit le nombre déclaré : c'est le propre d'un moteur
	// de réconciliation.
	h.mustRun("apply", "shopfr", "--yes")
	entries, _ = os.ReadDir(wants)
	if len(entries) != 2 {
		t.Errorf("%d instances après réconciliation, 2 déclarées", len(entries))
	}
}

func TestServiceRefusesMessengerOnSymfony34(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "ancien.fr", "--kind", "php-symfony", "--app-version", "3.4", "--php", "7.4")
	out, code := h.run("service", "add", "ancienfr", "worker", "--template", "symfony-messenger")
	if code != CodeValidation {
		t.Fatalf("code %d attendu %d :\n%s", code, CodeValidation, out)
	}
	if !strings.Contains(out, "rabbitmq-consumer") {
		t.Errorf("le message doit indiquer le template de remplacement :\n%s", out)
	}
	// Le template de remplacement est accepté.
	h.mustRun("service", "add", "ancienfr", "consumer", "--template", "rabbitmq-consumer", "--transport", "async")
}

func TestSudoersDelegationIsNarrow(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("service", "add", "shopfr", "messenger-async", "--transport", "async")
	h.mustRun("apply", "shopfr", "--yes")

	path := h.path("etc", "sudoers.d", "ogs-shopfr")
	content := h.read(t, filepath.Join("etc", "sudoers.d", "ogs-shopfr"))
	// Aucun métacaractère, aucun systemctl nu, chemin absolu.
	if strings.ContainsAny(strings.Join(nonCommentLines(content), " "), "*?[]") {
		t.Errorf("aucun métacaractère ne doit apparaître :\n%s", content)
	}
	for _, verb := range []string{"start", "stop", "restart", "status"} {
		if !strings.Contains(content, "/usr/bin/ogs my "+verb) {
			t.Errorf("verbe %s absent :\n%s", verb, content)
		}
	}
	// sudo refuse un fichier de sudoers.d inscriptible.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o440 {
		t.Errorf("mode %04o, attendu 0440", info.Mode().Perm())
	}
}

func nonCommentLines(content string) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			out = append(out, trimmed)
		}
	}
	return out
}

func TestSudoersSystemctlStyleEnumerates(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("service", "add", "shopfr", "messenger-async", "--transport", "async", "--instances", "2")
	// Basculer le style dans le fichier de projet.
	path := h.path("etc", "ogs", "projects", "shopfr.yaml")
	data := h.read(t, filepath.Join("etc", "ogs", "projects", "shopfr.yaml"))
	data = strings.Replace(data, "spec:", "spec:\n  sudo:\n    style: systemctl\n    verbs: [restart]", 1)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustRun("apply", "shopfr", "--yes")
	content := h.read(t, filepath.Join("etc", "sudoers.d", "ogs-shopfr"))
	if !strings.Contains(content, "/usr/bin/systemctl restart ogs-shopfr.target") {
		t.Errorf("la cible doit être autorisée :\n%s", content)
	}
	for i := 1; i <= 2; i++ {
		want := fmt.Sprintf("/usr/bin/systemctl restart ogs-shopfr-messenger-async@%d.service", i)
		if !strings.Contains(content, want) {
			t.Errorf("instance %d absente :\n%s", i, content)
		}
	}
	// Aucun verbe non demandé.
	if strings.Contains(content, "systemctl stop") {
		t.Errorf("seul le verbe demandé doit être autorisé :\n%s", content)
	}
}

func TestMyRequiresIdentifiableCaller(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	// Sans SUDO_UID, l'appelant n'est pas identifiable : la commande doit renvoyer
	// vers la forme explicite plutôt que de deviner.
	t.Setenv("SUDO_UID", "")
	out, code := h.run("my", "restart")
	if code != CodePermission && code != CodeUsage {
		t.Errorf("code %d :\n%s", code, out)
	}
	if !strings.Contains(out, "ogs service") && !strings.Contains(out, "aucun projet") {
		t.Errorf("le message doit orienter l'appelant :\n%s", out)
	}
}

func TestRuntimeDirectoriesAreDeclared(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "--yes")
	tmpfiles := h.read(t, filepath.Join("etc", "tmpfiles.d", "ogs.conf"))
	// Le répertoire du projet doit lui appartenir : il y écrit ses verrous et ses
	// sockets, et le groupe traverse jusqu'au socket sans énumérer le reste.
	if !strings.Contains(tmpfiles, "0710 shopfr shopfr") {
		t.Errorf("propriétaire attendu :\n%s", tmpfiles)
	}
	if !strings.Contains(tmpfiles, "0755 root root") {
		t.Errorf("la racine doit rester traversable :\n%s", tmpfiles)
	}
	// Et le répertoire existe immédiatement, sans attendre un redémarrage — au
	// mode que le fichier tmpfiles vient de déclarer, jamais à un autre : deux
	// juges pour un même répertoire le feraient osciller entre deux valeurs à
	// chaque redémarrage.
	info, err := os.Stat(h.path("run", "ogs", "shopfr"))
	if err != nil {
		t.Fatalf("répertoire d'exécution absent : %v", err)
	}
	if info.Mode().Perm() != runtimeprov.ProjectRunMode.Perm() {
		t.Errorf("mode %04o, attendu %04o", info.Mode().Perm(), runtimeprov.ProjectRunMode.Perm())
	}
}

// --- Lot L6 : composition, suppression, restauration ---

func TestSiteCreateAssemblesEverything(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	state := newBrokerState()
	h.fakeBroker(t, state)

	out := h.mustRunPartial("site", "create", "shop.fr",
		"--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5",
		"--alias", "www.shop.fr", "--redirect", "ancien-shop.fr",
		"--db", "mysql", "--messenger", "async",
		"--service", "symfony-messenger:async:2",
		"--cron", "* * * * *:bin/console schedule:run",
		"--yes")

	// Le récapitulatif dit ce qui a été assemblé.
	for _, want := range []string{"utilisateur  shopfr:shopfr", "php          8.5", "base         mysql shopfr",
		"transport    vhost shopfr", "service      messenger-async", "tâche"} {
		if !strings.Contains(out, want) {
			t.Errorf("récapitulatif incomplet, %q absent :\n%s", want, out)
		}
	}
	// Toutes les ressources sont en place.
	for _, rel := range []string{
		filepath.Join("home", "shop.fr", "www", "public"),
		filepath.Join("home", "shop.fr", "logs", "cron-schedule-run.log"),
		filepath.Join("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf"),
		filepath.Join("etc", "apache2", "sites-available", "shopfr.conf"),
		filepath.Join("etc", "apache2", "sites-available", "shopfr_redirect.conf"),
		filepath.Join("etc", "cron.d", "ogs-shopfr"),
		filepath.Join("etc", "systemd", "system", "ogs-shopfr.target"),
		filepath.Join("etc", "sudoers.d", "ogs-shopfr"),
		filepath.Join("var", "lib", "ogs", "acme", "certificates", "shopfr", "fullchain.pem"),
	} {
		if _, err := os.Stat(h.path(rel)); err != nil {
			t.Errorf("%s absent après site create : %v", rel, err)
		}
	}
	// Le vhost sert le domaine principal et son alias.
	vhost := h.read(t, filepath.Join("etc", "apache2", "sites-available", "shopfr.conf"))
	if !strings.Contains(vhost, "ServerName shop.fr") || !strings.Contains(vhost, "ServerAlias www.shop.fr") {
		t.Errorf("domaines mal servis :\n%s", vhost)
	}
	// Le vhost de redirection ne porte pas le site.
	redirect := h.read(t, filepath.Join("etc", "apache2", "sites-available", "shopfr_redirect.conf"))
	if !strings.Contains(redirect, "ancien-shop.fr") || !strings.Contains(redirect, "shop.fr$1") {
		t.Errorf("redirection inattendue :\n%s", redirect)
	}
	// Le transport a bien été déclaré sur le broker.
	if !state.vhosts["shopfr"] {
		t.Errorf("vhost amqp non créé : %v", state.writes)
	}
}

func TestSiteCreateRejectsExistingProject(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	if out, code := h.run("site", "create", "shop.fr", "--kind", "static", "--yes"); code != CodeConflict {
		t.Errorf("code %d attendu %d :\n%s", code, CodeConflict, out)
	}
}

func TestSiteCreateCronOptionSplitsOnFirstColon(t *testing.T) {
	// Une commande contient souvent un deux-points (« schedule:run ») : la découpe
	// doit se faire sur le premier, celui qui suit l'expression.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static",
		"--cron", "*/5 * * * *:bin/console app:sync --force", "--yes")
	cronFile := h.read(t, filepath.Join("etc", "cron.d", "ogs-shopfr"))
	if !strings.Contains(cronFile, "*/5 * * * * shopfr") {
		t.Errorf("expression mal découpée :\n%s", cronFile)
	}
	if !strings.Contains(cronFile, "app:sync --force") {
		t.Errorf("commande tronquée :\n%s", cronFile)
	}
}

func TestSiteCreateWithoutTLSDeclaresNoCertificate(t *testing.T) {
	// Un site explicitement laissé en HTTP ne doit pas déclarer de certificat :
	// autrement chaque apply solliciterait l'autorité — et son quota — pour un
	// certificat que le vhost n'emploiera jamais.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	decl := h.read(t, filepath.Join("etc", "ogs", "projects", "shopfr.yaml"))
	if strings.Contains(decl, "certificates:") {
		t.Errorf("aucun certificat ne doit être déclaré sans TLS :\n%s", decl)
	}
	// Le refus doit être inscrit dans la déclaration : le vhost étant implicite,
	// c'est le seul endroit où il peut survivre à une relecture.
	if !strings.Contains(decl, "tls:") {
		t.Errorf("le renoncement au TLS n'est pas déclaré :\n%s", decl)
	}
	// Et le vhost écrit doit être en HTTP seul.
	vhost := h.read(t, filepath.Join("etc", "apache2", "sites-available", "shopfr.conf"))
	// Le port est cherché sous sa forme de directive : le chemin du répertoire
	// temporaire porte un nombre aléatoire, qui peut contenir « 443 ».
	for _, interdit := range []string{"SSLEngine", "*:443", "listen 443"} {
		if strings.Contains(vhost, interdit) {
			t.Errorf("le vhost contient %q alors que le TLS est refusé :\n%s", interdit, vhost)
		}
	}
	// Le mode préfixé ne crée pas les comptes système, l'écart les concernant est
	// normal ; ce qui ne doit plus apparaître, c'est le certificat — sans quoi
	// chaque apply le redemanderait.
	out, _ := h.run("diff", "shopfr")
	if strings.Contains(out, "certificat") {
		t.Errorf("le certificat est encore planifié :\n%s", out)
	}

	// Avec --self-signed, en revanche, le certificat est voulu.
	h.mustRun("site", "create", "boutique.fr", "--kind", "static", "--tls=false",
		"--self-signed", "--yes")
	decl = h.read(t, filepath.Join("etc", "ogs", "projects", "boutiquefr.yaml"))
	if !strings.Contains(decl, "selfsigned") {
		t.Errorf("le certificat auto-signé doit rester déclaré :\n%s", decl)
	}
}

func TestSiteCloneToPrep(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4",
		"--php", "8.5", "--db", "mysql", "--yes")
	// Du contenu applicatif, pour vérifier la copie.
	if err := os.WriteFile(h.path("home", "shop.fr", "www", "public", "index.php"),
		[]byte("<?php echo 'production';"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := h.run("site", "clone", "shopfr", "--to", "prep", "--yes")
	if code != CodeOK && code != CodePartial {
		t.Fatalf("code %d :\n%s", code, out)
	}
	// La préproduction est un projet distinct, sur le domaine dédié.
	shown := h.mustRun("project", "show", "shopfrprep")
	for _, want := range []string{"shopfrprep", "shop.preprod-pp.net"} {
		if !strings.Contains(shown, want) {
			t.Errorf("le clone doit être en préproduction, %q absent :\n%s", want, shown)
		}
	}
	// Aucune ressource partagée avec la production.
	if strings.Contains(shown, "mysql shopfr\n") {
		t.Error("la base de la préproduction doit être distincte")
	}
	if !strings.Contains(shown, "shopfrprep") {
		t.Error("les ressources doivent porter l'identifiant de préproduction")
	}
	// Les deux homes existent.
	for _, host := range []string{"shop.fr", "shop.preprod-pp.net"} {
		if _, err := os.Stat(h.path("home", host)); err != nil {
			t.Errorf("home %s absent : %v", host, err)
		}
	}
}

func TestDeleteArchivesThenRemoves(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5",
		"--service", "symfony-messenger:async:1", "--yes")
	if err := os.WriteFile(h.path("home", "shop.fr", "www", "public", "index.php"),
		[]byte("<?php echo 'à archiver';"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := h.mustRun("project", "delete", "shopfr", "--yes")
	if !strings.Contains(out, "archive créée") {
		t.Errorf("une archive doit précéder la suppression :\n%s", out)
	}
	// Tout a disparu du système.
	for _, rel := range []string{
		filepath.Join("home", "shop.fr"),
		filepath.Join("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf"),
		filepath.Join("etc", "apache2", "sites-available", "shopfr.conf"),
		filepath.Join("etc", "cron.d", "ogs-shopfr"),
		filepath.Join("etc", "sudoers.d", "ogs-shopfr"),
		filepath.Join("etc", "systemd", "system", "ogs-shopfr.target"),
		filepath.Join("etc", "ogs", "projects", "shopfr.yaml"),
	} {
		if _, err := os.Stat(h.path(rel)); !os.IsNotExist(err) {
			t.Errorf("%s devrait avoir été supprimé : %v", rel, err)
		}
	}
	// La déclaration est conservée sous archived/ : la trace de ce qui a existé.
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "archived", "shopfr.yaml")); err != nil {
		t.Errorf("la déclaration archivée devrait subsister : %v", err)
	}
	// L'archive est listable et son manifeste lisible.
	list := h.mustRun("project", "archives")
	if !strings.Contains(list, "shopfr") {
		t.Errorf("l'archive doit être listée :\n%s", list)
	}
}

func TestDeleteThenRestoreRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4",
		"--php", "8.5", "--yes")
	content := "<?php echo 'contenu à retrouver';"
	if err := os.WriteFile(h.path("home", "shop.fr", "www", "public", "index.php"),
		[]byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// Un secret scellé, pour vérifier qu'il traverse l'archive.
	st, err := store.Open(h.path("var", "lib", "ogs", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	ks, err := secret.Open(h.path("etc", "ogs", "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	nonce, ciphertext, err := ks.Seal("shopfr", "mysql/shopfr", []byte("secret-a-retrouver"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSecret("shopfr", "mysql/shopfr", nonce, ciphertext); err != nil {
		t.Fatal(err)
	}
	st.Close()

	out := h.mustRun("project", "delete", "shopfr", "--yes")
	if !strings.Contains(out, "1 secret(s)") {
		t.Errorf("le secret doit être inclus dans l'archive :\n%s", out)
	}

	// Restauration.
	entries, err := os.ReadDir(h.path("var", "lib", "ogs", "archives"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("aucune archive : %v", err)
	}
	archivePath := h.path("var", "lib", "ogs", "archives", entries[0].Name())
	out, code := h.run("project", "restore", archivePath, "--yes")
	if code != CodeOK && code != CodePartial {
		t.Fatalf("code %d :\n%s", code, out)
	}
	// Le contenu est revenu à l'identique.
	restored, err := os.ReadFile(h.path("home", "shop.fr", "www", "public", "index.php"))
	if err != nil {
		t.Fatalf("contenu non restauré : %v", err)
	}
	if string(restored) != content {
		t.Errorf("contenu restauré : %q", restored)
	}
	// Le secret est relisible avec la même clé maître.
	st, err = store.Open(h.path("var", "lib", "ogs", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	nonce, ciphertext, err = st.GetSecret("shopfr", "mysql/shopfr")
	if err != nil {
		t.Fatalf("secret absent après restauration : %v", err)
	}
	plain, err := ks.Open("shopfr", "mysql/shopfr", nonce, ciphertext)
	if err != nil {
		t.Fatalf("secret illisible : %v", err)
	}
	if string(plain) != "secret-a-retrouver" {
		t.Errorf("secret restitué : %q", plain)
	}
}

func TestPurgeRequiresExactIdentifier(t *testing.T) {
	// Une suppression sans recours ne doit pas pouvoir être confirmée par un
	// simple --yes.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	out, code := h.run("project", "delete", "shopfr", "--purge", "--yes")
	if code != CodeCancelled {
		t.Fatalf("code %d attendu %d :\n%s", code, CodeCancelled, out)
	}
	if !strings.Contains(out, "sans recours") {
		t.Errorf("le motif doit être explicite :\n%s", out)
	}
	// Le projet est intact.
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "shopfr.yaml")); err != nil {
		t.Errorf("le projet ne devait pas être supprimé : %v", err)
	}
}

func TestDeleteKeepData(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	out := h.mustRun("project", "delete", "shopfr", "--keep-data", "--yes")
	if !strings.Contains(out, "conservés") {
		t.Errorf("la conservation doit être annoncée :\n%s", out)
	}
	// Le contenu subsiste, les configurations non.
	if _, err := os.Stat(h.path("home", "shop.fr")); err != nil {
		t.Errorf("le home devait être conservé : %v", err)
	}
	if _, err := os.Stat(h.path("etc", "apache2", "sites-available", "shopfr.conf")); !os.IsNotExist(err) {
		t.Error("le vhost devait être retiré")
	}
}

func TestRestoreRefusesExistingProject(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	h.mustRun("project", "delete", "shopfr", "--yes")
	entries, _ := os.ReadDir(h.path("var", "lib", "ogs", "archives"))
	archivePath := h.path("var", "lib", "ogs", "archives", entries[0].Name())
	if _, code := h.run("project", "restore", archivePath, "--yes"); code != CodeOK && code != CodePartial {
		t.Fatalf("première restauration : code %d", code)
	}
	out, code := h.run("project", "restore", archivePath, "--yes")
	if code != CodeConflict {
		t.Errorf("restaurer par-dessus un projet existant : code %d attendu %d\n%s",
			code, CodeConflict, out)
	}
}

func TestStatusReportsHealthAndExitCode(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5", "--yes")
	h.simulateSystemEffects("shopfr", "shop.fr", 1004)

	out, code := h.run("status")
	if !strings.Contains(out, "shopfr") || !strings.Contains(out, "8.5") {
		t.Errorf("le tableau doit décrire le projet :\n%s", out)
	}
	if code != CodeOK {
		t.Errorf("un projet conforme doit donner le code 0, obtenu %d :\n%s", code, out)
	}
	// Une dérive doit être détectée et rendue par le code de retour : c'est ce
	// qui rend la commande utilisable en supervision.
	if err := os.Chmod(h.path("home", "shop.fr", "logs"), 0o777); err != nil {
		t.Fatal(err)
	}
	out, code = h.run("status")
	if code != CodePartial {
		t.Errorf("une dérive doit donner le code %d, obtenu %d :\n%s", CodePartial, code, out)
	}
	if !strings.Contains(out, "dérive") {
		t.Errorf("l'état doit être signalé :\n%s", out)
	}
}

// --- Lot L8 : import d'un serveur existant ---

// legacyServer installe une configuration « faite à la main » sous le préfixe.
func (h *harness) legacyServer(t *testing.T) {
	t.Helper()
	mk := func(dir string) {
		if err := os.MkdirAll(h.path(dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, content string) {
		mk(filepath.Dir(rel))
		if err := os.WriteFile(h.path(rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk(filepath.Join("etc", "apache2", "sites-available"))
	mk(filepath.Join("etc", "apache2", "sites-enabled"))
	mk(filepath.Join("var", "www", "boutique", "public"))
	write(filepath.Join("etc", "apache2", "sites-available", "boutique.conf"), `
<VirtualHost *:80>
    ServerName boutique.fr
    ServerAlias www.boutique.fr
    DocumentRoot /var/www/boutique/public
    <FilesMatch \.php$>
        SetHandler "proxy:unix:/run/php/php8.1-fpm-boutique.sock|fcgi://localhost"
    </FilesMatch>
</VirtualHost>
`)
	if err := os.Symlink("../sites-available/boutique.conf",
		h.path("etc", "apache2", "sites-enabled", "boutique.conf")); err != nil {
		t.Fatal(err)
	}
	mk(filepath.Join("etc", "php", "8.1", "fpm", "pool.d"))
	write(filepath.Join("etc", "php", "8.1", "fpm", "pool.d", "boutique.conf"),
		"[boutique]\nuser = boutique\nlisten = /run/php/php8.1-fpm-boutique.sock\n")
	write(filepath.Join("usr", "bin", "php8.1"), "#!/bin/sh\n")
	write(filepath.Join("usr", "sbin", "php-fpm8.1"), "#!/bin/sh\n")
	write(filepath.Join("etc", "passwd"),
		"boutique:x:1001:1001::/var/www/boutique:/bin/bash\n"+
			"humain:x:1002:1002:Un humain:/home/humain:/bin/bash\n")
}

func TestImportScanIsReadOnlyAndProposes(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.legacyServer(t)

	out := h.mustRun("import", "scan")
	for _, want := range []string{"boutiquefr", "confiance élevée", "boutique.fr",
		"/var/www/boutique", "php-symfony", "Non géré", "humain"} {
		if !strings.Contains(out, want) {
			t.Errorf("le rapport doit contenir %q :\n%s", want, out)
		}
	}
	// Le scan ne déclare rien : c'est une inspection.
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "boutiquefr.yaml")); !os.IsNotExist(err) {
		t.Error("« import scan » ne doit rien écrire")
	}
}

func TestImportAdoptKeepsExistingNamesAndAnnouncesDrift(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.legacyServer(t)

	out := h.mustRun("import", "adopt", "boutiquefr", "--yes")
	// Les écarts à la convention sont annoncés avant, pas découverts après.
	for _, want := range []string{"écarts à la convention", "utilisateur : boutique", "convention : boutiquefr"} {
		if !strings.Contains(out, want) {
			t.Errorf("l'écart doit être annoncé, %q absent :\n%s", want, out)
		}
	}
	declaration := h.read(t, filepath.Join("etc", "ogs", "projects", "boutiquefr.yaml"))
	// L'adoption décrit l'existant : les noms réels sont conservés.
	for _, want := range []string{"name: boutique", "/var/www/boutique", "adopted: \"true\""} {
		if !strings.Contains(declaration, want) {
			t.Errorf("la déclaration doit conserver l'existant, %q absent :\n%s", want, declaration)
		}
	}
	// Et le projet est exploitable : le diff fonctionne, sans conflit avec
	// lui-même sur ses propres domaines.
	out, code := h.run("diff", "boutiquefr")
	if code != CodePartial && code != CodeOK {
		t.Fatalf("le diff d'un projet adopté doit fonctionner (code %d) :\n%s", code, out)
	}
	if strings.Contains(out, "déjà servi par") {
		t.Errorf("un projet adopté ne doit pas entrer en conflit avec ses propres vhosts :\n%s", out)
	}
}

func TestImportAdoptRecordsUnmanaged(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.legacyServer(t)
	h.mustRun("import", "adopt", "boutiquefr", "--yes")
	out := h.mustRun("import", "unmanaged")
	if !strings.Contains(out, "humain") {
		t.Errorf("les ressources hors gestion doivent être mémorisées :\n%s", out)
	}
}

func TestImportAdoptRejectsUnknownCandidate(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.legacyServer(t)
	out, code := h.run("import", "adopt", "inexistant")
	if code != CodeUsage {
		t.Errorf("code %d attendu %d :\n%s", code, CodeUsage, out)
	}
	if !strings.Contains(out, "boutiquefr") {
		t.Errorf("les candidats disponibles doivent être énumérés :\n%s", out)
	}
}

func TestImportIgnoresAlreadyManaged(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	out := h.mustRun("import", "scan")
	// Un site déjà géré ne doit pas être reproposé à l'adoption.
	if strings.Contains(out, "Projet candidat : shopfr") {
		t.Errorf("un projet déjà géré ne doit pas être proposé :\n%s", out)
	}
}

// --- Lot L9 : applications Go, Node, Ruby, Python ---

func TestNodeApplicationFullCycle(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "app.fr", "--kind", "node")
	// La version de runtime se déclare dans le fichier de projet.
	path := h.path("etc", "ogs", "projects", "appfr.yaml")
	data := h.read(t, filepath.Join("etc", "ogs", "projects", "appfr.yaml"))
	data = strings.Replace(data, "kind: node",
		"kind: node\n    runtime: 22.11.0\n    entrypoint: server.js", 1)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := h.mustRun("plan", "appfr")
	// L'application précède le vhost de proxy : l'inverse exposerait un 502.
	iApp := strings.Index(plan, "unité applicative")
	iWait := strings.Index(plan, "attente de disponibilité")
	iVHost := strings.Index(plan, "vhost apache appfr")
	if iApp < 0 || iWait < 0 || iVHost < 0 {
		t.Fatalf("plan incomplet :\n%s", plan)
	}
	if !(iApp < iWait && iWait < iVHost) {
		t.Errorf("ordre incorrect (unité=%d attente=%d vhost=%d) :\n%s", iApp, iWait, iVHost, plan)
	}

	h.mustRun("apply", "appfr", "--yes")
	unit := h.read(t, filepath.Join("etc", "systemd", "system", "ogs-appfr-app@.service"))
	for _, want := range []string{
		"ExecStart=", filepath.Join(".local", "bin", "node"), "server.js",
		"SOCKET_PATH=", "UMask=0007", "NoNewPrivileges=true",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unité incomplète, %q absent :\n%s", want, unit)
		}
	}
	// Le vhost proxifie vers le socket, et le challenge ACME reste servi.
	vhost := h.read(t, filepath.Join("etc", "apache2", "sites-available", "appfr.conf"))
	if !strings.Contains(vhost, "ProxyPass        / unix:") {
		t.Errorf("le proxy doit viser le socket :\n%s", vhost)
	}
	if !strings.Contains(vhost, "ProxyPass        /.well-known/acme-challenge !") {
		t.Errorf("le challenge ACME doit échapper au proxy :\n%s", vhost)
	}
	// Le lien est en place et le runtime listé.
	target, err := os.Readlink(h.path("home", "app.fr", ".local", "bin", "node"))
	if err != nil {
		t.Fatalf("lien absent : %v", err)
	}
	if !strings.Contains(target, "v22.11.0") {
		t.Errorf("cible du lien : %q", target)
	}
	list := h.mustRun("runtime", "list")
	if !strings.Contains(list, "node") || !strings.Contains(list, "22.11.0") {
		t.Errorf("le runtime doit être listé :\n%s", list)
	}
}

func TestGoApplicationUsesPort(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "api.fr", "--kind", "go")
	h.mustRun("apply", "apifr", "--yes")
	unit := h.read(t, filepath.Join("etc", "systemd", "system", "ogs-apifr-app@.service"))
	// Un binaire Go n'a pas de convention de socket : il écoute sur un port, borné
	// à la boucle locale.
	if !strings.Contains(unit, "PORT=21001") || !strings.Contains(unit, "HOST=127.0.0.1") {
		t.Errorf("le port doit être transmis :\n%s", unit)
	}
	if strings.Contains(unit, "0.0.0.0") {
		t.Error("aucune écoute sur toutes les interfaces ne doit être suggérée")
	}
	if !strings.Contains(unit, filepath.Join(".local", "bin", "app")) {
		t.Errorf("le binaire doit passer par .local/bin/app :\n%s", unit)
	}
}

func TestAppTypesAndSet(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	types := h.mustRun("app", "types")
	for _, want := range []string{"php-symfony", "php-prestashop", "node", "proxy"} {
		if !strings.Contains(types, want) {
			t.Errorf("le type %q doit être listé :\n%s", want, types)
		}
	}
	// Basculer de type déplace le docroot : le laisser figé servirait le mauvais
	// répertoire.
	h.mustRun("project", "create", "shop.fr", "--kind", "php-prestashop", "--php", "8.5")
	before := h.mustRun("project", "show", "shopfr")
	if strings.Contains(before, filepath.Join("www", "public")) {
		t.Errorf("Prestashop sert www :\n%s", before)
	}
	h.mustRun("app", "set", "shopfr", "--kind", "php-symfony")
	after := h.mustRun("project", "show", "shopfr")
	if !strings.Contains(after, filepath.Join("www", "public")) {
		t.Errorf("Symfony sert www/public :\n%s", after)
	}
}

func TestRuntimeInstallRequiresRuntimeKind(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5")
	out, code := h.run("runtime", "install", "shopfr", "22.11.0")
	if code != CodeUsage {
		t.Errorf("code %d attendu %d :\n%s", code, CodeUsage, out)
	}
	if !strings.Contains(out, "go, node, python, ruby") {
		t.Errorf("les types concernés doivent être énumérés :\n%s", out)
	}
}

func TestApplicationProjectGetsDelegationAndRuntimeFlag(t *testing.T) {
	// Un projet applicatif sans service déclaré n'obtenait aucune règle sudoers :
	// son propriétaire ne pouvait pas redémarrer sa propre application, ce qui est
	// pourtant le geste le plus courant après un déploiement.
	h := newHarness(t)
	h.mustRun("init")
	// L'option --runtime est indispensable : sans elle la déclaration est refusée
	// à l'apply, et rien ne permettait de la donner sans éditer le fichier.
	h.mustRunPartial("site", "create", "app.fr", "--kind", "node",
		"--runtime", "22.14.0", "--tls=false", "--yes")

	decl := h.read(t, filepath.Join("etc", "ogs", "projects", "appfr.yaml"))
	if !strings.Contains(decl, "runtime: 22.14.0") {
		t.Errorf("la version du runtime doit être déclarée :\n%s", decl)
	}
	sudoers := h.read(t, filepath.Join("etc", "sudoers.d", "ogs-appfr"))
	if !strings.Contains(sudoers, "appfr") {
		t.Errorf("la délégation doit être écrite :\n%s", sudoers)
	}
}

// uidMatches lit l'UID réel, le premier des quatre nombres du champ « Uid: » de
// /proc/<pid>/status : confondre l'UID effectif avec l'UID réel désignerait le
// mauvais compte pour tout processus setuid.
func TestUIDMatchesReadsRealUID(t *testing.T) {
	const status = "Name:\tphp-fpm8.4\nState:\tS (sleeping)\nUid:\t1001\t1001\t1001\t1001\nGid:\t1001\t1001\t1001\t1001\n"
	if !uidMatches(status, 1001) {
		t.Error("l'UID 1001 doit correspondre")
	}
	if uidMatches(status, 0) {
		t.Error("l'UID 0 ne doit pas correspondre")
	}
	// Un processus dont l'UID réel diffère de l'UID effectif appartient à son UID
	// réel : c'est lui qui empêche userdel d'aboutir.
	const setuid = "Name:\tpasswd\nUid:\t1001\t0\t0\t0\n"
	if !uidMatches(setuid, 1001) || uidMatches(setuid, 0) {
		t.Error("seul l'UID réel doit être retenu")
	}
	if uidMatches("Name:\tsans-uid\n", 0) {
		t.Error("un status sans champ Uid ne doit correspondre à personne")
	}
}

// processesOfUID énumère les processus d'un compte : c'est ce qui permet de dire
// ce qui tient un compte que userdel refuse de supprimer.
func TestProcessesOfUIDFindsSelf(t *testing.T) {
	procs := processesOfUID(os.Getuid())
	if len(procs) == 0 {
		t.Skip("/proc illisible sur cette machine")
	}
	self := fmt.Sprintf("%d ", os.Getpid())
	for _, p := range procs {
		if strings.HasPrefix(p, self) {
			return
		}
	}
	// La liste est bornée : le processus de test peut en être absent sans que ce
	// soit un défaut.
	if len(procs) < maxListedProcesses+1 {
		t.Errorf("le processus courant devrait figurer parmi %v", procs)
	}
}

func TestSupervisionEcritLesFichiersDuParc(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5", "--yes")

	// Fragment de l'agent : les pools passent par le vhost de supervision, jamais
	// par leur socket — le donner à l'agent lui donnerait un moyen d'exécution
	// sous l'identité du projet.
	fragment := h.read(t, filepath.Join("etc", "telegraf", "telegraf.d", "ogs.conf"))
	if !strings.Contains(fragment, "[[inputs.phpfpm]]") {
		t.Errorf("le pool du projet doit être supervisé :\n%s", fragment)
	}
	if strings.Contains(fragment, ".sock") {
		t.Errorf("aucun socket FPM ne doit figurer dans le fragment :\n%s", fragment)
	}
	if strings.Contains(fragment, "[[outputs.") {
		t.Errorf("la destination des métriques reste à l'administrateur :\n%s", fragment)
	}

	// Prisons : toutes préfixées, aucune ne recouvre celles de l'administrateur.
	jails := h.read(t, filepath.Join("etc", "fail2ban", "jail.d", "ogs.conf"))
	for _, want := range []string{"[ogs-sshd]", "ignoreip = 127.0.0.1/8"} {
		if !strings.Contains(jails, want) {
			t.Errorf("%q absent du fichier de prisons :\n%s", want, jails)
		}
	}

	// Vhost de supervision : boucle locale seulement.
	vhost := h.read(t, filepath.Join("etc", "apache2", "sites-available", "ogs-status.conf"))
	if !strings.Contains(vhost, "127.0.0.1:8099") {
		t.Errorf("le vhost de supervision doit rester sur la boucle locale :\n%s", vhost)
	}
}

func TestSupervisionOublieUnProjetSupprime(t *testing.T) {
	// Une prison dont le journal a disparu empêche fail2ban de démarrer, et toutes
	// les autres tombent avec elle. La suppression d'un projet doit donc recalculer
	// les fichiers de parc, non les laisser désigner ce qui n'existe plus.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5", "--yes")
	h.mustRunPartial("site", "create", "boutique.fr", "--kind", "php-prestashop", "--php", "7.4", "--yes")

	h.mustRun("project", "delete", "shopfr", "--yes")

	fragment := h.read(t, filepath.Join("etc", "telegraf", "telegraf.d", "ogs.conf"))
	if strings.Contains(fragment, "shopfr") {
		t.Errorf("le projet supprimé subsiste dans les entrées de l'agent :\n%s", fragment)
	}
	if !strings.Contains(fragment, "boutiquefr") {
		t.Errorf("le projet restant doit toujours être supervisé :\n%s", fragment)
	}
	vhost := h.read(t, filepath.Join("etc", "apache2", "sites-available", "ogs-status.conf"))
	if strings.Contains(vhost, "shopfr") {
		t.Errorf("le vhost de supervision proxifie encore un pool disparu :\n%s", vhost)
	}
}

func TestSupervisionDesactivable(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	cfg := h.path("etc", "ogs", "config.yaml")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// « ogs init » écrit déjà la section : elle est remplacée, non dupliquée — un
	// YAML ne tolère pas deux fois la même clé de premier niveau.
	desactive := "monitoring:\n  status:\n    enabled: false\n" +
		"  telegraf:\n    enabled: false\n  fail2ban:\n    enabled: false\n"
	updated := regexp.MustCompile(`(?s)monitoring:.*$`).ReplaceAllString(string(data), desactive)
	if updated == string(data) {
		t.Fatalf("la section monitoring est attendue dans la configuration produite :\n%s", data)
	}
	if err := os.WriteFile(cfg, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5", "--yes")

	for _, rel := range []string{
		filepath.Join("etc", "telegraf", "telegraf.d", "ogs.conf"),
		filepath.Join("etc", "fail2ban", "jail.d", "ogs.conf"),
		filepath.Join("etc", "apache2", "sites-available", "ogs-status.conf"),
	} {
		if _, err := os.Stat(h.path(rel)); !os.IsNotExist(err) {
			t.Errorf("%s ne devrait pas exister : la supervision est désactivée", rel)
		}
	}
}

// Les cinq fichiers de portée parc sont recalculés en entier, jamais complétés :
// les nourrir de la seule cible d'un « apply <id> » — la commande que « project
// create » conseille lui-même — effacerait tous les autres projets. Rien ne le
// dirait : l'en-tête réécrit reste conforme et systemd-tmpfiles ne supprime pas,
// si bien que la panne n'apparaîtrait qu'au redémarrage suivant, /run étant un
// tmpfs, quand plus aucun /run/ogs/<id> ne serait recréé et que tous les verrous
// de tâches échoueraient.
func TestApplyCiblePreserveLesFichiersDuParc(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5")
	h.mustRun("project", "create", "blog.fr", "--kind", "php-symfony", "--php", "8.5")

	// « plan » partage le calcul de « apply » : le plan de parc porte sur tout le
	// parc, cible ou non, et ses résumés le disent. L'assertion vient avant tout
	// apply — après, les fichiers de parc étant conformes, ces étapes sont des NoOp
	// que « plan » n'imprime pas, et rien ne discriminerait plus.
	out := h.mustRun("plan", "shopfr")
	if !strings.Contains(out, "2 projet(s)") {
		t.Errorf("le plan de parc raisonne sur la seule cible :\n%s", out)
	}

	h.mustRun("apply", "--yes")

	h.mustRun("apply", "shopfr", "--yes")

	for _, cas := range []struct{ rel, veut string }{
		{filepath.Join("etc", "logrotate.d", "ogs"), "blogfr"},
		{filepath.Join("etc", "tmpfiles.d", "ogs.conf"), "blogfr"},
		{filepath.Join("etc", "fail2ban", "jail.d", "ogs.conf"), "blogfr"},
		{filepath.Join("etc", "telegraf", "telegraf.d", "ogs.conf"), "blogfr"},
		{filepath.Join("etc", "apache2", "sites-available", "ogs-status.conf"), "blogfr"},
	} {
		got := h.read(t, cas.rel)
		if !strings.Contains(got, cas.veut) {
			t.Errorf("%s ne cite plus %s après un apply ciblé :\n%s", cas.rel, cas.veut, got)
		}
	}
}

// Le refus d'un fichier de parc retouché à la main (§6.4) est une étape critique :
// elle arrête la saga. La rotation des journaux — le fichier qu'un exploitant
// retouche le plus volontiers — passe donc en dernier du plan de parc, faute de
// quoi un « compress » ajouté à la main emporterait les répertoires d'exécution,
// donc tous les verrous de tâches au redémarrage suivant, et le timer de
// renouvellement des certificats.
func TestUneRotationRetoucheeNEmportePasLInfrastructureDuParc(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5")

	// L'infrastructure du parc est posée avant ce qui peut refuser : la rotation
	// ferme la marche du plan de parc.
	plan := h.mustRun("plan")
	if strings.Index(plan, "rotation des journaux") < strings.Index(plan, "répertoires d'exécution") {
		t.Errorf("la rotation des journaux doit venir après les répertoires d'exécution :\n%s", plan)
	}

	h.mustRun("apply", "--yes")

	rot := h.path("etc", "logrotate.d", "ogs")
	if err := os.WriteFile(rot, []byte(h.read(t, filepath.Join("etc", "logrotate.d", "ogs"))+
		"\n# ajouté à la main\ncompress\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Un second projet, dont ni le répertoire d'exécution ni la prison ne sont
	// encore posés : c'est ce que le refus emporterait.
	h.mustRun("project", "create", "blog.fr", "--kind", "php-symfony", "--php", "8.5")

	out, code := h.run("apply", "--yes")
	// Le refus est dit, et il vaut écart, non panne : code 6.
	if code != CodePartial {
		t.Fatalf("un fichier de rotation retouché est un écart (code %d), obtenu %d :\n%s",
			CodePartial, code, out)
	}
	if !strings.Contains(out, "modifié en dehors d'ogs") {
		t.Errorf("le refus doit nommer sa cause :\n%s", out)
	}
	if got := h.read(t, filepath.Join("etc", "tmpfiles.d", "ogs.conf")); !strings.Contains(got, "blogfr") {
		t.Errorf("les répertoires d'exécution doivent être posés malgré le refus :\n%s", got)
	}
	if _, err := os.Stat(h.path("etc", "systemd", "system", "ogs-renew.timer")); err != nil {
		t.Errorf("le timer de renouvellement doit être posé malgré le refus : %v", err)
	}
	// Et le fichier retouché est intact : le refus n'écrase rien.
	if !strings.Contains(h.read(t, filepath.Join("etc", "logrotate.d", "ogs")), "compress") {
		t.Error("le refus ne doit pas avoir écrasé le fichier retouché")
	}
}

// Même exigence par la porte de « site create » : créer un second site ne doit
// pas effacer le premier des fichiers de parc.
func TestCreationDeSitePreserveLesFichiersDuParc(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5", "--yes")
	h.mustRunPartial("site", "create", "blog.fr", "--kind", "php-symfony", "--php", "8.5", "--yes")

	rot := h.read(t, filepath.Join("etc", "logrotate.d", "ogs"))
	if !strings.Contains(rot, "shopfr") || !strings.Contains(rot, "blogfr") {
		t.Errorf("les deux sites doivent être tournés :\n%s", rot)
	}
	tmpfiles := h.read(t, filepath.Join("etc", "tmpfiles.d", "ogs.conf"))
	if !strings.Contains(tmpfiles, "shopfr") || !strings.Contains(tmpfiles, "blogfr") {
		t.Errorf("les deux répertoires d'exécution doivent être déclarés :\n%s", tmpfiles)
	}
}

// Un vhost de proxy nginx emploie $connection_upgrade, que seule la « map » du
// fragment de portée serveur définit. Le fragment doit donc être écrit dans le
// plan du projet, avant ses vhosts : dans le plan global, qui ne s'exécute
// qu'après les projets, il arriverait trop tard — nginx refuserait toute la
// configuration à l'activation du premier site applicatif d'une machine, la saga
// déroulerait le projet et le plan global ne serait jamais atteint.
func TestFragmentNginxPrecedeLesVhosts(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.choisirNginx(t)
	h.mustRun("project", "create", "api.fr", "--kind", "go")

	var rendu struct {
		Items []planStep `json:"items"`
	}
	out := h.mustRun("plan", "apifr", "--output", "json")
	if err := json.Unmarshal([]byte(out), &rendu); err != nil {
		t.Fatalf("plan illisible (%v) :\n%s", err, out)
	}
	fragment, vhost := -1, -1
	for i, s := range rendu.Items {
		if s.Project != "apifr" {
			continue
		}
		if s.Resource == "web/snippet" && fragment < 0 {
			fragment = i
		}
		if strings.HasPrefix(s.Resource, "web/vhost/") && vhost < 0 {
			vhost = i
		}
	}
	if fragment < 0 {
		t.Fatalf("le plan du projet doit porter le fragment de portée serveur :\n%s", out)
	}
	if vhost < 0 {
		t.Fatalf("le plan du projet doit porter son vhost :\n%s", out)
	}
	if fragment > vhost {
		t.Errorf("le fragment (%d) doit précéder le vhost (%d) :\n%s", fragment, vhost, out)
	}
}

// choisirNginx bascule le moteur web du parc : les vhosts de proxy nginx sont les
// seuls à dépendre du fragment de portée serveur.
func (h *harness) choisirNginx(t *testing.T) {
	t.Helper()
	cfg := h.path("etc", "ogs", "config.yaml")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(data), "web:\n  engine: \"\"\n", "web:\n  engine: nginx\n", 1)
	if updated == string(data) {
		t.Fatalf("la section web est attendue dans la configuration produite :\n%s", data)
	}
	if err := os.WriteFile(cfg, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Un dump contient l'intégralité des données de l'application : il ne doit
// jamais être écrit dans un fichier qu'ogs n'a pas créé lui-même. Un fichier
// planté d'avance garderait son propriétaire et son mode — le 0600 annoncé ne
// vaut qu'à la création — et l'inode resterait lisible par qui a gardé un
// descripteur dessus.
func TestSauvegardeNEcritPasDansUnFichierPreexistant(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static", "--db", "mysql")

	// Le mode préfixé simule les commandes : il suffit que l'outil et le socket
	// existent sous le préfixe pour que la sauvegarde aille jusqu'à l'écriture.
	outil := h.path("usr", "bin", "mysqldump")
	if err := os.MkdirAll(filepath.Dir(outil), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outil, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	socket := h.path("run", "mysqld", "mysqld.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cible := filepath.Join(t.TempDir(), "shopfr.sql")
	if err := os.WriteFile(cible, []byte("données d'un autre"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Le mode est posé explicitement : un umask ferait dépendre le verdict de la
	// machine.
	if err := os.Chmod(cible, 0o666); err != nil {
		t.Fatal(err)
	}
	// Le témoin partage l'inode planté : si ogs y écrit, le dump y est lisible.
	temoin := filepath.Join(filepath.Dir(cible), "temoin.sql")
	if err := os.Link(cible, temoin); err != nil {
		t.Fatal(err)
	}

	h.mustRun("db", "dump", "shopfr", "--output", cible)

	info, err := os.Stat(cible)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v : le dump a réutilisé le fichier planté", info.Mode().Perm())
	}
	reste, err := os.ReadFile(temoin)
	if err != nil {
		t.Fatal(err)
	}
	if string(reste) != "données d'un autre" {
		t.Errorf("le dump a été écrit dans l'inode planté : %q", reste)
	}
}

// Les sauvegardes de passage ne transitent pas par /tmp : le nom y serait
// déductible et le répertoire est inscriptible par tous, alors que chaque projet
// a un compte local dont le shell est ouvert (D62).
func TestRepertoireDeTravailDesSauvegardesHorsTmp(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	app := &App{Prefix: h.prefix, Paths: config.NewPaths(h.prefix)}
	dir, err := app.dumpWorkDir()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if !strings.HasPrefix(dir, app.Paths.DumpsDir()) {
		t.Errorf("%s doit être sous %s", dir, app.Paths.DumpsDir())
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("mode %v : le répertoire doit rester privé à root", info.Mode().Perm())
	}
}

// Une interruption demandée par l'exploitant n'est pas une panne du système : les
// observations de supervision portent désormais le contexte de la commande, et
// leur échec sur annulation doit rendre le code 9, pas le code 1.
func TestPlanErrDistingueLAnnulation(t *testing.T) {
	if code := ExitCode(planErr(context.Canceled)); code != CodeCancelled {
		t.Errorf("annulation : code %d, %d attendu", code, CodeCancelled)
	}
	if code := ExitCode(planErr(fmt.Errorf("moteur injoignable"))); code != CodeError {
		t.Errorf("panne : code %d, %d attendu", code, CodeError)
	}
}

func TestApplyRefuseUnVHostModifieALaMainPuisLEcraseAvecForce(t *testing.T) {
	// Le message d'erreur annonçait --force depuis toujours, sans que le drapeau
	// n'atteigne aucun provider : l'apply du projet restait bloqué, et le plan
	// entier compensait (§6.4).
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("apply", "shopfr", "--yes")

	vhost := h.path("etc", "apache2", "sites-available", "shopfr.conf")
	data, err := os.ReadFile(vhost)
	if err != nil {
		t.Fatal(err)
	}
	const ajout = "\n# réglage posé à la main\n"
	if err := os.WriteFile(vhost, append(data, []byte(ajout)...), 0o644); err != nil {
		t.Fatal(err)
	}

	// Sans --force : refus, et le fichier reste tel quel.
	out, code := h.run("apply", "shopfr", "--yes")
	if code == CodeOK {
		t.Fatalf("l'apply devait être refusé :\n%s", out)
	}
	if !strings.Contains(out, "modifié en dehors d'ogs") {
		t.Errorf("le refus doit nommer la cause :\n%s", out)
	}
	if intact := h.read(t, filepath.Join("etc", "apache2", "sites-available", "shopfr.conf")); !strings.Contains(intact, "posé à la main") {
		t.Error("le refus ne doit rien écrire")
	}
	// La copie sort du répertoire de configuration : sites-available est
	// inventorié — par « import », qui proposerait d'adopter une seconde fois le
	// même site — et une sauvegarde y serait un vhost de plus.
	sauvegardes := func() []string {
		baks, _ := filepath.Glob(h.path("var", "lib", "ogs", "backups", "shopfr.conf.ogs-bak-*"))
		return baks
	}
	if baks := sauvegardes(); len(baks) != 0 {
		t.Errorf("aucune sauvegarde ne doit être créée par un refus : %v", baks)
	}

	// Avec --force : l'écrasement a lieu, l'original est conservé.
	out, code = h.run("apply", "shopfr", "--yes", "--force")
	if code != CodeOK && code != CodePartial {
		t.Fatalf("« apply --force » devait aboutir (code %d) :\n%s", code, out)
	}
	if !strings.Contains(out, "écrasement forcé") {
		t.Errorf("l'écrasement forcé doit être annoncé :\n%s", out)
	}
	after := h.read(t, filepath.Join("etc", "apache2", "sites-available", "shopfr.conf"))
	if strings.Contains(after, "posé à la main") {
		t.Error("le vhost doit avoir été réécrit")
	}
	baks := sauvegardes()
	if len(baks) != 1 {
		t.Fatalf("une sauvegarde attendue, %d trouvée(s)", len(baks))
	}
	saved, _ := os.ReadFile(baks[0])
	if !strings.Contains(string(saved), "posé à la main") {
		t.Errorf("la sauvegarde doit contenir l'original :\n%s", saved)
	}

	// La sauvegarde ne doit pas mettre le projet en conflit avec lui-même :
	// l'apply suivant reste planifiable.
	out, code = h.run("apply", "shopfr", "--yes")
	if code != CodeOK && code != CodePartial {
		t.Fatalf("l'apply suivant devait aboutir (code %d) :\n%s", code, out)
	}
}

func TestApplyRefuseUneUniteModifieeALaMain(t *testing.T) {
	// L'unité était écrasée en silence, et les services du projet redémarrés dans
	// la foulée : l'intention de l'exploitant disparaissait sans un mot.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("service", "add", "shopfr", "veille", "--template", "worker", "--command", "/usr/bin/true")
	h.mustRun("apply", "shopfr", "--yes")

	unite := h.path("etc", "systemd", "system", "ogs-shopfr-veille@.service")
	data, err := os.ReadFile(unite)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unite, append(data, []byte("MemoryMax=512M\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := h.run("apply", "shopfr", "--yes")
	if code == CodeOK {
		t.Fatalf("l'apply devait être refusé :\n%s", out)
	}
	if !strings.Contains(out, "modifié en dehors d'ogs") {
		t.Errorf("le refus doit nommer la cause :\n%s", out)
	}
	if intact := h.read(t, filepath.Join("etc", "systemd", "system", "ogs-shopfr-veille@.service")); !strings.Contains(intact, "MemoryMax=512M") {
		t.Error("le refus ne doit rien écrire")
	}

	out, code = h.run("apply", "shopfr", "--yes", "--force")
	if code != CodeOK && code != CodePartial {
		t.Fatalf("« apply --force » devait aboutir (code %d) :\n%s", code, out)
	}
	after := h.read(t, filepath.Join("etc", "systemd", "system", "ogs-shopfr-veille@.service"))
	if strings.Contains(after, "MemoryMax=512M") {
		t.Error("l'unité doit avoir été réécrite")
	}
	if baks, _ := filepath.Glob(h.path("var", "lib", "ogs", "backups",
		"ogs-shopfr-veille@.service.ogs-bak-*")); len(baks) != 1 {
		t.Fatalf("une sauvegarde attendue, %d trouvée(s)", len(baks))
	}
}

func TestMigrationDesLiensVersLocalBin(t *testing.T) {
	// Un projet déclaré avant D70 : son fichier porte .bin/ en root, son lien php
	// y vit, et ses tâches le désignent. Un apply doit tout passer à
	// ~/.local/bin, puis retirer l'ancien répertoire.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4")
	h.mustRun("cron", "add", "shopfr", "purge", "--schedule", "0 3 * * *", "--command", "bin/console app:purge")
	h.mustRun("apply", "shopfr", "--yes")

	decl := h.path("etc", "ogs", "projects", "shopfr.yaml")
	data, err := os.ReadFile(decl)
	if err != nil {
		t.Fatal(err)
	}
	ancien := strings.Replace(string(data), "path: .ssh", "path: .bin\n        mode: \"0710\"\n        owner: root\n      - path: .ssh", 1)
	if ancien == string(data) {
		t.Fatalf("déclaration inattendue, impossible d'y réintroduire .bin :\n%s", data)
	}
	if err := os.WriteFile(decl, []byte(ancien), 0o600); err != nil {
		t.Fatal(err)
	}
	home := h.path("home", "shop.fr")
	if err := os.RemoveAll(filepath.Join(home, ".local")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".bin"), 0o710); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/php8.3", filepath.Join(home, ".bin", "php")); err != nil {
		t.Fatal(err)
	}

	out := h.mustRun("apply", "shopfr", "--yes")
	if !strings.Contains(out, "retrait de l'ancien") {
		t.Errorf("l'apply doit annoncer le retrait de l'ancien .bin :\n%s", out)
	}
	if _, err := os.Lstat(filepath.Join(home, ".bin")); !os.IsNotExist(err) {
		t.Error("l'ancien .bin doit avoir disparu")
	}
	if target, err := os.Readlink(filepath.Join(home, ".local", "bin", "php")); err != nil || target == "" {
		t.Errorf("le lien php doit vivre dans .local/bin : %q (%v)", target, err)
	}
	cron := h.read(t, "etc/cron.d/ogs-shopfr")
	if strings.Contains(cron, "/.bin/") || !strings.Contains(cron, "/.local/bin/php") {
		t.Errorf("la tâche doit désigner le nouveau lien :\n%s", cron)
	}
	// « apply » n'écrit pas la déclaration : l'ancienne entrée y reste jusqu'au
	// prochain enregistrement, mais la normalisation l'écarte à chaque chargement,
	// et rien ne recrée .bin.
	h.simulateSystemEffects("shopfr", "shop.fr", 1004)
	if out := h.mustRun("apply", "shopfr", "--yes"); !strings.Contains(out, "aucun changement") {
		t.Errorf("la migration faite, un apply rejoué doit être vide :\n%s", out)
	}
	if _, err := os.Lstat(filepath.Join(home, ".bin")); !os.IsNotExist(err) {
		t.Error("l'ancien .bin ne doit pas être recréé")
	}
}

func TestMigrationDesJournauxHorsDuHome(t *testing.T) {
	// Un projet déclaré avant D71 : sa déclaration désigne <home>/logs, et ses
	// journaux y vivent, rotés compris. Un apply doit tout reprendre sous
	// logs.root et laisser un lien à la place.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-plain")
	h.mustRun("apply", "shopfr", "--yes")

	home := h.path("home", "shop.fr")
	decl := h.path("etc", "ogs", "projects", "shopfr.yaml")
	data, _ := os.ReadFile(decl)
	nouveau := h.path("var", "log", "ogs-projects", "shopfr")
	ancien := strings.Replace(string(data), "logDir: "+nouveau, "logDir: "+filepath.Join(home, "logs"), 1)
	if ancien == string(data) {
		t.Fatalf("déclaration inattendue :\n%s", data)
	}
	if err := os.WriteFile(decl, []byte(ancien), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, "logs")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(nouveau); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(home, "logs")
	if err := os.MkdirAll(legacy, 0o710); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"apache-access.log":   "accès récent\n",
		"apache-access.log.1": "accès d'hier\n",
	} {
		if err := os.WriteFile(filepath.Join(legacy, name), []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	out := h.mustRun("apply", "shopfr", "--yes")
	if !strings.Contains(out, "reprise des journaux") {
		t.Errorf("l'apply doit annoncer la reprise :\n%s", out)
	}
	if got, _ := os.ReadFile(filepath.Join(nouveau, "apache-access.log")); string(got) != "accès récent\n" {
		t.Errorf("journal courant mal repris : %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(nouveau, "apache-access.log.1")); string(got) != "accès d'hier\n" {
		t.Errorf("journal roté mal repris : %q", got)
	}
	if target, err := os.Readlink(legacy); err != nil || target != nouveau {
		t.Errorf("~/logs doit désormais être un lien vers %s : %q (%v)", nouveau, target, err)
	}
	if vhost := h.read(t, "etc/apache2/sites-available/shopfr.conf"); strings.Contains(vhost, home+"/logs") ||
		!strings.Contains(vhost, nouveau) {
		t.Errorf("le vhost doit journaliser sous logs.root :\n%s", vhost)
	}

	// La suppression archive les journaux puis les retire : ils ne vivent plus
	// dans l'arborescence du projet.
	out = h.mustRunPartial("project", "delete", "shopfr", "--yes")
	if _, err := os.Stat(nouveau); !os.IsNotExist(err) {
		t.Errorf("les journaux du projet supprimé doivent partir :\n%s", out)
	}
}
