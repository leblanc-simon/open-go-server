// Package amqp porte le provider RabbitMQ : vhosts, utilisateurs, permissions,
// exchanges, queues, bindings et policies.
package amqp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"leblanc.io/open-go-server/internal/exec"
)

// DefaultManagementURL est l'adresse du plugin management sur une installation
// standard. Le plugin n'écoute que sur la boucle locale par défaut.
const DefaultManagementURL = "http://127.0.0.1:15672"

// ErrUnavailable indique que le broker n'est pas joignable.
var ErrUnavailable = errors.New("broker RabbitMQ indisponible")

// ErrPluginMissing indique que l'API management n'est pas activée.
var ErrPluginMissing = errors.New("plugin rabbitmq_management non activé")

// Credentials porte les identifiants d'administration.
type Credentials struct {
	URL      string `yaml:"url"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	// AMQPPort est le port du protocole AMQP, pour construire les DSN applicatifs.
	AMQPPort int `yaml:"amqpPort,omitempty"`
	// BrokerAccount est le compte système sous lequel tourne le broker : sur la
	// boucle locale, seul lui — ou root — reçoit les identifiants (D93). Vide,
	// c'est celui du paquet Debian, DefaultBrokerAccount.
	BrokerAccount string `yaml:"brokerAccount,omitempty"`
}

// Vhost décrit un vhost observé.
type Vhost struct {
	Name string `json:"name"`
}

// User décrit un compte observé.
type User struct {
	Name string `json:"name"`
	Tags any    `json:"tags"`
}

// Permission décrit les droits d'un compte sur un vhost.
type Permission struct {
	Vhost     string `json:"vhost"`
	User      string `json:"user"`
	Configure string `json:"configure"`
	Write     string `json:"write"`
	Read      string `json:"read"`
}

// Exchange décrit un exchange observé.
type Exchange struct {
	Name       string         `json:"name"`
	Vhost      string         `json:"vhost"`
	Type       string         `json:"type"`
	Durable    bool           `json:"durable"`
	AutoDelete bool           `json:"auto_delete"`
	Arguments  map[string]any `json:"arguments"`
}

// Queue décrit une queue observée. Les compteurs servent à prévenir avant toute
// opération destructrice.
type Queue struct {
	Name      string         `json:"name"`
	Vhost     string         `json:"vhost"`
	Durable   bool           `json:"durable"`
	Type      string         `json:"type"`
	Arguments map[string]any `json:"arguments"`
	Messages  int            `json:"messages"`
	Consumers int            `json:"consumers"`
}

// EffectiveType renvoie le type de la queue, en tenant compte de l'argument
// x-queue-type qui prime sur le champ dédié selon les versions du broker.
func (q Queue) EffectiveType() string {
	if v, ok := q.Arguments["x-queue-type"]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	if q.Type != "" {
		return q.Type
	}
	return "classic"
}

// Binding décrit un lien entre un exchange et une queue.
type Binding struct {
	Vhost           string         `json:"vhost"`
	Source          string         `json:"source"`
	Destination     string         `json:"destination"`
	DestinationType string         `json:"destination_type"`
	RoutingKey      string         `json:"routing_key"`
	Arguments       map[string]any `json:"arguments"`
	// PropertiesKey est la clé par laquelle l'API désigne le binding pour le
	// retirer : la clé de routage et les arguments, encodés par le broker.
	PropertiesKey string `json:"properties_key"`
}

// Policy décrit une policy observée.
type Policy struct {
	Vhost      string         `json:"vhost"`
	Name       string         `json:"name"`
	Pattern    string         `json:"pattern"`
	ApplyTo    string         `json:"apply-to"`
	Definition map[string]any `json:"definition"`
	Priority   int            `json:"priority"`
}

// Client est l'accès d'administration au broker.
type Client interface {
	// Overview vérifie la disponibilité et renvoie la version du broker.
	Overview(ctx context.Context) (string, error)
	Vhosts(ctx context.Context) ([]Vhost, error)
	EnsureVhost(ctx context.Context, name string) error
	DeleteVhost(ctx context.Context, name string) error
	Users(ctx context.Context) ([]User, error)
	EnsureUser(ctx context.Context, name, password string, tags []string) error
	DeleteUser(ctx context.Context, name string) error
	Permissions(ctx context.Context) ([]Permission, error)
	EnsurePermission(ctx context.Context, p Permission) error
	Exchanges(ctx context.Context, vhost string) ([]Exchange, error)
	EnsureExchange(ctx context.Context, e Exchange) error
	Queues(ctx context.Context, vhost string) ([]Queue, error)
	EnsureQueue(ctx context.Context, q Queue) error
	DeleteQueue(ctx context.Context, vhost, name string) error
	// DeleteIdleQueue retire une file seulement si elle est vide et sans
	// consommateur : le broker en juge lui-même, au moment du retrait. C'est le
	// retrait d'une file que la déclaration ne porte plus (D100), jamais celui
	// d'une recréation consentie.
	DeleteIdleQueue(ctx context.Context, vhost, name string) error
	// DeleteExchange retire un exchange seulement s'il n'a plus de binding.
	DeleteExchange(ctx context.Context, vhost, name string) error
	PurgeQueue(ctx context.Context, vhost, name string) error
	Bindings(ctx context.Context, vhost string) ([]Binding, error)
	EnsureBinding(ctx context.Context, b Binding) error
	DeleteBinding(ctx context.Context, b Binding) error
	Policies(ctx context.Context, vhost string) ([]Policy, error)
	EnsurePolicy(ctx context.Context, p Policy) error
	DeletePolicy(ctx context.Context, vhost, name string) error
}

// httpClient parle à l'API management.
type httpClient struct {
	base  string
	creds Credentials
	http  *http.Client
}

// NewHTTPClient construit un client de l'API management. Celle-ci est privilégiée
// sur rabbitmqctl : elle est idempotente par construction (les créations sont des
// PUT) et permet de déclarer les arguments de queue, ce que la CLI ne fait pas.
func NewHTTPClient(creds Credentials) (Client, error) {
	if creds.URL == "" {
		creds.URL = DefaultManagementURL
	}
	if creds.User == "" {
		return nil, fmt.Errorf("identifiants d'administration RabbitMQ absents : " +
			"les enregistrer avec « ogs amqp credentials set »")
	}
	if _, err := url.Parse(creds.URL); err != nil {
		return nil, fmt.Errorf("URL de management %q invalide : %w", creds.URL, err)
	}
	return newHTTPClient(creds, peerGuard{account: brokerAccount(creds)}), nil
}

func newHTTPClient(creds Credentials, guard peerGuard) *httpClient {
	// Chaque connexion passe par la garde : une connexion ouverte avant l'arrêt du
	// broker n'est jamais réemployée vers un autre, le noyau la fermant avec lui.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = guard.dial
	return &httpClient{
		base:  strings.TrimRight(creds.URL, "/"),
		creds: creds,
		http:  &http.Client{Timeout: 15 * time.Second, Transport: transport},
	}
}

func brokerAccount(creds Credentials) string {
	if creds.BrokerAccount == "" {
		return DefaultBrokerAccount
	}
	return creds.BrokerAccount
}

// do exécute une requête et décode la réponse éventuelle.
func (c *httpClient) do(ctx context.Context, method, path string, body, out any) error {
	res, err := c.send(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNotFound && method == http.MethodGet:
		// Une ressource absente n'est pas une erreur pour une lecture.
		return nil
	case res.StatusCode == http.StatusUnauthorized:
		return c.refused(ctx, method, path, res)
	case res.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w : %s %s introuvable — vérifier « rabbitmq-plugins enable rabbitmq_management »",
			ErrPluginMissing, method, path)
	case res.StatusCode >= 300:
		data, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("%s %s : %s %s", method, path, res.Status, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// send émet une requête authentifiée, sans en interpréter la réponse.
func (c *httpClient) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.creds.User, c.creds.Password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w : %w", ErrUnavailable, err)
	}
	return res, nil
}

// refused explique un 401. Le broker le rend pour deux causes que son code ne
// distingue pas : des identifiants refusés, et un compte reconnu à qui le vhost
// visé n'est pas ouvert — le tag « administrator » fait tout lire, il ne donne
// aucune permission sur un vhost (D124). « Identifiants refusés » envoyait
// vérifier un mot de passe que les lectures venaient d'accepter : /api/whoami,
// qui n'exige que l'authentification, départage.
func (c *httpClient) refused(ctx context.Context, method, path string, res *http.Response) error {
	var reply struct {
		Reason string `json:"reason"`
	}
	data, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
	_ = json.Unmarshal(data, &reply)
	detail := res.Status
	if reply.Reason != "" {
		detail += " : " + reply.Reason
	}
	vhost, inVhost := vhostOf(path)
	if inVhost && path != "/api/whoami" {
		if check, err := c.send(ctx, http.MethodGet, "/api/whoami", nil); err == nil {
			check.Body.Close()
			if check.StatusCode < 300 {
				return fmt.Errorf("%w : le compte d'administration %s n'a pas de permissions sur le vhost %s "+
					"(%s %s, %s) — un « ogs apply » du projet les pose, ou « %s »", ErrVhostAccess, c.creds.User,
					vhost, method, path, detail,
					exec.ShellLine("rabbitmqctl", "set_permissions", "-p", vhost, c.creds.User, ".*", ".*", ".*"))
			}
		}
	}
	return fmt.Errorf("identifiants d'administration RabbitMQ refusés (%s) — les vérifier, puis « ogs amqp "+
		"credentials set --user %s »", detail, c.creds.User)
}

// ErrVhostAccess indique un compte d'administration reconnu, mais sans
// permissions sur le vhost visé (D124).
var ErrVhostAccess = errors.New("accès au vhost refusé")

// vhostOf rend le vhost que désigne un chemin de l'API, pour les ressources qui
// vivent dans un vhost : /api/<sorte>/<vhost>/…
func vhostOf(path string) (string, bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/api/"), "/")
	if len(parts) < 2 {
		return "", false
	}
	switch parts[0] {
	case "exchanges", "queues", "bindings", "policies", "permissions", "topic-permissions":
	default:
		return "", false
	}
	vhost, err := url.PathUnescape(parts[1])
	if err != nil || vhost == "" {
		return "", false
	}
	return vhost, true
}

// escape encode un segment de chemin. Le vhost est déclaré sans barre oblique
// (D9), mais l'encodage reste nécessaire pour les autres caractères.
func escape(s string) string { return url.PathEscape(s) }

func (c *httpClient) Overview(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"rabbitmq_version"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/overview", nil, &out); err != nil {
		return "", err
	}
	return out.Version, nil
}

func (c *httpClient) Vhosts(ctx context.Context) ([]Vhost, error) {
	var out []Vhost
	err := c.do(ctx, http.MethodGet, "/api/vhosts", nil, &out)
	return out, err
}

func (c *httpClient) EnsureVhost(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPut, "/api/vhosts/"+escape(name), map[string]any{}, nil)
}

func (c *httpClient) DeleteVhost(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/api/vhosts/"+escape(name), nil, nil)
}

func (c *httpClient) Users(ctx context.Context) ([]User, error) {
	var out []User
	err := c.do(ctx, http.MethodGet, "/api/users", nil, &out)
	return out, err
}

func (c *httpClient) EnsureUser(ctx context.Context, name, password string, tags []string) error {
	body := map[string]any{"tags": strings.Join(tags, ",")}
	if password != "" {
		body["password"] = password
	}
	return c.do(ctx, http.MethodPut, "/api/users/"+escape(name), body, nil)
}

func (c *httpClient) DeleteUser(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/api/users/"+escape(name), nil, nil)
}

func (c *httpClient) Permissions(ctx context.Context) ([]Permission, error) {
	var out []Permission
	err := c.do(ctx, http.MethodGet, "/api/permissions", nil, &out)
	return out, err
}

func (c *httpClient) EnsurePermission(ctx context.Context, p Permission) error {
	return c.do(ctx, http.MethodPut,
		"/api/permissions/"+escape(p.Vhost)+"/"+escape(p.User),
		map[string]any{"configure": p.Configure, "write": p.Write, "read": p.Read}, nil)
}

func (c *httpClient) Exchanges(ctx context.Context, vhost string) ([]Exchange, error) {
	var out []Exchange
	err := c.do(ctx, http.MethodGet, "/api/exchanges/"+escape(vhost), nil, &out)
	return out, err
}

func (c *httpClient) EnsureExchange(ctx context.Context, e Exchange) error {
	return c.do(ctx, http.MethodPut,
		"/api/exchanges/"+escape(e.Vhost)+"/"+escape(e.Name),
		map[string]any{
			"type": e.Type, "durable": e.Durable,
			"auto_delete": e.AutoDelete, "arguments": orEmptyArgs(e.Arguments),
		}, nil)
}

func (c *httpClient) Queues(ctx context.Context, vhost string) ([]Queue, error) {
	var out []Queue
	err := c.do(ctx, http.MethodGet, "/api/queues/"+escape(vhost), nil, &out)
	return out, err
}

func (c *httpClient) EnsureQueue(ctx context.Context, q Queue) error {
	args := orEmptyArgs(q.Arguments)
	// Le type de queue se déclare par un argument : le champ « type » de l'API
	// n'est pas accepté à la création sur toutes les versions.
	if t := q.Type; t != "" && t != "classic" {
		args["x-queue-type"] = t
	}
	return c.do(ctx, http.MethodPut,
		"/api/queues/"+escape(q.Vhost)+"/"+escape(q.Name),
		map[string]any{"durable": q.Durable, "auto_delete": false, "arguments": args}, nil)
}

func (c *httpClient) DeleteQueue(ctx context.Context, vhost, name string) error {
	return c.do(ctx, http.MethodDelete, "/api/queues/"+escape(vhost)+"/"+escape(name), nil, nil)
}

func (c *httpClient) DeleteIdleQueue(ctx context.Context, vhost, name string) error {
	path := "/api/queues/" + escape(vhost) + "/" + escape(name)
	err := c.do(ctx, http.MethodDelete, path+"?if-empty=true&if-unused=true", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "not supported by quorum queues") {
		return err
	}
	// Une file quorum refuse if-empty et if-unused : le broker ne sait pas en
	// juger lui-même au retrait. Ses compteurs sont relus juste avant, et elle ne
	// part que vide et sans consommateur. Une fenêtre subsiste entre la lecture et
	// le retrait — les compteurs du broker ont quelques secondes de retard —, mais
	// ses bindings sont retirés d'abord : seul un envoi direct à la file, par son
	// nom, peut encore l'atteindre.
	var q Queue
	if err := c.do(ctx, http.MethodGet, path, nil, &q); err != nil {
		return err
	}
	if q.Name == "" {
		return nil
	}
	if q.Messages > 0 || q.Consumers > 0 {
		return fmt.Errorf("la file %s porte %d message(s) et %d consommateur(s)", name, q.Messages, q.Consumers)
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

func (c *httpClient) DeleteExchange(ctx context.Context, vhost, name string) error {
	return c.do(ctx, http.MethodDelete,
		"/api/exchanges/"+escape(vhost)+"/"+escape(name)+"?if-unused=true", nil, nil)
}

func (c *httpClient) PurgeQueue(ctx context.Context, vhost, name string) error {
	return c.do(ctx, http.MethodDelete,
		"/api/queues/"+escape(vhost)+"/"+escape(name)+"/contents", nil, nil)
}

func (c *httpClient) Bindings(ctx context.Context, vhost string) ([]Binding, error) {
	var out []Binding
	err := c.do(ctx, http.MethodGet, "/api/bindings/"+escape(vhost), nil, &out)
	return out, err
}

func (c *httpClient) EnsureBinding(ctx context.Context, b Binding) error {
	return c.do(ctx, http.MethodPost,
		"/api/bindings/"+escape(b.Vhost)+"/e/"+escape(b.Source)+"/q/"+escape(b.Destination),
		map[string]any{"routing_key": b.RoutingKey, "arguments": orEmptyArgs(b.Arguments)}, nil)
}

func (c *httpClient) DeleteBinding(ctx context.Context, b Binding) error {
	key := b.PropertiesKey
	if key == "" {
		// Sans arguments, la clé de propriétés est la clé de routage elle-même
		// (« ~ » quand elle est vide).
		key = b.RoutingKey
		if key == "" {
			key = "~"
		}
	}
	return c.do(ctx, http.MethodDelete,
		"/api/bindings/"+escape(b.Vhost)+"/e/"+escape(b.Source)+"/q/"+escape(b.Destination)+"/"+escape(key),
		nil, nil)
}

func (c *httpClient) Policies(ctx context.Context, vhost string) ([]Policy, error) {
	var out []Policy
	err := c.do(ctx, http.MethodGet, "/api/policies/"+escape(vhost), nil, &out)
	return out, err
}

func (c *httpClient) EnsurePolicy(ctx context.Context, p Policy) error {
	applyTo := p.ApplyTo
	if applyTo == "" {
		applyTo = "queues"
	}
	return c.do(ctx, http.MethodPut,
		"/api/policies/"+escape(p.Vhost)+"/"+escape(p.Name),
		map[string]any{
			"pattern": p.Pattern, "definition": orEmptyArgs(p.Definition),
			"priority": p.Priority, "apply-to": applyTo,
		}, nil)
}

func (c *httpClient) DeletePolicy(ctx context.Context, vhost, name string) error {
	return c.do(ctx, http.MethodDelete, "/api/policies/"+escape(vhost)+"/"+escape(name), nil, nil)
}

func orEmptyArgs(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// SortedKeys renvoie les clés triées d'une table d'arguments, pour un affichage
// et des comparaisons déterministes.
func SortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
