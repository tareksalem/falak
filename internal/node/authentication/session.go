package authentication

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// AuthSession represents a temporary authentication session during the 4-step protocol
// This is deleted immediately after authentication completes (success/failure)
// Long-term peer tracking is handled by the existing phonebook
type AuthSession struct {
	// Identity
	ID        string
	PeerID    peer.ID
	ClusterID string

	// Protocol state
	State         AuthState
	IsClient      bool
	Stream        network.Stream
	Challenge     []byte
	PeerPublicKey ed25519.PublicKey

	// Timing (for timeout detection)
	StartTime    time.Time
	LastActivity time.Time

	// Thread safety
	mutex sync.RWMutex
}

// NewAuthSession creates a new temporary authentication session
func NewAuthSession(peerID peer.ID, clusterID string, isClient bool) (*AuthSession, error) {
	sessionID, err := generateSessionID()
	if err != nil {
		return nil, err
	}

	return &AuthSession{
		ID:           sessionID,
		PeerID:       peerID,
		ClusterID:    clusterID,
		State:        AuthStateEnum.Initiated,
		IsClient:     isClient,
		StartTime:    time.Now(),
		LastActivity: time.Now(),
	}, nil
}

// UpdateState updates the session state and activity time
func (as *AuthSession) UpdateState(newState AuthState) {
	as.mutex.Lock()
	defer as.mutex.Unlock()

	as.State = newState
	as.LastActivity = time.Now()
}

// GetState returns the current session state (thread-safe)
func (as *AuthSession) GetState() AuthState {
	as.mutex.RLock()
	defer as.mutex.RUnlock()

	return as.State
}

// SetStream sets the network stream for this session
func (as *AuthSession) SetStream(stream network.Stream) {
	as.mutex.Lock()
	defer as.mutex.Unlock()

	as.Stream = stream
	as.LastActivity = time.Now()
}

// SetChallenge sets the challenge data for this session
func (as *AuthSession) SetChallenge(challenge []byte) {
	as.mutex.Lock()
	defer as.mutex.Unlock()

	as.Challenge = make([]byte, len(challenge))
	copy(as.Challenge, challenge)
	as.LastActivity = time.Now()
}

// GetChallenge returns a copy of the challenge data
func (as *AuthSession) GetChallenge() []byte {
	as.mutex.RLock()
	defer as.mutex.RUnlock()

	if as.Challenge == nil {
		return nil
	}

	challenge := make([]byte, len(as.Challenge))
	copy(challenge, as.Challenge)
	return challenge
}

// SetPeerPublicKey stores the peer's public key extracted from their certificate
func (as *AuthSession) SetPeerPublicKey(publicKey ed25519.PublicKey) {
	as.mutex.Lock()
	defer as.mutex.Unlock()

	as.PeerPublicKey = make(ed25519.PublicKey, len(publicKey))
	copy(as.PeerPublicKey, publicKey)
	as.LastActivity = time.Now()
}

// GetPeerPublicKey returns the peer's public key (for signature verification)
func (as *AuthSession) GetPeerPublicKey() ed25519.PublicKey {
	as.mutex.RLock()
	defer as.mutex.RUnlock()

	if as.PeerPublicKey == nil {
		return nil
	}

	publicKey := make(ed25519.PublicKey, len(as.PeerPublicKey))
	copy(publicKey, as.PeerPublicKey)
	return publicKey
}

// IsExpired checks if the session has exceeded the timeout
func (as *AuthSession) IsExpired(timeout time.Duration) bool {
	as.mutex.RLock()
	defer as.mutex.RUnlock()

	return time.Since(as.LastActivity) > timeout
}

// UpdateActivity updates the last activity timestamp
func (as *AuthSession) UpdateActivity() {
	as.mutex.Lock()
	defer as.mutex.Unlock()

	as.LastActivity = time.Now()
}

// Close closes the session stream if it exists
func (as *AuthSession) Close() {
	as.mutex.Lock()
	defer as.mutex.Unlock()

	if as.Stream != nil {
		as.Stream.Close()
		as.Stream = nil
	}
}

// GetDuration returns how long the session has been active
func (as *AuthSession) GetDuration() time.Duration {
	as.mutex.RLock()
	defer as.mutex.RUnlock()

	return time.Since(as.StartTime)
}

// generateSessionID creates a unique session identifier
func generateSessionID() (string, error) {
	bytes := make([]byte, 8) // 64-bit random ID (shorter since temporary)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// Session management methods for AuthenticationManager

// createSession creates a new authentication session
func (am *AuthenticationManager) createSession(peerID peer.ID, isClient bool) (*AuthSession, error) {
	// Check if we already have an active session for this peer
	existingSession, exists := am.getSessionByPeer(peerID)
	fmt.Println("========= checking session exists result", existingSession, exists)
	if exists {
		// If already authenticated, return the existing session
		if existingSession.GetState() == AuthStateEnum.Authenticated {
			return existingSession, nil
		}
		// If authentication in progress, return error
		if !existingSession.GetState().IsTerminal() {
			return nil, ErrProtocolViolation(peerID.String(), existingSession.ID,
				"authentication already in progress")
		}
		// Clean up old terminated session
		am.cleanupSession(existingSession.ID)
	}

	// Check session limits
	activeCount := 0
	am.sessions.Range(func(key, value interface{}) bool {
		activeCount++
		return true
	})

	if activeCount >= am.config.MaxSessions {
		return nil, ErrMaxSessionsExceeded()
	}

	// Create new session
	session, err := NewAuthSession(peerID, am.config.GetPrimaryCluster(), isClient)
	if err != nil {
		return nil, err
	}

	// Store session
	am.sessions.Store(session.ID, session)

	// Emit session created event
	event := NewAuthEvent(EventTypeEnum.SessionCreated, peerID, session.ID).
		WithClusterID(session.ClusterID)
	am.EmitEvent(event)

	return session, nil
}

// getSession retrieves a session by ID
func (am *AuthenticationManager) getSession(sessionID string) (*AuthSession, bool) {
	if session, exists := am.sessions.Load(sessionID); exists {
		return session.(*AuthSession), true
	}
	return nil, false
}

// getSessionByPeer retrieves a session by peer ID
func (am *AuthenticationManager) getSessionByPeer(peerID peer.ID) (*AuthSession, bool) {
	var foundSession *AuthSession

	am.sessions.Range(func(key, value interface{}) bool {
		session := value.(*AuthSession)
		if session.PeerID == peerID {
			foundSession = session
			return false // Stop iteration
		}
		return true // Continue iteration
	})

	return foundSession, foundSession != nil
}

// cleanupSession removes a session (only for failed/expired sessions)
func (am *AuthenticationManager) cleanupSession(sessionID string) {
	if session, exists := am.sessions.Load(sessionID); exists {
		authSession := session.(*AuthSession)

		// Only cleanup if not authenticated (keep authenticated sessions alive)
		if authSession.GetState() != AuthStateEnum.Authenticated {
			// Close the stream
			authSession.Close()

			// Remove from sessions map
			am.sessions.Delete(sessionID)

			// Emit cleanup event
			event := NewAuthEvent(EventTypeEnum.SessionCleaned, authSession.PeerID, sessionID)
			am.EmitEvent(event)
		}
	}
}

// forceCleanupSession removes any session regardless of state (for shutdown)
func (am *AuthenticationManager) forceCleanupSession(sessionID string) {
	if session, exists := am.sessions.Load(sessionID); exists {
		authSession := session.(*AuthSession)

		// Close the stream
		authSession.Close()

		// Remove from sessions map
		am.sessions.Delete(sessionID)

		// Emit cleanup event
		event := NewAuthEvent(EventTypeEnum.SessionCleaned, authSession.PeerID, sessionID)
		am.EmitEvent(event)
	}
}

// getExpiredSessions returns sessions that have exceeded the timeout
func (am *AuthenticationManager) getExpiredSessions() []*AuthSession {
	var expiredSessions []*AuthSession
	timeout := am.config.SessionTimeout

	am.sessions.Range(func(key, value interface{}) bool {
		session := value.(*AuthSession)
		if session.IsExpired(timeout) {
			expiredSessions = append(expiredSessions, session)
		}
		return true
	})

	return expiredSessions
}
