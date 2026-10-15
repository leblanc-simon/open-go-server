package amqp

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
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
	client, err := NewHTTPClient(Credentials{URL: srv.URL, User: "admin", Password: "s3cr3t", BrokerAccount: compteDuTest(t)})
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
	client, err := NewHTTPClient(Credentials{URL: srv.URL, User: "admin", Password: "x", BrokerAccount: compteDuTest(t)})
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
	client, _ := NewHTTPClient(Credentials{URL: srv.URL, User: "admin", Password: "mauvais", BrokerAccount: compteDuTest(t)})
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

// compteDuTest rend le compte qui fait tourner les tests : c'est lui qui tient les
// serveurs de test, et la garde de la boucle locale doit le reconnaître comme le
// compte du broker.
func compteDuTest(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return u.Username
}

func TestIdentifiantsRefusesAUnPortLocalTenuParUnAutreCompte(t *testing.T) {
	// Broker arrêté, l'utilisateur d'un projet ouvre 127.0.0.1:15672 : ogs lui
	// envoyait les identifiants d'administration en Basic, et le mot de passe de
	// chaque compte de projet dans le corps des requêtes (S1R-07).
	var reçu []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); ok {
			reçu = append(reçu, user+":"+pass)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	// Le serveur tourne sous le compte des tests, qui n'est ni root ni rabbitmq
	// (sauf à lancer les tests en root, où la garde admet légitimement root).
	if os.Getuid() == 0 {
		t.Skip("en root, le serveur de test est tenu par root, que la garde admet")
	}
	client, err := NewHTTPClient(Credentials{URL: srv.URL, User: "admin", Password: "s3cr3t",
		BrokerAccount: "compte-absent-ogs"})
	if err != nil {
		t.Fatal(err)
	}
	err = client.EnsureUser(context.Background(), "shopfr", "mot-de-passe-du-projet", nil)
	if !errors.Is(err, ErrUntrustedPeer) || !errors.Is(err, ErrUnavailable) {
		t.Errorf("refus attendu, rendu comme une indisponibilité : %v", err)
	}
	if len(reçu) != 0 {
		t.Errorf("identifiants envoyés à un port tenu par un autre compte : %v", reçu)
	}
	// Le compte du broker, lui, les reçoit.
	client, _ = NewHTTPClient(Credentials{URL: srv.URL, User: "admin", Password: "s3cr3t",
		BrokerAccount: compteDuTest(t)})
	if err := client.EnsureUser(context.Background(), "shopfr", "x", nil); err != nil {
		t.Fatal(err)
	}
	if len(reçu) != 1 {
		t.Errorf("le broker légitime doit recevoir la requête : %v", reçu)
	}
}

func TestAdresseDeProcNetTCP(t *testing.T) {
	// /proc/net/tcp écrit l'adresse par mots de 32 bits dans l'ordre de la
	// machine ; une connexion IPv4 vers un serveur qui écoute sur « :: » y figure
	// sous sa forme projetée.
	if binary.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Skip("machine gros-boutiste : les exemples sont ceux d'une machine petit-boutiste")
	}
	cases := map[string]string{
		"0100007F:3D70":                         "127.0.0.1:15728",
		"0000000000000000FFFF00000100007F:3D70": "127.0.0.1:15728",
		"00000000000000000000000001000000:0050": "[::1]:80",
	}
	for raw, want := range cases {
		addr, err := parseProcAddr(raw)
		if err != nil {
			t.Fatalf("%s : %v", raw, err)
		}
		wantAddr, _ := net.ResolveTCPAddr("tcp", want)
		if !sameAddr(addr, wantAddr) {
			t.Errorf("%s : %s, attendu %s", raw, addr, want)
		}
	}
}

func TestGardeVautPourToutesLesAdressesDeLaMachine(t *testing.T) {
	// Une URL qui désigne la machine par l'une de ses adresses, et non par la
	// boucle locale, aboutit au même port : la garde ne s'en tenait qu'à
	// 127.0.0.1.
	if os.Getuid() == 0 {
		t.Skip("en root, le serveur de test est tenu par root, que la garde admet")
	}
	var ip net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			ip = n.IP
			break
		}
	}
	if ip == nil {
		t.Skip("aucune adresse IPv4 hors boucle locale sur cette machine")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Skip(err)
	}
	var reçu int
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reçu++
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	client, _ := NewHTTPClient(Credentials{URL: srv.URL, User: "admin", Password: "s3cr3t",
		BrokerAccount: "compte-absent-ogs"})
	if err := client.EnsureVhost(context.Background(), "shopfr"); !errors.Is(err, ErrUntrustedPeer) {
		t.Errorf("refus attendu vers %s : %v", srv.URL, err)
	}
	if reçu != 0 {
		t.Error("identifiants envoyés à un port de la machine tenu par un autre compte")
	}
}

// connFictive n'a que des adresses : celles d'une connexion dont aucun socket de
// cette machine ne tient l'autre bout.
type connFictive struct {
	net.Conn
	local, remote net.Addr
}

func (c connFictive) LocalAddr() net.Addr  { return c.local }
func (c connFictive) RemoteAddr() net.Addr { return c.remote }

func TestSocketAbsentVersLaMachineEstRefuseEnNommantLeRemede(t *testing.T) {
	// Docker sans userland-proxy : 127.0.0.1:15672 est redirigé par DNAT vers un
	// conteneur, et aucun socket de l'hôte ne répond. Vers un broker distant, au
	// contraire, l'absence est la règle.
	conn := connFictive{
		local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 40000},
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 15672},
	}
	distant := peerGuard{account: "rabbitmq", local: func(net.IP) bool { return false }}
	if err := distant.check(conn); err != nil {
		t.Errorf("un broker distant ne figure pas dans les tables de la machine : %v", err)
	}
	redirigé := peerGuard{account: "rabbitmq", local: func(net.IP) bool { return true }}
	err := redirigé.check(conn)
	if !errors.Is(err, ErrUntrustedPeer) || !strings.Contains(err.Error(), "--url") {
		t.Errorf("refus nommant le remède attendu : %v", err)
	}
}

func TestCompteDuBrokerParSonUID(t *testing.T) {
	// Un broker en conteneur, sur le réseau de l'hôte, tourne sous un uid sans nom
	// dans /etc/passwd.
	uids, err := trustedUIDs("999")
	if err != nil {
		t.Fatal(err)
	}
	if !uids[999] || !uids[0] || len(uids) != 2 {
		t.Errorf("uid admis : %v", uids)
	}
}

func TestRetraitDUneFileQuorumRelitSesCompteurs(t *testing.T) {
	// Une file quorum refuse if-empty et if-unused (400) : le retrait relit ses
	// compteurs et ne la retire que vide et sans consommateur.
	for _, cas := range []struct {
		messages int
		retiree  bool
	}{{0, true}, {4, false}} {
		var seen []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = append(seen, r.Method+" "+r.URL.RequestURI())
			switch {
			case r.Method == http.MethodDelete && r.URL.RawQuery != "":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"bad_request","reason":"queue.delete operations with ` +
					`if-unused flag set are not supported by quorum queues"}`))
			case r.Method == http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]any{"name": "emails", "messages": cas.messages})
			default:
				w.WriteHeader(http.StatusNoContent)
			}
		}))
		client, err := NewHTTPClient(Credentials{URL: srv.URL, User: "admin", Password: "x",
			BrokerAccount: compteDuTest(t)})
		if err != nil {
			t.Fatal(err)
		}
		err = client.DeleteIdleQueue(context.Background(), "shopfr", "emails")
		srv.Close()
		retiree := len(seen) > 0 && seen[len(seen)-1] == "DELETE /api/queues/shopfr/emails"
		if retiree != cas.retiree || (err == nil) != cas.retiree {
			t.Errorf("%d message(s) : retirée %v, erreur %v (%v)", cas.messages, retiree, err, seen)
		}
	}
}

func TestRetraitDUnBindingParSaCleDeProprietes(t *testing.T) {
	client, seen := fakeBroker(t, nil)
	err := client.DeleteBinding(context.Background(), Binding{Vhost: "shopfr", Source: "messages",
		Destination: "emails", RoutingKey: "emails", PropertiesKey: "emails"})
	if err != nil {
		t.Fatal(err)
	}
	got := (*seen)[0]
	if got.Method != http.MethodDelete || got.Path != "/api/bindings/shopfr/e/messages/q/emails/emails" {
		t.Errorf("retrait du binding : %s %s", got.Method, got.Path)
	}
}

func TestRefusSurUnVhostDistingueDesIdentifiants(t *testing.T) {
	// Le broker rend 401 au compte reconnu à qui le vhost n'est pas ouvert : le
	// message « identifiants refusés » envoyait vérifier un mot de passe que
	// les lectures venaient d'accepter (D124).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/whoami" {
			_, _ = w.Write([]byte(`{"name":"ogs-admin","tags":["administrator"]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"not_authorised","reason":"Access refused."}`))
	}))
	defer srv.Close()
	client, _ := NewHTTPClient(Credentials{URL: srv.URL, User: "ogs-admin", Password: "bon",
		BrokerAccount: compteDuTest(t)})
	err := client.EnsureBinding(context.Background(),
		Binding{Vhost: "shopfr", Source: "messages", Destination: "email", RoutingKey: "email"})
	if !errors.Is(err, ErrVhostAccess) {
		t.Fatalf("un refus sur le vhost doit être distingué : %v", err)
	}
	for _, attendu := range []string{"ogs-admin", "vhost shopfr", "Access refused.",
		"rabbitmqctl set_permissions -p shopfr ogs-admin", "ogs apply"} {
		if !strings.Contains(err.Error(), attendu) {
			t.Errorf("le message doit porter %q : %v", attendu, err)
		}
	}
	if strings.Contains(err.Error(), "identifiants d'administration RabbitMQ refusés") {
		t.Errorf("les identifiants ne sont pas en cause : %v", err)
	}
}

func TestIdentifiantsRefusesSurUnVhostRestentNommes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"not_authorised","reason":"Login failed"}`))
	}))
	defer srv.Close()
	client, _ := NewHTTPClient(Credentials{URL: srv.URL, User: "ogs-admin", Password: "mauvais",
		BrokerAccount: compteDuTest(t)})
	err := client.EnsureQueue(context.Background(), Queue{Vhost: "shopfr", Name: "email"})
	if err == nil || errors.Is(err, ErrVhostAccess) ||
		!strings.Contains(err.Error(), "refusés") || !strings.Contains(err.Error(), "Login failed") {
		t.Errorf("des identifiants refusés doivent être nommés comme tels : %v", err)
	}
}
