package fs

import (
	"os/user"
	"strconv"
)

// LookupOwner résout un compte et un groupe en propriétaire. Une résolution
// impossible — compte pas encore créé, mode préfixé sans compte système — rend
// KeepOwner : le propriétaire en place est laissé, et c'est systemd-tmpfiles qui
// le corrigera au vidage des intentions.
func LookupOwner(account, group string) Owner {
	u, err := user.Lookup(account)
	if err != nil {
		return KeepOwner
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return KeepOwner
	}
	uid, errU := strconv.Atoi(u.Uid)
	gid, errG := strconv.Atoi(g.Gid)
	if errU != nil || errG != nil {
		return KeepOwner
	}
	return Owner{UID: uid, GID: gid}
}
