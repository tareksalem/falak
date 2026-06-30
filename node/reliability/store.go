package reliability

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
)

// Store persists the node-global execution-reliability counters (decayed
// success/failure totals and the timestamp they were last updated) in a
// small SQLite database. The counters are keyed node-global — host/runtime
// health is independent of which cluster a node belongs to — so the table
// holds exactly one row per node_id.
//
// The store wraps a single SQLite connection behind a mutex; it is safe for
// concurrent use.
type Store struct {
	mu     sync.Mutex
	db     *sql.DB
	logger *zap.Logger
}

// StoreOption configures a Store.
type StoreOption func(*Store)

// WithStoreLogger sets the logger.
func WithStoreLogger(logger *zap.Logger) StoreOption {
	return func(s *Store) {
		s.logger = logger
	}
}

// OpenStore opens (or creates) the reliability database at the given path.
// Migrations run on every open and are idempotent.
func OpenStore(path string, opts ...StoreOption) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("reliability store: open %s: %w", path, err)
	}
	s := &Store{
		db:     db,
		logger: zap.NewNop(),
	}
	for _, opt := range opts {
		opt(s)
	}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("reliability store: migrate: %w", err)
	}
	s.logger.Debug("reliability store opened", zap.String("path", path))
	return s, nil
}

// migrate creates the schema if it does not already exist.
func (s *Store) migrate() error {
	const schema = `
	CREATE TABLE IF NOT EXISTS execution_reliability (
		node_id      TEXT PRIMARY KEY,
		successes    REAL NOT NULL,
		failures     REAL NOT NULL,
		last_update  INTEGER NOT NULL
	);`
	_, err := s.db.Exec(schema)
	return err
}

// Close closes the underlying database connection. Safe to call once.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// counters is the persisted execution-reliability state for one node.
type counters struct {
	successes  float64
	failures   float64
	lastUpdate time.Time
}

// Load returns the persisted counters for nodeID. found is false (with a
// nil error) when no row exists yet — the caller should start from a clean
// slate in that case.
func (s *Store) Load(nodeID string) (c counters, found bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var lastUpdateMs int64
	row := s.db.QueryRow(`
		SELECT successes, failures, last_update
		FROM execution_reliability
		WHERE node_id = ?
	`, nodeID)
	scanErr := row.Scan(&c.successes, &c.failures, &lastUpdateMs)
	if scanErr == sql.ErrNoRows {
		return counters{}, false, nil
	}
	if scanErr != nil {
		return counters{}, false, fmt.Errorf("reliability store: load %s: %w", nodeID, scanErr)
	}
	c.lastUpdate = time.UnixMilli(lastUpdateMs)
	return c, true, nil
}

// Save upserts the counters for nodeID.
func (s *Store) Save(nodeID string, c counters) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO execution_reliability (node_id, successes, failures, last_update)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(node_id) DO UPDATE SET
			successes   = excluded.successes,
			failures    = excluded.failures,
			last_update = excluded.last_update
	`, nodeID, c.successes, c.failures, c.lastUpdate.UnixMilli())
	if err != nil {
		return fmt.Errorf("reliability store: save %s: %w", nodeID, err)
	}
	return nil
}
