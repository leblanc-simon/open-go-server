package system

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// Journaux d'infrastructure hors du home (D71).
//
// Le home appartenant à l'utilisateur (D51), un répertoire root qu'il contient se
// renomme sans privilège : l'utilisateur déplaçait logs/ et en recréait un à lui,
// où apache-error.log devenait un lien vers un fichier système que le maître du
// serveur web ouvrait en root à son redémarrage suivant. Les journaux vivent donc
// sous logs.root, chaîne entièrement root, et le home n'en porte qu'un lien de
// commodité qu'aucun processus root n'emprunte.

// logFile est un journal pré-créé : son nom et celui qui l'écrit.
type logFile struct {
	name string
	// user vaut vrai quand le fichier est écrit sous l'identité du projet — les
	// workers PHP, les tâches. Sinon il l'est par un maître root, et appartient à
	// root:<groupe du projet>.
	user bool
	// reopened vaut vrai pour les journaux de nginx : à chaque réouverture
	// (USR1, que logrotate envoie chaque nuit), le maître les donne au compte de
	// ses workers, www-data, groupe et mode gardés. Exiger root les faisait
	// réécrire à chaque apply, et le plan n'était plus jamais vide (D116).
	reopened bool
}

// logFiles énumère les journaux qu'ogs pré-crée pour un projet. Les pré-créer
// n'est pas une commodité : le répertoire n'étant inscriptible que par root, un
// worker PHP ne pourrait pas créer php-error.log — PHP se rabattait alors sur le
// journal global de FPM, où se mêlaient les erreurs de tous les projets —, et un
// maître root créerait les siens avec son umask, en 0644 ou 0600, illisibles ou
// lisibles de travers. Les journaux des tâches sont pré-créés par le provider des
// tâches, qui en connaît les noms.
func (p *Provider) logFiles(proj *model.Project) []logFile {
	var out []logFile
	if len(proj.Spec.Domains) > 0 && p.WebEngine != "" {
		out = append(out,
			logFile{name: p.WebEngine + "-access.log", reopened: p.WebEngine == "nginx"},
			logFile{name: p.WebEngine + "-error.log", reopened: p.WebEngine == "nginx"})
	}
	if len(proj.Spec.PHP.Pools) > 0 {
		out = append(out,
			logFile{name: "php-error.log", user: true},
			logFile{name: "php-slow.log"})
	}
	return out
}

// logOwners résout les propriétaires des journaux. En mode simulé, rien n'est
// changé : l'essai tourne sans privilèges.
func (p *Provider) logOwners(proj *model.Project) (rootOwned, userOwned fs.Owner, err error) {
	if p.Exec.Simulated() {
		return fs.KeepOwner, fs.KeepOwner, nil
	}
	uid, gid, err := p.ResolveOwner(proj, "")
	if err != nil {
		return fs.KeepOwner, fs.KeepOwner, err
	}
	return fs.Owner{UID: 0, GID: gid}, fs.Owner{UID: uid, GID: gid}, nil
}

// logSteps pose la racine des journaux, le répertoire du projet et ses fichiers.
func (p *Provider) logSteps(proj *model.Project, obs Observed) []*engine.Step {
	dir := proj.Spec.Layout.LogDir
	root := filepath.Dir(dir)
	var steps []*engine.Step

	// La racine : traversable, non énumérable — les voisins ne se voient pas.
	steps = append(steps, p.logDirStep(proj, "system/logs-root", root, 0o711, true, obs))
	// Le répertoire du projet : le projet lit et liste, seul root crée ou
	// renomme. Il ne doit jamais devenir inscriptible par le groupe : logrotate
	// refuserait alors de le traiter sans « su », et la substitution que D71
	// ferme redeviendrait possible.
	steps = append(steps, p.logDirStep(proj, "system/logs/"+proj.ID(), dir, 0o750, false, obs))

	for _, f := range p.logFiles(proj) {
		f := f
		path := filepath.Join(dir, f.name)
		label := fmt.Sprintf("journal %s (0640 %s)", path, ownerLabel(proj, f.user))
		resource := "system/log/" + proj.ID() + "/" + f.name
		info, err := os.Lstat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o640 &&
			(p.Exec.Simulated() || p.fileOwnerMatches(info, proj, f, obs)) {
			steps = append(steps, engine.NewStep(resource, engine.NoOp, label))
			continue
		}
		action := engine.Create
		if err == nil {
			action = engine.Update
		}
		s := engine.NewStep(resource, action, label)
		s.Apply = func(context.Context) error {
			rootOwned, userOwned, err := p.logOwners(proj)
			if err != nil {
				return err
			}
			owner := rootOwned
			if f.user {
				owner = userOwned
			}
			var accept []int
			if f.reopened && obs.WebUID >= 0 && !p.Exec.Simulated() {
				accept = append(accept, obs.WebUID)
			}
			_, err = fs.EnsureFileAccepting(path, 0o640, owner, accept...)
			return err
		}
		if action == engine.Create {
			// Un journal créé par l'étape, et resté vide, part au déroulage.
			s.Compensate = func(context.Context) error {
				if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Size() == 0 {
					return os.Remove(path)
				}
				return nil
			}
		}
		steps = append(steps, s)
	}
	return steps
}

// nginxUser lit la directive « user » de la configuration principale de nginx.
var nginxUser = regexp.MustCompile(`(?m)^\s*user\s+([^\s;#]+)`)

// webWorkerUser nomme le compte des workers du serveur web : celui de la
// directive « user » de nginx.conf sous nginx, www-data sinon — c'est à lui que
// nginx donne ses journaux en les rouvrant. Sans directive, nginx lance ses
// workers sous « nobody » ; un fichier illisible fait retenir www-data, le
// réglage de Debian.
func (p *Provider) webWorkerUser() string {
	if p.WebEngine != "nginx" {
		return WebUser
	}
	data, err := os.ReadFile(filepath.Join(p.Paths.Prefix(), "/etc/nginx/nginx.conf"))
	if err != nil {
		return WebUser
	}
	if m := nginxUser.FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return "nobody"
}

func ownerLabel(proj *model.Project, user bool) string {
	if user {
		return proj.Spec.User.Name + ":" + proj.Spec.User.Group
	}
	return "root:" + proj.Spec.User.Group
}

// fileOwnerMatches compare le propriétaire d'un journal à celui attendu.
// Un journal de nginx peut aussi appartenir au compte de ses workers, qui le
// reçoit à chaque réouverture : c'est le seul écart admis, et pour nginx seul —
// un journal d'Apache à www-data reste un écart.
func (p *Provider) fileOwnerMatches(info os.FileInfo, proj *model.Project, f logFile, obs Observed) bool {
	uid, gid, ok := OwnerOf(info)
	if !ok || obs.Group == nil {
		return true
	}
	wantUID := 0
	if f.user {
		if obs.User == nil {
			return true
		}
		wantUID = obs.User.UID
	}
	if gid != obs.Group.GID {
		return false
	}
	return uid == wantUID || (f.reopened && obs.WebUID >= 0 && uid == obs.WebUID)
}

// logDirStep pose un répertoire de la chaîne des journaux, root de bout en bout.
// Un lien symbolique à sa place est refusé : la chaîne entière doit appartenir à
// root, et suivre un lien ferait dépendre la garantie d'une cible inconnue.
func (p *Provider) logDirStep(proj *model.Project, resource, dir string, mode os.FileMode, parcRoot bool, obs Observed) *engine.Step {
	label := fmt.Sprintf("répertoire des journaux %s (%04o)", dir, mode)
	info, err := os.Lstat(dir)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		s := engine.NewStep(resource, engine.Update, label+" — lien symbolique refusé")
		s.Apply = func(context.Context) error {
			return fmt.Errorf("%s est un lien symbolique : la chaîne des journaux doit appartenir "+
				"entièrement à root (D71)", dir)
		}
		return s
	}
	// Le propriétaire est résolu à l'application : à la création d'un projet, son
	// groupe n'existe pas encore au moment du plan — ce sont les premières étapes
	// qui le créent.
	owner := func() (fs.Owner, error) {
		if p.Exec.Simulated() {
			return fs.KeepOwner, nil
		}
		if parcRoot {
			return fs.Owner{UID: 0, GID: 0}, nil
		}
		uid, gid, err := p.ResolveOwner(proj, "root")
		if err != nil {
			return fs.KeepOwner, err
		}
		return fs.Owner{UID: uid, GID: gid}, nil
	}
	if err == nil && info.IsDir() && info.Mode().Perm() == mode {
		ok := p.Exec.Simulated()
		if !ok {
			if uid, gid, known := OwnerOf(info); known {
				wantGID := 0
				if !parcRoot {
					wantGID = -1
					if obs.Group != nil {
						wantGID = obs.Group.GID
					}
				}
				ok = uid == 0 && (wantGID < 0 || gid == wantGID)
			}
		}
		if ok {
			return engine.NewStep(resource, engine.NoOp, label)
		}
	}
	action := engine.Create
	if err == nil {
		action = engine.Update
	}
	s := engine.NewStep(resource, action, label)
	s.Apply = func(context.Context) error {
		o, err := owner()
		if err != nil {
			return err
		}
		return fs.EnsureDir(dir, mode, o)
	}
	if action == engine.Create {
		// Seul un répertoire resté vide est retiré : la racine du parc porte les
		// journaux des autres projets.
		s.Compensate = func(context.Context) error {
			if err := os.Remove(dir); err != nil && !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTEMPTY) &&
				!errors.Is(err, syscall.EEXIST) {
				return err
			}
			return nil
		}
	}
	return s
}

// logLinkStep pose ~/logs vers le répertoire des journaux. Il est posé sans suivre
// de lien — le home appartient à l'utilisateur (D70) — et rétabli à chaque apply
// s'il a été changé ; un fichier ou un répertoire à sa place n'est remplacé que
// sous --force. Tant que l'ancien répertoire est là, c'est la migration qui s'en
// charge.
func (p *Provider) logLinkStep(proj *model.Project) *engine.Step {
	root := proj.Spec.Layout.Root
	target := proj.Spec.Layout.LogDir
	resource := "system/logs-link/" + proj.ID()
	label := fmt.Sprintf("lien %s → %s", filepath.Join(root, model.LogLink), target)
	kind, current, err := fs.LinkStateAt(root, "", model.LogLink)
	switch {
	case err != nil:
		s := engine.NewStep(resource, engine.Update, label+" — chemin refusé")
		s.Critical = false
		s.Apply = func(context.Context) error { return err }
		return s
	case kind == fs.EntrySymlink && current == target:
		return engine.NewStep(resource, engine.NoOp, label)
	case kind == fs.EntryOther:
		if p.legacyLogDir(proj) {
			return nil
		}
		if !p.Force {
			s := engine.NewStep(resource, engine.Update, label+" — un fichier occupe déjà ce nom")
			s.Critical = false
			s.Apply = func(context.Context) error {
				return fmt.Errorf("%s existe et n'est pas un lien : le déplacer, ou relancer avec --force",
					filepath.Join(root, model.LogLink))
			}
			return s
		}
	}
	action := engine.Create
	if kind != fs.EntryAbsent {
		action = engine.Update
	}
	s := engine.NewStep(resource, action, label)
	// Non critique : le lien n'est qu'une commodité de lecture, et son absence
	// n'empêche aucun journal d'être écrit.
	s.Critical = false
	s.Apply = func(context.Context) error {
		return fs.SymlinkAt(root, "", model.LogLink, target, p.linkOwner(proj), p.Force)
	}
	if kind == fs.EntryAbsent {
		s.Compensate = func(context.Context) error { return fs.RemoveLinkAt(root, "", model.LogLink) }
	}
	return s
}

// legacyLogDir dit si l'ancien répertoire des journaux est encore dans le home.
func (p *Provider) legacyLogDir(proj *model.Project) bool {
	info, err := os.Lstat(model.LegacyLogDir(proj.Spec.Layout.Root))
	return err == nil && info.IsDir()
}

// reRotated reconnaît un journal déjà roté par logrotate : « x.log.1 »,
// « x.log.2.gz ».
var reRotated = regexp.MustCompile(`\.log\.\d+(\.gz)?$`)

// logMigration porte ce que les deux étapes de la migration se transmettent :
// la taille recopiée de chaque journal courant, dont la seconde ajoute la suite.
type logMigration struct {
	copied map[string]int64
}

// PlanLogMigration reprend l'ancien ~/logs dans le nouveau répertoire, sans perte
// de lignes (D71). Elle rend deux étapes : la première recopie les journaux
// courants avant que les serveurs ne soient rechargés sur le nouvel emplacement ;
// la seconde, en fin de plan et après le vidage des rechargements, ajoute ce qui
// a été écrit entre-temps dans l'ancien fichier, déplace les journaux rotés sous
// leur nom — logrotate poursuit la numérotation —, retire l'ancien répertoire et
// pose le lien à sa place.
//
// L'ancien répertoire vit dans le home : l'utilisateur a pu le substituer. Il
// n'est lu que par descripteur ouvert sans suivre de lien, **s'il appartient à
// root**, et seuls ses fichiers réguliers appartenant à root ou au projet sont
// repris. Sans cela, root recopierait un « apache-error.log → /etc/shadow » posé
// par l'utilisateur dans un répertoire que ce même utilisateur lit.
func (p *Provider) PlanLogMigration(proj *model.Project, obs Observed) (prepare, finish *engine.Step) {
	if !p.legacyLogDir(proj) {
		return nil, nil
	}
	root := proj.Spec.Layout.Root
	legacy := model.LegacyLogDir(root)
	dest := proj.Spec.Layout.LogDir
	state := &logMigration{copied: map[string]int64{}}

	prepare = engine.NewStep("system/logs-migration/"+proj.ID(), engine.Update,
		fmt.Sprintf("reprise des journaux de %s dans %s", legacy, dest))
	prepare.Critical = false
	// Les serveurs qui écrivent les journaux doivent les rouvrir au nouvel
	// emplacement avant la seconde étape, même quand aucun vhost ni aucun pool n'a
	// changé : sans rechargement, ils écrivent par leur descripteur dans l'ancien
	// fichier, que la migration va retirer — et ce qu'ils y écrivent est perdu.
	if p.WebReload != "" && len(proj.Spec.Domains) > 0 {
		prepare.Reload = append(prepare.Reload, p.WebReload)
	}
	seen := map[string]bool{}
	for _, pool := range proj.Spec.PHP.Pools {
		if !seen[pool.Version] {
			seen[pool.Version] = true
			prepare.Reload = append(prepare.Reload, "php:"+pool.Version)
		}
	}
	prepare.Apply = func(context.Context) error {
		entries, err := p.trustedLegacyEntries(proj, obs)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name, ".log") {
				continue
			}
			n, err := p.copyLegacy(proj, obs, e, filepath.Join(dest, e.Name), 0)
			if err != nil {
				return err
			}
			state.copied[e.Name] = n
		}
		return nil
	}

	finish = engine.NewStep("system/logs-migration-fin/"+proj.ID(), engine.Delete,
		fmt.Sprintf("retrait de l'ancien %s (journaux désormais dans %s, lien %s)", legacy, dest, legacy))
	finish.Critical = false
	// Les serveurs doivent avoir été rechargés sur le nouvel emplacement avant
	// qu'on ne recueille la suite de l'ancien fichier et qu'on ne le retire.
	finish.FlushReloads = true
	finish.Apply = func(context.Context) error {
		entries, err := p.trustedLegacyEntries(proj, obs)
		if err != nil {
			return err
		}
		var kept []string
		for _, e := range entries {
			target := filepath.Join(dest, e.Name)
			offset, current := state.copied[e.Name]
			switch {
			case current:
				// La suite écrite depuis la recopie, ajoutée au nouveau fichier.
				if _, err := p.copyLegacy(proj, obs, e, target, offset); err != nil {
					return err
				}
			case reRotated.MatchString(e.Name) || !strings.HasSuffix(e.Name, ".log"):
				if _, err := os.Lstat(target); err == nil {
					kept = append(kept, e.Name)
					continue
				}
				if _, err := p.copyLegacy(proj, obs, e, target, 0); err != nil {
					return err
				}
			default:
				// Un journal apparu après la recopie : repris en entier.
				if _, err := p.copyLegacy(proj, obs, e, target, 0); err != nil {
					return err
				}
			}
			if err := fs.RemoveFileAt(root, model.LogLink, e.Name); err != nil {
				return err
			}
		}
		remaining, err := fs.RemoveEmptyDirAt(root, model.LogLink)
		if err != nil {
			return err
		}
		if len(remaining) > 0 {
			return fmt.Errorf("%s conservé : il contient encore %s, qui n'a pas pu être repris",
				legacy, strings.Join(remaining, ", "))
		}
		if len(kept) > 0 {
			return fmt.Errorf("déjà présents dans %s, non repris : %s", dest, strings.Join(kept, ", "))
		}
		return fs.SymlinkAt(root, "", model.LogLink, dest, p.linkOwner(proj), false)
	}
	return prepare, finish
}

// trustedLegacyEntries énumère l'ancien répertoire, et refuse de le lire s'il
// n'appartient pas à root : il a alors été substitué, et son contenu est choisi
// par l'utilisateur. Les entrées qui ne sont pas des fichiers réguliers
// appartenant à root ou au projet sont écartées — et restent, ce qui interdit de
// retirer le répertoire.
func (p *Provider) trustedLegacyEntries(proj *model.Project, obs Observed) ([]fs.DirEntryAt, error) {
	dirUID, entries, err := fs.ListDirAt(proj.Spec.Layout.Root, model.LogLink)
	if err != nil {
		return nil, err
	}
	if !p.Exec.Simulated() && dirUID != 0 {
		return nil, fmt.Errorf("%s n'appartient pas à root (uid %d) : il a été substitué, rien n'en est "+
			"repris — le vérifier à la main (D71)", model.LegacyLogDir(proj.Spec.Layout.Root), dirUID)
	}
	var out []fs.DirEntryAt
	for _, e := range entries {
		if !e.Regular {
			continue
		}
		if !p.Exec.Simulated() && e.UID != 0 && (obs.User == nil || e.UID != uint32(obs.User.UID)) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// copyLegacy recopie un ancien journal, à partir d'offset, à la fin de target, et
// rend la taille totale lue. Le fichier cible garde son propriétaire s'il existe ;
// sinon il reçoit celui de l'original, dans le groupe du projet, en 0640.
func (p *Provider) copyLegacy(proj *model.Project, obs Observed, e fs.DirEntryAt, target string, offset int64) (int64, error) {
	src, uid, err := fs.OpenFileAt(proj.Spec.Layout.Root, model.LogLink, e.Name)
	if err != nil {
		return 0, err
	}
	defer src.Close()
	owner := fs.KeepOwner
	if !p.Exec.Simulated() {
		_, gid, err := p.ResolveOwner(proj, "root")
		if err != nil {
			return 0, err
		}
		owner = fs.Owner{UID: int(uid), GID: gid}
	}
	if _, err := os.Lstat(target); os.IsNotExist(err) {
		if _, err := fs.EnsureFile(target, 0o640, owner); err != nil {
			return 0, err
		}
	}
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return 0, err
	}
	defer dst.Close()
	if _, err := src.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.Copy(dst, src)
	if err != nil {
		return 0, err
	}
	if err := dst.Sync(); err != nil {
		return 0, err
	}
	return offset + n, nil
}

// LogFinding est un constat de « doctor » sur les journaux.
type LogFinding struct {
	Name, Status, Detail, Fix string
}

// LogChecks porte ce qu'aucune vérification de configuration ne voit (D71) : un
// composant du chemin des journaux qui n'appartient pas à root — c'est la
// substitution que la décision ferme, rouverte par un réglage —, un ancien
// ~/logs resté en place, un lien qui ne désigne plus le bon répertoire, et
// l'espace libre de la partition, qui porte aussi les bases.
func (p *Provider) LogChecks(projects []*model.Project, logsRoot string) []LogFinding {
	var out []LogFinding
	add := func(name, status, detail, fix string) {
		out = append(out, LogFinding{Name: name, Status: status, Detail: detail, Fix: fix})
	}
	simulated := p.Exec.Simulated()

	var unsafe, legacy, links []string
	for _, proj := range projects {
		dir := proj.Spec.Layout.LogDir
		if !simulated {
			if bad := unsafeComponent(dir); bad != "" {
				unsafe = append(unsafe, proj.ID()+" : "+bad)
			}
		}
		if p.legacyLogDir(proj) {
			legacy = append(legacy, model.LegacyLogDir(proj.Spec.Layout.Root))
		}
		kind, target, err := fs.LinkStateAt(proj.Spec.Layout.Root, "", model.LogLink)
		switch {
		case err != nil:
			links = append(links, proj.ID()+" : "+err.Error())
		case kind == fs.EntrySymlink && target != dir:
			links = append(links, fmt.Sprintf("%s : ~/logs désigne %s au lieu de %s", proj.ID(), target, dir))
		case kind == fs.EntryAbsent && len(projects) > 0:
			links = append(links, proj.ID()+" : ~/logs absent")
		}
	}
	switch {
	case len(unsafe) > 0:
		add("chaîne des journaux", "échec", strings.Join(unsafe, " ; ")+
			" : un composant qui n'appartient pas à root, ou qu'un autre peut modifier, rouvre la "+
			"substitution d'un journal que le serveur web ouvre en root", "ogs apply — ou corriger logs.root")
	case len(projects) > 0:
		add("chaîne des journaux", "ok", "entièrement à root sous "+logsRoot, "")
	}
	if len(legacy) > 0 {
		add("anciens répertoires de journaux", "avertissement", strings.Join(legacy, ", ")+
			" encore en place : la migration n'a pas pu les reprendre", "ogs apply, puis vérifier à la main")
	}
	if len(links) > 0 {
		add("liens ~/logs", "avertissement", strings.Join(links, " ; "), "ogs apply")
	}
	if avail, total, err := freeSpace(logsRoot); err == nil && total > 0 {
		pct := float64(avail) / float64(total) * 100
		detail := fmt.Sprintf("%s libre(s) sur %s (%.0f %%) pour %s", humanSize(avail), humanSize(total), pct, logsRoot)
		if pct < 10 || avail < 1<<30 {
			add("espace des journaux", "avertissement", detail+" : la partition porte aussi, le plus "+
				"souvent, les bases de données", "réduire logs.rotate ou logs.maxSize, ou poser logs.root sur une partition dédiée")
		} else {
			add("espace des journaux", "ok", detail, "")
		}
	}
	return out
}

// unsafeComponent rend le premier composant du chemin qui n'est pas un
// répertoire root inscriptible par root seul — au mieux par root et le groupe du
// projet en lecture —, ou une chaîne vide.
func unsafeComponent(dir string) string {
	for path := filepath.Clean(dir); ; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) && path == filepath.Clean(dir) {
				// Le répertoire du projet peut ne pas encore exister : c'est l'apply
				// qui le crée.
				if path == "/" {
					return ""
				}
				continue
			}
			return path + " : " + err.Error()
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return path + " est un lien symbolique"
		}
		if uid, _, ok := OwnerOf(info); ok && uid != 0 {
			return fmt.Sprintf("%s appartient à l'uid %d", path, uid)
		}
		if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return fmt.Sprintf("%s est inscriptible au-delà de root (%04o)", path, info.Mode().Perm())
		}
		if path == "/" {
			return ""
		}
	}
}

func humanSize(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d o", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cio", float64(n)/float64(div), "KMGTPE"[exp])
}
