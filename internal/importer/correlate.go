package importer

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/naming"
)

// Confidence exprime la solidité d'un regroupement.
type Confidence string

const (
	// ConfidenceHigh : le vhost, le pool et le compte se recoupent.
	ConfidenceHigh Confidence = "élevée"
	// ConfidenceMedium : deux indices concordent.
	ConfidenceMedium Confidence = "moyenne"
	// ConfidenceLow : un seul indice, le reste est supposé.
	ConfidenceLow Confidence = "faible"
)

// Candidate est un projet proposé à l'adoption.
type Candidate struct {
	// ID est l'identifiant que produirait la règle de dérivation.
	ID string `json:"id" yaml:"id"`
	// Host est le domaine principal retenu.
	Host string `json:"host" yaml:"host"`
	// Domains énumère tous les domaines servis.
	Domains []string `json:"domains" yaml:"domains"`
	// User est le compte système repéré, s'il existe.
	User string `json:"user,omitempty" yaml:"user,omitempty"`
	// Home est la racine déduite du docroot ou du compte.
	Home string `json:"home,omitempty" yaml:"home,omitempty"`
	// Docroot est le répertoire servi.
	Docroot string `json:"docroot,omitempty" yaml:"docroot,omitempty"`
	// PHPVersion est la version du pool associé.
	PHPVersion string `json:"phpVersion,omitempty" yaml:"phpVersion,omitempty"`
	// Kind est le type d'application deviné d'après le docroot.
	Kind model.AppKind `json:"kind" yaml:"kind"`
	// Resources énumère les ressources rattachées.
	Resources []Resource `json:"resources" yaml:"resources"`
	// Uncertain énumère ce qui n'est pas déductible et reste à confirmer.
	Uncertain  []string   `json:"uncertain,omitempty" yaml:"uncertain,omitempty"`
	Confidence Confidence `json:"confidence" yaml:"confidence"`
}

// Correlate regroupe les ressources en projets candidats. Le rapprochement
// s'appuie sur ce qui est vérifiable — socket FPM référencé par un vhost,
// utilisateur d'un pool, docroot commun — et signale explicitement ce qui ne
// l'est pas, plutôt que de deviner.
func Correlate(resources []Resource, homeRoot, prepDomain string) ([]Candidate, []Resource) {
	byKind := map[Kind][]Resource{}
	for _, r := range resources {
		byKind[r.Kind] = append(byKind[r.Kind], r)
	}
	claimed := map[string]bool{}
	var candidates []Candidate

	// Le vhost est le point de départ : c'est lui qui porte les domaines, et un
	// site sans vhost n'est pas servi.
	for _, vhost := range byKind[KindVHost] {
		if Managed(vhost) {
			claimed[key(vhost)] = true
			continue
		}
		domains := splitList(vhost.Attr("domains"))
		if len(domains) == 0 {
			continue
		}
		candidate := Candidate{
			Host: domains[0], Domains: domains, Docroot: vhost.Attr("docroot"),
			Resources: []Resource{vhost}, Confidence: ConfidenceLow,
		}
		claimed[key(vhost)] = true
		indices := 1

		// Le socket FPM référencé par le vhost désigne le pool sans ambiguïté.
		if socket := vhost.Attr("fpm"); socket != "" {
			for _, pool := range byKind[KindPool] {
				if Managed(pool) {
					continue
				}
				if pool.Attr("listen") == socket {
					candidate.Resources = append(candidate.Resources, pool)
					candidate.PHPVersion = pool.Attr("version")
					candidate.User = pool.Attr("user")
					claimed[key(pool)] = true
					indices++
					break
				}
			}
		}
		// À défaut, un pool portant un nom proche du domaine.
		if candidate.PHPVersion == "" {
			base, _ := naming.BaseID(candidate.Host)
			for _, pool := range byKind[KindPool] {
				if Managed(pool) || claimed[key(pool)] {
					continue
				}
				if similar(pool.Name, base) || similar(pool.Attr("pool"), base) {
					candidate.Resources = append(candidate.Resources, pool)
					candidate.PHPVersion = pool.Attr("version")
					if candidate.User == "" {
						candidate.User = pool.Attr("user")
					}
					claimed[key(pool)] = true
					candidate.Uncertain = append(candidate.Uncertain,
						fmt.Sprintf("pool %s rapproché par son nom, non par le socket", pool.Name))
					break
				}
			}
		}

		// Le compte système : celui du pool, ou celui dont le home contient le docroot.
		for _, user := range byKind[KindUser] {
			if Managed(user) {
				continue
			}
			home := user.Attr("home")
			switch {
			case candidate.User != "" && user.Name == candidate.User:
			case candidate.User == "" && candidate.Docroot != "" && home != "" &&
				strings.HasPrefix(candidate.Docroot, home):
				candidate.User = user.Name
				indices++
			default:
				continue
			}
			candidate.Home = home
			candidate.Resources = append(candidate.Resources, user)
			claimed[key(user)] = true
			indices++
			break
		}
		if candidate.Home == "" && candidate.Docroot != "" {
			// Sans compte, la racine est déduite du docroot : www/ ou public/ en
			// dessous, sinon le docroot lui-même.
			candidate.Home = deduceHome(candidate.Docroot)
			candidate.Uncertain = append(candidate.Uncertain,
				"racine déduite du docroot : aucun compte système rapproché")
		}
		if candidate.User == "" {
			candidate.Uncertain = append(candidate.Uncertain,
				"aucun utilisateur système repéré : il sera créé à l'application")
		}
		candidate.Kind = guessKind(candidate.Docroot, candidate.PHPVersion)

		// Les tâches et unités de l'utilisateur lui sont rattachées.
		if candidate.User != "" {
			for _, cron := range byKind[KindCron] {
				if Managed(cron) || claimed[key(cron)] {
					continue
				}
				if cron.Attr("user") == candidate.User ||
					strings.Contains(cron.Name, candidate.User) {
					candidate.Resources = append(candidate.Resources, cron)
					claimed[key(cron)] = true
				}
			}
			for _, u := range byKind[KindUnit] {
				if Managed(u) || claimed[key(u)] {
					continue
				}
				if u.Attr("user") == candidate.User {
					candidate.Resources = append(candidate.Resources, u)
					claimed[key(u)] = true
				}
			}
		}

		// Les bases : rapprochées par nom, avec une incertitude assumée sur la
		// propriété, qui n'est pas déductible du serveur.
		base, _ := naming.BaseID(candidate.Host)
		for _, database := range byKind[KindDatabase] {
			if claimed[key(database)] {
				continue
			}
			if similar(database.Name, base) ||
				(candidate.User != "" && similar(database.Name, candidate.User)) {
				candidate.Resources = append(candidate.Resources, database)
				claimed[key(database)] = true
				candidate.Uncertain = append(candidate.Uncertain,
					fmt.Sprintf("propriété de la base %s à confirmer : elle n'est pas déductible",
						database.Name))
			}
		}
		// Les certificats couvrant l'un des domaines.
		for _, cert := range byKind[KindCertificate] {
			if Managed(cert) || claimed[key(cert)] {
				continue
			}
			for _, d := range domains {
				if cert.Name == d {
					candidate.Resources = append(candidate.Resources, cert)
					claimed[key(cert)] = true
					if cert.Attr("source") == "certbot" {
						candidate.Uncertain = append(candidate.Uncertain,
							"certificat géré par certbot : conserver son timer ou réémettre avec ogs")
					}
					break
				}
			}
		}

		// Trois sources concordantes — le vhost, le pool désigné par son socket et
		// le compte système — suffisent à une confiance élevée : au-delà, on
		// n'apprend plus rien sur l'appartenance.
		switch {
		case indices >= 3:
			candidate.Confidence = ConfidenceHigh
		case indices >= 2:
			candidate.Confidence = ConfidenceMedium
		}
		if id, err := naming.BaseID(candidate.Host); err == nil {
			candidate.ID = id
		} else {
			candidate.Uncertain = append(candidate.Uncertain,
				fmt.Sprintf("identifiant non dérivable du host %q : à fournir", candidate.Host))
		}
		candidates = append(candidates, candidate)
	}

	// Tout ce qui n'a pas été rattaché reste non géré, et ne sera jamais touché.
	var unmanaged []Resource
	for _, r := range resources {
		if claimed[key(r)] || Managed(r) {
			continue
		}
		unmanaged = append(unmanaged, r)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	return candidates, unmanaged
}

// deduceHome remonte du docroot vers la racine probable du projet.
func deduceHome(docroot string) string {
	base := filepath.Base(docroot)
	if base == "public" || base == "web" || base == "html" {
		parent := filepath.Dir(docroot)
		if filepath.Base(parent) == "www" {
			return filepath.Dir(parent)
		}
		return parent
	}
	if base == "www" {
		return filepath.Dir(docroot)
	}
	return docroot
}

// guessKind devine le type d'application d'après le docroot. La supposition est
// annoncée comme telle : c'est à l'adoption de la confirmer.
func guessKind(docroot, phpVersion string) model.AppKind {
	if phpVersion == "" {
		return model.KindStatic
	}
	switch filepath.Base(docroot) {
	case "public":
		return model.KindSymfony
	case "web":
		// web/ était l'usage de Symfony 2 et 3.
		return model.KindSymfony
	}
	return model.KindPHPPlain
}

// similar rapproche deux noms en ignorant les séparateurs : « shop_fr »,
// « shop-fr » et « shopfr » désignent en pratique le même site.
func similar(a, b string) bool {
	na, nb := normalizeName(a), normalizeName(b)
	if na == "" || nb == "" {
		return false
	}
	return na == nb || strings.HasPrefix(na, nb) || strings.HasPrefix(nb, na)
}

func normalizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func key(r Resource) string { return string(r.Kind) + "\x00" + r.Name }

func splitList(v string) []string {
	if v == "" {
		return nil
	}
	return dedupe(strings.Split(v, ","))
}
