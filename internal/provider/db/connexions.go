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

// accountLimits rend la limite voulue de chaque compte du projet. Un compte
// nommé par plusieurs déclarations — principal d'une base, additionnel d'une
// autre — reçoit la plus large, l'absence de limite l'emportant : deux limites
// différentes feraient osciller le plan d'un apply à l'autre.
func accountLimits(proj *model.Project, phpWorkers int) map[string]int {
	out := map[string]int{}
	merge := func(key string, limit int) {
		cur, ok := out[key]
		switch {
		case !ok, limit == NoConnectionLimit:
			out[key] = limit
		case cur != NoConnectionLimit && limit > cur:
			out[key] = limit
		}
	}
	budget := ConnectionBudget(proj, phpWorkers)
	for _, decl := range proj.Spec.Databases {
		main := budget
		if decl.MaxConnections != 0 {
			main = decl.MaxConnections
		}
		merge(accountKey(decl, decl.User), main)
		for _, u := range decl.ExtraUsers {
			extra := ExtraUserConnections
			if u.MaxConnections != 0 {
				extra = u.MaxConnections
			}
			merge(accountKey(decl, u.User), extra)
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
