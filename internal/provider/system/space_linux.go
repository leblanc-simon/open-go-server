package system

import "golang.org/x/sys/unix"

// freeSpace rend l'espace disponible et total de la partition qui porte path.
func freeSpace(path string) (avail, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), nil
}
