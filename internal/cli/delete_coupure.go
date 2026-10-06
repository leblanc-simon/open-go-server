package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/cron"
)

// siteCut est le site d'un projet coupé avant l'archive (D114) : ses vhosts et
// leurs liens, ses pools et son fichier cron quittent le disque, les serveurs
// sont rechargés sans eux, et leur contenu reste en mémoire — l'archive le
// recueille, un abandon le rend. Laissé servi pendant l'archive, le site
// continuait d'écrire en base et dans son arborescence après le dump : l'archive
// mêlait deux instants, et ce qui avait été écrit entre-temps partait sans copie.
type siteCut struct {
	files []cutFile
	// versions nomme les versions de PHP dont un pool est parti, à recharger.
	versions []string
	// web dit qu'un vhost est parti : le serveur web est à recharger.
	web bool
}

// cutFile est un fichier retiré par la coupure, tel qu'il était.
type cutFile struct {
	path string
	// link est la cible d'un lien ; data, le contenu d'un fichier ordinaire.
	link     string
	data     []byte
	mode     os.FileMode
	uid, gid int
}

// lookup rend le fichier coupé à ce chemin, ou nil.
func (c *siteCut) lookup(path string) *cutFile {
	if c == nil {
		return nil
	}
	for i := range c.files {
		if c.files[i].path == path {
			return &c.files[i]
		}
	}
	return nil
}

// siteFiles énumère, parmi les fichiers que la suppression retire, ceux qui
// servent le site — vhosts et leurs liens, pools, fichier cron —, et les
// versions de PHP de ces pools. Ce que la suppression laisse en place
// (projectFiles) n'est jamais coupé : ogs ne l'a pas écrit pour ce projet.
func (a *App) siteFiles(provs *providers, p *model.Project, removing []string) (web, pools map[string]string, cronFile string) {
	set := map[string]bool{}
	for _, f := range removing {
		set[f] = true
	}
	web, pools = map[string]string{}, map[string]string{}
	for _, name := range vhostFileNames(provs, p) {
		for _, path := range []string{filepath.Join(provs.Engine.EnabledDir(), name),
			filepath.Join(provs.Engine.AvailableDir(), name)} {
			if set[path] {
				web[path] = ""
			}
		}
	}
	for _, pool := range p.Spec.PHP.Pools {
		name := p.ID()
		if pool.Name != model.MainPoolName {
			name = p.ID() + "_" + pool.Name
		}
		if path := a.Paths.PHPPoolFile(pool.Version, name); set[path] {
			pools[path] = pool.Version
		}
	}
	// Et tout pool que l'en-tête attribue au projet sans que la déclaration le
	// nomme (D115) : il tourne sous le compte, et part de toute façon.
	for _, path := range provs.PHP.OwnedPools(p.ID()) {
		if _, ok := pools[path]; ok || !set[path] {
			continue
		}
		if v := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path)))); filepath.Dir(path) == a.Paths.PHPPoolDir(v) {
			pools[path] = v
		}
	}
	if path := filepath.Join(a.Paths.CronDir(), cron.FileName(p.ID())); set[path] {
		cronFile = path
	}
	return web, pools, cronFile
}

// cutSite coupe le site avant l'archive : les tâches d'abord, puis les vhosts —
// plus aucune requête n'atteint les pools —, puis les pools. Un serveur à
// l'arrêt n'a rien à recharger. Ce qui ne peut être relu ni retiré arrête la
// coupure, qui rend ce qu'elle a déjà retiré : l'appelant abandonne alors la
// suppression. Un rechargement refusé, lui, n'est qu'un avertissement — le
// serveur sert encore le site, l'archive n'est alors pas plus incohérente
// qu'auparavant, et l'étape 4 rechargera de nouveau.
func (a *App) cutSite(ctx context.Context, provs *providers, p *model.Project, removing []string,
	warn func(format string, args ...any)) (*siteCut, error) {
	web, pools, cronFile := a.siteFiles(provs, p, removing)
	cut := &siteCut{}
	var order []string
	if cronFile != "" {
		order = append(order, cronFile)
	}
	// Les liens d'activation avant les fichiers qu'ils désignent : removing suit
	// l'ordre de generatedFiles, qui les place ainsi.
	for _, f := range removing {
		if _, ok := web[f]; ok {
			order = append(order, f)
		}
	}
	for _, f := range removing {
		if _, ok := pools[f]; ok {
			order = append(order, f)
		}
	}
	if len(order) == 0 {
		return cut, nil
	}
	a.printf("coupure du site de %s : vhost, pool et tâches retirés le temps de l'archive…\n", p.ID())
	for _, path := range order {
		f, err := readCutFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil {
			err = os.Remove(path)
		}
		if err != nil {
			a.restoreSite(ctx, provs, p, cut, warn)
			return nil, wrap(CodeError, fmt.Errorf("coupure du site : %s : %w", path, err))
		}
		cut.files = append(cut.files, f)
		if _, ok := web[path]; ok {
			cut.web = true
		}
		if v, ok := pools[path]; ok && !slices.Contains(cut.versions, v) {
			cut.versions = append(cut.versions, v)
		}
	}
	if !a.reloadCut(ctx, provs, p, cut, warn, "coupure du site") {
		// Le serveur sert encore la configuration chargée : l'archive n'est pas
		// figée, et l'exploitant doit le savoir avant de s'y fier.
		warn("le site de %s reste servi pendant l'archive : rechargement refusé, l'archive et le dump "+
			"ne portent pas un état figé", p.ID())
	}
	return cut, nil
}

// restoreSite rend ce que cutSite a retiré, quand la suppression renonce avant
// le point de non-retour : les pools d'abord, puis les vhosts, puis les tâches,
// et les serveurs rechargés. Le contexte est détaché, comme pour la relance des
// services : une interruption est l'un des abandons à réparer. Un fichier
// qu'on n'a pas pu rendre est nommé — « ogs apply <id> » le réécrit.
func (a *App) restoreSite(ctx context.Context, provs *providers, p *model.Project, cut *siteCut,
	warn func(format string, args ...any)) {
	if cut == nil || len(cut.files) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	a.printf("rétablissement du site de %s : rien n'a été supprimé\n", p.ID())
	failed := false
	// Ordre inverse de la coupure : pools, vhosts (le fichier avant son lien),
	// tâches.
	for i := len(cut.files) - 1; i >= 0; i-- {
		if err := cut.files[i].restore(); err != nil {
			failed = true
			a.warnf("⚠ rétablissement de %s : %v\n", cut.files[i].path, err)
		}
	}
	a.reloadCut(ctx, provs, p, cut, warn, "rétablissement du site")
	if failed {
		a.warnf("⚠ le site de %s n'est pas entièrement rétabli — « ogs apply %s » réécrit ce qui manque\n",
			p.ID(), p.ID())
	}
}

// reloadCut recharge les serveurs après une coupure ou un rétablissement, et dit
// s'ils l'ont tous accepté. Un
// serveur web à l'arrêt n'a rien à recharger ; FPM, lui, est démarré ou arrêté
// selon qu'il garde des pools (D74) — le dernier pool coupé arrête la version.
func (a *App) reloadCut(ctx context.Context, provs *providers, p *model.Project, cut *siteCut,
	warn func(format string, args ...any), what string) bool {
	ok := true
	if cut.web && a.unitActive(ctx, provs.Web.Engine.Unit()) {
		if err := provs.Web.Reload(ctx); err != nil {
			ok = false
			warn("%s de %s : rechargement du serveur web : %v", what, p.ID(), err)
		}
	}
	for _, v := range cut.versions {
		if err := provs.PHP.Reload(ctx, v); err != nil {
			ok = false
			warn("%s de %s : rechargement de PHP %s : %v", what, p.ID(), v, err)
		}
	}
	return ok
}

// readCutFile relit un fichier avant de le retirer : contenu ou cible du lien,
// mode et propriétaire.
func readCutFile(path string) (cutFile, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return cutFile{}, err
	}
	f := cutFile{path: path, mode: info.Mode().Perm(), uid: -1, gid: -1}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		f.uid, f.gid = int(st.Uid), int(st.Gid)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		f.link, err = os.Readlink(path)
	case info.Mode().IsRegular():
		f.data, err = os.ReadFile(path)
	default:
		err = fmt.Errorf("ni fichier ordinaire ni lien")
	}
	return f, err
}

// restore rend le fichier tel qu'il était. Un fichier reposé entre-temps à cet
// emplacement n'est pas écrasé.
func (f cutFile) restore() error {
	if _, err := os.Lstat(f.path); err == nil {
		return fmt.Errorf("un fichier a été reposé à cet emplacement, laissé tel quel")
	}
	if f.link != "" {
		if err := os.Symlink(f.link, f.path); err != nil {
			return err
		}
		if f.uid >= 0 {
			return os.Lchown(f.path, f.uid, f.gid)
		}
		return nil
	}
	return fs.WriteAtomic(f.path, f.data, f.mode, fs.Owner{UID: f.uid, GID: f.gid})
}
