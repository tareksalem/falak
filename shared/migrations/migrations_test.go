package migrations

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// openDB returns a fresh sqlite database at a tmp path. MaxOpenConns is
// pinned to 1 so the concurrency test serializes through the connection
// pool — SQLite is single-writer, and production callers (capsule store)
// rely on the same shape.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "test.db")+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// countApplied returns how many rows schema_migrations holds for the module.
func countApplied(t *testing.T, db *sql.DB, module string) int {
	t.Helper()
	var n int
	row := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE module = ?`, module)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count applied: %v", err)
	}
	return n
}

// tableExists is a small helper for assertions on bootstrap.
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var got string
	row := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`,
		name,
	)
	switch err := row.Scan(&got); {
	case errors.Is(err, sql.ErrNoRows):
		return false
	case err != nil:
		t.Fatalf("table exists: %v", err)
	}
	return got == name
}

// migration helpers used by multiple tests.
func createTableMigration(version int, table, description string) Migration {
	return Migration{
		Version:     version,
		Description: description,
		Apply: func(tx *sql.Tx) error {
			_, err := tx.Exec(fmt.Sprintf(`CREATE TABLE %s (id INTEGER PRIMARY KEY)`, table))
			return err
		},
	}
}

func TestRunner_BootstrapCreatesTable(t *testing.T) {
	db := openDB(t)
	r := NewRunner("alpha")
	if err := r.Run(db); err != nil {
		t.Fatalf("Run with empty schedule: %v", err)
	}
	if !tableExists(t, db, "schema_migrations") {
		t.Fatal("schema_migrations table not created by bootstrap")
	}
	if countApplied(t, db, "alpha") != 0 {
		t.Error("empty schedule should leave 0 rows")
	}
}

func TestRunner_AppliesInOrder(t *testing.T) {
	db := openDB(t)
	r := NewRunner("alpha", WithMigrations(
		createTableMigration(1, "t1", "create t1"),
		createTableMigration(2, "t2", "create t2"),
	))
	if err := r.Run(db); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !tableExists(t, db, "t1") || !tableExists(t, db, "t2") {
		t.Fatal("expected t1 and t2 to exist")
	}
	if got := countApplied(t, db, "alpha"); got != 2 {
		t.Errorf("applied rows: got %d, want 2", got)
	}
}

func TestRunner_RerunIsNoop(t *testing.T) {
	db := openDB(t)
	r := NewRunner("alpha", WithMigrations(
		createTableMigration(1, "t1", "create t1"),
		createTableMigration(2, "t2", "create t2"),
	))
	if err := r.Run(db); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	// Second run must not error and must not double-insert rows.
	if err := r.Run(db); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if got := countApplied(t, db, "alpha"); got != 2 {
		t.Errorf("applied rows after rerun: got %d, want 2", got)
	}
}

func TestRunner_OutOfOrderErrors(t *testing.T) {
	db := openDB(t)
	r := NewRunner("alpha", WithMigrations(
		createTableMigration(2, "t2", "create t2"),
		createTableMigration(1, "t1", "create t1"),
	))
	if err := r.Run(db); err == nil {
		t.Fatal("expected out-of-order schedule to error")
	}
	// Validation runs before bootstrap, so the table need not exist — but
	// if it does, no rows should have been written.
	if tableExists(t, db, "schema_migrations") {
		if countApplied(t, db, "alpha") != 0 {
			t.Error("out-of-order failure should not apply any migrations")
		}
	}
}

func TestRunner_DuplicateVersionErrors(t *testing.T) {
	db := openDB(t)
	r := NewRunner("alpha", WithMigrations(
		createTableMigration(1, "t1", "create t1"),
		createTableMigration(1, "t1_dup", "duplicate"),
	))
	if err := r.Run(db); err == nil {
		t.Fatal("expected duplicate version to error")
	}
}

func TestRunner_NonPositiveVersionErrors(t *testing.T) {
	db := openDB(t)
	r := NewRunner("alpha", WithMigrations(
		createTableMigration(0, "t0", "zero"),
	))
	if err := r.Run(db); err == nil {
		t.Fatal("expected version 0 to error")
	}
}

func TestRunner_NilApplyErrors(t *testing.T) {
	db := openDB(t)
	r := NewRunner("alpha", WithMigrations(
		Migration{Version: 1, Description: "nil apply"},
	))
	if err := r.Run(db); err == nil {
		t.Fatal("expected nil Apply to error")
	}
}

func TestRunner_FailingMigrationRollsBack(t *testing.T) {
	db := openDB(t)
	failErr := errors.New("intentional failure")
	r := NewRunner("alpha", WithMigrations(
		createTableMigration(1, "t1", "create t1"),
		Migration{
			Version:     2,
			Description: "fails",
			Apply: func(tx *sql.Tx) error {
				// Side-effect first to prove the rollback works.
				if _, err := tx.Exec(`CREATE TABLE half (id INTEGER)`); err != nil {
					return err
				}
				return failErr
			},
		},
	))
	if err := r.Run(db); err == nil {
		t.Fatal("expected migration failure to surface")
	}
	// V1 should be applied; V2's side-effect rolled back; schema_migrations
	// must reflect only v1.
	if !tableExists(t, db, "t1") {
		t.Error("v1 must remain applied")
	}
	if tableExists(t, db, "half") {
		t.Error("failed v2 must roll back its side-effect")
	}
	if got := countApplied(t, db, "alpha"); got != 1 {
		t.Errorf("applied rows after failure: got %d, want 1", got)
	}
}

func TestRunner_DowngradeAttemptErrors(t *testing.T) {
	db := openDB(t)
	// First, apply v1 and v2 cleanly.
	if err := NewRunner("alpha", WithMigrations(
		createTableMigration(1, "t1", "create t1"),
		createTableMigration(2, "t2", "create t2"),
	)).Run(db); err != nil {
		t.Fatalf("setup Run: %v", err)
	}
	// Now manually delete v1 to simulate a malformed history that
	// claims v2 was applied but v1 is missing — exercises the
	// downgrade detector in currentVersion.
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE module = ? AND version = ?`, "alpha", 1); err != nil {
		t.Fatalf("delete v1: %v", err)
	}
	err := NewRunner("alpha", WithMigrations(
		createTableMigration(1, "t1", "create t1"),
		createTableMigration(2, "t2", "create t2"),
	)).Run(db)
	if err == nil {
		t.Fatal("expected downgrade detection to error")
	}
}

func TestRunner_PerModuleIsolation(t *testing.T) {
	db := openDB(t)
	if err := NewRunner("alpha", WithMigrations(
		createTableMigration(1, "alpha_t", "alpha v1"),
	)).Run(db); err != nil {
		t.Fatalf("alpha Run: %v", err)
	}
	if err := NewRunner("beta", WithMigrations(
		createTableMigration(1, "beta_t", "beta v1"),
		createTableMigration(2, "beta_t2", "beta v2"),
	)).Run(db); err != nil {
		t.Fatalf("beta Run: %v", err)
	}
	if countApplied(t, db, "alpha") != 1 {
		t.Error("alpha should have exactly 1 row")
	}
	if countApplied(t, db, "beta") != 2 {
		t.Error("beta should have exactly 2 rows")
	}
}

func TestRunner_BaselineStamping(t *testing.T) {
	db := openDB(t)
	// Simulate a legacy schema: tables created out-of-band, no rows in
	// schema_migrations. The detector reports baseline=2; the runner
	// must record v1+v2 without re-running their Apply funcs (which
	// would fail since the tables already exist).
	if _, err := db.Exec(`CREATE TABLE t1 (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("seed t1: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE t2 (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("seed t2: %v", err)
	}
	detector := func(_ *sql.DB) (int, error) { return 2, nil }
	r := NewRunner("alpha",
		WithMigrations(
			createTableMigration(1, "t1", "create t1"),
			createTableMigration(2, "t2", "create t2"),
			createTableMigration(3, "t3", "create t3"),
		),
		WithBaselineDetector(detector),
	)
	if err := r.Run(db); err != nil {
		t.Fatalf("Run with baseline: %v", err)
	}
	if got := countApplied(t, db, "alpha"); got != 3 {
		t.Errorf("applied rows after baseline+v3: got %d, want 3", got)
	}
	if !tableExists(t, db, "t3") {
		t.Error("v3 should have been applied")
	}
}

func TestRunner_BaselineDetectorError(t *testing.T) {
	db := openDB(t)
	want := errors.New("boom")
	r := NewRunner("alpha",
		WithMigrations(createTableMigration(1, "t1", "create t1")),
		WithBaselineDetector(func(_ *sql.DB) (int, error) { return 0, want }),
	)
	err := r.Run(db)
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("expected baseline error to wrap %v, got %v", want, err)
	}
}

func TestRunner_NilDatabaseErrors(t *testing.T) {
	if err := NewRunner("alpha").Run(nil); err == nil {
		t.Fatal("expected nil database to error")
	}
}

func TestRunner_EmptyModuleErrors(t *testing.T) {
	db := openDB(t)
	if err := NewRunner("").Run(db); err == nil {
		t.Fatal("expected empty module name to error")
	}
}

func TestRunner_ConcurrentRunsSerialize(t *testing.T) {
	db := openDB(t)
	// Two independent Runner instances calling Run on the same db
	// concurrently must each complete without panic and converge on the
	// same applied state. SQLite's writer lock serializes them.
	build := func() *Runner {
		return NewRunner("alpha", WithMigrations(
			createTableMigration(1, "t1", "create t1"),
			createTableMigration(2, "t2", "create t2"),
		))
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = build().Run(db)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
	// Whichever goroutine won, the final state has exactly one row per
	// version — no duplicates, no panics.
	if got := countApplied(t, db, "alpha"); got != 2 {
		t.Errorf("applied rows after concurrent runs: got %d, want 2", got)
	}
}
