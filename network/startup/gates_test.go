package startup

import (
	"errors"
	"fmt"
	"runtime"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
)

// fakeFS keys file contents on absolute path so tests can fake procfs
// without touching the real kernel.
type fakeFS map[string][]byte

func (f fakeFS) read(path string) ([]byte, error) {
	v, ok := f[path]
	if !ok {
		return nil, fmt.Errorf("not found: %s", path)
	}
	return v, nil
}

// fakeCmd routes command invocations to canned (output, error) pairs.
// Key format: "<name> <arg1> <arg2> ...".
type fakeCmd map[string]cmdResult

type cmdResult struct {
	out []byte
	err error
}

func (f fakeCmd) run(name string, args ...string) ([]byte, error) {
	key := name
	for _, a := range args {
		key += " " + a
	}
	if r, ok := f[key]; ok {
		return r.out, r.err
	}
	return nil, fmt.Errorf("command not stubbed: %s", key)
}

// rpFilterPath is a fixed virtual path used by all rp_filter tests so the
// fakeFS keys are stable regardless of the real defaultRPFilterPath.
const testRPFilterPath = "/test/proc/rp_filter"

func TestNew_DefaultsAreStrict(t *testing.T) {
	g := New()
	if !g.requireRPFilter {
		t.Fatalf("expected requireRPFilter=true by default")
	}
	if g.requireIptablesTakeover {
		t.Fatalf("expected requireIptablesTakeover=false by default")
	}
}

func TestWithRPFilterCheckDisabled(t *testing.T) {
	g := New(WithRPFilterCheckDisabled())
	if g.requireRPFilter {
		t.Fatalf("WithRPFilterCheckDisabled did not clear requireRPFilter")
	}
}

func TestWithIptablesTakeover(t *testing.T) {
	g := New(WithIptablesTakeover())
	if !g.requireIptablesTakeover {
		t.Fatalf("WithIptablesTakeover did not set requireIptablesTakeover")
	}
}

func TestWithLogger_NilIsIgnored(t *testing.T) {
	g := New(WithLogger(nil))
	if g.logger == nil {
		t.Fatalf("logger must never be nil")
	}
}

// emptyCmd stubs every probe to "command not found", which the probes
// interpret as "manager not active". Combined with a fakeFS that
// excludes firewalldPidPath, the iptables manager check passes.
func emptyCmd() fakeCmd {
	return fakeCmd{
		"systemctl is-active firewalld": {nil, errors.New("not found")},
		"ufw status":                    {nil, errors.New("not found")},
		"nft list ruleset":              {nil, errors.New("not found")},
		"iptables --version":            {nil, errors.New("not found")},
	}
}

func TestVerify_RPFilterStrictPasses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rp_filter assertions are Linux-only")
	}
	fs := fakeFS{testRPFilterPath: []byte("1\n")}
	cmd := emptyCmd()
	g := New(
		WithProcReader(fs.read),
		WithCommandRunner(cmd.run),
		WithRPFilterPath(testRPFilterPath),
		WithLogger(zaptest.NewLogger(t)),
	)
	if err := g.Verify(); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestVerify_RPFilterDisabled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rp_filter assertions are Linux-only")
	}
	fs := fakeFS{testRPFilterPath: []byte("0\n")}
	cmd := emptyCmd()
	g := New(
		WithProcReader(fs.read),
		WithCommandRunner(cmd.run),
		WithRPFilterPath(testRPFilterPath),
	)
	err := g.Verify()
	if !errors.Is(err, ErrRPFilterNotStrict) {
		t.Fatalf("expected ErrRPFilterNotStrict, got %v", err)
	}
}

func TestVerify_RPFilterLoose(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rp_filter assertions are Linux-only")
	}
	fs := fakeFS{testRPFilterPath: []byte("2\n")}
	cmd := emptyCmd()
	g := New(
		WithProcReader(fs.read),
		WithCommandRunner(cmd.run),
		WithRPFilterPath(testRPFilterPath),
	)
	err := g.Verify()
	if !errors.Is(err, ErrRPFilterNotStrict) {
		t.Fatalf("expected ErrRPFilterNotStrict for loose mode (2), got %v", err)
	}
}

func TestVerify_FirewalldActive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("iptables manager probe is Linux-only")
	}
	fs := fakeFS{testRPFilterPath: []byte("1\n")}
	cmd := emptyCmd()
	cmd["systemctl is-active firewalld"] = cmdResult{out: []byte("active\n")}
	g := New(
		WithProcReader(fs.read),
		WithCommandRunner(cmd.run),
		WithRPFilterPath(testRPFilterPath),
	)
	err := g.Verify()
	if !errors.Is(err, ErrFirewalldActive) {
		t.Fatalf("expected ErrFirewalldActive, got %v", err)
	}
}

func TestVerify_FirewalldPidfileFallback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("iptables manager probe is Linux-only")
	}
	fs := fakeFS{
		testRPFilterPath: []byte("1\n"),
		firewalldPidPath: []byte("1234"),
	}
	cmd := emptyCmd()
	g := New(
		WithProcReader(fs.read),
		WithCommandRunner(cmd.run),
		WithRPFilterPath(testRPFilterPath),
	)
	err := g.Verify()
	if !errors.Is(err, ErrFirewalldActive) {
		t.Fatalf("expected ErrFirewalldActive via pidfile, got %v", err)
	}
}

func TestVerify_UFWActive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("iptables manager probe is Linux-only")
	}
	fs := fakeFS{testRPFilterPath: []byte("1\n")}
	cmd := emptyCmd()
	cmd["ufw status"] = cmdResult{out: []byte("Status: active\nLogging: on\n")}
	g := New(
		WithProcReader(fs.read),
		WithCommandRunner(cmd.run),
		WithRPFilterPath(testRPFilterPath),
	)
	err := g.Verify()
	if !errors.Is(err, ErrUFWActive) {
		t.Fatalf("expected ErrUFWActive, got %v", err)
	}
}

func TestVerify_UFWInactiveDoesNotFire(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("iptables manager probe is Linux-only")
	}
	fs := fakeFS{testRPFilterPath: []byte("1\n")}
	cmd := emptyCmd()
	cmd["ufw status"] = cmdResult{out: []byte("Status: inactive\n")}
	g := New(
		WithProcReader(fs.read),
		WithCommandRunner(cmd.run),
		WithRPFilterPath(testRPFilterPath),
	)
	if err := g.Verify(); err != nil {
		t.Fatalf("expected nil for inactive ufw, got %v", err)
	}
}

// largeNftRuleset is a contrived `nft list ruleset` output with > 5
// lines, indicating user-defined rules.
const largeNftRuleset = `table inet filter {
	chain input {
		type filter hook input priority 0;
		policy drop;
		ct state established,related accept
		iif lo accept
		tcp dport 22 accept
	}
}`

func TestVerify_NftablesUserRules(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("iptables manager probe is Linux-only")
	}
	fs := fakeFS{testRPFilterPath: []byte("1\n")}
	cmd := emptyCmd()
	cmd["nft list ruleset"] = cmdResult{out: []byte(largeNftRuleset)}
	cmd["iptables --version"] = cmdResult{out: []byte("iptables v1.8.10 (nf_tables)\n")}
	g := New(
		WithProcReader(fs.read),
		WithCommandRunner(cmd.run),
		WithRPFilterPath(testRPFilterPath),
	)
	err := g.Verify()
	if !errors.Is(err, ErrNftablesActive) {
		t.Fatalf("expected ErrNftablesActive, got %v", err)
	}
}

func TestVerify_NftablesEmptyDoesNotFire(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("iptables manager probe is Linux-only")
	}
	fs := fakeFS{testRPFilterPath: []byte("1\n")}
	cmd := emptyCmd()
	cmd["nft list ruleset"] = cmdResult{out: []byte("")}
	cmd["iptables --version"] = cmdResult{out: []byte("iptables v1.8.10 (nf_tables)\n")}
	g := New(
		WithProcReader(fs.read),
		WithCommandRunner(cmd.run),
		WithRPFilterPath(testRPFilterPath),
	)
	if err := g.Verify(); err != nil {
		t.Fatalf("expected nil for empty nft ruleset, got %v", err)
	}
}

func TestVerify_TakeoverOptInLogsWarn(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("iptables manager probe is Linux-only")
	}
	core, recorded := observer.New(zap.WarnLevel)
	fs := fakeFS{testRPFilterPath: []byte("1\n")}
	cmd := emptyCmd()
	cmd["systemctl is-active firewalld"] = cmdResult{out: []byte("active\n")}
	g := New(
		WithProcReader(fs.read),
		WithCommandRunner(cmd.run),
		WithRPFilterPath(testRPFilterPath),
		WithIptablesTakeover(),
		WithLogger(zap.New(core)),
	)
	if err := g.Verify(); err != nil {
		t.Fatalf("expected nil with takeover opt-in, got %v", err)
	}
	warnings := recorded.FilterMessage("iptables manager active but takeover opted in").All()
	if len(warnings) != 1 {
		t.Fatalf("expected 1 takeover warning, got %d", len(warnings))
	}
}

func TestVerify_AllChecksDisabled(t *testing.T) {
	// rp_filter check disabled + no managers stubbed active => clean.
	fs := fakeFS{} // empty: rp_filter read would fail if it ran.
	cmd := emptyCmd()
	g := New(
		WithRPFilterCheckDisabled(),
		WithProcReader(fs.read),
		WithCommandRunner(cmd.run),
		WithRPFilterPath(testRPFilterPath),
	)
	if err := g.Verify(); err != nil {
		t.Fatalf("expected nil with all checks neutralised, got %v", err)
	}
}

func TestVerify_JoinsMultipleErrors(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rp_filter assertions are Linux-only")
	}
	fs := fakeFS{testRPFilterPath: []byte("0\n")}
	cmd := emptyCmd()
	cmd["ufw status"] = cmdResult{out: []byte("Status: active\n")}
	g := New(
		WithProcReader(fs.read),
		WithCommandRunner(cmd.run),
		WithRPFilterPath(testRPFilterPath),
	)
	err := g.Verify()
	if !errors.Is(err, ErrRPFilterNotStrict) {
		t.Fatalf("expected joined error to include ErrRPFilterNotStrict, got %v", err)
	}
	if !errors.Is(err, ErrUFWActive) {
		t.Fatalf("expected joined error to include ErrUFWActive, got %v", err)
	}
}

func TestVerify_RPFilterUnparseable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rp_filter assertions are Linux-only")
	}
	fs := fakeFS{testRPFilterPath: []byte("not-a-number")}
	cmd := emptyCmd()
	g := New(
		WithProcReader(fs.read),
		WithCommandRunner(cmd.run),
		WithRPFilterPath(testRPFilterPath),
	)
	err := g.Verify()
	if err == nil {
		t.Fatalf("expected parse error, got nil")
	}
	// Should NOT match the not-strict sentinel — it's a different failure.
	if errors.Is(err, ErrRPFilterNotStrict) {
		t.Fatalf("parse error should not alias ErrRPFilterNotStrict: %v", err)
	}
}
