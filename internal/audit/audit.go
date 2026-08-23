// Package audit écrit le journal des opérations, en base et dans un fichier
// append-only.
package audit

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/leviathan/ogs/internal/store"
)

// Logger journalise les étapes d'une exécution.
type Logger struct {
	mu    sync.Mutex
	st    *store.Store
	file  *os.File
	runID string
	actor string
}

// NewRunID produit un identifiant d'exécution court, repris dans les messages
// affichés à l'utilisateur pour retrouver l'opération dans le journal.
func NewRunID() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// Actor décrit l'appelant. L'outil étant root-strict, cette information est
// indicative : elle sert la traçabilité, pas la preuve, puisque quiconque est
// déjà root peut la falsifier (§14.3).
func Actor() string {
	uid := os.Getuid()
	if sudo := os.Getenv("SUDO_USER"); sudo != "" {
		return fmt.Sprintf("%s (via sudo, uid %d)", sudo, uid)
	}
	if u := os.Getenv("USER"); u != "" {
		return fmt.Sprintf("%s (uid %d)", u, uid)
	}
	return fmt.Sprintf("uid %d", uid)
}

// New ouvre le journal. L'écriture dans le fichier se fait en mode ajout
// exclusif ; une entrée déjà écrite n'est jamais modifiée.
func New(st *store.Store, path, runID, actor string) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		runID = NewRunID()
	}
	if actor == "" {
		actor = Actor()
	}
	return &Logger{st: st, file: f, runID: runID, actor: actor}, nil
}

// RunID renvoie l'identifiant de l'exécution en cours.
func (l *Logger) RunID() string { return l.runID }

// Close referme le fichier de journal.
func (l *Logger) Close() error {
	if l.file == nil {
		return nil
	}
	return l.file.Close()
}

// Record consigne une opération. Une erreur d'écriture du journal n'interrompt
// pas l'opération en cours mais est remontée à l'appelant.
func (l *Logger) Record(project, resource, action, outcome, detail string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	at := time.Now()
	line := fmt.Sprintf("%s run=%s actor=%q projet=%s ressource=%s action=%s résultat=%s %s\n",
		at.UTC().Format(time.RFC3339), l.runID, l.actor, orDash(project), orDash(resource), action, outcome, detail)
	var firstErr error
	if l.file != nil {
		if _, err := l.file.WriteString(line); err != nil {
			firstErr = err
		}
	}
	if l.st != nil {
		if err := l.st.AppendAudit(store.AuditEntry{
			RunID: l.runID, At: at, Actor: l.actor, Project: project,
			Resource: resource, Action: action, Outcome: outcome, Detail: detail,
		}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
