package cli

import (
	"fmt"
	"strings"

	"leblanc.io/open-go-server/internal/archive"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
)

// dumpEntry nomme dans l'archive la sauvegarde d'une base, sous son moteur
// (D113). « dumps/<nom>.sql » ne disait pas lequel : deux bases homonymes d'un
// projet — « db add --engine postgres » reprend le nom de la base MySQL,
// l'identifiant du projet — y partageaient un nom, et la restauration rechargeait
// la sauvegarde de l'une dans l'autre. Le moteur est celui que la déclaration
// porte, validé parmi mysql, mariadb et postgres ; le nom suit la liste blanche
// des identifiants SQL : ni l'un ni l'autre ne porte de « / ».
func dumpEntry(d model.Database) string {
	return "dumps/" + d.Engine + "/" + d.Name + ".sql"
}

// legacyDumpEntry est le nom qu'une archive antérieure à D113 donne à la
// sauvegarde d'une base : relu tant qu'il ne désigne qu'une base.
func legacyDumpEntry(name string) string { return "dumps/" + name + ".sql" }

// dbLabel nomme une base avec son moteur, dans ce qu'ogs affiche d'une
// sauvegarde : le nom seul ne la distingue pas d'une homonyme.
func dbLabel(d model.Database) string { return d.Engine + " " + d.Name }

// dbArg rend l'argument qui désigne la base dans une ligne de commande : son nom,
// ou « <moteur>:<nom> » quand une autre base du projet porte le même nom.
func dbArg(p *model.Project, d model.Database) string {
	if len(homonyms(p, d.Name)) > 1 {
		return d.Engine + ":" + d.Name
	}
	return d.Name
}

// homonyms rend les bases du projet qui portent ce nom.
func homonyms(p *model.Project, name string) []model.Database {
	var out []model.Database
	for _, d := range p.Spec.Databases {
		if d.Name == name {
			out = append(out, d)
		}
	}
	return out
}

// findDatabase retrouve la base que désigne arg : « <moteur>:<nom> », ou le nom
// seul s'il ne désigne qu'une base. Un nom porté par deux bases est refusé en
// nommant les deux formes : la première trouvée était prise, et « db restore »
// rechargeait une sauvegarde MySQL dans la base PostgreSQL homonyme (D113).
func findDatabase(p *model.Project, arg string) (model.Database, error) {
	engine, name, qualified := strings.Cut(arg, ":")
	if !qualified {
		name, engine = arg, ""
	}
	var found []model.Database
	for _, d := range homonyms(p, name) {
		if engine == "" || d.Engine == engine {
			found = append(found, d)
		}
	}
	switch len(found) {
	case 0:
		return model.Database{}, exitf(CodeUsage, "aucune base %q déclarée dans %s", arg, p.ID())
	case 1:
		return found[0], nil
	}
	forms := make([]string, len(found))
	for i, d := range found {
		forms[i] = d.Engine + ":" + d.Name
	}
	return model.Database{}, exitf(CodeUsage, "plusieurs bases %s sont déclarées dans %s, sur des moteurs "+
		"différents : préciser laquelle, %s", name, p.ID(), strings.Join(forms, " ou "))
}

// archivedDump rend l'entrée de l'archive qui porte la sauvegarde de la base :
// celle que son moteur nomme, à défaut celle d'une archive antérieure à D113,
// si elle ne peut être qu'à cette base — l'archive n'en porte qu'une sous ce nom
// et la déclaration une seule base de ce nom. Sinon, la raison du refus.
func archivedDump(manifest archive.Manifest, p *model.Project, d model.Database) (string, error) {
	legacy := 0
	for _, e := range manifest.Entries {
		if e.Kind != "dump" {
			continue
		}
		switch e.Name {
		case dumpEntry(d):
			return e.Name, nil
		case legacyDumpEntry(d.Name):
			legacy++
		}
	}
	switch {
	case legacy == 0:
		return "", fmt.Errorf("aucune sauvegarde de la base %s", dbLabel(d))
	case legacy > 1 || len(homonyms(p, d.Name)) > 1:
		return "", ambiguousLegacy(d.Name, legacy)
	}
	return legacyDumpEntry(d.Name), nil
}

// ambiguousDumpError est le refus d'une sauvegarde d'archive antérieure à D113
// qu'ogs ne sait rapporter à aucune base : deux bases portent son nom, dans la
// déclaration ou dans l'archive, qui les rangeait alors sous un même nom.
type ambiguousDumpError struct {
	name  string
	count int
}

func ambiguousLegacy(name string, count int) error { return &ambiguousDumpError{name, count} }

func (e *ambiguousDumpError) Error() string {
	return fmt.Sprintf("l'archive, antérieure à D113, porte %d sauvegarde(s) %s sans leur moteur, et deux "+
		"bases portent ce nom : ogs ne choisit pas", e.count, legacyDumpEntry(e.name))
}

// remedy dit comment recharger à la main une sauvegarde ambiguë : l'en-tête du
// fichier dit le moteur.
func (e *ambiguousDumpError) remedy(id string) string {
	return fmt.Sprintf("les extraire à la main (tar --zstd -xf <archive> %s, zstd requis ; une seconde "+
		"entrée de même nom remplace la première), en lire l'en-tête — « MySQL dump », « MariaDB dump » ou "+
		"« PostgreSQL database dump » —, puis : ogs db restore %s <moteur>:%s -i <fichier>",
		legacyDumpEntry(e.name), id, e.name)
}

// heldBack retire d'un plan ce qui exécuterait le code du projet avant que la
// restauration n'ait remis son contenu (D113) : l'activation et le démarrage des
// instances, l'attente de disponibilité, le fichier de tâches planifiées. Le
// reste — compte, home, bases, unités et cible écrites, pool, vhosts — est posé :
// l'extraction a besoin du compte, le rechargement des bases de leur compte SGBD.
// Les instances démarraient sur un home vide, l'application échouait jusqu'à
// épuiser StartLimitBurst, et rien ne la relançait après l'extraction (R1-56).
// L'apply qui suit le contenu les pose : une instance qu'il ne trouve pas activée,
// il l'active et la démarre, puis attend l'application.
func heldBack(p *model.Project, steps []*engine.Step) []*engine.Step {
	kept := steps[:0]
	for _, s := range steps {
		switch {
		case strings.HasPrefix(s.Resource, "unit/instance/") && s.Action == engine.Create,
			s.Resource == "app/health/"+p.ID(),
			s.Resource == "cron/"+p.ID():
			continue
		}
		kept = append(kept, s)
	}
	return kept
}
