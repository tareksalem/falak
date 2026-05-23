// Package overlay owns the Falak per-group L2 overlay plumbing: MTU
// budget computation (this file), kernel VXLAN device setup, VNI
// allocation, and VTEP membership wiring. mtu.go implements task
// 11A.4 — underlay MTU autodetection and bridge MTU computation. The
// Linux default-route probe lives in mtu_linux.go; non-Linux hosts
// return ErrUnsupported via mtu_other.go.
package overlay

import (
	"errors"
	"fmt"

	"go.uber.org/zap"
)

// Sentinel errors returned by Resolver.Resolve. Match with errors.Is.
var (
	// ErrUnsupported signals MTU autodetection is not implemented on
	// the current GOOS. Operators must set WithUnderlayMTU or
	// WithBridgeMTU explicitly on non-Linux hosts.
	ErrUnsupported = errors.New("overlay: underlay MTU autodetection unsupported on this platform")

	// ErrBridgeMTUTooSmall signals the computed bridge MTU is below
	// the IPv4 minimum of 576 bytes (RFC 791 §3.2).
	ErrBridgeMTUTooSmall = errors.New("overlay: computed bridge MTU below IPv4 minimum (576)")

	// ErrRouteResolverFailed wraps the underlying detection failure
	// when no override was supplied and the RouteResolver could not
	// return a usable value.
	ErrRouteResolverFailed = errors.New("overlay: underlay MTU detection failed")
)

// defaultOverlayOverhead is VXLAN (50) + IPsec ESP with AES-GCM (60).
// Plan 11A.6 amended the original 50-byte assumption to 110 once
// IPsec transform-mode encryption became part of the v1 transport.
const defaultOverlayOverhead = 110

// minIPv4MTU is the IPv4 minimum link MTU. Falling below it on the
// bridge breaks TCP/IP for any reasonable workload.
const minIPv4MTU = 576

// RouteResolver returns the MTU of the interface that holds the host's
// default route. Implementations live in mtu_linux.go (procfs probe)
// and mtu_other.go (ErrUnsupported stub). Tests inject fakes via
// WithRouteResolver.
type RouteResolver interface {
	DefaultRouteMTU() (int, error)
}

// Plan is the resolved MTU budget for a node. Consumers use BridgeMTU
// directly on the per-group Linux bridge; UnderlayMTU and
// OverlayOverhead are reported for diagnostics and gossip telemetry.
// When WithBridgeMTU forces the bridge, UnderlayMTU is reported as
// bridge + overhead so the implied budget is still visible.
type Plan struct {
	UnderlayMTU     int
	BridgeMTU       int
	OverlayOverhead int
}

// Resolver computes the bridge MTU budget. Construct with New, call
// Resolve once per node startup. Stateless after construction; safe
// for concurrent use.
type Resolver struct {
	explicitUnderlay int // 0 = unset
	explicitBridge   int // 0 = unset
	overhead         int
	routeResolver    RouteResolver
	logger           *zap.Logger
}

// Option configures a Resolver.
type Option func(*Resolver)

// New constructs a Resolver with the default 110-byte overhead, the
// platform-default RouteResolver, and a no-op logger.
func New(opts ...Option) *Resolver {
	r := &Resolver{
		overhead:      defaultOverlayOverhead,
		routeResolver: defaultRouteResolver(),
		logger:        zap.NewNop(),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// WithUnderlayMTU forces the underlay MTU and skips detection. The
// bridge MTU is then underlay − overhead. Non-positive values ignored.
func WithUnderlayMTU(mtu int) Option {
	return func(r *Resolver) {
		if mtu > 0 {
			r.explicitUnderlay = mtu
		}
	}
}

// WithBridgeMTU forces the bridge MTU directly, skipping detection
// and the overhead subtraction. Non-positive values ignored.
func WithBridgeMTU(mtu int) Option {
	return func(r *Resolver) {
		if mtu > 0 {
			r.explicitBridge = mtu
		}
	}
}

// WithOverlayOverhead overrides the per-packet overhead subtracted
// from underlay MTU (default 110 = VXLAN 50 + IPsec ESP 60).
// Non-positive values ignored.
func WithOverlayOverhead(bytes int) Option {
	return func(r *Resolver) {
		if bytes > 0 {
			r.overhead = bytes
		}
	}
}

// WithLogger installs a zap logger. Nil is ignored.
func WithLogger(logger *zap.Logger) Option {
	return func(r *Resolver) {
		if logger != nil {
			r.logger = logger
		}
	}
}

// WithRouteResolver injects a RouteResolver, primarily for tests.
// Nil is ignored.
func WithRouteResolver(rr RouteResolver) Option {
	return func(r *Resolver) {
		if rr != nil {
			r.routeResolver = rr
		}
	}
}

// Resolve computes the MTU plan. Precedence: explicit bridge MTU >
// explicit underlay MTU > default-route detection. Returns
// ErrBridgeMTUTooSmall when the computed bridge value is below 576,
// or ErrRouteResolverFailed (wrapping the underlying cause) when
// detection is required and fails.
func (r *Resolver) Resolve() (Plan, error) {
	var plan Plan
	var source string
	switch {
	case r.explicitBridge > 0:
		plan = Plan{UnderlayMTU: r.explicitBridge + r.overhead, BridgeMTU: r.explicitBridge, OverlayOverhead: r.overhead}
		source = "explicit bridge override"
	case r.explicitUnderlay > 0:
		plan = Plan{UnderlayMTU: r.explicitUnderlay, BridgeMTU: r.explicitUnderlay - r.overhead, OverlayOverhead: r.overhead}
		source = "explicit underlay override"
	default:
		detected, err := r.routeResolver.DefaultRouteMTU()
		if err != nil {
			r.logger.Error("default route MTU probe failed", zap.String("error", err.Error()))
			return Plan{}, fmt.Errorf("%w: %v", ErrRouteResolverFailed, err)
		}
		plan = Plan{UnderlayMTU: detected, BridgeMTU: detected - r.overhead, OverlayOverhead: r.overhead}
		source = "default-route detection"
	}
	if plan.BridgeMTU < minIPv4MTU {
		return Plan{}, fmt.Errorf("%w (got %d; underlay=%d overhead=%d)",
			ErrBridgeMTUTooSmall, plan.BridgeMTU, plan.UnderlayMTU, plan.OverlayOverhead)
	}
	r.logger.Info("overlay MTU plan resolved",
		zap.String("source", source),
		zap.Int("underlay_mtu", plan.UnderlayMTU),
		zap.Int("bridge_mtu", plan.BridgeMTU),
		zap.Int("overlay_overhead", plan.OverlayOverhead))
	return plan, nil
}
