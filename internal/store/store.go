// Package store porte l'état observé, les secrets chiffrés, les allocations de
// numéros et le journal d'audit, dans une base SQLite unique.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pilote SQLite en Go pur (compatible CGO_ENABLED=0)
)

// Store encapsule la base d'état.
type Store struct {
	db   *sql.DB
	path string
}

// ErrNotFound signale une ligne absente.
var ErrNotFound = errors.New("enregistrement absent")

// Open ouvre la base et applique les migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// La base est écrite séquentiellement sous verrou global : une seule
	// connexion évite tout conflit d'écriture.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migration de %s : %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close ferme la base.
func (s *Store) Close() error { return s.db.Close() }

// Path renvoie le chemin du fichier de base.
func (s *Store) Path() string { return s.path }

// migrations est la liste ordonnée des migrations. Une migration n'est jamais
// modifiée après publication : on en ajoute une nouvelle.
var migrations = []string{
	// 1 — schéma initial
	`
	CREATE TABLE projects (
		id           TEXT PRIMARY KEY,
		host         TEXT NOT NULL,
		env          TEXT NOT NULL,
		effective_host TEXT NOT NULL,
		home         TEXT NOT NULL,
		app_kind     TEXT NOT NULL,
		app_version  TEXT NOT NULL DEFAULT '',
		status       TEXT NOT NULL DEFAULT 'active',
		spec_hash    TEXT NOT NULL DEFAULT '',
		created_at   TEXT NOT NULL,
		updated_at   TEXT NOT NULL
	);
	CREATE TABLE resources (
		project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
		kind       TEXT NOT NULL,
		name       TEXT NOT NULL,
		attributes TEXT NOT NULL DEFAULT '{}',
		created_at TEXT NOT NULL,
		PRIMARY KEY (project_id, kind, name)
	);
	CREATE TABLE managed_files (
		path        TEXT PRIMARY KEY,
		project_id  TEXT REFERENCES projects(id) ON DELETE CASCADE,
		resource    TEXT NOT NULL,
		fingerprint TEXT NOT NULL,
		mode        TEXT NOT NULL DEFAULT '',
		written_at  TEXT NOT NULL
	);
	CREATE TABLE allocations (
		scope      TEXT NOT NULL,
		number     INTEGER NOT NULL,
		project_id TEXT NOT NULL,
		name       TEXT NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY (scope, number)
	);
	CREATE TABLE secrets (
		scope      TEXT NOT NULL,
		name       TEXT NOT NULL,
		nonce      BLOB NOT NULL,
		ciphertext BLOB NOT NULL,
		version    INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL,
		rotated_at TEXT,
		PRIMARY KEY (scope, name)
	);
	CREATE TABLE audit_log (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id     TEXT NOT NULL,
		at         TEXT NOT NULL,
		actor      TEXT NOT NULL DEFAULT '',
		project_id TEXT NOT NULL DEFAULT '',
		resource   TEXT NOT NULL DEFAULT '',
		action     TEXT NOT NULL,
		outcome    TEXT NOT NULL,
		detail     TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX audit_log_at ON audit_log(at);
	CREATE INDEX audit_log_project ON audit_log(project_id);
	CREATE TABLE unmanaged (
		kind       TEXT NOT NULL,
		name       TEXT NOT NULL,
		detail     TEXT NOT NULL DEFAULT '',
		seen_at    TEXT NOT NULL,
		PRIMARY KEY (kind, name)
	);
	`,
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return err
	}
	for i := current; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d : %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`,
			i+1, now()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion renvoie la version de schéma appliquée.
func (s *Store) SchemaVersion() (int, error) {
	var v int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v)
	return v, err
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// ProjectRow est la vue persistée d'un projet.
type ProjectRow struct {
	ID            string
	Host          string
	Env           string
	EffectiveHost string
	Home          string
	AppKind       string
	AppVersion    string
	Status        string
	SpecHash      string
	CreatedAt     string
	UpdatedAt     string
}

// UpsertProject enregistre ou met à jour un projet.
func (s *Store) UpsertProject(p ProjectRow) error {
	_, err := s.db.Exec(`
		INSERT INTO projects (id, host, env, effective_host, home, app_kind, app_version, status, spec_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), 'active'), ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			host = excluded.host, env = excluded.env, effective_host = excluded.effective_host,
			home = excluded.home, app_kind = excluded.app_kind, app_version = excluded.app_version,
			status = excluded.status, spec_hash = excluded.spec_hash, updated_at = excluded.updated_at`,
		p.ID, p.Host, p.Env, p.EffectiveHost, p.Home, p.AppKind, p.AppVersion, p.Status, p.SpecHash, now(), now())
	return err
}

// Project lit un projet.
func (s *Store) Project(id string) (ProjectRow, error) {
	var p ProjectRow
	err := s.db.QueryRow(`SELECT id, host, env, effective_host, home, app_kind, app_version,
		status, spec_hash, created_at, updated_at FROM projects WHERE id = ?`, id).
		Scan(&p.ID, &p.Host, &p.Env, &p.EffectiveHost, &p.Home, &p.AppKind, &p.AppVersion,
			&p.Status, &p.SpecHash, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, fmt.Errorf("%w : projet %q", ErrNotFound, id)
	}
	return p, err
}

// Projects liste les projets, triés par identifiant.
func (s *Store) Projects() ([]ProjectRow, error) {
	rows, err := s.db.Query(`SELECT id, host, env, effective_host, home, app_kind, app_version,
		status, spec_hash, created_at, updated_at FROM projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectRow
	for rows.Next() {
		var p ProjectRow
		if err := rows.Scan(&p.ID, &p.Host, &p.Env, &p.EffectiveHost, &p.Home, &p.AppKind,
			&p.AppVersion, &p.Status, &p.SpecHash, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteProject supprime un projet et, par cascade, ses ressources.
func (s *Store) DeleteProject(id string) error {
	_, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, id)
	return err
}

// SetProjectStatus change le statut d'un projet (active, archived).
func (s *Store) SetProjectStatus(id, status string) error {
	res, err := s.db.Exec(`UPDATE projects SET status = ?, updated_at = ? WHERE id = ?`, status, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w : projet %q", ErrNotFound, id)
	}
	return nil
}

// RecordFile enregistre l'empreinte d'un fichier généré, afin de détecter plus
// tard une modification manuelle.
func (s *Store) RecordFile(path, projectID, resource, fingerprint, mode string) error {
	_, err := s.db.Exec(`
		INSERT INTO managed_files (path, project_id, resource, fingerprint, mode, written_at)
		VALUES (?, NULLIF(?, ''), ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET project_id = excluded.project_id,
			resource = excluded.resource, fingerprint = excluded.fingerprint,
			mode = excluded.mode, written_at = excluded.written_at`,
		path, projectID, resource, fingerprint, mode, now())
	return err
}

// FileFingerprint renvoie l'empreinte enregistrée pour un fichier.
func (s *Store) FileFingerprint(path string) (string, error) {
	var fp string
	err := s.db.QueryRow(`SELECT fingerprint FROM managed_files WHERE path = ?`, path).Scan(&fp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w : fichier %q", ErrNotFound, path)
	}
	return fp, err
}

// ForgetFile oublie un fichier géré (après suppression).
func (s *Store) ForgetFile(path string) error {
	_, err := s.db.Exec(`DELETE FROM managed_files WHERE path = ?`, path)
	return err
}

// Allocate réserve le plus petit numéro libre d'une plage pour une ressource.
// Les numéros de pool PHP et de port applicatif suivent une norme lisible : la
// stabilité de l'attribution compte autant que l'unicité.
func (s *Store) Allocate(scope, projectID, name string, min, max int) (int, error) {
	var existing int
	err := s.db.QueryRow(`SELECT number FROM allocations WHERE scope = ? AND project_id = ? AND name = ?`,
		scope, projectID, name).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	rows, err := s.db.Query(`SELECT number FROM allocations WHERE scope = ? ORDER BY number`, scope)
	if err != nil {
		return 0, err
	}
	taken := map[int]bool{}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return 0, err
		}
		taken[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for n := min; n <= max; n++ {
		if taken[n] {
			continue
		}
		if _, err := s.db.Exec(`INSERT INTO allocations (scope, number, project_id, name, created_at)
			VALUES (?, ?, ?, ?, ?)`, scope, n, projectID, name, now()); err != nil {
			return 0, err
		}
		return n, nil
	}
	return 0, fmt.Errorf("plage %s saturée (%d à %d) : aucun numéro disponible", scope, min, max)
}

// Release libère un numéro.
func (s *Store) Release(scope, projectID, name string) error {
	_, err := s.db.Exec(`DELETE FROM allocations WHERE scope = ? AND project_id = ? AND name = ?`,
		scope, projectID, name)
	return err
}

// Allocation est un numéro attribué à une ressource.
type Allocation struct {
	Scope   string
	Number  int
	Project string
	Name    string
}

// Allocations liste les numéros attribués, triés par plage puis par numéro.
func (s *Store) Allocations(scope string) ([]Allocation, error) {
	query := `SELECT scope, number, project_id, name FROM allocations`
	args := []any{}
	if scope != "" {
		query += ` WHERE scope = ?`
		args = append(args, scope)
	}
	query += ` ORDER BY scope, number`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Allocation
	for rows.Next() {
		var a Allocation
		if err := rows.Scan(&a.Scope, &a.Number, &a.Project, &a.Name); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PutSecret enregistre un secret scellé.
func (s *Store) PutSecret(scope, name string, nonce, ciphertext []byte) error {
	_, err := s.db.Exec(`
		INSERT INTO secrets (scope, name, nonce, ciphertext, version, created_at)
		VALUES (?, ?, ?, ?, 1, ?)
		ON CONFLICT(scope, name) DO UPDATE SET nonce = excluded.nonce,
			ciphertext = excluded.ciphertext, version = secrets.version + 1, rotated_at = ?`,
		scope, name, nonce, ciphertext, now(), now())
	return err
}

// GetSecret lit un secret scellé.
func (s *Store) GetSecret(scope, name string) (nonce, ciphertext []byte, err error) {
	err = s.db.QueryRow(`SELECT nonce, ciphertext FROM secrets WHERE scope = ? AND name = ?`,
		scope, name).Scan(&nonce, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w : secret %s/%s", ErrNotFound, scope, name)
	}
	return nonce, ciphertext, err
}

// SecretNames liste les noms de secrets d'un scope, sans jamais exposer de valeur.
func (s *Store) SecretNames(scope string) ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM secrets WHERE scope = ? ORDER BY name`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// AuditEntry est une ligne du journal d'audit.
type AuditEntry struct {
	RunID    string
	At       time.Time
	Actor    string
	Project  string
	Resource string
	Action   string
	Outcome  string
	Detail   string
}

// AppendAudit ajoute une entrée au journal.
func (s *Store) AppendAudit(e AuditEntry) error {
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	_, err := s.db.Exec(`INSERT INTO audit_log (run_id, at, actor, project_id, resource, action, outcome, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.RunID, at.UTC().Format(time.RFC3339Nano), e.Actor, e.Project, e.Resource, e.Action, e.Outcome, e.Detail)
	return err
}

// Audit lit le journal, du plus récent au plus ancien.
func (s *Store) Audit(projectID string, since time.Time, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT run_id, at, actor, project_id, resource, action, outcome, detail FROM audit_log WHERE 1 = 1`
	args := []any{}
	if projectID != "" {
		query += ` AND project_id = ?`
		args = append(args, projectID)
	}
	if !since.IsZero() {
		query += ` AND at >= ?`
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at string
		if err := rows.Scan(&e.RunID, &at, &e.Actor, &e.Project, &e.Resource, &e.Action, &e.Outcome, &e.Detail); err != nil {
			return nil, err
		}
		e.At, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, e)
	}
	return out, rows.Err()
}
