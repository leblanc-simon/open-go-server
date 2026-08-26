package amqp

import (
	"context"
	"fmt"
	"strings"

	"leblanc.io/open-go-server/internal/exec"
)

// ctlClient pilote le broker par rabbitmqctl, en mode dégradé : la CLI ne permet
// pas de déclarer les arguments d'une queue, ni les exchanges et les bindings,
// qui relèvent du protocole AMQP lui-même.
type ctlClient struct {
	exec exec.Executor
	bin  string
}

// NewCtlClient construit le client de repli.
func NewCtlClient(ex exec.Executor, binary string) Client {
	if binary == "" {
		binary = "rabbitmqctl"
	}
	return &ctlClient{exec: ex, bin: binary}
}

// ErrDegraded signale une opération hors de portée du mode dégradé.
var ErrDegraded = fmt.Errorf("opération indisponible sans le plugin management")

func (c *ctlClient) run(ctx context.Context, args ...string) (string, error) {
	res, err := c.exec.Run(ctx, exec.Command{Name: c.bin, Args: args})
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

func (c *ctlClient) Overview(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "version")
	return strings.TrimSpace(out), err
}

func (c *ctlClient) Vhosts(ctx context.Context) ([]Vhost, error) {
	out, err := c.run(ctx, "list_vhosts", "--quiet")
	if err != nil {
		return nil, err
	}
	var vhosts []Vhost
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			vhosts = append(vhosts, Vhost{Name: name})
		}
	}
	return vhosts, nil
}

func (c *ctlClient) EnsureVhost(ctx context.Context, name string) error {
	// rabbitmqctl échoue si le vhost existe déjà : l'idempotence est obtenue en
	// vérifiant d'abord, contrairement à l'API qui est idempotente par PUT.
	existing, err := c.Vhosts(ctx)
	if err != nil {
		return err
	}
	for _, v := range existing {
		if v.Name == name {
			return nil
		}
	}
	_, err = c.run(ctx, "add_vhost", name)
	return err
}

func (c *ctlClient) DeleteVhost(ctx context.Context, name string) error {
	_, err := c.run(ctx, "delete_vhost", name)
	return err
}

func (c *ctlClient) Users(ctx context.Context) ([]User, error) {
	out, err := c.run(ctx, "list_users", "--quiet")
	if err != nil {
		return nil, err
	}
	var users []User
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			users = append(users, User{Name: fields[0]})
		}
	}
	return users, nil
}

func (c *ctlClient) EnsureUser(ctx context.Context, name, password string, tags []string) error {
	existing, err := c.Users(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, u := range existing {
		if u.Name == name {
			found = true
		}
	}
	if !found {
		if _, err := c.run(ctx, "add_user", name, password); err != nil {
			return err
		}
	} else if password != "" {
		if _, err := c.run(ctx, "change_password", name, password); err != nil {
			return err
		}
	}
	args := append([]string{"set_user_tags", name}, tags...)
	_, err = c.run(ctx, args...)
	return err
}

func (c *ctlClient) DeleteUser(ctx context.Context, name string) error {
	_, err := c.run(ctx, "delete_user", name)
	return err
}

// Permissions n'est pas lisible de façon structurée par la CLI : le mode dégradé
// réapplique les permissions au lieu de les comparer.
func (c *ctlClient) Permissions(context.Context) ([]Permission, error) { return nil, nil }

func (c *ctlClient) EnsurePermission(ctx context.Context, p Permission) error {
	_, err := c.run(ctx, "set_permissions", "-p", p.Vhost, p.User, p.Configure, p.Write, p.Read)
	return err
}

func (c *ctlClient) Exchanges(context.Context, string) ([]Exchange, error) { return nil, nil }

func (c *ctlClient) EnsureExchange(_ context.Context, e Exchange) error {
	return fmt.Errorf("%w : déclarer l'exchange %s exige le plugin management "+
		"(rabbitmq-plugins enable rabbitmq_management)", ErrDegraded, e.Name)
}

func (c *ctlClient) Queues(ctx context.Context, vhost string) ([]Queue, error) {
	out, err := c.run(ctx, "list_queues", "-p", vhost, "--quiet", "name", "messages", "consumers")
	if err != nil {
		return nil, err
	}
	var queues []Queue
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		q := Queue{Name: fields[0], Vhost: vhost, Durable: true}
		if len(fields) > 1 {
			fmt.Sscanf(fields[1], "%d", &q.Messages)
		}
		if len(fields) > 2 {
			fmt.Sscanf(fields[2], "%d", &q.Consumers)
		}
		queues = append(queues, q)
	}
	return queues, nil
}

func (c *ctlClient) EnsureQueue(_ context.Context, q Queue) error {
	return fmt.Errorf("%w : déclarer la queue %s avec ses arguments exige le plugin management",
		ErrDegraded, q.Name)
}

func (c *ctlClient) DeleteQueue(ctx context.Context, vhost, name string) error {
	_, err := c.run(ctx, "delete_queue", "-p", vhost, name)
	return err
}

func (c *ctlClient) PurgeQueue(ctx context.Context, vhost, name string) error {
	_, err := c.run(ctx, "purge_queue", "-p", vhost, name)
	return err
}

func (c *ctlClient) Bindings(context.Context, string) ([]Binding, error) { return nil, nil }

func (c *ctlClient) EnsureBinding(_ context.Context, b Binding) error {
	return fmt.Errorf("%w : déclarer le binding %s → %s exige le plugin management",
		ErrDegraded, b.Source, b.Destination)
}

func (c *ctlClient) Policies(context.Context, string) ([]Policy, error) { return nil, nil }

func (c *ctlClient) EnsurePolicy(ctx context.Context, p Policy) error {
	_, err := c.run(ctx, "set_policy", "-p", p.Vhost, p.Name, p.Pattern,
		fmt.Sprintf("%v", p.Definition), "--apply-to", orDefault(p.ApplyTo, "queues"))
	return err
}

func (c *ctlClient) DeletePolicy(ctx context.Context, vhost, name string) error {
	_, err := c.run(ctx, "clear_policy", "-p", vhost, name)
	return err
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
