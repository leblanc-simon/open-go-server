package system

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"leblanc.io/open-go-server/internal/model"
)

// foreignEntriesShown borne les entrées nommées par un refus : il s'agit de les
// reconnaître, non de les inventorier.
const foreignEntriesShown = 5

// LoginDefsPath porte la plage des comptes ordinaires. Une variable, pour les
// tests.
var LoginDefsPath = "/etc/login.defs"

// PreexistingHome refuse de créer un compte sur un home tenu par quelqu'un
// d'autre (§7.1.7, D95). Le compte est créé sans --create-home, et
// l'arborescence reçoit le propriétaire du projet : un home laissé par un autre
// compte passait ainsi au nouveau compte, sa racine chownée sans que le plan le
// dise, et « project delete » l'effaçait ensuite en entier, le contrôle du
// propriétaire de la racine voyant alors celui du projet.
//
// Le critère est le propriétaire, non le contenu : est refusée une racine qui
// appartient à un compte existant autre que root, ou dont une entrée de premier
// niveau appartient à un compte ordinaire existant (ForeignEntries, le critère
// de « project delete »). Une racine à root ou à un UID orphelin, dont les
// entrées sont à root, à un UID orphelin ou à un compte système, n'est reprise
// — le plan l'annonce (dirSteps) — que vide, connue de la base d'état à ce
// chemin (KnownHome), ou réduite à ce qu'ogs y pose (notSkeleton) : c'est ce que
// laissent un compte supprimé à la main, une saga de création déroulée —
// l'ancien UID devient orphelin —, ou une suppression qui a retiré le compte
// sans le home. Refuser tout home non vide empêchait l'apply de réparer ces cas ;
// reprendre tout home orphelin donnait au projet celui d'un ancien compte.
//
// Ne s'applique qu'à la création du compte, hors adoption : un projet adopté
// reprend légitimement ce qui existe, et un compte déjà là a déjà son home. En
// mode préfixé, useradd est simulé et le compte n'existe jamais : chaque apply
// serait une création, sur l'arborescence posée par le précédent.
func (p *Provider) PreexistingHome(proj *model.Project, obs Observed) error {
	if obs.User != nil || proj.Metadata.Labels["adopted"] == "true" || p.Exec.Simulated() {
		return nil
	}
	root := proj.Spec.Layout.Root
	state := obs.Dirs[root]
	if !state.Exists || state.Symlink {
		// Un lien à la place de la racine a son propre refus (dirSteps).
		return nil
	}
	remede := "le rendre à root ou le déplacer, choisir un autre spec.layout.root, ou reprendre le site " +
		"existant avec « ogs import »"
	nameOf := p.AccountOfUID
	if nameOf == nil {
		nameOf = AccountOfUID
	}
	if state.UID > 0 {
		owner, err := nameOf(state.UID)
		if err != nil {
			return fmt.Errorf("le home %s existe déjà et son propriétaire (UID %d) est illisible (%v) ; %s",
				root, state.UID, err, remede)
		}
		if owner != "" {
			return fmt.Errorf("le home %s existe déjà et appartient au compte %s (UID %d) : ogs ne le donne "+
				"pas à l'utilisateur %s, à créer ; %s", root, owner, state.UID, proj.Spec.User.Name, remede)
		}
	}
	found, err := ForeignEntries(root, -1, nameOf)
	if err != nil {
		return fmt.Errorf("le home %s existe déjà et n'a pas pu être inspecté (%v) : ogs ne crée pas "+
			"l'utilisateur %s sur un home qu'il ne voit pas ; %s", root, err, proj.Spec.User.Name, remede)
	}
	if len(found) > 0 {
		return fmt.Errorf("le home %s existe déjà et porte des entrées d'autres comptes — %s : ogs ne crée "+
			"pas l'utilisateur %s sur un home tenu par quelqu'un d'autre, qui lui serait donné puis effacé "+
			"avec le projet ; %s", root, strings.Join(found, ", "), proj.Spec.User.Name, remede)
	}
	// À root ou à un UID orphelin, un home non vide n'est repris que s'il est
	// reconnu comme celui du projet : la base d'état le porte à ce chemin, ou il
	// ne contient que ce qu'ogs y pose. Le home d'un compte supprimé sans -r a
	// précisément ce propriétaire, et useradd redonne volontiers l'UID libéré au
	// nouveau compte : celui-ci recevait les fichiers de l'ancien, et son
	// authorized_keys régulier — jamais remplacé (D61), accepté par StrictModes —
	// ouvrait un shell sous le compte du projet à la clé de l'ancien titulaire.
	if p.KnownHome != nil && p.KnownHome(proj) {
		return nil
	}
	strangers, err := p.notSkeleton(proj)
	if err != nil {
		return fmt.Errorf("le home %s existe déjà et n'a pas pu être inspecté (%v) : ogs ne crée pas "+
			"l'utilisateur %s sur un home qu'il ne voit pas ; %s", root, err, proj.Spec.User.Name, remede)
	}
	if len(strangers) > 0 {
		tenu := "root"
		if state.UID > 0 {
			tenu = fmt.Sprintf("l'UID %d, sans compte", state.UID)
		}
		return fmt.Errorf("le home %s existe déjà, à %s, et porte ce qu'ogs n'y pose pas — %s : sans doute le "+
			"home d'un compte supprimé, dont le nouveau compte %s recevrait les fichiers, l'UID et l'accès "+
			"ssh, puis qu'il effacerait avec le projet ; %s", root, tenu, strings.Join(strangers, ", "),
			proj.Spec.User.Name, remede)
	}
	return nil
}

// notSkeleton nomme ce qui, dans la racine, n'est pas de ceux qu'ogs y pose :
// au premier niveau, les répertoires de spec.layout.dirs, ceux des gestionnaires
// de runtime (.nvm, .rbenv, .pyenv, et les venvs .venv-<version> de D123 avec
// leur lien ~/.venv, qu'une saga déroulée laisse), les anciens
// .bin et logs et le lien ~/logs (D70, D71) ; dans .ssh, la clé de déploiement,
// known_hosts et le lien vers le trousseau commun (D61) ; dans .local, bin, qui
// ne porte que des liens (D70). Ni le contenu des répertoires du projet — www/,
// c'est le site —, ni celui des gestionnaires de runtime ne sont jugés : pour ces
// derniers, c'est une limite, que D95 nomme.
// Tout est lu sans suivre de lien.
func (p *Provider) notSkeleton(proj *model.Project) ([]string, error) {
	root := proj.Spec.Layout.Root
	dirs := map[string]bool{".local": true, ".ssh": true, ".nvm": true, ".rbenv": true, ".pyenv": true,
		model.LegacyLinkDir: true, model.LogLink: true}
	for _, d := range proj.Spec.Layout.Dirs {
		dirs[strings.SplitN(filepath.Clean(d.Path), string(filepath.Separator), 2)[0]] = true
	}
	var out []string
	add := func(s string) bool {
		if len(out) == foreignEntriesShown {
			out = append(out, "…")
			return false
		}
		out = append(out, s)
		return true
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	entries, err := readDirFD(rootFD, root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == model.LogLink && e.Type()&os.ModeSymlink != 0:
			continue
		case name == ".venv" && e.Type()&os.ModeSymlink != 0:
			// Le lien vers le venv de la version déclarée (D123).
			continue
		case strings.HasPrefix(name, ".venv-") && e.IsDir():
			// Le venv d'une version de Python (D123), qu'une saga déroulée laisse
			// comme .pyenv : son contenu n'est pas jugé, avec la même limite.
			continue
		case !dirs[name] || !e.IsDir():
			if !add(strconv.Quote(name)) {
				return out, nil
			}
			continue
		}
		var allowed func(os.DirEntry) bool
		switch name {
		case ".ssh":
			keyring := p.Paths.DefaultAuthorizedKeys()
			allowed = func(c os.DirEntry) bool {
				switch c.Name() {
				case KeyName, KeyName + ".pub", "known_hosts", "known_hosts.old":
					return c.Type().IsRegular()
				case "authorized_keys":
					if c.Type()&os.ModeSymlink == 0 {
						return false
					}
					target, err := readlinkAt(rootFD, filepath.Join(".ssh", "authorized_keys"))
					return err == nil && target == keyring
				}
				return false
			}
		case ".local":
			allowed = func(c os.DirEntry) bool { return c.Name() == "bin" && c.IsDir() }
		default:
			continue
		}
		if name == ".local" {
			// ogs ne pose que des liens dans .local/bin (D70) : un fichier ou un
			// répertoire y serait exécuté par le PATH du projet.
			bin, err := unix.Openat(rootFD, model.LinkDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			switch {
			case errors.Is(err, unix.ENOENT):
			case err != nil:
				if !errors.Is(err, unix.ENOTDIR) && !errors.Is(err, unix.ELOOP) {
					return nil, err
				}
			default:
				links, err := readDirFD(bin, filepath.Join(root, model.LinkDir))
				unix.Close(bin)
				if err != nil {
					return nil, err
				}
				for _, l := range links {
					if l.Type()&os.ModeSymlink == 0 && !add(strconv.Quote(model.LinkDir+"/"+l.Name())) {
						return out, nil
					}
				}
			}
		}
		fd, err := unix.Openat(rootFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		children, err := readDirFD(fd, filepath.Join(root, name))
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		for _, c := range children {
			if allowed(c) {
				continue
			}
			label := strconv.Quote(name + "/" + c.Name())
			if name == ".ssh" && c.Name() == "authorized_keys" {
				label += " (l'accès ssh d'un autre titulaire)"
			}
			if !add(label) {
				return out, nil
			}
		}
	}
	return out, nil
}

// readDirFD énumère le répertoire ouvert fd, sans le refermer.
func readDirFD(fd int, name string) ([]os.DirEntry, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(dup), name)
	defer f.Close()
	return f.ReadDir(-1)
}

// readlinkAt lit la cible d'un lien relatif au répertoire ouvert dirFD, chaque
// parent étant atteint sans suivre de lien.
func readlinkAt(dirFD int, rel string) (string, error) {
	dir, base := filepath.Split(rel)
	fd := dirFD
	if dir != "" {
		var err error
		fd, err = unix.Openat(dirFD, filepath.Clean(dir), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return "", err
		}
		defer unix.Close(fd)
	}
	buf := make([]byte, 4096)
	n, err := unix.Readlinkat(fd, base, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

// ForeignEntries nomme, avec leur compte, les entrées de premier niveau de root
// qui appartiennent à un compte ordinaire existant autre que l'UID uid (-1 s'il
// n'y en a pas). Sont ignorés root, un UID orphelin — ce que laisse un compte
// supprimé —, et les comptes hors de la plage UID_MIN–UID_MAX de login.defs :
// www-data, nobody, un compte de service. Un WordPress adopté dont les fichiers
// sont à www-data n'est pas le home de quelqu'un d'autre. nameOf nomme le compte
// d'un UID, "" s'il n'en a aucun.
//
// La racine est ouverte sans suivre de lien : ses parents appartiennent à root,
// mais l'observation a pu précéder une substitution.
func ForeignEntries(root string, uid int, nameOf func(uid int) (string, error)) ([]string, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), root)
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	low, high := uidRange()
	var found []string
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		owner, _, ok := OwnerOf(info)
		if !ok || owner == 0 || owner == uid || owner < low || owner > high {
			continue
		}
		name, err := nameOf(owner)
		if err != nil {
			return nil, err
		}
		if name == "" || name == WebUser {
			continue
		}
		if len(found) == foreignEntriesShown {
			found = append(found, "…")
			break
		}
		found = append(found, fmt.Sprintf("%q (%s)", e.Name(), name))
	}
	return found, nil
}

// uidRange lit la plage des comptes ordinaires dans login.defs, 1000–60000 à
// défaut — les valeurs de Debian.
func uidRange() (low, high int) {
	low, high = 1000, 60000
	f, err := os.Open(LoginDefsPath)
	if err != nil {
		return low, high
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		switch fields[0] {
		case "UID_MIN":
			low = v
		case "UID_MAX":
			high = v
		}
	}
	return low, high
}

// AccountOfUID nomme le compte système d'un UID, ou "" s'il n'en a aucun.
func AccountOfUID(uid int) (string, error) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		var unknown user.UnknownUserIdError
		if errors.As(err, &unknown) {
			return "", nil
		}
		return "", err
	}
	return u.Username, nil
}
