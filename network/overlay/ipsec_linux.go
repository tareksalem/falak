//go:build linux

package overlay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/vishvananda/netlink"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// AES-GCM AEAD identifiers as understood by the kernel xfrm_algo_aead
// table. `rfc4106(gcm(aes))` is the AES-GCM AEAD with the 4-byte salt
// appended to the key (the salt seeds the IV per RFC 4106).
const (
	aeadAESGCMName  = "rfc4106(gcm(aes))"
	aeadICVBits     = 128 // 16-byte ICV — kernel expects bits.
	aeadKeyBytes    = 32  // AES-256 key length.
	aeadSaltBytes   = 4   // RFC 4106 salt appended to the key.
	xfrmReqIDFalak  = 0xFA1A
	xfrmReplayWnd   = 64
)

// linuxIPsecManager is the production IPsecManager backed by netlink
// XFRM. Stateless; per-pair locking lives in PeerManager.
type linuxIPsecManager struct {
	logger *zap.Logger
}

// IPsecManagerOption configures an IPsecManager.
type IPsecManagerOption func(*linuxIPsecManager)

// WithIPsecManagerLogger replaces the default no-op logger.
func WithIPsecManagerLogger(l *zap.Logger) IPsecManagerOption {
	return func(m *linuxIPsecManager) {
		if l != nil {
			m.logger = l
		}
	}
}

// NewIPsecManager constructs the platform IPsecManager. On Linux this
// wraps netlink/XFRM; on every other GOOS the !linux build tag returns
// a stub whose every method yields ErrIPsecUnsupported.
func NewIPsecManager(opts ...IPsecManagerOption) IPsecManager {
	m := &linuxIPsecManager{logger: zap.NewNop()}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// falakMark returns a fresh XfrmMark struct on every call so callers
// don't accidentally share pointers across states/policies.
func falakMark() *netlink.XfrmMark {
	return &netlink.XfrmMark{Value: FalakXfrmMarkValue, Mask: FalakXfrmMarkMask}
}

// pairSPIs derives deterministic SPI values from the unordered pair so
// both sides agree without coordination. Splitting on lexicographic
// order gives stable outbound/inbound SPIs per direction.
func pairSPIs(local, peer string) (outbound, inbound int) {
	const baseSPI = 0xFA1A0001
	// Lex order picks the same "low" side on both nodes. Outbound
	// (local→peer) on node-low corresponds to inbound (peer→local) on
	// node-high. Use distinct values per direction to avoid collision.
	if local < peer {
		return baseSPI, baseSPI + 1
	}
	return baseSPI + 1, baseSPI
}

// buildAEAD returns the AEAD algo descriptor; the kernel reads the
// trailing 4-byte salt directly out of the key buffer.
func buildAEAD(key []byte) *netlink.XfrmStateAlgo {
	return &netlink.XfrmStateAlgo{
		Name:   aeadAESGCMName,
		Key:    append([]byte(nil), key...),
		ICVLen: aeadICVBits,
	}
}

// composeKey returns the kernel-format AEAD key: the AES key followed
// by the 4-byte RFC 4106 salt. The salt is derived from the AES key
// itself so both sides agree without storing it separately.
func composeKey(rawKey []byte) ([]byte, error) {
	if len(rawKey) != aeadKeyBytes {
		return nil, fmt.Errorf("overlay: ipsec key must be %d bytes (got %d)", aeadKeyBytes, len(rawKey))
	}
	out := make([]byte, aeadKeyBytes+aeadSaltBytes)
	copy(out, rawKey)
	// Salt = first 4 bytes of the key XOR'd with a fixed constant so
	// it differs from the cipher key but is deterministic per pair.
	for i := 0; i < aeadSaltBytes; i++ {
		out[aeadKeyBytes+i] = rawKey[i] ^ 0xA5
	}
	return out, nil
}

// parsedEndpoints holds the canonicalized v4-mapped IPs used across
// state/policy construction. Centralized so InstallSA, RemoveSA and
// ProbeConflict all parse identically.
type parsedEndpoints struct {
	local, peer net.IP
}

func parseEndpoints(local, peer string, port int) (parsedEndpoints, error) {
	if port <= 0 {
		return parsedEndpoints{}, fmt.Errorf("overlay: invalid port %d", port)
	}
	l := net.ParseIP(local)
	if l == nil {
		return parsedEndpoints{}, fmt.Errorf("overlay: parse local %q", local)
	}
	p := net.ParseIP(peer)
	if p == nil {
		return parsedEndpoints{}, fmt.Errorf("overlay: parse peer %q", peer)
	}
	if v4 := l.To4(); v4 != nil {
		l = v4
	}
	if v4 := p.To4(); v4 != nil {
		p = v4
	}
	return parsedEndpoints{local: l, peer: p}, nil
}

// InstallSA writes outbound (local→peer) and inbound (peer→local) SAs
// plus matching transport-mode policies. The XFRM mark scopes every
// row to Falak so ProbeConflict can ignore our own rules.
func (m *linuxIPsecManager) InstallSA(ctx context.Context, local, peer string, port int, key []byte) error {
	eps, err := parseEndpoints(local, peer, port)
	if err != nil {
		return err
	}
	composed, err := composeKey(key)
	if err != nil {
		return err
	}
	outSPI, inSPI := pairSPIs(local, peer)
	outbound := &netlink.XfrmState{
		Src: eps.local, Dst: eps.peer,
		Proto: netlink.XFRM_PROTO_ESP, Mode: netlink.XFRM_MODE_TRANSPORT,
		Spi: outSPI, Reqid: xfrmReqIDFalak, ReplayWindow: xfrmReplayWnd,
		Mark: falakMark(), Aead: buildAEAD(composed),
	}
	inbound := &netlink.XfrmState{
		Src: eps.peer, Dst: eps.local,
		Proto: netlink.XFRM_PROTO_ESP, Mode: netlink.XFRM_MODE_TRANSPORT,
		Spi: inSPI, Reqid: xfrmReqIDFalak, ReplayWindow: xfrmReplayWnd,
		Mark: falakMark(), Aead: buildAEAD(composed),
	}
	if err := addStateIdempotent(outbound); err != nil {
		return fmt.Errorf("overlay: install outbound SA: %w", err)
	}
	if err := addStateIdempotent(inbound); err != nil {
		// Best-effort roll back the outbound so we don't leave a half-installed pair.
		_ = netlink.XfrmStateDel(outbound)
		return fmt.Errorf("overlay: install inbound SA: %w", err)
	}
	outPol := buildPolicy(eps, port, netlink.XFRM_DIR_OUT, outSPI)
	inPol := buildPolicy(eps, port, netlink.XFRM_DIR_IN, inSPI)
	if err := addPolicyIdempotent(outPol); err != nil {
		_ = netlink.XfrmStateDel(outbound)
		_ = netlink.XfrmStateDel(inbound)
		return fmt.Errorf("overlay: install outbound policy: %w", err)
	}
	if err := addPolicyIdempotent(inPol); err != nil {
		_ = netlink.XfrmPolicyDel(outPol)
		_ = netlink.XfrmStateDel(outbound)
		_ = netlink.XfrmStateDel(inbound)
		return fmt.Errorf("overlay: install inbound policy: %w", err)
	}
	m.logger.Info("ipsec sa installed",
		zap.String("local", eps.local.String()),
		zap.String("peer", eps.peer.String()),
		zap.Int("port", port))
	return nil
}

// buildPolicy constructs an XFRM SPD entry for one direction. The
// selector is /32 host-pair + UDP/port; the template references the
// matching SA by SPI/proto/mode.
func buildPolicy(eps parsedEndpoints, port int, dir netlink.Dir, spi int) *netlink.XfrmPolicy {
	src, dst := eps.local, eps.peer
	srcPort, dstPort := 0, port
	if dir == netlink.XFRM_DIR_IN {
		src, dst = eps.peer, eps.local
		srcPort, dstPort = port, 0
	}
	return &netlink.XfrmPolicy{
		Src:     &net.IPNet{IP: src, Mask: net.CIDRMask(32, 32)},
		Dst:     &net.IPNet{IP: dst, Mask: net.CIDRMask(32, 32)},
		Proto:   netlink.Proto(unix.IPPROTO_UDP),
		DstPort: dstPort,
		SrcPort: srcPort,
		Dir:     dir,
		Mark:    falakMark(),
		Tmpls: []netlink.XfrmPolicyTmpl{{
			Src: src, Dst: dst,
			Proto: netlink.XFRM_PROTO_ESP,
			Mode:  netlink.XFRM_MODE_TRANSPORT,
			Spi:   spi, Reqid: xfrmReqIDFalak,
		}},
	}
}

// RemoveSA enumerates Falak-marked XFRM states + policies matching the
// (local, peer, UDP/port) selector and deletes them. Missing rows are
// silently treated as success.
func (m *linuxIPsecManager) RemoveSA(ctx context.Context, local, peer string, port int) error {
	eps, err := parseEndpoints(local, peer, port)
	if err != nil {
		return err
	}
	outSPI, inSPI := pairSPIs(local, peer)
	states := []*netlink.XfrmState{
		{Src: eps.local, Dst: eps.peer, Proto: netlink.XFRM_PROTO_ESP, Spi: outSPI, Mark: falakMark()},
		{Src: eps.peer, Dst: eps.local, Proto: netlink.XFRM_PROTO_ESP, Spi: inSPI, Mark: falakMark()},
	}
	for _, s := range states {
		if err := netlink.XfrmStateDel(s); err != nil && !isXfrmNotFound(err) {
			return fmt.Errorf("overlay: delete xfrm state: %w", err)
		}
	}
	policies := []*netlink.XfrmPolicy{
		buildPolicy(eps, port, netlink.XFRM_DIR_OUT, outSPI),
		buildPolicy(eps, port, netlink.XFRM_DIR_IN, inSPI),
	}
	for _, p := range policies {
		if err := netlink.XfrmPolicyDel(p); err != nil && !isXfrmNotFound(err) {
			return fmt.Errorf("overlay: delete xfrm policy: %w", err)
		}
	}
	m.logger.Info("ipsec sa removed",
		zap.String("local", eps.local.String()),
		zap.String("peer", eps.peer.String()),
		zap.Int("port", port))
	return nil
}

// ProbeConflict returns true if a non-Falak XFRM policy already
// covers (local, peer, UDP/port). We list all policies and filter in
// userspace — XfrmPolicyGet wants exact selectors and would miss
// shadow policies with wider selectors.
func (m *linuxIPsecManager) ProbeConflict(ctx context.Context, local, peer string, port int) (bool, error) {
	eps, err := parseEndpoints(local, peer, port)
	if err != nil {
		return false, err
	}
	policies, err := netlink.XfrmPolicyList(unix.AF_UNSPEC)
	if err != nil {
		return false, fmt.Errorf("overlay: list xfrm policies: %w", err)
	}
	udp := netlink.Proto(unix.IPPROTO_UDP)
	for i := range policies {
		p := &policies[i]
		if isFalakMark(p.Mark) {
			continue
		}
		if p.Proto != 0 && p.Proto != udp {
			continue
		}
		if p.DstPort != 0 && p.DstPort != port && p.SrcPort != 0 && p.SrcPort != port {
			continue
		}
		if !policyCovers(p, eps) {
			continue
		}
		m.logger.Error("ipsec policy conflict detected",
			zap.String("local", eps.local.String()),
			zap.String("peer", eps.peer.String()),
			zap.Int("port", port),
			zap.String("policy", p.String()))
		return true, nil
	}
	return false, nil
}

// policyCovers checks whether an existing policy's src/dst CIDRs
// overlap with the (local, peer) host pair in either direction.
func policyCovers(p *netlink.XfrmPolicy, eps parsedEndpoints) bool {
	matches := func(cidr *net.IPNet, ip net.IP) bool {
		if cidr == nil {
			return true // unset = wildcard
		}
		return cidr.Contains(ip)
	}
	fwd := matches(p.Src, eps.local) && matches(p.Dst, eps.peer)
	rev := matches(p.Src, eps.peer) && matches(p.Dst, eps.local)
	return fwd || rev
}

func isFalakMark(m *netlink.XfrmMark) bool {
	if m == nil {
		return false
	}
	return (m.Value&FalakXfrmMarkMask) == (FalakXfrmMarkValue&FalakXfrmMarkMask) && m.Mask&FalakXfrmMarkMask != 0
}

// addStateIdempotent treats "already exists" (EEXIST) as success so
// re-installing the same SA after a partial crash is safe.
func addStateIdempotent(s *netlink.XfrmState) error {
	if err := netlink.XfrmStateAdd(s); err != nil {
		if isXfrmExists(err) {
			return nil
		}
		return err
	}
	return nil
}

func addPolicyIdempotent(p *netlink.XfrmPolicy) error {
	if err := netlink.XfrmPolicyAdd(p); err != nil {
		if isXfrmExists(err) {
			return nil
		}
		return err
	}
	return nil
}

// isXfrmNotFound matches the ENOENT/ESRCH surfaces the kernel returns
// for missing XFRM rows across the v1.x netlink line.
func isXfrmNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) {
		return true
	}
	m := err.Error()
	return strings.Contains(m, "no such") || strings.Contains(m, "not found")
}

// isXfrmExists matches the EEXIST kernel surface for duplicate SA/policy adds.
func isXfrmExists(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, unix.EEXIST) {
		return true
	}
	return strings.Contains(err.Error(), "file exists")
}
