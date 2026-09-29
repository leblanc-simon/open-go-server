package importer

import (
	"fmt"
	"path/filepath"
	"strings"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/provider/db"
)

// Adopt construit la déclaration d'un projet à partir d'un candidat. L'adoption
// ne renomme jamais : les ressources gardent leurs noms réels, l'identifiant ogs
// n'étant qu'une clé de regroupement. Un renommage vers la convention canonique
// est une opération distincte et destructrice.
func Adopt(c Candidate, homeRoot, prepDomain string, opts ...model.NormalizeOption) (*model.Project, []string, error) {
	if c.Host == "" {
		return nil, nil, fmt.Errorf("candidat sans host : rien à adopter")
	}
	env := naming.EnvProd
	kind := c.Kind
	if kind == "" {
		kind = model.KindStatic
	}
	p := model.New(c.Host, env, kind)
	if c.ID != "" {
		p.Metadata.Name = c.ID
	}
	p.Metadata.Description = "projet adopté depuis la configuration existante"
	if p.Metadata.Labels == nil {
		p.Metadata.Labels = map[string]string{}
	}
	p.Metadata.Labels["adopted"] = "true"

	// L'arborescence et le compte sont ceux du système, non ceux qu'ogs aurait
	// choisis : l'adoption décrit l'existant.
	if c.Home != "" {
		p.Spec.Layout.Root = c.Home
		p.Spec.User.Home = c.Home
	}
	if c.Docroot != "" {
		p.Spec.Layout.Docroot = c.Docroot
	}
	if c.User != "" {
		p.Spec.User.Name = c.User
		p.Spec.User.Group = c.User
	}
	// Le shell aussi est un nom réel : la normalisation a posé celui du modèle, or
	// un compte adopté existait avant ogs, et lui ouvrir ou lui fermer l'accès au
	// premier apply serait un effet de bord que personne n'a demandé.
	if c.Shell != "" {
		p.Spec.User.Shell = c.Shell
	}
	if c.PHPVersion != "" {
		p.Spec.PHP.Pools = []model.Pool{{Name: model.MainPoolName, Version: c.PHPVersion}}
	}
	p.Spec.Domains = nil
	for i, d := range c.Domains {
		role := "alias"
		if i == 0 {
			role = "primary"
		}
		p.Spec.Domains = append(p.Spec.Domains, model.Domain{Name: d, Role: role})
	}

	var notes []string
	for _, r := range c.Resources {
		switch r.Kind {
		case KindVHost:
			// Le nom du fichier existant est conservé : le renommer reviendrait à
			// recréer le vhost, et le domaine serait vu comme un conflit.
			p.Spec.Web.VHosts = []model.VHost{{
				Name: r.Name, Domains: c.Domains, Docroot: c.Docroot,
			}}
			if c.PHPVersion != "" {
				p.Spec.Web.VHosts[0].Pool = model.MainPoolName
			}
		case KindDatabase:
			// Un compte seul de son nom, ou aucun : le premier d'une liste — le plus
			// court, dans l'ordre alphabétique — était celui d'un voisin, qu'ogs
			// accordait à l'apply et supprimait avec le projet. Sans compte sûr, la
			// base n'est pas déclarée : Correlate l'a déjà laissée hors gestion, et
			// cette garde tient pour un candidat composé ailleurs.
			compte, ok := compteSur(r)
			if !ok {
				notes = append(notes, fmt.Sprintf("base %s non déclarée : %s", r.Name, sansCompteSur(r)))
				continue
			}
			engine := r.Attr("engine")
			decl := db.DefaultDatabase(p, engine)
			// Le nom réel est conservé : renommer une base exigerait de la recréer.
			decl.Name = r.Name
			// L'hôte aussi est un nom réel : 'shop'@'%' et 'shop'@'localhost' sont
			// deux comptes. Déclarer le second faisait créer à l'apply un compte
			// neuf, au mot de passe tiré, que MySQL préfère au premier pour toute
			// connexion locale — et l'application perdait l'accès à sa base.
			user, host := splitAccount(engine, compte)
			decl.User = user
			if host != "" {
				decl.Host = host
			}
			p.Spec.Databases = append(p.Spec.Databases, decl)
			notes = append(notes, fmt.Sprintf(
				"le mot de passe de %s est inconnu d'ogs : il n'est ni deviné ni réinitialisé. "+
					"Employer « ogs db user rotate %s %s » pour en reprendre la main",
				compte, p.ID(), decl.User))
		case KindCertificate:
			if r.Attr("source") == "certbot" {
				notes = append(notes, fmt.Sprintf(
					"certificat %s géré par certbot : son timer continue de le renouveler. "+
						"Pour le confier à ogs, lancer « ogs cert issue %s », ce qui consomme une émission",
					r.Name, p.ID()))
			}
		case KindCron:
			notes = append(notes, fmt.Sprintf(
				"les tâches de %s ne sont pas reprises automatiquement : les redéclarer avec "+
					"« ogs cron add » pour qu'ogs les gère", r.Name))
		case KindUnit:
			notes = append(notes, fmt.Sprintf(
				"l'unité %s reste en place et hors gestion : la redéclarer avec « ogs service add » "+
					"si elle doit être pilotée par ogs", r.Name))
		}
	}
	if err := p.Normalize(homeRoot, prepDomain, opts...); err != nil {
		return nil, nil, err
	}
	// La normalisation a pu compléter des chemins : on rétablit ceux du système —
	// sauf les journaux, qui rejoignent la racine du parc (D71) : les vhosts d'un
	// projet adopté sont réécrits par ogs, et un répertoire de journaux dans le
	// home se renomme sans privilège.
	if c.Home != "" {
		p.Spec.Layout.Root = c.Home
		p.Spec.User.Home = c.Home
	}
	if c.Docroot != "" {
		p.Spec.Layout.Docroot = c.Docroot
	}
	if c.User != "" {
		p.Spec.User.Name = c.User
		p.Spec.User.Group = c.User
	}
	return p, notes, nil
}

// splitAccount sépare le nom et l'hôte d'un compte tel que le scan le relève :
// « user@host » pour MySQL et MariaDB, qui les concatènent, le rôle seul pour
// PostgreSQL, qui n'a pas d'hôte et dont un rôle peut contenir « @ ». Un hôte
// n'en contient jamais : la coupure se fait au dernier.
func splitAccount(engine, account string) (string, string) {
	if engine == "postgres" {
		return account, ""
	}
	if i := strings.LastIndex(account, "@"); i >= 0 {
		return account[:i], account[i+1:]
	}
	return account, ""
}

// Diverges compare la déclaration adoptée à ce qu'ogs aurait produit, et renvoie
// les écarts. C'est ce qui permet d'annoncer honnêtement qu'un premier apply
// normaliserait la configuration.
func Diverges(p *model.Project, homeRoot, prepDomain string) []string {
	canonical := model.New(p.Metadata.Host, p.Env(), p.Spec.App.Kind)
	canonical.Spec.App.Version = p.Spec.App.Version
	if err := canonical.Normalize(homeRoot, prepDomain); err != nil {
		return nil
	}
	var out []string
	if p.Spec.User.Name != canonical.Spec.User.Name {
		out = append(out, fmt.Sprintf("utilisateur : %s (convention : %s)",
			p.Spec.User.Name, canonical.Spec.User.Name))
	}
	if filepath.Clean(p.Spec.Layout.Root) != filepath.Clean(canonical.Spec.Layout.Root) {
		out = append(out, fmt.Sprintf("racine : %s (convention : %s)",
			p.Spec.Layout.Root, canonical.Spec.Layout.Root))
	}
	if p.Spec.Layout.Docroot != "" && canonical.Spec.Layout.Docroot != "" &&
		filepath.Clean(p.Spec.Layout.Docroot) != filepath.Clean(canonical.Spec.Layout.Docroot) {
		out = append(out, fmt.Sprintf("docroot : %s (convention : %s)",
			p.Spec.Layout.Docroot, canonical.Spec.Layout.Docroot))
	}
	for _, decl := range p.Spec.Databases {
		expected := db.DefaultDatabase(canonical, decl.Engine)
		if decl.Name != expected.Name {
			out = append(out, fmt.Sprintf("base %s : %s (convention : %s)",
				decl.Engine, decl.Name, expected.Name))
		}
	}
	return out
}
