// Package exec abstrait l'exécution des commandes système. Cette indirection
// permet le mode simulé, la journalisation exhaustive et, à terme, un backend SSH
// sans toucher aux providers.
package exec

import (
	"bytes"
	"context"
	"fmt"
	osexec "os/exec"
	"strings"
	"sync"
)

// Command décrit une commande à exécuter. Les arguments sont toujours séparés :
// aucune commande ne passe par un shell, ce qui exclut l'injection.
type Command struct {
	Name  string
	Args  []string
	Env   []string
	Dir   string
	Stdin []byte
	// AsUser exécute la commande sous une autre identité. L'utilisateur d'un
	// projet ayant un shell nologin, l'implémentation force le shell.
	AsUser string
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
		// -s force le shell : l'utilisateur du projet a nologin, ce qui ferait
		// échouer un su ordinaire.
		script := strings.Join(append([]string{cmd.Name}, cmd.Args...), " ")
		name, args = "su", []string{"-s", "/bin/bash", "-c", script, cmd.AsUser}
	}
	c := osexec.CommandContext(ctx, name, args...)
	c.Dir = cmd.Dir
	if len(cmd.Env) > 0 {
		c.Env = cmd.Env
	}
	if len(cmd.Stdin) > 0 {
		c.Stdin = bytes.NewReader(cmd.Stdin)
	}
	var out, errBuf bytes.Buffer
	c.Stdout, c.Stderr = &out, &errBuf
	err := c.Run()
	res := Result{Stdout: out.String(), Stderr: errBuf.String()}
	if ee, ok := err.(*osexec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
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
