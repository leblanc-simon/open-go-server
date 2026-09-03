package monitoring

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/provider/php"
	"leblanc.io/open-go-server/internal/secret"
	"leblanc.io/open-go-server/internal/store"
	"leblanc.io/open-go-server/internal/template"
)

func fixture(t *testing.T) (*Provider, config.Paths) {
	t.Helper()
	prefix := t.TempDir()
	paths := config.NewPaths(prefix)
	for _, v := range []string{"7.4", "8.3"} {
		if err := os.MkdirAll(paths.PHPPoolDir(v), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, bin := range []string{paths.PHPBin(v), paths.PHPFpmBin(v)} {
			if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	st, err := store.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	keyPath := filepath.Join(prefix, "master.key")
	if err := secret.GenerateKey(keyPath); err != nil {
		t.Fatal(err)
	}
	ks, err := secret.Open(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }
	tmpl := template.New(paths.TemplatesDir())
	rec := exec.NewRecorder()
	cfg := config.Default()
	cfg.Prefix = prefix
	return &Provider{
		Paths: paths, Cfg: cfg, Exec: rec, Templates: tmpl, Store: st, Keys: ks,
		PHP:       &php.Provider{Paths: paths, Exec: rec, Templates: tmpl, Store: st, Now: now},
		WebEngine: "nginx", WebUnit: "nginx", Now: now,
	}, paths
}

func project(t *testing.T, host string, kind model.AppKind, phpVersion string) *model.Project {
	t.Helper()
	p := model.New(host, naming.EnvProd, kind)
	if err := p.Normalize(filepath.Join(t.TempDir(), "home"), "preprod-pp.net"); err != nil {
		t.Fatal(err)
	}
	if phpVersion != "" && len(p.Spec.PHP.Pools) > 0 {
		p.Spec.PHP.Pools[0].Version = phpVersion
	}
	return p
}

// withLogs crée les journaux du projet : sans eux, aucune prison web n'est
// écrite, ce qui est précisément le comportement éprouvé plus bas.
func withLogs(t *testing.T, p *model.Project, engine string) {
	t.Helper()
	if err := os.MkdirAll(p.Spec.Layout.LogDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{engine + "-error.log", engine + "-access.log"} {
		if err := os.WriteFile(filepath.Join(p.Spec.Layout.LogDir, name), nil, 0o640); err != nil {
			t.Fatal(err)
		}
	}
}

// real remplace l'exécuteur simulé : le mode simulé retient tous les chemins
// sans les vérifier, ce qui masquerait le filtrage éprouvé ici.
func real(p *Provider) { p.Exec = &exec.Local{} }

func TestPrisonSansJournalNestPasEcrite(t *testing.T) {
	p, _ := fixture(t)
	real(p)
	proj := project(t, "shop.fr", model.KindSymfony, "8.3")

	data := p.BuildFail2ban([]*model.Project{proj})
	for _, jail := range data.Jails {
		if strings.HasPrefix(jail.Name, "ogs-web") {
			t.Fatalf("prison %s écrite alors qu'aucun journal n'existe : fail2ban "+
				"refuserait de démarrer, et toutes les prisons tomberaient avec elle", jail.Name)
		}
	}
}

func TestPrisonEnumereLesJournauxExistants(t *testing.T) {
	p, _ := fixture(t)
	real(p)
	proj := project(t, "shop.fr", model.KindSymfony, "8.3")
	withLogs(t, proj, "nginx")

	data := p.BuildFail2ban([]*model.Project{proj})
	var web *Jail
	for i := range data.Jails {
		if data.Jails[i].Name == "ogs-web-auth" {
			web = &data.Jails[i]
		}
	}
	if web == nil {
		t.Fatal("la prison ogs-web-auth est attendue dès qu'un journal d'erreurs existe")
	}
	want := filepath.Join(proj.Spec.Layout.LogDir, "nginx-error.log")
	if len(web.LogPaths) != 1 || web.LogPaths[0] != want {
		t.Errorf("journaux surveillés = %v, attendu [%s]", web.LogPaths, want)
	}
	if web.Filter != "nginx-http-auth" {
		t.Errorf("filtre = %q, attendu celui du moteur en service", web.Filter)
	}
}

func TestJournauxJamaisEnumeresParJoker(t *testing.T) {
	p, _ := fixture(t)
	proj := project(t, "shop.fr", model.KindSymfony, "8.3")
	withLogs(t, proj, "nginx")

	step, err := p.planFail2ban([]*model.Project{proj})
	if err != nil {
		t.Fatal(err)
	}
	if step.Diff == nil {
		t.Fatal("le fichier de prisons doit être neuf, donc porter un diff")
	}
	// Un joker sur la racine des homes embarquerait les répertoires d'utilisateurs
	// humains : fail2ban lirait des fichiers qu'un compte non géré contrôle.
	data := p.BuildFail2ban([]*model.Project{proj})
	for _, jail := range data.Jails {
		for _, path := range jail.LogPaths {
			if strings.Contains(path, "*") {
				t.Errorf("prison %s : chemin à joker %q", jail.Name, path)
			}
		}
	}
}

func TestBoucleLocaleToujoursIgnoree(t *testing.T) {
	p, _ := fixture(t)
	p.Cfg.Monitoring.Fail2ban.IgnoreIP = []string{"203.0.113.4"}
	data := p.BuildFail2ban(nil)
	found := map[string]bool{}
	for _, ip := range data.IgnoreIP {
		found[ip] = true
	}
	for _, want := range []string{"127.0.0.1/8", "::1", "203.0.113.4"} {
		if !found[want] {
			t.Errorf("%s absent des adresses ignorées : se bannir soi-même couperait "+
				"la supervision et les contrôles de disponibilité", want)
		}
	}
}

func TestPrisonDesactiveeNestPasEcrite(t *testing.T) {
	p, _ := fixture(t)
	p.Cfg.Monitoring.Fail2ban.Jails = map[string]bool{"sshd": false}
	data := p.BuildFail2ban(nil)
	for _, jail := range data.Jails {
		if jail.Name == "ogs-sshd" {
			t.Error("une prison désactivée dans la configuration ne doit pas être écrite")
		}
	}
}

func TestPrisonsPrefixeesPourNePasRecouvrirCellesDeLadministrateur(t *testing.T) {
	p, _ := fixture(t)
	data := p.BuildFail2ban(nil)
	for _, jail := range data.Jails {
		if !strings.HasPrefix(jail.Name, "ogs-") {
			t.Errorf("prison %q sans préfixe : jail.d est partagé avec l'administrateur, "+
				"et une prison homonyme recouvrirait la sienne en silence", jail.Name)
		}
	}
}

func TestFragmentTelegrafPorteLesPoolsEtLesProjets(t *testing.T) {
	p, _ := fixture(t)
	shop := project(t, "shop.fr", model.KindSymfony, "8.3")
	data, err := p.BuildTelegraf([]*model.Project{shop}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Pools) != 1 {
		t.Fatalf("un pool attendu, %d obtenu(s)", len(data.Pools))
	}
	if !strings.HasPrefix(data.Pools[0].URL, "http://127.0.0.1:8099/ogs-status/fpm/") {
		t.Errorf("le statut d'un pool passe par le vhost de supervision, obtenu %q", data.Pools[0].URL)
	}
	if len(data.Procs) != 1 || data.Procs[0].User != shop.Spec.User.Name {
		t.Errorf("les processus sont comptés par utilisateur de projet, obtenu %+v", data.Procs)
	}
}

func TestFragmentTelegrafNexposeAucunSocketFPM(t *testing.T) {
	p, _ := fixture(t)
	shop := project(t, "shop.fr", model.KindSymfony, "8.3")
	obs := Observed{TelegrafPresent: true, TelegrafGroup: -1}
	step, err := p.planTelegraf([]*model.Project{shop}, Credentials{}, obs)
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(p.Paths.TelegrafFile())
	if err != nil {
		t.Fatal(err)
	}
	// Parler au socket d'un pool, c'est pouvoir y faire exécuter du code sous
	// l'identité du projet : l'agent ne doit jamais l'atteindre directement. Le
	// socket d'un moteur de bases, lui, y figure légitimement — l'agent s'y
	// authentifie —, d'où un contrôle porté sur le répertoire des sockets FPM.
	if strings.Contains(string(content), p.Paths.PHPRunDir()) {
		t.Errorf("le fragment référence un socket FPM :\n%s", content)
	}
}

func TestFragmentTelegrafEcritEn0640(t *testing.T) {
	p, _ := fixture(t)
	obs := Observed{TelegrafPresent: true, TelegrafGroup: -1}
	step, err := p.planTelegraf(nil, Credentials{
		MySQL: &MySQLAccess{User: "telegraf", Password: "s3cr3t", Socket: "/run/mysqld/mysqld.sock"},
	}, obs)
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p.Paths.TelegrafFile())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Errorf("mode %04o : le fragment porte les mots de passe de supervision, 0640 attendu", perm)
	}
}

func TestSecretDeParcNeCollisionnePasAvecUnProjet(t *testing.T) {
	// Le scope des secrets de parc doit être impossible à porter par un projet,
	// sans quoi un projet nommé ainsi lirait — et réécrirait — les mots de passe
	// de supervision.
	if err := naming.ValidateIdentifier(SecretScope); err == nil {
		t.Fatalf("%q est un identifiant de projet valide : collision possible", SecretScope)
	}
}

func TestSecretDeSupervisionNestPasRegenere(t *testing.T) {
	p, _ := fixture(t)
	first, err := p.ensureSecret("mysql/telegraf")
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.ensureSecret("mysql/telegraf")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("un mot de passe scellé ne doit jamais être régénéré : l'agent " +
			"resterait avec l'ancien, sans que rien ne le dise")
	}
	if len(first) != secret.DefaultPasswordLength {
		t.Errorf("longueur %d, attendu %d", len(first), secret.DefaultPasswordLength)
	}
}

func TestFichiersIdempotents(t *testing.T) {
	p, _ := fixture(t)
	shop := project(t, "shop.fr", model.KindSymfony, "8.3")
	withLogs(t, shop, "nginx")
	projects := []*model.Project{shop}
	obs := Observed{TelegrafPresent: true, Fail2banPresent: true, TelegrafGroup: -1}

	steps, err := p.Plan(context.Background(), projects, obs)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Apply != nil {
			if err := s.Apply(context.Background()); err != nil {
				t.Fatalf("%s : %v", s.Resource, err)
			}
		}
	}
	// Second passage : l'empreinte du corps est comparée, non les octets — l'en-tête
	// porte un horodatage, et comparer les octets ferait passer tout fichier « en
	// écart » la seconde suivant son écriture.
	again, err := p.Plan(context.Background(), projects, obs)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range again {
		if s.Resource == "monitoring/telegraf" || s.Resource == "monitoring/fail2ban" {
			if s.Apply != nil {
				t.Errorf("%s : second plan non vide, le fichier n'est pas idempotent", s.Resource)
			}
		}
	}
}
