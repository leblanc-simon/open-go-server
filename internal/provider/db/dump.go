package db

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
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
	// La destination est résolue avant la sauvegarde : un chemin refusé l'est sans
	// avoir fait tourner le dump d'une base entière pour rien.
	dir, name, err := dumpDir(output)
	if err != nil {
		return "", err
	}
	if dir != nil {
		defer dir.Close()
	}
	res, err := p.Exec.Run(ctx, exec.Command{Name: tool, Args: args})
	if err != nil {
		return "", err
	}
	f, err := openDump(output, dir, name)
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

// outputStreams sont les destinations qu'ogs ouvre par leur nom, sans parcours :
// les flux standard et /dev/null — et stdinPath la seule source. /dev/stdout est
// un lien vers /proc/self/fd/1, lui-même un lien « magique » vers un descripteur
// du processus, que seul le noyau sait suivre : le parcours d'OpenParentDir n'y
// mènerait nulle part. Aucun compte de projet ne contrôle ces noms.
var outputStreams = map[string]bool{"/dev/stdout": true, "/dev/stderr": true, "/dev/null": true}

const stdinPath = "/dev/stdin"

// dumpDir ouvre le répertoire de la destination d'une sauvegarde, créé au
// besoin, et contrôle ce qui occupe déjà son nom. Il rend un répertoire nul pour
// un flux standard (outputStreams).
//
// La destination est souvent le home d'un projet, à la demande de son
// utilisateur, et l'utilisateur possède ce home (D51) : il y pose un lien à la
// place du fichier, ou d'un répertoire du chemin, vers le fichier de root de son
// choix. Ouverte par son nom, la destination faisait tronquer puis réécrire par
// root la cible du lien — et le contenu d'un dump, les données de sa base, lui
// est en partie dicté. Le chemin est donc parcouru sans suivre un lien qu'un
// autre compte que root aurait pu poser (fs.OpenParentDir), et seul un fichier
// ordinaire, ou rien, peut occuper le nom.
func dumpDir(output string) (*fs.Dir, string, error) {
	if outputStreams[output] {
		return nil, "", nil
	}
	dir, name, err := fs.OpenParentDir(output, true, 0o750)
	if err != nil {
		return nil, "", err
	}
	if err := checkDumpEntry(dir, name, output); err != nil {
		dir.Close()
		return nil, "", err
	}
	return dir, name, nil
}

// checkDumpEntry refuse ce qui, à la place de la destination, n'est pas un
// fichier ordinaire. Un lien, d'abord : il n'est jamais suivi, et jamais délié —
// ce n'est pas la sauvegarde que l'on remplacerait. Un tube nommé ou un
// périphérique ensuite : le retrait ne remettrait pas les droits à zéro, il
// détruirait l'objet, et écrire à travers livrerait les données à qui l'a posé.
// Un répertoire enfin, qu'une destination mal visée — « -O /var/backups/dumps »
// — ne doit pas supprimer.
func checkDumpEntry(dir *fs.Dir, name, output string) error {
	info, err := dir.Lstat(name)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	case info.Mode().IsRegular():
		return nil
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s est un lien symbolique, posé dans un répertoire qu'un autre compte que root peut "+
			"modifier : ogs n'écrit pas une sauvegarde à travers lui — désigner le chemin réel", output)
	case info.IsDir():
		return fmt.Errorf("%s est un répertoire : désigner le fichier de la sauvegarde", output)
	}
	return fmt.Errorf("%s n'est pas un fichier ordinaire (tube nommé, socket ou périphérique) : ogs n'y écrit "+
		"pas de sauvegarde", output)
}

// openDump ouvre la destination d'une sauvegarde, avec des permissions
// restrictives : un dump contient l'intégralité des données de l'application.
//
// Sur un fichier ordinaire, le retrait préalable et la création exclusive
// interdisent d'écrire dans un inode qu'on n'a pas créé soi-même : réutiliser un
// fichier préexistant lui laisserait son propriétaire et son mode, le 0600
// annoncé ici ne s'appliquant qu'à la création — et une course sur le retrait
// échoue franchement au lieu de livrer les données dans l'inode d'un autre. Le
// retrait et la création passent par le descripteur du répertoire, sans suivre
// de lien : O_EXCL refuse un lien reposé entre les deux (fs.Dir.Create).
//
// Un flux standard est ouvert par son nom et écrit à travers, sans jamais être
// délié. Il peut mener à un fichier ordinaire — « -O /dev/stdout » sous une
// redirection — : l'ouverture sans O_TRUNC y laisserait la queue d'un contenu
// plus long, et tronquer n'a de sens que là, un périphérique ou un tube ne se
// tronquant pas.
func openDump(output string, dir *fs.Dir, name string) (*os.File, error) {
	if dir == nil {
		f, err := os.OpenFile(output, os.O_WRONLY, 0)
		if err != nil {
			return nil, err
		}
		if st, err := f.Stat(); err == nil && st.Mode().IsRegular() {
			if err := f.Truncate(0); err != nil {
				f.Close()
				return nil, err
			}
		}
		return f, nil
	}
	// Le nom a pu changer pendant la sauvegarde : il est jugé de nouveau.
	if err := checkDumpEntry(dir, name, output); err != nil {
		return nil, err
	}
	if err := dir.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return dir.Create(name, 0o600, fs.KeepOwner)
}

// OpenInput ouvre, en lecture, la sauvegarde qu'une restauration doit charger.
//
// Elle est chargée sous le super-utilisateur du moteur, dans une base que le
// projet lit : lue par son nom, depuis un home où l'utilisateur pose un lien à
// sa place, elle livrait au projet tout fichier que root peut lire — la
// sauvegarde d'un voisin sous /var/lib/ogs/dumps, dont le nom se devine à la
// seconde près. Le chemin est donc parcouru sans suivre un lien qu'un autre
// compte que root aurait pu poser (fs.OpenParentDir), et seul un fichier
// ordinaire est ouvert, constaté sur son descripteur : ni lien, ni tube nommé
// — qui bloquerait la restauration —, ni périphérique. /dev/stdin est ouvert
// par son nom, comme les flux de sortie (outputStreams).
func OpenInput(input string) (*os.File, error) {
	if input == stdinPath {
		return os.Open(input)
	}
	dir, name, err := fs.OpenParentDir(input, false, 0)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	f, err := dir.OpenFile(name)
	if errors.Is(err, fs.ErrNotRegular) {
		return nil, fmt.Errorf("%s n'est pas un fichier ordinaire, ou c'est un lien posé dans un répertoire qu'un "+
			"autre compte que root peut modifier : ogs ne le restaure pas — désigner le fichier réel", input)
	}
	return f, err
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
	f, err := OpenInput(input)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("lecture de %s : %w", input, err)
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
