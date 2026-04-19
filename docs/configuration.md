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
falakd --config=node1.cue
```

Or with air (development):

```bash
cd cmd
air -- --config=../configs/node1.cue
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
falakd --config=node1.cue
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
falakd --config=node2.cue
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
falakd --config=node.cue

# CLI flags (quick single-cluster use)
falakd --name=node1 --port=4001 --cluster=test/dc1 --psk=mysecretkey1234567890123456789012

# CLI flags with external CA
falakd --name=node1 --port=4001 --cluster=prod/dc1 \
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
