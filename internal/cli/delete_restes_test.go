package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"leblanc.io/open-go-server/internal/archive"
	ogsfs "leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// numéros rend les numéros attribués d'une plage, par projet et ressource.
func (h *harness) numéros(t *testing.T, scope string) map[string]int {
	t.Helper()
	app := h.ouvrirApp(t, true)
	list, err := app.Store.Allocations(scope)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, a := range list {
		out[a.Project+"/"+a.Name] = a.Number
	}
	app.close()
	return out
}

// secretsDe rend les noms des secrets scellés d'un projet.
func (h *harness) secretsDe(t *testing.T, id string) []string {
	t.Helper()
	app := h.ouvrirApp(t, true)
	names, err := app.Store.SecretNames(id)
	if err != nil {
		t.Fatal(err)
	}
	app.close()
	return names
}

// derniereArchive rend le chemin de l'unique archive d'un projet.
func (h *harness) derniereArchive(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(h.path("var", "lib", "ogs", "archives"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("archive attendue : %v %v", entries, err)
	}
	return h.path("var", "lib", "ogs", "archives", entries[len(entries)-1].Name())
}

// R1-20 (D100) : « plan » réservait un numéro à chaque pool calculé, la
// suppression n'en libérait aucun, et un numéro imposé par la déclaration
// n'était pas inscrit — l'attribution automatique d'un autre pool le prenait.
func TestNumérosDePoolRéservésAuSeulApplyEtLibérésALaSuppression(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5")

	h.mustRun("plan", "shopfr")
	h.mustRunPartial("diff", "shopfr")
	h.mustRunPartial("status")
	h.mustRun("--dry-run", "apply", "shopfr", "--yes")
	if got := h.numéros(t, "php-8.5"); len(got) != 0 {
		t.Fatalf("plan, diff, status et --dry-run ne doivent rien réserver : %v", got)
	}

	// Un apply refusé à la confirmation n'a rien écrit : rien ne reste réservé.
	h.stdin = "n\n"
	if _, code := h.run("apply", "shopfr"); code != CodeCancelled {
		t.Fatalf("refus de confirmation : code %d, attendu %d", code, CodeCancelled)
	}
	if got := h.numéros(t, "php-8.5"); len(got) != 0 {
		t.Fatalf("un apply refusé ne doit rien réserver : %v", got)
	}

	h.mustRun("apply", "shopfr", "--yes")
	if got := h.numéros(t, "php-8.5"); got["shopfr/shopfr"] != 18501 {
		t.Fatalf("le pool appliqué doit tenir 18501 : %v", got)
	}

	// Un numéro imposé est inscrit, et l'attribution automatique l'évite.
	h.mustRun("project", "create", "blog.fr", "--kind", "php-symfony", "--php", "8.5")
	h.modifierDéclaration(t, "blogfr", func(p *model.Project) { p.Spec.PHP.Pools[0].Number = "18502" })
	h.mustRun("apply", "blogfr", "--yes")
	h.mustRun("project", "create", "news.fr", "--kind", "php-symfony", "--php", "8.5")
	h.mustRun("apply", "newsfr", "--yes")
	got := h.numéros(t, "php-8.5")
	if got["blogfr/blogfr"] != 18502 || got["newsfr/newsfr"] != 18503 {
		t.Fatalf("le numéro imposé doit être réservé, l'automatique l'éviter : %v", got)
	}

	// La suppression libère le numéro, une fois le pool retiré.
	h.mustRun("project", "delete", "shopfr", "--yes")
	got = h.numéros(t, "php-8.5")
	if _, ok := got["shopfr/shopfr"]; ok {
		t.Fatalf("la suppression doit libérer le numéro du pool : %v", got)
	}
	h.mustRun("project", "create", "cc.fr", "--kind", "php-symfony", "--php", "8.5")
	h.mustRun("apply", "ccfr", "--yes")
	if got := h.numéros(t, "php-8.5"); got["ccfr/ccfr"] != 18501 {
		t.Errorf("le numéro libéré doit revenir à la plage : %v", got)
	}
}

// Un pool qui change de version libère le numéro de l'ancienne.
func TestPoolChangeDeVersionLibèreSonNuméro(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.4")
	h.mustRun("apply", "shopfr", "--yes")
	if got := h.numéros(t, "php-8.4"); got["shopfr/shopfr"] == 0 {
		t.Fatalf("numéro attendu en 8.4 : %v", got)
	}
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) { p.Spec.PHP.Pools[0].Version = "8.5" })
	h.mustRun("apply", "shopfr", "--yes")
	if got := h.numéros(t, "php-8.4"); len(got) != 0 {
		t.Errorf("le numéro de l'ancienne version doit être libéré : %v", got)
	}
	if got := h.numéros(t, "php-8.5"); got["shopfr/shopfr"] == 0 {
		t.Errorf("numéro attendu en 8.5 : %v", got)
	}
}

// Le port d'une application est libéré à la suppression, et quand le projet
// quitte son type à runtime — l'unité applicative partant alors avec lui
// (R1-74).
func TestPortApplicatifLibéréEtUnitéRetirée(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	command := "/home/api.fr/www/bin/serveur --listen ${LISTEN_ADDR}"
	h.mustRun("project", "create", "api.fr", "--kind", "custom", "--command", command)
	h.mustRun("apply", "apifr", "--yes")
	unit := h.path("etc", "systemd", "system", "ogs-apifr-app@.service")
	if _, err := os.Stat(unit); err != nil {
		t.Fatal(err)
	}
	if got := h.numéros(t, "app-custom"); got["apifr/app"] == 0 {
		t.Fatalf("port attendu : %v", got)
	}
	h.modifierDéclaration(t, "apifr", func(p *model.Project) {
		p.Spec.App.Kind = model.KindStatic
		p.Spec.App.Command = ""
	})
	h.mustRun("apply", "apifr", "--yes")
	if _, err := os.Lstat(unit); !os.IsNotExist(err) {
		t.Errorf("l'unité d'une application quittée doit partir : %v", err)
	}
	if got := h.numéros(t, "app-custom"); len(got) != 0 {
		t.Errorf("le port d'une application quittée doit être libéré : %v", got)
	}

	h.mustRun("project", "create", "web.fr", "--kind", "custom", "--command", "/home/web.fr/www/serveur")
	h.mustRun("apply", "webfr", "--yes")
	h.mustRun("project", "delete", "webfr", "--yes")
	if got := h.numéros(t, "app-custom"); len(got) != 0 {
		t.Errorf("la suppression doit libérer le port : %v", got)
	}
}

// Un numéro dont le fichier reste en place — un pool qu'ogs n'a pas écrit pour
// ce projet, laissé — n'est pas libéré : il le sert encore (D94).
func TestNuméroDUnFichierConservéNEstPasLibéré(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5")
	h.mustRun("apply", "shopfr", "--yes")
	pool := h.path("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf")
	data, err := os.ReadFile(pool)
	if err != nil {
		t.Fatal(err)
	}
	// Le pool réécrit sans la marque d'ogs : celui d'un site adopté.
	lines := strings.SplitN(string(data), "\n", 3)
	if err := os.WriteFile(pool, []byte(lines[2]), 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustRunPartial("project", "delete", "shopfr", "--keep-data", "--yes")
	if _, err := os.Stat(pool); err != nil {
		t.Fatalf("le pool non écrit par ogs doit rester : %v", err)
	}
	if got := h.numéros(t, "php-8.5"); got["shopfr/shopfr"] != 18501 {
		t.Errorf("le numéro d'un pool resté en place ne doit pas être libéré : %v", got)
	}
}

// R1-21 (D100) : les secrets restaient au keystore après la suppression, et un
// projet recréé sous le même identifiant en héritait — sa clé DKIM comprise.
func TestSuppressionVideLeKeystoreDuProjet(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("mail", "dkim", "enable", "shopfr")
	h.mustRun("apply", "shopfr", "--yes")
	if len(h.secretsDe(t, "shopfr")) == 0 {
		t.Fatal("la clé DKIM doit être scellée : le test ne prouverait rien")
	}
	h.mustRun("project", "delete", "shopfr", "--yes")
	if got := h.secretsDe(t, "shopfr"); len(got) != 0 {
		t.Errorf("les secrets doivent quitter le keystore : %v", got)
	}
	// L'archive en garde la copie, et la restauration les rend.
	secrets, err := archive.Secrets(h.derniereArchive(t))
	if err != nil || len(secrets) == 0 {
		t.Fatalf("l'archive doit porter les secrets : %v %v", secrets, err)
	}
	h.mustRunPartial("project", "restore", h.derniereArchive(t), "--yes")
	if got := h.secretsDe(t, "shopfr"); len(got) == 0 {
		t.Error("la restauration doit réinjecter les secrets")
	}
}

// Sous --keep-data, rien ne quitte le keystore : bases et files restent, avec ce
// qui les prouve (D91, D93).
func TestSuppressionKeepDataGardeLesSecrets(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("mail", "dkim", "enable", "shopfr")
	h.mustRun("apply", "shopfr", "--yes")
	before := h.secretsDe(t, "shopfr")
	out := h.mustRun("project", "delete", "shopfr", "--keep-data", "--yes")
	if !strings.Contains(out, "secret(s) conservé(s) au keystore") {
		t.Errorf("--keep-data doit dire que les secrets restent :\n%s", out)
	}
	if got := h.secretsDe(t, "shopfr"); len(got) != len(before) {
		t.Errorf("--keep-data ne doit retirer aucun secret : %v, avant %v", got, before)
	}
}

// R1-22 (D100) : le certificat et sa clé privée n'étaient ni archivés ni retirés,
// et doctor avertissait à vie de leur expiration.
func TestSuppressionArchiveEtRetireLeCertificat(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "shopfr", "--yes")
	dir := h.path("var", "lib", "ogs", "acme", "certificates", "shopfr")
	key, err := os.ReadFile(filepath.Join(dir, "privkey.pem"))
	if err != nil {
		t.Fatalf("certificat attendu après l'apply : %v", err)
	}
	h.mustRun("project", "delete", "shopfr", "--yes")
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("le certificat du projet doit partir avec lui : %v", err)
	}
	path := h.derniereArchive(t)
	compressed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := zstd.NewReader(nil)
	raw, err := dec.DecodeAll(compressed, nil)
	dec.Close()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("PRIVATE KEY")) {
		t.Error("la clé privée ne doit pas voyager en clair dans l'archive")
	}
	secrets, err := archive.Secrets(path)
	if err != nil || len(secrets[certSecretPrefix+"shopfr/privkey.pem"][1]) == 0 {
		t.Fatalf("la clé doit être scellée dans l'archive : %v", err)
	}
	h.mustRunPartial("project", "restore", path, "--yes")
	if got, err := os.ReadFile(filepath.Join(dir, "privkey.pem")); err != nil || !bytes.Equal(got, key) {
		t.Errorf("la restauration doit rendre le certificat d'origine (%v)", err)
	}
	for _, name := range h.secretsDe(t, "shopfr") {
		if strings.HasPrefix(name, certSecretPrefix) {
			t.Errorf("le certificat ne va pas au keystore : %s", name)
		}
	}
}

// Un certificat qu'un autre projet déclare sous le même nom — un parc antérieur à
// D94 — reste en place.
func TestSuppressionGardeUnCertificatDéclaréAilleurs(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("project", "create", "blog.fr", "--kind", "static")
	h.mustRun("apply", "--yes")
	h.modifierDéclaration(t, "blogfr", func(p *model.Project) {
		p.Spec.Certificates = []model.Certificate{{Name: "shopfr", Domains: []string{"blog.fr"}}}
	})
	dir := h.path("var", "lib", "ogs", "acme", "certificates", "shopfr")
	out := h.mustRunPartial("project", "delete", "shopfr", "--yes")
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("un certificat qu'un voisin déclare doit rester : %v\n%s", err, out)
	}
	if !strings.Contains(out, "certificat shopfr conservé") || !strings.Contains(out, "blogfr") {
		t.Errorf("le certificat conservé doit être nommé, avec son voisin :\n%s", out)
	}
}

// R1-76 (D100) : le fichier d'environnement, contenu de l'exploitant, partait
// sous --keep-data même.
func TestSuppressionKeepDataGardeLeFichierDEnvironnement(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "api.fr", "--kind", "custom", "--command", "/home/api.fr/www/serveur", "--env-file")
	h.mustRun("apply", "apifr", "--yes")
	envFile := h.path("etc", "ogs", "env", "apifr.env")
	if err := os.WriteFile(envFile, []byte("JETON=a-garder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("project", "delete", "apifr", "--keep-data", "--yes")
	if data, err := os.ReadFile(envFile); err != nil || string(data) != "JETON=a-garder\n" {
		t.Errorf("--keep-data doit garder le fichier d'environnement : %q %v\n%s", data, err, out)
	}
	if !strings.Contains(out, envFile+" conservé") {
		t.Errorf("le fichier conservé doit être nommé :\n%s", out)
	}
}

// Sous --purge, il part sans copie : il est nommé avant la saisie de
// l'identifiant, qui seule engage la purge.
func TestSuppressionPurgeAnnonceLeFichierDEnvironnement(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "api.fr", "--kind", "custom", "--command", "/home/api.fr/www/serveur", "--env-file")
	h.mustRun("apply", "apifr", "--yes")
	envFile := h.path("etc", "ogs", "env", "apifr.env")
	out, code := h.run("project", "delete", "apifr", "--purge")
	if code != CodeCancelled {
		t.Fatalf("sans saisie, la purge est annulée : code %d\n%s", code, out)
	}
	annonce := strings.Index(out, envFile+" (contenu de l'exploitant) sera effacé sans copie")
	if annonce < 0 || annonce > strings.Index(out, "Saisir l'identifiant") {
		t.Errorf("la purge doit nommer le fichier effacé sans copie avant la saisie :\n%s", out)
	}
	if _, err := os.Stat(envFile); err != nil {
		t.Errorf("une purge annulée n'efface rien : %v", err)
	}
}

// R1-74 (D100) : un modèle d'unité laissé par un service retiré avant la
// correction part avec le projet — l'en-tête l'attribue, la déclaration ne le
// nomme plus.
func TestSuppressionRetireLesUnitésQueLaDéclarationNeNommePlus(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "shopfr", "--yes")
	dir := h.path("etc", "systemd", "system")
	ancien := filepath.Join(dir, "ogs-shopfr-ancien@.service")
	cible := filepath.Join(dir, "ogs-shopfr.target")
	voisin := filepath.Join(dir, "ogs-shopfr-x@.service")
	for path, header := range map[string]ogsfs.Header{
		ancien: {Project: "shopfr", Resource: "unit/ancien"},
		cible:  {Project: "shopfr", Resource: "unit/target"},
		voisin: {Project: "shopfrx", Resource: "unit/x"},
	} {
		header.Generated = time.Now()
		if err := os.WriteFile(path, ogsfs.Render(header, []byte("[Unit]\n"), "#"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h.mustRun("project", "delete", "shopfr", "--yes")
	for _, path := range []string{ancien, cible} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s doit partir avec le projet : %v", path, err)
		}
	}
	if _, err := os.Stat(voisin); err != nil {
		t.Errorf("l'unité d'un autre projet doit rester : %v", err)
	}
}

// Un certificat laissé par une suppression antérieure à D100 : doctor le nomme,
// avec le geste qui le retire.
func TestDoctorNommeLeCertificatSansProjet(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "static")
	h.mustRun("apply", "shopfr", "--yes")
	certs := h.path("var", "lib", "ogs", "acme", "certificates")
	if err := os.Rename(filepath.Join(certs, "shopfr"), filepath.Join(certs, "ancien")); err != nil {
		t.Fatal(err)
	}
	out, _ := h.run("doctor")
	if !strings.Contains(out, "certificat ancien sans projet") || !strings.Contains(out, "ogs cert delete ancien") {
		t.Errorf("doctor doit nommer le certificat orphelin :\n%s", out)
	}
	if strings.Contains(out, "certificat shopfr sans projet") {
		t.Errorf("le certificat d'un projet du parc n'est pas orphelin :\n%s", out)
	}
}

// Un numéro imposé à un pool existant n'est inscrit que par l'étape qui réécrit
// le pool : le plan, ou un apply refusé, ne retirent pas l'ancien (D100).
func TestNuméroImposéInscritParLÉtape(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5")
	h.mustRun("apply", "shopfr", "--yes")
	h.modifierDéclaration(t, "shopfr", func(p *model.Project) { p.Spec.PHP.Pools[0].Number = "18540" })
	h.mustRun("plan", "shopfr")
	h.stdin = "n\n"
	h.run("apply", "shopfr")
	if got := h.numéros(t, "php-8.5"); got["shopfr/shopfr"] != 18501 {
		t.Fatalf("ni le plan ni un apply refusé ne déplacent le numéro : %v", got)
	}
	h.mustRun("apply", "shopfr", "--yes")
	if got := h.numéros(t, "php-8.5"); got["shopfr/shopfr"] != 18540 || len(got) != 1 {
		t.Errorf("l'apply inscrit le numéro imposé à la place de l'ancien : %v", got)
	}
}

// Le plan global d'un apply lit les pools de tout le parc pour le vhost d'état :
// il ne réserve pas le numéro d'un projet qu'aucun apply n'a visé (D100).
func TestPlanGlobalNeRéservePasLesProjetsNonAppliqués(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5")
	h.mustRun("project", "create", "blog.fr", "--kind", "php-symfony", "--php", "8.5")
	h.mustRun("apply", "shopfr", "--yes")
	got := h.numéros(t, "php-8.5")
	if _, ok := got["blogfr/blogfr"]; ok || got["shopfr/shopfr"] == 0 {
		t.Errorf("seul le projet appliqué tient un numéro : %v", got)
	}
}
