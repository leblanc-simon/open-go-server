package db

import (
	"context"
	"database/sql"
	"fmt"
	"os/user"
	"strconv"
	"strings"
)

// PostgresAdminRole renvoie le rôle d'administration à employer. L'authentification
// « peer » reconnaît l'utilisateur du système : le rôle doit donc porter le nom de
// l'utilisateur qui exécute ogs — root en exploitation — et non « postgres », qui
// serait systématiquement refusé. OGS_PG_ADMIN permet d'en désigner un autre.
func PostgresAdminRole(override string) string {
	if override != "" {
		return override
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "postgres"
}

// NewPostgresClient ouvre une connexion administrateur sur un cluster, par socket
// Unix : l'authentification « peer » reconnaît l'utilisateur du système, ce qui
// évite tout mot de passe pour l'accès d'administration.
func NewPostgresClient(cluster Cluster, adminUser string) (Client, error) {
	adminUser = PostgresAdminRole(adminUser)
	socketDir, err := cluster.SocketDir("")
	if err != nil {
		return nil, err
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
	return &pgClient{sqlClient{engine: "postgres", db: db, dsn: dsn, port: cluster.Port}, cluster, adminUser}, nil
}

type pgClient struct {
	sqlClient
	cluster   Cluster
	adminRole string
}

// explainPostgresRefusal traduit un refus d'authentification en consigne. Le cas
// est le plus courant à l'installation : ogs s'exécute en root, l'authentification
// « peer » exige donc un rôle « root », qu'une installation neuve ne comporte pas.
func explainPostgresRefusal(role string, err error) error {
	msg := err.Error()
	if !strings.Contains(msg, "Peer authentication failed") &&
		!strings.Contains(msg, "role \""+role+"\" does not exist") {
		return fmt.Errorf("%w : %v", ErrUnavailable, err)
	}
	return fmt.Errorf("%w : le cluster refuse le rôle d'administration %q — "+
		"l'authentification « peer » exige un rôle portant le nom de l'utilisateur système. "+
		"Le créer une fois : su - postgres -c 'createuser --superuser %s' ; "+
		"ou en désigner un autre par OGS_PG_ADMIN (%v)", ErrUnavailable, role, role, err)
}

// Endpoint renvoie le point de connexion applicatif.
func (c *pgClient) Endpoint(ctx context.Context) (Endpoint, error) {
	var setting string
	if err := c.db.QueryRowContext(ctx, "SHOW server_version").Scan(&setting); err != nil {
		return Endpoint{}, explainPostgresRefusal(c.adminRole, err)
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

// databaseAccess est la relecture de l'ACL d'une base PostgreSQL (D111).
type databaseAccess interface {
	// DatabaseAccess rend le propriétaire de la base, si PUBLIC peut s'y
	// connecter, et si le rôle role le peut — par PUBLIC, par la propriété ou par
	// un GRANT ; faux pour un rôle absent. Une base absente rend sql.ErrNoRows.
	DatabaseAccess(ctx context.Context, database, role string) (owner string, publicConnect, roleConnect bool, err error)
}

// DatabaseAccess relit le propriétaire d'une base et le droit CONNECT de PUBLIC
// et du rôle donné. Une ACL nulle est celle par défaut, qui accorde CONNECT et
// TEMPORARY à PUBLIC : c'est l'état de toute base qu'aucun GRANT ni REVOKE n'a
// touchée.
func (c *pgClient) DatabaseAccess(ctx context.Context, database, role string) (string, bool, bool, error) {
	var (
		owner        string
		public, mine bool
	)
	err := c.db.QueryRowContext(ctx, `SELECT pg_get_userbyid(d.datdba),
		d.datacl IS NULL OR EXISTS (SELECT 1 FROM aclexplode(d.datacl) a
			WHERE a.grantee = 0 AND a.privilege_type = 'CONNECT'),
		COALESCE((SELECT has_database_privilege(r.oid, d.oid, 'CONNECT') FROM pg_roles r
			WHERE r.rolname = $2), false)
		FROM pg_database d WHERE d.datname = $1`, database, role).Scan(&owner, &public, &mine)
	return owner, public, mine, err
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
