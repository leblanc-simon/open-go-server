package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Client est l'accès administrateur à un moteur. Il est réduit au strict
// nécessaire : observer et exécuter. Les requêtes sont produites par le paquet
// statements, ce qui permet de les tester sans serveur.
type Client interface {
	// Endpoint décrit le point de connexion applicatif du moteur.
	Endpoint(ctx context.Context) (Endpoint, error)
	// Databases renvoie les bases existantes.
	Databases(ctx context.Context) (map[string]bool, error)
	// Users renvoie les comptes existants. Pour MySQL, la clé est « user@host ».
	Users(ctx context.Context) (map[string]bool, error)
	// Grants renvoie les privilèges observés d'un compte, normalisés.
	Grants(ctx context.Context, user, host string) (map[string]string, error)
	// Extensions renvoie les extensions installées dans une base (PostgreSQL).
	Extensions(ctx context.Context, database string) (map[string]bool, error)
	// Exec exécute une requête d'administration.
	Exec(ctx context.Context, stmt string) error
	// ExecIn exécute une requête connecté à une base précise.
	ExecIn(ctx context.Context, database, stmt string) error
	// Close libère la connexion.
	Close() error
}

// Sockets où chercher les moteurs. L'accès administrateur passe par le socket
// Unix : l'authentification par pair évite de stocker un mot de passe root.
var (
	mysqlSockets = []string{"/run/mysqld/mysqld.sock", "/var/run/mysqld/mysqld.sock", "/tmp/mysql.sock"}
	pgSocketDirs = []string{"/var/run/postgresql", "/run/postgresql", "/tmp"}
)

// ErrUnavailable indique qu'un moteur n'est pas joignable.
var ErrUnavailable = errors.New("moteur indisponible")

// FindMySQLSocket cherche le socket du serveur MySQL ou MariaDB.
func FindMySQLSocket(prefix string) (string, bool) {
	for _, s := range mysqlSockets {
		p := filepath.Join(prefix, s)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// Cluster décrit un cluster PostgreSQL détecté.
type Cluster struct {
	Version string
	Name    string
	Port    int
	Socket  string
}

// ID identifie un cluster sous la forme « 16/main ».
func (c Cluster) ID() string { return c.Version + "/" + c.Name }

// DetectClusters énumère les clusters PostgreSQL depuis /etc/postgresql. Le port
// est lu dans la configuration : un cluster secondaire n'écoute pas sur 5432, et
// s'y connecter par défaut viserait le mauvais serveur.
func DetectClusters(prefix string) []Cluster {
	root := filepath.Join(prefix, "/etc/postgresql")
	versions, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Cluster
	for _, v := range versions {
		if !v.IsDir() {
			continue
		}
		names, err := os.ReadDir(filepath.Join(root, v.Name()))
		if err != nil {
			continue
		}
		for _, n := range names {
			if !n.IsDir() {
				continue
			}
			c := Cluster{Version: v.Name(), Name: n.Name(), Port: 5432}
			if port, ok := readPort(filepath.Join(root, v.Name(), n.Name(), "postgresql.conf")); ok {
				c.Port = port
			}
			for _, dir := range pgSocketDirs {
				sock := filepath.Join(prefix, dir, fmt.Sprintf(".s.PGSQL.%d", c.Port))
				if _, err := os.Stat(sock); err == nil {
					c.Socket = filepath.Join(prefix, dir)
					break
				}
			}
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// readPort extrait la directive « port » d'un postgresql.conf.
func readPort(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "port") {
			continue
		}
		_, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if i := strings.IndexAny(value, "#\t "); i > 0 {
			value = value[:i]
		}
		if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			return n, true
		}
	}
	return 0, false
}

// sqlClient est l'implémentation commune sur database/sql.
type sqlClient struct {
	engine string
	db     *sql.DB
	dsn    func(database string) string
	port   int
}

func (c *sqlClient) Close() error { return c.db.Close() }

func (c *sqlClient) Exec(ctx context.Context, stmt string) error {
	if _, err := c.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("%s : %w", firstWords(stmt), err)
	}
	return nil
}

// ExecIn ouvre une connexion sur la base cible : les extensions PostgreSQL et le
// durcissement du schéma public ne peuvent pas être appliqués depuis une autre base.
func (c *sqlClient) ExecIn(ctx context.Context, database, stmt string) error {
	conn, err := sql.Open(driverFor(c.engine), c.dsn(database))
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("%s (base %s) : %w", firstWords(stmt), database, err)
	}
	return nil
}

func firstWords(stmt string) string {
	fields := strings.Fields(stmt)
	if len(fields) > 4 {
		fields = fields[:4]
	}
	return strings.Join(fields, " ")
}

func driverFor(engine string) string {
	if engine == "postgres" {
		return "pgx"
	}
	return "mysql"
}

// queryStrings exécute une requête ne renvoyant qu'une colonne de chaînes.
func (c *sqlClient) queryStrings(ctx context.Context, query string, args ...any) (map[string]bool, error) {
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}
