package node

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// BootManager orchestrates the complete node boot sequence
type BootManager struct {
	node          *Node
	cache         *PhonebookCache
	seedLoader    *SeedLoader
	connManager   *ConnectionManager
	orchestrator  *ConnectionOrchestrator
	bootStage     BootStage
	startTime     time.Time
	config        *BootConfig
	mu            sync.RWMutex
	bootID        string
}

// BootStage represents the current stage of the boot process
type BootStage int

const (
	BootStageInit BootStage = iota
	BootStagePhonebookLoad
	BootStageConnectionStrategy
	BootStageConnection
	BootStageAuthentication
	BootStageClusterJoin
	BootStageReady
	BootStageFailed
)

func (bs BootStage) String() string {
	switch bs {
	case BootStageInit:
		return "initialization"
	case BootStagePhonebookLoad:
		return "phonebook_loading"
	case BootStageConnectionStrategy:
		return "connection_strategy"
	case BootStageConnection:
		return "connecting"
	case BootStageAuthentication:
		return "authentication"
	case BootStageClusterJoin:
		return "cluster_join"
	case BootStageReady:
		return "ready"
	case BootStageFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// BootConfig configures the boot process behavior
type BootConfig struct {
	CacheDir            string        `json:"cache_dir"`
	SeedPaths           []string      `json:"seed_paths"`
	DataCenter          string        `json:"data_center"`
	ConnectionStrategy  string        `json:"connection_strategy"`
	BootTimeout         time.Duration `json:"boot_timeout"`
	PhonebookTimeout    time.Duration `json:"phonebook_timeout"`
	ConnectionTimeout   time.Duration `json:"connection_timeout"`
	AuthTimeout         time.Duration `json:"auth_timeout"`
	RetryOnFailure      bool          `json:"retry_on_failure"`
	MaxBootRetries      int           `json:"max_boot_retries"`
	FallbackToSeedOnly  bool          `json:"fallback_to_seed_only"`
	PreferCachedPeers   bool          `json:"prefer_cached_peers"`
}

// DefaultBootConfig returns sensible boot configuration defaults
func DefaultBootConfig() *BootConfig {
	return &BootConfig{
		CacheDir:           DefaultCachePath(),
		SeedPaths:          []string{},
		DataCenter:         "default",
		ConnectionStrategy: "balanced",
		BootTimeout:        60 * time.Second,
		PhonebookTimeout:   10 * time.Second,
		ConnectionTimeout:  30 * time.Second,
		AuthTimeout:        15 * time.Second,
		RetryOnFailure:     true,
		MaxBootRetries:     3,
		FallbackToSeedOnly: true,
		PreferCachedPeers:  true,
	}
}

// NewBootManager creates a new boot manager
func NewBootManager(node *Node, config *BootConfig) *BootManager {
	if config == nil {
		config = DefaultBootConfig()
	}

	// Ensure cache directory is absolute
	if config.CacheDir == "" {
		config.CacheDir = DefaultCachePath()
	}

	bm := &BootManager{
		node:      node,
		config:    config,
		bootStage: BootStageInit,
		startTime: time.Now(),
		bootID:    fmt.Sprintf("boot-%d", time.Now().UnixNano()),
	}

	// Initialize components
	bm.cache = NewPhonebookCache(config.CacheDir)
	bm.seedLoader = NewSeedLoader(config.SeedPaths)

	return bm
}

// ExecuteBootSequence runs the complete boot sequence
func (bm *BootManager) ExecuteBootSequence() error {
	log.Printf("[%s] Starting node boot sequence", bm.bootID)
	
	ctx, cancel := context.WithTimeout(context.Background(), bm.config.BootTimeout)
	defer cancel()

	// Track boot progress
	bm.setBootStage(BootStageInit)

	retryCount := 0
	for retryCount <= bm.config.MaxBootRetries {
		if retryCount > 0 {
			log.Printf("[%s] Boot attempt %d/%d", bm.bootID, retryCount+1, bm.config.MaxBootRetries+1)
		}

		err := bm.executeBootAttempt(ctx)
		if err == nil {
			bm.setBootStage(BootStageReady)
			duration := time.Since(bm.startTime)
			log.Printf("[%s] ✓ Boot sequence completed successfully in %v", bm.bootID, duration)
			return nil
		}

		log.Printf("[%s] Boot attempt failed: %v", bm.bootID, err)
		
		if !bm.config.RetryOnFailure || retryCount >= bm.config.MaxBootRetries {
			bm.setBootStage(BootStageFailed)
			return fmt.Errorf("boot sequence failed after %d attempts: %w", retryCount+1, err)
		}

		retryCount++
		
		// Exponential backoff for retries
		backoffDuration := time.Duration(retryCount) * 2 * time.Second
		log.Printf("[%s] Retrying boot in %v...", bm.bootID, backoffDuration)
		
		select {
		case <-time.After(backoffDuration):
			// Continue to retry
		case <-ctx.Done():
			bm.setBootStage(BootStageFailed)
			return fmt.Errorf("boot sequence timed out during retry backoff")
		}
	}

	bm.setBootStage(BootStageFailed)
	return fmt.Errorf("boot sequence failed after all retry attempts")
}

// executeBootAttempt runs a single boot attempt
func (bm *BootManager) executeBootAttempt(ctx context.Context) error {
	// Step 1: Load phonebook
	if err := bm.loadPhonebook(ctx); err != nil {
		return fmt.Errorf("phonebook loading failed: %w", err)
	}

	// Step 2: Initialize connection strategy
	if err := bm.initializeConnectionStrategy(); err != nil {
		return fmt.Errorf("connection strategy initialization failed: %w", err)
	}

	// Step 3: Attempt connections
	if err := bm.attemptConnections(ctx); err != nil {
		return fmt.Errorf("connection attempts failed: %w", err)
	}

	// Step 4: Perform authentication (handled by connection manager)
	if err := bm.waitForAuthentication(ctx); err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	// Step 5: Join cluster topics
	if err := bm.joinClusterTopics(ctx); err != nil {
		return fmt.Errorf("cluster join failed: %w", err)
	}

	return nil
}

// loadPhonebook loads phonebook data from cache and seeds
func (bm *BootManager) loadPhonebook(ctx context.Context) error {
	bm.setBootStage(BootStagePhonebookLoad)
	log.Printf("[%s] Loading phonebook data", bm.bootID)

	loadCtx, cancel := context.WithTimeout(ctx, bm.config.PhonebookTimeout)
	defer cancel()

	// Initialize with empty phonebook
	phonebook := NewPhonebook()
	
	// Load from cache if preferred and available
	if bm.config.PreferCachedPeers && bm.cache.Exists() {
		log.Printf("[%s] Loading cached phonebook", bm.bootID)
		
		cachedPhonebook, err := bm.cache.Load()
		if err != nil {
			log.Printf("[%s] Warning: failed to load cached phonebook: %v", bm.bootID, err)
		} else {
			// Merge cached data
			cachedPeers := cachedPhonebook.GetPeers()
			for _, peer := range cachedPeers {
				phonebook.AddPeer(peer)
			}
			log.Printf("[%s] Loaded %d peers from cache", bm.bootID, len(cachedPeers))
		}
	}

	// Load seed data
	if len(bm.config.SeedPaths) > 0 {
		log.Printf("[%s] Loading seed data from %d files", bm.bootID, len(bm.config.SeedPaths))
		
		seedPhonebook, err := bm.seedLoader.LoadSeedData()
		if err != nil {
			log.Printf("[%s] Warning: failed to load seed data: %v", bm.bootID, err)
		} else {
			// Merge seed data
			seedPeers := seedPhonebook.GetPeers()
			for _, peer := range seedPeers {
				phonebook.AddPeer(peer)
			}
			log.Printf("[%s] Loaded %d peers from seeds", bm.bootID, len(seedPeers))
		}
	}

	// Update node phonebook
	allPeers := phonebook.GetPeers()
	if len(allPeers) == 0 {
		if bm.config.FallbackToSeedOnly {
			// Try to load high-priority seeds only
			log.Printf("[%s] No peers found, attempting high-priority seed fallback", bm.bootID)
			fallbackPhonebook, err := bm.seedLoader.GetHighPrioritySeeds(90)
			if err == nil {
				fallbackPeers := fallbackPhonebook.GetPeers()
				for _, peer := range fallbackPeers {
					phonebook.AddPeer(peer)
				}
				log.Printf("[%s] Loaded %d high-priority seed peers", bm.bootID, len(fallbackPeers))
			}
		}
		
		if len(phonebook.GetPeers()) == 0 {
			return fmt.Errorf("no peers available in phonebook after loading cache and seeds")
		}
	}

	// Replace node's phonebook
	bm.node.phonebook = phonebook
	
	datacenters := phonebook.GetDataCenters()
	log.Printf("[%s] Phonebook loaded: %d peers across %d data centers: %v", 
		bm.bootID, len(allPeers), len(datacenters), datacenters)

	// Check context cancellation
	select {
	case <-loadCtx.Done():
		return fmt.Errorf("phonebook loading timed out")
	default:
		return nil
	}
}

// initializeConnectionStrategy sets up the connection management components
func (bm *BootManager) initializeConnectionStrategy() error {
	bm.setBootStage(BootStageConnectionStrategy)
	log.Printf("[%s] Initializing connection strategy: %s", bm.bootID, bm.config.ConnectionStrategy)

	// Create connection manager
	connConfig := DefaultConnectionConfig()
	connConfig.ConnectionTimeout = bm.config.ConnectionTimeout
	
	// Adjust strategy based on configuration
	switch bm.config.ConnectionStrategy {
	case "aggressive":
		connConfig.MaxConcurrentPerDC = 8
		connConfig.MinSuccessfulDCs = 1
		connConfig.InitialBackoff = 200 * time.Millisecond
	case "conservative":
		connConfig.MaxConcurrentPerDC = 2
		connConfig.MinSuccessfulDCs = 2
		connConfig.InitialBackoff = 1 * time.Second
	case "balanced":
		// Use defaults
	default:
		log.Printf("[%s] Warning: unknown connection strategy '%s', using balanced", 
			bm.bootID, bm.config.ConnectionStrategy)
	}

	bm.connManager = NewConnectionManager(bm.node, connConfig)

	// Create orchestrator
	orchConfig := DefaultOrchestrationConfig()
	orchConfig.MaxWaitTime = bm.config.ConnectionTimeout
	orchConfig.MinSuccessfulDCs = connConfig.MinSuccessfulDCs
	
	bm.orchestrator = NewConnectionOrchestrator(bm.connManager, orchConfig)

	log.Printf("[%s] Connection strategy initialized", bm.bootID)
	return nil
}

// attemptConnections runs the parallel connection process
func (bm *BootManager) attemptConnections(ctx context.Context) error {
	bm.setBootStage(BootStageConnection)
	log.Printf("[%s] Attempting connections to cluster", bm.bootID)

	connCtx, cancel := context.WithTimeout(ctx, bm.config.ConnectionTimeout)
	defer cancel()

	// Run orchestrated connections
	if err := bm.orchestrator.AttemptParallelConnections(); err != nil {
		return fmt.Errorf("parallel connection orchestration failed: %w", err)
	}

	// Wait for minimum connections
	if err := bm.orchestrator.WaitForMinimumConnections(); err != nil {
		return fmt.Errorf("minimum connection requirements not met: %w", err)
	}

	// Check context cancellation
	select {
	case <-connCtx.Done():
		return fmt.Errorf("connection attempts timed out")
	default:
		log.Printf("[%s] Connections established successfully", bm.bootID)
		return nil
	}
}

// waitForAuthentication waits for authentication to complete
func (bm *BootManager) waitForAuthentication(ctx context.Context) error {
	bm.setBootStage(BootStageAuthentication)
	log.Printf("[%s] Waiting for authentication completion", bm.bootID)

	authCtx, cancel := context.WithTimeout(ctx, bm.config.AuthTimeout)
	defer cancel()

	// Authentication is handled automatically by the connection manager
	// We just need to verify that we have authenticated connections
	
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-authCtx.Done():
			return fmt.Errorf("authentication timed out")
		case <-ticker.C:
			// Check if we have healthy authenticated connections
			status := bm.orchestrator.GetOrchestrationStatus()
			if status.IsHealthy() {
				log.Printf("[%s] Authentication completed successfully", bm.bootID)
				return nil
			}
		}
	}
}

// joinClusterTopics joins the necessary cluster pub/sub topics
func (bm *BootManager) joinClusterTopics(ctx context.Context) error {
	bm.setBootStage(BootStageClusterJoin)
	log.Printf("[%s] Joining cluster topics", bm.bootID)

	// Join default cluster topics (this is already handled by existing code)
	// The node automatically joins cluster topics during initialization
	// We just need to verify the topics are connected

	if bm.node.pubSub == nil {
		return fmt.Errorf("pubsub not initialized")
	}

	// Verify connection to topics
	for _, cluster := range bm.node.clusters {
		topicName := fmt.Sprintf("falak/%s/members/membership", cluster)
		if _, exists := bm.node.connectedTopics[topicName]; !exists {
			log.Printf("[%s] Warning: not connected to cluster topic: %s", bm.bootID, topicName)
		}
	}

	log.Printf("[%s] Cluster join completed", bm.bootID)
	return nil
}

// setBootStage atomically updates the boot stage
func (bm *BootManager) setBootStage(stage BootStage) {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	
	oldStage := bm.bootStage
	bm.bootStage = stage
	
	log.Printf("[%s] Boot stage: %s → %s", bm.bootID, oldStage.String(), stage.String())
}

// GetBootStage returns the current boot stage
func (bm *BootManager) GetBootStage() BootStage {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	return bm.bootStage
}

// GetBootStatus returns detailed boot status information
func (bm *BootManager) GetBootStatus() BootStatus {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	status := BootStatus{
		BootID:      bm.bootID,
		Stage:       bm.bootStage,
		StartTime:   bm.startTime,
		Duration:    time.Since(bm.startTime),
		DataCenter:  bm.config.DataCenter,
		Strategy:    bm.config.ConnectionStrategy,
	}

	if bm.orchestrator != nil {
		orchStatus := bm.orchestrator.GetOrchestrationStatus()
		status.OrchestrationStatus = &orchStatus
	}

	return status
}

// BootStatus provides comprehensive information about the boot process
type BootStatus struct {
	BootID               string                   `json:"boot_id"`
	Stage                BootStage                `json:"stage"`
	StartTime            time.Time                `json:"start_time"`
	Duration             time.Duration            `json:"duration"`
	DataCenter           string                   `json:"data_center"`
	Strategy             string                   `json:"strategy"`
	OrchestrationStatus  *OrchestrationStatus     `json:"orchestration_status,omitempty"`
}

// IsComplete returns true if the boot process is complete (success or failure)
func (bs BootStatus) IsComplete() bool {
	return bs.Stage == BootStageReady || bs.Stage == BootStageFailed
}

// IsSuccessful returns true if the boot process completed successfully
func (bs BootStatus) IsSuccessful() bool {
	return bs.Stage == BootStageReady
}

// GetSummary returns a human-readable summary of the boot status
func (bs BootStatus) GetSummary() string {
	if bs.IsSuccessful() {
		return fmt.Sprintf("Boot %s: ready (took %v)", bs.BootID, bs.Duration)
	} else if bs.Stage == BootStageFailed {
		return fmt.Sprintf("Boot %s: failed (took %v)", bs.BootID, bs.Duration)
	} else {
		return fmt.Sprintf("Boot %s: %s (running %v)", bs.BootID, bs.Stage.String(), bs.Duration)
	}
}

// SavePhonebookCache saves the current phonebook to cache
func (bm *BootManager) SavePhonebookCache() error {
	if bm.cache == nil || bm.node.phonebook == nil {
		return fmt.Errorf("cache or phonebook not initialized")
	}

	log.Printf("[%s] Saving phonebook to cache", bm.bootID)
	return bm.cache.Save(bm.node.phonebook)
}

// Close gracefully shuts down the boot manager
func (bm *BootManager) Close() error {
	if bm.connManager != nil {
		return bm.connManager.Close()
	}
	return nil
}