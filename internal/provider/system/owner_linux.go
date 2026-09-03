package system

import (
	"io/fs"
	"syscall"
)

// OwnerOf extrait le propriétaire d'un fichier depuis les informations système.
// Exporté : « doctor » contrôle ainsi le propriétaire de fichiers qu'ogs ne gère
// pas — le trousseau de clés commun, que sshd refuse s'il n'appartient pas à root.
func OwnerOf(info fs.FileInfo) (uid, gid int, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, -1, false
	}
	return int(st.Uid), int(st.Gid), true
}
