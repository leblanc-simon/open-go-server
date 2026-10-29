package db

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
)

// limitesMySQL complète le faux moteur de la relecture des limites : chaque
// compte relu rend limite. Elle passe avant « mysql.user », que la requête
// contient aussi.
func limitesMySQL(m *fauxMoteur, limite int64) {
	m.réponses = append([]fauxRéponse{
		{"max_user_connections", []string{"n"}, [][]driver.Value{{limite}}},
	}, m.réponses...)
}

func exécutéesAvec(m *fauxMoteur, fragment string) []string {
	var out []string
	for _, q := range m.exécutées {
		if strings.Contains(q, fragment) {
			out = append(out, q)
		}
	}
	return out
}

func TestLeBudgetDeConnexionsSuitLaDeclaration(t *testing.T) {
	_, proj := fixture(t)
	// Un worker PHP, une instance de service et une tâche active ouvrent chacun
	// une connexion ; la moitié en sus, au moins 10.
	if got := ConnectionBudget(proj, 12); got != 22 {
		t.Errorf("12 workers, rien d'autre : 22 attendu, obtenu %d", got)
	}
	off := false
	proj.Spec.Services = []model.Service{{Name: "messenger", Instances: 4}, {Name: "arrêté", Enabled: &off}}
	proj.Spec.Crons = []model.CronJob{{Name: "a"}, {Name: "b"}, {Name: "c", Enabled: &off}}
	if got := ConnectionBudget(proj, 20); got != 26+13 {
		t.Errorf("20 workers, 4 instances, 2 tâches : 39 attendu, obtenu %d", got)
	}
	node := model.New("api.fr", "prod", model.KindNode)
	node.Spec.App.Instances = 2
	if got := ConnectionBudget(node, 0); got != 30 {
		t.Errorf("deux instances node (un pool chacune) : 30 attendu, obtenu %d", got)
	}
}

func TestUnCompteNommeDeuxFoisRecoitLaLimiteLaPlusLarge(t *testing.T) {
	// Principal d'une base, additionnel d'une autre : deux limites feraient
	// osciller le plan.
	_, proj := fixture(t)
	proj.Spec.Databases = []model.Database{
		{Engine: "mysql", Name: "a", User: "shopfr", MaxConnections: 40},
		{Engine: "mariadb", Name: "b", User: "autre", ExtraUsers: []model.DBUser{{User: "shopfr"}, {User: "lecteur"}}},
		{Engine: "postgres", Name: "c", User: "shopfr", MaxConnections: NoConnectionLimit},
	}
	limits := accountLimits(proj, 0)
	if got := limits["mysql|shopfr@localhost"]; got != 40 {
		t.Errorf("40 attendu (le plus large des deux), obtenu %d", got)
	}
	if got := limits["mysql|lecteur@localhost"]; got != ExtraUserConnections {
		t.Errorf("compte additionnel : %d attendu, obtenu %d", ExtraUserConnections, got)
	}
	if got := limits["postgres||shopfr"]; got != NoConnectionLimit {
		t.Errorf("le rôle PostgreSQL est un autre compte : sans limite attendu, obtenu %d", got)
	}
	proj.Spec.Databases[1].ExtraUsers[0].MaxConnections = NoConnectionLimit
	if got := accountLimits(proj, 0)["mysql|shopfr@localhost"]; got != NoConnectionLimit {
		t.Errorf("l'absence de limite l'emporte, obtenu %d", got)
	}
}

func TestUnCompteCreeRecoitSaLimite(t *testing.T) {
	prov, proj := projetMySQL(t, "8.3")
	prov.PHPWorkers = func(*model.Project) int { return 12 }
	proj.Spec.Databases[0].ExtraUsers = []model.DBUser{{User: "lecteur", Privileges: "readonly"}}
	client, m := fauxMySQL(t, nil, nil, nil)
	prov.clients = map[string]Client{"mysql": client}
	steps := planifier(t, prov, proj)
	d := décrire(t, steps[0])
	user := proj.Spec.Databases[0].User
	for _, want := range []string{
		"ALTER USER '" + user + "'@'localhost' WITH MAX_USER_CONNECTIONS 22",
		"ALTER USER 'lecteur'@'localhost' WITH MAX_USER_CONNECTIONS 10",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("%q attendu :\n%s", want, d)
		}
	}
	if err := steps[0].Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := exécutéesAvec(m, "MAX_USER_CONNECTIONS"); len(got) != 2 {
		t.Errorf("deux limites posées attendues : %v", m.exécutées)
	}
}

func TestUneLimiteEcarteeEstReposee(t *testing.T) {
	prov, proj := projetMySQL(t, "8.3")
	prov.PHPWorkers = func(*model.Project) int { return 12 }
	decl := proj.Spec.Databases[0]
	if _, err := prov.ensurePassword(proj.ID(), "mysql", decl.User); err != nil {
		t.Fatal(err)
	}
	client, m := fauxMySQL(t, []string{decl.Name}, []string{decl.User + "@localhost"},
		[][2]string{{decl.Name, tousDroits}})
	limitesMySQL(m, 0)
	prov.clients = map[string]Client{"mysql": client}
	steps := planifier(t, prov, proj)
	if steps[0].Action == engine.NoOp || !strings.Contains(steps[0].Summary, "sans limite → 22") {
		t.Fatalf("la limite doit être annoncée : %s", steps[0].Summary)
	}
	if err := steps[0].Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := exécutéesAvec(m, "WITH MAX_USER_CONNECTIONS 22"); len(got) != 1 {
		t.Errorf("la limite doit être posée : %v", m.exécutées)
	}
	m.réponses[0].lignes = [][]driver.Value{{int64(22)}}
	if steps := planifier(t, prov, proj); steps[0].Action != engine.NoOp {
		t.Errorf("limite posée, rien à faire : %s", steps[0].Summary)
	}
	// La surcharge de la déclaration l'emporte sur le calcul ; -1 s'écrit 0.
	proj.Spec.Databases[0].MaxConnections = NoConnectionLimit
	if d := décrire(t, planifier(t, prov, proj)[0]); !strings.Contains(d, "WITH MAX_USER_CONNECTIONS 0") {
		t.Errorf("aucune limite s'écrit 0 sous MySQL :\n%s", d)
	}
}

func TestUnCompteAdopteRecoitSaLimiteSansMotDePasse(t *testing.T) {
	// Adopté, son mot de passe inconnu d'ogs : la limite se pose quand même,
	// ALTER USER … WITH MAX_USER_CONNECTIONS n'en exige aucun.
	prov, proj := projetMySQL(t, "8.3")
	adopter(proj)
	decl := proj.Spec.Databases[0]
	client, m := fauxMySQL(t, []string{decl.Name}, []string{decl.User + "@localhost"},
		[][2]string{{decl.Name, tousDroits}})
	limitesMySQL(m, 0)
	prov.clients = map[string]Client{"mysql": client}
	steps := planifier(t, prov, proj)
	if steps[0].Action == engine.NoOp {
		t.Fatalf("la limite d'un compte adopté doit être posée : %s", steps[0].Summary)
	}
	if err := steps[0].Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(exécutéesAvec(m, "MAX_USER_CONNECTIONS")) != 1 {
		t.Errorf("limite attendue : %v", m.exécutées)
	}
	if got := exécutéesAvec(m, "IDENTIFIED"); len(got) != 0 {
		t.Errorf("aucun mot de passe posé sur un compte adopté : %v", got)
	}
}

func TestUneLimiteIllisibleNeFondeAucuneCorrection(t *testing.T) {
	// Une lecture en échec n'est pas une absence (Q-20) : le faux moteur rend des
	// noms de comptes à la relecture de la limite, qui échoue.
	prov, proj := projetMySQL(t, "8.3")
	decl := proj.Spec.Databases[0]
	if _, err := prov.ensurePassword(proj.ID(), "mysql", decl.User); err != nil {
		t.Fatal(err)
	}
	client, _ := fauxMySQL(t, []string{decl.Name}, []string{decl.User + "@localhost"},
		[][2]string{{decl.Name, tousDroits}})
	prov.clients = map[string]Client{"mysql": client}
	if steps := planifier(t, prov, proj); steps[0].Action != engine.NoOp {
		t.Errorf("rien à faire sans relecture : %s", steps[0].Summary)
	}
}

func TestLimiteSousPostgreSQL(t *testing.T) {
	stmt, err := ConnectionLimitStatement("postgres", "shopfr", "", engineLimit("postgres", NoConnectionLimit))
	if err != nil || stmt != `ALTER ROLE "shopfr" CONNECTION LIMIT -1` {
		t.Errorf("obtenu %q, %v", stmt, err)
	}
	if describeLimit("postgres", 0) != "0" || describeLimit("mysql", 0) != "sans limite" {
		t.Error("0 vaut aucune limite sous MySQL, aucune connexion sous PostgreSQL")
	}
	if _, err := ConnectionLimitStatement("mysql", "x'y", "localhost", 5); err == nil {
		t.Error("un nom hors de la liste blanche doit être refusé")
	}
}
