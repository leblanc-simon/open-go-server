package fs

import (
	"os"
	"path/filepath"
	"strings"
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

func TestBackupNommeLaCopieParSonHorodatage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf")
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst, err := BackupTo(path, filepath.Join(t.TempDir(), "backups"),
		time.Date(2026, 8, 23, 3, 4, 5, 0, time.UTC))
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

func TestBackupToSortDuRepertoireDOrigine(t *testing.T) {
	// Un répertoire lu en entier par son démon — /etc/logrotate.d — ne doit pas
	// recevoir la copie : elle y deviendrait une seconde configuration.
	src := t.TempDir()
	path := filepath.Join(src, "ogs")
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "backups")
	dst, err := BackupTo(path, dir, time.Date(2026, 8, 23, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dst) != dir {
		t.Errorf("sauvegarde déposée dans %s, attendue dans %s", filepath.Dir(dst), dir)
	}
	if filepath.Base(dst) != "ogs.ogs-bak-20260823-030405" {
		t.Errorf("nom de sauvegarde inattendu : %s", dst)
	}
	entries, _ := os.ReadDir(src)
	if len(entries) != 1 || entries[0].Name() != "ogs" {
		t.Errorf("le répertoire d'origine ne doit contenir que l'original, il contient %v", entries)
	}
	if data, _ := os.ReadFile(dst); string(data) != "original\n" {
		t.Errorf("contenu sauvegardé %q", data)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("mode du répertoire de sauvegarde %v, attendu 0700", info.Mode().Perm())
	}
}

func TestBackupToNeCreeRienSurUnOriginalAbsent(t *testing.T) {
	// L'appelant passe outre sur os.IsNotExist : le répertoire de sauvegarde ne
	// doit pas être créé pour rien.
	dir := filepath.Join(t.TempDir(), "backups")
	_, err := BackupTo(filepath.Join(t.TempDir(), "absent"), dir, time.Now())
	if !os.IsNotExist(err) {
		t.Fatalf("erreur attendue « fichier absent », obtenu : %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("le répertoire de sauvegarde n'avait pas lieu d'être créé")
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

func TestInspectSupporteUnCorpsCommencantParUnCommentaire(t *testing.T) {
	// Cinq fichiers de portée parc commencent leur corps par un commentaire — vhost
	// de supervision, fragments Telegraf et fail2ban, extrait nginx — et toute
	// surcharge de template peut faire de même. Les compter comme en-tête amputait
	// le corps : l'empreinte recalculée ne valait jamais celle annoncée, le fichier
	// passait pour modifié à la main, et chaque apply le réécrivait en rechargeant
	// le moteur au passage.
	corps := map[string][]byte{
		"commentaire":   []byte("# Entrées dérivées du parc.\n# suite\n<VirtualHost 127.0.0.1:8099>\n</VirtualHost>\n"),
		"vide":          nil,
		"ligne vide":    []byte("\n"),
		"point-virgule": []byte("; Pool dérivé du parc.\n[shopfr]\nuser = shopfr\n"),
	}
	for nom, body := range corps {
		t.Run(nom, func(t *testing.T) {
			commentaire := "#"
			if nom == "point-virgule" {
				commentaire = ";"
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "fragment.conf")
			h := Header{Project: "", Resource: "monitoring/telegraf",
				Generated: time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)}

			if err := os.WriteFile(path, Render(h, body, commentaire), 0o644); err != nil {
				t.Fatal(err)
			}

			// Même corps, rendu une minute plus tard : seul l'horodatage diffère.
			plusTard := h
			plusTard.Generated = h.Generated.Add(time.Minute)
			state, err := Inspect(path, Render(plusTard, body, commentaire))
			if err != nil {
				t.Fatal(err)
			}
			if state != StateCurrent {
				t.Errorf("état = %s, attendu conforme", state)
			}

			// Une vraie modification manuelle doit rester détectée.
			edite := append([]byte{}, Render(h, body, commentaire)...)
			edite = append(edite, []byte(commentaire+" ajout manuel\n")...)
			if err := os.WriteFile(path, edite, 0o644); err != nil {
				t.Fatal(err)
			}
			state, err = Inspect(path, Render(plusTard, body, commentaire))
			if err != nil {
				t.Fatal(err)
			}
			if state != StateHandEdited {
				t.Errorf("état = %s, attendu modifié à la main", state)
			}
		})
	}
}

func TestSplitHeaderNeConsommeQueLEnTete(t *testing.T) {
	// La troisième ligne d'en-tête n'est écrite que si le template est
	// surchargeable : splitHeader doit la reconnaître, et elle seule.
	body := []byte("# commentaire du corps\nServerName shop.fr\n")

	avec := Render(header(), body, "#")
	fp, extrait := splitHeader(avec)
	if fp != Fingerprint(body) {
		t.Errorf("empreinte %q, attendue %q", fp, Fingerprint(body))
	}
	if string(extrait) != string(body) {
		t.Errorf("corps extrait %q, attendu %q", extrait, body)
	}

	sans := header()
	sans.Template = ""
	fp, extrait = splitHeader(Render(sans, body, "#"))
	if fp != Fingerprint(body) {
		t.Errorf("sans template : empreinte %q, attendue %q", fp, Fingerprint(body))
	}
	if string(extrait) != string(body) {
		t.Errorf("sans template : corps extrait %q, attendu %q", extrait, body)
	}

	// Un corps qui imite la ligne d'empreinte ne doit pas se faire passer pour
	// l'en-tête : c'est la première ligne qui la porte qui fait foi.
	piege := []byte("# empreinte: sha256:" + strings.Repeat("0", 64) + "\nServerName shop.fr\n")
	fp, extrait = splitHeader(Render(sans, piege, "#"))
	if fp != Fingerprint(piege) {
		t.Errorf("leurre : empreinte %q, attendue %q", fp, Fingerprint(piege))
	}
	if string(extrait) != string(piege) {
		t.Errorf("leurre : corps extrait %q, attendu %q", extrait, piege)
	}
}

func TestEnsureDirRefuseUnLienSymbolique(t *testing.T) {
	// Les répertoires d'un projet vivent sous un home que son utilisateur écrit :
	// il peut en retirer un et poser à sa place un lien vers un répertoire du
	// système. Suivre ce lien poserait le mode et le propriétaire du projet sur la
	// cible — escalade vers root.
	base := t.TempDir()
	cible := filepath.Join(base, "cible")
	if err := os.Mkdir(cible, 0o755); err != nil {
		t.Fatal(err)
	}
	lien := filepath.Join(base, "www")
	if err := os.Symlink(cible, lien); err != nil {
		t.Fatal(err)
	}

	err := EnsureDir(lien, 0o710, KeepOwner)
	if err == nil {
		t.Fatal("un lien symbolique a été suivi sans un mot")
	}
	if !strings.Contains(err.Error(), "lien symbolique") {
		t.Errorf("erreur peu explicite : %v", err)
	}
	info, statErr := os.Stat(cible)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode de la cible = %v, attendu 0755 : le lien a été suivi", info.Mode().Perm())
	}
	if fi, err := os.Lstat(lien); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("le lien a été altéré : %v, %v", fi, err)
	}
}

func TestManagedProjectLitLeProjetDansLEnTete(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	voisin := header()
	voisin.Project = "shop_api"
	cas := []struct {
		nom, attendu string
		path         string
	}{
		{"fichier du voisin, dont le nom prolonge un autre identifiant", "shop_api",
			write("shop_api.conf", Render(voisin, []byte("[shop_api]\n"), ";"))},
		{"en-tête sans ligne de personnalisation", "shopfr",
			write("shopfr.conf", Render(Header{Project: "shopfr", Resource: "php/pool/shopfr"}, []byte("x\n"), "#"))},
		{"fichier posé à la main", "", write("shop_old.conf", []byte("[shop_old]\nuser = shop\n"))},
		{"corps qui imite un en-tête hors des premières lignes", "",
			write("imite.conf", []byte("server {}\n# Généré par ogs\n# projet: shopfr  ressource: x\n"))},
		{"fichier vide", "", write("vide.conf", nil)},
		{"fichier absent", "", filepath.Join(dir, "absent.conf")},
	}
	for _, c := range cas {
		got, err := ManagedProject(c.path)
		if err != nil {
			t.Errorf("%s : erreur %v", c.nom, err)
		}
		if got != c.attendu {
			t.Errorf("%s : projet %q, attendu %q", c.nom, got, c.attendu)
		}
	}

	// Un lien n'est jamais suivi, même vers un fichier géré : c'est la cible
	// qui porte l'en-tête, pas le lien.
	lien := filepath.Join(dir, "lien.conf")
	if err := os.Symlink(filepath.Join(dir, "shop_api.conf"), lien); err != nil {
		t.Fatal(err)
	}
	if got, err := ManagedProject(lien); got != "" || err != nil {
		t.Errorf("lien : projet %q, erreur %v ; attendu aucun projet, sans erreur", got, err)
	}
}
