package metrics

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
)

// HistoryDepth is how many local snapshots are retained in the rolling
// window. Older entries are deleted on every insert. Ten snapshots at the
// default 1s interval gives operators a 10-second history — enough to see
// trends in `falak node metrics history` later without bloating the DB.
const HistoryDepth = 10

// Store is the persistence layer for metrics. It owns a SQLite database
// with two tables:
//
//   - local_history: rolling window of the last HistoryDepth snapshots
//     for the local node, used for trend analysis and observability.
//
//   - peer_latest: exactly one row per peer node holding the most recent
//     ResourceUpdate received via gossip. Older rows are upserted in
//     place; we never keep history for peers.
//
// The store wraps every write in the database itself (no in-memory
// caching) so a single SQLite connection serializes access. The store
// is safe for concurrent use.
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

// OpenStore opens (or creates) the metrics database at the given path.
// Migrations run on every open and are idempotent. Returns an error
// when the database cannot be opened or the schema fails to migrate.
func OpenStore(path string, opts ...StoreOption) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("metrics store: open %s: %w", path, err)
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
		return nil, fmt.Errorf("metrics store: migrate: %w", err)
	}

	s.logger.Debug("metrics store opened", zap.String("path", path))
	return s, nil
}

// migrate creates the schema if it doesn't already exist. Both tables
// have explicit field types so future schema changes can use ALTER TABLE.
func (s *Store) migrate() error {
	const schema = `
	CREATE TABLE IF NOT EXISTS local_history (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		captured_at     INTEGER NOT NULL,
		cpu_cores       INTEGER NOT NULL,
		cpu_used_pct    REAL NOT NULL,
		mem_total_mb    INTEGER NOT NULL,
		mem_avail_mb    INTEGER NOT NULL,
		mem_used_mb     INTEGER NOT NULL,
		mem_used_pct    REAL NOT NULL,
		disk_total_mb   INTEGER NOT NULL,
		disk_free_mb    INTEGER NOT NULL,
		disk_used_mb    INTEGER NOT NULL,
		disk_used_pct   REAL NOT NULL,
		load_one        REAL NOT NULL,
		load_five       REAL NOT NULL,
		load_fifteen    REAL NOT NULL,
		net_sent_bps    INTEGER NOT NULL,
		net_recv_bps    INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_local_history_captured_at ON local_history(captured_at DESC);

	CREATE TABLE IF NOT EXISTS peer_latest (
		node_id         TEXT PRIMARY KEY,
		captured_at     INTEGER NOT NULL,
		cpu_cores       INTEGER NOT NULL,
		cpu_used_pct    REAL NOT NULL,
		mem_total_mb    INTEGER NOT NULL,
		mem_avail_mb    INTEGER NOT NULL,
		mem_used_mb     INTEGER NOT NULL,
		mem_used_pct    REAL NOT NULL,
		disk_total_mb   INTEGER NOT NULL,
		disk_free_mb    INTEGER NOT NULL,
		disk_used_mb    INTEGER NOT NULL,
		disk_used_pct   REAL NOT NULL,
		load_one        REAL NOT NULL,
		load_five       REAL NOT NULL,
		load_fifteen    REAL NOT NULL,
		net_sent_bps    INTEGER NOT NULL,
		net_recv_bps    INTEGER NOT NULL
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

// InsertLocal stores a new local snapshot in the rolling window. After
// insertion, the oldest rows beyond HistoryDepth are deleted in the same
// transaction so the table never exceeds the cap.
//
// Returns an error when the insert or trim fails. Callers should log
// such errors but not stop sampling — losing a single sample is recoverable.
func (s *Store) InsertLocal(snap Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		INSERT INTO local_history (
			captured_at, cpu_cores, cpu_used_pct,
			mem_total_mb, mem_avail_mb, mem_used_mb, mem_used_pct,
			disk_total_mb, disk_free_mb, disk_used_mb, disk_used_pct,
			load_one, load_five, load_fifteen,
			net_sent_bps, net_recv_bps
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		snap.CapturedAt.UnixMilli(),
		snap.CPU.Cores, snap.CPU.UsedPercent,
		snap.Memory.TotalMB, snap.Memory.AvailableMB, snap.Memory.UsedMB, snap.Memory.UsedPercent,
		snap.Disk.TotalMB, snap.Disk.FreeMB, snap.Disk.UsedMB, snap.Disk.UsedPercent,
		snap.Load.One, snap.Load.Five, snap.Load.Fifteen,
		snap.Network.BytesSentPerSec, snap.Network.BytesRecvPerSec,
	); err != nil {
		return fmt.Errorf("insert local snapshot: %w", err)
	}

	// Trim the table to HistoryDepth rows. We delete rows whose id is
	// not in the most recent HistoryDepth ids — equivalent to "keep the
	// newest 10".
	if _, err := tx.Exec(`
		DELETE FROM local_history
		WHERE id NOT IN (
			SELECT id FROM local_history ORDER BY id DESC LIMIT ?
		)
	`, HistoryDepth); err != nil {
		return fmt.Errorf("trim local history: %w", err)
	}

	return tx.Commit()
}

// LocalLatest returns the most recent local snapshot, or zero (and nil
// error) when the history is empty. Callers can detect "no data yet"
// by checking that CapturedAt is the zero value.
func (s *Store) LocalLatest() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	row := s.db.QueryRow(`
		SELECT captured_at, cpu_cores, cpu_used_pct,
		       mem_total_mb, mem_avail_mb, mem_used_mb, mem_used_pct,
		       disk_total_mb, disk_free_mb, disk_used_mb, disk_used_pct,
		       load_one, load_five, load_fifteen,
		       net_sent_bps, net_recv_bps
		FROM local_history
		ORDER BY id DESC
		LIMIT 1
	`)
	snap, err := scanRow(row)
	if err == sql.ErrNoRows {
		return Snapshot{}, nil
	}
	return snap, err
}

// LocalHistory returns the rolling window of local snapshots in
// chronological order (oldest first), capped at HistoryDepth entries.
// Callers iterate forward to read trends.
func (s *Store) LocalHistory() ([]Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`
		SELECT captured_at, cpu_cores, cpu_used_pct,
		       mem_total_mb, mem_avail_mb, mem_used_mb, mem_used_pct,
		       disk_total_mb, disk_free_mb, disk_used_mb, disk_used_pct,
		       load_one, load_five, load_fifteen,
		       net_sent_bps, net_recv_bps
		FROM local_history
		ORDER BY captured_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query local history: %w", err)
	}
	defer rows.Close()

	var out []Snapshot
	for rows.Next() {
		snap, err := scanRowsOnce(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

// UpsertPeer replaces (or inserts) the latest snapshot for a peer node.
// The node ID is taken from the snapshot itself; callers must populate
// snap.NodeID before calling. Returns an error on a database failure.
func (s *Store) UpsertPeer(snap Snapshot) error {
	if snap.NodeID == "" {
		return fmt.Errorf("metrics store: peer snapshot missing NodeID")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO peer_latest (
			node_id, captured_at, cpu_cores, cpu_used_pct,
			mem_total_mb, mem_avail_mb, mem_used_mb, mem_used_pct,
			disk_total_mb, disk_free_mb, disk_used_mb, disk_used_pct,
			load_one, load_five, load_fifteen,
			net_sent_bps, net_recv_bps
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id) DO UPDATE SET
			captured_at   = excluded.captured_at,
			cpu_cores     = excluded.cpu_cores,
			cpu_used_pct  = excluded.cpu_used_pct,
			mem_total_mb  = excluded.mem_total_mb,
			mem_avail_mb  = excluded.mem_avail_mb,
			mem_used_mb   = excluded.mem_used_mb,
			mem_used_pct  = excluded.mem_used_pct,
			disk_total_mb = excluded.disk_total_mb,
			disk_free_mb  = excluded.disk_free_mb,
			disk_used_mb  = excluded.disk_used_mb,
			disk_used_pct = excluded.disk_used_pct,
			load_one      = excluded.load_one,
			load_five     = excluded.load_five,
			load_fifteen  = excluded.load_fifteen,
			net_sent_bps  = excluded.net_sent_bps,
			net_recv_bps  = excluded.net_recv_bps
	`,
		snap.NodeID, snap.CapturedAt.UnixMilli(),
		snap.CPU.Cores, snap.CPU.UsedPercent,
		snap.Memory.TotalMB, snap.Memory.AvailableMB, snap.Memory.UsedMB, snap.Memory.UsedPercent,
		snap.Disk.TotalMB, snap.Disk.FreeMB, snap.Disk.UsedMB, snap.Disk.UsedPercent,
		snap.Load.One, snap.Load.Five, snap.Load.Fifteen,
		snap.Network.BytesSentPerSec, snap.Network.BytesRecvPerSec,
	)
	if err != nil {
		return fmt.Errorf("upsert peer %s: %w", snap.NodeID, err)
	}
	return nil
}

// PeerLatest returns the latest snapshot for the named peer, or
// (zero, nil) when no row exists for that peer.
func (s *Store) PeerLatest(nodeID string) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	row := s.db.QueryRow(`
		SELECT captured_at, cpu_cores, cpu_used_pct,
		       mem_total_mb, mem_avail_mb, mem_used_mb, mem_used_pct,
		       disk_total_mb, disk_free_mb, disk_used_mb, disk_used_pct,
		       load_one, load_five, load_fifteen,
		       net_sent_bps, net_recv_bps
		FROM peer_latest
		WHERE node_id = ?
	`, nodeID)
	snap, err := scanRow(row)
	if err == sql.ErrNoRows {
		return Snapshot{}, nil
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("peer latest %s: %w", nodeID, err)
	}
	snap.NodeID = nodeID
	return snap, nil
}

// AllPeers returns the latest snapshot for every peer the store has ever
// received metrics from, in arbitrary order. Used by the StateProvider
// when answering EligibleNodes queries.
func (s *Store) AllPeers() ([]Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`
		SELECT node_id, captured_at, cpu_cores, cpu_used_pct,
		       mem_total_mb, mem_avail_mb, mem_used_mb, mem_used_pct,
		       disk_total_mb, disk_free_mb, disk_used_mb, disk_used_pct,
		       load_one, load_five, load_fifteen,
		       net_sent_bps, net_recv_bps
		FROM peer_latest
	`)
	if err != nil {
		return nil, fmt.Errorf("query peers: %w", err)
	}
	defer rows.Close()

	var out []Snapshot
	for rows.Next() {
		var nodeID string
		var capturedMs int64
		var snap Snapshot
		if err := rows.Scan(
			&nodeID, &capturedMs,
			&snap.CPU.Cores, &snap.CPU.UsedPercent,
			&snap.Memory.TotalMB, &snap.Memory.AvailableMB, &snap.Memory.UsedMB, &snap.Memory.UsedPercent,
			&snap.Disk.TotalMB, &snap.Disk.FreeMB, &snap.Disk.UsedMB, &snap.Disk.UsedPercent,
			&snap.Load.One, &snap.Load.Five, &snap.Load.Fifteen,
			&snap.Network.BytesSentPerSec, &snap.Network.BytesRecvPerSec,
		); err != nil {
			return nil, fmt.Errorf("scan peer row: %w", err)
		}
		snap.NodeID = nodeID
		snap.CapturedAt = time.UnixMilli(capturedMs)
		out = append(out, snap)
	}
	return out, rows.Err()
}

// scanRow decodes a single QueryRow result into a Snapshot. The NodeID
// field is left blank — callers populate it from context (LocalLatest
// returns a snapshot for the local node, PeerLatest knows its own ID).
func scanRow(row *sql.Row) (Snapshot, error) {
	var capturedMs int64
	var snap Snapshot
	err := row.Scan(
		&capturedMs,
		&snap.CPU.Cores, &snap.CPU.UsedPercent,
		&snap.Memory.TotalMB, &snap.Memory.AvailableMB, &snap.Memory.UsedMB, &snap.Memory.UsedPercent,
		&snap.Disk.TotalMB, &snap.Disk.FreeMB, &snap.Disk.UsedMB, &snap.Disk.UsedPercent,
		&snap.Load.One, &snap.Load.Five, &snap.Load.Fifteen,
		&snap.Network.BytesSentPerSec, &snap.Network.BytesRecvPerSec,
	)
	if err != nil {
		return Snapshot{}, err
	}
	snap.CapturedAt = time.UnixMilli(capturedMs)
	return snap, nil
}

// scanRowsOnce decodes the current row of a *sql.Rows iterator into a
// Snapshot. Used by LocalHistory inside the for-rows loop.
func scanRowsOnce(rows *sql.Rows) (Snapshot, error) {
	var capturedMs int64
	var snap Snapshot
	err := rows.Scan(
		&capturedMs,
		&snap.CPU.Cores, &snap.CPU.UsedPercent,
		&snap.Memory.TotalMB, &snap.Memory.AvailableMB, &snap.Memory.UsedMB, &snap.Memory.UsedPercent,
		&snap.Disk.TotalMB, &snap.Disk.FreeMB, &snap.Disk.UsedMB, &snap.Disk.UsedPercent,
		&snap.Load.One, &snap.Load.Five, &snap.Load.Fifteen,
		&snap.Network.BytesSentPerSec, &snap.Network.BytesRecvPerSec,
	)
	if err != nil {
		return Snapshot{}, fmt.Errorf("scan local row: %w", err)
	}
	snap.CapturedAt = time.UnixMilli(capturedMs)
	return snap, nil
}
