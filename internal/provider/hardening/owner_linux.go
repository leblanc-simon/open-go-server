package hardening

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// ownerNames traduit l'uid et le gid d'un fichier en noms. Un identifiant sans
// compte correspondant est rendu tel quel : mieux vaut un rapport qui affiche
// « 998 » qu'un rapport qui se taise.
func ownerNames(info os.FileInfo) (string, string) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", ""
	}
	owner := strconv.FormatUint(uint64(st.Uid), 10)
	if u, err := user.LookupId(owner); err == nil {
		owner = u.Username
	}
	group := strconv.FormatUint(uint64(st.Gid), 10)
	if g, err := user.LookupGroupId(group); err == nil {
		group = g.Name
	}
	return owner, group
}

// fileGID rend le groupe numérique d'un fichier, ou -1. Il sert à décider si un
// fichier porte déjà le groupe du service : comparer les noms passerait par une
// résolution par fichier, et rendrait « 998 » là où la comparaison doit être
// exacte.
func fileGID(info os.FileInfo) int {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(st.Gid)
}
