package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
)

// Leftovers énumère ce qu'ogs a écrit pour l'autre moteur web et qui y est resté
// (R2-27, D118) : vhosts des projets et de supervision, complément d'unité,
// fragment de portée serveur. Seul web.engine décide du moteur piloté ; à sa
// bascule, rien ne retirait la configuration de l'ancien, qui servait encore
// ses vhosts s'il tournait — journaux, certificats et pools compris — et que
// « project delete » ne retirait plus.
//
// Un fichier n'est rangé ici que par son en-tête (D76) : la marque d'ogs, et une
// ressource du web. Rien n'est retiré ni arrêté — l'ancien moteur peut servir
// autre chose, et le retirer serait couper des sites sans le dire : le plan et
// doctor le signalent, l'exploitant décide. Un vhost activé est suivi de
// « (activé) ».
func (p *Provider) Leftovers() (Engine, []string) {
	other := otherEngine(p.Engine)
	if other == nil {
		return nil, nil
	}
	var out []string
	if entries, err := os.ReadDir(other.AvailableDir()); err == nil {
		enabled := enabledTargets(other)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			path := filepath.Join(other.AvailableDir(), e.Name())
			owner, resource, err := fs.ManagedResource(path)
			if err != nil || owner == "" || !strings.HasPrefix(resource, "web/") {
				continue
			}
			if enabled[filepath.Clean(path)] {
				path += " (activé)"
			}
			out = append(out, path)
		}
	}
	sort.Strings(out)
	for _, c := range []struct{ path, resource string }{
		{filepath.Join(p.Paths.SystemdDir(), other.Unit()+".service.d", "ogs.conf"), "web/dropin"},
		{other.SnippetPath(), "web/snippet"},
	} {
		if c.path == "" {
			continue
		}
		if owner, resource, err := fs.ManagedResource(c.path); err == nil && owner != "" && resource == c.resource {
			out = append(out, c.path)
		}
	}
	return other, out
}

// enabledTargets rend les cibles des liens d'activation d'un moteur.
func enabledTargets(e Engine) map[string]bool {
	out := map[string]bool{}
	entries, err := os.ReadDir(e.EnabledDir())
	if err != nil {
		return out
	}
	for _, en := range entries {
		link := filepath.Join(e.EnabledDir(), en.Name())
		dest, err := os.Readlink(link)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(e.EnabledDir(), dest)
		}
		out[filepath.Clean(dest)] = true
	}
	return out
}

// LeftoversNote rend la ligne d'information du plan sur la configuration d'ogs
// restée sous l'autre moteur, vide s'il n'y en a pas. Ce n'est pas une étape :
// une bascule voulue n'est pas un écart, et le parc ne doit pas rendre le code 6
// pour toujours ; doctor porte l'avertissement (D118).
func (p *Provider) LeftoversNote() string {
	other, files := p.Leftovers()
	if len(files) == 0 {
		return ""
	}
	return fmt.Sprintf("configuration d'ogs restée sous %s, ni retirée ni arrêtée (D118, voir « ogs doctor ») : %s",
		other.Name(), strings.Join(files, ", "))
}

// LeftoversAdvice explique ce que la configuration restée sous l'ancien moteur
// implique, et ce qu'il faut en faire.
func LeftoversAdvice(current, other Engine, files []string) string {
	return fmt.Sprintf("web.engine désigne %s, mais %s porte encore %s ; ogs ne les retire pas et "+
		"n'arrête pas %s : s'il tourne, il sert ces vhosts d'après des journaux, certificats et pools "+
		"qu'ogs ne tient plus à jour, et « project delete » n'y touche plus. Les retirer à la main, "+
		"puis « systemctl disable --now %s » si ce moteur n'a plus d'usage (D118)",
		current.Name(), other.Name(), strings.Join(files, ", "), other.Unit(), other.Unit())
}

// OtherUnitActive dit si l'unité de l'autre moteur tourne. Faux en mode
// simulé. Elle suit le contexte de la commande.
func (p *Provider) OtherUnitActive(ctx context.Context) bool {
	other := otherEngine(p.Engine)
	if other == nil || p.Exec.Simulated() {
		return false
	}
	_, err := p.Exec.Run(ctx, exec.Command{
		Name: "systemctl", Args: []string{"is-active", "--quiet", other.Unit()}})
	return err == nil
}

// LeftoversServed dit si l'un des fichiers que rend Leftovers est un vhost
// activé : c'est ce qu'un ancien moteur en marche sert encore.
func (p *Provider) LeftoversServed(files []string) bool {
	for _, f := range files {
		if strings.HasSuffix(f, " (activé)") {
			return true
		}
	}
	return false
}
