package archive

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// Une arborescence de projet réaliste, avec un lien symbolique.
	root := filepath.Join(dir, "home", "shop.fr")
	for _, sub := range []string{"www/public", "var/log", "logs", ".bin"} {
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
	if err := os.Symlink("/usr/bin/php8.5", filepath.Join(root, ".bin", "php")); err != nil {
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
	if err := w.AddTree("tree", "tree", root); err != nil {
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
	count, err := Extract(path, "tree", dest)
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
	target, err := os.Readlink(filepath.Join(dest, ".bin", "php"))
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
	if _, err := Extract(path, "tree", dest); err == nil {
		t.Fatal("l'extraction devait être refusée")
	}
	if _, err := os.Stat(filepath.Join(dir, "evasion.txt")); !os.IsNotExist(err) {
		t.Error("aucun fichier ne doit avoir été écrit hors de la destination")
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
