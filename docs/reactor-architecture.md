# Falak Reactor Pattern Architecture

## Overview

This document outlines the comprehensive reactor pattern architecture for Falak's node communication and lifecycle management system. The reactor pattern provides event-driven, state-managed, and self-healing infrastructure for all node operations.

## Architecture Design

### Three-Layer Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                     APPLICATION LAYER                          │
│  ┌─────────────────────┐  ┌─────────────────────┐              │
│  │   Capsule Manager   │  │   Orbit Manager     │              │
│  │   Election System   │  │   Runtime Manager   │              │
│  └─────────────────────┘  └─────────────────────┘              │
└─────────────────────────────────────────────────────────────────┘
┌─────────────────────────────────────────────────────────────────┐
│                     REACTOR LAYER                               │
│  ┌─────────────────────┐  ┌─────────────────────┐              │
│  │   Event Reactor     │  │  State Machines     │              │
│  │   - EventBus        │  │  - Node States      │              │
│  │   - EventLoop       │  │  - Peer States      │              │
│  │   - Subscribers     │  │  - Protocol States  │              │
│  └─────────────────────┘  └─────────────────────┘              │
└─────────────────────────────────────────────────────────────────┘
┌─────────────────────────────────────────────────────────────────┐
│                    INFRASTRUCTURE LAYER                        │
│  ┌─────────────────────┐  ┌─────────────────────┐              │
│  │   libp2p Network    │  │   Protocol Handlers │              │
│  │   - Streams         │  │   - Authentication  │              │
│  │   - PubSub          │  │   - Health Checks   │              │
│  │   - DHT             │  │   - Recovery        │              │
│  └─────────────────────┘  └─────────────────────┘              │
└─────────────────────────────────────────────────────────────────┘
```

### Core Components

#### 1. Central Event Reactor (EventReactor)

The heart of the system that processes all events through a single-threaded event loop:

- **Event Queue**: High-performance, buffered event queue (capacity: 10,000+ events)
- **Event Loop**: Single-threaded processing for deterministic behavior
- **Event Router**: Routes events to appropriate state machines and handlers
- **Batch Processing**: Groups related events for efficient processing

**Key Features:**
- Thread-safe event emission from any component
- Guaranteed event ordering and processing
- Circuit breaker patterns for overload protection
- Event priority and queuing strategies

#### 2. State Machine System

Comprehensive state management for all node lifecycle aspects:

##### Node State Machine
```
Initial → Bootstrapping → Connecting → Authenticating → Active → Leaving → Disconnected
                ↓              ↓            ↓           ↓         ↓
            [timeout]     [auth_fail]   [net_error]  [shutdown] [cleanup]
                ↓              ↓            ↓           ↓         ↓
              Failed ←────────────────────────────────────────────┘
```

##### Peer State Machine  
```
Unknown → Discovered → Authenticating → Active → Suspected → Failed
             ↓            ↓              ↓         ↓          ↓
         [connect]    [auth_success]  [timeout]  [failure]  [recover]
             ↓            ↓              ↓         ↓          ↓
         Connecting → Authenticated ←─────┘       Failed ←────┘
```

##### Protocol State Machine
```
Unregistered → Registering → Active → Degraded → Failed → Recovering
                   ↓           ↓        ↓         ↓         ↓
               [success]   [errors]  [timeout]  [retry]  [success]
                   ↓           ↓        ↓         ↓         ↓
               Active ←────────┘        Failed ←──┘       Active
```

#### 3. Event System

**40+ Event Types** covering complete node lifecycle:

**Connection Events:**
- `peer_discovered`, `peer_connected`, `peer_disconnected`
- `connection_established`, `connection_lost`, `connection_recovered`

**Authentication Events:**
- `auth_initiated`, `auth_challenge_sent`, `auth_response_received`
- `auth_success`, `auth_failure`, `auth_timeout`

**Health Events:**
- `heartbeat_sent`, `heartbeat_received`, `heartbeat_missed`
- `peer_suspected`, `peer_failed`, `peer_recovered`

**Protocol Events:**
- `stream_opened`, `stream_closed`, `stream_error`
- `handler_registered`, `handler_lost`, `handler_recovered`

**PubSub Events:**
- `topic_joined`, `topic_left`, `message_published`, `message_received`
- `peer_subscribed`, `peer_unsubscribed`

**Node Lifecycle Events:**
- `node_starting`, `node_ready`, `node_leaving`, `node_stopped`
- `bootstrap_started`, `bootstrap_completed`, `bootstrap_failed`

#### 4. Protocol Integration

**Stream Handler Reactor:**
- Automatic protocol registration and recovery
- Stream lifecycle management
- Error handling and circuit breakers
- Performance monitoring and metrics

**Authentication Protocol:**
- State machine-driven authentication flow
- Automatic retry with exponential backoff  
- TLS certificate validation
- Phonebook synchronization

**Health Check Protocol:**
- Phi Accrual failure detection
- SWIM-style probing (direct + indirect)
- Adaptive suspicion thresholds
- Automatic peer recovery

#### 5. Recovery and Resilience

**Handler Recovery System:**
- Automatic detection of lost stream handlers
- Exponential backoff retry strategy
- Circuit breaker patterns
- Health monitoring and verification

**Peer Recovery System:**
- Failed peer detection and marking
- Automatic reconnection attempts
- Trust score adjustment
- Graceful degradation

**Network Partition Handling:**
- Split-brain detection
- Quorum-based decisions
- Automatic cluster reformation
- Data consistency guarantees

## Implementation Phases

### Phase 1: Core Reactor (Weeks 1-3)
- Event reactor engine
- Basic state machines
- Event routing and processing
- Unit tests and benchmarks

### Phase 2: Authentication Integration (Weeks 4-5)  
- Integrate existing auth state machine
- Stream handler reactor
- Protocol registration system
- Authentication event handling

### Phase 3: Health and Recovery (Weeks 6-8)
- Health check protocols
- Failure detection systems
- Recovery mechanisms
- Peer state management

### Phase 4: PubSub Integration (Weeks 9-10)
- PubSub event handling
- Topic management
- Message routing
- Subscription lifecycle

### Phase 5: Testing and Optimization (Weeks 11-12)
- Performance optimization
- Stress testing
- Integration testing
- Documentation and examples

## Performance Requirements

- **Event Processing**: 100,000+ events/second
- **Latency**: Sub-millisecond event processing
- **Memory**: Bounded memory usage with event queue limits
- **Recovery**: Handler recovery within 100ms
- **Scalability**: Support 1000+ concurrent peers

## Configuration

```go
type ReactorConfig struct {
    EventQueueSize      int           `default:"10000"`
    BatchSize          int           `default:"100"`
    BatchTimeout       time.Duration `default:"1ms"`
    WorkerPoolSize     int           `default:"4"`
    HealthCheckInterval time.Duration `default:"30s"`
    RecoveryPolicy     RecoveryPolicy
    CircuitBreaker     CircuitBreakerConfig
}
```

## Monitoring and Observability

- **Metrics**: Event rates, processing latency, queue depths
- **Tracing**: End-to-end request tracing
- **Logging**: Structured logging with correlation IDs
- **Health Endpoints**: Real-time system health status

## Integration Points

The reactor system integrates with existing Falak components:

- **Node**: Core node lifecycle management
- **Phonebook**: Peer discovery and management  
- **Health Registry**: Peer health tracking
- **Suspicion Manager**: Failure detection
- **TLS Authenticator**: Certificate validation
- **PubSub**: Event-driven messaging

This architecture provides the foundation for a robust, scalable, and self-healing distributed system that can handle the demands of Falak's orchestration-less container execution platform.