package db

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
)

// pluginsMySQL complète le faux moteur des lectures de plugin : celui du compte,
// et l'état de mysql_native_password. Elles passent avant « mysql.user », que la
// lecture du plugin contient aussi.
func pluginsMySQL(m *fauxMoteur, compte, natif string) {
	var statut [][]driver.Value
	if natif != "" {
		statut = [][]driver.Value{{natif}}
	}
	var plugin [][]driver.Value
	if compte != "" {
		plugin = [][]driver.Value{{compte}}
	}
	m.réponses = append([]fauxRéponse{
		{"SELECT plugin", []string{"p"}, plugin},
		{"information_schema.PLUGINS", []string{"s"}, statut},
	}, m.réponses...)
}

func projetMySQL(t *testing.T, php string) (*Provider, *model.Project) {
	t.Helper()
	prov, proj := fixture(t)
	proj.Spec.PHP.Pools[0].Version = php
	proj.Spec.Databases = []model.Database{DefaultDatabase(proj, "mysql")}
	return prov, proj
}

func TestPHP71CreeSonCompteEnMysqlNativePassword(t *testing.T) {
	// mysqlnd de PHP 7.1 et 7.2 ne s'authentifie pas par caching_sha2_password,
	// le plugin par défaut de MySQL 8 (D126).
	for php, want := range map[string]bool{"7.1": true, "7.2": true, "7.3": false, "8.3": false} {
		prov, proj := projetMySQL(t, php)
		client, m := fauxMySQL(t, nil, nil, nil)
		pluginsMySQL(m, "", "ACTIVE")
		prov.clients = map[string]Client{"mysql": client}
		d := décrire(t, planifier(t, prov, proj)[0])
		if got := strings.Contains(d, "IDENTIFIED WITH mysql_native_password BY"); got != want {
			t.Errorf("PHP %s : plugin natif = %v, attendu %v :\n%s", php, got, want, d)
		}
	}
}

func TestUneTacheEnPHP72SuffitADemanderLePluginNatif(t *testing.T) {
	prov, proj := projetMySQL(t, "8.3")
	proj.Spec.Crons = []model.CronJob{{Name: "vieux", Schedule: "* * * * *", Command: "x.php", PHP: "7.2"}}
	client, m := fauxMySQL(t, nil, nil, nil)
	pluginsMySQL(m, "", "ACTIVE")
	prov.clients = map[string]Client{"mysql": client}
	if d := décrire(t, planifier(t, prov, proj)[0]); !strings.Contains(d, "IDENTIFIED WITH mysql_native_password") {
		t.Errorf("la tâche se connecte à la même base :\n%s", d)
	}
}

func TestMariaDBGardeSonPluginParDefaut(t *testing.T) {
	// MariaDB authentifie déjà par mysql_native_password, et sa grammaire
	// diffère : rien n'est imposé.
	prov, proj := projetMySQL(t, "7.1")
	client, m := fauxMySQL(t, nil, nil, nil)
	m.réponses[0].lignes = [][]driver.Value{{"11.8.3-MariaDB-0+deb13u1"}}
	prov.clients = map[string]Client{"mysql": client}
	if d := décrire(t, planifier(t, prov, proj)[0]); strings.Contains(d, "IDENTIFIED WITH") {
		t.Errorf("aucun plugin sous MariaDB :\n%s", d)
	}
}

func TestUnCompteExistantPasseAuPluginNatif(t *testing.T) {
	// Un projet passé de PHP 8 à 7.1 : « CREATE USER IF NOT EXISTS » ne touche
	// pas le compte en place, qui garde caching_sha2_password.
	prov, proj := projetMySQL(t, "7.1")
	decl := proj.Spec.Databases[0]
	if _, err := prov.ensurePassword(proj.ID(), "mysql", decl.User); err != nil {
		t.Fatal(err)
	}
	client, m := fauxMySQL(t, []string{decl.Name}, []string{decl.User + "@localhost"},
		[][2]string{{decl.Name, tousDroits}})
	pluginsMySQL(m, "caching_sha2_password", "ACTIVE")
	prov.clients = map[string]Client{"mysql": client}
	steps := planifier(t, prov, proj)
	if steps[0].Action == engine.NoOp || !strings.Contains(steps[0].Summary, "caching_sha2_password → mysql_native_password") {
		t.Fatalf("le changement de plugin doit être annoncé : %s", steps[0].Summary)
	}
	if err := steps[0].Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	alter := ""
	for _, q := range m.exécutées {
		if strings.HasPrefix(q, "ALTER USER") {
			alter = q
		}
	}
	if !strings.Contains(alter, "IDENTIFIED WITH mysql_native_password BY") {
		t.Errorf("le plugin doit être reposé avec le mot de passe scellé : %v", m.exécutées)
	}
	m.réponses[0].lignes = [][]driver.Value{{"mysql_native_password"}}
	if steps := planifier(t, prov, proj); steps[0].Action != engine.NoOp {
		t.Errorf("le plugin posé, rien à faire : %s", steps[0].Summary)
	}
}

func TestUnCompteSansMotDePasseConnuNEstPasModifie(t *testing.T) {
	// Un compte adopté dont ogs ne tient pas le mot de passe : changer de plugin
	// exige de le reposer, ce qui couperait l'application.
	prov, proj := projetMySQL(t, "7.1")
	adopter(proj)
	decl := proj.Spec.Databases[0]
	client, m := fauxMySQL(t, []string{decl.Name}, []string{decl.User + "@localhost"},
		[][2]string{{decl.Name, tousDroits}})
	pluginsMySQL(m, "caching_sha2_password", "ACTIVE")
	prov.clients = map[string]Client{"mysql": client}
	steps := planifier(t, prov, proj)
	var avis *engine.Step
	for _, s := range steps {
		if strings.HasSuffix(s.Resource, "/auth") {
			avis = s
		}
	}
	if avis == nil || avis.Critical {
		t.Fatalf("un avis non critique est attendu : %v", steps)
	}
	if err := avis.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "ALTER USER") {
		t.Errorf("l'avis doit nommer la commande à passer : %v", err)
	}
	for _, s := range steps {
		if s != avis && s.Apply != nil {
			_ = s.Apply(context.Background())
		}
	}
	for _, q := range m.exécutées {
		if strings.Contains(q, "IDENTIFIED WITH") {
			t.Errorf("aucun changement de plugin sans mot de passe : %s", q)
		}
	}
}

func TestPluginNatifInactifNommeLeRemede(t *testing.T) {
	// MySQL 8.4 livre mysql_native_password désactivé, MySQL 9 l'a retiré : le
	// compte est créé avec le plugin du serveur, et le plan le dit.
	prov, proj := projetMySQL(t, "7.1")
	client, m := fauxMySQL(t, nil, nil, nil)
	pluginsMySQL(m, "", "")
	prov.clients = map[string]Client{"mysql": client}
	steps := planifier(t, prov, proj)
	if d := décrire(t, steps[0]); strings.Contains(d, "IDENTIFIED WITH") {
		t.Errorf("le plugin inactif ne doit pas être demandé :\n%s", d)
	}
	var avis *engine.Step
	for _, s := range steps {
		if strings.HasSuffix(s.Resource, "/auth") {
			avis = s
		}
	}
	if avis == nil || avis.Critical {
		t.Fatalf("un avis non critique est attendu : %v", steps)
	}
	if err := avis.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "mysql_native_password=ON") {
		t.Errorf("l'avis doit nommer le remède : %v", err)
	}
}

func TestUnPluginSansMotDePasseNEstJamaisReposeSurUnCompteEnPlace(t *testing.T) {
	// S2R-08 : authPlugin accepte tout identifiant ; reposé sur un compte en
	// place, auth_socket l'ouvrirait au compte système homonyme.
	prov, proj := projetMySQL(t, "8.3")
	proj.Spec.Databases[0].AuthPlugin = "auth_socket"
	decl := proj.Spec.Databases[0]
	if _, err := prov.ensurePassword(proj.ID(), "mysql", decl.User); err != nil {
		t.Fatal(err)
	}
	client, m := fauxMySQL(t, []string{decl.Name}, []string{decl.User + "@localhost"},
		[][2]string{{decl.Name, tousDroits}})
	pluginsMySQL(m, "caching_sha2_password", "ACTIVE")
	prov.clients = map[string]Client{"mysql": client}
	if steps := planifier(t, prov, proj); steps[0].Action != engine.NoOp {
		t.Errorf("aucun changement de plugin attendu : %s", steps[0].Summary)
	}
}
