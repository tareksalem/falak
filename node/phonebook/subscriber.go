package phonebook

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/shared"
)

// Subscriber handles event subscriptions for the phonebook.
type Subscriber struct {
	phonebook IPhonebook
	eventBus  events.Bus
	logger    *zap.Logger
	selfID    string // optional — when an announcement targets self, skip PendingAuth
	parentCtx context.Context // Set via WithContext option
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// SubscriberOption configures a Subscriber.
type SubscriberOption func(*Subscriber)

// WithContext sets the parent context for the subscriber.
// The subscriber will derive its internal context from this parent,
// enabling proper cancellation propagation from the parent component.
func WithContext(ctx context.Context) SubscriberOption {
	return func(s *Subscriber) {
		s.parentCtx = ctx
	}
}

// WithPhonebook sets the phonebook instance.
func WithPhonebook(pb IPhonebook) SubscriberOption {
	return func(s *Subscriber) {
		s.phonebook = pb
	}
}

// WithEventBus sets the event bus.
func WithEventBus(bus events.Bus) SubscriberOption {
	return func(s *Subscriber) {
		s.eventBus = bus
	}
}

// WithSubscriberLogger sets the logger.
func WithSubscriberLogger(logger *zap.Logger) SubscriberOption {
	return func(s *Subscriber) {
		s.logger = logger
	}
}

// WithSubscriberSelfID tells the subscriber which peer ID belongs to us.
// When an inbound announcement names self (e.g. the first-node bootstrap
// path that re-publishes our own entry), the subscriber adds it as Active
// instead of PendingAuth — we are by definition already authenticated.
func WithSubscriberSelfID(id string) SubscriberOption {
	return func(s *Subscriber) {
		s.selfID = id
	}
}

// NewSubscriber creates a new phonebook event subscriber.
func NewSubscriber(opts ...SubscriberOption) *Subscriber {
	s := &Subscriber{
		logger: zap.NewNop(),
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

// Start begins listening for events.
func (s *Subscriber) Start() error {
	// Validate required dependencies
	if s.phonebook == nil {
		return errors.New("phonebook is required")
	}
	if s.eventBus == nil {
		return errors.New("eventBus is required")
	}

	// Subscribe to auth events
	newMemberAnnouncedCh := s.eventBus.Subscribe(events.TypeNewMemberAnnounced)
	newMemberReceivedCh := s.eventBus.Subscribe(events.TypeNewMemberReceived)
	clusterMembersCh := s.eventBus.Subscribe(events.TypeClusterMembersReceived)

	s.wg.Add(3)
	go func() {
		defer s.wg.Done()
		s.handleNewMemberAnnounced(newMemberAnnouncedCh)
	}()
	go func() {
		defer s.wg.Done()
		s.handleNewMemberReceived(newMemberReceivedCh)
	}()
	go func() {
		defer s.wg.Done()
		s.handleClusterMembersReceived(clusterMembersCh)
	}()

	s.logger.Debug("phonebook subscriber started")
	return nil
}

// Stop stops listening for events.
func (s *Subscriber) Stop() {
	s.cancel()
	s.wg.Wait()
	s.logger.Debug("phonebook subscriber stopped")
}

// handleNewMemberAnnounced processes NewMemberAnnounced events (we authenticated a new member).
// New entries enter PendingAuth: the voucher just signed the cert but the
// joiner's libp2p mesh / ping handler may not be ready yet, so SWIM
// should hold off probing until the grace window elapses or the auth
// handler explicitly promotes (Bug #13).
func (s *Subscriber) handleNewMemberAnnounced(ch <-chan events.Event) {
	for {
		select {
		case <-s.ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}

			e, ok := event.(events.NewMemberAnnounced)
			if !ok {
				continue
			}

			s.addOrUpdateEntry(e.NodeID, e.ClusterPath, e.Addresses, e.PublicKey, e.Capabilities, NodeStatusEnum.PendingAuth())
		}
	}
}

// handleNewMemberReceived processes NewMemberReceived events (received via
// the gossipsub Step 2 broadcast). The announcement is voucher-signed but
// we have no direct libp2p connection to the new member yet, so SWIM
// probes would fail until libp2p dials. Hence PendingAuth (Bug #13).
func (s *Subscriber) handleNewMemberReceived(ch <-chan events.Event) {
	for {
		select {
		case <-s.ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}

			e, ok := event.(events.NewMemberReceived)
			if !ok {
				continue
			}

			s.addOrUpdateEntry(e.NodeID, e.ClusterPath, e.Addresses, e.PublicKey, e.Capabilities, NodeStatusEnum.PendingAuth())
		}
	}
}

// handleClusterMembersReceived processes ClusterMembersReceived events
// emitted by the joiner side after a successful auth handshake. These
// members come from the voucher's AuthComplete payload — the cluster has
// authenticated them, and we hold a working libp2p connection to the
// voucher itself. Adding them as Active lets the syncer's GetBestPeers
// pick the voucher for the initial post-join sync; using PendingAuth here
// would starve the syncer until the SWIM grace window expired.
func (s *Subscriber) handleClusterMembersReceived(ch <-chan events.Event) {
	for {
		select {
		case <-s.ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}

			e, ok := event.(events.ClusterMembersReceived)
			if !ok {
				continue
			}

			for _, member := range e.Members {
				s.addOrUpdateEntry(member.NodeID, e.ClusterPath, member.Addresses, member.PublicKey, member.Capabilities, NodeStatusEnum.Active())
			}

			s.logger.Info("stored cluster members",
				zap.String("cluster", e.ClusterPath),
				zap.Int("count", len(e.Members)))
		}
	}
}

// MetadataKeyNodeName is the well-known capabilities.Metadata key
// carrying the operator-assigned friendly node name. Kept in sync with
// auth.MetadataKeyNodeName — duplicated here to avoid pulling the auth
// package into the phonebook subscriber's import graph.
const MetadataKeyNodeName = "node_name"

// addOrUpdateEntry adds or updates a phonebook entry. initialStatus is the
// status used when the row is freshly inserted; updates always preserve the
// existing row's status (so a stale re-announcement can never demote an
// already-promoted peer).
func (s *Subscriber) addOrUpdateEntry(nodeID, clusterPath string, addresses []string, publicKey []byte, caps *events.Capabilities, initialStatus NodeStatus) {
	cp, _ := shared.ParseClusterPath(clusterPath)

	entry := &Entry{
		NodeID:      nodeID,
		ClusterPath: clusterPath,
		PublicKey:   publicKey,
		Addresses:   addresses,
		Region:      cp.Region,
		Datacenter:  cp.Datacenter,
		FirstSeen:   time.Now(),
		LastSeen:    time.Now(),
		Status:      initialStatus,
	}

	// Self is, by definition, already authenticated — don't park our
	// own entry in PendingAuth (which SWIM would skip forever since
	// it never probes self).
	if s.selfID != "" && nodeID == s.selfID {
		entry.Status = NodeStatusEnum.Active()
	}

	if caps != nil {
		entry.Capabilities = &Capabilities{
			CPUCores:   caps.CPUCores,
			MemoryMB:   caps.MemoryMB,
			DiskGB:     caps.DiskGB,
			Datacenter: caps.Datacenter,
			Tags:       caps.Tags,
			Metadata:   caps.Metadata,
		}
		if name, ok := caps.Metadata[MetadataKeyNodeName]; ok {
			entry.Name = name
		}
	}

	existing, _ := s.phonebook.Get(nodeID, clusterPath)
	if existing != nil {
		// Preserve fields that the announcement doesn't carry. Critical
		// for Status: a re-announcement (delta sync, periodic gossip)
		// must not flip an Active peer back to PendingAuth, nor undo a
		// Departed marker — those are derived from local observation,
		// not the announcement payload.
		entry.Status = existing.Status
		entry.FirstSeen = existing.FirstSeen
		entry.ReliabilityScore = existing.ReliabilityScore
		entry.LastProbeTime = existing.LastProbeTime
		entry.LastProbeSuccess = existing.LastProbeSuccess
		entry.ConnectionAttempts = existing.ConnectionAttempts
		entry.ConnectionSuccess = existing.ConnectionSuccess
		entry.SuccessRate = existing.SuccessRate
		entry.ConsecutiveFails = existing.ConsecutiveFails
		entry.LastConnected = existing.LastConnected
		entry.IsConnected = existing.IsConnected
		if err := s.phonebook.Update(entry); err != nil {
			s.logger.Error("failed to update phonebook entry",
				zap.String("nodeId", nodeID),
				zap.Error(err))
		}
	} else {
		if err := s.phonebook.Add(entry); err != nil {
			s.logger.Error("failed to add phonebook entry",
				zap.String("nodeId", nodeID),
				zap.Error(err))
		}
	}

	s.logger.Debug("phonebook entry updated",
		zap.String("nodeId", nodeID),
		zap.String("cluster", clusterPath))
}
