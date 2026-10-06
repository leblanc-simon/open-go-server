// Package verify éprouve un fichier de configuration écrit par ogs avec l'outil
// qui le lira, avant que celui-ci ne le relise (D117).
//
// Trois outils, trois façons de ne pas refuser à tort. Un validateur qui se
// plaint de ce qu'ogs n'a pas écrit — une unité tierce, un journal absent, une
// prison de l'administrateur — ferait rendre un fichier correct et bloquerait
// le parc sur un défaut qui n'est pas le sien :
//
//   - systemd-analyze verify charge l'unité et ses dépendances, et se plaint de
//     tout : seules comptent les lignes localisées dans le fichier d'ogs
//     (« <chemin>:<ligne>: … »). Un binaire encore absent (« is not executable »)
//     est préfixé du nom de l'unité, non du chemin : il est écarté de lui-même,
//     l'application pouvant n'être déployée qu'après l'apply ;
//   - logrotate -d ne lit que le fichier d'ogs ; seules comptent les erreurs et
//     les avertissements localisés dans ce fichier — une option ignorée l'est en
//     code 0 —, un journal absent ou un répertoire aux droits lâches étant des
//     constats d'exploitation, non des défauts du fichier ;
//   - fail2ban-client -t éprouve toute la configuration de la machine : le
//     refus n'est retenu que s'il disparaît une fois le fichier précédent rendu.
//
// Un outil absent ne valide rien et ne refuse rien : ogs n'installe pas l'outil,
// et le fichier n'a alors personne pour le lire.
package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"leblanc.io/open-go-server/internal/engine"
	"leblanc.io/open-go-server/internal/exec"
	"leblanc.io/open-go-server/internal/fs"
)

// Refused est le refus d'un fichier par l'outil qui le lit. Le fichier a été
// rendu par l'étape qui l'avait écrit.
type Refused struct {
	Tool   string
	Path   string
	Detail string
}

func (e *Refused) Error() string {
	return fmt.Sprintf("%s refuse %s : %s — le fichier est rendu à son état précédent", e.Tool, e.Path, e.Detail)
}

// IsRefused dit si err est un refus de validateur.
func IsRefused(err error) bool {
	var r *Refused
	return errors.As(err, &r)
}

// active dit si un validateur doit tourner : jamais en simulation.
func active(ex exec.Executor) bool { return ex != nil && !ex.Simulated() }

// located retient les lignes de sortie localisées dans path : « path:N… ».
// prefix est ce qui peut précéder le chemin (« error: » pour logrotate).
func located(out, path, prefix string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		l = strings.TrimSpace(strings.TrimPrefix(l, prefix))
		if strings.HasPrefix(l, path+":") {
			lines = append(lines, l)
		}
	}
	return lines
}

// SystemdUnit éprouve une unité écrite par ogs. Un modèle (« x@.service ») est
// vérifié par systemd sous une instance d'essai.
func SystemdUnit(ctx context.Context, ex exec.Executor, path string) error {
	if !active(ex) {
		return nil
	}
	res, err := ex.Run(ctx, exec.Command{Name: "systemd-analyze",
		Args: []string{"verify", "--man=no", "--recursive-errors=no", path}})
	if err != nil && unknownOption(res, "--recursive-errors") {
		// systemd < 250 (Ubuntu 22.04 : 249) ne connaît pas l'option : sans elle,
		// le code de retour juge aussi les dépendances, mais seules les lignes
		// localisées dans le fichier d'ogs font refuser.
		res, err = ex.Run(ctx, exec.Command{Name: "systemd-analyze",
			Args: []string{"verify", "--man=no", path}})
	}
	if err == nil || exec.Missing(err) {
		return nil
	}
	if lines := located(res.Stdout+"\n"+res.Stderr, path, ""); len(lines) > 0 {
		return &Refused{Tool: "systemd-analyze verify", Path: path, Detail: strings.Join(lines, " ; ")}
	}
	// Les plaintes portent sur autre chose que le fichier d'ogs : unités
	// tierces, binaire pas encore déployé, ou une version de systemd qui ignore
	// une option — rien qui tienne au fichier.
	return nil
}

// Logrotate éprouve le fichier de rotation d'ogs, seul, sans toucher à l'état
// de logrotate : -d n'écrit rien, et l'état lu est vide.
func Logrotate(ctx context.Context, ex exec.Executor, path string) error {
	if !active(ex) {
		return nil
	}
	res, err := ex.Run(ctx, exec.Command{Name: "logrotate", Args: []string{"-d", "-s", "/dev/null", path}})
	if exec.Missing(err) {
		return nil
	}
	// Une option inconnue n'est qu'un avertissement pour logrotate, qui ignore la
	// ligne et rend 0 : une directive de rotation perdue sans un mot. Elle fait
	// refuser le fichier comme une erreur, pourvu qu'elle soit localisée dans
	// le fichier d'ogs.
	out := res.Stdout + "\n" + res.Stderr
	lines := append(located(out, path, "error:"), located(out, path, "warning:")...)
	if len(lines) > 0 {
		return &Refused{Tool: "logrotate -d", Path: path, Detail: strings.Join(lines, " ; ")}
	}
	return nil
}

// Fail2ban éprouve la configuration de fail2ban après l'écriture de path. Le
// test portant sur toute la machine, un échec n'est imputé au fichier d'ogs
// que s'il disparaît une fois le fichier précédent rendu : restore rend ce
// fichier, rewrite réécrit le nouveau quand le défaut n'est pas le sien.
func Fail2ban(ctx context.Context, ex exec.Executor, path string, restore, rewrite func() error) error {
	if !active(ex) {
		return nil
	}
	test := func() (exec.Result, error) {
		return ex.Run(ctx, exec.Command{Name: "fail2ban-client", Args: []string{"-t"}})
	}
	res, err := test()
	if err == nil || exec.Missing(err) {
		return nil
	}
	detail := firstLines(res)
	if rerr := restore(); rerr != nil {
		return fmt.Errorf("fail2ban-client -t refuse la configuration (%s), et %s n'a pas pu être rendu : %w",
			detail, path, rerr)
	}
	if _, err := test(); err != nil {
		// Le défaut précède ce fichier : la configuration de la machine était
		// déjà refusée. Le nouveau fichier n'y est pour rien, il est remis.
		if werr := rewrite(); werr != nil {
			return werr
		}
		return nil
	}
	return &Refused{Tool: "fail2ban-client -t", Path: path, Detail: detail}
}

func firstLines(res exec.Result) string {
	var out []string
	for _, l := range strings.Split(res.Stderr+"\n"+res.Stdout, "\n") {
		if l = strings.TrimSpace(l); l != "" && (strings.Contains(l, "ERROR") || len(out) == 0) {
			out = append(out, l)
		}
		if len(out) >= 3 {
			break
		}
	}
	if len(out) == 0 {
		return "configuration refusée"
	}
	return strings.Join(out, " ; ")
}

// Restore rend le fichier précédent quand err est un refus : l'étape qui a
// écrit le fichier le rend elle-même, le moteur ne compensant jamais l'étape en
// échec. Sans fichier précédent, le nouveau est retiré. Toute autre erreur, ou
// aucune, est rendue telle quelle.
func Restore(err error, path string, previous []byte, mode os.FileMode) error {
	if !IsRefused(err) {
		return err
	}
	var rerr error
	if previous == nil {
		rerr = os.Remove(path)
	} else {
		rerr = fs.WriteAtomic(path, previous, mode, fs.KeepOwner)
	}
	if rerr != nil && !os.IsNotExist(rerr) {
		// Le refus n'est plus enveloppé : le fichier refusé est resté en place,
		// l'étape ne doit pas être tolérée.
		return fmt.Errorf("%v ; le fichier n'a pas pu être rendu : %w", err, rerr)
	}
	return err
}

// unknownOption dit si la commande a refusé une option qu'elle ne connaît pas.
func unknownOption(res exec.Result, option string) bool {
	out := res.Stdout + "\n" + res.Stderr
	return strings.Contains(out, option) &&
		(strings.Contains(out, "unrecognized option") || strings.Contains(out, "unknown option"))
}

// ReadPrevious relit le fichier en place avant son écriture : son contenu et
// son mode, que Restore rend tels quels. Absent, il rend nil.
func ReadPrevious(path string) ([]byte, os.FileMode) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0
	}
	return data, info.Mode().Perm()
}

// Tolerate rend toléré (engine.Tolerated) le refus d'un fichier qui existait :
// rendu à son état précédent, il continue de servir ce qui tournait, et une
// garde neuve ne doit pas dérouler le plan d'un site qui fonctionnait (D117).
// Un fichier neuf refusé reste une erreur ordinaire : rien ne tournait sur lui.
func Tolerate(err error, previous []byte) error {
	if previous != nil && IsRefused(err) {
		return engine.Tolerated(err)
	}
	return err
}
