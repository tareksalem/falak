package capsule

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
)

// Store provides thread-safe CRUD operations for capsules, backed by SQLite
// with an in-memory cache for fast reads.
type Store struct {
	mu       sync.RWMutex
	capsules map[CapsuleID]*Capsule // in-memory cache
	db       *sql.DB               // SQLite persistence (nil for in-memory only)
	logger   *zap.Logger
}

// StoreOption configures a Store.
type StoreOption func(*Store)

// WithStoreLogger sets the logger for the store.
func WithStoreLogger(logger *zap.Logger) StoreOption {
	return func(s *Store) {
		s.logger = logger
	}
}

// NewStore creates a new in-memory-only capsule store (no persistence).
func NewStore(opts ...StoreOption) *Store {
	s := &Store{
		capsules: make(map[CapsuleID]*Capsule),
		logger:   zap.NewNop(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// OpenStore creates or opens a SQLite-backed capsule store at the given path.
// All capsules are loaded into the in-memory cache on open.
func OpenStore(path string, opts ...StoreOption) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("failed to open capsule database: %w", err)
	}

	s := &Store{
		capsules: make(map[CapsuleID]*Capsule),
		db:       db,
		logger:   zap.NewNop(),
	}
	for _, opt := range opts {
		opt(s)
	}

	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to run capsule migrations: %w", err)
	}

	if err := s.loadAll(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to load capsules from database: %w", err)
	}

	s.logger.Info("capsule store opened",
		zap.String("path", path),
		zap.Int("loaded", len(s.capsules)))

	return s, nil
}

// migrate creates the schema if it doesn't exist.
func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS capsules (
		id             TEXT PRIMARY KEY,
		cluster_id     TEXT NOT NULL DEFAULT '',
		name           TEXT NOT NULL,
		status         TEXT NOT NULL DEFAULT 'created',
		version        TEXT NOT NULL DEFAULT '1',
		orbit          TEXT NOT NULL DEFAULT '',
		tier           TEXT NOT NULL DEFAULT 'standard',
		spec_json      TEXT NOT NULL,
		replicas_json  TEXT,
		momentum_json  TEXT,
		created_at     INTEGER NOT NULL,
		updated_at     INTEGER NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_capsules_name ON capsules(name);
	CREATE INDEX IF NOT EXISTS idx_capsules_orbit ON capsules(orbit);
	CREATE INDEX IF NOT EXISTS idx_capsules_status ON capsules(status);
	CREATE INDEX IF NOT EXISTS idx_capsules_cluster ON capsules(cluster_id);
	`
	_, err := s.db.Exec(schema)
	return err
}

// loadAll reads all capsules from SQLite into the in-memory cache.
func (s *Store) loadAll() error {
	rows, err := s.db.Query(`
		SELECT id, cluster_id, name, status, version, orbit, tier,
		       spec_json, replicas_json, momentum_json, created_at, updated_at
		FROM capsules
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		c, err := s.scanRow(rows)
		if err != nil {
			return fmt.Errorf("failed to scan capsule row: %w", err)
		}
		s.capsules[c.ID] = c
	}
	return rows.Err()
}

// Create stores a new capsule. Returns an error if the ID already exists.
func (s *Store) Create(capsule *Capsule) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.capsules[capsule.ID]; exists {
		return fmt.Errorf("capsule %s already exists", capsule.ID)
	}

	now := time.Now()
	capsule.CreatedAt = now
	capsule.UpdatedAt = now

	if s.db != nil {
		if err := s.insertDB(capsule); err != nil {
			return fmt.Errorf("failed to persist capsule: %w", err)
		}
	}

	s.capsules[capsule.ID] = capsule
	s.logger.Debug("capsule stored",
		zap.String("id", capsule.ID.String()),
		zap.String("name", capsule.Spec.Name))
	return nil
}

// Get retrieves a capsule by ID. Returns nil if not found.
func (s *Store) Get(id CapsuleID) *Capsule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.capsules[id]
}

// GetByName retrieves a capsule by its spec name. Returns nil if not found.
func (s *Store) GetByName(name string) *Capsule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, c := range s.capsules {
		if c.Spec.Name == name {
			return c
		}
	}
	return nil
}

// Update replaces a capsule in the store. Returns an error if not found.
func (s *Store) Update(capsule *Capsule) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.capsules[capsule.ID]; !exists {
		return fmt.Errorf("capsule %s not found", capsule.ID)
	}

	capsule.UpdatedAt = time.Now()

	if s.db != nil {
		if err := s.updateDB(capsule); err != nil {
			return fmt.Errorf("failed to persist capsule update: %w", err)
		}
	}

	s.capsules[capsule.ID] = capsule
	s.logger.Debug("capsule updated",
		zap.String("id", capsule.ID.String()),
		zap.String("name", capsule.Spec.Name))
	return nil
}

// Delete removes a capsule by ID. Returns an error if not found.
func (s *Store) Delete(id CapsuleID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.capsules[id]; !exists {
		return fmt.Errorf("capsule %s not found", id)
	}

	if s.db != nil {
		if _, err := s.db.Exec(`DELETE FROM capsules WHERE id = ?`, string(id)); err != nil {
			return fmt.Errorf("failed to delete capsule from database: %w", err)
		}
	}

	delete(s.capsules, id)
	s.logger.Debug("capsule deleted",
		zap.String("id", id.String()))
	return nil
}

// List returns all capsules in the store.
func (s *Store) List() []*Capsule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*Capsule, 0, len(s.capsules))
	for _, c := range s.capsules {
		result = append(result, c)
	}
	return result
}

// ListByOrbit returns all capsules in the given orbit.
func (s *Store) ListByOrbit(orbit string) []*Capsule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*Capsule
	for _, c := range s.capsules {
		if c.Spec.Orbit == orbit {
			result = append(result, c)
		}
	}
	return result
}

// ListByLabels returns all capsules whose labels match all the given labels.
func (s *Store) ListByLabels(labels Labels) []*Capsule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*Capsule
	for _, c := range s.capsules {
		if LabelsMatch(c.Spec.Labels, labels) {
			result = append(result, c)
		}
	}
	return result
}

// ListByStatus returns all capsules with the given status.
func (s *Store) ListByStatus(status CapsuleStatus) []*Capsule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*Capsule
	for _, c := range s.capsules {
		if c.Status == status {
			result = append(result, c)
		}
	}
	return result
}

// Count returns the number of capsules in the store.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.capsules)
}

// Close closes the SQLite database connection.
func (s *Store) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// --- SQLite persistence helpers ---

func (s *Store) insertDB(c *Capsule) error {
	specJSON, err := json.Marshal(c.Spec)
	if err != nil {
		return fmt.Errorf("failed to marshal spec: %w", err)
	}

	replicasJSON, err := json.Marshal(c.Replicas)
	if err != nil {
		return fmt.Errorf("failed to marshal replicas: %w", err)
	}

	momentumJSON, err := json.Marshal(c.Momentum)
	if err != nil {
		return fmt.Errorf("failed to marshal momentum: %w", err)
	}

	_, err = s.db.Exec(`
		INSERT INTO capsules (
			id, cluster_id, name, status, version, orbit, tier,
			spec_json, replicas_json, momentum_json, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		string(c.ID), c.ClusterID, c.Spec.Name, string(c.Status), c.Version,
		c.Spec.Orbit, string(c.Spec.Tier),
		string(specJSON), string(replicasJSON), string(momentumJSON),
		c.CreatedAt.UnixMilli(), c.UpdatedAt.UnixMilli(),
	)
	return err
}

func (s *Store) updateDB(c *Capsule) error {
	specJSON, err := json.Marshal(c.Spec)
	if err != nil {
		return fmt.Errorf("failed to marshal spec: %w", err)
	}

	replicasJSON, err := json.Marshal(c.Replicas)
	if err != nil {
		return fmt.Errorf("failed to marshal replicas: %w", err)
	}

	momentumJSON, err := json.Marshal(c.Momentum)
	if err != nil {
		return fmt.Errorf("failed to marshal momentum: %w", err)
	}

	_, err = s.db.Exec(`
		UPDATE capsules SET
			cluster_id = ?, name = ?, status = ?, version = ?, orbit = ?, tier = ?,
			spec_json = ?, replicas_json = ?, momentum_json = ?, updated_at = ?
		WHERE id = ?
	`,
		c.ClusterID, c.Spec.Name, string(c.Status), c.Version,
		c.Spec.Orbit, string(c.Spec.Tier),
		string(specJSON), string(replicasJSON), string(momentumJSON),
		c.UpdatedAt.UnixMilli(),
		string(c.ID),
	)
	return err
}

func (s *Store) scanRow(rows *sql.Rows) (*Capsule, error) {
	var (
		id, clusterID, name, status, version, orbit, tier string
		specJSON, replicasJSON, momentumJSON               sql.NullString
		createdAt, updatedAt                               int64
	)

	err := rows.Scan(
		&id, &clusterID, &name, &status, &version, &orbit, &tier,
		&specJSON, &replicasJSON, &momentumJSON, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	c := &Capsule{
		ID:        CapsuleID(id),
		ClusterID: clusterID,
		Status:    CapsuleStatus(status),
		Version:   version,
		CreatedAt: time.UnixMilli(createdAt),
		UpdatedAt: time.UnixMilli(updatedAt),
	}

	if specJSON.Valid {
		if err := json.Unmarshal([]byte(specJSON.String), &c.Spec); err != nil {
			return nil, fmt.Errorf("failed to unmarshal spec: %w", err)
		}
	}

	if replicasJSON.Valid && replicasJSON.String != "" {
		if err := json.Unmarshal([]byte(replicasJSON.String), &c.Replicas); err != nil {
			return nil, fmt.Errorf("failed to unmarshal replicas: %w", err)
		}
	}

	if momentumJSON.Valid && momentumJSON.String != "" {
		if err := json.Unmarshal([]byte(momentumJSON.String), &c.Momentum); err != nil {
			return nil, fmt.Errorf("failed to unmarshal momentum: %w", err)
		}
	}

	return c, nil
}
