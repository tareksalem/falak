package tests

import (
	"context"
	"time"

	"github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/stretchr/testify/mock"
	ma "github.com/multiformats/go-multiaddr"
)

// MockHost is a mock implementation of libp2p host.Host
type MockHost struct {
	mock.Mock
}

func (m *MockHost) ID() peer.ID {
	args := m.Called()
	return args.Get(0).(peer.ID)
}

func (m *MockHost) Peerstore() peerstore.Peerstore {
	args := m.Called()
	return args.Get(0).(peerstore.Peerstore)
}

func (m *MockHost) Addrs() []ma.Multiaddr {
	args := m.Called()
	return args.Get(0).([]ma.Multiaddr)
}

func (m *MockHost) Network() network.Network {
	args := m.Called()
	return args.Get(0).(network.Network)
}

func (m *MockHost) Mux() protocol.Switch {
	args := m.Called()
	return args.Get(0).(protocol.Switch)
}

func (m *MockHost) Connect(ctx context.Context, pi peer.AddrInfo) error {
	args := m.Called(ctx, pi)
	return args.Error(0)
}

func (m *MockHost) SetStreamHandler(pid protocol.ID, handler network.StreamHandler) {
	m.Called(pid, handler)
}

func (m *MockHost) SetStreamHandlerMatch(pid protocol.ID, match func(protocol.ID) bool, handler network.StreamHandler) {
	m.Called(pid, match, handler)
}

func (m *MockHost) RemoveStreamHandler(pid protocol.ID) {
	m.Called(pid)
}

func (m *MockHost) NewStream(ctx context.Context, p peer.ID, pids ...protocol.ID) (network.Stream, error) {
	args := m.Called(ctx, p, pids)
	return args.Get(0).(network.Stream), args.Error(1)
}

func (m *MockHost) Close() error {
	args := m.Called()
	return args.Error(0)
}

func (m *MockHost) ConnManager() interface{} {
	args := m.Called()
	return args.Get(0)
}

func (m *MockHost) EventBus() interface{} {
	args := m.Called()
	return args.Get(0)
}

// MockPubSub is a mock implementation of pubsub.PubSub
type MockPubSub struct {
	mock.Mock
}

func (m *MockPubSub) GetTopics() []string {
	args := m.Called()
	return args.Get(0).([]string)
}

func (m *MockPubSub) ListPeers(topic string) []peer.ID {
	args := m.Called(topic)
	return args.Get(0).([]peer.ID)
}

func (m *MockPubSub) Join(topic string, opts ...pubsub.TopicOpt) (*pubsub.Topic, error) {
	args := m.Called(topic, opts)
	return args.Get(0).(*pubsub.Topic), args.Error(1)
}

// MockTopic is a mock implementation of pubsub.Topic
type MockTopic struct {
	mock.Mock
}

func (m *MockTopic) String() string {
	args := m.Called()
	return args.String(0)
}

func (m *MockTopic) Subscribe(opts ...pubsub.SubOpt) (*pubsub.Subscription, error) {
	args := m.Called(opts)
	return args.Get(0).(*pubsub.Subscription), args.Error(1)
}

func (m *MockTopic) Publish(ctx context.Context, data []byte, opts ...pubsub.PubOpt) error {
	args := m.Called(ctx, data, opts)
	return args.Error(0)
}

func (m *MockTopic) ListPeers() []peer.ID {
	args := m.Called()
	return args.Get(0).([]peer.ID)
}

func (m *MockTopic) Close() error {
	args := m.Called()
	return args.Error(0)
}

// MockSubscription is a mock implementation of pubsub.Subscription
type MockSubscription struct {
	mock.Mock
}

func (m *MockSubscription) Topic() string {
	args := m.Called()
	return args.String(0)
}

func (m *MockSubscription) Next(ctx context.Context) (*pubsub.Message, error) {
	args := m.Called(ctx)
	return args.Get(0).(*pubsub.Message), args.Error(1)
}

func (m *MockSubscription) Cancel() {
	m.Called()
}

// MockStream is a mock implementation of network.Stream
type MockStream struct {
	mock.Mock
}

func (m *MockStream) Close() error {
	args := m.Called()
	return args.Error(0)
}

func (m *MockStream) Reset() error {
	args := m.Called()
	return args.Error(0)
}

func (m *MockStream) SetDeadline(t time.Time) error {
	args := m.Called(t)
	return args.Error(0)
}

func (m *MockStream) SetReadDeadline(t time.Time) error {
	args := m.Called(t)
	return args.Error(0)
}

func (m *MockStream) SetWriteDeadline(t time.Time) error {
	args := m.Called(t)
	return args.Error(0)
}

func (m *MockStream) Read(p []byte) (n int, err error) {
	args := m.Called(p)
	return args.Int(0), args.Error(1)
}

func (m *MockStream) Write(p []byte) (n int, err error) {
	args := m.Called(p)
	return args.Int(0), args.Error(1)
}

func (m *MockStream) Protocol() protocol.ID {
	args := m.Called()
	return args.Get(0).(protocol.ID)
}

func (m *MockStream) SetProtocol(id protocol.ID) error {
	args := m.Called(id)
	return args.Error(0)
}

func (m *MockStream) Stat() network.Stats {
	args := m.Called()
	return args.Get(0).(network.Stats)
}

func (m *MockStream) Conn() network.Conn {
	args := m.Called()
	return args.Get(0).(network.Conn)
}

func (m *MockStream) CloseWrite() error {
	args := m.Called()
	return args.Error(0)
}

func (m *MockStream) CloseRead() error {
	args := m.Called()
	return args.Error(0)
}

// MockNetwork is a mock implementation of network.Network
type MockNetwork struct {
	mock.Mock
}

func (m *MockNetwork) Peerstore() peerstore.Peerstore {
	args := m.Called()
	return args.Get(0).(peerstore.Peerstore)
}

func (m *MockNetwork) LocalPeer() peer.ID {
	args := m.Called()
	return args.Get(0).(peer.ID)
}

func (m *MockNetwork) DialPeer(ctx context.Context, p peer.ID) (network.Conn, error) {
	args := m.Called(ctx, p)
	return args.Get(0).(network.Conn), args.Error(1)
}

func (m *MockNetwork) ClosePeer(p peer.ID) error {
	args := m.Called(p)
	return args.Error(0)
}

func (m *MockNetwork) Connectedness(p peer.ID) network.Connectedness {
	args := m.Called(p)
	return args.Get(0).(network.Connectedness)
}

func (m *MockNetwork) Peers() []peer.ID {
	args := m.Called()
	return args.Get(0).([]peer.ID)
}

func (m *MockNetwork) Conns() []network.Conn {
	args := m.Called()
	return args.Get(0).([]network.Conn)
}

func (m *MockNetwork) ConnsToPeer(p peer.ID) []network.Conn {
	args := m.Called(p)
	return args.Get(0).([]network.Conn)
}

func (m *MockNetwork) Notify(notifiee network.Notifiee) {
	m.Called(notifiee)
}

func (m *MockNetwork) StopNotify(notifiee network.Notifiee) {
	m.Called(notifiee)
}

func (m *MockNetwork) Close() error {
	args := m.Called()
	return args.Error(0)
}