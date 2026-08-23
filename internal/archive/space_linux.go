package archive

import "golang.org/x/sys/unix"

// available interroge le système de fichiers : archiver sans vérifier l'espace
// libre reviendrait à échouer au milieu d'une suppression.
func available(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}
