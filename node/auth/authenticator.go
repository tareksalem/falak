// Package auth provides authentication for joining clusters.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/node/auth/certs"
	"github.com/tareksalem/falak/node/auth/clusterkeys"
	"github.com/tareksalem/falak/node/auth/nodekeys"
	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/internal/ratelimit"
	"github.com/tareksalem/falak/node/internal/signing"
	"github.com/tareksalem/falak/node/phonebook"
	"github.com/tareksalem/falak/node/proto/authpb"
	"github.com/tareksalem/falak/shared"
)

const (
	// ProtocolID is the protocol identifier for authentication streams.
	ProtocolID = "/falak/auth/1.0"

	// AuthTimeout is the maximum time for authentication handshake.
	AuthTimeout = 30 * time.Second

	// MaxMessageAge is the maximum age of a PubSub message before it's rejected.
	MaxMessageAge = 30 * time.Second

	// MinPSKLength is the minimum required PSK length in bytes.
	MinPSKLength = 32

	// DefaultSessionStaleThreshold is the default time after which a session is considered stale.
	DefaultSessionStaleThreshold = 5 * time.Minute

	// DefaultStaleCheckInterval is the default interval for checking session staleness.
	DefaultStaleCheckInterval = 1 * time.Minute

	// DefaultMaxStaleDuration is the maximum time a session can remain stale before being removed.
	DefaultMaxStaleDuration = 15 * time.Minute

	// Step2MaxRetries is the number of retries for Step 2 broadcast.
	Step2MaxRetries = 5

	// Step2RetryDelay is the delay between Step 2 broadcast retries.
	Step2RetryDelay = 500 * time.Millisecond

	// DefaultAuthRateLimit is the max auth requests per peer per window.
	DefaultAuthRateLimit = 10

	// DefaultAuthRateWindow is the rate limiting window.
	DefaultAuthRateWindow = 1 * time.Minute

	// ConfirmationTimeout is how long to wait for auth confirmations from other nodes.
	// Set to 15s to allow GossipSub mesh to form in larger clusters.
	ConfirmationTimeout = 15 * time.Second

	// MinConfirmationsRequired is the minimum number of confirmations needed (if cluster has enough nodes).
	MinConfirmationsRequired = 1
)

var (
	// ErrPSKTooShort is returned when PSK is shorter than MinPSKLength.
	ErrPSKTooShort = errors.New("PSK must be at least 32 bytes")

	// ErrSessionNotFound is returned when session doesn't exist.
	ErrSessionNotFound = errors.New("session not found")

	// ErrSessionStale is returned when session is stale.
	ErrSessionStale = errors.New("session is stale")
)

// Authenticator handles cluster authentication.
type Authenticator struct {
	host       host.Host
	pubsub     *pubsub.PubSub
	eventBus   events.Bus
	phonebook  phonebook.IPhonebook // Read-only access
	privateKey crypto.PrivKey
	logger     *zap.Logger
	dataDir    string // Data directory for persistent storage

	// Derived HMAC keys for challenge-response (protected by hmacKeyMu).
	// Raw PSKs are zeroed after deriving these keys and the cluster root keys.
	hmacKeyMu   sync.RWMutex
	hmacKeys    map[string][]byte // clusterPath -> derived HMAC key

	// Cluster root keys (derived from PSK, protected by rootKeyMu)
	rootKeyMu       sync.RWMutex
	clusterRootKeys map[string]*clusterkeys.ClusterRootKey // clusterPath -> root key

	// Node keys manager for per-cluster keys
	nodeKeyManager *nodekeys.Manager

	// Certificate providers per cluster (clusterPath → provider).
	// Populated at startup — auto or external based on config.
	// The auth code only calls Provider methods, never checks mode.
	providerMu sync.RWMutex
	providers  map[string]certs.Provider

	// Per-cluster cert configs (set via WithClusterCertConfig, consumed during cluster join)
	certConfigs map[string]*certs.ClusterCertConfig

	// Per-cluster revocation lists
	revocationMu sync.RWMutex
	revocations  map[string]*certs.RevocationList

	// Auth topics per cluster (protected by topicMu)
	topicMu    sync.RWMutex
	authTopics map[string]*pubsub.Topic

	// Sessions (protected by sessionMu)
	sessionMu sync.RWMutex
	sessions  map[string]*Session // clusterPath -> Session

	// Pending auth confirmations (protected by pendingMu)
	pendingMu   sync.RWMutex
	pendingAuth map[string]*PendingAuth // nodeID:clusterPath -> pending state

	// Session staleness config
	sessionStaleThreshold time.Duration
	staleCheckInterval    time.Duration
	maxStaleDuration      time.Duration

	// Rate limiting
	rateLimiter *ratelimit.Limiter

	// Message deduplication (replay protection within MaxMessageAge window)
	messageDedup *MessageDedup

	// Testing flags
	rejectAllAuth bool // When true, always reject incoming auth announcements

	// Operator-supplied node identity + capabilities. Populated on the
	// outbound JoinRequest so vouchers + downstream phonebook entries
	// can display the friendly name and CPU/memory the operator brought.
	nodeName     string
	capabilities *authpb.Capabilities

	// Lifecycle
	parentCtx context.Context // Set via WithContext option
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// PendingAuth tracks confirmations/rejections for a pending authentication.
type PendingAuth struct {
	NodeID       string
	ClusterPath  string
	Certificate  *certs.Certificate
	Confirmations map[string]bool // nodeID -> confirmed
	Rejections    map[string]string // nodeID -> reason
	Done         chan AuthResult
	StartedAt    time.Time
}

// AuthResult represents the result of authentication confirmation.
type AuthResult struct {
	Confirmed bool
	Reason    string
}

// Option configures an Authenticator.
type Option func(*Authenticator)

// WithContext sets the parent context for the authenticator.
// The authenticator will derive its internal context from this parent,
// enabling proper cancellation propagation from the parent component.
func WithContext(ctx context.Context) Option {
	return func(a *Authenticator) {
		a.parentCtx = ctx
	}
}

// WithHost sets the libp2p host.
func WithHost(h host.Host) Option {
	return func(a *Authenticator) {
		a.host = h
	}
}

// WithPubSub sets the PubSub instance.
func WithPubSub(ps *pubsub.PubSub) Option {
	return func(a *Authenticator) {
		a.pubsub = ps
	}
}

// WithEventBus sets the event bus.
func WithEventBus(bus events.Bus) Option {
	return func(a *Authenticator) {
		a.eventBus = bus
	}
}

// WithPhonebook sets the phonebook for read-only lookups.
func WithPhonebook(pb phonebook.IPhonebook) Option {
	return func(a *Authenticator) {
		a.phonebook = pb
	}
}

// WithPrivateKey sets the private key for signing.
func WithPrivateKey(key crypto.PrivKey) Option {
	return func(a *Authenticator) {
		a.privateKey = key
	}
}

// WithLogger sets the logger.
func WithLogger(logger *zap.Logger) Option {
	return func(a *Authenticator) {
		a.logger = logger
	}
}

// WithDataDir sets the data directory for persistent storage.
func WithDataDir(dataDir string) Option {
	return func(a *Authenticator) {
		a.dataDir = dataDir
	}
}

// WithClusterCertConfig registers a per-cluster certificate configuration.
// If the config specifies external CA (CACertPath is set), an ExternalProvider is created.
// Otherwise, an AutoProvider is created when the cluster PSK is set.
// This option can be called multiple times for different clusters.
func WithClusterCertConfig(clusterPath string, cfg *certs.ClusterCertConfig) Option {
	return func(a *Authenticator) {
		if a.certConfigs == nil {
			a.certConfigs = make(map[string]*certs.ClusterCertConfig)
		}
		a.certConfigs[clusterPath] = cfg
	}
}

// WithRejectAllAuth makes the authenticator reject every incoming auth
// announcement, regardless of certificate validity.
//
// TEST/DEBUG ONLY — never enable this in production. The flag exists so
// integration tests can drive the rejection path; leaving it on in a
// live cluster breaks auth for every peer.
func WithRejectAllAuth(reject bool) Option {
	return func(a *Authenticator) {
		a.rejectAllAuth = reject
	}
}

// WithSessionStaleThreshold sets the threshold for session staleness.
func WithSessionStaleThreshold(d time.Duration) Option {
	return func(a *Authenticator) {
		a.sessionStaleThreshold = d
	}
}

// WithStaleCheckInterval sets the interval for checking session staleness.
// WithNodeName sets the operator-assigned friendly name that travels
// inside JoinRequest.Capabilities.Metadata["node_name"]. Used by
// downstream phonebook subscribers + the CLI to display "node1" instead
// of the bare peer ID.
func WithNodeName(name string) Option {
	return func(a *Authenticator) {
		a.nodeName = name
	}
}

// WithCapabilities sets the resource capabilities (CPU cores, memory MB,
// disk GB, datacenter, tags) the joining node announces. The voucher
// stores this on the phonebook entry it creates for the new member.
// When nil, the JoinRequest carries no capabilities (CLI will show 0).
func WithCapabilities(caps *authpb.Capabilities) Option {
	return func(a *Authenticator) {
		a.capabilities = caps
	}
}

func WithStaleCheckInterval(d time.Duration) Option {
	return func(a *Authenticator) {
		a.staleCheckInterval = d
	}
}

// New creates a new Authenticator with the given options.
func New(opts ...Option) *Authenticator {
	a := &Authenticator{
		logger:                zap.NewNop(),
		hmacKeys:              make(map[string][]byte),
		clusterRootKeys:       make(map[string]*clusterkeys.ClusterRootKey),
		authTopics:            make(map[string]*pubsub.Topic),
		sessions:              make(map[string]*Session),
		pendingAuth:           make(map[string]*PendingAuth),
		providers:             make(map[string]certs.Provider),
		revocations:           make(map[string]*certs.RevocationList),
		sessionStaleThreshold: DefaultSessionStaleThreshold,
		staleCheckInterval:    DefaultStaleCheckInterval,
		maxStaleDuration:      DefaultMaxStaleDuration,
		rateLimiter:           ratelimit.NewLimiter(DefaultAuthRateLimit, DefaultAuthRateWindow),
		messageDedup:          NewMessageDedup(MaxMessageAge, MaxMessageAge/3),
	}

	// Apply options first to capture parentCtx if provided
	for _, opt := range opts {
		opt(a)
	}

	// Initialize node key manager if data directory is set
	if a.dataDir != "" {
		a.nodeKeyManager = nodekeys.NewManager(a.dataDir)
	}

	// Derive context from parent if provided, otherwise use Background
	if a.parentCtx != nil {
		a.ctx, a.cancel = context.WithCancel(a.parentCtx)
	} else {
		a.ctx, a.cancel = context.WithCancel(context.Background())
	}

	return a
}

// Start starts the authenticator background tasks.
// Returns an error if required dependencies are not configured.
func (a *Authenticator) Start() error {
	// Validate required dependencies
	if a.host == nil {
		return errors.New("host is required")
	}
	if a.pubsub == nil {
		return errors.New("pubsub is required")
	}
	if a.privateKey == nil {
		return errors.New("privateKey is required")
	}
	if a.eventBus == nil {
		return errors.New("eventBus is required")
	}

	// Subscribe to cluster join requests
	joinRequestCh := a.eventBus.Subscribe(events.TypeClusterJoinRequested)

	a.wg.Add(2)
	go func() {
		defer a.wg.Done()
		a.sessionStaleCheckLoop()
	}()
	go func() {
		defer a.wg.Done()
		a.handleClusterJoinRequestLoop(joinRequestCh)
	}()

	return nil
}

// Stop stops the authenticator and cleans up resources.
func (a *Authenticator) Stop() {
	a.cancel()

	// Wait for all goroutines to finish
	a.wg.Wait()

	// Close dedup cache
	if a.messageDedup != nil {
		a.messageDedup.Close()
	}

	// Close all topics
	a.topicMu.Lock()
	for _, topic := range a.authTopics {
		topic.Close()
	}
	a.authTopics = make(map[string]*pubsub.Topic)
	a.topicMu.Unlock()
}

// SetClusterPSK sets the PSK for a cluster, derives the cluster root key and HMAC key,
// then zeros the raw PSK from memory. After this call, only the derived keys are retained.
func (a *Authenticator) SetClusterPSK(clusterPath string, psk []byte) error {
	if len(psk) < MinPSKLength {
		return ErrPSKTooShort
	}

	// Derive cluster root key from PSK
	rootKey, err := clusterkeys.DeriveClusterRootKey(psk, clusterPath)
	if err != nil {
		return fmt.Errorf("failed to derive cluster root key: %w", err)
	}

	// Derive HMAC key from PSK (separate derivation for challenge-response)
	hmacKey, err := clusterkeys.DeriveHMACKey(psk, clusterPath)
	if err != nil {
		return fmt.Errorf("failed to derive HMAC key: %w", err)
	}

	// Zero the raw PSK — we no longer need it after deriving keys
	for i := range psk {
		psk[i] = 0
	}

	a.hmacKeyMu.Lock()
	a.hmacKeys[clusterPath] = hmacKey
	a.hmacKeyMu.Unlock()

	a.rootKeyMu.Lock()
	a.clusterRootKeys[clusterPath] = rootKey
	a.rootKeyMu.Unlock()

	return nil
}

// GetHMACKey returns the derived HMAC key for a cluster's challenge-response protocol.
func (a *Authenticator) GetHMACKey(clusterPath string) ([]byte, bool) {
	a.hmacKeyMu.RLock()
	defer a.hmacKeyMu.RUnlock()
	key, ok := a.hmacKeys[clusterPath]
	return key, ok
}

// GetClusterRootKey returns the cluster root key for a cluster.
func (a *Authenticator) GetClusterRootKey(clusterPath string) (*clusterkeys.ClusterRootKey, bool) {
	a.rootKeyMu.RLock()
	defer a.rootKeyMu.RUnlock()
	key, ok := a.clusterRootKeys[clusterPath]
	return key, ok
}

// GetOrCreateNodeKey gets or creates the node's key pair for a cluster.
func (a *Authenticator) GetOrCreateNodeKey(clusterPath string) (*nodekeys.NodeKeyPair, error) {
	if a.nodeKeyManager == nil {
		return nil, errors.New("node key manager not initialized (data directory not set)")
	}
	return a.nodeKeyManager.LoadOrCreate(clusterPath)
}

// GetNodeCertificate loads the node's certificate for a cluster from disk.
func (a *Authenticator) GetNodeCertificate(clusterPath string) (*certs.Certificate, error) {
	if a.nodeKeyManager == nil {
		return nil, errors.New("node key manager not initialized")
	}
	certPath := a.nodeKeyManager.CertPath(clusterPath)
	return certs.LoadFromFile(certPath)
}

// SaveNodeCertificate saves the node's certificate for a cluster to disk.
func (a *Authenticator) SaveNodeCertificate(clusterPath string, cert *certs.Certificate) error {
	if a.nodeKeyManager == nil {
		return errors.New("node key manager not initialized")
	}
	certPath := a.nodeKeyManager.CertPath(clusterPath)
	return cert.SaveToFile(certPath)
}

// RegisterCertConfig registers a per-cluster certificate configuration at runtime.
// Called by Node.Join() before publishing the ClusterJoinRequested event.
func (a *Authenticator) RegisterCertConfig(clusterPath string, cfg *certs.ClusterCertConfig) {
	if a.certConfigs == nil {
		a.certConfigs = make(map[string]*certs.ClusterCertConfig)
	}
	a.certConfigs[clusterPath] = cfg
}

// GetProvider returns the certificate provider for a cluster.
func (a *Authenticator) GetProvider(clusterPath string) (certs.Provider, bool) {
	a.providerMu.RLock()
	defer a.providerMu.RUnlock()
	p, ok := a.providers[clusterPath]
	return p, ok
}

// SetProvider sets the certificate provider for a cluster.
func (a *Authenticator) SetProvider(clusterPath string, p certs.Provider) {
	a.providerMu.Lock()
	defer a.providerMu.Unlock()
	a.providers[clusterPath] = p
}

// initProviderForCluster creates and registers the appropriate provider for a cluster.
// If a ClusterCertConfig was set via WithClusterCertConfig, it determines the mode.
// Otherwise, auto mode is used with the PSK-derived root key.
func (a *Authenticator) initProviderForCluster(clusterPath string) error {
	// Check if an external cert config was provided for this cluster
	cfg := a.certConfigs[clusterPath]

	if cfg != nil && cfg.IsExternal() {
		// External mode
		provider, err := certs.NewExternalProvider(cfg,
			certs.WithExternalClusterPath(clusterPath),
			certs.WithExternalKeyManager(a.nodeKeyManager),
		)
		if err != nil {
			return fmt.Errorf("failed to create external provider: %w", err)
		}
		a.SetProvider(clusterPath, provider)
		a.logger.Info("initialized external CA provider",
			zap.String("cluster", clusterPath),
			zap.Bool("canSign", provider.CanSign()))
		return nil
	}

	// Auto mode — requires cluster root key (derived from PSK)
	a.rootKeyMu.RLock()
	rootKey, ok := a.clusterRootKeys[clusterPath]
	a.rootKeyMu.RUnlock()

	if !ok {
		return fmt.Errorf("no cluster root key available for auto provider (PSK not set for %s)", clusterPath)
	}

	provider, err := certs.NewAutoProvider(
		certs.WithAutoClusterPath(clusterPath),
		certs.WithAutoRootKey(rootKey),
		certs.WithAutoKeyManager(a.nodeKeyManager),
	)
	if err != nil {
		return fmt.Errorf("failed to create auto provider: %w", err)
	}

	a.SetProvider(clusterPath, provider)
	a.logger.Debug("initialized auto CA provider",
		zap.String("cluster", clusterPath))
	return nil
}

// GetRevocationList returns or creates the revocation list for a cluster.
func (a *Authenticator) GetRevocationList(clusterPath string) *certs.RevocationList {
	a.revocationMu.RLock()
	rl, ok := a.revocations[clusterPath]
	a.revocationMu.RUnlock()

	if ok {
		return rl
	}

	// Create new revocation list
	a.revocationMu.Lock()
	defer a.revocationMu.Unlock()

	// Double-check after acquiring write lock
	if rl, ok := a.revocations[clusterPath]; ok {
		return rl
	}

	rl, err := certs.NewRevocationList(clusterPath, a.dataDir)
	if err != nil {
		a.logger.Error("failed to create revocation list",
			zap.String("cluster", clusterPath),
			zap.Error(err))
		// Return an empty in-memory list as fallback
		rl, _ = certs.NewRevocationList(clusterPath, "")
	}

	a.revocations[clusterPath] = rl
	return rl
}

// RevokeCertificate revokes a node's certificate and broadcasts the revocation to the cluster.
func (a *Authenticator) RevokeCertificate(ctx context.Context, nodeID, clusterPath, reason string, certFingerprint string) error {
	entry := certs.RevocationEntry{
		CertFingerprint: certFingerprint,
		NodeID:          nodeID,
		ClusterPath:     clusterPath,
		RevokedAt:       time.Now(),
		Reason:          reason,
		RevokedBy:       a.host.ID().String(),
	}

	// Add to local revocation list
	rl := a.GetRevocationList(clusterPath)
	if err := rl.Revoke(entry); err != nil {
		return fmt.Errorf("failed to add revocation: %w", err)
	}

	// Broadcast to cluster
	revocation := &authpb.CertificateRevocation{
		CertFingerprint: certFingerprint,
		NodeId:          nodeID,
		ClusterPath:     clusterPath,
		Reason:          reason,
		RevokedBy:       a.host.ID().String(),
		RevokedAt:       timestamppb.Now(),
	}

	// Sign the revocation
	buf := signing.NewBuffer(256)
	buf.WriteString(certFingerprint)
	buf.WriteString(nodeID)
	buf.WriteString(clusterPath)
	buf.WriteString(reason)
	buf.WriteString(a.host.ID().String())
	tsBytes, _ := revocation.RevokedAt.AsTime().MarshalBinary()
	buf.WriteTimeBinary(tsBytes)

	sig, err := a.privateKey.Sign(buf.Bytes())
	if err != nil {
		return fmt.Errorf("failed to sign revocation: %w", err)
	}
	revocation.Signature = sig

	if err := a.publishAuthMessage(ctx, clusterPath, "cert_revoked", revocation); err != nil {
		return fmt.Errorf("failed to broadcast revocation: %w", err)
	}

	a.logger.Info("certificate revoked and broadcast",
		zap.String("nodeId", nodeID),
		zap.String("cluster", clusterPath),
		zap.String("reason", reason))

	return nil
}

// IsCertRevoked checks if a certificate fingerprint is revoked in a cluster.
func (a *Authenticator) IsCertRevoked(clusterPath, certFingerprint string) bool {
	rl := a.GetRevocationList(clusterPath)
	return rl.IsRevoked(certFingerprint)
}

// IsNodeRevoked checks if any certificate for a node is revoked in a cluster.
func (a *Authenticator) IsNodeRevoked(clusterPath, nodeID string) bool {
	rl := a.GetRevocationList(clusterPath)
	return rl.IsNodeRevoked(nodeID)
}

// RegisterProtocol registers the auth stream handler.
func (a *Authenticator) RegisterProtocol() {
	a.host.SetStreamHandler(ProtocolID, a.handleAuthStream)
}

// SubscribeToCluster subscribes to the auth topic for a cluster.
func (a *Authenticator) SubscribeToCluster(ctx context.Context, clusterPath string) error {
	topicName := shared.BuildAuthTopic(clusterPath)

	a.logger.Info("joining auth pubsub topic",
		zap.String("cluster", clusterPath),
		zap.String("topic", topicName))

	topic, err := a.pubsub.Join(topicName)
	if err != nil {
		return fmt.Errorf("failed to join auth topic: %w", err)
	}

	a.topicMu.Lock()
	a.authTopics[clusterPath] = topic
	a.topicMu.Unlock()

	sub, err := topic.Subscribe()
	if err != nil {
		return fmt.Errorf("failed to subscribe to auth topic: %w", err)
	}

	a.logger.Info("subscribed to auth pubsub topic",
		zap.String("cluster", clusterPath),
		zap.String("topic", topicName))

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.logger.Debug("starting auth message loop",
			zap.String("cluster", clusterPath))
		a.authMessageLoop(ctx, clusterPath, sub)
	}()

	return nil
}

// UnsubscribeFromCluster unsubscribes from the auth topic for a cluster.
// It closes the topic only — per-cluster keys, sessions, providers, and
// revocation lists remain cached. Use LeaveCluster for a full cleanup.
func (a *Authenticator) UnsubscribeFromCluster(clusterPath string) {
	a.topicMu.Lock()
	defer a.topicMu.Unlock()

	if topic, ok := a.authTopics[clusterPath]; ok {
		topic.Close()
		delete(a.authTopics, clusterPath)
	}
}

// LeaveCluster performs a full teardown of per-cluster authentication
// state: closes the auth topic, then drops the session, HMAC key, cluster
// root key, certificate provider, revocation list, and cert config for
// the cluster. Idempotent.
//
// Callers that only want to stop receiving auth pubsub messages (without
// losing session state) should call UnsubscribeFromCluster instead.
func (a *Authenticator) LeaveCluster(clusterPath string) {
	a.UnsubscribeFromCluster(clusterPath)

	a.sessionMu.Lock()
	delete(a.sessions, clusterPath)
	a.sessionMu.Unlock()

	a.hmacKeyMu.Lock()
	if key, ok := a.hmacKeys[clusterPath]; ok {
		for i := range key {
			key[i] = 0
		}
		delete(a.hmacKeys, clusterPath)
	}
	a.hmacKeyMu.Unlock()

	a.rootKeyMu.Lock()
	delete(a.clusterRootKeys, clusterPath)
	a.rootKeyMu.Unlock()

	a.providerMu.Lock()
	delete(a.providers, clusterPath)
	a.providerMu.Unlock()

	a.revocationMu.Lock()
	delete(a.revocations, clusterPath)
	a.revocationMu.Unlock()

	if a.certConfigs != nil {
		delete(a.certConfigs, clusterPath)
	}
}

// GetSession returns the session for a cluster.
func (a *Authenticator) GetSession(clusterPath string) (*Session, error) {
	a.sessionMu.RLock()
	defer a.sessionMu.RUnlock()

	session, ok := a.sessions[clusterPath]
	if !ok {
		return nil, ErrSessionNotFound
	}
	return session, nil
}

// IsAuthenticated returns true if authenticated to the cluster.
func (a *Authenticator) IsAuthenticated(clusterPath string) bool {
	a.sessionMu.RLock()
	defer a.sessionMu.RUnlock()

	session, ok := a.sessions[clusterPath]
	return ok && session.Status == SessionStatusEnum.Authenticated()
}

// IsSessionStale returns true if the session is stale or doesn't exist.
func (a *Authenticator) IsSessionStale(clusterPath string) bool {
	a.sessionMu.RLock()
	defer a.sessionMu.RUnlock()

	session, ok := a.sessions[clusterPath]
	if !ok {
		return true
	}
	return session.Status == SessionStatusEnum.Stale()
}

// RefreshSession refreshes a stale session back to authenticated status.
// Call this after successful re-authentication.
func (a *Authenticator) RefreshSession(clusterPath string) {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()

	if session, ok := a.sessions[clusterPath]; ok {
		session.Status = SessionStatusEnum.Authenticated()
		session.LastActivity = time.Now()
		session.StaleAt = time.Time{} // Reset stale time
	}
}

// UpdateSessionActivity updates the last activity time for a session.
func (a *Authenticator) UpdateSessionActivity(clusterPath string) {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()

	if session, ok := a.sessions[clusterPath]; ok {
		session.LastActivity = time.Now()
	}
}

// Authenticate performs authentication to join a cluster.
func (a *Authenticator) Authenticate(ctx context.Context, clusterPath string, targetPeer peer.ID) (*Session, error) {
	a.hmacKeyMu.RLock()
	hmacKey, ok := a.hmacKeys[clusterPath]
	a.hmacKeyMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("no HMAC key derived for cluster %s (PSK not set)", clusterPath)
	}

	ctx, cancel := context.WithTimeout(ctx, AuthTimeout)
	defer cancel()

	// Open auth stream
	stream, err := a.host.NewStream(ctx, targetPeer, ProtocolID)
	if err != nil {
		a.eventBus.Publish(events.AuthenticationFailed{
			BaseEvent:   events.NewBaseEvent(),
			ClusterPath: clusterPath,
			TargetPeer:  targetPeer.String(),
			Reason:      fmt.Sprintf("failed to open stream: %v", err),
		})
		return nil, fmt.Errorf("failed to open auth stream: %w", err)
	}
	defer stream.Close()

	// Generate nonce
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	// Get our public key bytes
	pubKeyBytes, err := crypto.MarshalPublicKey(a.privateKey.GetPublic())
	if err != nil {
		return nil, fmt.Errorf("failed to marshal public key: %w", err)
	}

	// Get our addresses
	addrs := make([]string, 0, len(a.host.Addrs()))
	for _, addr := range a.host.Addrs() {
		addrs = append(addrs, addr.String())
	}

	// Send JoinRequest
	joinReq := &authpb.JoinRequest{
		NodeId:       a.host.ID().String(),
		ClusterPath:  clusterPath,
		Addresses:    addrs,
		Nonce:        nonce,
		PublicKey:    pubKeyBytes,
		Capabilities: a.buildCapabilities(),
	}

	if err := shared.WriteProto(stream, joinReq); err != nil {
		return nil, fmt.Errorf("failed to send join request: %w", err)
	}

	// Receive Challenge
	var challenge authpb.Challenge
	if err := shared.ReadProto(stream, &challenge); err != nil {
		return nil, fmt.Errorf("failed to receive challenge: %w", err)
	}

	// Compute HMAC response
	responseData := append(challenge.Challenge, nonce...)
	responseData = append(responseData, challenge.ServerNonce...)

	mac := hmac.New(sha256.New, hmacKey)
	mac.Write(responseData)
	response := mac.Sum(nil)

	// Sign the response
	signature, err := a.privateKey.Sign(response)
	if err != nil {
		return nil, fmt.Errorf("failed to sign response: %w", err)
	}

	// Send ChallengeResponse
	challengeResp := &authpb.ChallengeResponse{
		Response:  response,
		Signature: signature,
	}

	if err := shared.WriteProto(stream, challengeResp); err != nil {
		return nil, fmt.Errorf("failed to send challenge response: %w", err)
	}

	// Receive Step1Result
	var step1Result authpb.Step1Result
	if err := shared.ReadProto(stream, &step1Result); err != nil {
		return nil, fmt.Errorf("failed to receive step1 result: %w", err)
	}

	if !step1Result.Success {
		a.eventBus.Publish(events.AuthenticationFailed{
			BaseEvent:   events.NewBaseEvent(),
			ClusterPath: clusterPath,
			TargetPeer:  targetPeer.String(),
			Reason:      step1Result.Error,
		})
		return nil, fmt.Errorf("authentication failed: %s", step1Result.Error)
	}

	// Wait for AuthComplete (after Step 2 broadcast)
	var authComplete authpb.AuthComplete
	if err := shared.ReadProto(stream, &authComplete); err != nil {
		return nil, fmt.Errorf("failed to receive auth complete: %w", err)
	}

	if !authComplete.FullyAuthenticated {
		return nil, fmt.Errorf("full authentication failed")
	}

	// Save the certificate we received from the voucher
	if authComplete.NodeCertificate != nil {
		provider, providerOK := a.GetProvider(clusterPath)
		if providerOK {
			cert := &certs.Certificate{NodeCertificate: authComplete.NodeCertificate}
			if err := provider.SaveNodeCertificate(cert); err != nil {
				a.logger.Error("failed to save received certificate",
					zap.String("cluster", clusterPath),
					zap.Error(err))
			} else {
				a.logger.Info("saved node certificate from voucher",
					zap.String("cluster", clusterPath),
					zap.String("voucher", targetPeer.String()))
			}
		}
	}

	// Emit event with cluster members - phonebook will subscribe and store them
	if len(authComplete.ClusterMembers) > 0 {
		members := make([]events.MemberInfo, 0, len(authComplete.ClusterMembers))
		for _, m := range authComplete.ClusterMembers {
			member := events.MemberInfo{
				NodeID:    m.NodeId,
				Addresses: m.Addresses,
				PublicKey: m.PublicKey,
				JoinedAt:  m.JoinedAt,
			}
			if m.Capabilities != nil {
				member.Capabilities = &events.Capabilities{
					CPUCores:   m.Capabilities.CpuCores,
					MemoryMB:   m.Capabilities.MemoryMb,
					DiskGB:     m.Capabilities.DiskGb,
					Datacenter: m.Capabilities.Datacenter,
					Tags:       m.Capabilities.Tags,
					Metadata:   m.Capabilities.Metadata,
				}
			}
			members = append(members, member)
		}

		a.eventBus.Publish(events.ClusterMembersReceived{
			BaseEvent:   events.NewBaseEvent(),
			ClusterPath: clusterPath,
			Members:     members,
		})
	}

	// Flip the voucher from PendingAuth → Active immediately. The
	// announcement we just acknowledged came over a working libp2p
	// connection, so SWIM should be free to probe (Bug #13). The
	// subscriber goroutine consuming ClusterMembersReceived above is
	// async — without this explicit hop the voucher would sit in
	// PendingAuth for the SWIM grace window.
	if a.phonebook != nil {
		if err := a.phonebook.SetStatus(targetPeer.String(), clusterPath, phonebook.NodeStatusEnum.Active()); err != nil {
			a.logger.Debug("failed to promote voucher to active",
				zap.String("voucher", targetPeer.String()),
				zap.Error(err))
		}
	}

	// Create and store session
	now := time.Now()
	session := &Session{
		ClusterPath:     clusterPath,
		Status:          SessionStatusEnum.Authenticated(),
		VoucherNodeID:   targetPeer.String(),
		AuthenticatedAt: now,
		LastActivity:    now,
	}

	a.sessionMu.Lock()
	a.sessions[clusterPath] = session
	a.sessionMu.Unlock()

	// Emit authenticated event
	a.eventBus.Publish(events.PeerAuthenticated{
		BaseEvent:     events.NewBaseEvent(),
		ClusterPath:   clusterPath,
		VoucherNodeID: targetPeer.String(),
	})

	// Request a sync to ensure we have latest member data
	a.eventBus.Publish(events.SyncRequested{
		BaseEvent:   events.NewBaseEvent(),
		ClusterPath: clusterPath,
		Reason:      "post_auth",
	})

	a.logger.Info("authenticated to cluster",
		zap.String("cluster", clusterPath),
		zap.String("voucher", targetPeer.String()))

	return session, nil
}

// sessionStaleCheckLoop periodically checks for stale sessions and triggers re-auth.
func (a *Authenticator) sessionStaleCheckLoop() {
	ticker := time.NewTicker(a.staleCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.checkStaleSessions()
		}
	}
}

// handleClusterJoinRequestLoop processes ClusterJoinRequested events.
func (a *Authenticator) handleClusterJoinRequestLoop(ch <-chan events.Event) {
	for {
		select {
		case <-a.ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}

			req, ok := event.(events.ClusterJoinRequested)
			if !ok {
				continue
			}

			// Handle each join request in a goroutine to not block the loop
			a.wg.Add(1)
			go func(r events.ClusterJoinRequested) {
				defer a.wg.Done()
				a.handleClusterJoinRequest(r)
			}(req)
		}
	}
}

// handleClusterJoinRequest processes a single ClusterJoinRequested event.
func (a *Authenticator) handleClusterJoinRequest(req events.ClusterJoinRequested) {
	a.logger.Info("handling cluster join request",
		zap.String("cluster", req.ClusterPath),
		zap.Int("bootstrapPeers", len(req.BootstrapPeers)))

	// 1. Set PSK for cluster (derives HMAC key and cluster root key, zeros PSK)
	if err := a.SetClusterPSK(req.ClusterPath, req.PSK); err != nil {
		a.publishJoinFailed(req.ClusterPath, fmt.Sprintf("invalid PSK: %v", err))
		return
	}

	// 2. Initialize certificate provider for this cluster
	if err := a.initProviderForCluster(req.ClusterPath); err != nil {
		a.publishJoinFailed(req.ClusterPath, fmt.Sprintf("failed to init cert provider: %v", err))
		return
	}

	// 3. Subscribe to cluster auth topic (use long-lived context, not timeout)
	if err := a.SubscribeToCluster(a.ctx, req.ClusterPath); err != nil {
		a.publishJoinFailed(req.ClusterPath, fmt.Sprintf("failed to subscribe to cluster: %v", err))
		return
	}

	// Create timeout context for authentication steps
	ctx, cancel := context.WithTimeout(a.ctx, AuthTimeout)
	defer cancel()

	// 4. If we have bootstrap peers, connect and authenticate
	if len(req.BootstrapPeers) > 0 {
		session, err := a.bootstrapAndAuthenticate(ctx, req.ClusterPath, req.BootstrapPeers)
		if err != nil {
			a.publishJoinFailed(req.ClusterPath, fmt.Sprintf("bootstrap failed: %v", err))
			return
		}

		// Success with bootstrap peer
		a.eventBus.Publish(events.ClusterJoined{
			BaseEvent:     events.NewBaseEvent(),
			ClusterPath:   req.ClusterPath,
			VoucherNodeID: session.VoucherNodeID,
		})
		return
	}

	// No bootstrap peers - we're the first node in the cluster
	a.logger.Info("no bootstrap peers, starting as first node",
		zap.String("cluster", req.ClusterPath))

	// Get or create node key for this cluster
	if a.nodeKeyManager != nil {
		if _, err := a.nodeKeyManager.LoadOrCreate(req.ClusterPath); err != nil {
			a.logger.Error("failed to get/create node key for cluster",
				zap.String("cluster", req.ClusterPath),
				zap.Error(err))
		}
	}

	// Create self-signed certificate via provider
	provider, ok := a.GetProvider(req.ClusterPath)
	if ok {
		cert, err := provider.CreateSelfSignedCert(
			a.host.ID().String(),
			a.privateKey.GetPublic(),
			a.privateKey,
			req.ClusterPath,
		)
		if err != nil {
			a.logger.Error("failed to create self-signed certificate",
				zap.String("cluster", req.ClusterPath),
				zap.Error(err))
		} else {
			a.logger.Info("created self-signed certificate for first node",
				zap.String("cluster", req.ClusterPath))
			_ = cert // saved by provider
		}
	}

	// Create session for first node
	now := time.Now()
	session := &Session{
		ClusterPath:     req.ClusterPath,
		Status:          SessionStatusEnum.Authenticated(),
		VoucherNodeID:   "", // No voucher for first node
		AuthenticatedAt: now,
		LastActivity:    now,
	}

	a.sessionMu.Lock()
	a.sessions[req.ClusterPath] = session
	a.sessionMu.Unlock()

	// Add ourselves to our own phonebook so other nodes can verify us during sync
	pubKeyBytes, err := crypto.MarshalPublicKey(a.privateKey.GetPublic())
	if err == nil {
		addrs := make([]string, 0, len(a.host.Addrs()))
		for _, addr := range a.host.Addrs() {
			addrs = append(addrs, addr.String())
		}

		a.eventBus.Publish(events.NewMemberAnnounced{
			BaseEvent:    events.NewBaseEvent(),
			NodeID:       a.host.ID().String(),
			ClusterPath:  req.ClusterPath,
			Addresses:    addrs,
			PublicKey:    pubKeyBytes,
			Capabilities: a.buildCapabilitiesEvent(),
		})
	}

	a.eventBus.Publish(events.ClusterJoined{
		BaseEvent:     events.NewBaseEvent(),
		ClusterPath:   req.ClusterPath,
		VoucherNodeID: "",
	})
}

// bootstrapAndAuthenticate connects to bootstrap peers and authenticates with one.
func (a *Authenticator) bootstrapAndAuthenticate(ctx context.Context, clusterPath string, bootstrapPeers []string) (*Session, error) {
	var lastErr error

	for _, peerAddr := range bootstrapPeers {
		maddr, err := multiaddr.NewMultiaddr(peerAddr)
		if err != nil {
			a.logger.Warn("invalid bootstrap peer address",
				zap.String("addr", peerAddr),
				zap.Error(err))
			lastErr = fmt.Errorf("invalid address %s: %w", peerAddr, err)
			continue
		}

		peerInfo, err := peer.AddrInfoFromP2pAddr(maddr)
		if err != nil {
			a.logger.Warn("failed to parse peer info",
				zap.String("addr", peerAddr),
				zap.Error(err))
			lastErr = fmt.Errorf("invalid peer info %s: %w", peerAddr, err)
			continue
		}

		// Connect to peer
		a.logger.Info("dialing bootstrap peer",
			zap.String("peer", peerInfo.ID.String()),
			zap.String("addr", peerAddr),
			zap.String("cluster", clusterPath))
		connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := a.host.Connect(connectCtx, *peerInfo); err != nil {
			cancel()
			a.logger.Warn("failed to connect to bootstrap peer",
				zap.String("peer", peerInfo.ID.String()),
				zap.Error(err))
			lastErr = fmt.Errorf("connection to %s failed: %w", peerInfo.ID.ShortString(), err)
			continue
		}
		cancel()
		a.logger.Info("bootstrap peer connected, requesting auth (gossipsub mesh forming, may take 5-15s)",
			zap.String("peer", peerInfo.ID.String()),
			zap.String("cluster", clusterPath))

		// Authenticate. Run a heartbeat in the background so operators
		// see progress instead of silence during the gossipsub mesh
		// formation window.
		authStart := time.Now()
		hbCtx, hbCancel := context.WithCancel(ctx)
		go a.authProgressHeartbeat(hbCtx, clusterPath, peerInfo.ID.String(), authStart)
		session, err := a.Authenticate(ctx, clusterPath, peerInfo.ID)
		hbCancel()
		if err != nil {
			a.logger.Warn("failed to authenticate with bootstrap peer",
				zap.String("peer", peerInfo.ID.String()),
				zap.Error(err))
			lastErr = fmt.Errorf("authentication with %s failed: %w", peerInfo.ID.ShortString(), err)
			continue
		}

		a.logger.Info("authenticated with bootstrap peer",
			zap.String("peer", peerInfo.ID.String()),
			zap.String("cluster", clusterPath),
			zap.Duration("elapsed", time.Since(authStart)))

		return session, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no bootstrap peers provided")
}

// MetadataKeyNodeName is the well-known Capabilities.Metadata key
// carrying the operator-assigned friendly node name. Defined as a
// constant so the joiner publisher and the voucher/phonebook
// subscriber agree on the spelling.
const MetadataKeyNodeName = "node_name"

// buildCapabilities returns the protobuf Capabilities the joiner should
// stamp onto its outbound JoinRequest. Returns a copy with the node
// name merged into metadata so the voucher (and downstream phonebook
// subscribers) see it without needing a proto schema change.
func (a *Authenticator) buildCapabilities() *authpb.Capabilities {
	if a.capabilities == nil && a.nodeName == "" {
		return nil
	}
	out := &authpb.Capabilities{}
	if a.capabilities != nil {
		out.CpuCores = a.capabilities.CpuCores
		out.MemoryMb = a.capabilities.MemoryMb
		out.DiskGb = a.capabilities.DiskGb
		out.Datacenter = a.capabilities.Datacenter
		out.Tags = append([]string(nil), a.capabilities.Tags...)
		if len(a.capabilities.Metadata) > 0 {
			out.Metadata = make(map[string]string, len(a.capabilities.Metadata))
			for k, v := range a.capabilities.Metadata {
				out.Metadata[k] = v
			}
		}
	}
	if a.nodeName != "" {
		if out.Metadata == nil {
			out.Metadata = map[string]string{}
		}
		out.Metadata[MetadataKeyNodeName] = a.nodeName
	}
	return out
}

// buildCapabilitiesEvent is the events.Capabilities mirror of
// buildCapabilities, used for the in-process publish on the first-node
// self-add path (where the subscriber consumes events directly without
// passing through the auth wire format).
func (a *Authenticator) buildCapabilitiesEvent() *events.Capabilities {
	caps := a.buildCapabilities()
	if caps == nil {
		return nil
	}
	out := &events.Capabilities{
		CPUCores:   caps.CpuCores,
		MemoryMB:   caps.MemoryMb,
		DiskGB:     caps.DiskGb,
		Datacenter: caps.Datacenter,
		Tags:       caps.Tags,
	}
	if len(caps.Metadata) > 0 {
		out.Metadata = make(map[string]string, len(caps.Metadata))
		for k, v := range caps.Metadata {
			out.Metadata[k] = v
		}
	}
	return out
}

// authProgressHeartbeat logs an INFO every 5 seconds while waiting for
// the voucher's auth-complete reply so operators can see the handshake
// is still in flight (gossipsub mesh formation can take 5-15s and is
// otherwise invisible).
func (a *Authenticator) authProgressHeartbeat(ctx context.Context, clusterPath, peer string, startedAt time.Time) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.logger.Info("still waiting for voucher response",
				zap.String("peer", peer),
				zap.String("cluster", clusterPath),
				zap.Duration("elapsed", time.Since(startedAt)))
		}
	}
}

// publishJoinFailed publishes a ClusterJoinFailed event.
func (a *Authenticator) publishJoinFailed(clusterPath, reason string) {
	a.logger.Error("cluster join failed",
		zap.String("cluster", clusterPath),
		zap.String("reason", reason))

	a.eventBus.Publish(events.ClusterJoinFailed{
		BaseEvent:   events.NewBaseEvent(),
		ClusterPath: clusterPath,
		Reason:      reason,
	})
}

// checkStaleSessions checks all sessions for staleness, marks them, and cleans up old ones.
func (a *Authenticator) checkStaleSessions() {
	now := time.Now()
	newlyStale := make([]string, 0)
	toRemove := make([]string, 0)

	a.sessionMu.Lock()
	for clusterPath, session := range a.sessions {
		// Check for sessions that should be removed (stale for too long)
		if session.Status == SessionStatusEnum.Stale() &&
			!session.StaleAt.IsZero() &&
			now.Sub(session.StaleAt) > a.maxStaleDuration {
			toRemove = append(toRemove, clusterPath)
			continue
		}

		// Check for newly stale sessions
		if session.Status == SessionStatusEnum.Authenticated() &&
			now.Sub(session.LastActivity) > a.sessionStaleThreshold {
			session.Status = SessionStatusEnum.Stale()
			session.StaleAt = now
			newlyStale = append(newlyStale, clusterPath)
		}
	}

	// Remove sessions that have been stale for too long
	for _, clusterPath := range toRemove {
		delete(a.sessions, clusterPath)
		a.logger.Info("removed stale session",
			zap.String("cluster", clusterPath))
	}
	a.sessionMu.Unlock()

	// Emit events for newly stale sessions (outside the lock)
	for _, clusterPath := range newlyStale {
		// On a single-node cluster (just self in the phonebook) there
		// is nobody to re-auth against — skip the event entirely so
		// idle bootstrap nodes don't log every 5 minutes.
		if a.phonebook != nil {
			if count, err := a.phonebook.CountByCluster(clusterPath); err == nil && count <= 1 {
				a.logger.Debug("session stale on single-node cluster, skipping re-auth",
					zap.String("cluster", clusterPath))
				continue
			}
		}

		a.logger.Info("session stale, triggering re-auth",
			zap.String("cluster", clusterPath))

		a.eventBus.Publish(events.SessionStale{
			BaseEvent:   events.NewBaseEvent(),
			ClusterPath: clusterPath,
		})
	}
}

// publishAuthMessage publishes an auth message to a cluster topic with retry.
func (a *Authenticator) publishAuthMessage(ctx context.Context, clusterPath, msgType string, payload proto.Message) error {
	a.topicMu.RLock()
	topic, ok := a.authTopics[clusterPath]
	a.logger.Debug("publish auth message started", zap.String("cluster", clusterPath), zap.String("topic", topic.String()))
	a.topicMu.RUnlock()

	if !ok {
		a.logger.Error("not subscribed to auth topic",
			zap.String("cluster", clusterPath),
			zap.String("msgType", msgType))
		return fmt.Errorf("not subscribed to auth topic for cluster %s", clusterPath)
	}

	payloadBytes, err := proto.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	envelope := &authpb.AuthMessage{
		Type:      msgType,
		Payload:   payloadBytes,
		SenderId:  a.host.ID().String(),
		Timestamp: timestamppb.Now(),
	}

	envelope.Signature = a.signMessage(envelope)

	data, err := proto.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("failed to marshal envelope: %w", err)
	}

	// Log peers on this topic before publishing
	topicPeers := topic.ListPeers()
	a.logger.Debug("publishing auth message to pubsub",
		zap.String("cluster", clusterPath),
		zap.String("msgType", msgType),
		zap.Int("size", len(data)),
		zap.Int("peersOnTopic", len(topicPeers)),
		zap.Any("peers", peerIDsToStrings(topicPeers)))

	// Publish with retry
	err = shared.Retry(ctx, shared.DefaultRetryConfig(), func() error {
		return topic.Publish(ctx, data)
	})

	if err != nil {
		a.logger.Error("failed to publish auth message",
			zap.String("cluster", clusterPath),
			zap.String("msgType", msgType),
			zap.Error(err))
		return fmt.Errorf("auth publish failed: %w", err)
	}

	a.logger.Debug("successfully published auth message",
		zap.String("cluster", clusterPath),
		zap.String("msgType", msgType))

	return nil
}

// publishStep2 publishes Step 2 broadcast with extended retries.
func (a *Authenticator) publishStep2(ctx context.Context, clusterPath string, announcement *authpb.NewMemberAnnouncement) error {
	cfg := shared.RetryConfig{
		MaxRetries: Step2MaxRetries,
		Delay:      Step2RetryDelay,
	}

	return shared.Retry(ctx, cfg, func() error {
		return a.publishAuthMessage(ctx, clusterPath, "new_member", announcement)
	})
}

// authMessageContent builds the canonical bytes for signing/verifying an AuthMessage envelope.
func authMessageContent(msg *authpb.AuthMessage) []byte {
	buf := signing.NewBuffer(len(msg.Payload) + 128)
	buf.WriteString(msg.Type)
	buf.WriteField(msg.Payload)
	buf.WriteString(msg.SenderId)

	if msg.Timestamp != nil {
		tsBytes, _ := msg.Timestamp.AsTime().MarshalBinary()
		buf.WriteTimeBinary(tsBytes)
	} else {
		buf.WriteField(nil)
	}

	return buf.Bytes()
}

// signMessage signs an auth message envelope using canonical length-prefixed encoding.
func (a *Authenticator) signMessage(msg *authpb.AuthMessage) []byte {
	data := authMessageContent(msg)
	sig, err := a.privateKey.Sign(data)
	if err != nil {
		a.logger.Error("failed to sign auth message", zap.Error(err))
		return nil
	}
	return sig
}

// verifyMessageSignature verifies the signature on an auth message using the sender's public key.
func (a *Authenticator) verifyMessageSignature(msg *authpb.AuthMessage, senderPubKey crypto.PubKey) bool {
	data := authMessageContent(msg)
	ok, err := senderPubKey.Verify(data, msg.Signature)
	return err == nil && ok
}

// lookupPublicKey looks up a node's public key from phonebook.
func (a *Authenticator) lookupPublicKey(nodeID, clusterPath string) (crypto.PubKey, error) {
	if a.phonebook == nil {
		return nil, errors.New("phonebook not configured")
	}

	entry, err := a.phonebook.Get(nodeID, clusterPath)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, errors.New("node not found in phonebook")
	}

	return crypto.UnmarshalPublicKey(entry.PublicKey)
}

// Session represents an authentication session for a cluster.
type Session struct {
	ClusterPath     string
	Status          SessionStatus
	VoucherNodeID   string
	AuthenticatedAt time.Time
	LastActivity    time.Time
	StaleAt         time.Time // When the session became stale (zero if not stale)
}

// SessionStatus represents the authentication state.
type SessionStatus string

const (
	sessionStatusPendingStep2  SessionStatus = "pending_step2"
	sessionStatusAuthenticated SessionStatus = "authenticated"
	sessionStatusStale         SessionStatus = "stale"
	sessionStatusFailed        SessionStatus = "failed"
)

type sessionStatusEnum struct{}

// SessionStatusEnum provides access to SessionStatus values.
var SessionStatusEnum sessionStatusEnum

func (sessionStatusEnum) PendingStep2() SessionStatus  { return sessionStatusPendingStep2 }
func (sessionStatusEnum) Authenticated() SessionStatus { return sessionStatusAuthenticated }
func (sessionStatusEnum) Stale() SessionStatus         { return sessionStatusStale }
func (sessionStatusEnum) Failed() SessionStatus        { return sessionStatusFailed }

// peerIDsToStrings converts a slice of peer IDs to strings for logging.
func peerIDsToStrings(peers []peer.ID) []string {
	result := make([]string, len(peers))
	for i, p := range peers {
		result[i] = p.String()
	}
	return result
}
