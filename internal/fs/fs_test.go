package fs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func header() Header {
	return Header{
		Project:   "shopfr",
		Resource:  "vhost/shopfr",
		Generated: time.Date(2026, 8, 23, 14, 30, 11, 0, time.UTC),
		Template:  "/etc/ogs/templates/apache/php-fpm.conf.tmpl",
	}
}

func TestRenderCarriesBodyFingerprint(t *testing.T) {
	body := []byte("ServerName shop.fr\n")
	out := Render(header(), body, "#")
	fp, extracted := splitHeader(out)
	if fp != Fingerprint(body) {
		t.Errorf("empreinte annoncée %q, attendue %q", fp, Fingerprint(body))
	}
	if string(extracted) != string(body) {
		t.Errorf("corps extrait %q, attendu %q", extracted, body)
	}
}

func TestInspectStates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vhost.conf")
	body := []byte("ServerName shop.fr\n")
	expected := Render(header(), body, "#")

	if st, _ := Inspect(path, expected); st != StateAbsent {
		t.Errorf("fichier absent : %v", st)
	}

	if err := WriteAtomic(path, expected, 0o644, KeepOwner); err != nil {
		t.Fatal(err)
	}
	if st, _ := Inspect(path, expected); st != StateCurrent {
		t.Errorf("fichier conforme : %v", st)
	}

	// Rendu différent, fichier intact : simple mise à jour.
	newExpected := Render(header(), []byte("ServerName shop.fr\nServerAlias www.shop.fr\n"), "#")
	if st, _ := Inspect(path, newExpected); st != StateOutdated {
		t.Errorf("fichier à mettre à jour : %v", st)
	}

	// Modification manuelle : le corps ne correspond plus à l'empreinte annoncée.
	handEdited := append([]byte{}, expected...)
	handEdited = append(handEdited, []byte("# ajout manuel\nServerAlias pirate.fr\n")...)
	if err := os.WriteFile(path, handEdited, 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ := Inspect(path, expected); st != StateHandEdited {
		t.Errorf("fichier modifié à la main : %v", st)
	}

	// Fichier étranger : aucune marque ogs.
	if err := os.WriteFile(path, []byte("ServerName autre.fr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ := Inspect(path, expected); st != StateForeign {
		t.Errorf("fichier étranger : %v", st)
	}
}

func TestWriteAtomicSetsModeAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pool.conf")
	if err := WriteAtomic(path, []byte("[shopfr]\n"), 0o640, KeepOwner); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, attendu 0640", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("%d entrées dans le répertoire, 1 attendue (temporaire non nettoyé ?)", len(entries))
	}
}

func TestBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf")
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst, err := Backup(path, time.Date(2026, 8, 23, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dst) != "conf.ogs-bak-20260823-030405" {
		t.Errorf("nom de sauvegarde inattendu : %s", dst)
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "original\n" {
		t.Errorf("contenu sauvegardé %q", data)
	}
}

func TestEnsureDirFixesMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := EnsureDir(dir, 0o750, KeepOwner); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(dir, 0o750, KeepOwner); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(dir)
	if info.Mode().Perm() != 0o750 {
		t.Errorf("mode non corrigé : %v", info.Mode().Perm())
	}
}

func TestParseMode(t *testing.T) {
	for s, want := range map[string]os.FileMode{"0750": 0o750, "0600": 0o600, "0755": 0o755} {
		got, err := ParseMode(s)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %v, %v", s, got, err)
		}
	}
	if _, err := ParseMode("zzz"); err == nil {
		t.Error("mode illisible accepté")
	}
}

func TestInspectIgnoresTheHeaderTimestamp(t *testing.T) {
	// L'en-tête porte l'instant du rendu : comparer les octets ferait apparaître
	// un écart dès la seconde suivante, et chaque apply réécrirait tous les
	// fichiers — avec un rechargement du serveur web à la clé.
	dir := t.TempDir()
	path := filepath.Join(dir, "vhost.conf")
	corps := []byte("<VirtualHost *:80>\n  ServerName shop.fr\n</VirtualHost>\n")
	h := Header{Project: "shopfr", Resource: "web/vhost/shopfr",
		Generated: time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)}

	if err := os.WriteFile(path, Render(h, corps, "#"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Même corps, rendu une minute plus tard.
	plusTard := h
	plusTard.Generated = h.Generated.Add(time.Minute)
	state, err := Inspect(path, Render(plusTard, corps, "#"))
	if err != nil {
		t.Fatal(err)
	}
	if state != StateCurrent {
		t.Errorf("état = %s, attendu conforme : seul l'horodatage diffère", state)
	}

	// En revanche, un corps réellement différent doit être vu comme à mettre à jour.
	autre := []byte("<VirtualHost *:80>\n  ServerName boutique.fr\n</VirtualHost>\n")
	state, err = Inspect(path, Render(plusTard, autre, "#"))
	if err != nil {
		t.Fatal(err)
	}
	if state != StateOutdated {
		t.Errorf("état = %s, attendu à mettre à jour", state)
	}
}
