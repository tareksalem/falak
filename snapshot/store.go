// Package snapshot manages per-node checkpoint storage for fast capsule
// startup. Snapshots are CRIU checkpoint archives produced by the
// runtime after a container's first cold start, stored on disk under
// ~/.local/share/falak/<node>/snapshots/<capsule_id>/<tag>/, and
// tracked in a local SQLite table for metadata, eviction, and lookup.
package snapshot

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Record is a single row in the snapshot metadata table.
type Record struct {
	CapsuleID    string
	Tag          string
	Size         int64  // bytes
	Path         string // absolute path on disk
	Checksum     string // SHA-256 hex
	CreatedAt    time.Time
	LastAccessed time.Time
	TTL          time.Duration
	InUse        bool // true while a container is running from this snapshot
}

// Store manages snapshot metadata in SQLite and coordinates eviction.
// Safe for concurrent use.
type Store struct {
	db  *sql.DB
	mu  sync.Mutex
	dir string // base directory for snapshots on disk
}

// StoreOption configures a Store.
type StoreOption func(*Store)

// WithBaseDir sets the root directory for snapshot storage on disk.
// Defaults to the caller-provided path in New.
func WithBaseDir(dir string) StoreOption {
	return func(s *Store) { s.dir = dir }
}

// New opens or creates a snapshot store at the given SQLite database path.
// The base directory is where snapshot files are stored on disk.
func New(dbPath, baseDir string, opts ...StoreOption) (*Store, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("snapshot store: open db: %w", err)
	}

	s := &Store{db: db, dir: baseDir}
	for _, opt := range opts {
		opt(s)
	}

	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("snapshot store: migrate: %w", err)
	}

	return s, nil
}

// migrate creates the snapshots table if it doesn't exist.
func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS snapshots (
			capsule_id    TEXT NOT NULL,
			tag           TEXT NOT NULL,
			size          INTEGER NOT NULL DEFAULT 0,
			path          TEXT NOT NULL,
			checksum      TEXT NOT NULL DEFAULT '',
			created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			last_accessed DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			ttl_seconds   INTEGER NOT NULL DEFAULT 259200,
			in_use        INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (capsule_id, tag)
		);
		CREATE INDEX IF NOT EXISTS idx_snapshots_capsule ON snapshots(capsule_id);
		CREATE INDEX IF NOT EXISTS idx_snapshots_accessed ON snapshots(last_accessed);
	`)
	return err
}

// Put inserts or replaces a snapshot record.
func (s *Store) Put(rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO snapshots
			(capsule_id, tag, size, path, checksum, created_at, last_accessed, ttl_seconds, in_use)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.CapsuleID, rec.Tag, rec.Size, rec.Path, rec.Checksum,
		rec.CreatedAt, rec.LastAccessed, int64(rec.TTL.Seconds()),
		boolToInt(rec.InUse))
	return err
}

// Get returns the record for a (capsule_id, tag) pair, or nil if not found.
func (s *Store) Get(capsuleID, tag string) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	row := s.db.QueryRow(`
		SELECT capsule_id, tag, size, path, checksum, created_at, last_accessed, ttl_seconds, in_use
		FROM snapshots WHERE capsule_id = ? AND tag = ?`, capsuleID, tag)

	rec, err := scanRecord(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// ListByCapsule returns all snapshot records for a capsule, ordered by
// creation time (newest first).
func (s *Store) ListByCapsule(capsuleID string) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`
		SELECT capsule_id, tag, size, path, checksum, created_at, last_accessed, ttl_seconds, in_use
		FROM snapshots WHERE capsule_id = ?
		ORDER BY created_at DESC`, capsuleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanRecords(rows)
}

// TouchAccess updates the last_accessed timestamp for a snapshot.
// Called when a container is restored from the snapshot.
func (s *Store) TouchAccess(capsuleID, tag string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		UPDATE snapshots SET last_accessed = ? WHERE capsule_id = ? AND tag = ?`,
		time.Now(), capsuleID, tag)
	return err
}

// SetInUse marks a snapshot as in-use (or not). In-use snapshots are
// protected from eviction.
func (s *Store) SetInUse(capsuleID, tag string, inUse bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		UPDATE snapshots SET in_use = ? WHERE capsule_id = ? AND tag = ?`,
		boolToInt(inUse), capsuleID, tag)
	return err
}

// Delete removes a snapshot record from the database. Does NOT delete
// the on-disk files — the caller is responsible for that.
func (s *Store) Delete(capsuleID, tag string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		DELETE FROM snapshots WHERE capsule_id = ? AND tag = ?`, capsuleID, tag)
	return err
}

// DeleteByCapsule removes all snapshot records for a capsule.
func (s *Store) DeleteByCapsule(capsuleID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`DELETE FROM snapshots WHERE capsule_id = ?`, capsuleID)
	return err
}

// EvictExpired removes snapshots whose TTL has expired and that are not
// currently in use. Returns the list of records that were evicted so the
// caller can delete the on-disk files.
func (s *Store) EvictExpired() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	rows, err := s.db.Query(`
		SELECT capsule_id, tag, size, path, checksum, created_at, last_accessed, ttl_seconds, in_use
		FROM snapshots
		WHERE in_use = 0
		  AND (julianday(?) - julianday(last_accessed)) * 86400 > ttl_seconds`,
		now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	evicted, err := scanRecords(rows)
	if err != nil {
		return nil, err
	}

	for _, rec := range evicted {
		s.db.Exec(`DELETE FROM snapshots WHERE capsule_id = ? AND tag = ?`,
			rec.CapsuleID, rec.Tag)
	}
	return evicted, nil
}

// EvictOverCap removes the oldest snapshots for a capsule that exceed
// the per-capsule cap. In-use snapshots are protected. Returns evicted
// records.
func (s *Store) EvictOverCap(capsuleID string, maxPerCapsule int) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`
		SELECT capsule_id, tag, size, path, checksum, created_at, last_accessed, ttl_seconds, in_use
		FROM snapshots WHERE capsule_id = ?
		ORDER BY created_at DESC`, capsuleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	all, err := scanRecords(rows)
	if err != nil {
		return nil, err
	}

	// Keep the newest maxPerCapsule, evict the rest (if not in use).
	var evicted []Record
	kept := 0
	for _, rec := range all {
		if kept < maxPerCapsule || rec.InUse {
			if !rec.InUse || kept < maxPerCapsule {
				kept++
				continue
			}
		}
		s.db.Exec(`DELETE FROM snapshots WHERE capsule_id = ? AND tag = ?`,
			rec.CapsuleID, rec.Tag)
		evicted = append(evicted, rec)
	}
	return evicted, nil
}

// SnapshotPath returns the on-disk path for a snapshot.
func (s *Store) SnapshotPath(capsuleID, tag string) string {
	return filepath.Join(s.dir, capsuleID, tag)
}

// EnsureDir creates the on-disk directory for a snapshot if it doesn't exist.
func (s *Store) EnsureDir(capsuleID, tag string) error {
	return os.MkdirAll(s.SnapshotPath(capsuleID, tag), 0700)
}

// RemoveDir removes the on-disk directory for a snapshot.
func (s *Store) RemoveDir(capsuleID, tag string) error {
	return os.RemoveAll(s.SnapshotPath(capsuleID, tag))
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// --- helpers -------------------------------------------------------------

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

type scannable interface {
	Scan(dest ...any) error
}

func scanRecord(row scannable) (*Record, error) {
	var rec Record
	var ttlSec int64
	var inUse int
	err := row.Scan(
		&rec.CapsuleID, &rec.Tag, &rec.Size, &rec.Path, &rec.Checksum,
		&rec.CreatedAt, &rec.LastAccessed, &ttlSec, &inUse)
	if err != nil {
		return nil, err
	}
	rec.TTL = time.Duration(ttlSec) * time.Second
	rec.InUse = inUse != 0
	return &rec, nil
}

func scanRecords(rows *sql.Rows) ([]Record, error) {
	var out []Record
	for rows.Next() {
		var rec Record
		var ttlSec int64
		var inUse int
		if err := rows.Scan(
			&rec.CapsuleID, &rec.Tag, &rec.Size, &rec.Path, &rec.Checksum,
			&rec.CreatedAt, &rec.LastAccessed, &ttlSec, &inUse); err != nil {
			return nil, err
		}
		rec.TTL = time.Duration(ttlSec) * time.Second
		rec.InUse = inUse != 0
		out = append(out, rec)
	}
	return out, rows.Err()
}
