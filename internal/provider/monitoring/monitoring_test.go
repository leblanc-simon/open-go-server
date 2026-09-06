package monitoring

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/provider/amqp"
	"leblanc.io/open-go-server/internal/provider/db"
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
		// L'invariant porte sur toutes les prisons, non sur un préfixe de nom : seule
		// la prison sshd a le droit de n'énumérer aucun journal, et elle bascule alors
		// sur le backend systemd. Une prison en backend « auto » sans journal serait
		// écrite sans « logpath » — muette, et invisible à « doctor », qui n'aurait
		// aucun chemin à contrôler.
		if jail.Backend == "auto" && len(jail.LogPaths) == 0 {
			t.Fatalf("prison %s écrite alors qu'aucun journal n'existe : elle ne "+
				"surveille rien, et sa protection est annoncée sans exister", jail.Name)
		}
	}
}

func TestPrisonDeRecidiveExigeLeJournalDeFail2ban(t *testing.T) {
	// La récidive lit le journal de fail2ban lui-même, qui peut manquer
	// durablement : « logtarget » n'est pas toujours un fichier, et un démon
	// jamais démarré n'en a écrit aucun.
	p, paths := fixture(t)
	real(p)
	proj := project(t, "shop.fr", model.KindSymfony, "8.3")
	withLogs(t, proj, "nginx")

	data := p.BuildFail2ban([]*model.Project{proj})
	for _, jail := range data.Jails {
		if jail.Name == "ogs-recidive" {
			t.Fatal("prison de récidive écrite sans son journal : elle ne bannit personne")
		}
	}

	if err := os.MkdirAll(filepath.Dir(paths.Fail2banLog()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Fail2banLog(), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	data = p.BuildFail2ban([]*model.Project{proj})
	found := false
	for _, jail := range data.Jails {
		if jail.Name == "ogs-recidive" {
			found = true
		}
	}
	if !found {
		t.Error("la prison de récidive est attendue dès que son journal existe")
	}
}

func TestLeFichierDePrisonsNecritAucuneSectionCommune(t *testing.T) {
	// Une section [DEFAULT] posée dans jail.d n'est pas locale à son fichier :
	// fail2ban fusionne tous les fichiers dans un même analyseur, et elle
	// imposerait bantime, findtime et surtout ignoreip à toutes les prisons de la
	// machine — la liste blanche d'administration serait remplacée, pas complétée.
	p, _ := fixture(t)
	p.Cfg.Monitoring.Fail2ban.IgnoreIP = []string{"203.0.113.4"}
	proj := project(t, "shop.fr", model.KindSymfony, "8.3")
	withLogs(t, proj, "nginx")

	step, err := p.planFail2ban([]*model.Project{proj})
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(p.Paths.Fail2banJailFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "[DEFAULT]") {
		t.Errorf("section commune écrite dans jail.d :\n%s", content)
	}
	// Les réglages ne disparaissent pas pour autant : ils sont portés par chaque
	// prison, où ils ne débordent sur personne.
	for _, want := range []string{"ignoreip = 127.0.0.1/8 ::1 203.0.113.4", "bantime  = 1h"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("%q absent du fichier :\n%s", want, content)
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
	first, sealed, err := p.lookupSecret("mysql/telegraf")
	if err != nil {
		t.Fatal(err)
	}
	if sealed {
		t.Error("un secret jamais scellé ne doit pas être annoncé comme tel : " +
			"le moteur ne l'a pas reçu, le compte doit être repassé")
	}
	if err := p.sealSecret("mysql/telegraf", first); err != nil {
		t.Fatal(err)
	}
	second, sealed, err := p.lookupSecret("mysql/telegraf")
	if err != nil {
		t.Fatal(err)
	}
	if !sealed || first != second {
		t.Error("un mot de passe scellé ne doit jamais être régénéré : l'agent " +
			"resterait avec l'ancien, sans que rien ne le dise")
	}
	if len(first) != secret.DefaultPasswordLength {
		t.Errorf("longueur %d, attendu %d", len(first), secret.DefaultPasswordLength)
	}
}

// fakeBroker répond aux seuls appels que la planification du compte de
// supervision fait. L'interface est incorporée sans valeur : tout autre appel
// mettrait le test en échec plutôt que de passer inaperçu.
type fakeBroker struct {
	amqp.Client
	users   []amqp.User
	created map[string]string
	// refuse fait échouer la pose du compte, comme un broker qui refuse le mot de
	// passe ou une API momentanément fermée.
	refuse error
}

func (b *fakeBroker) Overview(context.Context) (string, error) { return "3.13.7", nil }
func (b *fakeBroker) Users(context.Context) ([]amqp.User, error) {
	return b.users, nil
}
func (b *fakeBroker) EnsureUser(_ context.Context, name, password string, _ []string) error {
	if b.refuse != nil {
		return b.refuse
	}
	if b.created == nil {
		b.created = map[string]string{}
	}
	b.created[name] = password
	return nil
}

func TestPlanNeScellePasDeSecretDeParc(t *testing.T) {
	// « ogs plan » annonce ne rien appliquer : il ne doit pas laisser derrière lui
	// un mot de passe de parc, non journalisé et non compensable, que seul
	// « ogs secrets show @parc » retrouverait — y compris quand l'exploitant
	// renonce à l'apply.
	p, _ := fixture(t)
	broker := &fakeBroker{}
	p.AMQP = &amqp.Provider{Client: broker, ManagementURL: amqp.DefaultManagementURL}

	step, access := p.planRabbitAccount(context.Background())
	if step == nil || access == nil {
		t.Fatal("une étape et des identifiants sont attendus")
	}
	names, err := p.Store.SecretNames(SecretScope)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Fatalf("le calcul du plan a écrit dans le keystore : %v", names)
	}

	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if names, err = p.Store.SecretNames(SecretScope); err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "rabbitmq/"+p.Cfg.Monitoring.TelegrafAccount() {
		t.Errorf("le secret doit être scellé par l'application : %v", names)
	}
	// Et c'est bien celui que le broker a reçu, sans quoi le fragment porterait un
	// mot de passe que le broker refuserait.
	if broker.created[p.Cfg.Monitoring.TelegrafAccount()] != access.Password {
		t.Error("le mot de passe scellé et celui posé sur le broker diffèrent")
	}
}

func TestCompteDeSupervisionPreexistantRecoitSonMotDePasse(t *testing.T) {
	// Un compte créé par l'administrateur — ou une base d'état restaurée sans ses
	// secrets — était jugé conforme sur son seul tag : le fragment partait avec un
	// mot de passe que le broker ignore, et l'agent échouait à chaque collecte
	// sans que rien — ni le plan, ni l'apply, ni doctor — ne le dise.
	p, _ := fixture(t)
	account := p.Cfg.Monitoring.TelegrafAccount()
	broker := &fakeBroker{users: []amqp.User{{Name: account, Tags: "monitoring"}}}
	p.AMQP = &amqp.Provider{Client: broker, ManagementURL: amqp.DefaultManagementURL}

	step, _ := p.planRabbitAccount(context.Background())
	if step == nil || step.Apply == nil {
		t.Fatal("le compte doit être repassé tant que son mot de passe n'est pas scellé")
	}
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Une fois posé et scellé, le compte est conforme : le plan ne doit plus rien
	// annoncer.
	again, _ := p.planRabbitAccount(context.Background())
	if again == nil || again.Apply != nil {
		t.Errorf("second plan non vide : le compte n'est pas idempotent (%+v)", again)
	}
}

func TestPrivilegesDeSupervisionCherchesLaOuIlsSont(t *testing.T) {
	// PROCESS et REPLICATION CLIENT sont accordés « ON *.* » : ils vivent dans
	// mysql.user, jamais dans SCHEMA_PRIVILEGES. Les chercher pêle-mêle avec les
	// privilèges par base laissait un SELECT accordé sur la base d'un projet tenir
	// lieu de conformité — et ne voyait jamais les deux privilèges attendus.
	cases := []struct {
		nom      string
		observed map[string]string
		want     bool
	}{
		{"compte conforme (MySQL)", map[string]string{
			db.GlobalGrantKey: "PROCESS,REPLICATION CLIENT", "performance_schema": "SELECT"}, true},
		{"compte conforme (MariaDB)", map[string]string{
			db.GlobalGrantKey: "BINLOG MONITOR,PROCESS", "performance_schema": "SELECT"}, true},
		{"privilèges globaux absents", map[string]string{
			"performance_schema": "SELECT"}, false},
		{"performance_schema absent", map[string]string{
			db.GlobalGrantKey: "PROCESS,REPLICATION CLIENT"}, false},
		{"SELECT sur la base d'un projet", map[string]string{
			"shopfr": "PROCESS,REPLICATION CLIENT,SELECT"}, false},
	}
	for _, c := range cases {
		if got := mysqlGrantsConform(c.observed); got != c.want {
			t.Errorf("%s : conformité %v, attendu %v", c.nom, got, c.want)
		}
	}
}

func TestOutilDesactiveRetireSonFichier(t *testing.T) {
	// Le premier réflexe d'un administrateur dont fail2ban ne démarre plus est de
	// poser « enabled: false ». Le fichier laissé en place resterait chargé, avec
	// ses prisons et ses journaux, et ne serait plus jamais recalculé — pas même à
	// la suppression d'un projet, ce que D68 interdit.
	p, _ := fixture(t)
	shop := project(t, "shop.fr", model.KindSymfony, "8.3")
	withLogs(t, shop, "nginx")
	projects := []*model.Project{shop}

	on := Observed{TelegrafPresent: true, Fail2banPresent: true, TelegrafGroup: -1}
	steps, err := p.Plan(context.Background(), projects, on)
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

	off := Observed{TelegrafGroup: -1}
	steps, err = p.Plan(context.Background(), projects, off)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("deux retraits attendus, %d étape(s) : %+v", len(steps), steps)
	}
	for _, s := range steps {
		if s.Critical {
			t.Errorf("%s : le retrait ne doit pas faire échouer un déploiement", s.Resource)
		}
		if err := s.Apply(context.Background()); err != nil {
			t.Fatalf("%s : %v", s.Resource, err)
		}
	}
	for _, path := range []string{p.Paths.TelegrafFile(), p.Paths.Fail2banJailFile()} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s subsiste alors que l'outil est désactivé", path)
		}
	}
	// Rien à retirer une seconde fois : le plan doit redevenir vide.
	steps, err = p.Plan(context.Background(), projects, off)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Errorf("plan non vide sur un outil désactivé sans fichier : %+v", steps)
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

// appliquer déroule un plan comme le moteur : une étape non critique en échec
// laisse l'exécution continuer, ce qui est tout le sujet ici — la supervision ne
// fait jamais échouer un déploiement.
func appliquer(t *testing.T, p *Provider, projects []*model.Project, obs Observed) []*engine.Step {
	t.Helper()
	steps, err := p.Plan(context.Background(), projects, obs)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Apply == nil {
			continue
		}
		if err := s.Apply(context.Background()); err != nil && s.Critical {
			t.Fatalf("%s : %v", s.Resource, err)
		}
	}
	return steps
}

// etape retrouve une étape par sa ressource.
func etape(t *testing.T, steps []*engine.Step, resource string) *engine.Step {
	t.Helper()
	for _, s := range steps {
		if s.Resource == resource {
			return s
		}
	}
	t.Fatalf("étape %s absente du plan : %+v", resource, steps)
	return nil
}

func TestFragmentTelegrafNePorteAucunMotDePasseQueLeMoteurIgnore(t *testing.T) {
	// Le compte n'a pas pu être posé : le mot de passe calculé n'a jamais atteint
	// le broker. L'écrire dans le fragment donnerait à l'agent un identifiant
	// refusé — et, un secret non scellé étant tiré au hasard à chaque
	// planification, le fichier serait réécrit à chaque apply, l'agent relancé à
	// chaque fois, et « diff » resterait en écart pour toujours.
	p, _ := fixture(t)
	broker := &fakeBroker{refuse: errors.New("api d'administration fermée")}
	p.AMQP = &amqp.Provider{Client: broker, ManagementURL: amqp.DefaultManagementURL}
	obs := Observed{TelegrafPresent: true, TelegrafGroup: -1}

	appliquer(t, p, nil, obs)
	content, err := os.ReadFile(p.Paths.TelegrafFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "[[inputs.rabbitmq]]") {
		t.Errorf("le fragment porte une entrée dont le broker ignore le mot de passe :\n%s", content)
	}
	// Et le plan converge : sans quoi le fragment oscillerait indéfiniment.
	steps, err := p.Plan(context.Background(), nil, obs)
	if err != nil {
		t.Fatal(err)
	}
	if s := etape(t, steps, "monitoring/telegraf"); s.Apply != nil {
		diff, _ := s.Diff()
		t.Errorf("second plan non vide alors que rien n'a changé :\n%s", diff)
	}
}

func TestFragmentTelegrafPorteLeMotDePasseQueLeBrokerARecu(t *testing.T) {
	// Le corps est recalculé à l'application, après l'étape du compte : c'est là
	// que le mot de passe devient scellé. Sans ce second calcul, l'entrée
	// n'apparaîtrait qu'à l'apply suivant.
	p, _ := fixture(t)
	broker := &fakeBroker{}
	p.AMQP = &amqp.Provider{Client: broker, ManagementURL: amqp.DefaultManagementURL}
	obs := Observed{TelegrafPresent: true, TelegrafGroup: -1}

	appliquer(t, p, nil, obs)
	content, err := os.ReadFile(p.Paths.TelegrafFile())
	if err != nil {
		t.Fatal(err)
	}
	pose := broker.created[p.Cfg.Monitoring.TelegrafAccount()]
	if pose == "" {
		t.Fatal("le compte n'a pas été posé sur le broker")
	}
	if !strings.Contains(string(content), "password = "+strconv.Quote(pose)) {
		t.Errorf("le fragment ne porte pas le mot de passe posé sur le broker :\n%s", content)
	}
	steps, err := p.Plan(context.Background(), nil, obs)
	if err != nil {
		t.Fatal(err)
	}
	if s := etape(t, steps, "monitoring/telegraf"); s.Apply != nil {
		diff, _ := s.Diff()
		t.Errorf("second plan non vide : le fragment n'est pas idempotent\n%s", diff)
	}
}

func TestPrisonsRetoucheesALaMainNeSontPasEcrasees(t *testing.T) {
	// §6.4 : un fichier généré que l'exploitant a retouché n'est pas réécrit sans
	// qu'on le lui dise. Le fichier de prisons est celui qu'il a le plus de raisons
	// d'ouvrir, et il est recalculé en entier à chaque apply : sans refus, sa ligne
	// disparaîtrait sans un mot et sans copie.
	p, _ := fixture(t)
	shop := project(t, "shop.fr", model.KindSymfony, "8.3")
	withLogs(t, shop, "nginx")
	projects := []*model.Project{shop}
	obs := Observed{Fail2banPresent: true, TelegrafGroup: -1}

	appliquer(t, p, projects, obs)
	path := p.Paths.Fail2banJailFile()
	genere, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	retouche := append(append([]byte{}, genere...), "ignoreip = 10.0.0.0/8\n"...)
	if err := os.WriteFile(path, retouche, 0o644); err != nil {
		t.Fatal(err)
	}

	steps, err := p.Plan(context.Background(), projects, obs)
	if err != nil {
		t.Fatal(err)
	}
	s := etape(t, steps, "monitoring/fail2ban")
	if s.Apply == nil {
		t.Fatal("aucune étape : le fichier retouché passerait pour conforme")
	}
	err = s.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("l'écrasement doit être refusé et dire comment passer outre : %v", err)
	}
	if s.Critical {
		t.Error("le refus ne doit pas faire compenser le déploiement d'un site")
	}
	if data, _ := os.ReadFile(path); string(data) != string(retouche) {
		t.Error("le fichier retouché a été réécrit malgré le refus")
	}

	// --force : l'écrasement est permis, mais l'original part en sauvegarde.
	p.Force = true
	appliquer(t, p, projects, obs)
	if data, _ := os.ReadFile(path); string(data) == string(retouche) {
		t.Error("--force n'a pas réécrit le fichier")
	}
	copies, err := filepath.Glob(filepath.Join(p.Paths.Lib(), "backups",
		filepath.Base(path)+".ogs-bak-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 1 {
		t.Fatalf("une sauvegarde horodatée attendue hors du répertoire des prisons : %v", copies)
	}
	if data, _ := os.ReadFile(copies[0]); string(data) != string(retouche) {
		t.Error("la sauvegarde ne porte pas le fichier de l'exploitant")
	}
}
