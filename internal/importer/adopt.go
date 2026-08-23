package importer

import (
	"fmt"
	"path/filepath"

	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/naming"
	"github.com/leviathan/ogs/internal/provider/db"
)

// Adopt construit la déclaration d'un projet à partir d'un candidat. L'adoption
// ne renomme jamais : les ressources gardent leurs noms réels, l'identifiant ogs
// n'étant qu'une clé de regroupement. Un renommage vers la convention canonique
// est une opération distincte et destructrice.
func Adopt(c Candidate, homeRoot, prepDomain string) (*model.Project, []string, error) {
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
			decl := db.DefaultDatabase(p, r.Attr("engine"))
			// Le nom réel est conservé : renommer une base exigerait de la recréer.
			decl.Name = r.Name
			if users := splitList(r.Attr("users")); len(users) > 0 {
				decl.User = accountName(users[0])
			}
			p.Spec.Databases = append(p.Spec.Databases, decl)
			notes = append(notes, fmt.Sprintf(
				"le mot de passe de %s est inconnu d'ogs : il n'est ni deviné ni réinitialisé. "+
					"Employer « ogs db user rotate %s %s » pour en reprendre la main",
				decl.User, p.ID(), decl.User))
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
	if err := p.Normalize(homeRoot, prepDomain); err != nil {
		return nil, nil, err
	}
	// La normalisation a pu compléter des chemins : on rétablit ceux du système.
	if c.Home != "" {
		p.Spec.Layout.Root = c.Home
		p.Spec.User.Home = c.Home
		p.Spec.Layout.LogDir = filepath.Join(c.Home, "logs")
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

// accountName retire la partie hôte d'un compte MySQL.
func accountName(account string) string {
	for i := 0; i < len(account); i++ {
		if account[i] == '@' {
			return account[:i]
		}
	}
	return account
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
