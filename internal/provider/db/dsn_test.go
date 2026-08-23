package db

import (
	"net/url"
	"strings"
	"testing"
)

func TestDSNEncodesPassword(t *testing.T) {
	// Notre alphabet de mots de passe contient « + » et « = », qui ont une
	// signification dans une URL : ils doivent être encodés, sinon la chaîne de
	// connexion est fausse et l'erreur est difficile à diagnostiquer.
	dsn, err := DSN(Endpoint{Engine: "mysql", Version: "8.0"}, "shopfr", "shopfr", "a+b=c/d@e")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("DSN non analysable : %v\n%s", err, dsn)
	}
	pass, _ := u.User.Password()
	if pass != "a+b=c/d@e" {
		t.Errorf("mot de passe restitué %q", pass)
	}
	if u.Scheme != "mysql" || u.Host != "127.0.0.1:3306" || u.Path != "/shopfr" {
		t.Errorf("DSN inattendu : %s", dsn)
	}
	if !strings.Contains(dsn, "serverVersion=8.0") || !strings.Contains(dsn, "charset=utf8mb4") {
		t.Errorf("paramètres manquants : %s", dsn)
	}
}

func TestDSNPostgres(t *testing.T) {
	dsn, err := DSN(Endpoint{Engine: "postgres", Port: 5433, Version: "16"}, "shopfr", "shopfr", "x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dsn, "postgresql://shopfr:x@127.0.0.1:5433/shopfr") {
		t.Errorf("DSN PostgreSQL : %s", dsn)
	}
}

func TestDSNUnknownEngine(t *testing.T) {
	if _, err := DSN(Endpoint{Engine: "oracle"}, "a", "b", "c"); err == nil {
		t.Error("moteur inconnu accepté")
	}
}

func TestAMQPDSNHasNoEncodedSlash(t *testing.T) {
	dsn := AMQPDSN("", 0, "shopfr", "x", "shopfr")
	if strings.Contains(dsn, "%2F") {
		t.Errorf("le vhost sans barre oblique évite tout encodage : %s", dsn)
	}
	if dsn != "amqp://shopfr:x@127.0.0.1:5672/shopfr" {
		t.Errorf("DSN AMQP : %s", dsn)
	}
}

func TestEnvVarName(t *testing.T) {
	cases := map[string]string{
		"":          "DATABASE_URL",
		"analytics": "ANALYTICS_DATABASE_URL",
		"read-only": "READ_ONLY_DATABASE_URL",
	}
	for role, want := range cases {
		if got := EnvVarName(role); got != want {
			t.Errorf("EnvVarName(%q) = %q, attendu %q", role, got, want)
		}
	}
}
