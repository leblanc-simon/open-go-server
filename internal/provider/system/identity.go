// Package system porte le provider des utilisateurs, groupes et arborescences.
package system

import (
	"bufio"
	"errors"
	"os"
	"os/user"
	"strconv"
	"strings"
)

// UserInfo décrit un compte système observé.
type UserInfo struct {
	Name  string
	UID   int
	GID   int
	Home  string
	Shell string
	// Managed indique que le compte porte la marque d'ogs dans son champ
	// commentaire : c'est ce qui distingue un compte créé par l'outil d'un compte
	// adopté ou étranger.
	Managed bool
}

// GroupInfo décrit un groupe observé.
type GroupInfo struct {
	Name    string
	GID     int
	Members []string
}

// Identity interroge la base des comptes. L'indirection permet de tester le
// provider sans toucher aux comptes de la machine.
type Identity interface {
	LookupUser(name string) (*UserInfo, error)
	LookupGroup(name string) (*GroupInfo, error)
}

// ErrNoSuchEntry signale un compte ou un groupe absent.
var ErrNoSuchEntry = errors.New("entrée absente")

// SystemIdentity lit la base des comptes de la machine.
type SystemIdentity struct {
	// PasswdPath et GroupPath permettent de pointer d'autres fichiers en test.
	PasswdPath string
	GroupPath  string
}

// LookupUser cherche un compte.
func (s *SystemIdentity) LookupUser(name string) (*UserInfo, error) {
	if s.PasswdPath != "" {
		return s.lookupUserFile(name)
	}
	u, err := user.Lookup(name)
	if err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return nil, nil
		}
		return nil, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return &UserInfo{
		Name: u.Username, UID: uid, GID: gid, Home: u.HomeDir,
		Managed: strings.HasPrefix(u.Name, managedMarker),
	}, nil
}

// LookupGroup cherche un groupe.
func (s *SystemIdentity) LookupGroup(name string) (*GroupInfo, error) {
	if s.GroupPath != "" {
		return s.lookupGroupFile(name)
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		var unknown user.UnknownGroupError
		if errors.As(err, &unknown) {
			return nil, nil
		}
		return nil, err
	}
	gid, _ := strconv.Atoi(g.Gid)
	members, err := groupMembers(defaultGroupPath, name)
	if err != nil {
		return nil, err
	}
	return &GroupInfo{Name: g.Name, GID: gid, Members: members}, nil
}

const (
	// managedMarker est inscrit dans le champ commentaire (GECOS) du compte. Il ne
	// peut contenir ni « : » (séparateur de champs de /etc/passwd) ni « , »
	// (séparateur de sous-champs du GECOS).
	managedMarker    = "ogs="
	defaultGroupPath = "/etc/group"
)

func (s *SystemIdentity) lookupUserFile(name string) (*UserInfo, error) {
	f, err := os.Open(s.PasswdPath)
	if err != nil {
		// Une base de comptes absente équivaut à une base vide : c'est le cas au
		// premier apply en mode préfixé.
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 7 || fields[0] != name {
			continue
		}
		uid, _ := strconv.Atoi(fields[2])
		gid, _ := strconv.Atoi(fields[3])
		return &UserInfo{
			Name: fields[0], UID: uid, GID: gid, Home: fields[5], Shell: fields[6],
			Managed: strings.HasPrefix(fields[4], managedMarker),
		}, nil
	}
	return nil, sc.Err()
}

func (s *SystemIdentity) lookupGroupFile(name string) (*GroupInfo, error) {
	f, err := os.Open(s.GroupPath)
	if err != nil {
		// Une base de comptes absente équivaut à une base vide : c'est le cas au
		// premier apply en mode préfixé.
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 4 || fields[0] != name {
			continue
		}
		gid, _ := strconv.Atoi(fields[2])
		var members []string
		if fields[3] != "" {
			members = strings.Split(fields[3], ",")
		}
		return &GroupInfo{Name: fields[0], GID: gid, Members: members}, nil
	}
	return nil, sc.Err()
}

// groupMembers lit les membres secondaires d'un groupe. os/user ne les expose
// pas sans CGO, d'où la lecture directe de /etc/group.
func groupMembers(path, name string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 4 || fields[0] != name {
			continue
		}
		if fields[3] == "" {
			return nil, nil
		}
		return strings.Split(fields[3], ","), nil
	}
	return nil, sc.Err()
}

// FakeIdentity est une base de comptes en mémoire, pour les tests.
type FakeIdentity struct {
	Users  map[string]*UserInfo
	Groups map[string]*GroupInfo
}

// NewFakeIdentity construit une base vide.
func NewFakeIdentity() *FakeIdentity {
	return &FakeIdentity{Users: map[string]*UserInfo{}, Groups: map[string]*GroupInfo{}}
}

// LookupUser cherche un compte en mémoire.
func (f *FakeIdentity) LookupUser(name string) (*UserInfo, error) { return f.Users[name], nil }

// LookupGroup cherche un groupe en mémoire.
func (f *FakeIdentity) LookupGroup(name string) (*GroupInfo, error) { return f.Groups[name], nil }
