package snapshot

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "snapshots.db")
	baseDir := filepath.Join(dir, "data")
	os.MkdirAll(baseDir, 0700)
	s, err := New(dbPath, baseDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, baseDir
}

func TestPutAndGet(t *testing.T) {
	s, _ := tempStore(t)

	rec := Record{
		CapsuleID:    "cap1",
		Tag:          "v1",
		Size:         1024,
		Path:         "/data/cap1/v1",
		Checksum:     "abc123",
		CreatedAt:    time.Now(),
		LastAccessed: time.Now(),
		TTL:          72 * time.Hour,
	}
	if err := s.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get("cap1", "v1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("expected record, got nil")
	}
	if got.CapsuleID != "cap1" || got.Tag != "v1" {
		t.Errorf("got %+v", got)
	}
	if got.Size != 1024 {
		t.Errorf("size = %d, want 1024", got.Size)
	}
	if got.Checksum != "abc123" {
		t.Errorf("checksum = %q, want abc123", got.Checksum)
	}
}

func TestGet_NotFound(t *testing.T) {
	s, _ := tempStore(t)
	got, err := s.Get("nope", "nope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestListByCapsule(t *testing.T) {
	s, _ := tempStore(t)
	now := time.Now()

	s.Put(Record{CapsuleID: "cap1", Tag: "v1", Path: "/a", CreatedAt: now.Add(-2 * time.Hour), LastAccessed: now, TTL: 72 * time.Hour})
	s.Put(Record{CapsuleID: "cap1", Tag: "v2", Path: "/b", CreatedAt: now.Add(-1 * time.Hour), LastAccessed: now, TTL: 72 * time.Hour})
	s.Put(Record{CapsuleID: "cap2", Tag: "v1", Path: "/c", CreatedAt: now, LastAccessed: now, TTL: 72 * time.Hour})

	list, err := s.ListByCapsule("cap1")
	if err != nil {
		t.Fatalf("ListByCapsule: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 records, got %d", len(list))
	}
	// newest first
	if list[0].Tag != "v2" {
		t.Errorf("first should be v2 (newest), got %s", list[0].Tag)
	}
}

func TestDelete(t *testing.T) {
	s, _ := tempStore(t)
	s.Put(Record{CapsuleID: "cap1", Tag: "v1", Path: "/a", CreatedAt: time.Now(), LastAccessed: time.Now(), TTL: 72 * time.Hour})

	if err := s.Delete("cap1", "v1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, _ := s.Get("cap1", "v1")
	if got != nil {
		t.Error("should be deleted")
	}
}

func TestDeleteByCapsule(t *testing.T) {
	s, _ := tempStore(t)
	now := time.Now()
	s.Put(Record{CapsuleID: "cap1", Tag: "v1", Path: "/a", CreatedAt: now, LastAccessed: now, TTL: 72 * time.Hour})
	s.Put(Record{CapsuleID: "cap1", Tag: "v2", Path: "/b", CreatedAt: now, LastAccessed: now, TTL: 72 * time.Hour})

	if err := s.DeleteByCapsule("cap1"); err != nil {
		t.Fatalf("DeleteByCapsule: %v", err)
	}
	list, _ := s.ListByCapsule("cap1")
	if len(list) != 0 {
		t.Errorf("expected 0 records, got %d", len(list))
	}
}

func TestSetInUse(t *testing.T) {
	s, _ := tempStore(t)
	s.Put(Record{CapsuleID: "cap1", Tag: "v1", Path: "/a", CreatedAt: time.Now(), LastAccessed: time.Now(), TTL: 72 * time.Hour})

	if err := s.SetInUse("cap1", "v1", true); err != nil {
		t.Fatalf("SetInUse: %v", err)
	}
	got, _ := s.Get("cap1", "v1")
	if !got.InUse {
		t.Error("should be in use")
	}

	s.SetInUse("cap1", "v1", false)
	got, _ = s.Get("cap1", "v1")
	if got.InUse {
		t.Error("should not be in use")
	}
}

func TestTouchAccess(t *testing.T) {
	s, _ := tempStore(t)
	old := time.Now().Add(-24 * time.Hour)
	s.Put(Record{CapsuleID: "cap1", Tag: "v1", Path: "/a", CreatedAt: old, LastAccessed: old, TTL: 72 * time.Hour})

	if err := s.TouchAccess("cap1", "v1"); err != nil {
		t.Fatalf("TouchAccess: %v", err)
	}
	got, _ := s.Get("cap1", "v1")
	if got.LastAccessed.Before(old.Add(23 * time.Hour)) {
		t.Error("last_accessed should be updated to near-now")
	}
}

func TestEvictExpired(t *testing.T) {
	s, _ := tempStore(t)
	old := time.Now().Add(-48 * time.Hour)
	// TTL 1 second, last accessed 48h ago → expired
	s.Put(Record{CapsuleID: "cap1", Tag: "old", Path: "/a", CreatedAt: old, LastAccessed: old, TTL: 1 * time.Second})
	// TTL 72h, last accessed now → not expired
	s.Put(Record{CapsuleID: "cap1", Tag: "fresh", Path: "/b", CreatedAt: time.Now(), LastAccessed: time.Now(), TTL: 72 * time.Hour})

	evicted, err := s.EvictExpired()
	if err != nil {
		t.Fatalf("EvictExpired: %v", err)
	}
	if len(evicted) != 1 || evicted[0].Tag != "old" {
		t.Errorf("expected 1 evicted (old), got %d: %+v", len(evicted), evicted)
	}

	// Fresh should remain
	got, _ := s.Get("cap1", "fresh")
	if got == nil {
		t.Error("fresh snapshot should still exist")
	}
}

func TestEvictExpired_ProtectsInUse(t *testing.T) {
	s, _ := tempStore(t)
	old := time.Now().Add(-48 * time.Hour)
	s.Put(Record{CapsuleID: "cap1", Tag: "old", Path: "/a", CreatedAt: old, LastAccessed: old, TTL: 1 * time.Second, InUse: true})

	evicted, _ := s.EvictExpired()
	if len(evicted) != 0 {
		t.Error("in-use snapshots should not be evicted")
	}
}

func TestEvictOverCap(t *testing.T) {
	s, _ := tempStore(t)
	now := time.Now()
	s.Put(Record{CapsuleID: "cap1", Tag: "v1", Path: "/a", CreatedAt: now.Add(-3 * time.Hour), LastAccessed: now, TTL: 72 * time.Hour})
	s.Put(Record{CapsuleID: "cap1", Tag: "v2", Path: "/b", CreatedAt: now.Add(-2 * time.Hour), LastAccessed: now, TTL: 72 * time.Hour})
	s.Put(Record{CapsuleID: "cap1", Tag: "v3", Path: "/c", CreatedAt: now.Add(-1 * time.Hour), LastAccessed: now, TTL: 72 * time.Hour})

	evicted, err := s.EvictOverCap("cap1", 2)
	if err != nil {
		t.Fatalf("EvictOverCap: %v", err)
	}
	if len(evicted) != 1 {
		t.Fatalf("expected 1 evicted, got %d", len(evicted))
	}
	if evicted[0].Tag != "v1" {
		t.Errorf("oldest (v1) should be evicted, got %s", evicted[0].Tag)
	}

	remaining, _ := s.ListByCapsule("cap1")
	if len(remaining) != 2 {
		t.Errorf("expected 2 remaining, got %d", len(remaining))
	}
}

func TestSetPinned(t *testing.T) {
	s, _ := tempStore(t)
	now := time.Now()
	s.Put(Record{CapsuleID: "cap1", Tag: "v1", Path: "/a", CreatedAt: now, LastAccessed: now, TTL: 72 * time.Hour})

	if err := s.SetPinned("cap1", "v1", true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}
	got, _ := s.Get("cap1", "v1")
	if got == nil || !got.Pinned {
		t.Fatalf("record should be pinned, got %+v", got)
	}

	s.SetPinned("cap1", "v1", false)
	got, _ = s.Get("cap1", "v1")
	if got.Pinned {
		t.Error("record should not be pinned after clear")
	}
}

// TestEvictOverCap_ProtectsPinnedStandby proves a pinned standby replica
// survives over-cap eviction so a holder cannot silently drop below K
// (plan part 7).
func TestEvictOverCap_ProtectsPinnedStandby(t *testing.T) {
	s, _ := tempStore(t)
	now := time.Now()
	// Three snapshots; the OLDEST is a pinned standby. With cap=2 the oldest
	// would normally be evicted — but pinning protects it.
	s.Put(Record{CapsuleID: "cap1", Tag: "standby", Path: "/a", CreatedAt: now.Add(-3 * time.Hour), LastAccessed: now, TTL: 72 * time.Hour, Pinned: true})
	s.Put(Record{CapsuleID: "cap1", Tag: "v2", Path: "/b", CreatedAt: now.Add(-2 * time.Hour), LastAccessed: now, TTL: 72 * time.Hour})
	s.Put(Record{CapsuleID: "cap1", Tag: "v3", Path: "/c", CreatedAt: now.Add(-1 * time.Hour), LastAccessed: now, TTL: 72 * time.Hour})

	evicted, err := s.EvictOverCap("cap1", 2)
	if err != nil {
		t.Fatalf("EvictOverCap: %v", err)
	}
	for _, e := range evicted {
		if e.Tag == "standby" {
			t.Fatal("pinned standby must not be over-cap evicted")
		}
	}
	if got, _ := s.Get("cap1", "standby"); got == nil {
		t.Fatal("pinned standby should still exist after over-cap eviction")
	}
	// The two unprotected snapshots stay; pinned is extra on top of the cap.
	all, _ := s.ListByCapsule("cap1")
	if len(all) != 3 {
		t.Errorf("expected 3 records (pinned + cap of 2), got %d", len(all))
	}
}

// TestEvictExpired_ReclaimsPinnedAtTTL confirms pinning protects only from
// over-cap eviction, not from TTL expiry — so disk stays bounded.
func TestEvictExpired_ReclaimsPinnedAtTTL(t *testing.T) {
	s, _ := tempStore(t)
	old := time.Now().Add(-48 * time.Hour)
	s.Put(Record{CapsuleID: "cap1", Tag: "old", Path: "/a", CreatedAt: old, LastAccessed: old, TTL: 1 * time.Second, Pinned: true})

	evicted, err := s.EvictExpired()
	if err != nil {
		t.Fatalf("EvictExpired: %v", err)
	}
	if len(evicted) != 1 || evicted[0].Tag != "old" {
		t.Errorf("pinned-but-TTL-expired standby should still be reclaimed, got %+v", evicted)
	}
}

func TestSnapshotPath(t *testing.T) {
	s, baseDir := tempStore(t)
	got := s.SnapshotPath("cap1", "v1")
	want := filepath.Join(baseDir, "cap1", "v1")
	if got != want {
		t.Errorf("SnapshotPath = %q, want %q", got, want)
	}
}

func TestEnsureAndRemoveDir(t *testing.T) {
	s, _ := tempStore(t)

	if err := s.EnsureDir("cap1", "v1"); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	path := s.SnapshotPath("cap1", "v1")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("directory should exist after EnsureDir")
	}

	if err := s.RemoveDir("cap1", "v1"); err != nil {
		t.Fatalf("RemoveDir: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("directory should not exist after RemoveDir")
	}
}

func TestPut_Upsert(t *testing.T) {
	s, _ := tempStore(t)
	now := time.Now()
	s.Put(Record{CapsuleID: "cap1", Tag: "v1", Path: "/a", Size: 100, CreatedAt: now, LastAccessed: now, TTL: 72 * time.Hour})
	s.Put(Record{CapsuleID: "cap1", Tag: "v1", Path: "/a", Size: 200, CreatedAt: now, LastAccessed: now, TTL: 72 * time.Hour})

	got, _ := s.Get("cap1", "v1")
	if got.Size != 200 {
		t.Errorf("upsert should update size to 200, got %d", got.Size)
	}
}
