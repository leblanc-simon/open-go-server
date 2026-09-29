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
	f, err := openDump(output)
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

// openDump ouvre la destination d'une sauvegarde, avec des permissions
// restrictives : un dump contient l'intégralité des données de l'application.
//
// Sur un **fichier ordinaire**, le retrait préalable et O_EXCL interdisent
// d'écrire dans un inode qu'on n'a pas créé soi-même : réutiliser un fichier
// préexistant lui laisserait son propriétaire et son mode, le 0600 annoncé ici
// ne s'appliquant qu'à la création — et une course sur le retrait échoue
// franchement au lieu de livrer les données dans l'inode d'un autre.
//
// Ce raisonnement ne vaut que pour le fichier ordinaire, et c'est tout l'objet
// de la distinction. Sur un périphérique, un tube nommé, un socket ou un lien —
// « -O /dev/null », « -O /dev/stdout » —, il n'y a ni mode ni propriétaire à
// assainir : le retrait ne remettrait pas les droits à zéro, il **détruirait**
// l'objet, sous une commande dont le contrat est d'écrire là où l'exploitant lui
// dit d'écrire. On écrit alors à travers, sans jamais délier. Un répertoire suit
// le même chemin et rend EISDIR à l'ouverture, comme avant qu'on ne durcisse.
func openDump(output string) (*os.File, error) {
	info, err := os.Lstat(output)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil && !info.Mode().IsRegular() {
		// Sans O_CREATE : un lien pendant doit rendre son erreur, non faire écrire
		// le dump dans le fichier que le lien désigne.
		f, err := os.OpenFile(output, os.O_WRONLY, 0)
		if err != nil {
			return nil, err
		}
		// Un lien peut mener à un fichier ordinaire — « -O /dev/stdout » sous une
		// redirection. L'ouverture sans O_TRUNC y laisserait la queue d'un contenu
		// plus long ; tronquer n'a de sens que là, un périphérique ou un tube ne se
		// tronquant pas.
		if st, err := f.Stat(); err == nil && st.Mode().IsRegular() {
			if err := f.Truncate(0); err != nil {
				f.Close()
				return nil, err
			}
		}
		return f, nil
	}
	if err := os.Remove(output); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
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
			adminRoleFlag(),
			"--no-password",
			decl.Name,
		}, nil
	}
	return "", nil, fmt.Errorf("moteur %q inconnu", decl.Engine)
}

// adminRoleFlag présente pg_dump et psql sous le rôle du client d'administration
// (OGS_PG_ADMIN). Laissés au rôle par défaut — celui du compte système —, ils
// échouaient là où le client, lui, joignait la base : la suppression d'un projet
// sauvegardait sans succès une base qu'elle supprimait ensuite sans peine.
func adminRoleFlag() string {
	return "--username=" + PostgresAdminRole(os.Getenv("OGS_PG_ADMIN"))
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
		args = []string{"--host=" + socketDir, fmt.Sprintf("--port=%d", cluster.Port), adminRoleFlag(),
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
