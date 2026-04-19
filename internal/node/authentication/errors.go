package authentication

import (
	"fmt"
	"time"
)

// ErrorCode represents authentication error types
type ErrorCode int

const (
	ErrCodeUnknown ErrorCode = iota
	ErrCodeInvalidConfig
	ErrCodeTLSHandshakeFailed
	ErrCodeCertificateInvalid
	ErrCodeCertificateExpired
	ErrCodeCertificateRevoked
	ErrCodeCertificateChainInvalid
	ErrCodeSignatureVerificationFailed
	ErrCodeSignatureInvalid
	ErrCodeKeyMismatch
	ErrCodeProtocolViolation
	ErrCodeSessionNotFound
	ErrCodeSessionExpired
	ErrCodeStreamTimeout
	ErrCodeMessageTimeout
	ErrCodeMaxSessionsExceeded
	ErrCodeClusterMismatch
	ErrCodeInvalidChallenge
	ErrCodeInvalidResponse
	ErrCodePhonebookSyncFailed
)

// AuthenticationError represents an authentication-specific error
type AuthenticationError struct {
	Code      ErrorCode
	Message   string
	PeerID    string
	SessionID string
	Timestamp time.Time
	Retryable bool
	Cause     error
}

func (e AuthenticationError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%s] %s: %v", e.codeString(), e.Message, e.Cause)
	}
	return fmt.Sprintf("[%s] %s", e.codeString(), e.Message)
}

func (e AuthenticationError) Unwrap() error {
	return e.Cause
}

func (e AuthenticationError) codeString() string {
	switch e.Code {
	case ErrCodeInvalidConfig:
		return "INVALID_CONFIG"
	case ErrCodeTLSHandshakeFailed:
		return "TLS_HANDSHAKE_FAILED"
	case ErrCodeCertificateInvalid:
		return "CERTIFICATE_INVALID"
	case ErrCodeCertificateExpired:
		return "CERTIFICATE_EXPIRED"
	case ErrCodeCertificateRevoked:
		return "CERTIFICATE_REVOKED"
	case ErrCodeCertificateChainInvalid:
		return "CERTIFICATE_CHAIN_INVALID"
	case ErrCodeSignatureVerificationFailed:
		return "SIGNATURE_VERIFICATION_FAILED"
	case ErrCodeSignatureInvalid:
		return "SIGNATURE_INVALID"
	case ErrCodeKeyMismatch:
		return "KEY_MISMATCH"
	case ErrCodeProtocolViolation:
		return "PROTOCOL_VIOLATION"
	case ErrCodeSessionNotFound:
		return "SESSION_NOT_FOUND"
	case ErrCodeSessionExpired:
		return "SESSION_EXPIRED"
	case ErrCodeStreamTimeout:
		return "STREAM_TIMEOUT"
	case ErrCodeMessageTimeout:
		return "MESSAGE_TIMEOUT"
	case ErrCodeMaxSessionsExceeded:
		return "MAX_SESSIONS_EXCEEDED"
	case ErrCodeClusterMismatch:
		return "CLUSTER_MISMATCH"
	case ErrCodeInvalidChallenge:
		return "INVALID_CHALLENGE"
	case ErrCodeInvalidResponse:
		return "INVALID_RESPONSE"
	case ErrCodePhonebookSyncFailed:
		return "PHONEBOOK_SYNC_FAILED"
	default:
		return "UNKNOWN"
	}
}

// Error constructor functions
func ErrInvalidConfig(message string) error {
	return AuthenticationError{
		Code:      ErrCodeInvalidConfig,
		Message:   message,
		Timestamp: time.Now(),
		Retryable: false,
	}
}

func ErrTLSHandshakeFailed(peerID, sessionID string, cause error) error {
	return AuthenticationError{
		Code:      ErrCodeTLSHandshakeFailed,
		Message:   "TLS handshake failed",
		PeerID:    peerID,
		SessionID: sessionID,
		Timestamp: time.Now(),
		Retryable: true,
		Cause:     cause,
	}
}

func ErrCertificateInvalid(peerID, sessionID string, cause error) error {
	return AuthenticationError{
		Code:      ErrCodeCertificateInvalid,
		Message:   "Certificate validation failed",
		PeerID:    peerID,
		SessionID: sessionID,
		Timestamp: time.Now(),
		Retryable: false,
		Cause:     cause,
	}
}

func ErrSignatureVerificationFailed(peerID, sessionID string, cause error) error {
	return AuthenticationError{
		Code:      ErrCodeSignatureVerificationFailed,
		Message:   "Signature verification failed",
		PeerID:    peerID,
		SessionID: sessionID,
		Timestamp: time.Now(),
		Retryable: false,
		Cause:     cause,
	}
}

func ErrProtocolViolation(peerID, sessionID, message string) error {
	return AuthenticationError{
		Code:      ErrCodeProtocolViolation,
		Message:   message,
		PeerID:    peerID,
		SessionID: sessionID,
		Timestamp: time.Now(),
		Retryable: false,
	}
}

func ErrSessionNotFound(sessionID string) error {
	return AuthenticationError{
		Code:      ErrCodeSessionNotFound,
		Message:   "Authentication session not found",
		SessionID: sessionID,
		Timestamp: time.Now(),
		Retryable: false,
	}
}

func ErrSessionExpired(peerID, sessionID string) error {
	return AuthenticationError{
		Code:      ErrCodeSessionExpired,
		Message:   "Authentication session expired",
		PeerID:    peerID,
		SessionID: sessionID,
		Timestamp: time.Now(),
		Retryable: true,
	}
}

func ErrStreamTimeout(peerID, sessionID string) error {
	return AuthenticationError{
		Code:      ErrCodeStreamTimeout,
		Message:   "Stream operation timeout",
		PeerID:    peerID,
		SessionID: sessionID,
		Timestamp: time.Now(),
		Retryable: true,
	}
}

func ErrMaxSessionsExceeded() error {
	return AuthenticationError{
		Code:      ErrCodeMaxSessionsExceeded,
		Message:   "Maximum concurrent sessions exceeded",
		Timestamp: time.Now(),
		Retryable: true,
	}
}

func ErrClusterMismatch(expected, received string) error {
	return AuthenticationError{
		Code:      ErrCodeClusterMismatch,
		Message:   fmt.Sprintf("Cluster mismatch: expected %s, received %s", expected, received),
		Timestamp: time.Now(),
		Retryable: false,
	}
}

func ErrInvalidChallenge(peerID, sessionID string) error {
	return AuthenticationError{
		Code:      ErrCodeInvalidChallenge,
		Message:   "Invalid challenge data",
		PeerID:    peerID,
		SessionID: sessionID,
		Timestamp: time.Now(),
		Retryable: false,
	}
}

func ErrInvalidResponse(peerID, sessionID string) error {
	return AuthenticationError{
		Code:      ErrCodeInvalidResponse,
		Message:   "Invalid response data",
		PeerID:    peerID,
		SessionID: sessionID,
		Timestamp: time.Now(),
		Retryable: false,
	}
}

func ErrPhonebookSyncFailed(peerID, sessionID string, cause error) error {
	return AuthenticationError{
		Code:      ErrCodePhonebookSyncFailed,
		Message:   "Phonebook synchronization failed",
		PeerID:    peerID,
		SessionID: sessionID,
		Timestamp: time.Now(),
		Retryable: true,
		Cause:     cause,
	}
}

func ErrCertificateExpired(peerID, sessionID string, expiredAt time.Time) error {
	return AuthenticationError{
		Code:      ErrCodeCertificateExpired,
		Message:   fmt.Sprintf("Certificate expired at %s", expiredAt.Format(time.RFC3339)),
		PeerID:    peerID,
		SessionID: sessionID,
		Timestamp: time.Now(),
		Retryable: false,
	}
}

func ErrCertificateRevoked(peerID, sessionID string, reason string) error {
	return AuthenticationError{
		Code:      ErrCodeCertificateRevoked,
		Message:   fmt.Sprintf("Certificate revoked: %s", reason),
		PeerID:    peerID,
		SessionID: sessionID,
		Timestamp: time.Now(),
		Retryable: false,
	}
}

func ErrCertificateChainInvalid(peerID, sessionID string, cause error) error {
	return AuthenticationError{
		Code:      ErrCodeCertificateChainInvalid,
		Message:   "Certificate chain validation failed",
		PeerID:    peerID,
		SessionID: sessionID,
		Timestamp: time.Now(),
		Retryable: false,
		Cause:     cause,
	}
}

func ErrSignatureInvalid(peerID, sessionID string, reason string) error {
	return AuthenticationError{
		Code:      ErrCodeSignatureInvalid,
		Message:   fmt.Sprintf("Invalid signature: %s", reason),
		PeerID:    peerID,
		SessionID: sessionID,
		Timestamp: time.Now(),
		Retryable: false,
	}
}

func ErrKeyMismatch(peerID, sessionID string, reason string) error {
	return AuthenticationError{
		Code:      ErrCodeKeyMismatch,
		Message:   fmt.Sprintf("Key mismatch: %s", reason),
		PeerID:    peerID,
		SessionID: sessionID,
		Timestamp: time.Now(),
		Retryable: false,
	}
}