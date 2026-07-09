package auth

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"go.uber.org/zap/zaptest"

	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/phonebook"
)

// deterministicPeer builds a valid, stable peer ID from a fixed ed25519 seed.
func deterministicPeer(seed byte) peer.ID {
	raw := make([]byte, ed25519.SeedSize)
	for i := range raw {
		raw[i] = seed
	}
	priv := ed25519.NewKeyFromSeed(raw)
	pk, err := crypto.UnmarshalEd25519PrivateKey(priv)
	if err != nil {
		panic("failed to build test key: " + err.Error())
	}
	id, err := peer.IDFromPrivateKey(pk)
	if err != nil {
		panic("failed to derive test peer id: " + err.Error())
	}
	return id
}

// fakeReauthenticator records Authenticate calls and returns programmed errors
// per target peer.
type fakeReauthenticator struct {
	mu        sync.Mutex
	authCalls []peer.ID
	failFor   map[peer.ID]error
	session   *Session
}

func (f *fakeReauthenticator) GetSession(string) (*Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.session == nil {
		return nil, ErrSessionNotFound
	}
	return f.session, nil
}

func (f *fakeReauthenticator) RefreshSession(string) {}

func (f *fakeReauthenticator) Authenticate(_ context.Context, _ string, target peer.ID) (*Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authCalls = append(f.authCalls, target)
	if err, ok := f.failFor[target]; ok {
		return nil, err
	}
	return &Session{Status: SessionStatusEnum.Authenticated()}, nil
}

func (f *fakeReauthenticator) calls() []peer.ID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]peer.ID(nil), f.authCalls...)
}

// stubPhonebook returns a fixed best-peers list; all other methods are no-ops.
type stubPhonebook struct {
	best []*phonebook.Entry
}

func (s *stubPhonebook) GetBestPeers(string, int) ([]*phonebook.Entry, error) {
	return s.best, nil
}
func (s *stubPhonebook) Get(string, string) (*phonebook.Entry, error) {
	return nil, errors.New("unused")
}
func (s *stubPhonebook) Exists(string, string) (bool, error)                  { return false, nil }
func (s *stubPhonebook) GetByCluster(string) ([]*phonebook.Entry, error)      { return nil, nil }
func (s *stubPhonebook) GetByNode(string) ([]*phonebook.Entry, error)         { return nil, nil }
func (s *stubPhonebook) Add(*phonebook.Entry) error                           { return nil }
func (s *stubPhonebook) Update(*phonebook.Entry) error                        { return nil }
func (s *stubPhonebook) Remove(string, string) error                          { return nil }
func (s *stubPhonebook) RemoveAllForNode(string) error                        { return nil }
func (s *stubPhonebook) RecordConnectionAttempt(string, string, bool) error   { return nil }
func (s *stubPhonebook) RecordDisconnect(string, string) error                { return nil }
func (s *stubPhonebook) SetReliabilityScore(string, string, float64) error    { return nil }
func (s *stubPhonebook) SetStatus(string, string, phonebook.NodeStatus) error { return nil }
func (s *stubPhonebook) GetByStatus(string, phonebook.NodeStatus) ([]*phonebook.Entry, error) {
	return nil, nil
}
func (s *stubPhonebook) RecordProbe(string, string, bool) error { return nil }
func (s *stubPhonebook) Prune(time.Duration, int) (int, error)  { return 0, nil }
func (s *stubPhonebook) Count() (int, error)                    { return 0, nil }
func (s *stubPhonebook) CountByCluster(string) (int, error)     { return 0, nil }
func (s *stubPhonebook) Close() error                           { return nil }

func newSubscriberForTest(t *testing.T, auth Reauthenticator, pb phonebook.IPhonebook) (*ReauthSubscriber, events.Bus) {
	t.Helper()
	bus := events.NewBus()
	r := NewReauthSubscriber(
		WithReauthAuthenticator(auth),
		WithReauthPhonebook(pb),
		WithReauthEventBus(bus),
		WithReauthLogger(zaptest.NewLogger(t)),
	)
	if err := r.Start(); err != nil {
		t.Fatalf("start subscriber: %v", err)
	}
	return r, bus
}

// TestReauthWithPeer_TargetsSpecificPeer verifies the subscriber, on
// ReauthWithPeerRequested, authenticates against exactly the requested peer.
func TestReauthWithPeer_TargetsSpecificPeer(t *testing.T) {
	target := deterministicPeer(0x31)
	auth := &fakeReauthenticator{}
	pb := &stubPhonebook{}

	r, bus := newSubscriberForTest(t, auth, pb)
	defer func() { r.Stop(); bus.Close() }()

	syncCh := bus.Subscribe(events.TypeSyncRequested)

	bus.Publish(events.ReauthWithPeerRequested{
		BaseEvent:   events.NewBaseEvent(),
		ClusterPath: "c/dc/prod",
		PeerID:      target.String(),
	})

	// Expect a sync request as the success signal.
	select {
	case ev := <-syncCh:
		sr, ok := ev.(events.SyncRequested)
		if !ok {
			t.Fatalf("unexpected event %T", ev)
		}
		if sr.PreferredPeer != target.String() {
			t.Fatalf("expected sync preferred peer %s, got %s", target, sr.PreferredPeer)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for post-reauth sync")
	}

	calls := auth.calls()
	if len(calls) != 1 || calls[0] != target {
		t.Fatalf("expected exactly one Authenticate to %s, got %v", target, calls)
	}
}

// TestReauthWithPeer_FallsBackWhenPinnedPeerFails verifies that when the pinned
// peer refuses, the subscriber falls back to another phonebook peer, excluding
// the one it already tried.
func TestReauthWithPeer_FallsBackWhenPinnedPeerFails(t *testing.T) {
	pinned := deterministicPeer(0x41)
	alt := deterministicPeer(0x42)

	auth := &fakeReauthenticator{
		failFor: map[peer.ID]error{pinned: errors.New("refused")},
	}
	pb := &stubPhonebook{
		best: []*phonebook.Entry{
			{NodeID: pinned.String()},
			{NodeID: alt.String()},
		},
	}

	r, bus := newSubscriberForTest(t, auth, pb)
	defer func() { r.Stop(); bus.Close() }()

	syncCh := bus.Subscribe(events.TypeSyncRequested)

	bus.Publish(events.ReauthWithPeerRequested{
		BaseEvent:   events.NewBaseEvent(),
		ClusterPath: "c/dc/prod",
		PeerID:      pinned.String(),
	})

	select {
	case ev := <-syncCh:
		sr := ev.(events.SyncRequested)
		if sr.PreferredPeer != alt.String() {
			t.Fatalf("expected fallback to alt peer %s, got %s", alt, sr.PreferredPeer)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for fallback sync")
	}

	calls := auth.calls()
	if len(calls) != 2 || calls[0] != pinned || calls[1] != alt {
		t.Fatalf("expected pinned-then-alt auth order, got %v", calls)
	}
}
