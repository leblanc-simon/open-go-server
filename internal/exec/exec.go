// Package exec abstrait l'exécution des commandes système. Cette indirection
// permet le mode simulé, la journalisation exhaustive et, à terme, un backend SSH
// sans toucher aux providers.
package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Command décrit une commande à exécuter. Les arguments sont toujours séparés :
// aucune commande ne passe par un shell, ce qui exclut l'injection.
type Command struct {
	Name  string
	Args  []string
	Env   []string
	Dir   string
	Stdin []byte
	// StdinStream, fourni, est lu au fil de l'eau à la place de Stdin : une
	// sauvegarde à recharger pèse ce que pèse la base, et la tenir en entier en
	// mémoire — plusieurs fois — ne tenait pas sur une base de plusieurs
	// gigaoctets (R1-57, D122).
	StdinStream io.Reader
	// AsUser exécute la commande sous l'identité d'un compte, par setpriv —
	// runuser à défaut —, jamais par su (D122, voir Local.Run). Aucun shell
	// n'intervient : le compte d'un projet peut en avoir un fermé (D62), et
	// l'argv est passé tel quel. L'environnement est construit de toutes pièces
	// (userEnv) et le répertoire courant est Dir, « / » à défaut. La commande
	// ouvre sa propre session, sans terminal de contrôle (procAttr), et ne reçoit
	// aucun descripteur de fichier de root : ses flux passent par des tubes.
	AsUser string
	// Stdout et Stderr, fournis, reçoivent la sortie au fil de l'eau au lieu
	// qu'elle soit retenue dans Result. Un flux sans fin — « journalctl -f » —
	// s'accumulait sinon en mémoire sans rien montrer (Q-66). Un flux d'erreur
	// fourni n'est plus cité par l'erreur de Run.
	Stdout io.Writer
	Stderr io.Writer
}

// String rend la commande lisible pour le plan et les journaux.
func (c Command) String() string {
	parts := append([]string{c.Name}, c.Args...)
	s := strings.Join(parts, " ")
	if c.AsUser != "" {
		return fmt.Sprintf("(en tant que %s) %s", c.AsUser, s)
	}
	return s
}

// shellQuote assemble un argv en script shell sûr. Les guillemets simples
// protègent tout, y compris les métacaractères ; un guillemet simple présent dans
// la valeur est fermé, échappé, puis réouvert.
func shellQuote(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(parts, " ")
}

// ShellLine compose une ligne de commande à afficher, que l'exploitant copiera
// dans un shell root : un argument sans métacaractère reste nu, lisible ; tout
// autre est cité comme shellQuote le fait. Un nom venu d'une archive ou d'une
// déclaration — un fichier de vhost est libre — ne doit jamais faire exécuter
// autre chose que la commande affichée.
func ShellLine(argv ...string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./:@%+=,-") == "" {
			parts[i] = a
			continue
		}
		parts[i] = shellQuote([]string{a})
	}
	return strings.Join(parts, " ")
}

// Missing dit qu'une commande a échoué faute d'exécutable : l'outil n'est pas
// installé. Un appelant pour qui l'outil est facultatif — at, cron — s'en tait.
func Missing(err error) bool { return errors.Is(err, osexec.ErrNotFound) }

// Result porte la sortie d'une commande.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Executor exécute des commandes.
type Executor interface {
	Run(ctx context.Context, cmd Command) (Result, error)
	// Simulated indique qu'aucune action réelle n'est entreprise.
	Simulated() bool
}

// Local exécute réellement les commandes sur la machine.
type Local struct {
	// Trace reçoit chaque commande exécutée, pour la journalisation en mode verbeux.
	Trace func(Command, Result, error)
}

// Simulated répond faux : les commandes sont réellement exécutées.
func (l *Local) Simulated() bool { return false }

// Run exécute la commande et renvoie sa sortie.
func (l *Local) Run(ctx context.Context, cmd Command) (Result, error) {
	name, args := cmd.Name, cmd.Args
	env, dir := cmd.Env, cmd.Dir
	if cmd.AsUser != "" {
		// « su -s /bin/bash -c » ouvrait une session PAM et, pour -c, une
		// nouvelle session Unix dans un processus fils : le shell de
		// l'utilisateur sortait du groupe de processus qu'ogs tue à l'annulation,
		// et une installation de runtime ou « ogs cron run » interrompus
		// continuaient sans lui, hors du verrou global (R4-27). Sans « -l », su
		// laissait en outre passer tout l'environnement de root. setpriv change
		// d'identité puis exécute la commande elle-même, et la nouvelle session —
		// la protection de su contre TIOCSTI — est ouverte par ogs sur ce
		// processus même (procAttr).
		// Une entrée qui est un fichier ouvert — un terminal, un tube que rien
		// ne ferme — serait recopiée par un goroutine d'os/exec que WaitDelay
		// n'interrompt pas : Wait restait bloqué au-delà de l'annulation. Elle
		// est refusée ; un lecteur qui finit (une sauvegarde lue en flux) passe.
		if _, ok := cmd.StdinStream.(*os.File); ok {
			err := fmt.Errorf("échec de « %s » : une commande sous l'identité d'un compte ne reçoit pas un "+
				"fichier ouvert en entrée (terminal ou tube) — passer un lecteur qui se termine", cmd)
			if l.Trace != nil {
				l.Trace(cmd, Result{}, err)
			}
			return Result{}, err
		}
		var err error
		name, args, env, err = l.asUser(cmd)
		if err != nil {
			err = fmt.Errorf("échec de « %s » : %w", cmd, err)
			if l.Trace != nil {
				l.Trace(cmd, Result{}, err)
			}
			return Result{}, err
		}
		// Le répertoire courant de root peut être fermé au compte : « / » à
		// défaut d'un répertoire demandé.
		if dir == "" {
			dir = "/"
		}
	}
	c := osexec.CommandContext(ctx, name, args...)
	// Le processus est placé dans son propre groupe, et l'annulation tue tout le
	// groupe : sans cela, tuer un shell laisserait ses enfants en vie, et Wait
	// resterait bloqué sur les tuyaux qu'ils ont hérités.
	c.SysProcAttr = procAttr(cmd.AsUser != "")
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	// Filet de sécurité : si un enfant survit malgré tout, les tuyaux sont fermés
	// après ce délai plutôt que d'attendre indéfiniment.
	c.WaitDelay = 3 * time.Second
	c.Dir = dir
	if len(env) > 0 {
		c.Env = env
	}
	if cmd.StdinStream != nil {
		c.Stdin = cmd.StdinStream
	} else if len(cmd.Stdin) > 0 {
		c.Stdin = bytes.NewReader(cmd.Stdin)
	}
	var out, errBuf bytes.Buffer
	c.Stdout, c.Stderr = &out, &errBuf
	if cmd.Stdout != nil {
		c.Stdout = cmd.Stdout
	}
	if cmd.Stderr != nil {
		c.Stderr = cmd.Stderr
	}
	if cmd.AsUser != "" {
		c.Stdout, c.Stderr = noFileW(c.Stdout), noFileW(c.Stderr)
	}
	err := c.Run()
	res := Result{Stdout: out.String(), Stderr: errBuf.String()}
	var ee *osexec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
	}
	// Une annulation se reconnaît comme telle : le processus est tué, et
	// « signal: killed » ne dirait pas qu'on l'a demandé. Les deux causes restent
	// accessibles à errors.Is et errors.As.
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("%w (%w)", ctx.Err(), err)
	}
	if l.Trace != nil {
		l.Trace(cmd, res, err)
	}
	if err != nil {
		return res, fmt.Errorf("échec de « %s » : %w%s", cmd, err, stderrSuffix(res.Stderr))
	}
	return res, nil
}

// procAttr rend les attributs du processus d'une commande.
//
// Une commande lancée sous l'identité d'un projet ouvre une **nouvelle session**
// (setsid), dont elle est le chef — son groupe de processus a son pid, que
// l'annulation tue comme les autres —, et n'a donc aucun terminal de contrôle.
// Restée dans la session d'ogs, elle gardait celui de root : elle ouvrait
// /dev/tty et y poussait par ioctl(TIOCSTI) une ligne que le shell root
// exécutait au retour d'ogs (noyau < 6.2, ou dev.tty.legacy_tiocsti=1). Le
// setsid de « su -c » était cette protection ; setpriv ne la fait pas (D122).
// Une commande root garde la session d'ogs : « journalctl --no-pager »
// écrit sur le terminal de l'exploitant.
func procAttr(asUser bool) *syscall.SysProcAttr {
	if asUser {
		return &syscall.SysProcAttr{Setsid: true}
	}
	return &syscall.SysProcAttr{Setpgid: true}
}

// noFileW masque un fichier ouvert derrière une interface : os/exec passe alors
// un tube, jamais le descripteur de root lui-même. Un terminal passé tel quel à
// une commande d'un projet lui laisserait un descripteur sur le terminal de
// l'exploitant. L'entrée, elle, n'est jamais un fichier sous AsUser (Local.Run).
func noFileW(w io.Writer) io.Writer {
	if f, ok := w.(*os.File); ok {
		return struct{ io.Writer }{f}
	}
	return w
}

func stderrSuffix(stderr string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		return ""
	}
	return " : " + firstLine(s)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Recorder enregistre les commandes sans les exécuter. Il sert au mode simulé et
// aux tests unitaires des providers.
type Recorder struct {
	mu    sync.Mutex
	calls []Command
	// Responses associe un préfixe de commande à une sortie simulée.
	Responses map[string]Result
	// Errors associe un préfixe de commande à une erreur simulée.
	Errors map[string]error
}

// NewRecorder construit un enregistreur vide.
func NewRecorder() *Recorder {
	return &Recorder{Responses: map[string]Result{}, Errors: map[string]error{}}
}

// Simulated répond vrai.
func (r *Recorder) Simulated() bool { return true }

// Run enregistre la commande et renvoie la réponse simulée éventuelle.
func (r *Recorder) Run(_ context.Context, cmd Command) (Result, error) {
	// Un flux d'entrée est consommé, comme le ferait la commande, et retenu dans
	// Stdin : les assertions des tests le relisent là.
	if cmd.StdinStream != nil {
		data, err := io.ReadAll(cmd.StdinStream)
		if err != nil {
			return Result{ExitCode: 1}, err
		}
		cmd.Stdin, cmd.StdinStream = data, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, cmd)
	key := cmd.Name
	if len(cmd.Args) > 0 {
		key = cmd.Name + " " + cmd.Args[0]
	}
	if err, ok := r.Errors[key]; ok {
		return Result{ExitCode: 1}, err
	}
	if res, ok := r.Responses[key]; ok {
		// Comme Local, un flux fourni reçoit la sortie au lieu de Result.
		if cmd.Stdout != nil {
			_, _ = io.WriteString(cmd.Stdout, res.Stdout)
			res.Stdout = ""
		}
		if cmd.Stderr != nil {
			_, _ = io.WriteString(cmd.Stderr, res.Stderr)
			res.Stderr = ""
		}
		return res, nil
	}
	return Result{}, nil
}

// Calls renvoie les commandes enregistrées.
func (r *Recorder) Calls() []Command {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Command, len(r.calls))
	copy(out, r.calls)
	return out
}

// Contains indique si une commande contenant la sous-chaîne donnée a été appelée.
func (r *Recorder) Contains(sub string) bool {
	for _, c := range r.Calls() {
		if strings.Contains(c.String(), sub) {
			return true
		}
	}
	return false
}
