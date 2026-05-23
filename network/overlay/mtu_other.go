//go:build !linux

package overlay

// unsupportedRouteResolver is the non-Linux stub. macOS and Windows
// have no equivalent of /proc/net/route in a portable form; operators
// on those platforms must set WithUnderlayMTU or WithBridgeMTU
// explicitly. Resolve will propagate ErrUnsupported via
// ErrRouteResolverFailed.
type unsupportedRouteResolver struct{}

// DefaultRouteMTU always returns ErrUnsupported on non-Linux hosts.
func (unsupportedRouteResolver) DefaultRouteMTU() (int, error) {
	return 0, ErrUnsupported
}

// defaultRouteResolver returns the non-Linux stub.
func defaultRouteResolver() RouteResolver { return unsupportedRouteResolver{} }
