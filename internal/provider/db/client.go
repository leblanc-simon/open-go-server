package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

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
	// Grants renvoie les privilèges observés d'un compte, normalisés par base ;
	// les privilèges de niveau serveur, s'il y en a, sont rendus sous
	// GlobalGrantKey.
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
//
// /tmp n'y figure pas (D92) : inscriptible par tous, il laisse n'importe quel
// compte local — l'utilisateur d'un projet a un shell (D62) — y poser un socket
// au nom attendu. Tant que le vrai moteur tourne, rien ne se passe ; qu'il soit
// arrêté, le temps d'une mise à jour, et root se connectait au faux : il lui
// envoyait les mots de passe du parc, en clair dans les CREATE USER et ALTER
// ROLE, et le contenu de la base d'un clone. Aucun paquet Debian ni Ubuntu n'y
// place le socket d'un cluster décrit sous /etc/postgresql.
var (
	mysqlSockets = []string{"/run/mysqld/mysqld.sock", "/var/run/mysqld/mysqld.sock"}
	pgSocketDirs = []string{"/var/run/postgresql", "/run/postgresql"}
)

// Comptes système des moteurs : seuls eux, et root, peuvent poser un socket
// auquel ogs accepte de se connecter.
const (
	mysqlSystemUser    = "mysql"
	postgresSystemUser = "postgres"
)

// trustedIDs rend les uid et gid à qui un socket de moteur et son répertoire
// peuvent appartenir : root, le compte système du moteur — et l'identité qui
// exécute ogs, root en exploitation, l'utilisateur du développeur en mode
// préfixé. C'est une variable pour que les tests puissent la restreindre.
var trustedIDs = func(systemUser string) (uids, gids map[uint32]bool) {
	uids = map[uint32]bool{0: true, uint32(os.Getuid()): true}
	gids = map[uint32]bool{0: true, uint32(os.Getgid()): true}
	if u, err := user.Lookup(systemUser); err == nil {
		if id, err := strconv.ParseUint(u.Uid, 10, 32); err == nil {
			uids[uint32(id)] = true
		}
		if id, err := strconv.ParseUint(u.Gid, 10, 32); err == nil {
			gids[uint32(id)] = true
		}
	}
	return uids, gids
}

// checkSocket n'admet un socket que si personne d'autre que root ou le moteur
// n'a pu le poser (D92) : il doit appartenir à l'un d'eux, et son répertoire
// aussi, sans être inscriptible par les autres — sticky bit ou non — ni par un
// groupe qui ne serait pas le leur. Le répertoire Debian de PostgreSQL est en
// 2775 postgres:postgres, d'où l'admission du groupe du moteur.
//
// Le chemin est examiné sans suivre le socket lui-même ; son répertoire est
// résolu, /var/run étant un lien de root vers /run.
func checkSocket(path, systemUser string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	dinfo, err := os.Stat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	dst, dok := dinfo.Sys().(*syscall.Stat_t)
	if !ok || !dok {
		return fmt.Errorf("%s : propriétaire illisible", path)
	}
	uids, gids := trustedIDs(systemUser)
	return judgeSocket(socketFacts{
		path: path, dir: dir, link: info.Mode()&os.ModeSymlink != 0, uid: st.Uid,
		dirUID: dst.Uid, dirGID: dst.Gid, dirPerm: dinfo.Mode().Perm(),
	}, systemUser, uids, gids)
}

// socketFacts est ce que checkSocket relève d'un socket et de son répertoire.
type socketFacts struct {
	path, dir      string
	link           bool
	uid            uint32
	dirUID, dirGID uint32
	dirPerm        os.FileMode
}

// judgeSocket rend le verdict de checkSocket sur les faits relevés : séparé du
// relevé, chaque motif de refus s'éprouve sans changer de propriétaire.
func judgeSocket(f socketFacts, systemUser string, uids, gids map[uint32]bool) error {
	switch {
	case f.link:
		return fmt.Errorf("%s est un lien symbolique", f.path)
	case !uids[f.uid]:
		return fmt.Errorf("%s appartient à l'uid %d, ni root ni %s", f.path, f.uid, systemUser)
	case !uids[f.dirUID]:
		return fmt.Errorf("le répertoire %s appartient à l'uid %d, ni root ni %s", f.dir, f.dirUID, systemUser)
	case f.dirPerm&0o002 != 0:
		return fmt.Errorf("le répertoire %s est inscriptible par tous (%04o)", f.dir, f.dirPerm)
	case f.dirPerm&0o020 != 0 && !gids[f.dirGID]:
		return fmt.Errorf("le répertoire %s est inscriptible par le groupe %d, ni root ni %s",
			f.dir, f.dirGID, systemUser)
	}
	return nil
}

// procNet est le répertoire des tables de sockets du noyau ; les tests le
// remplacent par des tables écrites à la main.
var procNet = "/proc/net"

// checkTCPListener n'admet une connexion à target (127.0.0.1 ou ::1) sur ce
// port que si une écoute du moteur la recevra, et qu'aucune écoute d'un autre
// compte ne le peut (D92). Le port d'un moteur arrêté, ou qui n'écoute pas sur
// cette famille d'adresses (listen_addresses = ” ou '::1'), est libre : tout
// compte local peut s'y lier — un port au-dessus de 1024 ne demande aucun
// privilège — et recevoir le mot de passe que le client lui présente.
// /proc/net/tcp et tcp6 portent l'uid de chaque socket en écoute.
//
// Une écoute sur « :: » reçoit aussi l'IPv4 si elle n'est pas V6ONLY, ce que
// /proc ne dit pas. Elle est donc toujours tenue pour capable de recevoir — une
// écoute étrangère sur « :: » fait refuser —, mais ne compte comme l'écoute du
// moteur pour 127.0.0.1 que si le moteur ouvre une écoute double pile : MySQL
// sur « :: » ou « * », quand net.ipv6.bindv6only vaut 0 ; jamais PostgreSQL,
// qui pose V6ONLY sur toutes ses écoutes IPv6.
func checkTCPListener(port int, systemUser, target string) error {
	uids, _ := trustedIDs(systemUser)
	dualStack := systemUser == mysqlSystemUser && bindV6Only() == "0"
	found := false
	for _, table := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile(filepath.Join(procNet, table))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			fields := strings.Fields(line)
			// sl local rem st tx:rx tr:when retrnsmt uid …
			if len(fields) < 8 || fields[3] != "0A" {
				continue
			}
			addr, hexPort, ok := strings.Cut(fields[1], ":")
			if !ok {
				continue
			}
			if p, err := strconv.ParseUint(hexPort, 16, 32); err != nil || int(p) != port {
				continue
			}
			reaches, counts := listenerReaches(table, addr, target, dualStack)
			if !reaches {
				continue
			}
			uid, err := strconv.ParseUint(fields[7], 10, 32)
			if err != nil || !uids[uint32(uid)] {
				return fmt.Errorf("%w : le port %d de %s peut être reçu par une écoute de l'uid %s, ni root ni %s — "+
					"ogs ne lui présente pas de mot de passe", ErrUnavailable, port, target, fields[7], systemUser)
			}
			found = found || counts
		}
	}
	if !found {
		return fmt.Errorf("%w : le moteur n'écoute pas sur %s, port %d — ogs ne présente pas de mot de passe à "+
			"un port que tout compte local peut prendre", ErrUnavailable, target, port)
	}
	return nil
}

// Adresses de /proc/net/tcp* en notation hexadécimale.
const (
	hexV4Loopback = "0100007F"                         // 127.0.0.1
	hexV4Any      = "00000000"                         // 0.0.0.0
	hexV6Any      = "00000000000000000000000000000000" // ::
	hexV6Loopback = "00000000000000000000000001000000" // ::1
	hexV6Mapped   = "0000000000000000FFFF00000100007F" // ::ffff:127.0.0.1
)

// listenerReaches dit si une écoute à cette adresse peut recevoir une
// connexion vers target (reaches), et si elle vaut écoute du moteur pour elle
// (counts) : voir checkTCPListener pour « :: ».
func listenerReaches(table, addr, target string, dualStack bool) (reaches, counts bool) {
	switch {
	case target == "::1" && table == "tcp6":
		ok := addr == hexV6Loopback || addr == hexV6Any
		return ok, ok
	case target == "::1":
		return false, false
	case table == "tcp":
		ok := addr == hexV4Loopback || addr == hexV4Any
		return ok, ok
	case addr == hexV6Mapped:
		return true, true
	case addr == hexV6Any:
		return true, dualStack
	}
	return false, false
}

// bindV6Only lit net.ipv6.bindv6only ; les tests la remplacent.
var bindV6Only = func() string {
	data, err := os.ReadFile(filepath.Join(procSys, "net/ipv6/bindv6only"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// procSys est la racine des réglages du noyau.
const procSys = "/proc/sys"

// ErrUnavailable indique qu'un moteur n'est pas joignable.
var ErrUnavailable = errors.New("moteur indisponible")

// GlobalGrantKey est la clé sous laquelle Grants range les privilèges accordés
// « ON *.* ». Ce n'est pas un nom de base : la liste blanche des identifiants
// refuse l'astérisque, donc la réconciliation des comptes de projet, qui
// interroge la carte par nom de base, ne la rencontre jamais.
const GlobalGrantKey = "*"

// FindMySQLSocket cherche le socket du serveur MySQL ou MariaDB. Un socket que
// checkSocket refuse n'est pas retenu : le moteur est alors indisponible, et la
// supervision le tient pour absent.
func FindMySQLSocket(prefix string) (string, bool) {
	socket, _ := findMySQLSocket(prefix)
	return socket, socket != ""
}

// MySQLSocket distingue l'absence de socket — le moteur n'est pas installé, ou
// arrêté : ni socket ni erreur — d'un socket vu puis refusé, rendu en erreur
// pour que l'appelant l'annonce au lieu de tenir le moteur pour absent.
func MySQLSocket(prefix string) (string, error) {
	socket, refused := findMySQLSocket(prefix)
	if socket == "" && len(refused) > 0 {
		return "", mysqlUnavailable(refused)
	}
	return socket, nil
}

// mysqlUnavailable décrit l'absence de socket MySQL admis.
func mysqlUnavailable(refused []string) error {
	if len(refused) > 0 {
		return fmt.Errorf("%w : socket MySQL refusé — %s", ErrUnavailable, strings.Join(refused, " ; "))
	}
	return fmt.Errorf("%w : aucun socket MySQL trouvé (%s) — le serveur est-il installé et démarré ?",
		ErrUnavailable, strings.Join(mysqlSockets, ", "))
}

// findMySQLSocket rend le premier socket admis, et les motifs de refus des
// autres, pour que l'indisponibilité dise pourquoi.
func findMySQLSocket(prefix string) (string, []string) {
	var refused []string
	for _, s := range mysqlSockets {
		p := filepath.Join(prefix, s)
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		if err := checkSocket(p, mysqlSystemUser); err != nil {
			refused = append(refused, err.Error())
			continue
		}
		return p, nil
	}
	return "", refused
}

// Cluster décrit un cluster PostgreSQL détecté.
type Cluster struct {
	Version string
	Name    string
	Port    int
	Socket  string
	// Refused porte les motifs de refus des sockets écartés par checkSocket.
	Refused []string
}

// ID identifie un cluster sous la forme « 16/main ».
func (c Cluster) ID() string { return c.Version + "/" + c.Name }

// SocketDir rend le répertoire du socket d'administration du cluster, ou le
// répertoire par défaut quand aucun socket n'a été vu — le moteur est peut-être
// arrêté, la connexion le dira. Il refuse de retomber sur ce défaut quand un
// socket y a été vu et écarté (checkSocket) : ce serait s'y connecter quand même.
func (c Cluster) SocketDir(prefix string) (string, error) {
	if c.Socket != "" {
		return c.Socket, nil
	}
	if len(c.Refused) > 0 {
		return "", fmt.Errorf("%w : cluster %s, socket refusé — %s", ErrUnavailable, c.ID(),
			strings.Join(c.Refused, " ; "))
	}
	return filepath.Join(prefix, "/var/run/postgresql"), nil
}

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
				if _, err := os.Lstat(sock); err != nil {
					continue
				}
				if err := checkSocket(sock, postgresSystemUser); err != nil {
					c.Refused = append(c.Refused, err.Error())
					continue
				}
				c.Socket = filepath.Join(prefix, dir)
				break
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
