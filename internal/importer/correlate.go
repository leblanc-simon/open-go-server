package importer

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
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
	// Shell est le shell réel du compte repéré. Un projet adopté le garde : le
	// défaut du parc n'a pas à ouvrir — ni à fermer — l'accès d'un compte qui
	// existait avant ogs.
	Shell string `json:"shell,omitempty" yaml:"shell,omitempty"`
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
		if user, parLeHome := chercherCompte(byKind[KindUser], candidate, claimed); user != nil {
			if parLeHome {
				// Le home englobant le docroot est une déduction, pas une preuve :
				// un home partagé par plusieurs sites (/var/www, /srv) ne désigne
				// personne, et le premier vhost rencontré s'approprierait sinon le
				// compte, la racine et l'arborescence de tous les autres — l'apply
				// chownant spec.layout.root à l'utilisateur du projet. D'où la
				// revendication unique, et l'incertitude annoncée.
				candidate.User = user.Name
				candidate.Uncertain = append(candidate.Uncertain, fmt.Sprintf(
					"compte %s rapproché par le home englobant le docroot, non par un pool",
					user.Name))
			}
			candidate.Home = user.Attr("home")
			candidate.Shell = user.Attr("shell")
			candidate.Resources = append(candidate.Resources, *user)
			claimed[key(*user)] = true
			indices++
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

// chercherCompte retient le compte système d'un candidat, et dit s'il l'a été par
// son home faute de mieux.
//
// Le socket du pool a le dernier mot : c'est une preuve, et deux vhosts servis
// par le même pool partagent légitimement le compte — « claimed » ne s'y applique
// donc pas. À défaut, le home englobant le docroot est une déduction, et c'est le
// home le plus profond qui l'emporte : sous une racine commune vivent le compte
// du site et celui qui possède la racine, tous deux englobants, et retenir le
// premier venu dans l'ordre alphabétique donnerait au projet le compte et
// l'arborescence de tous ses voisins.
func chercherCompte(users []Resource, candidate Candidate, claimed map[string]bool) (*Resource, bool) {
	if candidate.User != "" {
		for i, user := range users {
			if !Managed(user) && user.Name == candidate.User {
				return &users[i], false
			}
		}
		return nil, false
	}
	if candidate.Docroot == "" {
		return nil, false
	}
	var retenu *Resource
	for i, user := range users {
		home := user.Attr("home")
		if Managed(user) || claimed[key(user)] || home == "" ||
			!sousRacine(home, candidate.Docroot) {
			continue
		}
		if retenu == nil ||
			len(filepath.Clean(home)) > len(filepath.Clean(retenu.Attr("home"))) {
			retenu = &users[i]
		}
	}
	return retenu, retenu != nil
}

// sousRacine dit si un chemin vit sous une racine, frontière de chemin comprise.
// Le préfixe brut rapprocherait « /var/www » de « /var/www2/site », qui n'a rien
// à voir avec lui : le candidat en hériterait le compte et la racine d'un autre.
func sousRacine(racine, chemin string) bool {
	rel, err := filepath.Rel(filepath.Clean(racine), filepath.Clean(chemin))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
