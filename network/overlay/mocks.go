package overlay

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// MockDeviceOp records one DeviceManager call. Exported so tests in
// other files (peers_test.go, future 11A.14 integration tests) can
// assert against the recorded sequence.
type MockDeviceOp struct {
	Kind        string // "create" | "destroy" | "fdb_add" | "fdb_remove"
	BridgeName  string
	DeviceName  string
	PeerExtIP   string
	VNI         uint32
	LocalIP     string
	Port        int
	MTU         int
}

// MockDeviceManager is an in-memory DeviceManager used by tests. It
// tracks every call in Ops, simulates name allocation, and lets tests
// inject failures via the FailNext function.
type MockDeviceManager struct {
	mu       sync.Mutex
	Ops      []MockDeviceOp
	devices  map[string]bool          // devName -> exists
	fdb      map[string]map[string]bool
	FailNext func(kind string) error // optional injector
}

// NewMockDeviceManager constructs an empty MockDeviceManager.
func NewMockDeviceManager() *MockDeviceManager {
	return &MockDeviceManager{devices: map[string]bool{}, fdb: map[string]map[string]bool{}}
}

// Create records the call and returns a deterministic device name
// derived from the VNI (matches the production naming convention).
func (m *MockDeviceManager) Create(ctx context.Context, bridgeName string, vni uint32, localIP string, port int, mtu int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailNext != nil {
		if err := m.FailNext("create"); err != nil {
			return "", err
		}
	}
	name := fmt.Sprintf("mock-vx-%x", vni)
	m.devices[name] = true
	m.fdb[name] = map[string]bool{}
	m.Ops = append(m.Ops, MockDeviceOp{
		Kind: "create", BridgeName: bridgeName, DeviceName: name,
		VNI: vni, LocalIP: localIP, Port: port, MTU: mtu,
	})
	return name, nil
}

// Destroy records the call. Missing device is success.
func (m *MockDeviceManager) Destroy(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailNext != nil {
		if err := m.FailNext("destroy"); err != nil {
			return err
		}
	}
	delete(m.devices, name)
	delete(m.fdb, name)
	m.Ops = append(m.Ops, MockDeviceOp{Kind: "destroy", DeviceName: name})
	return nil
}

// AddFDBEntry records the call and tracks the entry.
func (m *MockDeviceManager) AddFDBEntry(ctx context.Context, devName, peerExternalIP string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailNext != nil {
		if err := m.FailNext("fdb_add"); err != nil {
			return err
		}
	}
	if !m.devices[devName] {
		return fmt.Errorf("%w: %s", ErrDeviceNotFound, devName)
	}
	if m.fdb[devName] == nil {
		m.fdb[devName] = map[string]bool{}
	}
	m.fdb[devName][peerExternalIP] = true
	m.Ops = append(m.Ops, MockDeviceOp{Kind: "fdb_add", DeviceName: devName, PeerExtIP: peerExternalIP})
	return nil
}

// RemoveFDBEntry records the call. Missing entry is success.
func (m *MockDeviceManager) RemoveFDBEntry(ctx context.Context, devName, peerExternalIP string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailNext != nil {
		if err := m.FailNext("fdb_remove"); err != nil {
			return err
		}
	}
	if m.fdb[devName] != nil {
		delete(m.fdb[devName], peerExternalIP)
	}
	m.Ops = append(m.Ops, MockDeviceOp{Kind: "fdb_remove", DeviceName: devName, PeerExtIP: peerExternalIP})
	return nil
}

// HasDevice returns whether the named device is currently recorded
// as existing. Useful for test assertions.
func (m *MockDeviceManager) HasDevice(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.devices[name]
}

// HasFDBEntry returns whether (devName, peerIP) is currently recorded.
func (m *MockDeviceManager) HasFDBEntry(devName, peerIP string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fdb[devName] != nil && m.fdb[devName][peerIP]
}

// OpsByKind returns every recorded op with the given kind in order.
func (m *MockDeviceManager) OpsByKind(kind string) []MockDeviceOp {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MockDeviceOp, 0)
	for _, o := range m.Ops {
		if o.Kind == kind {
			out = append(out, o)
		}
	}
	return out
}

// MockIPsecOp records one IPsecManager call. Exported so peer-manager
// tests can assert the full install/remove sequence.
type MockIPsecOp struct {
	Kind  string // "install" | "remove" | "probe"
	Local string
	Peer  string
	Port  int
	Key   []byte
}

// saKey identifies an SA by ordered (local, peer, port). The peer
// manager always calls Install/Remove with the same local-first
// convention, so we can key the map directly.
type saKey struct {
	local, peer string
	port        int
}

// MockIPsecManager is an in-memory IPsecManager used by tests.
type MockIPsecManager struct {
	mu                 sync.Mutex
	Ops                []MockIPsecOp
	sas                map[saKey][]byte // current SAs -> key
	ConflictResponse   bool             // ProbeConflict returns this
	FailNext           func(kind string) error
}

// NewMockIPsecManager constructs an empty MockIPsecManager.
func NewMockIPsecManager() *MockIPsecManager {
	return &MockIPsecManager{sas: map[saKey][]byte{}}
}

// InstallSA records the call and tracks the installed SA. A second
// install with the same key is recorded but treated as idempotent.
func (m *MockIPsecManager) InstallSA(ctx context.Context, local, peer string, port int, key []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailNext != nil {
		if err := m.FailNext("install"); err != nil {
			return err
		}
	}
	m.sas[saKey{local, peer, port}] = append([]byte(nil), key...)
	m.Ops = append(m.Ops, MockIPsecOp{
		Kind: "install", Local: local, Peer: peer, Port: port,
		Key: append([]byte(nil), key...),
	})
	return nil
}

// RemoveSA records the call. Missing SA is success.
func (m *MockIPsecManager) RemoveSA(ctx context.Context, local, peer string, port int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailNext != nil {
		if err := m.FailNext("remove"); err != nil {
			return err
		}
	}
	delete(m.sas, saKey{local, peer, port})
	m.Ops = append(m.Ops, MockIPsecOp{Kind: "remove", Local: local, Peer: peer, Port: port})
	return nil
}

// ProbeConflict returns the value of ConflictResponse.
func (m *MockIPsecManager) ProbeConflict(ctx context.Context, local, peer string, port int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailNext != nil {
		if err := m.FailNext("probe"); err != nil {
			return false, err
		}
	}
	m.Ops = append(m.Ops, MockIPsecOp{Kind: "probe", Local: local, Peer: peer, Port: port})
	return m.ConflictResponse, nil
}

// HasSA returns whether (local, peer, port) is currently installed.
func (m *MockIPsecManager) HasSA(local, peer string, port int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.sas[saKey{local, peer, port}]
	return ok
}

// SAKey returns the most recently installed key for (local, peer, port).
func (m *MockIPsecManager) SAKey(local, peer string, port int) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.sas[saKey{local, peer, port}]
	if !ok {
		return nil, errors.New("mock: no SA installed for pair")
	}
	return append([]byte(nil), k...), nil
}

// OpsByKind returns recorded ops filtered by kind.
func (m *MockIPsecManager) OpsByKind(kind string) []MockIPsecOp {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MockIPsecOp, 0)
	for _, o := range m.Ops {
		if o.Kind == kind {
			out = append(out, o)
		}
	}
	return out
}

// Compile-time assertions: mocks satisfy the production interfaces.
var (
	_ DeviceManager = (*MockDeviceManager)(nil)
	_ IPsecManager  = (*MockIPsecManager)(nil)
)
