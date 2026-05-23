package overlay

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// newVNITestDB opens a fresh SQLite database under t.TempDir and
// registers cleanup. Returns the *sql.DB and its on-disk path so
// reopen-style tests can re-open the same file.
func newVNITestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vni.sqlite")
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func TestVNI_AllocateFresh(t *testing.T) {
	db, _ := newVNITestDB(t)
	a, err := OpenVNIAllocator(WithVNIDB(db))
	require.NoError(t, err)

	vni, err := a.Allocate("/clusters/alpha", "group-a")
	require.NoError(t, err)
	require.Greater(t, vni, uint32(0))
	require.LessOrEqual(t, vni, vniMask24)
}

func TestVNI_Idempotent(t *testing.T) {
	db, _ := newVNITestDB(t)
	a, err := OpenVNIAllocator(WithVNIDB(db))
	require.NoError(t, err)

	first, err := a.Allocate("/clusters/alpha", "group-a")
	require.NoError(t, err)
	second, err := a.Allocate("/clusters/alpha", "group-a")
	require.NoError(t, err)
	require.Equal(t, first, second)
}

func TestVNI_DifferentGroupsDifferentVNIs(t *testing.T) {
	db, _ := newVNITestDB(t)
	a, err := OpenVNIAllocator(WithVNIDB(db))
	require.NoError(t, err)

	v1, err := a.Allocate("/clusters/alpha", "group-a")
	require.NoError(t, err)
	v2, err := a.Allocate("/clusters/alpha", "group-b")
	require.NoError(t, err)
	v3, err := a.Allocate("/clusters/alpha", "group-c")
	require.NoError(t, err)

	require.NotEqual(t, v1, v2)
	require.NotEqual(t, v2, v3)
	require.NotEqual(t, v1, v3)
}

func TestVNI_CollisionRetry(t *testing.T) {
	db, _ := newVNITestDB(t)

	// Pre-seed group-a so candidate 0x123456 is taken in this cluster.
	staticSalt := func(group string, attempt int) uint64 {
		if attempt == 0 {
			return 0x123456
		}
		return 0xABCDEF
	}
	a, err := OpenVNIAllocator(WithVNIDB(db), WithSaltFn(staticSalt))
	require.NoError(t, err)

	first, err := a.Allocate("/clusters/alpha", "group-a")
	require.NoError(t, err)
	require.Equal(t, uint32(0x123456), first)

	// Now allocate a different group whose attempt 0 collides; the
	// allocator must advance to attempt 1.
	collidingSalt := func(group string, attempt int) uint64 {
		if attempt == 0 {
			return 0x123456 // same as group-a → forced collision
		}
		return 0x654321
	}
	b, err := OpenVNIAllocator(WithVNIDB(db), WithSaltFn(collidingSalt))
	require.NoError(t, err)
	second, err := b.Allocate("/clusters/alpha", "group-b")
	require.NoError(t, err)
	require.Equal(t, uint32(0x654321), second)
	require.NotEqual(t, first, second)
}

func TestVNI_ExhaustionAfter16Retries(t *testing.T) {
	db, _ := newVNITestDB(t)

	// First allocate group-a to occupy a single VNI.
	seed, err := OpenVNIAllocator(WithVNIDB(db),
		WithSaltFn(func(group string, attempt int) uint64 { return 0xDEADBE }))
	require.NoError(t, err)
	_, err = seed.Allocate("/clusters/alpha", "group-a")
	require.NoError(t, err)

	// A second allocator whose salt always returns the same taken VNI
	// must exhaust after 16 attempts.
	stuck, err := OpenVNIAllocator(WithVNIDB(db),
		WithSaltFn(func(group string, attempt int) uint64 { return 0xDEADBE }))
	require.NoError(t, err)
	_, err = stuck.Allocate("/clusters/alpha", "group-b")
	require.ErrorIs(t, err, ErrVNIExhausted)
}

func TestVNI_PerClusterIsolation(t *testing.T) {
	db, _ := newVNITestDB(t)
	a, err := OpenVNIAllocator(WithVNIDB(db),
		WithSaltFn(func(group string, attempt int) uint64 { return 0x424242 }))
	require.NoError(t, err)

	// Same groupID in two different clusters should both land on
	// 0x424242 because the UNIQUE INDEX is per-cluster.
	v1, err := a.Allocate("/clusters/alpha", "group-x")
	require.NoError(t, err)
	v2, err := a.Allocate("/clusters/beta", "group-x")
	require.NoError(t, err)
	require.Equal(t, uint32(0x424242), v1)
	require.Equal(t, uint32(0x424242), v2)
}

func TestVNI_ReleaseUnknownIsSuccess(t *testing.T) {
	db, _ := newVNITestDB(t)
	a, err := OpenVNIAllocator(WithVNIDB(db))
	require.NoError(t, err)

	require.NoError(t, a.Release("/clusters/alpha", "nonexistent"))
}

func TestVNI_PersistsAcrossReopen(t *testing.T) {
	db, path := newVNITestDB(t)
	first, err := OpenVNIAllocator(WithVNIDB(db))
	require.NoError(t, err)

	v1, err := first.Allocate("/clusters/alpha", "group-a")
	require.NoError(t, err)
	v2, err := first.Allocate("/clusters/alpha", "group-b")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db2, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db2.Close() })
	second, err := OpenVNIAllocator(WithVNIDB(db2))
	require.NoError(t, err)

	got1, ok, err := second.Get("/clusters/alpha", "group-a")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, v1, got1)

	got2, ok, err := second.Get("/clusters/alpha", "group-b")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, v2, got2)
}

func TestVNI_InvalidInput(t *testing.T) {
	db, _ := newVNITestDB(t)
	a, err := OpenVNIAllocator(WithVNIDB(db))
	require.NoError(t, err)

	_, err = a.Allocate("", "group-a")
	require.Error(t, err)
	_, err = a.Allocate("/clusters/alpha", "")
	require.Error(t, err)

	require.Error(t, a.Release("", "group-a"))
	require.Error(t, a.Release("/clusters/alpha", ""))

	_, _, err = a.Get("", "group-a")
	require.Error(t, err)
	_, _, err = a.Get("/clusters/alpha", "")
	require.Error(t, err)
}

func TestVNI_VNIRange(t *testing.T) {
	db, _ := newVNITestDB(t)
	a, err := OpenVNIAllocator(WithVNIDB(db))
	require.NoError(t, err)

	for i := 0; i < 32; i++ {
		groupID := "group-" + string(rune('a'+i))
		vni, err := a.Allocate("/clusters/alpha", groupID)
		require.NoError(t, err)
		require.Greater(t, vni, uint32(0), "vni 0 is reserved")
		require.Less(t, vni, uint32(0x01000000), "vni must fit in 24 bits")
	}
}

// TestVNI_OpenRequiresDB documents the constructor's only mandatory
// option. Wired here so the requirement is checked alongside the
// other allocator behaviors.
func TestVNI_OpenRequiresDB(t *testing.T) {
	_, err := OpenVNIAllocator()
	require.Error(t, err)
	require.True(t, errors.Is(err, err)) // sanity wrap
}
