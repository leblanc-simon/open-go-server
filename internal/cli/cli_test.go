package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func (h *harness) path(elem ...string) string {
	return filepath.Join(append([]string{h.prefix}, elem...)...)
}

// declareAccounts simule ce que useradd et groupadd auraient écrit, afin
// d'éprouver l'idempotence complète sans créer de comptes réels.
func (h *harness) declareAccounts(id string, uid int) {
	h.t.Helper()
	home := h.path("home", "shop.fr")
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
	h.declareAccounts("shopfr", 1004)
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
