package hardening

import "syscall"

// systemesDistants associe la magie d'un système de fichiers à son nom, pour
// ceux dont le mode n'appartient pas à cette machine : le serveur en décide, et
// un export en root_squash fait échouer le chmod de root lui-même. La racine des
// homes est le cas courant — /home sur NFS est une pratique répandue.
//
// La liste ne prétend pas être exhaustive : un système de fichiers absent d'ici
// est simplement traité comme local, et un chmod qui échouerait rendrait un
// avertissement. L'inventaire évite le cas fréquent, il ne remplace pas le
// constat.
var systemesDistants = map[int64]string{
	0x6969:     "nfs",
	0xff534d42: "cifs",
	0xfe534d42: "smb2",
	0x517b:     "smbfs",
	0x65735546: "fuse",
	0x01021997: "9p",
	0x5346414f: "afs",
	0x00c36400: "ceph",
	0x47504653: "gpfs",
	0xbacbacbc: "vmhgfs",
}

// remoteFS nomme le système de fichiers du chemin lorsqu'il est distant. Une
// erreur de statfs ne conclut rien : le chemin est traité comme local, et le
// chmod dira lui-même s'il échoue.
func remoteFS(path string) (string, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return "", false
	}
	nom, distant := systemesDistants[int64(st.Type)]
	return nom, distant
}
