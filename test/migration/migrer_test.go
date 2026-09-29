// Package migration éprouve docs/migrer.sh, le script qui migre un ancien
// serveur vers un serveur géré par ogs.
//
// L'ancien serveur est un répertoire local, joint par un faux ssh qui lit
// l'entrée standard comme le vrai (testdata/bin/ssh) ; moteurs de base, crontab,
// supervisorctl, dig, curl et runuser sont simulés. rsync, install et ogs sont
// les vrais — ogs construit depuis le dépôt et lancé en mode préfixé —, derrière
// des enveloppes qui notent chaque appel dans un journal que les tests relisent.
//
// Le script exige root pour --executer : le banc en lance une copie dont seule
// cette garde est retirée. Le banc ne tourne pas en root, donc runuser ne change
// pas d'identité : ce qui est vérifié, c'est l'identité que le script demande
// pour chaque écriture.
package migration

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	ipSource  = "203.0.113.10"
	ipCible   = "198.51.100.20"
	dnsSource = "FAUX_DIG_A=" + ipSource
	dnsCible  = "FAUX_DIG_A=" + ipCible
	// La garde retirée de la copie du script : le banc ne tourne pas en root.
	gardeRoot = `    ((EUID == 0)) || fatal "--executer exige root"` + "\n"
)

var (
	ogsBinaire, rsyncReel, installReel string
	manque                             string // outil absent : les tests sont sautés
)

func TestMain(m *testing.M) {
	os.Exit(func() int {
		for _, outil := range []string{"bash", "python3", "rsync", "flock", "gzip", "zcat", "install", "go"} {
			if _, err := exec.LookPath(outil); err != nil {
				manque = outil
				return m.Run()
			}
		}
		if exec.Command("python3", "-c", "import yaml").Run() != nil {
			manque = "python3-yaml"
			return m.Run()
		}
		rsyncReel, _ = exec.LookPath("rsync")
		installReel, _ = exec.LookPath("install")
		dir, err := os.MkdirTemp("", "banc-migration-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		ogsBinaire = filepath.Join(dir, "ogs")
		construire := exec.Command("go", "build", "-o", ogsBinaire, "../../cmd/ogs")
		construire.Env = append(os.Environ(), "CGO_ENABLED=0")
		if sortie, err := construire.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "construction d'ogs : %v\n%s", err, sortie)
			return 1
		}
		return m.Run()
	}())
}

type (
	M = map[string]any
	L = []any
)

// banc est une migration d'essai : une cible (le préfixe d'ogs), une source, un
// répertoire de travail et un manifeste.
type banc struct {
	t       *testing.T
	dir     string
	prefixe string // la cible, telle qu'ogs la voit
	source  string // l'ancien serveur : son /root, son spool cron, ses /etc/cron.d
	racine  string // source.racine du manifeste
	travail string
	home    string // le home de shopfr sur la cible
	script  string
	manif   string
}

func nouveauBanc(t *testing.T) *banc {
	t.Helper()
	if manque != "" {
		t.Skipf("%s absent : banc de migration sauté", manque)
	}
	t.Parallel()
	dir := t.TempDir()
	if reel, err := filepath.EvalSymlinks(dir); err == nil {
		dir = reel
	}
	b := &banc{
		t:       t,
		dir:     dir,
		prefixe: filepath.Join(dir, "cible"),
		source:  filepath.Join(dir, "ancien"),
		travail: filepath.Join(dir, "travail"),
		script:  filepath.Join(dir, "migrer.sh"),
		manif:   filepath.Join(dir, "migration.yaml"),
	}
	b.racine = filepath.Join(b.source, "var/www/shop")
	b.home = filepath.Join(b.prefixe, "home/shop.fr")

	texte, err := os.ReadFile("../../docs/migrer.sh")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(texte), gardeRoot); n != 1 {
		t.Fatalf("garde root du script introuvable (%d occurrence(s)) : le banc est à mettre à jour", n)
	}
	b.ecrire(b.script, strings.Replace(string(texte), gardeRoot, "", 1))

	if sortie, err := exec.Command(ogsBinaire, "--prefix", b.prefixe, "init").CombinedOutput(); err != nil {
		t.Fatalf("ogs init : %v\n%s", err, sortie)
	}
	for _, d := range []string{"tmp", "ancien/root"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	b.ecrire(filepath.Join(b.racine, "current/index.php"), "<?php echo 'Ma boutique';\n")
	b.ecrire(filepath.Join(b.racine, "current/public/robots.txt"), "User-agent: *\n")
	b.ecrire(filepath.Join(b.racine, "shared/uploads/photo.jpg"), "image\n")
	b.ecrire(filepath.Join(b.racine, "shared/media/film.mp4"), "film\n")
	b.ecrire(filepath.Join(b.racine, "shared/.env.local"), "APP_ENV=prod\nDATABASE_URL=mysql://ancien@localhost/shop_prod\n")
	b.ecrire(filepath.Join(b.source, "spool/shop"), "*/5 * * * * php bin/console app:sync\n")
	b.ecrire(filepath.Join(b.source, "etc/cron.d/shop-a"), "0 * * * * root true\n")
	b.ecrire(filepath.Join(b.source, "etc/cron.d/shop-b"), "30 * * * * root true\n")
	b.ecrire(filepath.Join(dir, "secrets.json"),
		`{"items":[{"name":"DATABASE_URL","value":"mysql://shopfr:S3cret@localhost/shopfr"}]}`+"\n")
	return b
}

func (b *banc) ecrire(chemin, contenu string) {
	b.t.Helper()
	if err := os.MkdirAll(filepath.Dir(chemin), 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(chemin, []byte(contenu), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

func (b *banc) lire(chemin string) string {
	b.t.Helper()
	data, err := os.ReadFile(chemin)
	if err != nil {
		b.t.Fatalf("%s : %v", chemin, err)
	}
	return string(data)
}

func existe(chemin string) bool {
	_, err := os.Lstat(chemin)
	return err == nil
}

// projetShop est shopfr : deux bases, trois données hors du code, deux fichiers
// cron et deux workers — des listes de plus d'un élément, que chaque boucle du
// script doit traiter en entier.
func (b *banc) projetShop() M {
	r := b.racine
	return M{
		"id": "shopfr", "vague": 1,
		"source": M{
			"utilisateur": "shop", "racine": r, "code": "current",
			"donnees_hors_code": L{
				M{"de": "shared/uploads", "vers": "www/public/uploads"},
				M{"de": "shared/media", "vers": "www/public/media"},
				M{"de": "shared/.env.local", "vers": "www/.env.local"},
			},
			"exclure": L{"var/cache/"},
			"configuration": L{M{
				"fichier": "www/.env.local", "format": "dotenv",
				"cles": M{"DATABASE_URL": "@ogs:DATABASE_URL"},
			}},
			"maintenance": M{"activer": "touch " + r + "/maintenance.flag", "desactiver": "rm -f " + r + "/maintenance.flag"},
		},
		"maintenance_cible": M{"activer": "touch maintenance-cible.flag", "desactiver": "rm -f maintenance-cible.flag"},
		"apres_synchro":     L{},
		"bases": L{
			M{"moteur": "mysql", "source": "shop_prod", "cible": "shopfr"},
			M{"moteur": "mysql", "source": "shop_stats", "cible": "shopfrstats"},
		},
		"arret_a_la_bascule": M{
			"crontab_utilisateur": true,
			"fichiers_cron":       L{b.source + "/etc/cron.d/shop-a", b.source + "/etc/cron.d/shop-b"},
			"workers": L{
				M{"arreter": "supervisorctl stop w1", "relancer": "supervisorctl start w1"},
				M{"arreter": "supervisorctl stop w2", "relancer": "supervisorctl start w2"},
			},
		},
		"verifications": L{},
		"ogs": M{
			"apiVersion": "ogs/v1", "kind": "Project",
			"metadata": M{"name": "shopfr", "host": "shop.fr"},
			"spec": M{
				"app": M{"kind": "php-plain"},
				"php": M{"pools": L{M{"name": "web", "version": "8.2"}}},
				"databases": L{
					M{"engine": "mariadb", "name": "shopfr", "user": "shopfr"},
					M{"engine": "mariadb", "name": "shopfrstats", "user": "shopfrstats"},
				},
			},
		},
	}
}

// projetShopPrep est la préproduction de shopfr, telle que la déclare
// « ogs site clone shopfr --to prep » : même metadata.host, env prep, et ni
// domaines ni home écrits — ogs les construit sur le domaine de préproduction.
func (b *banc) projetShopPrep() M {
	p := b.projetShop()
	p["id"], p["vague"] = "shopfrprep", 2
	p["bases"] = L{M{"moteur": "mysql", "source": "shop_prep", "cible": "shopfrprep"}}
	o := p["ogs"].(M)
	o["metadata"] = M{"name": "shopfrprep", "host": "shop.fr", "env": "prep"}
	o["spec"].(M)["databases"] = L{M{"engine": "mariadb", "name": "shopfrprep", "user": "shopfrprep"}}
	return p
}

func (b *banc) manifeste(projets ...M) M {
	l := L{}
	for _, p := range projets {
		l = append(l, p)
	}
	return M{
		"version":   1,
		"source":    M{"ssh": "root@ancien.test", "port_ssh": 22, "ip_publique": ipSource},
		"cible":     M{"ip_publique": ipCible},
		"migration": M{"travail": b.travail, "conservation_source_jours": 15},
		"projets":   l,
	}
}

// ecrireManifeste l'écrit en JSON : c'est du YAML pour yaml.safe_load.
func (b *banc) ecrireManifeste(m M) {
	b.t.Helper()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		b.t.Fatal(err)
	}
	b.ecrire(b.manif, string(data)+"\n")
}

// voisin dépose sur la cible un projet que la migration n'a pas créé ; `extra`
// complète sa déclaration (metadata puis spec, en YAML en ligne).
func (b *banc) voisin(id, host, base string, extra ...string) {
	meta, spec := "", ""
	if len(extra) > 0 {
		meta = extra[0]
	}
	if len(extra) > 1 {
		spec = extra[1]
	}
	b.ecrire(filepath.Join(b.prefixe, "etc/ogs/projects", id+".yaml"), fmt.Sprintf(`apiVersion: ogs/v1
kind: Project
metadata: {name: %s, host: %s%s}
spec:
  app: {kind: php-plain}
  php: {pools: [{name: web, version: "8.2"}]}
  databases: [{engine: mariadb, name: %s, user: %s}]
%s`, id, host, meta, base, id, spec))
}

func (b *banc) env(extra []string) []string {
	bin, _ := filepath.Abs("testdata/bin")
	ancien, _ := filepath.Abs("testdata/ancien")
	return append(append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"OGS="+filepath.Join(bin, "ogs")+" --prefix "+b.prefixe,
		"TMPDIR="+filepath.Join(b.dir, "tmp"),
		"FAUX_JOURNAL="+filepath.Join(b.dir, "journal"),
		"FAUX_SOURCE="+b.source,
		"FAUX_SOURCE_BIN="+ancien,
		"FAUX_RSYNC="+rsyncReel,
		"FAUX_INSTALL="+installReel,
		"FAUX_OGS="+ogsBinaire,
		"FAUX_BASES_CIBLE="+filepath.Join(b.dir, "bases-cible"),
		"FAUX_UTILISATEURS_BASE_CIBLE="+filepath.Join(b.dir, "utilisateurs-base-cible"),
		"FAUX_COMPTES_CIBLE="+filepath.Join(b.dir, "comptes-cible"),
		"FAUX_OGS_SECRETS="+filepath.Join(b.dir, "secrets.json"),
	), extra...)
}

// lancer exécute le script ; l'entrée standard est vide, comme sous --oui.
func (b *banc) lancer(extra []string, args ...string) (string, int) {
	b.t.Helper()
	return b.lancerAvec("", extra, args...)
}

// lancerAvec exécute le script, `entree` pour entrée standard : les réponses de
// l'exploitant.
func (b *banc) lancerAvec(entree string, extra []string, args ...string) (string, int) {
	b.t.Helper()
	cmd := exec.Command("bash", append([]string{b.script, "-m", b.manif}, args...)...)
	cmd.Env = b.env(extra)
	cmd.Dir = b.dir
	cmd.Stdin = strings.NewReader(entree)
	sortie, err := cmd.CombinedOutput()
	var fin *exec.ExitError
	switch {
	case err == nil:
		return string(sortie), 0
	case errors.As(err, &fin):
		return string(sortie), fin.ExitCode()
	default:
		b.t.Fatalf("migrer.sh %v : %v", args, err)
		return "", -1
	}
}

func (b *banc) reussir(extra []string, args ...string) string {
	b.t.Helper()
	return b.reussirAvec("", extra, args...)
}

func (b *banc) reussirAvec(entree string, extra []string, args ...string) string {
	b.t.Helper()
	sortie, code := b.lancerAvec(entree, extra, args...)
	if code != 0 {
		b.t.Fatalf("migrer.sh %s : code %d\n%s", strings.Join(args, " "), code, sortie)
	}
	return sortie
}

func (b *banc) echouer(extra []string, args ...string) string {
	b.t.Helper()
	sortie, code := b.lancer(extra, args...)
	if code == 0 {
		b.t.Fatalf("migrer.sh %s : réussite inattendue\n%s", strings.Join(args, " "), sortie)
	}
	return sortie
}

// marque et depuis délimitent la part du journal écrite par une exécution.
func (b *banc) marque() int { return len(b.journal()) }

func (b *banc) depuis(n int) string { return b.journal()[n:] }

func (b *banc) journal() string {
	data, err := os.ReadFile(filepath.Join(b.dir, "journal"))
	if err != nil && !os.IsNotExist(err) {
		b.t.Fatal(err)
	}
	return string(data)
}

func (b *banc) etat() string { return b.lire(filepath.Join(b.travail, "shopfr/etat")) }

func contient(t *testing.T, quoi, texte string, attendus ...string) {
	t.Helper()
	for _, a := range attendus {
		if !strings.Contains(texte, a) {
			t.Errorf("%s : « %s » manque\n%s", quoi, a, texte)
		}
	}
}

func exclut(t *testing.T, quoi, texte string, interdits ...string) {
	t.Helper()
	for _, a := range interdits {
		if strings.Contains(texte, a) {
			t.Errorf("%s : « %s » inattendu\n%s", quoi, a, texte)
		}
	}
}

var executer = []string{"--executer", "--oui"}

func phase(args ...string) []string { return append(append([]string{}, executer...), args...) }

// jusquALaBascule déclare, synchronise et bascule shopfr, le DNS désignant déjà la cible.
func (b *banc) jusquALaBascule() {
	b.t.Helper()
	b.reussir(nil, phase("declarer", "shopfr")...)
	b.reussir(nil, phase("synchroniser", "shopfr")...)
	b.reussir([]string{dnsCible}, phase("basculer", "shopfr")...)
}

// Chaque élément des listes du manifeste est traité : ssh, sans -n, lisait
// l'entrée standard de la boucle « while read » qui l'appelait, et seul le
// premier élément de chaque liste l'était — la seconde base restait vide, le
// second worker tournait sur les deux serveurs.
func TestChaqueElementDesListesEstTraite(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrireManifeste(b.manifeste(b.projetShop()))
	b.reussir(nil, phase("declarer", "shopfr")...)

	m := b.marque()
	b.reussir(nil, phase("synchroniser", "shopfr")...)
	contient(t, "synchronisation", b.depuis(m),
		"[mysql<] shopfr : -- dump de shop_prod",
		"[mysql<] shopfrstats : -- dump de shop_stats")
	for _, f := range []string{"www/index.php", "www/public/uploads/photo.jpg", "www/public/media/film.mp4", "www/.env.local"} {
		if !existe(filepath.Join(b.home, f)) {
			t.Errorf("%s absent du home après la synchronisation", f)
		}
	}

	m = b.marque()
	b.reussir([]string{dnsCible}, phase("basculer", "shopfr")...)
	contient(t, "bascule", b.depuis(m),
		"shop-a.migre-ogs", "shop-b.migre-ogs", "supervisorctl stop w1", "supervisorctl stop w2",
		"DROP DATABASE IF EXISTS `shopfr`", "DROP DATABASE IF EXISTS `shopfrstats`")

	m = b.marque()
	b.reussir([]string{dnsSource}, phase("retour", "shopfr")...)
	contient(t, "retour", b.depuis(m), "supervisorctl start w1", "supervisorctl start w2")
	for _, f := range []string{"shop-a", "shop-b"} {
		if !existe(filepath.Join(b.source, "etc/cron.d", f)) {
			t.Errorf("/etc/cron.d/%s de la source non rétabli", f)
		}
	}
}

// Une fois basculé, synchroniser est refusé avant toute copie : rejoué — un
// sélecteur oublié suffit —, il supprimait la base de production (DROP
// DATABASE) et les fichiers reçus depuis la bascule (rsync --delete).
func TestSynchroniserRefuseUnProjetBascule(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrireManifeste(b.manifeste(b.projetShop()))
	b.jusquALaBascule()

	for _, args := range [][]string{phase("synchroniser"), phase("synchroniser", "shopfr"), {"synchroniser"}} {
		m := b.marque()
		sortie := b.echouer(nil, args...)
		contient(t, "synchroniser après la bascule", sortie, "synchronisation refusée")
		exclut(t, "synchroniser après la bascule", sortie+b.depuis(m), "DROP DATABASE", "--delete")
	}

	// Le retour rend la source maîtresse : resynchroniser redevient légitime.
	b.reussir([]string{dnsSource}, phase("retour", "shopfr")...)
	m := b.marque()
	b.reussir(nil, phase("synchroniser", "shopfr")...)
	contient(t, "synchroniser après le retour", b.depuis(m), "DROP DATABASE IF EXISTS `shopfr`")
}

// Une bascule interrompue après la copie finale — DNS trop lent — reprend à
// l'attente du DNS : refaire la copie écraserait ce que la cible a reçu des
// résolveurs déjà basculés. Le conseil de reprise est exécutable tel quel.
func TestRepriseDeBasculeNeRefaitPasLaCopieFinale(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrireManifeste(b.manifeste(b.projetShop()))
	b.reussir(nil, phase("declarer", "shopfr")...)
	b.reussir(nil, phase("synchroniser", "shopfr")...)

	sortie := b.echouer([]string{dnsSource}, phase("--delai-dns", "0", "basculer", "shopfr")...)
	contient(t, "bascule interrompue", sortie,
		"DNS pas encore propagé",
		"Reprendre : "+b.script+" -m "+b.manif+" --executer basculer shopfr")

	// La bascule interrompue — qui fait refuser synchroniser — se voit dans etat.
	contient(t, "etat, bascule interrompue", b.reussir(nil, "etat"),
		"shopfr : bascule engagée le", "copie finale faite le")

	m := b.marque()
	b.reussir([]string{dnsCible}, phase("basculer", "shopfr")...)
	j := b.depuis(m)
	exclut(t, "reprise de la bascule", j, "DROP DATABASE", "[rsync ", "supervisorctl stop", "crontab -l")
	contient(t, "état", b.etat(), "copie ", "bascule ")
	exclut(t, "etat, bascule faite", b.reussir(nil, "etat"), "bascule engagée")
}

// Une bascule reprise après un échec survenu entre l'arrêt des tâches et la
// copie finale rejoue l'arrêt : la sauvegarde de la crontab de la source n'en
// est pas vidée, et le retour la réinstalle. Le « supervisorctl stop » rejoué
// échoue sur un worker déjà arrêté, comme le vrai : la reprise ne s'y arrête
// plus, l'arrêt ayant été constaté au premier passage.
func TestRepriseDeBasculeGardeLaCrontabDeLaSource(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrireManifeste(b.manifeste(b.projetShop()))
	b.reussir(nil, phase("declarer", "shopfr")...)
	b.reussir(nil, phase("synchroniser", "shopfr")...)
	crontab := b.lire(filepath.Join(b.source, "spool/shop"))
	sauvegarde := filepath.Join(b.source, "root/migration-ogs/shopfr/crontab")

	// Le code de la source introuvable : la copie finale échoue, tâches déjà arrêtées.
	code := filepath.Join(b.racine, "current")
	if err := os.Rename(code, code+".ailleurs"); err != nil {
		t.Fatal(err)
	}
	b.echouer([]string{dnsCible}, phase("basculer", "shopfr")...)
	if got := b.lire(sauvegarde); got != crontab {
		t.Fatalf("sauvegarde de la crontab après le premier passage : %q", got)
	}
	if err := os.Rename(code+".ailleurs", code); err != nil {
		t.Fatal(err)
	}

	sortie := b.reussir([]string{dnsCible}, phase("basculer", "shopfr")...)
	contient(t, "reprise de la bascule", sortie,
		"« supervisorctl stop w1 » a échoué ; la même commande avait réussi le",
		"« supervisorctl stop w2 » a échoué ; la même commande avait réussi le")
	if got := b.lire(sauvegarde); got != crontab {
		t.Errorf("sauvegarde de la crontab vidée par la reprise : %q", got)
	}
	b.reussir([]string{dnsSource}, phase("retour", "shopfr")...)
	if got := b.lire(filepath.Join(b.source, "spool/shop")); got != crontab {
		t.Errorf("crontab de la source après le retour : %q, attendu %q", got, crontab)
	}
}

// La base d'un voisin déjà présent sur la cible n'est ni reprise ni supprimée :
// la déclaration est refusée avant tout dépôt, et la synchronisation n'affiche
// aucun DROP. MySQL et MariaDB partagent le même serveur.
func TestLaGardeRefuseLaBaseDUnVoisin(t *testing.T) {
	b := nouveauBanc(t)
	b.voisin("boutiquefr", "boutique.fr", "prestashop")
	p := b.projetShop()
	p["bases"] = L{M{"moteur": "mysql", "source": "prestashop", "cible": "prestashop"}}
	p["ogs"].(M)["spec"].(M)["databases"] = L{M{"engine": "mariadb", "name": "prestashop", "user": "shopfr"}}
	b.ecrireManifeste(b.manifeste(p))

	sortie := b.echouer(nil, phase("declarer", "shopfr")...)
	contient(t, "déclaration", sortie, "la base mysql:prestashop est déclarée par le projet boutiquefr")
	if existe(filepath.Join(b.prefixe, "etc/ogs/projects/shopfr.yaml")) {
		t.Error("shopfr.yaml déposé malgré le refus")
	}
	exclut(t, "déclaration refusée", b.journal(), "[ogs] apply shopfr")

	sortie = b.echouer(nil, "synchroniser", "shopfr")
	exclut(t, "synchronisation simulée", sortie, "DROP DATABASE")
}

// Un projet de même identifiant, présent sur la cible sans que la migration l'y
// ait déposé, n'est pas repris : ni apply, ni rsync --delete dans son home.
func TestUnProjetDejaPresentSurLaCibleNEstPasRepris(t *testing.T) {
	b := nouveauBanc(t)
	b.voisin("shopfr", "shop.fr", "shopfr")
	b.ecrireManifeste(b.manifeste(b.projetShop()))

	sortie := b.echouer(nil, phase("declarer", "shopfr")...)
	contient(t, "déclaration", sortie, "sans avoir été déposé par cette migration")
	exclut(t, "déclaration refusée", b.journal(), "[ogs] apply shopfr")

	sortie = b.echouer(nil, "synchroniser", "shopfr")
	exclut(t, "synchronisation simulée", sortie, "--delete", "DROP DATABASE")
}

// Une base qui existe déjà sur le moteur de la cible, sans qu'aucun projet la
// déclare, n'est pas reprise par la première déclaration.
func TestUneBaseDejaPresenteSurLaCibleEstRefusee(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrire(filepath.Join(b.dir, "bases-cible"), "shopfrstats\n")
	b.ecrireManifeste(b.manifeste(b.projetShop()))

	sortie := b.echouer(nil, phase("declarer", "shopfr")...)
	contient(t, "déclaration", sortie, "la base shopfrstats existe déjà sur la cible")
	if existe(filepath.Join(b.prefixe, "etc/ogs/projects/shopfr.yaml")) {
		t.Error("shopfr.yaml déposé malgré le refus")
	}
}

// Un compte système qui existe déjà sur la cible, hors de tout projet d'ogs,
// n'est pas repris par la première déclaration.
func TestUnCompteDejaPresentSurLaCibleEstRefuse(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrire(filepath.Join(b.dir, "comptes-cible"), "shopfr\n")
	b.ecrireManifeste(b.manifeste(b.projetShop()))

	sortie := b.echouer(nil, phase("declarer", "shopfr")...)
	contient(t, "déclaration", sortie, "le compte shopfr existe déjà sur la cible")
	exclut(t, "déclaration refusée", b.journal(), "[ogs] apply shopfr")
}

// Deux projets du manifeste qui visent la même base se détruiraient l'un l'autre.
func TestDeuxProjetsDuManifesteNePartagentPasUneBase(t *testing.T) {
	b := nouveauBanc(t)
	autre := b.projetShop()
	autre["id"] = "autrefr"
	autre["ogs"].(M)["metadata"] = M{"name": "autrefr", "host": "autre.fr"}
	autre["ogs"].(M)["spec"].(M)["databases"] = L{M{"engine": "mariadb", "name": "shopfr", "user": "autrefr"}}
	autre["bases"] = L{M{"moteur": "mysql", "source": "autre_prod", "cible": "shopfr"}}
	b.ecrireManifeste(b.manifeste(b.projetShop(), autre))

	sortie, code := b.lancer(nil, "controle", "shopfr")
	if code != 3 {
		t.Fatalf("controle : code %d, attendu 3\n%s", code, sortie)
	}
	contient(t, "controle", sortie, "base en commun : mysql:shopfr")
}

// Une préproduction de la cible ne fait pas refuser la migration de sa
// production : « ogs site clone --to prep » lui laisse le metadata.host de
// celle-ci, mais ogs juge l'unicité sur les domaines servis, qu'elles ne
// partagent pas. La garde comparait le host brut, et refusait déclaration,
// synchronisation et bascule.
func TestUnePreproductionDeLaCibleNeBloquePasSaProduction(t *testing.T) {
	b := nouveauBanc(t)
	b.voisin("shopfrprep", "shop.fr", "shopfrprep", ", env: prep")
	b.ecrireManifeste(b.manifeste(b.projetShop()))
	b.jusquALaBascule()
	contient(t, "état", b.etat(), "bascule ")
}

// Un manifeste migre une production et sa préproduction, au même host. Deux
// préproductions d'un même host, elles, recevraient d'ogs le même domaine.
func TestLeManifesteMigreUneProductionEtSaPreproduction(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrireManifeste(b.manifeste(b.projetShop(), b.projetShopPrep()))
	b.reussir(nil, "controle")
	b.reussir(nil, phase("declarer", "shopfr")...)
	b.reussir(nil, phase("declarer", "shopfrprep")...)
	b.reussir(nil, phase("synchroniser", "shopfr")...)
	b.reussir(nil, phase("synchroniser", "shopfrprep")...)
	// Le DNS d'une préproduction est celui du host qu'ogs lui construit : jugée
	// sur shop.fr, sa bascule attendait que la production désigne la cible.
	sortie := b.reussir([]string{dnsSource}, "prealables", "shopfrprep")
	contient(t, "préalables de la préproduction", sortie, "shop.preprod-pp.net → "+ipSource)
	exclut(t, "préalables de la préproduction", sortie, "shop.fr → ")

	autre := b.projetShopPrep()
	autre["id"] = "shopfrprep2"
	autre["bases"] = L{M{"moteur": "mysql", "source": "shop_prep2", "cible": "shopfrprep2"}}
	o := autre["ogs"].(M)
	o["metadata"].(M)["name"] = "shopfrprep2"
	o["spec"].(M)["databases"] = L{M{"engine": "mariadb", "name": "shopfrprep2", "user": "shopfrprep2"}}
	b.ecrireManifeste(b.manifeste(b.projetShop(), b.projetShopPrep(), autre))
	sortie, code := b.lancer(nil, "controle")
	if code != 3 {
		t.Fatalf("controle de deux préproductions d'un même host : code %d, attendu 3\n%s", code, sortie)
	}
	contient(t, "controle", sortie, "shopfrprep, shopfrprep2: domaine en commun : shop.preprod-pp.net")
	exclut(t, "controle", sortie, "shopfr, shopfrprep:")
}

// La garde voit ce qu'ogs compare : les domaines servis, alias compris — www.
// ôté, comme le fait ogs —, et non le seul metadata.host.
func TestLaGardeRefuseUnDomaineServiParUnVoisin(t *testing.T) {
	b := nouveauBanc(t)
	b.voisin("boutiquefr", "boutique.fr", "boutique", "",
		"  domains: [{name: boutique.fr}, {name: www.shop.fr, role: alias}]\n")
	b.ecrireManifeste(b.manifeste(b.projetShop()))

	sortie := b.echouer(nil, phase("declarer", "shopfr")...)
	contient(t, "déclaration", sortie, "le domaine shop.fr est celui du projet boutiquefr")
	exclut(t, "déclaration refusée", b.journal(), "[ogs] apply shopfr")
}

// Une préproduction sans spec.domains écrit sert le host qu'ogs construit sur son
// domaine dédié, où shop.fr et shop.com donnent le même, shop.preprod-pp.net. La
// garde ne comparait avant le dépôt que le host brut et l'environnement : la
// déclaration passait, ogs créait le compte dans le home du voisin, et la
// synchronisation y aurait remplacé son site.
func TestLaGardeRefuseLeDomaineDePreproductionDUnVoisin(t *testing.T) {
	b := nouveauBanc(t)
	b.voisin("shopcomprep", "shop.com", "shopcomprep", ", env: prep")
	b.ecrireManifeste(b.manifeste(b.projetShopPrep()))

	sortie := b.echouer(nil, phase("declarer", "shopfrprep")...)
	contient(t, "déclaration", sortie,
		"le domaine shop.preprod-pp.net est celui du projet shopcomprep",
		"le home "+filepath.Join(b.prefixe, "home/shop.preprod-pp.net")+" recoupe celui du projet shopcomprep")
	if existe(filepath.Join(b.prefixe, "etc/ogs/projects/shopfrprep.yaml")) {
		t.Error("shopfrprep.yaml déposé malgré le refus")
	}
	exclut(t, "déclaration refusée", b.journal(), "[ogs] apply shopfrprep")
}

// Un voisin dont le home est celui que le projet migré recevrait par défaut —
// un projet adopté garde ses noms réels — fait refuser la déclaration : le home
// n'était comparé que s'il était écrit dans le manifeste, et ogs y aurait créé
// le compte du projet migré.
func TestLaGardeRefuseLeHomeParDefautDUnVoisin(t *testing.T) {
	b := nouveauBanc(t)
	b.voisin("ancienshop", "ancien-shop.fr", "ancienshop", "", "  layout: {root: "+b.home+"}\n")
	b.ecrireManifeste(b.manifeste(b.projetShop()))

	sortie := b.echouer(nil, phase("declarer", "shopfr")...)
	contient(t, "déclaration", sortie, "le home "+b.home+" recoupe celui du projet ancienshop")
	exclut(t, "déclaration refusée", b.journal(), "[ogs] apply shopfr")
}

// Un home déjà présent sur la cible, hors de tout projet d'ogs — un site hors
// d'ogs, les restes d'un projet supprimé —, n'est pas repris par la première
// déclaration : la synchronisation remplacerait ce qu'il contient.
func TestUnHomeDejaPresentSurLaCibleEstRefuse(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrire(filepath.Join(b.home, "www/index.php"), "<?php // un site hors d'ogs\n")
	b.ecrireManifeste(b.manifeste(b.projetShop()))

	sortie := b.echouer(nil, phase("declarer", "shopfr")...)
	contient(t, "déclaration", sortie, "le home "+b.home+" existe déjà sur la cible")
	exclut(t, "déclaration refusée", b.journal(), "[ogs] apply shopfr")
}

// Un utilisateur de base déjà présent sur le moteur de la cible n'est pas repris
// par la première déclaration : ogs se l'approprierait, aux dépens de
// l'application qui l'emploie.
func TestUnUtilisateurDeBaseDejaPresentSurLaCibleEstRefuse(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrire(filepath.Join(b.dir, "utilisateurs-base-cible"), "shopfrstats\n")
	b.ecrireManifeste(b.manifeste(b.projetShop()))

	sortie := b.echouer(nil, phase("declarer", "shopfr")...)
	contient(t, "déclaration", sortie, "l'utilisateur de base shopfrstats existe déjà sur la cible")
	exclut(t, "déclaration refusée", b.journal(), "[ogs] apply shopfr")
}

// Le modèle du dépôt, contrôlé tel quel, ne signale que les champs à remplir : le
// marqueur « À VÉRIFIER » cité par son déroulé, en bas du fichier, était compté
// dans le dernier projet, et tout manifeste rempli d'après lui était refusé.
func TestLeModeleNeSignaleQueSesChampsARemplir(t *testing.T) {
	b := nouveauBanc(t)
	modele, err := filepath.Abs("../../docs/migration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b.manif = modele
	sortie, code := b.lancer(nil, "controle", "shopfr")
	if code != 3 {
		t.Fatalf("controle du modèle : code %d, attendu 3\n%s", code, sortie)
	}
	contient(t, "controle du modèle", sortie, "source.ssh est requis", "source.ip_publique est requis")
	exclut(t, "controle du modèle", sortie, "À VÉRIFIER", "shopfr:")
}

// Root n'écrit jamais par le nom dans le home du projet, qui appartient à ce
// dernier : la source est tirée vers un miroir hors du home, et tout ce qui
// écrit dans le home — rsync récepteur, création de répertoire, réécriture de
// la configuration — le fait sous le compte du projet.
func TestRootNEcritJamaisDansLeHome(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrireManifeste(b.manifeste(b.projetShop()))
	b.reussir(nil, phase("declarer", "shopfr")...)

	m := b.marque()
	b.reussir(nil, phase("synchroniser", "shopfr")...)
	b.examinerEcritures(b.depuis(m))
	contient(t, "configuration réécrite", b.lire(filepath.Join(b.home, "www/.env.local")),
		"APP_ENV=prod", "DATABASE_URL='mysql://shopfr:S3cret@localhost/shopfr'")
	exclut(t, "configuration réécrite", b.journal(), "S3cret")

	m = b.marque()
	b.reussir([]string{dnsCible}, phase("basculer", "shopfr")...)
	b.examinerEcritures(b.depuis(m))
}

// Un lien symbolique venu de la source, sur un composant de la destination d'une
// donnée hors code — www/public, pour www/public/uploads —, ne mène pas root
// hors du miroir : chaque donnée y est tirée dans un sous-arbre à elle, jamais
// sous la copie du code. Tirée sous elle, par le nom, elle suivait le lien : root
// créait, écrivait et supprimait (--delete) là où la source l'avait décidé.
//
// Le lien est relatif, pour que le banc — qui ne change pas d'identité — sépare
// les deux côtés : depuis le miroir, il désigne un répertoire de root hors du
// miroir ; depuis le home, un répertoire où le compte du projet écrirait de
// toute façon, et où la copie, qui s'y fait sous ce compte, peut aller.
func TestUnLienDeLaSourceNeMenePasRootHorsDuMiroir(t *testing.T) {
	b := nouveauBanc(t)
	public := filepath.Join(b.racine, "current/public")
	if err := os.RemoveAll(public); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../victime", public); err != nil {
		t.Fatal(err)
	}
	precieux := filepath.Join(b.travail, "victime/uploads/precieux.conf")
	b.ecrire(precieux, "à root\n")
	if err := os.MkdirAll(filepath.Join(b.prefixe, "victime"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.ecrireManifeste(b.manifeste(b.projetShop()))
	b.reussir(nil, phase("declarer", "shopfr")...)
	b.reussir(nil, phase("synchroniser", "shopfr")...)

	if !existe(precieux) {
		t.Error("root a supprimé, à travers un lien de la source, un fichier hors du miroir")
	}
	for _, f := range []string{"uploads/photo.jpg", "media/film.mp4"} {
		if existe(filepath.Join(b.travail, "victime", f)) {
			t.Errorf("root a écrit %s à travers un lien de la source, hors du miroir", f)
		}
	}
}

// Une destination dont le nom porte une espace arrive telle quelle. Le récepteur,
// lancé sans shell, recevait de rsync 3.2.4 et suivants un chemin échappé pour
// un shell — « mes\ envois » — et la copie échouait, ou visait un autre
// répertoire.
func TestUneDestinationAvecEspaceArriveTelleQuelle(t *testing.T) {
	b := nouveauBanc(t)
	p := b.projetShop()
	src := p["source"].(M)
	src["donnees_hors_code"] = L{M{"de": "shared/uploads", "vers": "www/public/mes envois"}}
	src["configuration"] = L{}
	b.ecrireManifeste(b.manifeste(p))
	b.reussir(nil, phase("declarer", "shopfr")...)
	b.reussir(nil, phase("synchroniser", "shopfr")...)
	if !existe(filepath.Join(b.home, "www/public/mes envois/photo.jpg")) {
		t.Error("www/public/mes envois/photo.jpg absent du home après la synchronisation")
	}
}

// prealables signale un domaine qui ne désigne pas exactement la source :
// bascule et retour attendent que chaque résolveur rende exactement l'adresse
// d'une machine, ce qu'un domaine derrière un CDN ou un répartiteur ne fera
// jamais — l'attente échouait alors en pleine fenêtre.
func TestLesPrealablesSignalentUnDomaineQuiNeDesignePasLaSource(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrireManifeste(b.manifeste(b.projetShop()))
	contient(t, "préalables", b.reussir([]string{dnsSource}, "prealables", "shopfr"), "shop.fr → "+ipSource)

	sortie := b.echouer([]string{"FAUX_DIG_A=192.0.2.80"}, "prealables", "shopfr")
	contient(t, "préalables, domaine derrière un CDN", sortie,
		"shop.fr ne résout pas exactement vers "+ipSource, "--sans-attente-dns")
}

// Un domaine sans aucune adresse — nouveau, déclaré du seul côté d'ogs — ne fait
// pas échouer les préalables : il sera créé vers la cible à la bascule. Et un
// projet déjà basculé n'y est plus jugé sur la source : ses domaines désignent la
// cible, et prealables sans sélecteur — tous les projets — échouait dès la
// première vague faite, en conseillant de corriger source.ip_publique.
func TestLesPrealablesNeJugentNiUnDomaineNouveauNiUnProjetBascule(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrireManifeste(b.manifeste(b.projetShop()))
	contient(t, "préalables, domaine nouveau", b.reussir([]string{"FAUX_DIG_A="}, "prealables", "shopfr"),
		"shop.fr : aucune adresse")

	b.jusquALaBascule()
	sortie := b.reussir([]string{dnsCible}, "prealables")
	contient(t, "préalables après la bascule", sortie, "shopfr : basculé le")
	exclut(t, "préalables après la bascule", sortie, "ne résout pas exactement")
}

// Sans issue jusque-là, une bascule ou un retour dont le domaine ne désignera
// jamais exactement la machine — derrière un CDN — laissaient source et cible en
// maintenance. --sans-attente-dns laisse l'exploitant constater la propagation,
// et il doit l'affirmer même sous --oui : sans cela, le retour rouvrait la source
// aussitôt la consigne DNS affichée, les domaines désignant encore la cible.
//
// Chaque appel porte --delai-dns 0 : si l'option cessait d'agir, l'attente du
// DNS échouerait sur-le-champ au lieu de bloquer le banc une demi-heure.
func TestSansAttenteDNSBasculeEtRetourAboutissent(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrireManifeste(b.manifeste(b.projetShop()))
	cdn := []string{"FAUX_DIG_A=192.0.2.80"}
	b.reussir(nil, phase("declarer", "shopfr")...)
	b.reussir(nil, phase("synchroniser", "shopfr")...)

	sortie := b.echouer(cdn, phase("--delai-dns", "0", "basculer", "shopfr")...)
	contient(t, "bascule, DNS jamais exact", sortie, "reprendre avec --sans-attente-dns")
	sansAttente := func(p string) []string { return phase("--sans-attente-dns", "--delai-dns", "0", p, "shopfr") }
	contient(t, "bascule sans affirmation, sous --oui", b.echouer(cdn, sansAttente("basculer")...), "abandon")
	exclut(t, "état", b.etat(), "bascule ")
	sortie = b.reussirAvec("shopfr\n", cdn, sansAttente("basculer")...)
	contient(t, "bascule reprise", sortie, "propagation non vérifiée (--sans-attente-dns)")
	contient(t, "état", b.etat(), "bascule ")

	maintenance := filepath.Join(b.racine, "maintenance.flag")
	b.echouer(cdn, phase("--delai-dns", "0", "retour", "shopfr")...)
	contient(t, "retour sans affirmation, sous --oui", b.echouer(cdn, sansAttente("retour")...), "abandon")
	if !existe(maintenance) {
		t.Error("source sortie de maintenance sans que personne ait constaté le retour du DNS")
	}
	b.reussirAvec("shopfr\n", cdn, sansAttente("retour")...)
	if existe(maintenance) || existe(filepath.Join(b.home, "www/maintenance-cible.flag")) {
		t.Error("maintenance laissée en place après le retour")
	}
	contient(t, "état", b.etat(), "retour ")
}

// examinerEcritures relit le journal d'une phase. Une copie vers le home est un
// rsync de root vers « env:<home>/… », dont le récepteur — le processus qui
// écrit — est lancé par runuser sous le compte du projet : c'est la ligne
// « rsync-serveur » qui la suit. Sous -s, ce récepteur reçoit ses chemins par le
// protocole, et sa ligne de commande n'en porte aucun.
func (b *banc) examinerEcritures(journal string) {
	b.t.Helper()
	sousHome := func(champ string) bool { return champ == b.home || strings.HasPrefix(champ, b.home+"/") }
	recuParLeProjet, recepteurAttendu := false, ""
	for _, ligne := range strings.Split(journal, "\n") {
		champs := strings.Fields(ligne)
		if len(champs) < 2 {
			continue
		}
		dest := champs[len(champs)-1]
		switch {
		case strings.HasPrefix(ligne, "[rsync root]"):
			if sousHome(dest) {
				b.t.Errorf("root écrit dans le home par rsync : %s", ligne)
			}
			if distant, ok := strings.CutPrefix(dest, "env:"); ok && sousHome(distant) {
				if !strings.Contains(ligne, " -e runuser -u shopfr -g shopfr -- ") {
					b.t.Errorf("copie vers le home dont le récepteur n'est pas lancé sous shopfr : %s", ligne)
				}
				if !strings.Contains(" "+ligne+" ", " -s ") {
					b.t.Errorf("copie vers le home sans -s : le chemin arriverait échappé : %s", ligne)
				}
				recepteurAttendu = ligne
			}
		case strings.HasPrefix(ligne, "[rsync-serveur ") && !strings.Contains(ligne, "--sender"):
			if recepteurAttendu != "" {
				if strings.HasPrefix(ligne, "[rsync-serveur shopfr]") {
					recuParLeProjet = true
				} else {
					b.t.Errorf("rsync reçoit dans le home sous une autre identité que shopfr : %s\n(pour %s)", ligne, recepteurAttendu)
				}
				recepteurAttendu = ""
			} else if sousHome(dest) {
				b.t.Errorf("rsync reçoit dans le home hors d'une copie par le compte du projet : %s", ligne)
			}
		case strings.HasPrefix(ligne, "[install root]"):
			for _, c := range champs[2:] {
				if sousHome(c) {
					b.t.Errorf("root crée dans le home : %s", ligne)
				}
			}
		}
	}
	if !recuParLeProjet {
		b.t.Errorf("aucune copie reçue dans le home sous le compte shopfr\n%s", journal)
	}
	contient(b.t, "écritures", journal, "[runuser shopfr:shopfr] python3 - config "+b.home+"/www/.env.local")
}

// Une vérification passée en HTTP faute de certificat compte ses échecs : une
// 500, ou une page sans le texte attendu, arrêtait jusque-là d'un simple
// avertissement. Jamais -k : avec lui, le premier vhost TLS de la cible — un
// voisin — répondait pour ce domaine.
func TestLaVerificationDegradeeNeLaissePasPasserUnSiteCasse(t *testing.T) {
	b := nouveauBanc(t)
	p := b.projetShop()
	p["verifications"] = L{M{"url": "https://shop.fr/", "statut": 200, "contient": "Ma boutique"}}
	b.ecrireManifeste(b.manifeste(p))

	for _, c := range []struct {
		nom  string
		env  []string
		code int
	}{
		{"erreur 500 en HTTP", []string{"FAUX_CURL_HTTP=500", "FAUX_CURL_CORPS=<h1>500</h1>"}, 1},
		{"page sans le texte attendu", []string{"FAUX_CURL_HTTP=200", "FAUX_CURL_CORPS=Bienvenue chez le voisin"}, 1},
		{"page attendue", []string{"FAUX_CURL_HTTP=200", "FAUX_CURL_CORPS=<title>Ma boutique</title>"}, 0},
		{"redirection vers le même site en HTTPS", []string{"FAUX_CURL_HTTP=301", "FAUX_CURL_REDIRECTION=https://shop.fr/"}, 0},
		{"redirection vers un autre site", []string{"FAUX_CURL_HTTP=301", "FAUX_CURL_REDIRECTION=https://boutique.fr/"}, 1},
	} {
		if sortie, code := b.lancer(c.env, "verifier", "shopfr"); code != c.code {
			t.Errorf("%s : code %d, attendu %d\n%s", c.nom, code, c.code, sortie)
		}
	}
	for _, ligne := range strings.Split(b.journal(), "\n") {
		if strings.HasPrefix(ligne, "[curl]") && strings.Contains(" "+ligne+" ", " -k ") {
			t.Errorf("curl -k : %s", ligne)
		}
	}

	// À la bascule, la même 500 rétablit la source au lieu de passer au DNS.
	casse := []string{"FAUX_CURL_HTTP=500", dnsCible}
	b.reussir(nil, phase("declarer", "shopfr")...)
	b.lancer(casse, phase("synchroniser", "shopfr")...)
	sortie := b.echouer(casse, phase("basculer", "shopfr")...)
	contient(t, "bascule", sortie, "bascule abandonnée")
	exclut(t, "bascule", sortie, "DNS : faire pointer")
	if existe(filepath.Join(b.racine, "maintenance.flag")) {
		t.Error("source laissée en maintenance après l'abandon")
	}
	b.reussir([]string{"FAUX_CURL_HTTP=200", "FAUX_CURL_CORPS=Ma boutique"}, phase("synchroniser", "shopfr")...)
}

// Une vérification par URL sans statut ni texte attendus exige au moins une
// réponse du site, sans erreur du serveur : elle acceptait jusque-là le code 000,
// une cible muette passant pour vérifiée.
func TestUneVerificationSansStatutExigeUneReponse(t *testing.T) {
	b := nouveauBanc(t)
	p := b.projetShop()
	p["verifications"] = L{M{"url": "https://shop.fr/"}}
	b.ecrireManifeste(b.manifeste(p))

	for _, c := range []struct {
		nom  string
		env  []string
		code int
	}{
		{"aucune réponse", nil, 1},
		{"erreur du serveur", []string{"FAUX_CURL_HTTP=502"}, 1},
		{"réponse du site", []string{"FAUX_CURL_HTTP=200"}, 0},
		{"accès refusé, mais réponse", []string{"FAUX_CURL_HTTP=403"}, 0},
	} {
		if sortie, code := b.lancer(c.env, "verifier", "shopfr"); code != c.code {
			t.Errorf("%s : code %d, attendu %d\n%s", c.nom, code, c.code, sortie)
		}
	}
}

// Un retour sans bascule engagée ne touche à rien : il réinstallerait sur la
// source la crontab sauvegardée par une bascule ancienne.
func TestRetourRefuseSansBascule(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrireManifeste(b.manifeste(b.projetShop()))
	b.reussir(nil, phase("declarer", "shopfr")...)
	b.ecrire(filepath.Join(b.source, "root/migration-ogs/shopfr/crontab"), "# ancienne crontab\n")

	m := b.marque()
	sortie := b.echouer([]string{dnsSource}, phase("retour", "shopfr")...)
	contient(t, "retour", sortie, "aucune bascule engagée")
	exclut(t, "retour refusé", b.depuis(m), "[source]", "[ogs] apply")
}

// Le retour met la cible en maintenance et rétablit les tâches de la source,
// mais n'ouvre la source qu'une fois que le DNS la désigne : sinon, pendant la
// propagation, chaque machine recevait sa part des écritures.
func TestRetourAttendLeDNSAvantDOuvrirLaSource(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrireManifeste(b.manifeste(b.projetShop()))
	b.jusquALaBascule()
	maintenance := filepath.Join(b.racine, "maintenance.flag")
	maintenanceCible := filepath.Join(b.home, "www/maintenance-cible.flag")

	m := b.marque()
	sortie := b.echouer([]string{dnsCible}, phase("--delai-dns", "0", "retour", "shopfr")...)
	contient(t, "retour interrompu", sortie, "Reprendre : "+b.script+" -m "+b.manif+" --executer retour shopfr")
	contient(t, "retour interrompu", b.depuis(m), "supervisorctl start w1", "supervisorctl start w2")
	if !existe(maintenance) {
		t.Error("source sortie de maintenance avant que le DNS la désigne")
	}
	if !existe(maintenanceCible) {
		t.Error("cible laissée ouverte pendant le retour du DNS")
	}

	// Le retour repris rejoue les relances : sur un worker déjà relancé,
	// « supervisorctl start » échoue, sans que la relance soit dite incomplète.
	sortie = b.reussir([]string{dnsSource}, phase("retour", "shopfr")...)
	if existe(maintenance) || existe(maintenanceCible) {
		t.Error("maintenance laissée en place après le retour")
	}
	contient(t, "retour repris", sortie, "« supervisorctl start w1 » a échoué ; la même commande avait réussi le")
	exclut(t, "retour repris", sortie, "relance incomplète")
}

// Un apply en échec sur la cible — la machine qu'on quitte — ne bloque plus le
// retour : la source est rétablie, et l'échec est signalé.
func TestRetourNeDependPasDUnApplyReussiSurLaCible(t *testing.T) {
	b := nouveauBanc(t)
	b.ecrireManifeste(b.manifeste(b.projetShop()))
	crontab := b.lire(filepath.Join(b.source, "spool/shop"))
	b.jusquALaBascule()

	m := b.marque()
	sortie := b.echouer([]string{dnsSource, "FAUX_OGS_APPLY_RC=1"}, phase("retour", "shopfr")...)
	contient(t, "retour", sortie, "cible non désactivée : shopfr")
	contient(t, "retour", b.depuis(m), "[ogs] service stop shopfr", "supervisorctl start w1")
	if existe(filepath.Join(b.racine, "maintenance.flag")) {
		t.Error("source laissée en maintenance")
	}
	if got := b.lire(filepath.Join(b.source, "spool/shop")); got != crontab {
		t.Errorf("crontab de la source non rétablie : %q", got)
	}
	contient(t, "état", b.etat(), "retour ")
}
