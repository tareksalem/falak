# Falak Node Communication & Authentication Workflow

## Overview

In Falak’s decentralized, orchestration-less design:

- **Cluster** = A logical grouping of nodes that share workloads and membership state.
- **Node** = An individual peer in one or more clusters.
- A node can be a member of **one or more clusters**.
- If **no cluster is provided** at initialization, the node automatically joins a single default cluster named `"default"`.

This document describes:
- The boot process for a node.
- How nodes discover and connect to peers using a **phonebook**.
- How authentication is performed before joining a cluster's pubsub mesh.
- Which topics are joined after authentication.

---

## Concepts

### Cluster
A **Cluster** is identified by a unique `ClusterID` string. All nodes within a cluster share:
- The same **membership topic**.
- The same authentication domain (PSK or invite).

### Node
A **Node** has:
- A **NodeID** (derived from its libp2p peer ID).
- A list of cluster memberships.
- A **phonebook** of known peers.

### Phonebook
The **Phonebook** is a local list of peer entries:
- `PeerID`
- Multiaddrs
- Trust score
- Last seen timestamp

The phonebook is:
- Used at boot to find peers.
- Updated on every successful authentication.
- Persisted for crash recovery.

### Authentication
Nodes **must authenticate** with at least one existing peer in a cluster before joining its pubsub mesh.
- Authentication method can be **PSK** (MVP) or **invite token**.
- Uses a dedicated libp2p direct stream (`/falak/join/1.0`).
- Once authenticated, a node can subscribe to the cluster’s topics and participate in membership gossip.

---

## Protocols & Topics

### Direct Stream Protocols
- **Join Protocol**: `/falak/join/1.0`
  - Handles authentication and admission.
  - Exchanges:
    1. `ClientHello`
    2. `ServerChallenge`
    3. `ClientResponse`
    4. `ServerAck` (includes phonebook delta)

- **Phonebook Sync** (optional at this stage): `/falak/phonebook/1.0`
  - Periodic delta pulls from peers to refresh phonebook.

### Pubsub Topics
After successful authentication to a cluster, a node subscribes to:

- **Membership Topic**:  
  `falak/<clusterId>/membership`  
  Carries:
  - `NodeJoined`
  - `NodeLeft`
  - `NodeUpdate`

- **Election Topics** *(created as needed)*:  
  `falak/<clusterId>/elect/<capsuleId>`  
  Used for capsule election coordination.

- **Run Topics** *(created as needed)*:  
  `falak/<clusterId>/run/<capsuleId>`  
  Used for announcing capsule start/stop events.

---

## Message Schemas

### ClientHello
```json
{
  "clusterId": "string",
  "nodeId": "string",
  "addrs": ["string"],
  "caps": {"tags": ["string"], "ver": "string"},
  "nonce": "string",
  "auth": {
    "type": "psk",
    "payload": "string"
  }
}
