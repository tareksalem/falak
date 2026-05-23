package bridge

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/tareksalem/falak/shared/migrations"
	"go.uber.org/zap"
)

// Sentinel errors returned by the Allocator. Match with errors.Is.
var (
	// ErrPoolExhausted signals every child subnet in the pool is taken.
	ErrPoolExhausted = errors.New("bridge: subnet pool exhausted")
	// ErrInvalidPool signals the pool mask is not strictly narrower than prefixBits.
	ErrInvalidPool = errors.New("bridge: pool mask is wider than prefix bits")
	// ErrIPv6Unsupported signals an IPv6 pool was supplied (v1 carves IPv4 only).
	ErrIPv6Unsupported = errors.New("bridge: IPv6 pools are not supported")
)

const (
	defaultPoolCIDR   = "10.88.0.0/16" // service-networking.md decision #25.
	defaultPrefixBits = 24
)

// Allocation is one /prefixBits subnet handed out to a single group.
// Gateway is the first usable IP (`<network>.1`), matching Podman's bridge
// driver convention.
type Allocation struct {
	GroupID   string
	Subnet    *net.IPNet
	Gateway   net.IP
	CreatedAt time.Time
}

// Allocator hands out per-group child subnets carved from a parent pool,
// persisting each allocation in SQLite so it survives node restarts. The
// zero value is not usable — construct one via OpenAllocator.
type Allocator struct {
	mu         sync.Mutex
	db         *sql.DB
	pool       *net.IPNet
	prefixBits int
	logger     *zap.Logger
}

// Option configures an Allocator returned by OpenAllocator.
type Option func(*Allocator)

// WithPool overrides the default 10.88.0.0/16 parent pool. Must be IPv4
// and strictly wider (smaller prefix number) than prefixBits.
func WithPool(pool *net.IPNet) Option { return func(a *Allocator) { a.pool = pool } }

// WithPrefixBits overrides the default child subnet size (24).
func WithPrefixBits(bits int) Option { return func(a *Allocator) { a.prefixBits = bits } }

// WithLogger replaces the default no-op logger.
func WithLogger(logger *zap.Logger) Option {
	return func(a *Allocator) {
		if logger != nil {
			a.logger = logger
		}
	}
}

// OpenAllocator constructs an Allocator backed by db, runs the
// bridge_subnets schema migration, and validates pool/prefixBits. The
// caller retains ownership of db.
func OpenAllocator(db *sql.DB, opts ...Option) (*Allocator, error) {
	if db == nil {
		return nil, errors.New("bridge: nil database")
	}
	_, defaultPool, err := net.ParseCIDR(defaultPoolCIDR)
	if err != nil {
		return nil, fmt.Errorf("bridge: parse default pool: %w", err)
	}
	a := &Allocator{db: db, pool: defaultPool, prefixBits: defaultPrefixBits, logger: zap.NewNop()}
	for _, opt := range opts {
		opt(a)
	}
	if err := validatePool(a.pool, a.prefixBits); err != nil {
		return nil, err
	}
	runner := migrations.NewRunner("network_bridge",
		migrations.WithMigrations(bridgeMigrations()...),
		migrations.WithLogger(a.logger))
	if err := runner.Run(db); err != nil {
		return nil, fmt.Errorf("bridge: run migrations: %w", err)
	}
	a.logger.Info("subnet allocator opened",
		zap.String("pool", a.pool.String()),
		zap.Int("prefix_bits", a.prefixBits),
		zap.Int("slots", a.totalSlots()))
	return a, nil
}

// validatePool rejects IPv6 and pools that cannot be carved into prefixBits.
func validatePool(pool *net.IPNet, prefixBits int) error {
	if pool == nil {
		return fmt.Errorf("%w: pool is nil", ErrInvalidPool)
	}
	if pool.IP.To4() == nil {
		return ErrIPv6Unsupported
	}
	maskBits, totalBits := pool.Mask.Size()
	if totalBits != 32 {
		return ErrIPv6Unsupported
	}
	if prefixBits <= maskBits || prefixBits > 32 {
		return fmt.Errorf("%w: pool=/%d prefixBits=%d", ErrInvalidPool, maskBits, prefixBits)
	}
	return nil
}

// totalSlots returns the number of /prefixBits subnets that fit in pool.
func (a *Allocator) totalSlots() int {
	maskBits, _ := a.pool.Mask.Size()
	return 1 << (a.prefixBits - maskBits)
}

// Allocate returns the existing allocation for groupID if any (idempotent),
// otherwise picks the lowest-numbered free child subnet, persists it, and
// returns it. Returns ErrPoolExhausted when every slot is taken.
func (a *Allocator) Allocate(groupID string) (Allocation, error) {
	if groupID == "" {
		return Allocation{}, errors.New("bridge: empty groupID")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if existing, ok, err := a.getLocked(groupID); err != nil {
		return Allocation{}, err
	} else if ok {
		return existing, nil
	}
	used, err := a.usedSubnetsLocked()
	if err != nil {
		return Allocation{}, fmt.Errorf("bridge: read used subnets: %w", err)
	}
	poolBase := poolBaseUint32(a.pool)
	step := uint32(1) << (32 - a.prefixBits)
	for slot, total := 0, a.totalSlots(); slot < total; slot++ {
		ip := uint32ToIP(poolBase + uint32(slot)*step)
		subnet := &net.IPNet{IP: ip, Mask: net.CIDRMask(a.prefixBits, 32)}
		if _, taken := used[subnet.String()]; taken {
			continue
		}
		alloc := Allocation{GroupID: groupID, Subnet: subnet, Gateway: gatewayFor(subnet), CreatedAt: time.Now()}
		if err := a.insertLocked(alloc); err != nil {
			return Allocation{}, fmt.Errorf("bridge: insert allocation: %w", err)
		}
		a.logger.Info("subnet allocated",
			zap.String("group", groupID),
			zap.String("subnet", subnet.String()),
			zap.String("gateway", alloc.Gateway.String()))
		return alloc, nil
	}
	return Allocation{}, ErrPoolExhausted
}

// Release deletes groupID's allocation. Unknown groupID returns nil
// (reaper-safe idempotence per IMPLEMENTATION_RULES.md section 7).
func (a *Allocator) Release(groupID string) error {
	if groupID == "" {
		return errors.New("bridge: empty groupID")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	res, err := a.db.Exec(`DELETE FROM bridge_subnets WHERE group_id = ?`, groupID)
	if err != nil {
		return fmt.Errorf("bridge: delete allocation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		a.logger.Debug("release on unknown group is a no-op", zap.String("group", groupID))
		return nil
	}
	a.logger.Info("subnet released", zap.String("group", groupID))
	return nil
}

// Reserved returns every current allocation sorted ascending by subnet CIDR.
func (a *Allocator) Reserved() ([]Allocation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rows, err := a.db.Query(`SELECT group_id, subnet_cidr, gateway_ip, created_at FROM bridge_subnets ORDER BY subnet_cidr`)
	if err != nil {
		return nil, fmt.Errorf("bridge: query reserved: %w", err)
	}
	defer rows.Close()
	var out []Allocation
	for rows.Next() {
		alloc, err := scanAllocation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, alloc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("bridge: iterate reserved: %w", err)
	}
	return out, nil
}

// Get looks up groupID's allocation without allocating. ok is false (with
// zero Allocation, nil error) when no row exists.
func (a *Allocator) Get(groupID string) (Allocation, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.getLocked(groupID)
}

func (a *Allocator) getLocked(groupID string) (Allocation, bool, error) {
	row := a.db.QueryRow(`SELECT group_id, subnet_cidr, gateway_ip, created_at FROM bridge_subnets WHERE group_id = ?`, groupID)
	alloc, err := scanAllocation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Allocation{}, false, nil
	}
	if err != nil {
		return Allocation{}, false, err
	}
	return alloc, true, nil
}

func (a *Allocator) usedSubnetsLocked() (map[string]struct{}, error) {
	rows, err := a.db.Query(`SELECT subnet_cidr FROM bridge_subnets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var cidr string
		if err := rows.Scan(&cidr); err != nil {
			return nil, err
		}
		out[cidr] = struct{}{}
	}
	return out, rows.Err()
}

func (a *Allocator) insertLocked(alloc Allocation) error {
	_, err := a.db.Exec(
		`INSERT INTO bridge_subnets (group_id, subnet_cidr, gateway_ip, created_at) VALUES (?, ?, ?, ?)`,
		alloc.GroupID, alloc.Subnet.String(), alloc.Gateway.String(), alloc.CreatedAt.UnixMilli())
	return err
}

// rowScanner unifies *sql.Row and *sql.Rows for scanAllocation.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanAllocation(s rowScanner) (Allocation, error) {
	var groupID, subnetStr, gatewayIP string
	var createdAt int64
	if err := s.Scan(&groupID, &subnetStr, &gatewayIP, &createdAt); err != nil {
		return Allocation{}, err
	}
	_, subnet, err := net.ParseCIDR(subnetStr)
	if err != nil {
		return Allocation{}, fmt.Errorf("bridge: parse stored subnet %q: %w", subnetStr, err)
	}
	gw := net.ParseIP(gatewayIP)
	if gw == nil {
		return Allocation{}, fmt.Errorf("bridge: parse stored gateway %q", gatewayIP)
	}
	if v4 := gw.To4(); v4 != nil {
		gw = v4
	}
	return Allocation{GroupID: groupID, Subnet: subnet, Gateway: gw, CreatedAt: time.UnixMilli(createdAt)}, nil
}

func poolBaseUint32(pool *net.IPNet) uint32 {
	return binary.BigEndian.Uint32(pool.IP.To4().Mask(pool.Mask))
}

func uint32ToIP(n uint32) net.IP {
	out := make(net.IP, 4)
	binary.BigEndian.PutUint32(out, n)
	return out
}

func gatewayFor(subnet *net.IPNet) net.IP {
	return uint32ToIP(binary.BigEndian.Uint32(subnet.IP.To4().Mask(subnet.Mask)) + 1)
}

// bridgeMigrations is the ordered forward-only migration list for the
// network_bridge module. Append new versions — never edit or reorder.
func bridgeMigrations() []migrations.Migration {
	return []migrations.Migration{{
		Version:     1,
		Description: "initial bridge_subnets schema",
		Apply: func(tx *sql.Tx) error {
			_, err := tx.Exec(`
				CREATE TABLE bridge_subnets (
					group_id     TEXT PRIMARY KEY,
					subnet_cidr  TEXT NOT NULL,
					gateway_ip   TEXT NOT NULL,
					created_at   INTEGER NOT NULL
				);
				CREATE INDEX idx_bridge_subnets_subnet ON bridge_subnets(subnet_cidr);
			`)
			return err
		},
	}}
}
