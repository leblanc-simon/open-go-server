package amqp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordedRequest garde la trace d'un appel reçu par le faux broker.
type recordedRequest struct {
	Method string
	Path   string
	Body   map[string]any
	Auth   string
}

// fakeBroker rend un serveur HTTP qui imite l'API management. Tester contre un
// vrai serveur HTTP vérifie les URL, les méthodes et les corps réellement émis,
// ce qu'un double d'objet ne ferait pas.
func fakeBroker(t *testing.T, responses map[string]any) (Client, *[]recordedRequest) {
	t.Helper()
	var seen []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordedRequest{Method: r.Method, Path: r.URL.Path}
		if user, pass, ok := r.BasicAuth(); ok {
			rec.Auth = user + ":" + pass
		}
		if r.Body != nil {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				rec.Body = body
			}
		}
		seen = append(seen, rec)
		if payload, ok := responses[r.Method+" "+r.URL.Path]; ok {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(payload)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	client, err := NewHTTPClient(Credentials{URL: srv.URL, User: "admin", Password: "s3cr3t"})
	if err != nil {
		t.Fatal(err)
	}
	return client, &seen
}

func TestNewHTTPClientRequiresCredentials(t *testing.T) {
	if _, err := NewHTTPClient(Credentials{}); err == nil {
		t.Error("des identifiants sont indispensables")
	} else if !strings.Contains(err.Error(), "credentials set") {
		t.Errorf("le message doit indiquer comment les fournir : %v", err)
	}
}

func TestOverviewAndAuthentication(t *testing.T) {
	client, seen := fakeBroker(t, map[string]any{
		"GET /api/overview": map[string]any{"rabbitmq_version": "3.13.7"},
	})
	version, err := client.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "3.13.7" {
		t.Errorf("version %q", version)
	}
	if len(*seen) != 1 || (*seen)[0].Auth != "admin:s3cr3t" {
		t.Errorf("authentification absente : %+v", *seen)
	}
}

func TestEnsureVhostAndUserUsePut(t *testing.T) {
	// Les créations passent par PUT : l'API est idempotente par construction,
	// donc rejouable sans effet de bord.
	client, seen := fakeBroker(t, nil)
	ctx := context.Background()
	if err := client.EnsureVhost(ctx, "shopfr"); err != nil {
		t.Fatal(err)
	}
	if err := client.EnsureUser(ctx, "shopfr", "mot-de-passe", nil); err != nil {
		t.Fatal(err)
	}
	if err := client.EnsurePermission(ctx, Permission{
		Vhost: "shopfr", User: "shopfr", Configure: ".*", Write: ".*", Read: ".*",
	}); err != nil {
		t.Fatal(err)
	}
	calls := *seen
	if len(calls) != 3 {
		t.Fatalf("%d appels : %+v", len(calls), calls)
	}
	if calls[0].Method != http.MethodPut || calls[0].Path != "/api/vhosts/shopfr" {
		t.Errorf("vhost : %s %s", calls[0].Method, calls[0].Path)
	}
	if calls[1].Path != "/api/users/shopfr" || calls[1].Body["password"] != "mot-de-passe" {
		t.Errorf("utilisateur : %+v", calls[1])
	}
	if calls[2].Path != "/api/permissions/shopfr/shopfr" {
		t.Errorf("permissions : %s", calls[2].Path)
	}
}

func TestEnsureUserWithoutPasswordKeepsExisting(t *testing.T) {
	// Sans mot de passe, la requête ne doit pas en envoyer : cela réinitialiserait
	// le compte à chaque application.
	client, seen := fakeBroker(t, nil)
	if err := client.EnsureUser(context.Background(), "shopfr", "", []string{"monitoring"}); err != nil {
		t.Fatal(err)
	}
	body := (*seen)[0].Body
	if _, present := body["password"]; present {
		t.Errorf("aucun mot de passe ne devait être transmis : %+v", body)
	}
	if body["tags"] != "monitoring" {
		t.Errorf("tags : %v", body["tags"])
	}
}

func TestEnsureQueueDeclaresTypeAsArgument(t *testing.T) {
	// Le type de queue se déclare par x-queue-type : le champ « type » n'est pas
	// accepté à la création sur toutes les versions du broker.
	client, seen := fakeBroker(t, nil)
	err := client.EnsureQueue(context.Background(), Queue{
		Vhost: "shopfr", Name: "async", Durable: true, Type: "quorum",
		Arguments: map[string]any{"x-dead-letter-exchange": "dlx"},
	})
	if err != nil {
		t.Fatal(err)
	}
	call := (*seen)[0]
	if call.Method != http.MethodPut || call.Path != "/api/queues/shopfr/async" {
		t.Fatalf("%s %s", call.Method, call.Path)
	}
	args, ok := call.Body["arguments"].(map[string]any)
	if !ok {
		t.Fatalf("arguments absents : %+v", call.Body)
	}
	if args["x-queue-type"] != "quorum" {
		t.Errorf("type non transmis : %+v", args)
	}
	if args["x-dead-letter-exchange"] != "dlx" {
		t.Errorf("arguments perdus : %+v", args)
	}
	if call.Body["durable"] != true {
		t.Error("la durabilité doit être transmise")
	}
}

func TestEnsureQueueClassicOmitsType(t *testing.T) {
	client, seen := fakeBroker(t, nil)
	if err := client.EnsureQueue(context.Background(), Queue{
		Vhost: "shopfr", Name: "async", Durable: true, Type: "classic",
	}); err != nil {
		t.Fatal(err)
	}
	args := (*seen)[0].Body["arguments"].(map[string]any)
	if _, present := args["x-queue-type"]; present {
		t.Errorf("le type classique est le défaut et n'a pas à être déclaré : %+v", args)
	}
}

func TestEnsureBindingPath(t *testing.T) {
	client, seen := fakeBroker(t, nil)
	err := client.EnsureBinding(context.Background(), Binding{
		Vhost: "shopfr", Source: "messages", Destination: "async", RoutingKey: "async",
	})
	if err != nil {
		t.Fatal(err)
	}
	call := (*seen)[0]
	if call.Method != http.MethodPost {
		t.Errorf("un binding se crée par POST, obtenu %s", call.Method)
	}
	if call.Path != "/api/bindings/shopfr/e/messages/q/async" {
		t.Errorf("chemin : %s", call.Path)
	}
	if call.Body["routing_key"] != "async" {
		t.Errorf("clé de routage : %v", call.Body["routing_key"])
	}
}

func TestQueuesDecodesCounters(t *testing.T) {
	client, _ := fakeBroker(t, map[string]any{
		"GET /api/queues/shopfr": []map[string]any{{
			"name": "async", "vhost": "shopfr", "durable": true, "type": "quorum",
			"messages": 1240, "consumers": 3,
			"arguments": map[string]any{"x-message-ttl": 86400000},
		}},
	})
	queues, err := client.Queues(context.Background(), "shopfr")
	if err != nil {
		t.Fatal(err)
	}
	if len(queues) != 1 {
		t.Fatalf("%d queues", len(queues))
	}
	q := queues[0]
	if q.Messages != 1240 || q.Consumers != 3 {
		t.Errorf("compteurs : %d messages, %d consumers", q.Messages, q.Consumers)
	}
	if q.EffectiveType() != "quorum" {
		t.Errorf("type effectif : %q", q.EffectiveType())
	}
}

func TestEffectiveTypePrefersArgument(t *testing.T) {
	cases := []struct {
		queue Queue
		want  string
	}{
		{Queue{}, "classic"},
		{Queue{Type: "quorum"}, "quorum"},
		{Queue{Arguments: map[string]any{"x-queue-type": "quorum"}}, "quorum"},
		// L'argument prime : c'est lui qui a été employé à la déclaration.
		{Queue{Type: "classic", Arguments: map[string]any{"x-queue-type": "quorum"}}, "quorum"},
	}
	for _, c := range cases {
		if got := c.queue.EffectiveType(); got != c.want {
			t.Errorf("EffectiveType() = %q, attendu %q (%+v)", got, c.want, c.queue)
		}
	}
}

func TestPluginMissingIsDistinguished(t *testing.T) {
	// Un 404 sur une écriture signale presque toujours un plugin management
	// absent : le message doit le dire, pas se contenter d'un code HTTP.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	client, err := NewHTTPClient(Credentials{URL: srv.URL, User: "admin", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	err = client.EnsureVhost(context.Background(), "shopfr")
	if err == nil {
		t.Fatal("erreur attendue")
	}
	if !strings.Contains(err.Error(), "rabbitmq_management") {
		t.Errorf("le message doit orienter vers le plugin : %v", err)
	}
}

func TestUnauthorizedIsExplicit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	client, _ := NewHTTPClient(Credentials{URL: srv.URL, User: "admin", Password: "mauvais"})
	err := client.EnsureVhost(context.Background(), "shopfr")
	if err == nil || !strings.Contains(err.Error(), "refusés") {
		t.Errorf("le refus d'authentification doit être explicite : %v", err)
	}
}

func TestUnavailableBroker(t *testing.T) {
	client, err := NewHTTPClient(Credentials{URL: "http://127.0.0.1:1", User: "admin", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Overview(context.Background()); err == nil {
		t.Error("un broker injoignable doit produire une erreur")
	}
}

func TestPurgeAndDeleteQueuePaths(t *testing.T) {
	client, seen := fakeBroker(t, nil)
	ctx := context.Background()
	if err := client.PurgeQueue(ctx, "shopfr", "async"); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteQueue(ctx, "shopfr", "async"); err != nil {
		t.Fatal(err)
	}
	calls := *seen
	if calls[0].Path != "/api/queues/shopfr/async/contents" || calls[0].Method != http.MethodDelete {
		t.Errorf("purge : %s %s", calls[0].Method, calls[0].Path)
	}
	if calls[1].Path != "/api/queues/shopfr/async" {
		t.Errorf("suppression : %s", calls[1].Path)
	}
}

func TestEnsurePolicyDefaultsToQueues(t *testing.T) {
	client, seen := fakeBroker(t, nil)
	err := client.EnsurePolicy(context.Background(), Policy{
		Vhost: "shopfr", Name: "ha", Pattern: "^.*",
		Definition: map[string]any{"max-length": 100000},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := (*seen)[0].Body
	if body["apply-to"] != "queues" {
		t.Errorf("apply-to : %v", body["apply-to"])
	}
	if body["pattern"] != "^.*" {
		t.Errorf("motif : %v", body["pattern"])
	}
}
