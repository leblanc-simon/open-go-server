package app

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
)

// Listener est une écoute TCP de la machine, avec le compte qui l'a ouverte.
type Listener struct {
	IP   net.IP
	Port int
	UID  int
}

// ProcNet est le répertoire où le noyau publie les sockets de l'espace de noms
// réseau du processus.
const ProcNet = "/proc/net"

// ReadListeners énumère les écoutes TCP publiées par le noyau, IPv4 et IPv6.
//
// C'est la seule source qui dise **à qui** appartient une écoute : la colonne
// uid de /proc/net/tcp est celle du compte qui a créé le socket. Une connexion
// réussie ne prouve rien — n'importe quel compte local peut tenir le port d'un
// projet et recevoir tout ce que le vhost lui relaie (D94).
func ReadListeners(dir string) ([]Listener, error) {
	var out []Listener
	for _, name := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		ls, err := parseProcNet(data)
		if err != nil {
			return nil, fmt.Errorf("%s : %w", filepath.Join(dir, name), err)
		}
		out = append(out, ls...)
	}
	return out, nil
}

// tcpListen est l'état LISTEN dans la colonne « st » de /proc/net/tcp.
const tcpListen = "0A"

// parseProcNet lit un fichier au format de /proc/net/tcp ou tcp6.
func parseProcNet(data []byte) ([]Listener, error) {
	var out []Listener
	sc := bufio.NewScanner(bytes.NewReader(data))
	first := true
	for sc.Scan() {
		if first {
			first = false // ligne d'en-tête
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 || fields[3] != tcpListen {
			continue
		}
		addr, port, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		ip, err := decodeProcIP(addr)
		if err != nil {
			return nil, err
		}
		p, err := strconv.ParseUint(port, 16, 16)
		if err != nil {
			return nil, fmt.Errorf("port %q illisible", port)
		}
		uid, err := strconv.Atoi(fields[7])
		if err != nil {
			return nil, fmt.Errorf("uid %q illisible", fields[7])
		}
		out = append(out, Listener{IP: ip, Port: int(p), UID: uid})
	}
	return out, sc.Err()
}

// decodeProcIP décode une adresse de /proc/net/tcp, mot de 32 bits par mot.
func decodeProcIP(s string) (net.IP, error) {
	raw, err := hex.DecodeString(s)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return nil, fmt.Errorf("adresse %q illisible", s)
	}
	// Le noyau imprime la valeur du mot (%08X) : la relire en poids fort
	// d'abord, puis la reposer dans l'ordre de la mémoire, rend les octets
	// dans l'ordre du réseau.
	ip := make(net.IP, len(raw))
	for i := 0; i < len(raw); i += 4 {
		binary.NativeEndian.PutUint32(ip[i:], binary.BigEndian.Uint32(raw[i:i+4]))
	}
	return ip, nil
}

// reachesLoopback dit si une écoute sur cette adresse reçoit les connexions
// adressées à 127.0.0.1 : la boucle locale elle-même, ou toute adresse joker,
// IPv6 comprise — un socket [::] reçoit aussi l'IPv4 hors IPV6_V6ONLY.
func reachesLoopback(ip net.IP) bool {
	return ip.IsUnspecified() || ip.Equal(net.IPv4(127, 0, 0, 1))
}

// listenersNow rend les écoutes de la machine, ou rien en mode simulé : un plan
// calculé sous --prefix ne doit pas dépendre de ce qui tourne sur le poste.
func (p *Provider) listenersNow() ([]Listener, error) {
	if p.Listeners != nil {
		return p.Listeners()
	}
	if p.Exec == nil || p.Exec.Simulated() {
		return nil, nil
	}
	return ReadListeners(ProcNet)
}

// PortsInUse rend les ports TCP en écoute sur la machine, quelle que soit
// l'adresse. L'attribution d'un numéro neuf les écarte (D94) : le port suivant
// d'une plage est prévisible, et un compte qui l'aurait pris d'avance recevrait
// le trafic du prochain projet. Nil quand la lecture est impossible.
func (p *Provider) PortsInUse() map[int]bool {
	ls, err := p.listenersNow()
	if err != nil || ls == nil {
		return nil
	}
	out := make(map[int]bool, len(ls))
	for _, l := range ls {
		out[l.Port] = true
	}
	return out
}

// ForeignListenerOnDisk nomme le compte étranger qui tient le port écrit dans
// l'unité applicative d'un projet, vide si le port est libre, tenu par le
// compte du projet ou par root, ou si l'unité n'en porte pas (socket Unix,
// unité absente ou d'un autre projet). C'est le contrôle de doctor : il relit
// l'unité et n'attribue rien — doctor ne fabrique pas d'état, et sans base
// d'état tous les projets auraient reçu le premier port de leur plage.
func (p *Provider) ForeignListenerOnDisk(proj *model.Project) (string, error) {
	port, ok := p.WrittenPort(proj)
	if !ok {
		return "", nil
	}
	return p.foreignListener(proj, Endpoint{Address: fmt.Sprintf("127.0.0.1:%d", port), Port: port})
}

// WrittenPort relit le port que l'unité applicative écrite transmet à
// l'application, si elle appartient au projet (en-tête, D76).
func (p *Provider) WrittenPort(proj *model.Project) (int, bool) {
	path := p.unitPath(proj)
	project, resource, err := fs.ManagedResource(path)
	if err != nil || project != proj.ID() || resource != "app/"+ServiceName {
		return 0, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	m := rePortLine.FindSubmatch(data)
	if m == nil {
		return 0, false
	}
	port, err := strconv.Atoi(string(m[1]))
	return port, err == nil
}

// foreignListener nomme le compte étranger qui tient le port d'un projet, vide
// si le port est libre, tenu par le compte du projet ou par root, ou si le point
// d'écoute est un socket Unix — que seuls le projet et le serveur web
// atteignent.
//
// La sonde de disponibilité se contentait d'une connexion : un processus d'un
// autre compte, installé sur le port avant la création du projet ou pendant
// les cinq secondes d'un redémarrage, faisait passer l'application pour prête
// et recevait les requêtes du vhost, cookies et en-têtes d'authentification
// compris (S2R-02). Root n'est pas cet attaquant — il lit déjà tout — et c'est
// sous root qu'écoute ce qui publie le port pour l'application : docker-proxy,
// pour une application custom lancée par « docker run -p ».
func (p *Provider) foreignListener(proj *model.Project, e Endpoint) (string, error) {
	if e.Unix() {
		return "", nil
	}
	ls, err := p.listenersNow()
	if err != nil || ls == nil {
		return "", err
	}
	// Un compte introuvable — le projet n'est pas encore créé — ne tient rien :
	// toute écoute sur son port est alors étrangère.
	uid := -1
	if p.Owner != nil {
		if u, _, err := p.Owner(proj); err == nil {
			uid = u
		}
	}
	for _, l := range ls {
		if l.Port != e.Port || !reachesLoopback(l.IP) || l.UID == uid || l.UID == 0 {
			continue
		}
		return fmt.Sprintf("%s:%d tenu par %s, non par %s", l.IP, l.Port, describeUID(l.UID),
			proj.Spec.User.Name), nil
	}
	return "", nil
}

// describeUID nomme un compte par son nom quand il est connu.
func describeUID(uid int) string {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return fmt.Sprintf("le compte %s (uid %d)", u.Username, uid)
	}
	return fmt.Sprintf("l'uid %d", uid)
}

// rePortLine relit le port qu'une unité applicative transmet à l'application,
// cité ou non par systemdEnv.
var rePortLine = regexp.MustCompile(`(?m)^Environment="?PORT=([0-9]+)"?\s*$`)

// recoverPorts inscrit dans la base d'état les ports que portent les unités
// applicatives écrites par ogs (D94). La base n'est pas seule à savoir quel port
// sert quel projet : le vhost d'un voisin relaie déjà vers celui de son unité.
// Perdue ou recréée, elle redistribuait les ports dans l'ordre alphabétique des
// projets, et le premier calculé prenait le port en service chez un autre.
//
// Le projet est lu sur l'en-tête, jamais sur le nom du fichier (D76), et le
// runtime sur le port lui-même, qui porte son code (2<runtime><incrément>).
func (p *Provider) recoverPorts() error {
	if p.Store == nil {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(p.Paths.SystemdDir(), "ogs-*-"+ServiceName+"@.service"))
	if err != nil {
		return err
	}
	for _, path := range matches {
		project, resource, err := fs.ManagedResource(path)
		if err != nil || project == "" || resource != "app/"+ServiceName {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		m := rePortLine.FindSubmatch(data)
		if m == nil {
			continue // socket Unix : le port n'est pas en service
		}
		port, _ := strconv.Atoi(string(m[1]))
		runtime := runtimeOfPort(port)
		if runtime == "" {
			continue
		}
		if _, err := p.Store.Claim("app-"+runtime, project, ServiceName, port); err != nil {
			return err
		}
	}
	return nil
}

// runtimeOfPort retrouve le runtime dont la plage contient un port.
func runtimeOfPort(port int) string {
	incr := port % 1000
	if port < 20000 || incr == 0 {
		return ""
	}
	code := (port - 20000) / 1000
	for runtime, c := range naming.RuntimeCode {
		if c == code {
			return runtime
		}
	}
	return ""
}
