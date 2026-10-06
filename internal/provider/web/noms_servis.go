package web

import (
	"strconv"
	"strings"
)

// servedNames extrait d'une configuration les noms qu'elle sert sur les ports
// web, 80 et 443 — ceux des vhosts d'ogs (R2-44, D118).
//
// Seuls comptent les noms d'un bloc qui sert : <VirtualHost> sous Apache, server
// sous nginx. Un « ServerName shop.fr » global — le remède courant à AH00558,
// posé dans conf-enabled — ne sert aucun site, et le compter faisait refuser le
// plan d'un site en place. Le port compte quand il est lisible : un vhost
// étranger sur 8080, le backend d'un Varnish, ne reçoit pas les requêtes du 80
// ni du 443. Un port illisible compte comme web : le doute refuse, comme avant.
func servedNames(engineName, content string) []string {
	if engineName == "nginx" {
		return nginxServedNames(content)
	}
	return apacheServedNames(content)
}

// webPort dit si un port lu peut recevoir les requêtes d'un vhost d'ogs. Un
// port illisible compte comme web : le doute refuse.
func webPort(port string) bool {
	n, err := strconv.Atoi(port)
	return err != nil || n == 80 || n == 443
}

// apacheServedNames lit ServerName et ServerAlias dans les sections
// <VirtualHost>, dont les adresses disent les ports : « *:8080 » n'est pas web,
// « * » ou « _default_ » sans port valent pour tous les ports.
func apacheServedNames(content string) []string {
	var out, names []string
	in, web := false, false
	for _, line := range strings.Split(content, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(lower, "<virtualhost"):
			in, names = true, nil
			web = apacheVHostWeb(strings.TrimSuffix(trimmed[len("<virtualhost"):], ">"))
			continue
		case strings.HasPrefix(lower, "</virtualhost"):
			if in && web {
				out = append(out, names...)
			}
			in, names = false, nil
			continue
		}
		if !in {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		switch strings.ToLower(fields[0]) {
		case "servername", "serveralias":
			for _, f := range fields[1:] {
				names = append(names, apacheHost(f))
			}
		}
	}
	return out
}

// apacheVHostWeb dit si l'une des adresses d'un <VirtualHost> reçoit le 80 ou
// le 443 : une adresse sans port les reçoit tous.
func apacheVHostWeb(addrs string) bool {
	fields := strings.Fields(addrs)
	if len(fields) == 0 {
		return true
	}
	for _, a := range fields {
		port := ""
		if strings.HasPrefix(a, "[") {
			if i := strings.Index(a, "]:"); i >= 0 {
				port = a[i+2:]
			}
		} else if i := strings.LastIndex(a, ":"); i >= 0 {
			port = a[i+1:]
		}
		if port == "" || port == "*" {
			return true
		}
		if webPort(port) {
			return true
		}
	}
	return false
}

// nginxServedNames lit server_name dans les blocs server, dont les directives
// listen disent les ports. Un bloc sans listen écoute le 80 ; un listen sur un
// socket Unix ne reçoit pas le trafic web. Le texte est découpé en jetons — « { »,
// « } » et « ; » à part — pour suivre l'imbrication : un server_name d'un bloc
// location n'existe pas, et un « server » d'upstream n'ouvre pas de bloc.
func nginxServedNames(content string) []string {
	tokens := nginxTokens(content)

	type bloc struct {
		name    string
		names   []string
		listens bool
		web     bool
	}
	var out []string
	var stack []*bloc
	var stmt []string
	for _, t := range tokens {
		switch t {
		case "{":
			name := ""
			if len(stmt) > 0 {
				name = stmt[0]
			}
			stack = append(stack, &bloc{name: name})
			stmt = nil
		case "}":
			if n := len(stack); n > 0 {
				top := stack[n-1]
				stack = stack[:n-1]
				if top.name == "server" && (!top.listens || top.web) {
					out = append(out, top.names...)
				}
			}
			stmt = nil
		case ";":
			if n := len(stack); n > 0 && stack[n-1].name == "server" && len(stmt) > 0 {
				top := stack[n-1]
				switch stmt[0] {
				case "server_name":
					for _, n := range stmt[1:] {
						top.names = append(top.names, strings.Trim(n, `"'`))
					}
				case "listen":
					top.listens = true
					if len(stmt) > 1 && nginxListenWeb(strings.Trim(stmt[1], `"'`)) {
						top.web = true
					}
				}
			}
			stmt = nil
		default:
			stmt = append(stmt, t)
		}
	}
	return out
}

// nginxTokens découpe une configuration nginx en jetons : « { », « } » et
// « ; » à part, un commentaire de « # » à la fin de ligne écarté. Entre
// guillemets — simples ou doubles, « \ » échappant le suivant —, ni « # » ni
// accolade ni « ; » ne comptent : « add_header X "#fff"; » ne coupe pas la ligne.
func nginxTokens(content string) []string {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	var quote rune
	escaped, comment := false, false
	for _, c := range content {
		switch {
		case comment:
			if c == '\n' {
				comment = false
			}
		case quote != 0:
			cur.WriteRune(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == quote:
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
			cur.WriteRune(c)
		case c == '#':
			flush()
			comment = true
		case c == '{' || c == '}' || c == ';':
			flush()
			tokens = append(tokens, string(c))
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return tokens
}

// nginxListenWeb dit si une adresse de listen reçoit le 80 ou le 443 :
// « 80 », « [::]:443 », « 127.0.0.1:80 ». Une adresse sans port écoute le 80.
func nginxListenWeb(addr string) bool {
	if strings.HasPrefix(addr, "unix:") {
		return false
	}
	port := addr
	if strings.HasPrefix(addr, "[") {
		i := strings.Index(addr, "]:")
		if i < 0 {
			return true
		}
		port = addr[i+2:]
	} else if i := strings.LastIndex(addr, ":"); i >= 0 {
		port = addr[i+1:]
	} else if _, err := strconv.Atoi(addr); err != nil {
		return true // un nom d'hôte seul : port 80
	}
	return webPort(port)
}
