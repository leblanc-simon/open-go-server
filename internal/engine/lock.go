package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Lock est le verrou global d'exécution : deux applications simultanées ne
// doivent jamais se croiser sur la configuration du système.
type Lock struct {
	file *os.File
	path string
}

// ErrLocked signale qu'une autre instance détient le verrou.
type ErrLocked struct {
	Path string
	PID  string
}

func (e ErrLocked) Error() string {
	if e.PID != "" {
		return fmt.Sprintf("une autre instance d'ogs est en cours (pid %s, verrou %s)", e.PID, e.Path)
	}
	return fmt.Sprintf("une autre instance d'ogs est en cours (verrou %s)", e.Path)
}

// Acquire prend le verrou sans attendre. L'échec est immédiat et explicite,
// plutôt qu'une attente silencieuse : l'appelant doit savoir pourquoi il ne peut
// pas travailler.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		pid := readPID(f)
		f.Close()
		return nil, ErrLocked{Path: path, PID: pid}
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{file: f, path: path}, nil
}

// Release libère le verrou.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	if cerr := l.file.Close(); err == nil {
		err = cerr
	}
	l.file = nil
	return err
}

func readPID(f *os.File) string {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	return strings.TrimSpace(string(buf[:n]))
}
