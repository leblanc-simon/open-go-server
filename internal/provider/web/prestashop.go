package web

import (
	"regexp"
	"strconv"
	"strings"

	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// Les .htaccess que livre Prestashop diffèrent d'une branche à l'autre (D99) :
// réécritures d'images, listes blanches de img/, js/ et themes/, refus de
// modules/ et d'upload/. Le template les porte branche par branche, d'après la
// version en place, lue dans les sources de la boutique au rendu du vhost.

// Branches de Prestashop dont le template porte les règles.
const (
	Prestashop17 = "1.7"
	Prestashop8  = "8"
	Prestashop9  = "9"
)

// BrancheParDefaut est celle dont les règles valent pour une version
// indéterminée — code pas encore déployé, 1.6, fichier illisible : la 8 est le
// moyen terme. Les refus propres à la 9 (scripts de modules/) casseraient les
// appels ajax des modules d'une 8, et les listes blanches de la 8 ne retirent à
// une 1.7 que des fichiers qu'aucune page ne demande.
const BrancheParDefaut = Prestashop8

// tailleMaxVersion borne la lecture d'un fichier de version : il appartient au
// compte du projet, qui lui donne la taille qu'il veut (D82).
const tailleMaxVersion = 256 << 10

// reVersion lit la constante VERSION telle que Prestashop l'écrit :
// « public const VERSION = '8.1.7'; » (src/Core/Version.php, depuis la 8),
// « const VERSION = '1.7.8.11'; » (app/AppKernel.php, en 1.7). Les préversions
// y portent le numéro seul (9.0.0-rc.1 écrit '9.0.0') ; un suffixe « -beta.1 »
// est admis et ignoré.
var reVersion = regexp.MustCompile(`(?m)^\s*(?:public\s+)?const\s+VERSION\s*=\s*'([0-9]+(?:\.[0-9]+)*)(?:-[0-9A-Za-z.]+)?'\s*;`)

// sourcesDeVersion sont, dans l'ordre, les fichiers où Prestashop porte sa
// version : en 8 et en 9, app/AppKernel.php la reprend de src/Core/Version.php.
var sourcesDeVersion = [][2]string{{"src/Core", "Version.php"}, {"app", "AppKernel.php"}}

// VersionPrestashop lit la version de la boutique servie par le docroot d'un
// projet, sans suivre de lien et seulement dans un fichier du propriétaire du
// docroot (D82). Rend "" si elle est introuvable ou illisible.
func VersionPrestashop(proj *model.Project) string {
	for _, src := range sourcesDeVersion {
		contenu, err := lireDansDocroot(proj, src[0], src[1], tailleMaxVersion)
		if err != nil || contenu == nil {
			continue
		}
		if m := reVersion.FindSubmatch(contenu); m != nil {
			return string(m[1])
		}
	}
	return ""
}

// BranchePrestashop range une version dans la branche dont le template porte
// les règles ; "" pour une version qu'il ne connaît pas (1.6, vide). Une
// version postérieure à la 9 reçoit les règles de la 9, les plus strictes.
func BranchePrestashop(version string) string {
	parties := strings.Split(version, ".")
	majeure, err := strconv.Atoi(parties[0])
	if err != nil {
		return ""
	}
	switch {
	case majeure == 1 && len(parties) > 1 && parties[1] == "7":
		return Prestashop17
	case majeure == 8:
		return Prestashop8
	case majeure >= 9:
		return Prestashop9
	}
	return ""
}

// CodePrestashopPresent dit si le docroot porte déjà le code d'une boutique —
// son index.php —, sans suivre de lien : une version indéterminée ne mérite
// d'être signalée qu'une fois le code déployé.
func CodePrestashopPresent(proj *model.Project) bool {
	base, ok := docrootRelatif(proj)
	if !ok {
		return false
	}
	d, err := fs.OpenDirAt(proj.Spec.Layout.Root, base)
	if err != nil {
		return false
	}
	defer d.Close()
	e, present, err := d.Entry("index.php")
	return err == nil && present && e.Regular
}

// MarquePrestashop est la ligne de commentaire par laquelle le vhost dit pour
// quelle branche il a été rendu : doctor la compare à la version en place.
func MarquePrestashop(branche string) string {
	return "Règles de Prestashop " + branche
}

// RenduPourBranche dit si un vhost rendu porte la marque de la branche, suivie
// du point final ou de la mention d'une version indéterminée.
func RenduPourBranche(vhost []byte, branche string) bool {
	m := MarquePrestashop(branche)
	return strings.Contains(string(vhost), m+".") || strings.Contains(string(vhost), m+" (")
}
