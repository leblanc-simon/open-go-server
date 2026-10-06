package fs

import (
	"errors"
	"fmt"
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

// Le pool et le vhost d'un projet s'appellent tous deux shopfr.conf, et un même
// apply les sauvegarde dans la même seconde : la seconde copie écrasait la
// première, que le plan annonçait pourtant sauvegardée.
func TestBackupToNEcraseJamaisUneAutreSauvegarde(t *testing.T) {
	src := t.TempDir()
	dir := filepath.Join(t.TempDir(), "backups")
	now := time.Date(2026, 8, 23, 3, 4, 5, 0, time.UTC)
	originaux := map[string]string{}
	for _, sous := range []string{"pool.d", "sites-available"} {
		path := filepath.Join(src, sous, "shopfr.conf")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("retouche de "+sous+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		originaux[sous] = path
	}
	// Un lien posé d'avance au nom suivant n'est pas suivi : il est sauté.
	cible := filepath.Join(t.TempDir(), "cible")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cible, filepath.Join(dir, "shopfr.conf.ogs-bak-20260823-030405.2")); err != nil {
		t.Fatal(err)
	}

	contenus := map[string]string{}
	for _, path := range []string{originaux["pool.d"], originaux["sites-available"], originaux["pool.d"]} {
		dst, err := BackupTo(path, dir, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, pris := contenus[dst]; pris {
			t.Fatalf("%s a servi deux fois", dst)
		}
		data, _ := os.ReadFile(path)
		contenus[dst] = string(data)
	}
	attendus := []string{
		"shopfr.conf.ogs-bak-20260823-030405",
		"shopfr.conf.ogs-bak-20260823-030405.1",
		"shopfr.conf.ogs-bak-20260823-030405.3",
	}
	for _, nom := range attendus {
		dst := filepath.Join(dir, nom)
		want, ok := contenus[dst]
		if !ok {
			t.Errorf("sauvegarde %s absente ; obtenu %v", nom, contenus)
			continue
		}
		if data, err := os.ReadFile(dst); err != nil || string(data) != want {
			t.Errorf("%s contient %q (%v), attendu %q", nom, data, err, want)
		}
	}
	if _, err := os.Lstat(cible); !os.IsNotExist(err) {
		t.Errorf("la sauvegarde a suivi le lien posé à son nom : %v", err)
	}
}

// Deux rotations de la clé de déploiement dans la même seconde : la seconde
// sauvegarde écrasait la première, et l'ancienne clé privée était perdue.
func TestBackupDataToNEcraseJamaisUneAutreSauvegarde(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backups")
	now := time.Date(2026, 8, 23, 3, 4, 5, 0, time.UTC)
	contenus := map[string]string{}
	for _, cle := range []string{"première clé\n", "seconde clé\n"} {
		dst, err := BackupDataTo("id_ed25519", []byte(cle), 0o600, dir, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, pris := contenus[dst]; pris {
			t.Fatalf("%s a servi deux fois", dst)
		}
		contenus[dst] = cle
	}
	for dst, want := range contenus {
		info, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(dst); string(data) != want || info.Mode().Perm() != 0o600 {
			t.Errorf("%s contient %q en %04o, attendu %q en 0600", dst, data, info.Mode().Perm(), want)
		}
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

func TestInspectRemovalNeRetireQueCeQuOgsAEcritPourLeProjet(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	intact := Render(header(), []byte("[shopfr]\n"), ";")
	voisin := header()
	voisin.Project = "shopfr_api"
	parc := Header{Project: "3 projet(s)", Resource: "mail/dkim-keytable"}

	cas := []struct {
		nom, projet string
		path        string
		attendu     State
	}{
		{"fichier du projet, intact", "shopfr", write("intact.conf", intact), StateCurrent},
		{"fichier du projet, retouché", "shopfr",
			write("retouche.conf", append(append([]byte{}, intact...), "pm.max_children = 4\n"...)), StateHandEdited},
		{"fichier posé à la main", "shopfr", write("main.conf", []byte("[shopfr_admin]\nuser = shopfr\n")), StateForeign},
		{"fichier d'un autre projet", "shopfr", write("voisin.conf", Render(voisin, []byte("x\n"), ";")), StateForeign},
		{"fichier de parc, sans identifiant attendu", "", write("parc", Render(parc, []byte("x\n"), "#")), StateCurrent},
		{"fichier de parc sans la marque", "", write("parc-main", []byte("x\n")), StateForeign},
		{"fichier absent", "shopfr", filepath.Join(dir, "absent.conf"), StateAbsent},
		// Inspect cherche la marque dans les trois premières lignes : une ligne
		// posée au-dessus de l'en-tête fait un fichier retouché, que --force
		// retire, et non un fichier étranger qu'on ne pourrait jamais retirer.
		{"ligne ajoutée au-dessus de l'en-tête", "shopfr",
			write("dessus.conf", append([]byte("; posé à la main\n"), intact...)), StateHandEdited},
		{"ligne ajoutée au-dessus de l'en-tête d'un autre projet", "shopfr",
			write("dessus-voisin.conf", append([]byte("; posé à la main\n"), Render(voisin, []byte("x\n"), ";")...)),
			StateForeign},
	}
	for _, c := range cas {
		got, err := InspectRemoval(c.path, c.projet)
		if err != nil {
			t.Errorf("%s : erreur %v", c.nom, err)
		}
		if got != c.attendu {
			t.Errorf("%s : %v, attendu %v", c.nom, got, c.attendu)
		}
	}

	// Un lien vers un fichier du projet n'est pas ce qu'ogs a écrit : il n'est
	// jamais pris pour lui, et le suivre ferait juger la cible à sa place.
	lien := filepath.Join(dir, "lien.conf")
	if err := os.Symlink(filepath.Join(dir, "intact.conf"), lien); err != nil {
		t.Fatal(err)
	}
	if got, err := InspectRemoval(lien, "shopfr"); got != StateForeign || err != nil {
		t.Errorf("lien : %v (%v), attendu %v", got, err, StateForeign)
	}
	if got, err := InspectRemoval(dir, "shopfr"); got != StateForeign || err != nil {
		t.Errorf("répertoire : %v (%v), attendu %v", got, err, StateForeign)
	}
}

// La ressource range un fichier parmi ceux d'un provider : le vhost de
// supervision porte « parc » à la place d'un projet, et « parc » est aussi un
// identifiant valide.
func TestManagedResourceLitLaRessourceDeLEnTete(t *testing.T) {
	dir := t.TempDir()
	cas := []struct {
		nom, projet, ressource string
		contenu                []byte
	}{
		{"vhost d'un projet", "shopfr", "web/vhost/shopfr",
			Render(Header{Project: "shopfr", Resource: "web/vhost/shopfr"}, []byte("x\n"), "#")},
		{"vhost de supervision", "parc", "web/status",
			Render(Header{Project: "parc", Resource: "web/status", Template: "/etc/ogs/templates/status"},
				[]byte("x\n"), "#")},
		{"fichier posé à la main", "", "", []byte("# projet: shopfr  ressource: web/vhost/shopfr  empreinte: x\n")},
		// La marque se cherche là où Inspect la cherche : ce qu'Inspect juge
		// retouché reste attribué à son projet.
		{"ligne ajoutée au-dessus de l'en-tête", "shopfr", "web/vhost/shopfr",
			append([]byte("# posé à la main\n"),
				Render(Header{Project: "shopfr", Resource: "web/vhost/shopfr"}, []byte("x\n"), "#")...)},
		{"marque au-delà des trois premières lignes", "", "",
			append([]byte("#\n#\n#\n"),
				Render(Header{Project: "shopfr", Resource: "web/vhost/shopfr"}, []byte("x\n"), "#")...)},
		{"marque sans ligne de projet", "", "", []byte("# Généré par ogs\nserver {}\n")},
	}
	for i, c := range cas {
		path := filepath.Join(dir, fmt.Sprintf("f%d", i))
		if err := os.WriteFile(path, c.contenu, 0o644); err != nil {
			t.Fatal(err)
		}
		projet, ressource, err := ManagedResource(path)
		if err != nil || projet != c.projet || ressource != c.ressource {
			t.Errorf("%s : %q, %q (%v), attendu %q, %q", c.nom, projet, ressource, err, c.projet, c.ressource)
		}
	}
}

// Le fichier d'un autre projet n'est jamais écrit pour celui-ci ; un fichier
// sans la marque, absent ou du projet lui-même relève des cinq états.
func TestCheckOwnerRefuseLeFichierDUnAutreProjet(t *testing.T) {
	dir := t.TempDir()
	voisin := filepath.Join(dir, "shop_api.conf")
	if err := os.WriteFile(voisin, Render(Header{Project: "shop_api", Resource: "php/pool/shop_api"},
		[]byte("[shop_api]\n"), ";"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := CheckOwner(voisin, "shop", "pool « api » de shop")
	var owned *OwnerError
	if !errors.As(err, &owned) || owned.Owner != "shop_api" || owned.Project != "shop" {
		t.Fatalf("le fichier du voisin doit être refusé : %v", err)
	}
	if !strings.Contains(err.Error(), "--force compris") || !strings.Contains(err.Error(), "« ogs apply shop_api »") {
		t.Errorf("le refus doit dire que --force n'y peut rien, et renvoyer au propriétaire : %v", err)
	}
	if !strings.HasPrefix(err.Error(), "pool « api » de shop : ") {
		t.Errorf("le refus doit nommer ce qui voulait écrire : %v", err)
	}
	// Un propriétaire disparu n'a plus d'apply qui le retire : le message ne
	// doit pas y renvoyer. L'appelant l'apprend après coup, et le message rendu
	// doit le refléter.
	owned.Gone = true
	if msg := err.Error(); strings.Contains(msg, "ogs apply") || !strings.Contains(msg, "n'est plus déclaré") {
		t.Errorf("propriétaire disparu : %s", msg)
	}

	// Retoucher le haut du fichier d'un voisin n'en fait pas un fichier sans
	// maître : Inspect le tiendrait pour modifié à la main, et --force le
	// réécrirait au compte du mauvais projet.
	retouche := filepath.Join(dir, "shop_b2b.conf")
	if err := os.WriteFile(retouche, append([]byte("; ajout\n"), Render(Header{Project: "shop_b2b",
		Resource: "php/pool/shop_b2b"}, []byte("[shop_b2b]\n"), ";")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckOwner(retouche, "shop", ""); !errors.As(err, &owned) || owned.Owner != "shop_b2b" {
		t.Errorf("le fichier retouché du voisin doit être refusé : %v", err)
	}

	manuel := filepath.Join(dir, "shop_old.conf")
	if err := os.WriteFile(manuel, []byte("[shop_old]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, projet string }{
		{voisin, "shop_api"}, {manuel, "shop"}, {filepath.Join(dir, "absent.conf"), "shop"},
	} {
		if err := CheckOwner(c.path, c.projet, ""); err != nil {
			t.Errorf("%s pour %s : %v", c.path, c.projet, err)
		}
	}
}

func TestEnsureSharedDirNeDesserreJamais(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cron.d")
	if err := EnsureSharedDir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o755 {
		t.Errorf("créé en %04o, attendu 0755", info.Mode().Perm())
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSharedDir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o750 {
		t.Errorf("rouvert en %04o", info.Mode().Perm())
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSharedDir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o755 {
		t.Errorf("un mode plus ouvert doit être ramené à 0755, obtenu %04o", info.Mode().Perm())
	}
	link := dir + ".lien"
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSharedDir(link, 0o755); err == nil {
		t.Error("un lien doit être refusé")
	}
}
