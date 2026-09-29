package fs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Arborescence d'un projet : les répertoires déclarés sous son home.
//
// EnsureDir n'ouvre sans le suivre que le dernier composant : ses parents sont
// résolus par leur nom, à chaque appel. Sous le home, c'est laisser l'utilisateur
// choisir où root crée et à qui il donne un répertoire — il possède son home
// (D51), et y renomme une entrée sans privilège. Entre l'observation de ~/.local,
// jugé conforme, et l'étape de ~/.local/bin, un « mv .local .x && ln -s
// /usr/local .local » faisait poser par root le mode et le propriétaire du projet
// sur /usr/local/bin, en tête du PATH de root. Ces fonctions partent donc de la
// racine du projet, ouverte sans suivre de lien, et ouvrent chaque composant par
// un descripteur relatif au précédent (Dir) : le répertoire corrigé est celui que
// l'on a atteint sans jamais suivre de lien, quoi qu'il advienne des noms
// ensuite. La racine elle-même vit sous la racine des homes, qui appartient à
// root : l'utilisateur ne peut pas la substituer.

// relComponents découpe rel, relatif à la racine d'un projet, en composants. Un
// « .. » est refusé : il ferait sortir de la racine.
func relComponents(rel string) ([]string, error) {
	var out []string
	for _, comp := range strings.Split(filepath.ToSlash(filepath.Clean(rel)), "/") {
		switch comp {
		case "", ".":
			continue
		case "..":
			return nil, fmt.Errorf("%s : « .. » refusé dans un chemin relatif au projet", rel)
		}
		out = append(out, comp)
	}
	return out, nil
}

// walkDir ouvre root, puis chacun des composants comps sans suivre de lien. Avec
// create, un composant absent est créé en mode mode et donné à owner.
func walkDir(root string, comps []string, create bool, mode os.FileMode, owner Owner) (*Dir, error) {
	d, err := OpenDir(root)
	if err != nil {
		return nil, err
	}
	for _, comp := range comps {
		sub, err := d.OpenDir(comp)
		if create && errors.Is(err, unix.ENOENT) {
			sub, err = d.Mkdir(comp, mode, owner)
			if errors.Is(err, unix.EEXIST) {
				// Apparu entre l'ouverture et la création : il est rouvert, sans
				// être suivi s'il est un lien.
				sub, err = d.OpenDir(comp)
			}
		}
		d.Close()
		if err != nil {
			return nil, err
		}
		d = sub
	}
	return d, nil
}

// EnsureDirAt crée au besoin le répertoire rel de la racine root, puis lui pose
// mode et propriétaire, sans jamais suivre de lien — ni à l'entrée, ni en chemin.
// Un composant remplacé par un lien, ou par autre chose qu'un répertoire, rend
// ErrNotDirectory au lieu d'être traversé. Un parent absent est créé comme
// l'entrée, en mode mode et au nom d'owner : laissé à root, il serait
// infranchissable pour le projet, et aucune étape ne le reprendrait.
//
// Mode et propriétaire sont posés sur le descripteur du répertoire atteint,
// jamais sur son nom. Les modes sont explicites : l'umask ambiant ne les rogne
// pas.
func EnsureDirAt(root, rel string, mode os.FileMode, owner Owner) error {
	comps, err := relComponents(rel)
	if err != nil {
		return err
	}
	if len(comps) == 0 {
		return SetDirAt(root, rel, mode, owner)
	}
	parent, err := walkDir(root, comps[:len(comps)-1], true, mode, owner)
	if err != nil {
		return err
	}
	defer parent.Close()
	name := comps[len(comps)-1]
	d, err := parent.OpenDir(name)
	if errors.Is(err, unix.ENOENT) {
		d, err = parent.Mkdir(name, mode, owner)
		if err == nil {
			return d.Close()
		}
		if errors.Is(err, unix.EEXIST) {
			d, err = parent.OpenDir(name)
		}
	}
	if err != nil {
		return err
	}
	defer d.Close()
	return setOwnerMode(d.fd, d.path, mode, owner)
}

// SetDirAt repose mode et propriétaire du répertoire existant rel de la racine
// root — la racine elle-même si rel est vide —, sans suivre de lien ni à l'entrée
// ni en chemin. Il ne crée rien : c'est la compensation d'une correction, qui
// rend son état d'origine à un répertoire que l'étape a trouvé en place.
func SetDirAt(root, rel string, mode os.FileMode, owner Owner) error {
	comps, err := relComponents(rel)
	if err != nil {
		return err
	}
	d, err := walkDir(root, comps, false, 0, KeepOwner)
	if err != nil {
		return err
	}
	defer d.Close()
	return setOwnerMode(d.fd, d.path, mode, owner)
}

// LstatAt décrit l'entrée rel de la racine root sans la suivre, et sans suivre
// de lien en chemin : un lien est décrit comme tel, un parent qui n'est pas un
// répertoire véritable rend ErrNotDirectory. os.Lstat ne protège que le dernier
// composant, et décrivait la cible d'un parent substitué — le plan annonçait
// alors une correction de /usr/local/bin sous le nom de ~/.local/bin. Sys rend
// un *unix.Stat_t.
func LstatAt(root, rel string) (os.FileInfo, error) {
	comps, err := relComponents(rel)
	if err != nil {
		return nil, err
	}
	if len(comps) == 0 {
		return nil, fmt.Errorf("%s : entrée attendue sous la racine", root)
	}
	d, err := walkDir(root, comps[:len(comps)-1], false, 0, KeepOwner)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.Lstat(comps[len(comps)-1])
}
