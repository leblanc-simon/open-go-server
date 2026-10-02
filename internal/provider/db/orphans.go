package db

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/model"
)

// Ce fichier porte ce qu'une déclaration changée laisse sur les moteurs (D100) :
// un compte additionnel retiré, un compte principal renommé, un compte sous
// l'ancien hôte. Ils gardaient leurs droits indéfiniment, la suppression du
// projet comprise, qui ne connaissait que les comptes encore déclarés.
//
// La preuve reste celle de D91 : un compte n'est à ogs pour un projet que si son
// mot de passe est scellé dans la portée du projet. Le secret ne quitte le
// keystore qu'après le retrait du compte ; retiré avant, il ferait du compte resté
// en place un étranger pour toujours.

// accountEngines sont les préfixes des secrets de comptes SGBD (secretName).
var accountEngines = []string{"mysql", "mariadb", "postgres"}

// family ramène un moteur à son serveur : mysql et mariadb désignent le même.
func family(engine string) string {
	if engine == "mariadb" {
		return "mysql"
	}
	return engine
}

// sealedUsers rend, par moteur, les comptes dont le mot de passe est scellé dans
// la portée du projet. Une base d'état illisible ne rend rien : aucun compte n'est
// alors retiré, le sens sûr.
func (p *Provider) sealedUsers(project string) map[string][]string {
	out := map[string][]string{}
	if p.Store == nil {
		return out
	}
	names, err := p.Store.SecretNames(project)
	if err != nil {
		return out
	}
	for _, name := range names {
		for _, e := range accountEngines {
			if user, ok := strings.CutPrefix(name, e+"/"); ok && user != "" && !strings.Contains(user, "/") {
				out[e] = append(out[e], user)
			}
		}
	}
	return out
}

// HasAccounts dit si le projet tient des comptes SGBD scellés : un projet qui ne
// déclare plus aucune base doit encore voir retirés ceux qu'il a laissés.
func (p *Provider) HasAccounts(proj *model.Project) bool {
	return len(p.sealedUsers(proj.ID())) > 0
}

// declaredUsers rend les comptes que les déclarations du projet nomment, par
// famille de moteur.
func declaredUsers(proj *model.Project) map[string]bool {
	out := map[string]bool{}
	for _, d := range proj.Spec.Databases {
		out[family(d.Engine)+"/"+d.User] = true
		for _, u := range d.ExtraUsers {
			out[family(d.Engine)+"/"+u.User] = true
		}
	}
	return out
}

// observeOthers complète l'état d'une déclaration par les autres comptes à ogs
// du projet qui y ont leurs droits (Holders, Revoke), et par le compte resté sous
// l'ancien hôte (Moved).
func (p *Provider) observeOthers(ctx context.Context, client Client, proj *model.Project, state *DBState,
	users map[string]bool, host string) {
	decl := state.Declaration
	if decl.Engine != "postgres" {
		candidates := []string{decl.User}
		for _, u := range decl.ExtraUsers {
			candidates = append(candidates, u.User)
		}
		for i, user := range candidates {
			exists, owned := state.UserExists, state.UserOwned
			if i > 0 {
				exists, owned = state.ExtraUsers[user], state.ExtraOwned[user]
			}
			other := state.OtherHosts[user]
			// Un projet adopté ne déplace jamais un compte (D100) : le mot de passe
			// scellé n'y prouve pas qu'ogs l'a posé — tiré au premier apply d'un
			// compte qui existait, il n'est pas celui de l'application. Recréer le
			// compte sous l'hôte déclaré avec ce mot de passe, MySQL le préférant
			// par le socket, coupait l'application, et l'apply suivant détruisait le
			// compte réel. Le refus de D78 demeure.
			if exists || !owned || state.Adopted || len(other) == 0 || p.reserved(decl.Engine, user) != "" {
				continue
			}
			// Un seul autre hôte désigne le compte qu'ogs a créé ; plusieurs, s'ils
			// sont tous ceux que la marque d'un déplacement précédent porte — l'hôte
			// changé deux fois avant l'apply qui retire l'ancien. Sinon, le refus
			// de D78.
			if len(other) > 1 {
				marked := p.movedHosts(proj.ID(), decl.Engine, user)
				all := true
				for _, h := range other {
					all = all && slices.Contains(marked, h)
				}
				if !all {
					continue
				}
			}
			state.Moved[user] = other
			if i == 0 && state.DBExists {
				for _, h := range other {
					if grants, err := client.Grants(ctx, user, h); err == nil && grants[decl.Name] != "" {
						state.Holders = append(state.Holders, fmt.Sprintf("'%s'@'%s'", user, h))
					}
				}
			}
		}
	}
	if !state.DBExists {
		return
	}
	inDecl := map[string]bool{decl.User: true}
	for _, u := range decl.ExtraUsers {
		inDecl[u.User] = true
	}
	declared := declaredUsers(proj)
	sealed := p.sealedUsers(proj.ID())
	for _, e := range accountEngines {
		if family(e) != family(decl.Engine) {
			continue
		}
		for _, user := range sealed[e] {
			if inDecl[user] || p.reserved(decl.Engine, user) != "" {
				continue
			}
			if decl.Engine == "postgres" {
				if !users[user] {
					continue
				}
				if grants, err := client.Grants(ctx, user, ""); err == nil && grants[decl.Name] != "" {
					state.Holders = append(state.Holders, "rôle "+user)
					state.pgOwner = user
				}
				continue
			}
			if !users[user+"@"+host] {
				continue
			}
			grants, err := client.Grants(ctx, user, host)
			if err != nil || grants[decl.Name] == "" {
				continue
			}
			state.Holders = append(state.Holders, fmt.Sprintf("'%s'@'%s'", user, host))
			if declared[family(e)+"/"+user] {
				// Employé par une autre base du projet, il reste ; ses droits sur
				// celle-ci, d'où sa déclaration l'a retiré, partent.
				state.Revoke = append(state.Revoke, user)
			}
		}
	}
	sort.Strings(state.Revoke)
}

// pgOwnership rend la reprise de propriété d'une base PostgreSQL par son compte
// déclaré, quand elle appartient à un autre compte d'ogs du projet : l'ancien
// compte principal d'une déclaration dont user a changé.
func pgOwnership(decl model.Database, state DBState) []string {
	if decl.Engine != "postgres" || state.pgOwner == "" || !state.DBExists {
		return nil
	}
	return []string{fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", quotePGIdent(decl.Name), quotePGIdent(decl.User))}
}

// pgOwnershipRestore rend la requête qui rend la base à son ancien propriétaire,
// quand l'étape la lui a reprise.
func pgOwnershipRestore(decl model.Database, state DBState) string {
	if decl.Engine != "postgres" || state.pgOwner == "" || !state.DBExists {
		return ""
	}
	return fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", quotePGIdent(decl.Name), quotePGIdent(state.pgOwner))
}

// sortedListKeys rend les clés d'une table de listes, triées.
func sortedListKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// followSteps rend les étapes qui suivent celle d'une base : le retrait de
// l'ancien compte sous l'hôte d'avant, et la révocation des droits d'un compte
// que cette base ne déclare plus. Toutes sont non critiques : un reste en trop ne
// met pas le site en défaut, et compenser le déploiement pour lui défairait ce qui
// marche.
func (p *Provider) followSteps(resource string, decl model.Database, state DBState, host string) []*engine.Step {
	if decl.Engine == "postgres" {
		return nil
	}
	var steps []*engine.Step
	// L'ancien compte d'un compte recréé sous l'hôte déclaré reste jusqu'à
	// l'apply suivant (D100) : son remplaçant n'existe pas encore, et une saga
	// déroulée plus loin retirerait celui-ci — la base resterait sans compte.
	for _, user := range sortedListKeys(state.Moved) {
		olds := strings.Join(state.Moved[user], "', '")
		steps = append(steps, engine.NewStep(resource+"/"+user+"@"+olds, engine.NoOp, fmt.Sprintf(
			"ancien compte '%s' sous '%s' conservé jusqu'au prochain apply, après la création de '%s'@'%s'",
			user, olds, user, host)))
	}
	for _, user := range sortedListKeys(state.Retired) {
		user, olds := user, state.Retired[user]
		summary := fmt.Sprintf("retrait de l'ancien compte '%s' sous '%s' (hôte déclaré : %s)", user,
			strings.Join(olds, "', '"), host)
		if len(olds) == 0 {
			summary = fmt.Sprintf("marque de déplacement de '%s' oubliée : il ne vit plus que sous l'hôte déclaré %s",
				user, host)
		}
		s := engine.NewStep(resource+"/"+user+"@moved", engine.Delete, summary)
		s.Critical = false
		s.Diff = func() (string, error) {
			var all []string
			for _, old := range olds {
				stmts, err := DropMySQL("", user, old, nil)
				if err != nil {
					return "", err
				}
				all = append(all, stmts...)
			}
			return strings.Join(all, "\n"), nil
		}
		s.Apply = func(ctx context.Context) error {
			client, err := p.clientFor(decl)
			if err != nil {
				return err
			}
			users, err := client.Users(ctx)
			if err != nil {
				return err
			}
			if !users[user+"@"+host] {
				return fmt.Errorf("'%s'@'%s' n'existe pas : les anciens comptes de '%s' sont gardés", user, host, user)
			}
			for _, old := range olds {
				if old == host || !users[user+"@"+old] {
					continue
				}
				stmts, err := DropMySQL("", user, old, nil)
				if err != nil {
					return err
				}
				for _, stmt := range stmts {
					if err := client.Exec(ctx, stmt); err != nil {
						return err
					}
				}
			}
			if p.Store != nil {
				return p.Store.DeleteSecret(state.projectID, movedName(decl.Engine, user))
			}
			return nil
		}
		steps = append(steps, s)
	}
	for _, user := range state.Revoke {
		user := user
		stmt, err := RevokeMySQL(decl.Name, user, host)
		if err != nil {
			continue
		}
		s := engine.NewStep(resource+"/"+user, engine.Update, fmt.Sprintf(
			"révocation des droits de '%s'@'%s' sur %s, qui ne le déclare plus", user, host, decl.Name))
		s.Critical = false
		s.Diff = func() (string, error) { return stmt, nil }
		s.Apply = func(ctx context.Context) error {
			client, err := p.clientFor(decl)
			if err != nil {
				return err
			}
			return client.Exec(ctx, stmt)
		}
		steps = append(steps, s)
	}
	return steps
}

// Orphan est un compte dont le mot de passe est scellé dans la portée du projet
// et qu'aucune de ses déclarations ne nomme plus.
type Orphan struct {
	// Engine est le préfixe de son secret ; User son nom.
	Engine, User string
	// Host est, sous MySQL, l'hôte de l'unique compte de ce nom ; Cluster, sous
	// PostgreSQL, le cluster où le rôle a été trouvé.
	Host, Cluster string
	// Exists dit qu'un compte de ce nom, et un seul, existe sur le moteur.
	Exists bool
	// Candidates nomme les comptes de ce nom quand il y en a plusieurs — plusieurs
	// hôtes, plusieurs clusters : rien ne dit lequel ogs a créé.
	Candidates []string
	// Keeps nomme les bases présentes, non déclarées, où il a ses droits (la
	// propriété sous PostgreSQL) : il est la seule preuve qu'ogs les a créées, et
	// le retirer les rendrait étrangères à une redéclaration (db remove, puis db
	// add).
	Keeps []string
	// Waits nomme, sous MySQL, les bases déclarées où il a ses droits et dont le
	// compte principal n'existe pas encore — refusé, ou en échec : retiré, il
	// couperait l'application de sa base avant que son remplaçant n'existe.
	Waits []string
	// Reserved dit pourquoi le compte est réservé ; il n'est alors jamais retiré.
	Reserved string
	// Unavailable porte le motif d'un moteur injoignable ou d'un rôle qu'on ne
	// peut situer.
	Unavailable error
	// databases sont les déclarations PostgreSQL du même cluster, où ses objets
	// sont réattribués à leur propriétaire avant son retrait.
	databases []model.Database
}

// account nomme le compte orphelin comme le moteur le désigne.
func (o Orphan) account() string {
	if o.Engine == "postgres" {
		return "rôle " + o.User
	}
	if o.Host == "" {
		return "compte " + o.User
	}
	return fmt.Sprintf("compte '%s'@'%s'", o.User, o.Host)
}

// observeOrphans relève les comptes orphelins du projet.
func (p *Provider) observeOrphans(ctx context.Context, proj *model.Project) []Orphan {
	declared := declaredUsers(proj)
	sealed := p.sealedUsers(proj.ID())
	var out []Orphan
	for _, e := range accountEngines {
		for _, user := range sealed[e] {
			if declared[family(e)+"/"+user] {
				continue
			}
			out = append(out, p.observeOrphan(ctx, proj, e, user))
		}
	}
	return out
}

// observeOrphan situe un compte orphelin et dit s'il garde une base.
func (p *Provider) observeOrphan(ctx context.Context, proj *model.Project, engineName, user string) Orphan {
	o := Orphan{Engine: engineName, User: user, Reserved: p.reserved(engineName, user)}
	declaredDBs := map[string]bool{}
	for _, d := range proj.Spec.Databases {
		if family(d.Engine) == family(engineName) {
			declaredDBs[d.Name] = true
		}
	}
	if engineName != "postgres" {
		client, err := p.clientFor(model.Database{Engine: engineName})
		if err != nil {
			o.Unavailable = err
			return o
		}
		users, err := client.Users(ctx)
		if err != nil {
			o.Unavailable = err
			return o
		}
		var hosts []string
		for account := range users {
			if i := strings.LastIndex(account, "@"); i >= 0 && account[:i] == user {
				hosts = append(hosts, account[i+1:])
			}
		}
		sort.Strings(hosts)
		switch len(hosts) {
		case 0:
			return o
		case 1:
			o.Exists, o.Host = true, hosts[0]
		default:
			for _, h := range hosts {
				o.Candidates = append(o.Candidates, fmt.Sprintf("'%s'@'%s'", user, h))
			}
			return o
		}
		grants, err := client.Grants(ctx, user, o.Host)
		if err != nil {
			o.Unavailable = err
			return o
		}
		databases, err := client.Databases(ctx)
		if err != nil {
			o.Unavailable = err
			return o
		}
		for db, privs := range grants {
			if db == GlobalGrantKey || privs == "" || !databases[db] {
				continue
			}
			if !declaredDBs[db] {
				o.Keeps = append(o.Keeps, db)
				continue
			}
			// Une base déclarée dont le compte principal n'est pas en place — sa
			// création refusée (un homonyme étranger) ou en échec : l'ancien compte
			// est encore le seul par lequel l'application y accède.
			adopted := proj.Metadata.Labels["adopted"] == "true"
			for _, d := range proj.Spec.Databases {
				host := d.Host
				if host == "" {
					host = "localhost"
				}
				ready := users[d.User+"@"+host] && (adopted || p.sealed(proj.ID(), d.Engine, d.User))
				if family(d.Engine) == family(engineName) && d.Name == db && !ready {
					o.Waits = append(o.Waits, db)
					break
				}
			}
		}
		sort.Strings(o.Keeps)
		sort.Strings(o.Waits)
		return o
	}

	// PostgreSQL : le secret ne dit pas le cluster. Seuls comptent ceux que les
	// déclarations du projet emploient — un rôle homonyme d'un autre cluster n'est
	// pas le sien —, ou l'unique cluster de la machine quand plus aucune ne le dit.
	clusters := map[string]bool{}
	for _, d := range proj.Spec.Databases {
		if d.Engine == "postgres" {
			clusters[d.Cluster] = true
		}
	}
	if len(clusters) == 0 {
		if n := len(DetectClusters(p.Paths.Prefix())); n > 1 {
			o.Unavailable = fmt.Errorf("%d clusters PostgreSQL et plus aucune base déclarée : rien ne dit où "+
				"vit le rôle %s", n, user)
			return o
		}
		clusters[""] = true
	}
	names := make([]string, 0, len(clusters))
	for c := range clusters {
		names = append(names, c)
	}
	sort.Strings(names)
	var found []string
	for _, c := range names {
		client, err := p.clientFor(model.Database{Engine: "postgres", Cluster: c})
		if err != nil {
			o.Unavailable = err
			return o
		}
		users, err := client.Users(ctx)
		if err != nil {
			o.Unavailable = err
			return o
		}
		if users[user] {
			found = append(found, c)
		}
	}
	switch len(found) {
	case 0:
		return o
	case 1:
		o.Exists, o.Cluster = true, found[0]
	default:
		for _, c := range found {
			o.Candidates = append(o.Candidates, fmt.Sprintf("rôle %s du cluster %q", user, c))
		}
		return o
	}
	for _, d := range proj.Spec.Databases {
		if d.Engine == "postgres" && d.Cluster == o.Cluster {
			o.databases = append(o.databases, d)
		}
	}
	client, err := p.clientFor(model.Database{Engine: "postgres", Cluster: o.Cluster})
	if err != nil {
		o.Unavailable = err
		return o
	}
	owned, err := client.Grants(ctx, user, "")
	if err != nil {
		o.Unavailable = err
		return o
	}
	pgUsers, err := client.Users(ctx)
	if err != nil {
		o.Unavailable = err
		return o
	}
	adopted := proj.Metadata.Labels["adopted"] == "true"
	for db := range owned {
		if !declaredDBs[db] {
			o.Keeps = append(o.Keeps, db)
			continue
		}
		// Une base déclarée qu'il possède encore, dont le compte déclaré n'existe
		// pas : la reprise de propriété attend l'apply qui le crée, le retrait de
		// l'ancien l'apply suivant (D100).
		for _, d := range o.databases {
			if d.Name == db && !(pgUsers[d.User] && (adopted || p.sealed(proj.ID(), d.Engine, d.User))) {
				o.Waits = append(o.Waits, db)
			}
		}
	}
	sort.Strings(o.Keeps)
	sort.Strings(o.Waits)
	return o
}

// orphanSteps rend les étapes des comptes orphelins : le retrait de ceux qu'ogs a
// créés, le retrait du seul secret d'un compte qui n'existe plus, et le refus,
// nommé, de ce qu'on ne peut juger. Un compte réservé n'a pas d'étape (D91), ni un
// moteur injoignable — la base déclarée le dit déjà, et plus aucune ne l'est
// peut-être.
func (p *Provider) orphanSteps(proj *model.Project, orphans []Orphan) []*engine.Step {
	var steps []*engine.Step
	for _, o := range orphans {
		o := o
		resource := fmt.Sprintf("db/%s/compte/%s", o.Engine, o.User)
		switch {
		case o.Reserved != "", o.Unavailable != nil:
			continue
		case len(o.Candidates) > 0:
			s := engine.NewStep(resource, engine.Delete, fmt.Sprintf(
				"compte %s %s retiré de la déclaration — plusieurs homonymes, conservés", o.Engine, o.User))
			s.Critical = false
			reason := fmt.Errorf("%s n'est plus déclaré, mais %s existent : rien ne dit lequel ogs a créé. "+
				"Retirer à la main ceux qui sont sans objet ; ogs n'y touche pas",
				o.User, strings.Join(o.Candidates, ", "))
			s.Apply = func(context.Context) error { return reason }
			steps = append(steps, s)
		case len(o.Keeps) > 0:
			steps = append(steps, engine.NewStep(resource, engine.NoOp, fmt.Sprintf(
				"%s conservé : il porte les droits de %s, retirée de la déclaration mais présente — "+
					"seule preuve qu'ogs l'a créée", o.account(), strings.Join(o.Keeps, ", "))))
		case len(o.Waits) > 0:
			// Le remplaçant n'existait pas à l'observation : l'apply qui le crée ne
			// retire pas l'ancien (D100). Une saga déroulée plus loin retirerait le
			// nouveau compte, et la base resterait sans aucun — tenue ensuite pour
			// étrangère. L'apply suivant, qui l'observe, retire l'ancien.
			steps = append(steps, engine.NewStep(resource, engine.NoOp, fmt.Sprintf(
				"%s, qui n'est plus déclaré, conservé jusqu'au prochain apply : le compte déclaré de %s "+
					"n'existe pas encore", o.account(), strings.Join(o.Waits, ", "))))
		case o.Exists && proj.Metadata.Labels["adopted"] == "true":
			// Un projet adopté : ogs n'a pas la propriété de ses comptes, même
			// scellés par une rotation (D77). L'apply ne les retire jamais — son
			// --force reprend un fichier retouché, il ne vaut pas consentement à
			// supprimer un compte, comme D86 l'a tranché pour le pare-feu ; seul
			// « ogs project delete --force » les emporte.
			steps = append(steps, engine.NewStep(resource, engine.NoOp, fmt.Sprintf(
				"%s, qui n'est plus déclaré, conservé : compte d'un projet adopté, retiré seulement par "+
					"« ogs project delete %s --force »", o.account(), proj.ID())))
		case !o.Exists:
			s := engine.NewStep(resource, engine.Delete, fmt.Sprintf(
				"mot de passe scellé de %s %s retiré : le compte n'existe plus", o.Engine, o.User))
			s.Critical = false
			s.Apply = func(ctx context.Context) error {
				_, err := p.dropOrphan(ctx, proj, o)
				return err
			}
			steps = append(steps, s)
		default:
			s := engine.NewStep(resource, engine.Delete, fmt.Sprintf(
				"retrait du %s, qui n'est plus déclaré — l'application qui s'en sert encore perd l'accès",
				o.account()))
			s.Critical = false
			s.Diff = func() (string, error) { return strings.Join(orphanStatements(o), "\n"), nil }
			s.Apply = func(ctx context.Context) error {
				_, err := p.dropOrphan(ctx, proj, o)
				return err
			}
			steps = append(steps, s)
		}
	}
	return steps
}

// orphanStatements rend les requêtes du retrait d'un compte orphelin, hors celles
// de PostgreSQL qui s'exécutent dans chaque base (orphanInDatabase).
func orphanStatements(o Orphan) []string {
	if o.Engine == "postgres" {
		var out []string
		for _, d := range o.databases {
			for _, stmt := range orphanInDatabase(o.User, d.User) {
				out = append(out, fmt.Sprintf("[%s] %s", d.Name, stmt))
			}
		}
		stmts, _ := DropPostgres("", o.User, nil, false)
		return append(out, stmts...)
	}
	stmts, _ := DropMySQL("", o.User, o.Host, nil)
	return stmts
}

// orphanInDatabase rend, pour une base PostgreSQL, la réattribution des objets
// d'un rôle à son propriétaire puis le retrait de ses droits. DROP ROLE refuse un
// rôle qui possède un objet ou porte un droit ; DROP OWNED seul supprimerait les
// tables qu'il a créées, d'où la réattribution d'abord. Le rôle ne possède plus
// aucune base à ce stade (dropOrphan l'a vérifié) : REASSIGN OWNED, qui touche
// aussi aux bases du cluster, ne déplace rien d'autre.
func orphanInDatabase(role, owner string) []string {
	return []string{
		fmt.Sprintf("REASSIGN OWNED BY %s TO %s", quotePGIdent(role), quotePGIdent(owner)),
		fmt.Sprintf("DROP OWNED BY %s", quotePGIdent(role)),
	}
}

// dropOrphan retire un compte orphelin, puis son secret. L'état est relu : ce qui
// a changé depuis le plan — un homonyme apparu, une base gardée — se refuse. Il
// rend vrai si le compte a été retiré (et non seulement son secret).
func (p *Provider) dropOrphan(ctx context.Context, proj *model.Project, planned Orphan) (bool, error) {
	o := p.observeOrphan(ctx, proj, planned.Engine, planned.User)
	switch {
	case o.Unavailable != nil:
		return false, o.Unavailable
	case o.Reserved != "":
		if o.Exists {
			return false, fmt.Errorf("%s est réservé (%s) : ogs ne le retire jamais", o.account(), o.Reserved)
		}
	case len(o.Candidates) > 0:
		return false, fmt.Errorf("%s : %s existent, rien ne dit lequel ogs a créé", o.User,
			strings.Join(o.Candidates, ", "))
	case len(o.Keeps) > 0:
		return false, fmt.Errorf("%s conservé : il porte les droits de %s, présente hors de la déclaration",
			o.account(), strings.Join(o.Keeps, ", "))
	case len(o.Waits) > 0:
		return false, fmt.Errorf("%s conservé : le compte déclaré de %s n'existe pas, et il est encore le seul "+
			"à y accéder", o.account(), strings.Join(o.Waits, ", "))
	}
	if o.Exists {
		if err := p.dropOrphanAccount(ctx, o); err != nil {
			return false, err
		}
	}
	if p.Store != nil {
		if err := p.Store.DeleteSecret(proj.ID(), secretName(o.Engine, o.User)); err != nil {
			return o.Exists, err
		}
	}
	return o.Exists, nil
}

func (p *Provider) dropOrphanAccount(ctx context.Context, o Orphan) error {
	if o.Engine != "postgres" {
		client, err := p.clientFor(model.Database{Engine: o.Engine})
		if err != nil {
			return err
		}
		stmts, err := DropMySQL("", o.User, o.Host, nil)
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
	client, err := p.clientFor(model.Database{Engine: "postgres", Cluster: o.Cluster})
	if err != nil {
		return err
	}
	// Une base déclarée qu'il possède encore — la reprise de propriété a échoué —
	// le garde : REASSIGN OWNED la déplacerait sans que personne l'ait voulu.
	owned, err := client.Grants(ctx, o.User, "")
	if err != nil {
		return err
	}
	if len(owned) > 0 {
		names := make([]string, 0, len(owned))
		for db := range owned {
			names = append(names, db)
		}
		sort.Strings(names)
		return fmt.Errorf("le rôle %s possède encore %s : il est gardé", o.User, strings.Join(names, ", "))
	}
	databases, err := client.Databases(ctx)
	if err != nil {
		return err
	}
	for _, d := range o.databases {
		if !databases[d.Name] {
			continue
		}
		for _, stmt := range orphanInDatabase(o.User, d.User) {
			if err := client.ExecIn(ctx, d.Name, stmt); err != nil {
				return err
			}
		}
	}
	stmts, err := DropPostgres("", o.User, nil, false)
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

// OrphanResult est le sort d'un compte orphelin à la suppression du projet.
type OrphanResult struct {
	Orphan Orphan
	// Dropped dit que le compte a été retiré ; Forgotten que seul son secret l'a
	// été, le compte n'existant plus.
	Dropped, Forgotten bool
	// Kept dit pourquoi le compte et son secret restent.
	Kept error
	// Unreachable dit que le moteur est injoignable alors que le projet n'en
	// déclare plus aucune base : rien ne dit qu'un compte y reste, mais rien ne
	// dit le contraire — le secret est gardé, sans en faire un défaut de la
	// suppression (un moteur désinstallé après le retrait de ses bases).
	Unreachable bool
}

// DropOrphans retire, à la suppression du projet, les comptes orphelins qu'ogs a
// créés, avec leur secret, et le secret de ceux qui n'existent plus. Ce qui
// reste — compte réservé, moteur injoignable, homonymes, base non déclarée qu'il
// porte, retrait en échec — garde son secret : il prouve une ressource encore
// présente. Les bases déclarées doivent avoir été supprimées avant (Drop).
func (p *Provider) DropOrphans(ctx context.Context, proj *model.Project) []OrphanResult {
	used := map[string]bool{}
	for _, d := range proj.Spec.Databases {
		used[family(d.Engine)] = true
	}
	var out []OrphanResult
	for _, o := range p.observeOrphans(ctx, proj) {
		r := OrphanResult{Orphan: o, Unreachable: o.Unavailable != nil && !used[family(o.Engine)]}
		if o.Reserved != "" && o.Exists {
			r.Kept = fmt.Errorf("%s est réservé (%s)", o.account(), o.Reserved)
			out = append(out, r)
			continue
		}
		if o.Exists && proj.Metadata.Labels["adopted"] == "true" && !p.Force {
			r.Kept = fmt.Errorf("%s : compte d'un projet adopté, retiré seulement sous « project delete --force » (D77)",
				o.account())
			out = append(out, r)
			continue
		}
		dropped, err := p.dropOrphan(ctx, proj, o)
		switch {
		case err != nil && !dropped:
			r.Kept = err
		case dropped:
			r.Dropped = true
			r.Kept = err
		default:
			r.Forgotten = true
		}
		out = append(out, r)
	}
	return out
}
