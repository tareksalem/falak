// This file holds small helpers shared between tcp.go and udp.go:
// port-name translation, copy-end heuristics, listener-closed
// detection, and the half-close helper. Kept in a separate file to
// keep tcp.go below the LOC-per-file threshold.
package proxy

import (
	"errors"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/tareksalem/falak/network/endpoints"
)

// selectNamedPort translates the listener's service-port name to the
// replica's named container port. If portMap supplies a remap, the
// remapped name is used; otherwise identity. Returns the resolved
// container port and a found flag.
func selectNamedPort(ep endpoints.Endpoint, servicePort string, portMap map[string]string) (uint32, bool) {
	want := servicePort
	if remapped, ok := portMap[servicePort]; ok && remapped != "" {
		want = remapped
	}
	if want == "" {
		// Single-port replica fallback: if the replica exposes exactly
		// one named port, use it. This handles the "no port-name on
		// service" listener path.
		if len(ep.NamedPorts) == 1 {
			return ep.NamedPorts[0].ContainerPort, true
		}
		return 0, false
	}
	for _, np := range ep.NamedPorts {
		if np.Name == want {
			return np.ContainerPort, true
		}
	}
	return 0, false
}

// portToString stringifies a uint32 port without allocating a fmt
// path.
func portToString(p uint32) string { return strconv.FormatUint(uint64(p), 10) }

// isClosedListener reports whether err is the "listener closed"
// error returned by Accept after Close. Stdlib's net.ErrClosed
// matches both TCP listeners and PacketConn closes.
func isClosedListener(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	// Some older stdlib paths wrap the error in a string-form text;
	// match defensively.
	return strings.Contains(err.Error(), "use of closed network connection")
}

// isExpectedCopyEnd reports whether err is one of the "this side
// closed normally" cases we don't want to flag as a forward failure.
func isExpectedCopyEnd(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		// Timeout from SetReadDeadline-on-peer-close is how we cancel
		// the other-direction copy; not a real failure.
		return true
	}
	return false
}

// closeWrite calls CloseWrite on TCP connections to signal EOF to the
// peer without closing the read side. For non-TCP connections it
// falls back to Close.
func closeWrite(c net.Conn) error {
	if tc, ok := c.(*net.TCPConn); ok {
		return tc.CloseWrite()
	}
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}

// copyWeights returns a shallow copy of the weights map for safe
// retention by the SWRR cache.
func copyWeights(in map[string]int32) map[string]int32 {
	out := make(map[string]int32, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// weightsEqual reports whether two weight maps are identical.
func weightsEqual(a, b map[string]int32) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
