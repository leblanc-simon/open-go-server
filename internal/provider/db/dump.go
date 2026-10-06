package db

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
	"leblanc.io/open-go-server/internal/model"
)

// Dump sauvegarde une base. Les sauvegardes planifiées sont hors périmètre : cette
// commande ponctuelle sert l'exploitation courante et l'archivage à la suppression
// d'un projet, où une archive sans données serait inexploitable.
func (p *Provider) Dump(ctx context.Context, proj *model.Project, decl model.Database, output string) (string, error) {
	return p.dump(ctx, decl, output, false)
}

// CloneData copie le contenu d'une base dans celle d'un autre projet — un
// clone —, par une sauvegarde portable écrite dans tmp : elle ne nomme aucun
// rôle de la source (dumpCommand), puis une restauration confinée au compte de
// la cible (Restore).
func (p *Provider) CloneData(ctx context.Context, source *model.Project, from model.Database,
	target *model.Project, to model.Database, tmp string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := p.dump(ctx, from, tmp, true); err != nil {
		return fmt.Errorf("sauvegarde de la source : %w", err)
	}
	if err := p.Restore(ctx, target, to, tmp); err != nil {
		return fmt.Errorf("restauration dans la cible : %w", err)
	}
	return nil
}

func (p *Provider) dump(ctx context.Context, decl model.Database, output string, portable bool) (string, error) {
	if output == "" {
		return "", fmt.Errorf("fichier de destination requis")
	}
	if err := checkIdentifier("nom de base", decl.Name); err != nil {
		return "", err
	}
	compress := strings.HasSuffix(output, ".gz")
	tool, args, err := p.dumpCommand(decl, portable)
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
	// La sortie du client va au fil de l'eau dans la destination : retenue en
	// mémoire, puis recopiée en chaîne et compressée d'un bloc, une base de
	// plusieurs gigaoctets y passait trois fois (R1-57, D122).
	out, err := createDump(output, dir, name)
	if err != nil {
		return "", err
	}
	var w io.Writer = out
	var gz *gzip.Writer
	if compress {
		gz = gzip.NewWriter(out)
		w = gz
	}
	_, err = p.Exec.Run(ctx, exec.Command{Name: tool, Args: args, Stdout: w})
	if err == nil && gz != nil {
		err = gz.Close()
	}
	if err == nil {
		err = out.commit()
	}
	if err != nil {
		out.abort()
		return "", err
	}
	return output, nil
}

// dumpFile est la destination d'une sauvegarde en cours d'écriture.
type dumpFile struct {
	*os.File
	// dir, tmp et name désignent le fichier temporaire et le nom final ; dir est
	// nul pour un flux standard, écrit directement.
	dir       *fs.Dir
	tmp, name string
	output    string
}

// createDump ouvre la destination d'une sauvegarde. Un fichier ordinaire est
// écrit sous un nom temporaire du même répertoire, puis renommé une fois la
// sauvegarde complète : écrite à sa place, une sauvegarde qui échoue en
// chemin — moteur arrêté, disque plein, interruption — aurait déjà détruit la
// précédente, que le dump tenu en mémoire laissait en place jusqu'au bout.
func createDump(output string, dir *fs.Dir, name string) (*dumpFile, error) {
	if dir == nil {
		f, err := openStream(output)
		if err != nil {
			return nil, err
		}
		return &dumpFile{File: f, output: output}, nil
	}
	for attempt := 0; ; attempt++ {
		var alea [8]byte
		if _, err := rand.Read(alea[:]); err != nil {
			return nil, err
		}
		tmp := "." + name + ".ogs-" + hex.EncodeToString(alea[:])
		// Création exclusive, sans suivre de lien (fs.Dir.Create) : le répertoire
		// peut appartenir à l'utilisateur d'un projet.
		f, err := dir.Create(tmp, 0o600, fs.KeepOwner)
		if err == nil {
			return &dumpFile{File: f, dir: dir, tmp: tmp, name: name, output: output}, nil
		}
		if !errors.Is(err, os.ErrExist) || attempt >= 3 {
			return nil, err
		}
	}
}

// commit ferme la sauvegarde et la met à sa place, une fois le nom de nouveau
// jugé (checkDumpEntry) : il a pu changer pendant la sauvegarde. Le renommage
// remplace l'entrée elle-même, jamais ce qu'un lien désignerait.
func (d *dumpFile) commit() error {
	if d.dir == nil {
		return d.File.Close()
	}
	if err := d.File.Sync(); err != nil {
		return err
	}
	if err := d.File.Close(); err != nil {
		return err
	}
	if err := checkDumpEntry(d.dir, d.name, d.output); err != nil {
		return err
	}
	return d.dir.Rename(d.tmp, d.name)
}

// abort abandonne une sauvegarde inachevée : le temporaire est retiré, la
// sauvegarde précédente reste.
func (d *dumpFile) abort() {
	_ = d.File.Close()
	if d.dir != nil {
		_ = d.dir.Remove(d.tmp)
	}
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

// openStream ouvre un flux standard désigné comme destination d'une sauvegarde
// (outputStreams), par son nom, et écrit à travers, sans jamais le délier. Il
// peut mener à un fichier ordinaire — « -O /dev/stdout » sous une redirection — :
// l'ouverture sans O_TRUNC y laisserait la queue d'un contenu plus long, et
// tronquer n'a de sens que là, un périphérique ou un tube ne se tronquant pas.
// Un fichier ordinaire passe par createDump : création exclusive en 0600 sous
// un nom temporaire — réutiliser un fichier préexistant lui laisserait son
// propriétaire et son mode —, puis renommage.
func openStream(output string) (*os.File, error) {
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

// OpenInput ouvre, en lecture, la sauvegarde qu'une restauration doit charger.
//
// Elle est lue par root, puis chargée dans une base que le projet lit : lue par
// son nom, depuis un home où l'utilisateur pose un lien à
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
//
// Côté PostgreSQL, aucune sauvegarde ne porte de propriétaire (--no-owner) :
// la restauration, confinée au rôle du projet (D92), ne peut donner un objet à
// un autre rôle, et « ALTER … OWNER TO » l'arrêterait sous ON_ERROR_STOP — le
// cas d'une base adoptée dont des tables appartiennent à un autre rôle. Les
// objets rechargés appartiennent au rôle qui restaure. Une sauvegarde portable
// (CloneData), destinée à la base d'un autre projet, ne porte pas non plus les
// privilèges, qui nomment les comptes de la source : les poser leur donnerait
// des droits sur la copie.
func (p *Provider) dumpCommand(decl model.Database, portable bool) (string, []string, error) {
	switch decl.Engine {
	case "mysql", "mariadb":
		socket, refused := findMySQLSocket(p.Paths.Prefix())
		if socket == "" {
			return "", nil, mysqlUnavailable(refused)
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
		cluster, err := p.pickCluster(decl)
		if err != nil {
			return "", nil, err
		}
		socketDir, err := cluster.SocketDir(p.Paths.Prefix())
		if err != nil {
			return "", nil, err
		}
		args := []string{
			"--host=" + socketDir,
			fmt.Sprintf("--port=%d", cluster.Port),
			adminRoleFlag(),
			"--no-password",
			"--no-owner",
		}
		if portable {
			args = append(args, "--no-privileges")
		}
		return filepath.Join(p.Paths.Prefix(), "/usr/bin/pg_dump"), append(args, decl.Name), nil
	}
	return "", nil, fmt.Errorf("moteur %q inconnu", decl.Engine)
}

// pickCluster retrouve le cluster d'une déclaration, le premier à défaut.
func (p *Provider) pickCluster(decl model.Database) (Cluster, error) {
	clusters := DetectClusters(p.Paths.Prefix())
	if len(clusters) == 0 {
		return Cluster{}, fmt.Errorf("%w : aucun cluster PostgreSQL", ErrUnavailable)
	}
	cluster := clusters[0]
	for _, c := range clusters {
		if c.ID() == decl.Cluster {
			cluster = c
		}
	}
	return cluster, nil
}

// adminRoleFlag présente pg_dump sous le rôle du client d'administration
// (OGS_PG_ADMIN). Laissé au rôle par défaut — celui du compte système —, il
// échouait là où le client, lui, joignait la base : la suppression d'un projet
// sauvegardait sans succès une base qu'elle supprimait ensuite sans peine.
func adminRoleFlag() string {
	return "--username=" + PostgresAdminRole(os.Getenv("OGS_PG_ADMIN"))
}

// Restore recharge une sauvegarde. L'opération est destructrice pour le contenu
// existant : l'appelant doit avoir obtenu confirmation.
//
// La restauration est confinée à la base du projet (D92). Le fichier est un
// script que le client du moteur exécute : chargé sous le super-utilisateur du
// moteur et sous root, il pouvait écrire dans toutes les bases du parc, créer un
// compte, lire un fichier du système — et lancer une commande shell par « \! »,
// que mysql comme psql acceptent. Il est donc chargé sous le **compte du
// projet**, avec son mot de passe scellé, par un client lancé sous
// l'**utilisateur Linux du projet** — ou, pour un compte additionnel, sous une
// identité jetable (restoreAccount) : le moteur ne lui accorde que sa base, et
// ce que le client exécute de lui-même — « \! », « \o |commande »,
// « \copy … program », LOAD DATA LOCAL — ne peut rien de plus que ce que cet
// utilisateur peut déjà, depuis son propre shell (D62). Les options des fichiers
// de son home (~/.my.cnf, ~/.psqlrc) sont ignorées : elles ne changeraient que sa
// propre session, mais rendraient la restauration imprévisible.
func (p *Provider) Restore(ctx context.Context, proj *model.Project, decl model.Database, input string) error {
	// Interrompue avant de commencer, elle ne commence pas.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkIdentifier("nom de base", decl.Name); err != nil {
		return err
	}
	osUser := proj.Spec.User.Name
	if osUser == "" || osUser == "root" {
		return fmt.Errorf("le projet %s n'a pas d'utilisateur Linux propre : la restauration ne s'exécute jamais "+
			"sous root", proj.ID())
	}
	f, err := OpenInput(input)
	if err != nil {
		return err
	}
	defer f.Close()
	account, delegated, err := restoreAccount(proj, decl)
	if err != nil {
		return err
	}
	password, err := p.Password(proj.ID(), decl.Engine, account)
	if err != nil {
		return fmt.Errorf("mot de passe de %s indisponible au keystore : la restauration s'exécute sous ce "+
			"compte, et ogs doit en connaître le mot de passe — celui d'une base adoptée lui est inconnu. "+
			"En reprendre la main : ogs db user rotate %s %s (%w)", account, proj.ID(), account, err)
	}
	cmd, err := p.restoreCommand(decl, account, password)
	if err != nil {
		return err
	}
	if _, err := os.Stat(cmd.Name); err != nil {
		return fmt.Errorf("%s est absent : installer les outils clients du moteur %s", cmd.Name, decl.Engine)
	}
	// La sauvegarde est lue au fil de l'eau, confinée ligne à ligne, et passée
	// au client sans jamais être tenue en entier en mémoire (R1-57, D122).
	var data io.Reader
	switch decl.Engine {
	case "postgres":
		data = confinePostgresStream(f)
	default:
		data = confineMySQLStream(f)
	}
	unit := ""
	if delegated {
		envFile, name, err := p.delegate(&cmd)
		if err != nil {
			return err
		}
		defer os.Remove(envFile)
		unit = name
	} else {
		cmd.AsUser = osUser
		// Le répertoire courant de root peut être fermé à l'utilisateur du projet.
		cmd.Dir = "/"
	}
	cmd.StdinStream = data
	_, err = p.Exec.Run(ctx, cmd)
	// Une interruption tue systemd-run, pas l'unité qu'il a lancée : hors du
	// groupe de processus d'ogs, le client continuait de charger la base après
	// qu'ogs avait rendu la main. L'unité est arrêtée, et attendue, avant.
	if unit != "" && ctx.Err() != nil {
		return p.stopUnit(ctx, unit)
	}
	if err != nil {
		switch {
		case scramRequired(err):
			return fmt.Errorf("%w — le cluster n'authentifie pas %s par SCRAM : son mot de passe est encore "+
				"haché en md5, d'un cluster mis à niveau, et ogs ne livre pas de mot de passe à un autre échange. "+
				"Le reposer : ogs db user rotate %s %s (il est haché selon password_encryption, "+
				"scram-sha-256 par défaut)", err, account, proj.ID(), account)
		case authRefused(err):
			return fmt.Errorf("%w — le moteur refuse le mot de passe scellé de %s : c'est le cas d'une base "+
				"adoptée, dont ogs n'a jamais posé le mot de passe. En reprendre la main : "+
				"ogs db user rotate %s %s", err, account, proj.ID(), account)
		}
		return err
	}
	return nil
}

// authRefused reconnaît le refus d'authentification des deux clients.
func authRefused(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "Access denied for user") ||
		strings.Contains(msg, "password authentication failed")
}

// scramRequired reconnaît le refus de libpq d'employer un autre échange que
// celui qu'exige PGREQUIREAUTH.
func scramRequired(err error) bool {
	return strings.Contains(err.Error(), "authentication method requirement")
}

// systemdRun lance le client d'une restauration déléguée.
const systemdRun = "systemd-run"

// delegate fait lancer le client par systemd sous une identité jetable
// (DynamicUser) : un uid alloué pour l'unité, rendu à son arrêt, qu'aucun
// compte n'atteint — l'utilisateur du projet ne peut ni lire l'environnement
// du client ni s'y attacher. systemd tue à l'arrêt tout ce que l'unité a lancé,
// « \! » compris, et DynamicUser emporte ProtectSystem=strict et PrivateTmp :
// une commande du script n'écrit nulle part où durer.
//
// Le mot de passe va dans un fichier d'environnement de root, en 0600 sous
// /run, que systemd lit en root avant de changer d'identité : sur la ligne de
// commande de systemd-run (--setenv), il serait lisible par tous. L'appelant
// retire le fichier une fois le client terminé.
func (p *Provider) delegate(cmd *exec.Command) (string, string, error) {
	// systemd-run joint systemd par le bus système : sans D-Bus, il échoue.
	for _, required := range []string{"/run/systemd/system", "/run/dbus/system_bus_socket"} {
		if _, err := os.Stat(filepath.Join(p.Paths.Prefix(), required)); err != nil {
			return "", "", fmt.Errorf("la restauration sous un compte additionnel exige systemd et son bus (%s "+
				"absent), qui lancent le client sous une identité jetable : à défaut, déclarer temporairement "+
				"« privileges: all » sur le compte principal, ogs apply, restaurer, puis revenir au jeu borné",
				required)
		}
	}
	var body strings.Builder
	for _, v := range cmd.Env {
		if strings.HasPrefix(v, "PATH=") {
			continue
		}
		// Les mots de passe d'ogs n'emploient que des lettres, des chiffres et
		// « -.+= » ; un autre caractère changerait la lecture du fichier.
		if strings.ContainsAny(v, " \t\n\r'\"\\$`#;") {
			return "", "", fmt.Errorf("mot de passe impropre à un fichier d'environnement : le reposer par ogs db " +
				"user rotate")
		}
		body.WriteString(v + "\n")
	}
	f, err := os.CreateTemp(filepath.Join(p.Paths.Prefix(), "/run"), "ogs-restauration-*.env")
	if err != nil {
		return "", "", err
	}
	name := f.Name()
	if _, err := f.WriteString(body.String()); err != nil {
		f.Close()
		os.Remove(name)
		return "", "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", "", err
	}
	var alea [8]byte
	if _, err := rand.Read(alea[:]); err != nil {
		os.Remove(name)
		return "", "", err
	}
	// L'unité est nommée pour pouvoir l'arrêter (stopUnit).
	unit := "ogs-restauration-" + hex.EncodeToString(alea[:]) + ".service"
	args := []string{"--quiet", "--pipe", "--wait", "--collect", "--service-type=exec", "--unit=" + unit,
		"--property=DynamicUser=yes", "--property=EnvironmentFile=" + name,
		"--property=WorkingDirectory=/", "--", cmd.Name}
	*cmd = exec.Command{Name: systemdRun, Args: append(args, cmd.Args...)}
	return name, unit, nil
}

// stopUnit arrête l'unité d'une restauration interrompue et attend son arrêt —
// « systemctl stop » ne rend la main qu'unité arrêtée, tous ses processus
// tués —, sous un contexte détaché de celui qu'on vient d'annuler. L'erreur
// rendue porte l'annulation, que l'appelant traduit en code 9.
func (p *Provider) stopUnit(ctx context.Context, unit string) error {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	// Une unité introuvable est déjà arrêtée : le client a fini au moment même de
	// l'interruption, et --collect l'a retirée.
	if _, err := p.Exec.Run(stopCtx, exec.Command{Name: "systemctl", Args: []string{"stop", unit}}); err != nil &&
		!strings.Contains(err.Error(), "not loaded") {
		return fmt.Errorf("restauration interrompue, et l'arrêt de %s a échoué — la base peut encore être en "+
			"cours de chargement : %w (%w)", unit, ctx.Err(), err)
	}
	return fmt.Errorf("restauration interrompue : %s arrêtée, la base est partiellement chargée : %w",
		unit, ctx.Err())
}

// restoreAccount rend le compte sous lequel charger une sauvegarde, et s'il
// faut déléguer le client à une identité tierce. Le compte doit avoir le jeu
// « all » : un jeu borné ne peut ni supprimer ni recréer une table, et une
// restauration qui échoue à mi-chemin laisse la base à moitié chargée.
//
// Le compte principal d'abord, lancé sous l'utilisateur Linux du projet, qui
// en détient déjà le mot de passe. Côté PostgreSQL, il est propriétaire de la
// base (§7.3) : c'est toujours lui. Un principal borné — le durcissement
// « crud, et un compte additionnel all pour les migrations » — fait passer au
// premier compte additionnel « all », mais jamais sous l'utilisateur du
// projet : il lirait dans l'environnement du client le mot de passe qu'il
// porte, ou s'y attacherait, et un projet « readonly » gagnerait l'écriture.
// Le client est alors délégué (delegate).
func restoreAccount(proj *model.Project, decl model.Database) (string, bool, error) {
	if decl.Engine == "postgres" {
		return decl.User, false, nil
	}
	if priv, err := ParsePrivilege(decl.Privileges); err == nil && priv == PrivAll {
		return decl.User, false, nil
	}
	for _, u := range decl.ExtraUsers {
		if priv, err := ParsePrivilege(u.Privileges); err == nil && priv == PrivAll {
			return u.User, true, nil
		}
	}
	return "", false, fmt.Errorf("aucun compte de la base %s n'a le jeu de privilèges « all » : la restauration "+
		"ne pourrait pas recréer les tables. Déclarer un compte additionnel « all » (ogs db user add %s <nom> "+
		"--privileges all, puis ogs apply %s), ou temporairement « privileges: all » sur le compte principal",
		decl.Name, proj.ID(), proj.ID())
}

// restorePath est le PATH donné au client : son environnement est construit de
// toutes pièces, pour ne transmettre à l'utilisateur du projet aucune variable
// de root.
const restorePath = "PATH=/usr/local/bin:/usr/bin:/bin"

// restoreCommand construit la commande de restauration, sans son entrée. Le mot
// de passe passe par l'environnement du client, jamais par ses arguments : la
// ligne de commande d'un processus est lisible par tous, son environnement par
// son seul propriétaire — ici l'utilisateur du projet, à qui il appartient déjà.
//
// Le mot de passe n'est présenté qu'au vrai moteur (D92). Il faut d'abord un
// socket admis (checkSocket) : un moteur arrêté n'en a pas, et son port TCP est
// alors libre pour quiconque. Une connexion TCP exige en outre que toute
// écoute qui peut la recevoir appartienne au moteur (checkTCPListener) — ce
// que le mode simulé ne vérifie pas, n'ouvrant aucune connexion.
func (p *Provider) restoreCommand(decl model.Database, account, password string) (exec.Command, error) {
	if err := checkIdentifier("nom d'utilisateur", account); err != nil {
		return exec.Command{}, err
	}
	switch decl.Engine {
	case "mysql", "mariadb":
		socket, refused := findMySQLSocket(p.Paths.Prefix())
		if socket == "" {
			return exec.Command{}, mysqlUnavailable(refused)
		}
		// --no-defaults doit venir en tête.
		args := []string{"--no-defaults"}
		switch decl.Host {
		case "127.0.0.1", "::1":
			// Un compte déclaré sous une adresse n'est pas celui que MySQL retient
			// pour une connexion par le socket, qui arrive de « localhost ».
			if err := p.checkListener(mysqlPort, mysqlSystemUser, decl.Host); err != nil {
				return exec.Command{}, err
			}
			args = append(args, "--protocol=TCP", "--host="+decl.Host, fmt.Sprintf("--port=%d", mysqlPort))
		default:
			// Le socket admis, directement : il est ouvert à tous (0777), et le
			// mot de passe y authentifie un compte « localhost » sous n'importe
			// quelle identité.
			args = append(args, "--socket="+socket)
		}
		args = append(args, "--user="+account, "--default-character-set=utf8mb4", decl.Name)
		return exec.Command{Name: filepath.Join(p.Paths.Prefix(), "/usr/bin/mysql"), Args: args,
			Env: []string{restorePath, "MYSQL_PWD=" + password}}, nil
	case "postgres":
		cluster, err := p.pickCluster(decl)
		if err != nil {
			return exec.Command{}, err
		}
		if cluster.Socket == "" {
			if _, err := cluster.SocketDir(p.Paths.Prefix()); err != nil {
				return exec.Command{}, err
			}
			return exec.Command{}, fmt.Errorf("%w : cluster %s sans socket — est-il démarré ? ogs ne présente "+
				"pas de mot de passe à un port que rien ne garantit être le sien", ErrUnavailable, cluster.ID())
		}
		if err := p.checkListener(cluster.Port, postgresSystemUser, "127.0.0.1"); err != nil {
			return exec.Command{}, err
		}
		// Par TCP et mot de passe, comme l'application (DSN) : l'authentification
		// « peer » du socket exigerait un rôle au nom de l'utilisateur Linux, ce
		// qu'un projet adopté ou un compte additionnel ne garantit pas.
		// PGREQUIREAUTH (libpq 16 et plus) refuse de livrer le mot de passe à un
		// serveur qui ne mène pas un échange SCRAM : un serveur qui le demande en
		// clair n'obtient rien. Une libpq plus ancienne ignore la variable, et
		// seul le contrôle de l'écoute protège alors.
		return exec.Command{Name: filepath.Join(p.Paths.Prefix(), "/usr/bin/psql"), Args: []string{
			"--no-psqlrc", "--host=127.0.0.1", fmt.Sprintf("--port=%d", cluster.Port),
			"--username=" + account, "--no-password", "--set=ON_ERROR_STOP=1", "--dbname=" + decl.Name,
		}, Env: []string{restorePath, "PGPASSWORD=" + password, "PGREQUIREAUTH=scram-sha-256"}}, nil
	}
	return exec.Command{}, fmt.Errorf("moteur %q inconnu", decl.Engine)
}

// mysqlPort est le port TCP du serveur MySQL, celui du DSN applicatif.
const mysqlPort = 3306

// listenerCheck remplace, s'il est posé, le contrôle de l'écoute TCP : les
// tests y branchent le leur, le mode simulé n'en faisant aucun.
var listenerCheck func(port int, systemUser, target string) error

// checkListener applique checkTCPListener, sauf en mode simulé — qui n'ouvre
// aucune connexion.
func (p *Provider) checkListener(port int, systemUser, target string) error {
	if listenerCheck != nil {
		return listenerCheck(port, systemUser, target)
	}
	if p.Exec.Simulated() {
		return nil
	}
	return checkTCPListener(port, systemUser, target)
}

// mysqlDDLDefiner repère la clause DEFINER que mysqldump et mariadb-dump posent
// en tête d'une instruction de définition : une routine (« CREATE DEFINER=… »),
// une vue (« /*!50013 DEFINER=… »), un déclencheur ou un événement
// (« /*!50003 CREATE*/ /*!50017 DEFINER=…*/ »), une routine d'un ancien client
// (« /*!50020 DEFINER=…*/ »). Elle n'est cherchée qu'à cette place, au début
// de la ligne : ailleurs, elle peut être une donnée.
var mysqlDDLDefiner = regexp.MustCompile("^((?:CREATE |/\\*!\\d{5} CREATE\\*/ )?(?:/\\*!\\d{5} )?)" +
	"DEFINER=`(?:[^`]|``)*`@`(?:[^`]|``)*`")

// confineMySQLDump retire les clauses DEFINER d'une sauvegarde MySQL : l'objet
// prend alors pour définisseur le compte qui restaure. Nommer un autre compte
// exige SET_USER_ID ou SUPER, qu'un compte de projet n'a pas — et c'est le cas
// de tout clone, dont les objets nomment le compte de la source. Chargées sous
// root, ces clauses passaient, et les vues de la copie s'exécutaient sous le
// compte de la source, sans droit sur la base cible.
//
// Seule la tête d'une ligne de définition est touchée (mysqlDDLDefiner) : une
// ligne de données ne commence jamais ainsi — mariadb-dump écrit ses tuples sur
// des lignes qui commencent par « ( », mysqldump sur celle de l'INSERT, et les
// fins de ligne des valeurs sont échappées.
func confineMySQLDump(data []byte) []byte {
	return confineAll(confineMySQLStream(bytes.NewReader(data)))
}

// confineMySQLStream est confineMySQLDump au fil de l'eau.
func confineMySQLStream(r io.Reader) io.Reader {
	return newLineFilter(r, func(head []byte, whole bool) []byte {
		if !bytes.Contains(head, []byte("DEFINER=`")) {
			return head
		}
		loc := mysqlDDLDefiner.FindSubmatchIndex(head)
		// Sur une tête tronquée (ligne plus longue que le tampon), une clause qui
		// toucherait la coupure n'est pas une clause entière : la ligne passe
		// telle quelle. Les identifiants d'une clause DEFINER, bornés par le
		// moteur, tiennent toujours loin en deçà de lineHead.
		if loc == nil || (!whole && loc[1] == len(head)) {
			return head
		}
		// Le motif est ancré : une seule clause, remplacée par son groupe 1. La
		// tête désigne le tampon du lecteur, d'où une copie.
		out := make([]byte, 0, len(head)-(loc[1]-loc[3]))
		out = append(out, head[:loc[3]]...)
		return append(out, head[loc[1]:]...)
	})
}

// pgExtensionComment est l'instruction complète que pg_dump écrit pour le
// commentaire d'une extension.
var pgExtensionComment = regexp.MustCompile(`^COMMENT ON EXTENSION ("[^"]+"|[a-z0-9_]+) IS '(?:[^']|'')*';\r?\n?$`)

// confinePostgresDump retire les commentaires d'extension d'une sauvegarde
// PostgreSQL. pg_dump en écrit un par extension, et COMMENT ON EXTENSION exige
// d'en être propriétaire — or ogs crée les extensions déclarées sous le rôle
// d'administration : sous ON_ERROR_STOP, la restauration confinée s'arrêtait
// là. Le commentaire est celui du paquet de l'extension, rien du projet ne s'y
// perd.
//
// N'est retirée que l'instruction complète qui suit l'en-tête que pg_dump pose
// devant elle (« -- Name: EXTENSION x; Type: COMMENT; … ») : la même ligne dans
// le corps d'une fonction, entre $$, ou dans un bloc de données COPY, reste.
func confinePostgresDump(data []byte) []byte {
	return confineAll(confinePostgresStream(bytes.NewReader(data)))
}

// confinePostgresStream est confinePostgresDump au fil de l'eau.
func confinePostgresStream(r io.Reader) io.Reader {
	header := false
	return newLineFilter(r, func(head []byte, whole bool) []byte {
		trimmed := bytes.TrimRight(head, "\r\n")
		switch {
		case bytes.HasPrefix(trimmed, []byte("-- Name: EXTENSION ")) &&
			bytes.Contains(trimmed, []byte("; Type: COMMENT;")):
			header = true
		case header && (len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte("--"))):
			// Les lignes de commentaire et le blanc entre l'en-tête et l'instruction.
		case header:
			header = false
			// Une ligne plus longue que le tampon n'est jamais ce commentaire.
			if whole && pgExtensionComment.Match(head) {
				return nil
			}
		}
		return head
	})
}

// confineAll lit tout un flux confiné : les tests et les appelants qui tiennent
// déjà la sauvegarde en mémoire.
func confineAll(r io.Reader) []byte {
	data, _ := io.ReadAll(r)
	return data
}

// lineHead est la longueur au-delà de laquelle une ligne n'est plus examinée :
// ce qui la suit passe tel quel. Une ligne de données de mysqldump — un INSERT
// étendu — peut peser des mégaoctets, et les deux filtres ne jugent que la tête
// d'une ligne : une clause DEFINER, aux identifiants bornés par le moteur, ou
// une ligne de commentaire.
const lineHead = 64 << 10

// lineFilter applique un filtre à la tête de chaque ligne d'un flux.
type lineFilter struct {
	src *bufio.Reader
	// filter reçoit la tête d'une ligne — la ligne entière, fin de ligne
	// comprise, si whole — et rend ce qui la remplace, nil pour la retirer.
	filter  func(head []byte, whole bool) []byte
	pending []byte
	// inLine dit que la tête de la ligne courante est passée : la suite est
	// recopiée telle quelle.
	inLine bool
	err    error
}

func newLineFilter(r io.Reader, filter func(head []byte, whole bool) []byte) *lineFilter {
	return &lineFilter{src: bufio.NewReaderSize(r, lineHead), filter: filter}
}

func (l *lineFilter) Read(p []byte) (int, error) {
	for len(l.pending) == 0 {
		if l.err != nil {
			return 0, l.err
		}
		chunk, err := l.src.ReadSlice('\n')
		full := errors.Is(err, bufio.ErrBufferFull)
		switch {
		case full:
			err = nil
		case err != nil && len(chunk) == 0:
			l.err = err
			continue
		}
		if l.inLine {
			l.pending = chunk
		} else {
			// Une ligne finale sans fin de ligne est entière, elle aussi.
			l.pending = l.filter(chunk, !full)
		}
		l.inLine = full
		l.err = err
	}
	// pending désigne, au plus, le tampon du lecteur : il est vidé avant toute
	// nouvelle lecture.
	n := copy(p, l.pending)
	l.pending = l.pending[n:]
	return n, nil
}
