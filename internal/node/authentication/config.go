package authentication

import (
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// Config holds configuration for the authentication manager
type Config struct {
	// Node identification
	NodeID      string
	Clusters    []string
	DataCenters []string

	// libp2p integration
	Host host.Host

	// Certificate configuration
	CABasePath         string        // Base path for CA directories (e.g., "/etc/falak/ca")
	CertificatePath    string        // Path to node certificate
	PrivateKeyPath     string        // Path to node private key
	RootCAPath         string        // Path to cluster root CA (overrides auto-discovery)
	CertificateChain   []string      // Certificate chain paths

	// Cryptographic settings
	SignatureAlgorithm string        // "Ed25519" (default)
	CertValidationMode string        // "strict", "relaxed"
	AllowSelfSigned    bool          // For development only
	EnableRealCrypto   bool          // Enable real crypto (vs placeholder mode)

	// Protocol settings
	ProtocolID       protocol.ID
	HandshakeTimeout time.Duration
	MessageTimeout   time.Duration
	SessionTimeout   time.Duration

	// Performance settings
	MaxConcurrentStreams int
	MaxSessions          int
	CleanupInterval      time.Duration

	// Retry settings (for future SWIM membership integration)
	RetryEnabled        bool          // Enable authentication retry
	InitialRetryDelay   time.Duration // Initial delay before first retry
	MaxRetryDelay       time.Duration // Maximum delay between retries
	RetryMultiplier     float64       // Exponential backoff multiplier
	MaxRetryAttempts    int           // Maximum retry attempts before giving up
	RetryJitter         bool          // Add jitter to retry delays
}

// DefaultConfig returns a configuration with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		// Certificate configuration
		CABasePath: ".", // Default to current directory

		// Cryptographic settings
		SignatureAlgorithm: "Ed25519",
		CertValidationMode: "strict",
		AllowSelfSigned:    false,
		EnableRealCrypto:   true, // Default to real crypto

		// Protocol settings
		ProtocolID:           "/falak/join/1.0",
		HandshakeTimeout:     30 * time.Second,
		MessageTimeout:       10 * time.Second,
		SessionTimeout:       5 * time.Minute,
		MaxConcurrentStreams: 100,
		MaxSessions:          1000,
		CleanupInterval:      30 * time.Second,

		// Retry settings (reasonable defaults for development)
		RetryEnabled:      true,
		InitialRetryDelay: 2 * time.Second,
		MaxRetryDelay:     60 * time.Second,
		RetryMultiplier:   2.0,
		MaxRetryAttempts:  5,
		RetryJitter:       true,
	}
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if c.NodeID == "" {
		return ErrInvalidConfig("NodeID is required")
	}
	if len(c.Clusters) == 0 {
		return ErrInvalidConfig("At least one cluster must be specified")
	}
	if c.Host == nil {
		return ErrInvalidConfig("libp2p Host is required")
	}
	if c.ProtocolID == "" {
		return ErrInvalidConfig("ProtocolID is required")
	}

	// Validate certificate configuration when real crypto is enabled
	if c.EnableRealCrypto {
		// Check if specific paths are provided OR if we have CA base path for auto-discovery
		if c.CertificatePath == "" && c.CABasePath == "" {
			return ErrInvalidConfig("Either CertificatePath or CABasePath is required when EnableRealCrypto is true")
		}
		if c.PrivateKeyPath == "" && c.CABasePath == "" {
			return ErrInvalidConfig("Either PrivateKeyPath or CABasePath is required when EnableRealCrypto is true")
		}
		if c.RootCAPath == "" && c.CABasePath == "" {
			return ErrInvalidConfig("Either RootCAPath or CABasePath is required when EnableRealCrypto is true")
		}
		if c.SignatureAlgorithm != "Ed25519" {
			return ErrInvalidConfig("Only Ed25519 signature algorithm is supported")
		}
		if c.CertValidationMode != "strict" && c.CertValidationMode != "relaxed" {
			return ErrInvalidConfig("CertValidationMode must be 'strict' or 'relaxed'")
		}
	}

	return nil
}

// GetPrimaryCluster returns the first cluster (primary cluster)
func (c *Config) GetPrimaryCluster() string {
	if len(c.Clusters) == 0 {
		return "default"
	}
	return c.Clusters[0]
}

// GetPrimaryDataCenter returns the first datacenter (primary datacenter)
func (c *Config) GetPrimaryDataCenter() string {
	if len(c.DataCenters) == 0 {
		return "default"
	}
	return c.DataCenters[0]
}

// HasCluster checks if the given cluster is in the configuration
func (c *Config) HasCluster(clusterID string) bool {
	for _, cluster := range c.Clusters {
		if cluster == clusterID {
			return true
		}
	}
	return false
}