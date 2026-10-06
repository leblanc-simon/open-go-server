package hook

import (
	"fmt"
	"io/fs"
	"syscall"
)

// ownedByRoot vérifie que le script appartient à root : un hook s'exécutant avec
// les privilèges du superutilisateur, son propriétaire en détient l'usage.
func ownedByRoot(info fs.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if st.Uid != rootUID {
		return fmt.Errorf("appartient à l'uid %d et non à root : il s'exécuterait pourtant en root", st.Uid)
	}
	return nil
}
