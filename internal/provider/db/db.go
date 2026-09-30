package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"leblanc.io/open-go-server/internal/config"
	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/model"
	"leblanc.io/open-go-server/internal/naming"
	"leblanc.io/open-go-server/internal/secret"
	"leblanc.io/open-go-server/internal/store"
	"leblanc.io/open-go-server/internal/validate"
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

	// Reserved porte les comptes SGBD que la configuration désigne — ceux de la
	// supervision — et qu'aucun projet ne doit toucher (D91) ; les noms fixes et
	// le rôle d'administration PostgreSQL s'y ajoutent d'eux-mêmes.
	Reserved []string

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

// EngineClient ouvre le client d'un moteur sans passer par une déclaration de
// projet. Il sert à la supervision, qui s'adresse au moteur lui-même et non à la
// base d'un projet : le compte de métriques existe même quand aucun projet n'est
// encore déclaré.
func (p *Provider) EngineClient(engine, cluster string) (Client, error) {
	return p.clientFor(model.Database{Engine: engine, Cluster: cluster})
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
	// ExtraGrants porte les privilèges observés de chaque compte additionnel sur
	// la base, sous la même clé qu'ExtraUsers. Leur existence ne suffit pas à les
	// juger conformes : un compte additionnel déclaré « readonly » et créé jadis
	// en « all » garderait ses droits d'écriture sans que rien ne l'annonce.
	ExtraGrants map[string]string
	// OtherHosts énumère, pour chaque compte MySQL absent sous l'hôte déclaré —
	// principal ou additionnel —, les hôtes sous lesquels son nom existe déjà.
	OtherHosts map[string][]string
	// Unavailable porte le motif si le moteur n'est pas joignable.
	Unavailable error

	// projectID est la portée du keystore où vivent les mots de passe du projet.
	projectID string
	// Adopted dit que le projet a été repris d'un serveur existant (« ogs
	// import ») : ses bases et ses comptes existaient avant ogs, et l'adoption les
	// lui a confiés explicitement (D91).
	Adopted bool
	// UserOwned et ExtraOwned disent qu'un compte est à ogs pour ce projet : son
	// mot de passe est scellé dans la portée du projet. ogs ne le scelle qu'en
	// créant le compte — ou en le changeant, sur un compte qui est déjà le sien —,
	// si bien qu'un compte dont il ne tient pas le mot de passe n'est pas le sien.
	UserOwned  bool
	ExtraOwned map[string]bool
	// DBOwned dit que la base est à ogs pour ce projet : elle existe, et le compte
	// principal, qui est à ogs, y a ses droits — la propriété sous PostgreSQL,
	// des privilèges sous MySQL. C'est ce que laisse toute création d'ogs, et ce
	// qui reconnaît sans marque les bases d'un parc antérieur à D91.
	DBOwned bool
}

// account nomme un compte comme le moteur le désigne.
func (s DBState) account(user string) string {
	if s.Declaration.Engine == "postgres" {
		return "rôle " + user
	}
	host := s.Declaration.Host
	if host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("compte '%s'@'%s'", user, host)
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
			Extensions: map[string]bool{}, ExtraUsers: map[string]bool{},
			ExtraGrants: map[string]string{}, OtherHosts: map[string][]string{},
			ExtraOwned: map[string]bool{}, Adopted: proj.Metadata.Labels["adopted"] == "true",
			projectID: proj.ID()}
		state.UserOwned = p.sealed(proj.ID(), decl.Engine, decl.User)
		for _, u := range decl.ExtraUsers {
			state.ExtraOwned[u.User] = p.sealed(proj.ID(), decl.Engine, u.User)
		}
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

		// Une lecture qui échoue après Endpoint n'est pas plus fatale que celui-ci :
		// le moteur est devenu injoignable, et Plan produit l'étape non critique
		// prévue. La rendre à l'appelant faisait abandonner la construction du plan
		// **entier**, donc renoncer au site tout comme au reste du parc, pour une
		// cause que la conception déclare non bloquante.
		databases, err := client.Databases(ctx)
		if err != nil {
			state.Unavailable = err
			obs.States = append(obs.States, state)
			continue
		}
		state.DBExists = databases[decl.Name]

		users, err := client.Users(ctx)
		if err != nil {
			state.Unavailable = err
			obs.States = append(obs.States, state)
			continue
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
			if !state.UserExists {
				if other := otherHosts(users, decl.User, host); len(other) > 0 {
					state.OtherHosts[decl.User] = other
				}
			}
			for _, u := range decl.ExtraUsers {
				state.ExtraUsers[u.User] = users[u.User+"@"+host]
				if !state.ExtraUsers[u.User] {
					if other := otherHosts(users, u.User, host); len(other) > 0 {
						state.OtherHosts[u.User] = other
					}
					continue
				}
				// Une lecture en échec laisse l'entrée vide, donc non conforme :
				// l'étape repasse, ce qui est le sens sûr — l'inverse ferait déclarer
				// conforme un compte dont on n'a rien pu relire.
				if grants, err := client.Grants(ctx, u.User, host); err == nil {
					state.ExtraGrants[u.User] = grants[decl.Name]
				}
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
		state.DBOwned = state.DBExists && state.UserExists && state.UserOwned && state.Grants[decl.Name] != ""
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

		// Un compte principal réservé se refuse sans le moteur : le dire même quand
		// il est injoignable, puisque rien d'autre n'arrêtera cette déclaration.
		if why := p.reserved(decl.Engine, decl.User); why != "" && state.Unavailable != nil {
			steps = append(steps, refusedStep(resource, engine.Create, decl, proj.ID(),
				[]string{fmt.Sprintf("%s est réservé (%s)", state.account(decl.User), why)}))
			continue
		}
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
		// Ce qu'ogs refuse de toucher (D91) : un compte réservé, et, hors adoption,
		// un homonyme qu'il n'a pas créé — le reprendre en faisait une ressource du
		// projet, qu'un rollback puis « project delete » supprimaient. Un refus qui
		// porte sur la base ou son compte principal arrête l'étape de la base ; celui
		// d'un compte additionnel ne vise que lui, la base et le compte principal
		// restant réconciliés. Les étapes sont non critiques, pour la raison donnée
		// plus bas aux comptes masqués.
		refused, extras := p.refusals(proj, state)
		if len(refused) > 0 {
			action := engine.Update
			if !state.DBExists {
				action = engine.Create
			}
			steps = append(steps, refusedStep(resource, action, decl, proj.ID(), refused))
			continue
		}
		if len(extras) > 0 {
			kept := make([]model.DBUser, 0, len(decl.ExtraUsers))
			for _, u := range decl.ExtraUsers {
				if why, ok := extras[u.User]; ok {
					action := engine.Update
					if !state.ExtraUsers[u.User] {
						action = engine.Create
					}
					steps = append(steps, refusedStep(resource+"/"+u.User, action, decl, proj.ID(), []string{why}))
					continue
				}
				kept = append(kept, u)
			}
			decl.ExtraUsers = kept
			state.Declaration = decl
		}
		observedGrants := state.Grants[decl.Name]
		hasGrant := grantsConform(decl.Engine, observedGrants, priv)
		conforming := state.DBExists && state.UserExists && hasGrant &&
			extensionsSatisfied(decl, state) && extraUsersSatisfied(decl, state)

		if conforming {
			steps = append(steps, engine.NewStep(resource, engine.NoOp,
				fmt.Sprintf("base %s %s (utilisateur %s)", decl.Engine, decl.Name, decl.User)))
			continue
		}

		action := engine.Update
		if !state.DBExists {
			action = engine.Create
		}
		// Un compte dont le nom existe sous un autre hôte n'est pas créé : MySQL
		// retient pour chaque connexion le compte à l'hôte le plus précis, et un
		// 'shop'@'localhost' neuf, au mot de passe tiré, masquerait 'shop'@'%' pour
		// toute connexion locale. C'était le sort de toute base adoptée tant que
		// l'import déclarait l'hôte par défaut — l'application perdait l'accès à sa
		// base au premier apply —, et les déclarations écrites alors le restent.
		// L'étape n'est pas critique : le site n'a pas à tomber pour un compte
		// qu'on refuse de créer.
		if masked := maskedAccounts(decl, state, host); masked != "" {
			reason := fmt.Errorf("base %s %s : %s existe déjà sous un autre hôte que %q, et un homonyme "+
				"sous celui-ci changerait le compte que MySQL retient pour une partie des connexions — "+
				"il préfère l'hôte le plus précis. Déclarer l'hôte réel (champ host de spec.databases), "+
				"ou retirer l'autre compte", decl.Engine, decl.Name, masked, host)
			s := engine.NewStep(resource, action, fmt.Sprintf(
				"base %s %s — compte refusé : %s existe sous un autre hôte", decl.Engine, decl.Name, masked))
			s.Critical = false
			s.Apply = func(context.Context) error { return reason }
			steps = append(steps, s)
			continue
		}
		summary := fmt.Sprintf("base %s %s (utilisateur %s, privilèges %s)",
			decl.Engine, decl.Name, decl.User, priv)
		s := engine.NewStep(resource, action, summary)
		declCopy, stateCopy := decl, state
		s.Diff = func() (string, error) { return p.describe(declCopy, stateCopy, priv, host) }
		// Les mots de passe que l'étape a scellés, pour que la compensation les
		// retire avec les comptes qu'elle supprime : un secret resté derrière ferait
		// d'un homonyme créé plus tard par un tiers un compte d'ogs.
		var sealedHere []string
		s.Apply = func(ctx context.Context) error {
			return p.apply(ctx, proj, declCopy, stateCopy, priv, host, func(user string) {
				sealedHere = append(sealedHere, user)
			})
		}
		s.Compensate = func(ctx context.Context) error {
			// La compensation ne défait que ce que l'étape a créé : la base si elle
			// était absente, les comptes absents à l'observation. Jamais une donnée ni
			// un compte préexistants dans un rollback — le compte d'une autre base du
			// projet, celle de la production, partait avec la base neuve (R1-05).
			targets := dropTargets{}
			if !stateCopy.DBExists {
				targets.database = true
			}
			if !stateCopy.UserExists {
				targets.users = append(targets.users, declCopy.User)
			}
			for _, u := range declCopy.ExtraUsers {
				if !stateCopy.ExtraUsers[u.User] {
					targets.users = append(targets.users, u.User)
				}
			}
			if err := p.dropTargets(ctx, declCopy, targets); err != nil {
				return err
			}
			for _, user := range sealedHere {
				if p.Store != nil {
					_ = p.Store.DeleteSecret(proj.ID(), secretName(declCopy.Engine, user))
				}
			}
			return nil
		}
		steps = append(steps, s)
	}
	return steps, nil
}

// refusals rend ce qu'ogs refuse de toucher dans une déclaration (D91) : refused
// pour la base ou son compte principal, extras pour les comptes additionnels, par
// nom de compte.
func (p *Provider) refusals(proj *model.Project, state DBState) (refused []string, extras map[string]string) {
	decl := state.Declaration
	if why := p.reserved(decl.Engine, decl.User); why != "" {
		refused = append(refused, fmt.Sprintf("%s est réservé (%s)", state.account(decl.User), why))
	}
	if !state.Adopted {
		if state.DBExists && !state.DBOwned {
			refused = append(refused, fmt.Sprintf("base %s existe déjà sans qu'ogs l'ait créée pour %s",
				decl.Name, proj.ID()))
		}
		if state.UserExists && !state.UserOwned {
			refused = append(refused, fmt.Sprintf("%s existe déjà sans qu'ogs l'ait créé pour %s",
				state.account(decl.User), proj.ID()))
		}
	}
	for _, u := range decl.ExtraUsers {
		why := p.reserved(decl.Engine, u.User)
		switch {
		case why != "":
			why = fmt.Sprintf("%s est réservé (%s)", state.account(u.User), why)
		case !state.Adopted && state.ExtraUsers[u.User] && !state.ExtraOwned[u.User]:
			why = fmt.Sprintf("%s existe déjà sans qu'ogs l'ait créé pour %s", state.account(u.User), proj.ID())
		default:
			continue
		}
		if extras == nil {
			extras = map[string]string{}
		}
		extras[u.User] = why
	}
	return refused, extras
}

// refusedStep est l'étape non critique d'un refus : rien n'est exécuté, aucun
// mot de passe scellé, et le message dit les issues réelles. L'import n'en est
// pas une : il refuse un projet déjà déclaré, et l'adoption d'une base isolée
// reste à arbitrer (§20).
func refusedStep(resource string, action engine.Action, decl model.Database, id string, refused []string) *engine.Step {
	what := strings.Join(refused, " ; ")
	reason := fmt.Errorf("base %s %s : %s — ogs n'y touche pas. Retirer ou renommer l'homonyme sur le "+
		"serveur, ou déclarer d'autres noms (champs name, user et extraUsers de spec.databases) ; "+
		"l'adoption d'une base isolée par un projet déjà déclaré n'existe pas encore (D91)",
		decl.Engine, decl.Name, what)
	s := engine.NewStep(resource, action, fmt.Sprintf("base %s %s — refusée : %s", decl.Engine, decl.Name, what))
	s.Critical = false
	s.Apply = func(context.Context) error { return reason }
	return s
}

// reserved dit pourquoi un compte est réservé (validate.ReservedDBAccount), ceux
// que la configuration désigne compris, ou rend une chaîne vide.
func (p *Provider) reserved(engine, user string) string {
	extra := append([]string(nil), p.Reserved...)
	if engine == "postgres" {
		extra = append(extra, PostgresAdminRole(os.Getenv("OGS_PG_ADMIN")))
	}
	return validate.ReservedDBAccount(engine, user, extra...)
}

// grantsConform compare les privilèges observés sur la base au jeu déclaré. La
// seule présence d'un privilège ne suffisait pas : un passage de « all » à
// « readonly » restait un NoOp éternel, et ogs affirmait un état qu'il n'avait
// jamais appliqué — dans l'autre sens, l'application restait sans droit
// d'écriture alors que le plan annonçait la conformité (§7.2).
//
// Côté PostgreSQL l'observation ne rend que la propriété de la base, qui ne
// s'énumère pas : elle fait foi, et l'y soumettre fabriquerait une dérive
// permanente.
func grantsConform(engine, observed string, priv Privilege) bool {
	if engine == "postgres" {
		return observed != ""
	}
	return MySQLGrantsSatisfied(observed, priv)
}

// otherHosts énumère les hôtes sous lesquels le compte MySQL user existe, hors
// l'hôte déclaré. Les comptes sont relevés sous la forme « user@host » ; un hôte
// ne contient jamais « @ », d'où la coupure au dernier.
func otherHosts(users map[string]bool, user, host string) []string {
	var out []string
	for account := range users {
		i := strings.LastIndex(account, "@")
		if i < 0 || account[:i] != user || account[i+1:] == host {
			continue
		}
		out = append(out, account[i+1:])
	}
	sort.Strings(out)
	return out
}

// maskedAccounts nomme les comptes existants qu'un compte à créer sous l'hôte
// déclaré masquerait, ou rend une chaîne vide.
func maskedAccounts(decl model.Database, state DBState, host string) string {
	users := []string{decl.User}
	for _, u := range decl.ExtraUsers {
		users = append(users, u.User)
	}
	var out []string
	for _, user := range users {
		for _, other := range state.OtherHosts[user] {
			out = append(out, fmt.Sprintf("'%s'@'%s'", user, other))
		}
	}
	return strings.Join(out, ", ")
}

func extensionsSatisfied(decl model.Database, state DBState) bool {
	for _, ext := range decl.Extensions {
		if !state.Extensions[ext] {
			return false
		}
	}
	return true
}

// extraUsersSatisfied juge les comptes additionnels sur les mêmes critères que
// le compte principal : leur existence, et le jeu de privilèges qu'ils portent
// réellement sur la base. Ne regarder que l'existence laissait un compte déclaré
// « readonly » garder l'écriture qu'un ancien apply lui avait donnée.
//
// Côté PostgreSQL, les comptes additionnels ne sont pas posés — l'observation n'y
// rend que la propriété de la base — et seule leur existence est jugée.
func extraUsersSatisfied(decl model.Database, state DBState) bool {
	for _, u := range decl.ExtraUsers {
		if !state.ExtraUsers[u.User] {
			return false
		}
		if decl.Engine == "postgres" {
			continue
		}
		priv, err := ParsePrivilege(u.Privileges)
		if err != nil {
			return false
		}
		if !MySQLGrantsSatisfied(state.ExtraGrants[u.User], priv) {
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
			Revoke:     MySQLGrantsOverflow(state.Grants[decl.Name], priv),
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
				Revoke: MySQLGrantsOverflow(state.ExtraGrants[u.User], extraPriv),
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
	state DBState, priv Privilege, host string, onSeal func(user string)) error {
	client, err := p.clientFor(decl)
	if err != nil {
		return err
	}
	password := func(user string) (string, error) {
		pw, created, err := p.obtainPassword(proj.ID(), decl.Engine, user)
		if err == nil && created && onSeal != nil {
			onSeal(user)
		}
		return pw, err
	}
	mainPassword, err := password(decl.User)
	if err != nil {
		return err
	}

	switch decl.Engine {
	case "mysql", "mariadb":
		spec := MySQLSpec{
			Database: decl.Name, Charset: decl.Charset, Collation: decl.Collation,
			User: decl.User, Host: host, Password: mainPassword, Privileges: priv,
			AuthPlugin: decl.AuthPlugin,
			Revoke:     MySQLGrantsOverflow(state.Grants[decl.Name], priv),
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
			extraPassword, err := password(u.User)
			if err != nil {
				return err
			}
			extra, err := CreateMySQL(MySQLSpec{
				Database: decl.Name, User: u.User, Host: host,
				Password: extraPassword, Privileges: extraPriv,
				Revoke: MySQLGrantsOverflow(state.ExtraGrants[u.User], extraPriv),
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
			Database: decl.Name, Role: decl.User, Password: mainPassword,
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
		// Les comptes additionnels : ils étaient observés sans jamais être créés,
		// de sorte que le plan les redemandait indéfiniment.
		for _, u := range decl.ExtraUsers {
			if state.ExtraUsers[u.User] {
				continue
			}
			extraPriv, err := ParsePrivilege(u.Privileges)
			if err != nil {
				return err
			}
			extraPassword, err := password(u.User)
			if err != nil {
				return err
			}
			extra, err := PostgresExtraUser(decl.Name, decl.User, u.User, extraPassword, extraPriv)
			if err != nil {
				return err
			}
			for _, stmt := range extra.Cluster {
				if err := client.Exec(ctx, stmt); err != nil {
					return err
				}
			}
			for _, stmt := range extra.InDatabase {
				if err := client.ExecIn(ctx, decl.Name, stmt); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Drop supprime une base et ses comptes, pour la suppression d'un projet.
// Opération destructrice : l'appelant doit avoir obtenu confirmation.
//
// Seul part ce qui est à ogs pour ce projet (D91) — sauf pour un projet adopté,
// dont la suppression des bases a été confirmée sous --force (D77). Un homonyme
// qu'ogs n'a pas créé reste en place, et kept le nomme : il porte peut-être les
// données d'un site resté hors d'ogs.
func (p *Provider) Drop(ctx context.Context, decl model.Database, state DBState) (kept []string, err error) {
	targets := dropTargets{database: state.Adopted || state.DBOwned}
	if state.DBExists && !targets.database {
		kept = append(kept, "base "+decl.Name)
	}
	users := []string{decl.User}
	for _, u := range decl.ExtraUsers {
		users = append(users, u.User)
	}
	for i, user := range users {
		owned, exists := state.UserOwned, state.UserExists
		if i > 0 {
			owned, exists = state.ExtraOwned[user], state.ExtraUsers[user]
		}
		switch {
		case p.reserved(decl.Engine, user) != "":
			// Jamais, --force compris : un projet d'avant D91 qui déclarait root en
			// tient un mot de passe scellé, et « DROP USER 'root'@'localhost' »
			// couperait ogs du moteur pour tout le parc.
			if exists {
				kept = append(kept, state.account(user)+" (réservé)")
			}
		case state.Adopted || owned:
			targets.users = append(targets.users, user)
		case exists:
			kept = append(kept, state.account(user))
		}
	}
	// Le mot de passe d'un compte supprimé part avec lui, au fil de chaque
	// suppression réussie — l'archive en garde la copie scellée. Laissé au
	// keystore, il ferait d'un homonyme recréé plus tard hors d'ogs le compte d'un
	// projet du même nom, repris puis supprimé avec lui ; et une suppression
	// interrompue en route ne doit pas garder ceux des comptes déjà partis.
	targets.dropped = func(user string) {
		if p.Store != nil {
			_ = p.Store.DeleteSecret(state.projectID, secretName(decl.Engine, user))
		}
	}
	return kept, p.dropTargets(ctx, decl, targets)
}

// dropTargets désigne ce qu'une suppression retire : la base ou non, et les
// comptes nommés.
type dropTargets struct {
	database bool
	users    []string
	// dropped, s'il est donné, est appelé après la suppression réussie de chaque
	// compte.
	dropped func(user string)
}

// dropTargets supprime la base et les comptes désignés, et rien d'autre.
func (p *Provider) dropTargets(ctx context.Context, decl model.Database, t dropTargets) error {
	if !t.database && len(t.users) == 0 {
		return nil
	}
	client, err := p.clientFor(decl)
	if err != nil {
		return err
	}
	host := decl.Host
	if host == "" {
		host = "localhost"
	}
	database := ""
	if t.database {
		database = decl.Name
	}
	force := false
	if pg, ok := client.(*pgClient); ok {
		force = pg.SupportsForceDrop()
	}
	// La base d'abord, puis chaque compte, une requête à la fois : chacun est
	// signalé dès qu'il est parti.
	drop := func(database string, users []string) ([]string, error) {
		if decl.Engine == "postgres" {
			return DropPostgres(database, "", users, force)
		}
		return DropMySQL(database, "", host, users)
	}
	if database != "" {
		stmts, err := drop(database, nil)
		if err != nil {
			return err
		}
		for _, stmt := range stmts {
			if err := client.Exec(ctx, stmt); err != nil {
				return err
			}
		}
	}
	for _, user := range t.users {
		stmts, err := drop("", []string{user})
		if err != nil {
			return err
		}
		for _, stmt := range stmts {
			if err := client.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		if t.dropped != nil {
			t.dropped(user)
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
	password, _, err := p.obtainPassword(project, engine, user)
	return password, err
}

// obtainPassword est ensurePassword, et dit en plus si le mot de passe vient
// d'être tiré et scellé.
func (p *Provider) obtainPassword(project, engine, user string) (password string, created bool, err error) {
	if p.Store == nil || p.Keys == nil {
		return "", false, fmt.Errorf("keystore indisponible : impossible de gérer le mot de passe de %s", user)
	}
	name := secretName(engine, user)
	nonce, ciphertext, err := p.Store.GetSecret(project, name)
	if err == nil {
		plain, err := p.Keys.Open(project, name, nonce, ciphertext)
		if err != nil {
			return "", false, err
		}
		return string(plain), false, nil
	}
	password, err = secret.Password(0)
	if err != nil {
		return "", false, err
	}
	nonce, ciphertext, err = p.Keys.Seal(project, name, []byte(password))
	if err != nil {
		return "", false, err
	}
	if err := p.Store.PutSecret(project, name, nonce, ciphertext); err != nil {
		return "", false, err
	}
	return password, true, nil
}

// sealed dit si le mot de passe d'un compte est scellé dans la portée du projet,
// ce qui fait de ce compte celui d'ogs (D91). Une base d'état injoignable ne fait
// rien reconnaître : c'est le sens sûr, un refus plutôt qu'une reprise.
func (p *Provider) sealed(project, engine, user string) bool {
	if p.Store == nil {
		return false
	}
	_, _, err := p.Store.GetSecret(project, secretName(engine, user))
	return err == nil
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
//
// Un compte réservé — celui du moteur, de son administration, de la supervision —
// n'est jamais visé (D91) : sous MariaDB, root y perdait l'authentification par
// socket dont ogs dépend pour tout le parc, et son mot de passe devenait un secret
// du projet. Un compte qui n'est pas à ogs ne l'est pas davantage, hors adoption :
// sceller son nouveau mot de passe en ferait le compte du projet, sans adoption.
func (p *Provider) RotatePassword(ctx context.Context, proj *model.Project, decl model.Database, user string) (string, error) {
	if why := p.reserved(decl.Engine, user); why != "" {
		return "", fmt.Errorf("compte %q réservé (%s) : ogs n'en change pas le mot de passe", user, why)
	}
	if proj.Metadata.Labels["adopted"] != "true" && !p.sealed(proj.ID(), decl.Engine, user) {
		return "", fmt.Errorf("le compte %s n'a pas été créé par ogs pour %s : en changer le mot de passe "+
			"le ferait sien sans adoption, que seul l'import accorde, à un projet entier (D91)", user, proj.ID())
	}
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
