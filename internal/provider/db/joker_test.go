package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
)

// faux pilote database/sql : il rend, pour chaque requête, les lignes de la
// première réponse dont le fragment y figure, et note ce qu'on lui fait
// exécuter. Il éprouve la relecture réelle de mysqlClient — ce que rend
// SCHEMA_PRIVILEGES, échappement compris — sans serveur.
type fauxMoteur struct {
	mu        sync.Mutex
	réponses  []fauxRéponse
	exécutées []string
}

type fauxRéponse struct {
	fragment string
	colonnes []string
	lignes   [][]driver.Value
}

var (
	fauxMoteursMu sync.Mutex
	fauxMoteurs   = map[string]*fauxMoteur{}
)

func init() { sql.Register("ogs-faux", fauxPilote{}) }

type fauxPilote struct{}

func (fauxPilote) Open(name string) (driver.Conn, error) {
	fauxMoteursMu.Lock()
	defer fauxMoteursMu.Unlock()
	m, ok := fauxMoteurs[name]
	if !ok {
		return nil, fmt.Errorf("faux moteur %q inconnu", name)
	}
	return fauxConn{m}, nil
}

type fauxConn struct{ m *fauxMoteur }

func (c fauxConn) Prepare(query string) (driver.Stmt, error) { return fauxStmt{c.m, query}, nil }
func (fauxConn) Close() error                                { return nil }
func (fauxConn) Begin() (driver.Tx, error)                   { return nil, fmt.Errorf("pas de transaction") }

type fauxStmt struct {
	m     *fauxMoteur
	query string
}

func (fauxStmt) Close() error  { return nil }
func (fauxStmt) NumInput() int { return -1 }

func (s fauxStmt) Exec([]driver.Value) (driver.Result, error) {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	s.m.exécutées = append(s.m.exécutées, s.query)
	return driver.RowsAffected(0), nil
}

func (s fauxStmt) Query([]driver.Value) (driver.Rows, error) {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	for _, r := range s.m.réponses {
		if strings.Contains(s.query, r.fragment) {
			return &fauxLignes{colonnes: r.colonnes, lignes: r.lignes}, nil
		}
	}
	return nil, fmt.Errorf("requête inattendue : %s", s.query)
}

type fauxLignes struct {
	colonnes []string
	lignes   [][]driver.Value
	i        int
}

func (l *fauxLignes) Columns() []string { return l.colonnes }
func (l *fauxLignes) Close() error      { return nil }
func (l *fauxLignes) Next(dest []driver.Value) error {
	if l.i >= len(l.lignes) {
		return io.EOF
	}
	copy(dest, l.lignes[l.i])
	l.i++
	return nil
}

// fauxMySQL branche un mysqlClient sur un faux moteur : bases, comptes, et les
// lignes de SCHEMA_PRIVILEGES telles que MySQL les stocke.
func fauxMySQL(t *testing.T, bases, comptes []string, privilèges [][2]string) (*mysqlClient, *fauxMoteur) {
	t.Helper()
	m := &fauxMoteur{}
	une := func(v []string) [][]driver.Value {
		var out [][]driver.Value
		for _, s := range v {
			out = append(out, []driver.Value{s})
		}
		return out
	}
	// « privilèges|YES » marque une ligne posée WITH GRANT OPTION, que
	// SCHEMA_PRIVILEGES ne dit que par IS_GRANTABLE.
	var schema [][]driver.Value
	for _, p := range privilèges {
		privs, grantable, _ := strings.Cut(p[1], "|")
		if grantable == "" {
			grantable = "NO"
		}
		schema = append(schema, []driver.Value{p[0], privs, grantable})
	}
	m.réponses = []fauxRéponse{
		{"VERSION()", []string{"v"}, [][]driver.Value{{"8.0.36"}}},
		{"SCHEMATA", []string{"s"}, une(bases)},
		{"mysql.user", []string{"u"}, une(comptes)},
		{"SCHEMA_PRIVILEGES", []string{"s", "p", "g"}, schema},
		{"USER_PRIVILEGES", []string{"p"}, [][]driver.Value{{nil}}},
	}
	nom := t.Name()
	fauxMoteursMu.Lock()
	fauxMoteurs[nom] = m
	fauxMoteursMu.Unlock()
	db, err := sql.Open("ogs-faux", nom)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &mysqlClient{sqlClient{engine: "mysql", db: db, port: 3306}}, m
}

// décrire rend la description de l'étape de la base — ses requêtes.
func décrire(t *testing.T, s *engine.Step) string {
	t.Helper()
	if s.Diff == nil {
		return ""
	}
	d, err := s.Diff()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestUnJokerHistoriqueNeMasquePasLeGrantEchappe(t *testing.T) {
	// R1-47. Un serveur repris porte souvent un GRANT historique écrit sans
	// échappement — « shop_fr », où le souligné est un joker — à côté du GRANT
	// échappé qu'ogs pose — « shop\_fr ». Les deux tombaient sous la même clé, le
	// dernier relu l'emportant : sur une déclaration « readonly », le joker en
	// ALL faisait annoncer à chaque apply une révocation… du GRANT échappé, le
	// joker restant en place, éternellement.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	decl.Name, decl.User, decl.Host, decl.Privileges = "shop_fr", "shop", "%", "readonly"
	proj.Spec.Databases = []model.Database{decl}
	adopter(proj)
	client, moteur := fauxMySQL(t, []string{"shop_fr"}, []string{"shop@%"}, [][2]string{
		{`shop\_fr`, "SELECT,SHOW VIEW"},
		{"shop_fr", tousDroits},
	})
	prov.clients = map[string]Client{"mysql": client}

	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) == 0 || steps[0].Action == engine.NoOp {
		t.Fatalf("le joker doit être annoncé : %+v", steps)
	}
	d := décrire(t, steps[0])
	if !strings.Contains(d, "REVOKE ALTER, CREATE, DELETE, DROP, INDEX, INSERT, SELECT, UPDATE ON `shop_fr`.* FROM 'shop'@'%'") {
		t.Errorf("le joker doit être révoqué :\n%s", d)
	}
	if strings.Contains(d, "REVOKE ALL PRIVILEGES ON `shop\\_fr`.*") {
		t.Errorf("le GRANT échappé est conforme, il ne doit pas être révoqué :\n%s", d)
	}
	if err := steps[0].Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Le GRANT échappé reste en place avant le retrait du joker : aucune fenêtre
	// sans droit pour l'application.
	grant, revoke := -1, -1
	for i, q := range moteur.exécutées {
		if strings.HasPrefix(q, "GRANT ") && strings.Contains(q, "`shop\\_fr`") {
			grant = i
		}
		if strings.HasPrefix(q, "REVOKE ") && strings.Contains(q, "`shop_fr`") {
			revoke = i
		}
	}
	if grant < 0 || revoke < grant {
		t.Errorf("le GRANT échappé doit précéder le retrait du joker : %v", moteur.exécutées)
	}

	// Le joker parti, la base est conforme.
	moteur.réponses[3].lignes = moteur.réponses[3].lignes[:1]
	obs, _ = prov.Observe(context.Background(), proj)
	steps, _ = prov.Plan(context.Background(), proj, obs)
	if len(steps) != 1 || steps[0].Action != engine.NoOp {
		t.Errorf("sans joker, rien à faire : %v", steps)
	}
}

func TestUnJokerQuiCouvreUneAutreBaseResteEnPlace(t *testing.T) {
	// Un joker qui couvre aussi une autre base existante en porte l'accès : le
	// retirer couperait l'application de cette base-là. Il reste, et ne compte
	// plus dans le jugement de la base déclarée — sans quoi le plan
	// recommencerait à chaque apply.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	decl.Name, decl.User, decl.Host, decl.Privileges = "shop_fr", "shop", "%", "readonly"
	proj.Spec.Databases = []model.Database{decl}
	adopter(proj)
	client, _ := fauxMySQL(t, []string{"shop_fr", "shop-fr"}, []string{"shop@%"}, [][2]string{
		{`shop\_fr`, "SELECT,SHOW VIEW"},
		{"shop_fr", tousDroits},
	})
	prov.clients = map[string]Client{"mysql": client}

	obs, err := prov.Observe(context.Background(), proj)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := prov.Plan(context.Background(), proj, obs)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Mutates() {
			t.Errorf("rien à faire : le GRANT échappé est conforme, le joker sert shop-fr : %s", s)
		}
	}
}

func TestUnJokerAvecGrantOptionPartEntier(t *testing.T) {
	// B3 : un joker posé WITH GRANT OPTION. SCHEMA_PRIVILEGES ne liste jamais
	// GRANT OPTION, seulement IS_GRANTABLE : le REVOKE des seuls privilèges relus
	// laissait la ligne (Grant_priv), relue ensuite « USAGE », et le plan émettait
	// « REVOKE USAGE », sans effet, à chaque apply.
	prov, proj := fixture(t)
	decl := DefaultDatabase(proj, "mysql")
	decl.Name, decl.User, decl.Host = "shop_fr", "shop", "%"
	proj.Spec.Databases = []model.Database{decl}
	adopter(proj)
	client, moteur := fauxMySQL(t, []string{"shop_fr"}, []string{"shop@%"}, [][2]string{
		{`shop\_fr`, tousDroits},
		{"shop_fr", "SELECT|YES"},
	})
	prov.clients = map[string]Client{"mysql": client}
	steps := planifier(t, prov, proj)
	if errs := appliquer(t, steps); len(errs) > 0 {
		t.Fatal(errs)
	}
	want := "REVOKE SELECT, GRANT OPTION ON `shop_fr`.* FROM 'shop'@'%'"
	if !strings.Contains(strings.Join(moteur.exécutées, "\n"), want) {
		t.Errorf("%s attendu : %v", want, moteur.exécutées)
	}

	// Une ligne qui ne porte plus que USAGE n'est pas un joker à révoquer : le
	// plan ne doit pas repasser sur un « REVOKE USAGE » sans effet.
	moteur.réponses[3].lignes[1] = []driver.Value{"shop_fr", "USAGE", "NO"}
	for _, s := range planifier(t, prov, proj) {
		if s.Mutates() {
			t.Errorf("USAGE seul ne doit faire repasser aucune étape : %s", s)
		}
	}
}
