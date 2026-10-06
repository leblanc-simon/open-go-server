package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"leblanc.io/open-go-server/internal/naming"
)

// RedirectVHostName est le nom du vhost qui porte les redirections vers le
// premier domaine servi : le seul qu'un projet avait avant D119, dont le fichier
// (<id>_redirect.conf) ne change donc pas.
const RedirectVHostName = "redirect"

// maxRedirectSuffix borne la part du nom d'un vhost de redirection tirée de sa
// cible : un nom de domaine va jusqu'à 253 caractères, un nom de fichier à 255
// octets, et le fichier porte encore l'identifiant du projet et l'extension.
const maxRedirectSuffix = 100

// RedirectTarget est la cible effective d'un domaine redirigé (D119).
type RedirectTarget struct {
	// Domain est le domaine redirigé.
	Domain string
	// Target est le nom vers lequel il redirige : sa cible déclarée, ramenée à
	// sa forme canonique, ou le premier domaine servi.
	Target string
	// Problem dit pourquoi la cible déclarée n'a pas été retenue, vide sinon.
	Problem string
}

// canonicalTarget ramène une cible à la forme qu'un serveur web compare : sans
// casse, sans blancs ni point final. Le préfixe « www. » est gardé : c'est un
// autre nom.
func canonicalTarget(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// RedirectTargets résout la cible de chaque domaine « redirect » du projet, dans
// l'ordre déclaré (D119). Sans cible, ou avec celle du premier domaine servi, la
// redirection vise celui-ci, comme avant D119. Une cible qui est elle-même une
// redirection du projet est suivie jusqu'à sa destination : le visiteur n'y
// gagne qu'un saut de moins. Une cible qui n'est pas un nom d'hôte, ou une boucle
// de redirections, est écartée au profit du premier domaine servi — ce que
// faisait ogs jusque-là pour toute cible —, et Problem le dit : le plan l'annonce
// (code 6), doctor le rappelle. Le refus n'est jamais au chargement : la cible
// était ignorée, et une déclaration qu'ogs servait sans broncher arrêterait sinon
// tout le parc.
//
// first est le premier domaine servi (primary ou alias), "" s'il n'y en a pas.
func (p *Project) RedirectTargets() (out []RedirectTarget, first string) {
	redirects := map[string]Domain{}
	served := map[string]string{}
	for _, d := range p.Spec.Domains {
		switch d.Role {
		case "", "primary", "alias":
			if first == "" {
				first = d.Name
			}
			served[canonicalTarget(d.Name)] = d.Name
		case "redirect":
			redirects[canonicalTarget(d.Name)] = d
		}
	}
	for _, d := range p.Spec.Domains {
		if d.Role != "redirect" {
			continue
		}
		rt := RedirectTarget{Domain: d.Name, Target: first}
		seen := map[string]bool{canonicalTarget(d.Name): true}
		cur := d
		for {
			t := canonicalTarget(cur.Target)
			if t == "" {
				break // sans cible : le premier domaine servi
			}
			if _, err := naming.NormalizeHost(t); err != nil || strings.ContainsAny(t, " \t;\"'{}$\\/:") {
				rt.Problem = fmt.Sprintf("cible %q de %s : ce n'est pas un nom d'hôte", cur.Target, cur.Name)
				break
			}
			if name, ok := served[t]; ok {
				rt.Target = name
				break
			}
			next, ok := redirects[t]
			if !ok {
				rt.Target = t // un nom étranger au projet
				break
			}
			if seen[t] {
				rt.Problem = fmt.Sprintf("cible %q de %s : boucle de redirections", d.Target, d.Name)
				break
			}
			seen[t] = true
			cur = next
		}
		if rt.Problem != "" {
			rt.Target = first
		}
		out = append(out, rt)
	}
	return out, first
}

// redirectVHostName nomme le vhost de redirection d'une cible : « redirect »
// pour le premier domaine servi, « redirect-<cible> » sinon — le fichier
// <id>_redirect-autre.fr.conf dit où il mène. Une cible très longue est tronquée
// et complétée d'une empreinte, qui garde le nom unique.
func redirectVHostName(target, first string) string {
	if canonicalTarget(target) == canonicalTarget(first) {
		return RedirectVHostName
	}
	suffix := canonicalTarget(target)
	if len(suffix) > maxRedirectSuffix {
		sum := sha256.Sum256([]byte(suffix))
		suffix = suffix[:maxRedirectSuffix-9] + "-" + hex.EncodeToString(sum[:4])
	}
	return RedirectVHostName + "-" + suffix
}

// RedirectProblems énumère les cibles de redirection écartées (D119).
func (p *Project) RedirectProblems() []string {
	targets, _ := p.RedirectTargets()
	var out []string
	for _, rt := range targets {
		if rt.Problem != "" {
			out = append(out, rt.Problem)
		}
	}
	return out
}
