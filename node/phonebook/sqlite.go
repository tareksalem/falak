package phonebook

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Phonebook implements IPhonebook using SQLite.
type Phonebook struct {
	db *sql.DB
}

// Open creates or opens a SQLite phonebook at the given path.
func Open(path string) (*Phonebook, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	pb := &Phonebook{db: db}

	if err := pb.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return pb, nil
}

// migrate creates the schema if it doesn't exist.
func (p *Phonebook) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS phonebook (
		node_id           TEXT NOT NULL,
		cluster_path      TEXT NOT NULL,
		name              TEXT NOT NULL DEFAULT '',
		public_key        BLOB,
		addresses         TEXT NOT NULL,
		region            TEXT NOT NULL DEFAULT '',
		datacenter        TEXT NOT NULL DEFAULT '',
		capabilities      TEXT,
		first_seen        INTEGER NOT NULL,
		last_seen         INTEGER NOT NULL,
		last_connected    INTEGER DEFAULT 0,
		updated_at        INTEGER NOT NULL,
		conn_attempts     INTEGER DEFAULT 0,
		conn_success      INTEGER DEFAULT 0,
		success_rate      REAL DEFAULT 0.0,
		consec_fails      INTEGER DEFAULT 0,
		reliability_score REAL DEFAULT 0.0,
		last_probe_time   INTEGER DEFAULT 0,
		last_probe_success INTEGER DEFAULT 1,
		status            TEXT DEFAULT 'active',
		is_connected      INTEGER DEFAULT 0,
		PRIMARY KEY (node_id, cluster_path)
	);

	CREATE INDEX IF NOT EXISTS idx_phonebook_cluster ON phonebook(cluster_path);
	CREATE INDEX IF NOT EXISTS idx_phonebook_node ON phonebook(node_id);
	CREATE INDEX IF NOT EXISTS idx_phonebook_status ON phonebook(cluster_path, status);
	CREATE INDEX IF NOT EXISTS idx_phonebook_reliability ON phonebook(cluster_path, success_rate DESC, last_seen DESC);
	`

	if _, err := p.db.Exec(schema); err != nil {
		return err
	}

	// Idempotent ALTER for upgrading DBs that pre-date the name column.
	// SQLite returns "duplicate column name" if it already exists; we
	// swallow that one specific error so re-runs are cheap.
	if _, err := p.db.Exec(`ALTER TABLE phonebook ADD COLUMN name TEXT NOT NULL DEFAULT ''`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("phonebook migration: add name column: %w", err)
		}
	}
	return nil
}

// Get retrieves a phonebook entry by node ID and cluster path.
func (p *Phonebook) Get(nodeID string, clusterPath string) (*Entry, error) {
	row := p.db.QueryRow(`
		SELECT node_id, cluster_path, name, public_key, addresses, region, datacenter,
		       capabilities, first_seen, last_seen, last_connected, updated_at,
		       conn_attempts, conn_success, success_rate, consec_fails,
		       reliability_score, last_probe_time, last_probe_success, status, is_connected
		FROM phonebook
		WHERE node_id = ? AND cluster_path = ?
	`, nodeID, clusterPath)

	return p.scanEntry(row)
}

// Exists checks if an entry exists.
func (p *Phonebook) Exists(nodeID string, clusterPath string) (bool, error) {
	var count int
	err := p.db.QueryRow(`
		SELECT COUNT(*) FROM phonebook WHERE node_id = ? AND cluster_path = ?
	`, nodeID, clusterPath).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// GetByCluster retrieves all entries for a cluster.
func (p *Phonebook) GetByCluster(clusterPath string) ([]*Entry, error) {
	rows, err := p.db.Query(`
		SELECT node_id, cluster_path, name, public_key, addresses, region, datacenter,
		       capabilities, first_seen, last_seen, last_connected, updated_at,
		       conn_attempts, conn_success, success_rate, consec_fails,
		       reliability_score, last_probe_time, last_probe_success, status, is_connected
		FROM phonebook
		WHERE cluster_path = ?
	`, clusterPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return p.scanEntries(rows)
}

// GetByNode retrieves all entries for a node across all clusters.
func (p *Phonebook) GetByNode(nodeID string) ([]*Entry, error) {
	rows, err := p.db.Query(`
		SELECT node_id, cluster_path, name, public_key, addresses, region, datacenter,
		       capabilities, first_seen, last_seen, last_connected, updated_at,
		       conn_attempts, conn_success, success_rate, consec_fails,
		       reliability_score, last_probe_time, last_probe_success, status, is_connected
		FROM phonebook
		WHERE node_id = ?
	`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return p.scanEntries(rows)
}

// GetBestPeers retrieves the best peers for a cluster, sorted by success rate.
func (p *Phonebook) GetBestPeers(clusterPath string, limit int) ([]*Entry, error) {
	rows, err := p.db.Query(`
		SELECT node_id, cluster_path, name, public_key, addresses, region, datacenter,
		       capabilities, first_seen, last_seen, last_connected, updated_at,
		       conn_attempts, conn_success, success_rate, consec_fails,
		       reliability_score, last_probe_time, last_probe_success, status, is_connected
		FROM phonebook
		WHERE cluster_path = ? AND status = 'active'
		ORDER BY success_rate DESC, last_seen DESC
		LIMIT ?
	`, clusterPath, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return p.scanEntries(rows)
}

// Add inserts a new entry.
func (p *Phonebook) Add(entry *Entry) error {
	addrsJSON, err := json.Marshal(entry.Addresses)
	if err != nil {
		return fmt.Errorf("failed to marshal addresses: %w", err)
	}

	var capsJSON []byte
	if entry.Capabilities != nil {
		capsJSON, err = json.Marshal(entry.Capabilities)
		if err != nil {
			return fmt.Errorf("failed to marshal capabilities: %w", err)
		}
	}

	now := time.Now().UnixMilli()
	firstSeen := entry.FirstSeen.UnixMilli()
	if firstSeen == 0 {
		firstSeen = now
	}
	lastSeen := entry.LastSeen.UnixMilli()
	if lastSeen == 0 {
		lastSeen = now
	}

	status := string(entry.Status)
	if status == "" {
		status = string(nodeStatusActive)
	}

	_, err = p.db.Exec(`
		INSERT INTO phonebook (
			node_id, cluster_path, name, public_key, addresses, region, datacenter,
			capabilities, first_seen, last_seen, last_connected, updated_at,
			conn_attempts, conn_success, success_rate, consec_fails,
			reliability_score, last_probe_time, last_probe_success, status, is_connected
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		entry.NodeID, entry.ClusterPath, entry.Name, entry.PublicKey, string(addrsJSON),
		entry.Region, entry.Datacenter, string(capsJSON),
		firstSeen, lastSeen, entry.LastConnected.UnixMilli(), now,
		entry.ConnectionAttempts, entry.ConnectionSuccess, entry.SuccessRate, entry.ConsecutiveFails,
		entry.ReliabilityScore, entry.LastProbeTime.UnixMilli(), boolToInt(entry.LastProbeSuccess),
		status, boolToInt(entry.IsConnected),
	)

	return err
}

// Update modifies an existing entry.
func (p *Phonebook) Update(entry *Entry) error {
	addrsJSON, err := json.Marshal(entry.Addresses)
	if err != nil {
		return fmt.Errorf("failed to marshal addresses: %w", err)
	}

	var capsJSON []byte
	if entry.Capabilities != nil {
		capsJSON, err = json.Marshal(entry.Capabilities)
		if err != nil {
			return fmt.Errorf("failed to marshal capabilities: %w", err)
		}
	}

	now := time.Now().UnixMilli()

	// COALESCE on name preserves an existing non-empty name when the
	// update carries an empty one (e.g. a phonebook-sync delta from an
	// older peer that didn't propagate the field).
	_, err = p.db.Exec(`
		UPDATE phonebook SET
			name = CASE WHEN ? = '' THEN name ELSE ? END,
			public_key = ?, addresses = ?, region = ?, datacenter = ?,
			capabilities = ?, last_seen = ?, last_connected = ?, updated_at = ?,
			conn_attempts = ?, conn_success = ?, success_rate = ?, consec_fails = ?,
			reliability_score = ?, last_probe_time = ?, last_probe_success = ?,
			status = ?, is_connected = ?
		WHERE node_id = ? AND cluster_path = ?
	`,
		entry.Name, entry.Name,
		entry.PublicKey, string(addrsJSON), entry.Region, entry.Datacenter,
		string(capsJSON), entry.LastSeen.UnixMilli(), entry.LastConnected.UnixMilli(), now,
		entry.ConnectionAttempts, entry.ConnectionSuccess, entry.SuccessRate, entry.ConsecutiveFails,
		entry.ReliabilityScore, entry.LastProbeTime.UnixMilli(), boolToInt(entry.LastProbeSuccess),
		string(entry.Status), boolToInt(entry.IsConnected),
		entry.NodeID, entry.ClusterPath,
	)

	return err
}

// Remove deletes an entry.
func (p *Phonebook) Remove(nodeID string, clusterPath string) error {
	_, err := p.db.Exec(`DELETE FROM phonebook WHERE node_id = ? AND cluster_path = ?`, nodeID, clusterPath)
	return err
}

// RemoveAllForNode deletes all entries for a node.
func (p *Phonebook) RemoveAllForNode(nodeID string) error {
	_, err := p.db.Exec(`DELETE FROM phonebook WHERE node_id = ?`, nodeID)
	return err
}

// RecordConnectionAttempt records a connection attempt.
func (p *Phonebook) RecordConnectionAttempt(nodeID string, clusterPath string, success bool) error {
	now := time.Now().UnixMilli()

	if success {
		_, err := p.db.Exec(`
			UPDATE phonebook SET
				conn_attempts = conn_attempts + 1,
				conn_success = conn_success + 1,
				success_rate = CAST(conn_success + 1 AS REAL) / CAST(conn_attempts + 1 AS REAL),
				consec_fails = 0,
				last_connected = ?,
				last_seen = ?,
				updated_at = ?,
				is_connected = 1
			WHERE node_id = ? AND cluster_path = ?
		`, now, now, now, nodeID, clusterPath)
		return err
	}

	_, err := p.db.Exec(`
		UPDATE phonebook SET
			conn_attempts = conn_attempts + 1,
			success_rate = CAST(conn_success AS REAL) / CAST(conn_attempts + 1 AS REAL),
			consec_fails = consec_fails + 1,
			updated_at = ?
		WHERE node_id = ? AND cluster_path = ?
	`, now, nodeID, clusterPath)
	return err
}

// RecordDisconnect records a peer disconnection.
func (p *Phonebook) RecordDisconnect(nodeID string, clusterPath string) error {
	now := time.Now().UnixMilli()
	_, err := p.db.Exec(`
		UPDATE phonebook SET
			is_connected = 0,
			last_seen = ?,
			updated_at = ?
		WHERE node_id = ? AND cluster_path = ?
	`, now, now, nodeID, clusterPath)
	return err
}

// SetReliabilityScore sets the reliability score for a node.
func (p *Phonebook) SetReliabilityScore(nodeID string, clusterPath string, score float64) error {
	now := time.Now().UnixMilli()
	_, err := p.db.Exec(`
		UPDATE phonebook SET reliability_score = ?, updated_at = ?
		WHERE node_id = ? AND cluster_path = ?
	`, score, now, nodeID, clusterPath)
	return err
}

// SetStatus sets the health status for a node.
func (p *Phonebook) SetStatus(nodeID string, clusterPath string, status NodeStatus) error {
	now := time.Now().UnixMilli()
	_, err := p.db.Exec(`
		UPDATE phonebook SET status = ?, updated_at = ?
		WHERE node_id = ? AND cluster_path = ?
	`, string(status), now, nodeID, clusterPath)
	return err
}

// GetByStatus retrieves entries by status.
func (p *Phonebook) GetByStatus(clusterPath string, status NodeStatus) ([]*Entry, error) {
	rows, err := p.db.Query(`
		SELECT node_id, cluster_path, name, public_key, addresses, region, datacenter,
		       capabilities, first_seen, last_seen, last_connected, updated_at,
		       conn_attempts, conn_success, success_rate, consec_fails,
		       reliability_score, last_probe_time, last_probe_success, status, is_connected
		FROM phonebook
		WHERE cluster_path = ? AND status = ?
	`, clusterPath, string(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return p.scanEntries(rows)
}

// RecordProbe records a probe result.
func (p *Phonebook) RecordProbe(nodeID string, clusterPath string, success bool) error {
	now := time.Now().UnixMilli()
	_, err := p.db.Exec(`
		UPDATE phonebook SET
			last_probe_time = ?,
			last_probe_success = ?,
			updated_at = ?
		WHERE node_id = ? AND cluster_path = ?
	`, now, boolToInt(success), now, nodeID, clusterPath)
	return err
}

// Prune removes old entries that have too many consecutive failures.
func (p *Phonebook) Prune(maxAge time.Duration, maxConsecutiveFails int) (int, error) {
	cutoff := time.Now().Add(-maxAge).UnixMilli()
	result, err := p.db.Exec(`
		DELETE FROM phonebook
		WHERE last_seen < ? AND consec_fails > ?
	`, cutoff, maxConsecutiveFails)
	if err != nil {
		return 0, err
	}

	count, err := result.RowsAffected()
	return int(count), err
}

// Count returns the total number of entries.
func (p *Phonebook) Count() (int, error) {
	var count int
	err := p.db.QueryRow(`SELECT COUNT(*) FROM phonebook`).Scan(&count)
	return count, err
}

// CountByCluster returns the number of entries for a cluster.
func (p *Phonebook) CountByCluster(clusterPath string) (int, error) {
	var count int
	err := p.db.QueryRow(`SELECT COUNT(*) FROM phonebook WHERE cluster_path = ?`, clusterPath).Scan(&count)
	return count, err
}

// Close closes the database connection.
func (p *Phonebook) Close() error {
	return p.db.Close()
}

// scanEntry scans a single row into an Entry.
func (p *Phonebook) scanEntry(row *sql.Row) (*Entry, error) {
	var entry Entry
	var addrsJSON, capsJSON sql.NullString
	var firstSeen, lastSeen, lastConnected, updatedAt, lastProbeTime int64
	var lastProbeSuccess, isConnected int
	var status string

	err := row.Scan(
		&entry.NodeID, &entry.ClusterPath, &entry.Name, &entry.PublicKey, &addrsJSON,
		&entry.Region, &entry.Datacenter, &capsJSON,
		&firstSeen, &lastSeen, &lastConnected, &updatedAt,
		&entry.ConnectionAttempts, &entry.ConnectionSuccess, &entry.SuccessRate, &entry.ConsecutiveFails,
		&entry.ReliabilityScore, &lastProbeTime, &lastProbeSuccess, &status, &isConnected,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if addrsJSON.Valid {
		json.Unmarshal([]byte(addrsJSON.String), &entry.Addresses)
	}
	if capsJSON.Valid && capsJSON.String != "" {
		entry.Capabilities = &Capabilities{}
		json.Unmarshal([]byte(capsJSON.String), entry.Capabilities)
	}

	entry.FirstSeen = time.UnixMilli(firstSeen)
	entry.LastSeen = time.UnixMilli(lastSeen)
	entry.LastConnected = time.UnixMilli(lastConnected)
	entry.UpdatedAt = time.UnixMilli(updatedAt)
	entry.LastProbeTime = time.UnixMilli(lastProbeTime)
	entry.LastProbeSuccess = lastProbeSuccess == 1
	entry.IsConnected = isConnected == 1
	entry.Status = NodeStatus(status)

	return &entry, nil
}

// scanEntries scans multiple rows into entries.
func (p *Phonebook) scanEntries(rows *sql.Rows) ([]*Entry, error) {
	var entries []*Entry

	for rows.Next() {
		var entry Entry
		var addrsJSON, capsJSON sql.NullString
		var firstSeen, lastSeen, lastConnected, updatedAt, lastProbeTime int64
		var lastProbeSuccess, isConnected int
		var status string

		err := rows.Scan(
			&entry.NodeID, &entry.ClusterPath, &entry.Name, &entry.PublicKey, &addrsJSON,
			&entry.Region, &entry.Datacenter, &capsJSON,
			&firstSeen, &lastSeen, &lastConnected, &updatedAt,
			&entry.ConnectionAttempts, &entry.ConnectionSuccess, &entry.SuccessRate, &entry.ConsecutiveFails,
			&entry.ReliabilityScore, &lastProbeTime, &lastProbeSuccess, &status, &isConnected,
		)
		if err != nil {
			return nil, err
		}

		if addrsJSON.Valid {
			json.Unmarshal([]byte(addrsJSON.String), &entry.Addresses)
		}
		if capsJSON.Valid && capsJSON.String != "" {
			entry.Capabilities = &Capabilities{}
			json.Unmarshal([]byte(capsJSON.String), entry.Capabilities)
		}

		entry.FirstSeen = time.UnixMilli(firstSeen)
		entry.LastSeen = time.UnixMilli(lastSeen)
		entry.LastConnected = time.UnixMilli(lastConnected)
		entry.UpdatedAt = time.UnixMilli(updatedAt)
		entry.LastProbeTime = time.UnixMilli(lastProbeTime)
		entry.LastProbeSuccess = lastProbeSuccess == 1
		entry.IsConnected = isConnected == 1
		entry.Status = NodeStatus(status)

		entries = append(entries, &entry)
	}

	return entries, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
