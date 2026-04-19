# Falak Development Plan & Architecture Guidelines

## Overview
Falak is a decentralized, orchestration-less container execution platform using physics-inspired concepts (gravity, orbits, momentum) for distributed workload management via libp2p.

---

## 1. Environment Setup

### Go Version & Workspace
```bash
# Install Go 1.24 via gvm
gvm install go1.24 -B
gvm use go1.24 --default

# Verify
go version  # go1.24.x
```

### Project Initialization
```bash
cd /Users/tarek.salem/tests/falak
# Create go.work after modules are initialized
```

---

## 2. Event-Driven Architecture Philosophy

### Why Event-Driven?
Instead of tightly coupled direct function calls with complex if/else chains, Falak uses **reactive/event-driven patterns** for decoupled communication between components.

**Example Problem (Bad Approach):**
```go
// BAD: Tightly coupled, hard to extend
func (n *Node) Connect(peer PeerID) error {
    if err := n.network.Connect(peer); err != nil {
        return err
    }
    if n.config.EnableAuth {
        if err := n.auth.Authenticate(peer); err != nil {
            return err
        }
    }
    if err := n.phonebook.Add(peer); err != nil {
        return err
    }
    if err := n.membership.Announce(peer); err != nil {
        return err
    }
    // ... more if statements
}
```

**Solution (Event-Driven):**
```go
// GOOD: Decoupled, reactive, extensible
func (n *Node) Connect(peer PeerID) error {
    if err := n.network.Connect(peer); err != nil {
        return err
    }
    // Emit event - all interested components react independently
    n.eventBus.Publish(events.PeerConnected{PeerID: peer})
    return nil
}

// Each component subscribes and reacts independently
func (a *Authenticator) init() {
    a.eventBus.Subscribe(events.PeerConnected{}, a.onPeerConnected)
}

func (p *Phonebook) init() {
    p.eventBus.Subscribe(events.PeerAuthenticated{}, p.onPeerAuthenticated)
}
```

### State Machines
Complex state transitions (e.g., capsule lifecycle, election process, node membership) use **finite state machines** for predictable behavior.

```go
// Capsule Lifecycle FSM
States: Created → Announced → Electing → Executing → Running → Stopping → Stopped
Events: Announce, ElectionWon, Started, Ready, Stop, Failed

// Election FSM
States: Idle → Voting → Collecting → Tallying → Decided
Events: CapsuleReceived, VoteCast, Timeout, QuorumReached

// Node Membership FSM
States: Disconnected → Connecting → Authenticating → Joined → Leaving
Events: Connect, AuthSuccess, AuthFailed, Leave, Disconnected
```

---

## 3. Final Module Structure (Bounded Context)

Based on DDD bounded contexts with orbit merged into capsule:

```
falak/
├── go.work                          # Go workspace definition
├── go.work.sum
├── Makefile
├── README.md
│
├── docs/
│   ├── DEVELOPMENT.md               # This guideline (main reference)
│   ├── architecture.md
│   └── protocols/
│       ├── authentication.md
│       ├── election.md
│       └── snapshot-transfer.md
│
├── scripts/
│   ├── setup.sh                     # Dev environment setup
│   ├── proto-gen.sh                 # Protobuf generation
│   └── test.sh                      # Run all tests
│
├── events/                          # github.com/tareksalem/falak/events
│   ├── go.mod
│   ├── bus.go                       # Event bus implementation
│   ├── types.go                     # Event type definitions
│   ├── subscriber.go                # Subscription management
│   └── events/                      # Domain events
│       ├── network.go               # PeerConnected, PeerDisconnected, StreamOpened
│       ├── cluster.go               # NodeJoined, NodeLeft, AuthSucceeded, AuthFailed
│       ├── capsule.go               # CapsuleCreated, CapsuleAnnounced, CapsuleUpdated
│       ├── election.go              # ElectionStarted, VoteReceived, ElectionWon
│       ├── snapshot.go              # SnapshotCreated, TransferStarted, TransferCompleted
│       └── execution.go             # ExecutionStarted, ExecutionReady, ExecutionFailed
│
├── fsm/                             # github.com/tareksalem/falak/fsm
│   ├── go.mod
│   ├── machine.go                   # Generic FSM implementation
│   ├── transition.go                # Transition definitions
│   └── machines/
│       ├── capsule_lifecycle.go     # Capsule state machine
│       ├── election.go              # Election state machine
│       ├── membership.go            # Node membership state machine
│       └── transfer.go              # Snapshot transfer state machine
│
├── proto/                           # github.com/tareksalem/falak/proto
│   ├── go.mod
│   ├── gen/                         # Generated Go code
│   │   └── falakpb/
│   └── definitions/
│       ├── common.proto             # Shared types (NodeID, Timestamp, etc.)
│       ├── capsule.proto            # Capsule & Orbit messages
│       ├── cluster.proto            # Membership & auth messages
│       ├── election.proto           # Election & gravity messages
│       ├── snapshot.proto           # Snapshot transfer messages
│       └── api.proto                # gRPC management API
│
├── types/                           # github.com/tareksalem/falak/types
│   ├── go.mod
│   ├── ids.go                       # NodeID, CapsuleID, ClusterID, etc.
│   ├── config.go                    # Configuration structs
│   ├── errors.go                    # Domain error types
│   └── constants.go                 # Protocol constants
│
├── network/                         # github.com/tareksalem/falak/network
│   ├── go.mod
│   ├── host.go                      # libp2p host wrapper
│   ├── host_options.go              # Functional options
│   ├── stream.go                    # Direct stream handling
│   ├── pubsub.go                    # GossipSub wrapper
│   ├── discovery.go                 # Peer discovery
│   ├── security.go                  # TLS & key management
│   └── internal/
│       └── protocol/
│           └── ids.go               # Protocol ID constants
│
├── cluster/                         # github.com/tareksalem/falak/cluster
│   ├── go.mod
│   ├── manager.go                   # ClusterManager implementation
│   ├── membership/
│   │   ├── service.go               # Membership tracking
│   │   ├── events.go                # Join/Leave/Update events
│   │   └── pubsub.go                # Membership topic handler
│   ├── phonebook/
│   │   ├── phonebook.go             # Peer directory
│   │   ├── store.go                 # Persistence
│   │   └── sync.go                  # Delta synchronization
│   ├── auth/
│   │   ├── authenticator.go         # Authentication logic
│   │   ├── challenge.go             # Challenge-response
│   │   └── handler.go               # /falak/join/1.0 handler
│   └── failure/
│       ├── detector.go              # FailureDetector interface
│       ├── phi_accrual.go           # Phi accrual algorithm
│       ├── swim.go                  # SWIM protocol
│       └── quorum.go                # Quorum-based confirmation
│
├── capsule/                         # github.com/tareksalem/falak/capsule
│   ├── go.mod
│   ├── manager.go                   # CapsuleManager implementation
│   ├── capsule.go                   # Capsule entity
│   ├── spec.go                      # CapsuleSpec parsing/validation
│   ├── store.go                     # Capsule persistence
│   ├── orbit/
│   │   ├── orbit.go                 # Orbit (topic) management
│   │   ├── subscription.go          # Topic subscriptions
│   │   ├── announcement.go          # Capsule announcements
│   │   └── matcher.go               # Wildcard topic matching
│   └── momentum/
│       ├── momentum.go              # Momentum state
│       ├── decay.go                 # Energy decay mechanics
│       └── lifecycle.go             # Execution lifecycle FSM
│
├── election/                        # github.com/tareksalem/falak/election
│   ├── go.mod
│   ├── manager.go                   # ElectionManager implementation
│   ├── gravity/
│   │   ├── calculator.go            # Gravity score calculation
│   │   ├── factors.go               # CPU, memory, latency, affinity
│   │   └── weights.go               # Configurable weights
│   ├── voting/
│   │   ├── voter.go                 # Vote casting
│   │   ├── collector.go             # Vote collection
│   │   └── tiebreaker.go            # Deterministic tiebreaker
│   └── quorum/
│       └── verifier.go              # Quorum verification
│
├── snapshot/                        # github.com/tareksalem/falak/snapshot
│   ├── go.mod
│   ├── manager.go                   # SnapshotManager implementation
│   ├── create.go                    # Snapshot creation (CRIU)
│   ├── compress.go                  # Zstandard compression
│   ├── transfer/
│   │   ├── sender.go                # /falak/snapshot/1.0 sender
│   │   ├── receiver.go              # Stream receiver
│   │   └── progress.go              # Transfer progress tracking
│   ├── restore.go                   # Container restoration
│   └── store.go                     # Local snapshot storage
│
├── runtime/                         # github.com/tareksalem/falak/runtime
│   ├── go.mod
│   ├── runtime.go                   # ContainerRuntime interface
│   ├── containerd/
│   │   ├── client.go                # containerd client
│   │   ├── container.go             # Container operations
│   │   └── checkpoint.go            # CRIU checkpointing
│   └── mock/
│       └── mock_runtime.go          # Mock for testing
│
├── node/                            # github.com/tareksalem/falak/node
│   ├── go.mod
│   ├── node.go                      # Node orchestration
│   ├── options.go                   # Functional options
│   ├── capabilities.go              # Node capability detection
│   ├── lifecycle.go                 # Start/Stop/Join/Leave
│   └── internal/
│       └── wiring/
│           └── wire.go              # Dependency wiring
│
├── api/                             # github.com/tareksalem/falak/api
│   ├── go.mod
│   ├── grpc/
│   │   ├── server.go                # gRPC server
│   │   └── handlers/
│   │       ├── capsule.go
│   │       ├── cluster.go
│   │       └── node.go
│   └── gateway/
│       └── gateway.go               # grpc-gateway REST proxy
│
└── cmd/                             # github.com/tareksalem/falak (main module)
    ├── go.mod
    ├── falakd/                      # Daemon
    │   ├── main.go
    │   └── config.go
    └── falakctl/                    # CLI tool
        ├── main.go
        └── commands/
            ├── root.go
            ├── join.go
            ├── capsule.go
            └── status.go
```

---

## 4. Module Dependencies Graph

```
                    ┌─────────┐
                    │  proto  │  (no deps, generated code)
                    └────┬────┘
                         │
                    ┌────▼────┐
                    │  types  │  (depends on proto)
                    └────┬────┘
                         │
              ┌──────────┼──────────┐
              │          │          │
         ┌────▼────┐ ┌───▼───┐ ┌────▼────┐
         │ events  │ │  fsm  │ │(shared) │
         └────┬────┘ └───┬───┘ └─────────┘
              │          │
              └────┬─────┘
                   │
         ┌─────────┼─────────────┐
         │         │             │
    ┌────▼────┐ ┌──▼─────┐  ┌────▼────┐
    │ network │ │runtime │  │ (other) │
    └────┬────┘ └──┬─────┘  └─────────┘
         │         │
    ┌────▼─────────▼──┐
    │     cluster     │ (uses events, fsm)
    └────────┬────────┘
             │
    ┌────────▼────────┐
    │     capsule     │ (uses events, fsm)
    └────────┬────────┘
             │
    ┌────────▼────────┐    ┌──────────┐
    │    election     │    │ snapshot │
    └────────┬────────┘    └────┬─────┘
             │                  │
             └────────┬─────────┘
                      │
              ┌───────▼───────┐
              │     node      │ (orchestrates via events)
              └───────┬───────┘
                      │
              ┌───────▼───────┐
              │      api      │
              └───────┬───────┘
                      │
              ┌───────▼───────┐
              │      cmd      │
              └───────────────┘
```

**Key Principle:** Components communicate via the EventBus, not direct calls.
- `events/` is the central nervous system
- `fsm/` manages state transitions triggered by events
- All domain modules subscribe to relevant events and publish their own

---

## 5. Core Interfaces

### 5.1 events/bus.go (Event-Driven Core)
```go
package events

import "context"

// Event is the base interface for all domain events
type Event interface {
    EventType() string
    Timestamp() time.Time
}

// EventBus provides publish-subscribe functionality
type EventBus interface {
    // Publish sends an event to all subscribers
    Publish(ctx context.Context, event Event) error

    // Subscribe registers a handler for a specific event type
    Subscribe(eventType string, handler Handler) Subscription

    // SubscribeAsync registers an async handler (non-blocking)
    SubscribeAsync(eventType string, handler Handler) Subscription

    // SubscribeOnce registers a one-time handler
    SubscribeOnce(eventType string, handler Handler) Subscription

    // Close shuts down the event bus
    Close() error
}

// Handler processes events
type Handler func(ctx context.Context, event Event) error

// Subscription represents an active subscription
type Subscription interface {
    Unsubscribe()
    EventType() string
}

// Example usage in components:
//
// type Authenticator struct {
//     eventBus events.EventBus
// }
//
// func (a *Authenticator) Start(ctx context.Context) {
//     a.eventBus.Subscribe(events.TypePeerConnected, a.onPeerConnected)
// }
//
// func (a *Authenticator) onPeerConnected(ctx context.Context, e events.Event) error {
//     evt := e.(*events.PeerConnected)
//     // Authenticate the peer...
//     a.eventBus.Publish(ctx, &events.PeerAuthenticated{PeerID: evt.PeerID})
//     return nil
// }
```

### 5.2 events/events/ (Domain Events)
```go
package events

// Network Events
const (
    TypePeerConnected    = "network.peer_connected"
    TypePeerDisconnected = "network.peer_disconnected"
    TypeStreamOpened     = "network.stream_opened"
    TypeMessageReceived  = "network.message_received"
)

type PeerConnected struct {
    BaseEvent
    PeerID   types.NodeID
    Addrs    []multiaddr.Multiaddr
}

type PeerDisconnected struct {
    BaseEvent
    PeerID types.NodeID
    Reason string
}

// Cluster Events
const (
    TypeNodeJoined        = "cluster.node_joined"
    TypeNodeLeft          = "cluster.node_left"
    TypeAuthSucceeded     = "cluster.auth_succeeded"
    TypeAuthFailed        = "cluster.auth_failed"
    TypePhonebookUpdated  = "cluster.phonebook_updated"
)

type NodeJoined struct {
    BaseEvent
    NodeID       types.NodeID
    ClusterID    types.ClusterID
    Capabilities map[string]string
}

// Capsule Events
const (
    TypeCapsuleCreated   = "capsule.created"
    TypeCapsuleAnnounced = "capsule.announced"
    TypeCapsuleReceived  = "capsule.received"
)

type CapsuleAnnounced struct {
    BaseEvent
    CapsuleID types.CapsuleID
    OrbitID   types.OrbitID
    Spec      *capsule.Spec
}

// Election Events
const (
    TypeElectionStarted = "election.started"
    TypeVoteReceived    = "election.vote_received"
    TypeElectionWon     = "election.won"
    TypeElectionLost    = "election.lost"
)

type ElectionWon struct {
    BaseEvent
    CapsuleID types.CapsuleID
    NodeID    types.NodeID
    Gravity   float64
}
```

### 5.3 fsm/machine.go (State Machine)
```go
package fsm

import "context"

// State represents a state in the machine
type State string

// EventType triggers transitions
type EventType string

// Machine is a finite state machine
type Machine interface {
    // Current returns the current state
    Current() State

    // Can checks if an event can trigger a transition
    Can(event EventType) bool

    // Fire triggers a state transition
    Fire(ctx context.Context, event EventType, args ...interface{}) error

    // OnEnter registers a callback for entering a state
    OnEnter(state State, fn StateCallback)

    // OnLeave registers a callback for leaving a state
    OnLeave(state State, fn StateCallback)

    // OnTransition registers a callback for any transition
    OnTransition(fn TransitionCallback)
}

type StateCallback func(ctx context.Context, state State, args ...interface{}) error
type TransitionCallback func(ctx context.Context, from, to State, event EventType) error

// Builder creates state machines
type Builder interface {
    Initial(state State) Builder
    State(state State) StateBuilder
    Build() Machine
}

type StateBuilder interface {
    On(event EventType) TransitionBuilder
    Done() Builder
}

type TransitionBuilder interface {
    GoTo(state State) StateBuilder
    If(cond ConditionFunc) TransitionBuilder
}

// Example: Capsule Lifecycle FSM
//
// machine := fsm.NewBuilder().
//     Initial(StateCreated).
//     State(StateCreated).
//         On(EventAnnounce).GoTo(StateAnnounced).
//     Done().
//     State(StateAnnounced).
//         On(EventElectionWon).GoTo(StateExecuting).
//         On(EventElectionLost).GoTo(StateIdle).
//     Done().
//     State(StateExecuting).
//         On(EventStarted).GoTo(StateRunning).
//         On(EventFailed).GoTo(StateFailed).
//     Done().
//     Build()
```

### 5.4 types/ids.go
```go
package types

import "github.com/libp2p/go-libp2p/core/peer"

type (
    NodeID    peer.ID
    ClusterID string
    CapsuleID string
    OrbitID   string
    SnapshotID string
)
```

### 4.2 types/errors.go
```go
package types

import "github.com/pkg/errors"

var (
    ErrNodeNotAuthenticated = errors.New("node not authenticated")
    ErrElectionTimeout      = errors.New("election timeout")
    ErrSnapshotNotFound     = errors.New("snapshot not found")
    ErrQuorumNotReached     = errors.New("quorum not reached")
    ErrInvalidSignature     = errors.New("invalid message signature")
)

type AuthError struct {
    PeerID  NodeID
    Reason  string
    cause   error
}

func (e *AuthError) Error() string { ... }
func (e *AuthError) Unwrap() error { return e.cause }
```

### 4.3 network/host.go
```go
package network

import (
    "context"
    "github.com/libp2p/go-libp2p/core/host"
    "github.com/libp2p/go-libp2p/core/protocol"
    pubsub "github.com/libp2p/go-libp2p-pubsub"
)

type Host interface {
    // Identity
    ID() peer.ID
    Addrs() []multiaddr.Multiaddr

    // Streams
    NewStream(ctx context.Context, p peer.ID, pids ...protocol.ID) (network.Stream, error)
    SetStreamHandler(pid protocol.ID, handler network.StreamHandler)
    RemoveStreamHandler(pid protocol.ID)

    // PubSub
    Join(topic string) (*pubsub.Topic, error)
    Subscribe(topic string) (*pubsub.Subscription, error)

    // Lifecycle
    Connect(ctx context.Context, pi peer.AddrInfo) error
    Close() error
}

type HostConfig struct {
    ListenAddrs   []string
    PrivateKey    crypto.PrivKey
    EnableRelay   bool
    EnableNATHole bool
}

func NewHost(ctx context.Context, cfg HostConfig) (Host, error)
```

### 4.4 cluster/manager.go
```go
package cluster

import "context"

type Manager interface {
    // Authentication
    Authenticate(ctx context.Context, peerID NodeID, token []byte) error
    IsAuthenticated(peerID NodeID) bool

    // Membership
    Members() []MemberInfo
    OnJoin(fn func(MemberInfo))
    OnLeave(fn func(MemberInfo))

    // Phonebook
    Phonebook() *phonebook.Phonebook
    SyncPhonebook(ctx context.Context, peer NodeID) error

    // Failure Detection
    FailureDetector() failure.Detector
}

type MemberInfo struct {
    ID           NodeID
    Addrs        []multiaddr.Multiaddr
    DataCenter   string
    Capabilities map[string]string
    JoinedAt     time.Time
}
```

### 4.5 capsule/manager.go
```go
package capsule

import "context"

type Manager interface {
    // CRUD
    Create(ctx context.Context, spec *Spec) (*Capsule, error)
    Get(ctx context.Context, id CapsuleID) (*Capsule, error)
    List(ctx context.Context, filter Filter) ([]*Capsule, error)
    Delete(ctx context.Context, id CapsuleID) error

    // Publishing
    Publish(ctx context.Context, capsule *Capsule) error

    // Orbits
    SubscribeOrbit(ctx context.Context, orbit OrbitID) (<-chan *Capsule, error)
    UnsubscribeOrbit(orbit OrbitID) error
}

type Capsule struct {
    ID          CapsuleID
    Version     string
    Spec        *Spec
    Orbit       OrbitID
    Momentum    *Momentum
    CreatedAt   time.Time
}

type Spec struct {
    Name        string
    Image       string
    ImageDigest string
    Resources   Resources
    Placement   Placement
    Gravity     GravityConfig
    Tags        []string
}
```

### 4.6 election/manager.go
```go
package election

import "context"

type Manager interface {
    // Gravity
    CalculateGravity(capsule *capsule.Capsule) GravityScore

    // Voting
    StartElection(ctx context.Context, capsuleID CapsuleID) (*Election, error)
    Vote(ctx context.Context, election *Election) error
    OnWon(fn func(capsuleID CapsuleID))

    // Quorum
    VerifyQuorum(election *Election) (bool, error)
}

type GravityScore struct {
    Total       float64
    CPUScore    float64
    MemScore    float64
    LatencyScore float64
    AffinityScore float64
}

type Election struct {
    CapsuleID  CapsuleID
    StartedAt  time.Time
    Votes      []Vote
    Winner     *NodeID
    Status     ElectionStatus
}
```

### 4.7 snapshot/manager.go
```go
package snapshot

import "context"

type Manager interface {
    // Creation
    Create(ctx context.Context, containerID string) (*Snapshot, error)

    // Transfer
    Request(ctx context.Context, peer NodeID, snapshotID SnapshotID) error
    Serve(ctx context.Context, snapshotID SnapshotID, stream network.Stream) error

    // Restoration
    Restore(ctx context.Context, snapshotID SnapshotID) (containerID string, error)

    // Storage
    Get(snapshotID SnapshotID) (*Snapshot, error)
    List() []*Snapshot
    Delete(snapshotID SnapshotID) error
}

type Snapshot struct {
    ID           SnapshotID
    CapsuleID    CapsuleID
    Size         int64
    CompressedSize int64
    Checksum     string
    CreatedAt    time.Time
    NodeID       NodeID
}
```

### 4.8 node/node.go
```go
package node

import "context"

type Node interface {
    // Identity
    ID() NodeID
    Capabilities() Capabilities

    // Lifecycle
    Start(ctx context.Context) error
    Stop(ctx context.Context) error

    // Cluster
    Join(ctx context.Context, clusterID ClusterID, peers []peer.AddrInfo) error
    Leave(ctx context.Context, clusterID ClusterID) error

    // Status
    Status() Status
}

type Capabilities struct {
    CPUCores    int
    MemoryMB    int64
    StorageGB   int64
    Tags        []string
    DataCenter  string
}

type Status struct {
    State       NodeState
    Clusters    []ClusterID
    ActiveCapsules []CapsuleID
    Uptime      time.Duration
}
```

---

## 5. Protocol Specifications

### 5.1 Authentication Protocol (`/falak/join/1.0`)

```
┌────────┐                           ┌────────┐
│ Joiner │                           │  Peer  │
└───┬────┘                           └───┬────┘
    │                                    │
    │─────── ClientHello ───────────────►│
    │  {cluster_id, node_id, addrs,      │
    │   capabilities, nonce, auth_type}  │
    │                                    │
    │◄────── ServerChallenge ────────────│
    │  {challenge_bytes, server_nonce}   │
    │                                    │
    │─────── ClientResponse ────────────►│
    │  {signed(challenge + nonces)}      │
    │                                    │
    │◄────── ServerAck ──────────────────│
    │  {success, phonebook_delta}        │
    │                                    │
```

### 5.2 Election Protocol

```
Topic: falak/<clusterId>/elect/<capsuleId>

1. Capsule announced on orbit topic
2. Interested nodes calculate gravity
3. Each node publishes ElectionVote:
   {
     node_id: NodeID,
     capsule_id: CapsuleID,
     gravity_score: float64,
     capabilities: {...},
     signature: bytes
   }
4. After timeout (5-30s), nodes tally votes
5. Highest gravity wins (tiebreaker: lowest NodeID)
6. Winner publishes ExecutionStarted
```

### 5.3 Snapshot Transfer Protocol (`/falak/snapshot/1.0`)

```
┌──────────┐                        ┌────────┐
│ Receiver │                        │ Sender │
└────┬─────┘                        └───┬────┘
     │                                  │
     │──── TransferRequest ────────────►│
     │  {snapshot_id, offset}           │
     │                                  │
     │◄─── TransferHeader ──────────────│
     │  {size, checksum, chunk_size}    │
     │                                  │
     │◄─── TransferChunk (repeated) ────│
     │  {sequence, data, checksum}      │
     │                                  │
     │──── TransferAck ────────────────►│
     │  {sequence, ok}                  │
     │                                  │
     │◄─── TransferComplete ────────────│
     │  {final_checksum}                │
```

### 5.4 Failure Detection

```
Phi Accrual Detection:
- Track heartbeat intervals per peer
- Calculate φ (phi) based on distribution
- φ > 5  → Suspect
- φ > 9  → Failed (initiate SWIM probe)

SWIM Probe:
1. Direct ping to suspected node
2. If no response: indirect ping via k random peers
3. If still no response: publish SuspicionMessage
4. Quorum of nodes must confirm (diversity required)
5. Mark as failed after quorum confirmation

Self-Recovery:
- Node receiving failure verdict about itself:
  1. Increment incarnation counter
  2. Re-authenticate with existing peer
  3. Publish rejoin announcement
```

---

## 6. Configuration (Viper)

### config.yaml
```yaml
node:
  name: "node-1"
  data_center: "us-east-1"

network:
  listen_addrs:
    - "/ip4/0.0.0.0/tcp/4001"
    - "/ip6/::/tcp/4001"
  enable_relay: true
  enable_nat_hole_punch: true

cluster:
  default_id: "default"
  auth:
    type: "psk"  # or "certificate"
    psk: "${FALAK_PSK}"

failure_detection:
  phi_suspect_threshold: 5.0
  phi_fail_threshold: 9.0
  heartbeat_interval: "1s"

election:
  timeout: "10s"
  gravity_weights:
    cpu: 0.3
    memory: 0.3
    latency: 0.2
    affinity: 0.2

snapshot:
  storage_path: "/var/lib/falak/snapshots"
  compression_level: 3

logging:
  level: "info"
  format: "json"

api:
  grpc:
    address: ":9090"
  gateway:
    address: ":8080"
```

---

## 8. Key Dependencies

| Module | Package | Purpose |
|--------|---------|---------|
| **Core Infrastructure** | | |
| events | (custom impl) | Event bus using Go channels |
| fsm | `github.com/looplab/fsm` | State machine library |
| all | `go.uber.org/zap` | Structured logging |
| types | `github.com/pkg/errors` | Error handling with stack traces |
| **Networking** | | |
| network | `github.com/libp2p/go-libp2p` | P2P networking |
| network | `github.com/libp2p/go-libp2p-pubsub` | GossipSub messaging |
| network | `github.com/multiformats/go-multiaddr` | Multi-address handling |
| **Serialization** | | |
| proto | `google.golang.org/protobuf` | Protocol buffers |
| proto | `google.golang.org/grpc` | gRPC framework |
| api | `github.com/grpc-ecosystem/grpc-gateway/v2` | REST gateway |
| **Runtime** | | |
| snapshot | `github.com/klauspost/compress/zstd` | Zstandard compression |
| runtime | `github.com/containerd/containerd` | Container runtime |
| **CLI & Config** | | |
| cmd | `github.com/spf13/viper` | Configuration management |
| cmd | `github.com/spf13/cobra` | CLI framework |
| **Testing** | | |
| test | `github.com/stretchr/testify` | Assertions & mocking |
| test | `github.com/golang/mock` | Mock generation |

---

## 9. Implementation Phases

### Phase 1: Foundation (Current)
- [x] Architecture design
- [x] Event-driven architecture design
- [ ] Go workspace setup
- [ ] events/ module (event bus)
- [ ] fsm/ module (state machines)
- [ ] proto/ module with definitions
- [ ] types/ module with IDs and errors
- [ ] Basic Makefile

### Phase 2: Network Layer
- [ ] network/ module
- [ ] libp2p host wrapper
- [ ] PubSub integration
- [ ] Stream handlers

### Phase 3: Cluster Membership
- [ ] cluster/ module
- [ ] Authentication protocol
- [ ] Phonebook management
- [ ] Membership PubSub
- [ ] Basic failure detection

### Phase 4: Capsule & Orbit
- [ ] capsule/ module
- [ ] Capsule CRUD
- [ ] Orbit subscriptions
- [ ] Announcements

### Phase 5: Election System
- [ ] election/ module
- [ ] Gravity calculator
- [ ] Voting mechanism
- [ ] Quorum verification

### Phase 6: Snapshot System
- [ ] snapshot/ module
- [ ] runtime/ module (containerd)
- [ ] CRIU integration
- [ ] Transfer protocol

### Phase 7: Node Orchestration
- [ ] node/ module
- [ ] Component wiring
- [ ] Lifecycle management

### Phase 8: API & CLI
- [ ] api/ module (gRPC + gateway)
- [ ] cmd/falakd
- [ ] cmd/falakctl

### Phase 9: Hardening
- [ ] Advanced phi accrual
- [ ] SWIM protocol
- [ ] Self-recovery
- [ ] Performance tuning

---

## 10. Development Guidelines

### Code Style
- Use `gofmt` and `goimports`
- Follow [Uber Go Style Guide](https://github.com/uber-go/guide/blob/master/style.md)
- Interfaces in consuming package
- Constructors return interfaces, not concrete types

### Logging (zap)
```go
logger.Info("capsule published",
    zap.String("capsule_id", capsule.ID),
    zap.String("orbit", orbit.ID),
    zap.Duration("elapsed", elapsed),
)
```

### Error Handling
```go
if err != nil {
    return errors.Wrapf(err, "failed to authenticate peer %s", peerID)
}
```

### Enum Pattern
Use this pattern for all enums to provide discoverability and type safety:

```go
// 1. Define the type
type NodeStatus string

// 2. Define private constants
const (
    nodeStatusActive      NodeStatus = "active"
    nodeStatusSuspected   NodeStatus = "suspected"
    nodeStatusQuarantined NodeStatus = "quarantined"
    nodeStatusFailed      NodeStatus = "failed"
)

// 3. Define private enum struct
type nodeStatusEnum struct{}

// 4. Define public accessor variable
var NodeStatusEnum nodeStatusEnum

// 5. Define methods that return the values
func (nodeStatusEnum) Active() NodeStatus      { return nodeStatusActive }
func (nodeStatusEnum) Suspected() NodeStatus   { return nodeStatusSuspected }
func (nodeStatusEnum) Quarantined() NodeStatus { return nodeStatusQuarantined }
func (nodeStatusEnum) Failed() NodeStatus      { return nodeStatusFailed }

// Usage:
var status NodeStatus = NodeStatusEnum.Active()

// Type checking works:
func processStatus(s NodeStatus) { ... }
processStatus(NodeStatusEnum.Quarantined())
```

**Benefits:**
- IDE autocomplete shows all options when typing `NodeStatusEnum.`
- Private constants prevent direct access to raw values
- Immutable - methods return values, can't be reassigned
- Type safety preserved

### Testing
- Unit tests: `*_test.go` alongside source
- Integration tests: `internal/integration/`
- Use table-driven tests
- Mock interfaces with `testify/mock`

### Git Workflow
- `main` - stable, deployable
- `develop` - integration branch
- `feature/*` - feature branches
- Squash merge to develop, merge to main

---

## 11. Verification Plan

### Unit Tests
```bash
go test -race -cover ./...
```

### Integration Tests
```bash
# Start test cluster
make test-cluster

# Run integration tests
go test -tags=integration ./internal/integration/...
```

### Manual Testing
```bash
# Terminal 1: Start node 1
air -- --name=node1 --address=/ip4/127.0.0.1/tcp/4001

# Terminal 2: Start node 2, connect to node 1
air -- --name=node2 --address=/ip4/127.0.0.1/tcp/4002 \
  --peers=/ip4/127.0.0.1/tcp/4001/p2p/<node1-peer-id>

# Terminal 3: Publish capsule via CLI
./falakctl capsule create --spec=examples/capsule.yaml
```

---

## 12. File Listing Summary

### Modules to Create (in order)
1. `events/` - 1 go.mod, ~6 .go files (event bus, domain events)
2. `fsm/` - 1 go.mod, ~6 .go files (state machine, predefined machines)
3. `proto/` - 1 go.mod, ~6 .proto files (protobuf definitions)
4. `types/` - 1 go.mod, ~4 .go files (IDs, errors, constants)
5. `network/` - 1 go.mod, ~8 .go files (libp2p wrapper)
6. `cluster/` - 1 go.mod, ~15 .go files (membership, auth, failure)
7. `capsule/` - 1 go.mod, ~12 .go files (capsule, orbit, momentum)
8. `election/` - 1 go.mod, ~10 .go files (gravity, voting)
9. `snapshot/` - 1 go.mod, ~10 .go files (create, transfer, restore)
10. `runtime/` - 1 go.mod, ~6 .go files (containerd integration)
11. `node/` - 1 go.mod, ~6 .go files (orchestration)
12. `api/` - 1 go.mod, ~8 .go files (gRPC + gateway)
13. `cmd/` - 1 go.mod, ~10 .go files (falakd, falakctl)

**Total: 13 go.mod files, ~107 .go files**

---

## Next Steps

1. ~~Set up Go 1.24~~ (done)
2. Initialize Go workspace and directory structure
3. Create events/ module (event bus - core infrastructure)
4. Create fsm/ module (state machines)
5. Create proto/ module with protobuf definitions
6. Create types/ module with shared types
7. Begin network/ module implementation
