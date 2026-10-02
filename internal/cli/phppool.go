package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/php"
)

// checkPoolDeclarations refuse, au plan et à l'apply du projet visé comme aux
// points d'écriture d'une déclaration, ce que le pool ne peut pas rendre (D103) :
// une clé ou une valeur qui injecterait une directive, un preset déclaré que ni
// ogs ni config.yaml ne connaissent. Jamais au chargement : un seul projet
// fautif arrêterait tout le parc, « cert renew » du minuteur compris.
func (a *App) checkPoolDeclarations(p *model.Project) error {
	at, err := p.AppType()
	if err != nil || !at.NeedsPHP {
		return nil
	}
	var lines []string
	for _, pool := range p.Spec.PHP.Pools {
		for _, pb := range php.Resolve(a.Cfg, p, pool, at).Blocking() {
			lines = append(lines, pb.String())
		}
	}
	if len(lines) > 0 {
		return exitf(CodeValidation, "projet %s :\n%s", p.ID(), strings.Join(lines, "\n"))
	}
	return nil
}

// poolPreset nomme le preset que le pool reçoit : déclaré, ou celui de la
// machine, descendu d'un cran en préproduction.
func (a *App) poolPreset(p *model.Project, pool model.Pool) string {
	at, err := p.AppType()
	if err != nil {
		return pool.PresetLabel()
	}
	return php.Resolve(a.Cfg, p, pool, at).DescribePreset()
}

// phpPoolChecks porte les contrôles des pools (D103) : la section « php.pool »
// de config.yaml, les déclarations des projets, la capacité mémoire par version.
// Ils nomment ce que le plan écarte ou refuse, sans rien bloquer.
func (a *App) phpPoolChecks(projects []*model.Project, add func(name, status, detail, fix string)) {
	cfgProblems := php.ConfigProblems(a.Cfg)
	for _, pb := range cfgProblems {
		add("config.yaml php.pool", "avertissement", pb.String(),
			"corriger "+a.Paths.ConfigFile()+" ; chaque pool est rendu sans ce réglage, code 6 à l'apply")
	}
	if len(cfgProblems) == 0 && (len(a.Cfg.PHP.Pool.Defaults) > 0 || len(a.Cfg.PHP.Pool.Presets) > 0 ||
		a.Cfg.PHP.Pool.DefaultPreset != "") {
		add("config.yaml php.pool", "ok", "défauts de la machine applicables, preset par défaut "+
			a.Cfg.PHP.Pool.PresetName(), "")
	}

	declared := 0
	var loads []poolLoad
	for _, p := range projects {
		at, err := p.AppType()
		if err != nil || !at.NeedsPHP {
			continue
		}
		for _, pool := range p.Spec.PHP.Pools {
			r := php.Resolve(a.Cfg, p, pool, at)
			for _, pb := range r.Problems {
				// Ceux de config.yaml sont déjà nommés, une fois pour toute la machine.
				if strings.HasPrefix(pb.Field, "config.yaml") {
					continue
				}
				declared++
				if pb.Blocking {
					add("pools php", "échec", "projet "+p.ID()+" : "+pb.String(),
						"corriger "+p.ID()+".yaml ; plan et apply de ce projet le refusent")
				} else {
					add("pools php", "avertissement", "projet "+p.ID()+" : "+pb.String(),
						"retirer ce réglage de "+p.ID()+".yaml ; le pool est rendu sans lui, code 6 à l'apply")
				}
			}
			loads = append(loads, poolLoad{Project: p.ID(), Pool: pool.Name, Version: pool.Version,
				Settings: r.Settings})
		}
	}
	if declared == 0 && len(loads) > 0 {
		add("pools php", "ok", fmt.Sprintf("%d pool(s) rendus sans réglage écarté", len(loads)), "")
	}

	memTotal, ok := readMemTotal(filepath.Join(a.Paths.Prefix(), "/proc/meminfo"))
	if !ok {
		// Sous préfixe sans meminfo simulé, ou /proc illisible : rien à comparer.
		return
	}
	for _, f := range capacityChecks(loads, memTotal, a.Cfg.PHP.Pool.CapacityWarn()) {
		add(f.name, f.status, f.detail, f.fix)
	}
}

// poolLoad est ce qu'un pool peut consommer au pire.
type poolLoad struct {
	Project, Pool, Version string
	Settings               map[string]string
}

type finding struct{ name, status, detail, fix string }

// capacityChecks compare, par version, la somme des pm.max_children ×
// memory_limit à la mémoire totale. Toutes versions confondues aussi : elles
// partagent la même mémoire. Le chiffre est toujours affiché, en information ;
// il ne devient un avertissement qu'au-delà de warnPercent % de la mémoire
// (« php.pool.capacityWarnPercent », 200 par défaut ; 0 ou moins, jamais). La
// somme est un pire cas — tous les workers de tous les pools présents à la fois,
// chacun à sa limite —, qu'un parc n'atteint presque jamais : le seuil est
// celui d'un dimensionnement déraisonnable, non d'un risque immédiat.
func capacityChecks(loads []poolLoad, memTotal int64, warnPercent int) []finding {
	type agg struct {
		bytes     int64
		pools     int
		unbounded []string
		unknown   []string
	}
	byVersion := map[string]*agg{}
	var total int64
	for _, l := range loads {
		g := byVersion[l.Version]
		if g == nil {
			g = &agg{}
			byVersion[l.Version] = g
		}
		name := l.Project + "/" + l.Pool
		children, err := strconv.ParseInt(strings.TrimSpace(l.Settings["pm.max_children"]), 10, 64)
		if err != nil || children <= 0 {
			g.unknown = append(g.unknown, name+" (pm.max_children illisible)")
			continue
		}
		limit, ok := l.Settings["php_admin_value[memory_limit]"]
		if !ok {
			limit, ok = l.Settings["php_value[memory_limit]"]
		}
		if !ok {
			// La valeur de php.ini s'applique, que doctor ne lit pas : le défaut de PHP.
			limit = "128M"
		}
		bytes, unlimited, ok := parseMemoryLimit(limit)
		switch {
		case unlimited:
			g.unbounded = append(g.unbounded, name)
			continue
		case !ok:
			g.unknown = append(g.unknown, fmt.Sprintf("%s (memory_limit %q illisible)", name, limit))
			continue
		case bytes == 0:
			// PHP refuse une limite nulle (« Failed to set memory limit to 0
			// bytes ») et garde la valeur précédente, que doctor ne connaît pas.
			g.unknown = append(g.unknown, name+" (memory_limit 0, que PHP refuse)")
			continue
		}
		g.bytes += children * bytes
		g.pools++
		total += children * bytes
	}
	versions := make([]string, 0, len(byVersion))
	for v := range byVersion {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	var out []finding
	over := func(bytes int64) bool {
		return warnPercent > 0 && bytes*100 > memTotal*int64(warnPercent)
	}
	for _, v := range versions {
		g := byVersion[v]
		name := "capacité php " + v
		detail := fmt.Sprintf("%d pool(s) : au pire %s (pm.max_children × memory_limit) pour %s de mémoire (%d %%)",
			g.pools, gib(g.bytes), gib(memTotal), percent(g.bytes, memTotal))
		if over(g.bytes) {
			out = append(out, finding{name, "avertissement", fmt.Sprintf("%s, au-delà du seuil de %d %%", detail, warnPercent),
				"réduire pm.max_children ou memory_limit (preset, php.pool.defaults, settings du projet), " +
					"ou régler php.pool.capacityWarnPercent"})
		} else {
			out = append(out, finding{name, "ok", detail, ""})
		}
		if len(g.unbounded) > 0 {
			out = append(out, finding{name, "avertissement",
				"memory_limit illimité (-1), capacité non bornée : " + strings.Join(g.unbounded, ", "),
				"donner une limite à ces pools"})
		}
		if len(g.unknown) > 0 {
			out = append(out, finding{name, "avertissement",
				"non comptés : " + strings.Join(g.unknown, ", "), ""})
		}
	}
	if len(versions) > 1 {
		detail := fmt.Sprintf("toutes versions : au pire %s pour %s de mémoire (%d %%)",
			gib(total), gib(memTotal), percent(total, memTotal))
		status, fix := "ok", ""
		if over(total) {
			status, fix = "avertissement", "les versions partagent la même mémoire : réduire les pools les plus larges"
		}
		out = append(out, finding{"capacité php", status, detail, fix})
	}
	return out
}

// parseMemoryLimit lit une taille au sens de PHP : un entier, suivi ou non de
// K, M ou G (casse indifférente). -1 — toute valeur négative pour PHP — est
// l'absence de limite.
func parseMemoryLimit(v string) (int64, bool, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false, false
	}
	mult := int64(1)
	switch strings.ToUpper(v[len(v)-1:]) {
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	}
	if mult != 1 {
		v = v[:len(v)-1]
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false, false
	}
	if n < 0 {
		return 0, true, true
	}
	return n * mult, false, true
}

// readMemTotal lit MemTotal (en kio) dans un fichier au format de /proc/meminfo.
func readMemTotal(path string) (int64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil || kb <= 0 {
				return 0, false
			}
			return kb << 10, true
		}
	}
	return 0, false
}

func gib(b int64) string {
	return strings.Replace(fmt.Sprintf("%.1f Gio", float64(b)/(1<<30)), ".", ",", 1)
}

func percent(part, whole int64) int64 {
	if whole <= 0 {
		return 0
	}
	return part * 100 / whole
}
