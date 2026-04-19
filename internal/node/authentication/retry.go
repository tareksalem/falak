package authentication

import (
	"context"
	"log"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// RetryFunc defines the function to retry
type RetryFunc func() error

// RetryState represents the current retry state for a peer
type RetryState struct {
	PeerID        peer.ID
	Attempts      int
	NextRetryTime time.Time
	LastError     error
	Status        RetryStatus
	TotalFailures int       // Track total failures for SWIM integration later
	RetryFunc     RetryFunc // The function to retry
}

// RetryStatus represents the status of retry attempts
type RetryStatus string

const (
	RetryStatusPending    RetryStatus = "pending"    // Ready to retry
	RetryStatusScheduled  RetryStatus = "scheduled"  // Retry scheduled
	RetryStatusExhausted  RetryStatus = "exhausted"  // Max attempts reached
	RetryStatusSucceeded  RetryStatus = "succeeded"  // Authentication succeeded
	RetryStatusDisabled   RetryStatus = "disabled"   // Retry disabled for this peer
)

// RetryManager manages retry attempts with exponential backoff (generic, not auth-specific)
type RetryManager struct {
	config     *Config
	retryStates sync.Map // peerID -> *RetryState
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	ticker     *time.Ticker
}

// NewRetryManager creates a new retry manager
func NewRetryManager(config *Config) *RetryManager {
	ctx, cancel := context.WithCancel(context.Background())

	return &RetryManager{
		config: config,
		ctx:    ctx,
		cancel: cancel,
	}
}

// Start begins the retry management process
func (rm *RetryManager) Start() error {
	if !rm.config.RetryEnabled {
		log.Printf("🔄 Authentication retry is disabled")
		return nil
	}

	log.Printf("🔄 Starting authentication retry manager")

	// Start retry scheduler
	rm.ticker = time.NewTicker(1 * time.Second) // Check every second for due retries
	rm.wg.Add(1)
	go rm.retryScheduler()

	return nil
}

// Stop stops the retry manager
func (rm *RetryManager) Stop() error {
	if rm.ticker != nil {
		rm.ticker.Stop()
	}
	rm.cancel()
	rm.wg.Wait()

	log.Printf("✅ Authentication retry manager stopped")
	return nil
}

// ScheduleRetry schedules a retry for a failed operation
func (rm *RetryManager) ScheduleRetry(peerID peer.ID, err error, retryFunc RetryFunc) {
	if !rm.config.RetryEnabled {
		return
	}

	// Load or create retry state
	var retryState *RetryState
	if state, exists := rm.retryStates.Load(peerID.String()); exists {
		retryState = state.(*RetryState)
	} else {
		retryState = &RetryState{
			PeerID:        peerID,
			Attempts:      0,
			Status:        RetryStatusPending,
			TotalFailures: 0,
		}
	}

	// Update the retry function (may change between retries)
	retryState.RetryFunc = retryFunc

	// Update retry state
	retryState.Attempts++
	retryState.TotalFailures++
	retryState.LastError = err

	// Check if max attempts reached
	if retryState.Attempts >= rm.config.MaxRetryAttempts {
		retryState.Status = RetryStatusExhausted
		rm.retryStates.Store(peerID.String(), retryState)

		log.Printf("🚫 Retry exhausted for peer %s after %d attempts (last error: %v)",
			peerID.ShortString(), retryState.Attempts, err)

		// TODO: For future SWIM integration - mark peer as failed in phonebook
		// This is where we'll integrate with the phonebook status management

		return
	}

	// Calculate next retry delay with exponential backoff
	delay := rm.calculateRetryDelay(retryState.Attempts)
	retryState.NextRetryTime = time.Now().Add(delay)
	retryState.Status = RetryStatusScheduled

	// Store updated state
	rm.retryStates.Store(peerID.String(), retryState)

	log.Printf("🔄 Scheduled retry %d/%d for peer %s in %v (error: %v)",
		retryState.Attempts, rm.config.MaxRetryAttempts, peerID.ShortString(), delay, err)
}

// MarkSuccess marks a peer authentication as successful and clears retry state
func (rm *RetryManager) MarkSuccess(peerID peer.ID) {
	if state, exists := rm.retryStates.Load(peerID.String()); exists {
		retryState := state.(*RetryState)
		retryState.Status = RetryStatusSucceeded
		retryState.Attempts = 0 // Reset for future use
		rm.retryStates.Store(peerID.String(), retryState)

		log.Printf("✅ Authentication succeeded for peer %s (cleared retry state)", peerID.ShortString())
	}
}

// GetRetryState returns the current retry state for a peer
func (rm *RetryManager) GetRetryState(peerID peer.ID) (*RetryState, bool) {
	if state, exists := rm.retryStates.Load(peerID.String()); exists {
		return state.(*RetryState), true
	}
	return nil, false
}

// retryScheduler periodically checks for and executes scheduled retries
func (rm *RetryManager) retryScheduler() {
	defer rm.wg.Done()

	log.Printf("🔄 Authentication retry scheduler started")

	for {
		select {
		case <-rm.ctx.Done():
			log.Printf("🔄 Retry scheduler stopped")
			return
		case <-rm.ticker.C:
			rm.processScheduledRetries()
		}
	}
}

// processScheduledRetries checks for and executes any due retries
func (rm *RetryManager) processScheduledRetries() {
	now := time.Now()

	rm.retryStates.Range(func(key, value interface{}) bool {
		retryState := value.(*RetryState)

		// Check if this retry is due
		if retryState.Status == RetryStatusScheduled && now.After(retryState.NextRetryTime) {
			// Execute retry
			go rm.executeRetry(retryState)
		}

		return true // Continue iteration
	})
}

// executeRetry performs the actual retry attempt using the stored retry function
func (rm *RetryManager) executeRetry(retryState *RetryState) {
	log.Printf("🔄 Executing retry %d/%d for peer %s",
		retryState.Attempts, rm.config.MaxRetryAttempts, retryState.PeerID.ShortString())

	// Mark as pending to prevent duplicate retries
	retryState.Status = RetryStatusPending
	rm.retryStates.Store(retryState.PeerID.String(), retryState)

	// Execute the retry function
	if retryState.RetryFunc == nil {
		log.Printf("❌ No retry function available for peer %s", retryState.PeerID.ShortString())
		return
	}

	err := retryState.RetryFunc()
	if err != nil {
		log.Printf("🔄 Retry failed for peer %s: %v", retryState.PeerID.ShortString(), err)

		// Schedule next retry if not exhausted
		rm.ScheduleRetry(retryState.PeerID, err, retryState.RetryFunc)
	} else {
		log.Printf("✅ Retry succeeded for peer %s", retryState.PeerID.ShortString())
		rm.MarkSuccess(retryState.PeerID)
	}
}

// calculateRetryDelay calculates the delay for the next retry using exponential backoff
func (rm *RetryManager) calculateRetryDelay(attempt int) time.Duration {
	// Calculate exponential backoff: initialDelay * (multiplier ^ (attempt - 1))
	delay := float64(rm.config.InitialRetryDelay) * math.Pow(rm.config.RetryMultiplier, float64(attempt-1))

	// Cap at maximum delay
	if delay > float64(rm.config.MaxRetryDelay) {
		delay = float64(rm.config.MaxRetryDelay)
	}

	retryDelay := time.Duration(delay)

	// Add jitter if enabled (±25% random variation)
	if rm.config.RetryJitter {
		jitter := float64(retryDelay) * 0.25 * (rand.Float64()*2 - 1) // -25% to +25%
		retryDelay = time.Duration(float64(retryDelay) + jitter)

		// Ensure jitter doesn't make delay negative
		if retryDelay < 0 {
			retryDelay = rm.config.InitialRetryDelay
		}
	}

	return retryDelay
}

// GetRetryStats returns statistics about retry attempts
func (rm *RetryManager) GetRetryStats() map[string]interface{} {
	stats := map[string]interface{}{
		"retry_enabled": rm.config.RetryEnabled,
		"active_retries": 0,
		"exhausted_retries": 0,
		"successful_retries": 0,
	}

	rm.retryStates.Range(func(key, value interface{}) bool {
		retryState := value.(*RetryState)
		switch retryState.Status {
		case RetryStatusScheduled, RetryStatusPending:
			stats["active_retries"] = stats["active_retries"].(int) + 1
		case RetryStatusExhausted:
			stats["exhausted_retries"] = stats["exhausted_retries"].(int) + 1
		case RetryStatusSucceeded:
			stats["successful_retries"] = stats["successful_retries"].(int) + 1
		}
		return true
	})

	return stats
}