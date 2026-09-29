package system

import (
	"io/fs"
	"syscall"

	"golang.org/x/sys/unix"
)

// OwnerOf extrait le propriétaire d'un fichier depuis les informations système.
// Exporté : « doctor » contrôle ainsi le propriétaire de fichiers qu'ogs ne gère
// pas — le trousseau de clés commun, que sshd refuse s'il n'appartient pas à root.
// Les descriptions lues par descripteur (fs.LstatAt) portent un *unix.Stat_t,
// celles d'os.Lstat un *syscall.Stat_t : les deux sont lues.
func OwnerOf(info fs.FileInfo) (uid, gid int, ok bool) {
	switch st := info.Sys().(type) {
	case *syscall.Stat_t:
		return int(st.Uid), int(st.Gid), true
	case *unix.Stat_t:
		return int(st.Uid), int(st.Gid), true
	}
	return -1, -1, false
}
