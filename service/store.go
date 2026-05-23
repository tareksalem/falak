package service

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
)

// ErrServiceNotFound is returned by Get/Update/Delete when the record
// is absent. Delete is reaper-idempotent — callers may swallow this
// sentinel with errors.Is.
var ErrServiceNotFound = errors.New("service: not found")

// ErrServiceExists is returned by Create when the supplied ID already
// exists in the store.
var ErrServiceExists = errors.New("service: already exists")

// Store is a thread-safe Service repository backed by SQLite with an
// in-memory cache. NewStore returns an in-memory-only store; OpenStore
// persists to disk on the originator.
type Store struct {
	mu       sync.RWMutex
	services map[ServiceID]*Service
	// byCapsuleName is a secondary index from a backend capsule name to
	// the set of ServiceIDs that reference it. Maintained on every
	// Create / Update / Delete under the same mutex as `services` so the
	// two are always consistent. ListReferencingCapsule reads it for an
	// O(M) lookup (M = referencing services) instead of the previous
	// O(N) full scan across every Service.
	byCapsuleName map[string]map[ServiceID]struct{}
	// indexedBackends snapshots the backend capsule names registered in
	// byCapsuleName for each ServiceID. Required because Manager.Update
	// mutates the live *Service in place before calling Store.Update —
	// without this snapshot we cannot tell which entries to remove from
	// byCapsuleName when the spec swaps backends.
	indexedBackends map[ServiceID][]string
	db              *sql.DB // nil for in-memory-only stores.
	logger          *zap.Logger
}

// StoreOption configures a Store.
type StoreOption func(*Store)

// WithStoreLogger sets the zap logger used by the store.
func WithStoreLogger(logger *zap.Logger) StoreOption {
	return func(s *Store) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// NewStore constructs an in-memory-only Service store.
func NewStore(opts ...StoreOption) *Store {
	s := &Store{
		services:        make(map[ServiceID]*Service),
		byCapsuleName:   make(map[string]map[ServiceID]struct{}),
		indexedBackends: make(map[ServiceID][]string),
		logger:          zap.NewNop(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// OpenStore opens (or creates) a SQLite-backed store at path and loads
// every existing row into the in-memory cache.
func OpenStore(path string, opts ...StoreOption) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("service: open database: %w", err)
	}
	s := &Store{
		services:        make(map[ServiceID]*Service),
		byCapsuleName:   make(map[string]map[ServiceID]struct{}),
		indexedBackends: make(map[ServiceID][]string),
		db:              db,
		logger:          zap.NewNop(),
	}
	for _, opt := range opts {
		opt(s)
	}
	if err := s.runMigrations(); err != nil {
		db.Close()
		return nil, fmt.Errorf("service: run migrations: %w", err)
	}
	if err := s.loadAll(); err != nil {
		db.Close()
		return nil, fmt.Errorf("service: load services: %w", err)
	}
	s.logger.Info("service store opened",
		zap.String("path", path), zap.Int("loaded", len(s.services)))
	return s, nil
}

// addCapsuleIndex registers svc.ID under every capsule name its
// backends reference, and records the registered names in
// indexedBackends so a later Update / Delete can reverse the exact same
// edits even when the live Service was mutated in place by the caller.
// Caller MUST hold s.mu.Lock.
func (s *Store) addCapsuleIndex(svc *Service) {
	if svc == nil {
		return
	}
	names := make([]string, 0, len(svc.Spec.Backends))
	for _, b := range svc.Spec.Backends {
		if b.Capsule == "" {
			continue
		}
		set, ok := s.byCapsuleName[b.Capsule]
		if !ok {
			set = make(map[ServiceID]struct{})
			s.byCapsuleName[b.Capsule] = set
		}
		set[svc.ID] = struct{}{}
		names = append(names, b.Capsule)
	}
	if len(names) > 0 {
		s.indexedBackends[svc.ID] = names
	} else {
		delete(s.indexedBackends, svc.ID)
	}
}

// removeCapsuleIndex drops every entry previously registered for id via
// addCapsuleIndex. Reads indexedBackends rather than the live Service's
// current backends so the operation is correct even when the caller
// already swapped the in-memory spec (the Manager.Update path does
// exactly that). Caller MUST hold s.mu.Lock.
func (s *Store) removeCapsuleIndex(id ServiceID) {
	names, ok := s.indexedBackends[id]
	if !ok {
		return
	}
	for _, name := range names {
		set, ok := s.byCapsuleName[name]
		if !ok {
			continue
		}
		delete(set, id)
		if len(set) == 0 {
			delete(s.byCapsuleName, name)
		}
	}
	delete(s.indexedBackends, id)
}

// Create stores a new Service. Returns ErrServiceExists when the ID is
// already present. CreatedAt and UpdatedAt are stamped here.
func (s *Store) Create(svc *Service) error {
	if svc == nil {
		return errors.New("service: nil Service in Create")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.services[svc.ID]; exists {
		return fmt.Errorf("%w: %s", ErrServiceExists, svc.ID)
	}
	now := time.Now()
	svc.CreatedAt = now
	svc.UpdatedAt = now
	if s.db != nil {
		if err := s.insertDB(svc); err != nil {
			return fmt.Errorf("service: persist create: %w", err)
		}
	}
	s.services[svc.ID] = svc
	s.addCapsuleIndex(svc)
	s.logger.Debug("service stored",
		zap.String("service", svc.ID.String()), zap.String("name", svc.Spec.Name))
	return nil
}

// Get returns the Service with the given ID or nil when absent.
func (s *Store) Get(id ServiceID) *Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.services[id]
}

// GetByName returns the (cluster-unique) Service named name, or nil.
func (s *Store) GetByName(name string) *Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, svc := range s.services {
		if svc.Spec.Name == name {
			return svc
		}
	}
	return nil
}

// Update replaces a Service. Returns ErrServiceNotFound when absent.
func (s *Store) Update(svc *Service) error {
	if svc == nil {
		return errors.New("service: nil Service in Update")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.services[svc.ID]; !exists {
		return fmt.Errorf("%w: %s", ErrServiceNotFound, svc.ID)
	}
	svc.UpdatedAt = time.Now()
	if s.db != nil {
		if err := s.updateDB(svc); err != nil {
			return fmt.Errorf("service: persist update: %w", err)
		}
	}
	// Rebuild the secondary index for the swap: drop the previous
	// backend set (resolved via indexedBackends, which records exactly
	// which buckets we registered the last time round — Manager.Update
	// mutates the Service in place so reading prev.Spec.Backends here
	// would yield the new spec), then register the new one. Same Lock,
	// strictly consistent with `services`.
	s.removeCapsuleIndex(svc.ID)
	s.services[svc.ID] = svc
	s.addCapsuleIndex(svc)
	s.logger.Debug("service updated",
		zap.String("service", svc.ID.String()), zap.String("name", svc.Spec.Name))
	return nil
}

// Delete removes a Service. Returns ErrServiceNotFound when absent;
// callers may swallow it via errors.Is for idempotent reapers.
func (s *Store) Delete(id ServiceID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.services[id]; !exists {
		return fmt.Errorf("%w: %s", ErrServiceNotFound, id)
	}
	if s.db != nil {
		if _, err := s.db.Exec(`DELETE FROM services WHERE id = ?`, string(id)); err != nil {
			return fmt.Errorf("service: persist delete: %w", err)
		}
	}
	s.removeCapsuleIndex(id)
	delete(s.services, id)
	s.logger.Debug("service deleted", zap.String("service", id.String()))
	return nil
}

// List returns every Service sorted by Spec.Name.
func (s *Store) List() []*Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return sortedByName(s.services, nil)
}

// ListByVisibility returns Services whose Spec.Visibility equals v.
func (s *Store) ListByVisibility(v Visibility) []*Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return sortedByName(s.services, func(svc *Service) bool { return svc.Spec.Visibility == v })
}

// ListByGroup returns Services whose Spec.Group equals group. Empty
// group returns nil; callers must pass a concrete name.
func (s *Store) ListByGroup(group string) []*Service {
	if group == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return sortedByName(s.services, func(svc *Service) bool { return svc.Spec.Group == group })
}

// ListReferencingCapsule returns Services with at least one backend
// whose Capsule name equals capsuleName. Empty capsuleName returns nil.
// Backed by the byCapsuleName secondary index — O(M) in the number of
// referencing Services, not O(N) over every Service in the store.
func (s *Store) ListReferencingCapsule(capsuleName string) []*Service {
	if capsuleName == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	idx, ok := s.byCapsuleName[capsuleName]
	if !ok || len(idx) == 0 {
		return nil
	}
	out := make([]*Service, 0, len(idx))
	for id := range idx {
		if svc, found := s.services[id]; found {
			out = append(out, svc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
	return out
}

// Count returns the number of Services in the store.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.services)
}

// Close releases the underlying SQLite handle. Safe on in-memory stores.
func (s *Store) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// sortedByName collects services matching pred (or every service when
// pred is nil) into a deterministic-order slice.
func sortedByName(in map[ServiceID]*Service, pred func(*Service) bool) []*Service {
	out := make([]*Service, 0, len(in))
	for _, svc := range in {
		if pred == nil || pred(svc) {
			out = append(out, svc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
	return out
}
