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
	// AsUser exécute la commande sous une autre identité. L'implémentation force
	// le shell : le compte d'un projet peut en avoir un fermé (D62).
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
	if cmd.AsUser != "" {
		// -s force le shell : le compte du projet peut en avoir un fermé — c'est un
		// réglage du parc (D62) et un compte adopté porte le sien —, ce qui ferait
		// échouer un su ordinaire.
		//
		// Chaque élément est cité : su attend un script, non un argv, et recoller
		// les arguments avec des espaces ferait redécouper par le shell tout
		// argument qui en contient — « sh -c "flock … sh -c 'date …'" » perdrait
		// ses imbrications et flock recevrait une commande vide.
		script := shellQuote(append([]string{cmd.Name}, cmd.Args...))
		name, args = "su", []string{"-s", "/bin/bash", "-c", script, cmd.AsUser}
	}
	c := osexec.CommandContext(ctx, name, args...)
	// Le processus est placé dans son propre groupe, et l'annulation tue tout le
	// groupe : sans cela, tuer un shell laisserait ses enfants en vie, et Wait
	// resterait bloqué sur les tuyaux qu'ils ont hérités.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	// Filet de sécurité : si un enfant survit malgré tout, les tuyaux sont fermés
	// après ce délai plutôt que d'attendre indéfiniment.
	c.WaitDelay = 3 * time.Second
	c.Dir = cmd.Dir
	if len(cmd.Env) > 0 {
		c.Env = cmd.Env
	}
	if len(cmd.Stdin) > 0 {
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
