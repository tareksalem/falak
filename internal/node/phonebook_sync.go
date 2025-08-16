package node

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"golang.org/x/time/rate"

	"github.com/tareksalem/falak/internal/node/protobuf/models"
)

// Constants for configuration
const (
	DefaultSyncInterval      = 30 * time.Second
	DefaultMaxDeltaSize      = 1024 * 1024    // 1MB
	DefaultMaxPeersPerDelta  = 100
	DefaultSyncTimeout       = 15 * time.Second
	DefaultDatacenterWeight  = 2.0
	DefaultMaxMessageSize    = 16 * 1024 * 1024 // 16MB
	DefaultMaxConcurrentSync = 10
	DefaultRateLimit         = rate.Limit(5)     // 5 requests per second per peer
	DefaultRateBurst         = 10
	SessionCleanupInterval   = 5 * time.Minute
)

// PhonebookSyncer handles anti-entropy synchronization of phonebook data
type PhonebookSyncer struct {
	node *Node
	
	// Synchronization state
	lastSyncTime    time.Time
	syncVersion     uint64
	localDigest     []byte
	datacenters     map[string]*DatacenterInfo
	mu              sync.RWMutex
	
	// Configuration
	syncInterval      time.Duration
	maxDeltaSize      int
	maxPeersPerDelta  int
	syncTimeout       time.Duration
	datacenterWeight  float64 // Preference for same-datacenter peers
	maxMessageSize    uint32  // Maximum message size for DoS protection
	maxConcurrentSync int     // Maximum concurrent sync operations
	
	// Anti-entropy state
	pendingSyncs map[peer.ID]*SyncSession
	syncStats    *SyncStatistics
	
	// Rate limiting and concurrency control
	rateLimiters  map[peer.ID]*rate.Limiter
	rateLimiterMu sync.RWMutex
	syncSemaphore chan struct{} // Semaphore for controlling concurrent syncs
	
	// Context for cancellation
	ctx    context.Context
	cancel context.CancelFunc
}

// DatacenterInfo tracks information about a datacenter
type DatacenterInfo struct {
	ID            string
	PeerCount     int
	LastSeen      time.Time
	Digest        []byte
	SyncPriority  float64 // Higher values sync more frequently
}

// SyncSession tracks an ongoing synchronization with a peer
type SyncSession struct {
	PeerID      peer.ID
	StartTime   time.Time
	Phase       SyncPhase
	LocalDigest []byte
	PeerDigest  []byte
	DeltaSent   bool
	DeltaRecv   bool
}

// SyncPhase represents the current phase of synchronization
type SyncPhase string

const (
	syncPhaseDigest  SyncPhase = "digest"
	syncPhaseDelta   SyncPhase = "delta"
	syncPhaseApply   SyncPhase = "apply"
	syncPhaseComplete SyncPhase = "complete"
)

var SyncPhaseEnum = struct {
	Digest   SyncPhase
	Delta    SyncPhase
	Apply    SyncPhase
	Complete SyncPhase
}{
	Digest:   syncPhaseDigest,
	Delta:    syncPhaseDelta,
	Apply:    syncPhaseApply,
	Complete: syncPhaseComplete,
}

// SyncStatistics tracks synchronization metrics
type SyncStatistics struct {
	TotalSyncs        uint64
	SuccessfulSyncs   uint64
	FailedSyncs       uint64
	PeersAdded        uint64
	PeersUpdated      uint64
	PeersRemoved      uint64
	BytesSent         uint64
	BytesReceived     uint64
	LastSyncDuration  time.Duration
	AvgSyncDuration   time.Duration
	mu                sync.RWMutex
}

// NewPhonebookSyncer creates a new phonebook synchronizer
func NewPhonebookSyncer(node *Node) *PhonebookSyncer {
	ctx, cancel := context.WithCancel(node.ctx)
	
	syncer := &PhonebookSyncer{
		node:              node,
		syncVersion:       1,
		datacenters:       make(map[string]*DatacenterInfo),
		pendingSyncs:      make(map[peer.ID]*SyncSession),
		syncStats:         &SyncStatistics{},
		syncInterval:      DefaultSyncInterval,
		maxDeltaSize:      DefaultMaxDeltaSize,
		maxPeersPerDelta:  DefaultMaxPeersPerDelta,
		syncTimeout:       DefaultSyncTimeout,
		datacenterWeight:  DefaultDatacenterWeight,
		maxMessageSize:    DefaultMaxMessageSize,
		maxConcurrentSync: DefaultMaxConcurrentSync,
		rateLimiters:      make(map[peer.ID]*rate.Limiter),
		syncSemaphore:     make(chan struct{}, DefaultMaxConcurrentSync),
		ctx:               ctx,
		cancel:            cancel,
	}
	
	// Register protocol handlers
	protocolHandler := NewPhonebookProtocolHandler(syncer)
	protocolHandler.RegisterHandlers()
	
	// Start background cleanup goroutine
	go syncer.sessionCleanupLoop()
	
	return syncer
}

// Stop gracefully stops the phonebook synchronizer
func (ps *PhonebookSyncer) Stop() {
	if ps.cancel != nil {
		ps.cancel()
	}
	
	// Clean up all pending sessions
	ps.mu.Lock()
	sessionCount := len(ps.pendingSyncs)
	ps.pendingSyncs = make(map[peer.ID]*SyncSession)
	ps.mu.Unlock()
	
	if sessionCount > 0 {
		log.Printf("📚 Stopped phonebook syncer, cleaned up %d pending sessions", sessionCount)
	} else {
		log.Printf("📚 Stopped phonebook syncer")
	}
}

// StartSynchronization starts the phonebook synchronization process
func (ps *PhonebookSyncer) StartSynchronization() error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	
	// Initialize local state
	ps.updateLocalDigest()
	ps.updateDatacenterInfo()
	
	// Start periodic synchronization
	go ps.syncLoop()
	
	log.Printf("📚 Phonebook synchronization started (interval: %v)", ps.syncInterval)
	return nil
}

// syncLoop runs the periodic synchronization process
func (ps *PhonebookSyncer) syncLoop() {
	ticker := time.NewTicker(ps.syncInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			ps.performAntiEntropy()
			
		case <-ps.node.ctx.Done():
			log.Printf("📚 Phonebook synchronization stopped")
			return
		}
	}
}

// performAntiEntropy performs anti-entropy synchronization with peers
func (ps *PhonebookSyncer) performAntiEntropy() {
	startTime := time.Now()
	
	// Update local state with minimal lock time
	ps.mu.Lock()
	ps.updateLocalDigest()
	ps.updateDatacenterInfo()
	syncPeers := ps.selectSyncPeers()
	ps.mu.Unlock()
	
	if len(syncPeers) == 0 {
		log.Printf("📚 No peers available for synchronization")
		return
	}
	
	log.Printf("📚 Starting async anti-entropy sync with %d peers", len(syncPeers))
	
	// Perform synchronization with selected peers asynchronously
	var wg sync.WaitGroup
	successCount := int32(0)
	
	for _, peerID := range syncPeers {
		wg.Add(1)
		go func(pid peer.ID) {
			defer wg.Done()
			
			// Use semaphore to limit concurrent syncs
			select {
			case ps.syncSemaphore <- struct{}{}:
				defer func() { <-ps.syncSemaphore }()
				
				if ps.syncWithPeerAsync(pid) {
					atomic.AddInt32(&successCount, 1)
				}
			case <-ps.ctx.Done():
				log.Printf("📚 Sync cancelled for peer %s", pid.ShortString())
				return
			case <-time.After(ps.syncTimeout):
				log.Printf("📚 Sync semaphore timeout for peer %s", pid.ShortString())
				return
			}
		}(peerID)
	}
	
	// Wait for all sync operations to complete
	wg.Wait()
	
	duration := time.Since(startTime)
	finalSuccessCount := int(atomic.LoadInt32(&successCount))
	
	// Update statistics
	ps.syncStats.mu.Lock()
	ps.syncStats.TotalSyncs++
	if finalSuccessCount > 0 {
		ps.syncStats.SuccessfulSyncs++
	} else {
		ps.syncStats.FailedSyncs++
	}
	ps.syncStats.LastSyncDuration = duration
	ps.syncStats.AvgSyncDuration = time.Duration(
		(int64(ps.syncStats.AvgSyncDuration) + int64(duration)) / 2)
	ps.syncStats.mu.Unlock()
	
	log.Printf("📚 Async anti-entropy sync completed: %d/%d successful (duration: %v)",
		finalSuccessCount, len(syncPeers), duration)
	
	ps.mu.Lock()
	ps.lastSyncTime = time.Now()
	ps.mu.Unlock()
}

// selectSyncPeers selects peers for synchronization based on various criteria
func (ps *PhonebookSyncer) selectSyncPeers() []peer.ID {
	if ps.node.phonebook == nil {
		return nil
	}
	
	peers := ps.node.phonebook.GetPeers()
	connectedPeers := ps.node.host.Network().Peers()
	
	// Create map of connected peers for quick lookup
	connected := make(map[peer.ID]bool)
	for _, peerID := range connectedPeers {
		connected[peerID] = true
	}
	
	// Score and select peers
	type peerScore struct {
		peerID peer.ID
		score  float64
	}
	
	var candidates []peerScore
	nodeDatacenter := ps.node.dataCenter
	
	for _, p := range peers {
		peerID, err := peer.Decode(p.ID)
		if err != nil {
			continue
		}
		
		// Skip if not connected
		if !connected[peerID] {
			continue
		}
		
		// Skip if already syncing
		if _, syncing := ps.pendingSyncs[peerID]; syncing {
			continue
		}
		
		score := ps.calculateSyncScore(p, nodeDatacenter)
		candidates = append(candidates, peerScore{peerID: peerID, score: score})
	}
	
	// Sort by score (highest first)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})
	
	// Select top candidates (max 5 peers per sync round)
	maxPeers := 5
	if len(candidates) > maxPeers {
		candidates = candidates[:maxPeers]
	}
	
	result := make([]peer.ID, len(candidates))
	for i, candidate := range candidates {
		result[i] = candidate.peerID
	}
	
	return result
}

// calculateSyncScore calculates synchronization priority score for a peer
func (ps *PhonebookSyncer) calculateSyncScore(peer Peer, nodeDatacenter string) float64 {
	score := 1.0
	
	// Prefer same datacenter
	if peer.DataCenter == nodeDatacenter && nodeDatacenter != "" {
		score *= ps.datacenterWeight
	}
	
	// Prefer recently active peers
	timeSinceLastSeen := time.Since(peer.LastSeen)
	if timeSinceLastSeen < time.Hour {
		score *= 1.5
	} else if timeSinceLastSeen > 24*time.Hour {
		score *= 0.5
	}
	
	// Prefer high trust score peers
	score *= peer.TrustScore
	
	// Prefer peers we haven't synced with recently
	// TODO: Track last sync time per peer
	
	return score
}

// getRateLimiter gets or creates a rate limiter for a peer
func (ps *PhonebookSyncer) getRateLimiter(peerID peer.ID) *rate.Limiter {
	ps.rateLimiterMu.RLock()
	limiter, exists := ps.rateLimiters[peerID]
	ps.rateLimiterMu.RUnlock()
	
	if !exists {
		ps.rateLimiterMu.Lock()
		// Double-check after acquiring write lock
		if limiter, exists = ps.rateLimiters[peerID]; !exists {
			limiter = rate.NewLimiter(DefaultRateLimit, DefaultRateBurst)
			ps.rateLimiters[peerID] = limiter
		}
		ps.rateLimiterMu.Unlock()
	}
	
	return limiter
}

// syncWithPeerAsync performs async synchronization with rate limiting and timeouts
func (ps *PhonebookSyncer) syncWithPeerAsync(peerID peer.ID) bool {
	// Apply rate limiting
	limiter := ps.getRateLimiter(peerID)
	
	ctx, cancel := context.WithTimeout(ps.ctx, ps.syncTimeout)
	defer cancel()
	
	// Wait for rate limiter
	if err := limiter.Wait(ctx); err != nil {
		log.Printf("📚 Rate limit exceeded for peer %s: %v", peerID.ShortString(), err)
		return false
	}
	
	return ps.syncWithPeer(ctx, peerID)
}

// syncWithPeer performs synchronization with a specific peer
func (ps *PhonebookSyncer) syncWithPeer(ctx context.Context, peerID peer.ID) bool {
	// Create sync session with proper context
	session := &SyncSession{
		PeerID:      peerID,
		StartTime:   time.Now(),
		Phase:       SyncPhaseEnum.Digest,
		LocalDigest: ps.localDigest,
	}
	
	// Add to pending syncs with proper locking
	ps.mu.Lock()
	ps.pendingSyncs[peerID] = session
	ps.mu.Unlock()
	
	defer func() {
		ps.mu.Lock()
		delete(ps.pendingSyncs, peerID)
		ps.mu.Unlock()
	}()
	
	log.Printf("📚 Starting sync with peer %s", peerID.ShortString())
	
	// Phase 1: Exchange digests
	if !ps.exchangeDigests(ctx, session) {
		log.Printf("📚 Failed digest exchange with peer %s", peerID.ShortString())
		return false
	}
	
	// Phase 2: Exchange deltas if needed
	if !ps.exchangeDeltas(ctx, session) {
		log.Printf("📚 Failed delta exchange with peer %s", peerID.ShortString())
		return false
	}
	
	// Phase 3: Apply changes
	if !ps.applyChanges(ctx, session) {
		log.Printf("📚 Failed to apply changes from peer %s", peerID.ShortString())
		return false
	}
	
	session.Phase = SyncPhaseEnum.Complete
	log.Printf("📚 Sync completed with peer %s", peerID.ShortString())
	
	return true
}

// sessionCleanupLoop periodically cleans up stale sync sessions
func (ps *PhonebookSyncer) sessionCleanupLoop() {
	ticker := time.NewTicker(SessionCleanupInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			ps.cleanupStaleSessions()
		case <-ps.ctx.Done():
			log.Printf("📚 Session cleanup loop stopped")
			return
		}
	}
}

// cleanupStaleSessions removes stale sync sessions
func (ps *PhonebookSyncer) cleanupStaleSessions() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	
	now := time.Now()
	staleThreshold := 2 * ps.syncTimeout
	staleSessions := make([]peer.ID, 0)
	
	for peerID, session := range ps.pendingSyncs {
		if now.Sub(session.StartTime) > staleThreshold {
			staleSessions = append(staleSessions, peerID)
		}
	}
	
	for _, peerID := range staleSessions {
		delete(ps.pendingSyncs, peerID)
		log.Printf("📚 Cleaned up stale sync session for peer %s", peerID.ShortString())
	}
	
	if len(staleSessions) > 0 {
		log.Printf("📚 Cleaned up %d stale sync sessions", len(staleSessions))
	}
}

// exchangeDigests exchanges phonebook digests with a peer
func (ps *PhonebookSyncer) exchangeDigests(ctx context.Context, session *SyncSession) bool {
	// Create digest request
	// Create digest request with consistent timestamp
	timestamp := uint64(time.Now().UnixNano() / 1000000)
	digestReq := &models.PhonebookDigestRequest{
		NodeId:      ps.node.id,
		ClusterId:   ps.getClusterID(),
		Digest:      session.LocalDigest,
		Version:     ps.syncVersion,
		Datacenter:  ps.node.dataCenter,
		PeerCount:   uint32(len(ps.node.phonebook.GetPeers())),
		Timestamp:   timestamp,
		Signature:   ps.signDigestMessageWithTimestamp(session.LocalDigest, timestamp),
	}
	
	// Send digest request
	digestResp, err := ps.sendDigestRequest(session.PeerID, digestReq)
	if err != nil {
		log.Printf("❌ Failed to exchange digests with %s: %v", session.PeerID.ShortString(), err)
		return false
	}
	
	// Verify response signature
	if !ps.verifyDigestSignature(digestResp) {
		log.Printf("❌ Invalid digest signature from %s", session.PeerID.ShortString())
		return false
	}
	
	session.PeerDigest = digestResp.Digest
	session.Phase = SyncPhaseEnum.Delta
	
	// Check if phonebooks are already in sync
	if ps.digestsEqual(session.LocalDigest, session.PeerDigest) {
		log.Printf("📚 Phonebooks already in sync with %s", session.PeerID.ShortString())
		session.Phase = SyncPhaseEnum.Complete
		return true
	}
	
	log.Printf("📚 Digests differ with %s, proceeding to delta exchange", session.PeerID.ShortString())
	return true
}

// exchangeDeltas exchanges phonebook deltas with a peer
func (ps *PhonebookSyncer) exchangeDeltas(ctx context.Context, session *SyncSession) bool {
	if session.Phase == SyncPhaseEnum.Complete {
		return true // Already in sync
	}
	
	// Generate delta based on digest differences
	delta := ps.generateDelta(session.PeerDigest)
	if delta == nil {
		log.Printf("📚 No delta needed for %s", session.PeerID.ShortString())
		return true
	}
	
	// Create delta request
	deltaReq := &models.PhonebookDeltaRequest{
		NodeId:     ps.node.id,
		ClusterId:  ps.getClusterID(),
		Version:    ps.syncVersion,
		Delta:      delta,
		Timestamp:  uint64(time.Now().UnixNano() / 1000000),
		Signature:  ps.signDeltaMessage(delta),
	}
	
	// Send delta request
	deltaResp, err := ps.sendDeltaRequest(session.PeerID, deltaReq)
	if err != nil {
		log.Printf("❌ Failed to exchange deltas with %s: %v", session.PeerID.ShortString(), err)
		return false
	}
	
	// Verify response signature
	if !ps.verifyDeltaSignature(deltaResp) {
		log.Printf("❌ Invalid delta signature from %s", session.PeerID.ShortString())
		return false
	}
	
	// Store received delta for application
	session.Phase = SyncPhaseEnum.Apply
	
	// Apply received delta
	if deltaResp.Delta != nil {
		if err := ps.applyReceivedDelta(deltaResp.Delta, session.PeerID); err != nil {
			log.Printf("❌ Failed to apply delta from %s: %v", session.PeerID.ShortString(), err)
			return false
		}
	}
	
	session.DeltaSent = true
	session.DeltaRecv = deltaResp.Delta != nil
	
	log.Printf("📚 Delta exchange completed with %s (sent: %v, received: %v)",
		session.PeerID.ShortString(), session.DeltaSent, session.DeltaRecv)
	
	return true
}

// applyChanges applies the synchronized changes to the local phonebook
func (ps *PhonebookSyncer) applyChanges(ctx context.Context, session *SyncSession) bool {
	// Update local digest after changes
	ps.updateLocalDigest()
	ps.syncVersion++
	
	// Update datacenter information
	ps.updateDatacenterInfo()
	
	log.Printf("📚 Applied changes from sync with %s (new version: %d)",
		session.PeerID.ShortString(), ps.syncVersion)
	
	return true
}

// generateDelta generates a phonebook delta based on differences
func (ps *PhonebookSyncer) generateDelta(peerDigest []byte) *models.PhonebookDelta {
	if ps.node.phonebook == nil {
		return nil
	}
	
	peers := ps.node.phonebook.GetPeers()
	nodeDatacenter := ps.node.dataCenter
	
	var added []*models.PeerInfo
	var updated []*models.PeerInfo
	
	// For simplicity, include all peers from our datacenter in the delta
	// In a real implementation, this would be more sophisticated
	for _, p := range peers {
		// Prioritize same-datacenter peers
		if p.DataCenter == nodeDatacenter || nodeDatacenter == "" {
			peerInfo := &models.PeerInfo{
				NodeId:      p.ID,
				Address:     p.AddrInfo.String(),
				Datacenter:  p.DataCenter,
				TrustScore:  float32(p.TrustScore),
				LastSeen:    uint64(p.LastSeen.UnixNano() / 1000000),
				Tags:        ps.convertTags(p.Tags),
				Status:      p.Status,
			}
			
			// For this implementation, treat all as "added"
			// Real implementation would track actual differences
			added = append(added, peerInfo)
			
			// Limit delta size
			if len(added) >= ps.maxPeersPerDelta {
				break
			}
		}
	}
	
	if len(added) == 0 && len(updated) == 0 {
		return nil
	}
	
	delta := &models.PhonebookDelta{
		Version:     ps.syncVersion,
		Datacenter:  nodeDatacenter,
		Added:       added,
		Updated:     updated,
		Removed:     nil, // TODO: Implement removal tracking
		Timestamp:   uint64(time.Now().UnixNano() / 1000000),
	}
	
	log.Printf("📚 Generated delta: %d added, %d updated, datacenter: %s",
		len(added), len(updated), nodeDatacenter)
	
	return delta
}

// applyReceivedDelta applies a received delta to the local phonebook
func (ps *PhonebookSyncer) applyReceivedDelta(delta *models.PhonebookDelta, fromPeer peer.ID) error {
	if ps.node.phonebook == nil {
		return fmt.Errorf("phonebook not available")
	}
	
	addedCount := 0
	updatedCount := 0
	
	// Apply added peers
	for _, peerInfo := range delta.Added {
		if ps.shouldAcceptPeer(peerInfo, delta.Datacenter, fromPeer) {
			if ps.addPeerFromDelta(peerInfo) {
				addedCount++
			}
		}
	}
	
	// Apply updated peers
	for _, peerInfo := range delta.Updated {
		if ps.shouldAcceptPeer(peerInfo, delta.Datacenter, fromPeer) {
			if ps.updatePeerFromDelta(peerInfo) {
				updatedCount++
			}
		}
	}
	
	// Apply removed peers (TODO: implement)
	removedCount := 0
	
	// Update statistics
	ps.syncStats.mu.Lock()
	ps.syncStats.PeersAdded += uint64(addedCount)
	ps.syncStats.PeersUpdated += uint64(updatedCount)
	ps.syncStats.PeersRemoved += uint64(removedCount)
	ps.syncStats.mu.Unlock()
	
	log.Printf("📚 Applied delta from %s: %d added, %d updated, %d removed",
		fromPeer.ShortString(), addedCount, updatedCount, removedCount)
	
	return nil
}

// shouldAcceptPeer determines if a peer should be accepted from a delta
func (ps *PhonebookSyncer) shouldAcceptPeer(peerInfo *models.PeerInfo, senderDatacenter string, fromPeer peer.ID) bool {
	// Don't accept ourselves
	if peerInfo.NodeId == ps.node.id {
		return false
	}
	
	// Validate trust score
	if peerInfo.TrustScore < 0.1 {
		return false
	}
	
	// Prefer same-datacenter information
	nodeDatacenter := ps.node.dataCenter
	if nodeDatacenter != "" && senderDatacenter != "" {
		// Accept if same datacenter or from same datacenter peer
		if peerInfo.Datacenter == nodeDatacenter || senderDatacenter == nodeDatacenter {
			return true
		}
		
		// Be more selective about cross-datacenter peers
		if peerInfo.TrustScore < 0.7 {
			return false
		}
	}
	
	// Check if peer is too old
	lastSeen := time.Unix(0, int64(peerInfo.LastSeen)*1000000)
	if time.Since(lastSeen) > 7*24*time.Hour {
		return false
	}
	
	return true
}

// addPeerFromDelta adds a peer from delta information
func (ps *PhonebookSyncer) addPeerFromDelta(peerInfo *models.PeerInfo) bool {
	// Check if peer already exists
	if existing, exists := ps.node.phonebook.GetPeer(peerInfo.NodeId); exists {
		// Update if newer
		lastSeenDelta := time.Unix(0, int64(peerInfo.LastSeen)*1000000)
		if lastSeenDelta.After(existing.LastSeen) {
			return ps.updatePeerFromDelta(peerInfo)
		}
		return false
	}
	
	// Parse address information
	// For simplicity, create a basic peer entry
	// Real implementation would parse the full address
	peer := Peer{
		ID:         peerInfo.NodeId,
		Status:     peerInfo.Status,
		DataCenter: peerInfo.Datacenter,
		TrustScore: float64(peerInfo.TrustScore),
		LastSeen:   time.Unix(0, int64(peerInfo.LastSeen)*1000000),
		Tags:       ps.convertFromTags(peerInfo.Tags),
	}
	
	ps.node.phonebook.AddPeer(peer)
	log.Printf("📚 Added peer from delta: %s (datacenter: %s, trust: %.2f)",
		peerInfo.NodeId, peerInfo.Datacenter, peerInfo.TrustScore)
	
	return true
}

// updatePeerFromDelta updates a peer from delta information
func (ps *PhonebookSyncer) updatePeerFromDelta(peerInfo *models.PeerInfo) bool {
	existing, exists := ps.node.phonebook.GetPeer(peerInfo.NodeId)
	if !exists {
		return ps.addPeerFromDelta(peerInfo)
	}
	
	// Update fields if newer
	lastSeenDelta := time.Unix(0, int64(peerInfo.LastSeen)*1000000)
	if lastSeenDelta.After(existing.LastSeen) {
		updated := existing
		updated.Status = peerInfo.Status
		updated.DataCenter = peerInfo.Datacenter
		updated.TrustScore = float64(peerInfo.TrustScore)
		updated.LastSeen = lastSeenDelta
		updated.Tags = ps.convertFromTags(peerInfo.Tags)
		
		ps.node.phonebook.AddPeer(updated) // AddPeer also updates
		log.Printf("📚 Updated peer from delta: %s", peerInfo.NodeId)
		return true
	}
	
	return false
}

// Utility methods

func (ps *PhonebookSyncer) updateLocalDigest() {
	if ps.node.phonebook == nil {
		ps.localDigest = []byte{}
		return
	}
	
	peers := ps.node.phonebook.GetPeers()
	
	// Create deterministic digest from peer information
	hasher := sha256.New()
	
	// Sort peers by ID for deterministic ordering
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].ID < peers[j].ID
	})
	
	for _, peer := range peers {
		// Include key peer information in digest
		hasher.Write([]byte(peer.ID))
		hasher.Write([]byte(peer.DataCenter))
		hasher.Write([]byte(fmt.Sprintf("%.2f", peer.TrustScore)))
		hasher.Write([]byte(peer.LastSeen.Format(time.RFC3339)))
	}
	
	ps.localDigest = hasher.Sum(nil)
}

func (ps *PhonebookSyncer) updateDatacenterInfo() {
	if ps.node.phonebook == nil {
		return
	}
	
	peers := ps.node.phonebook.GetPeers()
	datacenterCounts := make(map[string]int)
	datacenterLastSeen := make(map[string]time.Time)
	
	for _, peer := range peers {
		dc := peer.DataCenter
		if dc == "" {
			dc = "default"
		}
		
		datacenterCounts[dc]++
		if peer.LastSeen.After(datacenterLastSeen[dc]) {
			datacenterLastSeen[dc] = peer.LastSeen
		}
	}
	
	// Update datacenter info
	for dc, count := range datacenterCounts {
		info := &DatacenterInfo{
			ID:           dc,
			PeerCount:    count,
			LastSeen:     datacenterLastSeen[dc],
			SyncPriority: ps.calculateDatacenterPriority(dc, count),
		}
		ps.datacenters[dc] = info
	}
}

func (ps *PhonebookSyncer) calculateDatacenterPriority(datacenter string, peerCount int) float64 {
	priority := 1.0
	
	// Higher priority for our own datacenter
	if datacenter == ps.node.dataCenter {
		priority *= 2.0
	}
	
	// Higher priority for datacenters with more peers
	priority *= float64(peerCount) / 10.0
	
	return priority
}

func (ps *PhonebookSyncer) getClusterID() string {
	if len(ps.node.clusters) > 0 {
		return ps.node.clusters[0]
	}
	return "default"
}

func (ps *PhonebookSyncer) digestsEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (ps *PhonebookSyncer) convertTags(tags Tags) map[string]string {
	result := make(map[string]string)
	for k, v := range tags {
		if str, ok := v.(string); ok {
			result[k] = str
		} else {
			result[k] = fmt.Sprintf("%v", v)
		}
	}
	return result
}

func (ps *PhonebookSyncer) convertFromTags(tags map[string]string) Tags {
	result := make(Tags)
	for k, v := range tags {
		result[k] = v
	}
	return result
}

// Network communication methods are implemented in phonebook_protocols.go

func (ps *PhonebookSyncer) signDigestMessage(digest []byte) []byte {
	// Use current timestamp
	timestamp := uint64(time.Now().UnixNano() / 1000000)
	return ps.signDigestMessageWithTimestamp(digest, timestamp)
}

func (ps *PhonebookSyncer) signDigestMessageWithTimestamp(digest []byte, timestamp uint64) []byte {
	if ps.node.privateKey == nil {
		log.Printf("⚠️ No private key available for signing")
		return nil
	}
	
	// Create message to sign: timestamp + digest + node_id
	message := fmt.Sprintf("%d:%x:%s", timestamp, digest, ps.node.id)
	
	signature, err := ps.node.privateKey.Sign([]byte(message))
	if err != nil {
		log.Printf("❌ Failed to sign digest message: %v", err)
		return nil
	}
	
	return signature
}

func (ps *PhonebookSyncer) signDeltaMessage(delta *models.PhonebookDelta) []byte {
	if ps.node.privateKey == nil {
		log.Printf("⚠️ No private key available for signing")
		return nil
	}
	
	// Create message to sign: version + timestamp + datacenter + node_id
	message := fmt.Sprintf("%d:%d:%s:%s", delta.Version, delta.Timestamp, delta.Datacenter, ps.node.id)
	
	signature, err := ps.node.privateKey.Sign([]byte(message))
	if err != nil {
		log.Printf("❌ Failed to sign delta message: %v", err)
		return nil
	}
	
	return signature
}

func (ps *PhonebookSyncer) verifyDigestSignature(resp *models.PhonebookDigestResponse) bool {
	if len(resp.Signature) == 0 {
		log.Printf("❌ Empty signature in digest response")
		return false
	}
	
	// Extract peer ID and get public key
	peerID, err := peer.Decode(resp.NodeId)
	if err != nil {
		log.Printf("❌ Invalid peer ID in digest response: %v", err)
		return false
	}
	
	// Get public key from libp2p host
	pubKey := ps.node.host.Peerstore().PubKey(peerID)
	if pubKey == nil {
		log.Printf("❌ Cannot get public key for peer %s", peerID.ShortString())
		return false
	}
	
	// Reconstruct message: timestamp + digest + node_id
	message := fmt.Sprintf("%d:%x:%s", resp.Timestamp, resp.Digest, resp.NodeId)
	
	// Verify signature
	valid, err := pubKey.Verify([]byte(message), resp.Signature)
	if err != nil {
		log.Printf("❌ Signature verification failed for peer %s: %v", peerID.ShortString(), err)
		return false
	}
	
	if !valid {
		log.Printf("❌ Invalid signature from peer %s", peerID.ShortString())
		return false
	}
	
	log.Printf("✅ Signature verified for peer %s", peerID.ShortString())
	return true
}

func (ps *PhonebookSyncer) verifyDeltaSignature(resp *models.PhonebookDeltaResponse) bool {
	if len(resp.Signature) == 0 {
		log.Printf("❌ Empty signature in delta response")
		return false
	}
	
	// Extract peer ID and get public key
	peerID, err := peer.Decode(resp.NodeId)
	if err != nil {
		log.Printf("❌ Invalid peer ID in delta response: %v", err)
		return false
	}
	
	// Get public key from libp2p host
	pubKey := ps.node.host.Peerstore().PubKey(peerID)
	if pubKey == nil {
		log.Printf("❌ Cannot get public key for peer %s", peerID.ShortString())
		return false
	}
	
	// Reconstruct message: version + timestamp + datacenter + node_id
	var datacenter string
	if resp.Delta != nil {
		datacenter = resp.Delta.Datacenter
	}
	message := fmt.Sprintf("%d:%d:%s:%s", resp.Version, resp.Timestamp, datacenter, resp.NodeId)
	
	// Verify signature
	valid, err := pubKey.Verify([]byte(message), resp.Signature)
	if err != nil {
		log.Printf("❌ Signature verification failed for peer %s: %v", peerID.ShortString(), err)
		return false
	}
	
	if !valid {
		log.Printf("❌ Invalid signature from peer %s", peerID.ShortString())
		return false
	}
	
	log.Printf("✅ Signature verified for peer %s", peerID.ShortString())
	return true
}

// GetSyncStatistics returns current synchronization statistics
func (ps *PhonebookSyncer) GetSyncStatistics() *SyncStatistics {
	ps.syncStats.mu.RLock()
	defer ps.syncStats.mu.RUnlock()
	
	return &SyncStatistics{
		TotalSyncs:       ps.syncStats.TotalSyncs,
		SuccessfulSyncs:  ps.syncStats.SuccessfulSyncs,
		FailedSyncs:      ps.syncStats.FailedSyncs,
		PeersAdded:       ps.syncStats.PeersAdded,
		PeersUpdated:     ps.syncStats.PeersUpdated,
		PeersRemoved:     ps.syncStats.PeersRemoved,
		BytesSent:        ps.syncStats.BytesSent,
		BytesReceived:    ps.syncStats.BytesReceived,
		LastSyncDuration: ps.syncStats.LastSyncDuration,
		AvgSyncDuration:  ps.syncStats.AvgSyncDuration,
	}
}

// GetDatacenterInfo returns information about known datacenters
func (ps *PhonebookSyncer) GetDatacenterInfo() map[string]*DatacenterInfo {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	
	result := make(map[string]*DatacenterInfo)
	for dc, info := range ps.datacenters {
		result[dc] = &DatacenterInfo{
			ID:           info.ID,
			PeerCount:    info.PeerCount,
			LastSeen:     info.LastSeen,
			Digest:       append([]byte(nil), info.Digest...),
			SyncPriority: info.SyncPriority,
		}
	}
	
	return result
}