// Package startup implements the host-level pre-flight gates the daemon runs
// once at process start, before installing any networking rules.
//
// Two host conditions must hold for bridge isolation + visibility to be
// sound: (1) strict rp_filter so spoofed source IPs from another bridge
// can't bypass cross-bridge DROP; (2) no competing iptables manager
// (firewalld, ufw, nftables-as-master) that would flush Falak rules
// out-of-band. Operators accept the conflict explicitly via
// WithIptablesTakeover. Verify is read-only and safe to call repeatedly.
package startup

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// ProcReader reads a file from procfs/sysfs (test hook for `/proc/sys`).
type ProcReader func(path string) ([]byte, error)

// CommandRunner runs `name args...` and returns combined output + exit
// error (test hook for systemctl/ufw/nft/iptables).
type CommandRunner func(name string, args ...string) ([]byte, error)

// Sentinel errors returned by Verify; match with errors.Is.
var (
	// ErrRPFilterNotStrict signals rp_filter is not 1. Cross-bridge
	// source-IP spoofing is possible until fixed.
	ErrRPFilterNotStrict = errors.New("rp_filter is not in strict mode (expected 1)")

	// ErrFirewalldActive signals firewalld is running and will flush
	// Falak rules on reload.
	ErrFirewalldActive = errors.New("firewalld is active and will conflict with Falak iptables rules")

	// ErrUFWActive signals ufw is enabled and will rewrite the FILTER
	// chain on each reload.
	ErrUFWActive = errors.New("ufw is active and will conflict with Falak iptables rules")

	// ErrNftablesActive signals user-defined nftables rules are present
	// that may mask iptables rules Falak installs.
	ErrNftablesActive = errors.New("nftables has user-defined rules that may mask iptables")
)

// defaultRPFilterPath: the canonical "all" rp_filter knob. Effective
// rp_filter for an interface is MAX(all, intf); `all` is the
// hardened-host signal.
const defaultRPFilterPath = "/proc/sys/net/ipv4/conf/all/rp_filter"

// firewalldPidPath: fallback signal when systemctl is unavailable.
const firewalldPidPath = "/var/run/firewalld/firewalld.pid"

// nftablesUserRuleThreshold: line count above which `nft list ruleset`
// is user-defined. iptables-nft on a clean host emits 0–5 lines.
const nftablesUserRuleThreshold = 5

// Gates encapsulates the host-level pre-flight checks. Construct with
// New, tune via options, call Verify once at daemon start.
type Gates struct {
	requireRPFilter         bool
	requireIptablesTakeover bool
	procReader              ProcReader
	commandRunner           CommandRunner
	rpFilterPath            string
	logger                  *zap.Logger
}

// Option configures a Gates instance.
type Option func(*Gates)

// New constructs a Gates with strict defaults (rp_filter required,
// takeover off) and the real procfs + os/exec wired in.
func New(opts ...Option) *Gates {
	g := &Gates{
		requireRPFilter:         true,
		requireIptablesTakeover: false,
		procReader:              defaultProcReader,
		commandRunner:           defaultCommandRunner,
		rpFilterPath:            defaultRPFilterPath,
		logger:                  zap.NewNop(),
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// WithRPFilterCheckDisabled skips the rp_filter strict-mode assertion.
// Intended for dev builds and the `network.skip_rp_filter_check` knob.
func WithRPFilterCheckDisabled() Option {
	return func(g *Gates) { g.requireRPFilter = false }
}

// WithIptablesTakeover acknowledges the operator accepts running
// alongside firewalld/ufw/nftables. Verify logs WARN per detected
// manager but returns nil.
func WithIptablesTakeover() Option {
	return func(g *Gates) { g.requireIptablesTakeover = true }
}

// WithLogger installs a zap logger. Nil is ignored.
func WithLogger(logger *zap.Logger) Option {
	return func(g *Gates) {
		if logger != nil {
			g.logger = logger
		}
	}
}

// WithProcReader injects a procfs reader (test hook).
func WithProcReader(fn ProcReader) Option {
	return func(g *Gates) {
		if fn != nil {
			g.procReader = fn
		}
	}
}

// WithCommandRunner injects a command runner (test hook).
func WithCommandRunner(fn CommandRunner) Option {
	return func(g *Gates) {
		if fn != nil {
			g.commandRunner = fn
		}
	}
}

// WithRPFilterPath overrides the procfs path read for rp_filter (test
// hook for fakeFS keying).
func WithRPFilterPath(path string) Option {
	return func(g *Gates) {
		if path != "" {
			g.rpFilterPath = path
		}
	}
}

// Verify runs the configured gates and returns a joined error containing
// every failure. A nil return means the host is safe for the daemon to
// install bridge isolation rules. Verify never mutates state.
func (g *Gates) Verify() error {
	var errs []error
	if err := g.rpFilterCheck(); err != nil {
		errs = append(errs, err)
	}
	if err := g.iptablesManagerCheck(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// rpFilterCheck reads the configured procfs path and returns
// ErrRPFilterNotStrict on any value other than 1. Non-Linux hosts skip
// the check so dev laptops on macOS/Windows can run the daemon locally.
func (g *Gates) rpFilterCheck() error {
	if !g.requireRPFilter {
		g.logger.Debug("rp_filter check disabled by config")
		return nil
	}
	if runtime.GOOS != "linux" {
		g.logger.Debug("rp_filter check skipped: non-Linux host",
			zap.String("os", runtime.GOOS))
		return nil
	}
	raw, err := g.procReader(g.rpFilterPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", g.rpFilterPath, err)
	}
	trimmed := strings.TrimSpace(string(raw))
	value, err := strconv.Atoi(trimmed)
	if err != nil {
		return fmt.Errorf("parse rp_filter value %q: %w", trimmed, err)
	}
	if value != 1 {
		return fmt.Errorf("%w (got %d at %s; set `sysctl -w net.ipv4.conf.all.rp_filter=1`)",
			ErrRPFilterNotStrict, value, g.rpFilterPath)
	}
	g.logger.Debug("rp_filter strict mode confirmed",
		zap.String("path", g.rpFilterPath))
	return nil
}

// iptablesManagerCheck probes for firewalld, ufw, and user-defined
// nftables rules. With WithIptablesTakeover, each detected manager is
// logged WARN and Verify returns nil for this check.
func (g *Gates) iptablesManagerCheck() error {
	if runtime.GOOS != "linux" {
		g.logger.Debug("iptables manager check skipped: non-Linux host",
			zap.String("os", runtime.GOOS))
		return nil
	}
	var detected []error
	if g.firewalldActive() {
		detected = append(detected, ErrFirewalldActive)
	}
	if g.ufwActive() {
		detected = append(detected, ErrUFWActive)
	}
	if g.nftablesUserRulesPresent() {
		detected = append(detected, ErrNftablesActive)
	}
	if len(detected) == 0 {
		return nil
	}
	if g.requireIptablesTakeover {
		for _, e := range detected {
			g.logger.Warn("iptables manager active but takeover opted in",
				zap.String("error", e.Error()))
		}
		return nil
	}
	return errors.Join(detected...)
}

// firewalldActive returns true when firewalld is running. Tries
// `systemctl is-active firewalld` first; falls back to the pidfile so
// minimal images without systemd still surface the conflict.
func (g *Gates) firewalldActive() bool {
	out, err := g.commandRunner("systemctl", "is-active", "firewalld")
	if err == nil && strings.TrimSpace(string(out)) == "active" {
		return true
	}
	_, statErr := g.procReader(firewalldPidPath)
	return statErr == nil
}

// ufwActive returns true when `ufw status` first line contains
// "Status: active". `ufw status` needs root; in unprivileged dev
// scenarios it errors and we treat that as "not active".
func (g *Gates) ufwActive() bool {
	out, err := g.commandRunner("ufw", "status")
	if err != nil {
		return false
	}
	lines := strings.SplitN(string(out), "\n", 2)
	if len(lines) == 0 {
		return false
	}
	return strings.Contains(lines[0], "Status: active")
}

// nftablesUserRulesPresent returns true when `nft list ruleset` exceeds
// the compatibility-backend line budget AND iptables is installed (so
// the conflict can actually bite). iptables-nft underneath iptables is
// the standard backend on modern distros and is NOT flagged on its own.
func (g *Gates) nftablesUserRulesPresent() bool {
	out, err := g.commandRunner("nft", "list", "ruleset")
	if err != nil {
		return false
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return false
	}
	lineCount := strings.Count(text, "\n") + 1
	if lineCount <= nftablesUserRuleThreshold {
		return false
	}
	_, ipErr := g.commandRunner("iptables", "--version")
	return ipErr == nil
}

// defaultProcReader defers to the platform-specific readProcFile so
// non-Linux dev hosts don't fail on absent procfs entries.
func defaultProcReader(path string) ([]byte, error) { return readProcFile(path) }

// defaultCommandRunner runs `name args...` and returns combined output.
// Probes treat both "command not found" and "non-zero exit" as
// "manager not active".
func defaultCommandRunner(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}
