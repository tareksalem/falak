package authentication

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/reactivex/rxgo/v2"
)

// AuthenticationManager is the central coordinator for all authentication activities
type AuthenticationManager struct {
	// Configuration
	config *Config

	// libp2p integration
	host host.Host

	// Reactive streams (rxgo)
	eventSubject chan rxgo.Item
	eventStream  rxgo.Observable

	// Session management
	sessions sync.Map // sessionID -> *AuthSession

	// Components (stubs for now - will be implemented in later tasks)
	client *AuthClient
	server *AuthServer

	// Lifecycle management
	ctx     context.Context
	cancel  context.CancelFunc
	started atomic.Bool
	wg      sync.WaitGroup

	// Metrics and monitoring
	sessionCount     atomic.Int64
	authSuccessCount atomic.Int64
	authFailureCount atomic.Int64

	// Session cleanup
	cleanupInterval time.Duration
	cleanupTicker   *time.Ticker

	// Retry management (for future SWIM integration)
	retryManager *RetryManager
}

// AuthClient is now implemented in client.go

// AuthServer is now implemented in server.go

// ManagerStats provides statistics about the authentication manager
type ManagerStats struct {
	SessionCount     int64
	AuthSuccessCount int64
	AuthFailureCount int64
	IsRunning        bool
	ActiveSessions   int
}

// ManagerOption defines configuration options for the manager
type ManagerOption func(*AuthenticationManager)

// WithConfig sets the configuration for the manager
func WithConfig(config *Config) ManagerOption {
	return func(am *AuthenticationManager) {
		am.config = config
	}
}

// WithHost sets the libp2p host for the manager
func WithHost(host host.Host) ManagerOption {
	return func(am *AuthenticationManager) {
		am.host = host
	}
}

// WithSessionTimeout sets the session timeout duration
func WithSessionTimeout(timeout time.Duration) ManagerOption {
	return func(am *AuthenticationManager) {
		if am.config != nil {
			am.config.SessionTimeout = timeout
		}
	}
}

// NewAuthenticationManager creates a new authentication manager with the given options
func NewAuthenticationManager(opts ...ManagerOption) *AuthenticationManager {
	ctx, cancel := context.WithCancel(context.Background())

	am := &AuthenticationManager{
		eventSubject:    make(chan rxgo.Item, 1000), // Buffered channel for events
		ctx:             ctx,
		cancel:          cancel,
		cleanupInterval: 30 * time.Second, // Default cleanup interval
	}

	// Apply options
	for _, opt := range opts {
		opt(am)
	}

	// Set default config if none provided
	if am.config == nil {
		am.config = DefaultConfig()
	}

	// Update cleanup interval from config
	if am.config.CleanupInterval > 0 {
		am.cleanupInterval = am.config.CleanupInterval
	}

	// Create rxgo observable from event subject
	am.eventStream = rxgo.FromChannel(am.eventSubject, rxgo.WithContext(am.ctx))

	// Initialize components
	// AuthClient and AuthServer will be initialized in Start() when host is available

	return am
}

// Start initializes and starts the authentication manager
func (am *AuthenticationManager) Start() error {
	if am.started.Load() {
		return fmt.Errorf("authentication manager is already started")
	}

	// Validate configuration
	if err := am.config.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// Set host from config if not already set
	if am.host == nil && am.config.Host != nil {
		am.host = am.config.Host
	}

	if am.host == nil {
		return ErrInvalidConfig("libp2p host is required")
	}

	log.Printf("🔐 Starting Authentication Manager for node %s", am.config.NodeID)

	// Initialize crypto components if real crypto is enabled
	var cryptoManager *CryptoManager
	if am.config.EnableRealCrypto {
		// Initialize Certificate Manager
		certManager := NewCertificateManager(am.config)

		// Initialize Crypto Manager
		cryptoManager = NewCryptoManager(certManager)
		log.Printf("✅ Real cryptographic operations enabled")
	} else {
		// Use placeholder crypto for development
		cryptoManager = nil
		log.Printf("⚠️ Using placeholder cryptographic operations (development mode)")
	}

	// Initialize AuthClient with crypto manager
	am.client = NewAuthClient(am, am.host, cryptoManager)

	// Initialize and start AuthServer with crypto manager
	am.server = NewAuthServer(am, am.host, cryptoManager)
	if err := am.server.Start(); err != nil {
		return fmt.Errorf("failed to start authentication server: %w", err)
	}

	// Initialize and start retry manager
	am.retryManager = NewRetryManager(am.config)
	if err := am.retryManager.Start(); err != nil {
		return fmt.Errorf("failed to start retry manager: %w", err)
	}

	// Start event processing goroutine
	am.wg.Add(1)
	go am.eventProcessor()

	// Start session cleanup goroutine
	am.wg.Add(1)
	go am.sessionCleanupWorker()

	// Mark as started
	am.started.Store(true)

	log.Printf("✅ Authentication Manager started successfully")
	return nil
}

// Stop gracefully shuts down the authentication manager
func (am *AuthenticationManager) Stop() error {
	if !am.started.Load() {
		return fmt.Errorf("authentication manager is not running")
	}

	log.Printf("🔐 Stopping Authentication Manager...")

	// Stop AuthServer
	if am.server != nil {
		if err := am.server.Stop(); err != nil {
			log.Printf("⚠️ Error stopping authentication server: %v", err)
		}
	}

	// Stop retry manager
	if am.retryManager != nil {
		if err := am.retryManager.Stop(); err != nil {
			log.Printf("⚠️ Error stopping retry manager: %v", err)
		}
	}

	// Cancel context to signal shutdown
	am.cancel()

	// Stop cleanup ticker if running
	if am.cleanupTicker != nil {
		am.cleanupTicker.Stop()
	}

	// Close event subject
	close(am.eventSubject)

	// Wait for all goroutines to complete
	am.wg.Wait()

	// Cleanup all active sessions (force cleanup during shutdown)
	am.sessions.Range(func(key, value interface{}) bool {
		sessionID := key.(string)
		am.forceCleanupSession(sessionID)
		return true
	})

	// Mark as stopped
	am.started.Store(false)

	log.Printf("✅ Authentication Manager stopped")
	return nil
}

// IsRunning returns true if the manager is currently running
func (am *AuthenticationManager) IsRunning() bool {
	return am.started.Load()
}

// GetStats returns current statistics about the manager
func (am *AuthenticationManager) GetStats() ManagerStats {
	activeSessionCount := 0
	am.sessions.Range(func(key, value interface{}) bool {
		activeSessionCount++
		return true
	})

	return ManagerStats{
		SessionCount:     am.sessionCount.Load(),
		AuthSuccessCount: am.authSuccessCount.Load(),
		AuthFailureCount: am.authFailureCount.Load(),
		IsRunning:        am.started.Load(),
		ActiveSessions:   activeSessionCount,
	}
}

// EmitEvent emits an authentication event to the reactive stream
func (am *AuthenticationManager) EmitEvent(event AuthEvent) {
	if !am.started.Load() {
		log.Printf("⚠️ Attempted to emit event while manager is stopped: %s", event.Type)
		return
	}

	// Enrich event with timestamp if not set
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	// Update metrics based on event type
	am.updateMetrics(event)

	// Emit to rxgo stream
	select {
	case am.eventSubject <- rxgo.Of(event):
		// Event emitted successfully
	case <-am.ctx.Done():
		// Manager is shutting down
		return
	default:
		// Channel is full, log warning
		log.Printf("⚠️ Event channel full, dropping event: %s", event.Type)
	}
}

// Subscribe returns an observable that filters events by type
func (am *AuthenticationManager) Subscribe(eventType EventType) rxgo.Observable {
	return am.eventStream.Filter(func(item interface{}) bool {
		if event, ok := item.(AuthEvent); ok {
			return event.Type == eventType
		}
		return false
	})
}

// SubscribeAll returns an observable with all authentication events
func (am *AuthenticationManager) SubscribeAll() rxgo.Observable {
	return am.eventStream
}

// AuthenticatePeer initiates authentication with the specified peer
func (am *AuthenticationManager) AuthenticatePeer(peerID peer.ID) error {
	if !am.started.Load() {
		return fmt.Errorf("authentication manager is not running")
	}

	// Check if already authenticated or in progress
	existingSession, exists := am.getSessionByPeer(peerID)
	fmt.Println("duplicate records check result", existingSession, exists)
	if exists {
		if existingSession.State == AuthStateEnum.Authenticated {
			return fmt.Errorf("peer %s is already authenticated", peerID)
		}
		if existingSession != nil && !existingSession.State.IsTerminal() {
			return fmt.Errorf("authentication with peer %s is already in progress", peerID)
		}
	}

	// Delegate to client component - let it handle session creation
	return am.client.AuthenticatePeer(peerID)
}

// GetAuthenticatedPeers returns a list of currently authenticated peers
func (am *AuthenticationManager) GetAuthenticatedPeers() []peer.ID {
	var authenticatedPeers []peer.ID

	am.sessions.Range(func(key, value interface{}) bool {
		if session, ok := value.(*AuthSession); ok {
			if session.State == AuthStateEnum.Authenticated {
				authenticatedPeers = append(authenticatedPeers, session.PeerID)
			}
		}
		return true
	})

	return authenticatedPeers
}

// GetAuthenticatedSessions returns a map of all authenticated sessions
func (am *AuthenticationManager) GetAuthenticatedSessions() map[peer.ID]*AuthSession {
	authenticatedSessions := make(map[peer.ID]*AuthSession)

	am.sessions.Range(func(key, value interface{}) bool {
		if session, ok := value.(*AuthSession); ok {
			if session.State == AuthStateEnum.Authenticated {
				authenticatedSessions[session.PeerID] = session
			}
		}
		return true
	})

	return authenticatedSessions
}

// GetAuthenticatedStream returns the stream for an authenticated peer
func (am *AuthenticationManager) GetAuthenticatedStream(peerID peer.ID) (network.Stream, bool) {
	if session, exists := am.getSessionByPeer(peerID); exists {
		if session.State == AuthStateEnum.Authenticated && session.Stream != nil {
			return session.Stream, true
		}
	}
	return nil, false
}

// IsAuthenticated checks if the specified peer is authenticated
func (am *AuthenticationManager) IsAuthenticated(peerID peer.ID) bool {
	if session, exists := am.getSessionByPeer(peerID); exists {
		return session.State == AuthStateEnum.Authenticated
	}
	return false
}

// GetSessionState returns the current authentication state for the specified peer
func (am *AuthenticationManager) GetSessionState(peerID peer.ID) (AuthState, error) {
	if session, exists := am.getSessionByPeer(peerID); exists {
		return session.State, nil
	}
	return AuthState(""), ErrSessionNotFound(peerID.String())
}

// GetConfig returns the current configuration
func (am *AuthenticationManager) GetConfig() *Config {
	return am.config
}

// GetRetryStats returns retry statistics (for monitoring and debugging)
func (am *AuthenticationManager) GetRetryStats() map[string]interface{} {
	if am.retryManager != nil {
		return am.retryManager.GetRetryStats()
	}
	return map[string]interface{}{
		"retry_enabled": false,
	}
}

// GetRetryManager returns the retry manager (for external retry scheduling)
func (am *AuthenticationManager) GetRetryManager() *RetryManager {
	return am.retryManager
}

// UpdateConfig updates the configuration (some settings require restart)
func (am *AuthenticationManager) UpdateConfig(config *Config) error {
	if err := config.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	am.config = config
	log.Printf("🔧 Configuration updated for Authentication Manager")
	return nil
}

// eventProcessor processes events from the reactive stream
func (am *AuthenticationManager) eventProcessor() {
	defer am.wg.Done()

	log.Printf("🔄 Starting event processor")

	// Process events from the stream
	for item := range am.eventStream.Observe() {
		if item.Error() {
			log.Printf("❌ Error in event stream: %v", item.E)
			continue
		}

		if event, ok := item.V.(AuthEvent); ok {
			am.processEvent(event)
		}
	}
}

// processEvent handles individual events
func (am *AuthenticationManager) processEvent(event AuthEvent) {
	// Log the event for debugging
	log.Printf("📡 Processing event: %s for peer %s (session: %s)",
		event.Type, event.PeerID.ShortString(), event.SessionID)

	// Handle session state updates
	if session, exists := am.getSession(event.SessionID); exists {
		am.updateSessionFromEvent(session, event)
	}

	// Handle cleanup events
	if event.Type == EventTypeEnum.AuthenticationFailed {
		// Only cleanup failed authentications, not successful ones
		go func() {
			time.Sleep(5 * time.Second) // Grace period
			am.cleanupSession(event.SessionID)
		}()
	}
}

// sessionCleanupWorker periodically cleans up expired sessions
func (am *AuthenticationManager) sessionCleanupWorker() {
	defer am.wg.Done()

	am.cleanupTicker = time.NewTicker(am.cleanupInterval)
	defer am.cleanupTicker.Stop()

	log.Printf("🧹 Starting session cleanup worker (interval: %v)", am.cleanupInterval)

	for {
		select {
		case <-am.cleanupTicker.C:
			am.cleanupExpiredSessions()
		case <-am.ctx.Done():
			log.Printf("🧹 Session cleanup worker stopped")
			return
		}
	}
}

// cleanupExpiredSessions removes expired sessions
func (am *AuthenticationManager) cleanupExpiredSessions() {
	expiredSessions := am.getExpiredSessions()

	// Clean up expired sessions
	for _, session := range expiredSessions {
		log.Printf("🧹 Cleaning up expired session: %s (peer: %s)",
			session.ID, session.PeerID.ShortString())

		// Emit expired event
		event := NewAuthEvent(EventTypeEnum.SessionExpired, session.PeerID, session.ID)
		am.EmitEvent(event)

		// Remove from sessions
		am.cleanupSession(session.ID)
	}

	if len(expiredSessions) > 0 {
		log.Printf("🧹 Cleaned up %d expired sessions", len(expiredSessions))
	}
}

// updateMetrics updates internal metrics based on events
func (am *AuthenticationManager) updateMetrics(event AuthEvent) {
	switch event.Type {
	case EventTypeEnum.SessionCreated:
		am.sessionCount.Add(1)
	case EventTypeEnum.AuthenticationCompleted:
		am.authSuccessCount.Add(1)
	case EventTypeEnum.AuthenticationFailed:
		am.authFailureCount.Add(1)
	}
}

// updateSessionFromEvent updates session state based on received events
func (am *AuthenticationManager) updateSessionFromEvent(session *AuthSession, event AuthEvent) {
	// Update last activity
	session.LastActivity = event.Timestamp

	// Update state based on event type
	switch event.Type {
	case EventTypeEnum.ClientHelloSent:
		session.State = AuthStateEnum.ClientHelloSent
	case EventTypeEnum.ServerChallengeSent:
		session.State = AuthStateEnum.ServerChallengeSent
	case EventTypeEnum.ClientResponseSent:
		session.State = AuthStateEnum.ClientResponseSent
	case EventTypeEnum.ServerAckSent:
		session.State = AuthStateEnum.ServerAckSent
	case EventTypeEnum.AuthenticationCompleted:
		session.State = AuthStateEnum.Authenticated
	case EventTypeEnum.AuthenticationFailed:
		session.State = AuthStateEnum.Failed
	}
}

// Component implementations:
// - AuthClient methods are implemented in client.go
// - AuthServer methods are implemented in server.go
// - Session management methods are implemented in session.go
