package bridge

import (
	"database/sql"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// newTestDB opens a fresh SQLite database under t.TempDir(). The DB is
// closed automatically via t.Cleanup. Returns both the DB and the path so
// tests can reopen it.
func newTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge.sqlite")
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

// mustCIDR is a helper for inline CIDR literals in tests.
func mustCIDR(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(cidr)
	require.NoError(t, err)
	return n
}

func TestAllocator_AllocateFresh(t *testing.T) {
	db, _ := newTestDB(t)
	a, err := OpenAllocator(db)
	require.NoError(t, err)

	alloc, err := a.Allocate("group-a")
	require.NoError(t, err)
	require.Equal(t, "10.88.0.0/24", alloc.Subnet.String())
	require.Equal(t, "10.88.0.1", alloc.Gateway.String())
	require.Equal(t, "group-a", alloc.GroupID)
	require.False(t, alloc.CreatedAt.IsZero())
}

func TestAllocator_IdempotentAllocate(t *testing.T) {
	db, _ := newTestDB(t)
	a, err := OpenAllocator(db)
	require.NoError(t, err)

	first, err := a.Allocate("group-a")
	require.NoError(t, err)
	second, err := a.Allocate("group-a")
	require.NoError(t, err)
	require.Equal(t, first.Subnet.String(), second.Subnet.String())
	require.Equal(t, first.Gateway.String(), second.Gateway.String())

	reserved, err := a.Reserved()
	require.NoError(t, err)
	require.Len(t, reserved, 1)
}

func TestAllocator_AllocateMultipleGroups(t *testing.T) {
	db, _ := newTestDB(t)
	a, err := OpenAllocator(db)
	require.NoError(t, err)

	want := []string{"10.88.0.0/24", "10.88.1.0/24", "10.88.2.0/24"}
	for i, gid := range []string{"g0", "g1", "g2"} {
		alloc, err := a.Allocate(gid)
		require.NoError(t, err)
		require.Equal(t, want[i], alloc.Subnet.String())
	}

	reserved, err := a.Reserved()
	require.NoError(t, err)
	require.Len(t, reserved, 3)
	for i, alloc := range reserved {
		require.Equal(t, want[i], alloc.Subnet.String())
	}
}

func TestAllocator_ReleaseAndReuse(t *testing.T) {
	db, _ := newTestDB(t)
	a, err := OpenAllocator(db)
	require.NoError(t, err)

	a0, err := a.Allocate("g0") // 10.88.0.0/24
	require.NoError(t, err)
	a1, err := a.Allocate("g1") // 10.88.1.0/24
	require.NoError(t, err)
	require.Equal(t, "10.88.0.0/24", a0.Subnet.String())
	require.Equal(t, "10.88.1.0/24", a1.Subnet.String())

	require.NoError(t, a.Release("g0"))

	// Lowest-numbered free wins → reuses 10.88.0.0/24.
	a2, err := a.Allocate("g2")
	require.NoError(t, err)
	require.Equal(t, "10.88.0.0/24", a2.Subnet.String())
}

func TestAllocator_ReleaseUnknownIsSuccess(t *testing.T) {
	db, _ := newTestDB(t)
	a, err := OpenAllocator(db)
	require.NoError(t, err)
	require.NoError(t, a.Release("never-allocated"))
}

func TestAllocator_PoolExhaustion(t *testing.T) {
	db, _ := newTestDB(t)
	// /22 carved into /24s → 4 slots.
	a, err := OpenAllocator(db, WithPool(mustCIDR(t, "10.88.0.0/22")), WithPrefixBits(24))
	require.NoError(t, err)

	for i := 0; i < 4; i++ {
		_, err := a.Allocate(fmt.Sprintf("g%d", i))
		require.NoError(t, err)
	}
	_, err = a.Allocate("g4")
	require.ErrorIs(t, err, ErrPoolExhausted)
}

func TestAllocator_InvalidPool(t *testing.T) {
	db, _ := newTestDB(t)
	// /28 cannot host /24 children.
	_, err := OpenAllocator(db, WithPool(mustCIDR(t, "10.88.0.0/28")), WithPrefixBits(24))
	require.ErrorIs(t, err, ErrInvalidPool)
}

func TestAllocator_PersistsAcrossReopen(t *testing.T) {
	db1, path := newTestDB(t)
	a1, err := OpenAllocator(db1)
	require.NoError(t, err)
	_, err = a1.Allocate("g-keep-0")
	require.NoError(t, err)
	_, err = a1.Allocate("g-keep-1")
	require.NoError(t, err)
	require.NoError(t, db1.Close())

	db2, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db2.Close() })

	a2, err := OpenAllocator(db2)
	require.NoError(t, err)
	reserved, err := a2.Reserved()
	require.NoError(t, err)
	require.Len(t, reserved, 2)
	require.Equal(t, "10.88.0.0/24", reserved[0].Subnet.String())
	require.Equal(t, "g-keep-0", reserved[0].GroupID)
	require.Equal(t, "10.88.1.0/24", reserved[1].Subnet.String())
	require.Equal(t, "g-keep-1", reserved[1].GroupID)
}

func TestAllocator_ConcurrentAllocate(t *testing.T) {
	db, _ := newTestDB(t)
	a, err := OpenAllocator(db)
	require.NoError(t, err)

	const N = 10
	var wg sync.WaitGroup
	results := make([]Allocation, N)
	var errCount atomic.Int32
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			alloc, err := a.Allocate(fmt.Sprintf("g%d", idx))
			if err != nil {
				errCount.Add(1)
				return
			}
			results[idx] = alloc
		}(i)
	}
	wg.Wait()
	require.Zero(t, errCount.Load())

	seen := make(map[string]struct{}, N)
	for _, alloc := range results {
		require.NotNil(t, alloc.Subnet)
		require.NotContains(t, seen, alloc.Subnet.String())
		seen[alloc.Subnet.String()] = struct{}{}
	}
	require.Len(t, seen, N)

	reserved, err := a.Reserved()
	require.NoError(t, err)
	require.Len(t, reserved, N)
}

// TestAllocator_GetMissing covers the Get-without-row path so the
// signature is exercised under -race alongside the other tests.
func TestAllocator_GetMissing(t *testing.T) {
	db, _ := newTestDB(t)
	a, err := OpenAllocator(db)
	require.NoError(t, err)

	_, ok, err := a.Get("never-allocated")
	require.NoError(t, err)
	require.False(t, ok)

	_, err = a.Allocate("present")
	require.NoError(t, err)
	got, ok, err := a.Get("present")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "10.88.0.0/24", got.Subnet.String())
}
