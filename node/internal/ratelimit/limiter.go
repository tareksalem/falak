// Package ratelimit provides per-peer rate limiting for stream handlers.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter implements a per-key sliding window rate limiter.
type Limiter struct {
	mu         sync.Mutex
	requests   map[string][]time.Time
	maxReqs    int
	window     time.Duration
	cleanupInt time.Duration
}

// NewLimiter creates a new rate limiter.
// maxReqs is the maximum number of requests allowed per window.
// window is the time window for rate limiting.
func NewLimiter(maxReqs int, window time.Duration) *Limiter {
	l := &Limiter{
		requests:   make(map[string][]time.Time),
		maxReqs:    maxReqs,
		window:     window,
		cleanupInt: window * 2,
	}

	// Start background cleanup
	go l.cleanupLoop()

	return l
}

// Allow checks if a request from the given key is allowed.
// Returns true if allowed, false if rate limited.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-l.window)

	// Get existing requests and filter to window
	reqs := l.requests[key]
	var valid []time.Time
	for _, t := range reqs {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}

	// Check if over limit
	if len(valid) >= l.maxReqs {
		l.requests[key] = valid
		return false
	}

	// Add this request
	valid = append(valid, now)
	l.requests[key] = valid

	return true
}

// cleanupLoop periodically removes stale entries.
func (l *Limiter) cleanupLoop() {
	ticker := time.NewTicker(l.cleanupInt)
	defer ticker.Stop()

	for range ticker.C {
		l.cleanup()
	}
}

// cleanup removes entries with no recent requests.
func (l *Limiter) cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-l.window)

	for key, reqs := range l.requests {
		var valid []time.Time
		for _, t := range reqs {
			if t.After(cutoff) {
				valid = append(valid, t)
			}
		}

		if len(valid) == 0 {
			delete(l.requests, key)
		} else {
			l.requests[key] = valid
		}
	}
}
