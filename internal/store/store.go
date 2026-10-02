// Package store porte l'état observé, les secrets chiffrés, les allocations de
// numéros et le journal d'audit, dans une base SQLite unique.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pilote SQLite en Go pur (compatible CGO_ENABLED=0)
)

// Store encapsule la base d'état.
type Store struct {
	db   *sql.DB
	path string

	// preview fait d'Allocate un calcul sans écriture (PreviewAllocations) : ce
	// que « plan », « diff » et « status » affichent ne doit rien réserver.
	preview bool
	// pending retient, en aperçu, les numéros déjà promis dans ce processus :
	// deux pools neufs d'un même plan ne doivent pas s'afficher sous le même.
	pending map[string]map[int]string
	// fresh énumère les numéros inscrits par ce processus, dans l'ordre :
	// CancelAllocations retire ceux d'un plan qui ne s'est pas appliqué, et rend
	// ceux qu'une réservation avait remplacés.
	fresh []freshAllocation
}

// freshAllocation est un numéro inscrit par ce processus, et celui qu'il a
// remplacé pour la même ressource, s'il en est un.
type freshAllocation struct {
	added    Allocation
	replaced *Allocation
}

// ErrNotFound signale une ligne absente.
var ErrNotFound = errors.New("enregistrement absent")

// Open ouvre la base et applique les migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	// La base est créée ici, en 0600, et non par SQLite : le mode d'un fichier
	// créé par SQLite est rogné par l'umask ambiant, et /var/lib/ogs est
	// volontairement traversable (0751, pour le challenge ACME). Surtout, SQLite
	// calque le mode de « <base>-wal » et de « <base>-shm » sur celui de la base
	// (findCreateFileMode) : poser le mode après coup laissait un journal en 0644,
	// où passent l'inventaire du parc et le journal d'audit. Le chmod couvre le
	// cas d'une base héritée d'une version antérieure, créée en 0644.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
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
	// Un journal abandonné par un processus tué garde le mode qu'il avait à sa
	// création et n'est jamais recorrigé par SQLite, qui le reprend tel quel.
	for _, suffixe := range []string{"-wal", "-shm"} {
		if err := os.Chmod(path+suffixe, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
			db.Close()
			return nil, err
		}
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
	// 2 — suivi des échecs ACME, pour ne pas épuiser les quotas de l'autorité
	`
	CREATE TABLE acme_failures (
		key     TEXT PRIMARY KEY,
		count   INTEGER NOT NULL DEFAULT 0,
		last_at TEXT NOT NULL,
		until   TEXT,
		detail  TEXT NOT NULL DEFAULT ''
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
//
// busy écarte, pour une attribution nouvelle seulement, les numéros que la
// machine tient déjà : un port en écoute (D94). Le numéro déjà attribué à la
// ressource est rendu tel quel — c'est sa propre application qui l'écoute.
//
// En aperçu (PreviewAllocations), rien n'est inscrit : le numéro rendu est celui
// qu'une application choisirait (D100). « plan » réservait sinon un numéro à
// chaque pool calculé, projet jamais appliqué compris, et la plage finissait
// saturée de numéros que rien ne servait.
func (s *Store) Allocate(scope, projectID, name string, min, max int, busy ...func(int) bool) (int, error) {
	var existing int
	err := s.db.QueryRow(`SELECT number FROM allocations WHERE scope = ? AND project_id = ? AND name = ?`,
		scope, projectID, name).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	key := projectID + "\x00" + name
	if s.preview {
		for n, k := range s.pending[scope] {
			if k == key {
				return n, nil
			}
		}
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
		if taken[n] || anyBusy(busy, n) {
			continue
		}
		if s.preview {
			if _, promised := s.pending[scope][n]; promised {
				continue
			}
			if s.pending == nil {
				s.pending = map[string]map[int]string{}
			}
			if s.pending[scope] == nil {
				s.pending[scope] = map[int]string{}
			}
			s.pending[scope][n] = key
			return n, nil
		}
		if _, err := s.db.Exec(`INSERT INTO allocations (scope, number, project_id, name, created_at)
			VALUES (?, ?, ?, ?, ?)`, scope, n, projectID, name, now()); err != nil {
			return 0, err
		}
		s.fresh = append(s.fresh, freshAllocation{added: Allocation{Scope: scope, Number: n, Project: projectID, Name: name}})
		return n, nil
	}
	return 0, fmt.Errorf("plage %s saturée (%d à %d) : aucun numéro disponible", scope, min, max)
}

// PreviewAllocations fait d'Allocate, de Reserve et de Release des calculs sans
// écriture, jusqu'à nouvel ordre : les commandes qui calculent un plan sans
// l'appliquer — plan, diff, status, apply --dry-run, l'affichage de la TUI — ne
// réservent rien (D100). Le quitter oublie les numéros promis en aperçu.
func (s *Store) PreviewAllocations(on bool) {
	if s == nil {
		return
	}
	s.preview = on
	if !on {
		s.pending = nil
	}
}

// AllocationMark repère les numéros inscrits jusqu'ici par ce processus.
func (s *Store) AllocationMark() int {
	if s == nil {
		return 0
	}
	return len(s.fresh)
}

// CancelAllocations retire les numéros inscrits depuis mark : ceux d'un plan
// refusé à la confirmation, ou déroulé. Le pool ou l'unité qui les auraient
// servis n'ont pas été écrits — ou ont été retirés par la compensation.
func (s *Store) CancelAllocations(mark int) error {
	if s == nil || mark < 0 || mark > len(s.fresh) {
		return nil
	}
	for i := len(s.fresh) - 1; i >= mark; i-- {
		a, prev := s.fresh[i].added, s.fresh[i].replaced
		if _, err := s.db.Exec(`DELETE FROM allocations WHERE scope = ? AND number = ? AND project_id = ? AND name = ?`,
			a.Scope, a.Number, a.Project, a.Name); err != nil {
			return err
		}
		if prev != nil {
			if _, err := s.Claim(prev.Scope, prev.Project, prev.Name, prev.Number); err != nil {
				return err
			}
		}
	}
	s.fresh = s.fresh[:mark]
	return nil
}

// Reserve inscrit pour une ressource le numéro que sa déclaration impose
// (« number: 18542 »), à la place de celui qu'elle tenait (D100). Sans quoi
// l'attribution automatique d'un autre pool pouvait le prendre. Un numéro tenu
// par une autre ressource n'est jamais repris : holder la nomme, et rien ne
// change — un pool en socket Unix porte son nom dans celui du socket, deux
// numéros égaux n'y collisionnent pas, et le refuser bloquerait ce qui marche.
// En aperçu, rien n'est écrit.
func (s *Store) Reserve(scope, projectID, name string, number int) (holder *Allocation, err error) {
	var h Allocation
	err = s.db.QueryRow(`SELECT scope, number, project_id, name FROM allocations WHERE scope = ? AND number = ?`,
		scope, number).Scan(&h.Scope, &h.Number, &h.Project, &h.Name)
	switch {
	case err == nil && h.Project == projectID && h.Name == name:
		return nil, nil
	case err == nil:
		return &h, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	if s.preview {
		return nil, nil
	}
	var replaced *Allocation
	var previous int
	switch err := s.db.QueryRow(`SELECT number FROM allocations WHERE scope = ? AND project_id = ? AND name = ?`,
		scope, projectID, name).Scan(&previous); {
	case err == nil:
		replaced = &Allocation{Scope: scope, Number: previous, Project: projectID, Name: name}
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM allocations WHERE scope = ? AND project_id = ? AND name = ?`,
		scope, projectID, name); err != nil {
		tx.Rollback()
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO allocations (scope, number, project_id, name, created_at)
		VALUES (?, ?, ?, ?, ?)`, scope, number, projectID, name, now()); err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.fresh = append(s.fresh, freshAllocation{
		added:    Allocation{Scope: scope, Number: number, Project: projectID, Name: name},
		replaced: replaced,
	})
	return nil, nil
}

func anyBusy(busy []func(int) bool, n int) bool {
	for _, f := range busy {
		if f != nil && f(n) {
			return true
		}
	}
	return false
}

// Claim inscrit pour une ressource un numéro que le disque atteste — l'unité ou
// le pool qu'ogs a écrits —, s'il est libre et que la ressource n'en a pas
// encore (D94). Il rend vrai s'il l'a inscrit.
//
// La base d'état n'est pas la seule à savoir quel numéro sert quel projet : le
// vhost d'un voisin relaie déjà vers le port écrit dans son unité. Une base
// perdue ou recréée redistribuait les numéros dans l'ordre alphabétique des
// projets, et le premier servi prenait le port en service chez un autre — dont
// le vhost relayait alors vers l'application d'un autre client. Le numéro déjà
// attribué l'emporte sur le disque, et un numéro tenu par une autre ressource
// n'est jamais repris : la base, quand elle sait, fait foi.
func (s *Store) Claim(scope, projectID, name string, number int) (bool, error) {
	var existing int
	err := s.db.QueryRow(`SELECT number FROM allocations WHERE scope = ? AND project_id = ? AND name = ?`,
		scope, projectID, name).Scan(&existing)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	res, err := s.db.Exec(`INSERT INTO allocations (scope, number, project_id, name, created_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(scope, number) DO NOTHING`,
		scope, number, projectID, name, now())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Release libère le numéro d'une ressource. L'appelant s'assure d'abord que
// plus rien ne le sert — le pool ou l'unité retirés du disque (D100) : sans
// quoi l'attribution suivante le rendrait à un autre projet, dont le vhost
// relaierait vers l'application d'un voisin. En aperçu, rien n'est écrit.
func (s *Store) Release(scope, projectID, name string) error {
	if s.preview {
		return nil
	}
	_, err := s.db.Exec(`DELETE FROM allocations WHERE scope = ? AND project_id = ? AND name = ?`,
		scope, projectID, name)
	return err
}

// ProjectAllocations liste les numéros attribués aux ressources d'un projet.
func (s *Store) ProjectAllocations(projectID string) ([]Allocation, error) {
	all, err := s.Allocations("")
	if err != nil {
		return nil, err
	}
	var out []Allocation
	for _, a := range all {
		if a.Project == projectID {
			out = append(out, a)
		}
	}
	return out, nil
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

// DeleteSecret retire un secret. Il ne sert qu'aux secrets qu'ogs sait n'être
// plus utilisés nulle part — une clé DKIM remplacée dont la rétention a expiré :
// les mots de passe, eux, suivent leur ressource dans l'archive d'un projet.
func (s *Store) DeleteSecret(scope, name string) error {
	_, err := s.db.Exec(`DELETE FROM secrets WHERE scope = ? AND name = ?`, scope, name)
	return err
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

// ACMEFailureThreshold est le nombre d'échecs consécutifs au-delà duquel les
// tentatives sont suspendues. Les autorités de certification limitent les échecs
// de validation : insister ferait perdre le droit d'émettre pour le domaine.
const ACMEFailureThreshold = 5

// ACMEPauseDuration est la durée de la suspension.
const ACMEPauseDuration = time.Hour

// RecordACMEFailure enregistre un échec d'émission et renvoie le compteur ainsi
// que la date de fin de suspension si le seuil est atteint.
func (s *Store) RecordACMEFailure(key, detail string) (count int, until time.Time, err error) {
	if err := s.db.QueryRow(`SELECT count FROM acme_failures WHERE key = ?`, key).Scan(&count); err != nil {
		count = 0
	}
	count++
	if count >= ACMEFailureThreshold {
		until = time.Now().Add(ACMEPauseDuration)
	}
	var untilValue any
	if !until.IsZero() {
		untilValue = until.UTC().Format(time.RFC3339Nano)
	}
	_, err = s.db.Exec(`
		INSERT INTO acme_failures (key, count, last_at, until, detail) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET count = excluded.count, last_at = excluded.last_at,
			until = excluded.until, detail = excluded.detail`,
		key, count, now(), untilValue, detail)
	return count, until, err
}

// ClearACMEFailures remet le compteur à zéro après un succès.
func (s *Store) ClearACMEFailures(key string) error {
	_, err := s.db.Exec(`DELETE FROM acme_failures WHERE key = ?`, key)
	return err
}

// ACMEPause indique si les tentatives sont suspendues pour cette cible.
func (s *Store) ACMEPause(key string) (until time.Time, paused bool, err error) {
	var raw sql.NullString
	err = s.db.QueryRow(`SELECT until FROM acme_failures WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil || !raw.Valid {
		return time.Time{}, false, err
	}
	until, err = time.Parse(time.RFC3339Nano, raw.String)
	if err != nil {
		return time.Time{}, false, nil
	}
	return until, time.Now().Before(until), nil
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
