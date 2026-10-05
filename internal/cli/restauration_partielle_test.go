package cli

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"leblanc.io/open-go-server/internal/archive"
)

// restaurationAuditée dit si l'audit porte une restauration de id sous le
// résultat donné.
func (h *harness) restaurationAuditée(t *testing.T, résultat string) bool {
	t.Helper()
	for _, ligne := range strings.Split(h.read(t, filepath.Join("var", "log", "ogs", "audit.log")), "\n") {
		if strings.Contains(ligne, "project.restore") && strings.Contains(ligne, " résultat="+résultat+" ") {
			return true
		}
	}
	return false
}

// Q-01 : le chemin --with-data, celui par défaut, n'était éprouvé par aucun
// test. Une base que la restauration ne recharge pas laisse le projet en place
// et sa base vide : succès partiel, qui doit nommer la base et le remède, et
// s'auditer comme tel — jamais « ok ».
func TestUneRestaurationSansSaBaseEstUnSuccesPartielNomme(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	moteur := demarrerMySQLFactice(t, h.path("run", "mysqld", "mysqld.sock"))
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5",
		"--db", "mysql", "--tls=false", "--yes")
	déposer(t, h.path("usr", "bin", "mysqldump"), "#!/bin/sh\n")
	h.mustRunPartial("project", "delete", "shopfr", "--yes")
	if !moteur.reçu("DROP DATABASE IF EXISTS `shopfr`") {
		t.Fatalf("la base devait partir avec le projet : %q", moteur.requêtes())
	}
	archive := h.derniereArchive(t)

	// Le client du moteur est absent : la base est recréée vide, non rechargée.
	out, code := h.run("project", "restore", archive, "--yes")
	if code != CodePartial {
		t.Fatalf("code %d, %d attendu :\n%s", code, CodePartial, out)
	}
	sortie, motif, _ := strings.Cut(out, "\nogs : ")
	if !strings.Contains(motif, "base mysql shopfr non rechargée") {
		t.Errorf("DÉFAUT : le motif du code 6 doit nommer la base : %q", motif)
	}
	// Le remède : la sauvegarde reste dans l'archive, et ogs la recharge
	// lui-même — ni tar ni zstd, que Debian n'installe pas.
	remède := "ogs db restore shopfr shopfr --from-archive " + archive
	if !strings.Contains(sortie, remède) || strings.Contains(sortie, "tar --zstd") {
		t.Errorf("DÉFAUT : l'avertissement doit donner %q :\n%s", remède, sortie)
	}
	if strings.Contains(out, "✓ shopfr restauré") {
		t.Errorf("DÉFAUT : une restauration sans sa base ne s'annonce pas réussie :\n%s", out)
	}
	if h.restaurationAuditée(t, "ok") || !h.restaurationAuditée(t, "avertissement") {
		t.Errorf("DÉFAUT : la restauration partielle doit s'auditer « avertissement », jamais « ok »")
	}
	// Le remède fonctionne, une fois le client installé.
	déposer(t, h.path("usr", "bin", "mysql"), "#!/bin/sh\n")
	out = h.mustRun("db", "restore", "shopfr", "shopfr", "--from-archive", archive, "--yes")
	if !strings.Contains(out, "✓ base mysql shopfr restaurée") {
		t.Errorf("la base doit être rechargée depuis l'archive :\n%s", out)
	}
	// Une archive d'un autre projet, ou sans sauvegarde de la base, est refusée.
	if _, code := h.run("db", "restore", "shopfr", "shopfr", "--from-archive", h.path("etc", "ogs", "projects", "shopfr.yaml"),
		"--yes"); code != CodeUsage {
		t.Errorf("un fichier qui n'est pas une archive doit être refusé en code %d, obtenu %d", CodeUsage, code)
	}
}

// Le chemin --with-data réussi : la sauvegarde passe par le client du moteur,
// lancé sous l'utilisateur du projet (D92), et rien ne manque.
func TestUneRestaurationAvecSaBaseLaRecharge(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	demarrerMySQLFactice(t, h.path("run", "mysqld", "mysqld.sock"))
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5",
		"--db", "mysql", "--tls=false", "--yes")
	déposer(t, h.path("usr", "bin", "mysqldump"), "#!/bin/sh\n")
	h.mustRunPartial("project", "delete", "shopfr", "--yes")
	déposer(t, h.path("usr", "bin", "mysql"), "#!/bin/sh\n")
	out := h.mustRunPartial("project", "restore", h.derniereArchive(t), "--yes")
	if !strings.Contains(out, "✓ base mysql shopfr restaurée") {
		t.Errorf("la base doit être rechargée :\n%s", out)
	}
	if strings.Contains(out, "base mysql shopfr non rechargée") {
		t.Errorf("rien ne manque à la base :\n%s", out)
	}
}

// Un fichier d'environnement que la restauration ne peut pas réécrire — un
// fichier déjà posé à son emplacement — rendait 0 et « ✓ restauré » : les
// secrets de l'application n'étaient plus nulle part hors de l'archive.
func TestUnFichierDEnvironnementNonRestaureEstUnSuccesPartiel(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "api.fr", "--kind", "custom", "--command",
		"/home/api.fr/www/bin/serveur", "--env-file")
	h.mustRun("apply", "apifr", "--yes")
	envFile := h.path("etc", "ogs", "env", "apifr.env")
	if err := os.WriteFile(envFile, []byte("JETON=secret-a-retrouver\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.mustRun("project", "delete", "apifr", "--yes")
	if err := os.WriteFile(envFile, []byte("POSE=a-la-main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, code := h.run("project", "restore", h.derniereArchive(t), "--yes")
	if code != CodePartial {
		t.Fatalf("DÉFAUT : code %d, %d attendu :\n%s", code, CodePartial, out)
	}
	sortie, motif, _ := strings.Cut(out, "\nogs : ")
	if !strings.Contains(motif, envFile) {
		t.Errorf("DÉFAUT : le motif du code 6 doit nommer le fichier : %q", motif)
	}
	if !strings.Contains(sortie, "ogs secrets show apifr app/envfile") {
		t.Errorf("DÉFAUT : l'avertissement doit donner le remède :\n%s", sortie)
	}
	if data, _ := os.ReadFile(envFile); string(data) != "POSE=a-la-main\n" {
		t.Errorf("le fichier en place ne doit pas être écrasé : %q", data)
	}
	if h.restaurationAuditée(t, "ok") || !h.restaurationAuditée(t, "avertissement") {
		t.Error("DÉFAUT : la restauration partielle s'audite « avertissement », jamais « ok »")
	}
	// Le contenu archivé reste lisible : le remède annoncé fonctionne.
	if got := h.mustRun("secrets", "show", "apifr", "app/envfile"); !strings.Contains(got, "JETON=secret-a-retrouver") {
		t.Errorf("le contenu archivé doit rester au keystore :\n%s", got)
	}
	// Une nouvelle suppression archive le fichier en place, une seule fois : la
	// copie gardée au keystore ne l'écrase pas.
	première := h.derniereArchive(t)
	if err := os.Rename(première, filepath.Join(t.TempDir(), "lue.tar.zst")); err != nil {
		t.Fatal(err)
	}
	h.mustRun("project", "delete", "apifr", "--yes")
	h.mustRun("project", "restore", h.derniereArchive(t), "--yes")
	if data, _ := os.ReadFile(envFile); string(data) != "POSE=a-la-main\n" {
		t.Errorf("le fichier en place à la suppression fait foi : %q", data)
	}
}

// B1 (D104) : restauré sous une autre clé maître, le fichier d'environnement
// reste scellé au keystore, et l'apply de la restauration pose à sa place le
// modèle vide d'ogs. La suppression suivante archivait ce modèle et écartait la
// copie scellée : rendue la clé d'origine, la restauration rendait 0 sur un
// fichier vide, les secrets de l'application perdus sans un mot.
func TestLaCopieScelleeDuFichierDEnvironnementSurvitAuModele(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "api.fr", "--kind", "custom", "--command",
		"/home/api.fr/www/bin/serveur", "--env-file")
	h.mustRun("apply", "apifr", "--yes")
	envFile := h.path("etc", "ogs", "env", "apifr.env")
	if err := os.WriteFile(envFile, []byte("JETON=secret-a-retrouver\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.mustRun("project", "delete", "apifr", "--yes")
	archives := t.TempDir()
	mettreDeCôté := func(nom string) string {
		t.Helper()
		dest := filepath.Join(archives, nom)
		if err := os.Rename(h.derniereArchive(t), dest); err != nil {
			t.Fatal(err)
		}
		return dest
	}
	première := mettreDeCôté("première.tar.zst")

	// Une autre clé maître : la copie scellée ne se rouvre pas.
	clé := h.path("etc", "ogs", "master.key")
	if err := os.Rename(clé, filepath.Join(archives, "master.key")); err != nil {
		t.Fatal(err)
	}
	h.mustRun("init")
	if _, code := h.run("project", "restore", première, "--yes"); code != CodePartial {
		t.Fatalf("une restauration sans le fichier d'environnement rend %d, obtenu %d", CodePartial, code)
	}
	if data, _ := os.ReadFile(envFile); string(data) == "JETON=secret-a-retrouver\n" {
		t.Fatal("la copie ne pouvait pas être rouverte : le test ne prouverait rien")
	}

	// Supprimé de nouveau, puis restauré sous la clé d'origine.
	h.mustRun("project", "delete", "apifr", "--yes")
	seconde := mettreDeCôté("seconde.tar.zst")
	if err := os.Chmod(clé, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(archives, "master.key"), clé); err != nil {
		t.Fatal(err)
	}
	out, code := h.run("project", "restore", seconde, "--yes")
	if data, _ := os.ReadFile(envFile); string(data) != "JETON=secret-a-retrouver\n" {
		t.Errorf("DÉFAUT : le modèle vide d'ogs a supplanté la copie scellée (code %d) : %q\n%s", code, data, out)
	}
}

// Une copie scellée qu'un fichier écrit depuis par l'exploitant supplante n'est
// pas perdue pour autant : elle voyage dans l'archive, et retourne au keystore.
func TestUnFichierDEnvironnementEcritDepuisLEmporteSansPerdreLaCopie(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "api.fr", "--kind", "custom", "--command",
		"/home/api.fr/www/bin/serveur", "--env-file")
	h.mustRun("apply", "apifr", "--yes")
	envFile := h.path("etc", "ogs", "env", "apifr.env")
	if err := os.WriteFile(envFile, []byte("JETON=ancien\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.mustRun("project", "delete", "apifr", "--yes")
	if err := os.WriteFile(envFile, []byte("POSE=a-la-main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	première := filepath.Join(t.TempDir(), "première.tar.zst")
	if err := os.Rename(h.derniereArchive(t), première); err != nil {
		t.Fatal(err)
	}
	h.mustRunPartial("project", "restore", première, "--yes")
	if err := os.WriteFile(envFile, []byte("JETON=nouveau\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.mustRun("project", "delete", "apifr", "--yes")
	h.mustRun("project", "restore", h.derniereArchive(t), "--yes")
	if data, _ := os.ReadFile(envFile); string(data) != "JETON=nouveau\n" {
		t.Errorf("le fichier écrit depuis l'emporte : %q", data)
	}
	if got := h.mustRun("secrets", "show", "apifr", "app/envfile"); got != "JETON=ancien\n" {
		t.Errorf("la copie scellée doit retourner au keystore, octet pour octet : %q", got)
	}
}

// archiveFabriquée écrit une archive dont l'entrée de sauvegarde de shopfr est
// du type donné, et rend son chemin.
func archiveFabriquée(t *testing.T, typ byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fabriquée.tar.zst")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(enc)
	manifest := []byte(`{"version":2,"project":"shopfr"}`)
	entries := []tar.Header{
		{Name: archive.ManifestName, Mode: 0o600, Size: int64(len(manifest)), Typeflag: tar.TypeReg},
		{Name: "dumps/shopfr.sql", Mode: 0o600, Typeflag: typ, Linkname: "/etc/shadow"},
	}
	for i, h := range entries {
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := tw.Write(manifest); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []interface{ Close() error }{tw, enc, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// Une entrée de sauvegarde qui n'est pas un fichier ordinaire — un lien, un
// répertoire, dans une archive fabriquée — rendait un contenu vide, chargé en
// annonçant « ✓ base mysql shopfr restaurée », code 0.
func TestUneSauvegardeQuiNEstPasUnFichierEstRefusee(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	demarrerMySQLFactice(t, h.path("run", "mysqld", "mysqld.sock"))
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5",
		"--db", "mysql", "--tls=false", "--yes")
	déposer(t, h.path("usr", "bin", "mysql"), "#!/bin/sh\n")
	for _, typ := range []byte{tar.TypeSymlink, tar.TypeDir} {
		out, code := h.run("db", "restore", "shopfr", "shopfr", "--from-archive", archiveFabriquée(t, typ), "--yes")
		if code != CodeUsage || strings.Contains(out, "✓ base mysql shopfr restaurée") {
			t.Errorf("DÉFAUT : type %q : code %d, %d attendu :\n%s", typ, code, CodeUsage, out)
		}
	}
}
