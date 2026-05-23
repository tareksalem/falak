package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Defaults: Podman holds a transient reference for ~1s after the last
// container exits. Five attempts at 1s/2s/4s/8s/16s cover that window
// with headroom while capping total wait at ~31s.
const (
	defaultDestroyAttempts  = 5
	defaultDestroyBaseDelay = time.Second
	maxDestroyDelay         = 16 * time.Second
)

// WithDestroyRetries overrides the per-Destroy retry budget. < 1 ignored.
func WithDestroyRetries(n int) ManagerOption {
	return func(m *Manager) {
		if n >= 1 {
			m.destroyAttempts = n
		}
	}
}

// WithDestroyBaseDelay overrides the first backoff delay. <= 0 ignored.
func WithDestroyBaseDelay(d time.Duration) ManagerOption {
	return func(m *Manager) {
		if d > 0 {
			m.destroyBaseDelay = d
		}
	}
}

// PodmanNetworkLister lists every Podman network. ReapDangling uses it
// to discover falak-* networks leaked across process restarts.
type PodmanNetworkLister interface {
	ListNetworks(ctx context.Context) ([]string, error)
}

// destroyWithRetry attempts iptables-rule removal + Podman delete up to
// attempts times with exponential backoff. Transients retry, non-
// transients surface immediately, exhaustion registers the bridge in the
// dangling-set so a later ReapDangling can reap it. Reaper-idempotent.
func (m *Manager) destroyWithRetry(ctx context.Context, groupID, name string, alloc Allocation) error {
	attempts, base := m.destroyAttempts, m.destroyBaseDelay
	if attempts < 1 {
		attempts = defaultDestroyAttempts
	}
	if base <= 0 {
		base = defaultDestroyBaseDelay
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("bridge: destroy cancelled: %w", err)
		}
		rmErr := m.removeIsolationRules(ctx, name)
		delErr := m.podman.DeleteNetwork(ctx, name)
		if delErr == nil || errors.Is(delErr, ErrPodmanNetworkNotFound) {
			if rmErr != nil && !isIptablesNotFound(rmErr) {
				m.logger.Warn("bridge: iptables remove failed after podman delete",
					zap.String("group", groupID), zap.String("bridge", name), zap.Error(rmErr))
			}
			if err := m.allocator.Release(groupID); err != nil {
				return fmt.Errorf("bridge: release subnet: %w", err)
			}
			m.logger.Info("bridge destroyed", zap.String("group", groupID),
				zap.String("bridge", name), zap.String("subnet", alloc.Subnet.String()),
				zap.Int("attempt", i+1))
			return nil
		}
		lastErr = delErr
		if !isTransientPodmanError(delErr) {
			return fmt.Errorf("bridge: podman delete %s: %w", name, delErr)
		}
		delay := backoffDelay(base, i)
		m.logger.Warn("bridge: podman delete transient; retrying",
			zap.String("bridge", name), zap.Int("attempt", i+1),
			zap.Duration("backoff", delay), zap.Error(delErr))
		if !sleepCtx(ctx, delay) {
			return fmt.Errorf("bridge: destroy cancelled during backoff: %w", ctx.Err())
		}
	}
	m.registerDangling(name)
	m.logger.Error("bridge: podman delete failed after retries; dangling",
		zap.String("bridge", name), zap.Int("attempts", attempts), zap.Error(lastErr))
	return fmt.Errorf("bridge: podman delete %s after %d attempts: %w", name, attempts, lastErr)
}

// backoffDelay returns base*2^i, capped at maxDestroyDelay.
func backoffDelay(base time.Duration, i int) time.Duration {
	d := base
	for j := 0; j < i; j++ {
		if d >= maxDestroyDelay {
			return maxDestroyDelay
		}
		d *= 2
	}
	if d > maxDestroyDelay {
		return maxDestroyDelay
	}
	return d
}

// sleepCtx returns false when ctx fires before d elapses.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// isTransientPodmanError detects "network still in use" responses from
// Podman. The libpod API returns 409 with phrases that vary across
// versions; match the stable substrings.
func isTransientPodmanError(err error) bool {
	if err == nil || errors.Is(err, ErrPodmanNetworkNotFound) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "is being used") ||
		strings.Contains(msg, "network is in use") ||
		strings.Contains(msg, "has active endpoints") ||
		strings.Contains(msg, "container is using") ||
		strings.Contains(msg, "status 409")
}

func (m *Manager) registerDangling(name string) {
	m.danglingMu.Lock()
	m.dangling[name] = struct{}{}
	m.danglingMu.Unlock()
}

// Dangling returns a snapshot of bridge names abandoned by exhausted
// Destroy calls.
func (m *Manager) Dangling() []string {
	m.danglingMu.Lock()
	defer m.danglingMu.Unlock()
	out := make([]string, 0, len(m.dangling))
	for n := range m.dangling {
		out = append(out, n)
	}
	return out
}

// ReapDangling reclaims every `falak-*` Podman network not owned by the
// allocator, plus every name in the dangling-set. Idempotent; safe under
// concurrent Destroy. The reaper resolves leaks across process restarts
// (in-memory set is empty on cold start but PodmanNetworkLister exposes
// surviving networks).
func (m *Manager) ReapDangling(ctx context.Context) error {
	if ctx == nil {
		return errors.New("bridge: ReapDangling requires context")
	}
	seen := m.danglingSnapshot()
	if lister, ok := m.podman.(PodmanNetworkLister); ok {
		nets, err := lister.ListNetworks(ctx)
		if err != nil {
			m.logger.Warn("bridge: list networks for reap failed", zap.Error(err))
		}
		for _, n := range nets {
			if strings.HasPrefix(n, bridgeNamePrefix) {
				seen[n] = struct{}{}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	reserved, err := m.allocator.Reserved()
	if err != nil {
		return fmt.Errorf("bridge: enumerate allocations: %w", err)
	}
	known := make(map[string]struct{}, len(reserved))
	for _, r := range reserved {
		known[bridgeNamePrefix+r.GroupID] = struct{}{}
	}
	dang := m.danglingSnapshot()
	var firstErr error
	var reaped, skipped int
	for name := range seen {
		_, owned := known[name]
		_, listed := dang[name]
		if owned && !listed {
			skipped++
			continue
		}
		if err := m.reapOne(ctx, name); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if owned {
			if relErr := m.allocator.Release(strings.TrimPrefix(name, bridgeNamePrefix)); relErr != nil {
				m.logger.Warn("bridge: reap subnet release failed",
					zap.String("bridge", name), zap.Error(relErr))
			}
		}
		reaped++
	}
	if reaped+skipped > 0 {
		m.logger.Info("bridge: reap dangling pass complete",
			zap.Int("reaped", reaped), zap.Int("skipped", skipped))
	}
	return firstErr
}

// danglingSnapshot returns a fresh copy of the dangling-set.
func (m *Manager) danglingSnapshot() map[string]struct{} {
	m.danglingMu.Lock()
	defer m.danglingMu.Unlock()
	out := make(map[string]struct{}, len(m.dangling))
	for n := range m.dangling {
		out[n] = struct{}{}
	}
	return out
}

// reapOne removes iptables rules + Podman network for one dangling name.
// Idempotent: "not found" outcomes succeed silently.
func (m *Manager) reapOne(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.removeIsolationRules(ctx, name); err != nil && !isIptablesNotFound(err) {
		m.logger.Warn("bridge: reap iptables remove failed",
			zap.String("bridge", name), zap.Error(err))
	}
	if err := m.podman.DeleteNetwork(ctx, name); err != nil && !errors.Is(err, ErrPodmanNetworkNotFound) {
		return fmt.Errorf("bridge: reap podman delete %s: %w", name, err)
	}
	m.danglingMu.Lock()
	delete(m.dangling, name)
	m.danglingMu.Unlock()
	m.logger.Info("bridge: dangling reaped", zap.String("bridge", name))
	return nil
}
