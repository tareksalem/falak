package overlay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// defaultFlapGrace is the deferred-teardown window. 30s covers SWIM
// gossip jitter so a brief membership flap doesn't churn the tunnel.
const defaultFlapGrace = 30 * time.Second

// defaultOverlayPort is the wire UDP port (RFC 7348 §4.1).
const defaultOverlayPort = DefaultVXLANPort

// teardownTimeout bounds the kernel calls run from the grace-timer
// goroutine after Stop has cancelled their parent context.
const teardownTimeout = 10 * time.Second

// ErrPeerManagerStopped is returned by every public method after Stop.
var ErrPeerManagerStopped = errors.New("overlay: peer manager stopped")

// MTUResolver hands the peer manager the bridge-side MTU. Production
// wraps overlay.Plan from mtu.go; tests inject a fake.
type MTUResolver interface{ BridgeMTU() int }

// MTUResolverFunc adapts a plain func to MTUResolver.
type MTUResolverFunc func() int

// BridgeMTU calls the underlying func.
func (f MTUResolverFunc) BridgeMTU() int { return f() }

// peerStateKind is the lifecycle of one peer entry.
type peerStateKind uint8

const (
	peerStateActive peerStateKind = iota + 1
	peerStateLeaving
)

// peerState tracks one (group, peer) pair. cancel is the flap-grace
// timer's cancellation hook; nil means no removal is pending.
type peerState struct {
	externalIP string
	kind       peerStateKind
	cancel     context.CancelFunc
}

// groupKey is the map key for the manager's group table.
type groupKey struct{ clusterPath, groupID string }

// groupState owns the per-group VXLAN device + peer set. The mutex
// serializes EnsurePeer / RemovePeer within one group; different
// groups never block each other.
type groupState struct {
	vni        uint32
	deviceName string
	bridgeName string
	peers      map[string]*peerState
	mu         sync.Mutex
}

// PeerManager owns per-group VXLAN devices + per-pair IPsec SAs for a
// single node. One per node.
//
// Concurrency: top-level mu guards the groups map only; per-group mu
// serializes peer-level operations. Flap-grace timers run as tracked
// goroutines under wg + stopCtx.
type PeerManager struct {
	vniAlloc    *VNIAllocator
	dev         DeviceManager
	ipsec       IPsecManager
	keys        *KeyManager
	mtu         MTUResolver
	localNodeID string
	localIP     string
	port        int
	flapGrace   time.Duration
	logger      *zap.Logger

	mu     sync.Mutex
	groups map[groupKey]*groupState

	stopCtx    context.Context
	stopCancel context.CancelFunc
	wg         sync.WaitGroup
	stopped    bool
}

// PeerOption configures a PeerManager.
type PeerOption func(*PeerManager)

// WithVNIAllocator wires the VNI allocator. Required.
func WithVNIAllocator(a *VNIAllocator) PeerOption { return func(p *PeerManager) { p.vniAlloc = a } }

// WithDeviceManager wires the VXLAN DeviceManager. Required.
func WithDeviceManager(d DeviceManager) PeerOption { return func(p *PeerManager) { p.dev = d } }

// WithIPsecManager wires the IPsecManager. Required.
func WithIPsecManager(i IPsecManager) PeerOption { return func(p *PeerManager) { p.ipsec = i } }

// WithKeyManager wires the KeyManager. Required.
func WithKeyManager(k *KeyManager) PeerOption { return func(p *PeerManager) { p.keys = k } }

// WithLocalNodeID supplies this node's stable identity. Required.
func WithLocalNodeID(id string) PeerOption { return func(p *PeerManager) { p.localNodeID = id } }

// WithLocalIP supplies this node's underlay IP. Required.
func WithLocalIP(ip string) PeerOption { return func(p *PeerManager) { p.localIP = ip } }

// WithMTUResolver supplies the bridge-MTU lookup. Required.
func WithMTUResolver(r MTUResolver) PeerOption { return func(p *PeerManager) { p.mtu = r } }

// WithPeerManagerLogger replaces the default no-op logger. Nil ignored.
func WithPeerManagerLogger(l *zap.Logger) PeerOption {
	return func(p *PeerManager) {
		if l != nil {
			p.logger = l
		}
	}
}

// WithFlapGrace sets the deferred-remove window. Non-positive ignored.
func WithFlapGrace(d time.Duration) PeerOption {
	return func(p *PeerManager) {
		if d > 0 {
			p.flapGrace = d
		}
	}
}

// WithOverlayPort overrides the UDP port (test hook).
func WithOverlayPort(port int) PeerOption {
	return func(p *PeerManager) {
		if port > 0 {
			p.port = port
		}
	}
}

// NewPeerManager validates required options and returns a manager
// ready for EnsureGroup. Stop() must be called to release timers.
func NewPeerManager(opts ...PeerOption) (*PeerManager, error) {
	p := &PeerManager{
		logger:    zap.NewNop(),
		groups:    map[groupKey]*groupState{},
		flapGrace: defaultFlapGrace,
		port:      defaultOverlayPort,
	}
	for _, opt := range opts {
		opt(p)
	}
	switch {
	case p.vniAlloc == nil:
		return nil, errors.New("overlay: WithVNIAllocator required")
	case p.dev == nil:
		return nil, errors.New("overlay: WithDeviceManager required")
	case p.ipsec == nil:
		return nil, errors.New("overlay: WithIPsecManager required")
	case p.keys == nil:
		return nil, errors.New("overlay: WithKeyManager required")
	case p.mtu == nil:
		return nil, errors.New("overlay: WithMTUResolver required")
	case p.localNodeID == "":
		return nil, errors.New("overlay: WithLocalNodeID required")
	case p.localIP == "":
		return nil, errors.New("overlay: WithLocalIP required")
	}
	p.stopCtx, p.stopCancel = context.WithCancel(context.Background())
	return p, nil
}

// EnsureGroup is idempotent: first call allocates a VNI, creates the
// VXLAN device, tracks the group. Subsequent calls return nil.
func (p *PeerManager) EnsureGroup(ctx context.Context, clusterPath, groupID, bridgeName string) error {
	if err := p.checkRunning(); err != nil {
		return err
	}
	if clusterPath == "" || groupID == "" || bridgeName == "" {
		return errors.New("overlay: empty clusterPath, groupID or bridgeName")
	}
	key := groupKey{clusterPath, groupID}
	p.mu.Lock()
	if _, ok := p.groups[key]; ok {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	vni, err := p.vniAlloc.Allocate(clusterPath, groupID)
	if err != nil {
		return fmt.Errorf("overlay: allocate vni: %w", err)
	}
	mtu := p.mtu.BridgeMTU()
	devName, err := p.dev.Create(ctx, bridgeName, vni, p.localIP, p.port, mtu)
	if err != nil {
		_ = p.vniAlloc.Release(clusterPath, groupID)
		return fmt.Errorf("overlay: create vxlan device: %w", err)
	}
	p.mu.Lock()
	if existing, ok := p.groups[key]; ok {
		p.mu.Unlock()
		_ = p.dev.Destroy(ctx, devName)
		p.logger.Debug("ensure-group collapsed with concurrent caller",
			zap.String("cluster", clusterPath), zap.String("group", groupID),
			zap.String("device", existing.deviceName))
		return nil
	}
	p.groups[key] = &groupState{
		vni: vni, deviceName: devName, bridgeName: bridgeName,
		peers: map[string]*peerState{},
	}
	p.mu.Unlock()
	p.logger.Info("overlay group ensured",
		zap.String("cluster", clusterPath), zap.String("group", groupID),
		zap.Uint32("vni", vni), zap.String("device", devName),
		zap.String("bridge", bridgeName), zap.Int("mtu", mtu))
	return nil
}

// EnsurePeer is idempotent: first call derives the key, installs the
// SA, and adds the FDB entry. A peer mid-flap-grace flips back to
// active and the pending timer is cancelled.
func (p *PeerManager) EnsurePeer(ctx context.Context, clusterPath, groupID, peerNodeID, peerIP string) error {
	if err := p.checkRunning(); err != nil {
		return err
	}
	if peerNodeID == "" || peerIP == "" {
		return errors.New("overlay: empty peerNodeID or peerIP")
	}
	g, err := p.lookupGroup(clusterPath, groupID)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if existing, ok := g.peers[peerNodeID]; ok {
		if existing.kind == peerStateLeaving {
			if existing.cancel != nil {
				existing.cancel()
				existing.cancel = nil
			}
			existing.kind = peerStateActive
			p.logger.Info("overlay peer flap-cancel",
				zap.String("group", groupID), zap.String("peer", peerNodeID))
		}
		return nil
	}
	key, err := p.keys.DerivePairKey(groupID, p.localNodeID, peerNodeID)
	if err != nil {
		return fmt.Errorf("overlay: derive pair key: %w", err)
	}
	if err := p.ipsec.InstallSA(ctx, p.localIP, peerIP, p.port, key); err != nil {
		return fmt.Errorf("overlay: install sa: %w", err)
	}
	if err := p.dev.AddFDBEntry(ctx, g.deviceName, peerIP); err != nil {
		_ = p.ipsec.RemoveSA(ctx, p.localIP, peerIP, p.port)
		return fmt.Errorf("overlay: add fdb entry: %w", err)
	}
	g.peers[peerNodeID] = &peerState{externalIP: peerIP, kind: peerStateActive}
	p.logger.Info("overlay peer ensured",
		zap.String("group", groupID), zap.String("peer", peerNodeID),
		zap.String("peer_ip", peerIP), zap.String("device", g.deviceName))
	return nil
}

// RemovePeer schedules a deferred teardown after flapGrace. A
// concurrent EnsurePeer within the window cancels the timer.
func (p *PeerManager) RemovePeer(ctx context.Context, clusterPath, groupID, peerNodeID string) error {
	if err := p.checkRunning(); err != nil {
		return err
	}
	g, err := p.lookupGroup(clusterPath, groupID)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.peers[peerNodeID]
	if !ok || st.kind == peerStateLeaving {
		return nil
	}
	st.kind = peerStateLeaving
	graceCtx, cancel := context.WithCancel(p.stopCtx)
	st.cancel = cancel
	p.wg.Add(1)
	go p.flapGraceWorker(graceCtx, groupID, peerNodeID, st.externalIP, g)
	p.logger.Info("overlay peer remove scheduled",
		zap.String("group", groupID), zap.String("peer", peerNodeID),
		zap.Duration("grace", p.flapGrace))
	return nil
}

// flapGraceWorker waits flapGrace, then commits the teardown if the
// peer is still in the leaving state. Stop's stopCtx cancellation
// short-circuits the wait so shutdown isn't held up.
func (p *PeerManager) flapGraceWorker(ctx context.Context, groupID, peerNodeID, peerIP string, g *groupState) {
	defer p.wg.Done()
	t := time.NewTimer(p.flapGrace)
	defer t.Stop()
	select {
	case <-ctx.Done():
		p.logger.Debug("overlay peer grace cancelled",
			zap.String("group", groupID), zap.String("peer", peerNodeID))
		return
	case <-t.C:
	}
	g.mu.Lock()
	st, ok := g.peers[peerNodeID]
	if !ok || st.kind != peerStateLeaving {
		g.mu.Unlock()
		return
	}
	delete(g.peers, peerNodeID)
	devName := g.deviceName
	g.mu.Unlock()
	tdCtx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
	defer cancel()
	if err := p.dev.RemoveFDBEntry(tdCtx, devName, peerIP); err != nil {
		p.logger.Error("overlay fdb teardown failed",
			zap.String("group", groupID), zap.String("peer", peerNodeID),
			zap.String("error", err.Error()))
	}
	if err := p.ipsec.RemoveSA(tdCtx, p.localIP, peerIP, p.port); err != nil {
		p.logger.Error("overlay sa teardown failed",
			zap.String("group", groupID), zap.String("peer", peerNodeID),
			zap.String("error", err.Error()))
	}
	p.logger.Info("overlay peer removed",
		zap.String("group", groupID), zap.String("peer", peerNodeID))
}

// RemoveGroup tears down every per-peer SA + FDB entry, destroys the
// VXLAN device, releases the VNI, and cancels any pending flap-grace
// timers. Idempotent: missing group returns nil.
func (p *PeerManager) RemoveGroup(ctx context.Context, clusterPath, groupID string) error {
	if err := p.checkRunning(); err != nil {
		return err
	}
	key := groupKey{clusterPath, groupID}
	p.mu.Lock()
	g, ok := p.groups[key]
	if !ok {
		p.mu.Unlock()
		return nil
	}
	delete(p.groups, key)
	p.mu.Unlock()
	g.mu.Lock()
	type peerEntry struct {
		nodeID, ip string
		cancel     context.CancelFunc
	}
	entries := make([]peerEntry, 0, len(g.peers))
	for id, st := range g.peers {
		entries = append(entries, peerEntry{nodeID: id, ip: st.externalIP, cancel: st.cancel})
	}
	g.peers = map[string]*peerState{}
	devName := g.deviceName
	g.mu.Unlock()
	for _, e := range entries {
		if e.cancel != nil {
			e.cancel()
		}
		if err := p.dev.RemoveFDBEntry(ctx, devName, e.ip); err != nil {
			p.logger.Error("overlay fdb teardown failed during group remove",
				zap.String("group", groupID), zap.String("peer", e.nodeID),
				zap.String("error", err.Error()))
		}
		if err := p.ipsec.RemoveSA(ctx, p.localIP, e.ip, p.port); err != nil {
			p.logger.Error("overlay sa teardown failed during group remove",
				zap.String("group", groupID), zap.String("peer", e.nodeID),
				zap.String("error", err.Error()))
		}
	}
	if err := p.dev.Destroy(ctx, devName); err != nil {
		p.logger.Error("overlay device destroy failed",
			zap.String("group", groupID), zap.String("device", devName),
			zap.String("error", err.Error()))
	}
	if err := p.vniAlloc.Release(clusterPath, groupID); err != nil {
		p.logger.Error("overlay vni release failed",
			zap.String("group", groupID), zap.String("error", err.Error()))
	}
	p.logger.Info("overlay group removed",
		zap.String("cluster", clusterPath), zap.String("group", groupID),
		zap.String("device", devName), zap.Int("peers", len(entries)))
	return nil
}

// Stop cancels every in-flight grace timer and waits for the worker
// goroutines to exit. Subsequent calls return ErrPeerManagerStopped.
// Does NOT tear down kernel state — the network manager handles that
// on cluster leave (see plan 11A.14).
func (p *PeerManager) Stop() error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	p.mu.Unlock()
	p.stopCancel()
	p.wg.Wait()
	p.logger.Info("overlay peer manager stopped")
	return nil
}

// checkRunning gates every public method on the stopped flag.
func (p *PeerManager) checkRunning() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return ErrPeerManagerStopped
	}
	return nil
}

// lookupGroup returns the groupState for (cluster, group) or an error
// if EnsureGroup hasn't been called yet.
func (p *PeerManager) lookupGroup(clusterPath, groupID string) (*groupState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	g, ok := p.groups[groupKey{clusterPath, groupID}]
	if !ok {
		return nil, fmt.Errorf("overlay: group not tracked: %s/%s", clusterPath, groupID)
	}
	return g, nil
}
