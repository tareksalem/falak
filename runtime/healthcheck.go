package runtime

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

// HealthChecker runs periodic liveness probes against a running
// container and reports failures via a callback. It supports HTTP and
// TCP probe types.
//
// The checker is started per container after the initial_delay period,
// then probes every interval. After retries consecutive failures the
// container is declared unhealthy and the onUnhealthy callback fires.
type HealthChecker struct {
	config  HealthCheck
	logger  *zap.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// HealthCheckerOption configures a HealthChecker.
type HealthCheckerOption func(*HealthChecker)

// WithHealthCheckerLogger sets the logger.
func WithHealthCheckerLogger(logger *zap.Logger) HealthCheckerOption {
	return func(h *HealthChecker) { h.logger = logger }
}

// NewHealthChecker creates a checker with the given probe configuration.
func NewHealthChecker(config HealthCheck, opts ...HealthCheckerOption) *HealthChecker {
	h := &HealthChecker{
		config: config,
		logger: zap.NewNop(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Start begins the probe loop. onUnhealthy is called when retries are
// exhausted. The loop runs until Stop is called or ctx is cancelled.
func (h *HealthChecker) Start(ctx context.Context, containerAddr string, onUnhealthy func()) {
	probeCtx, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	h.cancel = cancel
	h.mu.Unlock()

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.run(probeCtx, containerAddr, onUnhealthy)
	}()
}

// Stop cancels the probe loop and waits for it to exit.
func (h *HealthChecker) Stop() {
	h.mu.Lock()
	if h.cancel != nil {
		h.cancel()
	}
	h.mu.Unlock()
	h.wg.Wait()
}

// run is the main probe loop.
func (h *HealthChecker) run(ctx context.Context, addr string, onUnhealthy func()) {
	// Wait for the initial delay before starting probes.
	if h.config.InitialDelay > 0 {
		select {
		case <-time.After(h.config.InitialDelay):
		case <-ctx.Done():
			return
		}
	}

	consecutiveFailures := 0
	ticker := time.NewTicker(h.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if h.probe(ctx, addr) {
				consecutiveFailures = 0
			} else {
				consecutiveFailures++
				h.logger.Debug("health check failed",
					zap.String("address", addr),
					zap.Int("consecutive_failures", consecutiveFailures),
					zap.Int("retries", h.config.Retries))

				if consecutiveFailures >= h.config.Retries {
					h.logger.Warn("health check exhausted retries",
						zap.String("address", addr),
						zap.Int("failures", consecutiveFailures))
					onUnhealthy()
					return
				}
			}
		}
	}
}

// probe runs a single health check and returns true if healthy.
func (h *HealthChecker) probe(ctx context.Context, addr string) bool {
	switch h.config.Type {
	case HealthCheckTypeEnum.HTTP():
		return h.probeHTTP(ctx, addr)
	case HealthCheckTypeEnum.TCP():
		return h.probeTCP(ctx, addr)
	default:
		h.logger.Error("unknown health check type", zap.String("type", string(h.config.Type)))
		return false
	}
}

// probeHTTP sends a GET request and expects a 2xx response.
func (h *HealthChecker) probeHTTP(ctx context.Context, addr string) bool {
	url := fmt.Sprintf("http://%s:%d%s", addr, h.config.Port, h.config.Path)

	reqCtx, cancel := context.WithTimeout(ctx, h.config.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// probeTCP attempts a TCP connection and returns true if it succeeds.
func (h *HealthChecker) probeTCP(ctx context.Context, addr string) bool {
	target := fmt.Sprintf("%s:%d", addr, h.config.Port)
	dialer := net.Dialer{Timeout: h.config.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
