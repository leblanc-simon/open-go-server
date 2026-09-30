package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"leblanc.io/open-go-server/internal/archive"
	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	appprov "leblanc.io/open-go-server/internal/provider/app"
	runtimeprov "leblanc.io/open-go-server/internal/provider/runtime"
	tlsprov "leblanc.io/open-go-server/internal/provider/tls"
	"leblanc.io/open-go-server/internal/secret"
	"leblanc.io/open-go-server/internal/spec"
	"leblanc.io/open-go-server/internal/store"
)

// harness exécute la CLI en mode préfixé, ce qui permet d'éprouver le flux
// complet sans privilèges et sans toucher au système réel.
type harness struct {
	t      *testing.T
	prefix string
	// stdin est l'entrée standard de la prochaine commande, puis vidée : jamais
	// celle du processus de test, qu'une saisie de secret attendrait.
	stdin string
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
	root.SetIn(strings.NewReader(h.stdin))
	h.stdin = ""
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

// Un identifiant qui en prolonge un autre (« shop_api » face à « shop », ou le
// « <id>_<hachage> » d'un host tronqué) n'en fait pas une ressource du premier :
// l'apply de shop retirait comme obsolètes le pool, le vhost et le lien
// d'activation de son voisin, avec le code 0, et l'apply du parc les supprimait
// puis les recréait à chaque passage.
func TestApplyNeRetireJamaisLesFichiersDUnVoisinAuNomProche(t *testing.T) {
	for _, cas := range []struct {
		nom         string
		court, long []string
		idCourt     string
		prefixeLong string
	}{
		{nom: "identifiant forcé",
			court: []string{"shop.fr", "--id", "shop"}, long: []string{"api.shop.fr", "--id", "shop_api"},
			idCourt: "shop", prefixeLong: "shop_api"},
		{nom: "host tronqué",
			court:   []string{"maboutiquedeventeen.li"},
			long:    []string{"ma-boutique-de-vente-en-ligne-de-chaussures.fr"},
			idCourt: "maboutiquedeventeenli", prefixeLong: "maboutiquedeventeenli_"},
	} {
		t.Run(cas.nom, func(t *testing.T) {
			h := newHarness(t)
			h.mustRun("init")
			h.mustRun(append(append([]string{"project", "create"}, cas.court...),
				"--kind", "php-symfony", "--php", "8.5")...)
			h.mustRun(append(append([]string{"project", "create"}, cas.long...),
				"--kind", "php-symfony", "--php", "8.5")...)
			var long string
			for _, f := range strings.Fields(h.mustRun("project", "list")) {
				if strings.HasPrefix(f, cas.prefixeLong) {
					long = f
				}
			}
			if long == "" || long == cas.idCourt {
				t.Fatalf("identifiant du voisin introuvable (préfixe %s)", cas.prefixeLong)
			}
			h.mustRunPartial("apply", long, "--yes")
			fichiers := []string{
				filepath.Join("etc", "php", "8.5", "fpm", "pool.d", long+".conf"),
				filepath.Join("etc", "apache2", "sites-available", long+".conf"),
				filepath.Join("etc", "apache2", "sites-enabled", long+".conf"),
			}

			plan := h.mustRun("plan", cas.idCourt)
			if strings.Contains(plan, "obsolète") {
				t.Errorf("le plan de %s ne doit rien retirer du voisin %s :\n%s", cas.idCourt, long, plan)
			}
			h.mustRunPartial("apply", cas.idCourt, "--yes")
			for _, f := range fichiers {
				if _, err := os.Lstat(h.path(f)); err != nil {
					t.Errorf("%s du projet %s retiré par l'apply de %s : %v", f, long, cas.idCourt, err)
				}
			}
			// Le parc converge : un second apply de tout le parc n'a plus rien à
			// retirer ni à recréer.
			h.mustRunPartial("apply", "--yes")
			if plan := h.mustRun("plan"); strings.Contains(plan, "obsolète") {
				t.Errorf("le parc ne converge pas :\n%s", plan)
			}
		})
	}
}

// Le sens inverse : le vhost « b2b » ou le pool « api » ajoutés à la déclaration
// de shop s'écrivent dans shop_b2b.conf et shop_api.conf, les fichiers
// principaux de deux voisins. « apply shop » ne validait l'unicité que sur sa
// cible (R1-07, corrigé par D94) : il réécrivait le fichier du voisin à son compte, avec le code 0 — le
// pool de shop_api tournait sous l'utilisateur de shop. Le fichier d'un autre
// projet est un conflit, --force compris.
func TestApplyCibleNeReecritJamaisLeFichierDUnVoisin(t *testing.T) {
	for _, cas := range []struct {
		nom, host, voisin, fichier string
		// ajout transforme la déclaration de shop.
		ajout func(string) string
	}{
		{nom: "vhost", host: "b2b.shop.fr", voisin: "shop_b2b",
			fichier: filepath.Join("etc", "apache2", "sites-available", "shop_b2b.conf"),
			ajout: func(decl string) string {
				return strings.Replace(decl, "  domains:\n    - name: shop.fr\n      role: primary\n",
					"  domains:\n    - name: shop.fr\n      role: primary\n    - name: pro.shop.fr\n      role: alias\n"+
						"  web:\n    vhosts:\n      - name: web\n        domains: [shop.fr]\n"+
						"      - name: b2b\n        domains: [pro.shop.fr]\n", 1)
			}},
		{nom: "pool", host: "api.shop.fr", voisin: "shop_api",
			fichier: filepath.Join("etc", "php", "8.5", "fpm", "pool.d", "shop_api.conf"),
			ajout: func(decl string) string {
				return decl + "      - name: api\n        version: \"8.5\"\n"
			}},
	} {
		t.Run(cas.nom, func(t *testing.T) {
			h := newHarness(t)
			h.mustRun("init")
			h.mustRun("project", "create", "shop.fr", "--id", "shop", "--kind", "php-symfony", "--php", "8.5")
			h.mustRun("project", "create", cas.host, "--id", cas.voisin, "--kind", "php-symfony", "--php", "8.5")
			h.mustRunPartial("apply", "--yes")
			avant := h.read(t, cas.fichier)

			decl := filepath.Join("etc", "ogs", "projects", "shop.yaml")
			modifie := cas.ajout(h.read(t, decl))
			if modifie == h.read(t, decl) {
				t.Fatal("la déclaration de shop n'a pas été modifiée")
			}
			if err := os.WriteFile(h.path(decl), []byte(modifie), 0o644); err != nil {
				t.Fatal(err)
			}

			for _, args := range [][]string{
				{"plan", "shop"},
				{"apply", "shop", "--yes"},
				{"apply", "shop", "--yes", "--force"},
			} {
				out, code := h.run(args...)
				if code != CodeConflict {
					t.Errorf("« ogs %s » : code %d, attendu %d (conflit) :\n%s",
						strings.Join(args, " "), code, CodeConflict, out)
				}
				// Depuis D94, le contrôle d'unicité, éprouvé contre le parc entier,
				// arrête la commande avant même le plan ; fs.CheckOwner reste la
				// seconde ligne. L'un comme l'autre nomme le voisin.
				if !strings.Contains(out, "est le fichier du projet "+cas.voisin) &&
					!strings.Contains(out, "(demandé par \""+cas.voisin+"\")") {
					t.Errorf("« ogs %s » doit nommer le propriétaire du fichier :\n%s", strings.Join(args, " "), out)
				}
			}
			if got := h.read(t, cas.fichier); got != avant {
				t.Errorf("le fichier de %s a été réécrit par shop :\n%s", cas.voisin, got)
			}
		})
	}
}

// Le vhost de supervision porte « projet: parc » dans son en-tête, et « parc »
// est un identifiant valide. Pour un projet ainsi nommé, ogs-status.conf passait
// pour l'un de ses vhosts obsolètes : chaque apply le retirait, le plan global le
// recréait, et le plan du projet n'était jamais vide.
func TestUnProjetNommeParcNeRetirePasLeVHostDeSupervision(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "parc.fr", "--id", "parc", "--kind", "php-symfony", "--php", "8.5")
	h.mustRunPartial("apply", "--yes")
	status := filepath.Join("etc", "apache2", "sites-available", "ogs-status.conf")
	avant := h.read(t, status)

	if plan := h.mustRun("plan", "parc"); strings.Contains(plan, "obsolète") {
		t.Errorf("le vhost de supervision n'est pas un vhost du projet parc :\n%s", plan)
	}
	out := h.mustRunPartial("apply", "parc", "--yes")
	if strings.Contains(out, "obsolète") {
		t.Errorf("l'apply de parc ne doit rien retirer :\n%s", out)
	}
	if got := h.read(t, status); got != avant {
		t.Errorf("le vhost de supervision a été retiré puis recréé :\n%s", got)
	}
}

// Le pool et le vhost d'un projet s'appellent tous deux shopfr.conf. Une bascule
// de version sous --force réécrit le vhost retouché puis retire l'ancien pool
// retouché, dans la même seconde : la seconde sauvegarde écrasait la première,
// et la retouche du vhost était perdue alors que le plan l'annonçait sauvegardée.
func TestUnApplyForceGardeChaqueSauvegardeDeMemeNom(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("apply", "shopfr", "--yes")
	retouches := map[string]string{
		filepath.Join("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf"): "pm.max_children = 4\n",
		filepath.Join("etc", "apache2", "sites-available", "shopfr.conf"):  "# redirection provisoire\n",
	}
	attendus := map[string]bool{}
	for f, ajout := range retouches {
		contenu := h.read(t, f) + ajout
		if err := os.WriteFile(h.path(f), []byte(contenu), 0o644); err != nil {
			t.Fatal(err)
		}
		attendus[contenu] = true
	}

	h.mustRun("php", "switch", "shopfr", "8.3", "--force")
	out := h.mustRunPartial("apply", "shopfr", "--yes", "--force")
	if !strings.Contains(out, "écrasement forcé") || !strings.Contains(out, "retrait forcé") {
		t.Fatalf("l'apply doit réécrire le vhost et retirer l'ancien pool sous --force :\n%s", out)
	}
	baks, _ := filepath.Glob(filepath.Join(h.path("var", "lib", "ogs", "backups"), "shopfr.conf.ogs-bak-*"))
	for _, b := range baks {
		data, _ := os.ReadFile(b)
		delete(attendus, string(data))
	}
	if len(attendus) != 0 {
		t.Errorf("%d retouche(s) sans sauvegarde parmi %d copie(s) : %v", len(attendus), len(baks), baks)
	}
}

// Un nom de fichier de vhost qu'Apache ne chargerait pas est une faute de
// déclaration (code 3), non un conflit.
func TestUnFichierDeVHostSansExtensionSurApacheEstUneErreurDeValidation(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	decl := filepath.Join("etc", "ogs", "projects", "shopfr.yaml")
	contenu := strings.Replace(h.read(t, decl), "metadata:\n", "metadata:\n  labels:\n    adopted: \"true\"\n", 1)
	contenu = strings.Replace(contenu, "spec:\n",
		"spec:\n  web:\n    vhosts:\n      - name: shop.fr\n        file: shop.fr\n        domains: [shop.fr]\n", 1)
	if err := os.WriteFile(h.path(decl), []byte(contenu), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := h.run("plan", "shopfr")
	if code != CodeValidation || !strings.Contains(out, "sites-enabled/*.conf") {
		t.Errorf("code %d, attendu %d :\n%s", code, CodeValidation, out)
	}
}

// Le fichier d'un projet qui n'est plus déclaré — laissé par une suppression —
// est toujours refusé, mais le message ne renvoie plus à « ogs apply » d'un
// projet disparu.
func TestLeFichierDUnProjetDisparuNeRenvoiePasASonApply(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "api.shop.fr", "--id", "shop_api", "--kind", "php-symfony", "--php", "8.5")
	h.mustRunPartial("apply", "--yes")
	if err := os.Remove(h.path("etc", "ogs", "projects", "shop_api.yaml")); err != nil {
		t.Fatal(err)
	}
	h.mustRun("project", "create", "shop.fr", "--id", "shop", "--kind", "php-symfony", "--php", "8.5")
	decl := filepath.Join("etc", "ogs", "projects", "shop.yaml")
	contenu := h.read(t, decl) + "      - name: api\n        version: \"8.5\"\n"
	if err := os.WriteFile(h.path(decl), []byte(contenu), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := h.run("apply", "shop", "--yes", "--force")
	if code != CodeConflict || !strings.Contains(out, "n'est plus déclaré") || strings.Contains(out, "ogs apply shop_api") {
		t.Errorf("code %d, attendu %d, et un message sans renvoi à l'apply du disparu :\n%s", code, CodeConflict, out)
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

func TestPHPSwitchNeSeDesarmePasParUnLien(t *testing.T) {
	// composer.json n'est plus lu à travers un lien : un refus traité en simple
	// avertissement laissait alors l'utilisateur désarmer le garde-fou à volonté,
	// sous un « ✓ compatibilité vérifiée ». Il décide comme un fichier absent :
	// par la table indicative, qui refuse PHP 8.3 à un Prestashop.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "boutique.fr", "--kind", "php-prestashop", "--php", "7.4")
	h.mustRun("apply", "boutiquefr", "--yes")
	permissif := h.path("root", "composer.json")
	if err := os.MkdirAll(filepath.Dir(permissif), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(permissif, []byte(`{"require":{"php":">=7.1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(permissif, h.path("home", "boutique.fr", "www", "composer.json")); err != nil {
		t.Fatal(err)
	}

	out, code := h.run("php", "switch", "boutiquefr", "8.3")
	if code != CodeValidation {
		t.Fatalf("la table indicative doit refuser PHP 8.3 (code %d, attendu %d) :\n%s", code, CodeValidation, out)
	}
	if !strings.Contains(out, "lien symbolique") || !strings.Contains(out, "table indicative") {
		t.Errorf("le refus doit dire que composer.json n'a pas été lu, et sur quoi il se fonde :\n%s", out)
	}
	out = h.mustRun("php", "switch", "boutiquefr", "8.1", "--dry-run")
	if strings.Contains(out, "✓ compatibilité vérifiée") || !strings.Contains(out, "composer.json non lu") {
		t.Errorf("une compatibilité jugée sans composer.json ne s'annonce pas vérifiée :\n%s", out)
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

func TestDatabaseUserRefuseLesComptesReserves(t *testing.T) {
	// « db user add shopfr root » était accepté, et « db user rotate shopfr root »
	// partait vers le moteur changer le mot de passe du compte d'administration
	// (D91). Le compte de supervision configuré est réservé au même titre.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static", "--db", "mariadb")
	for _, name := range []string{"root", "mysql", "telegraf"} {
		if out, code := h.run("db", "user", "add", "shopfr", name); code != CodeValidation ||
			!strings.Contains(out, "réservé") {
			t.Errorf("%s : code %d attendu %d :\n%s", name, code, CodeValidation, out)
		}
	}
	path := h.path("etc", "ogs", "projects", "shopfr.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "extraUsers") {
		t.Errorf("le compte refusé ne doit pas être écrit :\n%s", data)
	}
	// Un projet déclaré à la main avec root est refusé, et la rotation avec lui :
	// elle ne l'atteint jamais.
	patched := strings.Replace(string(data), "      user: shopfr\n",
		"      user: shopfr\n      extraUsers:\n        - user: root\n          privileges: all\n", 1)
	if patched == string(data) {
		t.Fatalf("déclaration inattendue :\n%s", data)
	}
	if err := os.WriteFile(path, []byte(patched), 0o640); err != nil {
		t.Fatal(err)
	}
	if out, code := h.run("db", "user", "rotate", "shopfr", "root", "--yes"); code != CodeValidation ||
		!strings.Contains(out, "réservé") {
		t.Errorf("rotation de root : code %d attendu %d :\n%s", code, CodeValidation, out)
	}
}

func TestUnCompteReserveDejaDeclareNArretePasLeParc(t *testing.T) {
	// Refusé au chargement, un seul projet d'avant D91 déclarant root rendait le
	// code 3 à tout le parc : plan et apply d'un projet sain, « cert renew » du
	// timer, et la suppression du fautif comme d'un voisin (D91).
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("project", "create", "shop.fr", "--kind", "static", "--db", "mariadb")
	h.mustRun("project", "create", "voisin.fr", "--kind", "static")
	path := h.path("etc", "ogs", "projects", "shopfr.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(data), "      user: shopfr\n", "      user: root\n", 1)
	if patched == string(data) {
		t.Fatalf("déclaration inattendue :\n%s", data)
	}
	if err := os.WriteFile(path, []byte(patched), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"plan", "voisinfr"}, {"apply", "voisinfr", "--yes"}, {"plan"}, {"cert", "renew", "--all"},
	} {
		if out, code := h.run(args...); code == CodeValidation || code == CodeError {
			t.Errorf("%v : code %d :\n%s", args, code, out)
		}
	}
	// Le projet fautif voit sa base refusée, sans que rien ne soit exécuté.
	if out, code := h.run("plan", "shopfr"); code == CodeValidation || !strings.Contains(out, "réservé") {
		t.Errorf("la base au compte réservé doit être refusée dans le plan, code %d :\n%s", code, out)
	}
	for _, id := range []string{"voisinfr", "shopfr"} {
		if out, code := h.run("project", "delete", id, "--yes", "--no-dump"); code != CodeOK && code != CodePartial {
			t.Errorf("suppression de %s : code %d :\n%s", id, code, out)
		}
	}
}

func TestDbAddRefuseUnJeuBorneSurLeProprietairePostgreSQL(t *testing.T) {
	// Le compte principal d'une base PostgreSQL en est propriétaire : « readonly »
	// n'y a aucun effet (D92). Le jeu borné se déclare sur un compte additionnel.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	out, code := h.run("db", "add", "shopfr", "--engine", "postgres", "--privileges", "readonly")
	if code != CodeUsage || !strings.Contains(out, "db user add") {
		t.Errorf("code %d attendu %d, et la porte de sortie nommée :\n%s", code, CodeUsage, out)
	}
	// MySQL applique le jeu borné au compte principal : il reste admis.
	h.mustRun("db", "add", "shopfr", "--engine", "mysql", "--privileges", "readonly")
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
	// Les secrets passent par l'entrée standard, une ligne chacun (D93).
	h.stdin = "as\nck\n"
	out := h.mustRun("cert", "dns-provider", "set", "ovh", "--application-key", "ak")
	if strings.Contains(out, "déconseillé") || strings.Contains(out, "⚠") {
		t.Errorf("aucun avertissement attendu sans option porteuse de secret :\n%s", out)
	}
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
	if !strings.Contains(content, "applicationKey: ak") || !strings.Contains(content, "endpoint: ovh-eu") ||
		!strings.Contains(content, "applicationSecret: as") || !strings.Contains(content, "consumerKey: ck") {
		t.Errorf("contenu inattendu :\n%s", content)
	}
	// Une entrée standard incomplète est une erreur d'usage, pas un secret vide.
	h.stdin = "as\n"
	if out, code := h.run("cert", "dns-provider", "set", "ovh", "--application-key", "ak"); code != CodeUsage ||
		!strings.Contains(out, "clé de consommateur requis") {
		t.Errorf("secret manquant : code %d attendu %d :\n%s", code, CodeUsage, out)
	}
	// L'option porteuse de valeur reste acceptée, mais signalée.
	out = h.mustRun("cert", "dns-provider", "set", "ovh", "--client-id", "ci", "--client-secret", "cs")
	if !strings.Contains(out, "--client-secret : la valeur figure dans la ligne de commande") {
		t.Errorf("l'option porteuse du secret doit être signalée :\n%s", out)
	}
}

func TestCertDNSProviderPlusieursJeux(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.stdin = "as\nck\n"
	out := h.mustRun("cert", "dns-provider", "set", "client-a", "--zone", "Client-A.fr", "--zone", "client-a.com",
		"--application-key", "ak")
	if !strings.Contains(out, "zones : client-a.fr, client-a.com") {
		t.Errorf("zones annoncées :\n%s", out)
	}
	content := h.read(t, filepath.Join("etc", "ogs", "credentials", "dns", "client-a.yaml"))
	if !strings.Contains(content, "provider: ovh") || !strings.Contains(content, "- client-a.fr") {
		t.Errorf("contenu inattendu :\n%s", content)
	}
	// Une zone déjà déclarée par un autre jeu rendrait le choix indéterminé.
	h.stdin = "cs\n"
	if out, code := h.run("cert", "dns-provider", "set", "client-b", "--zone", "client-a.fr",
		"--client-id", "ci"); code != CodeConflict || !strings.Contains(out, "déjà déclarée par le jeu client-a") {
		t.Errorf("zone en double : code %d attendu %d :\n%s", code, CodeConflict, out)
	}
	// Renouveler une clé sans --zone garde les zones : sans quoi les certificats
	// du jeu repasseraient en http-01 sans un mot.
	h.stdin = "cs\n"
	h.mustRun("cert", "dns-provider", "set", "client-a", "--client-id", "ci")
	content = h.read(t, filepath.Join("etc", "ogs", "credentials", "dns", "client-a.yaml"))
	if !strings.Contains(content, "- client-a.com") || strings.Contains(content, "applicationKey") {
		t.Errorf("zones conservées, clés remplacées :\n%s", content)
	}
	if out, code := h.run("cert", "dns-provider", "set", "../x", "--client-id", "ci"); code != CodeUsage {
		t.Errorf("nom qui sort du répertoire : code %d :\n%s", code, out)
	}
	h.stdin = "as\nck\n"
	h.mustRun("cert", "dns-provider", "set", "ovh", "--application-key", "ak")
	out = h.mustRun("cert", "dns-provider", "list")
	if !strings.Contains(out, "client-a") || !strings.Contains(out, "client-a.com client-a.fr") ||
		!strings.Contains(out, "dns-01 demandé seulement") {
		t.Errorf("liste :\n%s", out)
	}
	// La liste ne montre jamais un identifiant.
	json := h.mustRun("cert", "dns-provider", "list", "-o", "json")
	if strings.Contains(json, "clientSecret") || strings.Contains(json, "applicationKey") ||
		!strings.Contains(json, `"zones"`) {
		t.Errorf("identifiant affiché :\n%s", json)
	}
	// Un jeu lisible de tous est écarté, nommé, et rend le code 6.
	if err := os.Chmod(h.path("etc", "ogs", "credentials", "dns", "ovh.yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := h.run("cert", "dns-provider", "list"); code != CodePartial ||
		!strings.Contains(out, "lisible au-delà de son propriétaire") {
		t.Errorf("jeu lisible de tous : code %d :\n%s", code, out)
	}
}

func TestUneZoneDeclareeFaitEmettreParDNS01(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.stdin = "cs\n"
	h.mustRun("cert", "dns-provider", "set", "client-a", "--zone", "client-a.fr", "--client-id", "ci")
	h.mustRun("project", "create", "client-a.fr", "--kind", "static")
	out, _ := h.run("plan", "clientafr")
	if !strings.Contains(out, "dns-01 par la zone déclarée, repli http-01") {
		t.Errorf("le plan doit annoncer le challenge dns-01 de la zone :\n%s", out)
	}
	out, _ = h.run("cert", "issue", "clientafr", "--challenge", "http-01", "--dry-run")
	if !strings.Contains(out, "(http-01)") {
		t.Errorf("un challenge imposé l'emporte :\n%s", out)
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

// modifierDomaines réécrit les domaines déclarés d'un projet, comme le ferait
// l'exploitant dans son fichier.
func (h *harness) modifierDomaines(t *testing.T, id string, modifier func([]model.Domain) []model.Domain) {
	t.Helper()
	path := h.path("etc", "ogs", "projects", id+".yaml")
	p, err := spec.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p.Spec.Domains = modifier(p.Spec.Domains)
	if err := spec.Save(path, p); err != nil {
		t.Fatal(err)
	}
}

// nomsDuCertificat rend les noms que couvre un certificat présent sous le préfixe.
func (h *harness) nomsDuCertificat(t *testing.T, name string) []string {
	t.Helper()
	info, ok, err := tlsprov.Store{Paths: config.NewPaths(h.prefix)}.Read(name)
	if err != nil || !ok {
		t.Fatalf("certificat %s illisible : %v", name, err)
	}
	return info.Domains
}

// vhostEtBloc443 rend le chemin du vhost principal de shopfr et la marque de son
// bloc 443, selon le moteur.
func vhostEtBloc443(moteur string) (string, string) {
	if moteur == "nginx" {
		return filepath.Join("etc", "nginx", "sites-available", "shopfr.conf"), "listen 443 ssl;"
	}
	return filepath.Join("etc", "apache2", "sites-available", "shopfr.conf"), "<VirtualHost *:443>"
}

func TestAjoutDAliasNeRetirePasLeHTTPSDuSite(t *testing.T) {
	// shop.fr est en ligne en HTTPS, HSTS compris ; l'exploitant ajoute un alias,
	// souvent avant d'en avoir basculé le DNS. Le vhost était réécrit en HTTP
	// seul, rechargé ainsi avant le challenge, et le restait quand l'émission
	// échouait : HSTS rendait le site injoignable à qui l'avait déjà visité, et
	// les autres recevaient sur le port 443 le site d'un autre client.
	for _, moteur := range []string{"apache", "nginx"} {
		t.Run(moteur, func(t *testing.T) {
			h := newHarness(t)
			h.mustRun("init")
			if moteur == "nginx" {
				h.choisirNginx(t)
				// Le paquet nginx crée ses répertoires ; le préfixe ne porte que ceux
				// d'Apache.
				for _, d := range []string{"sites-available", "sites-enabled"} {
					if err := os.MkdirAll(h.path("etc", "nginx", d), 0o755); err != nil {
						t.Fatal(err)
					}
				}
			}
			vhostRel, bloc443 := vhostEtBloc443(moteur)
			h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5")
			h.mustRunPartial("apply", "shopfr", "--yes")
			if avant := h.read(t, vhostRel); !strings.Contains(avant, bloc443) {
				t.Fatalf("le site doit être servi en HTTPS au départ :\n%s", avant)
			}

			h.modifierDomaines(t, "shopfr", func(d []model.Domain) []model.Domain {
				return append(d, model.Domain{Name: "boutique.fr", Role: "alias"})
			})
			// Une autorité qui refuse boutique.fr.
			h.suspendreACME(t, "shop.fr", "boutique.fr")

			// Le plan n'annonce plus le retrait du bloc 443 ni du HSTS. L'en-tête,
			// dont l'empreinte peut contenir « 443 », n'est pas du rendu.
			diff, _ := h.run("diff", "shopfr")
			for _, l := range strings.Split(diff, "\n") {
				l = strings.TrimSpace(l)
				if strings.HasPrefix(l, "-") && !strings.Contains(l, "empreinte: sha256:") &&
					(strings.Contains(l, bloc443) || strings.Contains(l, "SSLEngine") ||
						strings.Contains(l, "ssl_certificate") || strings.Contains(l, "Strict-Transport-Security")) {
					t.Errorf("le diff retire le HTTPS du site : %q\n%s", l, diff)
				}
			}

			out, code := h.run("apply", "shopfr", "--yes")
			if code != CodePartial || !strings.Contains(out, "suspendues") {
				t.Fatalf("l'émission devait échouer en avertissement (code %d) :\n%s", code, out)
			}
			apres := h.read(t, vhostRel)
			if !strings.Contains(apres, bloc443) || !strings.Contains(apres, "Strict-Transport-Security") {
				t.Errorf("le site doit rester servi en HTTPS, HSTS compris, sur le certificat en place :\n%s", apres)
			}
			if !strings.Contains(apres, "certificates/shopfr/fullchain.pem") {
				t.Errorf("le vhost doit désigner le certificat en place :\n%s", apres)
			}
			if !strings.Contains(apres, "boutique.fr") {
				t.Errorf("le nouvel alias doit être servi, ne serait-ce que pour son challenge :\n%s", apres)
			}
			// L'état dit ce que voit le visiteur : un HTTPS servi, un alias en attente.
			status, _ := h.run("status", "shopfr")
			if !strings.Contains(status, "incomplet") || strings.Contains(status, "absent") ||
				!strings.Contains(status, "boutique.fr pas encore couvert") {
				t.Errorf("le certificat doit être annoncé incomplet, non absent :\n%s", status)
			}

			// La cause levée, le timer de renouvellement reprend l'émission seul.
			db, err := store.Open(h.path("var", "lib", "ogs", "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			_ = db.ClearACMEFailures("shop.fr,boutique.fr")
			db.Close()
			renew := h.mustRun("cert", "renew", "--all")
			if !strings.Contains(renew, "shopfr renouvelé") {
				t.Errorf("le timer doit réémettre un certificat qui ne couvre plus la demande :\n%s", renew)
			}
			if noms := h.nomsDuCertificat(t, "shopfr"); !slices.Contains(noms, "boutique.fr") {
				t.Errorf("le certificat réémis doit couvrir l'alias : %v", noms)
			}
			// Et le vhost, déjà prêt, n'a plus rien à changer.
			plan := h.mustRun("plan", "shopfr")
			for _, interdit := range []string{"~ vhost", "~ certificat", "activation TLS", "bascule du vhost"} {
				if strings.Contains(plan, interdit) {
					t.Errorf("%q inattendu après la réémission :\n%s", interdit, plan)
				}
			}
		})
	}
}

func TestAjoutDAliasRechargeLeServeurApresLEmission(t *testing.T) {
	// Le vhost d'un alias ajouté ne change plus après l'émission : c'est l'étape
	// du certificat qui doit recharger le serveur, sans quoi il présente
	// indéfiniment l'ancien matériel, qui ne couvre pas l'alias.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5")
	h.mustRunPartial("apply", "shopfr", "--yes")
	h.modifierDomaines(t, "shopfr", func(d []model.Domain) []model.Domain {
		return append(d, model.Domain{Name: "boutique.fr", Role: "alias"})
	})
	out := h.mustRunPartial("apply", "shopfr", "--yes")
	if strings.Contains(out, "activation TLS") {
		t.Errorf("le vhost ne doit jamais repasser par le HTTP seul :\n%s", out)
	}
	iCert := strings.Index(out, "✓ certificat shopfr : domaines à couvrir modifiés")
	iReload := strings.LastIndex(out, "↻ rechargement de apache2")
	if iCert < 0 || iReload < iCert {
		t.Errorf("un rechargement doit suivre l'émission (certificat=%d rechargement=%d) :\n%s",
			iCert, iReload, out)
	}
	if noms := h.nomsDuCertificat(t, "shopfr"); !slices.Contains(noms, "boutique.fr") {
		t.Errorf("le certificat doit couvrir l'alias : %v", noms)
	}
}

func TestCertIssueRechargeLeServeurWeb(t *testing.T) {
	// Un vhost déjà servi en HTTPS désigne son certificat par son chemin : une
	// réémission manuelle ne change pas son rendu, et « ogs apply » ne
	// rechargerait rien. C'est donc à « cert issue » de le faire.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "--yes")
	out := h.mustRun("cert", "issue", "shopfr")
	iEmis := strings.Index(out, "émis")
	iReload := strings.Index(out, "↻ rechargement de apache2")
	if iEmis < 0 || iReload < iEmis {
		t.Errorf("l'émission doit être suivie d'un rechargement :\n%s", out)
	}
}

func TestRedirectionCouverteParLeCertificatDuProjet(t *testing.T) {
	// Un domaine redirigé a autant besoin d'être chiffré (D43). Il était exclu du
	// certificat, et son vhost 443 servait pourtant le certificat du projet :
	// https://ancien-shop.fr rendait une erreur de nom au lieu de la 301.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--redirect", "ancien-shop.fr", "--yes")
	if noms := h.nomsDuCertificat(t, "shopfr"); !slices.Contains(noms, "ancien-shop.fr") {
		t.Errorf("le certificat du projet doit couvrir la redirection : %v", noms)
	}
	redirect := h.read(t, filepath.Join("etc", "apache2", "sites-available", "shopfr_redirect.conf"))
	if !strings.Contains(redirect, "<VirtualHost *:443>") ||
		!strings.Contains(redirect, "certificates/shopfr/fullchain.pem") {
		t.Errorf("la redirection doit être servie en HTTPS avec le certificat du projet :\n%s", redirect)
	}
	// Le certificat auto-signé que déclare « site create » suit la même règle.
	h.mustRun("site", "create", "atelier.fr", "--kind", "static", "--redirect", "ancien-atelier.fr",
		"--self-signed", "--yes")
	if noms := h.nomsDuCertificat(t, "atelierfr"); !slices.Contains(noms, "ancien-atelier.fr") {
		t.Errorf("le certificat déclaré doit couvrir la redirection : %v\n%s", noms,
			h.read(t, filepath.Join("etc", "ogs", "projects", "atelierfr.yaml")))
	}
}

func TestAliasDevenuRedirectionResteCouvertAuRenouvellement(t *testing.T) {
	// Un alias basculé en redirection restait couvert par le certificat… jusqu'au
	// renouvellement suivant, qui le réémettait sans lui : la redirection HTTPS
	// cassait en silence, une soixantaine de jours plus tard.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--alias", "ancien-shop.fr", "--yes")
	h.modifierDomaines(t, "shopfr", func(d []model.Domain) []model.Domain {
		for i := range d {
			if d[i].Name == "ancien-shop.fr" {
				d[i].Role, d[i].Target = "redirect", "shop.fr"
			}
		}
		return d
	})
	h.mustRunPartial("apply", "shopfr", "--yes")
	h.mustRun("cert", "renew", "shopfr", "--force")
	if noms := h.nomsDuCertificat(t, "shopfr"); !slices.Contains(noms, "ancien-shop.fr") {
		t.Errorf("le certificat renouvelé doit couvrir la redirection : %v", noms)
	}
	redirect := h.read(t, filepath.Join("etc", "apache2", "sites-available", "shopfr_redirect.conf"))
	if !strings.Contains(redirect, "<VirtualHost *:443>") {
		t.Errorf("la redirection doit rester servie en HTTPS :\n%s", redirect)
	}
}

// suspendreACME simule une autorité qui refuse une liste de domaines : le compteur
// porté au seuil suspend les tentatives, et l'émission de cette liste échoue comme
// après cinq refus. Toute autre liste — celle d'un repli — reste permise.
func (h *harness) suspendreACME(t *testing.T, domaines ...string) {
	t.Helper()
	db, err := store.Open(h.path("var", "lib", "ogs", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i < store.ACMEFailureThreshold; i++ {
		if _, _, err := db.RecordACMEFailure(strings.Join(domaines, ","), "challenge http-01 refusé"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUneRedirectionRefuseeNeCoutePasLeRenouvellementDuSite(t *testing.T) {
	// Un site en HTTPS reçoit une redirection dont le DNS pointe ailleurs. Mise dans
	// la demande du certificat (D43), elle faisait échouer toute la commande ACME :
	// le certificat du site ne se renouvelait plus, et le site tombait à
	// l'échéance, HSTS compris — des semaines après une mise à jour qui n'avait
	// rien cassé.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	h.modifierDomaines(t, "shopfr", func(d []model.Domain) []model.Domain {
		return append(d, model.Domain{Name: "ancien-shop.fr", Role: "redirect", Target: "shop.fr"})
	})
	h.suspendreACME(t, "shop.fr", "ancien-shop.fr")

	out, code := h.run("apply", "shopfr", "--yes")
	if code != CodePartial || !strings.Contains(out, "certificat shopfr sans ancien-shop.fr : le vhost reste en HTTP") {
		t.Errorf("la redirection non couverte doit être signalée (code %d) :\n%s", code, out)
	}
	sites := filepath.Join("etc", "apache2", "sites-available")
	if main := h.read(t, filepath.Join(sites, "shopfr.conf")); !strings.Contains(main, "<VirtualHost *:443>") {
		t.Errorf("le site reste servi en HTTPS :\n%s", main)
	}
	if redirect := h.read(t, filepath.Join(sites, "shopfr_redirect.conf")); strings.Contains(redirect, "<VirtualHost *:443>") {
		t.Errorf("aucun certificat ne couvre la redirection, aucun bloc 443 :\n%s", redirect)
	}

	chaîne := filepath.Join("var", "lib", "ogs", "acme", "certificates", "shopfr", "fullchain.pem")
	avant := h.read(t, chaîne)
	out, code = h.run("cert", "renew", "shopfr", "--force")
	if code != CodePartial {
		t.Errorf("la redirection écartée est un succès partiel (code %d) :\n%s", code, out)
	}
	for _, attendu := range []string{"✓ shopfr renouvelé", "ancien-shop.fr non couvert", "↻ rechargement de apache2"} {
		if !strings.Contains(out, attendu) {
			t.Errorf("%q absent :\n%s", attendu, out)
		}
	}
	if h.read(t, chaîne) == avant {
		t.Error("le certificat du site doit être renouvelé malgré la redirection refusée")
	}
	if noms := h.nomsDuCertificat(t, "shopfr"); strings.Join(noms, ",") != "shop.fr" {
		t.Errorf("renouvelé pour le site seul : %v", noms)
	}
}

func TestUnSiteNeufRecoitSonCertificatMalgreUneRedirectionRefusee(t *testing.T) {
	// La première émission d'un site dont la redirection ne pointe pas encore ici :
	// le site ne doit pas rester en HTTP pour autant.
	h := newHarness(t)
	h.mustRun("init")
	h.suspendreACME(t, "shop.fr", "ancien-shop.fr")
	out, code := h.run("site", "create", "shop.fr", "--kind", "static", "--redirect", "ancien-shop.fr", "--yes")
	if code != CodePartial || !strings.Contains(out, "émis pour shop.fr seulement") {
		t.Errorf("l'émission doit aboutir pour le site (code %d) :\n%s", code, out)
	}
	if noms := h.nomsDuCertificat(t, "shopfr"); strings.Join(noms, ",") != "shop.fr" {
		t.Errorf("certificat du site : %v", noms)
	}
	sites := filepath.Join("etc", "apache2", "sites-available")
	if main := h.read(t, filepath.Join(sites, "shopfr.conf")); !strings.Contains(main, "<VirtualHost *:443>") {
		t.Errorf("le site doit être basculé en HTTPS :\n%s", main)
	}
	redirect := h.read(t, filepath.Join(sites, "shopfr_redirect.conf"))
	if strings.Contains(redirect, "<VirtualHost *:443>") {
		t.Errorf("la redirection non couverte reste en HTTP :\n%s", redirect)
	}
	// Écrite quand rien n'était couvert, elle visait http://shop.fr ; le repli a
	// couvert le site, la bascule doit la réécrire vers https.
	if !strings.Contains(redirect, "https://shop.fr$1") {
		t.Errorf("la redirection doit viser le site en https une fois celui-ci couvert :\n%s", redirect)
	}
}

func TestUnAliasEstCouvertMalgreUneRedirectionRefusee(t *testing.T) {
	// Un site en HTTPS reçoit une redirection que l'autorité refuse et un alias
	// joignable. Le repli ne retenait que les noms déjà couverts, et seulement
	// près de l'échéance : l'alias n'était couvert ni à l'apply ni au timer, tant
	// que la redirection restait refusée — alors qu'avant elle, il l'aurait été.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	h.modifierDomaines(t, "shopfr", func(d []model.Domain) []model.Domain {
		return append(d, model.Domain{Name: "boutique.fr", Role: "alias"},
			model.Domain{Name: "ancien-shop.fr", Role: "redirect", Target: "shop.fr"})
	})
	h.suspendreACME(t, "shop.fr", "boutique.fr", "ancien-shop.fr")
	out, code := h.run("apply", "shopfr", "--yes")
	if code != CodePartial {
		t.Errorf("la redirection écartée est un succès partiel (code %d) :\n%s", code, out)
	}
	if noms := h.nomsDuCertificat(t, "shopfr"); strings.Join(noms, ",") != "shop.fr,boutique.fr" {
		t.Errorf("l'alias doit être couvert sans attendre la redirection : %v", noms)
	}
	// Le certificat est écrit et le serveur rechargé : l'étape n'a pas échoué.
	if strings.Contains(out, "✗ certificat") ||
		!strings.Contains(out, "✓ certificat shopfr : domaines à couvrir modifiés (shop.fr, boutique.fr, ancien-shop.fr) (en partie)") {
		t.Errorf("une émission repliée s'annonce aboutie en partie, non en échec :\n%s", out)
	}
	if !strings.Contains(out, "ancien-shop.fr non couvert") {
		t.Errorf("la redirection écartée doit être nommée :\n%s", out)
	}
}

func TestUneRedirectionDepuisUneAdresseIPNeBloquePasLeProjet(t *testing.T) {
	// La validation admet une adresse IP comme domaine. Mise dans la demande de
	// certificat, une redirection depuis une IP la faisait refuser entière : plus
	// de plan ni d'apply (code 3), « status » en erreur, et « cert renew --all »,
	// qui passe outre un projet en erreur, cessait sans un mot de renouveler le
	// site — le timer rendait 0 et le certificat expirait.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--redirect", "203.0.113.10", "--yes")
	if noms := h.nomsDuCertificat(t, "shopfr"); strings.Join(noms, ",") != "shop.fr" {
		t.Errorf("certificat du site : %v", noms)
	}
	if out, code := h.run("plan", "shopfr"); code == CodeValidation {
		t.Errorf("le projet doit rester planifiable :\n%s", out)
	}
	if status, _ := h.run("status", "shopfr"); strings.Contains(status, "erreur") {
		t.Errorf("le projet n'est pas en erreur :\n%s", status)
	}
	renew := h.mustRun("cert", "renew", "--all", "--force")
	if !strings.Contains(renew, "shopfr renouvelé") {
		t.Errorf("le renouvellement du parc doit traiter le site :\n%s", renew)
	}
	redirect := h.read(t, filepath.Join("etc", "apache2", "sites-available", "shopfr_redirect.conf"))
	if strings.Contains(redirect, "<VirtualHost *:443>") || !strings.Contains(redirect, "https://shop.fr$1") {
		t.Errorf("la redirection d'une IP reste en HTTP, vers le site en https :\n%s", redirect)
	}
	// Le certificat auto-signé que déclare « site create » suit la même règle.
	h.mustRun("site", "create", "atelier.fr", "--kind", "static", "--redirect", "203.0.113.20",
		"--self-signed", "--yes")
}

func TestLeRenouvellementDuParcSignaleUnProjetQuIlNePeutExaminer(t *testing.T) {
	// Un projet dont la demande de certificat est refusée — ici un alias en
	// adresse IP, qu'aucun certificat ACME ne peut porter — était passé en
	// silence par « cert renew --all » : le timer rendait 0, et le certificat du
	// projet allait à l'expiration sans que rien ne l'annonce.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	h.mustRun("site", "create", "atelier.fr", "--kind", "static", "--yes")
	h.modifierDomaines(t, "atelierfr", func(d []model.Domain) []model.Domain {
		return append(d, model.Domain{Name: "203.0.113.11", Role: "alias"})
	})
	out, code := h.run("cert", "renew", "--all", "--force")
	if code != CodePartial {
		t.Errorf("un projet non examiné est un échec partiel (code %d) :\n%s", code, out)
	}
	if !strings.Contains(out, "⚠ atelierfr : certificat non examiné, déclaration refusée") {
		t.Errorf("le projet écarté doit être nommé :\n%s", out)
	}
	// Les autres projets n'en dépendent pas.
	if !strings.Contains(out, "✓ shopfr renouvelé") || !strings.Contains(out, "↻ rechargement de apache2") {
		t.Errorf("le reste du parc doit être renouvelé et rechargé :\n%s", out)
	}
}

func TestLeTimerNeDevancePasLApply(t *testing.T) {
	// Un alias ajouté au fichier mais pas encore appliqué n'est servi par aucun
	// vhost : le timer qui réémettait pour lui échouait au challenge, et chaque
	// passage comptait un échec auprès de l'autorité, jusqu'à suspendre les
	// tentatives de l'apply qui suit.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	h.modifierDomaines(t, "shopfr", func(d []model.Domain) []model.Domain {
		return append(d, model.Domain{Name: "boutique.fr", Role: "alias"})
	})
	out := h.mustRun("cert", "renew", "--all")
	if !strings.Contains(out, "réémission laissée à « ogs apply shopfr »") {
		t.Errorf("la réémission doit attendre l'apply :\n%s", out)
	}
	if noms := h.nomsDuCertificat(t, "shopfr"); slices.Contains(noms, "boutique.fr") {
		t.Errorf("aucune émission avant l'apply : %v", noms)
	}
	// Une échéance, elle, n'attend pas : --force est la main de l'exploitant.
	h.mustRun("cert", "renew", "shopfr", "--force")
	if noms := h.nomsDuCertificat(t, "shopfr"); !slices.Contains(noms, "boutique.fr") {
		t.Errorf("--force réémet pour la déclaration : %v", noms)
	}
}

func TestStatutNommeCeQueLeCertificatEnPlaceSertEncore(t *testing.T) {
	// Le domaine principal change et l'ancien devient une redirection : tant que
	// le nouveau n'est pas couvert, c'est la redirection qui reste en HTTPS, le
	// site passant en HTTP seul. « HTTPS servi sur le certificat en place » disait
	// le site servi.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	h.modifierDomaines(t, "shopfr", func([]model.Domain) []model.Domain {
		return []model.Domain{{Name: "boutique.fr", Role: "primary"},
			{Name: "shop.fr", Role: "redirect", Target: "boutique.fr"}}
	})
	h.suspendreACME(t, "boutique.fr", "shop.fr")
	h.mustRunPartial("apply", "shopfr", "--yes")
	status, _ := h.run("status", "shopfr")
	if !strings.Contains(status, "boutique.fr pas encore couvert(s), HTTPS servi pour shop.fr sur") {
		t.Errorf("l'état doit nommer ce qui reste servi en HTTPS :\n%s", status)
	}
}

func TestStatutAnnonceUnCertificatExpire(t *testing.T) {
	// Un certificat expiré n'est ni absent ni incomplet : c'est le mot qui guide
	// vers la cause, un renouvellement qui échoue depuis un mois.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	passé := func() time.Time { return time.Now().Add(-48 * time.Hour) }
	matériel, err := tlsprov.SelfSigned{Validity: time.Hour, Now: passé}.Issue(context.Background(),
		tlsprov.Request{Name: "shopfr", Domains: []string{"shop.fr"}, Challenge: tlsprov.ChallengeSelfSigned})
	if err != nil {
		t.Fatal(err)
	}
	if err := (tlsprov.Store{Paths: config.NewPaths(h.prefix)}).Write("shopfr", matériel); err != nil {
		t.Fatal(err)
	}
	status, code := h.run("status", "shopfr")
	colonne := regexp.MustCompile(`(?m)^shopfr\s+prod\s+shop\.fr\s+\S+\s+\d+\s+\S+\s+expiré\s`)
	if code != CodePartial || !strings.Contains(status, "certificat shopfr expiré depuis le") ||
		!colonne.MatchString(status) {
		t.Errorf("l'expiration doit être nommée (code %d) :\n%s", code, status)
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
	// Le faux broker tourne sous le compte des tests : c'est lui que la garde de
	// la boucle locale doit tenir pour le compte du broker (D93).
	moi, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	creds := fmt.Sprintf("url: %s\nuser: admin\npassword: s3cr3t\namqpPort: 5672\nbrokerAccount: %s\n",
		srv.URL, moi.Username)
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
	case strings.HasPrefix(path, "/api/vhosts/") && r.Method == http.MethodDelete:
		delete(b.vhosts, strings.TrimPrefix(path, "/api/vhosts/"))
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "/api/users/") && r.Method == http.MethodDelete:
		delete(b.users, strings.TrimPrefix(path, "/api/users/"))
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

func TestSuppressionLaisseLeVhostAMQPQuOgsNaPasCree(t *testing.T) {
	// Un vhost et un compte homonymes, créés hors d'ogs, étaient repris sans
	// secret par l'apply puis détruits, messages compris, par « project delete »
	// (R2-40). Ils sont désormais refusés au plan et laissés à la suppression.
	h := newHarness(t)
	h.mustRun("init")
	state := newBrokerState()
	state.vhosts["shopfr"], state.users["shopfr"] = true, true
	h.fakeBroker(t, state)
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("amqp", "messenger", "shopfr", "--transport", "async")

	out, code := h.run("apply", "shopfr", "--yes")
	if code != CodePartial || !strings.Contains(out, "sans avoir été créé") {
		t.Errorf("le vhost homonyme doit être refusé en code 6 (code %d) :\n%s", code, out)
	}
	for _, w := range state.writes {
		if strings.Contains(w, "shopfr") && strings.Contains(w, "/api/") {
			t.Errorf("écriture sur les ressources de l'administrateur : %s", w)
		}
	}
	out = h.mustRunPartial("project", "delete", "shopfr", "--yes")
	if !strings.Contains(out, "vhost amqp shopfr conservé") || !strings.Contains(out, "compte amqp shopfr conservé") {
		t.Errorf("la suppression doit dire ce qu'elle laisse :\n%s", out)
	}
	for _, w := range state.writes {
		if strings.HasPrefix(w, "DELETE ") {
			t.Errorf("suppression d'une ressource qu'ogs n'a pas créée : %s", w)
		}
	}
}

func TestVhostDeLAdministrateurDesigneApresCoupNestPasRepris(t *testing.T) {
	// Le mot de passe scellé prouve le compte, pas le vhost : un spec.amqp.vhost
	// changé vers le vhost « legacy » de l'administrateur y ouvrait les
	// permissions « .* » du compte du projet, y déclarait files et bindings, puis
	// « project delete » supprimait legacy et ses messages.
	h := newHarness(t)
	h.mustRun("init")
	state := newBrokerState()
	h.fakeBroker(t, state)
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("amqp", "messenger", "shopfr", "--transport", "async")
	h.mustRun("apply", "shopfr", "--yes")

	state.vhosts["legacy"] = true
	decl := h.path("etc", "ogs", "projects", "shopfr.yaml")
	data, err := os.ReadFile(decl)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "vhost: shopfr") {
		t.Fatalf("déclaration inattendue :\n%s", data)
	}
	if err := os.WriteFile(decl, []byte(strings.ReplaceAll(string(data), "vhost: shopfr", "vhost: legacy")),
		0o600); err != nil {
		t.Fatal(err)
	}
	state.writes = nil
	out, code := h.run("apply", "shopfr", "--yes")
	if code != CodePartial || !strings.Contains(out, "aucune marque scellée pour le vhost legacy") {
		t.Errorf("le vhost de l'administrateur doit être refusé en code 6 (code %d) :\n%s", code, out)
	}
	for _, w := range state.writes {
		if strings.Contains(w, "legacy") {
			t.Errorf("écriture sur le vhost de l'administrateur : %s", w)
		}
	}
	out = h.mustRunPartial("project", "delete", "shopfr", "--yes")
	if slices.Contains(state.writes, "DELETE /api/vhosts/legacy") {
		t.Errorf("le vhost de l'administrateur est parti avec le projet :\n%s", out)
	}
	// Le compte, lui, est celui d'ogs : il part.
	if !slices.Contains(state.writes, "DELETE /api/users/shopfr") {
		t.Errorf("le compte d'ogs doit partir : %v", state.writes)
	}
}

func TestFilesDUnVhostHomonymeNiVideesNiAffichees(t *testing.T) {
	// « amqp queue purge » vidait la file d'un vhost qu'ogs n'a pas créé, et
	// « amqp status » l'affichait sous le nom du projet.
	h := newHarness(t)
	h.mustRun("init")
	state := newBrokerState()
	state.vhosts["shopfr"] = true
	state.queueList = []map[string]any{{"name": "async", "vhost": "shopfr", "messages": 12}}
	h.fakeBroker(t, state)
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("amqp", "messenger", "shopfr", "--transport", "async")
	state.writes = nil
	out, code := h.run("amqp", "queue", "purge", "shopfr", "async", "--yes")
	if code != CodeConflict || !strings.Contains(out, "aucune marque scellée") {
		t.Errorf("la purge doit être refusée (code %d) :\n%s", code, out)
	}
	for _, w := range state.writes {
		if strings.HasPrefix(w, "DELETE ") {
			t.Errorf("file de l'administrateur vidée : %s", w)
		}
	}
	out, _ = h.run("amqp", "status", "shopfr")
	if !strings.Contains(out, "non créé par ogs") || strings.Contains(out, "async") && strings.Contains(out, "12") {
		t.Errorf("les files d'un homonyme ne doivent pas être affichées :\n%s", out)
	}
}

func TestSuppressionRetireLeVhostAMQPDOgs(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	state := newBrokerState()
	h.fakeBroker(t, state)
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("amqp", "messenger", "shopfr", "--transport", "async")
	h.mustRun("apply", "shopfr", "--yes")
	h.mustRunPartial("project", "delete", "shopfr", "--yes")
	if !slices.Contains(state.writes, "DELETE /api/vhosts/shopfr") ||
		!slices.Contains(state.writes, "DELETE /api/users/shopfr") {
		t.Errorf("le vhost et le compte d'ogs doivent partir : %v", state.writes)
	}
	// Les secrets partent avec eux : un vhost homonyme créé ensuite par un autre
	// n'est pas tenu pour celui du projet recréé sous le même nom. Le compte est
	// absent du broker : seule la marque du vhost, restée, le ferait adopter.
	if state.vhosts["shopfr"] || state.users["shopfr"] {
		t.Fatalf("le faux broker doit avoir retiré vhost et compte : %v %v", state.vhosts, state.users)
	}
	state.vhosts["shopfr"] = true
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("amqp", "messenger", "shopfr", "--transport", "async")
	if out, code := h.run("apply", "shopfr", "--yes"); code != CodePartial ||
		!strings.Contains(out, "aucune marque scellée pour le vhost shopfr") {
		t.Errorf("le vhost d'un autre doit être refusé au projet recréé (code %d) :\n%s", code, out)
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
	h.fakeBroker(t, state)
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5")
	h.mustRun("amqp", "messenger", "shopfr")
	// Le vhost est celui d'ogs : ceux d'un homonyme ne sont pas affichés (D93).
	h.mustRun("apply", "shopfr", "--yes")
	state.queueList = []map[string]any{{
		"name": "async", "vhost": "shopfr", "durable": true, "type": "quorum",
		"messages": 42, "consumers": 2, "arguments": map[string]any{},
	}}
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
	if !strings.Contains(out, "--password : la valeur figure dans la ligne de commande") {
		t.Errorf("l'option --password doit être signalée :\n%s", out)
	}
	// Hors terminal, le mot de passe se lit sur l'entrée standard (S1R-11).
	h.stdin = "mot de passe lu\n"
	h.mustRun("amqp", "credentials", "set", "--user", "admin")
	if content := h.read(t, filepath.Join("etc", "ogs", "credentials", "amqp.yaml")); !strings.Contains(content,
		"password: mot de passe lu") {
		t.Errorf("le mot de passe de l'entrée standard doit être enregistré :\n%s", content)
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
	// La copie du contenu est une opération de fichiers : elle a lieu sous
	// préfixe comme les autres écritures.
	if data, err := os.ReadFile(h.path("home", "shop.preprod-pp.net", "www", "public", "index.php")); err != nil ||
		string(data) != "<?php echo 'production';" {
		t.Errorf("le contenu de www/ doit être recopié : %q, %v", string(data), err)
	}
}

// secretDeRoot prépare, hors de tout home, un répertoire que ni la production ni
// la préproduction ne devraient lire.
func secretDeRoot(t *testing.T, h *harness) string {
	t.Helper()
	secret := h.path("root", "secret")
	if err := os.MkdirAll(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secret, "shadow"), []byte("root:$6$hash-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return secret
}

func TestLeCloneNeSuitAucunLienDuWWWSource(t *testing.T) {
	// L'utilisateur de production possède son home (D51) : il remplace ~/www par
	// un lien vers un répertoire que root seul lit. « cp -a www/. » lancé en root
	// en recopiait la cible, que « chown -R » donnait ensuite à la préproduction.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	secret := secretDeRoot(t, h)
	srcWWW := h.path("home", "shop.fr", "www")
	if err := os.Rename(srcWWW, srcWWW+".orig"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, srcWWW); err != nil {
		t.Fatal(err)
	}

	out, code := h.run("site", "clone", "shopfr", "--to", "prep", "--yes")
	if code != CodePartial {
		t.Errorf("une copie refusée est un succès partiel : code %d, attendu %d\n%s", code, CodePartial, out)
	}
	if !strings.Contains(out, "copie du contenu") || !strings.Contains(out, "lien symbolique") ||
		!strings.Contains(out, "rien n'a été recopié") {
		t.Errorf("le refus doit être dit, avec sa raison :\n%s", out)
	}
	dstWWW := h.path("home", "shop.preprod-pp.net", "www")
	if _, err := os.Lstat(filepath.Join(dstWWW, "shadow")); !os.IsNotExist(err) {
		t.Errorf("la cible du lien a été recopiée dans la préproduction : %v", err)
	}
}

func TestLeCloneRecopieLesLiensCommeDesLiens(t *testing.T) {
	// Sous www, un lien vers un fichier ou un répertoire de root : la copie porte
	// le lien, jamais le contenu de sa cible — et ne descend pas dedans.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	secret := secretDeRoot(t, h)
	srcWWW := h.path("home", "shop.fr", "www")
	if err := os.MkdirAll(filepath.Join(srcWWW, "assets", "css"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcWWW, "assets", "css", "site.css"), []byte("body{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	liens := map[string]string{
		"fuite.txt":          filepath.Join(secret, "shadow"),
		"dossier":            secret,
		"assets/relatif.css": "css/site.css",
		"assets/css/pendant": "/nulle/part",
	}
	for nom, cible := range liens {
		if err := os.Symlink(cible, filepath.Join(srcWWW, filepath.FromSlash(nom))); err != nil {
			t.Fatal(err)
		}
	}

	// La préproduction suivra ce que la copie ne suit pas : un lien dont la
	// cible sort de la destination est nommé, et rend le code 6.
	out, code := h.run("site", "clone", "shopfr", "--to", "prep", "--yes")
	if code != CodePartial {
		t.Errorf("code %d, attendu %d\n%s", code, CodePartial, out)
	}
	for _, nom := range []string{"fuite.txt", "dossier", "pendant"} {
		if !strings.Contains(out, "⚠ lien recopié tel quel : ") || !strings.Contains(out, nom+"\" → ") {
			t.Errorf("le lien %s doit être nommé :\n%s", nom, out)
		}
	}
	if strings.Contains(out, "relatif.css\" → ") {
		t.Errorf("un lien qui reste dans la copie ne doit pas être nommé :\n%s", out)
	}
	dstWWW := h.path("home", "shop.preprod-pp.net", "www")
	data, err := os.ReadFile(filepath.Join(dstWWW, "assets", "css", "site.css"))
	if err != nil || string(data) != "body{}" {
		t.Errorf("un fichier ordinaire doit être recopié : %q, %v", string(data), err)
	}
	if info, err := os.Stat(filepath.Join(dstWWW, "assets", "css", "site.css")); err == nil && info.Mode().Perm() != 0o640 {
		t.Errorf("le mode du fichier doit être repris : %04o", info.Mode().Perm())
	}
	for nom, cible := range liens {
		chemin := filepath.Join(dstWWW, filepath.FromSlash(nom))
		info, err := os.Lstat(chemin)
		if err != nil {
			t.Errorf("%s absent de la copie : %v", nom, err)
			continue
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s doit rester un lien, la copie en a fait %v", nom, info.Mode())
			continue
		}
		if got, _ := os.Readlink(chemin); got != cible {
			t.Errorf("%s : cible %q, attendue %q", nom, got, cible)
		}
	}
	// Rien de la cible n'a été recopié, ni sous le lien ni à côté.
	err = filepath.Walk(dstWWW, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			if data, _ := os.ReadFile(path); strings.Contains(string(data), "hash-secret") {
				t.Errorf("%s porte le contenu d'un fichier de root", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// cpReel lance réellement /bin/cp, comme exec.Local le ferait en root, et se
// contente d'enregistrer le reste — un chown vers un autre compte échouerait sans
// root. Il garde la copie du clone de revenir à « cp -a » sans que l'essai le voie.
type cpReel struct {
	local  exec.Local
	appels []exec.Command
}

func (e *cpReel) Simulated() bool { return false }

func (e *cpReel) Run(ctx context.Context, c exec.Command) (exec.Result, error) {
	e.appels = append(e.appels, c)
	if c.Name == "/bin/cp" {
		return e.local.Run(ctx, c)
	}
	return exec.Result{}, nil
}

// cloneHorsSimulation prépare le chemin réel de la copie du clone de shopfr : un
// exécuteur non simulé, et des comptes aux uid donnés — celui de l'essai pour que
// les chown aboutissent sans root. Il rend l'application et les deux projets.
func cloneHorsSimulation(t *testing.T, h *harness, uidSource, uidCible int) (*App, *model.Project, *model.Project) {
	t.Helper()
	gid := os.Getgid()
	var passwd, group strings.Builder
	for _, c := range []struct {
		nom, home string
		uid       int
	}{
		{"shopfr", h.path("home", "shop.fr"), uidSource},
		{"shopfrprep", h.path("home", "shop.preprod-pp.net"), uidCible},
	} {
		fmt.Fprintf(&passwd, "%s:x:%d:%d:ogs=%s:%s:/bin/bash\n", c.nom, c.uid, gid, c.nom, c.home)
		fmt.Fprintf(&group, "%s:x:%d:\n", c.nom, gid)
	}
	if err := os.WriteFile(h.path("etc", "passwd"), []byte(passwd.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.path("etc", "group"), []byte(group.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	app := newApp()
	var out bytes.Buffer
	app.Prefix, app.Out, app.Err = h.prefix, &out, &out
	if err := app.setup(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.close)
	app.Exec = &cpReel{}
	source, err := app.loadProject("shopfr")
	if err != nil {
		t.Fatal(err)
	}
	target, err := app.loadProject("shopfrprep")
	if err != nil {
		t.Fatal(err)
	}
	return app, source, target
}

func TestLaCopieDuCloneHorsSimulationNeSuitPasLeLien(t *testing.T) {
	// Le chemin réel, hors mode simulé : « cp -a <www>/. » lancé en root recopiait
	// la cible du lien, puis « chown -R » la donnait à la préproduction.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	h.mustRun("site", "clone", "shopfr", "--to", "prep", "--yes")
	secret := secretDeRoot(t, h)
	srcWWW := h.path("home", "shop.fr", "www")
	if err := os.Rename(srcWWW, srcWWW+".orig"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, srcWWW); err != nil {
		t.Fatal(err)
	}

	app, source, target := cloneHorsSimulation(t, h, os.Getuid(), os.Getuid())
	if _, err := app.copyTree(context.Background(), source, target, fs.Guard{}); !errors.Is(err, fs.ErrNotDirectory) {
		t.Errorf("un www source lié doit faire refuser la copie, obtenu %v", err)
	}
	if _, err := os.Lstat(h.path("home", "shop.preprod-pp.net", "www", "shadow")); !os.IsNotExist(err) {
		t.Errorf("la cible du lien a été recopiée dans la préproduction : %v", err)
	}
}

func TestLaCopieDuCloneNeRecopieQueLesFichiersDuCompteSource(t *testing.T) {
	// Un lien physique vers /etc/shadow, posé dans le www de la production, n'a
	// plus qu'un lien une fois que le useradd du clone a réécrit /etc/shadow par
	// renommage : seul son propriétaire le trahit. Le compte source est celui que
	// l'identité résout, non le propriétaire de www — ici, les fichiers comme www
	// appartiennent à l'essai, et le compte source est un autre.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	h.mustRun("site", "clone", "shopfr", "--to", "prep", "--yes")
	durshadow := h.path("home", "shop.fr", "www", "durshadow")
	if err := os.WriteFile(durshadow, []byte("root:$6$hash-secret\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	app, source, target := cloneHorsSimulation(t, h, os.Getuid()+1, os.Getuid())
	skipped, err := app.copyTree(context.Background(), source, target, fs.Guard{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(skipped.Named, "\n"), "durshadow") {
		t.Errorf("un fichier d'un autre compte que la source doit être écarté : %+v", skipped)
	}
	if _, err := os.Lstat(h.path("home", "shop.preprod-pp.net", "www", "durshadow")); !os.IsNotExist(err) {
		t.Errorf("le fichier étranger a été recopié dans la préproduction : %v", err)
	}
}

func TestLeCloneDitEtCompteLesEntreesEcartees(t *testing.T) {
	// Une entrée écartée laisse la préproduction incomplète : l'avertissement ne
	// suffit pas, le code de retour doit le dire, et rien ne doit l'annoncer
	// comme copiée — pas même un nom d'entrée, qui est aux mains de l'utilisateur
	// de la production et forgeait, affiché tel quel, une ligne « ✓ ».
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	for _, nom := range []string{"tube", "x : fichier spécial\n✓ contenu copié : tout va bien\n\x1b[1A"} {
		if err := syscall.Mkfifo(h.path("home", "shop.fr", "www", nom), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, code := h.run("site", "clone", "shopfr", "--to", "prep", "--yes")
	if code != CodePartial {
		t.Errorf("une entrée écartée est un succès partiel : code %d, attendu %d\n%s", code, CodePartial, out)
	}
	if !strings.Contains(out, "non recopié") || !strings.Contains(out, "tube") ||
		!strings.Contains(out, "contenu copié en partie") {
		t.Errorf("l'entrée écartée doit être nommée, et la copie dite partielle :\n%s", out)
	}
	for _, ligne := range strings.Split(out, "\n") {
		if strings.HasPrefix(ligne, "✓ contenu copié") {
			t.Errorf("une copie incomplète ne doit pas s'annoncer comme faite : %q", ligne)
		}
	}
	if strings.Contains(out, "\x1b") || !strings.Contains(out, "2 entrée(s) écartée(s)") {
		t.Errorf("les noms écartés doivent être cités, sans échappement, et comptés :\n%s", out)
	}
}

// annuleDesLaCopie est un contexte qui se dit annulé dès que le www de
// destination n'est plus vide : l'apply qui précède la copie n'y écrit rien, et
// l'interruption tombe donc en pleine copie.
type annuleDesLaCopie struct {
	context.Context
	www string
}

func (c annuleDesLaCopie) Err() error {
	if entrees, _ := os.ReadDir(c.www); len(entrees) > 0 {
		return context.Canceled
	}
	return nil
}

// runSous exécute une commande comme run, sous le contexte ctx — celui que main
// annule sur SIGINT et SIGTERM.
func (h *harness) runSous(ctx context.Context, args ...string) (string, int) {
	h.t.Helper()
	var out bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"--prefix", h.prefix}, args...))
	err := root.ExecuteContext(ctx)
	if err != nil {
		out.WriteString("\nogs : " + err.Error() + "\n")
	}
	return out.String(), ExitCode(err)
}

func TestUnCloneInterrompuRendLeCodeDAnnulation(t *testing.T) {
	// « cp -a » mourait avec son groupe de processus ; la copie faite dans le
	// processus ne consultait pas le contexte. main captant SIGINT et SIGTERM,
	// Ctrl-C restait sans effet jusqu'au bout de la copie — que le locataire
	// allonge à volonté —, qui s'annonçait ensuite faite, code 0.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
	srcWWW := h.path("home", "shop.fr", "www")
	for _, nom := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(srcWWW, nom), []byte(nom), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dstWWW := h.path("home", "shop.preprod-pp.net", "www")
	out, code := h.runSous(annuleDesLaCopie{Context: context.Background(), www: dstWWW},
		"site", "clone", "shopfr", "--to", "prep", "--yes")
	if code != CodeCancelled {
		t.Errorf("une copie interrompue à la demande rend le code %d, obtenu %d\n%s", CodeCancelled, code, out)
	}
	if !strings.Contains(out, "copie interrompue à la demande") || !strings.Contains(out, "n'en porte qu'une partie") {
		t.Errorf("l'interruption doit être dite, avec ce qu'elle laisse :\n%s", out)
	}
	for _, ligne := range strings.Split(out, "\n") {
		if strings.HasPrefix(ligne, "✓ contenu copié") || strings.Contains(ligne, "créé depuis") {
			t.Errorf("une copie interrompue ne doit pas s'annoncer faite : %q", ligne)
		}
	}
	if _, err := os.Lstat(filepath.Join(dstWWW, "c.txt")); !os.IsNotExist(err) {
		t.Errorf("la copie a continué après l'interruption : %v", err)
	}
}

func TestLaCopieInterrompueNeSeDitPasRefusee(t *testing.T) {
	// Un lien ou une entrée absente rencontrés en cours de copie s'affichaient
	// comme à la racine — « rien n'est recopié à travers un lien », « source
	// absente » — alors qu'une partie du contenu était déjà en place.
	t.Run("fichier illisible", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root lit un fichier en 0000 : l'échec ne se provoque pas ainsi")
		}
		h := newHarness(t)
		h.mustRun("init")
		h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
		srcWWW := h.path("home", "shop.fr", "www")
		if err := os.WriteFile(filepath.Join(srcWWW, "a.txt"), []byte("a"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(srcWWW, "zz.txt"), []byte("z"), 0o000); err != nil {
			t.Fatal(err)
		}
		out, code := h.run("site", "clone", "shopfr", "--to", "prep", "--yes")
		if code != CodePartial {
			t.Errorf("une copie interrompue est un succès partiel : code %d, attendu %d\n%s", code, CodePartial, out)
		}
		if !strings.Contains(out, "copie du contenu interrompue") || strings.Contains(out, "rien n'a été recopié") {
			t.Errorf("la copie doit se dire interrompue, non refusée :\n%s", out)
		}
		if data, err := os.ReadFile(h.path("home", "shop.preprod-pp.net", "www", "a.txt")); err != nil ||
			string(data) != "a" {
			t.Errorf("ce qui précède l'interruption est en place : %q, %v", string(data), err)
		}
	})
	t.Run("lien dans la destination", func(t *testing.T) {
		h := newHarness(t)
		h.mustRun("init")
		h.mustRun("site", "create", "shop.fr", "--kind", "static", "--yes")
		h.mustRun("site", "clone", "shopfr", "--to", "prep", "--yes")
		secret := secretDeRoot(t, h)
		srcWWW := h.path("home", "shop.fr", "www")
		if err := os.WriteFile(filepath.Join(srcWWW, "a.txt"), []byte("a"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(srcWWW, "zz"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(srcWWW, "zz", "f"), []byte("f"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Le compte de préproduction pose un lien là où la source a un répertoire.
		if err := os.Symlink(secret, h.path("home", "shop.preprod-pp.net", "www", "zz")); err != nil {
			t.Fatal(err)
		}

		app, source, target := cloneHorsSimulation(t, h, os.Getuid(), os.Getuid())
		_, err := app.copyTree(context.Background(), source, target, fs.Guard{})
		if !errors.Is(err, fs.ErrNotDirectory) {
			t.Fatalf("un répertoire de destination lié doit arrêter la copie : %v", err)
		}
		if errors.Is(err, errNotCopied) || strings.Contains(err.Error(), "rien n'a été recopié") {
			t.Errorf("une copie commencée ne doit pas se dire refusée : %v", err)
		}
		if _, err := os.Lstat(filepath.Join(secret, "f")); !os.IsNotExist(err) {
			t.Errorf("root a écrit dans la cible du lien : %v", err)
		}
	})
}

func TestLaRotationDeCleRefuseUnSshLie(t *testing.T) {
	// « ogs user key rotate » tourne hors de tout plan. L'utilisateur remplace son
	// .ssh par un lien : la rotation lisait l'ancienne clé à travers lui, la
	// sauvegardait, puis — hors simulation — supprimait et recréait la paire dans
	// le répertoire visé.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "shopfr", "--yes")
	cible := h.path("root", ".ssh")
	if err := os.MkdirAll(cible, 0o700); err != nil {
		t.Fatal(err)
	}
	for nom, contenu := range map[string]string{"id_ed25519": "CLÉ PRIVÉE DE ROOT\n", "id_ed25519.pub": "CLÉ PUBLIQUE DE ROOT\n"} {
		if err := os.WriteFile(filepath.Join(cible, nom), []byte(contenu), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ssh := h.path("home", "shop.fr", ".ssh")
	if err := os.RemoveAll(ssh); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cible, ssh); err != nil {
		t.Fatal(err)
	}

	out, code := h.run("user", "key", "rotate", "shopfr", "--yes")
	if code == CodeOK {
		t.Errorf("la rotation doit refuser un .ssh lié :\n%s", out)
	}
	if strings.Contains(out, "DE ROOT") {
		t.Errorf("une clé a été lue à travers le lien :\n%s", out)
	}
	if copies, _ := os.ReadDir(h.path("var", "lib", "ogs", "backups")); len(copies) != 0 {
		t.Errorf("la clé de la cible a été sauvegardée : %v", copies)
	}
	out, code = h.run("user", "key", "show", "shopfr")
	if code == CodeOK || strings.Contains(out, "DE ROOT") {
		t.Errorf("« user key show » ne doit rien lire à travers le lien (code %d) :\n%s", code, out)
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

func TestRestaurationNEcritJamaisATraversLesLiensPosesParLApply(t *testing.T) {
	// La restauration applique le projet avant d'en remettre l'arborescence, et
	// l'apply pose dans le home neuf trois liens vers des fichiers qui ne sont pas
	// au projet : le trousseau commun (D61), l'interpréteur PHP (D70) et le
	// répertoire root des journaux (D71). L'archive, elle, porte le home tel que
	// l'utilisateur l'avait laissé — un authorized_keys régulier, que D61 admet, un
	// script à la place du lien php, un vrai répertoire à la place de ~/logs.
	// L'extraction écrivait par le nom, donc à travers ces liens, en root : le
	// trousseau de tout le parc, le binaire du système et le répertoire des
	// journaux recevaient le contenu choisi par le projet.
	h := newHarness(t)
	h.mustRun("init")
	trousseau := h.path("opt", "default_authorized_keys")
	phpBin := h.path("usr", "bin", "php8.5")
	for f, body := range map[string]string{
		trousseau: "ssh-ed25519 AAAA cle-des-administrateurs\n",
		phpBin:    "#!/bin/sh\n# vrai interpreteur\n",
	} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4",
		"--php", "8.5", "--yes")
	home := h.path("home", "shop.fr")
	logDir := h.path("var", "log", "ogs-projects", "shopfr")
	for rel, want := range map[string]string{
		".ssh/authorized_keys": trousseau, ".local/bin/php": phpBin, "logs": logDir,
	} {
		if got, _ := os.Readlink(filepath.Join(home, rel)); got != want {
			t.Fatalf("~/%s doit être un lien vers %s après la création, obtenu %q", rel, want, got)
		}
	}

	// L'utilisateur du projet, maître de son home (D51), remplace les trois liens.
	for rel, body := range map[string]string{
		".ssh/authorized_keys": "ssh-ed25519 BBBB cle-du-projet\n",
		".local/bin/php":       "#!/bin/sh\n# script du projet\n",
	} {
		p := filepath.Join(home, rel)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	logs := filepath.Join(home, "logs")
	if err := os.Remove(logs); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/shadow", filepath.Join(logs, "apache-error.log.1")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "depose.txt"), []byte("déposé\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h.mustRunPartial("project", "delete", "shopfr", "--yes")
	entries, err := os.ReadDir(h.path("var", "lib", "ogs", "archives"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("aucune archive : %v", err)
	}
	out := h.mustRunPartial("project", "restore",
		h.path("var", "lib", "ogs", "archives", entries[0].Name()), "--yes")
	if strings.Contains(out, "antérieur à D71") {
		t.Errorf("un ~/logs posé par l'utilisateur n'est pas l'ancien répertoire des journaux :\n%s", out)
	}

	if data, _ := os.ReadFile(trousseau); string(data) != "ssh-ed25519 AAAA cle-des-administrateurs\n" {
		t.Errorf("le trousseau commun a été réécrit par la restauration : %q\n%s", data, out)
	}
	if data, _ := os.ReadFile(phpBin); string(data) != "#!/bin/sh\n# vrai interpreteur\n" {
		t.Errorf("l'interpréteur du système a reçu le script du projet : %q", data)
	}
	for _, name := range []string{"apache-error.log.1", "depose.txt"} {
		if _, err := os.Lstat(filepath.Join(logDir, name)); err == nil {
			t.Errorf("%s posé dans le répertoire root des journaux par la restauration", name)
		}
	}
	// Le home, lui, revient tel que l'utilisateur l'avait laissé : les liens de
	// l'apply sont remplacés en tant que liens — jamais traversés —, et l'apply
	// suivant les traitera selon ses règles, un authorized_keys régulier n'étant
	// jamais remplacé (D61).
	if info, err := os.Lstat(logs); err != nil || !info.IsDir() {
		t.Errorf("~/logs doit revenir en répertoire du projet : %v %v", info, err)
	}
	for rel, want := range map[string]string{
		".ssh/authorized_keys": "ssh-ed25519 BBBB cle-du-projet\n",
		".local/bin/php":       "#!/bin/sh\n# script du projet\n",
		"logs/depose.txt":      "déposé\n",
	} {
		info, err := os.Lstat(filepath.Join(home, rel))
		if err != nil || !info.Mode().IsRegular() {
			t.Errorf("~/%s doit revenir en fichier régulier : %v %v", rel, info, err)
			continue
		}
		if data, _ := os.ReadFile(filepath.Join(home, rel)); string(data) != want {
			t.Errorf("~/%s restauré : %q, attendu %q", rel, data, want)
		}
	}
	if target, err := os.Readlink(filepath.Join(logs, "apache-error.log.1")); err != nil || target != "/etc/shadow" {
		t.Errorf("le lien du projet doit revenir tel quel dans son home : %q %v", target, err)
	}
}

func TestRestaurationDUneArchiveAnterieureAD71ReprendSesJournaux(t *testing.T) {
	// Une archive d'un projet antérieur à D71 porte son ~/logs, répertoire root des
	// journaux, et une déclaration qui le dit. L'apply de la restauration pose le
	// lien ~/logs, que l'extraction remplace — sans le traverser — par le répertoire
	// archivé, rendu à root comme il l'était (RestoredOwner, qui ne se voit qu'en
	// root) : c'est l'apply suivant qui migre ses journaux sous logs.root et
	// rétablit le lien. Ne rien extraire sous ~/logs aurait perdu ces journaux.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-plain")
	h.mustRun("apply", "shopfr", "--yes")
	home := h.path("home", "shop.fr")
	decl := h.path("etc", "ogs", "projects", "shopfr.yaml")
	data, _ := os.ReadFile(decl)
	nouveau := h.path("var", "log", "ogs-projects", "shopfr")
	ancien := strings.Replace(string(data), "logDir: "+nouveau, "logDir: "+filepath.Join(home, "logs"), 1)
	ancien = strings.Replace(ancien, "path: .ssh", "path: logs\n        mode: \"0710\"\n        owner: root\n      - path: .ssh", 1)
	if !strings.Contains(ancien, "logDir: "+filepath.Join(home, "logs")) || !strings.Contains(ancien, "path: logs") {
		t.Fatalf("déclaration inattendue, impossible d'y réintroduire l'ancien ~/logs :\n%s", data)
	}
	if err := os.WriteFile(decl, []byte(ancien), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(home, "logs")
	if err := os.Remove(legacy); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(nouveau); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(legacy, 0o710); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"apache-access.log":   "accès d'avant la suppression\n",
		"apache-access.log.1": "accès plus ancien\n",
	} {
		if err := os.WriteFile(filepath.Join(legacy, name), []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	h.mustRunPartial("project", "delete", "shopfr", "--yes")
	entries, err := os.ReadDir(h.path("var", "lib", "ogs", "archives"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("aucune archive : %v", err)
	}
	out := h.mustRunPartial("project", "restore", h.path("var", "lib", "ogs", "archives", entries[0].Name()), "--yes")
	if info, err := os.Lstat(legacy); err != nil || !info.IsDir() {
		t.Fatalf("l'ancien ~/logs doit revenir en répertoire : %v %v", info, err)
	}
	if !strings.Contains(out, "ogs apply shopfr") {
		t.Errorf("la restauration doit dire que l'apply suivant migrera l'ancien ~/logs :\n%s", out)
	}
	journaux, _ := os.ReadDir(nouveau)
	for _, e := range journaux {
		if data, _ := os.ReadFile(filepath.Join(nouveau, e.Name())); strings.Contains(string(data), "accès") {
			t.Errorf("%s écrit sous logs.root à travers le lien ~/logs par l'extraction", e.Name())
		}
	}

	out = h.mustRunPartial("apply", "shopfr", "--yes")
	if !strings.Contains(out, "reprise des journaux") {
		t.Errorf("l'apply suivant doit reprendre l'ancien ~/logs :\n%s", out)
	}
	if got, _ := os.ReadFile(filepath.Join(nouveau, "apache-access.log")); string(got) != "accès d'avant la suppression\n" {
		t.Errorf("journal courant mal repris : %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(nouveau, "apache-access.log.1")); string(got) != "accès plus ancien\n" {
		t.Errorf("journal roté mal repris : %q", got)
	}
	if target, err := os.Readlink(legacy); err != nil || target != nouveau {
		t.Errorf("~/logs doit redevenir un lien vers %s : %q (%v)", nouveau, target, err)
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

// Sur nginx, l'usage Debian est un vhost sans extension. Le scan tronquait
// « shop.fr » et « shop.de » en un même vhost « shop » : le projet adopté
// écrivait shop.conf, et son premier apply retirait comme obsolètes l'original
// et le site voisin, sans --force ni sauvegarde, avec le code 0.
func TestAdoptionNginxRepriseDuFichierDOrigineSansToucherAuVoisin(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.choisirNginx(t)
	avail := filepath.Join("etc", "nginx", "sites-available")
	enabled := filepath.Join("etc", "nginx", "sites-enabled")
	for _, dir := range []string{avail, enabled} {
		if err := os.MkdirAll(h.path(dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	origine := map[string]string{}
	for _, site := range []string{"shop.fr", "shop.de"} {
		docroot := h.path("var", "www", site, "public")
		if err := os.MkdirAll(docroot, 0o755); err != nil {
			t.Fatal(err)
		}
		origine[site] = "server {\n    listen 80;\n    server_name " + site + ";\n    root " + docroot + ";\n}\n"
		if err := os.WriteFile(h.path(avail, site), []byte(origine[site]), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../sites-available/"+site, h.path(enabled, site)); err != nil {
			t.Fatal(err)
		}
	}

	h.mustRun("import", "adopt", "shopfr", "--yes")
	decl := h.read(t, filepath.Join("etc", "ogs", "projects", "shopfr.yaml"))
	if !strings.Contains(decl, "file: shop.fr") {
		t.Errorf("la déclaration doit désigner le fichier réel :\n%s", decl)
	}

	plan, _ := h.run("plan", "shopfr")
	if strings.Contains(plan, "obsolète") {
		t.Errorf("le plan d'un projet adopté ne doit rien retirer :\n%s", plan)
	}
	// Sans --force, le vhost d'origine est refusé, et rien n'est retiré.
	out, code := h.run("apply", "shopfr", "--yes")
	if code == CodeOK {
		t.Errorf("reprendre un vhost non géré doit exiger --force :\n%s", out)
	}
	for site, corps := range origine {
		if got, err := os.ReadFile(h.path(avail, site)); err != nil || string(got) != corps {
			t.Errorf("%s a été touché sans --force : %v", site, err)
		}
	}

	h.mustRunPartial("apply", "shopfr", "--yes", "--force")
	if got := h.read(t, filepath.Join(avail, "shop.fr")); !strings.Contains(got, "Généré par ogs") {
		t.Errorf("le fichier d'origine doit être réécrit par ogs :\n%s", got)
	}
	if _, err := os.Stat(h.path(avail, "shop.conf")); !os.IsNotExist(err) {
		t.Errorf("aucun second fichier ne doit apparaître à côté de l'original : %v", err)
	}
	if got := h.read(t, filepath.Join(avail, "shop.de")); got != origine["shop.de"] {
		t.Errorf("le vhost du site voisin a été touché :\n%s", got)
	}
	for _, site := range []string{"shop.fr", "shop.de"} {
		if _, err := os.Stat(h.path(enabled, site)); err != nil {
			t.Errorf("le lien d'activation de %s doit rester en place : %v", site, err)
		}
	}
	baks, _ := filepath.Glob(filepath.Join(h.path("var", "lib", "ogs", "backups"), "shop.fr.ogs-bak-*"))
	if len(baks) != 1 {
		t.Fatalf("une sauvegarde de l'original attendue, %d trouvée(s)", len(baks))
	}
	if saved, _ := os.ReadFile(baks[0]); string(saved) != origine["shop.fr"] {
		t.Errorf("la sauvegarde doit porter l'original :\n%s", saved)
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

func TestCustomApplicationLifecycle(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	command := "/home/api.fr/www/bin/serveur --listen ${LISTEN_ADDR}"
	h.mustRun("project", "create", "api.fr", "--kind", "custom", "--command", command, "--env-file")
	h.mustRun("apply", "apifr", "--yes")

	unit := h.read(t, filepath.Join("etc", "systemd", "system", "ogs-apifr-app@.service"))
	envFile := h.path("etc", "ogs", "env", "apifr.env")
	for _, want := range []string{"ExecStart=" + command + "\n", "EnvironmentFile=" + envFile, "PORT=25001"} {
		if !strings.Contains(unit, want) {
			t.Errorf("%q absent de l'unité :\n%s", want, unit)
		}
	}
	if _, err := os.Stat(envFile); err != nil {
		t.Fatalf("le fichier d'environnement doit être créé : %v", err)
	}
	// Le contenu de l'exploitant survit aux apply suivants.
	if err := os.WriteFile(envFile, []byte("JETON=secret-a-retrouver\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.mustRun("apply", "apifr", "--yes")
	if data, _ := os.ReadFile(envFile); string(data) != "JETON=secret-a-retrouver\n" {
		t.Fatalf("ogs a réécrit le fichier d'environnement : %q", data)
	}

	// La suppression emporte l'unité et le fichier, que l'archive garde scellé.
	h.mustRun("project", "delete", "apifr", "--yes")
	for _, gone := range []string{envFile, h.path("etc", "systemd", "system", "ogs-apifr-app@.service")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s doit disparaître avec le projet (%v)", gone, err)
		}
	}
	entries, err := os.ReadDir(h.path("var", "lib", "ogs", "archives"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("archive attendue : %v %v", entries, err)
	}
	archivePath := h.path("var", "lib", "ogs", "archives", entries[0].Name())
	compressed, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := dec.DecodeAll(compressed, nil)
	dec.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("project.yaml")) {
		t.Fatal("archive illisible : la vérification suivante ne prouverait rien")
	}
	if bytes.Contains(raw, []byte("secret-a-retrouver")) {
		t.Error("le fichier d'environnement ne doit pas voyager en clair dans l'archive")
	}
	if secrets, err := archive.Secrets(archivePath); err != nil || len(secrets[appprov.EnvFileSecret][1]) == 0 {
		t.Errorf("le fichier d'environnement doit être scellé dans l'archive : %v", err)
	}

	h.mustRunPartial("project", "restore", archivePath, "--yes")
	if data, err := os.ReadFile(envFile); err != nil || string(data) != "JETON=secret-a-retrouver\n" {
		t.Fatalf("fichier d'environnement restauré : %q (%v)", data, err)
	}
	if info, _ := os.Stat(envFile); info.Mode().Perm() != 0o600 {
		t.Errorf("mode restauré %04o, attendu 0600", info.Mode().Perm())
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

// Le moteur web se choisit pour la machine (web.engine de config.yaml) : site
// create n'offre plus d'option qui l'écrirait dans le projet sans effet.
func TestSiteCreateNOffrePlusLeChoixDuMoteur(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	out, code := h.run("site", "create", "shop.fr", "--kind", "static", "--engine", "nginx", "--dry-run")
	if code != CodeUsage {
		t.Errorf("--engine : code %d attendu %d\n%s", code, CodeUsage, out)
	}
}

// Une cible de harden dont le groupe n'existe pas : le répertoire était fermé
// sans le groupe du service, « ✓ » s'affichait sans que rien n'ait été posé, et
// la même restriction revenait à chaque passage — code 6 à perpétuité.
func TestHardenNAnnoncePasUnGroupeQuIlNaPasPose(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	conf := h.path("etc", "ogs", "config.yaml")
	data, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	cible := "hardening:\n  targets:\n    - path: /etc/monagent\n      owner: root\n" +
		"      group: ogs-groupe-introuvable\n      dirMode: 0750\n"
	modifie := strings.Replace(string(data), "hardening: {}\n", cible, 1)
	if modifie == string(data) {
		t.Fatalf("config.yaml sans « hardening: {} » : le test ne sait plus où poser sa cible")
	}
	if err := os.WriteFile(conf, []byte(modifie), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := h.path("etc", "monagent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	out, code := h.run("harden")
	if code != CodeOK {
		t.Errorf("rien n'est à restreindre : code %d, attendu %d\n%s", code, CodeOK, out)
	}
	if !strings.Contains(out, "refusé") || !strings.Contains(out, "groupe ogs-groupe-introuvable introuvable") {
		t.Errorf("le rapport doit dire la cible refusée, et pourquoi :\n%s", out)
	}
	if strings.Contains(out, "conformes") {
		t.Errorf("une cible laissée ouverte ne se compte pas parmi les conformes :\n%s", out)
	}
	out = h.mustRun("harden", "--apply", "--yes")
	if strings.Contains(out, "✓ /etc/monagent") {
		t.Errorf("« ✓ » annoncé pour une restriction qui n'a pas eu lieu :\n%s", out)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("répertoire en %04o : fermé sans le groupe du service qui le lit", info.Mode().Perm())
	}
}
