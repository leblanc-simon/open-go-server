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

// planMySQLAccount crée ou met à jour le compte de métriques MySQL. Le moteur
// est réputé absent lorsqu'aucun socket n'existe — et alors ogs se tait : une
// machine sans MySQL n'a pas à voir un avertissement à chaque apply.
func (p *Provider) planMySQLAccount(ctx context.Context) (*engine.Step, *MySQLAccess) {
	if p.DB == nil {
		return nil, nil
	}
	socket, ok := db.FindMySQLSocket(p.Paths.Prefix())
	if !ok {
		return nil, nil
	}
	account := p.Cfg.Monitoring.TelegrafAccount()
	resource := "monitoring/account/mysql"

	client, err := p.DB.EngineClient("mysql", "")
	if err != nil {
		return unavailableStep(resource, "compte de supervision MySQL", err), nil
	}
	password, err := p.ensureSecret("mysql/" + account)
	if err != nil {
		return unavailableStep(resource, "compte de supervision MySQL", err), nil
	}
	access := &MySQLAccess{User: account, Password: password, Socket: socket}

	users, err := client.Users(ctx)
	if err != nil {
		return unavailableStep(resource, "compte de supervision MySQL", err), access
	}
	conforming := users[account+"@localhost"]
	if conforming {
		// Le mot de passe scellé et celui du moteur ne se comparent pas : MySQL ne
		// rend que l'empreinte. Les privilèges, eux, se vérifient — et c'est ce qui
		// dérive en pratique, un moteur mis à jour perdant parfois ses GRANT.
		if grants, err := client.Grants(ctx, account, "localhost"); err == nil {
			conforming = hasAllGrants(grants, db.MonitoringMySQLGrants)
		}
	}
	if conforming {
		return engine.NewStep(resource, engine.NoOp,
			fmt.Sprintf("compte de supervision MySQL %s", account)), access
	}

	action := engine.Update
	if !users[account+"@localhost"] {
		action = engine.Create
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
		return strings.Join(stmts, ";\n") + ";", nil
	}
	s.Apply = func(ctx context.Context) error {
		stmts, err := db.MonitoringMySQL(account, "localhost", password)
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
	password, err := p.ensureSecret("rabbitmq/" + account)
	if err != nil {
		return unavailableStep(resource, summary, err), nil
	}
	access := &RabbitAccess{URL: p.AMQP.ManagementURL, User: account, Password: password}

	users, err := p.AMQP.Client.Users(ctx)
	if err != nil {
		return unavailableStep(resource, summary, err), access
	}
	for _, u := range users {
		if u.Name == account && tagsContain(u.Tags, "monitoring") {
			return engine.NewStep(resource, engine.NoOp, summary), access
		}
	}
	s := engine.NewStep(resource, engine.Create, summary)
	s.Critical = false
	s.Diff = func() (string, error) {
		return fmt.Sprintf("PUT /api/users/%s {\"tags\": \"monitoring\", \"password\": \"<scellé>\"}", account), nil
	}
	s.Apply = func(ctx context.Context) error {
		return p.AMQP.Client.EnsureUser(ctx, account, password, []string{"monitoring"})
	}
	return s, access
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

// ensureSecret retrouve ou crée un mot de passe de parc. Comme pour les comptes
// de projet, un secret déjà scellé n'est jamais régénéré : le régénérer laisserait
// l'agent avec l'ancien jusqu'au prochain rechargement, et personne pour le dire.
func (p *Provider) ensureSecret(name string) (string, error) {
	if p.Store == nil || p.Keys == nil {
		return "", errors.New("keystore indisponible : impossible de gérer le mot de passe de supervision")
	}
	nonce, ciphertext, err := p.Store.GetSecret(SecretScope, name)
	if err == nil {
		plain, err := p.Keys.Open(SecretScope, name, nonce, ciphertext)
		if err != nil {
			return "", err
		}
		return string(plain), nil
	}
	password, err := secret.Password(0)
	if err != nil {
		return "", err
	}
	nonce, ciphertext, err = p.Keys.Seal(SecretScope, name, []byte(password))
	if err != nil {
		return "", err
	}
	if err := p.Store.PutSecret(SecretScope, name, nonce, ciphertext); err != nil {
		return "", err
	}
	return password, nil
}
