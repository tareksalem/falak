# Falak — Manual Testing Guide

A layered, step-by-step plan to verify every major subsystem we've built. Start at Layer 1 and work up. Use 3 terminal windows (or `tmux`).

> **Single binary.** Everything below uses one `falak` binary; the daemon
> runs via `falak daemon start`. (Historically there was a separate
> `falakd` binary — that's been folded into the CLI.)

## Prep — build the binary

```bash
source ~/.gvm/scripts/gvm && gvm use go1.25
cd /media/tarek/B/ideas/falak
go build -o /tmp/falak ./cmd/falak

# Shared cluster constants used in all layers below
export PSK=mysupersecretkey1234567890123456
export CLUSTER=test/dc1/prod
```

Some layers need **Linux + root** (VXLAN, IPsec, iptables). Without root you'll get most of Layers 1–6 cleanly; Layer 7's overlay path degrades.

For container layers (4 onwards) you need:

- **Podman** installed with a running rootless or system socket:
  ```bash
  systemctl --user start podman.socket
  systemctl --user enable podman.socket
  ```
- **CRIU** installed for the snapshot fast-restore path (locked Podman +
  CRIU runtime architecture). Without CRIU containers still cold-start
  but the daemon logs `snapshot capture failed` per capsule.
  ```bash
  sudo apt install -y criu          # or dnf / pacman
  sudo criu check                   # last line: "Looks good."
  ```

---

## Layer 1 — Single-node smoke

**Terminal 1:**
```bash
/tmp/falak daemon start --name=node1 --port=4001 --cluster=$CLUSTER --psk=$PSK --log-level=info
```

**Expected:**
- Logs show `node started`, `cluster joined` for `node1` (first node, self-vouches via PSK).
- Tail shows `health monitor started`, `auth subscriber started`.
- No errors.

**Verify:**
```bash
/tmp/falak --insecure node list   # one node, status=Active
```

Stop with Ctrl-C. Confirms basic boot + auth + data dir creation under `~/.local/share/falak/node1/`.

---

## Layer 2 — 2-node cluster + phonebook

**Terminal 1** (keep running from Layer 1, or restart):
```bash
/tmp/falak daemon start --name=node1 --port=4001 --cluster=$CLUSTER --psk=$PSK --log-level=info
```

Grab node1's multiaddr from the log — look for lines like:
```
Node peer ID: 12D3KooW...
listening on /ip4/127.0.0.1/tcp/4001
```

Construct:
```bash
BOOT=/ip4/127.0.0.1/tcp/4001/p2p/<node1-peer-id>
```

**Terminal 2:**
```bash
/tmp/falak daemon start --name=node2 --port=4002 --cluster=$CLUSTER --psk=$PSK --bootstrap=$BOOT --log-level=info
```

**Expected:**
- node2 logs: `auth: challenge received`, `auth: challenge ok`, `cluster joined`, `phonebook member added: node1`.
- node1 logs: `auth: new member node2 announced`, `phonebook member added: node2`.
- Phonebook count = 2 on both.

**Verify** (3rd terminal):
```bash
/tmp/falak --insecure node list
```
Both nodes appear.

---

## Layer 3 — SWIM health monitoring + failure detection

Add a 3rd node:
```bash
/tmp/falak daemon start --name=node3 --port=4003 --cluster=$CLUSTER --psk=$PSK --bootstrap=$BOOT --log-level=debug
```

Wait ~5s for the SWIM mesh to form. All 3 nodes' phonebooks have count=3.

In node3's debug log you'll see periodic:
```
health: probe sent to <peer>
health: probe ok
```
Score for each peer rises toward 1.0.

**Kill node2** (Ctrl-C in Terminal 2). Watch node1 and node3 logs:

- After ~3 probe failures: `health: node suspected node2 score=...`
- After more failures: `health: node quarantined node2`
- After the quarantine timeout: `health: node failed node2` + `phonebook member removed: node2`

```bash
/tmp/falak --insecure node list   # shows 2 nodes
```

This verifies **SWIM detection + dynamic cluster-size scaling**.

---

## Layer 4 — Capsule create + propagation

Restart node2 so you have a healthy 3-node cluster. Then on any terminal:

```bash
cat > /tmp/cap-api.cue <<'EOF'
capsule: {
    name:  "api"
    image: "docker.io/library/nginx:alpine"
    orbit: "default"
    tier:  "standard"
    runtime: network: ports: [{name: "http", container: 80}]
    replicas: { exact: 1 }
}
EOF

/tmp/falak --insecure capsule create -f /tmp/cap-api.cue --cluster=$CLUSTER
```

**Expected:**
- CLI returns a capsule ID.
- All 3 node logs: `capsule received via orbit announcement: api`.
- One node wins the election (`election won` + `runtime: cold starting falak-<capsule-id>-0`).
- Podman is required for the container to actually run; without it the runtime layer logs `runtime: pull failed` and the election re-fires.

**Verify:**
```bash
/tmp/falak --insecure capsule list
/tmp/falak --insecure capsule get <id>
```

This verifies **capsule.Manager.Create → orbit gossip → election → runtime handler → container start**.

---

## Layer 5 — CapsuleGroup (same-orbit) + dependency ordering

```bash
cat > /tmp/grp-stack.cue <<'EOF'
capsule: {
    name: "my-stack"
    kind: "group"
    group: {
        colocation: "same-orbit"
        cascade_delete: true
        members: {
            db: {
                image: "docker.io/library/postgres:15-alpine"
                orbit: "data"
                runtime: env: POSTGRES_PASSWORD: "test"
            }
            api: {
                image: "docker.io/library/nginx:alpine"
                orbit: "public"
                depends_on: ["db"]
            }
        }
    }
}
EOF

# NOTE: `capsule create -f` rejects `kind: "group"` today — only standalone
# capsules go through the gRPC Create surface. Until the daemon grows a
# CreateGroup RPC, deploy groups via the node config's `capsules:` block
# and restart the daemon with --config=node-with-stack.cue.
/tmp/falak daemon stop
/tmp/falak daemon start --name=node1 --port=4001 --config=node-with-stack.cue
```

**Expected:**
- Group `my-stack` created on the originating node, propagates over `__falak/groups` orbit (system orbit auto-joined by every node).
- Members `db` and `api` propagate over their respective orbits (`data`, `public`).
- Election fires per member (same-orbit = independent placement).
- Runtime starts `db` first; `api` is **parked** until db reaches Running:
  - `runtime: group dependency wait: parked capsule=api waiting=[db]`
  - `runtime: group dependency wait: released capsule=api`
- Both eventually running.

**Verify:**
```bash
/tmp/falak --insecure capsule list                 # shows group + both members
/tmp/falak --insecure capsule get <db-capsule-id>  # GroupID set + GroupMember=true
```

**Cascade-delete test:**
```bash
/tmp/falak --insecure capsule delete <my-stack-id>
```
Within ~1 gossip round, `capsule list` is empty everywhere.

This verifies **CapsuleGroup creation + member materialization + dependency ordering + cascade delete + cross-node propagation**.

---

## Layer 6 — CapsuleGroup (same-node) + crash recovery

```bash
cat > /tmp/grp-tight.cue <<'EOF'
capsule: {
    name: "sidecar-stack"
    kind: "group"
    group: {
        colocation: "same-node"
        cascade_delete: true
        members: {
            app:     { image: "docker.io/library/nginx:alpine",   orbit: "public" }
            sidecar: { image: "docker.io/library/busybox:latest", orbit: "public", depends_on: ["app"] }
        }
    }
}
EOF

# Same constraint as Layer 5 — declare the group under the daemon
# `capsules:` config block and restart with --config=node-with-sidecar.cue.
/tmp/falak daemon stop
/tmp/falak daemon start --name=node1 --port=4001 --config=node-with-sidecar.cue
```

**Expected:**
- `election: group claim published` — a single claim covering both members.
- One node wins → `election: group claim won`.
- That node's runtime starts **both** members.
- Other nodes log `election: group claim lost`.
- Reservation hold on the winning node: `election: group reservation recorded deadline=...`.

**Kill the winning node** (Ctrl-C). On the surviving nodes:
- `health: node failed <winner>` (SWIM)
- Sibling members transitioned Created/Assigned → Announced via `SyncStatus`
- `election: group reelection requested ExcludeNodes=[<failed-node>]`
- Surviving node wins the re-election, starts both members.

This verifies **GroupClaim distributed election + capacity reservation + crash-safe fan-out + recovery on node failure**.

---

## Layer 7 — Service mesh (DNS + traffic split)

### 7.1 Deploy two capsule versions

```bash
cat > /tmp/cap-v1.cue <<'EOF'
capsule: {
    name: "payments-v1"
    image: "docker.io/library/nginx:alpine"
    orbit: "default"
    runtime: network: ports: [{name: "http", container: 80}]
    replicas: { exact: 1 }
}
EOF

cp /tmp/cap-v1.cue /tmp/cap-v2.cue
sed -i 's/payments-v1/payments-v2/' /tmp/cap-v2.cue

/tmp/falak --insecure capsule create -f /tmp/cap-v1.cue --cluster=$CLUSTER
/tmp/falak --insecure capsule create -f /tmp/cap-v2.cue --cluster=$CLUSTER
```

### 7.2 Define the Service (90/10 split)

```bash
cat > /tmp/svc.cue <<'EOF'
services: payments: {
    name: "payments"
    visibility: "cluster"
    ports: [{ name: "http", port: 8080, protocol: "tcp" }]
    backends: [
        { capsule: "payments-v1", weight: 90 },
        { capsule: "payments-v2", weight: 10 },
    ]
}
EOF

/tmp/falak --insecure service apply -f /tmp/svc.cue
```

**Expected:**
- Service propagates via gossip topic `falak/<cluster>/services`.
- Each node's ProxyManager binds a TCP listener on every per-group bridge gateway IP for port 8080.
- DNS responder answers `payments` → local proxy IP.

### 7.3 Verify DNS + traffic split (Linux + Podman + root)

From a capsule container:
```bash
# Find a running container created by Falak
podman ps | grep falak-

# DNS lookup — should return 169.254.169.250 (the link-local DNS IP)
podman exec <container> nslookup payments

# Drive 10 connections — should land ~9 on v1, ~1 on v2
podman exec <container> sh -c 'for i in 1 2 3 4 5 6 7 8 9 10; do curl -s payments:8080 -o /dev/null -w "%{remote_ip}\n"; done'
```

### 7.4 Canary test

Edit `/tmp/svc.cue` to add a canary strategy:
```cue
services: payments: {
    name: "payments"
    visibility: "cluster"
    ports: [{ name: "http", port: 8080, protocol: "tcp" }]
    backends: [
        { capsule: "payments-v1", weight: 100 },
        { capsule: "payments-v2", weight: 0 },
    ]
    strategy: {
        type: "canary"
        canary: {
            target:   "payments-v2"
            from:     "payments-v1"
            step:     10
            interval: "10s"
        }
    }
}
```

```bash
/tmp/falak --insecure service apply -f /tmp/svc.cue

# Watch weights advance every 10s
watch -n 2 '/tmp/falak --insecure service get payments -o yaml | grep -A 6 backends'
```

This verifies **Service entity + gossip propagation + proxy listener + DNS resolution + SWRR backend selection + canary strategy auto-progression**.

---

## What needs Linux + root

| Layer | Linux | Root | Podman |
|-------|-------|------|--------|
| 1 — single node | any | no | no |
| 2 — 2-node cluster | any | no | no |
| 3 — SWIM | any | no | no |
| 4 — capsule create | any (Podman ineractions need Linux) | no | yes (or container runtime stays in cold-start retry loop) |
| 5 — group same-orbit | any | no | yes |
| 6 — group same-node | any | no | yes |
| 7.1–7.2 — Service definition | any | no | yes |
| 7.3 — DNS + proxy forwarding | Linux | yes | yes |
| 7.4 — canary progression | any (the strategy engine runs in the manager; observed via API even without proxy) | no for engine; yes for actual traffic | yes |

Cross-node Service traffic (the actual L4 forwarding across nodes) requires the **VXLAN + IPsec overlay** which is Linux + root + kernel modules (`vxlan`, `esp4`, `xfrm_user`) + `CAP_NET_ADMIN`. Single-node tests of the Service entity work on any host.

To run the privileged kernel tests once:
```bash
sudo make test-privileged
```

---

## What to expect to fail

- **Without Podman**: container starts fail at the runtime layer. Capsule + Service entity + metadata propagation still works.
- **Without root**: bridge create fails, DNS listener doesn't bind on the bridge gateway, proxy can't forward. Service definitions + gossip still propagate.
- **Cross-host distributed**: needs the cross-node overlay, requires root + Linux on each host + the same PSK + reachable IPs between nodes.

---

## What each layer proves

1. Boot + auth + data dir.
2. PSK challenge-response + phonebook propagation.
3. SWIM probe + suspect/quarantine/fail state machine.
4. Capsule lifecycle FSM + orbit gossip + election + runtime handler.
5. CapsuleGroup entity + dependency ordering + cascade delete + cross-orbit propagation.
6. Distributed `GroupClaim` election + capacity reservation + crash-safe rollback + recovery.
7. Service entity + Service gossip + per-bridge DNS + SWRR proxy + canary progression.

Run them in order. Each layer builds on the previous; if a layer fails, the log line + the `falak` CLI output together pinpoint the broken subsystem.
