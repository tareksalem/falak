# Node Communication and Membership Protocol

This document describes the communication, authentication, and membership management mechanisms implemented in the Falak distributed system based on the current flowchart.

## Table of Contents

1. [Overview](#overview)
2. [Node Discovery & Communication](#node-discovery--communication)
3. [Authentication Protocol](#authentication-protocol)
4. [Cluster Membership Management](#cluster-membership-management)
5. [Connection Maintenance](#connection-maintenance)
6. [Security Considerations](#security-considerations)
7. [Failure Detection](#failure-detection)

## Overview

The Falak distributed system uses a libp2p-based networking layer with GossipSub for pub/sub messaging. Nodes discover and connect to each other using a "phonebook" mechanism, authenticate through a challenge-response protocol, and maintain cluster membership through pub/sub topics.

## Node Discovery & Communication

### Phonebook Mechanism

The phonebook is a distributed registry of nodes in the cluster. It contains:

- Node IDs (cryptographic identities)
- Connection information (multiaddresses)
- Data center labels
- Node capabilities and tags

### Node Boot Process

1. **Initialization**: When a node boots, it initializes the libp2p stack and the phonebook component.

2. **Phonebook Loading**: The node loads its phonebook from:
   - Cached data from previous runs
   - Pre-provisioned seed data provided during deployment

3. **Data Center Awareness**: Peers in the phonebook are grouped by data center labels to:
   - Prioritize connections within the same data center
   - Ensure cross-data-center connectivity for resilience
   - Optimize network traffic patterns

4. **Parallel Connection Strategy**: The node attempts connections to peers across multiple data centers in parallel to:
   - Minimize join latency
   - Increase probability of successful joining
   - Handle partial data center outages gracefully

### Connection Algorithm

```
function attemptConnections():
    peersByDC = groupPeersByDataCenter(phonebook.peers)
    
    for each dataCenter in peersByDC:
        launchParallelTask(connectToDataCenter, dataCenter, peersByDC[dataCenter])
    
    wait for at least one successful connection or all attempts to fail

function connectToDataCenter(dataCenter, peers):
    for each peer in peers:
        backoff = initialBackoff
        while backoff < maxBackoff:
            success = attemptConnection(peer)
            if success:
                return success
            wait for backoff duration
            backoff = backoff * 2
    
    mark dataCenter as unreachable
    return failure
```

## Authentication Protocol

After establishing a connection, nodes must authenticate each other before joining the cluster. The authentication process ensures that only legitimate nodes can join the cluster and establishes secure communication channels.

### Certificate Authority Structure

```
Root CA (cluster authority)
    |
    +--- Node Certificates
```

- A trusted Root CA certificate is pre-installed on all nodes in the cluster
- This CA certificate serves as the fundamental trust anchor for the entire cluster

### Node Identity Components

Each node has:
- **Ed25519 Key Pair**: A public/private key pair that represents the node's identity
- **Node Certificate**: A certificate signed by the Root CA that binds the node's public key to its identity

### Initial Setup

1. **Node Provisioning**:
   - Each node is provisioned with:
     - The cluster's Root CA certificate (pre-installed, trusted)
     - The node's own Ed25519 private key
     - A certificate signed by the Root CA containing the node's public key

2. **Libp2p Identity Configuration**:
   - The Ed25519 key pair is used to configure the node's libp2p identity
   - This identity is used for signing messages and cryptographic operations

### Authentication Flow

1. **TLS Handshake with Mutual Authentication**:
   - When two nodes connect, they perform a TLS handshake
   - Both nodes present their certificates
   - Both nodes verify the peer's certificate against the Root CA certificate
   - This establishes an encrypted connection and verifies cluster membership

2. **Stream Establishment**: 
   - The joining node opens a dedicated authentication stream (`/falak/join/1.0`)
   - This stream uses the secure channel created by the TLS handshake

3. **ClientHello**:
   - The joining node sends:
     - Cluster ID it wishes to join
     - Its Node ID (derived from public key)
     - Its certificate (already verified during TLS handshake)

4. **ServerChallenge**:
   - The receiving node verifies the cluster ID
   - Generates a random nonce with a timestamp
   - Sends this challenge to the joining node

5. **ClientResponse**:
   - The joining node signs the challenge using its private key
   - Sends the signature back to the receiving node

6. **Authentication Verification**:
   - The receiving node verifies the signature using the public key from the certificate
   - This proves the joining node possesses the private key matching its certificate
   - Currently all nodes have full permissions by default (no granular permission checks)

7. **Phonebook Delta**:
   - Upon successful authentication, the receiving node sends a ServerAck containing:
     - Delta update to the phonebook (nodes from data centers the receiving node is aware of)
     - Current cluster state summary for those data centers
   - The joining node performs a partial update of its phonebook:
     - It preserves a copy of its initial phonebook for reference
     - Updates only the data center sections that the responding peer has authority over
     - For example, if joining node connects to peers in DC1 and DC2:
       - The DC1 peer response updates only the DC1 section of the joining node's phonebook
       - The DC2 peer response updates only the DC2 section of the joining node's phonebook
     - This data center-specific merging ensures accurate and compartmentalized updates
     - The merged phonebook contains the union of all data center-specific updates
     - This approach improves resilience by preventing a single peer from overwriting the entire phonebook

### Protocol Buffers Message Definitions

The following Protocol Buffer definitions model the messages exchanged during the authentication process:

```protobuf
syntax = "proto3";
package falak.auth;

// Authentication stream messages
message ClientHello {
  string cluster_id = 1;           // ID of the cluster to join
  string node_id = 2;              // Node's libp2p ID
  bytes certificate = 3;           // Node's certificate (PEM encoded)
  map<string, string> metadata = 4; // Additional metadata (version, capabilities, etc.)
  int64 timestamp = 5;             // Client timestamp for freshness verification
}

message ServerChallenge {
  bytes nonce = 1;                 // Random challenge data
  int64 timestamp = 2;             // Server timestamp
  bool cluster_id_valid = 3;       // Whether the requested cluster ID is valid
  string error_message = 4;        // Error message if cluster_id_valid is false
}

message ClientResponse {
  bytes signature = 1;             // Signature of nonce+timestamp using node's private key
  string node_id = 2;              // Node ID (redundant, for verification)
}

message ServerAck {
  bool auth_success = 1;           // Authentication result
  string error_message = 2;        // Error message if auth failed
  PhonebookDelta phonebook = 3;    // Partial phonebook update
  map<string, bytes> cluster_state = 4; // Relevant cluster state information
  int64 timestamp = 5;             // Server timestamp
}

// Phonebook-related messages
message PhonebookDelta {
  repeated DataCenterUpdate data_centers = 1;
}

message DataCenterUpdate {
  string dc_id = 1;                // Data center identifier
  repeated PeerInfo peers = 2;     // Peers in this data center
  bool is_authoritative = 3;       // Whether sender is authoritative for this DC
}

message PeerInfo {
  string node_id = 1;              // Node ID (libp2p PeerId)
  repeated string multiaddrs = 2;  // libp2p multiaddresses for connection
  map<string, string> labels = 3;  // Node labels/tags
  NodeStatus status = 4;           // Current node status
}

enum NodeStatus {
  UNKNOWN = 0;
  ACTIVE = 1;
  JOINING = 2;
  LEAVING = 3;
  UNREACHABLE = 4;
}
```

### Authentication Protocol Flow with Messages

1. **TLS Handshake with Mutual Authentication**
   - Standard TLS 1.3 handshake
   - No custom protobuf messages at this layer

2. **Stream Establishment & ClientHello**
   - Joining node opens stream: `/falak/join/1.0`
   - Joining node sends `ClientHello` message

3. **ServerChallenge**
   - Receiving node verifies cluster ID
   - Sends `ServerChallenge` with random nonce and timestamp

4. **ClientResponse**
   - Joining node signs the challenge
   - Sends `ClientResponse` with signature

5. **Authentication Verification & ServerAck**
   - Receiving node verifies signature
   - Sends `ServerAck` with authentication result and phonebook delta

### Key Usage Summary

- **TLS Certificates**: Used for secure transport and initial cluster membership verification
- **Ed25519 Keys**: Used for:
  - Node identity in libp2p
  - Signing challenge responses to prove identity
  - Signing pub/sub messages
  - Cryptographic operations related to cluster activities

## Cluster Membership Management

After authentication, nodes participate in cluster-wide membership management.

### Membership Topic

All nodes subscribe to a cluster-specific membership topic: `falak/<clusterId>/membership`

### Membership Events

Nodes publish and listen for the following events:

1. **NodeJoined**:
   - Published when a node successfully joins the cluster
   - Contains node details, capabilities, and connection information

2. **NodeLeft**:
   - Published when a node gracefully leaves the cluster
   - Allows for proper cleanup and task reassignment

3. **NodeUpdate**:
   - Published when a node's capabilities or status changes
   - Enables dynamic load balancing and task assignment

### Membership State Management

Each node maintains a local view of the cluster membership based on:
- Initial phonebook data received during authentication
- Ongoing membership events received via PubSub
- Direct observations (connection status, performance metrics)

### Protocol Buffers Message Definitions

The following Protocol Buffer definitions model the messages exchanged on the membership topic:

```protobuf
syntax = "proto3";
package falak.membership;

// Envelope for all membership messages with verification data
message MembershipMessage {
  string node_id = 1;              // Sender node ID
  int64 timestamp = 2;             // Message timestamp
  bytes signature = 3;             // Signature of the serialized event
  oneof event {
    NodeJoined node_joined = 4;
    NodeLeft node_left = 5;
    NodeUpdate node_update = 6;
  }
}

// Event when a node successfully joins the cluster
message NodeJoined {
  PeerInfo peer = 1;               // Information about the joining peer
  repeated string connected_to = 2; // Node IDs this node is connected to
}

// Event when a node gracefully leaves the cluster
message NodeLeft {
  string node_id = 1;              // ID of the leaving node
  string reason = 2;               // Reason for leaving (shutdown, maintenance, etc.)
}

// Event when a node's status or capabilities change
message NodeUpdate {
  string node_id = 1;              // ID of the updated node
  map<string, string> updated_labels = 2; // New or changed labels
  repeated string removed_labels = 3;     // Labels that were removed
  NodeStatus status = 4;           // Updated status if changed
}

// Reusing PeerInfo and NodeStatus from auth package
message PeerInfo {
  string node_id = 1;              // Node ID (libp2p PeerId)
  repeated string multiaddrs = 2;  // libp2p multiaddresses for connection
  map<string, string> labels = 3;  // Node labels/tags
  NodeStatus status = 4;           // Current node status
}

enum NodeStatus {
  UNKNOWN = 0;
  ACTIVE = 1;
  JOINING = 2;
  LEAVING = 3;
  UNREACHABLE = 4;
}
```

### Membership Topic Protocol Flow

1. **Subscribe to Membership Topic**
   - Node subscribes to `falak/<clusterId>/membership`
   - Uses libp2p GossipSub

2. **Announce Node Joining**
   - Node publishes a `MembershipMessage` containing a `NodeJoined` event
   - Message includes the node's PeerInfo and connections
   - Message is signed with the node's private key

3. **Process Membership Events**
   - When receiving a `MembershipMessage`:
     - Verify signature using sender's public key
     - Process the specific event type (NodeJoined, NodeLeft, NodeUpdate)
     - Update local membership state accordingly

4. **Announce Status Changes**
   - When node status or capabilities change:
     - Create a `NodeUpdate` event with the changes
     - Wrap in a signed `MembershipMessage`
     - Publish to the membership topic

5. **Graceful Departure**
   - Before shutting down:
     - Create a `NodeLeft` event with reason
     - Wrap in a signed `MembershipMessage`
     - Publish to the membership topic


## Falak Node Connectivity & Failure Detection
**Version:** v1.0
**Scope:** Decentralized, orchestration-less cluster health monitoring for Falak nodes.
**Objective:** Maintain an accurate, distributed view of peer health without a central authority, using a combination of gossip-based heartbeats, phi-accrual suspicion detection, SWIM-style probing, quorum-based confirmation, and self-recovery triggers.

---

### 1. Overview
In Falak's decentralized orchestration-less model, failure detection must:
* Operate without a central coordinator.
* Tolerate network partitions and latency.
* Detect node failures quickly while minimizing false positives.
* Allow reintegration without manual intervention.
* Be secure against spoofing or false accusations.

This design combines:
1.  **Heartbeat Topic** — primary health signal.
2.  **Phi Accrual Detector** — adaptive suspicion thresholds.
3.  **SWIM-style Probing** — direct and indirect checks when suspicion arises.
4.  **Suspicion/Refutation & Quorum Verdicts** — distributed agreement before marking failures.
5.  **Reintegration & Anti-flapping** — smooth recovery and stability control.
6.  **Failure Announcement Listening & Self-Reauthentication** — nodes respond to being declared failed.

---

### 2. Core Components

#### 2.1 Heartbeat Topic
* **Topic:** `falak/<clusterId>/heartbeat`
* **Purpose:** Passive, low-cost health monitoring.
* **Publish interval:** `HbPeriod` = 1–2s with &plusmn;20% jitter.
* **Fields:**
    * `clusterId` — scope.
    * `nodeId` — unique ID.
    * `incarnation` — monotonic counter incremented on restart/rejoin.
    * `hlc` — hybrid logical clock (or lamport clock) for ordering.
    * `seq` — per-node heartbeat sequence number.
    * `peersConnected` — quick connectivity summary.
    * `load` — smoothed CPU/memory usage.
    * `digest` — optional phonebook anti-entropy hash.
    * `sig` — signature (libp2p peer identity).
* **Security:** All heartbeats are signed; recipients verify signatures.

#### 2.2 Passive Monitoring & Health Registry
* Subscribe to the heartbeat topic.
* Maintain an **in-memory registry**:
    * `status` — healthy, suspect, failed, quarantine.
    * `lastHbHlc`, `lastHbSeq`, `incarnation`.
    * `phi` — current suspicion score.
    * `rttStats` — rolling ping round-trip time stats.
    * `strikes` — flap counter.
    * `zone`/`rack` — for quorum diversity checks.
* **Conflict resolution:** A higher `incarnation` wins.

#### 2.3 Phi Accrual Failure Detector
* **Goal:** Replace fixed timeout thresholds with adaptive suspicion.
* **Algorithm:**
    * Maintain a rolling window of heartbeat inter-arrival times.
    * Compute $\phi$:
    $$\phi = -log_{10}(1 - F(\Delta t))$$
    Where $\Delta t$ is the time since the last heartbeat and $F$ is the CDF of inter-arrival times.
* **Thresholds:**
    * $\phi_{suspect}$ = 5
    * $\phi_{fail\_edge}$ = 9
    * $\phi_{fail\_wan}$ = 12
* Adapts to latency variation and reduces false positives.

#### 2.4 SWIM-Style Probing (Fallback)
* **Trigger:** $\phi \ge \phi_{suspect}$ for a peer.
* **Procedure:**
    1.  **Direct ping**: `/falak/ping/1.0` RPC with a nonce; expect an echoed nonce + HLC.
    2.  If the direct ping fails:
        * Select $k$ random healthy peers.
        * Request each to ping the target (indirect probe).
        * Require a response within 300–500ms.
    3.  If all fail, raise suspicion.

#### 2.5 Distributed Suspicion & Refutation
* **Suspicion topic:** `falak/<clusterId>/suspicion`
    * Contains `targetNodeId`, `accuserNodeId`, `accuserIncarnation`, `hlc`, `phi`, and optional witnesses.
    * Signed by the accuser.
* **Refutation topic:** `falak/<clusterId>/refute`
    * Sent if a node can prove a suspected node is alive via a direct ACK.
    * Contains `targetNodeId`, `refuterNodeId`, `targetIncarnation`, `hlc`, and optional `recentAck`.
* **Mark failed** only if:
    1.  $\phi \ge \phi_{fail}$.
    2.  SWIM probes fail.
    3.  $\ge K$ suspicions from diverse zones/racks.

#### 2.6 Reintegration & Conflict Resolution
* **Reintegration:**
    * A failed node publishes a heartbeat with a higher `incarnation`.
    * The node is marked `Healthy` after a `coolOff` period (5s) to avoid flapping.
* **Conflict handling:**
    * Use `incarnation` + `hlc` to resolve split-brain events.

#### 2.7 Anti-flapping & Quarantine
* If a node toggles `failed \leftrightarrow healthy` frequently, it is moved to quarantine.
* **Quarantine rules:**
    * Exclude from elections.
    * Duration = `T_quarantine` (e.g., 30s).

### 3. Health State Machine

**States:**
* `Healthy` — regular heartbeats.
* `Suspect` — $\phi$ is above threshold, awaiting probe/quorum.
* `Failed` — quorum confirmed failure.
* `Rejoining` — self-recovery after being marked failed.
* `Quarantine` — stability control.

**Transitions:**
* Healthy &rarr; Suspect: $\phi \ge \phi_{suspect}$ and SWIM probe fails.
* Suspect &rarr; Failed: $\phi \ge \phi_{fail}$ and quorum is met.
* Suspect/Failed &rarr; Healthy: a higher incarnation heartbeat is received.
* Any &rarr; Rejoining: a quorum/failed message names self with a matching incarnation.
* Rejoining &rarr; Healthy: successful authentication and cool-off complete.

### 4. Recovery Actions

#### 4.1 Isolation Detection (Self)
* **Trigger:** If >X% of peers have $\phi \ge \phi_{suspect}$ and SWIM probes fail.
* **Actions:**
    * Pause elections.
    * Attempt reconnection via bootstrap/mDNS.
    * Increment `incarnation` on rejoin.

#### 4.2 Phonebook Anti-Entropy
* Heartbeats carry an optional digest.
* On mismatch, request a delta from `/falak/phonebook/1.0`.

### 5. Security Considerations
* All messages are signed (libp2p identity).
* Rate-limit suspicions per origin.
* Require quorum diversity.
* Ignore suspicions if a recent direct ACK exists.

### 6. Parameters (Defaults)

| Parameter             | Value              |
|-----------------------|--------------------|
| `HbPeriod`            | 1–2s &plusmn;20% jitter |
| $\phi_{suspect}$      | 5                  |
| $\phi_{fail\_edge}$   | 9                  |
| $\phi_{fail\_wan}$    | 12                 |
| SWIM ping timeout     | 300–500ms          |
| SWIM indirect fanout  | 3                  |
| Quorum K              | max(2, log2(N))    |
| Cool-off              | 5s                 |
| Quarantine            | 30s                |

### 7. Algorithm Summary

**Heartbeat Handling:**
```
onHeartbeat(hb):
    if hb.incarnation < registry[hb.nodeId].incarnation:
        discard
    updatePhi(hb.nodeId, interArrival)
    updateRegistry(hb)
    if registry[hb.nodeId].status in {Suspect, Failed} and hb.incarnation > oldIncarnation:
        markHealthy(hb.nodeId) after coolOff
```

**Periodic Check:**
```
for each peer in registry:
    phi = currentPhi(peer)
    if status == Healthy and phi >= phi_suspect:
        if SWIMprobe(peer) fails:
            markSuspect(peer)
            publishSuspicion(peer, phi)
    if status == Suspect and phi >= phi_fail and quorumMet(peer):
        markFailed(peer)
```

### 8. Quorum Verdict & Failure Announcements (with Self Re-Auth)

#### 8.1 Topics
* **Quorum Verdicts:** `falak/<clusterId>/quorum`
* **Failure Announcements:** `falak/<clusterId>/failed`

#### 8.2 Messages

**QuorumVerdict**
* `cluster_id`
* `target_node_id`
* `verdict` // e.g., "failed"
* `hlc`
* `target_incarnation`
* `accuser_ids`
* `accuser_zones`
* `accuser_sigs`
* `quorum_agg_sig`

**FailureAnnouncement**
* `cluster_id`
* `target_node_id`
* `hlc`
* `target_incarnation`
* `quorum_ref` // hash/pointer to QuorumVerdict
* `announcer_sig`

#### 8.3 Behavior

**On QuorumVerdict (target $\ne$ self):**
* Verify quorum proof & diversity.
* Mark target failed unless contradicted by a higher incarnation heartbeat + direct ACK.

**On QuorumVerdict (target == self):**
* If `target_incarnation >= self.incarnation` and HLC is fresh:
    * Enter `Rejoining` state.
    * Increment `incarnation++`.
    * Re-run the authentication flow (`/falak/join/1.0`).
    * Publish an immediate heartbeat.
    * Resume after cool-off.

**On FailureAnnouncement:** Same as above.

#### 8.4 Self-Reauthentication Flow
1.  **Trigger:** Receive a valid quorum/failed message naming self.
2.  **Pause** elections & workloads.
3.  **Increment `incarnation`** to invalidate old suspicions.
4.  **Re-init authentication flow**:
    * `ClientHello` &rarr; `ServerChallenge` &rarr; `ClientResponse` &rarr; `ServerAck`.
5.  **Publish a heartbeat** with the new incarnation.
6.  **Cool-off** before normal participation.

#### 8.5 DoS Protection
* Ignore stale self-failure messages (`target_incarnation < self.incarnation` or HLC is older than the last processed).
* Rate-limit self-rejoins.

### 9. Advantages
* **Adaptive:** The Phi Accrual detector handles variable network latency.
* **Partition-Tolerant:** SWIM-style indirect probes prevent false positives during network partitions.
* **Secure:** Signed messages and quorum diversity protect against malicious behavior.
* **Fast Recovery:** Automatic self-reauthentication allows nodes to rejoin the cluster quickly after a perceived failure.
* **Convergent:** The distributed quorum mechanism ensures that all peers quickly agree on the cluster state.

#### Protocol Buffers Message Definitions

The following Protocol Buffer definitions model the messages exchanged for failure detection:

```protobuf
syntax = "proto3";
package falak.health;

// Heartbeat message
message Heartbeat {
  string cluster_id = 1;
  string node_id = 2;
  uint64 incarnation = 3;
  uint64 hlc = 4;            // Hybrid logical clock
  uint64 seq = 5;            // Sequence number
  uint32 peers_connected = 6;
  Load load = 7;
  bytes digest = 8;          // Phonebook hash for anti-entropy
  bytes signature = 9;       // Signature using libp2p identity
}

message Load {
  float cpu = 1;             // 0.0-1.0 CPU utilization
  float memory = 2;          // 0.0-1.0 memory utilization
  uint32 conns = 3;          // Number of active connections
}

// Suspicion message
message Suspicion {
  string cluster_id = 1;
  string target_node_id = 2;
  string accuser_node_id = 3;
  uint64 accuser_incarnation = 4;
  uint64 hlc = 5;
  float phi = 6;             // Current suspicion level
  repeated Witness witnesses = 7; // Optional indirect probe results
  bytes signature = 8;       // Signature using libp2p identity
}

message Witness {
  string node_id = 1;
  uint64 hlc = 2;
  bool probe_result = 3;
  bytes signature = 4;
}

// Refutation message
message Refutation {
  string cluster_id = 1;
  string target_node_id = 2;
  string refuter_node_id = 3;
  uint64 target_incarnation = 4;
  uint64 hlc = 5;
  bytes recent_ack = 6;      // Proof of communication
  bytes signature = 7;       // Signature using libp2p identity
}

// Quorum verdict message
message QuorumVerdict {
  string cluster_id = 1;
  string target_node_id = 2;
  string verdict = 3;        // "failed", etc.
  uint64 hlc = 4;
  uint64 target_incarnation = 5;
  repeated string accuser_ids = 6;
  repeated string accuser_zones = 7;
  repeated bytes accuser_sigs = 8;
  bytes quorum_agg_sig = 9;  // Aggregate signature
}

// Failure announcement message
message FailureAnnouncement {
  string cluster_id = 1;
  string target_node_id = 2;
  uint64 hlc = 3;
  uint64 target_incarnation = 4;
  bytes quorum_ref = 5;      // Hash/pointer to QuorumVerdict
  bytes announcer_sig = 6;   // Signature using libp2p identity
}

// Ping request and response for SWIM probing
message PingRequest {
  string cluster_id = 1;
  string node_id = 2;
  bytes nonce = 3;
  uint64 hlc = 4;
  bytes signature = 5;
}

message PingResponse {
  string cluster_id = 1;
  string node_id = 2;
  bytes nonce = 3;           // Echo of request nonce
  uint64 hlc = 4;
  bytes signature = 5;
}

// Indirect probe request
message IndirectProbeRequest {
  string cluster_id = 1;
  string requester_node_id = 2;
  string target_node_id = 3;
  bytes nonce = 4;
  uint64 hlc = 5;
  bytes signature = 6;
}

message IndirectProbeResponse {
  string cluster_id = 1;
  string prober_node_id = 2;
  string target_node_id = 3;
  bool probe_result = 4;
  uint64 hlc = 5;
  bytes signature = 6;
}
```

---

This document describes the current implementation based on the flowchart. Future enhancements may include more sophisticated failure detection, consensus mechanisms, and recovery protocols.
