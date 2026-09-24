package firewall

import (
	"fmt"
	"strconv"
	"strings"
)

// Finding est un constat de « doctor ».
type Finding struct {
	Name   string
	Status string // ok | avertissement | échec
	Detail string
	Fix    string
}

// Checks porte ce que le plan ne voit pas : un profil qui annonce un port où
// rien n'écoute — le sshd déplacé, que la politique par défaut couperait —, un
// service qui n'écoute que la boucle locale et dont les règles n'ont pas
// d'objet, Docker qui publie ses ports en amont d'ufw, une activation qui
// attend sa confirmation.
func (p *Provider) Checks(obs Observed, listeners []string) []Finding {
	if !p.Managed() {
		return nil
	}
	var out []Finding
	add := func(name, status, detail, fix string) {
		out = append(out, Finding{Name: name, Status: status, Detail: detail, Fix: fix})
	}
	if !obs.Installed {
		add("pare-feu", "échec", "ufw.yaml est présent mais ufw n'est pas installé", "apt install ufw")
		return out
	}
	cfg := p.cfg()
	switch {
	case cfg.IsActive() && !obs.Enabled:
		add("pare-feu", "avertissement", "ufw est inactif alors qu'ufw.yaml le veut actif", "ogs firewall apply")
	case obs.Enabled:
		add("pare-feu", "ok", fmt.Sprintf("ufw actif, entrée %s, sortie %s", obs.Incoming, obs.Outgoing), "")
	default:
		add("pare-feu", "ok", "ufw inactif, comme ufw.yaml le demande", "")
	}
	if obs.RollbackPending {
		add("pare-feu : confirmation", "avertissement",
			"l'activation d'ufw n'a pas été confirmée : il se désactivera de lui-même au terme du délai",
			"ogs firewall confirm")
	}
	if unit, ok := p.unitExists("docker"); ok {
		add("pare-feu : docker", "avertissement", unit+" est installé : Docker publie les ports de ses "+
			"conteneurs par ses propres chaînes, examinées avant celles d'ufw — un port publié est ouvert "+
			"quelles que soient les règles", "publier sur 127.0.0.1, ou filtrer dans la chaîne DOCKER-USER")
	}

	desired, err := p.Desired()
	if err != nil {
		add("pare-feu", "échec", err.Error(), "corriger "+p.Paths.UFWConfig())
		return out
	}
	// L'écoute n'est éprouvée que pour les profils ouverts explicitement, et pour
	// ssh toujours. La confiance pose une règle sur chaque profil disponible :
	// MariaDB, PostgreSQL ou Postfix n'écoutent d'ordinaire que la boucle locale,
	// et le signaler pour chacun noierait le seul cas qui coupe un accès — le
	// sshd déplacé.
	withRules, explicit := map[string]bool{}, map[string]bool{}
	for _, r := range desired {
		if r.Profile != "" {
			withRules[r.Profile] = true
			if r.Kind == KindAllow {
				explicit[r.Profile] = true
			}
		}
	}
	for _, prof := range p.Profiles() {
		if !prof.Available || !withRules[prof.Name] {
			continue
		}
		name := "pare-feu : " + prof.Name
		if prof.PassiveUnknown {
			add(name, "avertissement", "plage de ports passifs inconnue : les transferts resteront bloqués",
				"déclarer profiles.ftp.passive dans "+p.Paths.UFWConfig())
		}
		if listeners == nil || (!explicit[prof.Name] && prof.Name != "ssh") {
			continue
		}
		public, loopback := listening(prof.Ports, listeners)
		switch {
		case public:
		case loopback:
			add(name, "avertissement", fmt.Sprintf("%s n'écoute que sur la boucle locale : ses règles "+
				"n'ouvrent rien", prof.PortsSpec()), "")
		default:
			add(name, "avertissement", fmt.Sprintf("rien n'écoute sur %s — le service est arrêté, ou écoute "+
				"ailleurs et le profil ouvre le mauvais port", prof.PortsSpec()),
				"profiles."+prof.Name+".ports dans "+p.Paths.UFWConfig())
		}
	}

	odd, foreign := 0, 0
	var oddRaw []string
	for _, r := range obs.Rules {
		switch {
		case r.Managed && !r.Recognized:
			odd++
			oddRaw = append(oddRaw, r.Raw)
		case !r.Managed:
			foreign++
		}
	}
	if odd > 0 {
		add("pare-feu : règles", "avertissement", fmt.Sprintf("%d règle(s) marquées par ogs mais retouchées, "+
			"qu'ogs ne sait plus retirer : %s", odd, strings.Join(oddRaw, " ; ")),
			"les retirer avec « ufw delete », ogs les reposera")
	}
	if foreign > 0 {
		add("pare-feu : règles", "ok", fmt.Sprintf("%d règle(s) de l'administrateur, laissées intactes", foreign), "")
	}
	return out
}

// listening dit si l'un des ports écoute sur une adresse publique, ou sur la
// seule boucle locale. Les plages — les ports passifs — ne sont pas éprouvées :
// rien n'y écoute hors d'un transfert.
func listening(ports []string, listeners []string) (public, loopback bool) {
	for _, spec := range ports {
		list, _, _ := strings.Cut(spec, "/")
		for _, part := range strings.Split(list, ",") {
			if strings.Contains(part, ":") {
				continue
			}
			if _, err := strconv.Atoi(part); err != nil {
				continue
			}
			for _, l := range listeners {
				i := strings.LastIndexByte(l, ':')
				if i < 0 || l[i+1:] != part {
					continue
				}
				host := strings.Trim(l[:i], "[]")
				if host == "127.0.0.1" || host == "::1" || strings.HasPrefix(host, "127.") {
					loopback = true
				} else {
					public = true
				}
			}
		}
	}
	return public, loopback
}
