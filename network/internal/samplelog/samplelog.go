// Package samplelog provides a small per-key rate limiter for noisy
// log call sites. Used by network/dns and network/endpoints to suppress
// per-connection / per-message error spam: a misbehaving peer can
// flood the responder with malformed packets, and an unsampled ERROR
// log would amplify the attack into the operator's logging pipeline.
package samplelog

import (
	"sync"
	"time"
)

// DefaultInterval is the default minimum gap between admitted log
// events per key.
const DefaultInterval = time.Second

// Sampler is a per-key rate limiter for log call sites. Safe for
// concurrent use.
type Sampler struct {
	mu       sync.Mutex
	interval time.Duration
	now      func() time.Time
	entries  map[string]*samplerEntry
}

type samplerEntry struct {
	lastAdmitted time.Time
	suppressed   int64
}

// Option configures a Sampler.
type Option func(*Sampler)

// WithInterval overrides the per-key admission interval. Non-positive
// values are ignored.
func WithInterval(d time.Duration) Option {
	return func(s *Sampler) {
		if d > 0 {
			s.interval = d
		}
	}
}

// WithNow overrides the clock used to evaluate admission windows.
func WithNow(fn func() time.Time) Option {
	return func(s *Sampler) {
		if fn != nil {
			s.now = fn
		}
	}
}

// NewSampler constructs a Sampler with the given options.
func NewSampler(opts ...Option) *Sampler {
	s := &Sampler{
		interval: DefaultInterval,
		now:      time.Now,
		entries:  make(map[string]*samplerEntry),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Allow reports whether the caller should log this event. The first
// call for a given key always returns true; subsequent calls return
// false until interval has elapsed since the last Allow=true result.
// Suppressed calls bump the per-key suppressed counter.
func (s *Sampler) Allow(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	entry, ok := s.entries[key]
	if !ok {
		s.entries[key] = &samplerEntry{lastAdmitted: now}
		return true
	}
	if now.Sub(entry.lastAdmitted) >= s.interval {
		entry.lastAdmitted = now
		entry.suppressed = 0
		return true
	}
	entry.suppressed++
	return false
}

// Suppressed returns the number of Allow=false results recorded for
// key since the previous Allow=true result. Returns zero for unknown
// keys. The counter resets only on a subsequent Allow=true.
func (s *Sampler) Suppressed(key string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return 0
	}
	return entry.suppressed
}
