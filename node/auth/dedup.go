package auth

import (
	"crypto/sha256"
	"sync"
	"time"
)

// MessageDedup provides message deduplication using a time-windowed cache of
// message fingerprints. Messages seen within the TTL window are considered
// duplicates and rejected. This prevents replay attacks within the MaxMessageAge
// window where age-based checks alone are insufficient.
type MessageDedup struct {
	mu      sync.Mutex
	seen    map[[sha256.Size]byte]time.Time // fingerprint → first seen time
	ttl     time.Duration                   // how long to remember messages
	closeCh chan struct{}
	wg      sync.WaitGroup
}

// NewMessageDedup creates a new deduplication cache with the given TTL.
// The cleanup interval controls how often expired entries are purged.
// A reasonable cleanup interval is ttl/2 or ttl/3.
func NewMessageDedup(ttl time.Duration, cleanupInterval time.Duration) *MessageDedup {
	d := &MessageDedup{
		seen:    make(map[[sha256.Size]byte]time.Time),
		ttl:     ttl,
		closeCh: make(chan struct{}),
	}

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.cleanupLoop(cleanupInterval)
	}()

	return d
}

// IsDuplicate checks if the message has been seen before. If not, it records
// the message fingerprint and returns false. If the message was already seen
// within the TTL window, it returns true.
//
// This method is safe for concurrent use.
func (d *MessageDedup) IsDuplicate(data []byte) bool {
	fingerprint := sha256.Sum256(data)

	d.mu.Lock()
	defer d.mu.Unlock()

	if seenAt, exists := d.seen[fingerprint]; exists {
		// Entry exists — check if it's still within the TTL window
		if time.Since(seenAt) <= d.ttl {
			return true
		}
		// Entry expired, treat as new and update the timestamp
	}

	d.seen[fingerprint] = time.Now()
	return false
}

// Close stops the background cleanup goroutine and releases resources.
func (d *MessageDedup) Close() {
	close(d.closeCh)
	d.wg.Wait()
}

// cleanupLoop periodically removes expired entries from the cache.
func (d *MessageDedup) cleanupLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-d.closeCh:
			return
		case <-ticker.C:
			d.evictExpired()
		}
	}
}

// evictExpired removes all entries older than the TTL.
func (d *MessageDedup) evictExpired() {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	for fingerprint, seenAt := range d.seen {
		if now.Sub(seenAt) > d.ttl {
			delete(d.seen, fingerprint)
		}
	}
}

// Len returns the current number of tracked fingerprints. Useful for monitoring.
func (d *MessageDedup) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}
