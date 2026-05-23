//go:build linux

package overlay

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// procNetRoutePath is the kernel-exported routing table in hex form.
// Columns: Iface Destination Gateway Flags RefCnt Use Metric Mask MTU
// Window IRTT. The MTU column is populated only for cached routes, so
// we read the per-interface MTU from /sys instead.
const procNetRoutePath = "/proc/net/route"

// sysClassNetMTUFmt yields the canonical MTU sysfs entry for an
// interface. Format string keeps the test injectable via fileReader.
const sysClassNetMTUFmt = "/sys/class/net/%s/mtu"

// fileReader returns the file at path. Defaults to os.Open; tests
// substitute via newProcRouteResolverWithReader to feed canned
// /proc/net/route + /sys content without touching the real kernel.
type fileReader func(path string) (io.ReadCloser, error)

// procRouteResolver implements RouteResolver by parsing /proc/net/route
// for the default-route interface and reading its MTU from /sys.
type procRouteResolver struct {
	routePath string
	mtuPathFn func(iface string) string
	open      fileReader
}

// defaultRouteResolver returns the production RouteResolver wired to
// the real /proc and /sys paths.
func defaultRouteResolver() RouteResolver {
	return &procRouteResolver{
		routePath: procNetRoutePath,
		mtuPathFn: func(iface string) string { return fmt.Sprintf(sysClassNetMTUFmt, iface) },
		open:      func(p string) (io.ReadCloser, error) { return os.Open(p) },
	}
}

// newProcRouteResolverWithReader is the test constructor: it returns
// a resolver whose file reads, route path, and MTU-path mapper are all
// injectable. Production code never calls this.
func newProcRouteResolverWithReader(routePath string, mtuPathFn func(string) string, open fileReader) *procRouteResolver {
	return &procRouteResolver{routePath: routePath, mtuPathFn: mtuPathFn, open: open}
}

// DefaultRouteMTU returns the MTU of the interface holding the default
// (0.0.0.0) IPv4 route, read from /sys/class/net/<iface>/mtu.
func (p *procRouteResolver) DefaultRouteMTU() (int, error) {
	iface, err := p.defaultRouteIface()
	if err != nil {
		return 0, err
	}
	raw, err := p.readAll(p.mtuPathFn(iface))
	if err != nil {
		return 0, fmt.Errorf("read MTU for %s: %w", iface, err)
	}
	mtu, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("parse MTU for %s: %w", iface, err)
	}
	if mtu <= 0 {
		return 0, fmt.Errorf("invalid MTU %d for %s", mtu, iface)
	}
	return mtu, nil
}

// defaultRouteIface scans /proc/net/route and returns the Iface column
// of the first row whose Destination is hex 00000000 (0.0.0.0).
func (p *procRouteResolver) defaultRouteIface() (string, error) {
	raw, err := p.readAll(p.routePath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p.routePath, err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	headerSeen := false
	for scanner.Scan() {
		line := scanner.Text()
		if !headerSeen {
			headerSeen = true
			continue
		}
		fields := strings.Fields(line)
		// Iface + Destination + Gateway + Flags + ... = ≥4 columns.
		if len(fields) < 4 {
			continue
		}
		if fields[1] == "00000000" {
			return fields[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan %s: %w", p.routePath, err)
	}
	return "", errors.New("no default route in /proc/net/route")
}

// readAll opens the path via the injected reader and slurps it. The
// indirection keeps Linux production code on os.Open and tests on a
// pure in-memory map.
func (p *procRouteResolver) readAll(path string) ([]byte, error) {
	rc, err := p.open(path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
