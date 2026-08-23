package system

import (
	"io/fs"
	"syscall"
)

// ownerOf extrait le propriétaire d'un fichier depuis les informations système.
func ownerOf(info fs.FileInfo) (uid, gid int, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, -1, false
	}
	return int(st.Uid), int(st.Gid), true
}
