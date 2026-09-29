package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"leblanc.io/open-go-server/internal/archive"
	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/exec"
	ogsfs "leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/system"
)

// projetDeRacine fabrique le strict nécessaire au dernier rempart : une racine et
// le marqueur d'adoption. Le projet n'est pas validé — c'est précisément le cas
// qu'on éprouve, celui d'une déclaration écrite avant que la validation ne se
// durcisse, ou à la main.
func projetDeRacine(root string, adopté bool) *model.Project {
	p := &model.Project{}
	p.Spec.Layout.Root = root
	if adopté {
		p.Metadata.Labels = map[string]string{"adopted": "true"}
	}
	return p
}

// appDeRempart construit le strict nécessaire au rempart de la racine, sans rien
// lire de la machine : un parc vide sous un préfixe jetable, une table des
// montages sans partage, des commandes simulées — le rempart consulte aussi le
// parc, le disque et les montages, et ceux du poste de test n'ont rien à y voir.
// La racine des homes reste /home : aucun préfixe n'est déclaré à l'App.
func appDeRempart(t *testing.T) *App {
	t.Helper()
	monterFactice(t)
	return &App{Cfg: config.Default(), Paths: config.NewPaths(t.TempDir()), Exec: exec.NewRecorder()}
}

func TestSuppressionRefuseUneRacineDangereuse(t *testing.T) {
	// os.RemoveAll ne demande rien à personne : le contrôle est refait au moment
	// du geste, la validation d'entrée pouvant n'avoir jamais vu ce fichier.
	a := appDeRempart(t)
	cas := []struct {
		racine string
		adopté bool
		motif  string
	}{
		{"/", false, "la racine du système"},
		{"", false, "une racine vide, donc un chemin relatif"},
		{"var/www/shop", false, "un chemin relatif"},
		{"/home", false, "la racine des homes elle-même"},
		{"/home/", true, "la racine des homes, adoption ou non"},
		{"/var/www", true, "un docroot partagé, adopté ou non"},
		{"/etc", true, "un emplacement système"},
		{"/srv/../var/log", true, "un emplacement système, une fois le chemin réduit"},
		{"/opt/appli/../..", true, "la racine du système, une fois le chemin réduit"},
		{"/var/www/boutique", false, "hors de la racine des homes sans adoption"},
	}
	for _, c := range cas {
		err := a.checkRemovableRoot(projetDeRacine(c.racine, c.adopté))
		if err == nil {
			t.Errorf("%q devait être refusée (%s)", c.racine, c.motif)
			continue
		}
		// Le chemin est nommé : l'exploitant doit savoir quoi corriger.
		if !strings.Contains(err.Error(), c.racine) && c.racine != "" {
			t.Errorf("le message doit nommer %q : %v", c.racine, err)
		}
		// Au moment d'effacer, la déclaration part aux archives juste après :
		// le refus dit ce qui reste, sans conseiller de relancer quoi que ce soit.
		if strings.Contains(err.Error(), "relancer") {
			t.Errorf("le dernier rempart ne doit conseiller aucune relance : %v", err)
		}
	}
}

func TestSuppressionAccepteLesRacinesLegitimes(t *testing.T) {
	// Le rempart ne doit rien empêcher de légitime : un projet du parc, et un
	// projet adopté qui garde le home réel de son compte (« ogs import »).
	a := appDeRempart(t)
	for _, c := range []struct {
		racine string
		adopté bool
	}{
		{"/home/shop.fr", false},
		{"/home/shop.fr/", false},
		{"/var/www/boutique", true},
		{"/srv/legacy/site", true},
	} {
		if err := a.checkRemovableRoot(projetDeRacine(c.racine, c.adopté)); err != nil {
			t.Errorf("%q devait être acceptée : %v", c.racine, err)
		}
	}
}

func TestSuppressionEffaceBienLArborescenceDUnProjetConforme(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")

	h.mustRun("project", "delete", "shopfr", "--yes")
	if _, err := os.Stat(h.path("home", "shop.fr")); !os.IsNotExist(err) {
		t.Errorf("l'arborescence du projet devait disparaître : %v", err)
	}
}

func TestSuppressionRefuseAvantLePointDeNonRetour(t *testing.T) {
	// Un rempart qui ne se dresse qu'à la dernière étape n'en est pas un : le
	// compte, les bases, les vhosts et la déclaration sont déjà partis quand
	// l'arborescence est refusée. La racine est donc éprouvée à l'entrée, et rien
	// n'est démonté.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")

	// Une racine que la validation d'alors laissait passer, écrite à la main dans
	// la déclaration : /var/www est partagé par tous les sites d'un serveur repris.
	decl := h.path("etc", "ogs", "projects", "shopfr.yaml")
	data, err := os.ReadFile(decl)
	if err != nil {
		t.Fatal(err)
	}
	patché := strings.ReplaceAll(string(data), h.path("home", "shop.fr"), "/var/www")
	if patché == string(data) {
		t.Fatalf("la racine du projet est attendue dans la déclaration :\n%s", data)
	}
	// Le label d'adoption : sans lui, c'est la validation d'entrée qui refuse une
	// racine hors de la racine des homes, et le rempart de la suppression ne serait
	// jamais atteint.
	patché = strings.Replace(patché, "  env: prod\n", "  env: prod\n  labels:\n    adopted: \"true\"\n", 1)
	if err := os.WriteFile(decl, []byte(patché), 0o600); err != nil {
		t.Fatal(err)
	}

	out, code := h.run("project", "delete", "shopfr", "--yes")
	if code != CodeValidation {
		t.Fatalf("une racine ineffaçable doit refuser le geste (code %d), obtenu %d :\n%s",
			CodeValidation, code, out)
	}
	if !strings.Contains(out, "--keep-data") {
		t.Errorf("le refus doit nommer la seule issue applicable :\n%s", out)
	}
	// Rien n'a été démonté : la déclaration est toujours à l'inventaire actif, et
	// le vhost du site n'a pas bougé.
	if _, err := os.Stat(decl); err != nil {
		t.Errorf("la déclaration doit rester à l'inventaire actif : %v", err)
	}
	if _, err := os.Stat(h.path("etc", "apache2", "sites-available", "shopfr.conf")); err != nil {
		t.Errorf("le vhost ne doit pas avoir été démonté : %v", err)
	}
}

func TestSuppressionSignaleUneArchiveSansDonnees(t *testing.T) {
	// Une archive à laquelle il manque un dump ne permet pas de restaurer le
	// site : rendre 0 ferait croire le contraire à qui enchaîne. Sous --keep-data
	// seulement : les bases restent, et la sauvegarde manquée ne coûte que cela.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5",
		"--db", "mysql", "--tls=false", "--yes")

	out, code := h.run("project", "delete", "shopfr", "--keep-data", "--yes")
	if !strings.Contains(out, "sauvegarde de") {
		t.Fatalf("un dump en échec doit être annoncé :\n%s", out)
	}
	if code != CodePartial {
		t.Errorf("une archive sans les données est un succès partiel, code %d :\n%s", code, out)
	}
}

// modifierDeclaration réécrit la déclaration d'un projet : c'est le cas qu'on
// éprouve, celui d'un fichier que la validation d'alors laissait passer, écrit à
// la main ou par une commande qui ne contrôlait pas le parc.
func (h *harness) modifierDeclaration(t *testing.T, id string, modifier func(string) string) {
	t.Helper()
	decl := h.path("etc", "ogs", "projects", id+".yaml")
	data, err := os.ReadFile(decl)
	if err != nil {
		t.Fatal(err)
	}
	modifié := modifier(string(data))
	if modifié == string(data) {
		t.Fatalf("la déclaration de %s n'a pas changé :\n%s", id, data)
	}
	if err := os.WriteFile(decl, []byte(modifié), 0o600); err != nil {
		t.Fatal(err)
	}
}

// adopter pose la marque d'adoption, celle que laisse « ogs import adopt ».
func adopter(decl string) string {
	return strings.Replace(decl, "  env: prod\n", "  env: prod\n  labels:\n    adopted: \"true\"\n", 1)
}

// déposer écrit un fichier témoin, en créant ses répertoires.
func déposer(t *testing.T, path, contenu string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contenu), 0o644); err != nil {
		t.Fatal(err)
	}
}

// intact exige qu'un fichier témoin ait survécu, contenu compris.
func intact(t *testing.T, path, contenu string) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || string(got) != contenu {
		t.Errorf("%s devait survivre intact : %q (%v)", path, got, err)
	}
}

// rienNAEteDemonte vérifie qu'un refus est tombé avant le premier geste : rien
// n'a été retiré, et les services n'ont pas même été arrêtés.
func (h *harness) rienNAEteDemonte(t *testing.T, id, sortie string) {
	t.Helper()
	h.rienNAEteRetire(t, id)
	if strings.Contains(sortie, "arrêt des services") {
		t.Errorf("le refus doit précéder l'arrêt des services :\n%s", sortie)
	}
}

// rienNAEteRetire vérifie qu'une suppression abandonnée n'a rien retiré : la
// déclaration est à l'inventaire actif, le vhost en place, aucune archive écrite.
func (h *harness) rienNAEteRetire(t *testing.T, id string) {
	t.Helper()
	if _, err := os.Stat(h.path("etc", "ogs", "projects", id+".yaml")); err != nil {
		t.Errorf("la déclaration doit rester à l'inventaire actif : %v", err)
	}
	if _, err := os.Stat(h.path("etc", "apache2", "sites-available", id+".conf")); err != nil {
		t.Errorf("le vhost ne doit pas avoir été démonté : %v", err)
	}
	if archives, _ := filepath.Glob(h.path("var", "lib", "ogs", "archives", id+"-*")); len(archives) > 0 {
		t.Errorf("aucune archive ne doit subsister d'une suppression refusée : %v", archives)
	}
}

// sortieEpiee recueille la sortie d'une commande et exécute un geste dès qu'un
// motif y paraît : l'état du système change alors entre deux étapes de la
// suppression, comme pendant une longue archive — un partage monté, un Ctrl+C.
type sortieEpiee struct {
	bytes.Buffer
	motif string
	geste func()
}

func (s *sortieEpiee) Write(p []byte) (int, error) {
	n, err := s.Buffer.Write(p)
	if s.geste != nil && strings.Contains(s.String(), s.motif) {
		geste := s.geste
		s.geste = nil
		geste()
	}
	return n, err
}

// runEpie exécute une commande comme run, sous le contexte ctx — celui qu'annule
// une interruption —, en déclenchant geste dès que motif paraît dans sa sortie.
func (h *harness) runEpie(ctx context.Context, motif string, geste func(), args ...string) (string, int) {
	h.t.Helper()
	out := &sortieEpiee{motif: motif, geste: geste}
	root := NewRootCommand()
	root.SetOut(out)
	root.SetErr(io.Discard)
	root.SetArgs(append([]string{"--prefix", h.prefix}, args...))
	err := root.ExecuteContext(ctx)
	if out.geste != nil {
		h.t.Fatalf("le motif %q n'a jamais paru :\n%s", motif, out.String())
	}
	if err != nil {
		out.WriteString("\nogs : " + err.Error() + "\n")
	}
	return out.String(), ExitCode(err)
}

func TestSuppressionRefuseLeHomeDUnVoisin(t *testing.T) {
	// Un clone de préproduction raté (« site clone shopfr --to prep ») déclarait
	// le home de shopcomprep, de même host effectif : le supprimer pour nettoyer
	// effaçait le home du voisin, toujours déclaré et servi, et l'archivait sous
	// le nom du clone.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "shop.com", "--env", "prep", "--kind", "static")
	h.mustRun("apply", "--yes")
	homeVoisin := h.path("home", "shop.preprod-pp.net")
	témoin := filepath.Join(homeVoisin, "www", "contenu-du-voisin.txt")
	déposer(t, témoin, "à shopcomprep\n")

	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	h.modifierDeclaration(t, "shopfr", func(s string) string {
		return strings.ReplaceAll(s, h.path("home", "shop.fr"), homeVoisin)
	})

	out, code := h.run("project", "delete", "shopfr", "--yes")
	if code != CodeConflict {
		t.Fatalf("une racine partagée doit refuser le geste (code %d), obtenu %d :\n%s", CodeConflict, code, out)
	}
	if !strings.Contains(out, "shopcomprep") || !strings.Contains(out, "--keep-data") {
		t.Errorf("le refus doit nommer le voisin et l'issue applicable :\n%s", out)
	}
	intact(t, témoin, "à shopcomprep\n")
	h.rienNAEteDemonte(t, "shopfr", out)

	// L'issue offerte fonctionne : le projet part, le voisin reste entier — et
	// l'archive du projet ne recueille pas son home, qu'elle rangeait sous le nom
	// du clone.
	out = h.mustRun("project", "delete", "shopfr", "--keep-data", "--yes")
	intact(t, témoin, "à shopcomprep\n")
	if _, err := os.Stat(h.path("etc", "apache2", "sites-available", "shopcomprep.conf")); err != nil {
		t.Errorf("le vhost du voisin doit rester : %v", err)
	}
	if !strings.Contains(out, "arborescence non archivée") || !strings.Contains(out, "shopcomprep") {
		t.Errorf("l'archive doit dire pourquoi elle ne recopie pas l'arborescence :\n%s", out)
	}
	if n := h.fichiersArchivés(t, "shopfr", "tree"); n != 0 {
		t.Errorf("l'archive ne doit rien recueillir du home du voisin : %d fichier(s)", n)
	}
}

// fichiersArchivés compte les fichiers qu'a recueillis, sous un préfixe,
// l'unique archive d'un projet.
func (h *harness) fichiersArchivés(t *testing.T, id, préfixe string) int {
	t.Helper()
	archives, _ := filepath.Glob(h.path("var", "lib", "ogs", "archives", id+"-*"))
	if len(archives) != 1 {
		t.Fatalf("une archive de %s est attendue : %v", id, archives)
	}
	n, err := archive.Extract(archives[0], préfixe, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSuppressionRefuseUneRacineImbriquee(t *testing.T) {
	// Deux docroots imbriqués, adoptés tels quels : /srv/boutique et
	// /srv/boutique/blog. Supprimer boutique.fr emportait le blog.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("project", "create", "boutique.fr", "--kind", "static")
	h.mustRun("project", "create", "blog.boutique.fr", "--kind", "static")
	racine := h.path("srv", "boutique")
	racineBlog := filepath.Join(racine, "blog")
	for id, r := range map[string][2]string{
		"boutiquefr":     {h.path("home", "boutique.fr"), racine},
		"blogboutiquefr": {h.path("home", "blog.boutique.fr"), racineBlog},
	} {
		h.modifierDeclaration(t, id, func(s string) string { return adopter(strings.ReplaceAll(s, r[0], r[1])) })
	}
	témoin := filepath.Join(racineBlog, "www", "billet.txt")
	déposer(t, témoin, "billet\n")

	// Le parc lui-même est refusé : l'unicité des homes porte aussi l'imbrication.
	if out, code := h.run("apply", "--yes"); code != CodeConflict || !strings.Contains(out, "contient") {
		t.Errorf("un parc aux homes imbriqués doit être refusé (code %d), obtenu %d :\n%s", CodeConflict, code, out)
	}

	out, code := h.run("project", "delete", "boutiquefr", "--yes")
	if code != CodeConflict || !strings.Contains(out, "blogboutiquefr") {
		t.Fatalf("une racine qui contient celle d'un voisin doit refuser le geste (code %d), obtenu %d :\n%s",
			CodeConflict, code, out)
	}
	intact(t, témoin, "billet\n")
	// Dans l'autre sens aussi : le blog vit dans l'arborescence de la boutique.
	if out, code := h.run("project", "delete", "blogboutiquefr", "--yes"); code != CodeConflict {
		t.Errorf("une racine sous celle d'un voisin doit refuser le geste, code %d :\n%s", code, out)
	}
}

func TestSuppressionRefuseUneDeclarationVoisineIllisible(t *testing.T) {
	// Dans le doute, rien n'est effacé : une déclaration dont on ne tire pas de
	// racine pourrait être celle d'un projet logé sous la nôtre.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	cassée := h.path("etc", "ogs", "projects", "casse.yaml")
	déposer(t, cassée, "metadata: [\n")

	out, code := h.run("project", "delete", "shopfr", "--yes")
	if code != CodeConflict || !strings.Contains(out, cassée) {
		t.Fatalf("une déclaration illisible doit être nommée et refuser le geste, code %d :\n%s", code, out)
	}
	h.rienNAEteDemonte(t, "shopfr", out)
}

func TestSuppressionRefuseUneRacineQuiEstUnLien(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	ailleurs := filepath.Join(t.TempDir(), "donnees")
	déposer(t, filepath.Join(ailleurs, "fichier"), "à garder\n")
	home := h.path("home", "shop.fr")
	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ailleurs, home); err != nil {
		t.Fatal(err)
	}

	out, code := h.run("project", "delete", "shopfr", "--yes")
	if code != CodeConflict || !strings.Contains(out, "lien symbolique") {
		t.Fatalf("une racine qui est un lien doit refuser le geste, code %d :\n%s", code, out)
	}
	intact(t, filepath.Join(ailleurs, "fichier"), "à garder\n")
	h.rienNAEteDemonte(t, "shopfr", out)
}

func TestProprietaireDeLaRacine(t *testing.T) {
	// Le compte existe : la racine doit être à lui. Il n'existe pas : elle ne
	// doit appartenir à aucun autre compte existant — un UID orphelin est ce que
	// laisse un compte supprimé, un UID nommé désigne le home de quelqu'un.
	comptes := map[uint32]string{0: "root", 1001: "shopfr", 1002: "shopcomprep"}
	nameOf := func(uid uint32) (string, error) { return comptes[uid], nil }
	shopfr := &system.UserInfo{Name: "shopfr", UID: 1001}
	for _, c := range []struct {
		nom     string
		owner   uint32
		account *system.UserInfo
		refus   string
	}{
		{"racine au compte du projet", 1001, shopfr, ""},
		{"racine au compte d'un voisin", 1002, shopfr, "shopcomprep"},
		{"racine à root", 0, shopfr, "root"},
		{"racine à un UID orphelin, compte du projet présent", 1500, shopfr, "sans compte"},
		{"compte absent, racine au compte d'un voisin", 1002, nil, "shopcomprep"},
		{"compte absent, racine à root", 0, nil, "root"},
		{"compte absent, racine à un UID orphelin", 1500, nil, ""},
	} {
		raison, err := ownerRefusal(c.owner, "shopfr", c.account, nameOf)
		if err != nil {
			t.Errorf("%s : %v", c.nom, err)
			continue
		}
		switch {
		case c.refus == "" && raison != "":
			t.Errorf("%s : refusé à tort (%s)", c.nom, raison)
		case c.refus != "" && !strings.Contains(raison, c.refus):
			t.Errorf("%s : refus attendu nommant %q, obtenu %q", c.nom, c.refus, raison)
		}
	}
	if _, err := ownerRefusal(1002, "shopfr", nil, func(uint32) (string, error) {
		return "", errors.New("base des comptes illisible")
	}); err == nil {
		t.Error("une base des comptes illisible ne doit pas valoir absence de compte")
	}
}

// monterFactice déclare des points de montage dans une table des montages de
// substitution : les vrais exigent des privilèges que les tests n'ont pas, et la
// garde ne lit que la table.
func monterFactice(t *testing.T, points ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n")
	for i, p := range points {
		escaped := strings.NewReplacer(`\`, `\134`, " ", `\040`).Replace(p)
		fmt.Fprintf(&b, "%d 22 0:%d /partage %s rw,relatime shared:2 - nfs srv:/medias rw\n", 90+i, 40+i, escaped)
	}
	table := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(table, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	ancienne := mountInfoPath
	mountInfoPath = table
	t.Cleanup(func() { mountInfoPath = ancienne })
}

func TestTableDesMontagesLueCommeLeNoyauLEcrit(t *testing.T) {
	// Le noyau protège blancs et barres obliques inverses par des séquences
	// octales : un point de montage au nom espacé ne serait jamais reconnu sinon.
	monterFactice(t, "/home/shop.fr/www/mé dias", `/home/shop.fr/a\b`, "/home/shop.fr", "/home/shop.fr2/x")
	got, err := mountPointsUnder("/home/shop.fr")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/home/shop.fr", `/home/shop.fr/a\b`, "/home/shop.fr/www/mé dias"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("points de montage : %q, attendu %q (un voisin au nom prolongé n'en est pas)", got, want)
	}
}

func TestSuppressionRefuseDeFranchirUnPointDeMontage(t *testing.T) {
	// Un partage de médias monté sous le home — bind mount, NFS commun à deux
	// frontaux — était vidé par os.RemoveAll pour tous ceux qui s'en servent, et
	// recopié dans l'archive.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	point := h.path("home", "shop.fr", "www", "mé dias")
	témoin := filepath.Join(point, "photo.jpg")
	déposer(t, témoin, "partagée\n")
	journaux := h.path("var", "log", "ogs-projects", "shopfr", "ancien")
	déposer(t, filepath.Join(journaux, "access.log"), "partagé\n")
	monterFactice(t, point, journaux)

	// Le refus précède la confirmation, celle de --purge comprise.
	for _, mode := range []string{"--no-dump", "--purge"} {
		out, code := h.run("project", "delete", "shopfr", mode, "--yes")
		if code != CodeConflict || !strings.Contains(out, point) || !strings.Contains(out, journaux) ||
			!strings.Contains(out, "umount") || !strings.Contains(out, "--keep-data") {
			t.Fatalf("%s : un point de montage sous la racine ou les journaux doit refuser le geste en le "+
				"nommant, avec ses issues, code %d :\n%s", mode, code, out)
		}
		intact(t, témoin, "partagée\n")
		h.rienNAEteDemonte(t, "shopfr", out)
	}

	// --keep-data n'efface rien : l'exploitant retire le site en laissant le
	// partage monté pour ceux qui s'en servent, et l'archive, qui le recopiait, ne
	// recueille ni l'arborescence ni les journaux — tous deux restent en place.
	out := h.mustRun("project", "delete", "shopfr", "--keep-data", "--yes")
	for _, motif := range []string{"arborescence non archivée", "journaux non archivés", point, journaux} {
		if !strings.Contains(out, motif) {
			t.Errorf("la sortie doit nommer %q :\n%s", motif, out)
		}
	}
	intact(t, témoin, "partagée\n")
	if n := h.fichiersArchivés(t, "shopfr", "tree") + h.fichiersArchivés(t, "shopfr", "logs"); n != 0 {
		t.Errorf("l'archive ne doit pas recopier un partage monté : %d fichier(s)", n)
	}
}

func TestSuppressionRefuseUnCompteNonMarqueAvantLePointDeNonRetour(t *testing.T) {
	// Le compte d'un projet adopté n'est jamais marqué : le refus tombait à
	// l'étape 5, une fois vhost, pool, bases et déclaration partis, et la reprise
	// conseillée (« --force ») échouait faute de projet.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	déposer(t, h.path("etc", "passwd"), "shopfr:x:1001:1001::"+h.path("home", "shop.fr")+":/bin/bash\n")

	out, code := h.run("project", "delete", "shopfr", "--yes")
	if code != CodeConflict || !strings.Contains(out, "n'a pas été créé par ogs") ||
		!strings.Contains(out, "--force") || !strings.Contains(out, "--keep-data") {
		t.Fatalf("un compte non créé par ogs doit refuser le geste à l'entrée, code %d :\n%s", code, out)
	}
	h.rienNAEteDemonte(t, "shopfr", out)

	// Le conseil est suivi d'effet : la déclaration est toujours là.
	h.mustRun("project", "delete", "shopfr", "--yes", "--force")
}

func TestSuppressionRefuseUneBaseInobservable(t *testing.T) {
	// Une base qu'on ne joint pas ne se sauvegarde pas : la suppression
	// continuait vers une archive sans ses données.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5",
		"--db", "mysql", "--tls=false", "--yes")

	out, code := h.run("project", "delete", "shopfr", "--yes")
	if code != CodeDependency || !strings.Contains(out, "inobservable") ||
		!strings.Contains(out, "--no-dump") || !strings.Contains(out, "--keep-data") {
		t.Fatalf("une base inobservable doit refuser le geste et nommer les issues, code %d :\n%s", code, out)
	}
	h.rienNAEteDemonte(t, "shopfr", out)
	if _, err := os.Stat(h.path("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf")); err != nil {
		t.Errorf("le pool ne doit pas avoir été démonté : %v", err)
	}

	// --no-dump est le choix explicite de supprimer sans sauvegarde ; la base,
	// injoignable, reste en place, et le code 6 le dit.
	out, code = h.run("project", "delete", "shopfr", "--no-dump", "--yes")
	if code != CodePartial || !strings.Contains(out, "base shopfr conservée") {
		t.Errorf("une base restée en place est un succès partiel, code %d :\n%s", code, out)
	}
}

func TestSuppressionAbandonneSurUnDumpEnEchec(t *testing.T) {
	// Le moteur répond au client d'administration, pas à l'outil de sauvegarde
	// (ici absent ; ailleurs un rôle PostgreSQL, un disque plein, une vue au
	// DEFINER disparu) : la base était supprimée sans que l'archive en garde rien.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5",
		"--db", "mysql", "--tls=false", "--yes")
	moteur := demarrerMySQLFactice(t, h.path("run", "mysqld", "mysqld.sock"), "shopfr")

	out, code := h.run("project", "delete", "shopfr", "--yes")
	if code != CodeError || !strings.Contains(out, "sauvegarde de shopfr impossible") ||
		!strings.Contains(out, "--no-dump") || !strings.Contains(out, "--keep-data") {
		t.Fatalf("un dump en échec doit abandonner la suppression, code %d :\n%s", code, out)
	}
	if moteur.reçu("DROP") {
		t.Fatalf("aucune base ne doit être supprimée sans sauvegarde : %q", moteur.requêtes())
	}
	// Les services arrêtés pour la sauvegarde sont relancés : l'abandon laissait
	// les workers à l'arrêt jusqu'au prochain redémarrage de la machine.
	if !strings.Contains(out, "arrêt des services") || !strings.Contains(out, "redémarrage des services") {
		t.Errorf("les services arrêtés doivent être relancés :\n%s", out)
	}
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "shopfr.yaml")); err != nil {
		t.Errorf("la déclaration doit rester à l'inventaire actif : %v", err)
	}
	if archives, _ := filepath.Glob(h.path("var", "lib", "ogs", "archives", "shopfr-*")); len(archives) > 0 {
		t.Errorf("l'archive inachevée ne doit pas subsister : %v", archives)
	}

	// L'outil présent, la sauvegarde entre dans l'archive, et la base part.
	déposer(t, h.path("usr", "bin", "mysqldump"), "#!/bin/sh\n")
	out = h.mustRunPartial("project", "delete", "shopfr", "--yes")
	if !strings.Contains(out, "sauvegarde de shopfr incluse") || !moteur.reçu("DROP DATABASE IF EXISTS `shopfr`") {
		t.Errorf("une base sauvegardée doit être supprimée :\n%s\nrequêtes : %q", out, moteur.requêtes())
	}
}

func TestSuppressionDUnProjetAdopteExigeForcePourSesBases(t *testing.T) {
	// L'import rattache des bases sur des indices qu'il ne vérifie pas toujours :
	// celle d'un voisin resté hors d'ogs, un compte dont personne ne connaît plus
	// le mot de passe. Elles partaient sans --force, là où le compte système
	// l'exigeait.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5",
		"--db", "mysql", "--tls=false", "--yes")
	h.modifierDeclaration(t, "shopfr", adopter)
	moteur := demarrerMySQLFactice(t, h.path("run", "mysqld", "mysqld.sock"), "shopfr")

	out, code := h.run("project", "delete", "shopfr", "--yes")
	if code != CodeConflict || !strings.Contains(out, "mysql shopfr (compte shopfr@localhost)") ||
		!strings.Contains(out, "--force") {
		t.Fatalf("les bases d'un projet adopté doivent exiger --force, nommées, code %d :\n%s", code, out)
	}
	if moteur.reçu("DROP") {
		t.Fatalf("aucune base ne doit être supprimée sans --force : %q", moteur.requêtes())
	}
	h.rienNAEteDemonte(t, "shopfr", out)

	h.mustRunPartial("project", "delete", "shopfr", "--yes", "--force", "--no-dump")
	if !moteur.reçu("DROP DATABASE IF EXISTS `shopfr`") {
		t.Errorf("avec --force, les bases partent : %q", moteur.requêtes())
	}
}

func TestSuppressionConserveLesFichiersQuOgsNAPasEcrits(t *testing.T) {
	// Le pool d'un site adopté porte le nom de l'identifiant alors qu'il sert
	// aussi un vhost voisin resté hors d'ogs : le retirer, même sous --keep-data,
	// mettait le voisin en 502. Et un fichier écrit pour un autre projet n'est
	// pas davantage à celui-ci.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5",
		"--service", "symfony-messenger:async:1", "--tls=false", "--yes")
	pool := h.path("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf")
	const poolÉtranger = "[shopfr]\nuser = shopfr\nlisten = /run/php/shopfr.sock\n"
	déposer(t, pool, poolÉtranger)
	vhost := h.path("etc", "apache2", "sites-available", "shopfr.conf")
	vhostVoisin := string(ogsfs.Render(ogsfs.Header{Project: "voisin", Resource: "web/vhost/voisin"},
		[]byte("<VirtualHost *:80>\n</VirtualHost>\n"), "#"))
	déposer(t, vhost, vhostVoisin)
	activé := h.path("etc", "apache2", "sites-enabled", "shopfr.conf")
	if _, err := os.Lstat(activé); err != nil {
		t.Fatalf("le lien d'activation est attendu : %v", err)
	}
	unité := h.path("etc", "systemd", "system", "ogs-shopfr.target")
	démarrage := h.path("etc", "systemd", "system", "multi-user.target.wants", "ogs-shopfr.target")
	for _, f := range []string{unité, démarrage} {
		if _, err := os.Lstat(f); err != nil {
			t.Fatalf("la cible du projet et son activation sont attendues : %v", err)
		}
	}

	out, code := h.run("project", "delete", "shopfr", "--keep-data", "--yes")
	if code != CodePartial {
		t.Errorf("des fichiers laissés en place sont un succès partiel, code %d :\n%s", code, out)
	}
	intact(t, pool, poolÉtranger)
	intact(t, vhost, vhostVoisin)
	// Le lien suit le sort de sa cible.
	if _, err := os.Lstat(activé); err != nil {
		t.Errorf("le lien d'activation d'un vhost conservé doit rester : %v", err)
	}
	for _, f := range []string{pool, vhost, "projet voisin"} {
		if !strings.Contains(out, f) {
			t.Errorf("la sortie doit nommer %q :\n%s", f, out)
		}
	}
	// Ce qu'ogs a écrit pour le projet part, lui, et son lien d'activation avec.
	for _, f := range []string{unité, démarrage} {
		if _, err := os.Lstat(f); !os.IsNotExist(err) {
			t.Errorf("%s doit disparaître avec le projet : %v", f, err)
		}
	}
}

func TestSuppressionRefuseDEffacerCeQueDesigneUnFichierConserve(t *testing.T) {
	// Le vhost d'origine d'un projet adopté, qu'ogs n'a jamais réécrit, reste en
	// place avec son lien d'activation. Effacer la racine où il journalise rendait
	// la configuration d'Apache invalide : plus aucun rechargement ne passait, et
	// le redémarrage suivant faisait tomber tous les sites. Un pool conservé qui
	// tourne sous le compte supprimé arrête FPM de la même façon. --force n'y
	// change rien : ces fichiers ne sont pas à ogs.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5", "--tls=false", "--yes")
	racine := h.path("home", "shop.fr")
	témoin := filepath.Join(racine, "www", "index.html")
	déposer(t, témoin, "<p>contenu</p>\n")
	vhost := h.path("etc", "apache2", "sites-available", "shopfr.conf")
	vhostÉtranger := "<VirtualHost *:80>\n    ServerName shop.fr\n    ErrorLog " +
		filepath.Join(racine, "logs", "error.log") + "\n</VirtualHost>\n"
	déposer(t, vhost, vhostÉtranger)
	pool := h.path("etc", "php", "8.5", "fpm", "pool.d", "shopfr.conf")
	poolÉtranger := "[shopfr]\nuser = shopfr\nchdir = " + racine + "\nlisten = /run/php/shopfr.sock\n"
	déposer(t, pool, poolÉtranger)
	activé := h.path("etc", "apache2", "sites-enabled", "shopfr.conf")

	out, code := h.run("project", "delete", "shopfr", "--yes", "--force")
	if code != CodeConflict || !strings.Contains(out, vhost) || !strings.Contains(out, pool) ||
		!strings.Contains(out, "--keep-data") {
		t.Fatalf("un fichier conservé qui peut désigner le compte ou la racine doit refuser le geste "+
			"en le nommant, code %d :\n%s", code, out)
	}
	// Le remède qui ne coupe pas le site est offert : les retirer à la main
	// l'arrête sur-le-champ.
	if !strings.Contains(out, "ogs apply shopfr --force") {
		t.Errorf("le refus doit offrir de confier les fichiers à ogs :\n%s", out)
	}
	intact(t, témoin, "<p>contenu</p>\n")
	intact(t, vhost, vhostÉtranger)
	intact(t, pool, poolÉtranger)
	h.rienNAEteDemonte(t, "shopfr", out)

	// Retirés par l'exploitant, ils ne retiennent plus rien ; le lien d'activation,
	// qui ne désigne plus rien, part avec le projet.
	for _, f := range []string{vhost, pool} {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	h.mustRun("project", "delete", "shopfr", "--yes")
	for _, f := range []string{racine, activé} {
		if _, err := os.Lstat(f); !os.IsNotExist(err) {
			t.Errorf("%s doit disparaître avec le projet : %v", f, err)
		}
	}
}

func TestSuppressionNeRetientPasLeFichierDUnAutreProjet(t *testing.T) {
	// Un fichier qu'ogs a écrit pour un autre projet n'est pas à celui-ci : il
	// reste, nommé, mais ne désigne ni son compte ni sa racine — le parc interdit
	// deux racines qui se recouvrent —, et n'arrête donc pas la suppression.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	vhost := h.path("etc", "apache2", "sites-available", "shopfr.conf")
	vhostVoisin := string(ogsfs.Render(ogsfs.Header{Project: "voisin", Resource: "web/vhost/voisin"},
		[]byte("<VirtualHost *:80>\n</VirtualHost>\n"), "#"))
	déposer(t, vhost, vhostVoisin)

	out, code := h.run("project", "delete", "shopfr", "--yes")
	if code != CodePartial || !strings.Contains(out, "projet voisin") {
		t.Fatalf("le fichier d'un autre projet doit rester, nommé, code %d :\n%s", code, out)
	}
	intact(t, vhost, vhostVoisin)
	if _, err := os.Stat(h.path("home", "shop.fr")); !os.IsNotExist(err) {
		t.Errorf("l'arborescence du projet devait disparaître : %v", err)
	}
}

func TestSuppressionNommeToutCeQueForceEfface(t *testing.T) {
	// Le compte d'un projet adopté n'est jamais marqué : son refus masquait celui
	// des bases, et le --force qu'il réclamait les emportait sans qu'elles aient
	// jamais été nommées — la base d'un voisin rattachée à tort par l'import en
	// tête. Un seul refus nomme tout ce que --force lève, et --force l'annonce
	// avant que rien ne parte.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5",
		"--db", "mysql", "--tls=false", "--yes")
	h.modifierDeclaration(t, "shopfr", adopter)
	déposer(t, h.path("etc", "passwd"), "shopfr:x:1001:1001::"+h.path("home", "shop.fr")+":/bin/bash\n")
	moteur := demarrerMySQLFactice(t, h.path("run", "mysqld", "mysqld.sock"), "shopfr")

	out, code := h.run("project", "delete", "shopfr", "--yes")
	if code != CodeConflict {
		t.Fatalf("un compte non marqué et des bases adoptées doivent refuser le geste, code %d :\n%s", code, out)
	}
	for _, motif := range []string{"compte shopfr", "n'a pas été créé par ogs",
		"mysql shopfr (compte shopfr@localhost)", "--force", "--keep-data"} {
		if !strings.Contains(out, motif) {
			t.Errorf("le refus doit nommer %q :\n%s", motif, out)
		}
	}
	if moteur.reçu("DROP") {
		t.Fatalf("aucune base ne doit être supprimée sans --force : %q", moteur.requêtes())
	}
	h.rienNAEteDemonte(t, "shopfr", out)

	déposer(t, h.path("usr", "bin", "mysqldump"), "#!/bin/sh\n")
	out, _ = h.run("project", "delete", "shopfr", "--yes", "--force")
	annonce := strings.Index(out, "= --force")
	if annonce < 0 || !strings.Contains(out[annonce:], "mysql shopfr (compte shopfr@localhost)") ||
		!strings.Contains(out[annonce:], "compte shopfr") {
		t.Fatalf("--force doit annoncer le compte et les bases qu'il fait supprimer :\n%s", out)
	}
	if arrêt := strings.Index(out, "arrêt des services"); arrêt >= 0 && arrêt < annonce {
		t.Errorf("l'annonce doit précéder le premier geste :\n%s", out)
	}
	if !moteur.reçu("DROP DATABASE IF EXISTS `shopfr`") {
		t.Errorf("avec --force, les bases partent : %q", moteur.requêtes())
	}
}

func TestSuppressionRelanceLesServicesSurUneArchiveEnEchec(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	// Un fichier à la place du répertoire des archives : l'archive ne peut pas
	// être créée, même par root.
	archives := h.path("var", "lib", "ogs", "archives")
	if err := os.RemoveAll(archives); err != nil {
		t.Fatal(err)
	}
	déposer(t, archives, "")

	out, code := h.run("project", "delete", "shopfr", "--yes")
	if code != CodeError {
		t.Fatalf("une archive impossible doit arrêter la suppression, code %d :\n%s", code, out)
	}
	if !strings.Contains(out, "arrêt des services") || !strings.Contains(out, "redémarrage des services") {
		t.Errorf("les services arrêtés doivent être relancés :\n%s", out)
	}
	if _, err := os.Stat(h.path("etc", "apache2", "sites-available", "shopfr.conf")); err != nil {
		t.Errorf("rien ne doit avoir été démonté : %v", err)
	}
}

func TestSuppressionDemandeLaTailleAvantDArreterLesServices(t *testing.T) {
	// Refuser l'archive trop grosse laissait les workers arrêtés : la question
	// est posée avant que rien ne s'arrête.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	déposer(t, h.path("home", "shop.fr", "www", "index.html"), "<p>contenu</p>\n")
	seuil := ArchiveSizeThreshold
	ArchiveSizeThreshold = 0
	t.Cleanup(func() { ArchiveSizeThreshold = seuil })
	réponses(t, "o\nn\n")

	out, code := h.run("project", "delete", "shopfr")
	if code != CodeCancelled || !strings.Contains(out, "l'archive contiendra environ") {
		t.Fatalf("le refus de la taille doit annuler, code %d :\n%s", code, out)
	}
	h.rienNAEteDemonte(t, "shopfr", out)
}

func TestSuppressionEnumereTousSesRefusEnsemble(t *testing.T) {
	// Chaque refus appelle son geste, et certains coûtent. Le premier refus seul
	// faisait retirer à l'exploitant le vhost d'origine d'un site adopté — donc
	// couper le site — avant qu'il n'apprenne, au passage suivant, que le compte
	// et les bases exigeaient --force, puis, au troisième, qu'une base était
	// injoignable ; et le voisin logé sous la racine masquait le partage monté
	// dessous. Tout est nommé d'un coup.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRunPartial("site", "create", "shop.fr", "--kind", "php-symfony", "--php", "8.5",
		"--db", "mysql", "--tls=false", "--yes")
	racine := h.path("home", "shop.fr")
	h.mustRun("project", "create", "blog.shop.fr", "--kind", "static")
	h.modifierDeclaration(t, "blogshopfr", func(s string) string {
		return strings.ReplaceAll(s, h.path("home", "blog.shop.fr"), filepath.Join(racine, "blog"))
	})
	h.modifierDeclaration(t, "shopfr", adopter)
	déposer(t, h.path("etc", "passwd"), "shopfr:x:1001:1001::"+racine+":/bin/bash\n")
	vhost := h.path("etc", "apache2", "sites-available", "shopfr.conf")
	const vhostÉtranger = "<VirtualHost *:80>\n    ServerName shop.fr\n</VirtualHost>\n"
	déposer(t, vhost, vhostÉtranger)
	point := filepath.Join(racine, "www", "medias")
	monterFactice(t, point)

	out, code := h.run("project", "delete", "shopfr", "--yes")
	if code != CodeConflict {
		t.Fatalf("les refus doivent arrêter le geste, code %d attendu, obtenu %d :\n%s", CodeConflict, code, out)
	}
	for _, motif := range []string{
		"blogshopfr", point, "umount", // la racine : le voisin, et le partage monté dessous
		vhost, "ogs apply shopfr --force", // le fichier conservé, et le remède qui ne coupe rien
		"n'a pas été créé par ogs", "mysql shopfr (compte shopfr@localhost)", "--force", // ce que --force lève
		"inobservable", "--no-dump", // la base qu'on ne pourra pas sauvegarder
		"--keep-data",
	} {
		if !strings.Contains(out, motif) {
			t.Errorf("le refus doit nommer %q :\n%s", motif, out)
		}
	}
	intact(t, vhost, vhostÉtranger)
	h.rienNAEteDemonte(t, "shopfr", out)

	// --force lève ce qui lui revient, et rien d'autre : le reste est toujours
	// nommé ensemble.
	out, code = h.run("project", "delete", "shopfr", "--yes", "--force")
	if code != CodeConflict {
		t.Fatalf("sous --force, les autres refus tiennent, code %d :\n%s", code, out)
	}
	for _, motif := range []string{"blogshopfr", point, vhost, "inobservable"} {
		if !strings.Contains(out, motif) {
			t.Errorf("sous --force, le refus doit nommer %q :\n%s", motif, out)
		}
	}
	if strings.Contains(out, "n'a pas été créé par ogs") {
		t.Errorf("--force lève le refus du compte et des bases adoptées :\n%s", out)
	}
	h.rienNAEteDemonte(t, "shopfr", out)
}

func TestSuppressionReprouveSesRempartsAuPointDeNonRetour(t *testing.T) {
	// L'archive peut prendre des minutes, pendant lesquelles un partage se monte
	// sous le home : les remparts sont éprouvés de nouveau avant le premier
	// retrait, tant qu'y renoncer ne coûte que l'archive.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	point := h.path("home", "shop.fr", "www", "medias")
	témoin := filepath.Join(point, "photo.jpg")
	déposer(t, témoin, "partagée\n")
	monterFactice(t)

	out, code := h.runEpie(context.Background(), "archive créée", func() { monterFactice(t, point) },
		"project", "delete", "shopfr", "--yes")
	if code != CodeConflict || !strings.Contains(out, point) || !strings.Contains(out, "umount") {
		t.Fatalf("un point de montage apparu pendant l'archive doit arrêter la suppression, code %d :\n%s",
			code, out)
	}
	if !strings.Contains(out, "redémarrage des services") {
		t.Errorf("les services arrêtés doivent être relancés :\n%s", out)
	}
	intact(t, témoin, "partagée\n")
	h.rienNAEteRetire(t, "shopfr")
}

func TestSuppressionAbandonneSurUneInterruptionPendantLArchive(t *testing.T) {
	// main n'arrête pas le processus sur Ctrl+C : il annule le contexte, que rien
	// ne consultait entre l'arrêt des services et le premier retrait. L'archive
	// allait à son terme, puis la suppression continuait, chaque commande échouant
	// sur « context canceled » : home effacé sous un compte et des workers FPM
	// toujours vivants, fichiers de parc jamais recalculés.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	témoin := h.path("home", "shop.fr", "www", "index.html")
	déposer(t, témoin, "<p>contenu</p>\n")

	ctx, interrompre := context.WithCancel(context.Background())
	defer interrompre()
	out, code := h.runEpie(ctx, "archive créée", interrompre, "project", "delete", "shopfr", "--yes")
	if code != CodeCancelled || !strings.Contains(out, "interruption") {
		t.Fatalf("une interruption avant le premier retrait doit abandonner la suppression (code %d), "+
			"obtenu %d :\n%s", CodeCancelled, code, out)
	}
	if !strings.Contains(out, "redémarrage des services") {
		t.Errorf("les services arrêtés doivent être relancés :\n%s", out)
	}
	intact(t, témoin, "<p>contenu</p>\n")
	h.rienNAEteRetire(t, "shopfr")
}

func TestSuppressionVaASonTermeApresLePointDeNonRetour(t *testing.T) {
	// Passé le premier retrait, renoncer ne rendrait rien et laisserait le pire
	// des états — un home effacé sous un compte vivant, une prison fail2ban
	// désignant des journaux disparus, qui empêche le démon de redémarrer : une
	// interruption n'y coupe plus rien, elle est signalée.
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("site", "create", "shop.fr", "--kind", "static", "--tls=false", "--yes")
	// Un vhost écrit pour un autre projet est conservé, et annoncé juste après le
	// point de non-retour : c'est là que tombe l'interruption.
	vhost := h.path("etc", "apache2", "sites-available", "shopfr.conf")
	déposer(t, vhost, string(ogsfs.Render(ogsfs.Header{Project: "voisin", Resource: "web/vhost/voisin"},
		[]byte("<VirtualHost *:80>\n</VirtualHost>\n"), "#")))
	tmpfiles := h.path("etc", "tmpfiles.d", "ogs.conf")
	if data, err := os.ReadFile(tmpfiles); err != nil || !strings.Contains(string(data), "shopfr") {
		t.Fatalf("les fichiers de parc doivent porter le projet avant sa suppression : %q (%v)", data, err)
	}

	ctx, interrompre := context.WithCancel(context.Background())
	defer interrompre()
	out, code := h.runEpie(ctx, "conservé : ogs l'a écrit pour le projet voisin", interrompre,
		"project", "delete", "shopfr", "--yes")
	if code != CodePartial || !strings.Contains(out, "interruption reçue après le point de non-retour") {
		t.Fatalf("une interruption après le premier retrait doit être signalée sans rien couper, code %d :\n%s",
			code, out)
	}
	if strings.Contains(out, "context canceled") {
		t.Errorf("aucune étape ne doit échouer sur l'interruption :\n%s", out)
	}
	if _, err := os.Stat(h.path("home", "shop.fr")); !os.IsNotExist(err) {
		t.Errorf("l'arborescence doit disparaître : %v", err)
	}
	if _, err := os.Stat(h.path("etc", "ogs", "projects", "archived", "shopfr.yaml")); err != nil {
		t.Errorf("la déclaration doit partir aux archives : %v", err)
	}
	if data, _ := os.ReadFile(tmpfiles); strings.Contains(string(data), "shopfr") {
		t.Errorf("les fichiers de parc doivent être recalculés sans le projet :\n%s", data)
	}
}

// réponses fournit les saisies de l'exploitant sur l'entrée standard.
func réponses(t *testing.T, saisies string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(saisies); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	ancienne := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = ancienne; _ = r.Close() })
}

// mysqlFactice est un moteur MySQL réduit au protocole texte, sur le socket que
// cherche ogs sous le préfixe : c'est le vrai client d'ogs qui s'y adresse, et
// chaque requête qui n'est pas une lecture est consignée — de quoi prouver
// qu'une base n'a pas été supprimée.
type mysqlFactice struct {
	mu      sync.Mutex
	bases   []string
	journal []string
}

// demarrerMySQLFactice écoute sur socket et sert les bases données. Le chemin
// doit tenir dans les 108 octets d'une adresse de socket : noms de tests courts.
func demarrerMySQLFactice(t *testing.T, socket string, bases ...string) *mysqlFactice {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	m := &mysqlFactice{bases: append([]string{"information_schema", "mysql"}, bases...)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go m.servir(c)
		}
	}()
	return m
}

func (m *mysqlFactice) requêtes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.journal...)
}

func (m *mysqlFactice) reçu(motif string) bool {
	for _, q := range m.requêtes() {
		if strings.Contains(q, motif) {
			return true
		}
	}
	return false
}

func paquetMySQL(w io.Writer, seq byte, p []byte) {
	n := len(p)
	_, _ = w.Write(append([]byte{byte(n), byte(n >> 8), byte(n >> 16), seq}, p...))
}

func lirePaquetMySQL(r io.Reader) ([]byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	buf := make([]byte, int(h[0])|int(h[1])<<8|int(h[2])<<16)
	_, err := io.ReadFull(r, buf)
	return buf, err
}

func chaîneMySQL(s string) []byte { return append([]byte{byte(len(s))}, s...) }

func (m *mysqlFactice) servir(c net.Conn) {
	defer c.Close()
	// Poignée de main v10 : protocole 4.1, authentification par greffon.
	caps := uint32(0x1 | 0x4 | 0x200 | 0x2000 | 0x8000 | 0x20000 | 0x80000)
	h := []byte{10}
	h = append(h, "8.0.36-factice\x00"...)
	h = append(h, 1, 0, 0, 0)
	h = append(h, "abcdefgh"...)
	h = append(h, 0, byte(caps), byte(caps>>8), 45, 2, 0, byte(caps>>16), byte(caps>>24), 21)
	h = append(h, make([]byte, 10)...)
	h = append(h, "ijklmnopqrst\x00"...)
	h = append(h, "mysql_native_password\x00"...)
	paquetMySQL(c, 0, h)
	if _, err := lirePaquetMySQL(c); err != nil {
		return
	}
	ok := []byte{0, 0, 0, 2, 0, 0, 0}
	paquetMySQL(c, 2, ok)
	for {
		p, err := lirePaquetMySQL(c)
		if err != nil || len(p) == 0 {
			return
		}
		switch p[0] {
		case 0x01: // COM_QUIT
			return
		case 0x0e: // COM_PING
			paquetMySQL(c, 1, ok)
		case 0x03: // COM_QUERY
			m.répondre(c, string(p[1:]))
		default:
			// Les requêtes préparées sont refusées : ogs tolère un échec de lecture
			// des privilèges, et c'est tout ce qu'elles servent ici.
			paquetMySQL(c, 1, append([]byte{0xff, 0x2a, 0x04, '#', 'H', 'Y', '0', '0', '0'}, "non pris en charge"...))
		}
	}
}

func (m *mysqlFactice) répondre(c net.Conn, q string) {
	var lignes []string
	m.mu.Lock()
	switch {
	case strings.Contains(q, "VERSION()"):
		lignes = []string{"8.0.36-factice"}
	case strings.Contains(q, "information_schema.SCHEMATA"):
		lignes = append(lignes, m.bases...)
	case strings.Contains(q, "mysql.user"):
		lignes = []string{"root@localhost"}
		for _, b := range m.bases[2:] {
			lignes = append(lignes, b+"@localhost")
		}
	case strings.HasPrefix(strings.ToUpper(strings.TrimSpace(q)), "SELECT"):
	default:
		m.journal = append(m.journal, q)
		m.mu.Unlock()
		paquetMySQL(c, 1, []byte{0, 0, 0, 2, 0, 0, 0})
		return
	}
	m.mu.Unlock()
	seq := byte(1)
	envoyer := func(p []byte) { paquetMySQL(c, seq, p); seq++ }
	eof := []byte{0xfe, 0, 0, 2, 0}
	envoyer([]byte{1})
	var col []byte
	for _, s := range []string{"def", "", "", "", "c", "c"} {
		col = append(col, chaîneMySQL(s)...)
	}
	col = append(col, 0x0c, 45, 0, 0, 1, 0, 0, 0xfd, 0, 0, 0, 0, 0)
	envoyer(col)
	envoyer(eof)
	for _, l := range lignes {
		envoyer(chaîneMySQL(l))
	}
	envoyer(eof)
}
