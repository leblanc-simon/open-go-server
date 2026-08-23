package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/leviathan/ogs/internal/config"
	"github.com/leviathan/ogs/internal/engine"
	"github.com/leviathan/ogs/internal/exec"
	"github.com/leviathan/ogs/internal/model"
	"github.com/leviathan/ogs/internal/naming"
	"github.com/leviathan/ogs/internal/secret"
	"github.com/leviathan/ogs/internal/store"
)

// Provider réconcilie les bases de données d'un projet.
type Provider struct {
	Paths config.Paths
	Exec  exec.Executor
	Store *store.Store
	Keys  *secret.Keystore

	// AMQPHost et AMQPPort servent à composer le transport applicatif.
	AMQPHost string
	AMQPPort int

	// Clients est le cache des connexions ouvertes, par moteur ou cluster.
	clients map[string]Client
}

// Name identifie le provider.
func (p *Provider) Name() string { return "db" }

// Close referme les connexions ouvertes.
func (p *Provider) Close() {
	for _, c := range p.clients {
		_ = c.Close()
	}
	p.clients = nil
}

// clientFor ouvre — ou retrouve — le client d'une déclaration de base.
func (p *Provider) clientFor(db model.Database) (Client, error) {
	key := db.Engine
	if db.Engine == "postgres" && db.Cluster != "" {
		key = "postgres:" + db.Cluster
	}
	if c, ok := p.clients[key]; ok {
		return c, nil
	}
	var (
		c   Client
		err error
	)
	switch db.Engine {
	case "mysql", "mariadb":
		socket, ok := FindMySQLSocket(p.Paths.Prefix())
		if !ok {
			return nil, fmt.Errorf("%w : aucun socket MySQL trouvé (%s) — le serveur est-il installé et démarré ?",
				ErrUnavailable, strings.Join(mysqlSockets, ", "))
		}
		c, err = NewMySQLClient(socket)
	case "postgres":
		clusters := DetectClusters(p.Paths.Prefix())
		if len(clusters) == 0 {
			return nil, fmt.Errorf("%w : aucun cluster PostgreSQL détecté sous %s",
				ErrUnavailable, filepath.Join(p.Paths.Prefix(), "/etc/postgresql"))
		}
		cluster := clusters[0]
		if db.Cluster != "" {
			found := false
			for _, cl := range clusters {
				if cl.ID() == db.Cluster {
					cluster, found = cl, true
				}
			}
			if !found {
				return nil, fmt.Errorf("cluster PostgreSQL %q inconnu (détectés : %s)",
					db.Cluster, describeClusters(clusters))
			}
		}
		c, err = NewPostgresClient(cluster, os.Getenv("OGS_PG_ADMIN"))
	default:
		return nil, fmt.Errorf("moteur %q inconnu", db.Engine)
	}
	if err != nil {
		return nil, err
	}
	if p.clients == nil {
		p.clients = map[string]Client{}
	}
	p.clients[key] = c
	return c, nil
}

func describeClusters(clusters []Cluster) string {
	out := make([]string, len(clusters))
	for i, c := range clusters {
		out[i] = c.ID()
	}
	return strings.Join(out, ", ")
}

// DBState est l'état observé d'une déclaration de base.
type DBState struct {
	Declaration model.Database
	Endpoint    Endpoint
	DBExists    bool
	UserExists  bool
	Grants      map[string]string
	Extensions  map[string]bool
	ExtraUsers  map[string]bool
	// Unavailable porte le motif si le moteur n'est pas joignable.
	Unavailable error
}

// Observed regroupe l'état de toutes les bases d'un projet.
type Observed struct {
	States []DBState
}

// Observe inspecte les moteurs déclarés par le projet.
func (p *Provider) Observe(ctx context.Context, proj *model.Project) (Observed, error) {
	var obs Observed
	for _, decl := range proj.Spec.Databases {
		state := DBState{Declaration: decl, Grants: map[string]string{},
			Extensions: map[string]bool{}, ExtraUsers: map[string]bool{}}
		client, err := p.clientFor(decl)
		if err != nil {
			state.Unavailable = err
			obs.States = append(obs.States, state)
			continue
		}
		endpoint, err := client.Endpoint(ctx)
		if err != nil {
			state.Unavailable = err
			obs.States = append(obs.States, state)
			continue
		}
		state.Endpoint = endpoint

		databases, err := client.Databases(ctx)
		if err != nil {
			return obs, err
		}
		state.DBExists = databases[decl.Name]

		users, err := client.Users(ctx)
		if err != nil {
			return obs, err
		}
		host := decl.Host
		if host == "" {
			host = "localhost"
		}
		if decl.Engine == "postgres" {
			state.UserExists = users[decl.User]
			for _, u := range decl.ExtraUsers {
				state.ExtraUsers[u.User] = users[u.User]
			}
		} else {
			state.UserExists = users[decl.User+"@"+host]
			for _, u := range decl.ExtraUsers {
				state.ExtraUsers[u.User] = users[u.User+"@"+host]
			}
		}

		if state.UserExists {
			if grants, err := client.Grants(ctx, decl.User, host); err == nil {
				state.Grants = grants
			}
		}
		if state.DBExists && decl.Engine == "postgres" && len(decl.Extensions) > 0 {
			if exts, err := client.Extensions(ctx, decl.Name); err == nil {
				state.Extensions = exts
			}
		}
		obs.States = append(obs.States, state)
	}
	return obs, nil
}

// Plan calcule les étapes relatives aux bases. Un moteur injoignable produit une
// étape non critique : un site dont la base est indisponible reste déployable, et
// l'échec est explicite plutôt que fatal.
func (p *Provider) Plan(ctx context.Context, proj *model.Project, obs Observed) ([]*engine.Step, error) {
	var steps []*engine.Step
	for _, state := range obs.States {
		decl := state.Declaration
		resource := fmt.Sprintf("db/%s/%s", decl.Engine, decl.Name)

		if state.Unavailable != nil {
			s := engine.NewStep(resource, engine.Create,
				fmt.Sprintf("base %s %s — moteur indisponible", decl.Engine, decl.Name))
			s.Critical = false
			reason := state.Unavailable
			s.Apply = func(context.Context) error { return reason }
			steps = append(steps, s)
			continue
		}

		priv, err := ParsePrivilege(decl.Privileges)
		if err != nil {
			return nil, err
		}
		host := decl.Host
		if host == "" {
			host = "localhost"
		}
		grantKey := decl.Name
		hasGrant := state.Grants[grantKey] != ""
		conforming := state.DBExists && state.UserExists && hasGrant &&
			extensionsSatisfied(decl, state) && extraUsersSatisfied(state)

		if conforming {
			steps = append(steps, engine.NewStep(resource, engine.NoOp,
				fmt.Sprintf("base %s %s (utilisateur %s)", decl.Engine, decl.Name, decl.User)))
			continue
		}

		action := engine.Update
		if !state.DBExists {
			action = engine.Create
		}
		summary := fmt.Sprintf("base %s %s (utilisateur %s, privilèges %s)",
			decl.Engine, decl.Name, decl.User, priv)
		s := engine.NewStep(resource, action, summary)
		declCopy, stateCopy := decl, state
		s.Diff = func() (string, error) { return p.describe(declCopy, stateCopy, priv, host) }
		s.Apply = func(ctx context.Context) error {
			return p.apply(ctx, proj, declCopy, stateCopy, priv, host)
		}
		s.Compensate = func(ctx context.Context) error {
			// La compensation ne détruit une base que si elle vient d'être créée :
			// jamais de suppression de données préexistantes dans un rollback.
			if stateCopy.DBExists {
				return nil
			}
			return p.drop(ctx, declCopy, stateCopy)
		}
		steps = append(steps, s)
	}
	return steps, nil
}

func extensionsSatisfied(decl model.Database, state DBState) bool {
	for _, ext := range decl.Extensions {
		if !state.Extensions[ext] {
			return false
		}
	}
	return true
}

func extraUsersSatisfied(state DBState) bool {
	for _, present := range state.ExtraUsers {
		if !present {
			return false
		}
	}
	return true
}

// describe rend le détail des requêtes qui seront exécutées, pour que le plan
// soit vérifiable avant application.
func (p *Provider) describe(decl model.Database, state DBState, priv Privilege, host string) (string, error) {
	stmts, err := p.statements(decl, state, priv, host, "«mot de passe généré»")
	if err != nil {
		return "", err
	}
	return strings.Join(stmts, "\n"), nil
}

// statements produit la liste des requêtes à exécuter pour une déclaration.
func (p *Provider) statements(decl model.Database, state DBState, priv Privilege, host, password string) ([]string, error) {
	switch decl.Engine {
	case "mysql", "mariadb":
		spec := MySQLSpec{
			Database: decl.Name, Charset: decl.Charset, Collation: decl.Collation,
			User: decl.User, Host: host, Password: password, Privileges: priv,
			AuthPlugin: decl.AuthPlugin,
		}
		stmts, err := CreateMySQL(spec)
		if err != nil {
			return nil, err
		}
		out := stmts.All()
		for _, u := range decl.ExtraUsers {
			extraPriv, err := ParsePrivilege(u.Privileges)
			if err != nil {
				return nil, err
			}
			extra, err := CreateMySQL(MySQLSpec{
				Database: decl.Name, Charset: decl.Charset, Collation: decl.Collation,
				User: u.User, Host: host, Password: password, Privileges: extraPriv,
			})
			if err != nil {
				return nil, err
			}
			out = append(out, extra.User...)
			out = append(out, extra.Grants...)
		}
		return out, nil
	case "postgres":
		stmts, err := CreatePostgres(PostgresSpec{
			Database: decl.Name, Role: decl.User, Password: password,
			Extensions: decl.Extensions, Privileges: priv,
		}, state.UserExists, state.DBExists)
		if err != nil {
			return nil, err
		}
		return append(stmts.Cluster, stmts.InDatabase...), nil
	}
	return nil, fmt.Errorf("moteur %q inconnu", decl.Engine)
}

// apply crée la base, son utilisateur et ses privilèges. Le mot de passe est
// généré puis scellé dans le keystore, ce qui permet de le restituer plus tard
// pour reconstruire un fichier d'environnement (§8).
func (p *Provider) apply(ctx context.Context, proj *model.Project, decl model.Database,
	state DBState, priv Privilege, host string) error {
	client, err := p.clientFor(decl)
	if err != nil {
		return err
	}
	password, err := p.ensurePassword(proj.ID(), decl.Engine, decl.User)
	if err != nil {
		return err
	}

	switch decl.Engine {
	case "mysql", "mariadb":
		spec := MySQLSpec{
			Database: decl.Name, Charset: decl.Charset, Collation: decl.Collation,
			User: decl.User, Host: host, Password: password, Privileges: priv,
			AuthPlugin: decl.AuthPlugin,
		}
		stmts, err := CreateMySQL(spec)
		if err != nil {
			return err
		}
		for _, stmt := range stmts.All() {
			if err := client.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		for _, u := range decl.ExtraUsers {
			extraPriv, err := ParsePrivilege(u.Privileges)
			if err != nil {
				return err
			}
			extraPassword, err := p.ensurePassword(proj.ID(), decl.Engine, u.User)
			if err != nil {
				return err
			}
			extra, err := CreateMySQL(MySQLSpec{
				Database: decl.Name, User: u.User, Host: host,
				Password: extraPassword, Privileges: extraPriv,
			})
			if err != nil {
				return err
			}
			for _, stmt := range append(extra.User, extra.Grants...) {
				if err := client.Exec(ctx, stmt); err != nil {
					return err
				}
			}
		}
	case "postgres":
		stmts, err := CreatePostgres(PostgresSpec{
			Database: decl.Name, Role: decl.User, Password: password,
			Extensions: decl.Extensions, Privileges: priv,
		}, state.UserExists, state.DBExists)
		if err != nil {
			return err
		}
		for _, stmt := range stmts.Cluster {
			if err := client.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		for _, stmt := range stmts.InDatabase {
			if err := client.ExecIn(ctx, decl.Name, stmt); err != nil {
				return err
			}
		}
	}
	return nil
}

// Drop supprime une base et son utilisateur. Opération destructrice : l'appelant
// doit avoir obtenu confirmation.
func (p *Provider) Drop(ctx context.Context, decl model.Database, state DBState) error {
	return p.drop(ctx, decl, state)
}

// drop supprime une base et son utilisateur.
func (p *Provider) drop(ctx context.Context, decl model.Database, state DBState) error {
	client, err := p.clientFor(decl)
	if err != nil {
		return err
	}
	host := decl.Host
	if host == "" {
		host = "localhost"
	}
	var stmts []string
	switch decl.Engine {
	case "mysql", "mariadb":
		stmts, err = DropMySQL(decl.Name, decl.User, host)
	case "postgres":
		force := false
		if pg, ok := client.(*pgClient); ok {
			force = pg.SupportsForceDrop()
		}
		stmts, err = DropPostgres(decl.Name, decl.User, force)
	}
	if err != nil {
		return err
	}
	for _, stmt := range stmts {
		if err := client.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// secretName localise un secret dans le keystore.
func secretName(engine, user string) string { return engine + "/" + user }

// ensurePassword renvoie le mot de passe du compte, en le générant et le
// scellant s'il n'existe pas encore. Un mot de passe déjà émis n'est jamais
// régénéré : cela invaliderait les configurations applicatives en place.
func (p *Provider) ensurePassword(project, engine, user string) (string, error) {
	if p.Store == nil || p.Keys == nil {
		return "", fmt.Errorf("keystore indisponible : impossible de gérer le mot de passe de %s", user)
	}
	name := secretName(engine, user)
	nonce, ciphertext, err := p.Store.GetSecret(project, name)
	if err == nil {
		plain, err := p.Keys.Open(project, name, nonce, ciphertext)
		if err != nil {
			return "", err
		}
		return string(plain), nil
	}
	password, err := secret.Password(0)
	if err != nil {
		return "", err
	}
	nonce, ciphertext, err = p.Keys.Seal(project, name, []byte(password))
	if err != nil {
		return "", err
	}
	if err := p.Store.PutSecret(project, name, nonce, ciphertext); err != nil {
		return "", err
	}
	return password, nil
}

// Password lit le mot de passe scellé d'un compte.
func (p *Provider) Password(project, engine, user string) (string, error) {
	name := secretName(engine, user)
	nonce, ciphertext, err := p.Store.GetSecret(project, name)
	if err != nil {
		return "", err
	}
	plain, err := p.Keys.Open(project, name, nonce, ciphertext)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// RotatePassword régénère le mot de passe d'un compte et l'applique au moteur.
func (p *Provider) RotatePassword(ctx context.Context, proj *model.Project, decl model.Database, user string) (string, error) {
	client, err := p.clientFor(decl)
	if err != nil {
		return "", err
	}
	password, err := secret.Password(0)
	if err != nil {
		return "", err
	}
	host := decl.Host
	if host == "" {
		host = "localhost"
	}
	var stmt string
	switch decl.Engine {
	case "mysql", "mariadb":
		stmt, err = SetMySQLPassword(user, host, password, decl.AuthPlugin)
	case "postgres":
		stmt, err = SetPostgresPassword(user, password)
	default:
		return "", fmt.Errorf("moteur %q inconnu", decl.Engine)
	}
	if err != nil {
		return "", err
	}
	if err := client.Exec(ctx, stmt); err != nil {
		return "", err
	}
	name := secretName(decl.Engine, user)
	nonce, ciphertext, err := p.Keys.Seal(proj.ID(), name, []byte(password))
	if err != nil {
		return "", err
	}
	if err := p.Store.PutSecret(proj.ID(), name, nonce, ciphertext); err != nil {
		return "", err
	}
	return password, nil
}

// EnvEntry est une variable d'environnement applicative.
type EnvEntry struct {
	Name  string `json:"name" yaml:"name"`
	Value string `json:"value" yaml:"value"`
}

// Env produit les chaînes de connexion du projet, prêtes à être collées dans un
// fichier .env applicatif.
func (p *Provider) Env(ctx context.Context, proj *model.Project) ([]EnvEntry, error) {
	var out []EnvEntry
	for i, decl := range proj.Spec.Databases {
		client, err := p.clientFor(decl)
		if err != nil {
			return nil, err
		}
		endpoint, err := client.Endpoint(ctx)
		if err != nil {
			return nil, err
		}
		password, err := p.Password(proj.ID(), decl.Engine, decl.User)
		if err != nil {
			return nil, fmt.Errorf("mot de passe de %s indisponible : %w", decl.User, err)
		}
		dsn, err := DSN(endpoint, decl.Name, decl.User, password)
		if err != nil {
			return nil, err
		}
		role := ""
		if i > 0 {
			role = strings.TrimPrefix(decl.Name, proj.ID()+"_")
		}
		out = append(out, EnvEntry{Name: EnvVarName(role), Value: dsn})
	}
	if proj.Spec.AMQP != nil {
		password, err := p.Password(proj.ID(), "amqp", proj.Spec.AMQP.User)
		if err == nil {
			out = append(out, EnvEntry{
				Name:  "MESSENGER_TRANSPORT_DSN",
				Value: AMQPDSN(p.AMQPHost, p.AMQPPort, proj.Spec.AMQP.User, password, proj.Spec.AMQP.Vhost),
			})
		}
	}
	return out, nil
}

// DefaultDatabase construit la déclaration de base par défaut d'un projet : tout
// porte l'identifiant du projet (D23).
func DefaultDatabase(proj *model.Project, engine string) model.Database {
	return model.Database{
		Engine: engine, Name: proj.ID(), User: proj.ID(), Host: "localhost",
		Charset: "utf8mb4", Collation: "utf8mb4_unicode_ci", Privileges: string(PrivAll),
	}
}

// RoleDatabase construit une déclaration additionnelle, suffixée par son rôle.
func RoleDatabase(proj *model.Project, engine, role string) model.Database {
	name := naming.RoleName(proj.ID(), role)
	return model.Database{
		Engine: engine, Name: name, User: name, Host: "localhost",
		Privileges: string(PrivAll),
	}
}
