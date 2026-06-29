// Package sync provides member list synchronization between nodes.
package sync

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/multiformats/go-multiaddr"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/internal/ratelimit"
	"github.com/tareksalem/falak/node/phonebook"
	"github.com/tareksalem/falak/node/proto/syncpb"
	"github.com/tareksalem/falak/shared"
)

const (
	// ProtocolID is the protocol identifier for sync streams.
	ProtocolID = "/falak/sync/1.0"

	// DefaultSyncInterval is the default interval for periodic sync.
	DefaultSyncInterval = 5 * time.Minute

	// DefaultSyncTimeout is the default timeout for sync requests.
	DefaultSyncTimeout = 30 * time.Second

	// DefaultMaxMembersPerResponse is the default max members per sync response.
	DefaultMaxMembersPerResponse = 1000

	// DefaultSyncRateLimit is the max sync requests per peer per window.
	DefaultSyncRateLimit = 20

	// DefaultSyncRateWindow is the rate limiting window.
	DefaultSyncRateWindow = 1 * time.Minute

	// MaxConsecutiveFailsBeforeQuarantine is the number of consecutive connection
	// failures before marking a peer as quarantined.
	MaxConsecutiveFailsBeforeQuarantine = 3

	// MaxConsecutiveFailsBeforeRemoval is the number of consecutive connection
	// failures before removing a peer from the phonebook.
	MaxConsecutiveFailsBeforeRemoval = 5
)

// RevocationSource provides access to certificate revocation data for sync.
// This interface avoids circular imports between the sync and auth packages.
type RevocationSource interface {
	// GetRevocations returns all revocation entries for a cluster.
	GetRevocations(clusterPath string) []RevocationEntry
	// MergeRevocations merges remote revocation entries into the local list.
	// Returns the number of new entries added.
	MergeRevocations(clusterPath string, entries []RevocationEntry) (int, error)
}

// RevocationEntry mirrors certs.RevocationEntry to avoid circular imports.
type RevocationEntry struct {
	CertFingerprint string
	NodeID          string
	ClusterPath     string
	RevokedAt       time.Time
	Reason          string
	RevokedBy       string
}

// Syncer handles member list synchronization between nodes.
type Syncer struct {
	host        host.Host
	phonebook   phonebook.IPhonebook
	eventBus    events.Bus
	logger      *zap.Logger
	revocations RevocationSource // optional: for syncing revocation lists

	// Sync configuration
	syncInterval          time.Duration
	syncTimeout           time.Duration
	maxMembersPerResponse int
	enablePeriodicSync    bool

	// Track last sync times per cluster
	lastSyncMu sync.RWMutex
	lastSync   map[string]time.Time

	// Rate limiting
	rateLimiter *ratelimit.Limiter

	// Lifecycle
	parentCtx context.Context // Set via WithContext option
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// Option configures a Syncer.
type Option func(*Syncer)

// WithContext sets the parent context for the syncer.
// The syncer will derive its internal context from this parent,
// enabling proper cancellation propagation from the parent component.
func WithContext(ctx context.Context) Option {
	return func(s *Syncer) {
		s.parentCtx = ctx
	}
}

// WithHost sets the libp2p host.
func WithHost(h host.Host) Option {
	return func(s *Syncer) {
		s.host = h
	}
}

// WithPhonebook sets the phonebook for member storage.
func WithPhonebook(pb phonebook.IPhonebook) Option {
	return func(s *Syncer) {
		s.phonebook = pb
	}
}

// WithEventBus sets the event bus.
func WithEventBus(bus events.Bus) Option {
	return func(s *Syncer) {
		s.eventBus = bus
	}
}

// WithLogger sets the logger.
func WithLogger(logger *zap.Logger) Option {
	return func(s *Syncer) {
		s.logger = logger
	}
}

// WithSyncInterval sets the periodic sync interval.
func WithSyncInterval(d time.Duration) Option {
	return func(s *Syncer) {
		s.syncInterval = d
	}
}

// WithSyncTimeout sets the sync request timeout.
func WithSyncTimeout(d time.Duration) Option {
	return func(s *Syncer) {
		s.syncTimeout = d
	}
}

// WithMaxMembersPerResponse sets the max members per response.
func WithMaxMembersPerResponse(max int) Option {
	return func(s *Syncer) {
		s.maxMembersPerResponse = max
	}
}

// WithPeriodicSyncEnabled enables or disables periodic sync.
func WithPeriodicSyncEnabled(enabled bool) Option {
	return func(s *Syncer) {
		s.enablePeriodicSync = enabled
	}
}

// WithRevocationSource sets the source for certificate revocation data.
func WithRevocationSource(rs RevocationSource) Option {
	return func(s *Syncer) {
		s.revocations = rs
	}
}

// New creates a new Syncer with the given options.
func New(opts ...Option) *Syncer {
	s := &Syncer{
		logger:                zap.NewNop(),
		syncInterval:          DefaultSyncInterval,
		syncTimeout:           DefaultSyncTimeout,
		maxMembersPerResponse: DefaultMaxMembersPerResponse,
		enablePeriodicSync:    true,
		lastSync:              make(map[string]time.Time),
		rateLimiter:           ratelimit.NewLimiter(DefaultSyncRateLimit, DefaultSyncRateWindow),
	}

	// Apply options first to capture parentCtx if provided
	for _, opt := range opts {
		opt(s)
	}

	// Derive context from parent if provided, otherwise use Background
	if s.parentCtx != nil {
		s.ctx, s.cancel = context.WithCancel(s.parentCtx)
	} else {
		s.ctx, s.cancel = context.WithCancel(context.Background())
	}

	return s
}

// Start starts the syncer and registers the protocol handler.
// Returns an error if required dependencies are not configured.
func (s *Syncer) Start() error {
	// Validate required dependencies
	if s.host == nil {
		return fmt.Errorf("host is required")
	}
	if s.phonebook == nil {
		return fmt.Errorf("phonebook is required")
	}
	if s.eventBus == nil {
		return fmt.Errorf("eventBus is required")
	}

	s.host.SetStreamHandler(ProtocolID, s.handleSyncStream)

	// Subscribe to sync request events
	syncReqCh := s.eventBus.Subscribe(events.TypeSyncRequested)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.syncRequestLoop(syncReqCh)
	}()

	// Subscribe to cluster joined events to start periodic sync
	clusterJoinedCh := s.eventBus.Subscribe(events.TypeClusterJoined)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.clusterJoinedLoop(clusterJoinedCh)
	}()

	return nil
}

// syncRequestLoop handles sync request events from the event bus.
func (s *Syncer) syncRequestLoop(ch <-chan events.Event) {
	for {
		select {
		case <-s.ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			evt, ok := e.(events.SyncRequested)
			if !ok {
				continue
			}
			go s.handleSyncRequest(evt)
		}
	}
}

// clusterJoinedLoop handles cluster joined events to start periodic sync.
func (s *Syncer) clusterJoinedLoop(ch <-chan events.Event) {
	for {
		select {
		case <-s.ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			evt, ok := e.(events.ClusterJoined)
			if !ok {
				continue
			}

			s.logger.Info("cluster joined, initiating sync",
				zap.String("cluster", evt.ClusterPath),
				zap.String("voucher", evt.VoucherNodeID))

			// Trigger initial sync with voucher as preferred peer (with fallback to others)
			go s.handleSyncRequest(events.SyncRequested{
				BaseEvent:     events.NewBaseEvent(),
				ClusterPath:   evt.ClusterPath,
				Reason:        "post_join",
				PreferredPeer: evt.VoucherNodeID,
			})

			// Start periodic sync for this cluster
			s.StartPeriodicSync(evt.ClusterPath)
		}
	}
}

// Stop stops the syncer and cleans up resources.
func (s *Syncer) Stop() {
	s.cancel()
	s.wg.Wait()
	s.host.RemoveStreamHandler(ProtocolID)
}

// StartPeriodicSync starts the periodic sync loop for a cluster.
func (s *Syncer) StartPeriodicSync(clusterPath string) {
	if !s.enablePeriodicSync {
		return
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.periodicSyncLoop(clusterPath)
	}()
}

// SyncFrom requests a full member list sync from a specific peer.
func (s *Syncer) SyncFrom(ctx context.Context, clusterPath string, targetPeer peer.ID) error {
	return s.syncFromPeer(ctx, clusterPath, targetPeer, nil)
}

// SyncDeltaFrom requests a delta sync (only changes since last sync) from a specific peer.
func (s *Syncer) SyncDeltaFrom(ctx context.Context, clusterPath string, targetPeer peer.ID) error {
	s.lastSyncMu.RLock()
	lastSync, ok := s.lastSync[clusterPath]
	s.lastSyncMu.RUnlock()

	var lastSyncTime *timestamppb.Timestamp
	if ok {
		lastSyncTime = timestamppb.New(lastSync)
	}

	return s.syncFromPeer(ctx, clusterPath, targetPeer, lastSyncTime)
}

// syncFromPeer performs the actual sync request to a peer.
func (s *Syncer) syncFromPeer(ctx context.Context, clusterPath string, targetPeer peer.ID, lastSyncTime *timestamppb.Timestamp) error {
	ctx, cancel := context.WithTimeout(ctx, s.syncTimeout)
	defer cancel()

	// Open sync stream
	stream, err := s.host.NewStream(ctx, targetPeer, ProtocolID)
	if err != nil {
		// Record connection failure
		s.recordConnectionFailure(targetPeer.String(), clusterPath)
		s.emitSyncFailed(clusterPath, targetPeer.String(), fmt.Sprintf("failed to open stream: %v", err))
		return fmt.Errorf("failed to open sync stream: %w", err)
	}
	defer stream.Close()

	// Record successful connection
	s.phonebook.RecordConnectionAttempt(targetPeer.String(), clusterPath, true)

	requestID := uuid.New().String()

	// Send SyncRequest
	req := &syncpb.SyncRequest{
		ClusterPath:  clusterPath,
		LastSyncTime: lastSyncTime,
		RequestId:    requestID,
		MaxMembers:   int32(s.maxMembersPerResponse),
	}

	if err := shared.WriteProto(stream, req); err != nil {
		s.emitSyncFailed(clusterPath, targetPeer.String(), fmt.Sprintf("failed to send request: %v", err))
		return fmt.Errorf("failed to send sync request: %w", err)
	}

	// Read SyncResponse
	var resp syncpb.SyncResponse
	if err := shared.ReadProto(stream, &resp); err != nil {
		s.emitSyncFailed(clusterPath, targetPeer.String(), fmt.Sprintf("failed to read response: %v", err))
		return fmt.Errorf("failed to read sync response: %w", err)
	}

	if resp.Error != "" {
		s.emitSyncFailed(clusterPath, targetPeer.String(), resp.Error)
		return fmt.Errorf("sync error from peer: %s", resp.Error)
	}

	// Process the members
	newMembers := 0
	for _, member := range resp.Members {
		added, err := s.processMember(clusterPath, member)
		if err != nil {
			s.logger.Error("failed to process member",
				zap.String("nodeId", member.NodeId),
				zap.Error(err))
			continue
		}
		if added {
			newMembers++
		}
	}

	// Process revocations if present
	if s.revocations != nil && len(resp.Revocations) > 0 {
		revEntries := make([]RevocationEntry, 0, len(resp.Revocations))
		for _, r := range resp.Revocations {
			entry := RevocationEntry{
				CertFingerprint: r.CertFingerprint,
				NodeID:          r.NodeId,
				ClusterPath:     r.ClusterPath,
				Reason:          r.Reason,
				RevokedBy:       r.RevokedBy,
			}
			if r.RevokedAt != nil {
				entry.RevokedAt = r.RevokedAt.AsTime()
			}
			revEntries = append(revEntries, entry)
		}

		newRevocations, err := s.revocations.MergeRevocations(clusterPath, revEntries)
		if err != nil {
			s.logger.Error("failed to merge revocations",
				zap.String("cluster", clusterPath),
				zap.Error(err))
		} else if newRevocations > 0 {
			s.logger.Info("merged revocations from sync",
				zap.String("cluster", clusterPath),
				zap.Int("new", newRevocations))
		}
	}

	// Update last sync time
	if resp.SyncTime != nil {
		s.lastSyncMu.Lock()
		s.lastSync[clusterPath] = resp.SyncTime.AsTime()
		s.lastSyncMu.Unlock()
	}

	// Emit success event
	s.eventBus.Publish(events.SyncCompleted{
		BaseEvent:   events.NewBaseEvent(),
		ClusterPath: clusterPath,
		MemberCount: len(resp.Members),
		NewMembers:  newMembers,
		SyncedFrom:  targetPeer.String(),
	})

	s.logger.Info("sync completed",
		zap.String("cluster", clusterPath),
		zap.String("peer", targetPeer.String()),
		zap.Int("members", len(resp.Members)),
		zap.Int("new", newMembers))

	// Handle pagination if not complete
	if !resp.IsComplete && resp.NextCursor != "" {
		return s.syncContinue(ctx, clusterPath, targetPeer, resp.NextCursor)
	}

	return nil
}

// syncContinue continues a paginated sync.
func (s *Syncer) syncContinue(ctx context.Context, clusterPath string, targetPeer peer.ID, cursor string) error {
	stream, err := s.host.NewStream(ctx, targetPeer, ProtocolID)
	if err != nil {
		return fmt.Errorf("failed to open continuation stream: %w", err)
	}
	defer stream.Close()

	req := &syncpb.SyncRequest{
		ClusterPath: clusterPath,
		RequestId:   uuid.New().String(),
		MaxMembers:  int32(s.maxMembersPerResponse),
		Cursor:      cursor,
	}

	if err := shared.WriteProto(stream, req); err != nil {
		return fmt.Errorf("failed to send continuation request: %w", err)
	}

	var resp syncpb.SyncResponse
	if err := shared.ReadProto(stream, &resp); err != nil {
		return fmt.Errorf("failed to read continuation response: %w", err)
	}

	// Process members
	for _, member := range resp.Members {
		if _, err := s.processMember(clusterPath, member); err != nil {
			s.logger.Error("failed to process member",
				zap.String("nodeId", member.NodeId),
				zap.Error(err))
		}
	}

	// Continue if more pages
	if !resp.IsComplete && resp.NextCursor != "" {
		return s.syncContinue(ctx, clusterPath, targetPeer, resp.NextCursor)
	}

	return nil
}

// processMember adds or updates a member in the phonebook.
func (s *Syncer) processMember(clusterPath string, member *syncpb.MemberInfo) (bool, error) {
	// Check if member already exists
	exists, err := s.phonebook.Exists(member.NodeId, clusterPath)
	if err != nil {
		return false, err
	}

	// Parse the cluster path so the entry carries Region/Datacenter
	// even when delta-sync overwrites a previously-subscriber-built
	// row. Without this, the subscriber set Region="test"/DC="dc1"
	// and the very next sync round blanked them.
	cp, _ := shared.ParseClusterPath(clusterPath)

	entry := &phonebook.Entry{
		NodeID:      member.NodeId,
		ClusterPath: clusterPath,
		PublicKey:   member.PublicKey,
		Addresses:   member.Addresses,
		Region:      cp.Region,
		Datacenter:  cp.Datacenter,
		UpdatedAt:   time.Now(),
	}
	if member.Capabilities != nil {
		if n, ok := member.Capabilities.Metadata[phonebook.MetadataKeyNodeName]; ok {
			entry.Name = n
		}
	}

	if member.JoinedAt != nil {
		entry.FirstSeen = member.JoinedAt.AsTime()
	}

	if member.LastSeen != nil {
		entry.LastSeen = member.LastSeen.AsTime()
	}

	if member.Status != "" {
		switch member.Status {
		case "active":
			entry.Status = phonebook.NodeStatusEnum.Active()
		case "suspected":
			entry.Status = phonebook.NodeStatusEnum.Suspected()
		case "quarantined":
			entry.Status = phonebook.NodeStatusEnum.Quarantined()
		default:
			entry.Status = phonebook.NodeStatusEnum.Active()
		}
	}

	if member.Capabilities != nil {
		entry.Capabilities = &phonebook.Capabilities{
			CPUCores:   member.Capabilities.CpuCores,
			MemoryMB:   member.Capabilities.MemoryMb,
			DiskGB:     member.Capabilities.DiskGb,
			Datacenter: member.Capabilities.Datacenter,
			Tags:       member.Capabilities.Tags,
			Metadata:   member.Capabilities.Metadata,
		}
	}

	if exists {
		return false, s.phonebook.Update(entry)
	}

	return true, s.phonebook.Add(entry)
}

// handleSyncStream handles incoming sync requests.
func (s *Syncer) handleSyncStream(stream network.Stream) {
	defer stream.Close()

	remotePeer := stream.Conn().RemotePeer()

	// Check rate limit
	if !s.rateLimiter.Allow(remotePeer.String()) {
		s.logger.Warn("rate limited sync request",
			zap.String("peer", remotePeer.String()))
		s.sendSyncError(stream, "", "rate limited")
		return
	}

	var req syncpb.SyncRequest
	if err := shared.ReadProto(stream, &req); err != nil {
		s.logger.Error("failed to read sync request", zap.Error(err))
		return
	}

	s.logger.Debug("received sync request",
		zap.String("peer", remotePeer.String()),
		zap.String("cluster", req.ClusterPath),
		zap.String("requestId", req.RequestId))

	// Verify peer is authenticated (exists in phonebook for requested cluster)
	exists, err := s.phonebook.Exists(remotePeer.String(), req.ClusterPath)
	if err != nil {
		s.logger.Error("failed to check peer in phonebook",
			zap.String("peer", remotePeer.String()),
			zap.Error(err))
		s.sendSyncError(stream, req.RequestId, "internal error")
		return
	}
	if !exists {
		s.logger.Warn("rejected sync request from unauthenticated peer",
			zap.String("peer", remotePeer.String()),
			zap.String("cluster", req.ClusterPath))
		s.sendSyncError(stream, req.RequestId, "peer not authenticated for cluster")
		return
	}

	// Get members from phonebook
	entries, err := s.phonebook.GetByCluster(req.ClusterPath)
	if err != nil {
		s.sendSyncError(stream, req.RequestId, fmt.Sprintf("failed to get members: %v", err))
		return
	}

	// Filter by last_sync_time if delta sync
	if req.LastSyncTime != nil {
		since := req.LastSyncTime.AsTime()
		entries = filterUpdatedSince(entries, since)
	}

	// Apply pagination limit
	maxMembers := int(req.MaxMembers)
	if maxMembers <= 0 || maxMembers > s.maxMembersPerResponse {
		maxMembers = s.maxMembersPerResponse
	}

	isComplete := len(entries) <= maxMembers
	if len(entries) > maxMembers {
		entries = entries[:maxMembers]
	}

	// Build response
	members := make([]*syncpb.MemberInfo, 0, len(entries))
	for _, entry := range entries {
		member := &syncpb.MemberInfo{
			NodeId:    entry.NodeID,
			Addresses: entry.Addresses,
			PublicKey: entry.PublicKey,
			JoinedAt:  timestamppb.New(entry.FirstSeen),
			LastSeen:  timestamppb.New(entry.LastSeen),
			Status:    string(entry.Status),
		}

		if entry.Capabilities != nil {
			member.Capabilities = &syncpb.Capabilities{
				CpuCores:   entry.Capabilities.CPUCores,
				MemoryMb:   entry.Capabilities.MemoryMB,
				DiskGb:     entry.Capabilities.DiskGB,
				Datacenter: entry.Capabilities.Datacenter,
				Tags:       entry.Capabilities.Tags,
				Metadata:   entry.Capabilities.Metadata,
			}
		}

		members = append(members, member)
	}

	resp := &syncpb.SyncResponse{
		RequestId:  req.RequestId,
		Members:    members,
		SyncTime:   timestamppb.Now(),
		IsComplete: isComplete,
	}

	// Include revocation entries in the sync response
	if s.revocations != nil {
		revEntries := s.revocations.GetRevocations(req.ClusterPath)
		for _, entry := range revEntries {
			resp.Revocations = append(resp.Revocations, &syncpb.RevocationEntry{
				CertFingerprint: entry.CertFingerprint,
				NodeId:          entry.NodeID,
				ClusterPath:     entry.ClusterPath,
				RevokedAt:       timestamppb.New(entry.RevokedAt),
				Reason:          entry.Reason,
				RevokedBy:       entry.RevokedBy,
			})
		}
	}

	if err := shared.WriteProto(stream, resp); err != nil {
		s.logger.Error("failed to send sync response", zap.Error(err))
		return
	}

	s.logger.Debug("sent sync response",
		zap.String("peer", remotePeer.String()),
		zap.Int("members", len(members)),
		zap.Int("revocations", len(resp.Revocations)),
		zap.Bool("complete", isComplete))
}

// sendSyncError sends an error response.
func (s *Syncer) sendSyncError(stream network.Stream, requestID, errMsg string) {
	resp := &syncpb.SyncResponse{
		RequestId: requestID,
		Error:     errMsg,
	}
	shared.WriteProto(stream, resp)
}

// periodicSyncLoop runs the periodic sync for a cluster.
func (s *Syncer) periodicSyncLoop(clusterPath string) {
	ticker := time.NewTicker(s.syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.performPeriodicSync(clusterPath)
		}
	}
}

// performPeriodicSync selects peers and syncs from them until one succeeds.
// Probes all non-failed peers (including quarantined) so they can either
// recover back to active or reach the removal threshold.
func (s *Syncer) performPeriodicSync(clusterPath string) {
	peers, err := s.phonebook.GetByCluster(clusterPath)
	if err != nil || len(peers) == 0 {
		s.logger.Debug("no peers for periodic sync",
			zap.String("cluster", clusterPath))
		return
	}

	selfID := s.host.ID().String()
	var candidates []*phonebook.Entry
	for _, p := range peers {
		if p.NodeID == selfID {
			continue
		}
		if p.Status == phonebook.NodeStatusEnum.Failed() {
			continue
		}
		candidates = append(candidates, p)
	}

	if len(candidates) == 0 {
		return
	}

	// Shuffle candidates for load balancing (start from random index)
	startIdx := int(time.Now().UnixNano() % int64(len(candidates)))

	// Try candidates in order (starting from random index) until one succeeds
	for i := 0; i < len(candidates); i++ {
		idx := (startIdx + i) % len(candidates)
		selected := candidates[idx]

		peerID, err := peer.Decode(selected.NodeID)
		if err != nil {
			s.logger.Debug("invalid peer ID in phonebook",
				zap.String("nodeId", selected.NodeID),
				zap.Error(err))
			continue
		}

		// Add peer addresses to libp2p peerstore so we can dial them
		s.addPeerAddresses(peerID, selected.Addresses)

		// Perform delta sync
		ctx, cancel := context.WithTimeout(s.ctx, s.syncTimeout)
		err = s.SyncDeltaFrom(ctx, clusterPath, peerID)
		cancel()

		if err == nil {
			return // Success
		}

		s.logger.Debug("periodic sync attempt failed, trying next peer",
			zap.String("cluster", clusterPath),
			zap.String("peer", peerID.String()),
			zap.Error(err))
	}

	s.logger.Warn("periodic sync failed for all peers",
		zap.String("cluster", clusterPath))
}

// syncFromPeerID attempts to sync from a specific peer by ID.
// Returns true if sync succeeded, false otherwise.
func (s *Syncer) syncFromPeerID(clusterPath, nodeID string) bool {
	peerID, err := peer.Decode(nodeID)
	if err != nil {
		s.logger.Debug("invalid peer ID for sync",
			zap.String("nodeId", nodeID),
			zap.Error(err))
		return false
	}

	// Get peer's addresses from phonebook
	entry, err := s.phonebook.Get(nodeID, clusterPath)
	if err != nil || entry == nil {
		s.logger.Debug("peer not found in phonebook",
			zap.String("nodeId", nodeID),
			zap.String("cluster", clusterPath))
		return false
	}

	// Add addresses to peerstore
	s.addPeerAddresses(peerID, entry.Addresses)

	ctx, cancel := context.WithTimeout(s.ctx, s.syncTimeout)
	defer cancel()

	if err := s.SyncFrom(ctx, clusterPath, peerID); err != nil {
		s.logger.Debug("sync from peer failed",
			zap.String("cluster", clusterPath),
			zap.String("peer", nodeID),
			zap.Error(err))
		return false
	}

	s.logger.Info("sync completed",
		zap.String("cluster", clusterPath),
		zap.String("peer", nodeID))
	return true
}

// handleSyncRequest handles sync request events from other components.
// It tries the preferred peer first (if specified), then falls back to other
// peers in the cluster until one succeeds.
func (s *Syncer) handleSyncRequest(evt events.SyncRequested) {
	s.logger.Debug("handling sync request event",
		zap.String("cluster", evt.ClusterPath),
		zap.String("reason", evt.Reason),
		zap.String("preferredPeer", evt.PreferredPeer))

	// If a preferred peer is specified, try that first
	if evt.PreferredPeer != "" {
		if s.syncFromPeerID(evt.ClusterPath, evt.PreferredPeer) {
			return // Success with preferred peer
		}
		s.logger.Debug("preferred peer sync failed, trying other peers",
			zap.String("cluster", evt.ClusterPath),
			zap.String("preferredPeer", evt.PreferredPeer))
	}

	// Get all peers for this cluster to try as fallback
	peers, err := s.phonebook.GetBestPeers(evt.ClusterPath, 10)
	if err != nil || len(peers) == 0 {
		// Single-node bootstrap: phonebook only has self (or is empty)
		// — there's literally nobody to sync with. Demote to Debug so
		// it doesn't clutter the operator's normal startup log. Keep
		// Warn for the case where peers exist but the lookup errored.
		if err != nil {
			s.logger.Warn("no peers available for sync request",
				zap.String("cluster", evt.ClusterPath),
				zap.Error(err))
		} else {
			s.logger.Debug("no peers to sync with yet (cluster size <= 1)",
				zap.String("cluster", evt.ClusterPath))
		}
		return
	}

	// Build candidate list, excluding ourselves, failed peers, and the preferred peer (already tried)
	selfID := s.host.ID().String()
	var candidates []*phonebook.Entry

	for _, p := range peers {
		// Skip ourselves
		if p.NodeID == selfID {
			continue
		}
		// Skip the preferred peer (already tried above)
		if p.NodeID == evt.PreferredPeer {
			continue
		}
		// Skip quarantined/failed peers
		if p.Status == phonebook.NodeStatusEnum.Quarantined() ||
			p.Status == phonebook.NodeStatusEnum.Failed() {
			continue
		}
		candidates = append(candidates, p)
	}

	if len(candidates) == 0 {
		s.logger.Debug("no other healthy peers available for sync",
			zap.String("cluster", evt.ClusterPath))
		return
	}

	// Try candidates until one succeeds
	for _, entry := range candidates {
		if s.syncFromPeerID(evt.ClusterPath, entry.NodeID) {
			return // Success
		}
	}

	s.logger.Warn("all sync attempts failed",
		zap.String("cluster", evt.ClusterPath))
}

// emitSyncFailed emits a sync failed event.
func (s *Syncer) emitSyncFailed(clusterPath, peer, reason string) {
	s.eventBus.Publish(events.SyncFailed{
		BaseEvent:   events.NewBaseEvent(),
		ClusterPath: clusterPath,
		Peer:        peer,
		Reason:      reason,
	})
}

// filterUpdatedSince filters entries to only those updated after a given time.
func filterUpdatedSince(entries []*phonebook.Entry, since time.Time) []*phonebook.Entry {
	result := make([]*phonebook.Entry, 0)
	for _, e := range entries {
		if e.UpdatedAt.After(since) || e.LastSeen.After(since) {
			result = append(result, e)
		}
	}
	return result
}

// recordConnectionFailure records a connection failure and handles quarantine/removal.
func (s *Syncer) recordConnectionFailure(nodeID string, clusterPath string) {
	// Record the failed connection attempt
	if err := s.phonebook.RecordConnectionAttempt(nodeID, clusterPath, false); err != nil {
		s.logger.Warn("failed to record connection attempt",
			zap.String("nodeId", nodeID),
			zap.Error(err))
		return
	}

	// Check current failure count
	entry, err := s.phonebook.Get(nodeID, clusterPath)
	if err != nil || entry == nil {
		return
	}

	// Remove peer if too many consecutive failures
	if entry.ConsecutiveFails >= MaxConsecutiveFailsBeforeRemoval {
		s.logger.Info("removing unreachable peer from phonebook",
			zap.String("nodeId", nodeID),
			zap.String("cluster", clusterPath),
			zap.Int("consecutiveFails", entry.ConsecutiveFails))

		if err := s.phonebook.Remove(nodeID, clusterPath); err != nil {
			s.logger.Warn("failed to remove stale peer",
				zap.String("nodeId", nodeID),
				zap.Error(err))
		}
		return
	}

	// Quarantine peer if consecutive failures exceed threshold
	if entry.ConsecutiveFails >= MaxConsecutiveFailsBeforeQuarantine &&
		entry.Status != phonebook.NodeStatusEnum.Quarantined() {
		s.logger.Info("quarantining unreachable peer",
			zap.String("nodeId", nodeID),
			zap.String("cluster", clusterPath),
			zap.Int("consecutiveFails", entry.ConsecutiveFails))

		if err := s.phonebook.SetStatus(nodeID, clusterPath, phonebook.NodeStatusEnum.Quarantined()); err != nil {
			s.logger.Warn("failed to quarantine peer",
				zap.String("nodeId", nodeID),
				zap.Error(err))
		}
	}
}

// addPeerAddresses adds a peer's addresses to the libp2p peerstore so we can dial them.
func (s *Syncer) addPeerAddresses(peerID peer.ID, addresses []string) {
	addrs := make([]multiaddr.Multiaddr, 0, len(addresses))
	for _, addrStr := range addresses {
		addr, err := multiaddr.NewMultiaddr(addrStr)
		if err != nil {
			s.logger.Debug("invalid multiaddr",
				zap.String("addr", addrStr),
				zap.Error(err))
			continue
		}
		addrs = append(addrs, addr)
	}

	if len(addrs) > 0 {
		s.host.Peerstore().AddAddrs(peerID, addrs, peerstore.TempAddrTTL)
	}
}
