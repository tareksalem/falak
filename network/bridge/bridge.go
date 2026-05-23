package bridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ErrBridgeNotFound signals Inspect for a group with neither a recorded
// allocation nor a known Podman network.
var ErrBridgeNotFound = errors.New("bridge: not found")

// ErrIptablesRuleNotFound is the sentinel a CommandRunner may return to
// indicate a rule already absent. Destroy treats it as success.
var ErrIptablesRuleNotFound = errors.New("bridge: iptables rule not found")

const (
	bridgeNamePrefix = "falak-" // Linux ifname cap is 15 bytes; keep it short.
	defaultBridgeMTU = 1450     // 1500 underlay − 50 VXLAN (see 11A.4).
)

// BridgeInfo describes a provisioned per-group bridge.
type BridgeInfo struct {
	GroupID string
	Name    string
	Subnet  *net.IPNet
	Gateway net.IP
	MTU     int
}

// CommandRunner is the seam through which iptables commands are executed.
// Defaults to exec.CommandContext. Tests inject a recorder.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) error
}

type execRunner struct{}

// Run executes name with args, surfacing combined stderr in the error.
func (execRunner) Run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, out)
	}
	return nil
}

// Manager owns the per-group Podman bridge lifecycle: subnet allocation,
// network create/destroy, and FILTER-table isolation rules. Construct via
// NewManager; safe under concurrent callers.
type Manager struct {
	mu               sync.Mutex
	podman           PodmanNetworkClient
	allocator        *Allocator
	iptablesRunner   CommandRunner
	logger           *zap.Logger
	takeover         bool
	mtu              int
	destroyAttempts  int
	destroyBaseDelay time.Duration

	danglingMu sync.Mutex
	dangling   map[string]struct{}
}

// ManagerOption configures a Manager.
type ManagerOption func(*Manager)

// WithPodmanClient injects the PodmanNetworkClient (required).
func WithPodmanClient(c PodmanNetworkClient) ManagerOption {
	return func(m *Manager) { m.podman = c }
}

// WithAllocator injects the subnet Allocator (required).
func WithAllocator(a *Allocator) ManagerOption { return func(m *Manager) { m.allocator = a } }

// WithManagerLogger replaces the default no-op logger.
func WithManagerLogger(l *zap.Logger) ManagerOption {
	return func(m *Manager) {
		if l != nil {
			m.logger = l
		}
	}
}

// WithIptablesRunner injects a CommandRunner for iptables (tests/mocks).
func WithIptablesRunner(r CommandRunner) ManagerOption {
	return func(m *Manager) {
		if r != nil {
			m.iptablesRunner = r
		}
	}
}

// WithIptablesTakeover acknowledges another iptables manager may flush
// Falak's rules. Rules are still installed; an INFO log notes the risk.
// Maps to the network.iptables_takeover config key.
func WithIptablesTakeover(enabled bool) ManagerOption {
	return func(m *Manager) { m.takeover = enabled }
}

// WithBridgeMTU overrides the default 1450 bridge MTU.
func WithBridgeMTU(mtu int) ManagerOption {
	return func(m *Manager) {
		if mtu > 0 {
			m.mtu = mtu
		}
	}
}

// NewManager constructs a Manager; errors when Podman client or Allocator
// are missing.
func NewManager(opts ...ManagerOption) (*Manager, error) {
	m := &Manager{
		iptablesRunner:   execRunner{},
		logger:           zap.NewNop(),
		mtu:              defaultBridgeMTU,
		destroyAttempts:  defaultDestroyAttempts,
		destroyBaseDelay: defaultDestroyBaseDelay,
		dangling:         make(map[string]struct{}),
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.podman == nil {
		return nil, errors.New("bridge: WithPodmanClient required")
	}
	if m.allocator == nil {
		return nil, errors.New("bridge: WithAllocator required")
	}
	return m, nil
}

// Create provisions the per-group bridge atomically: allocate a /24,
// create the Podman network, install the three FILTER-table isolation
// rules. On any failure later steps roll back so there is never a window
// where a bridge exists without isolation rules (the 11A.15b atomic
// install requirement).
func (m *Manager) Create(ctx context.Context, groupID string) (*BridgeInfo, error) {
	if groupID == "" {
		return nil, errors.New("bridge: empty groupID")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	alloc, err := m.allocator.Allocate(groupID)
	if err != nil {
		return nil, fmt.Errorf("bridge: allocate subnet: %w", err)
	}
	info := &BridgeInfo{
		GroupID: groupID, Name: bridgeNamePrefix + groupID,
		Subnet: alloc.Subnet, Gateway: alloc.Gateway, MTU: m.mtu,
	}
	if err := m.podman.CreateNetwork(ctx, info.Name, info.Subnet.String(), info.Gateway.String(), info.MTU); err != nil {
		m.releaseQuiet(groupID)
		return nil, fmt.Errorf("bridge: podman create %s: %w", info.Name, err)
	}
	if err := m.installIsolationRules(ctx, info.Name); err != nil {
		if delErr := m.podman.DeleteNetwork(ctx, info.Name); delErr != nil && !errors.Is(delErr, ErrPodmanNetworkNotFound) {
			m.logger.Error("bridge: rollback podman delete failed",
				zap.String("group", groupID), zap.String("bridge", info.Name), zap.Error(delErr))
		}
		m.releaseQuiet(groupID)
		return nil, fmt.Errorf("bridge: install isolation rules for %s: %w", info.Name, err)
	}
	if m.takeover {
		m.logger.Info("bridge: iptables_takeover enabled; another manager may flush rules",
			zap.String("group", groupID), zap.String("bridge", info.Name))
	}
	m.logger.Info("bridge created",
		zap.String("group", groupID), zap.String("bridge", info.Name),
		zap.String("subnet", info.Subnet.String()), zap.String("gateway", info.Gateway.String()),
		zap.Int("mtu", info.MTU))
	return info, nil
}

// Destroy tears down the per-group bridge in reverse order of Create:
// iptables rules, Podman network, subnet release. The Podman delete is
// wrapped in exponential-backoff retry (see teardown.go) because Podman
// holds a transient reference for ~1s after the last container exits.
// Reaper-idempotent — unknown group, Podman "not found", iptables
// rule-not-found are all success (IMPLEMENTATION_RULES §7).
func (m *Manager) Destroy(ctx context.Context, groupID string) error {
	if groupID == "" {
		return errors.New("bridge: empty groupID")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	name := bridgeNamePrefix + groupID
	alloc, known, err := m.allocator.Get(groupID)
	if err != nil {
		return fmt.Errorf("bridge: lookup allocation: %w", err)
	}
	if !known {
		if rmErr := m.removeIsolationRules(ctx, name); rmErr != nil && !isIptablesNotFound(rmErr) {
			m.logger.Warn("bridge: iptables remove on unknown group",
				zap.String("group", groupID), zap.Error(rmErr))
		}
		if delErr := m.podman.DeleteNetwork(ctx, name); delErr != nil && !errors.Is(delErr, ErrPodmanNetworkNotFound) {
			m.logger.Warn("bridge: podman delete on unknown group",
				zap.String("group", groupID), zap.Error(delErr))
		}
		m.logger.Debug("bridge: destroy on unknown group is a no-op", zap.String("group", groupID))
		return nil
	}
	return m.destroyWithRetry(ctx, groupID, name, alloc)
}

// Inspect returns the BridgeInfo for groupID, combining the allocator's
// IPAM with the Podman network's MTU. Returns ErrBridgeNotFound when
// neither side knows about the group.
func (m *Manager) Inspect(ctx context.Context, groupID string) (*BridgeInfo, error) {
	if groupID == "" {
		return nil, errors.New("bridge: empty groupID")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	alloc, known, err := m.allocator.Get(groupID)
	if err != nil {
		return nil, fmt.Errorf("bridge: lookup allocation: %w", err)
	}
	name := bridgeNamePrefix + groupID
	if !known {
		pnet, perr := m.podman.InspectNetwork(ctx, name)
		if errors.Is(perr, ErrPodmanNetworkNotFound) {
			return nil, ErrBridgeNotFound
		}
		if perr != nil {
			return nil, fmt.Errorf("bridge: podman inspect %s: %w", name, perr)
		}
		_, subnet, _ := net.ParseCIDR(pnet.Subnet)
		gw := net.ParseIP(pnet.Gateway)
		if v4 := gw.To4(); v4 != nil {
			gw = v4
		}
		return &BridgeInfo{GroupID: groupID, Name: name, Subnet: subnet, Gateway: gw, MTU: pnet.MTU}, nil
	}
	info := &BridgeInfo{
		GroupID: groupID, Name: name,
		Subnet: alloc.Subnet, Gateway: alloc.Gateway, MTU: m.mtu,
	}
	if pnet, perr := m.podman.InspectNetwork(ctx, name); perr == nil && pnet.MTU > 0 {
		info.MTU = pnet.MTU
	}
	return info, nil
}

func (m *Manager) releaseQuiet(groupID string) {
	if err := m.allocator.Release(groupID); err != nil {
		m.logger.Error("bridge: rollback subnet release failed",
			zap.String("group", groupID), zap.Error(err))
	}
}

// installIsolationRules adds the three FORWARD rules with rollback on
// first failure.
func (m *Manager) installIsolationRules(ctx context.Context, bridge string) error {
	rules := isolationRules(bridge)
	for i, r := range rules {
		if err := m.iptablesRunner.Run(ctx, "iptables", append([]string{"-I"}, r...)...); err != nil {
			for j := i - 1; j >= 0; j-- {
				if rbErr := m.iptablesRunner.Run(ctx, "iptables", append([]string{"-D"}, rules[j]...)...); rbErr != nil {
					m.logger.Error("bridge: iptables rollback failed",
						zap.String("bridge", bridge), zap.Strings("rule", rules[j]), zap.Error(rbErr))
				}
			}
			return err
		}
	}
	return nil
}

// removeIsolationRules deletes the three FORWARD rules in reverse order.
// rule-not-found errors are demoted to WARN (idempotent).
func (m *Manager) removeIsolationRules(ctx context.Context, bridge string) error {
	rules := isolationRules(bridge)
	for i := len(rules) - 1; i >= 0; i-- {
		if err := m.iptablesRunner.Run(ctx, "iptables", append([]string{"-D"}, rules[i]...)...); err != nil {
			if isIptablesNotFound(err) {
				m.logger.Warn("bridge: iptables rule already absent",
					zap.String("bridge", bridge), zap.Strings("rule", rules[i]))
				continue
			}
			return err
		}
	}
	return nil
}

// isolationRules returns the three FORWARD-table rule argument lists for
// bridge in install order.
func isolationRules(bridge string) [][]string {
	return [][]string{
		{"FORWARD", "-i", bridge, "-o", bridge, "-j", "ACCEPT"},
		{"FORWARD", "-i", bridge, "!", "-o", bridge, "-j", "DROP"},
		{"FORWARD", "!", "-i", bridge, "-o", bridge, "-j", "DROP"},
	}
}

// isIptablesNotFound detects "rule does not exist" outcomes from iptables.
func isIptablesNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrIptablesRuleNotFound) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "does a matching rule exist") ||
		strings.Contains(msg, "No chain/target/match") ||
		strings.Contains(msg, "Bad rule")
}
