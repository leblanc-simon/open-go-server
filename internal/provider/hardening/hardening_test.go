package hardening

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
)

func fixture(t *testing.T, cibles ...Target) (*Provider, string) {
	t.Helper()
	prefix := t.TempDir()
	return &Provider{
		Prefix:  prefix,
		Targets: cibles,
		// Seul root existe à coup sûr : les comptes des services, absents en test,
		// font refuser la cible qui les nomme — fermer sans son groupe couperait le
		// service. Les tests qui éprouvent un groupe le résolvent eux-mêmes.
		LookupIDs: func(owner, group string) (int, int, error) {
			for _, nom := range []string{owner, group} {
				if nom != "" && nom != "root" {
					return -1, -1, os.ErrNotExist
				}
			}
			uid, gid := -1, -1
			if owner != "" {
				uid = 0
			}
			if group != "" {
				gid = 0
			}
			return uid, gid, nil
		},
	}, prefix
}

// cibleParDefaut rend l'entrée de la table livrée pour chemin : les tests
// éprouvent la table elle-même, non une copie qui pourrait s'en écarter.
func cibleParDefaut(t *testing.T, chemin string) Target {
	t.Helper()
	for _, c := range Defaults("/home") {
		if c.Path == chemin {
			return c
		}
	}
	t.Fatalf("%s absent de la table par défaut", chemin)
	return Target{}
}

// auCompteDuTesteur pose la cible en remplaçant les comptes qu'elle nomme par
// ceux qui lancent les tests, et les résout : root et les comptes des services
// n'existent pas ici, et sans ce détour la mécanique — groupe conforme, bits
// conservés — ne s'éprouverait qu'en root. Un propriétaire ou un groupe vide le
// reste : c'est précisément ce que la table décide de laisser en place.
func auCompteDuTesteur(t *testing.T, c Target) (*Provider, string) {
	t.Helper()
	moi, err := user.Current()
	if err != nil {
		t.Skipf("compte courant introuvable : %v", err)
	}
	groupe, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Skipf("groupe courant introuvable : %v", err)
	}
	if c.Owner != "" {
		c.Owner = moi.Username
	}
	if c.Group != "" {
		c.Group = groupe.Name
	}
	p, prefix := fixture(t, c)
	p.LookupIDs = func(owner, group string) (int, int, error) {
		uid, gid := -1, -1
		if owner != "" {
			if owner != moi.Username {
				return -1, -1, os.ErrNotExist
			}
			uid = os.Getuid()
		}
		if group != "" {
			if group != groupe.Name {
				return -1, -1, os.ErrNotExist
			}
			gid = os.Getgid()
		}
		return uid, gid, nil
	}
	return p, prefix
}

func creer(t *testing.T, chemin string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(chemin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(chemin, mode); err != nil {
		t.Fatal(err)
	}
}

func appliquer(t *testing.T, p *Provider) []State {
	t.Helper()
	states := p.Observe()
	plan := &engine.Plan{Project: "système"}
	for _, s := range p.Plan(states) {
		plan.Add(s)
	}
	if _, err := engine.Execute(context.Background(), plan, engine.Options{}); err != nil {
		t.Fatalf("application : %v", err)
	}
	return states
}

func TestRestreintUnRepertoireTropOuvert(t *testing.T) {
	p, prefix := fixture(t, Target{Path: "/etc/nginx", DirMode: 0o750})
	creer(t, filepath.Join(prefix, "etc/nginx"), 0o755)

	states := appliquer(t, p)
	if states[0].Verdict != VerdictRestrict {
		t.Errorf("verdict = %s", states[0].Verdict)
	}
	info, err := os.Stat(filepath.Join(prefix, "etc/nginx"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Errorf("mode = %04o, attendu 0750", got)
	}
	// Rejoué, plus rien à faire.
	for _, st := range p.Observe() {
		if st.Verdict != VerdictOK {
			t.Errorf("second passage : %s pour %s", st.Verdict, st.Target.Path)
		}
	}
}

func TestNeDesserreJamais(t *testing.T) {
	// Un administrateur qui a fermé plus que la cible avait une raison : la
	// desserrer serait une régression de sécurité commise par un outil de
	// sécurité.
	p, prefix := fixture(t, Target{Path: "/etc/nginx", DirMode: 0o750})
	creer(t, filepath.Join(prefix, "etc/nginx"), 0o700)

	states := p.Observe()
	if states[0].Verdict != VerdictStricter {
		t.Fatalf("verdict = %s, attendu %s", states[0].Verdict, VerdictStricter)
	}
	appliquer(t, p)
	info, _ := os.Stat(filepath.Join(prefix, "etc/nginx"))
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("mode = %04o : le répertoire a été desserré", got)
	}
}

func TestIgnoreCeQuiNexistePas(t *testing.T) {
	p, _ := fixture(t, Target{Path: "/etc/absent", DirMode: 0o750})
	states := p.Observe()
	if states[0].Verdict != VerdictAbsent {
		t.Errorf("verdict = %s, attendu %s", states[0].Verdict, VerdictAbsent)
	}
	if Restrict(states) != 0 {
		t.Error("un service non installé ne doit rien réclamer")
	}
}

func TestRefuseUnLienSymbolique(t *testing.T) {
	// Suivre un lien reviendrait à modifier une cible inconnue, éventuellement
	// hors d'/etc.
	p, prefix := fixture(t, Target{Path: "/etc/nginx", DirMode: 0o750})
	ailleurs := filepath.Join(prefix, "ailleurs")
	creer(t, ailleurs, 0o777)
	if err := os.MkdirAll(filepath.Join(prefix, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ailleurs, filepath.Join(prefix, "etc/nginx")); err != nil {
		t.Fatal(err)
	}
	states := appliquer(t, p)
	if states[0].Verdict != VerdictRefused {
		t.Errorf("verdict = %s, attendu %s", states[0].Verdict, VerdictRefused)
	}
	info, _ := os.Stat(ailleurs)
	if got := info.Mode().Perm(); got != 0o777 {
		t.Errorf("la cible du lien a été modifiée (%04o)", got)
	}
}

func TestPreserveLeBitDExecutionDesFichiers(t *testing.T) {
	// Les hooks vivent sous /etc/ogs : les priver du bit d'exécution les ferait
	// refuser par ogs lui-même, qui exige un script exécutable.
	p, prefix := fixture(t, Target{
		Path: "/etc/ogs", DirMode: 0o700, FileMode: 0o600, Recursive: true,
	})
	creer(t, filepath.Join(prefix, "etc/ogs/hooks"), 0o700)
	hook := filepath.Join(prefix, "etc/ogs/hooks/post-create")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(prefix, "etc/ogs/config.yaml")
	if err := os.WriteFile(conf, []byte("a: b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	appliquer(t, p)

	info, _ := os.Stat(hook)
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("le hook (%04o) n'est plus exécutable", info.Mode().Perm())
	}
	if info.Mode().Perm()&0o044 != 0 {
		t.Errorf("le hook (%04o) reste lisible au-delà de son propriétaire", info.Mode().Perm())
	}
	info, _ = os.Stat(conf)
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("la configuration = %04o, attendu 0600", got)
	}
}

func TestSkipLaisseTranquille(t *testing.T) {
	p, prefix := fixture(t, Target{Path: "/etc/mysql", DirMode: 0o750})
	p.Skip = []string{"/etc/mysql"}
	creer(t, filepath.Join(prefix, "etc/mysql"), 0o755)

	if states := p.Observe(); len(states) != 0 {
		t.Errorf("un chemin exclu ne doit pas être rapporté : %v", states)
	}
}

func TestLaTableParDefautNeFermePasEtcPHP(t *testing.T) {
	// Le PHP en ligne de commande, exécuté sous l'identité du projet par les
	// crons, lit son php.ini dans /etc/php : le fermer changerait silencieusement
	// le comportement de PHP.
	for _, t2 := range Defaults("/home") {
		if t2.Path != "/etc/php" {
			continue
		}
		if t2.DirMode&0o005 != 0o005 {
			t.Errorf("/etc/php doit rester traversable et lisible (%04o)", t2.DirMode)
		}
		return
	}
	t.Error("/etc/php doit figurer dans la table, ne serait-ce que pour dire qu'on n'y touche pas")
}

func TestFermeLaRacineDesHomes(t *testing.T) {
	// D60 : 0711 ferme l'énumération sans toucher à la traversée — le serveur web
	// sert un fichier en traversant les répertoires et en lisant le fichier, il
	// n'a jamais besoin de lister /home.
	p, prefix := fixture(t, Target{Path: "/home", Owner: "root", Group: "root", DirMode: 0o711})
	racine := filepath.Join(prefix, "home")
	creer(t, racine, 0o755)
	// Un projet, avec les modes que model.defaultDirs lui donne.
	creer(t, filepath.Join(racine, "shop.fr"), 0o711)
	creer(t, filepath.Join(racine, "shop.fr/www"), 0o710)

	states := appliquer(t, p)
	if states[0].Verdict != VerdictRestrict {
		t.Fatalf("verdict = %s, attendu %s", states[0].Verdict, VerdictRestrict)
	}
	info, err := os.Stat(racine)
	if err != nil {
		t.Fatal(err)
	}
	got := info.Mode().Perm()
	if got != 0o711 {
		t.Errorf("mode = %04o, attendu 0711", got)
	}
	if got&0o001 == 0 {
		t.Error("la traversée doit rester ouverte à tous : sinon plus un site n'est servi")
	}
	if got&0o004 != 0 {
		t.Error("l'énumération reste ouverte : le durcissement n'a servi à rien")
	}
	// Ni Recursive ni FileMode : les répertoires des projets appartiennent à
	// model.defaultDirs, et un second juge ferait osciller le plan.
	for chemin, attendu := range map[string]os.FileMode{
		"shop.fr":     0o711,
		"shop.fr/www": 0o710,
	} {
		info, err := os.Stat(filepath.Join(racine, chemin))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != attendu {
			t.Errorf("%s = %04o, attendu %04o : harden a débordé sur l'arborescence du projet",
				chemin, info.Mode().Perm(), attendu)
		}
	}
}

func TestLaRacineDesHomesSuitLaConfiguration(t *testing.T) {
	// naming.homeRoot peut désigner ailleurs que /home ; la table par défaut doit
	// viser la racine réellement employée, sinon elle durcit un chemin vide.
	cible := func(targets []Target, chemin string) *Target {
		for i := range targets {
			if targets[i].Path == chemin {
				return &targets[i]
			}
		}
		return nil
	}
	t2 := cible(Defaults("/srv/sites"), "/srv/sites")
	if t2 == nil {
		t.Fatal("la racine des homes configurée doit figurer dans la table")
	}
	if t2.DirMode != 0o711 {
		t.Errorf("mode = %04o, attendu 0711", t2.DirMode)
	}
	if t2.Recursive || t2.FileMode != 0 {
		t.Error("la racine des homes ne se traite pas récursivement : sous elle vivent les projets")
	}
	if cible(Defaults("/home"), "/home") == nil {
		t.Error("/home doit figurer dans la table par défaut")
	}
	// Une racine absente ou réduite à « / » n'entre pas dans la table : durcir la
	// racine du système fermerait tout.
	for _, racine := range []string{"", "/", "//", "/.."} {
		for _, t3 := range Defaults(racine) {
			if t3.Path == "/" {
				t.Errorf("Defaults(%q) vise « / »", racine)
			}
		}
	}
}

func TestNeDesserrePasUneRacineDesHomesDejaFermee(t *testing.T) {
	// Un /home en 0700 casse tout le parc — plus un site servi — mais ce n'est pas
	// à un outil de durcissement de le rouvrir : il ne desserre jamais. C'est
	// « doctor » qui a le devoir de le dire.
	p, prefix := fixture(t, Target{Path: "/home", DirMode: 0o711})
	creer(t, filepath.Join(prefix, "home"), 0o700)

	states := p.Observe()
	if states[0].Verdict != VerdictStricter {
		t.Fatalf("verdict = %s, attendu %s", states[0].Verdict, VerdictStricter)
	}
	appliquer(t, p)
	info, _ := os.Stat(filepath.Join(prefix, "home"))
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("mode = %04o : la racine a été desserrée", got)
	}
}

func TestUnRefusSExplique(t *testing.T) {
	// « refusé » sans motif laisse soupçonner un défaut d'ogs là où il y a une
	// décision.
	p, prefix := fixture(t, Target{Path: "/home", DirMode: 0o711})
	if err := os.WriteFile(filepath.Join(prefix, "home"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := p.Observe()[0]
	if st.Verdict != VerdictRefused {
		t.Fatalf("verdict = %s, attendu %s", st.Verdict, VerdictRefused)
	}
	if st.Reason == "" {
		t.Error("un refus doit porter son motif")
	}
}

// autreGroupe rend un groupe que le processus de test peut réellement poser, et
// qui n'est pas déjà celui du fichier : sans lui, le test ne distinguerait pas
// « groupe posé » de « groupe inchangé ».
func autreGroupe(t *testing.T, actuel int) int {
	t.Helper()
	if os.Geteuid() == 0 {
		// root pose n'importe quel groupe ; « daemon » (1) existe sur toute Debian.
		if actuel != 1 {
			return 1
		}
		return 0
	}
	groupes, err := os.Getgroups()
	if err != nil {
		t.Skipf("groupes du processus illisibles : %v", err)
	}
	for _, g := range groupes {
		if g != actuel {
			return g
		}
	}
	t.Skip("le processus de test n'appartient qu'à un seul groupe : un chgrp ne s'éprouve pas")
	return -1
}

// telegraf pose la cible et son fichier principal, dans le mode et le groupe
// demandés, et rend le chemin du fichier.
func telegraf(t *testing.T, mode os.FileMode) (*Provider, string, int) {
	t.Helper()
	p, prefix := fixture(t, Target{
		Path: "/etc/telegraf", Owner: "root", Group: "telegraf",
		DirMode: 0o750, FileMode: 0o640,
	})
	creer(t, filepath.Join(prefix, "etc/telegraf"), 0o750)
	conf := filepath.Join(prefix, "etc/telegraf/telegraf.conf")
	if err := os.WriteFile(conf, []byte("[agent]\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(conf, mode); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(conf)
	if err != nil {
		t.Fatal(err)
	}
	gid := autreGroupe(t, fileGID(info))
	p.LookupIDs = func(_, group string) (int, int, error) {
		if group != "telegraf" {
			return -1, -1, os.ErrNotExist
		}
		return 0, gid, nil
	}
	return p, conf, gid
}

func TestLesFichiersRecoiventLeGroupeDeLaCible(t *testing.T) {
	// Fermer telegraf.conf à 0640 sans lui donner le groupe de l'agent le rend
	// illisible au démon que la restriction prétend protéger : il démarre, ne
	// collecte rien, et se tait (D56). Le champ Risk de la cible dit « l'agent
	// tourne sous telegraf, d'où le groupe » — encore faut-il le poser.
	p, conf, gid := telegraf(t, 0o644)

	states := appliquer(t, p)
	if len(states[0].Loose) != 1 {
		t.Fatalf("le fichier trop ouvert doit être relevé : %v", states[0].Loose)
	}
	info, err := os.Stat(conf)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %04o, attendu 0640", got)
	}
	if got := fileGID(info); got != gid {
		t.Errorf("groupe = %d, attendu %d : le fichier fermé à 0640 reste illisible à l'agent",
			got, gid)
	}
	// Rejoué, plus rien à signaler sur les fichiers : sans le groupe dans le
	// constat, le verdict oscillerait ou mentirait.
	if loose := p.Observe()[0].Loose; len(loose) != 0 {
		t.Errorf("second passage : %v", loose)
	}
}

func TestUnFichierDejaFermeMaisSansLeGroupeResteSignale(t *testing.T) {
	// Sur un parc où « harden » a déjà tourné, les fichiers sont en 0640 root:root
	// et le seul mode les dit conformes : le verdict resterait « conforme » pour
	// l'éternité, et la correction du groupe ne serait jamais appliquée.
	p, conf, gid := telegraf(t, 0o640)

	st := p.Observe()[0]
	if st.Verdict != VerdictRestrict {
		t.Fatalf("verdict = %s, attendu %s", st.Verdict, VerdictRestrict)
	}
	if len(st.Loose) != 1 {
		t.Fatalf("le fichier sans le groupe de l'agent doit être relevé : %v", st.Loose)
	}
	if resume := p.summary(st); !strings.Contains(resume, "telegraf") {
		t.Errorf("le résumé doit nommer le groupe attendu : %q", resume)
	}
	appliquer(t, p)
	info, err := os.Stat(conf)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileGID(info); got != gid {
		t.Errorf("groupe = %d, attendu %d", got, gid)
	}
}

func TestNeDesserrePasUnFichierPlusStrict(t *testing.T) {
	// Le fichier retenu pour son seul groupe ne doit pas voir son mode rouvert :
	// un outil de sécurité qui desserre est une régression de sécurité (D56).
	p, conf, gid := telegraf(t, 0o600)

	appliquer(t, p)
	info, err := os.Stat(conf)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o : le fichier a été desserré vers la cible", got)
	}
	if got := fileGID(info); got != gid {
		t.Errorf("groupe = %d, attendu %d", got, gid)
	}
}

func TestLooseReleveLeModeCommeLeGroupe(t *testing.T) {
	// State.Loose n'énumère pas que des fichiers trop permissifs : un fichier plus
	// fermé que la cible mais privé de son groupe y figure aussi, et c'est alors le
	// groupe qui est en cause — son mode, lui, restera tel quel. Le champ est le
	// seul détail que « ogs harden -v » rend à l'exploitant : sa définition doit
	// être tenue par un test, faute de quoi elle dérive au fil des réécritures.
	p, prefix := fixture(t, Target{
		Path: "/etc/telegraf", Owner: "root", Group: "telegraf",
		DirMode: 0o750, FileMode: 0o640,
	})
	dir := filepath.Join(prefix, "etc/telegraf")
	creer(t, dir, 0o750)
	ecrire := func(nom string, mode os.FileMode) string {
		chemin := filepath.Join(dir, nom)
		if err := os.WriteFile(chemin, []byte("[agent]\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(chemin, mode); err != nil {
			t.Fatal(err)
		}
		return chemin
	}
	conforme := ecrire("conforme.conf", 0o640)
	ferme := ecrire("ferme.conf", 0o600)
	ouvert := ecrire("ouvert.conf", 0o644)

	info, err := os.Stat(conforme)
	if err != nil {
		t.Fatal(err)
	}
	gid := autreGroupe(t, fileGID(info))
	p.LookupIDs = func(_, group string) (int, int, error) {
		if group != "telegraf" {
			return -1, -1, os.ErrNotExist
		}
		return 0, gid, nil
	}
	// Le groupe de la cible sur deux des trois : seul « ferme.conf » le manque, et
	// c'est par là qu'il est retenu, son mode étant plus strict que la cible.
	for _, chemin := range []string{conforme, ouvert} {
		if err := os.Chown(chemin, -1, gid); err != nil {
			t.Skipf("chgrp impossible ici : %v", err)
		}
	}

	loose := p.Observe()[0].Loose
	attendu := []string{ferme, ouvert}
	if strings.Join(loose, "\n") != strings.Join(attendu, "\n") {
		t.Errorf("Loose = %v, attendu %v", loose, attendu)
	}

	appliquer(t, p)

	info, err = os.Stat(ferme)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o : le fichier retenu pour son groupe a vu son mode rouvert", got)
	}
}

func TestNeReposePasLeBitSetgid(t *testing.T) {
	// Le chown efface le bit setgid d'un fichier exécutable par son groupe, et le
	// chmod ne le repose pas : le mode visé ne le porte pas, et un fichier de
	// configuration n'a que faire de ce bit — le rendre reviendrait à desserrer.
	// Le commentaire d'« apply » l'annonce ; ce test l'éprouve.
	p, conf, _ := telegraf(t, 0o644|os.ModeSetgid)
	info, err := os.Stat(conf)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetgid == 0 {
		t.Skip("le système de fichiers ne conserve pas le bit setgid")
	}

	appliquer(t, p)

	info, err = os.Stat(conf)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetgid != 0 {
		t.Errorf("mode = %v : le bit setgid a été reposé", info.Mode())
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %04o, attendu 0640", got)
	}
}

func TestOpenDKIMEstFermeSansToucherAuxCles(t *testing.T) {
	// Les clés DKIM vivent sous /etc/opendkim : le répertoire se ferme aux
	// utilisateurs des projets, mais une clé en 0600 au compte d'OpenDKIM ne doit
	// être ni desserrée ni reprise — illisible au démon, elle retiendrait en file
	// tout le courrier du domaine.
	cible := cibleParDefaut(t, "/etc/opendkim")
	if cible.DirMode != 0o750 || cible.FileMode != 0o640 || cible.Group != "opendkim" || !cible.Recursive {
		t.Fatalf("cible inattendue : %+v", cible)
	}
	// Les comptes root et opendkim n'existent pas en test : la mécanique des modes
	// est éprouvée seule, le propriétaire restant celui qui lance les tests.
	cible.Owner, cible.Group = "", ""
	p, prefix := fixture(t, cible)
	racine := filepath.Join(prefix, "etc/opendkim")
	creer(t, racine, 0o755)
	creer(t, filepath.Join(racine, "keys/shopfr/shop.fr"), 0o750)
	cle := filepath.Join(racine, "keys/shopfr/shop.fr/ogs202609.private")
	if err := os.WriteFile(cle, []byte("clé"), 0o600); err != nil {
		t.Fatal(err)
	}
	creer(t, filepath.Join(racine, "ogs"), 0o750)
	table := filepath.Join(racine, "ogs/KeyTable")
	if err := os.WriteFile(table, []byte("table"), 0o644); err != nil {
		t.Fatal(err)
	}

	states := appliquer(t, p)
	if states[0].Verdict != VerdictRestrict {
		t.Fatalf("un /etc/opendkim en 0755 doit être à restreindre, obtenu %s", states[0].Verdict)
	}
	if info, _ := os.Stat(racine); info.Mode().Perm() != 0o750 {
		t.Errorf("/etc/opendkim attendu en 0750, obtenu %04o", info.Mode().Perm())
	}
	if info, _ := os.Stat(table); info.Mode().Perm() != 0o640 {
		t.Errorf("une table en 0644 doit passer en 0640, obtenu %04o", info.Mode().Perm())
	}
	if info, _ := os.Stat(cle); info.Mode().Perm() != 0o600 {
		t.Errorf("la clé ne doit jamais être desserrée : %04o", info.Mode().Perm())
	}
	// Une fois fermé au régime qu'ogs pose lui-même, un second passage ne trouve
	// plus rien : l'apply et harden ne se contredisent pas.
	if again := p.Observe(); again[0].Verdict != VerdictOK {
		t.Errorf("second passage : %s (%v)", again[0].Verdict, again[0].Loose)
	}
}

func TestChaqueServiceGardeLeGroupeSousLequelIlLit(t *testing.T) {
	// D56 : fermer un répertoire qu'un démon lit sous sa propre identité
	// l'interromprait, donc le groupe cible est celui du service. La table l'avait
	// oublié pour MariaDB (User=mysql : au redémarrage suivant, il ignorait en
	// silence toute sa configuration et écoutait sur toutes les interfaces) et pour
	// les workers du serveur web (.htpasswd en 500, RewriteMap sans effet, mod_md
	// bloqué, apachectl -t au vert) ; elle prenait à Redis le répertoire qu'il
	// réécrit, et à postgres celui où pg_createcluster crée une version. Le
	// groupe doit au moins traverser et lire ; un propriétaire vide reste celui
	// du paquet ; et le risque affiché nomme ce que l'exploitant doit savoir.
	cas := []struct {
		chemin, proprio, groupe string
		bitsDuGroupe            os.FileMode
		risque                  []string
	}{
		{"/etc/apache2", "root", "www-data", 0o050, []string{"www-data", "AuthUserFile", "RewriteMap", "mod_md"}},
		{"/etc/nginx", "root", "www-data", 0o050, []string{"www-data", "auth_basic_user_file"}},
		{"/etc/mysql", "root", "mysql", 0o050, []string{"serveur", "mysql"}},
		{"/etc/postgresql", "", "postgres", 0o050, []string{"pg_wrapper", "5432", "-p"}},
		{"/etc/rabbitmq", "root", "rabbitmq", 0o050, nil},
		{"/etc/telegraf", "root", "telegraf", 0o050, nil},
		{"/etc/redis", "", "redis", 0o070, []string{"réécrit"}},
		{"/etc/ssl/private", "root", "ssl-cert", 0o010, nil},
		{"/etc/opendkim", "root", "opendkim", 0o050, nil},
	}
	for _, c := range cas {
		cible := cibleParDefaut(t, c.chemin)
		if cible.Owner != c.proprio || cible.Group != c.groupe {
			t.Errorf("%s : %s, attendu %s — le service qui y lit doit en garder l'accès",
				c.chemin, OwnerLabel(cible), OwnerLabel(Target{Owner: c.proprio, Group: c.groupe}))
		}
		if cible.DirMode&c.bitsDuGroupe != c.bitsDuGroupe {
			t.Errorf("%s : %04o ne laisse pas au groupe %s les droits %04o", c.chemin,
				cible.DirMode, c.groupe, c.bitsDuGroupe)
		}
		if cible.DirMode&0o007 != 0 {
			t.Errorf("%s : %04o laisse des droits aux autres, donc aux projets", c.chemin, cible.DirMode)
		}
		for _, mot := range c.risque {
			if !strings.Contains(cible.Risk, mot) {
				t.Errorf("%s : le risque affiché doit mentionner « %s » : %q", c.chemin, mot, cible.Risk)
			}
		}
	}
}

// ecrireFichier pose un fichier au mode exact demandé, umask écarté.
func ecrireFichier(t *testing.T, chemin string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(chemin, []byte("x\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(chemin, mode); err != nil {
		t.Fatal(err)
	}
}

func TestRedisGardeLeRepertoireQuIlReecrit(t *testing.T) {
	// Debian livre /etc/redis en 2770 redis:redis : Redis réécrit sa configuration
	// (CONFIG REWRITE, et Sentinel à chaque changement d'état) par un fichier
	// temporaire renommé dans le répertoire, et le setgid fait hériter ce fichier
	// du groupe redis. La table fermait ce répertoire en 0750 root:redis : « ERR
	// Rewriting config file: Permission denied », et l'état de Sentinel perdu au
	// redémarrage suivant, bascule comprise. Et un répertoire qui n'accorde rien
	// aux autres n'a rien à cacher aux projets.
	cas := []struct {
		nom           string
		avant, apres  os.FileMode
		verdict       string
		setgidAttendu bool
	}{
		{"régime du paquet", 0o770 | os.ModeSetgid, 0o770 | os.ModeSetgid, VerdictOK, true},
		{"ouvert aux autres", 0o775 | os.ModeSetgid, 0o770 | os.ModeSetgid, VerdictRestrict, true},
		// Rien n'est accordé : la cible admet l'écriture du groupe, mais un
		// répertoire qui ne la portait pas ne la reçoit pas d'un outil de
		// durcissement.
		{"sans écriture du groupe", 0o755, 0o750, VerdictRestrict, false},
	}
	for _, c := range cas {
		t.Run(c.nom, func(t *testing.T) {
			p, prefix := auCompteDuTesteur(t, cibleParDefaut(t, "/etc/redis"))
			dir := filepath.Join(prefix, "etc/redis")
			creer(t, dir, c.avant)
			if info, _ := os.Stat(dir); info.Mode()&os.ModeSetgid != c.avant&os.ModeSetgid {
				t.Skip("le système de fichiers ne conserve pas le bit setgid")
			}
			for _, f := range []string{"redis.conf", "sentinel.conf"} {
				ecrireFichier(t, filepath.Join(dir, f), 0o640)
			}

			states := appliquer(t, p)
			if states[0].Verdict != c.verdict {
				t.Errorf("verdict = %s, attendu %s", states[0].Verdict, c.verdict)
			}
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode() & (os.ModePerm | os.ModeSetgid); got != c.apres {
				t.Errorf("répertoire en %v, attendu %v", got, c.apres)
			}
			if c.setgidAttendu && info.Mode()&os.ModeSetgid == 0 {
				t.Error("le setgid a été retiré : le fichier que Redis réécrit perdrait le groupe redis")
			}
		})
	}
}

// peutLire dit si un processus d'identité uid, membre des seuls groupes donnés,
// lit rel sous base : la traversée de chaque répertoire du chemin, puis la
// lecture du fichier, selon la règle Unix de la première classe applicable, sans
// repli. Un lien est suivi, comme le démon le suit.
func peutLire(t *testing.T, base, rel string, uid int, groupes map[int]bool) bool {
	t.Helper()
	chemin := base
	composants := strings.Split(rel, "/")
	for i, c := range composants {
		chemin = filepath.Join(chemin, c)
		info, err := os.Stat(chemin)
		if err != nil {
			return false
		}
		st := info.Sys().(*syscall.Stat_t)
		droits := st.Mode
		switch {
		case int(st.Uid) == uid:
			droits >>= 6
		case groupes[int(st.Gid)]:
			droits >>= 3
		}
		voulu := uint32(0o1)
		if i == len(composants)-1 {
			voulu = 0o4
		}
		if droits&voulu == 0 {
			return false
		}
	}
	return true
}

func TestLetsencryptLaisseAuGroupeSslCertLaCleQuiLuiEstConfiee(t *testing.T) {
	// Convention Debian pour confier une clé certbot à un démon non root : le
	// groupe ssl-cert sur live/, archive/ et la clé, que certbot recopie d'une clé
	// à la suivante. La table rendait tout à root, récursivement, en 0700/0600 : le
	// démon perdait son certificat au redémarrage, et le groupe ne revenait plus.
	cible := cibleParDefaut(t, "/etc/letsencrypt")
	p, prefix := auCompteDuTesteur(t, cible)
	sslCert := autreGroupe(t, os.Getgid())
	le := filepath.Join(prefix, "etc/letsencrypt")
	for _, d := range []string{"", "archive/shop.fr", "live/shop.fr", "renewal", "renewal-hooks/deploy"} {
		creer(t, filepath.Join(le, d), 0o755)
	}
	for _, d := range []string{"archive", "live"} {
		if err := os.Chown(filepath.Join(le, d), -1, sslCert); err != nil {
			t.Skipf("chgrp impossible ici : %v", err)
		}
		creer(t, filepath.Join(le, d), 0o750)
	}
	fichiers := map[string]os.FileMode{
		"archive/shop.fr/privkey1.pem":   0o640,
		"archive/shop.fr/cert1.pem":      0o644,
		"archive/shop.fr/fullchain1.pem": 0o644,
		"renewal/shop.fr.conf":           0o644,
		"renewal-hooks/deploy/reload.sh": 0o755,
	}
	for f, m := range fichiers {
		ecrireFichier(t, filepath.Join(le, f), m)
	}
	cle := filepath.Join(le, "archive/shop.fr/privkey1.pem")
	if err := os.Chown(cle, -1, sslCert); err != nil {
		t.Skipf("chgrp impossible ici : %v", err)
	}
	for _, n := range []string{"privkey", "cert", "fullchain"} {
		if err := os.Symlink("../../archive/shop.fr/"+n+"1.pem",
			filepath.Join(le, "live/shop.fr/"+n+".pem")); err != nil {
			t.Fatal(err)
		}
	}
	// Le démon : User=nobody, Group=ssl-cert.
	demon := map[int]bool{sslCert: true}
	lus := []string{
		"letsencrypt/live/shop.fr/privkey.pem", "letsencrypt/live/shop.fr/fullchain.pem",
		"letsencrypt/archive/shop.fr/privkey1.pem", "letsencrypt/archive/shop.fr/fullchain1.pem",
	}
	for _, rel := range lus {
		if !peutLire(t, filepath.Join(prefix, "etc"), rel, 65534, demon) {
			t.Fatalf("fixture : le démon doit lire %s avant harden", rel)
		}
	}

	appliquer(t, p)

	for _, rel := range lus {
		if !peutLire(t, filepath.Join(prefix, "etc"), rel, 65534, demon) {
			t.Errorf("après harden, le démon du groupe ssl-cert ne lit plus %s", rel)
		}
	}
	for f, m := range fichiers {
		info, err := os.Stat(filepath.Join(le, f))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != m {
			t.Errorf("%s passé de %04o à %04o : les fichiers ne sont pas à reprendre", f, m, info.Mode().Perm())
		}
	}
	if info, _ := os.Stat(cle); fileGID(info) != sslCert {
		t.Errorf("la clé a perdu le groupe ssl-cert (gid %d, attendu %d)", fileGID(info), sslCert)
	}
	// La racine se ferme à l'énumération, jamais à la traversée.
	if info, _ := os.Stat(le); info.Mode().Perm()&0o004 != 0 || info.Mode().Perm()&0o001 == 0 {
		t.Errorf("racine en %04o : attendue traversable par tous et énumérable par personne",
			info.Mode().Perm())
	}
}

func TestUnCompteIntrouvableLaisseLeRepertoireEtLeDit(t *testing.T) {
	// Le groupe du service est ce par quoi il lit une fois les autres écartés :
	// sans lui, fermer le coupe. harden fermait quand même, annonçait « ✓ » sans
	// avoir posé le groupe, puis réclamait la même restriction à chaque passage —
	// code 6 et avertissement de doctor à perpétuité.
	p, prefix := fixture(t, Target{Path: "/etc/monagent", Owner: "root", Group: "monagent", DirMode: 0o750})
	dir := filepath.Join(prefix, "etc/monagent")
	creer(t, dir, 0o755)

	states := appliquer(t, p)
	st := states[0]
	if st.Verdict != VerdictRefused {
		t.Fatalf("verdict = %s, attendu %s", st.Verdict, VerdictRefused)
	}
	if !strings.Contains(st.Reason, "groupe monagent") {
		t.Errorf("le refus doit nommer le groupe manquant : %q", st.Reason)
	}
	if Restrict(states) != 0 || Refused(states) != 1 {
		t.Errorf("à restreindre = %d, refusés = %d : attendu 0 et 1", Restrict(states), Refused(states))
	}
	for _, s := range p.Plan(states) {
		if s.Mutates() {
			t.Errorf("une étape modifie une cible refusée : %s", s.Summary)
		}
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %04o : le répertoire a été fermé sans le groupe du service", info.Mode().Perm())
	}
}

func TestNeSuitPasUnLienPoseEntreLeConstatEtLEcriture(t *testing.T) {
	// La table laisse désormais des répertoires à leur service — /etc/redis à
	// redis —, qui peut remplacer une entrée par un lien entre le constat et
	// l'écriture. Écrire par le nom, c'est le laisser choisir sur quoi root pose
	// un groupe et un mode.
	t.Run("le répertoire", func(t *testing.T) {
		p, prefix := fixture(t, Target{Path: "/etc/nginx", DirMode: 0o750})
		dir := filepath.Join(prefix, "etc/nginx")
		creer(t, dir, 0o755)
		st := p.Observe()[0]

		ailleurs := filepath.Join(prefix, "ailleurs")
		creer(t, ailleurs, 0o777)
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(ailleurs, dir); err != nil {
			t.Fatal(err)
		}
		if err := p.apply(st); err == nil {
			t.Error("un lien posé à la place du répertoire doit faire échouer l'étape")
		}
		if info, _ := os.Stat(ailleurs); info.Mode().Perm() != 0o777 {
			t.Errorf("la cible du lien a été modifiée (%04o)", info.Mode().Perm())
		}
	})
	t.Run("un sous-répertoire", func(t *testing.T) {
		p, prefix := fixture(t, Target{Path: "/etc/ogs", DirMode: 0o700, FileMode: 0o600, Recursive: true})
		racine := filepath.Join(prefix, "etc/ogs")
		creer(t, filepath.Join(racine, "sous"), 0o700)
		ecrireFichier(t, filepath.Join(racine, "sous/f"), 0o644)
		st := p.Observe()[0]
		if len(st.Loose) != 1 {
			t.Fatalf("le fichier trop ouvert doit être relevé : %v", st.Loose)
		}

		ailleurs := filepath.Join(prefix, "ailleurs")
		creer(t, ailleurs, 0o755)
		ecrireFichier(t, filepath.Join(ailleurs, "f"), 0o644)
		if err := os.Rename(filepath.Join(racine, "sous"), filepath.Join(prefix, "ecarte")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(ailleurs, filepath.Join(racine, "sous")); err != nil {
			t.Fatal(err)
		}
		if err := p.apply(st); err != nil {
			t.Fatalf("application : %v", err)
		}
		if info, _ := os.Stat(filepath.Join(ailleurs, "f")); info.Mode().Perm() != 0o644 {
			t.Errorf("un fichier atteint par le lien a été modifié (%04o)", info.Mode().Perm())
		}
	})
}
