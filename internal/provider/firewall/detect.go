package firewall

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"leblanc.io/open-go-server/internal/config"
)

// Profile est un profil d'application tel qu'ogs l'écrit pour ufw : un nom, des
// ports, et la raison pour laquelle il est — ou n'est pas — disponible.
type Profile struct {
	// Name est le nom dans ufw.yaml (« mysql ») ; UFWName celui qu'ufw connaît
	// (« ogs-mysql »). Le préfixe n'est pas décoratif : applications.d est un
	// espace partagé avec les paquets, qui livrent leurs propres « OpenSSH » ou
	// « Apache Full » aux ports qu'ogs ne maîtrise pas.
	Name    string
	UFWName string
	Title   string
	Ports   []string
	// Available dit si le service est présent sur la machine : seul un profil
	// disponible reçoit des règles.
	Available bool
	// Reason dit ce qui a décidé de la disponibilité et des ports.
	Reason string
	// PassiveUnknown signale un FTP dont la plage passive n'a pas pu être lue.
	PassiveUnknown bool
	// Custom distingue un profil déclaré par l'exploitant.
	Custom bool
}

// PortsSpec rend les ports au format d'un fichier de profils ufw.
func (p Profile) PortsSpec() string { return strings.Join(p.Ports, "|") }

// UFWProfileName rend le nom ufw d'un profil d'ufw.yaml.
func UFWProfileName(name string) string { return "ogs-" + name }

// catalogEntry décrit un profil qu'ogs sait détecter.
type catalogEntry struct {
	title  string
	detect func(p *Provider) detection
}

// detection est le verdict d'une détection.
type detection struct {
	available      bool
	reason         string
	ports          []string
	passive        string
	passiveUnknown bool
}

var catalog = map[string]catalogEntry{
	"ssh":                 {"SSH", (*Provider).detectSSH},
	"sftp":                {"SFTP (proftpd)", (*Provider).detectSFTP},
	"ftp":                 {"FTP", (*Provider).detectFTP},
	"web":                 {"Serveur web (HTTP et HTTPS)", (*Provider).detectWeb},
	"mysql":               {"MySQL / MariaDB", unitDetector("3306/tcp", "mariadb", "mysql", "mysqld")},
	"postgresql":          {"PostgreSQL", unitDetector("5432/tcp", "postgresql")},
	"redis":               {"Redis", (*Provider).detectRedis},
	"rabbitmq":            {"RabbitMQ (AMQP)", unitDetector("5671,5672/tcp", "rabbitmq-server")},
	"rabbitmq-management": {"RabbitMQ (interface d’administration)", (*Provider).detectRabbitManagement},
	"smtp":                {"SMTP (réception)", unitDetector("25/tcp", "postfix", "exim4")},
	"submission":          {"SMTP (soumission)", unitDetector("465,587/tcp", "postfix", "exim4")},
}

// Profiles résout tous les profils : le catalogue, puis ceux que l'exploitant
// déclare. Les surcharges d'ufw.yaml l'emportent sur la détection.
func (p *Provider) Profiles() []Profile {
	cfg := p.cfg()
	var out []Profile
	for _, name := range config.UFWCatalogProfiles {
		entry := catalog[name]
		d := entry.detect(p)
		prof := Profile{Name: name, UFWName: UFWProfileName(name), Title: entry.title,
			Ports: d.ports, Available: d.available, Reason: d.reason, PassiveUnknown: d.passiveUnknown}
		if o, ok := cfg.Profiles[name]; ok {
			applyOverride(&prof, o, d)
		} else if d.passive != "" {
			prof.Ports = append(prof.Ports, d.passive+"/tcp")
		}
		out = append(out, prof)
	}
	for _, name := range cfg.SortedProfiles() {
		if config.IsUFWCatalogProfile(name) {
			continue
		}
		o := cfg.Profiles[name]
		prof := Profile{Name: name, UFWName: UFWProfileName(name), Title: o.Title,
			Ports: o.Ports, Custom: true, Available: true, Reason: "déclaré dans ufw.yaml"}
		if prof.Title == "" {
			prof.Title = name
		}
		if len(o.Units) > 0 {
			if unit, ok := p.unitExists(o.Units...); ok {
				prof.Reason = "unité " + unit
			} else {
				prof.Available = false
				prof.Reason = "aucune des unités " + strings.Join(o.Units, ", ")
			}
		}
		if o.Available != nil {
			prof.Available = *o.Available
			prof.Reason = forcedReason(*o.Available)
		}
		out = append(out, prof)
	}
	return out
}

// applyOverride porte les surcharges d'ufw.yaml sur un profil détecté.
func applyOverride(prof *Profile, o config.UFWProfile, d detection) {
	if len(o.Ports) > 0 {
		prof.Ports = o.Ports
		prof.Reason += " ; ports déclarés dans ufw.yaml"
	}
	passive := d.passive
	if o.Passive != "" {
		passive = o.Passive
		prof.PassiveUnknown = false
	}
	if passive != "" {
		prof.Ports = append(prof.Ports, passive+"/tcp")
	}
	if o.Available != nil {
		prof.Available = *o.Available
		prof.Reason = forcedReason(*o.Available) + " (détection : " + prof.Reason + ")"
	}
}

func forcedReason(v bool) string {
	if v {
		return "forcé disponible dans ufw.yaml"
	}
	return "écarté dans ufw.yaml"
}

// unitDirs sont les répertoires où systemd cherche une unité installée.
var unitDirs = []string{"/etc/systemd/system", "/lib/systemd/system", "/usr/lib/systemd/system"}

// unitExists dit si l'une des unités est installée. Le critère est le fichier,
// non l'état : un service arrêté reste un service de la machine, et ses règles
// doivent être là le jour où on le redémarre.
func (p *Provider) unitExists(names ...string) (string, bool) {
	for _, name := range names {
		if !strings.Contains(name, ".") {
			name += ".service"
		}
		for _, dir := range unitDirs {
			if _, err := os.Stat(filepath.Join(p.Paths.Rooted(dir), name)); err == nil {
				return name, true
			}
		}
	}
	return "", false
}

// unitDetector détecte un service par ses unités, à ports fixes.
func unitDetector(ports string, units ...string) func(*Provider) detection {
	return func(p *Provider) detection {
		if unit, ok := p.unitExists(units...); ok {
			return detection{available: true, reason: "unité " + unit, ports: []string{ports}}
		}
		return detection{reason: "aucune unité " + strings.Join(units, ", "), ports: []string{ports}}
	}
}

// detectWeb suit le moteur retenu par ogs : il est déjà détecté, et le parc n'en
// sert qu'un.
func (p *Provider) detectWeb() detection {
	d := detection{ports: []string{"80,443/tcp"}}
	if p.WebUnit == "" {
		d.reason = "aucun moteur web retenu"
		return d
	}
	d.available, d.reason = true, "moteur "+p.WebUnit
	return d
}

// detectSSH lit les ports de sshd. Un sshd sur un port non standard est le cas
// où un profil figé coupe l'accès : la règle ouvre 22, le démon écoute ailleurs,
// et la politique par défaut refuse tout le reste.
func (p *Provider) detectSSH() detection {
	unit, ok := p.unitExists("ssh", "sshd")
	d := detection{ports: []string{"22/tcp"}}
	if !ok {
		d.reason = "aucune unité ssh, sshd"
		return d
	}
	d.available, d.reason = true, "unité "+unit
	if ports := p.sshdPorts(); len(ports) > 0 {
		d.ports = nil
		for _, port := range ports {
			d.ports = append(d.ports, port+"/tcp")
		}
		d.reason += ", Port " + strings.Join(ports, ", ") + " dans sshd_config"
	}
	return d
}

// sshdPorts relève les directives Port de sshd_config et de ses inclusions.
// Contrairement à la plupart des directives, où la première l'emporte, chaque
// Port s'ajoute aux autres : sshd écoute sur tous.
func (p *Provider) sshdPorts() []string {
	seen := map[string]bool{}
	var out []string
	var visit func(path string, depth int)
	visit = func(path string, depth int) {
		if depth > 8 {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		for _, line := range configLines(data) {
			key, value := directive(line)
			switch strings.ToLower(key) {
			case "match":
				// Port est refusé dans un bloc Match : ce qui suit n'en porte pas.
				return
			case "port":
				if _, err := strconv.Atoi(value); err == nil && !seen[value] {
					seen[value] = true
					out = append(out, value)
				}
			case "listenaddress":
				// « ListenAddress hôte:port » ou « [ipv6]:port » fixe son propre
				// port, en plus des Port ; une IPv6 nue n'en porte pas.
				if port, ok := listenPort(value); ok && !seen[port] {
					seen[port] = true
					out = append(out, port)
				}
			case "include":
				for _, pattern := range strings.Fields(value) {
					if !filepath.IsAbs(pattern) {
						pattern = filepath.Join("/etc/ssh", pattern)
					}
					matches, _ := filepath.Glob(p.Paths.Rooted(pattern))
					sort.Strings(matches)
					for _, m := range matches {
						visit(m, depth+1)
					}
				}
			}
		}
	}
	visit(p.Paths.Rooted("/etc/ssh/sshd_config"), 0)
	return out
}

// detectRedis lit le port de redis.conf : « port 0 » coupe l'écoute TCP, et un
// profil ouvert sur un service qui n'écoute qu'un socket n'aurait pas d'objet.
func (p *Provider) detectRedis() detection {
	unit, ok := p.unitExists("redis-server", "redis")
	d := detection{ports: []string{"6379/tcp"}}
	if !ok {
		d.reason = "aucune unité redis-server, redis"
		return d
	}
	d.available, d.reason = true, "unité "+unit
	data, err := os.ReadFile(p.Paths.Rooted("/etc/redis/redis.conf"))
	if err != nil {
		return d
	}
	for _, line := range configLines(data) {
		key, value := directive(line)
		if strings.ToLower(key) != "port" {
			continue
		}
		if value == "0" {
			d.available, d.reason = false, "unité "+unit+", mais « port 0 » : aucune écoute TCP"
			return d
		}
		if _, err := strconv.Atoi(value); err == nil {
			d.ports = []string{value + "/tcp"}
			d.reason += ", port " + value + " dans redis.conf"
		}
	}
	return d
}

// detectRabbitManagement exige le greffon d'administration en plus du broker.
func (p *Provider) detectRabbitManagement() detection {
	d := detection{ports: []string{"15672/tcp"}}
	unit, ok := p.unitExists("rabbitmq-server")
	if !ok {
		d.reason = "aucune unité rabbitmq-server"
		return d
	}
	data, err := os.ReadFile(p.Paths.Rooted("/etc/rabbitmq/enabled_plugins"))
	if err != nil || !bytes.Contains(data, []byte("rabbitmq_management")) {
		d.reason = "unité " + unit + ", greffon rabbitmq_management inactif"
		return d
	}
	d.available, d.reason = true, "unité "+unit+", greffon rabbitmq_management"
	return d
}

// detectSFTP cherche un SFTP servi par proftpd (mod_sftp). Un SFTP servi par
// sshd n'est pas un profil à part : c'est le port de sshd, et son profil est
// « ssh ».
func (p *Provider) detectSFTP() detection {
	d := detection{ports: []string{"2222/tcp"}}
	unit, ok := p.unitExists("proftpd")
	if !ok {
		d.reason = "aucune unité proftpd"
		return d
	}
	conf := p.proftpd()
	if len(conf.sftpPorts) == 0 {
		d.reason = "unité " + unit + ", aucun contexte « SFTPEngine on »"
		return d
	}
	d.available = true
	d.ports = nil
	for _, port := range conf.sftpPorts {
		d.ports = append(d.ports, port+"/tcp")
	}
	d.reason = "unité " + unit + ", SFTPEngine sur le port " + strings.Join(conf.sftpPorts, ", ")
	return d
}

// detectFTP reconnaît proftpd, vsftpd et pure-ftpd. Un FTP sans plage passive
// connue est signalé : la connexion de contrôle passerait, et chaque transfert
// resterait bloqué sans que rien ne dise pourquoi.
func (p *Provider) detectFTP() detection {
	d := detection{ports: []string{"21/tcp"}}
	if unit, ok := p.unitExists("proftpd"); ok {
		conf := p.proftpd()
		if len(conf.ftpPorts) == 0 {
			d.reason = "unité " + unit + ", aucun contexte FTP (Port 0 ou SFTP seul)"
			return d
		}
		d.available = true
		d.ports = nil
		for _, port := range conf.ftpPorts {
			d.ports = append(d.ports, port+"/tcp")
		}
		d.reason = "unité " + unit + ", port " + strings.Join(conf.ftpPorts, ", ")
		d.passive = conf.passive
		d.passiveUnknown = conf.passive == ""
		if d.passive != "" {
			d.reason += ", PassivePorts " + d.passive
		}
		return d
	}
	if unit, ok := p.unitExists("vsftpd"); ok {
		d.available, d.reason = true, "unité "+unit
		values := keyValues(p.Paths.Rooted("/etc/vsftpd.conf"), "=")
		if port := values["listen_port"]; port != "" {
			d.ports = []string{port + "/tcp"}
		}
		lo, hi := values["pasv_min_port"], values["pasv_max_port"]
		if lo != "" && hi != "" && lo != "0" && hi != "0" {
			d.passive = lo + ":" + hi
			d.reason += ", pasv_min_port/pasv_max_port " + d.passive
		} else {
			d.passiveUnknown = true
		}
		return d
	}
	if unit, ok := p.unitExists("pure-ftpd"); ok {
		d.available, d.reason = true, "unité "+unit
		data, err := os.ReadFile(p.Paths.Rooted("/etc/pure-ftpd/conf/PassivePortRange"))
		if fields := strings.Fields(string(data)); err == nil && len(fields) == 2 {
			d.passive = fields[0] + ":" + fields[1]
			d.reason += ", PassivePortRange " + d.passive
		} else {
			d.passiveUnknown = true
		}
		return d
	}
	d.reason = "aucune unité proftpd, vsftpd, pure-ftpd"
	return d
}

// proftpdConf est ce que le pare-feu retient de la configuration de proftpd.
type proftpdConf struct {
	ftpPorts  []string
	sftpPorts []string
	passive   string
}

// proftpd lit proftpd.conf et ses inclusions. Chaque contexte — le serveur
// principal, chaque <VirtualHost> — a son port et son moteur : un contexte où
// « SFTPEngine on » figure sert du SFTP, les autres du FTP, et « Port 0 »
// éteint le serveur principal. Les sections <IfModule> et <Global> sont
// transparentes : c'est leur contenu qui compte.
func (p *Provider) proftpd() proftpdConf {
	type contexte struct {
		port string
		sftp bool
	}
	principal := &contexte{}
	var hôtes []*contexte
	courant := principal
	var passive string

	var visit func(path string, depth int)
	visit = func(path string, depth int) {
		if depth > 8 {
			return
		}
		info, err := os.Stat(path)
		if err != nil {
			return
		}
		if info.IsDir() {
			entries, _ := os.ReadDir(path)
			for _, e := range entries {
				if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
					visit(filepath.Join(path, e.Name()), depth+1)
				}
			}
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		for _, line := range configLines(data) {
			lower := strings.ToLower(line)
			switch {
			case strings.HasPrefix(lower, "<virtualhost"):
				courant = &contexte{}
				hôtes = append(hôtes, courant)
				continue
			case strings.HasPrefix(lower, "</virtualhost"):
				courant = principal
				continue
			case strings.HasPrefix(line, "<"):
				continue
			}
			key, value := directive(line)
			switch strings.ToLower(key) {
			case "port":
				courant.port = value
			case "sftpengine":
				courant.sftp = strings.EqualFold(value, "on")
			case "passiveports":
				if f := strings.Fields(value); len(f) == 2 {
					passive = f[0] + ":" + f[1]
				}
			case "include":
				pattern := value
				if !filepath.IsAbs(pattern) {
					pattern = filepath.Join("/etc/proftpd", pattern)
				}
				matches, _ := filepath.Glob(p.Paths.Rooted(pattern))
				sort.Strings(matches)
				for _, m := range matches {
					visit(m, depth+1)
				}
			}
		}
	}
	visit(p.Paths.Rooted("/etc/proftpd/proftpd.conf"), 0)

	var conf proftpdConf
	conf.passive = passive
	add := func(c *contexte) {
		port := c.port
		if port == "" {
			port = "21"
		}
		if port == "0" {
			return
		}
		if _, err := strconv.Atoi(port); err != nil {
			return
		}
		if c.sftp {
			conf.sftpPorts = appendUnique(conf.sftpPorts, port)
		} else {
			conf.ftpPorts = appendUnique(conf.ftpPorts, port)
		}
	}
	add(principal)
	for _, h := range hôtes {
		add(h)
	}
	return conf
}

// configLines rend les lignes utiles d'un fichier : commentaires et blancs ôtés.
func configLines(data []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// directive coupe une ligne « Clé valeur » ou « Clé=valeur » — sshd_config(5)
// admet les deux.
func directive(line string) (string, string) {
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return line, ""
	}
	key, value := line[:i], strings.TrimSpace(line[i:])
	value = strings.TrimSpace(strings.TrimPrefix(value, "="))
	return key, strings.Trim(value, `"`)
}

// listenPort extrait le port d'une ListenAddress : « 0.0.0.0:2222 »,
// « [::]:2222 », « hôte:2222 ». Une IPv6 sans crochets n'en porte pas, et
// « rdomain » suit d'une espace.
func listenPort(value string) (string, bool) {
	value, _, _ = strings.Cut(value, " ")
	var port string
	switch {
	case strings.HasPrefix(value, "["):
		i := strings.Index(value, "]:")
		if i < 0 {
			return "", false
		}
		port = value[i+2:]
	case strings.Count(value, ":") == 1:
		port = value[strings.IndexByte(value, ':')+1:]
	default:
		return "", false
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", false
	}
	return port, true
}

// keyValues lit un fichier « clé<sep>valeur ».
func keyValues(path, sep string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range configLines(data) {
		k, v, ok := strings.Cut(line, sep)
		if ok {
			out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return out
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// renderProfiles compose le fichier de profils : tous les profils, disponibles
// ou non, et ceux que des règles existantes emploient encore. Une définition
// ne coûte rien, et la retirer alors qu'une règle la désigne rendrait cette
// règle impossible à supprimer — ufw refuse une application qu'il ne connaît
// pas, jusque dans « ufw delete ».
func renderProfiles(profiles []Profile, kept map[string]string) []byte {
	var b strings.Builder
	written := map[string]bool{}
	for _, prof := range profiles {
		written[prof.UFWName] = true
		fmt.Fprintf(&b, "[%s]\n", prof.UFWName)
		fmt.Fprintf(&b, "title=%s\n", prof.Title)
		state := "non détecté"
		if prof.Available {
			state = "disponible"
		}
		fmt.Fprintf(&b, "description=Profil géré par ogs (%s) : %s\n", state, prof.Reason)
		fmt.Fprintf(&b, "ports=%s\n\n", prof.PortsSpec())
	}
	names := make([]string, 0, len(kept))
	for n := range kept {
		if !written[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "[%s]\ntitle=%s\ndescription=Profil retiré d’ufw.yaml, gardé tant qu’une règle l’emploie\nports=%s\n\n",
			n, n, kept[n])
	}
	return []byte(strings.TrimSuffix(b.String(), "\n"))
}

// parseProfiles relit un fichier de profils : nom → ports.
func parseProfiles(data []byte) map[string]string {
	out := map[string]string{}
	var current string
	for _, line := range configLines(data) {
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && current != "" && strings.TrimSpace(k) == "ports" {
			out[current] = strings.TrimSpace(v)
		}
	}
	return out
}
