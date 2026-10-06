package hook

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// rootUID est le compte dont un hook et sa chaîne de répertoires doivent
// relever. C'est une variable pour que les essais, qui ne tournent pas en root,
// éprouvent le contrôle.
var rootUID uint32

// trustedDir dit si seul root peut modifier les entrées d'un répertoire : il lui
// appartient, et ni son groupe ni les autres n'y écrivent. Un répertoire collant
// inscriptible par tous — /tmp — n'en est pas : chacun y pose ses propres liens.
var trustedDir = func(st *syscall.Stat_t) bool { return st.Uid == rootUID && st.Mode&0o022 == 0 }

// maxLinks borne les liens suivis, comme le noyau.
const maxLinks = 40

// trustedPath résout path depuis la racine, composant par composant, et rend le
// chemin réel du fichier qu'il désigne. Chaque répertoire où un nom est cherché
// — ceux du chemin désigné, ceux où vit un lien, ceux de sa cible — doit être
// de confiance : sinon un autre compte que root choisirait ce que root exécute,
// en renommant un répertoire qu'il possède ou en remplaçant un lien. Le contrôle
// d'un chemin dont seul root modifie chaque maillon ne se périme pas : root
// peut ensuite l'exécuter par son nom.
func trustedPath(path string) (string, error) {
	p := filepath.Clean(path)
	for links := 0; links <= maxLinks; links++ {
		comps := strings.Split(strings.TrimPrefix(p, "/"), "/")
		dir, next := "/", ""
		for i, comp := range comps {
			if err := checkDir(dir); err != nil {
				return "", err
			}
			full := filepath.Join(dir, comp)
			info, err := os.Lstat(full)
			if err != nil {
				return "", err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(full)
				if err != nil {
					return "", err
				}
				if !filepath.IsAbs(target) {
					target = filepath.Join(dir, target)
				}
				next = filepath.Clean(filepath.Join(append([]string{target}, comps[i+1:]...)...))
				break
			}
			if i == len(comps)-1 {
				return full, nil
			}
			if !info.IsDir() {
				return "", fmt.Errorf("%s n'est pas un répertoire", full)
			}
			dir = full
		}
		p = next
	}
	return "", fmt.Errorf("trop de liens symboliques")
}

// checkDir refuse un répertoire qu'un autre compte que root peut modifier, en
// nommant le remède.
func checkDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || trustedDir(st) {
		return nil
	}
	err = fmt.Errorf("le répertoire %s (uid %d, mode %04o) peut être modifié par un autre compte "+
		"que root, qui choisirait alors le script exécuté en root : copier le script sous le "+
		"répertoire des hooks (install -o root -g root -m 0750 <script> <hook>), ou fermer ce "+
		"répertoire (chown root %s && chmod go-w %s)", dir, st.Uid, info.Mode().Perm(), dir, dir)
	if st.Uid == rootUID && st.Mode&0o020 != 0 {
		if g, gerr := user.LookupGroupId(strconv.FormatUint(uint64(st.Gid), 10)); gerr == nil && g.Name == "staff" {
			// Une Debian mise à niveau depuis la 10 ou avant garde /usr/local en
			// root:staff 2775 tant que /etc/staff-group-for-usr-local existe : le
			// groupe staff y pose ce qu'il veut, donc ce que root exécuterait.
			err = fmt.Errorf("%w — %s est au groupe staff, en écriture (Debian mise à niveau depuis la 10 "+
				"ou avant) : retirer /etc/staff-group-for-usr-local puis « chgrp root %s && chmod g-w,g-s %s », "+
				"ou copier le script sous le répertoire des hooks", err, dir, dir, dir)
		}
	}
	return err
}
