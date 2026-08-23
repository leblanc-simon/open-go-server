package db

import (
	"fmt"
	"net/url"
	"strconv"
)

// Endpoint décrit le point de connexion d'un moteur.
type Endpoint struct {
	Engine  string
	Host    string
	Port    int
	Version string
}

// DSN construit la chaîne de connexion applicative. Le mot de passe est encodé
// par net/url : notre alphabet contient « + » et « = », qui ont une signification
// dans une URL et doivent donc être échappés.
func DSN(e Endpoint, database, user, password string) (string, error) {
	var scheme string
	switch e.Engine {
	case "mysql", "mariadb":
		scheme = "mysql"
	case "postgres":
		scheme = "postgresql"
	default:
		return "", fmt.Errorf("moteur %q inconnu", e.Engine)
	}
	host := e.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := e.Port
	if port == 0 {
		if scheme == "mysql" {
			port = 3306
		} else {
			port = 5432
		}
	}
	u := &url.URL{
		Scheme: scheme,
		User:   url.UserPassword(user, password),
		Host:   host + ":" + strconv.Itoa(port),
		Path:   "/" + database,
	}
	q := url.Values{}
	if e.Version != "" {
		// Doctrine s'appuie sur serverVersion pour choisir sa plateforme SQL sans
		// interroger le serveur au démarrage.
		q.Set("serverVersion", e.Version)
	}
	switch scheme {
	case "mysql":
		q.Set("charset", "utf8mb4")
	case "postgresql":
		q.Set("charset", "utf8")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// AMQPDSN construit la chaîne de connexion RabbitMQ. Le vhost étant déclaré sans
// barre oblique initiale (D9), aucun encodage %2F n'est nécessaire.
func AMQPDSN(host string, port int, user, password, vhost string) string {
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 5672
	}
	u := &url.URL{
		Scheme: "amqp",
		User:   url.UserPassword(user, password),
		Host:   host + ":" + strconv.Itoa(port),
		Path:   "/" + vhost,
	}
	return u.String()
}

// EnvVarName déduit le nom de variable d'environnement d'une base. La base
// principale porte DATABASE_URL, les suivantes un nom dérivé de leur rôle.
func EnvVarName(role string) string {
	if role == "" {
		return "DATABASE_URL"
	}
	out := make([]byte, 0, len(role)+14)
	for i := 0; i < len(role); i++ {
		c := role[i]
		switch {
		case c >= 'a' && c <= 'z':
			out = append(out, c-32)
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out) + "_DATABASE_URL"
}
