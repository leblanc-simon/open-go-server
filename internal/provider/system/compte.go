package system

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
)

// Ce fichier porte ce que le compte d'un projet laisse au système hors de son
// home (D107) : le groupe qu'il partage peut-être, les tâches posées hors d'ogs
// — crontab personnelle, tâches at, linger —, et les fichiers que son UID tient
// dans les répertoires temporaires communs. useradd redonne l'UID libéré au
// compte suivant, et le nom d'un projet supprimé à celui qui le reprend : ce qui
// survit au compte passe à son successeur.

// AccountsOfGIDFunc nomme les comptes dont gid est le groupe primaire.
type AccountsOfGIDFunc func(gid int) ([]string, error)

// AccountsOfGID lit /etc/passwd : os/user ne sait pas énumérer les comptes.
func AccountsOfGID(gid int) ([]string, error) { return accountsOfGID(defaultPasswdPath, gid) }

// AccountsOfGIDIn lit une autre base des comptes : celle du préfixe.
func AccountsOfGIDIn(path string) AccountsOfGIDFunc {
	return func(gid int) ([]string, error) { return accountsOfGID(path, gid) }
}

func accountsOfGID(path string, gid int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 4 {
			continue
		}
		if g, err := strconv.Atoi(fields[3]); err == nil && g == gid {
			out = append(out, fields[0])
		}
	}
	return out, sc.Err()
}

// GroupStrangers nomme ceux qui tiennent le groupe sans être le compte du projet
// ni le serveur web : ses membres secondaires, et les comptes dont il est le
// groupe primaire. C'est le critère du groupe (D107), celui de D95 pour le home :
// qui le tient, jamais ce qu'il possède.
func (p *Provider) GroupStrangers(g *GroupInfo, account string) ([]string, error) {
	if g == nil {
		return nil, nil
	}
	seen := map[string]bool{account: true, WebUser: true}
	var out []string
	for _, m := range g.Members {
		if m = strings.TrimSpace(m); m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	primary := p.AccountsOfGID
	if primary == nil {
		primary = AccountsOfGID
	}
	names, err := primary(g.GID)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n+" (groupe primaire)")
		}
	}
	return out, nil
}

// PreexistingGroup refuse de créer le compte d'un projet sur un groupe que
// d'autres tiennent (D107, R2-43). Le groupe existant était repris sans un
// regard : ses membres recevaient le socket FPM du projet — y parler, c'est y
// faire exécuter du code sous son identité — et ses fichiers, puis « project
// delete » supprimait le groupe sous eux.
//
// Comme pour le home (D95), le critère est qui le tient, jamais ce qu'il
// possède : un groupe sans autre membre que www-data, ce que laisse un compte
// supprimé à la main ou une saga déroulée, est repris. Ne s'y appliquent ni un
// projet adopté — il reprend légitimement ce qui existe —, ni un compte déjà là,
// ni un projet que la base d'état connaît avec ce home (KnownHome). En mode
// préfixé, le compte n'existe jamais : le contrôle se tait.
func (p *Provider) PreexistingGroup(proj *model.Project, obs Observed) error {
	if obs.Group == nil || obs.User != nil || proj.Metadata.Labels["adopted"] == "true" || p.Exec.Simulated() {
		return nil
	}
	if p.KnownHome != nil && p.KnownHome(proj) {
		return nil
	}
	strangers, err := p.GroupStrangers(obs.Group, proj.Spec.User.Name)
	if err != nil {
		return fmt.Errorf("le groupe %s existe déjà et ses membres sont illisibles (%v) : ogs ne crée pas "+
			"l'utilisateur %s sur un groupe qu'il ne voit pas", proj.Spec.User.Group, err, proj.Spec.User.Name)
	}
	if len(strangers) == 0 {
		return nil
	}
	return fmt.Errorf("le groupe %s existe déjà et d'autres comptes le tiennent — %s : ils recevraient les "+
		"fichiers et le socket PHP-FPM du projet %s, donc l'exécution sous son identité ; retirer ces comptes "+
		"du groupe (gpasswd -d) ou supprimer le groupe, choisir un autre identifiant, ou reprendre le site "+
		"existant avec « ogs import »", proj.Spec.User.Group, strings.Join(strangers, ", "), proj.ID())
}

// UserJobs est ce que le compte d'un projet a posé hors d'ogs et qui s'exécute
// sous son nom (D107, R4-18) : la crontab personnelle, les tâches at, le linger.
// cron et atd relisent par le nom : un compte recréé sous ce nom les reprenait.
type UserJobs struct {
	Account string
	// Crontab est le contenu de la crontab personnelle, vide s'il n'y en a pas.
	Crontab []byte
	// AtJobs associe le numéro d'une tâche at à son script (at -c).
	AtJobs map[string][]byte
	// Linger dit que systemd garde le gestionnaire de l'utilisateur hors session.
	Linger bool
	// Errors nomme ce qui n'a pas pu être relu : ce n'est alors ni archivé ni
	// retiré.
	CrontabErr, AtErr error
}

// Empty dit qu'il n'y a rien à archiver ni à retirer.
func (j UserJobs) Empty() bool {
	return len(j.Crontab) == 0 && len(j.AtJobs) == 0 && !j.Linger && j.CrontabErr == nil && j.AtErr == nil
}

// AtIDs rend les numéros des tâches at, triés.
func (j UserJobs) AtIDs() []string {
	ids := make([]string, 0, len(j.AtJobs))
	for id := range j.AtJobs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool {
		x, _ := strconv.Atoi(ids[a])
		y, _ := strconv.Atoi(ids[b])
		return x < y
	})
	return ids
}

// lingerDir est le répertoire où logind note les comptes à linger.
const lingerDir = "/var/lib/systemd/linger"

// ReadUserJobs relit les tâches du compte. Un outil absent — at, cron non
// installés — n'a rien posé : il se tait.
func (p *Provider) ReadUserJobs(ctx context.Context, account string) UserJobs {
	jobs := UserJobs{Account: account, AtJobs: map[string][]byte{}}
	jobs.Crontab, jobs.CrontabErr = p.readCrontab(ctx, account)
	ids, err := p.atIDs(ctx, account)
	jobs.AtErr = err
	for _, id := range ids {
		script, err := p.Exec.Run(ctx, exec.Command{Name: "at", Args: []string{"-c", id}})
		if err != nil {
			jobs.AtErr = errors.Join(jobs.AtErr, fmt.Errorf("tâche at %s : %w", id, err))
			continue
		}
		jobs.AtJobs[id] = []byte(script.Stdout)
	}
	if _, err := os.Lstat(filepath.Join(p.Paths.Rooted(lingerDir), account)); err == nil {
		jobs.Linger = true
	}
	return jobs
}

// readCrontab lit la crontab personnelle du compte ; vide s'il n'en a pas, ou
// si cron n'est pas installé.
func (p *Provider) readCrontab(ctx context.Context, account string) ([]byte, error) {
	res, err := p.Exec.Run(ctx, exec.Command{Name: "crontab", Args: []string{"-l", "-u", account}})
	switch {
	case err == nil:
		return []byte(res.Stdout), nil
	case exec.Missing(err), strings.Contains(res.Stderr+err.Error(), "no crontab for"):
		return nil, nil
	default:
		return nil, err
	}
}

// atIDs nomme les tâches at du compte ; aucune si at n'est pas installé.
func (p *Provider) atIDs(ctx context.Context, account string) ([]string, error) {
	res, err := p.Exec.Run(ctx, exec.Command{Name: "atq"})
	if err != nil {
		if exec.Missing(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[len(fields)-1] != account {
			continue
		}
		if _, err := strconv.Atoi(fields[0]); err == nil {
			ids = append(ids, fields[0])
		}
	}
	return ids, nil
}

// RemoveUserJobs retire ce que ReadUserJobs a relu et que l'archive a gardé, et
// seulement cela. Le compte existe encore entre la relecture et le retrait —
// l'archive et l'arrêt des services passent entre les deux —, et son titulaire
// peut y changer sa crontab ou poser une tâche at : la crontab est relue juste
// avant le retrait, et ne part que si elle est celle de l'archive ; une tâche at
// apparue depuis n'est pas retirée. Ce qui n'a pas pu être relu, ou a changé,
// n'est pas dans l'archive : il reste, nommé. uid sert à arrêter le
// gestionnaire de l'utilisateur (user@<uid>.service), que le linger ou une
// session gardaient : ses processus tiennent le compte, et userdel refuserait
// de le supprimer. Rend ce qui a été retiré, et ce qui a échoué ou reste.
func (p *Provider) RemoveUserJobs(ctx context.Context, jobs UserJobs, uid int) ([]string, error) {
	var removed []string
	var errs []error
	if jobs.CrontabErr != nil {
		errs = append(errs, fmt.Errorf("crontab de %s non relue, donc ni archivée ni retirée : %w",
			jobs.Account, jobs.CrontabErr))
	} else if len(jobs.Crontab) > 0 {
		now, err := p.readCrontab(ctx, jobs.Account)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("crontab de %s non relue avant son retrait, laissée en place : %w",
				jobs.Account, err))
		case len(now) == 0:
			// Retirée entre-temps : il n'y a plus rien à faire.
		case string(now) != string(jobs.Crontab):
			errs = append(errs, fmt.Errorf("crontab de %s modifiée depuis son archivage, laissée en place : "+
				"l'archive n'en porte que l'ancienne version ; la sauvegarder (crontab -l -u %s), puis "+
				"crontab -r -u %s", jobs.Account, jobs.Account, jobs.Account))
		default:
			if _, err := p.Exec.Run(ctx, exec.Command{Name: "crontab", Args: []string{"-r", "-u", jobs.Account}}); err != nil {
				errs = append(errs, fmt.Errorf("crontab de %s : %w", jobs.Account, err))
			} else {
				removed = append(removed, "crontab personnelle de "+jobs.Account)
			}
		}
	}
	if jobs.AtErr != nil {
		errs = append(errs, fmt.Errorf("tâches at de %s non relues, donc ni archivées ni retirées : %w",
			jobs.Account, jobs.AtErr))
	} else if now, err := p.atIDs(ctx, jobs.Account); err != nil {
		errs = append(errs, fmt.Errorf("tâches at de %s non relues avant leur retrait : une tâche posée depuis "+
			"l'archivage resterait sans être nommée : %w", jobs.Account, err))
	} else {
		// Une tâche posée depuis la relecture n'est pas dans l'archive : nommée.
		var added []string
		for _, id := range now {
			if _, known := jobs.AtJobs[id]; !known {
				added = append(added, id)
			}
		}
		if len(added) > 0 {
			errs = append(errs, fmt.Errorf("tâche(s) at de %s posée(s) depuis l'archivage, laissée(s) en place : "+
				"%s ; les relire (at -c <n>) puis atrm %s", jobs.Account, strings.Join(added, ", "),
				strings.Join(added, " ")))
		}
	}
	if ids := jobs.AtIDs(); len(ids) > 0 {
		_, err := p.Exec.Run(ctx, exec.Command{Name: "atrm", Args: ids})
		if err != nil {
			// Une tâche exécutée entre l'archivage et le retrait n'est plus dans la
			// file : atrm répond « Cannot find jobid ». Seule compte une tâche
			// archivée encore présente.
			if now, rerr := p.atIDs(ctx, jobs.Account); rerr == nil {
				var left []string
				for _, id := range now {
					if _, archived := jobs.AtJobs[id]; archived {
						left = append(left, id)
					}
				}
				if len(left) == 0 {
					err = nil
				} else {
					err = fmt.Errorf("%w (encore en file : %s)", err, strings.Join(left, ", "))
				}
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("tâches at de %s (%s) : %w", jobs.Account, strings.Join(ids, ", "), err))
		} else {
			removed = append(removed, fmt.Sprintf("tâche(s) at de %s : %s", jobs.Account, strings.Join(ids, ", ")))
		}
	}
	lingerKept := false
	if jobs.Linger {
		label := "linger de " + jobs.Account
		_, err := p.Exec.Run(ctx, exec.Command{Name: "loginctl", Args: []string{"disable-linger", jobs.Account}})
		if err != nil && !p.Exec.Simulated() {
			// logind injoignable — sans D-Bus, ou arrêté : le linger n'est que ce
			// fichier, sous une chaîne de root, que logind relit à son démarrage.
			if rmErr := os.Remove(filepath.Join(p.Paths.Rooted(lingerDir), jobs.Account)); rmErr == nil ||
				os.IsNotExist(rmErr) {
				err = nil
				label += " (logind injoignable : son fichier retiré)"
			}
		}
		if err != nil {
			lingerKept = true
			errs = append(errs, fmt.Errorf("linger de %s : %w", jobs.Account, err))
		} else {
			removed = append(removed, label)
		}
	}
	// Le gestionnaire de l'utilisateur, qu'un linger ou une session tiennent :
	// logind ne l'arrête qu'à sa guise, et ses processus feraient refuser
	// userdel. Arrêté s'il est actif, linger ou non.
	if uid > 0 && !lingerKept {
		unit := fmt.Sprintf("user@%d.service", uid)
		if _, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl",
			Args: []string{"is-active", "--quiet", unit}}); err == nil {
			if _, err := p.Exec.Run(ctx, exec.Command{Name: "systemctl", Args: []string{"stop", unit}}); err != nil {
				errs = append(errs, fmt.Errorf("gestionnaire %s de %s non arrêté, ses processus tiendront le "+
					"compte : %w", unit, jobs.Account, err))
			}
		}
	}
	return removed, errors.Join(errs...)
}

// TempDirs sont les répertoires communs où tout compte écrit, et où ses fichiers
// survivent au compte (D107, R4-19).
var TempDirs = []string{"/tmp", "/var/tmp", "/dev/shm"}

// MountInfoPath est la table des montages du processus : seule elle montre un
// montage lié issu du même système de fichiers, qui garde le st_dev de sa source.
// Une variable, pour que les tests en posent une.
var MountInfoPath = "/proc/self/mountinfo"

// Bornes de la purge : elle tourne après le point de non-retour, sous le verrou
// global, dans des répertoires où n'importe quel compte crée ce qu'il veut. Un
// parcours arrêté sur une borne est nommé, code 6, et rien n'est perdu : ce qui
// reste est seulement ce qui n'a pas été purgé.
var (
	// purgeMaxDepth borne la descente : 1024 niveaux, comme la copie du clone.
	purgeMaxDepth = 1024
	// purgeMaxEntries borne le nombre d'entrées examinées, tous répertoires confondus.
	purgeMaxEntries = 200000
	// purgeTimeout borne la durée du parcours.
	purgeTimeout = 2 * time.Minute
	// purgeBatch est le nombre d'entrées lues d'un coup dans un répertoire.
	purgeBatch = 256
	// purgeKept borne les chemins retirés que la purge garde en mémoire ; les
	// suivants ne sont que comptés.
	purgeKept = 1000
)

// purgeShown borne les chemins nommés.
const purgeShown = 10

// PurgeUID retire des répertoires temporaires communs ce qui appartient à uid
// (D107, R4-19) : useradd redonne l'UID libéré au compte suivant, qui recevait
// les fichiers de l'ancien — sessions, caches, téléversements en cours. Tout
// est atteint par descripteur, sans suivre de lien — un lien de l'UID est retiré
// comme lien, sa cible jamais touchée — et sans franchir de point de montage,
// reconnu par la table des montages (un montage lié du même système de fichiers
// garde le st_dev de sa source) comme par le changement de périphérique :
// os.RemoveAll les franchit. Une table illisible fait renoncer à toute la purge.
// Un répertoire de l'UID qui garde des entrées d'un autre compte, ou qu'un
// montage occupe, reste, et est nommé dans l'erreur ; un montage étranger à
// l'UID, hors de ses répertoires, ne regarde pas la suppression et est tu.
//
// Rend les chemins retirés — un répertoire retiré en entier est nommé seul — et
// ce qui a échoué. Les répertoires sont relatifs à la racine des chemins du
// provider : sous préfixe, ce sont ceux du préfixe.
func (p *Provider) PurgeUID(ctx context.Context, uid int) ([]string, error) {
	if uid <= 0 {
		return nil, nil
	}
	dirs := p.TempDirs
	if dirs == nil {
		for _, d := range TempDirs {
			dirs = append(dirs, p.Paths.Rooted(d))
		}
	}
	mounts, err := mountPoints(MountInfoPath)
	if err != nil {
		return nil, fmt.Errorf("table des montages illisible (%v) : rien n'a été purgé, un montage lié aurait "+
			"pu être franchi", err)
	}
	pg := &purge{ctx: ctx, uid: uint32(uid), mounts: mounts, deadline: time.Now().Add(purgeTimeout)}
	for _, d := range dirs {
		if pg.stopped != nil {
			break
		}
		fd, err := unix.Open(d, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			pg.fail(d, err)
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			pg.fail(d, err)
		} else {
			pg.dir(fd, d, st.Dev, 0, false)
		}
		unix.Close(fd)
	}
	if pg.stopped != nil {
		pg.errs = append(pg.errs, pg.stopped)
	}
	return pg.named(), errors.Join(pg.errs...)
}

// mountPoints lit les points de montage de la table, chemins décodés.
func mountPoints(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		// « 36 35 98:0 /racine /point options… - type source options » : le
		// point de montage est le cinquième champ.
		if fields := strings.Fields(line); len(fields) >= 5 {
			out[filepath.Clean(unescapeOctal(fields[4]))] = true
		}
	}
	return out, nil
}

// unescapeOctal décode les séquences octales dont le noyau protège blancs et
// barre oblique inverse dans la table des montages (« \040 » pour une espace).
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

type purge struct {
	ctx      context.Context
	uid      uint32
	mounts   map[string]bool
	deadline time.Time
	seen     int
	// stopped dit pourquoi le parcours s'est arrêté avant la fin.
	stopped error
	removed []string
	// dropped compte les retraits au-delà de purgeKept.
	dropped int
	errs    []error
}

func (pg *purge) fail(path string, err error) {
	pg.errs = append(pg.errs, fmt.Errorf("%s : %w", path, err))
}

func (pg *purge) took(path string) {
	if len(pg.removed) < purgeKept {
		pg.removed = append(pg.removed, path)
	} else {
		pg.dropped++
	}
}

// halt dit s'il faut arrêter le parcours : contexte annulé, délai ou nombre
// d'entrées dépassés.
func (pg *purge) halt() bool {
	if pg.stopped != nil {
		return true
	}
	switch {
	case pg.ctx.Err() != nil:
		pg.stopped = fmt.Errorf("purge interrompue (%v) : ce qui n'a pas été parcouru reste", pg.ctx.Err())
	case pg.seen >= purgeMaxEntries:
		pg.stopped = fmt.Errorf("purge arrêtée après %d entrées examinées : ce qui n'a pas été parcouru reste",
			pg.seen)
	case time.Now().After(pg.deadline):
		pg.stopped = fmt.Errorf("purge arrêtée après %s : ce qui n'a pas été parcouru reste", purgeTimeout)
	}
	return pg.stopped != nil
}

// named rend les chemins retirés, ceux d'un répertoire retiré en entier
// absorbés par lui, bornés.
func (pg *purge) named() []string {
	sort.Strings(pg.removed)
	var out []string
	for _, r := range pg.removed {
		if n := len(out); n > 0 && strings.HasPrefix(r, out[n-1]+"/") {
			continue
		}
		out = append(out, r)
	}
	more := 0
	if len(out) > purgeShown {
		more = len(out) - purgeShown
		out = out[:purgeShown]
	}
	if more+pg.dropped > 0 {
		out = append(out, fmt.Sprintf("… et %d autre(s)", more+pg.dropped))
	}
	return out
}

// dir parcourt le répertoire ouvert fd, mine s'il est à l'UID. Rend vrai s'il
// est désormais vide.
func (pg *purge) dir(fd int, path string, dev uint64, depth int, mine bool) bool {
	if depth > purgeMaxDepth {
		pg.fail(path, fmt.Errorf("plus de %d niveaux, parcours arrêté", purgeMaxDepth))
		return false
	}
	dup, err := unix.Dup(fd)
	if err != nil {
		pg.fail(path, err)
		return false
	}
	f := os.NewFile(uintptr(dup), path)
	defer f.Close()
	empty := true
	for {
		entries, err := f.ReadDir(purgeBatch)
		for _, e := range entries {
			pg.seen++
			if pg.halt() {
				return false
			}
			if !pg.entry(fd, path, e.Name(), dev, depth, mine) {
				empty = false
			}
		}
		if err == io.EOF || (err == nil && len(entries) == 0) {
			return empty
		}
		if err != nil {
			pg.fail(path, err)
			return false
		}
	}
}

// entry traite une entrée du répertoire ouvert fd. Rend vrai si elle n'y est plus.
func (pg *purge) entry(fd int, path, name string, dev uint64, depth int, mine bool) bool {
	child := filepath.Join(path, name)
	var st unix.Stat_t
	if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return true
		}
		pg.fail(child, err)
		return false
	}
	own := st.Uid == pg.uid
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		// Un fichier peut aussi être un point de montage (montage lié d'un
		// fichier) : le délier retirerait l'entrée de la table, pas la source,
		// mais la suppression ne démonte rien.
		if pg.mounts[child] {
			if own || mine {
				pg.fail(child, errors.New("point de montage, non franchi"))
			}
			return false
		}
		if !own {
			return false
		}
		if err := unix.Unlinkat(fd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			pg.fail(child, err)
			return false
		}
		pg.took(child)
		return true
	}
	if st.Dev != dev || pg.mounts[child] {
		// Un point de montage : ce qu'il porte appartient à son système de
		// fichiers, et peut-être à d'autres machines. Il n'est nommé que s'il
		// retient quelque chose de l'UID ; ceux du reste de la machine ne
		// regardent pas la suppression.
		if own || mine {
			pg.fail(child, errors.New("point de montage, non franchi"))
		}
		return false
	}
	sub, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return true
		}
		pg.fail(child, err)
		return false
	}
	// L'entrée a pu être remplacée entre le stat et l'ouverture : c'est le
	// répertoire ouvert qui fait foi.
	var opened unix.Stat_t
	if err := unix.Fstat(sub, &opened); err != nil || opened.Ino != st.Ino || opened.Dev != st.Dev {
		unix.Close(sub)
		pg.fail(child, errors.New("remplacé pendant le parcours"))
		return false
	}
	// La table des montages a été lue une fois, au début : un parent renommé
	// depuis, ou un montage posé depuis, ne s'y retrouve pas, et un montage lié
	// du même système de fichiers garde son st_dev. Le descripteur ouvert, lui,
	// dit sans course à quel montage il appartient.
	if crossesMount(fd, sub) {
		unix.Close(sub)
		if own || mine {
			pg.fail(child, errors.New("point de montage, non franchi"))
		}
		return false
	}
	subEmpty := pg.dir(sub, child, dev, depth+1, own)
	unix.Close(sub)
	if !own || pg.stopped != nil {
		return false
	}
	if !subEmpty {
		pg.fail(child, errors.New("répertoire de l'UID conservé : il porte des entrées d'autres comptes, ou un point de montage"))
		return false
	}
	if err := unix.Unlinkat(fd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		pg.fail(child, err)
		return false
	}
	pg.took(child)
	return true
}

// mountOfFD dit à quel montage appartient le descripteur ouvert fd, et s'il en
// est la racine (statx, STATX_MNT_ID et STATX_ATTR_MOUNT_ROOT, noyau ≥ 5.8). ok
// est faux quand le noyau ne le dit pas : restent alors la table des montages et
// st_dev. Une variable, pour que les tests simulent un montage sans privilège.
var mountOfFD = func(fd int) (id uint64, root, ok bool) {
	var stx unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &stx); err != nil {
		return 0, false, false
	}
	root = stx.Attributes_mask&unix.STATX_ATTR_MOUNT_ROOT != 0 && stx.Attributes&unix.STATX_ATTR_MOUNT_ROOT != 0
	if stx.Mask&unix.STATX_MNT_ID == 0 {
		return 0, root, root
	}
	return stx.Mnt_id, root, true
}

// crossesMount dit si le répertoire ouvert sub, enfant du répertoire ouvert
// parent, est la racine d'un autre montage que celui de son parent.
func crossesMount(parent, sub int) bool {
	id, root, ok := mountOfFD(sub)
	if !ok {
		return false
	}
	if root {
		return true
	}
	pid, _, pok := mountOfFD(parent)
	return pok && id != 0 && pid != 0 && id != pid
}
