// Package db porte les providers de bases de données : génération des requêtes,
// observation de l'état réel et réconciliation.
package db

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"leblanc.io/open-go-server/internal/naming"
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

// mysqlObservedPrivileges rend le jeu attendu tel qu'il ressort de l'observation,
// privilège par privilège. Pour « all », c'est un minimum vérifiable et non la
// liste complète : « ALL PRIVILEGES » se relit développé, et la liste diffère
// entre MySQL et MariaDB comme d'une version à l'autre — exiger l'égalité
// rejouerait éternellement un GRANT déjà appliqué (même piège que D67).
func mysqlObservedPrivileges(p Privilege) []string {
	switch p {
	case PrivCRUD:
		return []string{"SELECT", "INSERT", "UPDATE", "DELETE"}
	case PrivReadOnly:
		return []string{"SELECT", "SHOW VIEW"}
	default:
		return []string{"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP", "ALTER", "INDEX"}
	}
}

// parseGrantSet découpe la chaîne rendue par SCHEMA_PRIVILEGES ou
// USER_PRIVILEGES, où GROUP_CONCAT sépare les privilèges par des virgules.
func parseGrantSet(observed string) map[string]bool {
	out := map[string]bool{}
	for _, p := range strings.Split(observed, ",") {
		if p = strings.ToUpper(strings.TrimSpace(p)); p != "" {
			out[p] = true
		}
	}
	return out
}

// MySQLGrantsSatisfied compare les privilèges observés sur une base au jeu
// demandé. La seule présence d'un privilège ne suffit pas : un passage de
// « all » à « readonly » resterait un NoOp éternel, et le plan annoncerait un
// état qu'il n'a jamais appliqué (§7.2).
//
// Les jeux bornés — crud, readonly — se comparent par égalité : tout privilège
// en trop est un écart, c'est le sens même d'un jeu borné. « all » se compare par
// inclusion, pour la raison donnée à mysqlObservedPrivileges.
func MySQLGrantsSatisfied(observed string, p Privilege) bool {
	if observed == "" {
		return false
	}
	got := parseGrantSet(observed)
	want := mysqlObservedPrivileges(p)
	for _, w := range want {
		if !got[w] {
			return false
		}
	}
	return p == PrivAll || len(got) == len(want)
}

// MySQLGrantsOverflow dit si les privilèges observés débordent le jeu demandé.
// C'est la seule condition qui justifie un REVOKE : le GRANT de MySQL est
// additif, donc une réduction de privilèges laisserait INSERT, UPDATE et DROP en
// place — mais révoquer sans débordement ferait échouer la requête
// (ER_NONEXISTING_GRANT) sur un compte qui n'a encore aucun privilège de base.
// « all » ne déborde jamais : sa liste observée n'est pas stable d'un moteur à
// l'autre, et l'y soumettre ferait osciller le plan.
func MySQLGrantsOverflow(observed string, p Privilege) bool {
	if observed == "" || p == PrivAll {
		return false
	}
	want := map[string]bool{}
	for _, w := range mysqlObservedPrivileges(p) {
		want[w] = true
	}
	for g := range parseGrantSet(observed) {
		if !want[g] {
			return true
		}
	}
	return false
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
	// Revoke fait précéder le GRANT d'une révocation. Il ne se décide pas ici
	// mais à l'observation : GRANT est additif, et seule une réduction réelle du
	// jeu demandé justifie de retirer d'abord ce qui est en place.
	Revoke bool
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
	if spec.Revoke {
		// La révocation précède le grant, dans la même étape : elle n'a de sens que
		// suivie de ce qui doit rester, et les deux requêtes séparées laisseraient
		// une fenêtre où l'application n'aurait plus aucun droit.
		revoke, err := RevokeMySQL(spec.Database, spec.User, spec.Host)
		if err != nil {
			return out, err
		}
		out.Grants = append(out.Grants, revoke)
	}
	out.Grants = append(out.Grants, grant, "FLUSH PRIVILEGES")
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
func DropMySQL(database, user, host string, extra []string) ([]string, error) {
	if err := checkIdentifier("nom de base", database); err != nil {
		return nil, err
	}
	out := []string{fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteMySQLIdent(database))}
	if host == "" {
		host = "localhost"
	}
	// Les comptes additionnels partent avec la base : un compte laissé derrière
	// garderait la faculté de s'authentifier, avec un mot de passe que le keystore
	// ne conserve plus.
	for _, u := range append([]string{user}, extra...) {
		if u == "" {
			continue
		}
		if err := checkIdentifier("nom d'utilisateur", u); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("DROP USER IF EXISTS %s@%s", quoteLiteral(u), quoteLiteral(host)))
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
			stmt += fmt.Sprintf(" LC_COLLATE %s LC_CTYPE %s",
				quoteLiteral(spec.Collate), quoteLiteral(spec.Collate))
		}
		// TEMPLATE template0 systématiquement : un encodage explicite est refusé
		// dès qu'il diffère de celui de template1, ce qui arrive sur tout cluster
		// initialisé sans locale — la création échouerait alors avec un message
		// obscur sur la base modèle.
		stmt += " TEMPLATE template0"
		out.Cluster = append(out.Cluster, stmt)
	}

	// Isolation de la base. PostgreSQL accorde CONNECT à PUBLIC sur toute base
	// nouvelle : sans cette révocation, le rôle de n'importe quel projet peut se
	// connecter à la base d'un autre — y occuper une connexion, et lire du
	// catalogue ce que les privilèges ne filtrent pas. Les deux requêtes sont
	// idempotentes, donc rejouées à chaque apply : une base créée avant cette
	// correction se trouve durcie au passage.
	out.Cluster = append(out.Cluster,
		fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM PUBLIC", quotePGIdent(spec.Database)),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s",
			quotePGIdent(spec.Database), quotePGIdent(spec.Role)),
	)

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

// PostgresExtraUser produit les requêtes d'un compte additionnel sur une base
// existante : un rôle de lecture pour une réplication, un compte d'intégration.
// Les privilèges portent sur les objets présents et sur ceux à venir, sans quoi
// le compte perdrait l'accès à la première table créée par l'application.
func PostgresExtraUser(database, owner, role, password string, priv Privilege) (PostgresStatements, error) {
	var out PostgresStatements
	if err := checkIdentifier("nom de base", database); err != nil {
		return out, err
	}
	if err := checkIdentifier("nom de rôle", role); err != nil {
		return out, err
	}
	if err := checkIdentifier("nom de rôle", owner); err != nil {
		return out, err
	}
	if priv == "" {
		priv = PrivAll
	}
	stmt := fmt.Sprintf("CREATE ROLE %s LOGIN", quotePGIdent(role))
	if password != "" {
		stmt += " PASSWORD " + quoteLiteral(password)
	}
	out.Cluster = append(out.Cluster, stmt,
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s",
			quotePGIdent(database), quotePGIdent(role)))

	var sur, defauts string
	switch priv {
	case PrivAll:
		sur, defauts = "ALL PRIVILEGES", "ALL PRIVILEGES"
	case PrivCRUD:
		sur, defauts = "SELECT, INSERT, UPDATE, DELETE", "SELECT, INSERT, UPDATE, DELETE"
	case PrivReadOnly:
		sur, defauts = "SELECT", "SELECT"
	default:
		return out, fmt.Errorf("jeu de privilèges %q inconnu", priv)
	}
	q := quotePGIdent(role)
	out.InDatabase = append(out.InDatabase,
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", q),
		fmt.Sprintf("GRANT %s ON ALL TABLES IN SCHEMA public TO %s", sur, q),
		fmt.Sprintf("GRANT %s ON ALL SEQUENCES IN SCHEMA public TO %s",
			map[Privilege]string{PrivAll: "ALL PRIVILEGES", PrivCRUD: "USAGE, SELECT",
				PrivReadOnly: "SELECT"}[priv], q),
		// Les objets créés plus tard par le propriétaire doivent l'être avec les
		// mêmes droits, faute de quoi le compte devient aveugle à la prochaine
		// migration.
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT %s ON TABLES TO %s",
			quotePGIdent(owner), defauts, q),
	)
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
func DropPostgres(database, role string, extra []string, supportsForce bool) ([]string, error) {
	if err := checkIdentifier("nom de base", database); err != nil {
		return nil, err
	}
	drop := fmt.Sprintf("DROP DATABASE IF EXISTS %s", quotePGIdent(database))
	if supportsForce {
		drop += " WITH (FORCE)"
	}
	out := []string{drop}
	// La base d'abord, les rôles ensuite : leurs privilèges et privilèges par
	// défaut vivent dans la base, et disparaissent avec elle. Un rôle laissé
	// derrière conserverait la faculté de se connecter au cluster.
	for _, r := range append([]string{role}, extra...) {
		if r == "" {
			continue
		}
		if err := checkIdentifier("nom de rôle", r); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("DROP ROLE IF EXISTS %s", quotePGIdent(r)))
	}
	return out, nil
}

// MonitoringMySQL produit les requêtes du compte de supervision. Les privilèges
// sont ceux dont l'agent a besoin et rien de plus : PROCESS pour la liste des
// connexions, REPLICATION CLIENT pour la position des journaux, et la lecture du
// performance_schema d'où viennent presque toutes les métriques. Aucun accès aux
// bases des projets : un agent de métriques compromis ne doit pas être un accès
// aux données de clients.
func MonitoringMySQL(user, host, password string) ([]string, error) {
	if err := checkIdentifier("utilisateur de supervision", user); err != nil {
		return nil, err
	}
	if host == "" {
		host = "localhost"
	}
	account := fmt.Sprintf("%s@%s", quoteLiteral(user), quoteLiteral(host))
	return []string{
		fmt.Sprintf("CREATE USER IF NOT EXISTS %s IDENTIFIED BY %s", account, quoteLiteral(password)),
		fmt.Sprintf("ALTER USER %s IDENTIFIED BY %s", account, quoteLiteral(password)),
		fmt.Sprintf("GRANT PROCESS, REPLICATION CLIENT ON *.* TO %s", account),
		// Le nom de base placé après ON est un motif : sans échappement, le
		// privilège porterait aussi sur « performanceXschema ». Même règle que
		// GrantMySQL (§3.3.5).
		fmt.Sprintf("GRANT SELECT ON %s.* TO %s",
			quoteMySQLIdent(naming.EscapeMySQLPattern(MonitoringMySQLSchema)), account),
	}, nil
}

// MonitoringMySQLSchema est la seule base que le compte de supervision peut lire :
// c'est de là que viennent presque toutes les métriques du moteur.
const MonitoringMySQLSchema = "performance_schema"

// MonitoringMySQLLegacyRevoke retire le privilège hérité du compte de
// supervision. Les versions posant « GRANT SELECT ON `performance_schema`.* »
// sans échappement accordaient un **motif**, où le souligné est un joker : le
// compte lit aussi une base nommée « performanceXschema », qu'un projet adopté
// peut porter — l'inverse de ce que promet D67. GRANT étant additif, la ligne de
// mysql.db survit à tous les apply suivants : seul un REVOKE la retire, et il
// vise le motif **littéral**, tel qu'il a été stocké.
//
// Elle ne figure pas dans MonitoringMySQL, dont chaque requête doit réussir :
// celle-ci rend ER_NONEXISTING_GRANT partout où la ligne n'existe pas, c'est-à-dire
// sur toute installation postérieure à l'échappement. Son échec est le cas
// nominal, et l'appelant doit le traiter comme tel — la ligne héritée ne peut pas
// être cherchée d'abord, la relecture des privilèges ramenant les deux motifs
// sous la même clé (voir mysqlClient.Grants).
func MonitoringMySQLLegacyRevoke(user, host string) (string, error) {
	if err := checkIdentifier("utilisateur de supervision", user); err != nil {
		return "", err
	}
	if host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("REVOKE SELECT ON %s.* FROM %s@%s",
		quoteMySQLIdent(MonitoringMySQLSchema), quoteLiteral(user), quoteLiteral(host)), nil
}

// MonitoringMySQLGrants énumère les privilèges **globaux** attendus du compte de
// supervision, chaque entrée donnant les orthographes acceptées.
//
// Il en faut plusieurs : **MariaDB rend « BINLOG MONITOR » là où MySQL rend
// « REPLICATION CLIENT »**. Le GRANT est accepté sous les deux noms, mais la
// relecture ne rend que le nom propre au moteur — comparer au seul libellé de
// MySQL faisait voir un privilège manquant à chaque observation, et le plan
// annonçait éternellement une correction qu'il venait d'appliquer.
//
// Ils se comparent à la seule entrée GlobalGrantKey de Grants : accordés
// « ON *.* », ils ne figurent dans aucune ligne par base.
var MonitoringMySQLGrants = [][]string{
	{"PROCESS"},
	{"REPLICATION CLIENT", "BINLOG MONITOR"},
}

// MonitoringMySQLSchemaGrants énumère les privilèges attendus **par base**. Les
// chercher dans l'ensemble concaténé rendrait un SELECT accordé sur n'importe
// quelle base suffisant — y compris celle d'un projet.
var MonitoringMySQLSchemaGrants = map[string][]string{
	MonitoringMySQLSchema: {"SELECT"},
}

// MonitoringPostgres produit les requêtes du rôle de supervision. Il n'a pas de
// mot de passe : l'agent se connecte par le socket local, où PostgreSQL
// authentifie « peer » — il compare l'utilisateur système. C'est la même règle
// que pour le rôle d'administration d'ogs (D44), et la conséquence est la même :
// le rôle porte le nom de l'utilisateur système de l'agent, pas un nom choisi.
func MonitoringPostgres(role string) ([]string, error) {
	if err := checkIdentifier("rôle de supervision", role); err != nil {
		return nil, err
	}
	ident := quotePGIdent(role)
	return []string{
		fmt.Sprintf("DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = %s) "+
			"THEN CREATE ROLE %s LOGIN; END IF; END $$", quoteLiteral(role), ident),
		fmt.Sprintf("ALTER ROLE %s LOGIN", ident),
		// pg_monitor agrège pg_read_all_settings, pg_read_all_stats et
		// pg_stat_scan_tables : c'est exactement la lecture des statistiques, sans
		// aucun droit sur les données.
		fmt.Sprintf("GRANT pg_monitor TO %s", ident),
	}, nil
}
