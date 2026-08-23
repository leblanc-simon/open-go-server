// Package db porte les providers de bases de données : génération des requêtes,
// observation de l'état réel et réconciliation.
package db

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/leviathan/ogs/internal/naming"
)

// reIdentifier est la liste blanche des identifiants d'objets SQL. Les noms
// d'objets ne sont pas paramétrables en SQL : cette validation est la seule
// défense contre l'injection par nom de base ou d'utilisateur (§15). Elle est
// donc appliquée à chaque génération, et pas seulement à la validation du YAML.
var reIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// checkIdentifier refuse tout identifiant hors liste blanche.
func checkIdentifier(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%s : nom vide", kind)
	}
	if !reIdentifier.MatchString(name) {
		return fmt.Errorf("%s %q invalide : caractères autorisés [a-zA-Z0-9_]", kind, name)
	}
	return nil
}

// Privilege est un jeu de privilèges nommé.
type Privilege string

const (
	PrivAll      Privilege = "all"
	PrivCRUD     Privilege = "crud"
	PrivReadOnly Privilege = "readonly"
)

// ParsePrivilege valide un jeu de privilèges.
func ParsePrivilege(v string) (Privilege, error) {
	switch Privilege(strings.ToLower(strings.TrimSpace(v))) {
	case "", PrivAll:
		return PrivAll, nil
	case PrivCRUD:
		return PrivCRUD, nil
	case PrivReadOnly:
		return PrivReadOnly, nil
	}
	return "", fmt.Errorf("jeu de privilèges %q inconnu (all, crud ou readonly)", v)
}

// mysqlPrivileges traduit un jeu nommé en privilèges MySQL.
func mysqlPrivileges(p Privilege) string {
	switch p {
	case PrivCRUD:
		return "SELECT, INSERT, UPDATE, DELETE"
	case PrivReadOnly:
		return "SELECT, SHOW VIEW"
	default:
		return "ALL PRIVILEGES"
	}
}

// quoteMySQLIdent protège un identifiant MySQL. L'identifiant étant déjà passé
// par la liste blanche, le doublement des accents graves est une précaution
// supplémentaire plutôt qu'une nécessité.
func quoteMySQLIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// quotePGIdent protège un identifiant PostgreSQL.
func quotePGIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteLiteral protège une chaîne littérale SQL (mot de passe, nom d'hôte).
func quoteLiteral(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// MySQLStatements décrit les requêtes de création d'une base et de son utilisateur.
type MySQLStatements struct {
	Database []string
	User     []string
	Grants   []string
}

// All renvoie les requêtes dans l'ordre d'exécution.
func (s MySQLStatements) All() []string {
	out := make([]string, 0, len(s.Database)+len(s.User)+len(s.Grants)+1)
	out = append(out, s.Database...)
	out = append(out, s.User...)
	out = append(out, s.Grants...)
	return out
}

// MySQLSpec décrit ce qu'il faut créer côté MySQL.
type MySQLSpec struct {
	Database   string
	Charset    string
	Collation  string
	User       string
	Host       string
	Password   string
	Privileges Privilege
	AuthPlugin string
}

// normalize complète les valeurs par défaut.
func (s *MySQLSpec) normalize() error {
	if err := checkIdentifier("nom de base", s.Database); err != nil {
		return err
	}
	if err := checkIdentifier("nom d'utilisateur", s.User); err != nil {
		return err
	}
	if len(s.User) > naming.MaxIdentifier {
		return fmt.Errorf("nom d'utilisateur %q trop long (%d > %d, limite de MySQL 8)",
			s.User, len(s.User), naming.MaxIdentifier)
	}
	if s.Charset == "" {
		s.Charset = "utf8mb4"
	}
	if s.Collation == "" {
		s.Collation = "utf8mb4_unicode_ci"
	}
	if s.Host == "" {
		s.Host = "localhost"
	}
	if s.Privileges == "" {
		s.Privileges = PrivAll
	}
	if !reIdentifier.MatchString(strings.ReplaceAll(s.Charset, "-", "")) ||
		!reIdentifier.MatchString(s.Collation) {
		return fmt.Errorf("jeu de caractères ou collation invalide (%q / %q)", s.Charset, s.Collation)
	}
	return nil
}

// CreateMySQL produit les requêtes de création. Elles sont idempotentes : le
// moteur de réconciliation peut les rejouer sans conséquence.
func CreateMySQL(spec MySQLSpec) (MySQLStatements, error) {
	var out MySQLStatements
	if err := spec.normalize(); err != nil {
		return out, err
	}
	out.Database = []string{fmt.Sprintf(
		"CREATE DATABASE IF NOT EXISTS %s CHARACTER SET %s COLLATE %s",
		quoteMySQLIdent(spec.Database), spec.Charset, spec.Collation)}

	identified := ""
	if spec.Password != "" {
		identified = " IDENTIFIED BY " + quoteLiteral(spec.Password)
		if spec.AuthPlugin != "" && spec.AuthPlugin != "auto" {
			if err := checkIdentifier("plugin d'authentification", spec.AuthPlugin); err != nil {
				return out, err
			}
			identified = fmt.Sprintf(" IDENTIFIED WITH %s BY %s", spec.AuthPlugin, quoteLiteral(spec.Password))
		}
	}
	account := fmt.Sprintf("%s@%s", quoteLiteral(spec.User), quoteLiteral(spec.Host))
	out.User = []string{fmt.Sprintf("CREATE USER IF NOT EXISTS %s%s", account, identified)}

	grant, err := GrantMySQL(spec.Database, spec.User, spec.Host, spec.Privileges)
	if err != nil {
		return out, err
	}
	out.Grants = []string{grant, "FLUSH PRIVILEGES"}
	return out, nil
}

// GrantMySQL produit un GRANT. L'échappement du motif est le point critique :
// l'underscore est un joker dans les privilèges MySQL, donc GRANT ... ON
// `a_b`.* porterait aussi sur aXb (§3.3.5).
func GrantMySQL(database, user, host string, priv Privilege) (string, error) {
	if err := checkIdentifier("nom de base", database); err != nil {
		return "", err
	}
	if err := checkIdentifier("nom d'utilisateur", user); err != nil {
		return "", err
	}
	if host == "" {
		host = "localhost"
	}
	pattern := naming.EscapeMySQLPattern(database)
	return fmt.Sprintf("GRANT %s ON %s.* TO %s@%s",
		mysqlPrivileges(priv), quoteMySQLIdent(pattern), quoteLiteral(user), quoteLiteral(host)), nil
}

// RevokeMySQL retire tous les privilèges d'un utilisateur sur une base.
func RevokeMySQL(database, user, host string) (string, error) {
	if err := checkIdentifier("nom de base", database); err != nil {
		return "", err
	}
	if err := checkIdentifier("nom d'utilisateur", user); err != nil {
		return "", err
	}
	if host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("REVOKE ALL PRIVILEGES ON %s.* FROM %s@%s",
		quoteMySQLIdent(naming.EscapeMySQLPattern(database)), quoteLiteral(user), quoteLiteral(host)), nil
}

// SetMySQLPassword produit la requête de rotation de mot de passe.
func SetMySQLPassword(user, host, password, plugin string) (string, error) {
	if err := checkIdentifier("nom d'utilisateur", user); err != nil {
		return "", err
	}
	if host == "" {
		host = "localhost"
	}
	if plugin != "" && plugin != "auto" {
		if err := checkIdentifier("plugin d'authentification", plugin); err != nil {
			return "", err
		}
		return fmt.Sprintf("ALTER USER %s@%s IDENTIFIED WITH %s BY %s",
			quoteLiteral(user), quoteLiteral(host), plugin, quoteLiteral(password)), nil
	}
	return fmt.Sprintf("ALTER USER %s@%s IDENTIFIED BY %s",
		quoteLiteral(user), quoteLiteral(host), quoteLiteral(password)), nil
}

// DropMySQL produit les requêtes de suppression, base puis utilisateur.
func DropMySQL(database, user, host string) ([]string, error) {
	if err := checkIdentifier("nom de base", database); err != nil {
		return nil, err
	}
	out := []string{fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteMySQLIdent(database))}
	if user != "" {
		if err := checkIdentifier("nom d'utilisateur", user); err != nil {
			return nil, err
		}
		if host == "" {
			host = "localhost"
		}
		out = append(out, fmt.Sprintf("DROP USER IF EXISTS %s@%s", quoteLiteral(user), quoteLiteral(host)))
	}
	return out, nil
}

// PostgresSpec décrit ce qu'il faut créer côté PostgreSQL.
type PostgresSpec struct {
	Database   string
	Role       string
	Password   string
	Encoding   string
	Collate    string
	Extensions []string
	Privileges Privilege
}

func (s *PostgresSpec) normalize() error {
	if err := checkIdentifier("nom de base", s.Database); err != nil {
		return err
	}
	if err := checkIdentifier("nom de rôle", s.Role); err != nil {
		return err
	}
	if s.Encoding == "" {
		s.Encoding = "UTF8"
	}
	if s.Privileges == "" {
		s.Privileges = PrivAll
	}
	return nil
}

// PostgresStatements sépare les requêtes selon leur contexte d'exécution :
// CREATE DATABASE n'est pas transactionnel et les extensions exigent une
// connexion à la base cible.
type PostgresStatements struct {
	// Cluster s'exécute sur la base d'administration, hors transaction.
	Cluster []string
	// InDatabase s'exécute connecté à la base créée.
	InDatabase []string
}

// CreatePostgres produit les requêtes de création. PostgreSQL n'ayant pas de
// « CREATE ... IF NOT EXISTS » pour les rôles et les bases, l'idempotence est
// obtenue par l'observation préalable, pas par la requête elle-même.
func CreatePostgres(spec PostgresSpec, roleExists, dbExists bool) (PostgresStatements, error) {
	var out PostgresStatements
	if err := spec.normalize(); err != nil {
		return out, err
	}
	if !roleExists {
		stmt := fmt.Sprintf("CREATE ROLE %s LOGIN", quotePGIdent(spec.Role))
		if spec.Password != "" {
			stmt += " PASSWORD " + quoteLiteral(spec.Password)
		}
		out.Cluster = append(out.Cluster, stmt)
	}
	if !dbExists {
		stmt := fmt.Sprintf("CREATE DATABASE %s OWNER %s ENCODING %s",
			quotePGIdent(spec.Database), quotePGIdent(spec.Role), quoteLiteral(spec.Encoding))
		if spec.Collate != "" {
			// TEMPLATE template0 est requis dès qu'on impose une collation.
			stmt += fmt.Sprintf(" LC_COLLATE %s LC_CTYPE %s TEMPLATE template0",
				quoteLiteral(spec.Collate), quoteLiteral(spec.Collate))
		}
		out.Cluster = append(out.Cluster, stmt)
	}

	// Durcissement du schéma public : depuis PostgreSQL 15, il n'est plus
	// inscriptible par PUBLIC, mais on l'applique explicitement pour homogénéiser
	// le comportement entre les versions gérées.
	out.InDatabase = append(out.InDatabase,
		"REVOKE ALL ON SCHEMA public FROM PUBLIC",
		fmt.Sprintf("GRANT ALL ON SCHEMA public TO %s", quotePGIdent(spec.Role)),
		fmt.Sprintf("ALTER SCHEMA public OWNER TO %s", quotePGIdent(spec.Role)),
	)
	exts := append([]string(nil), spec.Extensions...)
	sort.Strings(exts)
	for _, ext := range exts {
		if !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(ext) {
			return out, fmt.Errorf("nom d'extension %q invalide", ext)
		}
		out.InDatabase = append(out.InDatabase,
			fmt.Sprintf("CREATE EXTENSION IF NOT EXISTS %s", quotePGIdent(ext)))
	}
	return out, nil
}

// SetPostgresPassword produit la requête de rotation de mot de passe.
func SetPostgresPassword(role, password string) (string, error) {
	if err := checkIdentifier("nom de rôle", role); err != nil {
		return "", err
	}
	return fmt.Sprintf("ALTER ROLE %s WITH PASSWORD %s", quotePGIdent(role), quoteLiteral(password)), nil
}

// DropPostgres produit les requêtes de suppression. WITH (FORCE) termine les
// connexions restantes ; il n'existe qu'à partir de PostgreSQL 13.
func DropPostgres(database, role string, supportsForce bool) ([]string, error) {
	if err := checkIdentifier("nom de base", database); err != nil {
		return nil, err
	}
	drop := fmt.Sprintf("DROP DATABASE IF EXISTS %s", quotePGIdent(database))
	if supportsForce {
		drop += " WITH (FORCE)"
	}
	out := []string{drop}
	if role != "" {
		if err := checkIdentifier("nom de rôle", role); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("DROP ROLE IF EXISTS %s", quotePGIdent(role)))
	}
	return out, nil
}
