# Falak Technical Architecture

## Overview

Falak is an **orchestration-less**, decentralized, event-driven mesh of nodes designed to deploy and run containerized and function-based applications using an emergent, self-electing execution model.

Unlike traditional orchestrators like Kubernetes or Nomad, Falak removes the concept of a central scheduler. Instead, nodes form a **peer-to-peer network** where applications (called **capsules**) orbit logically, and nodes **self-elect** to run them based on proximity, resources, and affinity (a concept called **gravity**).

## Purpose

The purpose of this project is to:

* 🪙 **Reduce cost** by eliminating the need for central orchestrators, control planes, or coordination services
* ⚖️ **Achieve resilience and scalability** through distributed, fault-tolerant execution across self-organizing nodes
* ⚡ **Enable rapid container start-up** using container snapshotting at the initialized state
* 🤖 **Support a wide range of workloads**, including:

  * Stateless function-based workloads (auto-start, auto-shutdown)
  * Stateful, long-running containerized apps
* 🛠️ **Support both WASM and Rust-native workloads**, along with container-based applications
* 🚀 Be more **cost-effective, scalable, and faster** than Kubernetes, Nomad, and Google Cloud Run

---

## Components

### Clusters

A **cluster** is a logical group of connected nodes. A cluster can be:

* Region-based (e.g., Dubai Cluster, Paris Cluster)
* Workload-based (e.g., GPU-intensive cluster, Edge APIs cluster)

Clusters form **organically** as nodes announce their presence and observe capsules orbiting relevant workloads.

### Nodes

Nodes are the **computational unit** of Falak. Each node is:

* Stateless and autonomous
* Connected to other nodes using libp2p
* Able to observe, elect, and run capsules
* Capable of failing and rejoining without affecting system-wide behavior

Each node:

* Participates in orbit topics via pub/sub
* Sends periodic heartbeats
* Calculates gravity to self-elect for execution

### Capsules

**Capsules** are the **unit of deployment** in Falak. A capsule is metadata + snapshot reference:

* Name, version, tags
* Resource requirements (CPU, RAM, runtime type)
* Reference to a container snapshot or WASM binary

Capsules are **not owned** by any node. They **orbit** in the mesh and are visible to any observing node.

### Orbits

Orbits are **logical topics** capsules are associated with.

* Each orbit is represented by a topic in the pub/sub mesh (e.g. `/orbit/api`, `/orbit/gpu`)
* Nodes **subscribe to orbits** based on their affinity, location, and capacity
* Orbit presence is TTL-based — if a capsule is not observed again, it expires

### Gravity

**Gravity** is a function that determines a node’s suitability to run a capsule:

```text
gravity = f(cpu_score, mem_score, latency, tag_affinity)
```

Nodes with higher gravity to a capsule orbit **self-elect** to run the capsule. Gravity is calculated per request or on demand.

### Snapshots

Snapshots are **pre-initialized container states** created once per capsule.

* Stored in a shared OCI registry or distributed peer cache
* Used to start apps near-instantly on any node
* Can be WASM binaries, Firecracker microVMs, or OCI-compatible images

### Momentum (Execution)

**Momentum** is the concept of a node **executing a capsule** after winning an election.

* When a request arrives, eligible nodes self-elect
* The winner achieves **momentum** and pulls the snapshot to start the capsule
* Momentum is temporary; another node may take over upon failure or shutdown

---

## Behaviors

### Node Communication

Nodes communicate over **libp2p** using:

* Pub/Sub (`gossipsub`) for capsule and orbit visibility
* Streams for direct protocols (e.g., vote propagation, snapshot sharing)
* Heartbeat messages to maintain mesh integrity

### Node Failure Detection

Each node:

* Sends periodic `heartbeat` messages
* Monitors TTLs from peers
* If a peer’s heartbeat expires, emits a `node.drop` event
* Capsules only visible to the failed node are marked **at-risk** and re-gossiped

### Capsules Moving

Capsules **move logically**, not physically.

* Movement = Metadata propagation via pub/sub
* They are not transferred or deployed — they **exist in orbit**
* All nodes in an orbit can observe and potentially elect to run them

### Election Process

* A client request arrives (HTTP, gRPC, etc.)
* Nodes observing the capsule **calculate gravity**
* Nodes **broadcast vote proposals** via event
* Nodes with highest quorum win (vote acks received from 3+ peers)
* Winner proceeds to execution (momentum)

### Running Application Process

* Winning node fetches snapshot if not cached
* Starts the container or WASM
* Handles input/output and serves client request
* Once done (or after TTL), the runtime is torn down

### Application Running Lifecycle

* **Start**: Elected and snapshot restored
* **Active**: Serving request or waiting for workload
* **Idle/Timeout**: Automatically stops or pauses
* **Failure**: Triggers re-election via timeout event
* **End**: Cleaned up by Container Runtime

### Capsule Lifecycle

1. **Build**: Developer defines capsule + snapshot
2. **Announce**: Capsule enters the mesh via pubsub
3. **Orbit**: Nodes subscribe and cache capsule metadata
4. **Request**: Client request triggers election
5. **Execution**: Winning node achieves momentum
6. **Expire**: Orbit TTL passes without renew
7. **Update**: Newer version may override old capsule

---

Let me know if you’d like this in Markdown, PDF, or expanded into detailed implementation notes.
