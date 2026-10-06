package hook

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
)

func fixture(t *testing.T) (*Runner, *model.Project, string) {
	t.Helper()
	dir := t.TempDir()
	proj := model.New("shop.fr", naming.EnvProd, model.KindStatic)
	if err := proj.Normalize(filepath.Join(dir, "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	return &Runner{Exec: &exec.Local{}, Dir: dir, Timeout: 5 * time.Second}, proj, dir
}

func writeHook(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	// Le mode passé à WriteFile est rogné par l'umask : sous 0022, le script du
	// test « inscriptible par tous » naissait en 0755 et le contrôle n'avait rien
	// à refuser. Chmod, lui, impose le mode demandé.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunPassesContextOnStdinAndEnvironment(t *testing.T) {
	runner, proj, dir := fixture(t)
	out := filepath.Join(dir, "recu.txt")
	writeHook(t, dir, "post.sh",
		"#!/bin/sh\ncat > "+out+"\necho \"$OGS_EVENT $OGS_PROJECT $OGS_HOST\" >> "+out+"\n", 0o750)
	proj.Spec.Hooks = map[string]string{PostCreate: "post.sh"}

	if err := runner.Run(context.Background(), proj, PostCreate,
		Context{Resources: []string{"system/user/shopfr"}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	// Le JSON sur l'entrée standard porte tout le contexte.
	for _, want := range []string{`"event":"postCreate"`, `"project":"shopfr"`, `"host":"shop.fr"`,
		`"resources":["system/user/shopfr"]`} {
		if !strings.Contains(content, want) {
			t.Errorf("contexte JSON incomplet, %q absent :\n%s", want, content)
		}
	}
	// Et l'environnement évite au script court d'avoir à l'analyser.
	if !strings.Contains(content, "postCreate shopfr shop.fr") {
		t.Errorf("variables d'environnement absentes :\n%s", content)
	}
}

func TestRunReportsFailure(t *testing.T) {
	runner, proj, dir := fixture(t)
	writeHook(t, dir, "echec.sh", "#!/bin/sh\necho 'motif du refus' >&2\nexit 1\n", 0o750)
	proj.Spec.Hooks = map[string]string{PreCreate: "echec.sh"}
	err := runner.Run(context.Background(), proj, PreCreate, Context{})
	if err == nil {
		t.Fatal("l'échec du hook doit être remonté")
	}
	if !strings.Contains(err.Error(), "preCreate") {
		t.Errorf("l'erreur doit nommer l'événement : %v", err)
	}
}

func TestPreHooksAreCriticalPostAreNot(t *testing.T) {
	// Un hook « pre » garde la porte : son refus doit arrêter ce qui suit. Un
	// hook « post » constate, et son échec est un avertissement.
	// L'attente se déduit du nom : ainsi un événement ajouté plus tard ne peut pas
	// échapper au contrôle, comme « preApply » l'avait fait.
	for _, event := range Events() {
		attendu := strings.HasPrefix(event, "pre")
		if Critical(event) != attendu {
			t.Errorf("%s : critique = %v, attendu %v", event, Critical(event), attendu)
		}
	}
}

func TestResolveRefusesScriptOutsideDirectory(t *testing.T) {
	// Un hook ne peut vivre qu'au sein du répertoire prévu : autrement, n'importe
	// quel chemin du système deviendrait exécutable en root.
	runner, proj, _ := fixture(t)
	elsewhere := filepath.Join(t.TempDir(), "ailleurs.sh")
	if err := os.WriteFile(elsewhere, []byte("#!/bin/sh\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	proj.Spec.Hooks = map[string]string{PostCreate: elsewhere}
	_, err := runner.Resolve(proj, PostCreate)
	if err == nil || !strings.Contains(err.Error(), "doit se trouver sous") {
		t.Errorf("le script hors répertoire doit être refusé : %v", err)
	}
	// La traversée est refusée elle aussi.
	proj.Spec.Hooks = map[string]string{PostCreate: "../../etc/shadow"}
	if _, err := runner.Resolve(proj, PostCreate); err == nil {
		t.Error("la traversée de répertoire doit être refusée")
	}
}

func TestResolveRefusesWorldWritableScript(t *testing.T) {
	// Un hook s'exécute en root : s'il est inscriptible par un autre compte, ce
	// compte obtient une exécution privilégiée.
	runner, proj, dir := fixture(t)
	writeHook(t, dir, "laxiste.sh", "#!/bin/sh\n", 0o777)
	proj.Spec.Hooks = map[string]string{PostCreate: "laxiste.sh"}
	_, err := runner.Resolve(proj, PostCreate)
	if err == nil || !strings.Contains(err.Error(), "inscriptible") {
		t.Errorf("un script inscriptible par tous doit être refusé : %v", err)
	}
}

func TestResolveRefusesNonExecutable(t *testing.T) {
	runner, proj, dir := fixture(t)
	writeHook(t, dir, "texte.sh", "#!/bin/sh\n", 0o640)
	proj.Spec.Hooks = map[string]string{PostCreate: "texte.sh"}
	_, err := runner.Resolve(proj, PostCreate)
	if err == nil || !strings.Contains(err.Error(), "exécutable") {
		t.Errorf("un script non exécutable doit être signalé : %v", err)
	}
}

func TestResolveReportsMissingScript(t *testing.T) {
	runner, proj, _ := fixture(t)
	proj.Spec.Hooks = map[string]string{PostCreate: "absent.sh"}
	if _, err := runner.Resolve(proj, PostCreate); err == nil {
		t.Error("un script absent doit être signalé")
	}
}

func TestDisabledSkipsEverything(t *testing.T) {
	runner, proj, dir := fixture(t)
	out := filepath.Join(dir, "ne-doit-pas-exister")
	writeHook(t, dir, "post.sh", "#!/bin/sh\ntouch "+out+"\n", 0o750)
	proj.Spec.Hooks = map[string]string{PostCreate: "post.sh"}
	runner.Disabled = true
	if err := runner.Run(context.Background(), proj, PostCreate, Context{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Error("--no-hooks doit tout court-circuiter")
	}
}

func TestDryRunAnnonceLeHookSansLExecuter(t *testing.T) {
	// Aucun hook en --dry-run (D40) : un hook s'exécute en root, et
	// « project delete --dry-run » lançait preDelete et postDelete pour de bon
	// (R3-20). Le court-circuit tient la règle même pour un appelant qui
	// oublierait de tester l'option.
	runner, proj, dir := fixture(t)
	out := filepath.Join(dir, "ne-doit-pas-exister")
	script := writeHook(t, dir, "pre.sh", "#!/bin/sh\ntouch "+out+"\n", 0o750)
	proj.Spec.Hooks = map[string]string{PreDelete: "pre.sh"}
	runner.DryRun = true
	var journal strings.Builder
	runner.Log = func(format string, args ...any) { fmt.Fprintf(&journal, format, args...) }
	if err := runner.Run(context.Background(), proj, PreDelete, Context{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Error("sous --dry-run, le hook ne doit pas être exécuté")
	}
	if !strings.Contains(journal.String(), script) || !strings.Contains(journal.String(), "non exécuté (--dry-run)") {
		t.Errorf("le hook qui aurait tourné doit être nommé, et dit non exécuté :\n%s", journal.String())
	}
	// Un hook refusé l'est encore : la simulation dit ce que l'exécution
	// rencontrerait.
	writeHook(t, dir, "ouvert.sh", "#!/bin/sh\n", 0o777)
	proj.Spec.Hooks = map[string]string{PreDelete: "ouvert.sh"}
	if err := runner.Run(context.Background(), proj, PreDelete, Context{}); err == nil {
		t.Error("sous --dry-run, un hook inscriptible par tous doit rester refusé")
	}
}

func TestNoHookDeclaredIsNotAnError(t *testing.T) {
	runner, proj, _ := fixture(t)
	if err := runner.Run(context.Background(), proj, PostCreate, Context{}); err != nil {
		t.Errorf("l'absence de hook n'est pas une erreur : %v", err)
	}
}

func TestTimeoutInterruptsRunawayHook(t *testing.T) {
	// Un hook qui ne rend pas la main bloquerait l'outil, verrou compris.
	runner, proj, dir := fixture(t)
	writeHook(t, dir, "lent.sh", "#!/bin/sh\nsleep 30\n", 0o750)
	proj.Spec.Hooks = map[string]string{PostCreate: "lent.sh"}
	runner.Timeout = 200 * time.Millisecond
	start := time.Now()
	err := runner.Run(context.Background(), proj, PostCreate, Context{})
	if err == nil {
		t.Fatal("le dépassement de délai doit être signalé")
	}
	if !strings.Contains(err.Error(), "rendre la main") {
		t.Errorf("le message doit être explicite : %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("l'interruption a pris %s", elapsed)
	}
}

func TestEventsAreExhaustive(t *testing.T) {
	// La liste sert à la documentation et à la validation : elle doit couvrir tous
	// les points du cycle de vie.
	events := Events()
	if len(events) != 10 {
		t.Errorf("%d événements : %v", len(events), events)
	}
	for _, want := range []string{PreCreate, PostCreate, PreDelete, PostDelete,
		PreApply, PostApply, PostCertRenew, PostPHPSwitch, PreApplyFleet, PostApplyFleet} {
		found := false
		for _, e := range events {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Errorf("événement %q absent de la liste", want)
		}
	}
}

func TestRequireRootOwnerWhenPrivileged(t *testing.T) {
	// En root, un hook appartenant à un autre compte donnerait à ce compte une
	// exécution privilégiée : le contrôle doit alors s'appliquer.
	runner, proj, dir := fixture(t)
	runner.RequireRootOwner = true
	writeHook(t, dir, "post.sh", "#!/bin/sh\n", 0o750)
	proj.Spec.Hooks = map[string]string{PostCreate: "post.sh"}
	_, err := runner.Resolve(proj, PostCreate)
	if os.Getuid() == 0 {
		if err != nil {
			t.Errorf("en root, un fichier créé par root doit être accepté : %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Errorf("un script n'appartenant pas à root doit être refusé : %v", err)
	}
}

func TestParcWideHookIsFoundByFilename(t *testing.T) {
	// Un hook de portée serveur n'est pas déclaré : sa présence dans le
	// répertoire suffit, sous l'une ou l'autre orthographe.
	for _, name := range []string{PostCreate, "post-create"} {
		t.Run(name, func(t *testing.T) {
			runner, proj, dir := fixture(t)
			out := filepath.Join(dir, "trace")
			writeHook(t, dir, name, "#!/bin/sh\necho \"$OGS_EVENT $OGS_USER\" > "+out+"\n", 0o750)

			if err := runner.Run(context.Background(), proj, PostCreate, Context{}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("le hook de parc n'a pas été exécuté : %v", err)
			}
			if got := strings.TrimSpace(string(data)); got != "postCreate shopfr" {
				t.Errorf("contexte transmis = %q", got)
			}
		})
	}
}

func TestProjectHookOverridesParcWideHook(t *testing.T) {
	runner, proj, dir := fixture(t)
	out := filepath.Join(dir, "trace")
	writeHook(t, dir, PostCreate, "#!/bin/sh\necho parc > "+out+"\n", 0o750)
	writeHook(t, dir, "propre.sh", "#!/bin/sh\necho projet > "+out+"\n", 0o750)
	proj.Spec.Hooks = map[string]string{PostCreate: "propre.sh"}

	if err := runner.Run(context.Background(), proj, PostCreate, Context{}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	if got := strings.TrimSpace(string(data)); got != "projet" {
		t.Errorf("le hook du projet doit primer, exécuté = %q", got)
	}
}

func TestParcWideHookIgnoredWhenAbsent(t *testing.T) {
	runner, proj, _ := fixture(t)
	// Aucun fichier : l'absence de hook n'est pas une erreur.
	if err := runner.Run(context.Background(), proj, PostApply, Context{}); err != nil {
		t.Fatalf("aucun hook déclaré doit être un cas normal : %v", err)
	}
}

func TestExtraValuesBecomeEnvironmentVariables(t *testing.T) {
	runner, proj, dir := fixture(t)
	out := filepath.Join(dir, "trace")
	writeHook(t, dir, PostDelete, "#!/bin/sh\necho \"$OGS_ARCHIVE\" > "+out+"\n", 0o750)

	if err := runner.Run(context.Background(), proj, PostDelete,
		Context{Extra: map[string]string{"archive": "/var/lib/ogs/archives/shopfr.tar.zst"}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	if got := strings.TrimSpace(string(data)); got != "/var/lib/ogs/archives/shopfr.tar.zst" {
		t.Errorf("OGS_ARCHIVE = %q", got)
	}
}
