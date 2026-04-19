package orbit

import (
	"crypto/sha256"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Deduplicator rejects duplicate messages using a SHA-256 cache.
type Deduplicator struct {
	mu      sync.Mutex
	seen    map[[32]byte]time.Time
	maxAge  time.Duration
	logger  *zap.Logger
}

// DeduplicatorOption configures a Deduplicator.
type DeduplicatorOption func(*Deduplicator)

// WithDeduplicatorMaxAge sets the maximum age of cached message hashes.
func WithDeduplicatorMaxAge(d time.Duration) DeduplicatorOption {
	return func(dd *Deduplicator) {
		dd.maxAge = d
	}
}

// WithDeduplicatorLogger sets the logger.
func WithDeduplicatorLogger(logger *zap.Logger) DeduplicatorOption {
	return func(dd *Deduplicator) {
		dd.logger = logger
	}
}

// NewDeduplicator creates a new message deduplicator.
func NewDeduplicator(opts ...DeduplicatorOption) *Deduplicator {
	d := &Deduplicator{
		seen:   make(map[[32]byte]time.Time),
		maxAge: 5 * time.Minute,
		logger: zap.NewNop(),
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// IsDuplicate returns true if this message has been seen before.
// If not a duplicate, it records the message hash.
func (d *Deduplicator) IsDuplicate(data []byte) bool {
	hash := sha256.Sum256(data)

	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.seen[hash]; exists {
		return true
	}

	d.seen[hash] = time.Now()
	return false
}

// Cleanup removes expired entries from the cache.
func (d *Deduplicator) Cleanup() {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()

	for hash, ts := range d.seen {
		if now.Sub(ts) > d.maxAge {
			delete(d.seen, hash)
		}
	}
}

// Size returns the number of entries in the cache.
func (d *Deduplicator) Size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}
