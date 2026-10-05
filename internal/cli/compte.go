package cli

import (
	"leblanc.io/open-go-server/internal/model"
)

// webAccessACL est le mode d'accès du serveur web que la validation accepte
// depuis le premier lot, et qu'aucun provider n'a jamais implémenté (§7.1.3).
const webAccessACL = "acl"

// webAccessRefusal décrit le refus de « webAccess: acl » (D107, R1-29) : le
// mode était accepté et rendu comme « none », sans appartenance ni ACL — le
// serveur web ne traversait plus le home, et le site tombait en 403 ou 404 sans
// que rien ne dise pourquoi.
const webAccessRefusal = "spec.user.webAccess « acl » n'est pas implémenté : aucune ACL n'est posée, et le " +
	"serveur web n'atteindrait pas le site"

// webAccessFix est le remède du refus.
const webAccessFix = "déclarer webAccess: group (le défaut), ou none pour un site servi par un proxy sans statique"

// webAccessArchiveFix est le remède du refus pour une archive, dont la
// déclaration ne se corrige pas avant la restauration : il faut la sortir de
// l'archive, la corriger et la déposer soi-même, puis reprendre le contenu.
const webAccessArchiveFix = "extraire project.yaml de l'archive (tar --zstd -xOf <archive> project.yaml), y " +
	"remplacer webAccess: acl par group, le déposer sous /etc/ogs/projects/<id>.yaml, « ogs apply <id> », " +
	"puis reprendre le home et les bases depuis l'archive à la main"

// checkWebAccess refuse, au plan et à l'apply du projet visé, un mode d'accès
// que rien n'applique. Jamais au chargement : une déclaration ancienne qui le
// porte arrêterait tout le parc, « cert renew » compris — c'est doctor qui la
// signale.
func checkWebAccess(p *model.Project) error {
	if p.Spec.User.WebAccess == webAccessACL {
		return exitf(CodeValidation, "projet %s : %s ; %s", p.ID(), webAccessRefusal, webAccessFix)
	}
	return nil
}

// webAccessChecks signale à doctor les déclarations qui portent « acl », et
// « none » sur un type servi depuis un docroot : ce mode n'accorde rien, et le
// serveur web ne traverse pas un docroot en 0710 (D51) — le site répond 403.
// ogs n'en retire jamais www-data du groupe : un site passé de « group » à
// « none » continue d'être servi tant que l'appartenance reste (D107).
func webAccessChecks(projects []*model.Project, add func(name, status, detail, fix string)) {
	for _, p := range projects {
		switch p.Spec.User.WebAccess {
		case webAccessACL:
			add("accès web ("+p.ID()+")", "échec", webAccessRefusal, webAccessFix)
		case "none":
			at, err := p.AppType()
			if err != nil || at.Docroot == "" {
				continue
			}
			add("accès web ("+p.ID()+")", "avertissement",
				"spec.user.webAccess « none » sur un type servi depuis un docroot ("+string(at.Kind)+") : "+
					"rien n'accorde au serveur web la traversée du docroot (0710), le site ne vit que de "+
					"l'appartenance de www-data au groupe, qu'ogs ne pose ni ne retire",
				"déclarer webAccess: group (le défaut)")
		}
	}
}
