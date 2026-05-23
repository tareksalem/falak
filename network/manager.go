// Package network owns the per-cluster network subsystem: bridge
// lifecycle, overlay peers, endpoint gossip, DNS, and the startup gates
// that fail-fast on hostile host state. Manager is the top-level
// orchestrator — one per cluster, owned by the node.
//
// Wiring sequence on Start (plan 11A.14):
//  1. startup.Gates.Verify — rp_filter + iptables-manager checks.
//  2. bridge.Manager.ReapDangling — sweep leaked bridges from a prior run.
//  3. dns.Server.Start — empty listener set; bridges add their own.
//  4. Subscribe to capsule lifecycle events via the supplied EventSource;
//     events drive ref-counted bridge provisioning (11A.3) and per-member
//     endpoint publish/withdraw.
//
// Stop drains the subscriptions and cascades to every subsystem.
package network

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/network/bridge"
	"github.com/tareksalem/falak/network/dns"
	"github.com/tareksalem/falak/network/endpoints"
	"github.com/tareksalem/falak/network/overlay"
	"github.com/tareksalem/falak/network/startup"

	endpointpb "github.com/tareksalem/falak/network/proto/endpointpb"
)

// CapsuleEvent is the narrow projection of a capsule lifecycle event the
// network manager consumes. Decoupled from the capsule package so the
// network module doesn't depend on capsule internals; the node-side
// adapter (11A.15) translates capsule.Capsule into this shape.
type CapsuleEvent struct {
	ClusterPath string
	GroupID     string
	IsGroup     bool
	CapsuleName string
	ReplicaID   string
	NodeID      string
	NodeIP      string
	BridgeIP    string
	NamedPorts  []endpoints.NamedPort
	SwimState   string
}

// EventSource is the narrow seam the manager subscribes through.
// Implementations dispatch each callback once per event, serial per
// (group, replica). Tests use an in-memory implementation; production
// wires this to the capsule package's event bus in 11A.15.
type EventSource interface {
	OnCapsuleReceived(fn func(CapsuleEvent)) (cancel func())
	OnCapsuleRunning(fn func(CapsuleEvent)) (cancel func())
	OnCapsuleStopped(fn func(CapsuleEvent)) (cancel func())
	OnCapsuleDeleted(fn func(CapsuleEvent)) (cancel func())
	OnPeerJoined(fn func(CapsuleEvent)) (cancel func())
	OnPeerLeft(fn func(CapsuleEvent)) (cancel func())
}

// Sentinel errors returned by Manager. Match via errors.Is.
var (
	ErrAlreadyStarted = errors.New("network: manager already started")
	ErrNotStarted     = errors.New("network: manager not started")
)

// Manager is the top-level network orchestrator for one cluster. It owns
// the bridge lifecycle ref-count map, the overlay peer manager, DNS
// listeners, and endpoint pub/sub. One per cluster.
type Manager struct {
	bridgeMgr        *bridge.Manager
	subnetAlloc      *bridge.Allocator
	peers            *overlay.PeerManager
	vniAlloc         *overlay.VNIAllocator
	dnsServer        *dns.Server
	registry         *endpoints.Registry
	subscriber       *endpoints.Subscriber
	publisher        *endpoints.Publisher
	gates            *startup.Gates
	mtuResolver      overlay.MTUResolver
	eventSource      EventSource
	logger           *zap.Logger
	clusterPath      string
	localNodeID      string
	localIP          string
	listenAddrLookup func(*bridge.BridgeInfo) (netip.Addr, error)

	mu        sync.Mutex
	started   bool
	stopped   bool
	groups    map[string]*groupRefState // groupID -> ref state
	cancelers []func()

	listenersMu sync.RWMutex
	listeners   []BridgeListener
}

// groupRefState carries the per-group lifecycle bookkeeping: how many
// members are running locally + the resolved bridge info needed by the
// teardown path.
type groupRefState struct {
	refCount   int
	bridgeInfo *bridge.BridgeInfo
}

// Option configures a Manager.
type Option func(*Manager)

// WithBridgeManager wires the bridge.Manager. Required.
func WithBridgeManager(m *bridge.Manager) Option { return func(x *Manager) { x.bridgeMgr = m } }

// WithSubnetAllocator wires the per-bridge IPAM. Required.
func WithSubnetAllocator(a *bridge.Allocator) Option { return func(x *Manager) { x.subnetAlloc = a } }

// WithPeerManager wires the overlay peer manager. Required.
func WithPeerManager(p *overlay.PeerManager) Option { return func(x *Manager) { x.peers = p } }

// WithVNIAllocator wires the overlay VNI allocator. Required.
func WithVNIAllocator(v *overlay.VNIAllocator) Option { return func(x *Manager) { x.vniAlloc = v } }

// WithDNSServer wires the DNS responder. Required.
func WithDNSServer(s *dns.Server) Option { return func(x *Manager) { x.dnsServer = s } }

// WithEndpointRegistry wires the in-memory endpoint mirror. Required.
func WithEndpointRegistry(r *endpoints.Registry) Option {
	return func(x *Manager) { x.registry = r }
}

// WithEndpointSubscriber wires the gossip subscriber. Required.
func WithEndpointSubscriber(s *endpoints.Subscriber) Option {
	return func(x *Manager) { x.subscriber = s }
}

// WithEndpointPublisher wires the gossip publisher. Required.
func WithEndpointPublisher(p *endpoints.Publisher) Option {
	return func(x *Manager) { x.publisher = p }
}

// WithStartupGates wires the host-level pre-flight gates. Required.
func WithStartupGates(g *startup.Gates) Option { return func(x *Manager) { x.gates = g } }

// WithMTUResolver overrides the default MTU resolver (optional).
func WithMTUResolver(r overlay.MTUResolver) Option {
	return func(x *Manager) {
		if r != nil {
			x.mtuResolver = r
		}
	}
}

// WithEventSource wires the lifecycle event seam. Required.
func WithEventSource(e EventSource) Option { return func(x *Manager) { x.eventSource = e } }

// WithLogger replaces the default no-op logger.
func WithLogger(l *zap.Logger) Option {
	return func(x *Manager) {
		if l != nil {
			x.logger = l
		}
	}
}

// WithClusterPath sets the cluster identity. Required.
func WithClusterPath(p string) Option { return func(x *Manager) { x.clusterPath = p } }

// WithLocalNodeID sets the local node's stable ID. Required.
func WithLocalNodeID(id string) Option { return func(x *Manager) { x.localNodeID = id } }

// WithLocalIP sets the local node's underlay IP. Required.
func WithLocalIP(ip string) Option { return func(x *Manager) { x.localIP = ip } }

// WithDNSListenAddrLookup overrides the function that maps a BridgeInfo
// to the listen address used when adding a DNS listener for that bridge.
// Production wiring uses the default (always dns.LinkLocalDNSAddr per
// plan 11A.13); tests can pin to 127.0.0.1.
func WithDNSListenAddrLookup(fn func(*bridge.BridgeInfo) (netip.Addr, error)) Option {
	return func(x *Manager) {
		if fn != nil {
			x.listenAddrLookup = fn
		}
	}
}

// New constructs a Manager from required options. Returns a descriptive
// error when any required dependency is missing.
func New(opts ...Option) (*Manager, error) {
	m := &Manager{
		logger:           zap.NewNop(),
		groups:           make(map[string]*groupRefState),
		listenAddrLookup: defaultBridgeListenAddr,
	}
	for _, opt := range opts {
		opt(m)
	}
	switch {
	case m.bridgeMgr == nil:
		return nil, errors.New("network: WithBridgeManager required")
	case m.subnetAlloc == nil:
		return nil, errors.New("network: WithSubnetAllocator required")
	case m.peers == nil:
		return nil, errors.New("network: WithPeerManager required")
	case m.vniAlloc == nil:
		return nil, errors.New("network: WithVNIAllocator required")
	case m.dnsServer == nil:
		return nil, errors.New("network: WithDNSServer required")
	case m.registry == nil:
		return nil, errors.New("network: WithEndpointRegistry required")
	case m.subscriber == nil:
		return nil, errors.New("network: WithEndpointSubscriber required")
	case m.publisher == nil:
		return nil, errors.New("network: WithEndpointPublisher required")
	case m.gates == nil:
		return nil, errors.New("network: WithStartupGates required")
	case m.eventSource == nil:
		return nil, errors.New("network: WithEventSource required")
	case m.clusterPath == "":
		return nil, errors.New("network: WithClusterPath required")
	case m.localNodeID == "":
		return nil, errors.New("network: WithLocalNodeID required")
	case m.localIP == "":
		return nil, errors.New("network: WithLocalIP required")
	}
	return m, nil
}

// Start verifies the host gates, reaps leaked bridges, opens the DNS
// listeners, and wires lifecycle subscriptions. Returns ErrAlreadyStarted
// on a second call.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return ErrAlreadyStarted
	}
	if m.stopped {
		m.mu.Unlock()
		return fmt.Errorf("network: manager already stopped")
	}
	m.started = true
	m.mu.Unlock()

	if err := m.gates.Verify(); err != nil {
		return fmt.Errorf("network: startup gates failed: %w", err)
	}
	if err := m.bridgeMgr.ReapDangling(ctx); err != nil {
		m.logger.Warn("network: reap dangling on start", zap.Error(err))
	}
	if err := m.dnsServer.Start(ctx); err != nil {
		return fmt.Errorf("network: start dns: %w", err)
	}
	m.subscribeEvents()
	m.logger.Info("network manager started",
		zap.String("cluster", m.clusterPath), zap.String("node", m.localNodeID))
	return nil
}

// Stop drains subscriptions and shuts down every subsystem. Idempotent.
func (m *Manager) Stop() error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return ErrNotStarted
	}
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	m.stopped = true
	cancels := m.cancelers
	m.cancelers = nil
	m.mu.Unlock()

	for _, c := range cancels {
		c()
	}
	if err := m.dnsServer.Stop(); err != nil {
		m.logger.Warn("network: dns stop", zap.Error(err))
	}
	m.subscriber.Stop()
	m.publisher.Stop()
	if err := m.peers.Stop(); err != nil {
		m.logger.Warn("network: peer manager stop", zap.Error(err))
	}
	m.logger.Info("network manager stopped", zap.String("cluster", m.clusterPath))
	return nil
}

// subscribeEvents wires every callback the manager cares about. The
// subscriptions are cancelled in Stop.
func (m *Manager) subscribeEvents() {
	src := m.eventSource
	c := []func(){
		src.OnCapsuleReceived(m.onCapsuleReceived),
		src.OnCapsuleRunning(m.onCapsuleRunning),
		src.OnCapsuleStopped(m.onCapsuleStopped),
		src.OnCapsuleDeleted(m.onCapsuleDeleted),
		src.OnPeerJoined(m.onPeerJoined),
		src.OnPeerLeft(m.onPeerLeft),
	}
	m.mu.Lock()
	m.cancelers = append(m.cancelers, c...)
	m.mu.Unlock()
}

// onCapsuleReceived: for Kind=Group, no immediate action — the bridge is
// provisioned lazily when the first member runs.
func (m *Manager) onCapsuleReceived(ev CapsuleEvent) {
	if !ev.IsGroup {
		return
	}
	m.logger.Debug("network: group received; bridge deferred to first member",
		zap.String("cluster", ev.ClusterPath), zap.String("group", ev.GroupID))
}

// onCapsuleRunning: increment ref, provision on 0→1, publish record.
func (m *Manager) onCapsuleRunning(ev CapsuleEvent) {
	if ev.IsGroup {
		return
	}
	if err := m.ensureGroup(ev.ClusterPath, ev.GroupID); err != nil {
		m.logger.Error("network: ensure group on member start failed",
			zap.String("group", ev.GroupID), zap.String("replica", ev.ReplicaID),
			zap.Error(err))
		return
	}
	rec := buildEndpointRecord(ev)
	if err := m.publisher.Publish(context.Background(), rec); err != nil {
		m.logger.Error("network: publish endpoint on running failed",
			zap.String("replica", ev.ReplicaID), zap.Error(err))
		return
	}
	m.registry.Insert(rec)
}

// onCapsuleStopped: withdraw the record, decrement ref, tear down on 1→0.
func (m *Manager) onCapsuleStopped(ev CapsuleEvent) {
	if ev.IsGroup {
		return
	}
	if err := m.publisher.Withdraw(context.Background(), ev.ClusterPath, ev.GroupID, ev.CapsuleName, ev.ReplicaID); err != nil {
		m.logger.Warn("network: withdraw endpoint on stop failed",
			zap.String("replica", ev.ReplicaID), zap.Error(err))
	}
	m.registry.WithdrawKey(ev.ClusterPath, ev.GroupID, ev.CapsuleName, ev.ReplicaID)
	if err := m.releaseGroup(ev.ClusterPath, ev.GroupID); err != nil {
		m.logger.Error("network: release group on member stop failed",
			zap.String("group", ev.GroupID), zap.Error(err))
	}
}

// onCapsuleDeleted: forced teardown of a group's bridge regardless of
// ref count. Per-member records are evicted via gossip TTL.
func (m *Manager) onCapsuleDeleted(ev CapsuleEvent) {
	if !ev.IsGroup {
		return
	}
	m.mu.Lock()
	st, ok := m.groups[ev.GroupID]
	if ok {
		delete(m.groups, ev.GroupID)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	m.teardownGroup(ev.ClusterPath, ev.GroupID, st.bridgeInfo)
}

// onPeerJoined: ensure overlay peer state for a peer that started
// hosting members of a shared group.
func (m *Manager) onPeerJoined(ev CapsuleEvent) {
	if ev.NodeID == "" || ev.NodeID == m.localNodeID {
		return
	}
	if err := m.peers.EnsurePeer(context.Background(), ev.ClusterPath, ev.GroupID, ev.NodeID, ev.NodeIP); err != nil {
		m.logger.Error("network: ensure peer failed",
			zap.String("group", ev.GroupID), zap.String("peer", ev.NodeID),
			zap.Error(err))
	}
}

// onPeerLeft: schedule peer teardown (overlay.PeerManager applies the
// flap-grace delay before committing).
func (m *Manager) onPeerLeft(ev CapsuleEvent) {
	if ev.NodeID == "" || ev.NodeID == m.localNodeID {
		return
	}
	if err := m.peers.RemovePeer(context.Background(), ev.ClusterPath, ev.GroupID, ev.NodeID); err != nil {
		m.logger.Error("network: remove peer failed",
			zap.String("group", ev.GroupID), zap.String("peer", ev.NodeID),
			zap.Error(err))
	}
}

// ensureGroup increments the ref count and provisions on the 0→1
// transition: bridge create + overlay group + DNS listener + endpoint
// subscribe. Any provisioning step that fails after bridge create rolls
// back the bridge to keep IPAM consistent.
func (m *Manager) ensureGroup(clusterPath, groupID string) error {
	m.mu.Lock()
	st, ok := m.groups[groupID]
	if ok {
		st.refCount++
		count := st.refCount
		m.mu.Unlock()
		m.logger.Debug("network: group ref incremented",
			zap.String("group", groupID), zap.Int("count", count))
		return nil
	}
	st = &groupRefState{refCount: 1}
	m.groups[groupID] = st
	m.mu.Unlock()

	ctx := context.Background()
	info, err := m.bridgeMgr.Create(ctx, groupID)
	if err != nil {
		m.unwindRef(groupID)
		return fmt.Errorf("create bridge: %w", err)
	}
	m.mu.Lock()
	st.bridgeInfo = info
	m.mu.Unlock()
	if err := m.peers.EnsureGroup(ctx, clusterPath, groupID, info.Name); err != nil {
		m.logger.Error("network: ensure overlay group failed; rolling back bridge",
			zap.String("group", groupID), zap.Error(err))
		_ = m.bridgeMgr.Destroy(ctx, groupID)
		m.unwindRef(groupID)
		return fmt.Errorf("ensure overlay group: %w", err)
	}
	if err := m.addDNSListener(ctx, info); err != nil {
		m.logger.Error("network: add dns listener failed",
			zap.String("group", groupID), zap.Error(err))
	}
	if err := m.subscriber.Subscribe(ctx, clusterPath, groupID); err != nil {
		m.logger.Error("network: subscribe endpoints failed",
			zap.String("group", groupID), zap.Error(err))
	}
	m.logger.Info("network: group provisioned",
		zap.String("cluster", clusterPath), zap.String("group", groupID),
		zap.String("bridge", info.Name), zap.String("gateway", info.Gateway.String()))
	if addr, ok := bridgeGatewayAddr(info); ok {
		m.notifyBridgeAdded(addr)
	}
	return nil
}

// releaseGroup decrements the ref count and tears down on the 1→0
// transition. Unknown groups are no-ops.
func (m *Manager) releaseGroup(clusterPath, groupID string) error {
	m.mu.Lock()
	st, ok := m.groups[groupID]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	st.refCount--
	if st.refCount > 0 {
		count := st.refCount
		m.mu.Unlock()
		m.logger.Debug("network: group ref decremented",
			zap.String("group", groupID), zap.Int("count", count))
		return nil
	}
	delete(m.groups, groupID)
	info := st.bridgeInfo
	m.mu.Unlock()
	m.teardownGroup(clusterPath, groupID, info)
	return nil
}

// teardownGroup performs the 1→0 / forced-delete steps. Errors are
// logged but not propagated — every step is independently best-effort
// and reaper-idempotent.
func (m *Manager) teardownGroup(clusterPath, groupID string, info *bridge.BridgeInfo) {
	ctx := context.Background()
	if err := m.subscriber.Unsubscribe(ctx, clusterPath, groupID); err != nil {
		m.logger.Warn("network: unsubscribe endpoints failed",
			zap.String("group", groupID), zap.Error(err))
	}
	if info != nil {
		m.removeDNSListener(info)
	}
	if err := m.peers.RemoveGroup(ctx, clusterPath, groupID); err != nil {
		m.logger.Warn("network: remove overlay group failed",
			zap.String("group", groupID), zap.Error(err))
	}
	if err := m.bridgeMgr.Destroy(ctx, groupID); err != nil {
		m.logger.Error("network: destroy bridge failed",
			zap.String("group", groupID), zap.Error(err))
	}
	m.logger.Info("network: group torn down",
		zap.String("cluster", clusterPath), zap.String("group", groupID))
	if addr, ok := bridgeGatewayAddr(info); ok {
		m.notifyBridgeRemoved(addr)
	}
}

// unwindRef removes a half-provisioned ref entry inserted by ensureGroup
// when a downstream step failed.
func (m *Manager) unwindRef(groupID string) {
	m.mu.Lock()
	delete(m.groups, groupID)
	m.mu.Unlock()
}

// addDNSListener adds a per-bridge listener at the configured listen
// address (default: link-local 169.254.169.250).
func (m *Manager) addDNSListener(ctx context.Context, info *bridge.BridgeInfo) error {
	addr, err := m.listenAddrLookup(info)
	if err != nil {
		return err
	}
	return m.dnsServer.AddListener(ctx, addr)
}

// removeDNSListener removes the per-bridge listener.
func (m *Manager) removeDNSListener(info *bridge.BridgeInfo) {
	addr, err := m.listenAddrLookup(info)
	if err != nil {
		m.logger.Warn("network: dns listener removal addr parse",
			zap.String("bridge", info.Name), zap.Error(err))
		return
	}
	if err := m.dnsServer.RemoveListener(addr); err != nil {
		m.logger.Warn("network: dns remove listener", zap.Error(err))
	}
}

// defaultBridgeListenAddr returns the link-local listen address. 11A.13
// pins this at 169.254.169.250 per bridge so resolv.conf survives CRIU
// restore unchanged.
func defaultBridgeListenAddr(info *bridge.BridgeInfo) (netip.Addr, error) {
	if info == nil {
		return netip.Addr{}, errors.New("network: nil bridge info")
	}
	addr, err := netip.ParseAddr(dns.LinkLocalDNSAddr)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parse link-local: %w", err)
	}
	return addr, nil
}

// buildEndpointRecord projects a CapsuleEvent into the wire record used
// by the gossip publisher.
func buildEndpointRecord(ev CapsuleEvent) *endpointpb.EndpointRecord {
	rec := &endpointpb.EndpointRecord{
		ClusterPath: ev.ClusterPath, GroupId: ev.GroupID,
		CapsuleName: ev.CapsuleName, ReplicaId: ev.ReplicaID,
		NodeId: ev.NodeID, BridgeIp: ev.BridgeIP,
		SwimState: strings.ToLower(ev.SwimState),
	}
	for _, np := range ev.NamedPorts {
		rec.NamedPorts = append(rec.NamedPorts, &endpointpb.NamedPort{
			Name: np.Name, ContainerPort: np.ContainerPort, Protocol: np.Protocol,
		})
	}
	return rec
}
