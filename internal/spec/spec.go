// Package spec lit et écrit les fichiers d'état désiré (YAML).
package spec

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/leviathan/ogs/internal/model"
)

// ErrNotFound est renvoyé lorsqu'aucun fichier de projet ne correspond.
var ErrNotFound = errors.New("projet inconnu")

// Decode lit un projet depuis des octets YAML, sans normalisation.
func Decode(data []byte) (*model.Project, error) {
	var p model.Project
	if err := yaml.UnmarshalWithOptions(data, &p, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("YAML illisible : %w", err)
	}
	if p.APIVersion != "" && p.APIVersion != model.APIVersion {
		return nil, fmt.Errorf("apiVersion %q non gérée par cette version d'ogs (attendu %q)",
			p.APIVersion, model.APIVersion)
	}
	return &p, nil
}

// Encode sérialise un projet en YAML.
func Encode(p *model.Project) ([]byte, error) {
	data, err := yaml.MarshalWithOptions(p, yaml.Indent(2), yaml.IndentSequence(true))
	if err != nil {
		return nil, fmt.Errorf("sérialisation YAML : %w", err)
	}
	return data, nil
}

// Load lit un fichier de projet.
func Load(path string) (*model.Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w : %s", ErrNotFound, path)
		}
		return nil, err
	}
	p, err := Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%s : %w", path, err)
	}
	return p, nil
}

// LoadAll lit tous les projets d'un répertoire, triés par identifiant. Le
// sous-répertoire archived/ est ignoré : ces projets ne sont plus réconciliés.
func LoadAll(dir string) ([]*model.Project, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*model.Project
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		p, err := Load(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, nil
}

// Save écrit un fichier de projet. Ces fichiers sont destinés à être lus, édités
// et versionnés par l'administrateur : ils ne portent donc pas l'en-tête de
// gestion des fichiers générés.
func Save(path string, p *model.Project) error {
	data, err := Encode(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Exists indique si un fichier de projet est présent.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}
