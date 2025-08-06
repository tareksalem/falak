# FALAK: Orbit-Driven Decentralized Container Execution Platform

## 🚀 Overview

**Falak** is a next-generation decentralized execution platform where deployable application metadata (called **capsules**) travel through a mesh of compute **nodes** in dynamic orbital paths.

Instead of relying on a central orchestrator (like Kubernetes or Cloud Run), Falak introduces an **orbit-based execution model**. Nodes self-elect to execute workloads based on proximity, available resources, and gravity-like affinity — resulting in faster, more resilient, and highly available container startup.


<video src="https://raw.githubusercontent.com/tareksalem/falak/main/assets/falak-workflow-demo.mp4" controls width="640">
  Your browser does not support the video tag.
</video>
---

## 🧩 Core Components & Concepts

### 1. **Capsules**

* Lightweight metadata describing deployable apps
* Point to a shared snapshot image (pre-initialized container)
* Orbit across the mesh, not bound to a node

### 2. **Nodes**

* Compute entities capable of running containers
* Participate in the mesh, handle election, and run capsules

### 3. **Orbits**

* Logical paths capsules follow
* Nodes observe orbits based on resource proximity and affinity

### 4. **Gravity**

* Calculated value per node based on:

  * Geo location
  * CPU/memory capacity
  * Historical latency
  * Capsule metadata matching (e.g., tag affinity)

### 5. **Snapshots**

* Capsules refer to a pre-initialized container snapshot
* Enables near-instant spin-up on any node

### 6. **Event Mesh**

* Decentralized pub/sub fabric
* All events: node join/leave, elections, syncs, failures, triggers

---

## 🔄 Detailed Workflow

### 🔍 1. **Capsule Discovery & Sync**

* Capsules are gossiped through the mesh using event propagation
* Nodes maintain a local registry of observed capsules per orbit
* Nodes cache capsule metadata with TTL for minimal memory footprint

### ⚠️ 2. **Node Failure & Drop Detection**

* Nodes emit periodic heartbeat events
* Neighbor nodes listen and maintain a TTL
* If a heartbeat expires, a **node-drop event** is broadcast
* Capsules that were observed only by the lost node are marked **at risk** and re-injected

### 🗳️ 3. **Election Triggering & Process**

* A client request enters the mesh (HTTP, socket, etc.)
* Nodes observing the capsule in orbit prepare to **self-elect**
* Each candidate node:

  * Calculates an **election weight** based on gravity function
  * Broadcasts a **vote proposal event** with its intent to serve

### 🧮 4. **Election Resolution**

* A quorum (e.g., 3+ other nodes) is needed to **validate** a node’s claim
* When a node receives sufficient support (via vote-ack events), it proceeds
* Other candidates **withdraw automatically** upon noticing majority consensus

### 🔁 5. **Consequence & Failover**

* If elected node fails before starting:

  * A **timeout event** triggers re-election
  * Other nodes restart election cycle with new context
* Ensures **automatic failover and HA** with minimal overhead

---

## 💼 Workload Distribution & High Availability

* Multiple nodes can observe a capsule — enabling **multi-region presence**
* Workloads are **stateless** and can be restarted quickly from snapshots
* Nodes cache active capsule states to reduce cold starts
* HA is achieved via:

  * Redundant observability (capsule seen by many nodes)
  * Auto-replication of snapshot layers
  * Hot standby via lower-weight nodes

---

## 🌐 Decentralized Communication Model

Falak is designed around **peer-to-peer communication** between nodes. Each node is:

* Autonomous and stateless
* Connected in a **distributed mesh network**
* Aware of nearby capsules via **gossip or event propagation protocols**

### 📡 Node-to-Node Communication:

* Uses **event-driven pub/sub mechanisms** (e.g., NATS, gossip, or custom distributed event queue)
* Nodes publish updates about capsule sightings, availability, and resource changes
* Nodes **synchronize capsule metadata and orbital presence** without relying on a central brain

---

## 🧠 Orchestration-less Behavior

There is **no central orchestrator**. Instead:

* Capsules move in logical orbits around the network
* Nodes **observe** capsules in their orbital zone
* When a request arrives, **nodes self-elect** based on:

  * Proximity to capsule
  * Resource availability
  * Latency to requester
  * Capsule execution affinity
* One or more nodes serve the request based on the election result

This model replaces scheduling queues with **gravity-aware execution locality**.

---

## 🛰️ Capsule and Snapshot Model

Each deployable application is represented as a **Capsule**:

* Metadata + snapshot pointer
* Contains: name, version, tags, resource needs, and container snapshot reference

### 💾 Snapshot Concept:

* When a capsule is first created, Falak snapshots the container at its **initialized state** (via CRIU or container-native snapshotting)
* Capsules only carry metadata; nodes use a **shared snapshot volume or distributed image registry** to pull exact state
* Enables **fast-start** containers with consistent behavior across nodes

---

## 🌌 Orbit and Gravity Driven Execution

### 🌀 Orbit

* Capsules travel through **virtual orbits**, not stored on a specific node
* Orbits represent **logical zones** of visibility for nodes

### 🪐 Nodes

* Each node is a gravitational body
* It can observe capsules based on **how close its gravity is** to the capsule's orbital path

### 🧲 Gravity

* A function of:

  * CPU & memory capacity
  * Network proximity
  * Capsule tag affinity (e.g., GPU, region)
  * Latency to client
* Nodes with **higher gravity** to a capsule orbit have stronger election chances

---

## ⚡ Event-Driven Everything

Falak’s architecture is entirely **event-driven**, including:

| Component         | Event Role                                              |
| ----------------- | ------------------------------------------------------- |
| Capsule Sync      | Gossip-based events keep capsules orbiting across nodes |
| Node Discovery    | Heartbeat and health events maintain network mesh       |
| Execution Trigger | Incoming request triggers election event                |
| Election          | Nodes vote via local events and resolve winner          |
| Failure Recovery  | Timeout and re-election events ensure high availability |

Benefits:

* **No polling**
* **Loose coupling**
* **Low-latency elections and failover**

---

## 🔄 Capsule Lifecycle

1. **Build** → Developer ships capsule with container + metadata
2. **Snapshot** → Platform snapshots it into an execution-ready image
3. **Orbiting** → Metadata propagates across the mesh
4. **Request** → User/client sends request (HTTP, WebSocket, etc.)
5. **Election** → Nodes in orbit self-elect to run the capsule
6. **Execute** → Winning node spins up container from snapshot
7. **Result** → Response is served and optionally cached

---

## 📈 Why Falak?

| Traditional Platform  | Falak                         |
| --------------------- | ----------------------------- |
| Centralized scheduler | Decentralized node mesh       |
| Delayed cold starts   | Snapshot-based warm startup   |
| Complex orchestration | Orbit-based self-election     |
| Polling for health    | Event-driven everything       |
| Vendor lock-in        | Edge-friendly and open design |

---

## 🔮 Vision & Extensibility

Falak aims to become a **developer-first, container-native orbit mesh** where deployment is:

* **Location aware**
* **Highly available** by design
* **Cost-optimized** using node gravity

In future releases:

* Capsule replication across geographies
* Capsule versioning and rollback
* Orbit visualization and debugging tools

---

## 📚 Glossary

* **Capsule**: Deployable app metadata + snapshot ref
* **Orbit**: Logical path where capsule metadata moves
* **Node**: Compute instance capable of executing capsules
* **Gravity**: Score used to determine capsule-node affinity
* **Snapshot**: Pre-initialized container ready for fast startup
* **Election**: Event-driven mechanism to determine executor

## 💼 Comparison Table

| Feature                | Falak                                    | Kubernetes                                   | Nomad                               |
| ---------------------- | ---------------------------------------- | -------------------------------------------- | ----------------------------------- |
| **Architecture**       | Decentralized, event-driven mesh         | Centralized scheduler & etcd                 | Server-client model with Raft       |
| **Orchestration**      | No orchestrator                          | Required controller manager                  | Required central Nomad server       |
| **Startup Time**       | Instant via snapshot                     | Cold start from container registry           | Cold start from container registry  |
| **Capsule Movement**   | Orbit-based logical movement             | Static pod assignments                       | Static job assignments              |
| **Node Communication** | Gossip + event pub/sub                   | Controller communication via API             | Server heartbeat and registration   |
| **Election**           | Gravity-based dynamic election           | Static pod scheduling rules                  | Job placement via central scheduler |
| **Failure Recovery**   | Automatic re-election from orbit         | Replication controllers                      | Task restarts by Nomad server       |
| **High Availability**  | Built-in via orbit overlap               | ReplicaSet or StatefulSet required           | Redundant clients, HA mode          |
| **Snapshot Execution** | Yes (container initialized state)        | No (image + init run)                        | No (image + init run)               |
| **Best Use Case**      | Edge workloads, bursty apps, low-latency | Enterprise workloads, Kubernetes-native apps | Batch jobs, flexible orchestration  |

---


---

> **Falak** is not just a platform. It's a **cosmic shift** in how we think about running containers — not orchestrated, but discovered.

---

## 🧠 Get Involved

Want to contribute to Falak's gravity engine, capsule format, or event bus? Reach out at \[email or repo]. Let’s build a universe where apps move like celestial bodies.
