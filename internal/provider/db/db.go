package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

	// Force autorise « project delete » à retirer les comptes orphelins d'un
	// projet adopté, comme ses bases (D77) : ogs n'en a pas la propriété. L'apply
	// n'en tient pas compte — son --force reprend un fichier retouché.
	Force bool

	// PHPWorkers rend la somme des pm.max_children résolus des pools d'un
	// projet, première part de son budget de connexions (D127) ; nul, zéro.
	PHPWorkers func(*model.Project) int

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
		socket, refused := findMySQLSocket(p.Paths.Prefix())
		if socket == "" {
			return nil, mysqlUnavailable(refused)
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
	// UserPlugin est le plugin d'authentification du compte principal, relu sur
	// un vrai MySQL quand la déclaration en appelle un (D126) ; vide sinon.
	UserPlugin string
	// NativeActive dit si mysql_native_password est actif sur le serveur ;
	// nativeRead, qu'on a pu le lire.
	NativeActive bool
	nativeRead   bool
	// legacyPHP porte les versions de PHP du projet qui exigent
	// mysql_native_password.
	legacyPHP []string
	// alterPlugin demande de reposer le plugin du compte principal, existant et
	// tenu par ogs, avec son mot de passe scellé.
	alterPlugin bool
	// pluginAuto dit que le plugin voulu vient de « auto », non de la déclaration.
	pluginAuto bool
	// Limits porte la limite de connexions relue de chaque compte existant de la
	// déclaration, dans la valeur du moteur ; un compte dont la lecture a échoué
	// n'y figure pas (D127).
	Limits map[string]int
	// SetLimits porte les limites que l'étape pose : comptes à créer, et comptes
	// existants dont la limite relue diffère de la voulue.
	SetLimits map[string]int

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
	// Moved associe à un compte MySQL du projet l'hôte sous lequel il existe
	// encore, quand l'hôte déclaré a changé (D100) : son mot de passe est scellé,
	// il n'existe pas sous l'hôte déclaré et n'existe que sous cet autre-là. C'est
	// le compte qu'ogs a créé sous l'ancienne déclaration ; l'apply le recrée sous
	// l'hôte déclaré, puis retire l'ancien. Plusieurs hôtes restent un refus : rien
	// ne dit lequel ogs a créé.
	Moved map[string][]string
	// Retired associe à un compte MySQL du projet, présent sous l'hôte déclaré,
	// les autres hôtes que sa marque désigne (movedName) : leurs comptes partent,
	// puis la marque — vide, il ne reste que la marque à oublier.
	Retired map[string][]string
	// Holders énumère les autres comptes à ogs pour ce projet — mot de passe
	// scellé — qui ont leurs droits sur la base (la propriété sous PostgreSQL).
	// Ils prouvent la base à ogs comme son compte principal (D91) : c'est l'ancien
	// compte principal quand spec.databases[].user a changé, ou le compte sous
	// l'ancien hôte (D100).
	Holders []string
	// Revoke énumère, sous MySQL, les comptes à ogs pour ce projet qu'une autre
	// déclaration emploie encore et qui gardent des droits sur cette base sans y
	// être déclarés : un compte additionnel retiré de cette base-ci (D100).
	Revoke []string
	// pgOwner est, sous PostgreSQL, le compte à ogs qui possède la base quand ce
	// n'est pas le compte déclaré : la propriété lui est reprise.
	pgOwner string
	// Owner et PublicConnect sont, sous PostgreSQL, le propriétaire relu de la
	// base et le droit CONNECT de PUBLIC, lu dans son ACL (D111). Une relecture
	// en échec les laisse vides : elle ne fonde aucune correction.
	Owner         string
	PublicConnect bool
	// RoleConnect dit que le rôle déclaré peut se connecter à la base ;
	// accessRead, que l'ACL a pu être relue.
	RoleConnect bool
	accessRead  bool
	// Jokers associe à un compte MySQL le motif à joker qui désigne la base —
	// « shop_fr », GRANT écrit sans échappement — et qu'aucune autre
	// base existante ne partage : il est retiré, le GRANT échappé posé avant lui.
	// JokersKept nomme ceux qui couvrent aussi d'autres bases, dont ils portent
	// l'accès : ils restent, hors du jugement de la base (D111).
	Jokers     map[string]string
	JokerPrivs map[string]string
	JokersKept map[string][]string
}

// CreatedByOGS dit si ogs a créé la base, ou la crée : celle de tout projet non
// adopté — une base qu'ogs n'a pas créée y est refusée (D91) —, et celle qui
// n'existe pas encore. La base existante d'un projet adopté était là avant lui :
// CONNECT et le schéma public n'y sont pas touchés (D111).
func (s DBState) CreatedByOGS() bool { return !s.Adopted || !s.DBExists }

// needsPassword dit si un compte appelle un mot de passe : absent, ogs le crée et
// le scelle ; présent, ogs ne le connaît que s'il le tient. Le compte existant
// d'un projet adopté sans secret n'en reçoit aucun (D78) : en tirer un pour
// « CREATE USER IF NOT EXISTS », qui ne le pose pas, scellait un mot de passe
// que le moteur ignore (R2-21).
func (p *Provider) needsPassword(project, engine, user string, exists bool) bool {
	return !exists || p.sealed(project, engine, user)
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
	// Orphans énumère les comptes dont le mot de passe est scellé dans la portée
	// du projet et qu'aucune de ses déclarations ne nomme plus (D100).
	Orphans []Orphan
}

// Observe inspecte les moteurs déclarés par le projet.
func (p *Provider) Observe(ctx context.Context, proj *model.Project) (Observed, error) {
	var obs Observed
	for _, decl := range proj.Spec.Databases {
		state := DBState{Declaration: decl, Grants: map[string]string{},
			Extensions: map[string]bool{}, ExtraUsers: map[string]bool{},
			ExtraGrants: map[string]string{}, OtherHosts: map[string][]string{},
			ExtraOwned: map[string]bool{}, Adopted: proj.Metadata.Labels["adopted"] == "true",
			Jokers: map[string]string{}, JokerPrivs: map[string]string{}, JokersKept: map[string][]string{},
			Limits: map[string]int{}, projectID: proj.ID()}
		state.UserOwned = p.sealed(proj.ID(), decl.Engine, decl.User)
		state.Moved = map[string][]string{}
		state.Retired = map[string][]string{}
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
			// Un compte recréé sous l'hôte déclaré par un apply précédent : celui
			// d'avant, que la marque désigne, part maintenant que le remplaçant
			// existait à l'observation (D100).
			names := []string{decl.User}
			for _, u := range decl.ExtraUsers {
				names = append(names, u.User)
			}
			for _, user := range names {
				if !users[user+"@"+host] || state.Adopted {
					continue
				}
				// Retour à un hôte déjà tenu compris : c'est alors le compte posé
				// sous l'hôte quitté qui part, jamais celui de l'hôte déclaré.
				if marked := p.movedHosts(proj.ID(), decl.Engine, user); len(marked) > 0 {
					retired := []string{}
					for _, h := range marked {
						if h != host {
							retired = append(retired, h)
						}
					}
					state.Retired[user] = retired
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
					state.observeJoker(u.User, grants, databases)
				}
			}
		}

		state.observePlugin(ctx, client, proj, host)
		state.observeLimits(ctx, client, host)
		if state.UserExists {
			if grants, err := client.Grants(ctx, decl.User, host); err == nil {
				state.Grants = grants
				if decl.Engine != "postgres" {
					state.observeJoker(decl.User, grants, databases)
				}
			}
		}
		// L'ACL de la base : CONNECT rendu à PUBLIC sur une base d'ogs est une
		// dérive à corriger, sur une base adoptée un constat (D111).
		if state.DBExists && decl.Engine == "postgres" {
			if acl, ok := client.(databaseAccess); ok {
				if owner, public, mine, err := acl.DatabaseAccess(ctx, decl.Name, decl.User); err == nil {
					state.Owner, state.PublicConnect, state.RoleConnect = owner, public, mine
					state.accessRead = true
				}
			}
		}
		if state.DBExists && decl.Engine == "postgres" && len(decl.Extensions) > 0 {
			if exts, err := client.Extensions(ctx, decl.Name); err == nil {
				state.Extensions = exts
			}
		}
		p.observeOthers(ctx, client, proj, &state, users, host)
		state.DBOwned = state.DBExists && (state.UserExists && state.UserOwned && state.Grants[decl.Name] != "" ||
			len(state.Holders) > 0)
		obs.States = append(obs.States, state)
	}
	obs.Orphans = p.observeOrphans(ctx, proj)
	return obs, nil
}

// Plan calcule les étapes relatives aux bases. Un moteur injoignable produit une
// étape non critique : un site dont la base est indisponible reste déployable, et
// l'échec est explicite plutôt que fatal.
func (p *Provider) Plan(ctx context.Context, proj *model.Project, obs Observed) ([]*engine.Step, error) {
	var steps []*engine.Step
	phpWorkers := 0
	if p.PHPWorkers != nil {
		phpWorkers = p.PHPWorkers(proj)
	}
	limits := accountLimits(proj, phpWorkers)
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
		// Refus non critique, comme un compte masqué plus bas : la base en place
		// continue de servir, rien n'y est changé (D92).
		if reason := CheckPostgresPrivilege(decl.Engine, priv); reason != nil {
			action := engine.Update
			if !state.DBExists {
				action = engine.Create
			}
			s := engine.NewStep(resource, action, fmt.Sprintf(
				"base %s %s — jeu %s refusé sur le compte principal", decl.Engine, decl.Name, priv))
			s.Critical = false
			s.Apply = func(context.Context) error { return reason }
			steps = append(steps, s)
			continue
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
		// Le plugin d'authentification (D126) : « auto » vaut mysql_native_password
		// sur un vrai MySQL dès qu'une version de PHP du projet l'exige.
		plugin, missing := wantedPlugin(state)
		authNotice := pluginNotice(resource, decl, state, plugin, missing, host)
		if decl.Engine != "postgres" {
			state.pluginAuto = !declaredPlugin(decl) && plugin != ""
			decl.AuthPlugin = plugin
			state.Declaration = decl
		}
		state.alterPlugin = pluginDrift(state, plugin) && state.UserOwned
		// La limite de connexions de chaque compte (D127), adoptés compris.
		state.planLimits(limits)
		observedGrants := state.Grants[decl.Name]
		hasGrant := grantsConform(decl.Engine, observedGrants, priv)
		// Une base adoptée n'appartient pas toujours au rôle que l'import a
		// déclaré d'après son nom (R1-46) : exiger la propriété faisait repasser
		// l'étape à chaque apply, sans jamais la rendre. Seule une base qu'un ancien
		// compte d'ogs possède lui est reprise (pgOwnership).
		// Le rôle déclaré doit pouvoir s'y connecter : sans quoi l'étape lui
		// accorde CONNECT, ce qui n'ôte rien à personne.
		if decl.Engine == "postgres" && !state.CreatedByOGS() && state.pgOwner == "" {
			hasGrant = !state.accessRead || state.RoleConnect
		}
		// CONNECT rendu à PUBLIC sur une base d'ogs : une correction annoncée,
		// jamais un refus (R1-45).
		reopened := decl.Engine == "postgres" && state.CreatedByOGS() && state.PublicConnect
		conforming := state.DBExists && state.UserExists && hasGrant && !reopened && len(state.Jokers) == 0 &&
			extensionsSatisfied(decl, state) && extraUsersSatisfied(decl, state) && !state.alterPlugin &&
			!state.limitDrift()

		// Ce qu'une déclaration changée laisse derrière elle (D100) : l'ancien
		// compte sous l'hôte d'avant, les droits d'un compte retiré de la base.
		follow := p.followSteps(resource, decl, state, host)
		if conforming {
			steps = append(steps, engine.NewStep(resource, engine.NoOp,
				fmt.Sprintf("base %s %s (utilisateur %s)", decl.Engine, decl.Name, decl.User)))
			if authNotice != nil {
				steps = append(steps, authNotice)
			}
			steps = append(steps, follow...)
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
			decl.Engine, decl.Name, decl.User, priv) + stateNotes(state, reopened) + pluginNote(state, plugin) +
			limitNote(state)
		s := engine.NewStep(resource, action, summary)
		declCopy, stateCopy := decl, state
		s.Diff = func() (string, error) { return p.describe(declCopy, stateCopy, priv, host) }
		// Les mots de passe que l'étape a scellés, pour que la compensation les
		// retire avec les comptes qu'elle supprime : un secret resté derrière ferait
		// d'un homonyme créé plus tard par un tiers un compte d'ogs.
		var sealedHere, movedHere []string
		// La marque d'avant, pour qu'un déroulage la rende au lieu de l'effacer :
		// elle prouve encore les comptes d'un déplacement précédent.
		previousMarks := map[string][]string{}
		s.Apply = func(ctx context.Context) error {
			// La marque d'un compte déplacé précède sa recréation sous l'hôte
			// déclaré : l'ancien ne partira qu'à l'apply suivant, le remplaçant
			// observé (D100). Le retirer dans la foulée laissait la base sans aucun
			// compte si la saga se déroulait ensuite.
			for _, user := range sortedListKeys(stateCopy.Moved) {
				before := p.movedHosts(proj.ID(), declCopy.Engine, user)
				previousMarks[user] = before
				hosts := append(append(append([]string(nil), before...), stateCopy.Moved[user]...), host)
				if err := p.sealMoved(proj.ID(), declCopy.Engine, user, hosts); err != nil {
					return err
				}
				movedHere = append(movedHere, user)
			}
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
			// Sous PostgreSQL, la base reprise par le compte déclaré revient à son
			// ancien propriétaire avant que la compensation ne retire le nouveau :
			// sans quoi DROP ROLE échoue, et la base resterait à un compte disparu.
			if restore := pgOwnershipRestore(declCopy, stateCopy); restore != "" {
				if client, err := p.clientFor(declCopy); err == nil {
					if err := client.Exec(ctx, restore); err != nil {
						return err
					}
				}
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
			for _, user := range movedHere {
				if p.Store == nil {
					continue
				}
				if before := previousMarks[user]; len(before) > 0 {
					_ = p.sealMoved(proj.ID(), declCopy.Engine, user, before)
					continue
				}
				_ = p.Store.DeleteSecret(proj.ID(), movedName(declCopy.Engine, user))
			}
			return nil
		}
		steps = append(steps, s)
		if authNotice != nil {
			steps = append(steps, authNotice)
		}
		if notice := p.recreatedNotice(resource, proj, decl, state, &sealedHere); notice != nil {
			steps = append(steps, notice)
		}
		steps = append(steps, follow...)
	}
	// Les comptes qu'aucune déclaration ne nomme plus viennent en dernier : un
	// compte renommé ne part qu'une fois son remplaçant créé, et la propriété
	// d'une base PostgreSQL reprise par le compte déclaré.
	steps = append(steps, p.orphanSteps(proj, obs.Orphans)...)
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

// observeJoker relève le motif à joker qui désigne la base parmi les privilèges
// d'un compte MySQL (D111).
func (s *DBState) observeJoker(user string, grants map[string]string, databases map[string]bool) {
	pattern, others := mysqlJoker(grants, s.Declaration.Name, databases)
	switch {
	case pattern == "":
	case len(others) > 0:
		s.JokersKept[user] = others
	default:
		s.Jokers[user] = pattern
		s.JokerPrivs[user] = grants[jokerKey(pattern)]
	}
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
		if len(state.Moved[user]) > 0 {
			// L'ancien compte d'ogs sous l'hôte d'avant : recréé sous l'hôte
			// déclaré, il est retiré aussitôt après (followSteps).
			continue
		}
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
	stmts, err := p.statements(decl, state, priv, host, func(user string, exists bool) string {
		if !p.needsPassword(state.projectID, decl.Engine, user, exists) {
			return ""
		}
		return "«mot de passe généré»"
	})
	if err != nil {
		return "", err
	}
	return strings.Join(stmts, "\n"), nil
}

// statements produit la liste des requêtes à exécuter pour une déclaration.
// password rend le mot de passe d'un compte, vide s'il n'en appelle aucun.
func (p *Provider) statements(decl model.Database, state DBState, priv Privilege, host string,
	password func(user string, exists bool) string) ([]string, error) {
	switch decl.Engine {
	case "mysql", "mariadb":
		spec := MySQLSpec{
			Database: decl.Name, Charset: decl.Charset, Collation: decl.Collation,
			User: decl.User, Host: host, Password: password(decl.User, state.UserExists), Privileges: priv,
			AuthPlugin: decl.AuthPlugin,
			Revoke:     MySQLGrantsOverflow(state.Grants[decl.Name], priv),
		}
		stmts, err := createMySQL(spec)
		if err != nil {
			return nil, err
		}
		out := stmts.All()
		if alter, err := alterPluginStatement(decl, state, host, spec.Password); err != nil {
			return nil, err
		} else if alter != "" {
			out = append(out, alter)
		}
		for _, u := range decl.ExtraUsers {
			extraPriv, err := ParsePrivilege(u.Privileges)
			if err != nil {
				return nil, err
			}
			extra, err := createMySQL(MySQLSpec{
				Database: decl.Name, Charset: decl.Charset, Collation: decl.Collation,
				User: u.User, Host: host, Password: password(u.User, state.ExtraUsers[u.User]), Privileges: extraPriv,
				AuthPlugin: decl.AuthPlugin,
				Revoke:     MySQLGrantsOverflow(state.ExtraGrants[u.User], extraPriv),
			})
			if err != nil {
				return nil, err
			}
			out = append(out, extra.User...)
			out = append(out, extra.Grants...)
		}
		jokers, err := jokerRevokes(decl, state, host)
		if err != nil {
			return nil, err
		}
		out = append(out, jokers...)
		limits, err := limitStatements(decl, state, host)
		if err != nil {
			return nil, err
		}
		return append(out, limits...), nil
	case "postgres":
		stmts, err := CreatePostgres(PostgresSpec{
			Database: decl.Name, Role: decl.User, Password: password(decl.User, state.UserExists),
			Extensions: decl.Extensions, Privileges: priv, Adopted: !state.CreatedByOGS(),
		}, state.UserExists, state.DBExists)
		if err != nil {
			return nil, err
		}
		stmts.Cluster = append(stmts.Cluster, pgOwnership(decl, state)...)
		out := append(stmts.Cluster, stmts.InDatabase...)
		limits, err := limitStatements(decl, state, host)
		if err != nil {
			return nil, err
		}
		return append(out, limits...), nil
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
	// Un mot de passe n'est tiré et scellé que pour être posé : sur un compte à
	// créer, ou relu quand ogs le tient déjà (D91, R2-21).
	password := func(user string, exists bool) (string, error) {
		if !p.needsPassword(proj.ID(), decl.Engine, user, exists) {
			return "", nil
		}
		pw, created, err := p.obtainPassword(proj.ID(), decl.Engine, user)
		if err == nil && created && onSeal != nil {
			onSeal(user)
		}
		return pw, err
	}
	mainPassword, err := password(decl.User, state.UserExists)
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
		stmts, err := createMySQL(spec)
		if err != nil {
			return err
		}
		all := stmts.All()
		alter, err := alterPluginStatement(decl, state, host, mainPassword)
		if err != nil {
			return err
		}
		if alter != "" {
			all = append(all, alter)
		}
		for _, stmt := range all {
			if err := client.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		for _, u := range decl.ExtraUsers {
			extraPriv, err := ParsePrivilege(u.Privileges)
			if err != nil {
				return err
			}
			extraPassword, err := password(u.User, state.ExtraUsers[u.User])
			if err != nil {
				return err
			}
			extra, err := createMySQL(MySQLSpec{
				Database: decl.Name, User: u.User, Host: host,
				Password: extraPassword, Privileges: extraPriv, AuthPlugin: decl.AuthPlugin,
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
		// Le joker en dernier : le GRANT échappé est en place, l'application ne
		// perd aucun droit dans l'intervalle.
		jokers, err := jokerRevokes(decl, state, host)
		if err != nil {
			return err
		}
		for _, stmt := range jokers {
			if err := client.Exec(ctx, stmt); err != nil {
				return err
			}
		}
	case "postgres":
		stmts, err := CreatePostgres(PostgresSpec{
			Database: decl.Name, Role: decl.User, Password: mainPassword,
			Extensions: decl.Extensions, Privileges: priv, Adopted: !state.CreatedByOGS(),
		}, state.UserExists, state.DBExists)
		if err != nil {
			return err
		}
		stmts.Cluster = append(stmts.Cluster, pgOwnership(decl, state)...)
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
			extraPassword, err := password(u.User, false)
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
	// Les limites de connexions en dernier : chaque compte existe (D127).
	limits, err := limitStatements(decl, state, host)
	if err != nil {
		return err
	}
	for _, stmt := range limits {
		if err := client.Exec(ctx, stmt); err != nil {
			return err
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
			// L'ancien compte sous l'hôte d'avant part avec lui (D100) ; un nom
			// présent sous plusieurs autres hôtes ne dit pas lequel ogs a créé.
			for _, other := range state.OtherHosts[user] {
				if !slices.Contains(state.Moved[user], other) {
					kept = append(kept, fmt.Sprintf("compte '%s'@'%s' (autre hôte que celui déclaré)", user, other))
				}
			}
		case exists:
			kept = append(kept, state.account(user))
		}
	}
	// Les comptes que la marque prouve partent aussi, sous l'hôte d'avant comme
	// sous un hôte quitté : entre l'apply qui a déplacé le compte et celui qui
	// aurait retiré l'ancien, la suppression du projet le laissait sans preuve.
	targets.moved = map[string][]string{}
	for user, hosts := range state.Moved {
		targets.moved[user] = append(targets.moved[user], hosts...)
	}
	for user, hosts := range state.Retired {
		targets.moved[user] = append(targets.moved[user], hosts...)
	}
	// Le mot de passe d'un compte supprimé part avec lui, au fil de chaque
	// suppression réussie — l'archive en garde la copie scellée. Laissé au
	// keystore, il ferait d'un homonyme recréé plus tard hors d'ogs le compte d'un
	// projet du même nom, repris puis supprimé avec lui ; et une suppression
	// interrompue en route ne doit pas garder ceux des comptes déjà partis.
	targets.dropped = func(user string) {
		if p.Store != nil {
			_ = p.Store.DeleteSecret(state.projectID, secretName(decl.Engine, user))
			_ = p.Store.DeleteSecret(state.projectID, movedName(decl.Engine, user))
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
	// moved associe à un compte MySQL l'hôte d'avant sous lequel ogs l'a créé
	// (DBState.Moved) : il part avec lui, avant que son mot de passe ne quitte le
	// keystore.
	moved map[string][]string
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
		for _, old := range t.moved[user] {
			if old == host || decl.Engine == "postgres" {
				continue
			}
			more, err := DropMySQL("", user, old, nil)
			if err != nil {
				return err
			}
			stmts = append(stmts, more...)
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

// movedName nomme la marque d'un compte MySQL qu'ogs a recréé sous un autre
// hôte (D100) : elle porte les hôtes sous lesquels ogs a tenu ce compte. Le
// compte d'un hôte quitté ne part qu'à l'apply suivant, une fois le remplaçant
// observé ; sans la marque, deux comptes de même nom sous deux hôtes ne disent
// pas lequel ogs a créé (D78).
func movedName(engine, user string) string { return engine + "-moved/" + user }

// sealMoved scelle la marque d'un compte déplacé : l'ensemble des hôtes sous
// lesquels ogs le tient.
func (p *Provider) sealMoved(project, engine, user string, hosts []string) error {
	if p.Store == nil || p.Keys == nil {
		return nil
	}
	set := map[string]bool{}
	var list []string
	for _, h := range hosts {
		if h != "" && !set[h] {
			set[h] = true
			list = append(list, h)
		}
	}
	sort.Strings(list)
	nonce, ciphertext, err := p.Keys.Seal(project, movedName(engine, user), []byte(strings.Join(list, "\n")))
	if err != nil {
		return err
	}
	return p.Store.PutSecret(project, movedName(engine, user), nonce, ciphertext)
}

// movedHosts rend les hôtes que la marque d'un compte déplacé porte, aucun sans
// marque.
func (p *Provider) movedHosts(project, engine, user string) []string {
	if p.Store == nil || p.Keys == nil {
		return nil
	}
	nonce, ciphertext, err := p.Store.GetSecret(project, movedName(engine, user))
	if err != nil {
		return nil
	}
	data, err := p.Keys.Open(project, movedName(engine, user), nonce, ciphertext)
	if err != nil {
		return nil
	}
	var out []string
	for _, h := range strings.Split(string(data), "\n") {
		if h != "" {
			out = append(out, h)
		}
	}
	return out
}

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
