package cli

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/db"
)

// Un clone ne laisse jamais la préproduction désigner une ressource de la
// production sans le dire. La copie de www/ emportait la configuration de
// l'application — .env.local de Symfony, parameters.php de Prestashop — sans
// rien en réécrire : la préproduction se connectait à la base, au transport et
// aux services de la production avec ses identifiants, y écrivait, et son compte
// recevait en clair ses secrets. ogs donne ses identifiants à une application de
// trois façons : « ogs secrets env », dont l'exploitant ou un hook postCreate
// recopie les chaînes dans la configuration de l'application, sous www/ ; le
// fichier d'environnement /etc/ogs/env/<id>.env (D75), propre à chaque projet,
// que le clone ne recopie pas ; les variables déclarées dans le projet
// (spec.app.env, spec.php.pools[].env, paramètres env.* d'un service), que le
// clone reprend.
//
// D'où la garde du clone. Les fichiers que le type d'application consacre à la
// configuration d'un environnement ne sont pas recopiés — celui qu'un hook
// postCreate a écrit pour la préproduction reste donc en place. Les secrets
// qu'ogs connaît de la source — les mots de passe qu'il a générés pour ses bases
// et son transport, les valeurs de son fichier d'environnement — sont masqués
// partout ailleurs, contenu copié comme déclaration. Réécrire les fichiers de
// configuration avec les identifiants de la préproduction ne suffirait pas : ils
// portent aussi la clé de l'application, les clés d'API, l'adresse du relais de
// courrier, qu'ogs ne connaît pas.
//
// La garde ne voit pas tout, et la sortie ne doit pas prétendre le contraire :
// un secret rangé dans un autre fichier — le .env versionné de Symfony, un
// fichier de config/ —, le reste d'une chaîne dont le mot de passe est masqué,
// le contenu de la base passent tels quels. Le clone dit, pour tous les types,
// ce qu'il a retiré et ce qu'il a repris, et nomme un à un ce qu'il sait
// désigner : fichier retenu ou expurgé, variable déclarée reprise, lien qui sort
// de la destination, secret qu'il n'a pas pu chercher. Chacun rend le code 6.

// minGuardedSecret est la longueur en deçà de laquelle un secret n'est pas
// cherché dans la copie : court, il se retrouverait par hasard dans des fichiers
// qui ne le portent pas, et les masquer altérerait la copie. ogs génère des mots
// de passe de 32 caractères ; un mot de passe plus court vient d'un projet
// adopté.
const minGuardedSecret = 12

// cloneGuard est ce que la copie d'un clone retient de la source. searched dit
// quels secrets sont cherchés dans la copie ; unchecked nomme ceux qu'ogs n'a
// pas pu lire ou pas pu chercher : la copie n'en est pas expurgée, et le clone
// le dit.
type cloneGuard struct {
	fs.Guard
	searched  []string
	unchecked []string
}

// cloneGuard prépare la garde d'un clone de source : les fichiers de
// configuration et les caches de son type d'application, les mots de passe de
// ses bases et de son transport, et les valeurs de son fichier d'environnement
// — les seuls secrets qu'ogs sait reconnaître dans un fichier, puisqu'il les a
// générés ou qu'il les garde. Le store doit être ouvert.
func (a *App) cloneGuard(source *model.Project) cloneGuard {
	var g cloneGuard
	if at, err := source.AppType(); err == nil {
		for _, p := range at.Config {
			g.Paths = append(g.Paths, fs.GuardPath{Pattern: p,
				Reason: fmt.Sprintf("fichier de configuration d'une application %s, où %s range ses identifiants "+
					"et ses secrets", at.Kind, source.ID())})
		}
		for _, p := range at.Cache {
			g.Paths = append(g.Paths, fs.GuardPath{Pattern: p,
				Reason: fmt.Sprintf("cache compilé depuis la configuration de %s, que l'application reconstruit",
					source.ID())})
		}
	}
	add := func(value, reason string) {
		for _, v := range g.Values {
			if string(v.Value) == value {
				return
			}
		}
		g.Values = append(g.Values, fs.GuardValue{Value: []byte(value), Reason: reason})
	}

	// owner nomme, pour la sortie, à quoi appartient le mot de passe : ceux qui
	// sont dits cherchés sont ceux qui l'ont été.
	type account struct{ engine, user, what, owner string }
	var accounts []account
	for _, d := range source.Spec.Databases {
		accounts = append(accounts, account{d.Engine, d.User,
			fmt.Sprintf("mot de passe de %s (base %s %s)", d.User, d.Engine, d.Name), "des bases"})
		for _, u := range d.ExtraUsers {
			accounts = append(accounts, account{d.Engine, u.User,
				fmt.Sprintf("mot de passe de %s (base %s %s)", u.User, d.Engine, d.Name), "des bases"})
		}
	}
	if q := source.Spec.AMQP; q != nil {
		accounts = append(accounts, account{"amqp", q.User,
			fmt.Sprintf("mot de passe de %s (vhost AMQP %s)", q.User, q.Vhost), "du transport"})
	}
	passwords := &db.Provider{Store: a.Store, Keys: a.Keys}
	var owners []string
	for _, acc := range accounts {
		if a.Keys == nil {
			g.unchecked = append(g.unchecked, fmt.Sprintf("%s : keystore indisponible (%s)", acc.what,
				a.Paths.MasterKey()))
			continue
		}
		pw, err := passwords.Password(source.ID(), acc.engine, acc.user)
		switch {
		case err != nil:
			g.unchecked = append(g.unchecked, fmt.Sprintf("%s : illisible dans le keystore (%v)", acc.what, err))
		case len(pw) < minGuardedSecret:
			g.unchecked = append(g.unchecked, fmt.Sprintf("%s : trop court (%d caractères) pour être cherché "+
				"sans altérer le reste de la copie", acc.what, len(pw)))
		default:
			if !slices.Contains(owners, acc.owner) {
				owners = append(owners, acc.owner)
			}
			add(pw, acc.what)
			// Le mot de passe d'un DSN est parfois écrit encodé : « + » et « = »,
			// que l'alphabet des mots de passe comprend, y deviennent %2B et %3D.
			if enc := url.QueryEscape(pw); enc != pw {
				add(enc, acc.what)
			}
		}
	}
	if len(owners) > 0 {
		g.searched = append(g.searched, fmt.Sprintf("les mots de passe %s de %s", enumerate(owners), source.ID()))
	}

	// Le fichier d'environnement n'est pas recopié, mais l'exploitant en
	// reporte souvent les valeurs dans un fichier de www : elles sont des
	// secrets de la source au même titre (D75), qu'ogs garde et reconnaît.
	path := a.Paths.EnvFile(source.ID())
	vars, err := readEnvFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		g.unchecked = append(g.unchecked, fmt.Sprintf("valeurs de %s : %v", path, err))
	default:
		n := 0
		for _, v := range vars {
			if len(v.value) >= minGuardedSecret {
				add(v.value, fmt.Sprintf("valeur de %s (%s)", v.key, path))
				n++
			}
		}
		if n > 0 {
			g.searched = append(g.searched, fmt.Sprintf("les valeurs de %s d'au moins %d caractères", path,
				minGuardedSecret))
		}
	}
	return g
}

// envVar est une affectation d'un fichier d'environnement.
type envVar struct{ key, value string }

// maxEnvFile borne la relecture du fichier d'environnement d'un projet : il est
// à root, mais un fichier démesuré n'en est pas un.
const maxEnvFile = 1 << 20

// readEnvFile relit les affectations du fichier d'environnement path, dans la
// forme où systemd le lit (EnvironmentFile=) : une affectation NOM=valeur par
// ligne, les lignes vides et celles qui commencent par « # » ou « ; » ignorées,
// une valeur entre guillemets rendue sans eux. Un lien à sa place est refusé,
// comme l'apply le refuse (D75).
func readEnvFile(path string) ([]envVar, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return nil, errors.New("lien refusé, que systemd suivrait en root")
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("pas un fichier ordinaire")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxEnvFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxEnvFile {
		return nil, fmt.Errorf("plus de %d octets", maxEnvFile)
	}
	var out []envVar
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		out = append(out, envVar{key: strings.TrimSpace(key), value: value})
	}
	return out, nil
}

// declaredReview est ce que le clone reprend de la déclaration de la source :
// une ligne par champ nommé, le nombre de ceux que l'exploitant doit revoir, et
// celui des champs où un secret a été masqué.
type declaredReview struct {
	lines  []string
	review int
	masked int
}

// reviewDeclared expurge la déclaration du clone p des valeurs de la garde,
// remplace la racine du projet source par celle du clone, et nomme chaque champ
// qu'elle reprend de la source et que l'exploitant doit revoir. Chaque variable
// d'environnement l'est, masquée ou non : elle parvient aux processus du projet
// sans passer par sa configuration applicative — par FPM, par systemd —, et ogs
// n'en connaît pas le sens, une clé d'API comme une adresse. Un autre champ —
// commande, répertoire de travail, réglage du pool — l'est quand un secret y a
// été masqué. La racine réécrite est nommée sans être à revoir : ogs calcule
// celle de chaque projet et sait donc laquelle substituer à laquelle, et la
// garder ferait lancer par le clone le programme de la source, là où la commande
// d'un projet « custom » le désigne — l'aide de --command en donne l'exemple.
// Les tables sont celles du clone, que cloneProject a dupliquées : celles de la
// source restent intactes.
func reviewDeclared(p, source *model.Project, values []fs.GuardValue) declaredReview {
	var r declaredReview
	from, to := source.Spec.Layout.Root, p.Spec.Layout.Root
	field := func(name string, variable bool, s *string) {
		var reasons []string
		for _, v := range values {
			if strings.Contains(*s, string(v.Value)) {
				*s = strings.ReplaceAll(*s, string(v.Value), string(fs.MaskOf(len(v.Value))))
				if !slices.Contains(reasons, v.Reason) {
					reasons = append(reasons, v.Reason)
				}
			}
		}
		var what []string
		if len(reasons) > 0 {
			what = append(what, "secret masqué")
			r.masked++
		}
		rebased := false
		if *s, rebased = rebase(*s, from, to); rebased {
			what = append(what, fmt.Sprintf("racine %s remplacée par %s", from, to))
		}
		line := ""
		switch {
		case len(what) > 0:
			line = fmt.Sprintf("repris de %s, %s : %s", source.ID(), strings.Join(what, ", "), name)
		case variable:
			line = fmt.Sprintf("repris de %s tel quel : %s", source.ID(), name)
		default:
			return
		}
		if len(reasons) > 0 {
			line += " (" + strings.Join(reasons, ", ") + ")"
		}
		r.lines = append(r.lines, line)
		if variable || len(reasons) > 0 {
			r.review++
		}
	}
	table := func(name string, variable func(key string) bool, m map[string]string) {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			v := m[k]
			field(name+"."+k, variable(k), &v)
			m[k] = v
		}
	}
	always := func(string) bool { return true }
	never := func(string) bool { return false }
	table("spec.app.env", always, p.Spec.App.Env)
	field("spec.app.command", false, &p.Spec.App.Command)
	for _, pool := range p.Spec.PHP.Pools {
		table(fmt.Sprintf("spec.php.pools[%s].env", pool.Name), always, pool.Env)
		table(fmt.Sprintf("spec.php.pools[%s].settings", pool.Name), never, pool.Settings)
	}
	for _, svc := range p.Spec.Services {
		// Les paramètres env.* deviennent des variables de l'unité ; les autres
		// règlent le service — transport, limites —, et seuls command et workdir
		// peuvent porter un secret ou un chemin.
		table(fmt.Sprintf("spec.services[%s].params", svc.Name), func(k string) bool {
			return strings.HasPrefix(k, "env.")
		}, svc.Params)
	}
	for i := range p.Spec.Crons {
		c := &p.Spec.Crons[i]
		field(fmt.Sprintf("spec.crons[%s].command", c.Name), false, &c.Command)
		field(fmt.Sprintf("spec.crons[%s].workdir", c.Name), false, &c.Workdir)
	}
	return r
}

// rebase remplace dans s le chemin from par to partout où il désigne from ou un
// chemin sous lui : from n'y est ni précédé ni suivi d'un caractère de nom,
// hormis la barre qui ouvre un chemin sous lui — « /home/shop.fr.old » n'est pas
// sous « /home/shop.fr ». Il dit s'il a remplacé quelque chose.
func rebase(s, from, to string) (string, bool) {
	if from == "" || from == "/" {
		return s, false
	}
	var b strings.Builder
	done, last := false, 0
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], from)
		if j < 0 {
			break
		}
		j += i
		end := j + len(from)
		if (j == 0 || !isPathByte(s[j-1])) && (end == len(s) || s[end] == '/' || !isPathByte(s[end])) {
			b.WriteString(s[last:j])
			b.WriteString(to)
			last, done = end, true
			i = end
			continue
		}
		i = j + 1
	}
	if !done {
		return s, false
	}
	b.WriteString(s[last:])
	return b.String(), true
}

// isPathByte dit si b peut appartenir à un nom de chemin tel qu'ogs en compose.
func isPathByte(b byte) bool {
	return b == '/' || b == '.' || b == '-' || b == '_' ||
		'0' <= b && b <= '9' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
}

// reportCloneCopy affiche ce que la garde a retenu de la copie, ce qu'elle en a
// expurgé et ce qu'elle n'a pas pu y chercher — si quelque chose a été recopié
// (copied) —, et ce que la copie a repris tel quel bien qu'il sorte de la
// destination ou double une entrée retenue ; il le résume pour le code de
// retour.
func (a *App) reportCloneCopy(skipped fs.Skipped, g cloneGuard, copied bool) []string {
	unchecked := g.unchecked
	if !copied {
		unchecked = nil
	}
	named := func(prefix string, names []string, total int, more string) {
		for _, s := range names {
			a.printf("⚠ %s : %s\n", prefix, s)
		}
		if n := total - len(names); n > 0 {
			a.printf("⚠ … et %d autre(s) %s\n", n, more)
		}
	}
	named("non recopié", skipped.WithheldNamed, skipped.Withheld, "entrée(s) de configuration non recopiée(s)")
	named("secret retiré de la copie", skipped.MaskedNamed, skipped.Masked, "fichier(s) expurgé(s)")
	named("recopié tel quel", skipped.ExposedNamed, skipped.Exposed,
		"fichier(s) lié(s) physiquement à une entrée non recopiée")
	named("lien recopié tel quel", skipped.OutsideNamed, skipped.Outside, "lien(s) qui sortent de la destination")
	for _, s := range unchecked {
		a.printf("⚠ copie non vérifiée : %s\n", s)
	}
	var parts []string
	count := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	count(skipped.Withheld, "entrée(s) de configuration non recopiée(s)")
	count(skipped.Masked, "fichier(s) expurgé(s)")
	count(skipped.Exposed, "fichier(s) lié(s) à une entrée non recopiée")
	count(skipped.Outside, "lien(s) qui sortent de la destination")
	count(len(unchecked), "secret(s) non cherché(s)")
	return parts
}

// exposedInstaller rend le répertoire d'installation que la copie a laissé dans
// dstWWW, vide si le type n'en a pas ou qu'un de ses fichiers de configuration y
// est — écrit par un hook postCreate : une application qui ne trouve aucun de
// ceux-ci renvoie tout visiteur vers celui-là.
func exposedInstaller(at model.AppType, dstWWW string) string {
	if len(at.Installer) == 0 {
		return ""
	}
	for _, p := range at.Config {
		if strings.ContainsAny(p, "*?[") {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dstWWW, filepath.FromSlash(p))); err == nil {
			return ""
		}
	}
	for _, d := range at.Installer {
		if info, err := os.Lstat(filepath.Join(dstWWW, d)); err == nil && info.IsDir() {
			return d
		}
	}
	return ""
}

// enumerate joint des éléments comme une phrase : « a, b et c ».
func enumerate(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " et " + items[len(items)-1]
}

// adviseCloneConfig dit ce que la garde a retiré de la copie et ce qu'elle a
// laissé passer, pour tout type qui fait tourner du code ou tient des secrets
// d'ogs — jamais davantage qu'elle n'a fait —, puis ce que la destination attend
// de l'exploitant : sa propre configuration, dont ogs lui donne les chaînes de
// connexion. copied est faux quand la copie a été refusée : aucun fichier n'est
// alors passé.
func (a *App) adviseCloneConfig(source, target *model.Project, g cloneGuard, skipped fs.Skipped,
	declared declaredReview, copied, dataCopied bool) {
	at, err := source.AppType()
	if err != nil {
		return
	}
	resources := len(target.Spec.Databases) > 0 || target.Spec.AMQP != nil
	if !at.NeedsPHP && at.Runtime == "" && !resources && len(g.Values) == 0 && len(g.unchecked) == 0 &&
		len(declared.lines) == 0 {
		return // un site statique : sa copie n'est que celle de ses fichiers
	}
	type line struct{ label, text string }
	var kept []line
	keep := func(label, text string, args ...any) { kept = append(kept, line{label, fmt.Sprintf(text, args...)}) }
	show := func(lines []line) {
		for _, l := range lines {
			a.printf("  %-22s %s\n", l.label, l.text)
		}
	}

	if copied {
		var removed []string
		switch {
		case len(at.Config) > 0 && len(at.Cache) > 0:
			removed = append(removed, fmt.Sprintf("les fichiers de configuration et les caches connus d'une "+
				"application %s", at.Kind))
		case len(at.Config) > 0:
			removed = append(removed, fmt.Sprintf("les fichiers de configuration connus d'une application %s",
				at.Kind))
		}
		removed = append(removed, g.searched...)
		switch {
		case len(removed) == 0:
			a.printf("\n⚠ ogs n'a rien retiré de la copie : il ne connaît pas les fichiers de configuration "+
				"d'une application %s, et n'a cherché aucun secret de %s\n", at.Kind, source.ID())
		case len(at.Config) == 0:
			a.printf("\n⚠ ogs n'a retiré de la copie que %s — il ne connaît pas les fichiers de configuration "+
				"d'une application %s\n", enumerate(removed), at.Kind)
		default:
			a.printf("\n⚠ ogs n'a retiré de la copie que %s\n", enumerate(removed))
		}
		if len(at.Config) > 0 {
			keep("fichiers", "tout autre fichier de www (un .env versionné, un fichier de config/…) : "+
				"un secret qu'ogs ne connaît pas y est en clair")
		} else {
			keep("fichiers", "tous les fichiers de www : un secret qu'ogs ne connaît pas y est en clair")
		}
		if skipped.Masked > 0 || declared.masked > 0 {
			keep("chaînes expurgées", "le secret seul y est masqué : elles nomment encore l'utilisateur, la "+
				"base ou le vhost de %s", source.ID())
		}
		if n := len(g.unchecked); n > 0 {
			keep("secrets non cherchés", "%d, nommé(s) plus haut : la copie peut les porter en clair", n)
		}
	} else if declared.masked > 0 {
		keep("chaînes expurgées", "le secret seul y est masqué : elles nomment encore l'utilisateur, la base "+
			"ou le vhost de %s", source.ID())
	}
	if n := declared.review; n > 0 {
		keep("déclaration", "%d champ(s) à revoir, nommé(s) plus haut", n)
	}
	if dataCopied {
		keep("base", "copie de celle de %s : ce que l'application y range — adresses et domaine du site, "+
			"clés de modules, relais de courrier — y est tel quel", source.ID())
	}
	if len(kept) > 0 {
		if !copied {
			a.printf("\n")
		}
		a.printf("⚠ %s reprend tel quel de %s :\n", target.ID(), source.ID())
		show(kept)
	}

	var todo []line
	if resources {
		todo = append(todo, line{"chaînes de connexion", "ogs secrets env " + target.ID()})
	}
	if target.Spec.App.EnvFile {
		todo = append(todo, line{"environnement", fmt.Sprintf("%s, créé vide — celui de %s n'est pas recopié",
			a.Paths.EnvFile(target.ID()), source.ID())})
	}
	if len(todo) > 0 {
		a.printf("= configuration de %s à écrire, avec ses propres identifiants :\n", target.ID())
		show(todo)
	}
}

// describeCloneGuard annonce, pour --dry-run, ce que la garde retiendra de la
// copie, ce qu'elle y masquera, ce qu'elle reprendra tel quel, et quand le clone
// rendra le code 6.
func (a *App) describeCloneGuard(g cloneGuard, declared declaredReview, withData bool) {
	row := func(label, text string) { a.printf("  %-22s %s\n", label, text) }
	a.printf("\ncopie de www :\n")
	var patterns []string
	for _, p := range g.Paths {
		patterns = append(patterns, p.Pattern)
	}
	if len(patterns) > 0 {
		row("non recopiés", strings.Join(patterns, ", "))
	}
	if len(g.searched) > 0 {
		row("masqués partout", enumerate(g.searched))
	}
	for _, u := range g.unchecked {
		row("non cherché", u)
	}
	for _, d := range declared.lines {
		row("déclaration", d)
	}
	rest := "le reste de www, liens qui en sortent compris — ceux-ci nommés un à un"
	if withData {
		rest += " ; le contenu de la base"
	}
	row("repris tels quels", rest)
	var certain []string
	if n := declared.review; n > 0 {
		certain = append(certain, fmt.Sprintf("%d champ(s) de la déclaration à revoir", n))
	}
	if n := len(g.unchecked); n > 0 {
		certain = append(certain, fmt.Sprintf("%d secret(s) non cherché(s)", n))
	}
	if len(certain) > 0 {
		a.printf("le clone rendra le code 6 : %s\n", strings.Join(certain, ", "))
		return
	}
	what := "un lien qui sort de www"
	if len(patterns) > 0 || len(g.searched) > 0 {
		what = "l'un de ces fichiers, l'un de ces secrets ou " + what
	}
	a.printf("le clone rendra le code 6 si la copie rencontre %s\n", what)
}

// adviseDisabledTasks nomme les tâches et les services que le clone a repris
// désactivés (cloneProject), et comment réactiver chacun.
func (a *App) adviseDisabledTasks(source, target *model.Project) {
	var lines []string
	for _, c := range source.Spec.Crons {
		if isEnabled(c.Enabled) {
			lines = append(lines, fmt.Sprintf("  tâche %s : ogs cron enable %s %s", c.Name, target.ID(), c.Name))
		}
	}
	for _, s := range source.Spec.Services {
		if isEnabled(s.Enabled) {
			lines = append(lines, fmt.Sprintf("  service %s : retirer « enabled: false » de %s, puis ogs apply %s",
				s.Name, a.Paths.ProjectFile(target.ID()), target.ID()))
		}
	}
	if len(lines) == 0 {
		return
	}
	a.printf("\n= tâches et services de %s repris désactivés : ils ne doivent tourner qu'une fois %s configuré\n",
		source.ID(), target.ID())
	for _, l := range lines {
		a.printf("%s\n", l)
	}
}
