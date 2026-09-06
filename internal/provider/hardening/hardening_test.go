package hardening

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
)

func fixture(t *testing.T, cibles ...Target) (*Provider, string) {
	t.Helper()
	prefix := t.TempDir()
	return &Provider{
		Prefix:  prefix,
		Targets: cibles,
		// Les comptes système n'existent pas en test : la résolution échoue, et le
		// provider doit s'en tenir au mode.
		LookupIDs: func(string, string) (int, int, error) { return -1, -1, os.ErrNotExist },
	}, prefix
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
