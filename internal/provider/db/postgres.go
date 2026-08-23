package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// NewPostgresClient ouvre une connexion administrateur sur un cluster, par socket
// Unix : l'authentification « peer » reconnaît l'utilisateur du système, ce qui
// évite tout mot de passe pour l'accès d'administration.
func NewPostgresClient(cluster Cluster, adminUser string) (Client, error) {
	if adminUser == "" {
		adminUser = "postgres"
	}
	socketDir := cluster.Socket
	if socketDir == "" {
		socketDir = "/var/run/postgresql"
	}
	dsn := func(database string) string {
		if database == "" {
			database = "postgres"
		}
		return fmt.Sprintf("postgres://%s@/%s?host=%s&port=%d",
			adminUser, database, socketDir, cluster.Port)
	}
	db, err := sql.Open("pgx", dsn(""))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	return &pgClient{sqlClient{engine: "postgres", db: db, dsn: dsn, port: cluster.Port}, cluster}, nil
}

type pgClient struct {
	sqlClient
	cluster Cluster
}

// Endpoint renvoie le point de connexion applicatif.
func (c *pgClient) Endpoint(ctx context.Context) (Endpoint, error) {
	var setting string
	if err := c.db.QueryRowContext(ctx, "SHOW server_version").Scan(&setting); err != nil {
		return Endpoint{}, fmt.Errorf("%w : %v", ErrUnavailable, err)
	}
	major := setting
	if i := strings.IndexAny(setting, ". "); i > 0 {
		major = setting[:i]
	}
	return Endpoint{Engine: "postgres", Host: "127.0.0.1", Port: c.port, Version: major}, nil
}

func (c *pgClient) Databases(ctx context.Context) (map[string]bool, error) {
	return c.queryStrings(ctx, "SELECT datname FROM pg_database WHERE NOT datistemplate")
}

func (c *pgClient) Users(ctx context.Context) (map[string]bool, error) {
	return c.queryStrings(ctx, "SELECT rolname FROM pg_roles WHERE rolcanlogin")
}

// Grants renvoie le propriétaire des bases : côté PostgreSQL, c'est la propriété
// qui porte l'essentiel des droits d'une application sur sa base.
func (c *pgClient) Grants(ctx context.Context, role, _ string) (map[string]string, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT d.datname, pg_get_userbyid(d.datdba) FROM pg_database d WHERE NOT d.datistemplate`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, owner string
		if err := rows.Scan(&name, &owner); err != nil {
			return nil, err
		}
		if owner == role {
			out[name] = "OWNER"
		}
	}
	return out, rows.Err()
}

// Extensions liste les extensions installées dans une base.
func (c *pgClient) Extensions(ctx context.Context, database string) (map[string]bool, error) {
	conn, err := sql.Open("pgx", c.dsn(database))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rows, err := conn.QueryContext(ctx, "SELECT extname FROM pg_extension")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// SupportsForceDrop indique si « DROP DATABASE ... WITH (FORCE) » est disponible :
// la clause n'existe qu'à partir de PostgreSQL 13.
func (c *pgClient) SupportsForceDrop() bool {
	major, err := strconv.Atoi(strings.SplitN(c.cluster.Version, ".", 2)[0])
	return err == nil && major >= 13
}
