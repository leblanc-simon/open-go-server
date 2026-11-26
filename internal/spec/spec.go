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

	"leblanc.io/open-go-server/internal/model"
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

// DeclarationMode est le mode d'une déclaration neuve, DeclarationDirMode celui
// de son répertoire : le régime qu'« ogs harden » exige sous /etc/ogs (0600,
// 0700). Une déclaration écrite en 0644 était rouverte à chaque création — et à
// chaque réécriture, db add ou cron add compris —, et harden la refermait.
const (
	DeclarationMode    os.FileMode = 0o600
	DeclarationDirMode os.FileMode = 0o700
)

// Save écrit un fichier de projet. Ces fichiers sont destinés à être lus, édités
// et versionnés par l'administrateur : ils ne portent donc pas l'en-tête de
// gestion des fichiers générés. Une déclaration existante garde son mode et son
// propriétaire (WriteFileAtomic) : ogs ne desserre jamais ce que l'exploitant ou
// harden a resserré, et ne referme pas ce qu'il a choisi d'ouvrir.
func Save(path string, p *model.Project) error {
	data, err := Encode(p)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return WriteFileAtomic(path, data)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, DeclarationDirMode); err != nil {
		return err
	}
	// CreateTemp crée en exclusif et en 0600 : ni un reste d'écriture
	// interrompue ni le umask ne décident du mode.
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".ogs-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(DeclarationMode); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// Exists indique si un fichier de projet est présent.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}
