package node

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// ConnectionHealthMonitor monitors connection health and handles recovery
type ConnectionHealthMonitor struct {
	connManager     *ConnectionManager
	optimizer       *ConnectionOptimizer
	healthChecks    map[string]*HealthCheck
	recoveryTasks   chan RecoveryTask
	mu              sync.RWMutex
	ctx             context.Context
	cancel          context.CancelFunc
	config          *HealthConfig
	isRunning       bool
	monitorID       string
}

// HealthCheck tracks the health of a specific peer connection
type HealthCheck struct {
	PeerID           string
	DataCenter       string
	LastSeen         time.Time
	LastHealthCheck  time.Time
	ConsecutiveFails int
	TotalChecks      int
	SuccessRate      float64
	Status           PeerHealthStatus
	LastError        error
	RecoveryAttempts int
	mu               sync.RWMutex
}

// PeerHealthStatus represents the health status of a peer
type PeerHealthStatus int

const (
	PeerHealthy PeerHealthStatus = iota
	PeerDegraded
	PeerSuspected
	PeerFailed
	PeerRecovering
	PeerQuarantined
)

func (phs PeerHealthStatus) String() string {
	switch phs {
	case PeerHealthy:
		return "healthy"
	case PeerDegraded:
		return "degraded"
	case PeerSuspected:
		return "suspected"
	case PeerFailed:
		return "failed"
	case PeerRecovering:
		return "recovering"
	case PeerQuarantined:
		return "quarantined"
	default:
		return "unknown"
	}
}

// RecoveryTask represents a recovery action to be performed
type RecoveryTask struct {
	Type         RecoveryType
	DataCenter   string
	PeerID       string
	Priority     int
	Reason       string
	Attempts     int
	MaxAttempts  int
	NextAttempt  time.Time
	CreatedAt    time.Time
}

// RecoveryType specifies the type of recovery action
type RecoveryType int

const (
	RecoveryReconnect RecoveryType = iota
	RecoveryDataCenterFailover
	RecoveryPeerReplacement
	RecoveryNetworkReset
	RecoveryFullReboot
)

func (rt RecoveryType) String() string {
	switch rt {
	case RecoveryReconnect:
		return "reconnect"
	case RecoveryDataCenterFailover:
		return "dc_failover"
	case RecoveryPeerReplacement:
		return "peer_replacement"
	case RecoveryNetworkReset:
		return "network_reset"
	case RecoveryFullReboot:
		return "full_reboot"
	default:
		return "unknown"
	}
}

// HealthConfig configures health monitoring behavior
type HealthConfig struct {
	CheckInterval        time.Duration `json:"check_interval"`
	TimeoutThreshold     time.Duration `json:"timeout_threshold"`
	FailureThreshold     int           `json:"failure_threshold"`
	RecoveryThreshold    int           `json:"recovery_threshold"`
	QuarantineDuration   time.Duration `json:"quarantine_duration"`
	MaxRecoveryAttempts  int           `json:"max_recovery_attempts"`
	HealthCheckTimeout   time.Duration `json:"health_check_timeout"`
	MinSuccessRate       float64       `json:"min_success_rate"`
	RecoveryBackoffBase  time.Duration `json:"recovery_backoff_base"`
	EnableAutoRecovery   bool          `json:"enable_auto_recovery"`
	PingEnabled          bool          `json:"ping_enabled"`
	DetailedLogging      bool          `json:"detailed_logging"`
}

// DefaultHealthConfig returns sensible health monitoring defaults
func DefaultHealthConfig() *HealthConfig {
	return &HealthConfig{
		CheckInterval:       30 * time.Second,
		TimeoutThreshold:    5 * time.Second,
		FailureThreshold:    3,
		RecoveryThreshold:   2,
		QuarantineDuration:  5 * time.Minute,
		MaxRecoveryAttempts: 5,
		HealthCheckTimeout:  10 * time.Second,
		MinSuccessRate:      0.7,
		RecoveryBackoffBase: 2 * time.Second,
		EnableAutoRecovery:  true,
		PingEnabled:         true,
		DetailedLogging:     false,
	}
}

// NewConnectionHealthMonitor creates a new connection health monitor
func NewConnectionHealthMonitor(cm *ConnectionManager, optimizer *ConnectionOptimizer, config *HealthConfig) *ConnectionHealthMonitor {
	if config == nil {
		config = DefaultHealthConfig()
	}

	ctx, cancel := context.WithCancel(context.Background())

	chm := &ConnectionHealthMonitor{
		connManager:  cm,
		optimizer:    optimizer,
		healthChecks: make(map[string]*HealthCheck),
		recoveryTasks: make(chan RecoveryTask, 100),
		ctx:          ctx,
		cancel:       cancel,
		config:       config,
		monitorID:    fmt.Sprintf("monitor-%d", time.Now().UnixNano()),
	}

	return chm
}

// StartMonitoring begins the health monitoring process
func (chm *ConnectionHealthMonitor) StartMonitoring() error {
	chm.mu.Lock()
	defer chm.mu.Unlock()

	if chm.isRunning {
		return fmt.Errorf("health monitoring is already running")
	}

	log.Printf("[%s] Starting connection health monitoring", chm.monitorID)
	
	chm.isRunning = true

	// Start health check loop
	go chm.healthCheckLoop()

	// Start recovery task processor
	go chm.recoveryTaskProcessor()

	// Start periodic cleanup
	go chm.cleanupLoop()

	log.Printf("[%s] Health monitoring started", chm.monitorID)
	return nil
}

// healthCheckLoop periodically checks the health of all connections
func (chm *ConnectionHealthMonitor) healthCheckLoop() {
	ticker := time.NewTicker(chm.config.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			chm.performHealthChecks()
		case <-chm.ctx.Done():
			return
		}
	}
}

// performHealthChecks checks the health of all monitored connections
func (chm *ConnectionHealthMonitor) performHealthChecks() {
	if chm.config.DetailedLogging {
		log.Printf("[%s] Performing health checks", chm.monitorID)
	}

	// Get current connection stats
	stats := chm.connManager.GetConnectionStats()
	
	for dcID, dcStats := range stats {
		chm.checkDataCenterHealth(dcID, dcStats)
	}

	// Check individual peer health
	chm.checkIndividualPeerHealth()

	// Trigger recovery if needed
	if chm.config.EnableAutoRecovery {
		chm.triggerRecoveryIfNeeded()
	}
}

// checkDataCenterHealth checks the health of an entire data center
func (chm *ConnectionHealthMonitor) checkDataCenterHealth(dcID string, stats ConnectionStats) {
	healthRatio := 0.0
	if stats.TotalPeers > 0 {
		healthRatio = float64(stats.ActiveConns) / float64(stats.TotalPeers)
	}

	if healthRatio < 0.3 { // Less than 30% healthy connections
		log.Printf("[%s] Data center %s is unhealthy: %d/%d connections", 
			chm.monitorID, dcID, stats.ActiveConns, stats.TotalPeers)
		
		chm.scheduleRecoveryTask(RecoveryTask{
			Type:        RecoveryDataCenterFailover,
			DataCenter:  dcID,
			Priority:    80,
			Reason:      fmt.Sprintf("low health ratio: %.2f", healthRatio),
			MaxAttempts: 3,
			CreatedAt:   time.Now(),
		})
	}
}

// checkIndividualPeerHealth checks the health of individual peer connections
func (chm *ConnectionHealthMonitor) checkIndividualPeerHealth() {
	chm.mu.RLock()
	healthChecksCopy := make(map[string]*HealthCheck)
	for k, v := range chm.healthChecks {
		healthChecksCopy[k] = v
	}
	chm.mu.RUnlock()

	for peerID, healthCheck := range healthChecksCopy {
		chm.performPeerHealthCheck(peerID, healthCheck)
	}
}

// performPeerHealthCheck checks the health of a specific peer
func (chm *ConnectionHealthMonitor) performPeerHealthCheck(peerID string, healthCheck *HealthCheck) {
	healthCheck.mu.Lock()
	defer healthCheck.mu.Unlock()

	healthCheck.LastHealthCheck = time.Now()
	healthCheck.TotalChecks++

	// Perform actual health check (ping or connection test)
	healthy := chm.isPeerHealthy(peerID)

	if healthy {
		healthCheck.ConsecutiveFails = 0
		healthCheck.LastSeen = time.Now()
		
		// Update status based on recovery threshold
		if healthCheck.Status == PeerRecovering {
			if healthCheck.TotalChecks-healthCheck.ConsecutiveFails >= chm.config.RecoveryThreshold {
				healthCheck.Status = PeerHealthy
				if chm.config.DetailedLogging {
					log.Printf("[%s] Peer %s recovered", chm.monitorID, peerID[:12])
				}
			}
		} else if healthCheck.Status == PeerSuspected || healthCheck.Status == PeerDegraded {
			healthCheck.Status = PeerHealthy
		}
	} else {
		healthCheck.ConsecutiveFails++
		
		// Update status based on failure threshold
		if healthCheck.ConsecutiveFails >= chm.config.FailureThreshold {
			if healthCheck.Status != PeerFailed {
				healthCheck.Status = PeerFailed
				log.Printf("[%s] Peer %s marked as failed after %d consecutive failures", 
					chm.monitorID, peerID[:12], healthCheck.ConsecutiveFails)
				
				// Schedule recovery
				chm.scheduleRecoveryTask(RecoveryTask{
					Type:        RecoveryPeerReplacement,
					PeerID:      peerID,
					DataCenter:  healthCheck.DataCenter,
					Priority:    60,
					Reason:      fmt.Sprintf("failed health checks: %d", healthCheck.ConsecutiveFails),
					MaxAttempts: chm.config.MaxRecoveryAttempts,
					CreatedAt:   time.Now(),
				})
			}
		} else if healthCheck.ConsecutiveFails >= chm.config.FailureThreshold/2 {
			healthCheck.Status = PeerSuspected
		}
	}

	// Update success rate
	successCount := healthCheck.TotalChecks - healthCheck.ConsecutiveFails
	healthCheck.SuccessRate = float64(successCount) / float64(healthCheck.TotalChecks)
}

// isPeerHealthy performs the actual health check on a peer
func (chm *ConnectionHealthMonitor) isPeerHealthy(peerID string) bool {
	// Check if peer is still connected in libp2p
	peerIDObj, err := peer.Decode(peerID)
	if err != nil {
		return false
	}

	conns := chm.connManager.node.host.Network().ConnsToPeer(peerIDObj)
	if len(conns) == 0 {
		return false
	}

	// Check if any connection is active
	for _, conn := range conns {
		if conn.Stat().Direction == network.DirOutbound {
			return true
		}
	}

	return false
}

// scheduleRecoveryTask adds a recovery task to the queue
func (chm *ConnectionHealthMonitor) scheduleRecoveryTask(task RecoveryTask) {
	task.NextAttempt = time.Now().Add(time.Duration(task.Attempts) * chm.config.RecoveryBackoffBase)
	
	select {
	case chm.recoveryTasks <- task:
		if chm.config.DetailedLogging {
			log.Printf("[%s] Scheduled recovery task: %s for %s", 
				chm.monitorID, task.Type.String(), task.DataCenter)
		}
	default:
		log.Printf("[%s] Recovery task queue full, dropping task", chm.monitorID)
	}
}

// recoveryTaskProcessor processes recovery tasks
func (chm *ConnectionHealthMonitor) recoveryTaskProcessor() {
	for {
		select {
		case task := <-chm.recoveryTasks:
			chm.processRecoveryTask(task)
		case <-chm.ctx.Done():
			return
		}
	}
}

// processRecoveryTask processes a single recovery task
func (chm *ConnectionHealthMonitor) processRecoveryTask(task RecoveryTask) {
	// Check if it's time to attempt recovery
	if time.Now().Before(task.NextAttempt) {
		// Reschedule for later
		go func() {
			time.Sleep(time.Until(task.NextAttempt))
			chm.scheduleRecoveryTask(task)
		}()
		return
	}

	log.Printf("[%s] Processing recovery task: %s (attempt %d/%d)", 
		chm.monitorID, task.Type.String(), task.Attempts+1, task.MaxAttempts)

	success := false
	
	switch task.Type {
	case RecoveryReconnect:
		success = chm.attemptReconnect(task.PeerID)
	case RecoveryDataCenterFailover:
		success = chm.attemptDataCenterFailover(task.DataCenter)
	case RecoveryPeerReplacement:
		success = chm.attemptPeerReplacement(task.PeerID, task.DataCenter)
	case RecoveryNetworkReset:
		success = chm.attemptNetworkReset()
	case RecoveryFullReboot:
		success = chm.attemptFullReboot()
	}

	task.Attempts++

	if !success && task.Attempts < task.MaxAttempts {
		// Reschedule with exponential backoff
		task.NextAttempt = time.Now().Add(
			time.Duration(task.Attempts*task.Attempts) * chm.config.RecoveryBackoffBase)
		chm.scheduleRecoveryTask(task)
	} else if success {
		log.Printf("[%s] Recovery task completed successfully: %s", 
			chm.monitorID, task.Type.String())
	} else {
		log.Printf("[%s] Recovery task failed after %d attempts: %s", 
			chm.monitorID, task.Attempts, task.Type.String())
	}
}

// attemptReconnect tries to reconnect to a specific peer
func (chm *ConnectionHealthMonitor) attemptReconnect(peerID string) bool {
	// Find peer in phonebook
	peer, exists := chm.connManager.node.phonebook.GetPeer(peerID)
	if !exists {
		return false
	}

	// Convert to PeerInput and attempt connection
	if len(peer.AddrInfo.Addrs) == 0 {
		return false
	}

	peerInput := PeerInput{
		ID:   peer.ID,
		Addr: peer.AddrInfo.Addrs[0].String(),
		Tags: peer.Tags,
	}

	return chm.connManager.node.ConnectPeer(peerInput) == nil
}

// attemptDataCenterFailover attempts to connect to alternative peers in other DCs
func (chm *ConnectionHealthMonitor) attemptDataCenterFailover(failedDC string) bool {
	// Get peers from other data centers
	allDCs := chm.connManager.node.phonebook.GetDataCenters()
	
	for _, dc := range allDCs {
		if dc != failedDC {
			peers := chm.connManager.node.phonebook.GetPeersByDataCenter(dc)
			if len(peers) > 0 {
				// Try to connect to the best peer in this DC
				if chm.optimizer != nil {
					peers = chm.optimizer.RankPeersByPriority(peers)
				}
				
				if chm.attemptReconnect(peers[0].ID) {
					return true
				}
			}
		}
	}
	
	return false
}

// attemptPeerReplacement tries to replace a failed peer with an alternative
func (chm *ConnectionHealthMonitor) attemptPeerReplacement(failedPeerID, dataCenter string) bool {
	// Get alternative peers from the same data center
	dcPeers := chm.connManager.node.phonebook.GetPeersByDataCenter(dataCenter)
	
	for _, peer := range dcPeers {
		if peer.ID != failedPeerID {
			if chm.attemptReconnect(peer.ID) {
				return true
			}
		}
	}
	
	// If no alternatives in same DC, try other DCs
	return chm.attemptDataCenterFailover(dataCenter)
}

// attemptNetworkReset performs a network reset (placeholder)
func (chm *ConnectionHealthMonitor) attemptNetworkReset() bool {
	log.Printf("[%s] Network reset not implemented", chm.monitorID)
	return false
}

// attemptFullReboot performs a full node reboot (placeholder)
func (chm *ConnectionHealthMonitor) attemptFullReboot() bool {
	log.Printf("[%s] Full reboot not implemented", chm.monitorID)
	return false
}

// triggerRecoveryIfNeeded checks if recovery actions are needed
func (chm *ConnectionHealthMonitor) triggerRecoveryIfNeeded() {
	stats := chm.connManager.GetConnectionStats()
	
	healthyDCs := 0
	totalDCs := len(stats)
	
	for _, dcStats := range stats {
		if dcStats.HealthStatus == "healthy" {
			healthyDCs++
		}
	}

	// If less than half the DCs are healthy, trigger broad recovery
	if totalDCs > 0 && float64(healthyDCs)/float64(totalDCs) < 0.5 {
		chm.scheduleRecoveryTask(RecoveryTask{
			Type:        RecoveryNetworkReset,
			Priority:    90,
			Reason:      fmt.Sprintf("insufficient healthy DCs: %d/%d", healthyDCs, totalDCs),
			MaxAttempts: 2,
			CreatedAt:   time.Now(),
		})
	}
}

// cleanupLoop periodically cleans up old health check data
func (chm *ConnectionHealthMonitor) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			chm.performCleanup()
		case <-chm.ctx.Done():
			return
		}
	}
}

// performCleanup removes stale health check entries
func (chm *ConnectionHealthMonitor) performCleanup() {
	chm.mu.Lock()
	defer chm.mu.Unlock()

	cutoff := time.Now().Add(-1 * time.Hour)
	removed := 0

	for peerID, healthCheck := range chm.healthChecks {
		healthCheck.mu.RLock()
		lastSeen := healthCheck.LastSeen
		healthCheck.mu.RUnlock()

		if lastSeen.Before(cutoff) {
			delete(chm.healthChecks, peerID)
			removed++
		}
	}

	if removed > 0 && chm.config.DetailedLogging {
		log.Printf("[%s] Cleaned up %d stale health check entries", chm.monitorID, removed)
	}
}

// RegisterPeer adds a peer to health monitoring
func (chm *ConnectionHealthMonitor) RegisterPeer(peerID, dataCenter string) {
	chm.mu.Lock()
	defer chm.mu.Unlock()

	if _, exists := chm.healthChecks[peerID]; exists {
		return
	}

	chm.healthChecks[peerID] = &HealthCheck{
		PeerID:     peerID,
		DataCenter: dataCenter,
		LastSeen:   time.Now(),
		Status:     PeerHealthy,
	}

	if chm.config.DetailedLogging {
		log.Printf("[%s] Registered peer for health monitoring: %s", chm.monitorID, peerID[:12])
	}
}

// UnregisterPeer removes a peer from health monitoring
func (chm *ConnectionHealthMonitor) UnregisterPeer(peerID string) {
	chm.mu.Lock()
	defer chm.mu.Unlock()

	delete(chm.healthChecks, peerID)
	
	if chm.config.DetailedLogging {
		log.Printf("[%s] Unregistered peer from health monitoring: %s", chm.monitorID, peerID[:12])
	}
}

// GetHealthStatus returns the current health status
func (chm *ConnectionHealthMonitor) GetHealthStatus() HealthMonitorStatus {
	chm.mu.RLock()
	defer chm.mu.RUnlock()

	status := HealthMonitorStatus{
		MonitorID:      chm.monitorID,
		IsRunning:      chm.isRunning,
		TrackedPeers:   len(chm.healthChecks),
		PendingTasks:   len(chm.recoveryTasks),
		LastUpdate:     time.Now(),
		PeerHealth:     make(map[string]PeerHealthStatus),
	}

	healthCounts := make(map[PeerHealthStatus]int)
	for peerID, healthCheck := range chm.healthChecks {
		healthCheck.mu.RLock()
		peerStatus := healthCheck.Status
		healthCheck.mu.RUnlock()
		
		status.PeerHealth[peerID] = peerStatus
		healthCounts[peerStatus]++
	}

	status.HealthSummary = healthCounts
	return status
}

// HealthMonitorStatus provides status information about health monitoring
type HealthMonitorStatus struct {
	MonitorID     string                       `json:"monitor_id"`
	IsRunning     bool                         `json:"is_running"`
	TrackedPeers  int                          `json:"tracked_peers"`
	PendingTasks  int                          `json:"pending_tasks"`
	LastUpdate    time.Time                    `json:"last_update"`
	PeerHealth    map[string]PeerHealthStatus  `json:"peer_health"`
	HealthSummary map[PeerHealthStatus]int     `json:"health_summary"`
}

// Stop shuts down the health monitor
func (chm *ConnectionHealthMonitor) Stop() error {
	chm.mu.Lock()
	defer chm.mu.Unlock()

	if !chm.isRunning {
		return nil
	}

	log.Printf("[%s] Stopping connection health monitoring", chm.monitorID)
	
	chm.cancel()
	chm.isRunning = false
	
	log.Printf("[%s] Health monitoring stopped", chm.monitorID)
	return nil
}