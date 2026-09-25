// Application d'essai des scénarios d'intégration : elle respecte le contrat
// qu'ogs impose à une application supervisée (§7.11) — écouter sur le socket
// désigné par SOCKET_PATH, ou à défaut sur le port donné par PORT, et rien
// d'autre. Elle rend son identité, ce qui permet de vérifier sous quel compte le
// service tourne réellement.
//
// L'option -ecoute l'emporte sur l'environnement : c'est ainsi qu'une application
// custom reçoit son adresse, par un ${LISTEN_ADDR} que systemd développe dans
// ExecStart (D75). MESSAGE, s'il est défini, est rendu dans la réponse : il vient
// du fichier d'environnement du projet.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/user"
)

var adresse = flag.String("ecoute", "", "adresse TCP d'écoute, à la place de PORT")

func main() {
	flag.Parse()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		nom := "inconnu"
		if u, err := user.Current(); err == nil {
			nom = u.Username
		}
		message := ""
		if m := os.Getenv("MESSAGE"); m != "" {
			message = ", message " + m
		}
		fmt.Fprintf(w, "application sous %s, écoute %s%s\n", nom, ecoute(), message)
	})
	// Une sonde distincte : le provider attend une réponse inférieure à 500 sur le
	// chemin déclaré dans health.path.
	http.HandleFunc("/sante", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	l, err := listener()
	if err != nil {
		log.Fatalf("écoute : %v", err)
	}
	log.Printf("écoute sur %s", ecoute())
	log.Fatal(http.Serve(l, nil))
}

func ecoute() string {
	if *adresse != "" {
		return *adresse
	}
	if s := os.Getenv("SOCKET_PATH"); s != "" {
		return "unix:" + s
	}
	return "127.0.0.1:" + os.Getenv("PORT")
}

// listener applique la règle : socket Unix si SOCKET_PATH est fourni, sinon port
// TCP sur la boucle locale exclusivement — jamais sur toutes les interfaces, ce
// qui contournerait le vhost.
func listener() (net.Listener, error) {
	if s := os.Getenv("SOCKET_PATH"); s != "" {
		// Le socket d'une exécution précédente est retiré : systemd redémarre le
		// service, le fichier survivrait et l'écoute échouerait.
		_ = os.Remove(s)
		l, err := net.Listen("unix", s)
		if err != nil {
			return nil, err
		}
		// 0660 : le serveur web est membre du groupe du projet (D36).
		if err := os.Chmod(s, 0o660); err != nil {
			return nil, err
		}
		return l, nil
	}
	if *adresse != "" {
		return net.Listen("tcp", *adresse)
	}
	port := os.Getenv("PORT")
	if port == "" {
		return nil, fmt.Errorf("ni SOCKET_PATH ni PORT dans l'environnement")
	}
	return net.Listen("tcp", "127.0.0.1:"+port)
}
