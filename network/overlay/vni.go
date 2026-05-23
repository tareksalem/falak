package overlay

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/tareksalem/falak/shared/migrations"
	"go.uber.org/zap"
)

// vniMask24 bounds a candidate to the 24-bit VXLAN VNI space (RFC 7348);
// VNI 0 is reserved. maxVNIAttempts caps salt-retry on per-cluster
// collision (16 keeps allocation under 1 ms at realistic group counts).
const (
	vniMask24       uint32 = 0x00FFFFFF
	maxVNIAttempts         = 16
)

// ErrVNIExhausted is returned when every salted retry collides
// per-cluster — only realistic near 2^24 groups in one cluster.
var ErrVNIExhausted = errors.New("overlay: VNI allocation exhausted after retries")

// SaltFunc derives the per-attempt VNI mixer. Production uses xxhash;
// tests inject a fake to force collisions.
type SaltFunc func(group string, attempt int) uint64

func defaultSalt(group string, attempt int) uint64 {
	h := xxhash.New()
	_, _ = h.WriteString("falak-vni-salt:" + group)
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(attempt))
	_, _ = h.Write(buf[:])
	return h.Sum64()
}

// VNIAllocator hands out per-(cluster, group) VXLAN VNIs in the 24-bit
// space, persisted in SQLite. The UNIQUE INDEX backstops concurrent racers.
type VNIAllocator struct {
	db     *sql.DB
	logger *zap.Logger
	salt   SaltFunc
}

// VNIOption configures a VNIAllocator.
type VNIOption func(*VNIAllocator)

// WithVNIDB injects the SQLite database (required).
func WithVNIDB(db *sql.DB) VNIOption { return func(v *VNIAllocator) { v.db = db } }

// WithVNILogger replaces the default no-op logger. Nil ignored.
func WithVNILogger(l *zap.Logger) VNIOption {
	return func(v *VNIAllocator) {
		if l != nil {
			v.logger = l
		}
	}
}

// WithSaltFn replaces the default xxhash salt; tests use it to force
// collisions and exhaustion paths.
func WithSaltFn(fn SaltFunc) VNIOption {
	return func(v *VNIAllocator) {
		if fn != nil {
			v.salt = fn
		}
	}
}

// OpenVNIAllocator constructs a VNIAllocator and runs the
// network_overlay_vni schema migration. Caller retains DB ownership.
func OpenVNIAllocator(opts ...VNIOption) (*VNIAllocator, error) {
	v := &VNIAllocator{logger: zap.NewNop(), salt: defaultSalt}
	for _, opt := range opts {
		opt(v)
	}
	if v.db == nil {
		return nil, errors.New("overlay: WithVNIDB required")
	}
	r := migrations.NewRunner("network_overlay_vni",
		migrations.WithMigrations(vniMigrations()...), migrations.WithLogger(v.logger))
	if err := r.Run(v.db); err != nil {
		return nil, fmt.Errorf("overlay: run vni migrations: %w", err)
	}
	v.logger.Info("vni allocator opened")
	return v, nil
}

// Allocate returns the VNI for (clusterPath, groupID), creating one if
// absent. Idempotent. Retries with a salted candidate up to
// maxVNIAttempts on per-cluster collisions before ErrVNIExhausted.
func (v *VNIAllocator) Allocate(clusterPath, groupID string) (uint32, error) {
	if clusterPath == "" || groupID == "" {
		return 0, errors.New("overlay: empty clusterPath or groupID")
	}
	if existing, ok, err := v.lookup(clusterPath, groupID); err != nil {
		return 0, fmt.Errorf("overlay: lookup vni: %w", err)
	} else if ok {
		return existing, nil
	}
	identity := clusterPath + "\x1f" + groupID
	for attempt := 0; attempt < maxVNIAttempts; attempt++ {
		candidate := uint32(v.salt(identity, attempt)) & vniMask24
		if candidate == 0 {
			candidate = 1
		}
		err := v.insert(clusterPath, groupID, candidate)
		fields := []zap.Field{zap.String("cluster", clusterPath), zap.String("group", groupID), zap.Uint32("vni", candidate), zap.Int("attempt", attempt)}
		if err == nil {
			v.logger.Info("vni allocated", fields...)
			return candidate, nil
		}
		if !isUniqueViolation(err) {
			return 0, fmt.Errorf("overlay: insert vni: %w", err)
		}
		if existing, ok, lerr := v.lookup(clusterPath, groupID); lerr == nil && ok {
			return existing, nil
		}
		v.logger.Warn("vni collision; retrying with next salt", fields...)
	}
	return 0, ErrVNIExhausted
}

// Release deletes (clusterPath, groupID). Missing rows return nil
// (reaper-idempotence, rules §7).
func (v *VNIAllocator) Release(clusterPath, groupID string) error {
	if clusterPath == "" || groupID == "" {
		return errors.New("overlay: empty clusterPath or groupID")
	}
	res, err := v.db.Exec(`DELETE FROM vni_allocations WHERE cluster_path=? AND group_id=?`,
		clusterPath, groupID)
	if err != nil {
		return fmt.Errorf("overlay: delete vni: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		v.logger.Debug("vni release on unknown group is a no-op",
			zap.String("cluster", clusterPath), zap.String("group", groupID))
		return nil
	}
	v.logger.Info("vni released", zap.String("cluster", clusterPath), zap.String("group", groupID))
	return nil
}

// Get returns the persisted VNI without allocating; ok=false on no row.
func (v *VNIAllocator) Get(clusterPath, groupID string) (uint32, bool, error) {
	if clusterPath == "" || groupID == "" {
		return 0, false, errors.New("overlay: empty clusterPath or groupID")
	}
	return v.lookup(clusterPath, groupID)
}

func (v *VNIAllocator) lookup(clusterPath, groupID string) (uint32, bool, error) {
	var vni int64
	err := v.db.QueryRow(`SELECT vni FROM vni_allocations WHERE cluster_path=? AND group_id=?`,
		clusterPath, groupID).Scan(&vni)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return uint32(vni), true, nil
}

func (v *VNIAllocator) insert(clusterPath, groupID string, vni uint32) error {
	_, err := v.db.Exec(
		`INSERT INTO vni_allocations (cluster_path, group_id, vni, created_at) VALUES (?,?,?,?)`,
		clusterPath, groupID, int64(vni), time.Now().UnixMilli())
	return err
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "UNIQUE constraint failed") || strings.Contains(m, "constraint failed: UNIQUE")
}

func vniMigrations() []migrations.Migration {
	return []migrations.Migration{{Version: 1, Description: "initial vni_allocations schema",
		Apply: func(tx *sql.Tx) error {
			_, err := tx.Exec(`CREATE TABLE vni_allocations (
				cluster_path TEXT NOT NULL, group_id TEXT NOT NULL,
				vni INTEGER NOT NULL, created_at INTEGER NOT NULL,
				PRIMARY KEY (cluster_path, group_id));
			CREATE UNIQUE INDEX idx_vni_allocations_cluster_vni
				ON vni_allocations(cluster_path, vni);`)
			return err
		}}}
}
