package node

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// ConnectionManager handles data center-aware peer connections
type ConnectionManager struct {
	node         *Node
	phonebook    *Phonebook
	activeConns  map[string]*Connection
	dcStrategies map[string]*DataCenterStrategy
	mu           sync.RWMutex
	ctx          context.Context
	cancel       context.CancelFunc
	config       *ConnectionConfig
}

// Connection represents an active peer connection
type Connection struct {
	PeerID       peer.ID
	DataCenter   string
	ConnectedAt  time.Time
	LastPing     time.Time
	Status       ConnectionStatus
	FailureCount int
	mu           sync.RWMutex
}

// ConnectionStatus represents the state of a connection
type ConnectionStatus int

const (
	ConnectionConnecting ConnectionStatus = iota
	ConnectionActive
	ConnectionDegraded
	ConnectionFailed
	ConnectionClosed
)

func (cs ConnectionStatus) String() string {
	switch cs {
	case ConnectionConnecting:
		return "connecting"
	case ConnectionActive:
		return "active"
	case ConnectionDegraded:
		return "degraded"
	case ConnectionFailed:
		return "failed"
	case ConnectionClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// DataCenterStrategy manages connections within a data center
type DataCenterStrategy struct {
	dcID            string
	peers           []Peer
	maxConcurrent   int
	backoffStrategy backoff.BackOff
	healthStatus    DCHealthStatus
	lastAttempt     time.Time
	successCount    int
	failureCount    int
	mu              sync.RWMutex
}

// DCHealthStatus represents the health of a data center
type DCHealthStatus int

const (
	DCHealthy DCHealthStatus = iota
	DCDegraded
	DCUnreachable
	DCRecovering
)

func (dcs DCHealthStatus) String() string {
	switch dcs {
	case DCHealthy:
		return "healthy"
	case DCDegraded:
		return "degraded"
	case DCUnreachable:
		return "unreachable"
	case DCRecovering:
		return "recovering"
	default:
		return "unknown"
	}
}

// ConnectionConfig configures connection behavior
type ConnectionConfig struct {
	MaxConcurrentPerDC  int           `json:"max_concurrent_per_dc"`
	MinSuccessfulDCs    int           `json:"min_successful_dcs"`
	ConnectionTimeout   time.Duration `json:"connection_timeout"`
	RetryStrategy       string        `json:"retry_strategy"`
	HealthCheckInterval time.Duration `json:"health_check_interval"`
	BackoffMultiplier   float64       `json:"backoff_multiplier"`
	MaxBackoffInterval  time.Duration `json:"max_backoff_interval"`
	InitialBackoff      time.Duration `json:"initial_backoff"`
}

// DefaultConnectionConfig returns sensible defaults
func DefaultConnectionConfig() *ConnectionConfig {
	return &ConnectionConfig{
		MaxConcurrentPerDC:  5,
		MinSuccessfulDCs:    1,
		ConnectionTimeout:   10 * time.Second,
		RetryStrategy:       "exponential",
		HealthCheckInterval: 30 * time.Second,
		BackoffMultiplier:   2.0,
		MaxBackoffInterval:  5 * time.Minute,
		InitialBackoff:      500 * time.Millisecond,
	}
}

// NewConnectionManager creates a new connection manager
func NewConnectionManager(node *Node, config *ConnectionConfig) *ConnectionManager {
	if config == nil {
		config = DefaultConnectionConfig()
	}

	ctx, cancel := context.WithCancel(node.ctx)

	cm := &ConnectionManager{
		node:         node,
		phonebook:    node.phonebook,
		activeConns:  make(map[string]*Connection),
		dcStrategies: make(map[string]*DataCenterStrategy),
		ctx:          ctx,
		cancel:       cancel,
		config:       config,
	}

	// Start health monitoring
	go cm.healthMonitorLoop()

	return cm
}

// ConnectToCluster attempts to connect to peers across multiple data centers
func (cm *ConnectionManager) ConnectToCluster() error {
	log.Printf("Starting cluster connection process")

	// Group peers by data center
	peersByDC := cm.groupPeersByDataCenter()
	if len(peersByDC) == 0 {
		return fmt.Errorf("no peers found in phonebook")
	}

	log.Printf("Found peers in %d data centers", len(peersByDC))

	// Initialize data center strategies
	cm.initializeDataCenterStrategies(peersByDC)

	// Attempt connections in parallel
	resultChan := make(chan DCConnectionResult, len(peersByDC))

	for dcID, peers := range peersByDC {
		go cm.connectToDataCenter(dcID, peers, resultChan)
	}

	// Collect results
	// var results []DCConnectionResult
	successfulDCs := 0

	for i := 0; i < len(peersByDC); i++ {
		select {
		case result := <-resultChan:
			if result.Success {
				successfulDCs++
			}
			log.Printf("DC %s connection result: success=%v, peers=%d, latency=%v",
				result.DataCenter, result.Success, result.PeersConnected, result.Latency)
		case <-time.After(cm.config.ConnectionTimeout * 2):
			log.Printf("Timeout waiting for connection results")
			return fmt.Errorf("timeout waiting for connection results")
		}
	}

	// Check if we met minimum requirements
	if successfulDCs < cm.config.MinSuccessfulDCs {
		return fmt.Errorf("failed to connect to minimum data centers: got %d, need %d",
			successfulDCs, cm.config.MinSuccessfulDCs)
	}

	log.Printf("Successfully connected to %d/%d data centers", successfulDCs, len(peersByDC))
	return nil
}

// groupPeersByDataCenter organizes peers by their data center
func (cm *ConnectionManager) groupPeersByDataCenter() map[string][]Peer {
	peers := cm.phonebook.GetPeers()
	peersByDC := make(map[string][]Peer)

	for _, peer := range peers {
		dc := peer.DataCenter
		if dc == "" {
			dc = "default"
		}

		peersByDC[dc] = append(peersByDC[dc], peer)
	}

	// Shuffle peers within each data center for load distribution
	for dc, peers := range peersByDC {
		rand.Shuffle(len(peers), func(i, j int) {
			peers[i], peers[j] = peers[j], peers[i]
		})
		peersByDC[dc] = peers
	}

	return peersByDC
}

// initializeDataCenterStrategies sets up connection strategies for each DC
func (cm *ConnectionManager) initializeDataCenterStrategies(peersByDC map[string][]Peer) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	for dcID, peers := range peersByDC {
		if _, exists := cm.dcStrategies[dcID]; !exists {
			strategy := &DataCenterStrategy{
				dcID:          dcID,
				peers:         peers,
				maxConcurrent: cm.config.MaxConcurrentPerDC,
				healthStatus:  DCHealthy,
				lastAttempt:   time.Time{},
			}

			// Create backoff strategy
			strategy.backoffStrategy = backoff.NewExponentialBackOff()
			strategy.backoffStrategy.(*backoff.ExponentialBackOff).InitialInterval = cm.config.InitialBackoff
			strategy.backoffStrategy.(*backoff.ExponentialBackOff).RandomizationFactor = 0.2
			strategy.backoffStrategy.(*backoff.ExponentialBackOff).Multiplier = cm.config.BackoffMultiplier
			strategy.backoffStrategy.(*backoff.ExponentialBackOff).MaxInterval = cm.config.MaxBackoffInterval
			strategy.backoffStrategy.Reset()

			cm.dcStrategies[dcID] = strategy
		} else {
			// Update peer list
			cm.dcStrategies[dcID].peers = peers
		}
	}
}

// connectToDataCenter attempts to connect to peers in a specific data center
func (cm *ConnectionManager) connectToDataCenter(dcID string, peers []Peer, resultChan chan<- DCConnectionResult) {
	startTime := time.Now()

	cm.mu.RLock()
	strategy := cm.dcStrategies[dcID]
	cm.mu.RUnlock()

	if strategy == nil {
		resultChan <- DCConnectionResult{
			DataCenter:     dcID,
			Success:        false,
			Error:          fmt.Errorf("no strategy found for DC %s", dcID),
			PeersConnected: 0,
			Latency:        time.Since(startTime),
		}
		return
	}

	strategy.mu.Lock()
	strategy.lastAttempt = time.Now()
	strategy.mu.Unlock()

	connected := 0
	maxAttempts := min(len(peers), strategy.maxConcurrent)

	for i := 0; i < maxAttempts && i < len(peers); i++ {
		peer := peers[i]

		if cm.isPeerConnected(peer.ID) {
			connected++
			continue
		}

		if err := cm.connectToPeer(peer, dcID, strategy); err != nil {
			log.Printf("Failed to connect to peer %s in DC %s: %v", peer.ID, dcID, err)
			strategy.mu.Lock()
			strategy.failureCount++
			strategy.mu.Unlock()
			continue
		}

		connected++
		strategy.mu.Lock()
		strategy.successCount++
		strategy.mu.Unlock()

		// Update DC health status
		cm.updateDataCenterHealth(dcID, connected > 0)
	}

	success := connected > 0
	result := DCConnectionResult{
		DataCenter:     dcID,
		Success:        success,
		PeersConnected: connected,
		Latency:        time.Since(startTime),
	}

	if !success {
		result.Error = fmt.Errorf("failed to connect to any peers in DC %s", dcID)
	}

	resultChan <- result
}

// connectToPeer attempts to connect to a single peer with backoff
func (cm *ConnectionManager) connectToPeer(peerStruct Peer, dcID string, strategy *DataCenterStrategy) error {
	peerID, err := peer.Decode(peerStruct.ID)
	if err != nil {
		return fmt.Errorf("invalid peer ID: %w", err)
	}

	// Create connection entry
	conn := &Connection{
		PeerID:      peerID,
		DataCenter:  dcID,
		ConnectedAt: time.Now(),
		Status:      ConnectionConnecting,
	}

	cm.mu.Lock()
	cm.activeConns[peerStruct.ID] = conn
	cm.mu.Unlock()

	// Convert peer to PeerInput format
	peerInput := PeerInput{
		ID:   peerStruct.ID,
		Addr: peerStruct.AddrInfo.Addrs[0].String(), // Use first address
		Tags: peerStruct.Tags,
	}

	// Attempt connection with backoff
	operation := func() (string, error) {
		err := cm.node.ConnectPeer(peerInput)
		return "", err
	}

	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = strategy.backoffStrategy.NextBackOff()
	bo.MaxInterval = cm.config.MaxBackoffInterval

	ctx, cancel := context.WithTimeout(cm.ctx, cm.config.ConnectionTimeout)
	defer cancel()

	// Use retry with context and backoff
	_, err = backoff.Retry(ctx, operation, backoff.WithBackOff(bo), backoff.WithMaxTries(5))

	conn.mu.Lock()
	if err != nil {
		conn.Status = ConnectionFailed
		conn.FailureCount++
	} else {
		conn.Status = ConnectionActive
		conn.LastPing = time.Now()
	}
	conn.mu.Unlock()

	return err
}

// isPeerConnected checks if we're already connected to a peer
func (cm *ConnectionManager) isPeerConnected(peerID string) bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	conn, exists := cm.activeConns[peerID]
	if !exists {
		return false
	}

	conn.mu.RLock()
	defer conn.mu.RUnlock()

	return conn.Status == ConnectionActive || conn.Status == ConnectionDegraded
}

// updateDataCenterHealth updates the health status of a data center
func (cm *ConnectionManager) updateDataCenterHealth(dcID string, hasConnections bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	strategy := cm.dcStrategies[dcID]
	if strategy == nil {
		return
	}

	strategy.mu.Lock()
	defer strategy.mu.Unlock()

	if hasConnections {
		if strategy.healthStatus == DCUnreachable {
			strategy.healthStatus = DCRecovering
		} else {
			strategy.healthStatus = DCHealthy
		}
	} else {
		if strategy.failureCount > strategy.successCount*2 {
			strategy.healthStatus = DCDegraded
		}
		if strategy.failureCount > 10 && strategy.successCount == 0 {
			strategy.healthStatus = DCUnreachable
		}
	}
}

// GetHealthyDataCenters returns a list of healthy data centers
func (cm *ConnectionManager) GetHealthyDataCenters() []string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	var healthy []string
	for dcID, strategy := range cm.dcStrategies {
		strategy.mu.RLock()
		if strategy.healthStatus == DCHealthy || strategy.healthStatus == DCRecovering {
			healthy = append(healthy, dcID)
		}
		strategy.mu.RUnlock()
	}

	return healthy
}

// GetConnectionStats returns connection statistics
func (cm *ConnectionManager) GetConnectionStats() map[string]ConnectionStats {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	stats := make(map[string]ConnectionStats)

	for dcID, strategy := range cm.dcStrategies {
		strategy.mu.RLock()
		stat := ConnectionStats{
			DataCenter:   dcID,
			ActiveConns:  cm.getActiveConnectionsInDC(dcID),
			TotalPeers:   len(strategy.peers),
			HealthStatus: strategy.healthStatus.String(),
			SuccessCount: strategy.successCount,
			FailureCount: strategy.failureCount,
			LastAttempt:  strategy.lastAttempt,
		}
		strategy.mu.RUnlock()

		stats[dcID] = stat
	}

	return stats
}

// ConnectionStats provides statistics about connections in a data center
type ConnectionStats struct {
	DataCenter   string
	ActiveConns  int
	TotalPeers   int
	HealthStatus string
	SuccessCount int
	FailureCount int
	LastAttempt  time.Time
}

// getActiveConnectionsInDC counts active connections in a data center
func (cm *ConnectionManager) getActiveConnectionsInDC(dcID string) int {
	count := 0
	for _, conn := range cm.activeConns {
		conn.mu.RLock()
		if conn.DataCenter == dcID &&
			(conn.Status == ConnectionActive || conn.Status == ConnectionDegraded) {
			count++
		}
		conn.mu.RUnlock()
	}
	return count
}

// healthMonitorLoop periodically checks connection health
func (cm *ConnectionManager) healthMonitorLoop() {
	ticker := time.NewTicker(cm.config.HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cm.performHealthCheck()
		case <-cm.ctx.Done():
			return
		}
	}
}

// performHealthCheck checks the health of all connections
func (cm *ConnectionManager) performHealthCheck() {
	cm.mu.RLock()
	connsCopy := make([]*Connection, 0, len(cm.activeConns))
	for _, conn := range cm.activeConns {
		connsCopy = append(connsCopy, conn)
	}
	cm.mu.RUnlock()

	for _, conn := range connsCopy {
		cm.checkConnectionHealth(conn)
	}
}

// checkConnectionHealth checks the health of a single connection
func (cm *ConnectionManager) checkConnectionHealth(conn *Connection) {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	// Check if connection is still active in libp2p
	conns := cm.node.host.Network().ConnsToPeer(conn.PeerID)
	if len(conns) == 0 {
		conn.Status = ConnectionClosed
		return
	}

	// Check if any connection is open
	hasOpenConn := false
	for _, c := range conns {
		if c.Stat().Direction == network.DirOutbound {
			hasOpenConn = true
			break
		}
	}

	if !hasOpenConn {
		conn.Status = ConnectionFailed
		conn.FailureCount++
	} else {
		conn.Status = ConnectionActive
		conn.LastPing = time.Now()
	}
}

// Close shuts down the connection manager
func (cm *ConnectionManager) Close() error {
	cm.cancel()

	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Close all connections
	for peerID, conn := range cm.activeConns {
		conn.mu.Lock()
		conn.Status = ConnectionClosed
		conn.mu.Unlock()

		// Close libp2p connection
		conns := cm.node.host.Network().ConnsToPeer(conn.PeerID)
		for _, c := range conns {
			c.Close()
		}

		delete(cm.activeConns, peerID)
	}

	log.Printf("Connection manager closed")
	return nil
}

// DCConnectionResult represents the result of connecting to a data center
type DCConnectionResult struct {
	DataCenter     string
	Success        bool
	Error          error
	PeersConnected int
	Latency        time.Duration
}

// min helper function
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
