package metrics

import (
	"path/filepath"
	"testing"
	"time"
)

// makeSnapshot returns a deterministic Snapshot used by store tests.
func makeSnapshot(at time.Time) Snapshot {
	return Snapshot{
		CapturedAt: at,
		CPU:        CPUStats{Cores: 8, UsedPercent: 25.5},
		Memory:     MemoryStats{TotalMB: 16384, AvailableMB: 12000, UsedMB: 4384, UsedPercent: 26.7},
		Disk:       DiskStats{TotalMB: 100000, FreeMB: 75000, UsedMB: 25000, UsedPercent: 25.0},
		Load:       LoadStats{One: 0.5, Five: 0.7, Fifteen: 0.9},
		Network:    NetworkStats{BytesSentPerSec: 10000, BytesRecvPerSec: 20000},
	}
}

func TestStore_OpenAndMigrate(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "metrics.db"))
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()
}

func TestStore_LocalLatestEmpty(t *testing.T) {
	store, _ := OpenStore(filepath.Join(t.TempDir(), "metrics.db"))
	defer store.Close()

	snap, err := store.LocalLatest()
	if err != nil {
		t.Fatalf("LocalLatest failed: %v", err)
	}
	if !snap.CapturedAt.IsZero() {
		t.Errorf("expected zero snapshot, got %+v", snap)
	}
}

func TestStore_InsertAndLatest(t *testing.T) {
	store, _ := OpenStore(filepath.Join(t.TempDir(), "metrics.db"))
	defer store.Close()

	now := time.Now().Truncate(time.Millisecond)
	in := makeSnapshot(now)

	if err := store.InsertLocal(in); err != nil {
		t.Fatalf("InsertLocal failed: %v", err)
	}

	out, err := store.LocalLatest()
	if err != nil {
		t.Fatalf("LocalLatest failed: %v", err)
	}
	if !out.CapturedAt.Equal(now) {
		t.Errorf("CapturedAt: got %v, want %v", out.CapturedAt, now)
	}
	if out.CPU.Cores != in.CPU.Cores {
		t.Errorf("CPU cores: got %d, want %d", out.CPU.Cores, in.CPU.Cores)
	}
	if out.Memory.UsedPercent != in.Memory.UsedPercent {
		t.Errorf("memory used pct mismatch")
	}
}

func TestStore_RollingWindowCapsAtHistoryDepth(t *testing.T) {
	store, _ := OpenStore(filepath.Join(t.TempDir(), "metrics.db"))
	defer store.Close()

	base := time.Now().Truncate(time.Millisecond)
	// Insert HistoryDepth + 5 snapshots — only the most recent 10 should survive.
	for i := 0; i < HistoryDepth+5; i++ {
		snap := makeSnapshot(base.Add(time.Duration(i) * time.Second))
		if err := store.InsertLocal(snap); err != nil {
			t.Fatalf("InsertLocal[%d] failed: %v", i, err)
		}
	}

	history, err := store.LocalHistory()
	if err != nil {
		t.Fatalf("LocalHistory failed: %v", err)
	}
	if len(history) != HistoryDepth {
		t.Errorf("expected %d records, got %d", HistoryDepth, len(history))
	}
	// History is in chronological order; the oldest should be the
	// 6th insertion (i=5) since 0-4 were trimmed.
	wantOldest := base.Add(5 * time.Second)
	if !history[0].CapturedAt.Equal(wantOldest) {
		t.Errorf("oldest survivor: got %v, want %v", history[0].CapturedAt, wantOldest)
	}
}

func TestStore_PeerUpsertReplacesPriorValue(t *testing.T) {
	store, _ := OpenStore(filepath.Join(t.TempDir(), "metrics.db"))
	defer store.Close()

	snap := makeSnapshot(time.Now().Truncate(time.Millisecond))
	snap.NodeID = "peer-1"

	if err := store.UpsertPeer(snap); err != nil {
		t.Fatalf("first UpsertPeer failed: %v", err)
	}

	// Update the peer with a different CPU usage.
	snap.CPU.UsedPercent = 80
	snap.CapturedAt = snap.CapturedAt.Add(1 * time.Second)
	if err := store.UpsertPeer(snap); err != nil {
		t.Fatalf("second UpsertPeer failed: %v", err)
	}

	got, err := store.PeerLatest("peer-1")
	if err != nil {
		t.Fatalf("PeerLatest failed: %v", err)
	}
	if got.NodeID != "peer-1" {
		t.Errorf("NodeID: got %q", got.NodeID)
	}
	if got.CPU.UsedPercent != 80 {
		t.Errorf("expected updated CPU 80, got %v", got.CPU.UsedPercent)
	}
}

func TestStore_PeerLatestMissing(t *testing.T) {
	store, _ := OpenStore(filepath.Join(t.TempDir(), "metrics.db"))
	defer store.Close()

	snap, err := store.PeerLatest("nobody")
	if err != nil {
		t.Fatalf("PeerLatest failed: %v", err)
	}
	if !snap.CapturedAt.IsZero() {
		t.Errorf("missing peer should return zero snapshot, got %+v", snap)
	}
}

func TestStore_AllPeers(t *testing.T) {
	store, _ := OpenStore(filepath.Join(t.TempDir(), "metrics.db"))
	defer store.Close()

	for _, id := range []string{"a", "b", "c"} {
		snap := makeSnapshot(time.Now().Truncate(time.Millisecond))
		snap.NodeID = id
		if err := store.UpsertPeer(snap); err != nil {
			t.Fatalf("UpsertPeer %s: %v", id, err)
		}
	}

	all, err := store.AllPeers()
	if err != nil {
		t.Fatalf("AllPeers failed: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 peers, got %d", len(all))
	}
	seen := map[string]bool{}
	for _, s := range all {
		seen[s.NodeID] = true
	}
	for _, id := range []string{"a", "b", "c"} {
		if !seen[id] {
			t.Errorf("missing peer %s", id)
		}
	}
}

func TestStore_UpsertPeerRequiresNodeID(t *testing.T) {
	store, _ := OpenStore(filepath.Join(t.TempDir(), "metrics.db"))
	defer store.Close()

	if err := store.UpsertPeer(Snapshot{}); err == nil {
		t.Error("expected error for empty NodeID")
	}
}

func TestStore_ReopenPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.db")

	store, _ := OpenStore(path)
	in := makeSnapshot(time.Now().Truncate(time.Millisecond))
	if err := store.InsertLocal(in); err != nil {
		t.Fatalf("insert failed: %v", err)
	}
	store.Close()

	store2, _ := OpenStore(path)
	defer store2.Close()
	out, err := store2.LocalLatest()
	if err != nil {
		t.Fatalf("LocalLatest after reopen: %v", err)
	}
	if out.CapturedAt.IsZero() {
		t.Error("data did not persist across reopen")
	}
}
