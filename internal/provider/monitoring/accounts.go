package monitoring

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/provider/db"
	"leblanc.io/open-go-server/internal/secret"
)

// Credentials porte ce que le fragment de l'agent doit savoir des moteurs. Un
// moteur absent de la machine n'y figure pas : mieux vaut une entrée manquante
// qu'une entrée qui échoue toutes les trente secondes dans le journal de l'agent.
type Credentials struct {
	MySQL    *MySQLAccess
	Postgres []PostgresAccess
	RabbitMQ *RabbitAccess
}

// MySQLAccess est le compte de supervision MySQL et le socket où l'atteindre.
type MySQLAccess struct {
	User     string
	Password string
	Socket   string
	// posed dit que le moteur a ce compte sous le mot de passe scellé : observé
	// conforme, ou posé par l'étape qui vient de réussir. Le secret est scellé
	// avant la pose (D93) ; il ne suffit donc plus à le dire.
	posed bool
}

// PostgresAccess est un cluster et le rôle sous lequel l'agent s'y connecte. Il
// n'y a pas de mot de passe : la connexion est locale et authentifiée « peer ».
type PostgresAccess struct {
	Cluster string
	Role    string
	Port    int
	// Socket est le répertoire du socket d'écoute. Vide, le cluster est déclaré
	// mais n'écoute pas : l'entrée n'est pas générée.
	Socket string
}

// RabbitAccess est le compte de supervision du broker et l'URL de son API.
type RabbitAccess struct {
	URL      string
	User     string
	Password string
	// posed : voir MySQLAccess.
	posed bool
}

// planAccounts réconcilie les comptes de supervision des moteurs installés.
//
// Chaque étape est **non critique** : un moteur injoignable ne doit pas faire
// échouer un apply, encore moins déclencher la compensation des étapes qui l'ont
// précédé. La supervision est une commodité, pas une dépendance d'un site.
func (p *Provider) planAccounts(ctx context.Context) ([]*engine.Step, Credentials) {
	var steps []*engine.Step
	var creds Credentials

	if p.Cfg.Monitoring.InputEnabled("mysql") {
		s, access := p.planMySQLAccount(ctx)
		if s != nil {
			steps = append(steps, s)
		}
		creds.MySQL = access
	}
	if p.Cfg.Monitoring.InputEnabled("postgres") {
		s, access := p.planPostgresAccounts(ctx)
		steps = append(steps, s...)
		creds.Postgres = access
	}
	if p.Cfg.Monitoring.InputEnabled("rabbitmq") {
		s, access := p.planRabbitAccount(ctx)
		if s != nil {
			steps = append(steps, s)
		}
		creds.RabbitMQ = access
	}
	return steps, creds
}

// secretName nomme le secret d'un compte de supervision : le moteur, puis le
// compte. Un seul endroit le compose — la planification du compte et le filtre
// du fragment doivent viser le même secret, faute de quoi le fragment porterait
// éternellement un mot de passe absent du keystore.
func secretName(engine, account string) string { return engine + "/" + account }

// sealedCredentials ne garde que les identifiants dont le mot de passe est
// **scellé**, et rend celui du keystore plutôt que celui du plan.
//
// C'est l'invariant du fragment : un mot de passe non scellé n'a jamais été posé
// sur le moteur — l'étape du compte reste à appliquer (elle scelle avant de poser,
// D93), ou son scellement même a échoué. L'y
// écrire donnerait à l'agent un identifiant que le moteur refuse, et, comme un
// secret non scellé est tiré au hasard à chaque planification, le fichier
// changerait à chaque apply sans jamais converger : « diff » resterait en code 6
// et l'agent serait relancé pour rien. L'entrée manquante est le sens sûr — elle
// revient dès que le compte est posé.
//
// PostgreSQL n'a pas de mot de passe : la connexion locale est authentifiée
// « peer », il n'y a donc rien à filtrer.
func (p *Provider) sealedCredentials(creds Credentials) Credentials {
	out := creds
	out.MySQL, out.RabbitMQ = nil, nil
	if creds.MySQL != nil && creds.MySQL.posed {
		if password, sealed, err := p.lookupSecret(secretName("mysql", creds.MySQL.User)); err == nil && sealed {
			access := *creds.MySQL
			access.Password = password
			out.MySQL = &access
		}
	}
	if creds.RabbitMQ != nil && creds.RabbitMQ.posed {
		if password, sealed, err := p.lookupSecret(secretName("rabbitmq", creds.RabbitMQ.User)); err == nil && sealed {
			access := *creds.RabbitMQ
			access.Password = password
			out.RabbitMQ = &access
		}
	}
	return out
}

// engineClient ouvre le client d'administration d'un moteur. Substituable en test :
// un moteur simulé se branche ici, le provider de bases n'en exposant aucun.
var engineClient = func(p *db.Provider, engine, cluster string) (db.Client, error) {
	return p.EngineClient(engine, cluster)
}

// planMySQLAccount crée ou met à jour le compte de métriques MySQL. Le moteur
// est réputé absent lorsqu'aucun socket n'existe — et alors ogs se tait : une
// machine sans MySQL n'a pas à voir un avertissement à chaque apply. Un socket
// vu puis refusé (D92) n'est pas une absence : l'étape le dit.
func (p *Provider) planMySQLAccount(ctx context.Context) (*engine.Step, *MySQLAccess) {
	if p.DB == nil {
		return nil, nil
	}
	resource := "monitoring/account/mysql"
	socket, err := db.MySQLSocket(p.Paths.Prefix())
	if err != nil {
		return unavailableStep(resource, "compte de supervision MySQL", err), nil
	}
	if socket == "" {
		return nil, nil
	}
	account := p.Cfg.Monitoring.TelegrafAccount()

	client, err := engineClient(p.DB, "mysql", "")
	if err != nil {
		return unavailableStep(resource, "compte de supervision MySQL", err), nil
	}
	name := secretName("mysql", account)
	password, sealed, err := p.lookupSecret(name)
	if err != nil {
		return unavailableStep(resource, "compte de supervision MySQL", err), nil
	}
	// Moteur momentanément injoignable, l'entrée d'un compte déjà scellé reste au
	// fragment : la retirer, puis la remettre, relancerait l'agent deux fois.
	access := &MySQLAccess{User: account, Password: password, Socket: socket, posed: sealed}

	users, err := client.Users(ctx)
	if err != nil {
		return unavailableStep(resource, "compte de supervision MySQL", err), access
	}
	access.posed = sealed && users[account+"@localhost"]
	// Un compte qui existe sans que son mot de passe soit scellé n'est pas à ogs
	// (D93) : l'administrateur l'a créé pour sa propre collecte, ou la base d'état
	// a été restaurée sans ses secrets. Le reprendre, c'était un ALTER USER au mot
	// de passe neuf, et la collecte de l'administrateur cassée sans un mot. Aucun
	// mot de passe n'est scellé, donc aucune entrée MySQL au fragment : l'agent ne
	// reçoit pas un identifiant que le moteur refuserait.
	if users[account+"@localhost"] && !sealed {
		return foreignAccountStep(resource, "MySQL", account+"@localhost"), nil
	}
	conforming := users[account+"@localhost"] && sealed
	// Les versions qui posaient « GRANT SELECT ON `performance_schema`.* » sans
	// échappement accordaient un motif, où le souligné est un joker : le compte
	// lisait aussi une base nommée « performanceXschema », qu'un projet adopté
	// peut porter — l'inverse de ce que promet D67. GRANT étant additif, la ligne
	// survit à tous les apply : elle est relue à part (D111) et retirée, après le
	// GRANT échappé, sur tout compte existant qui la porte.
	var joker, jokerPrivs string
	if users[account+"@localhost"] {
		// Le mot de passe scellé et celui du moteur ne se comparent pas : MySQL ne
		// rend que l'empreinte. Les privilèges, eux, se vérifient — et c'est ce qui
		// dérive en pratique, un moteur mis à jour perdant parfois ses GRANT. Un
		// échec de lecture est une raison de repasser, pas de se déclarer conforme.
		grants, err := client.Grants(ctx, account, "localhost")
		conforming = conforming && err == nil && mysqlGrantsConform(grants)
		if err == nil {
			joker, jokerPrivs = db.MySQLJokerPattern(grants, db.MonitoringMySQLSchema)
		}
		conforming = conforming && joker == ""
	}
	if conforming {
		return engine.NewStep(resource, engine.NoOp,
			fmt.Sprintf("compte de supervision MySQL %s", account)), access
	}

	action := engine.Update
	if !users[account+"@localhost"] {
		action = engine.Create
	}
	// Le joker relevé part après le GRANT échappé : le compte ne perd pas son
	// accès à performance_schema dans l'intervalle.
	var legacy string
	if joker != "" {
		if stmt, err := db.RevokeMySQLJoker(joker, jokerPrivs, account, "localhost"); err == nil {
			legacy = stmt
		}
	}
	s := engine.NewStep(resource, action,
		fmt.Sprintf("compte de supervision MySQL %s (PROCESS, REPLICATION CLIENT, performance_schema)", account))
	s.Critical = false
	s.Diff = func() (string, error) {
		// Le mot de passe ne figure jamais dans un diff : il est affiché et
		// journalisé.
		stmts, err := db.MonitoringMySQL(account, "localhost", "<scellé>")
		if err != nil {
			return "", err
		}
		if legacy != "" {
			stmts = append(stmts, legacy)
		}
		return strings.Join(stmts, ";\n") + ";", nil
	}
	s.Apply = func(ctx context.Context) error {
		// Le scellement est un acte d'application, pas de calcul — « ogs plan »
		// n'écrit rien —, et il **précède** le moteur (D93), comme pour le compte
		// AMQP d'un projet. Le secret scellé est la preuve que le compte est à
		// ogs : scellé après, un CREATE USER passé suivi d'un GRANT en échec, ou un
		// contexte annulé entre les deux, laissait un compte d'ogs sans preuve,
		// tenu pour celui de l'administrateur et refusé pour toujours. Scellé
		// avant, l'échec se répare de lui-même : le compte absent est recréé, le
		// compte aux droits incomplets repassé, toujours sous ce mot de passe. Le
		// prix est un fragment qui peut porter, jusqu'au prochain apply, le mot de
		// passe d'un compte que le moteur n'a pas encore — l'étape en échec le dit.
		if !sealed {
			if err := p.sealSecret(name, password); err != nil {
				return err
			}
		}
		stmts, err := db.MonitoringMySQL(account, "localhost", password)
		if err != nil {
			return err
		}
		if legacy != "" {
			stmts = append(stmts, legacy)
		}
		for _, stmt := range stmts {
			if err := client.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		access.posed = true
		return nil
	}
	return s, access
}

// planPostgresAccounts crée le rôle de supervision sur chaque cluster détecté.
// Le rôle porte le nom de l'utilisateur système de l'agent, et non celui du
// compte configuré : la connexion locale est authentifiée « peer », qui compare
// l'utilisateur du système (D44). Un rôle nommé autrement serait créé, puis
// refusé à chaque collecte.
func (p *Provider) planPostgresAccounts(ctx context.Context) ([]*engine.Step, []PostgresAccess) {
	if p.DB == nil {
		return nil, nil
	}
	clusters := db.DetectClusters(p.Paths.Prefix())
	if len(clusters) == 0 {
		return nil, nil
	}
	role := p.Cfg.Monitoring.TelegrafUser()

	var steps []*engine.Step
	var access []PostgresAccess
	for _, cluster := range clusters {
		resource := "monitoring/account/postgres/" + cluster.ID()
		summary := fmt.Sprintf("rôle de supervision PostgreSQL %s sur %s (pg_monitor)", role, cluster.ID())

		client, err := p.DB.EngineClient("postgres", cluster.ID())
		if err != nil {
			steps = append(steps, unavailableStep(resource, summary, err))
			continue
		}
		access = append(access, PostgresAccess{Cluster: cluster.ID(), Role: role,
			Port: cluster.Port, Socket: cluster.Socket})

		users, err := client.Users(ctx)
		if err != nil {
			steps = append(steps, unavailableStep(resource, summary, err))
			continue
		}
		if users[role] {
			// Le rôle existe : l'appartenance à pg_monitor n'est pas relue, les
			// requêtes étant idempotentes et le coût d'un GRANT nul. Ce qui compte
			// est de ne pas afficher un changement là où il n'y en a pas.
			steps = append(steps, engine.NewStep(resource, engine.NoOp, summary))
			continue
		}
		s := engine.NewStep(resource, engine.Create, summary)
		s.Critical = false
		s.Diff = func() (string, error) {
			stmts, err := db.MonitoringPostgres(role)
			if err != nil {
				return "", err
			}
			return strings.Join(stmts, ";\n") + ";", nil
		}
		s.Apply = func(ctx context.Context) error {
			stmts, err := db.MonitoringPostgres(role)
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
		steps = append(steps, s)
	}
	return steps, access
}

// planRabbitAccount crée le compte de supervision du broker. Le tag
// « monitoring » suffit : il donne accès en lecture à l'ensemble des vhosts par
// l'API d'administration, sans aucune permission de publication ni de
// consommation. Aucune permission par vhost n'est donc posée — en poser une
// donnerait au compte le droit de lire les messages des projets.
func (p *Provider) planRabbitAccount(ctx context.Context) (*engine.Step, *RabbitAccess) {
	if p.AMQP == nil || p.AMQP.Client == nil {
		return nil, nil
	}
	account := p.Cfg.Monitoring.TelegrafAccount()
	resource := "monitoring/account/rabbitmq"
	summary := fmt.Sprintf("compte de supervision RabbitMQ %s (tag monitoring)", account)

	if _, err := p.AMQP.Client.Overview(ctx); err != nil {
		return unavailableStep(resource, summary, err), nil
	}
	name := secretName("rabbitmq", account)
	password, sealed, err := p.lookupSecret(name)
	if err != nil {
		return unavailableStep(resource, summary, err), nil
	}
	access := &RabbitAccess{URL: p.AMQP.ManagementURL, User: account, Password: password, posed: sealed}

	users, err := p.AMQP.Client.Users(ctx)
	if err != nil {
		return unavailableStep(resource, summary, err), access
	}
	access.posed = false
	// Même règle que côté MySQL (D93) : un compte homonyme sans mot de passe
	// scellé est celui de l'administrateur. Le « réparer » remplaçait son mot de
	// passe et ses tags — un compte « administrator » ramené à « monitoring ».
	action := engine.Create
	for _, u := range users {
		if u.Name != account {
			continue
		}
		if !sealed {
			return foreignAccountStep(resource, "RabbitMQ", account), nil
		}
		access.posed = true
		if tagsContain(u.Tags, "monitoring") {
			return engine.NewStep(resource, engine.NoOp, summary), access
		}
		action = engine.Update
	}
	s := engine.NewStep(resource, action, summary)
	s.Critical = false
	s.Diff = func() (string, error) {
		return fmt.Sprintf("PUT /api/users/%s {\"tags\": \"monitoring\", \"password\": \"<scellé>\"}", account), nil
	}
	s.Apply = func(ctx context.Context) error {
		// Scellé avant le broker, pour la même raison que côté MySQL : c'est la
		// preuve que le compte est à ogs, et elle ne doit pas manquer à un compte
		// créé par une pose interrompue.
		if !sealed {
			if err := p.sealSecret(name, password); err != nil {
				return err
			}
		}
		if err := p.AMQP.Client.EnsureUser(ctx, account, password, []string{"monitoring"}); err != nil {
			return err
		}
		access.posed = true
		return nil
	}
	return s, access
}

// foreignAccountStep refuse de reprendre un compte de supervision homonyme qu'ogs
// n'a pas créé. Non critique, comme toute étape de supervision : elle échoue pour
// être rapportée (code 6), et le fragment de l'agent reste sans cette entrée.
func foreignAccountStep(resource, engineName, account string) *engine.Step {
	s := engine.NewStep(resource, engine.Update, fmt.Sprintf(
		"compte de supervision %s %s — refusé : il existe sans avoir été créé par ogs", engineName, account))
	s.Critical = false
	reason := fmt.Errorf("le compte %s %s existe déjà, et ogs ne l'a pas créé (aucun mot de passe scellé) : "+
		"ogs ne change ni son mot de passe ni ses droits. Nommer autrement les comptes de supervision "+
		"(monitoring.telegraf.account dans config.yaml), ou retirer ce compte du moteur s'il n'a plus "+
		"d'usage", engineName, account)
	s.Apply = func(context.Context) error { return reason }
	return s
}

// unavailableStep produit l'étape d'un moteur installé mais injoignable. Elle
// échoue à l'application — pour être rapportée — sans être critique.
func unavailableStep(resource, summary string, cause error) *engine.Step {
	s := engine.NewStep(resource, engine.Create, summary+" — moteur indisponible")
	s.Critical = false
	s.Apply = func(context.Context) error { return cause }
	return s
}

// hasAllGrants vérifie que chaque privilège attendu figure dans les privilèges
// observés, sous l'une au moins de ses orthographes. La comparaison est par
// sous-chaîne : un SHOW GRANTS mêle les privilèges sur une même ligne, et les
// deux moteurs ne les nomment pas pareil.
func hasAllGrants(observed map[string]string, wanted [][]string) bool {
	var all strings.Builder
	for _, v := range observed {
		all.WriteString(strings.ToUpper(v))
		all.WriteString(" ")
	}
	text := all.String()
	for _, alternatives := range wanted {
		found := false
		for _, w := range alternatives {
			if strings.Contains(text, w) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// mysqlGrantsConform juge les privilèges observés du compte de supervision. Les
// deux jeux se comparent séparément, et c'est tout l'enjeu : PROCESS et
// REPLICATION CLIENT sont accordés « ON *.* », donc rendus sous GlobalGrantKey,
// tandis que le SELECT porte sur performance_schema. Les chercher pêle-mêle
// laisserait un SELECT accordé sur la base d'un projet tenir lieu de conformité.
func mysqlGrantsConform(observed map[string]string) bool {
	return hasAllGrants(map[string]string{db.GlobalGrantKey: observed[db.GlobalGrantKey]},
		db.MonitoringMySQLGrants) &&
		hasSchemaGrants(observed, db.MonitoringMySQLSchemaGrants)
}

// hasSchemaGrants vérifie les privilèges attendus base par base. Les chercher
// dans l'ensemble concaténé, comme le fait hasAllGrants, rendrait un SELECT
// accordé sur n'importe quelle base suffisant — y compris celle d'un projet,
// alors que c'est précisément ce que le compte ne doit pas atteindre.
func hasSchemaGrants(observed map[string]string, wanted map[string][]string) bool {
	for schema, privs := range wanted {
		text := strings.ToUpper(observed[schema])
		for _, w := range privs {
			if !strings.Contains(text, w) {
				return false
			}
		}
	}
	return true
}

// tagsContain cherche un tag dans la valeur rendue par l'API, qui est tantôt une
// chaîne séparée par des virgules, tantôt une liste.
func tagsContain(tags any, want string) bool {
	switch v := tags.(type) {
	case string:
		for _, t := range strings.Split(v, ",") {
			if strings.TrimSpace(t) == want {
				return true
			}
		}
	case []any:
		for _, t := range v {
			if s, ok := t.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// lookupSecret retrouve un mot de passe de parc, ou en engendre un **en mémoire**
// s'il n'existe pas encore. Le second retour dit lequel des deux : un secret déjà
// scellé n'est jamais régénéré — le régénérer laisserait l'agent avec l'ancien
// jusqu'au prochain rechargement, et personne pour le dire.
//
// Rien n'est écrit ici : la planification ne modifie pas l'état. « ogs plan »
// annonce ne rien appliquer, et laissait pourtant dans le keystore un mot de
// passe de parc que seul « ogs secrets show @parc » retrouvait — non journalisé,
// non compensable, et créé même quand l'exploitant renonçait à l'apply. Le
// scellement appartient au Step.Apply du compte, comme partout ailleurs dans le
// dépôt.
func (p *Provider) lookupSecret(name string) (string, bool, error) {
	if p.Store == nil || p.Keys == nil {
		return "", false, errors.New("keystore indisponible : impossible de gérer le mot de passe de supervision")
	}
	nonce, ciphertext, err := p.Store.GetSecret(SecretScope, name)
	if err == nil {
		plain, err := p.Keys.Open(SecretScope, name, nonce, ciphertext)
		if err != nil {
			return "", false, err
		}
		return string(plain), true, nil
	}
	password, err := secret.Password(0)
	if err != nil {
		return "", false, err
	}
	return password, false, nil
}

// sealSecret enregistre un mot de passe de parc. Il est appelé depuis l'étape du
// compte, une fois le moteur réglé : sceller est un acte d'application.
func (p *Provider) sealSecret(name, password string) error {
	if p.Store == nil || p.Keys == nil {
		return errors.New("keystore indisponible : impossible de gérer le mot de passe de supervision")
	}
	nonce, ciphertext, err := p.Keys.Seal(SecretScope, name, []byte(password))
	if err != nil {
		return err
	}
	return p.Store.PutSecret(SecretScope, name, nonce, ciphertext)
}
