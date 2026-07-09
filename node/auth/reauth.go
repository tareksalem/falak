package auth

import (
	"context"
	"errors"
	"sync"

	"github.com/libp2p/go-libp2p/core/peer"
	"go.uber.org/zap"

	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/phonebook"
)

const (
	// ReauthSyncReason is the reason used when requesting sync after re-authentication.
	ReauthSyncReason = "post_reauth"
)

// Reauthenticator is the minimal authenticator surface the ReauthSubscriber
// drives. *Authenticator satisfies it. Depending on the interface (not the
// concrete type) keeps the subscriber unit-testable without a libp2p host.
type Reauthenticator interface {
	// GetSession returns the current session for a cluster.
	GetSession(clusterPath string) (*Session, error)
	// RefreshSession refreshes a stale session back to authenticated.
	RefreshSession(clusterPath string)
	// Authenticate performs (re)authentication against a specific peer.
	Authenticate(ctx context.Context, clusterPath string, targetPeer peer.ID) (*Session, error)
}

// ReauthSubscriber handles automatic re-authentication when sessions become stale.
type ReauthSubscriber struct {
	authenticator Reauthenticator
	phonebook     phonebook.IPhonebook
	eventBus      events.Bus
	logger        *zap.Logger

	parentCtx context.Context
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// ReauthOption configures a ReauthSubscriber.
type ReauthOption func(*ReauthSubscriber)

// WithReauthContext sets the parent context for the subscriber.
func WithReauthContext(ctx context.Context) ReauthOption {
	return func(r *ReauthSubscriber) {
		r.parentCtx = ctx
	}
}

// WithReauthAuthenticator sets the authenticator. Accepts the Reauthenticator
// interface so tests can inject a fake; production passes *Authenticator.
func WithReauthAuthenticator(a Reauthenticator) ReauthOption {
	return func(r *ReauthSubscriber) {
		r.authenticator = a
	}
}

// WithReauthPhonebook sets the phonebook.
func WithReauthPhonebook(pb phonebook.IPhonebook) ReauthOption {
	return func(r *ReauthSubscriber) {
		r.phonebook = pb
	}
}

// WithReauthEventBus sets the event bus.
func WithReauthEventBus(bus events.Bus) ReauthOption {
	return func(r *ReauthSubscriber) {
		r.eventBus = bus
	}
}

// WithReauthLogger sets the logger.
func WithReauthLogger(logger *zap.Logger) ReauthOption {
	return func(r *ReauthSubscriber) {
		r.logger = logger
	}
}

// NewReauthSubscriber creates a new re-authentication subscriber.
func NewReauthSubscriber(opts ...ReauthOption) *ReauthSubscriber {
	r := &ReauthSubscriber{
		logger: zap.NewNop(),
	}

	for _, opt := range opts {
		opt(r)
	}

	if r.parentCtx != nil {
		r.ctx, r.cancel = context.WithCancel(r.parentCtx)
	} else {
		r.ctx, r.cancel = context.WithCancel(context.Background())
	}

	return r
}

// Start begins listening for SessionStale events.
func (r *ReauthSubscriber) Start() error {
	if r.authenticator == nil {
		return errors.New("authenticator is required")
	}
	if r.phonebook == nil {
		return errors.New("phonebook is required")
	}
	if r.eventBus == nil {
		return errors.New("eventBus is required")
	}

	sessionStaleCh := r.eventBus.Subscribe(events.TypeSessionStale)
	reauthPeerCh := r.eventBus.Subscribe(events.TypeReauthWithPeer)

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.handleSessionStaleLoop(sessionStaleCh)
	}()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.handleReauthWithPeerLoop(reauthPeerCh)
	}()

	r.logger.Debug("reauth subscriber started")
	return nil
}

// Stop stops the subscriber.
func (r *ReauthSubscriber) Stop() {
	r.cancel()
	r.wg.Wait()
	r.logger.Debug("reauth subscriber stopped")
}

// handleSessionStaleLoop processes SessionStale events.
func (r *ReauthSubscriber) handleSessionStaleLoop(ch <-chan events.Event) {
	for {
		select {
		case <-r.ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}

			e, ok := event.(events.SessionStale)
			if !ok {
				continue
			}

			r.attemptReauth(e.ClusterPath)
		}
	}
}

// handleReauthWithPeerLoop processes ReauthWithPeerRequested events emitted
// by the reconnector after it re-dials a specific known peer.
func (r *ReauthSubscriber) handleReauthWithPeerLoop(ch <-chan events.Event) {
	for {
		select {
		case <-r.ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}

			e, ok := event.(events.ReauthWithPeerRequested)
			if !ok {
				continue
			}

			r.attemptReauthWithPeer(e.ClusterPath, e.PeerID)
		}
	}
}

// attemptReauthWithPeer drives re-authentication targeting a SPECIFIC peer
// that the reconnector just re-dialed. It pins the requested peer first and
// only falls back to other phonebook peers if that specific peer refuses —
// this is the reconnector-driven twin of attemptReauth, whose target is the
// stale session's original voucher instead of a freshly re-dialed peer.
func (r *ReauthSubscriber) attemptReauthWithPeer(clusterPath, peerID string) {
	r.logger.Info("attempting re-authentication with re-dialed peer",
		zap.String("cluster", clusterPath),
		zap.String("peer", peerID))

	targetPeerID, err := peer.Decode(peerID)
	if err != nil {
		r.logger.Warn("invalid peer ID in reauth-with-peer request, falling back to best peers",
			zap.String("cluster", clusterPath),
			zap.String("peer", peerID),
			zap.Error(err))
		r.tryAlternatePeerExcluding(clusterPath, "")
		return
	}

	ctx, cancel := context.WithTimeout(r.ctx, AuthTimeout)
	_, err = r.authenticator.Authenticate(ctx, clusterPath, targetPeerID)
	cancel()
	if err != nil {
		r.logger.Warn("re-authentication with re-dialed peer failed, trying alternates",
			zap.String("cluster", clusterPath),
			zap.String("peer", targetPeerID.String()),
			zap.Error(err))
		// Fall back to any other phonebook peer, excluding the one we
		// just failed on so we don't immediately retry it.
		r.tryAlternatePeerExcluding(clusterPath, targetPeerID.String())
		return
	}

	r.onReauthSuccess(clusterPath, targetPeerID)
}

// attemptReauth attempts to re-authenticate to a cluster.
func (r *ReauthSubscriber) attemptReauth(clusterPath string) {
	r.logger.Info("attempting re-authentication",
		zap.String("cluster", clusterPath))

	// Get the original voucher from the stale session
	session, err := r.authenticator.GetSession(clusterPath)
	if err != nil {
		r.logger.Error("failed to get session for re-auth",
			zap.String("cluster", clusterPath),
			zap.Error(err))
		return
	}

	// If no voucher (first node in cluster), just refresh the session
	// First nodes don't need to re-authenticate with anyone
	if session.VoucherNodeID == "" {
		r.logger.Info("first node session refresh (no voucher)",
			zap.String("cluster", clusterPath))
		r.authenticator.RefreshSession(clusterPath)
		return
	}

	// Try the original voucher first
	targetPeerID, err := peer.Decode(session.VoucherNodeID)
	if err != nil {
		r.logger.Warn("invalid voucher peer ID, will try another peer",
			zap.String("voucher", session.VoucherNodeID),
			zap.Error(err))
		targetPeerID = ""
	}

	// If voucher is invalid or fails, try to find another peer
	if targetPeerID == "" {
		peers, err := r.phonebook.GetBestPeers(clusterPath, 3)
		if err != nil || len(peers) == 0 {
			r.logger.Error("no peers available for re-authentication",
				zap.String("cluster", clusterPath))
			return
		}

		// Find a peer that isn't ourselves
		for _, p := range peers {
			pid, err := peer.Decode(p.NodeID)
			if err != nil {
				continue
			}
			targetPeerID = pid
			break
		}

		if targetPeerID == "" {
			r.logger.Error("no valid peers available for re-authentication",
				zap.String("cluster", clusterPath))
			return
		}
	}

	// Attempt authentication
	ctx, cancel := context.WithTimeout(r.ctx, AuthTimeout)
	defer cancel()

	_, err = r.authenticator.Authenticate(ctx, clusterPath, targetPeerID)
	if err != nil {
		r.logger.Warn("re-authentication failed",
			zap.String("cluster", clusterPath),
			zap.String("peer", targetPeerID.String()),
			zap.Error(err))

		// Try with a different peer if original voucher failed
		if targetPeerID.String() == session.VoucherNodeID {
			r.tryAlternatePeer(clusterPath)
		}
		return
	}

	// Success - session is already updated by Authenticate()
	r.onReauthSuccess(clusterPath, targetPeerID)
}

// onReauthSuccess logs a successful re-auth and requests a catch-up sync
// from the peer we just re-authenticated with. Shared by every re-auth
// entry point (stale-session, alternate-peer fallback, reconnector-pinned).
func (r *ReauthSubscriber) onReauthSuccess(clusterPath string, targetPeerID peer.ID) {
	r.logger.Info("re-authentication successful",
		zap.String("cluster", clusterPath),
		zap.String("peer", targetPeerID.String()))

	// Request sync to catch up on any missed updates during the stale
	// period. Use the peer we just re-authenticated with as the preferred
	// sync target.
	r.eventBus.Publish(events.SyncRequested{
		BaseEvent:     events.NewBaseEvent(),
		ClusterPath:   clusterPath,
		Reason:        ReauthSyncReason,
		PreferredPeer: targetPeerID.String(),
	})
}

// tryAlternatePeer attempts re-auth with a different peer from the
// phonebook, skipping the stale session's original voucher.
func (r *ReauthSubscriber) tryAlternatePeer(clusterPath string) {
	session, _ := r.authenticator.GetSession(clusterPath)
	excludeNodeID := ""
	if session != nil {
		excludeNodeID = session.VoucherNodeID
	}
	r.tryAlternatePeerExcluding(clusterPath, excludeNodeID)
}

// tryAlternatePeerExcluding attempts re-auth with any phonebook peer other
// than excludeNodeID (the peer already tried and failed). excludeNodeID may
// be empty to consider every peer.
func (r *ReauthSubscriber) tryAlternatePeerExcluding(clusterPath, excludeNodeID string) {
	peers, err := r.phonebook.GetBestPeers(clusterPath, 3)
	if err != nil || len(peers) == 0 {
		return
	}

	for _, p := range peers {
		// Skip the peer we already tried.
		if excludeNodeID != "" && p.NodeID == excludeNodeID {
			continue
		}

		targetPeerID, err := peer.Decode(p.NodeID)
		if err != nil {
			continue
		}

		ctx, cancel := context.WithTimeout(r.ctx, AuthTimeout)
		_, err = r.authenticator.Authenticate(ctx, clusterPath, targetPeerID)
		cancel()

		if err == nil {
			r.onReauthSuccess(clusterPath, targetPeerID)
			return
		}

		r.logger.Debug("alternate peer re-auth failed",
			zap.String("peer", targetPeerID.String()),
			zap.Error(err))
	}

	r.logger.Error("all re-authentication attempts failed",
		zap.String("cluster", clusterPath))
}
