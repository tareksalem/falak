//go:build linux

package overlay

import (
	"io"
	"strings"
	"testing"
)

// stringReadCloser adapts a string to io.ReadCloser for the injected
// fileReader. Close is a no-op.
type stringReadCloser struct{ *strings.Reader }

func (stringReadCloser) Close() error { return nil }

// TestResolver_ProcRouteParse feeds a synthetic /proc/net/route plus a
// fake /sys MTU file and confirms procRouteResolver picks the iface
// whose Destination column is 00000000 (default route).
func TestResolver_ProcRouteParse(t *testing.T) {
	const route = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"docker0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n" +
		"wlp0s20f3\t00000000\t0146A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
		"br-abc\t000013AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"

	files := map[string]string{
		"/fake/proc/net/route":           route,
		"/fake/sys/class/net/wlp0s20f3/mtu": "1500\n",
	}
	open := func(path string) (io.ReadCloser, error) {
		body, ok := files[path]
		if !ok {
			return nil, &fakeNotFound{path: path}
		}
		return stringReadCloser{strings.NewReader(body)}, nil
	}
	rr := newProcRouteResolverWithReader(
		"/fake/proc/net/route",
		func(iface string) string { return "/fake/sys/class/net/" + iface + "/mtu" },
		open,
	)
	mtu, err := rr.DefaultRouteMTU()
	if err != nil {
		t.Fatalf("DefaultRouteMTU: %v", err)
	}
	if mtu != 1500 {
		t.Fatalf("MTU: got %d, want 1500", mtu)
	}

	// End-to-end through Resolver, asserting bridge = underlay − default overhead.
	r := New(WithRouteResolver(rr))
	plan, err := r.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.BridgeMTU != 1500-defaultOverlayOverhead {
		t.Fatalf("BridgeMTU: got %d, want %d", plan.BridgeMTU, 1500-defaultOverlayOverhead)
	}
}

// fakeNotFound is a tiny error type used by the test fileReader so the
// resolver's read path exercises a realistic "missing file" failure
// shape.
type fakeNotFound struct{ path string }

func (e *fakeNotFound) Error() string { return "fake: not found: " + e.path }
