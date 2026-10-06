// Package packaging éprouve les scripts de maintenance du paquet Debian.
//
// Ils sont exécutés tels que dpkg les enchaîne, dans un bac à sable : leurs
// chemins absolus y sont décalés sous un répertoire jetable, et « systemctl »
// comme « ogs » y sont des doublures qui notent leurs appels. La doublure de
// systemctl tient en outre l'état d'activation des unités, pour que les scripts
// le relisent comme ils le liraient de systemd. Le vrai dpkg sert aux
// comparaisons de versions : c'est sa sémantique qu'il faut éprouver.
package packaging

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/fs"
)

const timer = "ogs-renew.timer"

// prermJusqua003 est le prerm livré par toutes les versions publiées jusqu'à
// 0.0.3 comprise. À une mise à jour, c'est le prerm de la version en place que
// dpkg exécute : le postinst nouveau doit réparer ce qu'il a fait.
const prermJusqua003 = `#!/bin/sh
set -e

case "$1" in
remove | upgrade | deconfigure)
    if command -v systemctl >/dev/null 2>&1; then
        systemctl disable --now ogs-renew.timer >/dev/null 2>&1 || true
    fi
    ;;
esac
`

const doublureSystemctl = `#!/bin/sh
echo "systemctl $*" >> "$JOURNAL"
verbe=
unite=
for a in "$@"; do
    case "$a" in
    -*) ;;
    *) if [ -z "$verbe" ]; then verbe=$a; else unite=$a; fi ;;
    esac
done
case "$verbe" in
enable) echo enabled > "$ETATS/$unite" ;;
disable) echo disabled > "$ETATS/$unite" ;;
is-enabled)
    e=$(cat "$ETATS/$unite" 2>/dev/null || echo disabled)
    echo "$e"
    [ "$e" = enabled ]
    exit
    ;;
esac
exit 0
`

// requeteTerminal est ce que toute commande d'ogs écrit sur une sortie standard
// qui est un terminal : bubbletea demande sa couleur de fond (OSC 11), puis la
// position du curseur (CSI 6n). Sous apt, ce terminal est le pseudo-terminal
// donné à dpkg, où personne ne répond.
const requeteTerminal = "\x1b]11;?\x1b\\\x1b[6n"

const doublureOgs = `#!/bin/sh
echo "ogs $*" >> "$JOURNAL"
printf '\033]11;?\033\\\033[6n'
if [ "${OGS_CODE:-0}" != 0 ]; then
    echo "ogs : base d'état verrouillée" >&2
fi
exit "${OGS_CODE:-0}"
`

// bac est un bac à sable où les scripts du paquet s'exécutent sans toucher au
// système : racine y joue le rôle de « / ».
type bac struct {
	t       *testing.T
	racine  string
	paths   config.Paths
	scripts string
	env     []string
}

func nouveauBac(t *testing.T) *bac {
	t.Helper()
	if _, err := exec.LookPath("dpkg"); err != nil {
		t.Skip("dpkg absent : les comparaisons de versions ne peuvent pas être éprouvées")
	}
	racine := t.TempDir()
	b := &bac{t: t, racine: racine, paths: config.NewPaths(racine), scripts: filepath.Join(racine, "scripts")}
	bin := filepath.Join(racine, "bin")
	etats := filepath.Join(racine, "etats")
	for _, d := range []string{bin, etats, b.scripts, b.paths.SystemdDir(), b.paths.Etc(), b.paths.Lib(), b.paths.LogDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	b.ecrire(filepath.Join(bin, "systemctl"), doublureSystemctl, 0o755)
	b.ecrire(filepath.Join(bin, "ogs"), doublureOgs, 0o755)
	// Les chemins absolus des scripts sont décalés sous la racine du bac : ce
	// sont ceux d'ogs, et TestLesScriptsEmploientLesCheminsDOgs le vérifie.
	decaler := strings.NewReplacer("/etc/", racine+"/etc/", "/var/", racine+"/var/")
	for fichier, nom := range map[string]string{
		"postinstall.sh": "postinst", "preremove.sh": "prerm", "postremove.sh": "postrm",
	} {
		data, err := os.ReadFile(fichier)
		if err != nil {
			t.Fatal(err)
		}
		b.ecrire(filepath.Join(b.scripts, nom), decaler.Replace(string(data)), 0o755)
	}
	b.env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"JOURNAL="+filepath.Join(racine, "journal"),
		"ETATS="+etats,
	)
	return b
}

func (b *bac) ecrire(chemin, contenu string, mode os.FileMode) {
	b.t.Helper()
	if err := os.MkdirAll(filepath.Dir(chemin), 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(chemin, []byte(contenu), mode); err != nil {
		b.t.Fatal(err)
	}
}

// remplacer substitue un script, par exemple le prerm d'une version antérieure.
func (b *bac) remplacer(nom, contenu string) {
	b.ecrire(filepath.Join(b.scripts, nom), contenu, 0o755)
}

// lancer exécute un script de maintenance avec les arguments que dpkg lui passe.
func (b *bac) lancer(nom string, args ...string) (sortie, erreurs string) {
	b.t.Helper()
	return b.lancerAvec(nil, nom, args...)
}

func (b *bac) lancerAvec(env []string, nom string, args ...string) (sortie, erreurs string) {
	b.t.Helper()
	cmd := exec.Command("sh", append([]string{filepath.Join(b.scripts, nom)}, args...)...)
	cmd.Env = append(append([]string{}, b.env...), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		// Un script de maintenance en échec laisse le paquet à moitié installé
		// ou retiré : aucun des cas éprouvés ici ne doit en arriver là.
		b.t.Fatalf("%s %s : %v\nsortie : %s\nerreurs : %s", nom, strings.Join(args, " "), err, out.String(), errb.String())
	}
	return out.String(), errb.String()
}

// configurer pose la configuration d'une instance initialisée.
func (b *bac) configurer() {
	b.ecrire(b.paths.ConfigFile(), "web:\n  engine: apache\n", 0o644)
	b.ecrire(b.paths.MasterKey(), "clé", 0o400)
}

// poserTimerDOgs écrit le timer tel que le plan global l'écrit : sous l'en-tête
// d'ogs.
func (b *bac) poserTimerDOgs() {
	for _, u := range []string{timer, "ogs-renew.service"} {
		contenu := fs.Render(fs.Header{
			Project: "*", Resource: "tls/renew/" + u, Generated: time.Now(),
		}, []byte("[Unit]\nDescription=ogs\n"), "#")
		b.ecrire(filepath.Join(b.paths.SystemdDir(), u), string(contenu), 0o644)
	}
}

func (b *bac) poserEtat(unite, etat string) {
	b.ecrire(filepath.Join(b.racine, "etats", unite), etat+"\n", 0o644)
}

func (b *bac) etat(unite string) string {
	data, err := os.ReadFile(filepath.Join(b.racine, "etats", unite))
	if errors.Is(err, os.ErrNotExist) {
		return "disabled"
	}
	if err != nil {
		b.t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func (b *bac) appels() string {
	data, err := os.ReadFile(filepath.Join(b.racine, "journal"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		b.t.Fatal(err)
	}
	return string(data)
}

func (b *bac) temoin() string { return filepath.Join(b.paths.Lib(), "ogs-renew.timer.avant-retrait") }

func existe(chemin string) bool {
	_, err := os.Lstat(chemin)
	return err == nil
}

// Une mise à jour, une réinstallation ou un retour arrière laissent l'outil en
// place : le timer de renouvellement doit le rester aussi. Le prerm le
// désactivait sur « upgrade » ; après la première mise à jour, plus aucun
// certificat ne se renouvelait, et apply ne le réactivait pas.
func TestUneMiseAJourLaisseLeTimerActif(t *testing.T) {
	cas := []struct {
		nom      string
		prerm    []string
		postinst []string
	}{
		{"mise à jour", []string{"upgrade", "0.0.5"}, []string{"configure", "0.0.4"}},
		{"réinstallation", []string{"upgrade", "0.0.4"}, []string{"configure", "0.0.4"}},
		{"retour arrière", []string{"upgrade", "0.0.4"}, []string{"configure", "0.0.5"}},
		{"prerm de la version en place en échec", []string{"failed-upgrade", "0.0.4"}, []string{"configure", "0.0.4"}},
		{"déconfiguration", []string{"deconfigure", "in-favour", "autre", "1.0"}, []string{"configure", "0.0.4"}},
	}
	for _, c := range cas {
		t.Run(c.nom, func(t *testing.T) {
			b := nouveauBac(t)
			b.configurer()
			b.poserTimerDOgs()
			b.poserEtat(timer, "enabled")

			b.lancer("prerm", c.prerm...)
			if strings.Contains(b.appels(), "systemctl disable") {
				t.Errorf("le prerm %v désactive le timer :\n%s", c.prerm, b.appels())
			}
			_, erreurs := b.lancer("postinst", c.postinst...)
			if got := b.etat(timer); got != "enabled" {
				t.Errorf("timer %s après prerm %v puis postinst %v, attendu enabled", got, c.prerm, c.postinst)
			}
			if erreurs != "" {
				t.Errorf("postinst : sortie d'erreur inattendue %q", erreurs)
			}
		})
	}
}

// Depuis une version jusqu'à 0.0.3, c'est l'ancien prerm qui s'exécute, et il
// désactive le timer sans rien noter. Le postinst nouveau le réactive, s'il
// porte l'en-tête d'ogs : ogs active le timer chaque fois qu'il l'écrit.
func TestMiseAJourDepuisUneVersionFautiveReactiveLeTimer(t *testing.T) {
	for _, ancienne := range []string{"0.0.1", "0.0.3"} {
		t.Run(ancienne, func(t *testing.T) {
			b := nouveauBac(t)
			b.configurer()
			b.poserTimerDOgs()
			b.poserEtat(timer, "enabled")
			b.remplacer("prerm", prermJusqua003)

			b.lancer("prerm", "upgrade", "0.0.4")
			if got := b.etat(timer); got != "disabled" {
				t.Fatalf("le prerm de %s devrait désactiver le timer (état %s) : le cas n'éprouve rien", ancienne, got)
			}
			_, erreurs := b.lancer("postinst", "configure", ancienne)
			if got := b.etat(timer); got != "enabled" {
				t.Errorf("timer %s après la mise à jour depuis %s, attendu enabled\nappels :\n%s", got, ancienne, b.appels())
			}
			if !strings.Contains(b.appels(), "systemctl enable --now "+timer) {
				t.Errorf("le postinst n'a pas demandé « enable --now » :\n%s", b.appels())
			}
			if erreurs != "" {
				t.Errorf("postinst : sortie d'erreur inattendue %q", erreurs)
			}
		})
	}

	// Un timer de même nom que l'administrateur a posé ne regarde pas le paquet.
	t.Run("timer étranger", func(t *testing.T) {
		b := nouveauBac(t)
		b.configurer()
		b.ecrire(filepath.Join(b.paths.SystemdDir(), timer), "[Timer]\nOnCalendar=daily\n", 0o644)
		b.lancer("postinst", "configure", "0.0.3")
		if strings.Contains(b.appels(), "systemctl enable") {
			t.Errorf("le postinst active un timer qu'ogs n'a pas écrit :\n%s", b.appels())
		}
	})

	// Au-delà de 0.0.3, le prerm n'a rien désactivé : un timer arrêté l'a été
	// par l'administrateur, et le paquet ne passe pas outre — il le signale.
	t.Run("timer désactivé depuis une version corrigée", func(t *testing.T) {
		b := nouveauBac(t)
		b.configurer()
		b.poserTimerDOgs()
		b.poserEtat(timer, "disabled")
		b.lancer("prerm", "upgrade", "0.0.5")
		_, erreurs := b.lancer("postinst", "configure", "0.0.4")
		if got := b.etat(timer); got != "disabled" {
			t.Errorf("timer %s, attendu disabled : le choix de l'administrateur est écrasé", got)
		}
		if !strings.Contains(erreurs, "systemctl enable --now "+timer) {
			t.Errorf("le postinst ne signale pas le timer désactivé : %q", erreurs)
		}
	})
}

// Le retrait arrête le timer, qui appellerait un binaire disparu. La
// réinstallation lui rend l'état qu'il avait : actif s'il l'était, désactivé si
// l'administrateur l'avait voulu.
func TestRetraitPuisReinstallationRendLEtatDuTimer(t *testing.T) {
	t.Run("timer actif", func(t *testing.T) {
		b := nouveauBac(t)
		b.configurer()
		b.poserTimerDOgs()
		b.poserEtat(timer, "enabled")

		b.lancer("prerm", "remove")
		if got := b.etat(timer); got != "disabled" {
			t.Fatalf("timer %s après le retrait, attendu disabled", got)
		}
		b.lancer("postrm", "remove")
		// Le postrm garde le paquet en « rc » : à la réinstallation, dpkg passe
		// la dernière version configurée.
		_, erreurs := b.lancer("postinst", "configure", "0.0.4")
		if got := b.etat(timer); got != "enabled" {
			t.Errorf("timer %s après la réinstallation, attendu enabled\nappels :\n%s", got, b.appels())
		}
		if existe(b.temoin()) {
			t.Errorf("témoin %s laissé après la réinstallation", b.temoin())
		}
		if erreurs != "" {
			t.Errorf("postinst : sortie d'erreur inattendue %q", erreurs)
		}
	})

	t.Run("timer désactivé par l'administrateur", func(t *testing.T) {
		b := nouveauBac(t)
		b.configurer()
		b.poserTimerDOgs()
		b.poserEtat(timer, "disabled")

		b.lancer("prerm", "remove")
		_, erreurs := b.lancer("postinst", "configure", "0.0.4")
		if got := b.etat(timer); got != "disabled" {
			t.Errorf("timer %s après la réinstallation, attendu disabled", got)
		}
		if !strings.Contains(erreurs, "n'est pas activé") {
			t.Errorf("le postinst ne signale pas le timer désactivé : %q", erreurs)
		}
	})

	// config.yaml a disparu entre le retrait et la réinstallation : le postinst
	// annonce « ogs init », et rend quand même au timer ce que le retrait lui a
	// pris. Le témoin n'était lu qu'en présence d'une configuration : il
	// attendait une mise à jour ultérieure, et d'ici là rien ne renouvelait.
	t.Run("configuration disparue d'ici la réinstallation", func(t *testing.T) {
		b := nouveauBac(t)
		b.configurer()
		b.poserTimerDOgs()
		b.poserEtat(timer, "enabled")

		b.lancer("prerm", "remove")
		b.lancer("postrm", "remove")
		if err := os.Remove(b.paths.ConfigFile()); err != nil {
			t.Fatal(err)
		}
		sortie, erreurs := b.lancer("postinst", "configure", "0.0.4")
		if !strings.Contains(sortie, "ogs init") {
			t.Errorf("la sortie n'annonce pas « ogs init » : %q", sortie)
		}
		if got := b.etat(timer); got != "enabled" {
			t.Errorf("timer %s après la réinstallation, attendu enabled\nappels :\n%s", got, b.appels())
		}
		if existe(b.temoin()) {
			t.Errorf("témoin %s laissé après la réinstallation", b.temoin())
		}
		if erreurs != "" {
			t.Errorf("postinst : sortie d'erreur inattendue %q", erreurs)
		}
	})

	// Une version jusqu'à 0.0.3 n'avait pas de postrm : dpkg oubliait le paquet
	// au retrait, et la réinstallation ne reçoit aucune version. Rien ne dit
	// alors ce que voulait l'administrateur : le paquet signale, sans activer.
	t.Run("retrait par une version jusqu'à 0.0.3", func(t *testing.T) {
		b := nouveauBac(t)
		b.configurer()
		b.poserTimerDOgs()
		b.poserEtat(timer, "enabled")
		b.remplacer("prerm", prermJusqua003)

		b.lancer("prerm", "remove")
		_, erreurs := b.lancer("postinst", "configure")
		if got := b.etat(timer); got != "disabled" {
			t.Errorf("timer %s, attendu disabled : rien ne dit qu'il était actif", got)
		}
		if !strings.Contains(erreurs, "systemctl enable --now "+timer) {
			t.Errorf("le postinst ne donne pas le remède : %q", erreurs)
		}
	})

	// Le prerm échoue : dpkg rappelle le postinst, et le paquet reste en place.
	t.Run("retrait avorté", func(t *testing.T) {
		b := nouveauBac(t)
		b.configurer()
		b.poserTimerDOgs()
		b.poserEtat(timer, "enabled")
		b.lancer("prerm", "remove")
		b.lancer("postinst", "abort-remove")
		if got := b.etat(timer); got != "enabled" {
			t.Errorf("timer %s après un retrait avorté, attendu enabled", got)
		}
	})
}

// Une mise à jour passe par « ogs init », idempotent, qui migre la base d'état
// et crée les répertoires nouveaux ; son échec se lit, sans faire échouer dpkg.
// Le postinst lançait « ogs doctor », sortie jetée : un diagnostic complet pour
// une migration que toute commande fait, et dont l'échec disparaissait.
func TestPostinstMigreParInitEtDitSonEchec(t *testing.T) {
	b := nouveauBac(t)
	b.configurer()
	b.lancer("postinst", "configure", "0.0.4")
	appels := b.appels()
	if !strings.Contains(appels, "ogs init --quiet") {
		t.Errorf("le postinst ne lance pas « ogs init --quiet » :\n%s", appels)
	}
	if strings.Contains(appels, "ogs doctor") {
		t.Errorf("le postinst lance encore doctor :\n%s", appels)
	}

	b = nouveauBac(t)
	b.configurer()
	_, erreurs := b.lancerAvec([]string{"OGS_CODE=5"}, "postinst", "configure", "0.0.4")
	if !strings.Contains(erreurs, "base d'état verrouillée") {
		t.Errorf("l'erreur d'ogs est masquée : %q", erreurs)
	}
	if !strings.Contains(erreurs, "« ogs init » a échoué") {
		t.Errorf("le postinst ne dit pas l'échec de la migration : %q", erreurs)
	}
}

// apt donne à dpkg un pseudo-terminal, où toute commande d'ogs interroge le
// terminal : sans personne pour répondre — unattended-upgrades —, elle attend
// cinq secondes, et la requête s'inscrit dans la sortie et le journal d'apt. Le
// postinst écarte donc la sortie standard d'ogs ; ses erreurs restent lisibles
// (TestPostinstMigreParInitEtDitSonEchec).
func TestPostinstEcarteLaSortieStandardDOgs(t *testing.T) {
	b := nouveauBac(t)
	b.configurer()
	sortie, erreurs := b.lancer("postinst", "configure", "0.0.4")
	if !strings.Contains(b.appels(), "ogs init --quiet") {
		t.Fatalf("le postinst ne lance pas « ogs init --quiet » : le cas n'éprouve rien\n%s", b.appels())
	}
	for nom, flux := range map[string]string{"sortie": sortie, "erreurs": erreurs} {
		if strings.Contains(flux, requeteTerminal) {
			t.Errorf("la requête d'ogs au terminal passe dans la %s du postinst : %q", nom, flux)
		}
	}
}

// Une instance configurée a perdu sa clé maître : « ogs init » en engendrerait
// une nouvelle, sans un mot sous --quiet, et les secrets scellés par l'ancienne
// ne se reliraient plus. Le paquet ne tranche pas à la place de
// l'administrateur : il le dit, et rend quand même au timer ce qui lui a été
// pris.
func TestPostinstNEngendrePasUneCleMaitrePerdue(t *testing.T) {
	b := nouveauBac(t)
	b.configurer()
	if err := os.Remove(b.paths.MasterKey()); err != nil {
		t.Fatal(err)
	}
	b.poserTimerDOgs()
	b.poserEtat(timer, "enabled")
	b.remplacer("prerm", prermJusqua003)

	b.lancer("prerm", "upgrade", "0.0.4")
	_, erreurs := b.lancer("postinst", "configure", "0.0.3")
	if strings.Contains(b.appels(), "ogs init") {
		t.Errorf("le postinst lance « ogs init » sans clé maître :\n%s", b.appels())
	}
	for _, attendu := range []string{b.paths.MasterKey(), "sauvegarde", "ogs init"} {
		if !strings.Contains(erreurs, attendu) {
			t.Errorf("le postinst ne dit pas %q : %q", attendu, erreurs)
		}
	}
	if got := b.etat(timer); got != "enabled" {
		t.Errorf("timer %s après la mise à jour, attendu enabled", got)
	}
}

// Une instance configurée a perdu sa base d'état alors que des projets sont
// déclarés : « ogs init » la recréerait vide, sans un mot sous --quiet, et les
// commandes d'ogs, qui la refusent jusque-là (D120), repartiraient sur une base
// sans secrets. Le paquet le dit et ne la recrée pas. Sans projet, init tourne.
func TestPostinstNeRecreePasUneBaseDEtatPerdue(t *testing.T) {
	b := nouveauBac(t)
	b.configurer()
	b.ecrire(filepath.Join(b.paths.ProjectsDir(), "shopfr.yaml"), "metadata:\n  host: shop.fr\n", 0o600)
	_, erreurs := b.lancer("postinst", "configure", "0.0.4")
	if strings.Contains(b.appels(), "ogs init") {
		t.Errorf("le postinst lance « ogs init » sur une base d'état perdue :\n%s", b.appels())
	}
	for _, attendu := range []string{b.paths.StateDB(), "sauvegarde", "ogs init"} {
		if !strings.Contains(erreurs, attendu) {
			t.Errorf("le postinst ne dit pas %q : %q", attendu, erreurs)
		}
	}

	b = nouveauBac(t)
	b.configurer()
	b.ecrire(filepath.Join(b.paths.ProjectsDir(), "shopfr.yaml"), "metadata:\n  host: shop.fr\n", 0o600)
	b.ecrire(b.paths.StateDB(), "", 0o600)
	b.lancer("postinst", "configure", "0.0.4")
	if !strings.Contains(b.appels(), "ogs init --quiet") {
		t.Errorf("base présente : le postinst doit migrer par « ogs init » :\n%s", b.appels())
	}
}

// Sur une machine neuve, le paquet n'engendre pas la clé maître à l'insu de
// l'administrateur : il annonce « ogs init », et ne touche à rien d'autre.
func TestPostinstSurMachineNeuveAnnonceInit(t *testing.T) {
	b := nouveauBac(t)
	sortie, _ := b.lancer("postinst", "configure")
	if !strings.Contains(sortie, "ogs init") {
		t.Errorf("la sortie n'annonce pas « ogs init » : %q", sortie)
	}
	if appels := b.appels(); appels != "" {
		t.Errorf("appels inattendus sur une machine neuve :\n%s", appels)
	}
}

// La purge retire les unités de renouvellement, qui appellent un binaire
// disparu, et l'état du paquet ; elle garde la configuration, la clé maître,
// l'état d'ogs — les vhosts servent encore ses certificats —, ses journaux et
// les archives, et dit ce qui reste : dpkg n'en nomme que les répertoires qu'il
// n'a pas pu retirer, et plus rien ne purge les archives.
func TestPurgeRetireLesUnitesDOgsEtDitCeQuiReste(t *testing.T) {
	b := nouveauBac(t)
	b.configurer()
	b.poserTimerDOgs()
	// Un service de même nom posé par l'administrateur n'est pas celui d'ogs.
	etranger := filepath.Join(b.paths.SystemdDir(), "ogs-renew.service")
	b.ecrire(etranger, "[Service]\nExecStart=/usr/local/bin/renouveler\n", 0o644)
	b.ecrire(b.temoin(), "enabled\n", 0o644)
	for _, a := range []string{"shopfr-20260901T000000Z.tar.zst", "blogfr-20260902T000000Z.tar.zst"} {
		b.ecrire(filepath.Join(b.paths.ArchivesDir(), a), "archive", 0o600)
	}

	b.lancer("postrm", "remove")
	if !existe(filepath.Join(b.paths.SystemdDir(), timer)) {
		t.Fatalf("un retrait simple a supprimé le timer : la réinstallation n'aurait plus rien à rendre")
	}

	sortie, _ := b.lancer("postrm", "purge")
	if existe(filepath.Join(b.paths.SystemdDir(), timer)) {
		t.Errorf("le timer d'ogs survit à la purge")
	}
	if !existe(etranger) {
		t.Errorf("la purge a retiré une unité que l'administrateur a posée")
	}
	if existe(b.temoin()) {
		t.Errorf("le témoin du timer survit à la purge")
	}
	if !strings.Contains(b.appels(), "systemctl daemon-reload") {
		t.Errorf("pas de daemon-reload après le retrait des unités :\n%s", b.appels())
	}
	for _, garde := range []string{b.paths.MasterKey(), b.paths.ConfigFile()} {
		if !existe(garde) {
			t.Errorf("la purge a retiré %s", garde)
		}
	}
	for _, attendu := range []string{
		b.paths.Etc() + " est conservé", b.paths.Lib() + " est conservé", b.paths.LogDir() + " est conservé",
		"2 archive(s)", b.paths.ArchivesDir(),
	} {
		if !strings.Contains(sortie, attendu) {
			t.Errorf("la purge ne dit pas %q :\n%s", attendu, sortie)
		}
	}
}

// Les scripts relisent les fichiers d'ogs : chemins et marque d'en-tête doivent
// être les siens, sans quoi le paquet ne reconnaîtrait plus le timer qu'il gère.
func TestLesScriptsEmploientLesCheminsDOgs(t *testing.T) {
	p := config.NewPaths("")
	// Le témoin est écrit par le prerm, lu par le postinst, effacé par le
	// postrm : un nom qui diffère d'un script à l'autre, et l'état se perd.
	temoin := filepath.Join(p.Lib(), "ogs-renew.timer.avant-retrait")
	communs := []string{fs.Marker, p.SystemdDir() + "/", temoin}
	attendus := map[string][]string{
		"postinstall.sh": append([]string{p.ConfigFile(), p.MasterKey(), p.StateDB(), p.ProjectsDir() + "/"}, communs...),
		"preremove.sh":   communs,
		"postremove.sh":  append([]string{p.ArchivesDir(), p.Etc(), p.Lib() + " ", p.LogDir()}, communs...),
	}
	for fichier, chaines := range attendus {
		data, err := os.ReadFile(fichier)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range chaines {
			if !strings.Contains(string(data), c) {
				t.Errorf("%s n'emploie pas %q", fichier, c)
			}
		}
	}
}

// nfpm doit livrer les trois scripts : sans postrm, dpkg oublie le paquet dès le
// retrait, et la purge n'a jamais lieu.
func TestNfpmLivreLesScriptsDeMaintenance(t *testing.T) {
	scripts := nfpmScripts(t)
	for _, cle := range []string{"postinstall", "preremove", "postremove"} {
		chemin, ok := scripts[cle]
		if !ok {
			t.Errorf("nfpm.yaml ne déclare pas le script %s", cle)
			continue
		}
		if _, err := os.Stat(filepath.Join("..", chemin)); err != nil {
			t.Errorf("script %s : %v", cle, err)
		}
	}
}

func nfpmScripts(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "nfpm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var desc struct {
		Scripts map[string]string `yaml:"scripts"`
	}
	if err := yaml.Unmarshal(data, &desc); err != nil {
		t.Fatal(err)
	}
	return desc.Scripts
}

// Les dérogations lintian désignent une ligne précise d'un script : elles
// doivent suivre les appels à systemctl que lintian relève, sans quoi le
// contrôle échoue sur un script retouché, ou garde une dérogation morte. Le
// motif est celui de lintian : un appel en tête de commande, daemon-reload
// excepté.
func TestDerogationsLintianSuiventLesScripts(t *testing.T) {
	appel := regexp.MustCompile(`^(?:.+;)?\s*systemctl\b`)
	noms := map[string]string{"postinstall": "postinst", "preremove": "prerm", "postremove": "postrm"}
	var releves []string
	for cle, chemin := range nfpmScripts(t) {
		data, err := os.ReadFile(filepath.Join("..", chemin))
		if err != nil {
			t.Fatal(err)
		}
		for i, ligne := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(ligne), "#") || strings.Contains(ligne, "daemon-reload") {
				continue
			}
			if appel.MatchString(ligne) {
				releves = append(releves, noms[cle]+":"+strconv.Itoa(i+1))
			}
		}
	}

	data, err := os.ReadFile("lintian-overrides")
	if err != nil {
		t.Fatal(err)
	}
	derogation := regexp.MustCompile(`^ogs: maintainer-script-calls-systemctl \[(\S+)\]$`)
	var derogations []string
	for _, ligne := range strings.Split(string(data), "\n") {
		if m := derogation.FindStringSubmatch(ligne); m != nil {
			derogations = append(derogations, m[1])
		}
	}

	sort.Strings(releves)
	sort.Strings(derogations)
	if strings.Join(releves, " ") != strings.Join(derogations, " ") {
		t.Errorf("dérogations %v, appels relevés par lintian %v", derogations, releves)
	}
}
