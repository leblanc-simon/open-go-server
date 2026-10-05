package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"leblanc.io/open-go-server/internal/naming"
)

// NewMySQLClient ouvre une connexion administrateur par socket Unix. L'absence de
// mot de passe est voulue : l'authentification par socket (auth_socket ou
// unix_socket) évite d'avoir à stocker un identifiant root.
func NewMySQLClient(socket string) (Client, error) {
	dsn := func(database string) string {
		return fmt.Sprintf("root@unix(%s)/%s?parseTime=true&multiStatements=false", socket, database)
	}
	db, err := sql.Open("mysql", dsn(""))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	return &mysqlClient{sqlClient{engine: "mysql", db: db, dsn: dsn, port: 3306}}, nil
}

type mysqlClient struct{ sqlClient }

// Endpoint renvoie le point de connexion applicatif et la version du serveur.
func (c *mysqlClient) Endpoint(ctx context.Context) (Endpoint, error) {
	var version string
	if err := c.db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return Endpoint{}, fmt.Errorf("%w : %v", ErrUnavailable, err)
	}
	engine := "mysql"
	if strings.Contains(strings.ToLower(version), "mariadb") {
		engine = "mariadb"
	}
	// La version est tronquée à « majeure.mineure » : c'est ce qu'attend Doctrine
	// dans serverVersion, et cela évite un DSN qui change à chaque correctif.
	short := version
	if parts := strings.SplitN(version, ".", 3); len(parts) >= 2 {
		short = parts[0] + "." + parts[1]
	}
	return Endpoint{Engine: engine, Host: "127.0.0.1", Port: c.port, Version: short}, nil
}

func (c *mysqlClient) Databases(ctx context.Context) (map[string]bool, error) {
	return c.queryStrings(ctx, "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA")
}

func (c *mysqlClient) Users(ctx context.Context) (map[string]bool, error) {
	return c.queryStrings(ctx, "SELECT CONCAT(User, '@', Host) FROM mysql.user")
}

// Grants lit les privilèges observés d'un compte et les normalise par base, afin
// de pouvoir les comparer à l'état désiré.
//
// Les privilèges **globaux** — ceux accordés « ON *.* » — sont lus à part et
// rendus sous GlobalGrantKey. SCHEMA_PRIVILEGES est alimentée par la seule
// mysql.db : elle ignore tout ce qui a été accordé globalement, et les y chercher
// serait ne jamais les voir. Un compte n'ayant que des privilèges globaux — celui
// de la supervision — n'était alors jamais vu conforme, et le plan annonçait
// éternellement une correction déjà appliquée.
func (c *mysqlClient) Grants(ctx context.Context, user, host string) (map[string]string, error) {
	grantee := fmt.Sprintf("'%s'@'%s'", user, host)
	rows, err := c.db.QueryContext(ctx,
		`SELECT TABLE_SCHEMA, GROUP_CONCAT(DISTINCT PRIVILEGE_TYPE ORDER BY PRIVILEGE_TYPE),
		        MAX(IS_GRANTABLE)
		 FROM information_schema.SCHEMA_PRIVILEGES
		 WHERE GRANTEE = ? GROUP BY TABLE_SCHEMA`, grantee)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var (
			schema, privs string
			grantable     sql.NullString
		)
		if err := rows.Scan(&schema, &privs, &grantable); err != nil {
			return nil, err
		}
		// Le motif est rendu tel qu'il est stocké, échappement compris : il faut le
		// ramener au nom réel pour pouvoir le comparer à l'état désiré. Un motif
		// à joker — le GRANT « shop_fr », écrit sans échappement — est
		// rangé à part (D111) : sous le nom réel, il écrasait ou masquait le GRANT
		// échappé d'ogs, selon l'ordre de relecture, et n'était jamais retiré.
		// GRANT OPTION n'est jamais un PRIVILEGE_TYPE de SCHEMA_PRIVILEGES — il
		// n'y paraît que par IS_GRANTABLE : un joker posé WITH GRANT OPTION que
		// l'on ne révoquerait que par ses privilèges relus garderait sa ligne
		// (Grant_priv), relue ensuite « USAGE », et le plan repasserait sans fin.
		if IsMySQLJoker(schema) {
			if grantable.String == "YES" {
				privs += ",GRANT OPTION"
			}
			out[jokerKey(schema)] = privs
			continue
		}
		out[naming.UnescapeMySQLPattern(schema)] = privs
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var global sql.NullString
	if err := c.db.QueryRowContext(ctx,
		`SELECT GROUP_CONCAT(DISTINCT PRIVILEGE_TYPE ORDER BY PRIVILEGE_TYPE)
		 FROM information_schema.USER_PRIVILEGES WHERE GRANTEE = ?`,
		grantee).Scan(&global); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if global.Valid && global.String != "" {
		out[GlobalGrantKey] = global.String
	}
	return out, nil
}

// Extensions n'a pas d'équivalent MySQL.
func (c *mysqlClient) Extensions(context.Context, string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// jokerPrefix préfixe, dans le relevé de Grants, la clé d'un motif à joker : un
// nom de base ne contient jamais d'octet nul.
const jokerPrefix = "\x00joker:"

func jokerKey(pattern string) string { return jokerPrefix + pattern }

// JokerGrantKey est la clé sous laquelle Grants range le motif à joker pattern.
func JokerGrantKey(pattern string) string { return jokerKey(pattern) }

// MySQLJokerPattern rend le motif à joker, relevé par Grants, qui désigne la
// base database — vide s'il n'y en a pas —, qu'il en couvre d'autres ou non, et
// les privilèges que porte sa ligne.
func MySQLJokerPattern(grants map[string]string, database string) (pattern, privileges string) {
	pattern, _ = mysqlJoker(grants, database, nil)
	return pattern, grants[jokerKey(pattern)]
}

// IsMySQLJoker dit si un motif de privilège MySQL contient un joker non
// échappé — « _ » ou « % » —, c'est-à-dire s'il désigne potentiellement
// plusieurs bases.
func IsMySQLJoker(pattern string) bool {
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			i++
		case '_', '%':
			return true
		}
	}
	return false
}

// matchMySQLPattern dit si une base répond à un motif de privilège MySQL : « _ »
// vaut un caractère, « % » une suite quelconque, « \x » le caractère x.
func matchMySQLPattern(pattern, name string) bool {
	p, n := []rune(pattern), []rune(name)
	var match func(i, j int) bool
	match = func(i, j int) bool {
		for i < len(p) {
			switch p[i] {
			case '%':
				for k := j; k <= len(n); k++ {
					if match(i+1, k) {
						return true
					}
				}
				return false
			case '_':
				if j >= len(n) {
					return false
				}
			case '\\':
				if i+1 < len(p) {
					i++
				}
				if j >= len(n) || n[j] != p[i] {
					return false
				}
			default:
				if j >= len(n) || n[j] != p[i] {
					return false
				}
			}
			i++
			j++
		}
		return j == len(n)
	}
	return match(0, 0)
}

// grantDatabase rend le nom de base qu'une clé du relevé de Grants désigne : le
// nom réel, ou celui qu'un motif à joker désigne une fois ses échappements
// retirés.
func grantDatabase(key string) (name string, joker bool) {
	if pattern, ok := strings.CutPrefix(key, jokerPrefix); ok {
		return naming.UnescapeMySQLPattern(pattern), true
	}
	return key, false
}

// mysqlJoker cherche, parmi les privilèges relevés d'un compte, le motif à joker
// qui désigne la base database — un GRANT écrit sans échappement. Il
// rend ce motif, et les autres bases existantes qu'il couvre : tant qu'il en
// couvre une, il en porte l'accès, et le retirer couperait l'application.
func mysqlJoker(grants map[string]string, database string, databases map[string]bool) (pattern string, others []string) {
	for key, privs := range grants {
		raw, ok := strings.CutPrefix(key, jokerPrefix)
		if !ok || naming.UnescapeMySQLPattern(raw) != database || len(revocable(privs)) == 0 {
			continue
		}
		pattern = raw
		for name := range databases {
			if name != database && matchMySQLPattern(raw, name) {
				others = append(others, name)
			}
		}
	}
	sort.Strings(others)
	return pattern, others
}

// revocable rend les privilèges d'une ligne relue qu'un REVOKE peut retirer :
// USAGE n'est pas un privilège — c'est l'absence de privilège —, et
// « REVOKE USAGE » ne retire rien. Une ligne qui ne porte rien d'autre n'est
// pas un joker à révoquer : elle ferait repasser le plan sans fin.
func revocable(privileges string) []string {
	var out []string
	for _, p := range strings.Split(privileges, ",") {
		p = strings.ToUpper(strings.TrimSpace(p))
		if p != "" && p != "USAGE" {
			out = append(out, p)
		}
	}
	return out
}
