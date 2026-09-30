package amqp

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"
)

// DefaultBrokerAccount est le compte système sous lequel le paquet Debian fait
// tourner RabbitMQ.
const DefaultBrokerAccount = "rabbitmq"

// procNetTCP énumère les tables de sockets TCP du noyau. Une connexion vers
// 127.0.0.1 aboutit dans la première, ou dans la seconde, en adresse IPv4
// projetée, quand le serveur écoute sur « :: ».
var procNetTCP = []string{"/proc/net/tcp", "/proc/net/tcp6"}

// ErrUntrustedPeer signale un port de la boucle locale tenu par un autre compte
// que celui du broker.
var ErrUntrustedPeer = errors.New("port de management tenu par un autre compte que le broker")

// peerGuard vérifie, pour chaque connexion vers cette machine, qui tient l'autre
// bout (D93). Le port 15672 n'est pas privilégié : quand le broker ou son plugin
// de management est arrêté — une mise à jour, un redémarrage —, tout compte local
// peut l'ouvrir, et l'utilisateur d'un projet a un shell (D62). ogs lui enverrait
// alors, en authentification Basic, les identifiants d'administration du broker,
// et dans le corps des requêtes le mot de passe scellé de chaque compte de projet.
//
// Le contrôle porte sur la connexion établie, non sur l'écoute : le noyau relève
// dans /proc/net/tcp l'uid de chaque socket, et le socket accepté hérite de celui
// du socket d'écoute. Chercher l'autre bout de sa propre connexion ne laisse
// aucune fenêtre entre le contrôle et l'envoi, là où relire l'écoute puis se
// connecter en laisserait une — le broker qui s'arrête entre les deux. Sont admis
// root, qui peut tout de toute façon, et le compte du broker.
//
// Le socket est cherché quelle que soit l'adresse : une URL qui désigne la
// machine par son nom ou l'une de ses adresses aboutit au même port. Trouvé, il
// doit appartenir à un compte admis. Absent, la connexion n'est admise que vers
// une adresse qui n'est pas celle de la machine — un broker distant, dont la
// liaison relève de l'URL elle-même (HTTPS). Absent vers une adresse locale, c'est
// une redirection (DNAT vers un conteneur, Docker sans userland-proxy) : ogs ne
// peut pas dire qui répond, et refuse en nommant le remède.
type peerGuard struct {
	account string
	// trusted rend les uid admis. Substituable en test.
	trusted func(account string) (map[uint32]bool, error)
	// local dit si une adresse est celle de la machine. Substituable en test.
	local func(net.IP) bool
}

// trustedUIDs rend root et l'uid du compte du broker, quand il existe. Le compte
// peut être donné par son uid : celui d'un broker en conteneur (réseau de l'hôte),
// qui n'a pas de nom dans /etc/passwd.
func trustedUIDs(account string) (map[uint32]bool, error) {
	uids := map[uint32]bool{0: true}
	if uid, err := strconv.ParseUint(account, 10, 32); err == nil {
		uids[uint32(uid)] = true
		return uids, nil
	}
	u, err := user.Lookup(account)
	if err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return uids, nil
		}
		return nil, err
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("uid %q du compte %s illisible", u.Uid, account)
	}
	uids[uint32(uid)] = true
	return uids, nil
}

// localIP dit si une adresse est celle de la machine : la boucle locale, ou
// l'adresse de l'une de ses interfaces.
func localIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true // dans le doute, le sens prudent : exiger le socket
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// dial ouvre la connexion puis en vérifie l'autre bout.
func (g peerGuard) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	if err := g.check(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func (g peerGuard) check(conn net.Conn) error {
	local, lok := conn.LocalAddr().(*net.TCPAddr)
	remote, rok := conn.RemoteAddr().(*net.TCPAddr)
	if !lok || !rok {
		return nil
	}
	isLocal := g.local
	if isLocal == nil {
		isLocal = localIP
	}
	here := isLocal(remote.IP)
	// Le socket accepté apparaît dès la fin de la poignée de main, que connect a
	// attendue ; l'attente ne couvre que le traitement différé du dernier ACK, et
	// ne vaut que vers la machine — un broker distant n'y figurera jamais.
	attempts := 1
	if here {
		attempts = 50
	}
	var uid uint32
	found := false
	var err error
	for attempt := 0; attempt < attempts && !found; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Millisecond)
		}
		uid, found, err = socketOwner(remote, local)
		if err != nil {
			return fmt.Errorf("%w : %v", ErrUntrustedPeer, err)
		}
	}
	switch {
	case !found && !here:
		return nil
	case !found:
		return fmt.Errorf("%w : aucun socket de cette machine ne répond pour %s (%s) — le port est sans "+
			"doute redirigé par DNAT vers un conteneur (Docker sans userland-proxy), et ogs ne peut pas dire "+
			"qui répond. Désigner le broker par son adresse propre (« ogs amqp credentials set --url "+
			"http://<adresse du conteneur>:15672 »), ou publier le port par docker-proxy",
			ErrUntrustedPeer, remote, strings.Join(procNetTCP, ", "))
	}
	trusted := g.trusted
	if trusted == nil {
		trusted = trustedUIDs
	}
	uids, err := trusted(g.account)
	if err != nil {
		return fmt.Errorf("%w : compte du broker %s illisible (%v)", ErrUntrustedPeer, g.account, err)
	}
	if !uids[uid] {
		return fmt.Errorf("%w : %s est tenu par l'uid %d, ni root ni le compte %s — les identifiants "+
			"d'administration ne lui sont pas envoyés. Vérifier que RabbitMQ et son plugin de management "+
			"tournent, et qui écoute sur ce port (ss -tlnp) ; un broker sous un autre compte se déclare par "+
			"« ogs amqp credentials set --broker-account <compte|uid> »", ErrUntrustedPeer, remote, uid, g.account)
	}
	return nil
}

// socketOwner cherche le socket dont l'adresse locale est local et l'adresse
// distante remote, et rend son uid.
func socketOwner(local, remote *net.TCPAddr) (uint32, bool, error) {
	for _, path := range procNetTCP {
		uid, found, err := socketOwnerIn(path, local, remote)
		if err != nil && !os.IsNotExist(err) {
			return 0, false, err
		}
		if found {
			return uid, true, nil
		}
	}
	return 0, false, nil
}

func socketOwnerIn(path string, local, remote *net.TCPAddr) (uint32, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // en-tête
	for sc.Scan() {
		// sl local_address rem_address st tx:rx tr:when retrnsmt uid …
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 {
			continue
		}
		l, err := parseProcAddr(fields[1])
		if err != nil || !sameAddr(l, local) {
			continue
		}
		r, err := parseProcAddr(fields[2])
		if err != nil || !sameAddr(r, remote) {
			continue
		}
		uid, err := strconv.ParseUint(fields[7], 10, 32)
		if err != nil {
			return 0, false, fmt.Errorf("%s : uid %q illisible", path, fields[7])
		}
		return uint32(uid), true, nil
	}
	return 0, false, sc.Err()
}

// parseProcAddr lit une adresse de /proc/net/tcp : l'adresse en hexadécimal, par
// mots de 32 bits dans l'ordre de la machine, puis le port.
func parseProcAddr(s string) (*net.TCPAddr, error) {
	host, port, ok := strings.Cut(s, ":")
	if !ok {
		return nil, fmt.Errorf("adresse %q illisible", s)
	}
	raw, err := hex.DecodeString(host)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return nil, fmt.Errorf("adresse %q illisible", s)
	}
	ip := make(net.IP, len(raw))
	for i := 0; i < len(raw); i += 4 {
		binary.NativeEndian.PutUint32(ip[i:], binary.BigEndian.Uint32(raw[i:]))
	}
	p, err := strconv.ParseUint(port, 16, 16)
	if err != nil {
		return nil, fmt.Errorf("port %q illisible", port)
	}
	return &net.TCPAddr{IP: ip, Port: int(p)}, nil
}

// sameAddr compare deux adresses, une IPv4 et sa forme projetée en IPv6 se valant.
func sameAddr(a, b *net.TCPAddr) bool { return a.Port == b.Port && a.IP.Equal(b.IP) }
