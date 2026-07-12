# Falak Configuration Guide

> **Note:** This document is actively maintained. New configuration sections will be added as features are implemented (capsules, orbits, gravity, runtime, etc.).

## Overview

Falak uses [CUE](https://cuelang.org/) for configuration. CUE provides:

- **Type-safe schemas** — invalid config is caught before the node starts
- **IDE autocomplete** — import the Falak schema for editor support
- **Validation constraints** — PSK length, port ranges, required fields
- **Default values** — only specify what you need to change

## Quick Start

### 1. Create a config file

```cue
// node1.cue
name: "node1"
port: 4001

clusters: {
    "my-cluster": {
        psk: "my-secret-key-must-be-32-chars!!"
    }
}
```

### 2. Run the node

```bash
falak daemon start --config=node1.cue
```

---

## Configuration Reference

### Node

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | **required** | Unique node identifier. Used for deterministic peer ID and data directory. |
| `port` | int (0-65535) | `0` | TCP port for libp2p. `0` = random available port. |
| `listen` | string | — | Multiaddr override for listen address (e.g., `"/ip4/0.0.0.0/tcp/4001"`). Overrides `port`. |
| `data_dir` | string | OS default | Path for persistent storage (phonebook, keys, certs). Default: `~/.local/share/falak/<name>` (Linux), `~/Library/Application Support/falak/<name>` (macOS). |
| `region` | string | `"default"` | Geographic region identifier. |
| `datacenter` | string | `"default"` | Datacenter within the region. |
| `log_level` | string | `"info"` | Logging verbosity: `"debug"`, `"info"`, `"warn"`, `"error"`. |
| `clusters` | map | **required** | Map of cluster path → cluster config. A node can join multiple clusters. |
| `health` | object | defaults | SWIM health monitoring settings. |

### Cluster

Each entry in `clusters` is keyed by the cluster path (e.g., `"prod/dc1"`, `"staging/us-east/dc2"`).

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `psk` | string (≥32 chars) | **required** | Pre-Shared Key for cluster authentication. All nodes in the cluster must use the same PSK. |
| `bootstrap` | list of strings | `[]` | Multiaddr list of existing cluster nodes. Empty for the first node in a cluster. |
| `certificates` | object | — | External PKI config. Omit for auto mode (PSK-derived certificates). |

### Certificates (External PKI)

When the `certificates` block is present, the cluster uses external CA mode instead of auto mode.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `ca_cert` | string | **required** | Path to CA certificate (PEM). All nodes in the cluster must trust the same CA. |
| `ca_key` | string | — | Path to CA private key (PEM). When set, this node can sign certificates for joining nodes. |
| `node_cert` | string | — | Path to pre-signed node certificate (PEM). |
| `node_key` | string | — | Path to node private key (PEM). |

### Health (SWIM Protocol)

All health settings are optional with sensible defaults. Thresholds are automatically scaled by cluster size at runtime (smaller clusters detect failures faster).

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `protocol_period` | duration | `"2s"` | How often each node probes a random peer. |
| `ping_timeout` | duration | `"500ms"` | Max wait time for a ping response. |
| `quarantine_timeout` | duration | `"30s"` | Time in quarantine before a node is removed. |
| `score_increment` | float | `0.5` | Score added per failed probe. |
| `suspected_threshold` | float | `4.0` | Base score to enter suspected state. |
| `quarantine_threshold` | float | `10.0` | Base score to trigger quarantine. |
| `max_responders` | int | `2` | Number of longest-lived nodes that self-select for indirect probing. |
| `quarantine_check_interval` | duration | `"5s"` | How often to check quarantined nodes for timeout. |
| `quarantine_probe_interval` | duration | `"10s"` | How often to publish probe requests for quarantined peers. |

### Network

Falak's network subsystem provisions one Linux bridge per capsule group, an
encrypted VXLAN overlay between hosting nodes (transport-mode IPsec with
AES-GCM), and a per-bridge DNS responder that serves bare capsule names from
a gossip-fed endpoint registry. The entire subsystem is **Linux-only** in
Phase 11A.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `network.enabled` | bool | `true` | Master switch for the network subsystem. Set `false` to disable bridges, overlay, and the DNS responder (containers fall back to whatever Podman wires by default). |
| `network.bridge_subnet_pool` | CIDR | `"10.88.0.0/16"` | IPv4 super-pool carved into `/24` subnets, one per group. The pool MUST NOT overlap any underlay or VPN network on the host. |
| `network.bridge_mtu` | int | autodetect | Bridge MTU. When unset, the daemon derives it as `underlay_mtu - 110` (VXLAN+IPsec headroom). Override only when the default fails (e.g. unusual encapsulation). |
| `network.underlay_mtu` | int | autodetect | MTU of the default-route interface. Read from `/proc/net/route` + `/sys/class/net/<iface>/mtu` on Linux. Override on hosts where autodetection picks the wrong NIC. |
| `network.iptables_takeover` | bool | `false` | Opt-in: when `true`, the daemon installs its FORWARD rules even when firewalld/ufw/nftables is detected as the iptables manager. Use only when you have coordinated with the host's firewall management. |
| `network.endpoint_ttl` | duration | `"30s"` | Publisher TTL stamped on every endpoint gossip record. Receivers evict records past `2 × ttl`. Lower values surface dead replicas faster at the cost of more gossip traffic. |

#### Linux requirements

Falak requires the following on every node when `network.enabled=true`:

- **Linux kernel modules**: `vxlan` (VXLAN device) and `esp4` + `xfrm` (IPsec
  transport mode). the daemon refuses to start if any are missing.
- **Capabilities**: `CAP_NET_ADMIN` to manage netlink, FDB entries, XFRM
  state, and iptables rules. Run the daemon as root or set capabilities on the
  binary.
- **rp_filter**: `/proc/sys/net/ipv4/conf/all/rp_filter` must be `1`
  (strict mode). the daemon refuses to start otherwise; override only in dev
  builds via the manager's `WithRPFilterCheckDisabled` option.
- **Iptables management**: no other manager (firewalld, ufw, nftables) may
  own the FORWARD chain unless `network.iptables_takeover` is `true`.

When any of these checks fail, the daemon refuses to start with a clear,
actionable error. The subsystem is binary: either everything is wired or
nothing is — there is no degraded "no network" mode.

### Runtime (crash & removal detection)

The runtime handler detects container crashes and removals using two
cooperating layers:

- **Event stream (primary, low-latency).** A single long-lived consumer
  subscribes to the container backend's lifecycle event stream
  (`GET /libpod/events` on Podman). A `died` event drives crash recovery
  (local restart up to the capsule's `restart_limit`, else re-election); a
  `remove` event is terminal and triggers re-election immediately. The
  stream is best-effort — it drops on a backend socket restart — so on a
  dropped stream the consumer reconciles every owned container and
  reconnects with jittered backoff.
- **Reconcile sweep (correctness backstop).** A periodic sweep inspects
  every owned container. A not-found result (the container was removed
  out-of-band) is terminal → re-election. A stopped/failed status takes
  the crash path. Repeated transient inspect errors escalate to
  re-election once `max_inspect_errors` consecutive failures accumulate
  (a single blip followed by recovery never escalates).

Falak's own teardowns (rolling-update swap, user stop, snapshot
checkpoint) are added to an internal ignore set before the intentional
stop/remove, so they never self-trigger a re-election.

These tunables are set via functional options on the runtime handler
(`runtime.With…`); they have production-sensible defaults and rarely need
overriding.

| Option | Default | Description |
|--------|---------|-------------|
| `WithReconcileInterval(d)` | `30s` | Cadence of the reconcile backstop sweep. The event stream carries the fast path, so this is deliberately long. |
| `WithMaxInspectErrors(n)` | `5` | Consecutive transient inspect failures tolerated during reconcile before a container is declared "runtime unreachable" and re-elected. |
| `WithEventReconnectBackoff(d)` | `1s` | Base (jittered) delay between event-stream reconnect attempts after the stream drops. |

### Snapshot replication (O11 — HA fast-restart)

After a successful cold-start capture the holder proactively replicates
the CRIU snapshot to **K** standby peers so a re-election winner can
restore locally instead of cold-starting when the original holder dies.
Replication is holder-driven and push-after-capture: the holder picks
targets and asks each (over `/falak/snapshot/replicate/1.0`) to pull the
bytes back via the existing transfer protocol. It runs on a bounded
background worker pool and never blocks the capture path or container
start.

Targets are selected for **failure-domain spread** (prefer a different
datacenter/node than the holder and than each other), filtered by
**disk headroom** (never push a large archive onto a nearly-full node),
and tiebroken by **power-of-two-choices** on a suitability score (sample a
couple of candidates, pick the better — NOT the global best, which would
herd every replica onto the beefiest nodes). A received standby is
**pinned** (protected from over-cap/LRU eviction up to its TTL) and
re-broadcast so every node's availability index reflects all K holders.
The index is reconciled against membership: a `NodeFailed`/`NodeDeparting`
prunes that node from the index so the puller never targets a dead holder.

These tunables are functional options on `snapshot.NewReplicator`
(`snapshot.WithReplication…`), with production-sensible defaults:

| Option | Default | Description |
|--------|---------|-------------|
| `WithReplicationFactor(k)` | `2` | K — number of EXTRA standby copies beyond the holder (K=2 → 3 total copies survive one failure). |
| `WithReplicationConcurrency(n)` | `2` | Per-node cap on simultaneous outbound replication sequences (thundering-herd control). |
| `WithReplicationRetries(n)` | `2` | Retry attempts per target after the first try (jittered backoff between tries). |
| `WithReplicationBackoff(d)` | `2s` | Base inter-retry delay; actual wait is `base + uniform[0,base)`. |
| `WithReplicationPrePushJitter(d)` | `3s` | Maximum random delay before a post-capture push so a cluster-wide rolling deploy does not fire N×K transfers in lockstep. |
| `WithReplicationDiskHeadroomMB(mb)` | `2048` | Skip targets with less free disk than this (0 disables the filter). |
| `WithReplicationSampleSize(n)` | `2` | Power-of-two-choices sample size for the per-domain gravity tiebreak. |
| `WithReplicationQueueSize(n)` | `256` | Bound on the pending-job backlog; on overflow the least-urgent job is dropped. |

Pin/eviction interplay: pinning protects a standby from **over-cap/LRU**
eviction only — TTL expiry (`EvictExpired`) still reclaims it, so disk
stays bounded. A capsule at **0 replicated copies** outranks one already at
**K-1** in the work queue, so the most under-replicated snapshots make
progress first.

### Reconnection (O1 — active re-dial + re-auth)

Bootstrap peers are dialed once at startup, and the only inbound-reconnect
path is the libp2p Notifiee reacting to a gracefully-departed peer that
dials **us**. Neither covers a killed-and-restarted seed node that has no
`--bootstrap` of its own: it dials nobody, and its former peers never
re-dial it, so the cluster stays partitioned even though every persistent
phonebook still holds the dead node's entry and multiaddrs.

The **reconnector** closes that gap. On a jittered tick it walks every
joined cluster and builds a candidate set of dial-worthy phonebook entries
unioned with the permanent `--bootstrap` seeds (deduped). A candidate is
**dial-worthy** when its status is one of `Active`, `Suspected`,
`Quarantined`, or `Failed` (never `Departed` — the returning peer re-dials
us), it is **not currently connected**, it is **past its backoff deadline**,
and it has at least one stored address. For a `Failed`/absent candidate the
reconnector first flips the phonebook status to `PendingAuth` so the SWIM
monitor does not immediately re-probe-and-fail a peer whose re-auth is still
mid-flight. On a successful dial it publishes a `ReauthWithPeerRequested`
event; the auth module's re-auth subscriber picks that up and
re-authenticates the pinned peer (falling back to another phonebook peer if
that specific peer refuses). The dial and the re-auth are split across the
event bus because a re-dialed libp2p connection is **not** cluster
membership.

Per-peer dial backoff is capped exponential (`min(base·2^fails, max)` with
jitter) and reset on a successful dial. The backoff map is pruned every tick
against current membership, so a peer SWIM removed from the phonebook simply
stops being a candidate — except `--bootstrap` seeds, which are **permanent**
candidates every tick but remain backoff-bounded, so a permanently-dead seed
is never thrashed.

These tunables are functional options on `node.NewReconnector`, with
production-sensible defaults:

| Option | Default | Description |
|--------|---------|-------------|
| `WithReconnectInterval(d)` | `15s` | Base sweep interval; each tick is jittered ±`WithReconnectJitter`. |
| `WithReconnectJitter(f)` | `0.2` | Fractional (0..1) jitter applied to the tick interval and every backoff deadline (de-synchronises cluster-wide re-dials). |
| `WithReconnectBaseBackoff(d)` | `5s` | Base per-peer dial backoff after the first failed dial. |
| `WithReconnectMaxBackoff(d)` | `5m` | Cap on the per-peer exponential backoff. |
| `WithReconnectDialTimeout(d)` | `10s` | Timeout for a single `host.Connect` dial. |
| `WithBootstrapSeeds([]string)` | — | Permanent seed multiaddrs (the node feeds each cluster's `--bootstrap` list here at join time). |

---

### Member sync & join convergence (O13)

When a new member joins via a voucher, every *other* existing member must
learn about it. Historically the only path for a peer that had already
finished its own one-shot post-join sync was the voucher's best-effort Step-2
`NewMemberAnnounced` PubSub broadcast — and if that peer's gossipsub mesh was
not ready when the voucher published, it missed the announcement and waited a
full steady sync interval (then 5m) to heal. On small clusters this showed up
as `expected N phonebook entries, got N-1`.

Three layers now close that gap, all configurable as functional options on
`nodesync.New` (Layer 3 lives on the authenticator):

**Layer 1 — voucher fan-out push (deterministic).** The voucher emits
`MemberAdmitted`; the syncer actively pushes the new member to every existing
**Active** peer over `/falak/sync/push/1.0`. The receiver applies the same
authentication gate as pull sync and inserts idempotently.

| Option | Default | Description |
|--------|---------|-------------|
| `WithMemberPushEnabled(b)` | `true` | Toggle Layer-1 fan-out (tests disable it to exercise the Layer-2 backstop alone). |
| `WithMemberPushConcurrency(n)` | `8` | Max existing peers pushed to concurrently — bounds stream fan-out on mass join. |
| `WithMemberPushTimeout(d)` | `5s` | Timeout for a single push delivery. |

**Layer 2 — convergence-burst anti-entropy (guaranteed-eventual backstop).**
On `ClusterJoined` and every membership change the periodic sync loop enters a
fast burst, then settles to the steady interval. Jitter is mandatory in
production to prevent synchronised sync storms on mass join.

| Option | Default | Description |
|--------|---------|-------------|
| `WithBurstInterval(d)` | `1s` | Base interval between anti-entropy syncs during a convergence burst. |
| `WithBurstDuration(d)` | `30s` | How long a burst runs after the last membership change before settling to steady. |
| `WithBurstJitter(d)` | `250ms` | ± jitter applied to each burst interval (de-synchronises peers). |
| `WithSyncInterval(d)` | `90s` | Steady-state anti-entropy interval (dropped from the historical 5m as defense-in-depth). |
| `WithSyncRateLimit(n)` | `60` | Max sync/push requests accepted per peer per window; raised from 20 so a 1/s×30s burst is never self-throttled. |
| `WithSyncRateWindow(d)` | `1m` | Rate-limiting window. |
| `WithClock(c)` | real clock | Injectable clock for the burst loop (deterministic tests). |

**Layer 3 — Step-2 mesh-readiness gate (hardening).** Before the first
`new_member` publish, the voucher briefly waits for the auth topic's gossipsub
mesh to have at least one peer, then publishes anyway on timeout so a join
never stalls. Options on `auth.New`:

| Option | Default | Description |
|--------|---------|-------------|
| `WithStep2MeshWaitTimeout(d)` | `2s` | Max wait for ≥1 mesh peer before the first Step-2 publish (0 disables the gate). |
| `WithStep2MeshPollInterval(d)` | `50ms` | How often the mesh peer count is re-checked while waiting. |

---

## Examples

### Single Node (First in Cluster)

```cue
name: "node1"
port: 4001

clusters: {
    "prod/dc1": {
        psk: "my-production-key-32-characters!!"
    }
}
```

```bash
falak daemon start --config=node1.cue
```

### Joining an Existing Cluster

Copy the peer ID from node1's startup logs, then:

```cue
name: "node2"
port: 4002

clusters: {
    "prod/dc1": {
        psk: "my-production-key-32-characters!!"
        bootstrap: [
            "/ip4/10.0.0.1/tcp/4001/p2p/12D3KooWHbogA5Qvs..."
        ]
    }
}
```

```bash
falak daemon start --config=node2.cue
```

### Multi-Cluster Node

A single node joining three clusters with different certificate modes:

```cue
name:       "edge-1"
port:       4001
region:     "eu-west"
datacenter: "dc2"
log_level:  "info"

clusters: {
    // External CA — this node can sign certs for new members
    "prod/eu-west/dc2": {
        psk: "prod-eu-west-psk-32chars-minimum!"
        bootstrap: ["/ip4/10.0.0.1/tcp/4001/p2p/12D3KooW..."]
        certificates: {
            ca_cert: "/etc/falak/prod-eu/ca.crt"
            ca_key:  "/etc/falak/prod-eu/ca.key"
        }
    }

    // External CA — pre-signed cert, can't sign for others
    "prod/us-east/dc1": {
        psk: "prod-us-east-psk-32chars-minimum"
        bootstrap: ["/ip4/10.1.0.1/tcp/4001/p2p/12D3KooW..."]
        certificates: {
            ca_cert:   "/etc/falak/prod-us/ca.crt"
            node_cert: "/etc/falak/prod-us/edge-1.crt"
            node_key:  "/etc/falak/prod-us/edge-1.key"
        }
    }

    // Auto mode — PSK-derived certs
    "staging/eu-west/dc2": {
        psk: "staging-eu-psk-at-least-32-chars!"
        bootstrap: ["/ip4/10.2.0.1/tcp/4001/p2p/12D3KooW..."]
    }
}
```

### Fast Failure Detection (Testing)

Lower health thresholds for quick failure detection during development:

```cue
name: "test-node"
port: 4001

clusters: {
    "test/local": {
        psk: "test-psk-must-be-at-least-32-chars"
    }
}

health: {
    protocol_period:    "500ms"
    ping_timeout:       "200ms"
    quarantine_timeout: "5s"
    score_increment:    1.0
    suspected_threshold:  2.0
    quarantine_threshold: 4.0
}
```

---

## CLI Flags vs Config File

The `--config` flag loads all settings from a CUE file. CLI flags still work for simple single-cluster use without a config file.

```bash
# Config file (recommended for multi-cluster or production)
falak daemon start --config=node.cue

# CLI flags (quick single-cluster use)
falak daemon start --name=node1 --port=4001 --cluster=test/dc1 --psk=mysecretkey1234567890123456789012

# CLI flags with external CA
falak daemon start --name=node1 --port=4001 --cluster=prod/dc1 \
    --psk=mysecretkey1234567890123456789012 \
    --ca-cert=/path/ca.crt --ca-key=/path/ca.key \
    --bootstrap=/ip4/10.0.0.1/tcp/4001/p2p/12D3KooW...
```

When using `--config`, all node settings come from the file. The `--force-reject-auth` testing flag can still be passed alongside `--config`.

---

## CUE Schema (for IDE Support)

The schema definitions are in `cue/falak.cue`. To get autocomplete in your editor:

1. Install the [CUE VS Code extension](https://marketplace.visualstudio.com/items?itemName=cuelang.cue) or your editor's CUE plugin
2. Import the Falak schema in your config:

```cue
import "github.com/tareksalem/falak/cue"

cue.#Node & {
    name: "my-node"
    // ... autocomplete works here
}
```

Or use the definitions directly without import — the schema file serves as reference for the available fields and their types.

---

## Data Directory Layout

After running a node, persistent data is stored at:

```
<data_dir>/                          # ~/.local/share/falak/<name> by default
├── phonebook.db                     # SQLite — cluster membership
├── phonebook.db-wal                 # SQLite WAL journal
└── clusters/
    └── <cluster-path-flat>/         # e.g., prod-dc1
        ├── node.key                 # Ed25519 private key (0600)
        ├── certificate.pem          # Node certificate (0600)
        ├── ca.pem                   # Cluster CA certificate (0600). Auto mode
        │                            # derives this deterministically from the
        │                            # PSK and overwrites it on every start;
        │                            # external mode does not use this file.
        └── revocations.json         # Revoked certificate fingerprints
```

---

## Validation

CUE validates your config before the node starts. Common errors:

| Error | Cause |
|-------|-------|
| `name is required` | Missing `name` field |
| `PSK must be at least 32 characters` | PSK string too short |
| `ca_cert file not found` | Certificate path doesn't exist |
| `port must be 0-65535` | Port out of range |

To validate a config without running the node:

```bash
cue vet cue/falak.cue your-config.cue
```
