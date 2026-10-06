package hook

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// enRoot fait tenir le compte de l'essai pour root : le contrôle de la chaîne
// et du propriétaire s'exerce alors comme en production. /tmp, collant, est
// admis pour que t.TempDir reste utilisable ; un répertoire ouvert en écriture
// sans le bit collant ne l'est pas.
func enRoot(t *testing.T, r *Runner) {
	t.Helper()
	// Sous un umask 0002, t.TempDir rend un répertoire ouvert au groupe.
	if err := os.Chmod(r.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	uid, root, trusted := uint32(os.Getuid()), rootUID, trustedDir
	rootUID = uid
	trustedDir = func(st *syscall.Stat_t) bool {
		if st.Mode&syscall.S_ISVTX != 0 {
			return true
		}
		return (st.Uid == uid || st.Uid == 0) && st.Mode&0o022 == 0
	}
	t.Cleanup(func() { rootUID, trustedDir = root, trusted })
	r.RequireRootOwner = true
}

// tempDir rend un répertoire temporaire fermé au groupe, quel que soit l'umask.
func tempDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

// R3-21 : un lien posé sous le répertoire des hooks vers un script hors de lui
// passait le contrôle, fait sur le seul chemin nettoyé ; os.Stat suivait le
// lien, et le répertoire de la cible n'était jamais examiné. Un compte qui le
// possède remplaçait le script exécuté en root.
func TestUnLienVersUnRepertoireOuvertEstRefuse(t *testing.T) {
	runner, proj, dir := fixture(t)
	enRoot(t, runner)
	ailleurs := filepath.Join(tempDir(t), "projet")
	if err := os.Mkdir(ailleurs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ailleurs, 0o777); err != nil {
		t.Fatal(err)
	}
	writeHook(t, ailleurs, "script", "#!/bin/sh\n", 0o750)
	if err := os.Symlink(filepath.Join(ailleurs, "script"), filepath.Join(dir, PostCreate)); err != nil {
		t.Fatal(err)
	}
	_, err := runner.Resolve(proj, PostCreate)
	if err == nil || !strings.Contains(err.Error(), "peut être modifié par un autre compte") {
		t.Fatalf("un lien vers un répertoire ouvert doit être refusé en nommant le répertoire : %v", err)
	}
	if !strings.Contains(err.Error(), ailleurs) || !strings.Contains(err.Error(), "chmod go-w") {
		t.Errorf("le refus doit nommer le répertoire et le remède : %v", err)
	}
}

// Un lien qui sort du répertoire vers un fichier dont seul root contrôle la
// chaîne reste admis (D117) : des parcs en dépendent. Le chemin exécuté est le
// fichier réel.
func TestUnLienVersUnFichierSurResteAdmis(t *testing.T) {
	runner, proj, dir := fixture(t)
	enRoot(t, runner)
	ailleurs := tempDir(t)
	cible := writeHook(t, ailleurs, "script", "#!/bin/sh\n", 0o750)
	if err := os.Symlink(cible, filepath.Join(dir, "post-create")); err != nil {
		t.Fatal(err)
	}
	res, err := runner.Inspect(proj, PostCreate)
	if err != nil {
		t.Fatalf("un lien vers un fichier sûr doit rester admis : %v", err)
	}
	if res.Real != cible || !res.Outside(dir) {
		t.Errorf("résolution = %+v, attendu le fichier réel %s hors de %s", res, cible, dir)
	}
	if res.Path != filepath.Join(dir, "post-create") {
		t.Errorf("chemin désigné = %s", res.Path)
	}
}

// Le répertoire des hooks lui-même, ou un sous-répertoire où le projet désigne
// son script, ouvert en écriture : n'importe qui y remplace le script.
func TestUnSousRepertoireOuvertDuRepertoireDesHooksEstRefuse(t *testing.T) {
	runner, proj, dir := fixture(t)
	enRoot(t, runner)
	sub := filepath.Join(dir, "projets")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeHook(t, sub, "post.sh", "#!/bin/sh\n", 0o750)
	if err := os.Chmod(sub, 0o775); err != nil {
		t.Fatal(err)
	}
	proj.Spec.Hooks = map[string]string{PostCreate: "projets/post.sh"}
	if _, err := runner.Resolve(proj, PostCreate); err == nil ||
		!strings.Contains(err.Error(), "peut être modifié par un autre compte") {
		t.Fatalf("un sous-répertoire inscriptible par le groupe doit être refusé : %v", err)
	}
	if err := os.Chmod(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Resolve(proj, PostCreate); err != nil {
		t.Fatalf("fermé, il doit être admis : %v", err)
	}
}

// Un lien intermédiaire posé dans un répertoire ouvert se remplace : même
// refus, quand bien même sa cible serait sûre.
func TestUnLienIntermediaireDansUnRepertoireOuvertEstRefuse(t *testing.T) {
	runner, proj, dir := fixture(t)
	enRoot(t, runner)
	sur := tempDir(t)
	cible := writeHook(t, sur, "script", "#!/bin/sh\n", 0o750)
	ouvert := filepath.Join(tempDir(t), "ouvert")
	if err := os.Mkdir(ouvert, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cible, filepath.Join(ouvert, "relais")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ouvert, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ouvert, "relais"), filepath.Join(dir, PostCreate)); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Resolve(proj, PostCreate); err == nil || !strings.Contains(err.Error(), ouvert) {
		t.Fatalf("le répertoire du lien intermédiaire doit être nommé : %v", err)
	}
}

// RunDeclared n'exécute que le hook du projet : celui du parc est appelé à part
// pour « postCertRenew » (D117).
func TestRunDeclaredNeSeRabatPasSurLeHookDeParc(t *testing.T) {
	runner, proj, dir := fixture(t)
	out := filepath.Join(dir, "trace")
	writeHook(t, dir, PostCertRenew, "#!/bin/sh\necho parc >> "+out+"\n", 0o750)
	if err := runner.RunDeclared(context.Background(), proj, PostCertRenew, Context{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("le hook de parc ne doit pas être exécuté par RunDeclared : %v", err)
	}
	writeHook(t, dir, "propre.sh", "#!/bin/sh\necho projet >> "+out+"\n", 0o750)
	proj.Spec.Hooks = map[string]string{PostCertRenew: "propre.sh"}
	if err := runner.RunDeclared(context.Background(), proj, PostCertRenew, Context{}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(out); strings.TrimSpace(string(data)) != "projet" {
		t.Errorf("trace = %q", data)
	}
}

// B1 de la relecture de D117 : lancé par son chemin réel, un répartiteur lié
// sous plusieurs noms d'événement voyait « $0 » changer, et « basename "$0" »
// ne désignait plus l'événement.
func TestUnHookLieEstLanceParLeNomDesigne(t *testing.T) {
	for _, privilégié := range []bool{false, true} {
		runner, proj, dir := fixture(t)
		if privilégié {
			enRoot(t, runner)
		}
		trace := filepath.Join(dir, "trace")
		writeHook(t, dir, "repartiteur", "#!/bin/sh\nbasename \"$0\" >> "+trace+"\n", 0o750)
		for _, nom := range []string{"post-create", "post-apply"} {
			if err := os.Symlink("repartiteur", filepath.Join(dir, nom)); err != nil {
				t.Fatal(err)
			}
		}
		for _, ev := range []string{PostCreate, PostApply} {
			if err := runner.Run(context.Background(), proj, ev, Context{}); err != nil {
				t.Fatal(err)
			}
		}
		if data, _ := os.ReadFile(trace); string(data) != "post-create\npost-apply\n" {
			t.Errorf("privilégié=%v : $0 vu par le hook = %q", privilégié, data)
		}
	}
}
