package capsule

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	enums "github.com/tareksalem/falak/capsule/enums"
	"github.com/tareksalem/falak/shared/migrations"
	"go.uber.org/zap"
)

// Store provides thread-safe CRUD operations for capsules, backed by SQLite
// with an in-memory cache for fast reads.
type Store struct {
	mu       sync.RWMutex
	capsules map[CapsuleID]*Capsule // in-memory cache
	db       *sql.DB                // SQLite persistence (nil for in-memory only)
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

	if err := s.runMigrations(); err != nil {
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

// runMigrations applies the capsule store's schema using the shared
// migrations runner. The schema is owned by an ordered list of versioned
// migrations (see capsuleMigrations) recorded in schema_migrations.
//
// Production databases built before task 11A.15a have the schema in place
// but no schema_migrations rows. capsuleBaselineDetector inspects those
// databases and lets the runner stamp V1+V2 as already applied without
// re-running their DDL.
func (s *Store) runMigrations() error {
	runner := migrations.NewRunner(
		"capsule",
		migrations.WithMigrations(capsuleMigrations()...),
		migrations.WithLogger(s.logger),
		migrations.WithBaselineDetector(capsuleBaselineDetector),
	)
	return runner.Run(s.db)
}

// capsuleMigrations returns the ordered list of forward-only schema
// migrations owned by the capsule module. Adding a new migration means
// appending to this list with the next integer version — never editing or
// reordering an existing entry.
func capsuleMigrations() []migrations.Migration {
	return []migrations.Migration{
		{
			Version:     1,
			Description: "initial capsules schema",
			Apply: func(tx *sql.Tx) error {
				const ddl = `
				CREATE TABLE capsules (
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
				CREATE INDEX idx_capsules_name ON capsules(name);
				CREATE INDEX idx_capsules_orbit ON capsules(orbit);
				CREATE INDEX idx_capsules_status ON capsules(status);
				CREATE INDEX idx_capsules_cluster ON capsules(cluster_id);
				`
				_, err := tx.Exec(ddl)
				return err
			},
		},
		{
			Version:     2,
			Description: "add kind + group_id columns and indexes",
			Apply: func(tx *sql.Tx) error {
				const ddl = `
				ALTER TABLE capsules ADD COLUMN kind     TEXT NOT NULL DEFAULT 'capsule';
				ALTER TABLE capsules ADD COLUMN group_id TEXT NOT NULL DEFAULT '';
				CREATE INDEX idx_capsules_kind  ON capsules(kind);
				CREATE INDEX idx_capsules_group ON capsules(group_id);
				`
				_, err := tx.Exec(ddl)
				return err
			},
		},
	}
}

// capsuleBaselineDetector inspects a pre-11A.15a capsule database. The
// presence of the `kind` column on the capsules table means V2 (which
// adds kind + group_id) had already been applied via the prior idempotent
// ALTER strategy; the runner can stamp V1 and V2 as applied without
// re-running their DDL.
//
// Returns 0 when:
//   - The capsules table does not exist (fresh database).
//   - The capsules table exists but the kind column is missing (only V1 was
//     applied out-of-band, which never shipped in any release — treat as
//     fresh so V1 will be rejected by SQLite if the table genuinely exists,
//     surfacing the inconsistency rather than silently stamping).
func capsuleBaselineDetector(db *sql.DB) (int, error) {
	rows, err := db.Query(`PRAGMA table_info(capsules)`)
	if err != nil {
		return 0, fmt.Errorf("inspect capsules columns: %w", err)
	}
	defer rows.Close()

	cols := make(map[string]struct{})
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notnull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return 0, err
		}
		cols[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(cols) == 0 {
		// Table doesn't exist — fresh database, run migrations normally.
		return 0, nil
	}
	if _, hasKind := cols["kind"]; hasKind {
		// Existing pre-11A.15a database with the full schema in place.
		return 2, nil
	}
	// Table exists without the kind column — never a shipped state. Refuse
	// to stamp; let the normal flow surface whatever inconsistency this is.
	return 0, nil
}

// loadAll reads all capsules from SQLite into the in-memory cache.
func (s *Store) loadAll() error {
	rows, err := s.db.Query(`
		SELECT id, cluster_id, name, status, version, orbit, tier,
		       spec_json, replicas_json, momentum_json, created_at, updated_at,
		       kind, group_id
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

// ListByGroup returns all member capsules whose Spec.GroupID equals groupID.
//
// Results are sorted by Spec.Name ascending so callers (tests, gossip
// emitters, the group manager) get a deterministic order on every call.
// An empty groupID returns an empty slice — capsules with empty GroupID are
// standalone, not "in the empty group".
func (s *Store) ListByGroup(groupID CapsuleID) []*Capsule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if groupID == "" {
		return nil
	}

	var result []*Capsule
	for _, c := range s.capsules {
		if c.Spec.GroupID == groupID {
			result = append(result, c)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Spec.Name < result[j].Spec.Name
	})
	return result
}

// ListByKind returns all capsules with the given Kind.
//
// Empty kind in stored capsules is treated as Capsule (back-compat with rows
// written before the kind column existed). Querying with an empty kind is
// rejected with a nil result; callers must pass a concrete kind. Results are
// sorted by Spec.Name ascending for determinism.
func (s *Store) ListByKind(kind CapsuleKind) []*Capsule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if kind == "" {
		return nil
	}

	var result []*Capsule
	for _, c := range s.capsules {
		stored := c.Spec.Kind
		if stored == "" {
			stored = CapsuleKindEnum.Capsule()
		}
		if stored == kind {
			result = append(result, c)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Spec.Name < result[j].Spec.Name
	})
	return result
}

// ListByStatus returns all capsules with the given status.
func (s *Store) ListByStatus(status enums.CapsuleStatus) []*Capsule {
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
			spec_json, replicas_json, momentum_json, created_at, updated_at,
			kind, group_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		string(c.ID), c.ClusterID, c.Spec.Name, string(c.Status), c.Version,
		c.Spec.Orbit, string(c.Spec.Tier),
		string(specJSON), string(replicasJSON), string(momentumJSON),
		c.CreatedAt.UnixMilli(), c.UpdatedAt.UnixMilli(),
		string(kindForStorage(c.Spec.Kind)), string(c.Spec.GroupID),
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
			spec_json = ?, replicas_json = ?, momentum_json = ?, updated_at = ?,
			kind = ?, group_id = ?
		WHERE id = ?
	`,
		c.ClusterID, c.Spec.Name, string(c.Status), c.Version,
		c.Spec.Orbit, string(c.Spec.Tier),
		string(specJSON), string(replicasJSON), string(momentumJSON),
		c.UpdatedAt.UnixMilli(),
		string(kindForStorage(c.Spec.Kind)), string(c.Spec.GroupID),
		string(c.ID),
	)
	return err
}

// kindForStorage normalizes a CapsuleKind for the kind column. Empty (legacy
// rows or callers that haven't run DefaultSpec) is stored as "capsule" so the
// column NOT NULL invariant holds and ListByKind has a single canonical form.
func kindForStorage(k CapsuleKind) CapsuleKind {
	if k == "" {
		return CapsuleKindEnum.Capsule()
	}
	return k
}

func (s *Store) scanRow(rows *sql.Rows) (*Capsule, error) {
	var (
		id, clusterID, name, status, version, orbit, tier string
		specJSON, replicasJSON, momentumJSON              sql.NullString
		createdAt, updatedAt                              int64
		kind, groupID                                     string
	)

	err := rows.Scan(
		&id, &clusterID, &name, &status, &version, &orbit, &tier,
		&specJSON, &replicasJSON, &momentumJSON, &createdAt, &updatedAt,
		&kind, &groupID,
	)
	if err != nil {
		return nil, err
	}

	c := &Capsule{
		ID:        CapsuleID(id),
		ClusterID: clusterID,
		Status:    enums.CapsuleStatus(status),
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

	// Reconcile dedicated columns with the JSON-decoded spec. Columns are the
	// authoritative source for kind/group_id (they drive index lookups); the
	// JSON-decoded spec might be stale on rows written before the column was
	// added. Empty kind in the column is normalized to Capsule for legacy rows.
	c.Spec.Kind = kindForStorage(CapsuleKind(kind))
	c.Spec.GroupID = CapsuleID(groupID)

	return c, nil
}
