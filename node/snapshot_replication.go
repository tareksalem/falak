package node

import (
	"context"
	"sync"

	"github.com/libp2p/go-libp2p/core/peer"
	"go.uber.org/zap"

	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/metrics"
	"github.com/tareksalem/falak/node/phonebook"
	"github.com/tareksalem/falak/snapshot"
)

// snapshotCandidateProvider satisfies snapshot.CandidateProvider for the
// O11 replicator. It assembles replication candidates from the phonebook
// (failure domain + Active status) and the metrics peer store (free disk +
// a gravity-suitability proxy). It deliberately does NOT call the election
// gravity calculator: that calculator scores only the LOCAL node
// (StateProvider.LocalNode by the Session-13 simplification), so a real
// per-peer gravity score is not reachable from here. The proxy below
// (free-resource ratios + connection reliability) is the suitability input
// to the power-of-two-choices tiebreak; the snapshot package consumes the
// float and never reaches into phonebook/gravity itself.
type snapshotCandidateProvider struct {
	phonebook   phonebook.IPhonebook
	metrics     *metrics.Manager // may be nil on metrics-less nodes
	clusterPath string
	selfID      string
	localDC     string
	localRegion string
	logger      *zap.Logger
}

// Candidates returns every known peer (excluding self) as a replication
// candidate. The Active and disk-headroom filters are applied inside the
// snapshot package's selectTargets; this method supplies the raw view.
func (p *snapshotCandidateProvider) Candidates(_, _ string) []snapshot.Candidate {
	entries, err := p.phonebook.GetByCluster(p.clusterPath)
	if err != nil {
		p.logger.Debug("snapshot candidates: phonebook lookup failed", zap.Error(err))
		return nil
	}
	out := make([]snapshot.Candidate, 0, len(entries))
	for _, e := range entries {
		if e == nil || e.NodeID == p.selfID {
			continue
		}
		pid, err := peer.Decode(e.NodeID)
		if err != nil {
			continue
		}
		diskFree, gravity := p.peerSuitability(e)
		out = append(out, snapshot.Candidate{
			NodeID:     e.NodeID,
			PeerID:     pid,
			Datacenter: e.Datacenter,
			Region:     e.Region,
			DiskMBFree: diskFree,
			Gravity:    gravity,
			Active:     e.Status == phonebook.NodeStatusEnum.Active(),
		})
	}
	return out
}

// LocalDatacenter returns the holder's datacenter for failure-domain spread.
func (p *snapshotCandidateProvider) LocalDatacenter() string { return p.localDC }

// LocalRegion returns the holder's region (failure-domain fallback).
func (p *snapshotCandidateProvider) LocalRegion() string { return p.localRegion }

// peerSuitability returns the peer's free disk in MiB and a suitability
// proxy used solely for the power-of-two-choices tiebreak. Higher is
// better. Free disk falls back to phonebook capabilities when no fresh
// metrics snapshot exists.
func (p *snapshotCandidateProvider) peerSuitability(e *phonebook.Entry) (diskFreeMB int64, gravity float64) {
	if e.Capabilities != nil {
		diskFreeMB = e.Capabilities.DiskGB * 1024
	}
	reliability := e.SuccessRate

	var diskRatio, memRatio, cpuRatio float64
	if p.metrics != nil {
		if snap, err := p.metrics.PeerLatest(e.NodeID); err == nil && !snap.CapturedAt.IsZero() {
			if snap.Disk.FreeMB > 0 {
				diskFreeMB = snap.Disk.FreeMB
			}
			if snap.Disk.TotalMB > 0 {
				diskRatio = float64(snap.Disk.FreeMB) / float64(snap.Disk.TotalMB)
			}
			if snap.Memory.TotalMB > 0 {
				memRatio = float64(snap.Memory.AvailableMB) / float64(snap.Memory.TotalMB)
			}
			used := snap.CPU.UsedPercent
			if used < 0 {
				used = 0
			}
			if used > 100 {
				used = 100
			}
			cpuRatio = (100 - used) / 100
		}
	}
	gravity = diskRatio + memRatio + cpuRatio + reliability
	return diskFreeMB, gravity
}

// indexPruner is the narrow surface the reconciler needs from the snapshot
// discovery index. Satisfied by *snapshot.Discovery; an interface so the
// reconciler is unit-testable without a live libp2p Discovery.
type indexPruner interface {
	PruneNode(nodeID string) int
}

// snapshotReconciler is the load-bearing index↔membership reconciliation
// wiring. It subscribes to NodeFailed and NodeDeparting on the node event
// bus and prunes the departed/failed node's entries from the snapshot
// discovery index so the puller never targets a dead holder (the failure
// that would otherwise defeat replication entirely — plan part 6).
type snapshotReconciler struct {
	bus    events.Bus
	pruner indexPruner
	logger *zap.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newSnapshotReconciler builds a reconciler. Call Start to subscribe.
func newSnapshotReconciler(bus events.Bus, pruner indexPruner, logger *zap.Logger) *snapshotReconciler {
	return &snapshotReconciler{bus: bus, pruner: pruner, logger: logger}
}

// Start subscribes to membership-loss events and prunes the index. The
// subscription goroutine is WaitGroup-tracked and exits on Stop.
func (r *snapshotReconciler) Start(parent context.Context) {
	r.ctx, r.cancel = context.WithCancel(parent)
	failedCh := r.bus.Subscribe(events.TypeNodeFailed)
	departCh := r.bus.Subscribe(events.TypeNodeDeparting)

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			select {
			case <-r.ctx.Done():
				return
			case ev, ok := <-failedCh:
				if !ok {
					failedCh = nil
					continue
				}
				if f, ok := ev.(events.NodeFailed); ok {
					if pruned := r.pruner.PruneNode(f.NodeID); pruned > 0 {
						r.logger.Info("snapshot reconcile: pruned failed node from index",
							zap.String("node", f.NodeID),
							zap.Int("count", pruned))
					}
				}
			case ev, ok := <-departCh:
				if !ok {
					departCh = nil
					continue
				}
				if d, ok := ev.(events.NodeDeparting); ok {
					if pruned := r.pruner.PruneNode(d.NodeID); pruned > 0 {
						r.logger.Info("snapshot reconcile: pruned departing node from index",
							zap.String("node", d.NodeID),
							zap.Int("count", pruned))
					}
				}
			}
			if failedCh == nil && departCh == nil {
				return // both subscriptions closed (bus shut down)
			}
		}
	}()
}

// Stop cancels the subscription goroutine and waits for it to exit.
func (r *snapshotReconciler) Stop() {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
}

// startSnapshotReplication brings up the O11 snapshot replication mesh for
// the given cluster: the gossip Discovery (signed availability index), the
// holder-driven Replicator, and the index↔membership reconciler. It then
// wires all three into the already-built runtime handler via
// SetSnapshotMesh (the handler is constructed at node start, before any
// join, so the mesh is injected post-construction).
//
// Best-effort and idempotent: it is a no-op on runtime-less nodes (no
// snapshot store) and only the FIRST cluster gets the mesh (the node keeps
// a single Discovery, matching the existing single-mesh design). Any
// failure logs at Warn and leaves the node running without replication.
func (n *Node) startSnapshotReplication(clusterPath string) {
	if n.snapshotStore == nil || n.host == nil || n.pubsub == nil {
		return
	}
	if n.snapshotDiscovery != nil {
		return // already initialized for an earlier cluster
	}

	disc, err := snapshot.NewDiscovery(n.host, n.pubsub, n.snapshotStore, clusterPath,
		snapshot.WithDiscoveryLogger(n.logger.Named("snapshot.discovery")),
		snapshot.WithDiscoverySigner(newCapsuleSigner(n.privateKey)),
		snapshot.WithDiscoveryVerifier(newCapsuleVerifier(n.phonebook, clusterPath)),
	)
	if err != nil {
		n.logger.Warn("snapshot replication: discovery init failed; node continues without HA replication",
			zap.String("cluster", clusterPath), zap.Error(err))
		return
	}
	n.snapshotDiscovery = disc

	provider := &snapshotCandidateProvider{
		phonebook:   n.phonebook,
		metrics:     n.metricsManager,
		clusterPath: clusterPath,
		selfID:      n.host.ID().String(),
		localDC:     n.datacenter,
		localRegion: n.region,
		logger:      n.logger.Named("snapshot.candidates"),
	}

	rep := snapshot.NewReplicator(
		snapshot.WithReplicationHost(n.host),
		snapshot.WithReplicationStore(n.snapshotStore),
		snapshot.WithReplicationIndex(disc),
		snapshot.WithReplicationBroadcaster(disc),
		snapshot.WithReplicationCandidateProvider(provider),
		snapshot.WithReplicationLogger(n.logger.Named("snapshot.replication")),
	)
	n.snapshotReplicator = rep

	// Wire the full mesh into the runtime handler: availability broadcaster,
	// remote puller (for winners without a local standby), and the
	// holder-driven replicator (push-after-capture).
	if n.runtimeHandler != nil {
		puller := &runtimeSnapshotPullerAdapter{
			discovery: disc,
			store:     n.snapshotStore,
			host:      n.host,
			logger:    n.logger.Named("runtime.snapshot"),
		}
		n.runtimeHandler.SetSnapshotMesh(disc, puller, rep)
	}

	// Index↔membership reconciliation (load-bearing): prune failed/departed
	// holders so the puller never targets a corpse.
	n.snapshotReconciler = newSnapshotReconciler(n.eventBus, disc, n.logger.Named("snapshot.reconcile"))
	n.snapshotReconciler.Start(n.ctx)

	n.logger.Info("snapshot replication mesh started",
		zap.String("cluster", clusterPath))
}
