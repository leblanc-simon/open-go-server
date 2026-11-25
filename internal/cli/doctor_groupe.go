package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/system"
	"leblanc.io/open-go-server/internal/validate"
)

// groupDirChecks contrôle les répertoires des groupes de projets (D135). L'apply
// en est le seul juge — « ogs harden » n'y touche pas —, et doctor dit ce qu'il
// trouverait :
//
//   - un répertoire de groupe à un autre compte que root : son propriétaire
//     substituerait le home d'un membre ; l'apply le refuse sans le corriger ;
//   - un mode ou un groupe qui ne sont pas root:root 0701 : rétablis au prochain
//     apply d'un membre, mais un répertoire non traversable par les autres
//     coupe d'ici là tous les sites du groupe ;
//   - une entrée qui n'est le home d'aucun membre : un home oublié, ou posé à
//     la main, que le retrait du dernier membre laisserait derrière lui ;
//   - un membre dont le home n'est plus sous le répertoire de son groupe ;
//   - un répertoire laissé par un groupe sans membre — reconnu à son régime,
//     root:root 0701, que rien d'autre ne pose sous la racine des homes.
//
// Sous préfixe, tout appartient à qui lance les tests : seuls le mode et le
// contenu sont jugés.
func (a *App) groupDirChecks(projects []*model.Project, add func(name, status, detail, fix string)) {
	homeRoot := filepath.Clean(a.homeRoot())
	members := map[string][]*model.Project{}
	homes := map[string]bool{}
	for _, p := range projects {
		homes[filepath.Clean(p.Spec.Layout.Root)] = true
		if p.Spec.Group == "" {
			continue
		}
		if err := validate.GroupPlacement(p, homeRoot); err != nil {
			add("groupe "+p.Spec.Group, "échec", fmt.Sprintf("projet %s : %v", p.ID(), err),
				"rendre à spec.group sa valeur d'origine dans "+a.Paths.ProjectFile(p.ID()))
			continue
		}
		members[p.Spec.Group] = append(members[p.Spec.Group], p)
	}
	groups := make([]string, 0, len(members))
	for g := range members {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		a.groupDirCheck(g, filepath.Join(homeRoot, g), members[g], homes, add)
	}
	a.orphanGroupDirChecks(homeRoot, members, homes, add)
}

// groupDirCheck juge le répertoire d'un groupe qui a des membres.
func (a *App) groupDirCheck(group, dir string, members []*model.Project, homes map[string]bool,
	add func(name, status, detail, fix string)) {
	name := "répertoire du groupe " + group
	info, err := os.Lstat(dir)
	switch {
	case os.IsNotExist(err):
		add(name, "ok", dir+" absent : posé au premier apply d'un membre", "")
		return
	case err != nil:
		add(name, "échec", err.Error(), "")
		return
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		add(name, "échec", dir+" n'est pas un répertoire véritable (lien symbolique ?) : l'apply de ses "+
			"membres échoue", "le retirer, puis ogs apply "+members[0].ID())
		return
	}
	uid, gid, _ := system.OwnerOf(info)
	mode := info.Mode().Perm()
	if a.Prefix == "" && uid != 0 {
		add(name, "échec", fmt.Sprintf("%s appartient à l'UID %d : son propriétaire peut substituer le home "+
			"d'un membre, et l'apply refuse le groupe (D135)", dir, uid),
			"chown root:root "+dir+" s'il ne porte que les homes du groupe")
		return
	}
	var écarts []string
	if mode != system.GroupDirMode {
		écarts = append(écarts, fmt.Sprintf("mode %04o", mode))
	}
	if a.Prefix == "" && gid != 0 {
		écarts = append(écarts, fmt.Sprintf("groupe %d", gid))
	}
	switch {
	case mode&0o001 == 0:
		add(name, "échec", fmt.Sprintf("%s (mode %04o) n'est pas traversable : aucun site du groupe n'est "+
			"servi", dir, mode), "ogs apply "+members[0].ID())
	case len(écarts) > 0:
		add(name, "avertissement", fmt.Sprintf("%s : %s, attendu root:root %04o — rétabli au prochain apply",
			dir, listeBornee(écarts, 2), system.GroupDirMode), "ogs apply "+members[0].ID())
	default:
		add(name, "ok", fmt.Sprintf("%s (%d membre(s))", dir, len(members)), "")
	}
	// Ce que le répertoire porte d'autre que les homes de ses membres. Root seul
	// l'énumère : sous préfixe, le compte des tests.
	entries, err := os.ReadDir(dir)
	if err != nil {
		add(name, "avertissement", "contenu illisible : "+err.Error(), "")
		return
	}
	var étrangers []string
	for _, e := range entries {
		if !homes[filepath.Join(dir, e.Name())] {
			étrangers = append(étrangers, e.Name())
		}
	}
	if len(étrangers) > 0 {
		add(name, "avertissement", fmt.Sprintf("%s porte ce qui n'est le home d'aucun projet : %s — le "+
			"répertoire ne partira pas avec le dernier membre", dir, listeBornee(étrangers, 5)),
			"retirer ou déplacer ces entrées")
	}
}

// orphanGroupDirChecks nomme, sous la racine des homes, un répertoire au régime
// d'un groupe — root:root 0701 — qu'aucun projet ne déclare : le dernier membre
// a été retiré à la main, ou le répertoire n'a pas pu partir.
func (a *App) orphanGroupDirChecks(homeRoot string, members map[string][]*model.Project, homes map[string]bool,
	add func(name, status, detail, fix string)) {
	entries, err := os.ReadDir(homeRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		dir := filepath.Join(homeRoot, e.Name())
		if members[e.Name()] != nil || homes[dir] || !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Mode().Perm() != system.GroupDirMode {
			continue
		}
		if uid, gid, _ := system.OwnerOf(info); a.Prefix == "" && (uid != 0 || gid != 0) {
			continue
		}
		contenu := "vide"
		if sub, err := os.ReadDir(dir); err == nil && len(sub) > 0 {
			contenu = fmt.Sprintf("%d entrée(s)", len(sub))
		}
		add("répertoire du groupe "+e.Name(), "avertissement", fmt.Sprintf("%s, au régime d'un répertoire de "+
			"groupe, n'est le groupe d'aucun projet (%s)", dir, contenu), "le retirer s'il ne sert plus : rmdir "+dir)
	}
}
