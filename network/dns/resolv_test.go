package dns

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildResolvConf(t *testing.T) {
	t.Parallel()
	body := BuildResolvConf()
	if !strings.Contains(body, "nameserver "+LinkLocalDNSAddr+"\n") {
		t.Fatalf("BuildResolvConf missing nameserver line: %q", body)
	}
	if !strings.Contains(body, "options ndots:0 timeout:1 attempts:2") {
		t.Fatalf("BuildResolvConf missing options line: %q", body)
	}
	// Idempotent: BuildResolvConf is pure.
	if BuildResolvConf() != body {
		t.Fatal("BuildResolvConf must be deterministic")
	}
}

func TestWriteResolvConfRoundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "resolv.conf")
	if err := WriteResolvConf(path); err != nil {
		t.Fatalf("WriteResolvConf: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("perm = %v want 0644", perm)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != BuildResolvConf() {
		t.Fatalf("body mismatch: got %q want %q", got, BuildResolvConf())
	}
	// Idempotent overwrite.
	if err := WriteResolvConf(path); err != nil {
		t.Fatalf("WriteResolvConf second call: %v", err)
	}
}

func TestWriteResolvConfRejectsEmpty(t *testing.T) {
	t.Parallel()
	if err := WriteResolvConf(""); err == nil {
		t.Fatal("WriteResolvConf(\"\") must fail")
	}
}

func TestWriteResolvConfMissingDir(t *testing.T) {
	t.Parallel()
	if err := WriteResolvConf(filepath.Join(t.TempDir(), "no-such-dir", "resolv.conf")); err == nil {
		t.Fatal("WriteResolvConf must reject missing parent")
	}
}

func TestPodmanDNSFlags(t *testing.T) {
	t.Parallel()
	want := []string{
		"--dns=" + LinkLocalDNSAddr,
		"--dns-search=",
		"--dns-option=ndots:0",
	}
	got := PodmanDNSFlags()
	if len(got) != len(want) {
		t.Fatalf("len = %d want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("flag[%d] = %q want %q", i, got[i], want[i])
		}
	}
}
