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

// ReauthSubscriber handles automatic re-authentication when sessions become stale.
type ReauthSubscriber struct {
	authenticator *Authenticator
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

// WithReauthAuthenticator sets the authenticator.
func WithReauthAuthenticator(a *Authenticator) ReauthOption {
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

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.handleSessionStaleLoop(sessionStaleCh)
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
	r.logger.Info("re-authentication successful",
		zap.String("cluster", clusterPath),
		zap.String("peer", targetPeerID.String()))

	// Request sync to catch up on any missed updates during stale period
	// Use the peer we just re-authenticated with as preferred sync target
	r.eventBus.Publish(events.SyncRequested{
		BaseEvent:     events.NewBaseEvent(),
		ClusterPath:   clusterPath,
		Reason:        ReauthSyncReason,
		PreferredPeer: targetPeerID.String(),
	})
}

// tryAlternatePeer attempts re-auth with a different peer from phonebook.
func (r *ReauthSubscriber) tryAlternatePeer(clusterPath string) {
	session, _ := r.authenticator.GetSession(clusterPath)

	peers, err := r.phonebook.GetBestPeers(clusterPath, 3)
	if err != nil || len(peers) == 0 {
		return
	}

	for _, p := range peers {
		// Skip the original voucher we already tried
		if session != nil && p.NodeID == session.VoucherNodeID {
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
			r.logger.Info("re-authentication successful with alternate peer",
				zap.String("cluster", clusterPath),
				zap.String("peer", targetPeerID.String()))

			// Request sync to catch up on any missed updates
			// Use the peer we just re-authenticated with as preferred sync target
			r.eventBus.Publish(events.SyncRequested{
				BaseEvent:     events.NewBaseEvent(),
				ClusterPath:   clusterPath,
				Reason:        ReauthSyncReason,
				PreferredPeer: targetPeerID.String(),
			})
			return
		}

		r.logger.Debug("alternate peer re-auth failed",
			zap.String("peer", targetPeerID.String()),
			zap.Error(err))
	}

	r.logger.Error("all re-authentication attempts failed",
		zap.String("cluster", clusterPath))
}
