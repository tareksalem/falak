package auth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"go.uber.org/zap"

	"github.com/tareksalem/falak/node/auth/certs"
	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/phonebook"
	"github.com/tareksalem/falak/node/proto/authpb"
	"github.com/tareksalem/falak/shared"
)

const (
	// RenewalProtocolID is the protocol identifier for certificate renewal streams.
	RenewalProtocolID = "/falak/auth/renew/1.0"

	// DefaultRenewalCheckInterval is how often to check for certificates nearing expiry.
	DefaultRenewalCheckInterval = 24 * time.Hour

	// RenewalTimeout is the maximum time for a renewal handshake.
	RenewalTimeout = 30 * time.Second

	// MaxRenewalAttempts is the maximum number of peers to try for renewal.
	MaxRenewalAttempts = 3
)

// --- Renewal Event ---

const (
	// TypeCertificateRenewalNeeded is emitted when a certificate needs renewal.
	TypeCertificateRenewalNeeded = "cert.renewal_needed"
	// TypeCertificateRenewed is emitted when a certificate is successfully renewed.
	TypeCertificateRenewed = "cert.renewed"
)

// CertificateRenewalNeeded is emitted when a node's certificate is nearing expiry.
type CertificateRenewalNeeded struct {
	events.BaseEvent
	ClusterPath    string
	ExpiresAt      time.Time
	TimeRemaining  time.Duration
}

func (e CertificateRenewalNeeded) EventType() string { return TypeCertificateRenewalNeeded }

// CertificateRenewed is emitted when a certificate has been successfully renewed.
type CertificateRenewed struct {
	events.BaseEvent
	ClusterPath string
	RenewedBy   string // Peer ID that signed the renewal
}

func (e CertificateRenewed) EventType() string { return TypeCertificateRenewed }

// --- Renewal Stream Handler ---

// handleRenewalStream handles incoming renewal requests from peers.
// Verifies the current cert is valid, not expired, not revoked, then
// issues a new certificate using the provider.
func (a *Authenticator) handleRenewalStream(stream network.Stream) {
	defer stream.Close()

	remotePeer := stream.Conn().RemotePeer()
	a.logger.Debug("handling renewal stream", zap.String("peer", remotePeer.String()))

	// Read RenewalRequest
	var req authpb.RenewalRequest
	if err := shared.ReadProto(stream, &req); err != nil {
		a.logger.Error("failed to read renewal request", zap.Error(err))
		return
	}

	// Validate cluster path
	provider, ok := a.GetProvider(req.ClusterPath)
	if !ok {
		a.sendRenewalResponse(stream, false, nil, "unknown cluster")
		return
	}

	if !provider.CanSign() {
		a.sendRenewalResponse(stream, false, nil, "this node cannot sign certificates")
		return
	}

	// Verify the current certificate
	if req.CurrentCert == nil {
		a.sendRenewalResponse(stream, false, nil, "no current certificate provided")
		return
	}

	currentCert := &certs.Certificate{NodeCertificate: req.CurrentCert}

	// Check if the current cert's node has been revoked
	fingerprint, err := certs.CertFingerprint(currentCert)
	if err == nil && a.IsCertRevoked(req.ClusterPath, fingerprint) {
		a.sendRenewalResponse(stream, false, nil, "certificate has been revoked")
		return
	}

	if a.IsNodeRevoked(req.ClusterPath, currentCert.NodeId) {
		a.sendRenewalResponse(stream, false, nil, "node has been revoked")
		return
	}

	// Verify the node is in our phonebook (known member)
	if a.phonebook != nil {
		entry, err := a.phonebook.Get(currentCert.NodeId, req.ClusterPath)
		if err != nil || entry == nil {
			a.sendRenewalResponse(stream, false, nil, "node not found in phonebook")
			return
		}
	}

	// Determine which public key to use for the new certificate
	var nodePubKey crypto.PubKey
	if len(req.NewPublicKey) > 0 {
		// Key rotation — use the new public key
		nodePubKey, err = crypto.UnmarshalPublicKey(req.NewPublicKey)
		if err != nil {
			a.sendRenewalResponse(stream, false, nil, "invalid new public key")
			return
		}
	} else {
		// Same key — use the key from current cert
		nodePubKey, err = crypto.UnmarshalPublicKey(currentCert.NodePublicKey)
		if err != nil {
			a.sendRenewalResponse(stream, false, nil, "invalid current public key")
			return
		}
	}

	// Sign a new certificate via the provider
	newCert, err := provider.SignForNode(currentCert.NodeId, nodePubKey, req.ClusterPath)
	if err != nil {
		a.logger.Error("failed to sign renewal certificate",
			zap.String("nodeId", currentCert.NodeId),
			zap.Error(err))
		a.sendRenewalResponse(stream, false, nil, "failed to sign new certificate")
		return
	}

	a.sendRenewalResponse(stream, true, newCert, "")

	a.logger.Info("renewed certificate for peer",
		zap.String("nodeId", currentCert.NodeId),
		zap.String("cluster", req.ClusterPath))
}

// sendRenewalResponse sends a RenewalResponse message.
func (a *Authenticator) sendRenewalResponse(stream network.Stream, success bool, cert *certs.Certificate, errMsg string) {
	resp := &authpb.RenewalResponse{
		Success: success,
		Error:   errMsg,
	}
	if cert != nil {
		resp.NewCert = cert.NodeCertificate
	}
	if err := shared.WriteProto(stream, resp); err != nil {
		a.logger.Error("failed to send renewal response", zap.Error(err))
	}
}

// RegisterRenewalProtocol registers the renewal stream handler.
func (a *Authenticator) RegisterRenewalProtocol() {
	a.host.SetStreamHandler(RenewalProtocolID, a.handleRenewalStream)
}

// --- Renewal Subscriber ---

// RenewalSubscriber periodically checks certificates for expiry and requests renewal.
type RenewalSubscriber struct {
	authenticator *Authenticator
	phonebook     phonebook.IPhonebook
	eventBus      events.Bus
	logger        *zap.Logger
	checkInterval time.Duration

	parentCtx context.Context
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// RenewalOption configures a RenewalSubscriber.
type RenewalOption func(*RenewalSubscriber)

// WithRenewalContext sets the parent context.
func WithRenewalContext(ctx context.Context) RenewalOption {
	return func(r *RenewalSubscriber) {
		r.parentCtx = ctx
	}
}

// WithRenewalAuthenticator sets the authenticator.
func WithRenewalAuthenticator(a *Authenticator) RenewalOption {
	return func(r *RenewalSubscriber) {
		r.authenticator = a
	}
}

// WithRenewalPhonebook sets the phonebook.
func WithRenewalPhonebook(pb phonebook.IPhonebook) RenewalOption {
	return func(r *RenewalSubscriber) {
		r.phonebook = pb
	}
}

// WithRenewalEventBus sets the event bus.
func WithRenewalEventBus(bus events.Bus) RenewalOption {
	return func(r *RenewalSubscriber) {
		r.eventBus = bus
	}
}

// WithRenewalLogger sets the logger.
func WithRenewalLogger(logger *zap.Logger) RenewalOption {
	return func(r *RenewalSubscriber) {
		r.logger = logger
	}
}

// WithRenewalCheckInterval sets how often to check for expiring certs.
func WithRenewalCheckInterval(d time.Duration) RenewalOption {
	return func(r *RenewalSubscriber) {
		r.checkInterval = d
	}
}

// NewRenewalSubscriber creates a new certificate renewal subscriber.
func NewRenewalSubscriber(opts ...RenewalOption) *RenewalSubscriber {
	r := &RenewalSubscriber{
		logger:        zap.NewNop(),
		checkInterval: DefaultRenewalCheckInterval,
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

// Start begins the periodic renewal check loop.
func (r *RenewalSubscriber) Start() error {
	if r.authenticator == nil {
		return fmt.Errorf("authenticator is required")
	}
	if r.eventBus == nil {
		return fmt.Errorf("event bus is required")
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.checkLoop()
	}()

	r.logger.Debug("renewal subscriber started",
		zap.Duration("checkInterval", r.checkInterval))
	return nil
}

// Stop stops the renewal subscriber.
func (r *RenewalSubscriber) Stop() {
	r.cancel()
	r.wg.Wait()
	r.logger.Debug("renewal subscriber stopped")
}

// checkLoop periodically checks all cluster certificates for expiry.
func (r *RenewalSubscriber) checkLoop() {
	// Initial check shortly after startup
	initialDelay := time.NewTimer(30 * time.Second)
	defer initialDelay.Stop()

	select {
	case <-r.ctx.Done():
		return
	case <-initialDelay.C:
		r.checkAllClusters()
	}

	ticker := time.NewTicker(r.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.checkAllClusters()
		}
	}
}

// checkAllClusters checks certificates across all joined clusters.
func (r *RenewalSubscriber) checkAllClusters() {
	r.authenticator.providerMu.RLock()
	clusterPaths := make([]string, 0, len(r.authenticator.providers))
	for cp := range r.authenticator.providers {
		clusterPaths = append(clusterPaths, cp)
	}
	r.authenticator.providerMu.RUnlock()

	for _, clusterPath := range clusterPaths {
		r.checkClusterCert(clusterPath)
	}
}

// checkClusterCert checks if a specific cluster's certificate needs renewal.
func (r *RenewalSubscriber) checkClusterCert(clusterPath string) {
	provider, ok := r.authenticator.GetProvider(clusterPath)
	if !ok {
		return
	}

	cert, err := provider.NodeCertificate()
	if err != nil {
		return // No cert yet, nothing to renew
	}

	if !cert.NeedsRenewal() {
		return
	}

	r.logger.Info("certificate nearing expiry, requesting renewal",
		zap.String("cluster", clusterPath),
		zap.Duration("timeRemaining", cert.TimeUntilExpiry()))

	// Emit event
	r.eventBus.Publish(CertificateRenewalNeeded{
		BaseEvent:     events.NewBaseEvent(),
		ClusterPath:   clusterPath,
		ExpiresAt:     cert.ExpiresAt.AsTime(),
		TimeRemaining: cert.TimeUntilExpiry(),
	})

	// Attempt renewal
	r.attemptRenewal(clusterPath, cert)
}

// attemptRenewal tries to renew a certificate by contacting healthy peers.
func (r *RenewalSubscriber) attemptRenewal(clusterPath string, currentCert *certs.Certificate) {
	if r.phonebook == nil {
		r.logger.Warn("no phonebook available for renewal peer lookup",
			zap.String("cluster", clusterPath))
		return
	}

	peers, err := r.phonebook.GetBestPeers(clusterPath, MaxRenewalAttempts)
	if err != nil || len(peers) == 0 {
		r.logger.Error("no peers available for certificate renewal",
			zap.String("cluster", clusterPath))
		return
	}

	for _, p := range peers {
		// Skip ourselves
		if p.NodeID == r.authenticator.host.ID().String() {
			continue
		}

		targetPeerID, err := peer.Decode(p.NodeID)
		if err != nil {
			continue
		}

		ctx, cancel := context.WithTimeout(r.ctx, RenewalTimeout)
		err = r.requestRenewal(ctx, clusterPath, targetPeerID, currentCert)
		cancel()

		if err == nil {
			r.logger.Info("certificate renewed successfully",
				zap.String("cluster", clusterPath),
				zap.String("peer", targetPeerID.String()))

			r.eventBus.Publish(CertificateRenewed{
				BaseEvent:   events.NewBaseEvent(),
				ClusterPath: clusterPath,
				RenewedBy:   targetPeerID.String(),
			})
			return
		}

		r.logger.Debug("renewal attempt failed",
			zap.String("peer", targetPeerID.String()),
			zap.Error(err))
	}

	r.logger.Error("all renewal attempts failed",
		zap.String("cluster", clusterPath))
}

// requestRenewal sends a renewal request to a specific peer.
func (r *RenewalSubscriber) requestRenewal(ctx context.Context, clusterPath string, targetPeer peer.ID, currentCert *certs.Certificate) error {
	stream, err := r.authenticator.host.NewStream(ctx, targetPeer, RenewalProtocolID)
	if err != nil {
		return fmt.Errorf("failed to open renewal stream: %w", err)
	}
	defer stream.Close()

	// Send renewal request
	req := &authpb.RenewalRequest{
		CurrentCert: currentCert.NodeCertificate,
		ClusterPath: clusterPath,
	}

	if err := shared.WriteProto(stream, req); err != nil {
		return fmt.Errorf("failed to send renewal request: %w", err)
	}

	// Read response
	var resp authpb.RenewalResponse
	if err := shared.ReadProto(stream, &resp); err != nil {
		return fmt.Errorf("failed to read renewal response: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("renewal rejected: %s", resp.Error)
	}

	if resp.NewCert == nil {
		return fmt.Errorf("renewal response has no certificate")
	}

	// Save the new certificate via provider
	provider, ok := r.authenticator.GetProvider(clusterPath)
	if !ok {
		return fmt.Errorf("no provider for cluster %s", clusterPath)
	}

	newCert := &certs.Certificate{NodeCertificate: resp.NewCert}
	if err := provider.SaveNodeCertificate(newCert); err != nil {
		return fmt.Errorf("failed to save renewed certificate: %w", err)
	}

	return nil
}
