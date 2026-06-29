# Falak — Full Self-Testing Guide

A consolidated, copy-pasteable plan to verify every Falak subsystem end-to-end.
Layered: each step only adds onto the previous one, so if something breaks
you'll know exactly which subsystem failed.

For the original narrative version see [`MANUAL_TESTING.md`](MANUAL_TESTING.md).

---

## Step 0 — Prereqs & build

```bash
# Toolchain
source ~/.gvm/scripts/gvm && gvm use go1.25
cd /media/tarek/B/ideas/falak

# Build the single binary
go build -o /tmp/falak ./cmd/falak

# Constants reused everywhere
export PSK=mysupersecretkey1234567890123456     # must be >= 32 bytes
export CLUSTER=test/dc1/prod
```

### Daemon flags worth knowing

The daemon starts a gRPC + HTTP API on a single port (default `:9090`)
multiplexed via content-type sniffing (HTTP/2 cleartext via H2C). On
SIGTERM/SIGINT it drains gracefully — broadcasts `NodeDeparting` to
peers so they evict sub-second instead of waiting for SWIM.

| Flag | Default | Purpose |
|---|---|---|
| `--api-listen` | `:9090` | API listen address. Vite-style auto port-shift on `EADDRINUSE` (up to 20 attempts). Use `:0` for random. |
| `--api-disable` | `false` | Skip the API entirely (control-plane-only mode). |
| `--drain-timeout` | `3s` | On SIGTERM/SIGINT, wait this long for the departure broadcast to reach peers before exit. |
| `--health-heartbeat` | `0` (off) | If `>0`, emit one INFO log per interval summarising cluster health counts (active/pending/suspected/quarantined/failed/departed). |
| `--force-reject-auth` | `false` | TEST ONLY — reject every incoming auth announcement. Used to exercise the rejection-consensus path. |

The CLI client picks the endpoint from (in order): `--endpoint` flag, the
active context in `~/.falak/config`, or — if neither is set — errors out
with "no context configured". Use `falak config set-context` to bind a
context once and skip the `--endpoint` flag thereafter:

```bash
/tmp/falak config set-context local --endpoint=localhost:9090 --insecure --set-current
/tmp/falak system info                # uses the stored context
```

### Clean state before each layered run

The phonebook is persistent SQLite. Stale entries from prior runs (with
different node keys at the same multiaddrs) used to surface as phantom
SWIM probe failures. The fix evicts them automatically now, but the
cleanest starting point is still a fresh data dir:

```bash
/tmp/falak daemon stop 2>/dev/null
rm -rf ~/.local/share/falak/node*
```

Run that once before Layer 1.

**Needed from Layer 4 onward (containers actually starting):**

```bash
# 1. Podman socket. The daemon dials it via libpod REST.
systemctl --user start podman.socket
systemctl --user enable podman.socket    # auto-start on login
podman --version

# 2. CRIU — required by the locked Podman + CRIU runtime decision.
#    Without it the daemon still starts containers (cold-start every
#    time) but logs `snapshot capture failed` and you lose the
#    sub-second fast-restore that's the platform's headline.
sudo apt install -y criu                 # or dnf / pacman
sudo criu check                          # last line: "Looks good."
```

**Needed from Layer 7 onward (cross-node overlay):**

```bash
sudo modprobe vxlan esp4 xfrm_user
sudo sysctl -w net.ipv4.conf.all.rp_filter=1
```

**Clean slate (run between layers if state gets messy):**

```bash
/tmp/falak daemon stop 2>/dev/null
rm -rf ~/.local/share/falak/node*
```

Open **3 terminals** (or use `tmux` panes). They are referred to as T1, T2, T3 below.

---

## Layer 1 — Single node smoke (~1 min)

**T1:**
```bash
/tmp/falak daemon start --name=node1 --port=4001 \
    --cluster=$CLUSTER --psk=$PSK --log-level=info
```

**What to look for in logs:**
- `node started  id=12D3KooW…  addrs=[/ip4/127.0.0.1/tcp/4001 …]` — the `addrs` list is whatever interfaces libp2p discovered (loopback, host LAN, docker bridges…); don't worry about the count.
- `no bootstrap peers, starting as first node  cluster=test/dc1/prod`
- `created self-signed certificate for first node  cluster=test/dc1/prod`
- `auth message loop started  cluster=test/dc1/prod`
- `health monitor started  cluster=test/dc1/prod  period=2s  pingTimeout=500ms`
- `metrics manager started`, `capsule handler started`, `election handler started`
- `cluster joined, initiating sync` and `cluster joined, capsule handler ready` (these are the closest things to a single "cluster joined" line — there isn't one canonical event)
- `api server listening  addr=:9090`
- **Copy node1's peer ID** from the `node started` line — you'll need it for Layer 2.

**T3 (verify):**
```bash
/tmp/falak daemon status                # -> RUNNING (pid ...)
/tmp/falak --insecure node list         # one node, Active
/tmp/falak --insecure system info       # version, uptime
ls -la ~/.local/share/falak/node1/      # node.key, certs, phonebook.db
```

**Proves:** boot, libp2p host init, PSK self-vouching, data-dir creation, gRPC API up on `:9090`.

---

## Layer 2 — 2-node cluster + PSK auth + PKI (~2 min)

Keep T1 running. Build the bootstrap multiaddr:

```bash
# Replace <peer-id> with node1's from T1 logs
export BOOT=/ip4/127.0.0.1/tcp/4001/p2p/<peer-id>
```

**T2:**
```bash
/tmp/falak daemon start --name=node2 --port=4002 \
    --cluster=$CLUSTER --psk=$PSK --bootstrap=$BOOT --log-level=info
```

**T2 logs (joiner):**
- `dialing bootstrap peer …`
- `bootstrap peer connected, requesting auth (gossipsub mesh forming, may take 5-15s)`
- (on a 2-node bootstrap, the next line lands in ~2.5ms; no `still waiting…` heartbeats fire)
- `authenticated with bootstrap peer  elapsed=…`
- `cluster joined`

**T1 logs (voucher):**
- `peer connected peer=… direction=inbound addr=…` (libp2p `Notifiee`)
- `received join request, validating PSK`
- `PSK validated, signing certificate`
- `authenticated new member`

Auth handshake on a 2-node bootstrap is sub-second (was 15s before
Session 17 — see `docs/BUGS.md` F16). The `still waiting for voucher
response (5s/10s/15s elapsed)` heartbeats only fire when something
genuinely stalls.

**T3:**
```bash
/tmp/falak --insecure node list         # 2 nodes, both Active
# ID              NAME   STATUS  DC   REGION  CPU  MEM(MB)
# 12D3KooW…ixrS7  node1  active  dc1  test    12   40028
# 12D3KooW…HgWV8  node2  active  dc1  test    12   40028

/tmp/falak --insecure cluster members
ls ~/.local/share/falak/node1/clusters/test-dc1-prod/
# Should see: node.key, certificate.pem, ca.pem (0600)
```

**Proves:** PSK challenge-response, PKI cert issuance via voucher,
phonebook propagation with NAME + CPU/MEM populated end-to-end,
dedup cache, signed envelope verification, auth progress logs.

---

## Layer 3 — SWIM health detection (~3 min)

**T3 (add 3rd node):**
```bash
/tmp/falak daemon start --name=node3 --port=4003 \
    --cluster=$CLUSTER --psk=$PSK --bootstrap=$BOOT --log-level=debug
```

Wait ~5 s for SWIM to converge. In debug logs you'll see periodic per-probe
lines (added by F24 — Bug #21):
```
probe ok      target=<peer>  cluster=test/dc1/prod  score=0.00
probe failed  target=<peer>  cluster=test/dc1/prod  score=0.50  error=...
```

Falak treats graceful shutdown and hard failure **differently** —
exercise both paths to see the full state machine.

### 3a. Graceful shutdown (Ctrl-C / `daemon stop`)

Ctrl-C in T2 sends SIGINT, which triggers `Node.Drain()`. The departing
node broadcasts `NodeDeparting` over the health PubSub topic; peers
evict it **sub-second** without walking through SWIM (F8/F21 — Bugs #16
and #24).

T1 and T3 logs:
- `peer announced graceful departure  peer=<node2-id>`
- Phonebook entry kept around with `status=departed` so the PKI
  signature can still be verified if node2 later reconnects (F21 — Bug #24).

Verify in a 4th shell:
```bash
/tmp/falak --insecure node list
# node2 row still present, STATUS=departed (NOT removed)
```

Restart node2 with the same identity and watch T1: SWIM reactivates
node2 on the first successful probe (`departed peer reconnected,
reactivated`).

### 3b. Hard failure (`kill -9`)

Now exercise the SWIM probe-failure path. From a 4th shell:
```bash
pkill -9 -f "name=node2"   # or kill -9 <node2-pid>
```

T1 and T3 logs (~17s end-to-end on the default thresholds):
- After ~3 probe failures → `node suspected  score=…`
- More failures → `node quarantined`
- Quarantine timeout → `node failed` + `phonebook member removed: node2`

```bash
/tmp/falak --insecure node list         # 2 nodes only — node2 is gone
```

### 3c. Live SWIM data via `falak node health`

The phonebook now records every probe's outcome (F15 — Bugs #18 + #22).
`node health` accepts **either the peer ID or the operator-assigned
`--name`** — pass the name, peer IDs are awkward to copy:

```bash
/tmp/falak --insecure node health node2
# node:        12D3KooW…HgWV8
# name:        node2
# cluster:     test/dc1/prod
# status:      active
# cpu cores:   12
# memory:      40028 MB
# last probe:  2s ago (ok)
# score:       0.00
```

`falak node get node2` works the same way. After `kill -9`, polling
`node health node2` shows the live transition
`active → quarantined → not found` over ~17s.

**Proves:** SWIM probe loop with per-probe Debug logs, suspect →
quarantined → failed → removed state machine, dynamic cluster-size
threshold scaling, graceful-departure broadcast bypassing SWIM,
Departed status preservation for re-auth, live SWIM exposure via
`node health`.

---

## Layer 4 — Capsule create + election + runtime (~3 min)

Restart node2 so you have a healthy 3-node cluster. **Podman must be running** for the runtime to start the container.

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

**Expected across logs:**
- All 3 nodes: `capsule received via orbit announcement: api`
- Each node: `election: claim published score=<gravity>`
- One winner: `election: claim won` + `runtime: cold starting falak-<id>-0`
- Losers: `election: claim lost`

**Verify:**
```bash
/tmp/falak --insecure capsule list
/tmp/falak --insecure capsule get <id>
/tmp/falak --insecure capsule logs <id> --tail=20
/tmp/falak --insecure capsule watch <id>      # SSE-streamed events
podman ps | grep falak-                       # the actual container
```

**Curl it (Linux):**
```bash
podman port falak-<id>-0                       # find published port
curl http://localhost:<port>
```

**Proves:** CUE loader, capsule FSM, orbit gossip with signed envelopes, gravity scoring + election, Podman runtime (Pull/Create/Start), SSE watch.

---

## Layer 5 — CapsuleGroup, dependency ordering, cascade delete (~3 min)

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

# NOTE: group capsules don't go through `capsule create -f` yet —
# the CLI loader rejects `kind: "group"` with a clear error. Until
# the daemon grows a CreateGroup RPC, declare the group under the
# daemon's `capsules:` config block and restart the daemon with
# --config=node.cue (Falak auto-creates the configured capsules after
# cluster join). The CUE body below is the same shape, just nested
# under `capsules: {…}` on the node config.
/tmp/falak daemon stop
/tmp/falak daemon start --name=node1 --port=4001 --config=node-with-stack.cue
```

**Watch for in logs:**
- Group propagates over `__falak/groups` (system orbit, auto-joined).
- `db` and `api` propagate over `data` / `public` orbits — independent elections.
- **`api` is parked**: `runtime: group dependency wait: parked capsule=api waiting=[db]`
- When db reaches Running: `runtime: group dependency wait: released capsule=api`

**Verify:**
```bash
/tmp/falak --insecure capsule list             # group + both members
/tmp/falak --insecure capsule get <db-id>      # GroupID set, GroupMember=true
```

**Cascade delete:**
```bash
/tmp/falak --insecure capsule delete <my-stack-id>
/tmp/falak --insecure capsule list             # all 3 gone within ~1 gossip round
```

**Proves:** Group DAG validation, member materialization, parked-start dependency queue, cross-orbit gossip, cascade reaper.

---

## Layer 6 — Same-node group + crash-safe recovery (~5 min)

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

# Same constraint as Layer 5: group capsules need the daemon
# `capsules:` config block. Restart the daemon with the file rebuilt
# under `capsules: {…}` on the node config.
/tmp/falak daemon stop
/tmp/falak daemon start --name=node1 --port=4001 --config=node-with-sidecar.cue
```

**Expected:**
- `election: group claim published` — a single claim covering both members
- One node: `election: group claim won` + reservation hold
- That node's runtime starts **both** containers (`podman ps` shows two)
- Losers: `election: group claim lost`

**Crash test** — Ctrl-C the winning node:
- Survivors: `health: node failed <winner>`
- Sibling capsules transitioned back to Announced via `SyncStatus`
- `election: group reelection requested ExcludeNodes=[<failed-node>]`
- Surviving node wins, starts both members.

**Proves:** distributed GroupClaim election, capacity reservation semaphore, crash-safe sibling rollback, `ExcludeNodes` re-election, `NodeJoined` recovery.

---

## Layer 7 — Service mesh (Linux + root + Podman) (~5 min)

### 7.1 Deploy two versions

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
/tmp/falak --insecure service list
/tmp/falak --insecure service get payments -o yaml
```

**Expected logs:**
- `service: announce published topic=falak/<cluster>/services`
- Per node: `proxy: listener bound bridge=<id> ip=<gw> port=8080`
- `dns: service registered name=payments -> 169.254.169.250`

### 7.3 Verify DNS + traffic split from inside a container

```bash
# Find a container started by Falak
podman ps | grep falak-
export CT=$(podman ps --format '{{.Names}}' | grep falak- | head -1)

# DNS resolves to the link-local DNS responder
podman exec $CT nslookup payments

# Drive 10 connections, count where each lands
podman exec $CT sh -c '
  for i in $(seq 1 10); do
    curl -s payments:8080 -o /dev/null -w "%{remote_ip}\n"
  done | sort | uniq -c
'
# Should land ~9 on v1, ~1 on v2 (SWRR with weight bias)
```

### 7.4 Canary auto-progression

Edit `/tmp/svc.cue` to start at 100/0 and add a canary block:

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

# Weights should advance every 10s: 90/10 -> 80/20 -> ... -> 0/100
watch -n 2 '/tmp/falak --insecure service get payments -o yaml | grep -A 6 backends'
```

**Other strategies to try:** `static`, `blue-green`. And:
```bash
/tmp/falak --insecure service rebind payments --backend=payments-v1 --capsule=<new-capsule-id>
/tmp/falak --insecure service watch
```

**Proves:** Service entity + FSM, gossip topic + signed envelopes, per-bridge proxy listener with SWRR + dial-failure ejection, DNS responder on `169.254.169.250`, canary engine auto-progression, identity-bound rebind.

---

## Bonus — things to poke at

| What | How |
|---|---|
| Multi-cluster isolation | Start node4 with `--cluster=other/dc1/prod` — phonebook + capsules stay separate |
| External CA mode | Generate a CA, pass `--ca-cert --ca-key --node-cert --node-key`; PKI uses your cert chain |
| Cert revocation | Stop a node, delete its cert from another node via API (if exposed), check sync |
| Config file vs flags | Write `--config=/path/to/node.cue`; declare capsules under `capsules:` — they auto-create after join |
| Snapshot transfer | Restart a node holding a `falak-<id>-0` container; the snapshot store should reuse it on next election |
| `falak config use-context` | Multi-cluster kubeconfig-style context switching |
| API directly | `grpcurl -plaintext localhost:9090 list`; also try `curl http://localhost:9090/healthz` |
| SSE watch from browser | `curl -N http://localhost:9090/v1alpha1/capsules/<id>/watch` |
| Privileged kernel tests | `sudo make test-privileged` — VXLAN + IPsec + rp_filter checks |

---

## What needs root / Linux / Podman

| Layer | Linux | Root | Podman |
|---|---|---|---|
| 1–3 (boot, auth, SWIM) | any | no | no |
| 4 (capsule + runtime) | any | no | **yes** |
| 5 (group same-orbit) | any | no | yes |
| 6 (group same-node + crash) | any | no | yes |
| 7.1–7.2 (Service entity) | any | no | yes |
| 7.3 (DNS + L4 proxy) | **Linux** | **yes** | yes |
| 7.4 canary engine | any | no | yes |
| Cross-node overlay (VXLAN+IPsec) | Linux | yes + caps | yes |

---

## Recovery / troubleshooting cheat-sheet

```bash
/tmp/falak daemon status                  # is it running?
/tmp/falak daemon stop                    # graceful shutdown
killall -9 falak                          # nuclear option
rm -rf ~/.local/share/falak/node*         # wipe state
podman rm -f $(podman ps -aq --filter name=falak-)   # kill all capsule containers
sudo iptables -L FORWARD -n | grep falak  # check Layer 7 isolation rules
sudo ip -d link show type vxlan           # check overlay devices
```

Run Layers 1->7 in order. If a layer fails, the log line + CLI output narrows
the broken subsystem precisely — that's the design intent of the layered guide.
