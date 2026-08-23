package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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
func (c *mysqlClient) Grants(ctx context.Context, user, host string) (map[string]string, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT TABLE_SCHEMA, GROUP_CONCAT(DISTINCT PRIVILEGE_TYPE ORDER BY PRIVILEGE_TYPE)
		 FROM information_schema.SCHEMA_PRIVILEGES
		 WHERE GRANTEE = ? GROUP BY TABLE_SCHEMA`,
		fmt.Sprintf("'%s'@'%s'", user, host))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var schema, privs string
		if err := rows.Scan(&schema, &privs); err != nil {
			return nil, err
		}
		out[schema] = privs
	}
	return out, rows.Err()
}

// Extensions n'a pas d'équivalent MySQL.
func (c *mysqlClient) Extensions(context.Context, string) (map[string]bool, error) {
	return map[string]bool{}, nil
}
