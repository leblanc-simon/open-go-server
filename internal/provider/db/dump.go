package db

import (
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
)

// Dump sauvegarde une base. Les sauvegardes planifiées sont hors périmètre : cette
// commande ponctuelle sert l'exploitation courante et l'archivage à la suppression
// d'un projet, où une archive sans données serait inexploitable.
func (p *Provider) Dump(ctx context.Context, proj *model.Project, decl model.Database, output string) (string, error) {
	if output == "" {
		return "", fmt.Errorf("fichier de destination requis")
	}
	if err := checkIdentifier("nom de base", decl.Name); err != nil {
		return "", err
	}
	compress := strings.HasSuffix(output, ".gz")
	tool, args, err := p.dumpCommand(decl)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(tool); err != nil {
		return "", fmt.Errorf("%s est absent : installer les outils clients du moteur %s", tool, decl.Engine)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o750); err != nil {
		return "", err
	}
	res, err := p.Exec.Run(ctx, exec.Command{Name: tool, Args: args})
	if err != nil {
		return "", err
	}
	// La sortie est écrite avec des permissions restrictives : un dump contient
	// l'intégralité des données de l'application.
	f, err := os.OpenFile(output, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if compress {
		gz := gzip.NewWriter(f)
		if _, err := gz.Write([]byte(res.Stdout)); err != nil {
			return "", err
		}
		if err := gz.Close(); err != nil {
			return "", err
		}
	} else if _, err := f.WriteString(res.Stdout); err != nil {
		return "", err
	}
	return output, nil
}

// dumpCommand construit la commande de sauvegarde du moteur.
func (p *Provider) dumpCommand(decl model.Database) (string, []string, error) {
	switch decl.Engine {
	case "mysql", "mariadb":
		socket, ok := FindMySQLSocket(p.Paths.Prefix())
		if !ok {
			return "", nil, fmt.Errorf("%w : aucun socket MySQL", ErrUnavailable)
		}
		return filepath.Join(p.Paths.Prefix(), "/usr/bin/mysqldump"), []string{
			"--socket=" + socket,
			"--user=root",
			// --single-transaction évite de verrouiller les tables InnoDB pendant
			// la sauvegarde ; sans elle, un dump bloque le site.
			"--single-transaction",
			"--routines",
			"--triggers",
			"--default-character-set=utf8mb4",
			decl.Name,
		}, nil
	case "postgres":
		clusters := DetectClusters(p.Paths.Prefix())
		if len(clusters) == 0 {
			return "", nil, fmt.Errorf("%w : aucun cluster PostgreSQL", ErrUnavailable)
		}
		cluster := clusters[0]
		for _, c := range clusters {
			if c.ID() == decl.Cluster {
				cluster = c
			}
		}
		socketDir := cluster.Socket
		if socketDir == "" {
			socketDir = filepath.Join(p.Paths.Prefix(), "/var/run/postgresql")
		}
		return filepath.Join(p.Paths.Prefix(), "/usr/bin/pg_dump"), []string{
			"--host=" + socketDir,
			fmt.Sprintf("--port=%d", cluster.Port),
			"--no-password",
			decl.Name,
		}, nil
	}
	return "", nil, fmt.Errorf("moteur %q inconnu", decl.Engine)
}

// Restore recharge une sauvegarde. L'opération est destructrice pour le contenu
// existant : l'appelant doit avoir obtenu confirmation.
func (p *Provider) Restore(ctx context.Context, proj *model.Project, decl model.Database, input string) error {
	if err := checkIdentifier("nom de base", decl.Name); err != nil {
		return err
	}
	data, err := os.ReadFile(input)
	if err != nil {
		return err
	}
	var tool string
	var args []string
	switch decl.Engine {
	case "mysql", "mariadb":
		socket, ok := FindMySQLSocket(p.Paths.Prefix())
		if !ok {
			return fmt.Errorf("%w : aucun socket MySQL", ErrUnavailable)
		}
		tool = filepath.Join(p.Paths.Prefix(), "/usr/bin/mysql")
		args = []string{"--socket=" + socket, "--user=root", decl.Name}
	case "postgres":
		clusters := DetectClusters(p.Paths.Prefix())
		if len(clusters) == 0 {
			return fmt.Errorf("%w : aucun cluster PostgreSQL", ErrUnavailable)
		}
		cluster := clusters[0]
		for _, c := range clusters {
			if c.ID() == decl.Cluster {
				cluster = c
			}
		}
		socketDir := cluster.Socket
		if socketDir == "" {
			socketDir = filepath.Join(p.Paths.Prefix(), "/var/run/postgresql")
		}
		tool = filepath.Join(p.Paths.Prefix(), "/usr/bin/psql")
		args = []string{"--host=" + socketDir, fmt.Sprintf("--port=%d", cluster.Port),
			"--no-password", "--set=ON_ERROR_STOP=1", "--dbname=" + decl.Name}
	default:
		return fmt.Errorf("moteur %q inconnu", decl.Engine)
	}
	if _, err := os.Stat(tool); err != nil {
		return fmt.Errorf("%s est absent : installer les outils clients du moteur %s", tool, decl.Engine)
	}
	_, err = p.Exec.Run(ctx, exec.Command{Name: tool, Args: args, Stdin: data})
	return err
}
