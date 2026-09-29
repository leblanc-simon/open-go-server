package web

import (
	"os"
	"path/filepath"
	"strings"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/provider/removal"
)

// removeStep retire un vhost devenu inutile, sous le régime des cinq états : un
// fichier retouché exige --force et part sauvegardé, un fichier qu'ogs n'a pas
// écrit pour le projet id reste en place.
//
// Les liens d'activation suivent le sort du fichier : ils partent avec lui, et
// la compensation les rétablit avec lui — rendre le fichier sans eux annoncerait
// un retour à l'état initial en laissant le vhost hors ligne, ni Apache ni nginx
// ne chargeant sites-available. Ce sont les liens qui désignent le fichier, et
// eux seuls : un fichier régulier posé dans sites-enabled sous le même nom, ou un
// lien de même nom vers ailleurs, n'est pas son activation.
func (p *Provider) removeStep(path, id string) (*engine.Step, error) {
	// Seule l'extension du moteur est ôtée : « shop.fr » et « shop.de » ne
	// doivent pas se confondre sous une même ressource.
	name := strings.TrimSuffix(filepath.Base(path), model.VHostExt)
	return removal.File{
		Resource: "web/vhost/" + name,
		Summary:  "vhost obsolète " + path,
		Path:     path,
		Project:  id,
		Force:    p.Force,
		Backups:  p.Paths.BackupsDir(),
		Now:      p.now,
		Links:    func() []string { return p.linksTo(path) },
		Reload:   []string{"web:" + p.Engine.Name()},
	}.Step()
}

// linksTo énumère les liens de sites-enabled qui désignent path, relatifs comme
// ceux d'a2ensite et d'ogs, ou absolus. C'est la cible qui décide, jamais le
// nom.
func (p *Provider) linksTo(path string) []string {
	dir := p.Engine.EnabledDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		link := filepath.Join(dir, e.Name())
		dest, err := os.Readlink(link)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(dir, dest)
		}
		if filepath.Clean(dest) == filepath.Clean(path) {
			out = append(out, link)
		}
	}
	return out
}
