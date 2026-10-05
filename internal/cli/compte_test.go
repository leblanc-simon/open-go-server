package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/archive"
	"leblanc.io/open-go-server/internal/exec"
)

// compteDuProjet inscrit le compte et le groupe de shopfr dans la base des
// comptes du préfixe, à l'UID de l'essai — la suppression les voit alors comme
// sur un vrai système — et rend l'enregistreur de la prochaine commande, préparé
// par prépare.
func (h *harness) compteDuProjet(t *testing.T, membres string, prépare func(*exec.Recorder)) func() *exec.Recorder {
	t.Helper()
	home := h.path("home", "shop.fr")
	passwd := fmt.Sprintf("shopfr:x:%d:%d:ogs=shopfr:%s:/bin/bash\n", os.Getuid(), os.Getgid(), home)
	group := fmt.Sprintf("shopfr:x:%d:%s\n", os.Getgid(), membres)
	if err := os.WriteFile(h.path("etc", "passwd"), []byte(passwd), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.path("etc", "group"), []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}
	var dernier *exec.Recorder
	précédent := newPrefixRecorder
	t.Cleanup(func() { newPrefixRecorder = précédent })
	newPrefixRecorder = func() *exec.Recorder {
		dernier = exec.NewRecorder()
		if prépare != nil {
			prépare(dernier)
		}
		return dernier
	}
	return func() *exec.Recorder { return dernier }
}

// rang rend la position de la première commande qui contient motif, -1 sinon.
func rang(rec *exec.Recorder, motif string) int {
	for i, c := range rec.Calls() {
		if strings.Contains(c.String(), motif) {
			return i
		}
	}
	return -1
}

func (h *harness) seuleArchive(t *testing.T) string {
	t.Helper()
	matches, _ := filepath.Glob(h.path("var", "lib", "ogs", "archives", "shopfr-*"))
	if len(matches) != 1 {
		t.Fatalf("une archive attendue : %v", matches)
	}
	return matches[0]
}

func TestSuppressionArchivePuisRetireCeQueLeCompteAPoseHorsDOgs(t *testing.T) {
	// La crontab personnelle, les tâches at et le linger survivaient au compte,
	// et cron comme atd les relisent par le nom : un compte recréé sous ce nom
	// les reprenait (R4-18). Les fichiers de l'UID dans les répertoires
	// temporaires passaient au compte suivant, à qui useradd redonne l'UID
	// (R4-19).
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	enregistreur := h.compteDuProjet(t, "www-data", func(r *exec.Recorder) {
		r.Responses["crontab -l"] = exec.Result{Stdout: "*/5 * * * * /home/shop.fr/tache.sh\n"}
		r.Responses["atq"] = exec.Result{Stdout: "7\tThu Oct  2 10:00:00 2026 a shopfr\n"}
		r.Responses["at -c"] = exec.Result{Stdout: "#!/bin/sh\necho sept\n"}
	})
	linger := h.path("var", "lib", "systemd", "linger", "shopfr")
	déposer(t, linger, "")
	session := h.path("tmp", "sess_shopfr")
	déposer(t, session, "session")

	out, code := h.run("project", "delete", "shopfr", "--yes")
	if code != CodeOK {
		t.Fatalf("code %d :\n%s", code, out)
	}
	rec := enregistreur()
	userdel := rang(rec, "userdel shopfr")
	if userdel < 0 {
		t.Fatalf("userdel attendu : %v", rec.Calls())
	}
	for _, motif := range []string{"crontab -r -u shopfr", "atrm 7", "loginctl disable-linger shopfr",
		fmt.Sprintf("systemctl stop user@%d.service", os.Getuid())} {
		if i := rang(rec, motif); i < 0 || i > userdel {
			t.Errorf("« %s » doit précéder userdel (%d, %d) : %v", motif, i, userdel, rec.Calls())
		}
	}
	for _, motif := range []string{"crontab personnelle de shopfr", "tâche(s) at de shopfr : 7",
		"linger de shopfr", session} {
		if !strings.Contains(out, motif) {
			t.Errorf("la sortie doit nommer %q :\n%s", motif, out)
		}
	}
	if _, err := os.Lstat(session); !os.IsNotExist(err) {
		t.Errorf("%s doit être retiré : %v", session, err)
	}
	path := h.seuleArchive(t)
	if data, err := archive.ReadFile(path, "user/crontab"); err != nil || !strings.Contains(string(data), "tache.sh") {
		t.Errorf("la crontab doit être dans l'archive : %q, %v", data, err)
	}
	if data, err := archive.ReadFile(path, "user/at/7"); err != nil || !strings.Contains(string(data), "echo sept") {
		t.Errorf("la tâche at doit être dans l'archive : %q, %v", data, err)
	}
	// Le groupe, à ogs et à personne d'autre, part après www-data.
	if g, d := rang(rec, "gpasswd -d www-data shopfr"), rang(rec, "groupdel shopfr"); g < userdel || d < g {
		t.Errorf("gpasswd -d puis groupdel attendus après userdel : %v", rec.Calls())
	}
}

func TestSuppressionSousKeepDataLaisseLeCompteEtCeQuIlTient(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	enregistreur := h.compteDuProjet(t, "www-data", func(r *exec.Recorder) {
		r.Responses["crontab -l"] = exec.Result{Stdout: "*/5 * * * * /home/shop.fr/tache.sh\n"}
	})
	session := h.path("tmp", "sess_shopfr")
	déposer(t, session, "session")
	out, code := h.run("project", "delete", "shopfr", "--keep-data", "--yes")
	if code != CodeOK && code != CodePartial {
		t.Fatalf("code %d :\n%s", code, out)
	}
	rec := enregistreur()
	for _, motif := range []string{"crontab", "atrm", "loginctl", "userdel", "groupdel"} {
		if rang(rec, motif) >= 0 {
			t.Errorf("--keep-data garde le compte : « %s » inattendu", motif)
		}
	}
	intact(t, session, "session")
	if !strings.Contains(out, "compte shopfr conservé") {
		t.Errorf("la sortie doit dire que le compte reste :\n%s", out)
	}
}

func TestSuppressionGardeUnGroupeQueDAutresComptesTiennent(t *testing.T) {
	// groupdel partait sans condition : le groupe homonyme d'un compte adopté,
	// qu'alice partageait, disparaissait sous elle (R1-25).
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	enregistreur := h.compteDuProjet(t, "www-data,alice", nil)
	out, code := h.run("project", "delete", "shopfr", "--yes")
	if code != CodePartial {
		t.Errorf("un groupe laissé en place est un succès partiel, code %d :\n%s", code, out)
	}
	rec := enregistreur()
	if rang(rec, "userdel shopfr") < 0 {
		t.Fatalf("le compte part quand même : %v", rec.Calls())
	}
	if rang(rec, "groupdel") >= 0 || rang(rec, "gpasswd") >= 0 {
		t.Errorf("le groupe d'alice ne doit pas être touché : %v", rec.Calls())
	}
	if !strings.Contains(out, "groupe shopfr conservé") || !strings.Contains(out, "alice") {
		t.Errorf("la sortie doit nommer le groupe conservé et alice :\n%s", out)
	}
}

func TestUnGroupdelEnEchecEstCompte(t *testing.T) {
	// L'échec de groupdel n'était qu'affiché : « ✓ supprimé », code 0 (R1-18).
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	h.compteDuProjet(t, "", func(r *exec.Recorder) {
		r.Errors["groupdel shopfr"] = errors.New("groupdel : impossible de supprimer le groupe")
	})
	out, code := h.run("project", "delete", "shopfr", "--yes")
	if code != CodePartial {
		t.Errorf("un groupdel en échec rend le code 6, obtenu %d :\n%s", code, out)
	}
	if !strings.Contains(out, "suppression du groupe shopfr") || strings.Contains(out, "✓ shopfr supprimé") {
		t.Errorf("l'échec doit être nommé, sans « ✓ » :\n%s", out)
	}
}

func TestWebAccessACLRefuseAuPlanJamaisAuChargement(t *testing.T) {
	// « acl » était accepté et implémenté nulle part : rendu comme « none », le
	// site tombait en 403 sans un mot (R1-29). Le refus vaut pour le projet visé ;
	// le reste du parc, et doctor, continuent.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	h.mustRun("site", "create", "blog.fr", "--kind", "static", "--tls=false", "--yes")
	h.modifierDeclaration(t, "shopfr", func(s string) string {
		return strings.Replace(s, "webAccess: group", "webAccess: acl", 1)
	})
	out, code := h.run("plan", "shopfr")
	if code != CodeValidation || !strings.Contains(out, "acl") {
		t.Errorf("le plan de shopfr doit refuser acl en code 3, obtenu %d :\n%s", code, out)
	}
	if _, code := h.run("apply", "shopfr", "--yes"); code != CodeValidation {
		t.Errorf("l'apply de shopfr doit refuser acl en code 3, obtenu %d", code)
	}
	h.mustRun("plan", "blogfr")
	if out, code := h.run("status"); code == CodeValidation {
		t.Errorf("le chargement du parc ne doit pas refuser acl :\n%s", out)
	}
	out, _ = h.run("doctor")
	if !strings.Contains(out, "accès web (shopfr)") {
		t.Errorf("doctor doit signaler acl :\n%s", out)
	}
}

func TestUnGroupeQuiNEstPasLePrimaireDuCompteResteEnCode6(t *testing.T) {
	// Un groupe homonyme qui n'est pas le groupe primaire du compte n'est pas
	// celui qu'ogs a créé avec lui : il reste, nommé, code 6 — non « = », code 0.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	enregistreur := h.compteDuProjet(t, "", nil)
	passwd := fmt.Sprintf("shopfr:x:%d:%d:ogs=shopfr:%s:/bin/bash\n", os.Getuid(), os.Getgid()+1,
		h.path("home", "shop.fr"))
	if err := os.WriteFile(h.path("etc", "passwd"), []byte(passwd), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := h.run("project", "delete", "shopfr", "--yes")
	if code != CodePartial || !strings.Contains(out, "pas le groupe primaire") {
		t.Errorf("le groupe conservé doit être nommé, code 6, obtenu %d :\n%s", code, out)
	}
	if rang(enregistreur(), "groupdel") >= 0 {
		t.Errorf("le groupe ne doit pas être touché : %v", enregistreur().Calls())
	}
}

func TestDoctorSignaleNoneSurUnTypeADocroot(t *testing.T) {
	// « none » n'accorde rien ; sur un type servi depuis un docroot en 0710, le
	// site ne vit que de l'appartenance de www-data, qu'ogs ne retire jamais.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	h.modifierDeclaration(t, "shopfr", func(s string) string {
		return strings.Replace(s, "webAccess: group", "webAccess: none", 1)
	})
	out, _ := h.run("doctor")
	if !strings.Contains(out, "accès web (shopfr)") || !strings.Contains(out, "none") {
		t.Errorf("doctor doit signaler none sur un site statique :\n%s", out)
	}
	if out, code := h.run("plan", "shopfr"); code == CodeValidation {
		t.Errorf("none n'est pas refusé :\n%s", out)
	}
}

func TestLaRestaurationDUneArchiveACLEstRefuseeAvantToutDepot(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	h.modifierDeclaration(t, "shopfr", func(s string) string {
		return strings.Replace(s, "webAccess: group", "webAccess: acl", 1)
	})
	h.mustRun("project", "delete", "shopfr", "--yes")
	out, code := h.run("project", "restore", h.seuleArchive(t), "--yes")
	if code != CodeValidation || !strings.Contains(out, "tar --zstd") {
		t.Errorf("la restauration doit refuser acl en code 3 avec un remède applicable, obtenu %d :\n%s", code, out)
	}
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "shopfr.yaml")); err == nil {
		t.Error("rien ne doit être déposé")
	}
}
