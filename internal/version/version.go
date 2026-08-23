// Package version porte les informations de build du binaire.
package version

import (
	"fmt"
	"runtime"
)

// Ces variables sont renseignées à la compilation via -ldflags -X.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String renvoie une description compacte, affichée par `ogs version`.
func String() string {
	return fmt.Sprintf("ogs %s (commit %s, %s, %s)", Version, Commit, Date, runtime.Version())
}

// Licence rappelle les termes de distribution. La WTFPL ne comportant aucune
// clause d'exclusion de garantie, celle-ci est portée explicitement (cf. NOTICE).
const Licence = `ogs est distribué sous licence WTFPL (Do What The Fuck You Want To Public License).

AVERTISSEMENT — ABSENCE DE GARANTIE
Ce logiciel est fourni « en l'état », sans aucune garantie d'aucune sorte, expresse
ou implicite. Il modifie la configuration du système, gère des utilisateurs, des
bases de données et des services, et peut supprimer des données. En aucun cas les
auteurs ne pourront être tenus responsables d'un dommage, d'une perte de données ou
d'une interruption de service résultant de son utilisation.`
