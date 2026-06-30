package node

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tareksalem/falak/snapshot"
)

// TestElectionSnapshotLookup_AgeAndTagDerivation exercises the node-side
// gravity.SnapshotLookup adapter against a real snapshot.Store with an
// injected clock. It proves three things the O10 wiring depends on:
//
//   - a held snapshot is reported present with the correct TTL,
//   - its age is derived from CreatedAt against the injectable clock,
//   - the (capsuleID, tag) key matches what the runtime restore path uses
//     (ImageDigest, here a digest), so a snapshot the runtime would restore
//     from is the same one the factor credits.
func TestElectionSnapshotLookup_AgeAndTagDerivation(t *testing.T) {
	dir := t.TempDir()
	store, err := snapshot.New(filepath.Join(dir, "snapshots.db"), filepath.Join(dir, "snap"))
	if err != nil {
		t.Fatalf("open snapshot store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		capsuleID = "cap-123"
		tag       = "sha256:deadbeef"
		ttl       = 72 * time.Hour
	)
	created := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := store.Put(snapshot.Record{
		CapsuleID:    capsuleID,
		Tag:          tag,
		Checksum:     "abc",
		Path:         filepath.Join(dir, "snap", "x"),
		Size:         1,
		TTL:          ttl,
		CreatedAt:    created,
		LastAccessed: created,
	}); err != nil {
		t.Fatalf("put record: %v", err)
	}

	// Clock fixed 12h after creation → expected age 12h.
	now := created.Add(12 * time.Hour)
	lookup := &electionSnapshotLookup{store: store, now: func() time.Time { return now }}

	info, ok := lookup.LocalSnapshot(capsuleID, tag)
	if !ok {
		t.Fatalf("expected snapshot to be reported present")
	}
	if info.Age != 12*time.Hour {
		t.Fatalf("age: got %s want 12h", info.Age)
	}
	if info.TTL != ttl {
		t.Fatalf("ttl: got %s want %s", info.TTL, ttl)
	}

	// Unknown tag → absent (tag is part of the key).
	if _, ok := lookup.LocalSnapshot(capsuleID, "sha256:other"); ok {
		t.Fatalf("snapshot must be absent for a different tag")
	}

	// Future CreatedAt (clock skew) must clamp age to zero, never negative.
	skewed := &electionSnapshotLookup{store: store, now: func() time.Time { return created.Add(-time.Hour) }}
	info, ok = skewed.LocalSnapshot(capsuleID, tag)
	if !ok {
		t.Fatalf("expected snapshot present under skewed clock")
	}
	if info.Age != 0 {
		t.Fatalf("clock skew age must clamp to 0, got %s", info.Age)
	}
}

// TestElectionSnapshotLookup_NilStore verifies the adapter degrades safely
// to "no snapshot" when no store is wired, so the gravity factor scores 0
// rather than panicking.
func TestElectionSnapshotLookup_NilStore(t *testing.T) {
	lookup := &electionSnapshotLookup{store: nil}
	if _, ok := lookup.LocalSnapshot("cap", "tag"); ok {
		t.Fatalf("nil store must report no snapshot")
	}
}
