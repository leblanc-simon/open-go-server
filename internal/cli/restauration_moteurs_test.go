package cli

import (
	"os"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/archive"
	"leblanc.io/open-go-server/internal/exec"
)

// R1-58 (D113) : les sauvegardes étaient rangées sous « dumps/<nom>.sql », sans
// le moteur. Deux bases homonymes d'un projet — « db add --engine postgres »
// reprend le nom de la base MySQL, l'identifiant du projet — partageaient ce
// nom, et la restauration rapportait la sauvegarde à la dernière base de ce nom
// déclarée : celle de MySQL rechargée dans PostgreSQL, MySQL jamais rechargée.
func TestDeuxBasesHomonymesSurDeuxMoteursGardentChacuneSaSauvegarde(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	demarrerMySQLFactice(t, h.path("run", "mysqld", "mysqld.sock"))
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5",
		"--db", "mysql", "--tls=false", "--yes")
	h.mustRun("db", "add", "shopfr", "--engine", "postgres")
	déposer(t, h.path("usr", "bin", "mysqldump"), "#!/bin/sh\n")
	h.mustRunPartial("project", "delete", "shopfr", "--keep-data", "--yes")
	chemin := h.derniereArchive(t)
	manifest, err := archive.ReadManifest(chemin)
	if err != nil {
		t.Fatal(err)
	}
	var dumps []string
	for _, e := range manifest.Entries {
		if e.Kind == "dump" {
			dumps = append(dumps, e.Name)
		}
	}
	if len(dumps) != 1 || dumps[0] != "dumps/mysql/shopfr.sql" {
		t.Errorf("DÉFAUT : la sauvegarde doit être rangée sous son moteur, « dumps/mysql/shopfr.sql » : %q", dumps)
	}

	déposer(t, h.path("usr", "bin", "mysql"), "#!/bin/sh\n")
	out, code := h.run("project", "restore", chemin, "--yes")
	if code != CodePartial && code != CodeOK {
		t.Fatalf("code %d :\n%s", code, out)
	}
	if !strings.Contains(out, "✓ base mysql shopfr restaurée") {
		t.Errorf("DÉFAUT : la sauvegarde MySQL doit être rechargée dans la base MySQL :\n%s", out)
	}
	if strings.Contains(out, "base postgres shopfr restaurée") || strings.Contains(out, "base postgres shopfr non rechargée") {
		t.Errorf("DÉFAUT : aucune sauvegarde n'est celle de la base PostgreSQL :\n%s", out)
	}
	// Le remède d'une base homonyme la désigne par son moteur.
	out = h.mustRun("db", "restore", "shopfr", "mysql:shopfr", "--from-archive", chemin, "--yes")
	if !strings.Contains(out, "✓ base mysql shopfr restaurée") {
		t.Errorf("« db restore » doit relire la sauvegarde rangée sous son moteur :\n%s", out)
	}
	if out, code := h.run("db", "restore", "shopfr", "shopfr", "--from-archive", chemin, "--yes"); code != CodeUsage ||
		!strings.Contains(out, "mysql:shopfr") {
		t.Errorf("DÉFAUT : un nom que portent deux bases est ambigu, et le refus nomme les deux formes (code %d) :\n%s",
			code, out)
	}
}

// R1-56 (D113) : l'apply de la restauration activait et démarrait les instances
// — et posait les tâches planifiées — avant que l'extraction n'ait rendu le
// home : l'application démarrait sur un home vide, échouait jusqu'à épuiser
// StartLimitBurst, et rien ne la relançait ensuite.
func TestLaRestaurationDemarreLesServicesApresLeContenu(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "php-symfony", "--app-version", "6.4", "--php", "8.5",
		"--tls=false", "--yes")
	h.mustRun("service", "add", "shopfr", "messenger-async", "--transport", "async")
	h.mustRun("cron", "add", "shopfr", "purge", "--schedule", "0 3 * * *", "--command", "bin/console app:purge")
	h.mustRunPartial("apply", "shopfr", "--yes")
	h.mustRunPartial("project", "delete", "shopfr", "--yes")

	out := h.mustRunPartial("project", "restore", h.derniereArchive(t), "--yes")
	contenu := strings.Index(out, "fichier(s) restauré(s)")
	démarrage := strings.Index(out, "✓ activation et démarrage de ogs-shopfr-messenger-async@1")
	tâches := strings.LastIndex(out, "✓ tâches planifiées")
	if contenu < 0 || démarrage < 0 {
		t.Fatalf("extraction et démarrage attendus :\n%s", out)
	}
	if démarrage < contenu {
		t.Errorf("DÉFAUT : l'instance démarre avant que le home ne soit rendu :\n%s", out)
	}
	if strings.Count(out, "✓ activation et démarrage de ogs-shopfr-messenger-async@1") != 1 {
		t.Errorf("l'instance ne doit être démarrée qu'une fois :\n%s", out)
	}
	if tâches >= 0 && tâches < contenu {
		t.Errorf("DÉFAUT : les tâches planifiées sont posées avant le contenu :\n%s", out)
	}
}

// R1-54 (D113) : la déclaration était écrite avant l'apply ; un apply en échec
// la laissait, avec les secrets, et toute relance était refusée — « le projet
// existe déjà : le supprimer d'abord ». La restauration inachevée est nommée, et
// se reprend par --resume une fois la cause levée.
func TestUneRestaurationInachevéeSeReprend(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "php-plain", "--php", "8.5", "--tls=false", "--yes")
	h.mustRunPartial("project", "delete", "shopfr", "--yes")
	archivePath := h.derniereArchive(t)

	// Un pool que ogs n'a pas écrit occupe la place de celui du projet : l'apply
	// de la restauration échoue.
	pool := h.path("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf")
	déposer(t, pool, "[shopfr]\n; pool de l'administrateur\n")
	out, code := h.run("project", "restore", archivePath, "--yes")
	if code == CodeOK || code == CodePartial {
		t.Fatalf("l'apply devait échouer (code %d) :\n%s", code, out)
	}
	if !strings.Contains(out, exec.ShellLine("ogs", "project", "restore", "--resume", archivePath)) ||
		!strings.Contains(out, "ogs project delete shopfr") {
		t.Errorf("DÉFAUT : l'échec doit nommer la reprise et l'abandon :\n%s", out)
	}
	if _, err := os.Stat(h.path("var", "lib", "ogs", "restores", "shopfr.json")); err != nil {
		t.Errorf("le témoin de la restauration inachevée doit rester : %v", err)
	}
	out, code = h.run("project", "restore", archivePath, "--yes")
	if code != CodeConflict || !strings.Contains(out, "--resume") {
		t.Errorf("DÉFAUT : une relance sans --resume doit renvoyer à la reprise (code %d) :\n%s", code, out)
	}

	// Le témoin ne reste pas invisible : status et doctor le nomment, et un
	// apply ordinaire avertit qu'il démarre les services sur un contenu non rendu.
	if out, _ := h.run("status", "shopfr"); !strings.Contains(out, "restauration inachevée") {
		t.Errorf("status doit nommer la restauration inachevée :\n%s", out)
	}
	if out, _ := h.run("doctor"); !strings.Contains(out, "restauration de shopfr") {
		t.Errorf("doctor doit nommer la restauration inachevée :\n%s", out)
	}
	if out, _ := h.run("apply", "shopfr", "--yes"); !strings.Contains(out, "restauration inachevée") {
		t.Errorf("un apply ordinaire doit avertir de la restauration inachevée :\n%s", out)
	}

	// Une déclaration retouchée n'est plus celle que la reprise connaît.
	decl := h.path("etc", "ogs", "projects", "shopfr.yaml")
	orig, err := os.ReadFile(decl)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(decl, append(append([]byte{}, orig...), "# retouche\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := h.run("project", "restore", "--resume", archivePath, "--yes"); code != CodeConflict {
		t.Errorf("une déclaration retouchée doit refuser la reprise (code %d) :\n%s", code, out)
	}
	if err := os.WriteFile(decl, orig, 0o600); err != nil {
		t.Fatal(err)
	}

	// La reprise ne sait pas simuler : --dry-run est refusé avant tout geste (D88).
	if out, code := h.run("project", "restore", "--resume", archivePath, "--dry-run", "--yes"); code != CodeUsage {
		t.Errorf("« project restore --resume --dry-run » doit refuser l'option (code %d) :\n%s", code, out)
	}

	// La cause levée, la reprise achève la restauration.
	if err := os.Remove(pool); err != nil {
		t.Fatal(err)
	}
	out, code = h.run("project", "restore", "--resume", archivePath, "--yes")
	if code != CodeOK && code != CodePartial {
		t.Fatalf("DÉFAUT : la reprise doit achever la restauration (code %d) :\n%s", code, out)
	}
	if strings.Contains(out, "secret(s) réinjecté(s)") {
		t.Errorf("la réinjection aboutie ne se rejoue pas :\n%s", out)
	}
	if !strings.Contains(out, "fichier(s) restauré(s)") {
		t.Errorf("l'arborescence, jamais extraite, doit l'être :\n%s", out)
	}
	if _, err := os.Stat(h.path("var", "lib", "ogs", "restores", "shopfr.json")); !os.IsNotExist(err) {
		t.Errorf("le témoin doit partir avec la restauration achevée : %v", err)
	}
	if out, code := h.run("project", "restore", "--resume", archivePath, "--yes"); code != CodeConflict {
		t.Errorf("plus rien à reprendre (code %d) :\n%s", code, out)
	}
}

// Le manifeste nomme le projet sous lequel la déclaration et le témoin sont
// écrits : une déclaration archivée d'un autre projet est refusée avant toute
// écriture.
func TestUneArchiveDontLeManifesteContreditLaDeclarationEstRefusee(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	chemin := h.path("autre.tar.zst")
	w, err := archive.Create(chemin, archive.Manifest{Project: "autrefr"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddFile("spec", "project.yaml", h.path("etc", "ogs", "projects", "shopfr.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if out, code := h.run("project", "restore", chemin, "--yes"); code != CodeUsage {
		t.Errorf("DÉFAUT : un manifeste qui contredit la déclaration doit être refusé en code %d, obtenu %d :\n%s",
			CodeUsage, code, out)
	}
	for _, f := range []string{h.path("etc", "ogs", "projects", "autrefr.yaml"),
		h.path("var", "lib", "ogs", "restores", "autrefr.json")} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s ne doit pas être écrit", f)
		}
	}
}
