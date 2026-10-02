package archive

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"

	"leblanc.io/open-go-server/internal/fs"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// Une arborescence de projet réaliste, avec un lien symbolique.
	root := filepath.Join(dir, "home", "shop.fr")
	for _, sub := range []string{"www/public", "var/log", "logs", ".local/bin"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "www", "public", "index.php"),
		[]byte("<?php echo 'bonjour';"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("APP_ENV=prod\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/php8.5", filepath.Join(root, ".local", "bin", "php")); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "archives", FileName("shopfr", time.Date(2026, 8, 23, 14, 30, 0, 0, time.UTC)))
	w, err := Create(path, Manifest{Project: "shopfr", Host: "shop.fr", Env: "prod",
		Reason: "suppression du projet", OGSVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddBytes("spec", "project.yaml", "/etc/ogs/projects/shopfr.yaml",
		[]byte("apiVersion: ogs/v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.AddBytes("dump", "dumps/shopfr.sql", "", []byte("-- dump\nCREATE TABLE x;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.AddSecret("mysql/shopfr", []byte("nonce"), []byte("chiffré")); err != nil {
		t.Fatal(err)
	}
	if err := w.AddTree(context.Background(), "tree", "tree", root); err != nil {
		t.Fatal(err)
	}
	w.Note("utilisateur shopfr")
	w.Note("base mysql shopfr")
	manifest, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}

	// L'archive est en 0600 : elle contient les données de l'application.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %04o, attendu 0600", info.Mode().Perm())
	}
	if manifest.Secrets != 1 || len(manifest.Removed) != 2 {
		t.Errorf("manifeste : %+v", manifest)
	}

	// Le manifeste est relisible sans extraire.
	read, err := ReadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if read.Project != "shopfr" || read.Version != Version || read.Reason == "" {
		t.Errorf("manifeste relu : %+v", read)
	}
	if len(read.Entries) < 3 {
		t.Errorf("entrées : %+v", read.Entries)
	}

	// Un fichier isolé est extractible.
	spec, err := ReadFile(path, "project.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(spec), "ogs/v1") {
		t.Errorf("contenu : %q", spec)
	}

	// L'arborescence se restaure à l'identique, liens compris.
	dest := filepath.Join(dir, "restore")
	count, err := Extract(path, "tree", dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count < 3 {
		t.Errorf("%d fichiers restaurés", count)
	}
	restored, err := os.ReadFile(filepath.Join(dest, "www", "public", "index.php"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), "bonjour") {
		t.Errorf("contenu restauré : %q", restored)
	}
	// Les permissions du .env sont conservées : c'est un fichier sensible.
	if info, err := os.Stat(filepath.Join(dest, ".env")); err != nil {
		t.Errorf(".env absent : %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf(".env restauré en %04o, attendu 0600", info.Mode().Perm())
	}
	target, err := os.Readlink(filepath.Join(dest, ".local", "bin", "php"))
	if err != nil {
		t.Fatalf("lien non restauré : %v", err)
	}
	if target != "/usr/bin/php8.5" {
		t.Errorf("cible du lien : %q", target)
	}

	// Les secrets restent scellés.
	secrets, err := Secrets(path)
	if err != nil {
		t.Fatal(err)
	}
	pair, ok := secrets["mysql/shopfr"]
	if !ok {
		t.Fatalf("secret absent : %v", secrets)
	}
	if string(pair[0]) != "nonce" || string(pair[1]) != "chiffré" {
		t.Errorf("secret restitué : %q, %q", pair[0], pair[1])
	}
}

func TestCreateRefusesExistingArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.tar.zst")
	w, err := Create(path, Manifest{Project: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(path, Manifest{Project: "x"}); err == nil {
		t.Error("écraser une archive existante doit être refusé")
	}
}

func TestAbortLeavesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.tar.zst")
	w, err := Create(path, Manifest{Project: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddBytes("spec", "x.yaml", "", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.Abort()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("une archive interrompue ne doit pas subsister")
	}
}

func TestExtractRefusesPathTraversal(t *testing.T) {
	// Faille classique : une entrée d'archive nommée « ../../etc/passwd »
	// écrirait hors du répertoire cible.
	dir := t.TempDir()
	path := filepath.Join(dir, "piege.tar.zst")
	w, err := Create(path, Manifest{Project: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddBytes("tree", "tree/../../evasion.txt", "", []byte("dehors"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "dest")
	if _, err := Extract(path, "tree", dest, nil); err == nil {
		t.Fatal("l'extraction devait être refusée")
	}
	if _, err := os.Stat(filepath.Join(dir, "evasion.txt")); !os.IsNotExist(err) {
		t.Error("aucun fichier ne doit avoir été écrit hors de la destination")
	}
}

// archiver écrit les fichiers donnés dans une arborescence neuve, en crée les
// liens, et l'archive sous « tree ».
func archiver(t *testing.T, dir string, files map[string]string, links map[string]string) string {
	t.Helper()
	src := filepath.Join(dir, "source")
	for rel, body := range files {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for rel, target := range links {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "archive"+Extension)
	w, err := Create(path, Manifest{Project: "shopfr"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddTree(context.Background(), "tree", "tree", src); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractNeTraverseJamaisUnLienDeLaDestination(t *testing.T) {
	// La restauration extrait dans un home où l'apply vient de poser ses liens :
	// .ssh/authorized_keys vers le trousseau commun (D61), .local/bin/php vers
	// l'interpréteur (D70), ~/logs vers le répertoire root des journaux (D71).
	// L'archive porte le home tel que l'utilisateur l'avait laissé. Root ne doit
	// écrire à travers aucun de ces liens : chacun est remplacé en tant que lien.
	dir := t.TempDir()
	systeme := filepath.Join(dir, "systeme")
	journaux := filepath.Join(systeme, "journaux")
	if err := os.MkdirAll(journaux, 0o750); err != nil {
		t.Fatal(err)
	}
	trousseau := filepath.Join(systeme, "default_authorized_keys")
	php := filepath.Join(systeme, "php8.5")
	for f, body := range map[string]string{trousseau: "cle-admin\n", php: "vrai php\n"} {
		if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := archiver(t, dir, map[string]string{
		".ssh/authorized_keys":  "cle-du-projet\n",
		".local/bin/php":        "script du projet\n",
		"logs/depose.txt":       "déposé\n",
		"www/current/index.php": "<?php // release",
	}, map[string]string{
		"logs/apache-error.log.1": "/etc/shadow",
		"www/public":              "current",
	})

	home := filepath.Join(dir, "home")
	for _, d := range []string{".ssh", ".local/bin", "www/public"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "www", "public", "garde.txt"), []byte("posé par l'apply"), 0o600); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		".ssh/authorized_keys": trousseau, ".local/bin/php": php, "logs": journaux,
	} {
		if err := os.Symlink(target, filepath.Join(home, link)); err != nil {
			t.Fatal(err)
		}
	}

	count, err := Extract(path, "tree", home, nil)
	for f, want := range map[string]string{trousseau: "cle-admin\n", php: "vrai php\n"} {
		if data, _ := os.ReadFile(f); string(data) != want {
			t.Errorf("%s réécrit à travers un lien de la destination : %q", f, data)
		}
	}
	if entries, _ := os.ReadDir(journaux); len(entries) != 0 {
		t.Errorf("entrées posées dans le répertoire désigné par ~/logs : %v", entries)
	}
	for rel, want := range map[string]string{
		".ssh/authorized_keys": "cle-du-projet\n", ".local/bin/php": "script du projet\n",
		"logs/depose.txt": "déposé\n", "www/current/index.php": "<?php // release",
	} {
		info, lerr := os.Lstat(filepath.Join(home, rel))
		if lerr != nil || !info.Mode().IsRegular() {
			t.Errorf("%s doit être un fichier régulier : %v %v", rel, info, lerr)
			continue
		}
		if data, _ := os.ReadFile(filepath.Join(home, rel)); string(data) != want {
			t.Errorf("%s : %q, attendu %q", rel, data, want)
		}
	}
	if info, lerr := os.Lstat(filepath.Join(home, "logs")); lerr != nil || !info.IsDir() {
		t.Errorf("~/logs doit être redevenu le répertoire archivé : %v %v", info, lerr)
	}
	if target, _ := os.Readlink(filepath.Join(home, "logs", "apache-error.log.1")); target != "/etc/shadow" {
		t.Errorf("le lien archivé doit revenir tel quel, cible %q", target)
	}
	// Un répertoire que l'apply a peuplé n'est jamais remplacé par un lien : la
	// suite écrirait dedans. L'entrée est écartée, les autres sont restaurées, et
	// l'erreur la nomme.
	if err == nil || !strings.Contains(err.Error(), "www/public") {
		t.Errorf("l'entrée www/public doit être signalée comme écartée : %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(home, "www", "public", "garde.txt")); string(data) != "posé par l'apply" {
		t.Errorf("le répertoire existant doit rester intact : %q", data)
	}
	if count != 5 {
		t.Errorf("%d entrée(s) posée(s), 5 attendues (quatre fichiers et un lien)", count)
	}
}

func TestExtractRefuseUneEntreeSousUnLienDeLArchive(t *testing.T) {
	// L'archive elle-même peut poser un lien, puis une entrée sous ce lien : c'est
	// la forme classique de l'évasion d'une extraction, que le contrôle lexical des
	// chemins ne voit pas. Aucun composant n'est suivi : l'entrée est écartée.
	dir := t.TempDir()
	dehors := filepath.Join(dir, "dehors")
	if err := os.MkdirAll(dehors, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "piege"+Extension)
	w, err := Create(path, Manifest{Project: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.tar.WriteHeader(&tar.Header{Name: "tree/evasion", Linkname: dehors, Typeflag: tar.TypeSymlink}); err != nil {
		t.Fatal(err)
	}
	if err := w.AddBytes("tree", "tree/evasion/piege.txt", "", []byte("dehors"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.AddBytes("tree", "tree/sain.txt", "", []byte("sain"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "dest")
	_, err = Extract(path, "tree", dest, nil)
	if err == nil || !strings.Contains(err.Error(), "evasion/piege.txt") {
		t.Errorf("l'entrée sous le lien doit être écartée et nommée : %v", err)
	}
	if entries, _ := os.ReadDir(dehors); len(entries) != 0 {
		t.Errorf("root a écrit à travers le lien de l'archive : %v", entries)
	}
	if data, _ := os.ReadFile(filepath.Join(dest, "sain.txt")); string(data) != "sain" {
		t.Errorf("les entrées saines doivent être restaurées malgré tout : %q", data)
	}
}

// proprietaireDEssai rend un propriétaire que le test peut poser et qui se
// distingue de celui du processus : n'importe lequel en root, un groupe dont le
// processus est membre sans que ce soit le sien sinon. C'est celui qui prouve que
// chaque entrée l'a reçu.
func proprietaireDEssai(t *testing.T) fs.Owner {
	t.Helper()
	if os.Geteuid() == 0 {
		return fs.Owner{UID: 4242, GID: 4242}
	}
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g != os.Getgid() {
			return fs.Owner{UID: -1, GID: g}
		}
	}
	t.Skip("ni root ni groupe secondaire : le propriétaire posé ne se distinguerait pas")
	return fs.KeepOwner
}

// aRecu dit si info porte le propriétaire o, dont un UID négatif n'est pas posé.
func aRecu(info os.FileInfo, o fs.Owner) bool {
	st := info.Sys().(*syscall.Stat_t)
	return int(st.Gid) == o.GID && (o.UID < 0 || int(st.Uid) == o.UID)
}

func TestExtractDonneChaqueEntreeCreeeASonProprietaire(t *testing.T) {
	// L'extraction crée en root : ce qui n'était pas rendu restait à root — un
	// authorized_keys que sshd lit sous l'identité de l'utilisateur, les sessions
	// PHP, les caches, tout le site d'un projet adopté servi hors de www/. Chaque
	// entrée créée reçoit le propriétaire que lui donne l'appelant d'après son
	// chemin et celui que l'archive porte, sur son descripteur — les liens compris,
	// par lchown —, et un répertoire existant garde le sien.
	essai := proprietaireDEssai(t)
	dir := t.TempDir()
	path := archiver(t, dir, map[string]string{
		"www/public/index.php":         "<?php",
		".ssh/authorized_keys":         "cle",
		"var/sessions/sess_abc":        "a|s:1:\"b\";",
		".config/composer/auth.json":   "{}",
		"public_html/index.php":        "<?php // adopté",
		"var/cache/prod/container.php": "cache",
	}, map[string]string{".local/bin/php": "/usr/bin/php8.5"})
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, "www"), 0o710); err != nil {
		t.Fatal(err)
	}
	source := fs.Owner{UID: os.Getuid(), GID: os.Getgid()}
	demandes := map[string]fs.Owner{}
	owner := func(rel string, orig fs.Owner) fs.Owner {
		demandes[rel] = orig
		return essai
	}
	if _, err := Extract(path, "tree", home, owner); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".ssh/authorized_keys", "public_html/index.php", ".local/bin/php", "var/cache"} {
		if got, ok := demandes[rel]; !ok || got != source {
			t.Errorf("%s : propriétaire demandé avec %+v (%v), attendu celui de la source %+v", rel, got, ok, source)
		}
	}
	err := filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
		if err != nil || p == home {
			return err
		}
		rel, _ := filepath.Rel(home, p)
		switch {
		case rel == "www":
			if aRecu(info, essai) {
				t.Errorf("www existait : son propriétaire ne doit pas changer")
			}
		case !aRecu(info, essai):
			st := info.Sys().(*syscall.Stat_t)
			t.Errorf("%s restauré sans le propriétaire demandé (%d:%d, attendu %+v)", rel, st.Uid, st.Gid, essai)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// enTetes relit les en-têtes d'une archive, par nom.
func enTetes(t *testing.T, path string) map[string]*tar.Header {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	out := map[string]*tar.Header{}
	tr := tar.NewReader(dec)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = h
	}
}

func TestAddTreePorteLeProprietaireDeChaqueEntree(t *testing.T) {
	// Les en-têtes ne portaient aucun propriétaire : la restauration ne savait pas
	// ce qui était au projet, et rendait à l'utilisateur jusqu'au lien dur qu'il
	// aurait posé dans son home vers un fichier du système. Chaque en-tête porte
	// celui de son entrée, relevé sur le descripteur, et le manifeste celui de la
	// racine, auquel la restauration les compare.
	essai := proprietaireDEssai(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	autre := filepath.Join(src, "www", "autre.txt")
	for f, body := range map[string]string{filepath.Join(src, "www", "index.php"): "<?php", autre: "autre"} {
		if err := os.MkdirAll(filepath.Dir(f), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("index.php", filepath.Join(src, "www", "lien")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(autre, essai.UID, essai.GID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "a"+Extension)
	w, err := Create(path, Manifest{Project: "shopfr"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddTree(context.Background(), "tree", "tree", src); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}

	moi := fs.Owner{UID: os.Getuid(), GID: os.Getgid()}
	manifest, err := ReadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := manifest.RootOwner("tree"); got == nil || *got != moi {
		t.Errorf("propriétaire de la racine au manifeste : %+v, attendu %+v", got, moi)
	}
	if got := (Manifest{}).RootOwner("tree"); got != nil {
		t.Errorf("une archive antérieure ne porte aucun propriétaire : %+v", got)
	}
	headers := enTetes(t, path)
	for name, want := range map[string]fs.Owner{
		"tree/www/": moi, "tree/www/index.php": moi, "tree/www/lien": moi, "tree/www/autre.txt": essai,
	} {
		h, ok := headers[name]
		switch {
		case !ok:
			t.Errorf("%s absent de l'archive", name)
		case h.Gid != want.GID || (want.UID >= 0 && h.Uid != want.UID):
			t.Errorf("%s : en-tête %d:%d, attendu %+v", name, h.Uid, h.Gid, want)
		}
	}
}

func TestReadManifestOnTruncatedArchive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tronquee.tar.zst")
	w, err := Create(path, Manifest{Project: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddBytes("spec", "x.yaml", "", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Fermeture sans écrire le manifeste, comme le ferait une interruption.
	_ = w.tar.Close()
	_ = w.zstd.Close()
	_ = w.file.Close()
	if _, err := ReadManifest(path); err == nil {
		t.Error("une archive sans manifeste doit être signalée")
	}
}

func TestFileName(t *testing.T) {
	at := time.Date(2026, 8, 23, 14, 30, 5, 0, time.UTC)
	if got := FileName("shopfr", at); got != "shopfr-20260823-143005.tar.zst" {
		t.Errorf("FileName : %q", got)
	}
}

func TestAvailableSpace(t *testing.T) {
	// Archiver sans vérifier l'espace libre reviendrait à échouer au milieu d'une
	// suppression, après avoir déjà retiré des ressources.
	free, err := Available(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if free == 0 {
		t.Error("espace disponible nul : la mesure est-elle correcte ?")
	}
}

func TestExpiredUsesTheManifestDate(t *testing.T) {
	// L'âge se lit dans le manifeste, non dans l'horodatage du fichier : restaurer
	// une sauvegarde du répertoire d'archives ne doit pas rajeunir ce qu'il
	// contient, ni le vieillir.
	dir := t.TempDir()
	maintenant := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	ecrire := func(nom string, cree time.Time) string {
		chemin := filepath.Join(dir, nom)
		w, err := Create(chemin, Manifest{Project: "shopfr", CreatedAt: cree})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Close(); err != nil {
			t.Fatal(err)
		}
		// L'horodatage du fichier dit le contraire du manifeste, exprès.
		if err := os.Chtimes(chemin, maintenant, maintenant); err != nil {
			t.Fatal(err)
		}
		return chemin
	}
	vieille := ecrire("shopfr-vieille"+Extension, maintenant.AddDate(0, 0, -20))
	recente := ecrire("shopfr-recente"+Extension, maintenant.AddDate(0, 0, -3))

	expirees, err := Expired(dir, 14*24*time.Hour, maintenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(expirees) != 1 || expirees[0] != vieille {
		t.Errorf("attendu la seule archive de plus de 14 jours, obtenu %v", expirees)
	}
	if _, err := os.Stat(recente); err != nil {
		t.Errorf("l'archive récente ne doit pas être touchée : %v", err)
	}

	// Une rétention nulle ne purge rien : c'est « conserver indéfiniment ».
	expirees, err = Expired(dir, 0, maintenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(expirees) != 0 {
		t.Errorf("une rétention nulle ne doit rien purger, obtenu %v", expirees)
	}

	// Un répertoire absent n'est pas une erreur : rien n'a encore été archivé.
	if _, err := Expired(filepath.Join(dir, "absent"), time.Hour, maintenant); err != nil {
		t.Errorf("répertoire absent : %v", err)
	}
}

func TestSecretsPreserventLeDoubleSouligne(t *testing.T) {
	// Un nom de secret peut porter un double souligné : « shopfr__ro » est un
	// nom d'utilisateur de base valide (validate.reDBObject), et un projet adopté
	// garde ses noms réels. Déduire le nom du chemin le rendait « mysql/shopfr/ro »,
	// et le secret réinjecté sous ce nom faux ne se rouvre plus jamais — sa donnée
	// associée le lie à son nom.
	path := filepath.Join(t.TempDir(), "a.tar.zst")
	w, err := Create(path, Manifest{Project: "shopfr", OGSVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	noms := []string{"mysql/shopfr__ro", "amqp/app__worker", "mysql/shopfr"}
	for _, nom := range noms {
		if err := w.AddSecret(nom, []byte("nonce-"+nom), []byte("chiffré-"+nom)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	secrets, err := Secrets(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets) != len(noms) {
		t.Fatalf("%d secrets relus, %d attendus : %v", len(secrets), len(noms), secrets)
	}
	for _, nom := range noms {
		pair, ok := secrets[nom]
		if !ok {
			t.Errorf("nom de secret altéré : %q absent de %v", nom, secrets)
			continue
		}
		if string(pair[0]) != "nonce-"+nom || string(pair[1]) != "chiffré-"+nom {
			t.Errorf("%s : secret restitué %q, %q", nom, pair[0], pair[1])
		}
	}
}

func TestSecretsRelisentLeFormatAnterieur(t *testing.T) {
	// Les archives déjà produites ne portent pas le nom dans la charge : le
	// chemin reste le seul repli, avec son ambiguïté sur le double souligné.
	path := filepath.Join(t.TempDir(), "ancienne.tar.zst")
	w, err := Create(path, Manifest{Project: "shopfr", OGSVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string][]byte{
		"nonce": []byte("nonce"), "ciphertext": []byte("chiffré"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddBytes("secret", "secrets/mysql__shopfr.json", "", payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	secrets, err := Secrets(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := secrets["mysql/shopfr"]; !ok {
		t.Fatalf("archive antérieure illisible : %v", secrets)
	}
}

func TestNomDeFichierDunSecretResteLuParUnOgsAnterieur(t *testing.T) {
	// Le codage du nom de fichier a changé sans que la version du format bouge :
	// un ogs antérieur, qui ne lit que le chemin, restituait « mysql%2Fshopfr » et
	// réinjectait le secret sous ce nom faux, donc indéchiffrable. Le codage
	// historique est donc conservé partout où il est réversible, et l'échappement
	// réservé au seul cas qu'il corrompait — sans que les deux puissent se
	// confondre.
	path := filepath.Join(t.TempDir(), "a.tar.zst")
	w, err := Create(path, Manifest{Project: "shopfr", OGSVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	attendus := map[string]string{
		// Réversible : le codage historique, celui que l'ogs antérieur sait relire.
		"mysql/shopfr": "secrets/mysql__shopfr.json",
		// Ambigus : échappés, « _ » compris, sans quoi ces deux-là et le premier se
		// disputeraient le même fichier.
		"mysql__shopfr":    "secrets/mysql%5F%5Fshopfr.json",
		"mysql/shopfr__ro": "secrets/mysql%2Fshopfr%5F%5Fro.json",
	}
	for nom := range attendus {
		if err := w.AddSecret(nom, []byte("nonce"), []byte("chiffré-"+nom)); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	noms := map[string]bool{}
	for _, e := range manifest.Entries {
		if e.Kind == "secret" {
			noms[e.Name] = true
		}
	}
	for nom, fichier := range attendus {
		if !noms[fichier] {
			t.Errorf("%q rangé ailleurs que dans %q : %v", nom, fichier, noms)
		}
	}
	if len(noms) != len(attendus) {
		t.Fatalf("%d noms de fichier pour %d secrets — collision : %v",
			len(noms), len(attendus), noms)
	}
	secrets, err := Secrets(path)
	if err != nil {
		t.Fatal(err)
	}
	for nom := range attendus {
		if string(secrets[nom][1]) != "chiffré-"+nom {
			t.Errorf("%s : secret restitué %q", nom, secrets[nom][1])
		}
	}
}

func TestArchiveDunFormatPlusRecentEstRefusee(t *testing.T) {
	// Un ogs qui ne connaît pas le format doit le dire, plutôt que de déduire du
	// chemin un nom de secret que la charge exprimait autrement : réinjecté sous
	// un nom faux, le secret ne se rouvre plus jamais.
	path := filepath.Join(t.TempDir(), "future.tar.zst")
	w, err := Create(path, Manifest{Project: "shopfr", OGSVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddSecret("mysql/shopfr", []byte("nonce"), []byte("chiffré")); err != nil {
		t.Fatal(err)
	}
	w.manifest.Version = Version + 1
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(path); err == nil ||
		!strings.Contains(err.Error(), "mettre ogs à jour") {
		t.Errorf("ReadManifest accepte un format inconnu : %v", err)
	}
	if _, err := Secrets(path); err == nil || !strings.Contains(err.Error(), "mettre ogs à jour") {
		t.Errorf("Secrets accepte un format inconnu : %v", err)
	}
}

// horsDuHome pose, hors de l'arborescence archivée, un fichier que l'utilisateur
// du projet ne peut pas lire : c'est lui qu'une substitution chercherait à faire
// entrer dans l'archive, pour le retrouver dans son home à la restauration.
func horsDuHome(t *testing.T, dir string) (string, string) {
	t.Helper()
	dehors := filepath.Join(dir, "dehors")
	if err := os.MkdirAll(dehors, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := "root:$y$j9T$SECRET-HORS-DU-HOME:19000:0:99999:7:::\n"
	if err := os.WriteFile(filepath.Join(dehors, "shadow"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	return dehors, secret
}

func TestAddTreeNeSuitPasUnAncetreSubstitue(t *testing.T) {
	// L'archivage lit en root un home qui appartient à l'utilisateur, dont le code
	// tourne encore — shell ouvert (D62), tâches cron, pool PHP. filepath.Walk
	// atteignait chaque entrée par son chemin complet : une fois la liste de www/
	// lue, remplacer www par un lien vers un répertoire du système faisait lire les
	// entrées suivantes à travers ce lien, un Lstat ne protégeant que le dernier
	// composant. La substitution est déclenchée ici au moment exact où le parcours
	// ouvre www/aaa, ce que l'utilisateur obtient lui-même par inotify.
	dir := t.TempDir()
	dehors, secret := horsDuHome(t, dir)
	root := filepath.Join(dir, "home", "shop.fr")
	www := filepath.Join(root, "www")
	lent := filepath.Join(www, "aaa")
	if err := os.MkdirAll(lent, 0o750); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		if err := os.WriteFile(filepath.Join(lent, fmt.Sprintf("f%05d", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(www, "shadow"), []byte("leurre"), 0o644); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if _, err := unix.InotifyAddWatch(fd, lent, unix.IN_OPEN); err != nil {
		t.Fatal(err)
	}
	substitue := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		if _, err := unix.Read(fd, buf); err != nil {
			substitue <- err
			return
		}
		if err := os.Rename(www, www+".orig"); err != nil {
			substitue <- err
			return
		}
		substitue <- os.Symlink(dehors, www)
	}()
	path := filepath.Join(dir, "archives", "a"+Extension)
	w, err := Create(path, Manifest{Project: "shopfr"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddTree(context.Background(), "tree", "tree", root); err != nil {
		t.Fatalf("AddTree : %v", err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-substitue; err != nil {
		t.Fatalf("substitution : %v", err)
	}
	got, err := ReadFile(path, "tree/www/shadow")
	if err == nil && string(got) == secret {
		t.Fatal("un fichier extérieur au home est entré dans l'archive par un répertoire substitué")
	}
	// Le répertoire parcouru reste celui que l'on avait ouvert, sous son nouveau
	// nom : le leurre qu'il contient est archivé, rien d'autre.
	if err != nil || string(got) != "leurre" {
		t.Errorf("tree/www/shadow : %q (%v), attendu le leurre du répertoire ouvert", got, err)
	}
}

func TestAddTreeNOuvreJamaisUnLienSubstitueAUnFichier(t *testing.T) {
	// Le type d'une entrée était constaté par Lstat, puis le fichier ouvert par
	// os.Open, qui suit les liens : entre les deux, l'utilisateur substitue un lien
	// vers un fichier de même taille — l'en-tête reprenait la taille du Lstat — et
	// root en archivait le contenu. La substitution est faite ici au moment exact
	// qui sépare le constat de l'ouverture, celui qu'une course viserait : l'entrée
	// doit être ouverte sans suivre de lien, et écartée sans faire échouer
	// l'archive.
	dir := t.TempDir()
	dehors, secret := horsDuHome(t, dir)
	root := filepath.Join(dir, "home", "shop.fr")
	www := filepath.Join(root, "www")
	if err := os.MkdirAll(www, 0o750); err != nil {
		t.Fatal(err)
	}
	cible := filepath.Join(www, "cible")
	if err := os.WriteFile(cible, []byte(strings.Repeat("-", len(secret))), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(www, "voisin"), []byte("voisin"), 0o644); err != nil {
		t.Fatal(err)
	}
	substitue := false
	defer func(prev func(string)) { entryChecked = prev }(entryChecked)
	entryChecked = func(name string) {
		if name != "tree/www/cible" {
			return
		}
		if err := os.Remove(cible); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(filepath.Join(dehors, "shadow"), cible); err != nil {
			t.Error(err)
		}
		substitue = true
	}
	path := filepath.Join(dir, "a"+Extension)
	w, err := Create(path, Manifest{Project: "shopfr"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddTree(context.Background(), "tree", "tree", root); err != nil {
		t.Fatalf("une entrée changée sous nos pieds ne doit pas faire échouer l'archive : %v", err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if !substitue {
		t.Fatal("la substitution n'a pas eu lieu : le test n'éprouve rien")
	}
	got, err := ReadFile(path, "tree/www/cible")
	if err == nil && string(got) == secret {
		t.Fatal("entrée constatée régulière, puis ouverte à travers le lien substitué : le secret est archivé")
	}
	if err == nil {
		t.Errorf("l'entrée substituée devait être écartée, archivée : %q", got)
	}
	if got, err := ReadFile(path, "tree/www/voisin"); err != nil || string(got) != "voisin" {
		t.Errorf("les autres entrées doivent être archivées : %q %v", got, err)
	}
}

// bail pose un bail en écriture sur path, comme le propriétaire d'un fichier le
// fait sans privilège ; il est rendu à la fin du test.
func bail(t *testing.T, path string) {
	t.Helper()
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETLEASE, unix.F_WRLCK); err != nil {
		unix.Close(fd)
		t.Skipf("bail refusé par ce système de fichiers : %v", err)
	}
	t.Cleanup(func() {
		_, _ = unix.FcntlInt(uintptr(fd), unix.F_SETLEASE, unix.F_UNLCK)
		unix.Close(fd)
	})
}

// arbreSousBaux crée une arborescence de n fichiers, chacun sous bail.
func arbreSousBaux(t *testing.T, n int) (src string, w *Writer) {
	t.Helper()
	dir := t.TempDir()
	src = filepath.Join(dir, "home")
	for i := range n {
		sub := filepath.Join(src, fmt.Sprintf("d%02d", i))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(sub, "tenu")
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		bail(t, f)
	}
	w, err := Create(filepath.Join(dir, "a"+Extension), Manifest{Project: "shopfr"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Abort() })
	return src, w
}

// Les attentes de baux se bornent sur l'archive entière, non fichier par
// fichier : chaque bail coûtait 45 s, les attentes s'additionnaient, et un
// locataire qui en posait un par répertoire tenait le verrou global d'ogs des
// heures durant (Q-86).
func TestArchiveBorneLAttenteDesBauxSurLOpérationEntière(t *testing.T) {
	defer func(prev time.Duration) { leaseBudget = prev }(leaseBudget)
	leaseBudget = 300 * time.Millisecond
	src, w := arbreSousBaux(t, 20)
	début := time.Now()
	err := w.AddTree(context.Background(), "tree", "tree", src)
	if err == nil || !strings.Contains(err.Error(), "bail") && !strings.Contains(err.Error(), "baux") {
		t.Fatalf("des baux jamais rendus doivent faire échouer l'archive en les nommant : %v", err)
	}
	if d := time.Since(début); d > 5*time.Second {
		t.Errorf("l'archive a attendu %v : le budget de l'opération (%v) n'a pas été tenu", d, leaseBudget)
	}
}

// Une interruption arrête l'attente d'un bail aussitôt, et rend l'annulation :
// la suppression l'abandonne alors en code 9, avant le point de non-retour.
func TestArchiveSuitLeContextePendantUnBail(t *testing.T) {
	src, w := arbreSousBaux(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	début := time.Now()
	err := w.AddTree(ctx, "tree", "tree", src)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("l'interruption doit être rendue : %v", err)
	}
	if d := time.Since(début); d > 5*time.Second {
		t.Errorf("l'interruption a attendu %v", d)
	}
}

// Une interruption reçue entre deux entrées arrête le parcours.
func TestArchiveSuitLeContexteEntreDeuxEntrées(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "home")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := Create(filepath.Join(dir, "a"+Extension), Manifest{Project: "shopfr"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Abort()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.AddTree(ctx, "tree", "tree", src); !errors.Is(err, context.Canceled) {
		t.Fatalf("un contexte annulé doit arrêter le parcours : %v", err)
	}
}
