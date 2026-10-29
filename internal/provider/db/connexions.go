package db

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"leblanc.io/open-go-server/internal/model"
)

// Le budget de connexions d'un projet (D127). Chaque compte d'un projet reçoit
// une limite de connexions simultanées : sans elle, un pic de trafic, une boucle
// de workers ou une fuite de connexions d'un seul projet épuisait
// max_connections, et toutes les bases du serveur refusaient leurs clients.
const (
	// NoConnectionLimit, dans maxConnections, retire toute limite au compte.
	NoConnectionLimit = -1
	// connectionMargin couvre l'exploitation : sauvegarde, console, migrations,
	// un apply qui recharge une base pendant que le site sert.
	connectionMargin = 10
	// runtimeInstanceConnections est ce qu'une instance d'application non PHP
	// ouvre : ses pilotes tiennent un pool (node-postgres 10, SQLAlchemy 5 + 10 de
	// débordement, Rails 5 par thread).
	runtimeInstanceConnections = 10
	// ExtraUserConnections est la limite par défaut d'un compte additionnel :
	// lecture, rapports, outil d'exploitation — aucun worker ne le compte.
	ExtraUserConnections = 10
)

// ConnectionBudget rend la limite calculée du compte principal d'un projet :
// un worker PHP, une instance de service et une tâche planifiée ouvrent chacun
// une connexion, une instance d'application non PHP un pool ; la moitié en sus,
// et au moins connectionMargin, pour les pics et l'exploitation. phpWorkers est
// la somme des pm.max_children résolus des pools.
func ConnectionBudget(proj *model.Project, phpWorkers int) int {
	base := phpWorkers
	for _, svc := range proj.Spec.Services {
		if svc.Enabled != nil && !*svc.Enabled {
			continue
		}
		base += max(svc.Instances, 1)
	}
	if at, err := proj.AppType(); err == nil && at.Runtime != "" {
		base += max(proj.Spec.App.Instances, 1) * runtimeInstanceConnections
	}
	for _, job := range proj.Spec.Crons {
		if job.Enabled == nil || *job.Enabled {
			base++
		}
	}
	return base + max(connectionMargin, base/2)
}

// accountKey désigne un compte sur son serveur : mysql et mariadb visent le même
// (D106), un rôle PostgreSQL vaut pour tout son cluster.
func accountKey(decl model.Database, user string) string {
	if decl.Engine == "postgres" {
		return "postgres|" + decl.Cluster + "|" + user
	}
	host := decl.Host
	if host == "" {
		host = "localhost"
	}
	return "mysql|" + user + "@" + host
}

// accountLimit est la limite voulue d'un compte, avec la déclaration qui le
// désigne — son moteur, son cluster, son hôte.
type accountLimit struct {
	decl  model.Database
	user  string
	limit int
}

// accountLimits rend la limite voulue de chaque compte du projet. Un compte
// nommé par plusieurs déclarations — principal d'une base, additionnel d'une
// autre — reçoit la plus large, l'absence de limite l'emportant : deux limites
// différentes feraient osciller le plan d'un apply à l'autre.
func accountLimits(proj *model.Project, phpWorkers int) map[string]int {
	out := map[string]int{}
	for key, a := range projectAccounts(proj, phpWorkers) {
		out[key] = a.limit
	}
	return out
}

func projectAccounts(proj *model.Project, phpWorkers int) map[string]*accountLimit {
	out := map[string]*accountLimit{}
	merge := func(decl model.Database, user string, limit int) {
		key := accountKey(decl, user)
		cur, ok := out[key]
		switch {
		case !ok:
			out[key] = &accountLimit{decl: decl, user: user, limit: limit}
		case limit == NoConnectionLimit, cur.limit != NoConnectionLimit && limit > cur.limit:
			cur.limit = limit
		}
	}
	budget := ConnectionBudget(proj, phpWorkers)
	for _, decl := range proj.Spec.Databases {
		main := budget
		if decl.MaxConnections != 0 {
			main = decl.MaxConnections
		}
		merge(decl, decl.User, main)
		for _, u := range decl.ExtraUsers {
			extra := ExtraUserConnections
			if u.MaxConnections != 0 {
				extra = u.MaxConnections
			}
			merge(decl, u.User, extra)
		}
	}
	return out
}

// engineLimit traduit une limite voulue dans la valeur que le moteur stocke et
// relit : « aucune » s'écrit 0 sous MySQL et MariaDB, -1 sous PostgreSQL.
func engineLimit(engine string, limit int) int {
	if limit == NoConnectionLimit && engine != "postgres" {
		return 0
	}
	return limit
}

// connectionLimits relit la limite de connexions d'un compte.
type connectionLimits interface {
	ConnectionLimit(ctx context.Context, user, host string) (int, error)
}

func (c *mysqlClient) ConnectionLimit(ctx context.Context, user, host string) (int, error) {
	var n int64
	err := c.db.QueryRowContext(ctx,
		"SELECT max_user_connections FROM mysql.user WHERE User = ? AND Host = ?", user, host).Scan(&n)
	return int(n), err
}

func (c *pgClient) ConnectionLimit(ctx context.Context, role, _ string) (int, error) {
	var n int64
	err := c.db.QueryRowContext(ctx, "SELECT rolconnlimit FROM pg_roles WHERE rolname = $1", role).Scan(&n)
	return int(n), err
}

// observeLimits relit la limite des comptes existants de la déclaration. Une
// lecture en échec laisse le compte hors de Limits : elle ne fonde aucune
// correction (Q-20).
func (s *DBState) observeLimits(ctx context.Context, client Client, host string) {
	reader, ok := client.(connectionLimits)
	if !ok {
		return
	}
	decl := s.Declaration
	users := []string{}
	if s.UserExists {
		users = append(users, decl.User)
	}
	for _, u := range decl.ExtraUsers {
		if s.ExtraUsers[u.User] {
			users = append(users, u.User)
		}
	}
	for _, user := range users {
		if n, err := reader.ConnectionLimit(ctx, user, host); err == nil {
			s.Limits[user] = n
		}
	}
}

// planLimits retient, pour chaque compte de la déclaration, la limite à poser :
// celle d'un compte à créer, et celle d'un compte existant dont la limite relue
// diffère. Un compte adopté la reçoit comme les autres — ALTER USER … WITH
// MAX_USER_CONNECTIONS et ALTER ROLE … CONNECTION LIMIT n'exigent aucun mot de
// passe.
func (s *DBState) planLimits(wanted map[string]int) {
	decl := s.Declaration
	s.SetLimits = map[string]int{}
	consider := func(user string, exists bool) {
		limit, ok := wanted[accountKey(decl, user)]
		if !ok {
			return
		}
		want := engineLimit(decl.Engine, limit)
		if !exists {
			s.SetLimits[user] = want
			return
		}
		if got, read := s.Limits[user]; read && got != want {
			s.SetLimits[user] = want
		}
	}
	consider(decl.User, s.UserExists)
	for _, u := range decl.ExtraUsers {
		consider(u.User, s.ExtraUsers[u.User])
	}
}

// limitDrift dit qu'un compte existant doit changer de limite.
func (s DBState) limitDrift() bool {
	for user := range s.SetLimits {
		if _, read := s.Limits[user]; read {
			return true
		}
	}
	return false
}

// limitStatements rend les requêtes qui posent les limites retenues, après la
// création des comptes.
func limitStatements(decl model.Database, state DBState, host string) ([]string, error) {
	users := make([]string, 0, len(state.SetLimits))
	for user := range state.SetLimits {
		users = append(users, user)
	}
	sort.Strings(users)
	var out []string
	for _, user := range users {
		stmt, err := ConnectionLimitStatement(decl.Engine, user, host, state.SetLimits[user])
		if err != nil {
			return nil, err
		}
		out = append(out, stmt)
	}
	return out, nil
}

// ConnectionLimitStatement produit la requête qui pose la limite de connexions
// d'un compte, dans la valeur du moteur (engineLimit).
func ConnectionLimitStatement(engine, user, host string, limit int) (string, error) {
	if err := checkIdentifier("nom d'utilisateur", user); err != nil {
		return "", err
	}
	if engine == "postgres" {
		return fmt.Sprintf("ALTER ROLE %s CONNECTION LIMIT %d", quotePGIdent(user), limit), nil
	}
	if host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("ALTER USER %s@%s WITH MAX_USER_CONNECTIONS %d",
		quoteLiteral(user), quoteLiteral(host), limit), nil
}

// limitNote complète le résumé de l'étape des limites qu'elle change.
func limitNote(state DBState) string {
	var notes []string
	users := make([]string, 0, len(state.SetLimits))
	for user := range state.SetLimits {
		users = append(users, user)
	}
	sort.Strings(users)
	for _, user := range users {
		got, read := state.Limits[user]
		if !read {
			continue
		}
		notes = append(notes, fmt.Sprintf("%s %s → %s", user,
			describeLimit(state.Declaration.Engine, got), describeLimit(state.Declaration.Engine, state.SetLimits[user])))
	}
	if len(notes) == 0 {
		return ""
	}
	return " — connexions simultanées : " + strings.Join(notes, ", ") + " (D127)"
}

func describeLimit(engine string, n int) string {
	if n < 0 || n == 0 && engine != "postgres" {
		return "sans limite"
	}
	return fmt.Sprint(n)
}

// Capacity confronte, pour un serveur de bases, la somme des limites voulues
// des comptes du parc à son max_connections (D127). Les limites bornent ce
// qu'un projet peut prendre ; leur somme au-delà de la capacité laisse des
// projets pris ensemble l'épuiser encore.
type Capacity struct {
	// Server nomme le serveur : « mysql », ou « postgres <cluster> ».
	Server string
	// MaxConnections est la capacité relue du serveur, connexions réservées aux
	// superutilisateurs déduites sous PostgreSQL.
	MaxConnections int
	// Total est la somme des limites des comptes limités, Accounts leur nombre.
	Total    int
	Accounts int
	// Unlimited nomme les comptes déclarés sans limite (maxConnections: -1).
	Unlimited []string
	// Err porte le motif d'une capacité illisible ; le reste vaut tout de même.
	Err error
}

// serverCapacity relit la capacité d'un serveur.
type serverCapacity interface {
	MaxConnections(ctx context.Context) (int, error)
}

func (c *mysqlClient) MaxConnections(ctx context.Context) (int, error) {
	var n int64
	err := c.db.QueryRowContext(ctx, "SELECT @@max_connections").Scan(&n)
	return int(n), err
}

func (c *pgClient) MaxConnections(ctx context.Context) (int, error) {
	var n int64
	err := c.db.QueryRowContext(ctx, "SELECT current_setting('max_connections')::int - "+
		"current_setting('superuser_reserved_connections')::int").Scan(&n)
	return int(n), err
}

// ConnectionCapacity rend, par serveur, la somme des limites voulues des comptes
// de tous les projets et la capacité du serveur. Un serveur injoignable est
// tu : Notices le dit déjà, base par base.
func (p *Provider) ConnectionCapacity(ctx context.Context, projects []*model.Project) []Capacity {
	byServer := map[string]*Capacity{}
	clients := map[string]Client{}
	var order []string
	for _, proj := range projects {
		workers := 0
		if p.PHPWorkers != nil {
			workers = p.PHPWorkers(proj)
		}
		accounts := projectAccounts(proj, workers)
		keys := make([]string, 0, len(accounts))
		for key := range accounts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			a := accounts[key]
			client, err := p.clientFor(a.decl)
			if err != nil {
				continue
			}
			server := "mysql"
			if a.decl.Engine == "postgres" {
				server = "postgres " + p.clusterID(a.decl)
			}
			c, ok := byServer[server]
			if !ok {
				c = &Capacity{Server: server}
				byServer[server], clients[server] = c, client
				order = append(order, server)
			}
			if a.limit == NoConnectionLimit {
				c.Unlimited = append(c.Unlimited, fmt.Sprintf("%s (%s)", accountLabel(a.decl, a.user), proj.ID()))
				continue
			}
			c.Total += a.limit
			c.Accounts++
		}
	}
	sort.Strings(order)
	out := make([]Capacity, 0, len(order))
	for _, server := range order {
		c := byServer[server]
		if reader, ok := clients[server].(serverCapacity); ok {
			c.MaxConnections, c.Err = reader.MaxConnections(ctx)
		}
		out = append(out, *c)
	}
	return out
}

// clusterID nomme le cluster d'une déclaration PostgreSQL : celui qu'elle
// désigne, ou le premier détecté, que clientFor retient à défaut.
func (p *Provider) clusterID(decl model.Database) string {
	if decl.Cluster != "" {
		return decl.Cluster
	}
	if clusters := DetectClusters(p.Paths.Prefix()); len(clusters) > 0 {
		return clusters[0].ID()
	}
	return "défaut"
}

// accountLabel nomme un compte comme le moteur le désigne.
func accountLabel(decl model.Database, user string) string {
	if decl.Engine == "postgres" {
		return "rôle " + user
	}
	host := decl.Host
	if host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("'%s'@'%s'", user, host)
}
