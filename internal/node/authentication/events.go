package authentication

import (
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// EventType represents different types of authentication events
type EventType string

const (
	// Connection events
	peerConnected    EventType = "peer_connected"
	peerDisconnected EventType = "peer_disconnected"

	// Authentication lifecycle events
	authenticationStarted   EventType = "authentication_started"
	authenticationCompleted EventType = "authentication_completed"
	authenticationFailed    EventType = "authentication_failed"

	// Protocol step events
	clientHelloSent        EventType = "client_hello_sent"
	clientHelloReceived    EventType = "client_hello_received"
	serverChallengeSent    EventType = "server_challenge_sent"
	serverChallengeReceived EventType = "server_challenge_received"
	clientResponseSent     EventType = "client_response_sent"
	clientResponseReceived EventType = "client_response_received"
	serverAckSent          EventType = "server_ack_sent"
	serverAckReceived      EventType = "server_ack_received"

	// Certificate events
	certificateValidated        EventType = "certificate_validated"
	certificateValidationFailed EventType = "certificate_validation_failed"

	// Session events
	sessionCreated EventType = "session_created"
	sessionExpired EventType = "session_expired"
	sessionCleaned EventType = "session_cleaned"

	// Phonebook events
	phonebookUpdateReceived EventType = "phonebook_update_received"
	phonebookUpdateApplied  EventType = "phonebook_update_applied"
)

var EventTypeEnum = struct {
	// Connection events
	PeerConnected    EventType
	PeerDisconnected EventType

	// Authentication lifecycle events
	AuthenticationStarted   EventType
	AuthenticationCompleted EventType
	AuthenticationFailed    EventType

	// Protocol step events
	ClientHelloSent        EventType
	ClientHelloReceived    EventType
	ServerChallengeSent    EventType
	ServerChallengeReceived EventType
	ClientResponseSent     EventType
	ClientResponseReceived EventType
	ServerAckSent          EventType
	ServerAckReceived      EventType

	// Certificate events
	CertificateValidated        EventType
	CertificateValidationFailed EventType

	// Session events
	SessionCreated EventType
	SessionExpired EventType
	SessionCleaned EventType

	// Phonebook events
	PhonebookUpdateReceived EventType
	PhonebookUpdateApplied  EventType
}{
	// Connection events
	PeerConnected:    peerConnected,
	PeerDisconnected: peerDisconnected,

	// Authentication lifecycle events
	AuthenticationStarted:   authenticationStarted,
	AuthenticationCompleted: authenticationCompleted,
	AuthenticationFailed:    authenticationFailed,

	// Protocol step events
	ClientHelloSent:        clientHelloSent,
	ClientHelloReceived:    clientHelloReceived,
	ServerChallengeSent:    serverChallengeSent,
	ServerChallengeReceived: serverChallengeReceived,
	ClientResponseSent:     clientResponseSent,
	ClientResponseReceived: clientResponseReceived,
	ServerAckSent:          serverAckSent,
	ServerAckReceived:      serverAckReceived,

	// Certificate events
	CertificateValidated:        certificateValidated,
	CertificateValidationFailed: certificateValidationFailed,

	// Session events
	SessionCreated: sessionCreated,
	SessionExpired: sessionExpired,
	SessionCleaned: sessionCleaned,

	// Phonebook events
	PhonebookUpdateReceived: phonebookUpdateReceived,
	PhonebookUpdateApplied:  phonebookUpdateApplied,
}

// AuthState represents the state of an authentication session
type AuthState string

const (
	initiated           AuthState = "initiated"
	clientHelloSentState     AuthState = "client_hello_sent"
	serverChallengeSentState AuthState = "server_challenge_sent"
	clientResponseSentState  AuthState = "client_response_sent"
	serverAckSentState       AuthState = "server_ack_sent"
	authenticated       AuthState = "authenticated"
	failed              AuthState = "failed"
	expired             AuthState = "expired"
)

var AuthStateEnum = struct {
	Initiated           AuthState
	ClientHelloSent     AuthState
	ServerChallengeSent AuthState
	ClientResponseSent  AuthState
	ServerAckSent       AuthState
	Authenticated       AuthState
	Failed              AuthState
	Expired             AuthState
}{
	Initiated:           initiated,
	ClientHelloSent:     clientHelloSentState,
	ServerChallengeSent: serverChallengeSentState,
	ClientResponseSent:  clientResponseSentState,
	ServerAckSent:       serverAckSentState,
	Authenticated:       authenticated,
	Failed:              failed,
	Expired:             expired,
}

// AuthEvent represents an authentication event
type AuthEvent struct {
	Type      EventType
	PeerID    peer.ID
	SessionID string
	ClusterID string
	Timestamp time.Time
	Data      interface{}
	Error     error
}

// NewAuthEvent creates a new authentication event
func NewAuthEvent(eventType EventType, peerID peer.ID, sessionID string) AuthEvent {
	return AuthEvent{
		Type:      eventType,
		PeerID:    peerID,
		SessionID: sessionID,
		Timestamp: time.Now(),
	}
}

// WithClusterID adds cluster ID to the event
func (e AuthEvent) WithClusterID(clusterID string) AuthEvent {
	e.ClusterID = clusterID
	return e
}

// WithData adds data to the event
func (e AuthEvent) WithData(data interface{}) AuthEvent {
	e.Data = data
	return e
}

// WithError adds error to the event
func (e AuthEvent) WithError(err error) AuthEvent {
	e.Error = err
	return e
}

// Event data payloads

// AuthenticationCompletedData contains data for successful authentication
type AuthenticationCompletedData struct {
	ClusterID      string
	Certificate    []byte
	PhonebookDelta []byte
	AuthDuration   time.Duration
	TrustLevel     float64
}

// AuthenticationFailedData contains data for failed authentication
type AuthenticationFailedData struct {
	Reason     string
	ErrorCode  ErrorCode
	Retryable  bool
	RetryAfter time.Duration
}

// CertificateValidatedData contains certificate validation results
type CertificateValidatedData struct {
	Valid      bool
	Issuer     string
	Subject    string
	ExpiresAt  time.Time
	TrustLevel float64
}

// PhonebookUpdateData contains phonebook delta information
type PhonebookUpdateData struct {
	DataCenterID  string
	PeerCount     int
	UpdateSize    int
	Authoritative bool
}

// SessionData contains session information
type SessionData struct {
	State        AuthState
	IsClient     bool
	StartTime    time.Time
	LastActivity time.Time
	MessageCount int
}

// IsTerminal returns true if this state represents a final state
func (as AuthState) IsTerminal() bool {
	return as == AuthStateEnum.Authenticated ||
		   as == AuthStateEnum.Failed ||
		   as == AuthStateEnum.Expired
}

// IsAuthenticationEvent returns true if this is a core authentication event
func (et EventType) IsAuthenticationEvent() bool {
	return et == EventTypeEnum.AuthenticationStarted ||
		   et == EventTypeEnum.AuthenticationCompleted ||
		   et == EventTypeEnum.AuthenticationFailed
}

// IsProtocolEvent returns true if this is a protocol step event
func (et EventType) IsProtocolEvent() bool {
	protocolEvents := []EventType{
		EventTypeEnum.ClientHelloSent,
		EventTypeEnum.ClientHelloReceived,
		EventTypeEnum.ServerChallengeSent,
		EventTypeEnum.ServerChallengeReceived,
		EventTypeEnum.ClientResponseSent,
		EventTypeEnum.ClientResponseReceived,
		EventTypeEnum.ServerAckSent,
		EventTypeEnum.ServerAckReceived,
	}

	for _, event := range protocolEvents {
		if et == event {
			return true
		}
	}
	return false
}

// IsSessionEvent returns true if this is a session-related event
func (et EventType) IsSessionEvent() bool {
	return et == EventTypeEnum.SessionCreated ||
		   et == EventTypeEnum.SessionExpired ||
		   et == EventTypeEnum.SessionCleaned
}