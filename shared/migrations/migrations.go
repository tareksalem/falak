// Package migrations provides forward-only SQLite schema migration tracking
// for Falak modules that own a local database. It replaces the prior
// `CREATE TABLE IF NOT EXISTS` + idempotent `ALTER TABLE` pattern with an
// ordered list of versioned migrations recorded in a per-database
// `schema_migrations` table keyed by module.
//
// Scope as of task 11A.15a: only `capsule/store.go` uses the runner.
// `snapshot/store.go` is left as-is (no pending schema change). The
// `service/store.go` landing in task 11B will use the runner from day one.
//
// Guarantees: forward-only (out-of-order, duplicate, zero, or
// below-watermark versions are rejected at Run time before any DDL
// executes); each migration's Apply executes in its own transaction with
// the `schema_migrations` insert (atomic rollback on failure); the
// bootstrap `CREATE IF NOT EXISTS schema_migrations` is the only sanctioned
// use of that pattern going forward; concurrent Run calls on the same
// *sql.DB serialize through a per-database lock, the loser exits cleanly.
package migrations

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
)

// dbLocks serializes concurrent Run calls against the same *sql.DB
// in-process so the read-watermark / apply / record cycle is atomic.
// Cross-process callers still serialize via SQLite's file lock.
var (
	dbLocksMu sync.Mutex
	dbLocks   = make(map[*sql.DB]*sync.Mutex)
)

func lockFor(db *sql.DB) *sync.Mutex {
	dbLocksMu.Lock()
	defer dbLocksMu.Unlock()
	if m, ok := dbLocks[db]; ok {
		return m
	}
	m := &sync.Mutex{}
	dbLocks[db] = m
	return m
}

// Migration is a single forward-only schema change owned by one module.
// Version must be a positive integer strictly greater than every prior
// migration in the slice. Apply runs the DDL/DML inside an open
// transaction; the runner commits on nil, rolls back otherwise.
type Migration struct {
	Version     int
	Description string
	Apply       func(tx *sql.Tx) error
}

// BaselineDetector inspects a database that predates the runner and
// reports the highest schema version already applied out-of-band. Runs
// only when `schema_migrations` has no rows for this module. N > 0 stamps
// versions 1..N as applied without invoking their Apply funcs; 0 means
// "fresh database, run all migrations"; a non-nil error aborts Run.
type BaselineDetector func(db *sql.DB) (int, error)

// Runner applies a forward-only Migration list to a SQLite database scoped
// by module name so multiple modules share one `schema_migrations` table
// without colliding on version numbers.
type Runner struct {
	module     string
	migrations []Migration
	logger     *zap.Logger
	baseline   BaselineDetector
}

// RunnerOption configures a Runner returned by NewRunner.
type RunnerOption func(*Runner)

// WithMigrations supplies the ordered list of migrations for this module.
func WithMigrations(ms ...Migration) RunnerOption {
	return func(r *Runner) { r.migrations = append(r.migrations[:0], ms...) }
}

// WithLogger replaces the default no-op logger.
func WithLogger(logger *zap.Logger) RunnerOption {
	return func(r *Runner) {
		if logger != nil {
			r.logger = logger
		}
	}
}

// WithBaselineDetector lets the runner adopt a pre-existing schema without re-running DDL.
func WithBaselineDetector(detector BaselineDetector) RunnerOption {
	return func(r *Runner) { r.baseline = detector }
}

// NewRunner constructs a Runner for the given module. Module names MUST be
// stable across releases since they key rows in `schema_migrations`.
func NewRunner(module string, opts ...RunnerOption) *Runner {
	r := &Runner{module: module, logger: zap.NewNop()}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run applies every registered migration with Version > the current
// watermark. Safe to call repeatedly (no-op when caught up) and
// concurrently from multiple goroutines on the same database.
func (r *Runner) Run(db *sql.DB) error {
	if db == nil {
		return errors.New("migrations: nil database")
	}
	if r.module == "" {
		return errors.New("migrations: empty module name")
	}
	if err := r.validateSchedule(); err != nil {
		return err
	}
	lock := lockFor(db)
	lock.Lock()
	defer lock.Unlock()
	if err := bootstrap(db); err != nil {
		return fmt.Errorf("migrations[%s]: bootstrap: %w", r.module, err)
	}
	current, err := r.currentVersion(db)
	if err != nil {
		return fmt.Errorf("migrations[%s]: read current version: %w", r.module, err)
	}
	// Baseline-import: a pre-runner database may carry the schema without
	// any schema_migrations rows. Stamp them.
	if current == 0 && r.baseline != nil {
		baseline, err := r.baseline(db)
		if err != nil {
			return fmt.Errorf("migrations[%s]: baseline detect: %w", r.module, err)
		}
		if baseline > 0 {
			if err := r.stampBaseline(db, baseline); err != nil {
				return fmt.Errorf("migrations[%s]: stamp baseline: %w", r.module, err)
			}
			r.logger.Info("schema baseline stamped",
				zap.String("module", r.module), zap.Int("version", baseline))
			current = baseline
		}
	}
	for _, m := range r.migrations {
		if m.Version <= current {
			continue
		}
		if err := r.applyOne(db, m); err != nil {
			return fmt.Errorf("migrations[%s] v%d (%s): %w", r.module, m.Version, m.Description, err)
		}
		r.logger.Info("schema migrated",
			zap.String("module", r.module),
			zap.Int("version", m.Version),
			zap.String("description", m.Description))
		current = m.Version
	}
	return nil
}

// validateSchedule enforces forward-only invariants on the registered slice
// before any DDL is run. A malformed schedule never partially applies.
func (r *Runner) validateSchedule() error {
	if len(r.migrations) == 0 {
		return nil
	}
	if !sort.SliceIsSorted(r.migrations, func(i, j int) bool {
		return r.migrations[i].Version < r.migrations[j].Version
	}) {
		return fmt.Errorf("migrations[%s]: schedule not strictly ascending", r.module)
	}
	seen := make(map[int]struct{}, len(r.migrations))
	for _, m := range r.migrations {
		if m.Version <= 0 {
			return fmt.Errorf("migrations[%s]: version must be > 0 (got %d)", r.module, m.Version)
		}
		if _, dup := seen[m.Version]; dup {
			return fmt.Errorf("migrations[%s]: duplicate version %d", r.module, m.Version)
		}
		if m.Apply == nil {
			return fmt.Errorf("migrations[%s] v%d: Apply is nil", r.module, m.Version)
		}
		seen[m.Version] = struct{}{}
	}
	return nil
}

// bootstrap creates the schema_migrations table. The only sanctioned use
// of `CREATE IF NOT EXISTS` in the codebase going forward.
func bootstrap(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			module      TEXT NOT NULL,
			version     INTEGER NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			applied_at  INTEGER NOT NULL,
			PRIMARY KEY (module, version)
		)
	`)
	return err
}

// currentVersion returns the highest applied version for this module (0
// when no rows exist) and rejects schedules that include a version at or
// below the watermark for which no recorded row exists — a downgrade.
func (r *Runner) currentVersion(db *sql.DB) (int, error) {
	var v sql.NullInt64
	row := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations WHERE module = ?`, r.module)
	if err := row.Scan(&v); err != nil {
		return 0, err
	}
	current := int(v.Int64)
	if current == 0 {
		return 0, nil
	}
	for _, m := range r.migrations {
		if m.Version > current {
			continue
		}
		var n int
		row := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE module = ? AND version = ?`, r.module, m.Version)
		if err := row.Scan(&n); err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, fmt.Errorf("migrations[%s]: version %d in schedule below watermark %d with no recorded row (downgrade)",
				r.module, m.Version, current)
		}
	}
	return current, nil
}

const insertVersionSQL = `INSERT INTO schema_migrations (module, version, description, applied_at) VALUES (?, ?, ?, ?)`

// applyOne runs a single migration inside its own transaction.
func (r *Runner) applyOne(db *sql.DB, m Migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := m.Apply(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("apply: %w", err)
	}
	if _, err := tx.Exec(insertVersionSQL, r.module, m.Version, m.Description, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// stampBaseline records versions 1..baseline as applied without executing
// their Apply funcs. Runs in a single transaction so partial stamping is
// impossible.
func (r *Runner) stampBaseline(db *sql.DB, baseline int) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	now := time.Now().UnixMilli()
	for _, m := range r.migrations {
		if m.Version > baseline {
			break
		}
		if _, err := tx.Exec(insertVersionSQL, r.module, m.Version, m.Description, now); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("stamp v%d: %w", m.Version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
