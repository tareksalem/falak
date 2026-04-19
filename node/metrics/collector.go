package metrics

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/net"
	"go.uber.org/zap"
)

// Sampler is the interface the collector uses to read host metrics.
// Defining the interface (rather than calling gopsutil functions directly)
// keeps the collector unit-testable: tests can plug in a deterministic
// fake without needing real hardware. The production implementation
// (gopsutilSampler) is a thin wrapper over gopsutil.
type Sampler interface {
	// CPU returns the current CPU stats: total core count and aggregate
	// utilization across all cores in [0, 100].
	CPU(ctx context.Context) (CPUStats, error)
	// Memory returns total / available / used / used%.
	Memory(ctx context.Context) (MemoryStats, error)
	// Disk returns aggregate primary-volume stats.
	Disk(ctx context.Context) (DiskStats, error)
	// Load returns kernel load averages (1m/5m/15m). Zero on platforms
	// where the metric is not available.
	Load(ctx context.Context) (LoadStats, error)
	// NetworkCounters returns the absolute byte counters across all
	// non-loopback interfaces. The collector subtracts two consecutive
	// readings and divides by elapsed time to compute throughput.
	NetworkCounters(ctx context.Context) (sentBytes, recvBytes int64, err error)
}

// Collector samples host metrics on a periodic schedule and emits each
// fresh Snapshot via a callback. It is the producer half of the metrics
// subsystem; the Manager owns the lifecycle and the consumer.
//
// The collector is intentionally a thin loop: it owns the sampling loop,
// network throughput delta computation, and per-iteration error handling.
// All storage and gossip live elsewhere.
type Collector struct {
	nodeID    string
	sampler   Sampler
	interval  time.Duration
	logger    *zap.Logger
	onSnapshot func(Snapshot)

	// Internal state for computing network throughput between samples.
	// We keep the previous absolute counter readings and the time at
	// which they were taken; the next sample subtracts and divides.
	prevSentBytes int64
	prevRecvBytes int64
	prevSampleAt  time.Time
	hasPrev       bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// CollectorOption configures a Collector.
type CollectorOption func(*Collector)

// WithCollectorLogger sets the logger.
func WithCollectorLogger(logger *zap.Logger) CollectorOption {
	return func(c *Collector) {
		c.logger = logger
	}
}

// WithCollectorSampler overrides the Sampler implementation. Tests use
// this to inject a deterministic fake; production lets the default
// gopsutil sampler stand.
func WithCollectorSampler(s Sampler) CollectorOption {
	return func(c *Collector) {
		c.sampler = s
	}
}

// WithCollectorOnSnapshot sets the callback invoked once per successful
// sample. The callback is called from the collector's own goroutine and
// must be safe to call concurrently with anything else the consumer does.
// It must also return promptly — slow consumers stretch the sampling
// interval and skew network throughput calculations.
func WithCollectorOnSnapshot(fn func(Snapshot)) CollectorOption {
	return func(c *Collector) {
		c.onSnapshot = fn
	}
}

// NewCollector constructs a Collector for the given node ID and interval.
// The interval must already be validated by the Config layer; the
// collector trusts it.
func NewCollector(nodeID string, interval time.Duration, opts ...CollectorOption) *Collector {
	c := &Collector{
		nodeID:     nodeID,
		interval:   interval,
		logger:     zap.NewNop(),
		sampler:    newGopsutilSampler(),
		onSnapshot: func(Snapshot) {},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Start launches the sampling goroutine. It does an immediate first
// sample so the consumer has data right away, then ticks at the
// configured interval. Idempotent: calling Start a second time without
// an intervening Stop is a no-op.
func (c *Collector) Start(parent context.Context) {
	if c.ctx != nil {
		return
	}
	c.ctx, c.cancel = context.WithCancel(parent)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.run()
	}()
	c.logger.Info("metrics collector started",
		zap.String("node_id", c.nodeID),
		zap.Duration("interval", c.interval))
}

// Stop signals the sampling goroutine to exit and waits for it. Safe to
// call multiple times; idempotent.
func (c *Collector) Stop() {
	if c.cancel == nil {
		return
	}
	c.cancel()
	c.wg.Wait()
	c.cancel = nil
	c.ctx = nil
	c.logger.Info("metrics collector stopped")
}

// run is the sampling loop. It performs an immediate first sample so the
// consumer has data without waiting for the first tick, then samples on
// every tick until the context is cancelled.
func (c *Collector) run() {
	c.sampleOnce()

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.sampleOnce()
		}
	}
}

// sampleOnce performs a single sample pass. Errors are logged at warn
// level (one error should not kill the loop) and the snapshot is dropped.
// Network throughput is only emitted on the second and subsequent samples
// because the first one has no previous reading to compare against.
func (c *Collector) sampleOnce() {
	now := time.Now()

	cpuStats, err := c.sampler.CPU(c.ctx)
	if err != nil {
		c.logger.Warn("metrics: CPU sample failed", zap.Error(err))
		return
	}
	memStats, err := c.sampler.Memory(c.ctx)
	if err != nil {
		c.logger.Warn("metrics: memory sample failed", zap.Error(err))
		return
	}
	diskStats, err := c.sampler.Disk(c.ctx)
	if err != nil {
		c.logger.Warn("metrics: disk sample failed", zap.Error(err))
		return
	}
	loadStats, err := c.sampler.Load(c.ctx)
	if err != nil {
		// Load average is optional on some platforms; downgrade to debug.
		c.logger.Debug("metrics: load sample failed", zap.Error(err))
	}

	sent, recv, err := c.sampler.NetworkCounters(c.ctx)
	if err != nil {
		c.logger.Warn("metrics: network sample failed", zap.Error(err))
		return
	}

	var netStats NetworkStats
	if c.hasPrev {
		elapsed := now.Sub(c.prevSampleAt).Seconds()
		if elapsed > 0 {
			netStats.BytesSentPerSec = int64(float64(sent-c.prevSentBytes) / elapsed)
			netStats.BytesRecvPerSec = int64(float64(recv-c.prevRecvBytes) / elapsed)
		}
	}
	c.prevSentBytes = sent
	c.prevRecvBytes = recv
	c.prevSampleAt = now
	c.hasPrev = true

	snap := Snapshot{
		NodeID:     c.nodeID,
		CapturedAt: now,
		CPU:        cpuStats,
		Memory:     memStats,
		Disk:       diskStats,
		Load:       loadStats,
		Network:    netStats,
	}
	c.onSnapshot(snap)
}

// --- Production Sampler implementation ---------------------------------------

// gopsutilSampler is the production Sampler. It calls gopsutil with
// short context timeouts so a hung kernel call cannot stall the
// sampling loop indefinitely.
type gopsutilSampler struct{}

func newGopsutilSampler() *gopsutilSampler {
	return &gopsutilSampler{}
}

// sampleTimeout caps how long any single gopsutil call may take.
// gopsutil has internal sleeps for some metrics (CPU percent uses a
// short measurement window); we give it a generous budget so accurate
// readings are possible while still preventing indefinite hangs.
const sampleTimeout = 2 * time.Second

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, sampleTimeout)
}

func (gopsutilSampler) CPU(ctx context.Context) (CPUStats, error) {
	cctx, cancel := withTimeout(ctx)
	defer cancel()

	cores, err := cpu.CountsWithContext(cctx, true)
	if err != nil {
		return CPUStats{}, fmt.Errorf("cpu count: %w", err)
	}

	// Pass interval=0 so gopsutil returns the average since the previous
	// call (or instant; both work). The collector loop interval gives us
	// the cadence we need.
	percents, err := cpu.PercentWithContext(cctx, 0, false)
	if err != nil {
		return CPUStats{}, fmt.Errorf("cpu percent: %w", err)
	}
	var aggregate float64
	if len(percents) > 0 {
		aggregate = percents[0]
	}
	return CPUStats{
		Cores:       int32(cores),
		UsedPercent: aggregate,
	}, nil
}

func (gopsutilSampler) Memory(ctx context.Context) (MemoryStats, error) {
	cctx, cancel := withTimeout(ctx)
	defer cancel()

	vm, err := mem.VirtualMemoryWithContext(cctx)
	if err != nil {
		return MemoryStats{}, fmt.Errorf("memory: %w", err)
	}
	const mb = uint64(1024 * 1024)
	return MemoryStats{
		TotalMB:     int64(vm.Total / mb),
		AvailableMB: int64(vm.Available / mb),
		UsedMB:      int64(vm.Used / mb),
		UsedPercent: vm.UsedPercent,
	}, nil
}

func (gopsutilSampler) Disk(ctx context.Context) (DiskStats, error) {
	cctx, cancel := withTimeout(ctx)
	defer cancel()

	// Use the root filesystem as the primary volume. On Windows this
	// reports the system drive (C:). On Linux/macOS it's `/`.
	usage, err := disk.UsageWithContext(cctx, "/")
	if err != nil {
		return DiskStats{}, fmt.Errorf("disk: %w", err)
	}
	const mb = uint64(1024 * 1024)
	return DiskStats{
		TotalMB:     int64(usage.Total / mb),
		FreeMB:      int64(usage.Free / mb),
		UsedMB:      int64(usage.Used / mb),
		UsedPercent: usage.UsedPercent,
	}, nil
}

func (gopsutilSampler) Load(ctx context.Context) (LoadStats, error) {
	cctx, cancel := withTimeout(ctx)
	defer cancel()

	avg, err := load.AvgWithContext(cctx)
	if err != nil {
		// Load is unsupported on plain Windows. Return zeros instead of
		// failing the entire sample.
		return LoadStats{}, nil
	}
	return LoadStats{
		One:     avg.Load1,
		Five:    avg.Load5,
		Fifteen: avg.Load15,
	}, nil
}

func (gopsutilSampler) NetworkCounters(ctx context.Context) (int64, int64, error) {
	cctx, cancel := withTimeout(ctx)
	defer cancel()

	// pernic=false aggregates across all interfaces.
	stats, err := net.IOCountersWithContext(cctx, false)
	if err != nil {
		return 0, 0, fmt.Errorf("network: %w", err)
	}
	if len(stats) == 0 {
		return 0, 0, nil
	}
	return int64(stats[0].BytesSent), int64(stats[0].BytesRecv), nil
}
