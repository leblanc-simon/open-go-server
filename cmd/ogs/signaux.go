package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// interruptions sont les signaux qui annulent le contexte de la commande (D122).
//
// SIGHUP y figure comme SIGTERM : c'est celui d'une session ssh coupée, d'un
// terminal fermé. Laissé à son effet par défaut, il tuait ogs sur-le-champ, au
// milieu d'une saga — ni compensation, ni rechargement groupé, ni détachement
// passé le point de non-retour —, et les commandes qu'ogs avait lancées, dans
// leur propre groupe de processus (exec.Local), lui survivaient hors du verrou
// global (R1-69).
var interruptions = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

// watchSignals rend un contexte qu'annule le premier signal d'interruption reçu.
//
// Trois règles :
//
//   - un signal **ignoré au lancement** le reste. « nohup ogs apply » ou une
//     commande lancée en arrière-plan par un script non interactif (SIGINT
//     ignoré) demandent explicitement de ne pas être interrompus : Go, lui,
//     rétablit un signal ignoré dès qu'on s'y abonne ;
//   - un signal suivant est **dit**, jamais avalé en silence : l'interruption
//     est déjà en cours, et rendre au second signal son effet par défaut
//     tuerait une suppression passé son point de non-retour (§20) ;
//   - SIGPIPE est capté sans rien annuler : une écriture vers une sortie fermée
//     — la session ssh sans terminal qui disparaît, un « | head » — échoue en
//     EPIPE au lieu de tuer ogs. Go le laisse sinon tuer le programme sur ses
//     descripteurs 1 et 2, et ce serait la mort en pleine saga que SIGHUP vient
//     d'éviter. Il ne vaut pas interruption : une écriture vers l'entrée d'un
//     client qui a fini — un client SGBD qui s'arrête sur une erreur — lève le
//     même signal. Un gestionnaire, contrairement à un signal ignoré, ne passe
//     pas aux commandes lancées : elles gardent le comportement par défaut.
//
// stop rend aux signaux leur effet et annule le contexte.
func watchSignals(parent context.Context, warn io.Writer) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	var caught []os.Signal
	for _, s := range interruptions {
		if !signal.Ignored(s) {
			caught = append(caught, s)
		}
	}
	sigs := make(chan os.Signal, len(interruptions))
	if len(caught) > 0 {
		signal.Notify(sigs, caught...)
	}
	pipe := make(chan os.Signal, 1)
	signal.Notify(pipe, syscall.SIGPIPE)
	done := make(chan struct{})
	go func() {
		first := true
		for {
			select {
			case <-done:
				return
			case <-pipe:
			case s := <-sigs:
				if first {
					first = false
					cancel()
					continue
				}
				// Vers un terminal fermé, l'écriture échoue : rien d'autre à faire.
				_, _ = fmt.Fprintf(warn, "\nogs : %s reçu — l'interruption est déjà en cours : ogs défait ce "+
					"qu'il a commencé, ou achève ce qui ne peut plus l'être, puis rend la main ; kill -9 l'arrête "+
					"sur-le-champ, au risque d'un état à moitié défait\n", signalName(s))
			}
		}
	}()
	stop := func() {
		signal.Stop(sigs)
		signal.Stop(pipe)
		select {
		case <-done:
		default:
			close(done)
		}
		cancel()
	}
	return ctx, stop
}

// signalName nomme un signal comme l'exploitant le connaît.
func signalName(s os.Signal) string {
	switch s {
	case os.Interrupt:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGHUP:
		return "SIGHUP"
	}
	return s.String()
}
