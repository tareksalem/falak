# Falak — End-to-End Retest Guide

A layered manual retest that validates the recovery, gravity/snapshot, and
election-agreement work. Runs in **root mode with the rootful Podman socket**
so the CRIU snapshot paths (capture, restore, replication) are exercised.
Each layer maps to the specific fix(es) it validates.

> Every layer is independent-ish but builds on the previous; run in order.
> The "✅ Look for" lines are the pass criteria.

---

## Data directory (where state lives)

Default per-node data dir on Linux is `$HOME/.local/share/falak/<node-name>/`.
Under `sudo` that's `/root/.local/share/falak/<node-name>/`. It is **persistent
data, not a cache** — wiping it removes identity + snapshots. Contents:

| Path | Holds |
|---|---|
| `capsules.db` | capsule store (source of "name already exists") |
| `phonebook.db` | peer directory / membership |
| `snapshots.db` | snapshot metadata index |
| `metrics.db` | node metrics |
| `reliability.db` | execution-reliability tracker (O12) |
| `snapshots/<capsule_id>/<tag>/` | CRIU snapshot archives (the bytes) |
| `clusters/<cluster>/` | `node.key`, `certificate.pem`, `ca.pem` (identity + PKI) |
| `falak.pid` | daemon pidfile |

`~/.local/share/falak/daemons/<name>.json` (sibling) is the `--node` CLI
registry — rewritten on daemon start; `rm -rf …/node*` doesn't touch it.

Inspect before wiping: `sudo ls -R /root/.local/share/falak/node1/`

---

## Step 0 — Prereqs, build, clean slate

```bash
source ~/.gvm/scripts/gvm && gvm use go1.25
cd /media/tarek/B/ideas/falak
go build -o /tmp/falak ./cmd/falak

export PSK=mysupersecretkey1234567890123456      # >= 32 bytes
export CLUSTER=test/dc1/prod
export SOCK=/run/podman/podman.sock

sudo systemctl enable --now podman.socket
sudo criu check                                   # expect "Looks good."

# Clean slate (removes ALL per-node state: DBs, keys, snapshots)
sudo /tmp/falak daemon stop 2>/dev/null
pkill -f "falak daemon start" 2>/dev/null
sudo rm -rf /root/.local/share/falak/node*
sudo podman rm -f $(sudo podman ps -aq --filter name=falak-) 2>/dev/null
```

Open 3 terminals (T1/T2/T3) for daemons, plus one for CLI/observation.

---

## Step 1 — Single node + placement + snapshot capture  (O9, O7-capture)

**T1:**
```bash
sudo /tmp/falak daemon start --name=node1 --port=4001 \
    --cluster=$CLUSTER --psk=$PSK --runtime-socket=$SOCK --log-level=info
```
Copy node1's peer ID from `node started id=12D3KooW…`, then:
```bash
export BOOT=/ip4/127.0.0.1/tcp/4001/p2p/<node1-peer-id>
```

Deploy (CLI as root so it finds the root daemon registry):
```bash
cat > /tmp/cap-api.cue <<'EOF'
capsule: { name: "api", image: "docker.io/library/nginx:alpine", orbit: "default"
  runtime: network: ports: [{name: "http", container: 80}], replicas: { exact: 1 } }
EOF
sudo /tmp/falak --insecure --node node1 capsule create -f /tmp/cap-api.cue --cluster=$CLUSTER
```

**✅ Look for (T1):**
- `delay strategy: eligible … gravity_score: <NON-ZERO>` — a real number (~80s), **not 0** (O9).
- `election won` (exactly one) → `runtime: cold starting` → `runtime: container running`.
- `runtime: snapshot captured` (O7 capture — rootful CRIU works).

```bash
sudo /tmp/falak --insecure --node node1 capsule list      # STATUS=running
sudo podman ps | grep falak-
```

---

## Step 2 — Crash → re-placement → snapshot restore  (O2, O3, O4, O6, O7)

```bash
sudo podman rm -f falak-<id>-0
```

**✅ Look for (T1) — the full recovery chain:**
```
runtime: container exited                 ← O2 (event-driven detect)
container crash detected, requesting re-election
capsule replica unassigned                ← O3 (clear binding)
delay strategy: eligible → election won   ← O4 (re-election completes, no dead-lock)
runtime: restoring from snapshot          ← O7 (NOT "cold starting", NO image pull)
runtime: container running
```
- **No** `WinElectionWithBinding failed` / `MarkRunning failed`  (O6 — clean FSM).
- **No** `restore failed, falling back to cold start`  (O7 — restore works).

```bash
sudo /tmp/falak --insecure --node node1 capsule list      # back to running, fresh container
```

---

## Step 3 — Delete stops the container (no orphan)  (O8)

```bash
sudo /tmp/falak --insecure --node node1 capsule delete <id>
sudo podman ps | grep falak-        # ← EMPTY (no orphan)
```
**✅** Container gone from `podman ps`; T1 shows **no** re-election fired by the
delete (O8 uses the O2 ignore-set).

---

## Step 4 — 3-node cluster: gravity differentiation + spreading  (O9, O10)

**T2 / T3:**
```bash
sudo /tmp/falak daemon start --name=node2 --port=4002 --cluster=$CLUSTER --psk=$PSK --bootstrap=$BOOT --runtime-socket=$SOCK --log-level=info
sudo /tmp/falak daemon start --name=node3 --port=4003 --cluster=$CLUSTER --psk=$PSK --bootstrap=$BOOT --runtime-socket=$SOCK --log-level=info
```
```bash
sudo /tmp/falak --insecure --node node1 node list         # 3 nodes, all Active

for n in web1 web2 web3 web4; do
  sed "s/name: \"api\"/name: \"$n\"/" /tmp/cap-api.cue > /tmp/$n.cue
  sudo /tmp/falak --insecure --node node1 capsule create -f /tmp/$n.cue --cluster=$CLUSTER; sleep 2
done
sudo podman ps --format '{{.Names}}'
```
**✅** `gravity_score` differs by node (busier = lower), and the 4 capsules
**spread across nodes** — not all on node1 (O9/O10). Before this work: all
scored 0 → node1 won everything.

---

## Step 5 — Node failure: single winner, no split-brain / no double-winner  (O14, O14c)

Kill the node running one capsule:
```bash
sudo pkill -9 -f "name=node2"
```
**✅ On survivors:** SWIM detects the failure → re-election where **exactly one**
node logs `election won` and the other `election lost`.
- **No** `no winner observed`  (O14 split-brain fixed).
- **Not** two nodes both winning / two containers for the same replica  (O14c double-winner fixed).

---

## Step 6 — Snapshot replication HA  (O11)

Confirm the snapshot exists on more than one node:
```bash
sudo ls /root/.local/share/falak/node1/snapshots/*/ 2>/dev/null
sudo ls /root/.local/share/falak/node3/snapshots/*/ 2>/dev/null   # replicated copy (K=2)
```
**✅** Archive present on the holder **and** a second node. Then kill the
**holder daemon** (not just the container): the survivor wins re-election and
logs `restoring from snapshot` from its **local** replica — no cold start,
even though the original holder is dead.

---

## Step 7 (optional) — Same-node group crash recovery  (O5b)

Deploy a same-node CapsuleGroup via `--config=node.cue` (groups don't go
through `capsule create -f` yet), let both members run on one node, then kill a
member container.
**✅** The group re-elects and re-wins **on the same node** — no
`GroupClaimFailed` wedge.

---

## Cleanup

```bash
sudo /tmp/falak daemon stop         # each terminal, or: pkill -9 -f "falak daemon start"
sudo podman rm -f $(sudo podman ps -aq --filter name=falak-) 2>/dev/null
sudo rm -rf /root/.local/share/falak/node*
```

---

## Fast subset (if short on time)

- **Steps 1–2** — snapshot capture + restore end to end (O7 + the O2/O3/O4/O6 recovery loop).
- **Steps 4–5** — gravity scores non-zero & spreading (O9/O10) + single-winner election (O14/O14c).

Those two cover the bulk of the recent work.

---

## Fix → layer map

| Fix | Validated in |
|---|---|
| O2 event-driven crash/removal detect | Step 2 |
| O3 clear stale replica binding | Step 2 |
| O4 release claim slot on Win | Step 2, 5 |
| O5b same-node group member unbind | Step 7 |
| O6 FSM downgrade on crash | Step 2 (absence of FSM warnings) |
| O7 snapshot restore (import fix) | Step 1 (capture), Step 2/6 (restore) |
| O8 delete stops container | Step 3 |
| O9 gravity scores non-zero | Step 1, 4 |
| O10 snapshot-locality wired | Step 4/6 |
| O11 snapshot replication HA | Step 6 |
| O14 no split-brain (single winner) | Step 5 |
| O14c no double-winner | Step 5 |

Notes: snapshotting needs CRIU (v4.2 built from source on kernel 6.17) + the
**rootful** Podman socket (`--runtime-socket=/run/podman/podman.sock`) and the
daemon + CLI run as root. Without root, control-plane + gravity + election
still work but capsules cold-start every time (no snapshot fast-path).
