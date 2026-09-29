package fs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Chemins que l'exploitant désigne en ligne de commande.
//
// « ogs db dump -O » et « ogs db restore -i » écrivent et lisent en root là où
// l'exploitant le leur dit — et c'est souvent le home d'un projet, à la demande
// de son utilisateur. Ouvrir ce chemin par son nom, c'est laisser l'utilisateur,
// qui possède son home (D51), y poser un lien vers le fichier de son choix : root
// tronquait puis réécrivait /etc/sudoers.d/ogs-voisin à travers un
// ~/sauvegarde.sql, créait un fichier dans /etc/cron.d à travers un répertoire
// lié, et chargeait dans la base du projet la sauvegarde d'un voisin. Refuser
// tout lien casserait en revanche ce que l'administrateur a posé lui-même —
// /var/run vers /run, un /home ou un /var/lib/ogs déplacé sur un autre disque.
//
// La règle est celle du §15 : aucun processus root n'ouvre un chemin dont un
// compte autre que root contrôle un composant. Le chemin est parcouru composant
// par composant depuis la racine du système, par des descripteurs (Dir), et un
// lien n'est suivi que si seul root a pu le poser : dans un répertoire qui
// appartient à root, que ni son groupe ni les autres ne peuvent modifier, et
// atteint lui-même par de tels répertoires. Tout autre lien est refusé, à
// l'entrée comme en chemin. Sa cible est reprise depuis la racine, avec les mêmes
// contrôles : un lien de confiance ne rend pas sa cible digne de confiance.

// maxLinks borne les liens suivis sur un chemin, comme le noyau (MAXSYMLINKS).
const maxLinks = 40

// trustedDir dit si seul root peut modifier les entrées d'un répertoire : il lui
// appartient, et ni son groupe ni les autres n'y écrivent. Un répertoire collant
// inscriptible par tous — /tmp — n'en est pas : chacun y crée ses propres liens.
// C'est une variable pour que les essais, qui ne tournent pas en root, puissent
// éprouver le suivi d'un lien de confiance.
var trustedDir = func(st *unix.Stat_t) bool { return st.Uid == 0 && st.Mode&0o022 == 0 }

func (d *Dir) trusted() bool {
	var st unix.Stat_t
	return unix.Fstat(d.fd, &st) == nil && trustedDir(&st)
}

// OpenParentDir ouvre le répertoire qui contient path, et rend le nom de
// l'entrée finale, sans jamais suivre un lien qu'un autre compte que root aurait
// pu poser — ni en chemin, ni à l'entrée. Un lien de confiance est suivi ; un
// autre fait échouer un composant en chemin (ErrNotDirectory), et reste tel quel
// à l'entrée, que l'appelant décrit par Lstat pour le refuser. Avec create, un
// répertoire absent en chemin est créé en mode mode, au compte d'ogs — root.
//
// Un chemin relatif part du répertoire courant, lu tel que le noyau le connaît
// (getcwd) et non par $PWD, qui peut passer par des liens.
func OpenParentDir(path string, create bool, mode os.FileMode) (*Dir, string, error) {
	p := path
	if !filepath.IsAbs(p) {
		wd, err := unix.Getwd()
		if err != nil {
			return nil, "", fmt.Errorf("répertoire courant : %w", err)
		}
		p = filepath.Join(wd, p)
	}
	p = filepath.Clean(p)
	for links := 0; ; links++ {
		d, name, next, err := walkParent(p, create, mode)
		if err != nil || next == "" {
			if err != nil && p != filepath.Clean(path) {
				err = fmt.Errorf("%s : %w", path, err)
			}
			return d, name, err
		}
		if links >= maxLinks {
			return nil, "", fmt.Errorf("%s : %w", path, unix.ELOOP)
		}
		p = next
	}
}

// walkParent parcourt le chemin absolu et nettoyé p depuis la racine. Un lien de
// confiance interrompt le parcours : next est alors le chemin à reprendre, sa
// cible substituée au lien.
func walkParent(p string, create bool, mode os.FileMode) (d *Dir, name, next string, err error) {
	comps := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if comps[len(comps)-1] == "" {
		return nil, "", "", fmt.Errorf("%s : aucune entrée désignée", p)
	}
	d, err = OpenDir("/")
	if err != nil {
		return nil, "", "", err
	}
	// La confiance tient à toute la chaîne : un répertoire de root posé dans un
	// home se renomme et se remplace par l'utilisateur, quels que soient son
	// propriétaire et son mode.
	trusted := d.trusted()
	prefix := "/"
	last := len(comps) - 1
	for i, comp := range comps[:last] {
		sub, err := d.OpenDir(comp)
		if create && errors.Is(err, unix.ENOENT) {
			sub, err = d.Mkdir(comp, mode, KeepOwner)
			if errors.Is(err, unix.EEXIST) {
				sub, err = d.OpenDir(comp)
			}
		}
		if errors.Is(err, ErrNotDirectory) {
			info, lerr := d.Lstat(comp)
			isLink := lerr == nil && info.Mode()&os.ModeSymlink != 0
			switch {
			case isLink && trusted:
				// Seul root modifie ce répertoire : le lien lu est celui qui a
				// fait échouer l'ouverture.
				target, rerr := d.Readlink(comp)
				d.Close()
				if rerr != nil {
					return nil, "", "", rerr
				}
				return nil, "", substitute(prefix, target, comps[i+1:]), nil
			case isLink:
				err = fmt.Errorf("%s : %w — lien posé dans un répertoire qu'un autre compte que root peut "+
					"modifier : ogs ne le suit pas, désigner le chemin réel", filepath.Join(prefix, comp), ErrNotDirectory)
			}
		}
		d.Close()
		if err != nil {
			return nil, "", "", err
		}
		trusted = trusted && sub.trusted()
		d, prefix = sub, filepath.Join(prefix, comp)
	}
	name = comps[last]
	if trusted {
		if info, lerr := d.Lstat(name); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
			target, rerr := d.Readlink(name)
			d.Close()
			if rerr != nil {
				return nil, "", "", rerr
			}
			return nil, "", substitute(prefix, target, nil), nil
		}
	}
	return d, name, "", nil
}

// substitute compose le chemin à reprendre quand le lien de dir a pour cible
// target, suivi des composants rest. dir a été atteint sans suivre de lien qui ne
// soit remplacé par sa cible : c'est un chemin réel, et un « .. » de la cible s'y
// résout lexicalement comme le noyau le résoudrait.
func substitute(dir, target string, rest []string) string {
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return filepath.Clean(filepath.Join(append([]string{target}, rest...)...))
}
