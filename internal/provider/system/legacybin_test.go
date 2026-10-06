package system

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/spec"
)

// ancienBinOgs reconstitue l'ancien répertoire d'ogs : 0710, lien php vers
// l'interpréteur d'une version.
func ancienBinOgs(t *testing.T, p *model.Project, prov *Provider) string {
	t.Helper()
	legacy := filepath.Join(p.Spec.Layout.Root, ".bin")
	if err := os.MkdirAll(legacy, 0o710); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(legacy, 0o710); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(prov.Paths.PHPBin("8.1"), filepath.Join(legacy, "php")); err != nil {
		t.Fatal(err)
	}
	liensGeres(t, p, "php")
	return legacy
}

// liensGeres pose dans ~/.local/bin les liens qu'ogs y aurait mis.
func liensGeres(t *testing.T, p *model.Project, names ...string) {
	t.Helper()
	dir := model.BinDir(p.Spec.Layout.Root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.Symlink("/cible/"+n, filepath.Join(dir, n)); err != nil && !os.IsExist(err) {
			t.Fatal(err)
		}
	}
}

func TestRienNePartNiNEstReecritSansLeLienDeRemplacement(t *testing.T) {
	// Relecture (D123) : l'installation du runtime a échoué, ~/.local/bin/python
	// n'est pas posé. La tâche ne doit pas être réécrite vers un lien absent, ni
	// l'ancien lien, qui fonctionne, retiré.
	p, prov, _, _ := fixture(t)
	legacy := ancienBinOgs(t, p, prov)
	root := p.Spec.Layout.Root
	if err := os.Symlink(filepath.Join(root, "www", ".venv", "bin", "python"), filepath.Join(legacy, "python")); err != nil {
		t.Fatal(err)
	}
	p.Spec.Crons = []model.CronJob{{Name: "nuit", Schedule: "0 3 * * *", Command: root + "/.bin/python manage.py x"}}
	deposer(t, p, prov)
	q, s := prov.PlanLegacyCommands(p)
	if s != nil || q.Spec.Crons[0].Command != p.Spec.Crons[0].Command {
		t.Fatalf("aucune réécriture attendue sans ~/.local/bin/python, obtenu %v / %q", s, q.Spec.Crons[0].Command)
	}
	step := prov.PlanLegacyBin(q)
	if step == nil {
		t.Fatal("étape attendue")
	}
	if err := step.Apply(context.Background()); err == nil {
		t.Error("le retrait doit être refusé")
	}
	if _, err := os.Lstat(filepath.Join(legacy, "python")); err != nil {
		t.Error("l'ancien lien python doit rester")
	}
	// La déclaration ne désigne plus .bin, mais le lien de remplacement manque :
	// .bin reste aussi.
	p.Spec.Crons = nil
	deposer(t, p, prov)
	err := prov.PlanLegacyBin(p).Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), ".local/bin/python") {
		t.Fatalf("le lien manquant doit être nommé, obtenu %v", err)
	}
	// Le lien posé, la réécriture et le retrait suivent.
	liensGeres(t, p, "python")
	if err := prov.PlanLegacyBin(p).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(legacy); !os.IsNotExist(err) {
		t.Error("l'ancien .bin doit partir une fois les liens en place")
	}
}

// deposer écrit la déclaration du projet là où la migration la relit.
func deposer(t *testing.T, p *model.Project, prov *Provider) {
	t.Helper()
	if err := spec.Save(prov.Paths.ProjectFile(p.ID()), p); err != nil {
		t.Fatal(err)
	}
}

func TestLAncienBinResteTantQueLaDeclarationDeposeeLeDesigne(t *testing.T) {
	// Mineur 1 de la relecture : la réécriture du fichier refusée, la copie du
	// plan est réécrite mais pas la déclaration — « cron run » la relit.
	p, prov, _, _ := fixture(t)
	legacy := ancienBinOgs(t, p, prov)
	root := p.Spec.Layout.Root
	p.Spec.Crons = []model.CronJob{{Name: "export", Schedule: "0 3 * * *", Command: root + "/.bin/php x.php"}}
	deposer(t, p, prov)
	q, _ := prov.PlanLegacyCommands(p)
	s := prov.PlanLegacyBin(q)
	if s == nil {
		t.Fatal("étape attendue")
	}
	err := s.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "spec.crons[export].command") {
		t.Fatalf("le retrait doit attendre la déclaration, obtenu %v", err)
	}
	if _, err := os.Lstat(filepath.Join(legacy, "php")); err != nil {
		t.Error("le lien doit rester")
	}
	// Sans déclaration déposée (« apply -f »), rien n'est retiré non plus.
	if err := os.Remove(prov.Paths.ProjectFile(p.ID())); err != nil {
		t.Fatal(err)
	}
	if err := prov.PlanLegacyBin(q).Apply(context.Background()); err == nil {
		t.Error("sans déclaration déposée, .bin doit rester")
	}
}

func TestUnBinAUnAutreQueRootNEstPasLAncienRepertoire(t *testing.T) {
	// R3N-07 : hors mode simulé, un .bin qui n'appartient pas à root n'a pas
	// été posé par ogs, même à la forme exacte de l'ancien.
	p, prov, _, _ := fixture(t)
	legacy := ancienBinOgs(t, p, prov)
	prov.Exec = &systemeFactice{}
	if os.Geteuid() == 0 {
		t.Skip("le répertoire de l'essai appartient à root")
	}
	if s := prov.PlanLegacyBin(p); s != nil {
		t.Fatalf("aucune étape attendue pour un .bin de l'utilisateur, obtenu %q", s.Summary)
	}
	if _, err := os.Lstat(filepath.Join(legacy, "php")); err != nil {
		t.Error("le lien doit rester")
	}
}

func TestUnBinAuxLiensEtrangersEstLaisseEnPlace(t *testing.T) {
	// R3N-07 : liens vers les binaires de l'utilisateur, ou mode qu'ogs n'a
	// jamais posé — le répertoire est le sien.
	for name, setup := range map[string]func(legacy string) error{
		"lien étranger": func(legacy string) error {
			if err := os.Remove(filepath.Join(legacy, "php")); err != nil {
				return err
			}
			return os.Symlink("/opt/php-8.1-custom/bin/php", filepath.Join(legacy, "php"))
		},
		"mode 0755": func(legacy string) error { return os.Chmod(legacy, 0o755) },
	} {
		t.Run(name, func(t *testing.T) {
			p, prov, _, _ := fixture(t)
			legacy := ancienBinOgs(t, p, prov)
			if err := setup(legacy); err != nil {
				t.Fatal(err)
			}
			if s := prov.PlanLegacyBin(p); s != nil {
				t.Fatalf("aucune étape attendue, obtenu %q", s.Summary)
			}
		})
	}
}

func TestUnBinDeclareParLExploitantNEstPasRetire(t *testing.T) {
	p, prov, _, _ := fixture(t)
	ancienBinOgs(t, p, prov)
	p.Spec.Layout.Dirs = append(p.Spec.Layout.Dirs, model.Dir{Path: ".bin", Mode: "0750"})
	if s := prov.PlanLegacyBin(p); s != nil {
		t.Fatalf("un .bin déclaré ne doit pas être retiré, obtenu %q", s.Summary)
	}
}

func TestLAncienBinEn0750EstReconnu(t *testing.T) {
	// Avant D51, ogs posait .bin en 0750.
	p, prov, _, _ := fixture(t)
	legacy := ancienBinOgs(t, p, prov)
	if err := os.Chmod(legacy, 0o750); err != nil {
		t.Fatal(err)
	}
	deposer(t, p, prov)
	s := prov.PlanLegacyBin(p)
	if s == nil {
		t.Fatal("l'ancien .bin en 0750 doit être retiré")
	}
	if err := s.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(legacy); !os.IsNotExist(err) {
		t.Error("l'ancien .bin doit avoir disparu")
	}
}

func TestUneReferenceRelativeRetientLAncienBin(t *testing.T) {
	// R3N-08 : une forme qu'ogs ne réécrit pas garde le lien en place, et
	// l'étape nomme la valeur.
	p, prov, _, _ := fixture(t)
	legacy := ancienBinOgs(t, p, prov)
	p.Spec.Crons = []model.CronJob{{Name: "export", Schedule: "0 3 * * *", Command: "../.bin/php scripts/export.php"}}
	if _, s := prov.PlanLegacyCommands(p); s != nil {
		t.Error("une forme relative n'est pas réécrite")
	}
	s := prov.PlanLegacyBin(p)
	if s == nil || s.Critical {
		t.Fatalf("étape non critique attendue, obtenu %v", s)
	}
	err := s.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "spec.crons[export].command") {
		t.Fatalf("l'étape doit nommer la valeur, obtenu %v", err)
	}
	if _, err := os.Lstat(filepath.Join(legacy, "php")); err != nil {
		t.Error("le lien doit rester tant qu'une déclaration le désigne")
	}
}

func TestLesCommandesSontReecritesEnMemoire(t *testing.T) {
	p, prov, _, _ := fixture(t)
	ancienBinOgs(t, p, prov)
	root := p.Spec.Layout.Root
	liensGeres(t, p, "node")
	p.Spec.Crons = []model.CronJob{{Name: "export", Command: root + "/.bin/php www/x.php"}}
	p.Spec.Services = []model.Service{{Name: "idx", Template: "worker",
		Params: map[string]string{"command": "~/.bin/node app.js", "env.PHP": "$HOME/.bin/php"}}}
	before := p.Spec.Crons[0].Command
	q, s := prov.PlanLegacyCommands(p)
	if s == nil || s.Action != engine.Update || s.Critical {
		t.Fatalf("réécriture non critique attendue, obtenu %v", s)
	}
	if got := q.Spec.Crons[0].Command; got != root+"/.local/bin/php www/x.php" {
		t.Errorf("tâche : %q", got)
	}
	if got := q.Spec.Services[0].Params["command"]; got != "~/.local/bin/node app.js" {
		t.Errorf("worker : %q", got)
	}
	if got := q.Spec.Services[0].Params["env.PHP"]; got != "$HOME/.local/bin/php" {
		t.Errorf("variable : %q", got)
	}
	// B1 : le projet reçu n'est pas touché, et un second calcul rend la même étape.
	if p.Spec.Crons[0].Command != before || p.Spec.Services[0].Params["command"] != "~/.bin/node app.js" {
		t.Error("le plan ne doit jamais modifier le projet reçu")
	}
	if _, again := prov.PlanLegacyCommands(p); again == nil || again.Summary != s.Summary {
		t.Errorf("un second calcul doit rendre la même étape, obtenu %v", again)
	}
}

func TestLeRetraitRejugeLeRepertoireALApplication(t *testing.T) {
	// Entre le plan et l'étape, l'utilisateur renomme l'ancien .bin et pose le
	// sien : ses liens ne doivent pas partir.
	p, prov, _, _ := fixture(t)
	legacy := ancienBinOgs(t, p, prov)
	s := prov.PlanLegacyBin(p)
	if s == nil {
		t.Fatal("retrait attendu")
	}
	if err := os.Rename(legacy, legacy+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/opt/mon-php", filepath.Join(legacy, "php")); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(context.Background()); err == nil {
		t.Error("l'étape doit refuser un répertoire qui n'est plus l'ancien")
	}
	if target, err := os.Readlink(filepath.Join(legacy, "php")); err != nil || target != "/opt/mon-php" {
		t.Errorf("le lien de l'utilisateur doit rester : %q (%v)", target, err)
	}
}

func TestLesJournauxDeNginxAWwwDataSontConformes(t *testing.T) {
	// R1-44 : la réouverture des journaux (USR1) donne ceux de nginx à
	// www-data. Ce n'est pas un écart — pour nginx seul.
	if os.Geteuid() == 0 {
		t.Skip("l'essai joue www-data par l'uid courant")
	}
	for _, engineName := range []string{"nginx", "apache"} {
		t.Run(engineName, func(t *testing.T) {
			p, prov, _, ident := fixture(t)
			prov.Exec = &systemeFactice{}
			prov.WebEngine = engineName
			ident.Users[WebUser] = &UserInfo{Name: WebUser, UID: os.Getuid()}
			ident.Users[p.Spec.User.Name] = &UserInfo{Name: p.Spec.User.Name, UID: 99999}
			ident.Groups[p.Spec.User.Group] = &GroupInfo{Name: p.Spec.User.Group, GID: os.Getgid()}
			dir := p.Spec.Layout.LogDir
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{engineName + "-access.log", engineName + "-error.log"} {
				if err := os.WriteFile(filepath.Join(dir, f), nil, 0o640); err != nil {
					t.Fatal(err)
				}
			}
			obs, err := prov.Observe(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range prov.logSteps(p, obs) {
				if !strings.HasPrefix(s.Resource, "system/log/") {
					continue
				}
				if want := engineName == "nginx"; (s.Action == engine.NoOp) != want {
					t.Errorf("%s : %s (NoOp attendu : %v)", s.Resource, s.Action, want)
				}
			}
		})
	}
}

func TestLeCompteDesWorkersDeNginxVientDeSaConfiguration(t *testing.T) {
	_, prov, _, _ := fixture(t)
	prov.WebEngine = "nginx"
	if got := prov.webWorkerUser(); got != WebUser {
		t.Errorf("sans nginx.conf lisible : %q", got)
	}
	conf := filepath.Join(prov.Paths.Prefix(), "etc", "nginx", "nginx.conf")
	if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]string{
		"# user nobody;\nuser nginx;\nworker_processes auto;\n": "nginx",
		"user www-data www-data;\n":                             "www-data",
		"worker_processes auto;\n":                              "nobody",
	} {
		if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := prov.webWorkerUser(); got != want {
			t.Errorf("%q : %q, attendu %q", body, got, want)
		}
	}
	prov.WebEngine = "apache"
	if got := prov.webWorkerUser(); got != WebUser {
		t.Errorf("sous Apache : %q", got)
	}
}
