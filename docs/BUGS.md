# Falak — Bug & Gap Tracker

Bugs and gaps discovered during manual end-to-end testing (session 17+).
Each entry has: observed behavior, root cause + location, proposed fix,
status. Fix order is rough priority — top of "Open" is the next thing
worth doing.

---

## Open

### O14. Election split-brain — nodes disagree on the winner → "no winner observed" (rare)

**Observed (Session 21, unmasked by O13):** `TestElection_3Node_SinglePicker`
flakes ~1/13 with `election_integration_test.go:177: no winner observed within
deadline`. Logs show the three nodes naming TWO DIFFERENT winners and ALL THREE
logging "election lost" — nobody claims → no winner runs the capsule.

**Why now:** pre-existing, not an O13 regression. Before O13 this test failed
EARLIER at the phonebook-count convergence barrier, MASKING this rarer election
race. O13 fixed convergence (the test now gets past setup), exposing the
split-brain. So O13 is strictly an improvement; this is a distinct, older bug.

**Hypothesis (needs investigation):** election claim/verdict agreement race —
two nodes each compute a different winner (tiebreak on score→timestamp→nodeID)
and both step aside, so no node claims. Possible contributors: (a) the new
gravity scoring (Step-2) producing near-equal scores that hit the tiebreak more
often; (b) claim-propagation timing under the O13 burst-sync churn; (c) a
genuine tiebreak-asymmetry where nodes don't deterministically agree. The many
"rejected sync request from unauthenticated peer" WARNs during the burst window
(O13 burst races peer auth) are noise here (phonebooks did converge) but worth
reducing separately (burst should back off a peer that rejects as unauth).

**Next step:** trace the election claim/tiebreak agreement path; determine why
two nodes name different winners. Likely needs architect review (election-core
agreement). Distinct from O5/O5b (those are the group/slot axis).

**Status: Open.** Rare (~7%); election agreement correctness. Does NOT block
O13 (committed, fixed the dominant convergence flake — TestCapsuleLifecycle_3Node
10/10, NodeFailureReElection 5/5, CascadeDelete 5/5).

---


### Gravity + Snapshot cluster (O9/O10/O11) — architect-reviewed design + sequencing

> Reviewed by falak-architect Session 19. Supersedes the per-entry
> "proposed fix" notes below where they conflict. Hard gate: **never ship
> O10 before the Step-2 scoring bundle** — doing so pins workloads onto
> sick nodes.

**Key correction to the causal model:** on a *crash* the snapshot holder
is DEAD — not a candidate (fails eligibility, publishes no claim). So
reliability does not "break a tie" with the dead holder; there is no tie.
The real opposing-factor risk is the **gray-failure holder** (alive,
`Active`, still claiming, but degraded) keeping its snapshot bonus.

**Sequencing:**
- **Step 1 — O9-B alone** (optimistic reliability). **IMPLEMENTED
  (Session 19).** Fixed in the PROVIDER (`node/metrics/provider.go`
  `buildState`), NOT the phonebook: when `entry.ConnectionAttempts == 0`
  the read path substitutes a configurable `neutralReliability` (default
  1.0, override via `WithNeutralReliability`) instead of
  `entry.SuccessRate`. `SuccessRate` stays honest (it feeds
  health/eviction/sync) — the optimistic prior is applied at the decision
  seam only. Covered by `node/metrics/provider_test.go`.
  Note: O9 is only PARTIALLY addressed — O9-B is done; O9-A (the
  headroom guard) plus the rest of the scoring-correctness work remain in
  the Step-2 bundle below.
- **Step 2 — scoring-correctness bundle (ship together):**
  1. O9-A: delete the `if required <= 0 { return notApplicable }` guard
     in the three headroom factors (keep the `total <= 0` guard). The
     existing `(free-required)/total` formula already yields `free/total`
     when required=0. Rewards EMPTIER nodes (relative free-fraction,
     K8s LeastAllocated) — NOT larger nodes (do not add absolute
     capacity; it fights spreading).
  2. Tension-#4 fix: `load_penalty` is currently subtracted and adds its
     weight to the denominator but 0 to the numerator on an empty node —
     it can ONLY drag scores down and compresses dynamic range. Re-model
     it as a SYMMETRIC positive free-capacity factor (`value =
     1-utilization`, contributed via `addFactor` like every other
     factor). Delete the special-case subtraction block (gravity.go
     ~150-161).
  3. Tension-#3 fix: O9-A does NOT spread IDLE capsules (idle nginx uses
     ~0 CPU → headroom ~1.0 everywhere). Keep `load_penalty`
     count-based (committed-capsule proxy, distinct from live-utilization
     headroom) but strengthen it: softcap 50→~20 and/or weight 0.5→1.0,
     so stacking N capsules actually moves the score.
  4. Weight rebalance: **Reliability 0.4 → 0.8** so health ≥
     snapshot-locality (0.6). Health is availability; locality is an
     optimization — health must win. (Affinity 0.8 left as-is — user
     intent — but now balanced with reliability.)
  - Acceptance: multi-node cluster yields differentiated, non-zero
    scores AND idle no-resource capsules spread instead of stacking.
- **Step 3 — O10, GATED on Step 2.** Wire `WithSnapshotLookup` at
  node.go:1381. Add AGE-DECAY to `factorSnapshotLocality` (1.0 fresh →
  0 near TTL): a stale CRIU image can be worse than cold start, and
  decay also damps the O11 reinforcement loop. Acceptance: a healthy
  holder outscores an identical non-holder, but LOSES to a healthy peer
  when the holder is loaded/degraded.
- **Step 4 — O11, last.** Default **K=2** (K=1 is just the bug renamed).
  Holder-driven push-after-capture. Targeting: **failure-domain-primary**
  (different DC/node than holder + each other) + disk-headroom filter +
  power-of-two-choices for the gravity tiebreak (NOT gravity-argmax —
  that herds all copies onto the beefiest nodes). ACK+retry; receivers
  re-broadcast `SnapshotAvailable`. Required sub-tasks / failure modes:
  - **Index↔membership reconciliation** (critical): nothing prunes a
    Failed/Departed node's entries from the discovery index, so the
    puller still targets the dead holder → cold start even WITH
    replication. Subscribe the snapshot index to the membership/departure
    event stream.
  - **Standby pinning**: standby copies are not `InUse`, so TTL/LRU
    eviction silently drops below K. Pin standbys (or re-replicate on
    eviction).
  - **Thundering herd**: per-node outbound-replication concurrency
    semaphore + jittered push + priority (0-copy capsule outranks
    K-1-copy).
  - Tradeoff to accept: optimistic reliability (O9-B) lets brand-new
    empty nodes claim aggressively on unproven trust — intended.

---

### O12. Add execution-reliability (placement-failure) gravity factor; strengthen connection-failure signal

**Requested (Session 19):** gravity should penalize nodes by (a) how
often their reconnection failed, and (b) how often they failed to PLACE
capsules (won election → container never reached Running).

**Current state:**
- (a) Connection/reconnection failures ARE already tracked
  (`phonebook.Entry.ConnectionAttempts/ConnectionSuccess/SuccessRate/
  ConsecutiveFails`) and ALREADY feed gravity: `factorReliability` reads
  `NodeState.ReliabilityScore`, which the provider sets from
  `entry.SuccessRate` (`node/metrics/provider.go:130`). **DECISION
  (Session 19): (a) is considered covered — no separate
  connection-failure factor will be added.** Only (b) below is new work.
- (b) Placement/execution failures are NOT tracked per node. Only
  per-capsule retry counters + the `CapsuleExecutionFailed` /
  `MemberPlacementFailed` events exist. No aggregate "this node fails to
  run what it wins."

**Proposed design (new factor `execution_reliability`):**
- The election scores the LOCAL node only, so each node tracks its OWN
  execution history — event-driven, no gossip needed for the hot path:
  subscribe to local start-success (CapsuleRunning) and start-failure
  (CapsuleExecutionFailed / reportStartFailure) and maintain a
  **rolling / time-decayed** success ratio (a node that had a transient
  problem since fixed must recover, not be penalized forever).
- New `factorExecutionReliability(node)` in [0,1]; add to weights
  (initial ~0.6, health-class weight — sits alongside Reliability).
  Persist the counter (metrics store or phonebook) so it survives
  restart.
- **Attribution (DECIDED Session 19): EXCLUDE capsule-global failures.**
  A failure caused by a bad capsule (image 404, malformed spec) would
  fail on EVERY node and must NOT count against the node's execution
  score. Count ONLY node-attributable failures (start failed, checkpoint
  failed, disk/runtime errors). The runtime's `reportStartFailure`
  reason strings already distinguish categories ("image pull failed: …"
  = capsule-global, excluded; "start failed: …"/"create failed: …"/
  checkpoint = node-attributable, counted) — classify by reason before
  incrementing the counter.
- Folds into the Step-2 scoring bundle (adding a factor interacts with
  normalization + weights — see the O9/O10/O11 design block above).

**Status: Open.** Part of the gravity-scoring rework.

---

### O11. Snapshots are single-copy (lazy pull) — lost when the holder dies, no HA fast-restart

**Observed (Session 19 live, 3 nodes):** a captured snapshot exists on
only ONE node; it is not replicated across the cluster.

**Root cause (by design, but a gap for HA):** snapshot distribution is
lazy / pull-on-demand. On capture, the runtime broadcasts
`SnapshotAvailable` (`snapshot/discovery.go BroadcastAvailable`) — but
that carries **metadata only** (capsuleID, tag, checksum, size, holder
nodeID). Receivers run `consumeGossip` → `addToIndex`
(`snapshot/discovery.go:317,360`), which only records *"node X holds
snapshot Y"* in an in-memory index — they do NOT pull the bytes. The
actual archive is transferred only when a node needs it (wins election →
`restoreFromSnapshot` → `snapshotPuller.Pull` from a holder in the
index). There is no proactive replication after capture.

**Impact (HA failure mode):** if the sole holder dies, the re-election
winner looks up the index, finds only the dead node, the pull fails, and
it **cold-starts** — the snapshot fast-restart is lost precisely on node
failure, the case it exists for. Compounds O10 (locality preference is
unwired) and O9 (scoring inert): snapshot-aware placement can't help
when only one node ever has the snapshot.

**Proposed fix (design — needs decision):** add a configurable snapshot
**replication factor** (e.g. replicate to K=2 peers after capture),
targeted at the highest-gravity / most-likely-failover peers rather than
random, so a survivor can restore locally. Push-after-capture (active
replication) vs. opportunistic pre-pull on gossip receive are the two
shapes. Ties into the gravity work: replicate toward the nodes the
re-election is most likely to pick. Bandwidth/disk cost is the tradeoff —
hence configurable, default small.

**Status: Fixed (Session 20).** Implemented holder-driven,
push-after-capture replication per
`.claude/plans/snapshot-replication-o11.md` (all 8 parts):
- **K (default 2), all timings/thresholds configurable** via
  `snapshot.Replicator` functional options
  (`WithReplicationFactor`, `WithReplicationConcurrency`,
  `WithReplicationRetries`, `WithReplicationBackoff`,
  `WithReplicationPrePushJitter`, `WithReplicationDiskHeadroomMB`,
  `WithReplicationSampleSize`, `WithReplicationQueueSize`).
- **Push transport:** after `BroadcastAvailable`, `captureSnapshot`
  enqueues a non-blocking replication job. A bounded worker pool selects
  targets and opens `/falak/snapshot/replicate/1.0`; the target services
  the request by PULLING the bytes back over the existing
  `TransferServer`/`PullSnapshot`, so no new bulk protocol was added.
- **Target selection** (`snapshot/replication_select.go`):
  failure-domain-primary spread (different DC/node than holder + each
  other), disk-headroom filter, power-of-two-choices gravity tiebreak
  (NOT global argmax). Healthy (Active), not-already-holding.
- **ACK + bounded jittered retry**; WARN on shortfall (< K copies).
- **Receiver re-broadcast:** a node receiving a standby pins it and
  re-`BroadcastAvailable`s so every index reflects all K holders.
- **Index↔membership reconciliation (load-bearing):** `Discovery.PruneNode`
  wired to `NodeFailed`/`NodeDeparting` so the puller never targets a dead
  holder.
- **Standby pinning:** received standbys are `pinned` in the store;
  `EvictOverCap` respects the flag (TTL still applies) so eviction never
  silently drops below K.
- **Thundering-herd control:** per-node outbound concurrency semaphore
  (worker pool), jittered pre-push delay, and a priority queue where a
  0-copy capsule outranks one already at K-1.

Production wiring: the mesh (Discovery + Replicator + reconciler) comes up
on first cluster join and is injected into the runtime handler via
`Handler.SetSnapshotMesh`. Gravity input for the tiebreak is a node-side
suitability proxy (free-resource ratios + connection reliability) because
the election gravity calculator is local-node-only by the Session-13
`StateProvider.LocalNode` simplification; the snapshot package consumes
only the resulting float and the selection algorithm is pure/table-tested.

---

### O10. Snapshot-locality gravity factor is implemented + weighted but never wired

**Observed (Session 19, during O9 investigation):** the
`snapshot_locality` gravity factor (`election/gravity/factors.go
factorSnapshotLocality`, weight 0.6 in `DefaultWeights`) is supposed to
give a node a bonus when it already holds the capsule's snapshot, so
re-election prefers the fast-restore node. In practice it NEVER
contributes: the election calculator is built at `node/node.go:1381`
with only `gravity.WithCapsuleTargetLookup(lookup)` — `WithSnapshotLookup`
is never called, so `c.snapLookup` is nil and `factorSnapshotLocality`
returns `notApplicable` on every evaluation.

**Impact:** snapshot presence does not influence placement at all.
On crash/scale re-election the node holding the snapshot gets no
preference, so the platform may cold-start (or restore after a peer
transfer) instead of restoring locally even when a local snapshot
exists. Compounds O9 (with snapshot locality dead too, even MORE of the
gravity signal is inert).

**Proposed fix:** pass the snapshot store/handler as a
`gravity.SnapshotLookup` into `NewCalculator` at node.go:1381 (the same
place the capsule target lookup is wired). The `HasLocalSnapshot` method
already exists on the snapshot side. Add a test asserting a
snapshot-holding node outscores an identical node without the snapshot.

**Status: FIXED (Session 19, gravity Step 3).** The election calculator now
receives `gravity.WithSnapshotLookup(&electionSnapshotLookup{store:
n.snapshotStore})` in `initializeElectionManager`; snapshot store init was
reordered to run before election so the reference exists at construction. The
`gravity.SnapshotLookup` interface was redesigned to `LocalSnapshot(...)
(SnapshotInfo{Age,TTL}, ok)` and the factor now AGE-DECAYS the bonus
(`(1-age/horizon)^exponent`, horizon = record TTL or a configurable 72h
fallback) so a fresh snapshot scores ~1.0 and a near-TTL one ~0. Tag
derivation (`ImageDigest` else `Image`) matches the runtime restore path.
Tests in `election/gravity/snapshot_locality_test.go` +
`node/election_snapshot_lookup_test.go`. See `.claude/PROGRESS.md`
"Step 3". O11 replication (Step 4) remains open.

---

### O8. `capsule delete` does not stop/remove the running container (orphan)

**Observed (Session 19 live):** `falak capsule delete <id>` removes the
capsule metadata and withdraws the orbit announcement, but the Podman
container on the host node **keeps running**. `podman ps` still shows
`falak-<id>-0` after the capsule is gone from `capsule list`.

**Root cause:** The `EventCapsuleDeleted` handler
(`node/capsule_handler.go:2058`) publishes `CapsuleWithdrawn`, notifies
the service handler, withdraws from orbit, unregisters scaling, and
forgets election state — but **never calls the runtime to stop/remove
the container**. `capsule.Manager.Delete` (`capsule/manager.go:440`)
only deletes the store row. Nothing syncs the capsule deletion down to
Podman, so the container is orphaned (running, untracked). The runtime
already exposes `StopContainer(capsuleID, replicaID, gracePeriod)`
(used by the group-rollback path at line 1257) — the delete path just
doesn't invoke it.

**Proposed fix:** On capsule deletion, for every replica hosted on THIS
node (`replica.NodeID == h.nodeID`), stop+remove the container via the
runtime before/alongside the store delete. Because deletion propagates
via orbit withdrawal, each receiving node must stop its own local
replica(s) — wire the stop into both the local delete path and the
withdrawal-received path. Must be idempotent (container may already be
gone) and must NOT trigger the O2 crash/re-election path (use the
self-removal ignore-set added in O2/F28 — this is an intentional
teardown, not a crash). Add an integration test: create → running →
delete → assert container stopped AND no re-election fired.

**Status: FIXED (Session 20).** `CapsuleHandler.stopLocalReplicas`
(`node/capsule_handler.go`) is invoked from the `EventCapsuleDeleted`
branch of `onManagerEvent` BEFORE the rest of the teardown. It iterates
`event.Capsule.Replicas` and, for every replica where
`replica.NodeID == h.nodeID`, calls
`runtimeGroupRollback.StopContainer(capsuleID, replicaID, grace)` — the
SAME intentional-stop method the group-rollback path uses, which adds the
container to the runtime handler's self-removal ignore set BEFORE issuing
Stop/Remove (runtime `handler.go:1600`), so the resulting Podman
died/remove events are suppressed and the delete does NOT self-trigger
the O2 re-election. Both the local CLI-delete path and the
withdrawal-received path (`onOrbitMessage` "withdrawal" branch) converge
on `capsule.Manager.Delete` → `EventCapsuleDeleted`, so one wiring covers
both; each node stops only the replicas it hosts. Idempotent: a missing
container makes `StopContainer` return an error which is logged at Warn
and tolerated (deletion always completes). Grace window is configurable
via `WithCapsuleHandlerDeleteStopGrace` (default 10s).
Tests: `node/capsule_handler_delete_test.go`
(`TestEventCapsuleDeleted_StopsLocalContainer`,
`TestEventCapsuleDeleted_SkipsRemoteReplica`,
`TestEventCapsuleDeleted_IdempotentWhenNoLocalContainer`,
`TestEventCapsuleDeleted_NoRollbackHookNoPanic`); the O2 runtime-level
guard `TestHandler_IntentionalRemoveIgnored` covers the ignore-set
suppression that delete reuses.

---

### O9. Gravity score is inert (≈0) — resource/reliability inputs not populated

**Observed (Session 19 live):** election winner logs
`gravity_score: 0`. On a single node this still wins correctly (sole
eligible candidate; score only ranks competitors + sets wait time), so
it is not an outcome bug — but the *exact* 0 reveals gravity is scoring
on empty inputs.

**Reproduced on a real 3-node cluster (Session 19):** node1 wins with
`gravity_score: 0` even with two peers present — so this is NOT
single-node-only; placement on a live multi-node cluster is not
gravity-driven.

**Root cause — CONFIRMED (code + runtime repro, Session 19).** Two
compounding defects, both proven by running the real `buildState` →
`gravity.Calculate` path with a real sampled snapshot:

- **Bug A — headroom factors skipped for resource-unconstrained
  capsules.** `factorCPUHeadroom`/`MemoryHeadroom`/`DiskHeadroom`
  (`election/gravity/factors.go:46-91`) return `notApplicable` when the
  capsule requests no resources (`if required <= 0 { return
  notApplicable }`, lines 48-49/64-65/80-81 — documented behavior). The
  test/nginx capsule declares NO `resources:` block, so all three
  headroom factors are skipped even though the node's resources are fully
  populated (verified: `CPUCoresTotal:12, MemoryMBTotal:40028`).
- **Bug B — fresh-node reliability is 0, not the documented 1.0.**
  `factorReliability` (`factors.go:188`) docs *"defaulting to 1.0 for
  newly-joined nodes"*, but the provider feeds `entry.SuccessRate`
  (`node/metrics/provider.go:130`) = 0 until SWIM probes accrue.

With headroom gone, reliability (0) and `load_penalty` (0 on an empty
node) are the only factors → `weightedSum = 0` → **Score 0**. Runtime
repro proof: factors map = `{load_penalty:1, reliability:0}` → score 0;
flipping ONLY reliability 0→1.0 → score 44.4. So all nodes on a fresh
cluster running a no-resource-request capsule score exactly 0, and the
winner is decided by jitter + lexicographic node-ID tiebreak (matches
the live "node1 always wins, score 0" observation).

**Proposed fix:**
- PRIMARY (Bug B): a node with no probe history should default to
  reliability ~1.0 (optimistic), not 0 — e.g. initialize phonebook
  `SuccessRate` to 1.0 for a new entry (0 attempts ⇒ assume reliable),
  or have the provider pass 1.0 when `ConnectionAttempts == 0`. This
  alone restores differentiation (nodes diverge as probe history
  accrues) and lifts the score off 0.
- SECONDARY (Bug B, design — raise with product): decide whether
  resource-unconstrained capsules should still prefer emptier/larger
  nodes (absolute free-capacity headroom even with no request), or
  whether reliability/load/affinity are sufficient signal. If the
  former, add an absolute-capacity factor that applies regardless of
  request.

**Impact:** On a multi-node cluster placement is NOT actually
gravity-driven — every node scores ~0, so the winner is decided by claim
timing/jitter + lexicographic node-ID tiebreak rather than fit (CPU/mem
headroom, reliability, locality). The "gravity-aware placement" value
prop is effectively inert until fixed.

**Proposed fix:** Ensure the local `NodeState` the gravity calculator
receives carries real resource totals (CPUCoresTotal/MemoryMBTotal/
DiskMBTotal from host capabilities — already sampled for `node list`)
and a sane reliability default (start reliability at a neutral value,
not 0, until probe history accrues — a brand-new node should not score
worst-possible on reliability). Verify a multi-node cluster produces
differentiated, non-zero scores. Add a unit test asserting Calculate
yields >0 for a node with populated totals and neutral reliability.

**Status: Open.** Quality/correctness for multi-node placement; not a
single-node blocker.

---

### O5. Group-claim election shares the retained-slot defect O4 fixed for single replicas

**Found (Session 19, while landing O4/F30):** the group-claim path
(`election/manager_group.go`) keeps a per-group in-flight guard
`localGroupClaims` that mirrors the single-replica `localClaims`. Its
`reportGroup` Won arm (≈line 321) does NOT release the slot — only Lost/Failed/
cancelled and `ForgetCapsule` do — so the slot is retained across a Won group
election just as `localClaims` was pre-O4. And unlike the single-replica path
(which retries via `waitForCapsuleClaimReleased`), `runGroupElection` jumps
straight to `waitForRemoteGroupVerdict` when `tryLocalGroupClaim` fails
(≈line 176). The predicted symptom is identical: a same-node group
re-election after a Won times out with "group election timeout (no claim
heard)" and the group is never re-placed on the only node.

**Why O4's fix doesn't transfer mechanically:** the single-replica fix made the
winner binding synchronous via `WinElectionWithBinding`→`AssignReplica`, then
released the slot after the binding was durable. The group path has no
equivalent per-member `AssignReplica` on the claim path — group placement
materializes members through the capacity reservation + runtime fan-out — so
the durable-state replacement for the retained slot needs its own design
(likely: release `localGroupClaims` on Won once the reservation is recorded,
with the reservation as the durable anti-over-commit guard). Needs
architect review before implementing.

**Status: FIXED (Session 20).** Three-part fix in `election/manager.go`,
`election/manager_group.go`, `node/capsule_handler.go`:
- **(a) Release the group slot on Won after the reservation is recorded.**
  Added `groupClaimReleased map[capsule.CapsuleID]chan struct{}` alongside
  `localGroupClaims` (guarded by `localGroupClaimsMu`); `tryLocalGroupClaim`
  installs a fresh channel, `releaseLocalGroupClaim` closes-and-deletes it
  idempotently, and new `waitForGroupClaimReleased` parks on it — all mirroring
  the single-replica helpers. Both Won arms of `runGroupElection` now
  `releaseLocalGroupClaim(req.GroupID)` AFTER `recordReservation` and BEFORE
  `reportGroup(...Won())`. `reportGroup`'s Won arm does NOT release (no
  double-release). The Failed-arm release is unchanged.
- **(b) Park-wake-redecide loop.** The `tryLocalGroupClaim`-fails jump to
  `waitForRemoteGroupVerdict` is replaced by a loop that parks on
  `waitForGroupClaimReleased` (bounded by the round deadline) and, on release,
  re-runs `CalculateCombinedFit` against the current node state: still eligible
  → acquire slot + publish; ineligible (a live reservation from a DIFFERENT
  group over-commits the node) → step aside. Group twin of the single-replica
  loop, minus the per-replica fan-out.
- **(c) Clear the stale reservation on same-node re-election triggers.** AUDIT
  finding: the single-node MEMBER-CRASH / rollback path
  (`onMemberPlacementFailed`) ALREADY cleared the reservation before refiring
  (`CancelGroupInFlight` → `ClearGroupReservation` → `GroupReelectionRequested`)
  — so the single-node O5 is a SLOT deadlock, closed by (a)+(b), not a
  reservation deadlock. The plan's premise (that the node-FAILURE path already
  cleared) was inverted: the node-failure paths
  (`emitGroupReelectionForGroup`, `maybeEmitGroupReelection`) only cancelled
  in-flight; they now also `ClearGroupReservation` in the same
  cancel→clear→refire order, removing a guaranteed-stale reservation (held by
  the failed node) that would otherwise over-commit the surviving node.

Tests (`election/manager_group_o5_test.go`, `node/capsule_group_o5_test.go`):
`TestRunGroupElection_WonReleasesGroupSlot`,
`TestGroupReElection_SameNode_MidFanout` (manager-level, reservation-live
re-win), `TestGroupClaim_ConcurrentSameGroup_NoDoubleClaim`,
`TestGroupClaim_ConcurrentDifferentGroups_ReservationRefuses`,
`TestGroupReElection_SameNode_AfterWin` (node-level end-to-end). The two
SameNode/slot-release tests fail on the pre-fix code with "group election
timeout (no claim heard)" / "group slot not released after win" and pass after.
Concurrency tests green at `-count=20`.

---

### O5b. Same-node group re-election is refused by member self-anti-affinity (stale member bindings never cleared) — FIXED (Session 22)

**Found (Session 20, while landing O5):** a same-node group re-election is
refused by MEMBER self-anti-affinity before the group election can win.
Trace: each group member's per-replica election records a durable
replica→node binding (`WinElectionWithBinding`→`AssignReplica`, `NodeID` set).
When a same-node group re-election runs `CalculateCombinedFit`, `IsEligible`
per member consults `electionCapsuleLookup.NodesRunningCapsule`
(`node/election_handler.go`), which returns any node with a member replica
whose `NodeID != ""` — so a still-bound member makes the local node
self-anti-affinity-excluded → "group ineligible: member X does not fit" →
`GroupClaimFailed`.

**Root cause:** no same-node group re-election trigger clears member bindings.
`emitGroupReelectionForGroup` / `maybeEmitGroupReelection` / the
`onMemberPlacementFailed` rollback resync sibling FSM state (`SyncStatus`→
Announced / `StopCapsule`) and clear the group reservation, but NONE call
`capsule.Manager.UnassignReplica` on siblings. The only production caller of
`UnassignReplica` is `onContainerCrash` (the O3 per-replica crash path).

**Severity:** latent on multi-node (masked whenever a fresh, never-bound node
can satisfy fit); FATAL on single-node and under capacity pressure (when the
only eligible home is a previously-bound node, the group wedges — the same
failure O5 targets, just via a different axis). The fix mirrors O3: add
`UnassignReplica`-per-sibling to the same-node group re-election trigger paths
before refiring. Separate from O5 (reservation-slot lifecycle) — this is
replica-binding lifecycle.

**Status: FIXED (Session 22).** Mirrors O3's `onContainerCrash` clear. Added a
per-sibling `UnassignReplica` loop — iterating each sibling's snapshot
`Replicas` (robust; not hardcoding replica "0") — to every same-node group
re-election trigger path, positioned inside the existing sibling-rollback loop
and BEFORE the `GroupReelectionRequested` publish so the re-election evaluates
member eligibility against cleared bindings. Idempotent (`UnassignReplica`
no-ops an already-unbound replica); errors logged at Debug and skipped so the
rollback always completes. Trigger paths fixed (`node/capsule_handler.go`):
- `onMemberPlacementFailed` — the core same-node fatal case (member-crash /
  placement-failure rollback).
- `emitGroupReelectionForGroup` — the onNodeFailed reservation-holder path;
  the binding points at the DEAD node, so the clear is hygiene (a stale
  dead-node member binding otherwise skews `NodesRunningCapsule` for the other
  members' placement).
- `maybeEmitGroupReelection` — the onNodeFailed per-member path; it already
  walks siblings, so the clear starts the node-failure re-election from cleared
  bindings too.

**Test tripwire removed.** The O5 node-level
`TestGroupReElection_SameNode_AfterWin` previously cleared member bindings in
setup (`resetGroupForSameNodeReelection`, with a TRIPWIRE comment pointing
here). That manual clear + helper is deleted; the test now drives the real
production trigger (`onMemberPlacementFailed` → `GroupReelectionRequested`) and
still re-wins on the same node. New `TestGroupReElection_SameNode_O5b_
ClearsBindingsThenReWins` drives the same real trigger and asserts BOTH halves:
(1) every member replica binding is cleared (`NodeID == ""`) after
`onMemberPlacementFailed` (the unit-level assertion mirroring O3's crash-path
binding-clear test), and (2) the refired same-node re-election re-wins on the
only node — proving eligibility was restored with NO manual `UnassignReplica`.

---

### O13. 3-node join convergence gap — joiner gets an incomplete roster (`expected N, got N-1`)

**Observed:** ~10 3-node integration tests fail at CLUSTER-JOIN (before any
restart) with `expected N phonebook entries ... got N-1 (timeout)`. This is
the long-standing "pre-existing O1 failures" set — but the architect (Session
19) flags it as a SEPARATE convergence bug, NOT O1's restart-reconnect issue:
no connection dropped, the third entry never *propagated* in time.

**Hypothesis (architect, needs confirmation):** the roster handed to a joiner
at AuthComplete (`ClusterMembersReceived` path) is the voucher's OWN phonebook
snapshot, which is itself incomplete at that instant (e.g. node2 vouches node3
before node2 has finished recording node1). There is no anti-entropy that
SYNCHRONOUSLY completes the roster before the test asserts — delta-sync heals
it eventually but too late for the assertion. Classic push-incomplete-roster
+ async-sync-heals-late gossip gap. Alternatives: PendingAuth entries not
counted; sync latency vs assertion timing.

**Confirmed mechanism (Session 21):** node1 seed; node2 AND node3 both vouch
via node1. node3 gets the full roster from node1's AuthComplete. The gap is
**node2 learning node3**: node2's one-shot post-join sync already fired before
node3 existed; its only path to node3 was node1's best-effort Step-2
`NewMemberAnnounced` PubSub broadcast — if node2's gossipsub mesh wasn't ready
when node1 published, node2 missed it and waited `DefaultSyncInterval` (then
5m) >> the 10s test window.

**Fix (Session 21, layered — one deterministic path + one guaranteed-eventual
backstop):**
- **Layer 1 — voucher fan-out push (primary, deterministic).** The voucher
  emits `events.MemberAdmitted` after AuthComplete; the syncer subscribes
  (event-driven seam — auth never calls sync directly) and actively pushes the
  new member to every existing **Active** peer over a dedicated
  `/falak/sync/push/1.0` stream (`SyncPush`/`SyncPushAck`). The receiver
  applies the SAME `phonebook.Exists` auth gate as pull sync and inserts via
  the existing idempotent `processMember`. Bounded by a concurrency semaphore
  (`WithMemberPushConcurrency`, default 8) and a per-push timeout
  (`WithMemberPushTimeout`, default 5s). Best-effort per target; failures fall
  through to Layer 2.
- **Layer 2 — convergence-burst anti-entropy (backstop, guaranteed-eventual).**
  The single-rate periodic loop became two-rate: on `ClusterJoined` and every
  membership change (`NewMemberReceived`, `MemberAdmitted`) it enters a burst
  window syncing every `burstInterval` (1s, jittered ±250ms) for
  `burstDuration` (30s), then settles to the steady `syncInterval`. Injectable
  clock for deterministic tests; mandatory jitter prevents mass-join sync
  storms.
- **Layer 3 — Step-2 mesh-readiness gate (hardening).** `publishStep2` waits
  (bounded by `WithStep2MeshWaitTimeout`, default 2s) for the auth topic's
  gossipsub mesh to have ≥1 peer before the first publish, then publishes
  anyway on timeout so a join never stalls.
- **Rate limit raised** 20→60/min/peer so a 1/s×30s burst never trips the
  receiver limiter (silent rejection would defeat the backstop).
- **Steady interval dropped** 5m→90s as defense-in-depth for the worst-case
  tail.

**Status: Fixed.** Deterministic unit tests in `node/sync/` (push fan-out,
burst backstop via mock clock, voucher-death, mass-join storm) pass under
`-race -count=20`; the previously-flaky 3-node integration tests pass under
`-race -count=10`. Distinct from O1.

**O13-adjacent (fixed as part of landing O13):** making convergence
deterministic let `TestCapsuleGroup_NonCascadeDelete_3Node` reach the
group-delete → announce path that the phonebook-convergence barrier had
always masked, exposing a **pre-existing capsule-module data race**: the
manager emits events carrying the LIVE `*Capsule`, and the announce path
(`CapsuleHandler.announceCapsule` → `orbit.AnnounceOn` →
`replicaStatesToProto`) iterated `c.Replicas` with no lock while
`Manager.AssignReplica` mutated that same slice under `m.mu`. This race is
reachable outside O13 too (any group-delete-triggered announce concurrent
with an election-lost `AssignReplica`); O13 merely made it deterministically
hit in CI. **Fix:** `announceCapsule` now serializes from a race-safe
`Manager.Get(c.ID)` snapshot (deep-copies `Replicas` under `m.mu.RLock` — the
sanctioned accessor) instead of the live pointer. Verified with
`TestCapsuleGroup_NonCascadeDelete_3Node -race -count=20`.

**Follow-up smell (separate ticket):** the capsule event bus emits LIVE
mutable `*Capsule` pointers (`Manager.emit`), so every event consumer is one
lockless mutable-field read away from this class of race. The correct
long-term fix is for `emit` to carry immutable snapshots; deferred here to
avoid perturbing the event-bus contract other consumers rely on under an O13
ticket.

---

### O1. No active re-dial — a restarted node (incl. the bootstrap) stays partitioned

**Observed (Session 18 manual pass):** Start 3 nodes, with node2 and
node3 both bootstrapped via node1 (`--bootstrap=<node1>`). Kill node1,
then restart it with the same identity. node2 and node3 do **not**
reconnect to node1 — the cluster stays partitioned (node1 alone;
node2+node3 without node1) even though all three still hold each other
in their persistent phonebooks with stored multiaddrs.

**Root cause:** Bootstrap dialing happens only once, at startup
(`node/node.go` ≈line 784). The only reconnection logic is the
*inbound* path (`node/node.go:1241`, "departed peer reconnected,
awaiting re-auth") which merely *reacts* when the other side happens to
redial. There is no active *outbound* re-dial loop: nobody re-initiates
a connection to a peer whose libp2p link dropped. A restarted node1 has
no `--bootstrap` of its own (it was the seed), and node2/node3 never
re-dial node1, so libp2p — which does not auto-reconnect — leaves the
partition in place. SWIM eventually removes the dead node but does
nothing to re-establish the link when it returns.

**Proposed fix:** Add a reconnection manager/loop that periodically
re-dials known phonebook peers that are currently disconnected (using
their stored multiaddrs), and on node start dials *all* phonebook peers
rather than only `--bootstrap`. Treat `--bootstrap` as seed-only and
maintain the steady-state mesh actively. libp2p's backoff connector /
a per-peer reconnect with jittered backoff is a good fit; key it off
phonebook entries and the SWIM `failed`/`departed → seen-again`
transitions.

**Status: FIXED (Session 21).** Implemented the phonebook-driven
reconnector per `.claude/plans/reconnector-o1.md`:

- **`node/reconnector.go`** — a single WaitGroup-tracked goroutine on a
  jittered tick, ctx-cancelled on Stop. Per tick, for each joined cluster it
  builds a candidate set = dial-worthy `GetByCluster` entries ∪ `--bootstrap`
  seeds (deduped). Dial-worthiness = status ∈ {Active, Suspected,
  Quarantined, Failed} (NOT Departed) AND not `network.Connected` AND past
  per-peer backoff AND has ≥1 address. Failed/absent candidates are flipped
  to `PendingAuth` **before** dialing (SWIM false-suspect gate). On dial
  success it publishes `ReauthWithPeerRequested{ClusterPath, PeerID}`; on
  failure it bumps a capped-exponential per-peer backoff
  (`min(base·2^fails, max)` ± jitter) and resets it on a successful dial. The
  backoff map is pruned every tick against current membership (bounded).
  `--bootstrap` seeds are permanent, backoff-bounded candidates.
- **`ReauthWithPeerRequested`** event (`node/internal/events/events.go`) —
  the dial↔re-auth boundary. The reconnector only dials + emits; auth owns
  the handshake.
- **`node/auth/reauth.go`** — `ReauthSubscriber` subscribes to the new event
  and re-authenticates the pinned peer via the existing `Authenticate` flow
  (falling back to another phonebook peer if that peer refuses). Refactored
  onto a small `Reauthenticator` interface for testability.
- **`node/node.go`** — constructs the Reconnector in Start (after
  host+phonebook+auth), feeds each cluster's `--bootstrap` list as seeds in
  Join, and Stops it in cleanup before host.Close.

Note on `TestElection_NodeFailureReElection`: the Session-19 plan tentatively
predicted this might go green from O1. It does NOT — verified against the
pre-O1 baseline, it fails at the IDENTICAL initial-join barrier
(`election_integration_test.go:219`, `expected 3 phonebook entries, got 2`)
with AND without O1. That barrier is O13 (the 3-node join-convergence gap)
and executes at test setup, before the node drop that O1's re-dial + re-auth
would heal. O13 remains a separate, open bug.

---

**Historical:** All originally-tracked bugs (Session 17 and the
post-Session-17 polish pass — #11, #13, #19, #21) are closed; see
F1–F28 below. O1 above (and O2, now fixed in F28) were found in the
Session 18 manual end-to-end pass.

---

## Fixed (manual testing pass)

### F32. O6 — Capsule lifecycle FSM not downgraded from `running` on crash → election/ready transitions rejected

**Observed (Session 19 live):** during crash recovery the lifecycle FSM
rejected two transitions (logged, execution limped through):
```
WinElectionWithBinding failed ... stateless: No valid leaving transitions
  are permitted from state 'running' for trigger 'election_won'
runtime: MarkRunning failed ... trigger=container_ready ... from state 'running'
```
The capsule recovered (re-elected, container running) but its lifecycle
state machine was left stuck at `running` the whole time and both recovery
transitions were rejected.

**Root cause:** The crash path (`node/capsule_handler.go onContainerCrash`)
cleared the *replica* binding (`UnassignReplica`, O3/F29) and fired
re-election, but never downgraded the *capsule-level* lifecycle FSM. The
`running` state (`capsule/lifecycle.go:189`) does NOT permit `election_won`
or `container_ready`, so the re-election's `WinElection` and the runtime's
later `MarkRunning` were both rejected. Recovery only succeeded by accident
— `WinElectionWithBinding` returned early on the FSM error (skipping O4's
synchronous bind) and the redundant `handleElectionWon` mirror re-bound the
replica off `EmitWon`. Fragile and state-inconsistent throughout.

**Fix:** Added `capsule/manager.go` `MarkNodeFailed(id)` — a godoc'd Fire
wrapper for `TriggerNodeFailed` (`running → announced`, the transition
already declared at `capsule/lifecycle.go:191`), consistent with the
existing `StartExecution`/`MarkRunning` convenience wrappers.
`onContainerCrash` now fires `MarkNodeFailed(id)` ONCE (the FSM is
per-capsule, not per-replica) before the per-replica `UnassignReplica` +
`requestElection` loop, so the re-election round starts from `announced`
with a cleared binding and the normal forward chain `announced → electing
→ assigned → executing → running` proceeds cleanly — `WinElectionWithBinding`
performs its intended synchronous bind rather than relying on the mirror.
The downgrade tolerates rejection (Debug log, continue): a non-`running`
state or a sibling replica's crash event may have already downgraded it.
Per-replica state continues to live in `c.Replicas[i].Status`; the coarse
capsule-level FSM model is unchanged.

**Tests:**
- `capsule/manager_test.go::TestMarkNodeFailed` — a capsule driven to
  `running` downgrades to `announced` on `MarkNodeFailed`; from a
  non-`running` state the call is rejected and leaves the state unchanged.
- `node/capsule_handler_reelection_test.go::TestOnContainerCrash_ClearsBindingThenFiresElection`
  — extended: the capsule FSM is driven to `running` before the crash and
  asserted to be `announced` after `onContainerCrash` (in addition to the
  binding-cleared + election-fired assertions).
- `node/crash_recovery_integration_test.go::TestSingleNode_CrashRecovery_Replaces`
  — extended: captures lifecycle logs via a `zaptest/observer` core and
  asserts the capsule reaches `running` after recovery with ZERO
  `WinElectionWithBinding failed` / `MarkRunning failed` lines. The test
  goes from "passes while logging FSM errors" to "passes with zero rejected
  transitions."

**Status: Fixed (F32).** The crash → re-election → running recovery loop is
now state-correct (zero rejected transitions). **Follow-up:** the group
crash path (if any `handleGroupMemberCrash` analogue exists) was NOT
audited or touched here — only the standalone `onContainerCrash`. Whether
group recovery needs the same coarse-FSM downgrade is open and tracked
alongside O5 (group-claim path).

---

### F31. O7 — Snapshot restore always failed — `import` passed as archive path, Podman expects a bool

**Observed (Session 19 live, rootful + CRIU):** snapshot **capture** worked
end-to-end (`runtime: snapshot captured`), but on re-placement the restore
fast-path **always failed and fell back to cold start**:
```
runtime: restore failed, falling back to cold start
  podman restore: status 400: {"cause":"schema: error converting value for \"import\"",
  ".../libpod/containers/<name>/restore?import=%2Froot%2F...%2Fnginx%3Aalpine&name=<name>"}
```
O2/O3/O4 correctly drove a re-election that *chose* restore (local snapshot
present), but the Podman call was malformed so every recovery cold-started
(pull + create + start) instead of restoring — the snapshot fast-restore
headline feature never delivered.

**Root cause:** `runtime/podman/runtime.go` `Restore` (~line 497) sent the
checkpoint **archive path** as the `import` query parameter with a `nil`
body: `q := url.Values{"import": {snapshotPath}, "name": {id}}`. Podman's
libpod restore endpoint types `import` as a **bool** (import-from-archive
flag); the archive bytes must be streamed in the **request body**
(`application/x-tar`), symmetric to the working `Checkpoint` path which
uses `export=true` and reads the archive from the *response* body. Passing
a filesystem path as a bool query value → `schema: error converting value
for "import"` → HTTP 400. The nil body meant there was nothing to import
even with the right flag.

**API shape empirically verified against the live Podman socket (2026-06-29):**
- `import=<path>` → 400 `schema: error converting value for "import"`
  (reproduces production; `import` is a bool).
- `import=true` + `Content-Type: application/x-tar` + archive in the body →
  passes schema parsing; Podman extracts the body to
  `/var/tmp/checkpoint.../` and reads `spec.dump` from it.

**Fix:** `Restore` now opens the archive at `snapshotPath`, sends it as the
POST request body with `Content-Type: application/x-tar`, sets
`import=true` (bool) plus `name`, and sets `req.ContentLength` from
`os.Stat` so the upload is fixed-length (not chunked — some libpod versions
reject chunked). The restored container inherits its checkpointed
network/ports/env config; the `RestoreOption` plumbing is retained for
callers but not forwarded as query params (the import-from-archive endpoint
rejects unknown keys — not fabricated). Doc comment rewritten to describe
import-from-body. `Restore` signature unchanged (handler.go
`restoreFromSnapshot` depends on it).

**Test (regression guard against silent rot back to cold-start):**
`runtime/podman/runtime_test.go::TestPodman_CheckpointRestoreRoundTrip` — a
live round-trip (Pull → Create → Start → Checkpoint to a temp path →
confirm container gone via `errors.Is(ErrContainerNotFound)` → **Restore**
→ Inspect shows Running → cleanup). The Restore call returned the 400
before this fix. Gated by `skipIfNoPodman`; checkpoint requires root + CRIU,
so on a rootless socket (where libpod surfaces a runc/CRIU checkpoint
failure) the test `t.Skip`s cleanly rather than failing.

**Status: Fixed (F31).** Snapshot fast-restore now exercises the correct
libpod import-from-body shape; the live round-trip test prevents regression.

---

### F30. O4 — Single-node re-election dead-locked on a retained local claim slot

**Observed (Session 18, while landing O3/F29):** After O3 clears the stale
replica binding on a container crash/removal, a single-node cluster STILL
failed to re-place the capsule. The re-election round started, the strategy
correctly logged `delay strategy: eligible` (O3 restored eligibility), then
~10s later reported `election timeout (no claim heard)` and the capsule stayed
`announced` with `NodeID=""`. O3 was necessary but not sufficient.

**Root cause:** `election/manager.go` kept a per-capsule in-flight claim guard
`localClaims map[string]bool`. It was acquired before publishing a claim and,
on a **Won** election, deliberately RETAINED (released only on Lost/Failed). On
a re-election for the same capsule on the same node, `runElection` saw
`hasLocalClaim == true` (held from the first win, never released — the holder
round had already finished) → `waitForCapsuleClaimReleased` blocked forever →
`waitForRemoteVerdict` → on a single node no remote claim ever arrives → "no
claim heard". Deadlock, independent of self-anti-affinity. Retaining the slot
after Win conflated a transient in-flight lock with durable placement state;
the real "this node already runs this capsule" guarantee was always provided
by self-anti-affinity in `gravity.IsEligible` via `NodesRunningCapsule` (which
reads `AssignReplica` bindings), not by `localClaims`.

**Fix (election-core, architect-reviewed):** Make the winner binding
synchronous on the claim critical path, then release the slot on Won.
- Widened the election `LifecycleController` (`election/manager.go`) with
  `WinElectionWithBinding(id, replica, nodeID)` — performs the Win FSM
  transition AND `AssignReplica` synchronously. The node adapter
  (`node/election_handler.go` `electionLifecycleAdapter`) implements it as
  `WinElection` then `AssignReplica`.
- `report`'s `OutcomeEnum.Won()` arm now calls `WinElectionWithBinding`, THEN
  `releaseCapsuleClaim` (AFTER the binding is durable), THEN `EmitWon`. Order
  is load-bearing: a parked sibling multi-replica round unblocks only on slot
  release, by which time the binding is persisted, so it re-decides ineligible
  and steps aside — preserving multi-replica anti-affinity through durable
  state instead of a retained slot. Both Won call sites route through
  `report()`, so this is the single release point.
- The existing `handleElectionWon` subscriber `AssignReplica` is now a
  redundant idempotent mirror on the winner; kept (harmless). The loser-mirror
  `handleElectionLost` path is unchanged.

**Why the moved binding is race-free:** the dangerous window is "won replica
A" → "A's binding persisted". The existing slot already serialises sibling
rounds (B is parked in `waitForCapsuleClaimReleased` on A's slot); A's slot is
released only AFTER `AssignReplica` returns, so B always re-decides against a
persisted A. No new lock needed — the binding simply moved to *before* the
release. Validated with `go test -race -count=20` of the multi-replica
anti-affinity regression.

**Tests:**
- `election`: `TestReport_WonReleasesLocalClaimSlot` (binding durable + slot
  released on Won), `TestReElection_SameCapsuleSameNode_AfterWin` (single-node
  recovery at the manager level), `TestMultiReplica_AntiAffinity_SameNodeNeverWinsBoth`
  (regression gate, run `-count=20`).
- `node`: the previously-skipped
  `TestSingleNode_CrashRecovery_Replaces` is now un-skipped and PASSES
  end-to-end — the full crash → re-placement loop (O2 event → O3 unbind → O4
  release → re-win → re-assign → mock runtime re-start) is closed and gated by
  this test.

**Follow-up — O5 (group path shares the defect, NOT fixed here):** the
group-claim election path (`election/manager_group.go`) keeps an analogous
per-group guard `localGroupClaims`. `reportGroup`'s `GroupClaimOutcomeEnum.Won()`
arm (≈line 321) does NOT release it — only the Lost/Failed/cancelled paths and
`ForgetCapsule` do. Worse than the single-replica path, `runGroupElection` has
no `waitForCapsuleClaimReleased`-style retry: when `tryLocalGroupClaim` fails
(≈line 176) it goes straight to `waitForRemoteGroupVerdict`, so a same-node
group re-election after a Won will time out ("no claim heard") exactly like the
pre-O4 single-replica path. The group path also has no synchronous winner
binding (group placement materializes members via the reservation/runtime fan-
out, not `AssignReplica`), so the O4 fix does not transfer mechanically.
Deliberately left out of O4 scope (per plan); tracked as **O5** in the Open
section. No live repro yet — group crash-recovery on a single node is the
likely trigger.

**Status: Fixed (F30).** The full crash → re-placement loop (O2 + O3 + O4) is
now closed and gated by the un-skipped single-node test. The group-claim
analogue is O5.

---

### F29. O3 — Crashed/removed capsule was never re-placed — stale replica→node binding blocked self-claim

**Observed (Session 18 live verification of the O2 fix):** On a single
node, after O2 correctly detected an out-of-band container removal
(`podman rm -f`) and fired re-election, the re-election **timed out with
"no claim heard"** and the capsule downgraded to `announced` and stayed
there — no container, no recovery. End-to-end log:
`container exited → attempting local restart → 404 no such container →
container crash detected, requesting re-election → election round started
→ (10s) election failed: election timeout (no claim heard)`.

**Root cause:** When the container was lost, the capsule's replica still
carried `NodeID = <this node>` in the local view — the assignment was
never cleared before re-election.
`electionCapsuleLookup.NodesRunningCapsule` (`node/election_handler.go`)
still reported this node as running the capsule, so the
self-anti-affinity check in `gravity.IsEligible`
(`election/gravity/eligibility.go`) — "a node never runs two replicas of
the same capsule" — marked the only node ineligible to re-place the
replica it had just lost. Single-node clusters dead-locked entirely;
multi-node clusters wrongly excluded the original node from reclaiming.
Pre-existing: any genuine crash reached the same path. O2 only made the
*removal* trigger reach it, exposing the deadlock.

**Fix:**
1. New `capsule.Manager.UnassignReplica(id, replicaID)` — clears the
   replica's `NodeID` (back to empty) and resets its `Status` to
   `Announced`, persisting under the manager mutex (same locking
   discipline as `AssignReplica`). Idempotent on unknown capsule /
   unknown replica / already-unbound. `NodesRunningCapsule` already
   skips empty-`NodeID` replicas, so once cleared the node stops being
   counted by self-anti-affinity.
2. `node/capsule_handler.go` — the `CapsuleExecutionFailed` subscriber
   (extracted to a testable `onContainerCrash`) now calls
   `UnassignReplica` for each locally-bound replica **before**
   `requestElection`. Order is load-bearing: unassign is synchronous and
   persists the cleared binding before the election round evaluates
   eligibility, so the originating node is eligible again.

The winner of the re-election re-announces Running (existing Won path →
`AssignReplica` + `SyncStatus` + re-announce), repairing peers' stale
view on the next gossip round — no new propagation path was needed for
recovery.

**Verification:** New deterministic tests, all green under
`go test -race`:
- `capsule/manager_unassign_test.go::TestUnassignReplica` — clear,
  idempotency cases, persistence across a store reload.
- `election/gravity/eligibility_test.go::TestIsEligible_SelfAntiAffinity_ClearedAfterUnbind`
  — Excluded while listed, OK after the binding is cleared (this is the
  exact eligibility flip O3 restores).
- `node/capsule_handler_reelection_test.go::TestOnContainerCrash_*` —
  binding cleared synchronously then `ElectionRequested` fired, in that
  order; remote-bound replicas untouched.

Live tracing of the single-node crash path confirmed O3 does its job: after
the crash the election strategy logs `delay strategy: eligible` (it was
ineligible/excluded before O3). The full single-node re-placement does NOT yet
complete end-to-end — that is blocked on O4 (retained `localClaims` slot), a
separate election-core defect O3's binding-clear exposed. The end-to-end
regression test
`node/crash_recovery_integration_test.go::TestSingleNode_CrashRecovery_Replaces`
is written and `t.Skip`'d referencing O4; it will go green once O4 lands.

**Status: Fixed (F29) — binding-clear half of the single-node crash/removal
recovery loop. The remaining half is tracked as O4.**

---

### F28. O2 — Runtime watcher never detected a *removed* container → no re-placement

**Observed (Session 18 manual pass, Layer 4/6b):** With a capsule
running, removing its container out-of-band
(`podman rm -f falak-<id>-0`) did **not** trigger re-placement. The
container stayed gone, Falak kept reporting the capsule as `Running`,
and no re-election fired. A wedged runtime socket also looped forever
with no escalation.

**Root cause:** `runtime/handler.go` `watchContainer` polled
`Inspect` every 2s and, on any error, did `continue // transient` —
forever. A removed container makes the Podman libpod API return HTTP
404, which `Inspect` collapsed into a generic `status %d` error
indistinguishable from a transient blip, so the removed case was
invisible.

**Fix (event-driven, reconcile-backstopped):**
1. `runtime.ErrContainerNotFound` sentinel; `podman.Inspect` and
   `mock.Inspect` return it `%w`-wrapped on not-found (HTTP 404 /
   unknown id).
2. New `Runtime.Events(ctx)` streams backend lifecycle events. Podman
   implements it over `GET /libpod/events?stream=true` filtered to
   container events; the consumer maps `died` → crash recovery and
   `remove` → terminal re-election, keyed off the Falak container name
   in `Actor.Attributes.name`.
3. The per-container 2s poll is gone. One long-lived event consumer
   (jittered reconnect on stream drop, with a full reconcile on
   reconnect) plus one periodic reconcile sweep (default 30s) now drive
   crash/removal/unreachable detection. `errors.Is(ErrContainerNotFound)`
   in reconcile → terminal; consecutive transient inspect errors
   escalate at `max_inspect_errors` (default 5).
4. A self-removal ignore set, populated before every handler-initiated
   Stop/Remove (rolling-update swap, user stop, snapshot checkpoint),
   prevents Falak's own teardowns from self-triggering re-election.

New tunables (functional options): `WithReconcileInterval` (30s),
`WithMaxInspectErrors` (5), `WithEventReconnectBackoff` (1s). See
`docs/configuration.md` → Runtime.

**Verification:** Unit/integration tests in `runtime/handler_test.go`
cover removed-event, died-event (restart_limit 0 and 2),
intentional-remove-ignored, reconcile-catches-missed-removal, and
transient-escalation/single-blip — all deterministic via the mock's
`EmitContainerEvent` and a 10ms reconcile interval, no real-clock
waits. `runtime/mock/runtime_test.go` asserts
`errors.Is(ErrContainerNotFound)`. `runtime/podman/runtime_test.go`
adds a live-socket `Events` smoke test (run→remove yields died+remove,
verified against Podman 4.9.3) and strengthens the post-Remove Inspect
assertion to the sentinel. `go test -race ./runtime/... ./node/...`
green.

**Status: Fixed (F28).**

### F27. Gap — CLI couldn't target a specific daemon when several run on one host

**Observed:** Running 3 daemons on one machine, every CLI command
(`capsule create`, `node list`, …) hit whichever daemon owned `:9090` —
i.e. always node1. The API server auto-port-shifts on collision
(`:9090 → :9091 → :9092`), but the CLI only knew the default `:9090`.
The pre-existing `--endpoint host:port` flag worked but forced the
operator to hand-track which daemon landed on which shifted port.

**Fix:** Name-based targeting via a local daemon registry.
1. Each daemon, once its API server binds, writes
   `~/.local/share/falak/daemons/<name>.json` `{name, endpoint, pid,
   data_dir}` with the resolved, dialable endpoint (wildcard/empty host
   rewritten to `127.0.0.1`). Removed on graceful shutdown. Best-effort:
   a write failure is logged, the daemon keeps serving `--endpoint`.
2. New global `--node <name>` flag. Resolution precedence:
   `--endpoint` (wins) → `--node` (registry lookup) → `--context` /
   current-context. A `--node` lookup builds an ad-hoc insecure context
   (local daemons speak plain h2c on an auto-shifted port — no TLS).
3. New `falak daemon list` (alias `ls`) enumerates registered daemons
   with a stale-PID marker, so operators see the name→endpoint→pid map.

The registry is central (not per-data-dir) so `--node` works even when a
daemon used a custom `--data-dir`. macOS/Windows operators (different
data-home layout) keep using `--endpoint`.

**Verified end-to-end:** Two daemons started with the same
`--api-listen=:9090`; `daemon list` showed `alpha→127.0.0.1:9091`,
`beta→127.0.0.1:9090`; `--node alpha`/`--node beta` routed to the right
daemon (distinct peer IDs); `--endpoint` overrode `--node`; unknown name
gave an actionable error; shutdown emptied the registry.

**Files:** `cmd/falak/internal/registry.go` (new),
`cmd/falak/internal/connect.go`, `cmd/falak/main.go`,
`cmd/falak/daemon_cmds.go`.

---

## Fixed (post-Session-17 polish pass)

### F26. Bug A — Notifiee reactivated Departed peers ahead of re-auth

**Observed:** When a `Departed` peer reconnected (same identity, e.g. operator
restart of the daemon), `node.go`'s libp2p `Notifiee.ConnectedF` flipped
the phonebook entry from `Departed` straight to `Active` at TCP-connect
time — ~10ms before the auth handshake even started. SWIM's protocol-period
tick (every 2s) was therefore eligible to probe a peer whose re-auth was
still in flight. On a fast 2-node bootstrap this didn't bite (auth finishes
in ~16ms), but on a slow gossipsub-mesh-formation window (5–15s) a healthy
peer could be falsely suspected. The asymmetry was: fresh joiners entered
`PendingAuth` (F23 / Bug #13) but reconnecting peers skipped that gate.

**Fix:** Notifiee now flips Departed → **PendingAuth** instead of Active.
The auth handler's explicit `SetStatus(joiner, Active)` after AuthComplete
(voucher side, F23) and the authenticator's `SetStatus(voucher, Active)`
after receiving AuthComplete (joiner side, F23) take over — the same gate
the fresh-join path uses. SWIM's `pendingAuthGrace` window covers the rare
case where re-auth gets stuck.

**Files:** `node/node.go` (`ConnectedF`).

---

### F25. Bug #19 — Optional `--health-heartbeat` flag

**Observed:** Operators had no built-in periodic summary of cluster health.
Inferring it required `falak node list` invocations or scraping individual
SWIM probe logs.

**Fix:** New `--health-heartbeat <duration>` flag on `falak daemon start`.
When >0 (disabled by default), a goroutine emits one INFO log per interval
per joined cluster summarising `active/pending/suspected/quarantined/failed/departed`
counts. Implementation in `cmd/falak/daemon_health_heartbeat.go`; wired into
both `runDaemon` and `runDaemonFromConfig`.

**Files:** `cmd/falak/daemon_cmds.go` (flag plumbing), `cmd/falak/daemon_health_heartbeat.go` (new).

---

### F24. Bug #21 — Per-probe Debug log

**Observed:** Successful SWIM probes were silent at every log level. The
`event published type=health.probe_result` Debug existed but didn't say
*which* peer was probed — operators had to grep ping handler logs to
answer "is SWIM probing this specific peer?".

**Fix:** Added explicit `probe ok target=X cluster=Y score=Z` and
`probe failed target=X cluster=Y score=Z error=...` Debug logs in
`monitor.runProtocolPeriod` immediately around the ping call.

**Files:** `node/health/monitor.go`.

---

### F23. Bug #13 — `pending-auth` phonebook status

**Observed:** SWIM picked freshly-announced peers as probe targets before
their libp2p mesh / ping handler had stabilised, briefly suspecting them
mid-handshake and adding noise to the health PubSub stream.

**Fix:**
1. Added `PendingAuth` status to the phonebook enum.
2. `phonebook.Subscriber.addOrUpdateEntry` now takes an `initialStatus`
   parameter; freshly-inserted entries from `NewMemberAnnounced` and
   `NewMemberReceived` enter `PendingAuth`, while `ClusterMembersReceived`
   inserts as `Active` (these are voucher-verified at AuthComplete time
   and the syncer's `GetBestPeers` needs an Active peer to bootstrap).
3. `Subscriber` now takes a `WithSubscriberSelfID` option so the self
   re-announce path (first-node bootstrap) skips PendingAuth.
4. On update, the subscriber preserves the existing row's `Status`,
   `FirstSeen`, `ReliabilityScore`, `LastProbeTime`, etc. — re-announcements
   never demote a promoted peer back to PendingAuth or zero out telemetry.
5. SWIM's `selectRandomActivePeer` skips peers in `PendingAuth` for the
   `WithPendingAuthGrace` window (default 10s, configurable). Once the
   grace elapses, the monitor auto-promotes the entry to Active so a
   stuck marker can't leave a peer permanently un-probed.
6. `auth/handler.go` flips the joiner to Active immediately after sending
   AuthComplete on the voucher side; `auth/authenticator.go` flips the
   voucher to Active immediately after receiving AuthComplete on the
   joiner side — both make the transition snappier than the grace timeout.

**Files:** `node/phonebook/phonebook.go`, `node/phonebook/subscriber.go`,
`node/health/monitor.go`, `node/auth/handler.go`, `node/auth/authenticator.go`,
`node/node.go`.

---

### F22. Bug #11 — Demote transient stream errors to Debug

**Observed:** `auth/handler.go` logged at ERROR when a peer reset a stream
mid-handshake (gossipsub mesh churn, operator-driven restart) — five
distinct sites in the voucher-side flow. F7 + F16 had already masked
most of the cosmetic damage, but the ERROR level still misled operators
into thinking the auth path was broken.

**Fix:** Added `isTransientStreamErr` helper that recognises `io.EOF`,
`context.Canceled`, `context.DeadlineExceeded`, and the standard libp2p /
TCP "stream reset" / "connection reset by peer" / "use of closed network
connection" / "broken pipe" messages. New `logStreamError` method routes
transient errors to Debug and genuine errors to Error. All five voucher-side
read/write log sites switched over.

**Files:** `node/auth/handler.go`.

---

## Fixed (Session 17)

### F21. Bug #24 — Zombie node after graceful-restart cycle

**Observed:** Stop node1 with `daemon stop`, then restart node1 with the same identity. node2 silently rejects every signed message from node1 — health pubsub: `rejecting health message with invalid signature`, sync: `rejected sync request from unauthenticated peer`. node1 is libp2p-connected but cluster-invisible.

**Root cause:** F8 (graceful drain) called `phonebook.Remove(node1)` on receivers, deleting node1's public key. When node1 restarted with the same identity, node2 had nothing to verify signatures against.

**Fix:**
1. Added `phonebook.NodeStatusEnum.Departed()` status.
2. `Monitor.onScoreUpdate` for `Reason==node_departing` now calls `SetStatus(Departed)` instead of `Remove`. Entry (cert + public key) survives.
3. SWIM's `selectRandomActivePeer` skips Departed peers (they said goodbye — no point probing).
4. libp2p `Notifiee.ConnectedF` detects a Departed entry for the connecting peer and flips it back to Active (`departed peer reconnected, reactivated` log).
5. `ScoreTracker.RecordSuccess` now treats Departed like Suspected/Quarantined — emits NodeRecovered on the first successful probe.
6. Sync's `phonebook.Exists` check passes for Departed entries (row still exists).
7. Pubsub `verifySignature` looks up entry regardless of status — Departed entries verify normally.

**Verified end-to-end:**
```
T+0: status=active
[node1 daemon stop]
T+3: status=departed (entry preserved)
[node1 daemon start]
T+10: status=active, last probe=1s ago (ok)
auth errors: 0
log: "departed peer reconnected, reactivated"
```

**Files:** `node/phonebook/phonebook.go`, `node/health/monitor.go`, `node/health/score.go`, `node/node.go`.

---

### F20. Bug #20 — Event bus had no "delivered" log to mirror "published"

**Observed:** Bus logged `event published type=X` when an event was queued for fanout, but the actual delivery to subscriber channels was silent. Operators could see something was published but had no way to tell whether anyone consumed it — the exact gap that hid Bug #22 for a long time.

**Fix:** Added a symmetric `event delivered type=X subscribers=N` Debug log in `bus.fanout` after the per-subscriber channel sends complete. `subscribers=0` is now the immediate signal of a published-into-the-void event.

**Verified end-to-end:**
```
DEBUG  node.eventbus  event published  type=health.probe_result
DEBUG  node.eventbus  event delivered  type=health.probe_result  subscribers=1
```
The `subscribers=1` proves F15 (Bug #22) is genuinely consuming the event — if F15 ever regresses, the log would immediately show `subscribers=0` for the probe_result event.

**Files:** `node/internal/events/bus.go`.

---

### F19. Bug #6 — Dedicated NodeService gRPC API

**Observed:** Node-listing went through `ClusterService.Members(cluster)` — a workaround because no `NodeService` proto existed. The CLI had to fan out manually across joined clusters for `falak node get` / `node health`.

**Fix:** Added `api/proto/v1alpha1/node_service.proto` with `List`, `Get`, `Health` RPCs. Generated stubs + grpc-gateway routes. New `api/grpc/node_service.go` implements the three handlers, fanning out across clusters server-side when the request `cluster` is empty. Registered in `server.go`. Added `Nodes` typed stub to `internal/client.go`. CLI commands now use `client.Nodes.List/Get/Health` directly. The fan-out loop in `resolveClusters` is no longer needed for Get/Health.

**Verified:** `falak node list`, `falak node get <peer>`, `falak node health <peer>` all return the same data via the new RPCs.

**Files:** `api/proto/v1alpha1/node_service.proto` (new), `api/proto/v1alpha1pb/node_service.pb*.go` (generated), `api/grpc/node_service.go` (new), `api/grpc/server.go`, `cmd/falak/internal/client.go`, `cmd/falak/node_cmds.go`.

---

### F18. Bug #1 — `falak capsule update` CLI was a stub

**Observed:** `falak capsule update <id>` returned `Error: not yet implemented` even though the gRPC backend (`api/grpc/capsule_service.go:90`) was fully implemented.

**Fix:** Added `capsuleUpdateCmd` to `cmd/falak/capsule_cmds.go` with flags `--image`, `--cluster`, `--env KEY=VALUE` (repeatable), `--command` (repeatable). Replaced the stub `RunE` in `cmd/falak/main.go` with the real constructor.

**Verified:** `falak capsule update fakeid --image=nginx:latest` returns `NotFound` (real gRPC error, not the placeholder).

**Files:** `cmd/falak/capsule_cmds.go`, `cmd/falak/main.go`.

---

### F17. Bug #7 — Uptime measured from API-start, not node-start

**Observed:** `falak system info` reported `uptime: 0s` immediately after daemon start because `core.Core.startedAt` was set when the Core struct was instantiated — which on the SIGTERM-restart path could happen seconds after the node itself started.

**Fix:** Added `Node.StartedAt() time.Time` (populated when state transitions to Running). Added `StartedAt()` to `core.NodeFacade` interface. `core.GetInfo` prefers the facade's value over its own startedAt, falling back to the latter only when the facade returns zero (test stubs).

**Verified:** `system info` shows `uptime: 23s` after running ~23s.

**Files:** `node/node.go`, `api/core/api.go`, `api/core/api_test.go` (stub method), `node/api_facade.go`.

---

### F16. Bug #9 — 15-second auth confirmation wait on 2-node bootstrap

**Observed:** Every cluster join took ~15 seconds even with only 2 nodes. The voucher waited for *other* peers to confirm the new member's PKI cert; on a 2-node cluster there are no others, so the wait was pure latency.

**Root cause:** `node/auth/handler.go` checked `clusterMemberCount > 0` which counted the voucher (self) + the just-added joiner — so even on a fresh 2-node bootstrap the count was 1 or 2 and the wait branch fired.

**Fix:** Count only third-party confirmers: iterate the phonebook entries, skip the entry whose NodeID matches `host.ID()` (self) or `joinReq.NodeId` (joiner). The wait now only fires when at least one other peer exists.

**Verified:** Auth handshake elapsed time dropped from `15.012s` to `2.471777ms` on a 2-node bootstrap. Total `falak daemon start` time on the joiner went from 15s+ to ~120ms.

**Files:** `node/auth/handler.go`.

---

### F15. Bugs #22 + #18 — Live SWIM data persisted to phonebook + exposed via CLI

**Observed:** Per-probe `NodeProbeResult` events were published every 2s but had zero production subscribers (only a unit test consumed them). The phonebook's `LastProbeTime` / `LastProbeSuccess` / `ReliabilityScore` columns existed but stayed empty. `falak node health` only showed the static phonebook entry — no way to know if SWIM was actually probing without restarting at `--log-level=debug` and grepping `event published`.

**Fix:**
1. Added `Monitor.probeResultPersistLoop` subscribing to `events.TypeNodeProbeResult`. Each event calls `phonebook.RecordProbe(NodeID, ClusterPath, Success)` which updates `LastProbeTime` + `LastProbeSuccess` on the row. `ReliabilityScore` was already updated by the score tracker.
2. Added `last_probe_time`, `last_probe_success`, `reliability_score` fields to the `NodeInfo` proto (3rd proto regen this session).
3. `core.NodeStatusView` + `APIFacade` + gRPC handler all carry the three fields end-to-end.
4. `falak node health` rewrite shows the live state:
   ```
   node:        12D3KooW…HgWV8
   name:        node2
   cluster:     test/dc1/prod
   status:      active
   cpu cores:   12
   memory:      40028 MB
   last probe:  2s ago (ok)
   score:       0.00
   ```

**Verified end-to-end** by killing node2 with `kill -9` and polling node1:
- T+0: `status=active  last probe=2s ago (ok)  score=0.00`
- T+7s: `status=quarantined  last probe=12s ago (FAIL)  score=3.00`
- T+17s: `Error: node "node2" not found` (phonebook reaper)

**Files:** `node/health/monitor.go` (`probeResultPersistLoop`), `api/proto/v1alpha1/cluster_service.proto` (new fields + regen), `api/core/types.go`, `node/api_facade.go`, `api/grpc/cluster_service.go`, `cmd/falak/node_cmds.go` (`printNodeHealth`).

---

### F14. Bugs #2 + #3 — NAME and CPU/MEM columns now populated end-to-end

**Observed:** `falak node list` showed peer IDs in the NAME column and `0` for CPU/MEM.

**Fix:**
1. Added `Name` field to `phonebook.Entry` + idempotent SQLite `ALTER TABLE` migration for upgrade-in-place. Update SQL uses `COALESCE` so a name-less delta sync doesn't clobber an existing name.
2. New `node/host_capabilities.go` samples CPU cores + memory MB + disk GB via gopsutil at startup and returns `*authpb.Capabilities`.
3. New `auth.WithNodeName` and `auth.WithCapabilities` options on the Authenticator. Joiner stamps name into `Capabilities.Metadata["node_name"]` and CPU/MEM into the typed fields.
4. Phonebook subscriber extracts the name from metadata (constant `MetadataKeyNodeName`).
5. Syncer's `processMember` was overwriting Region/Datacenter (and would have blanked Name) on every delta-sync round — now parses cluster path + reads name from capabilities metadata so sync preserves both.
6. API facade falls back to peer ID when Name is empty (older agents).

**Verified end-to-end (2 nodes):**
```
ID              NAME   STATUS  DC   REGION  CPU  MEM(MB)
12D3KooW…HgWV8  node2  active  dc1  test    12   40028
12D3KooW…ixrS7  node1  active  dc1  test    12   40028
```
Both nodes' views agree.

**Files:** `node/phonebook/phonebook.go`, `node/phonebook/sqlite.go`, `node/phonebook/subscriber.go`, `node/auth/authenticator.go`, `node/host_capabilities.go` (new), `node/node.go`, `node/sync/syncer.go`, `node/api_facade.go`.

---

### F13. Bug #14 — Session re-auth fired every 5 min on idle 1-node clusters

**Observed:** node1 alone logged `session stale, triggering re-auth → first node session refresh (no voucher)` every ~5 minutes forever. No peers existed so the re-auth was a pure no-op.

**Fix:** In `checkStaleSessions`, when the per-cluster phonebook count is ≤ 1 (just self), demote the event to Debug and skip the bus publish entirely.

**Files:** `node/auth/authenticator.go`.

---

### F12. Bug #5 — Sync "no peers available" was WARN on single-node bootstrap

**Observed:** First-time startup ended with `WARN no peers available for sync request`. Benign — a single-node cluster has no one to sync from.

**Fix:** Demote to Debug when `GetBestPeers` returns `(nil, nil)` (empty cluster, not an error). Keep Warn when `GetBestPeers` actually errors.

**Files:** `node/sync/syncer.go`.

---

### F11. Bug #4 — `cluster list` showed `JoinedAt: 0001-01-01T00:00:00Z`

**Observed:** `falak cluster list` always showed the zero time in the JOINED column.

**Fix:** Added `Node.JoinedClustersWithTime()` returning `map[string]time.Time`. APIFacade.ClusterList uses it to populate `core.ClusterResource.JoinedAt`. CLI now shows the real local-clock join time (rendered as UTC).

**Files:** `node/node.go`, `node/api_facade.go`.

---

### F10. Bug #0 — Daemon leaked node on early-return errors

**Observed:** Any error path between `n.Start()` and the SIGTERM-wait (API bind failure on a malformed addr, pidfile write race, etc.) returned without calling `n.Stop()`, leaking the libp2p host + SQLite handles + goroutines.

**Fix:** Registered `defer func() { if n.State() != Stopped { _ = n.Stop() } }()` immediately after a successful `n.Start()` in both `runDaemon` and `runDaemonFromConfig`. Stop is idempotent so the normal SIGTERM `shutdownNode` path still works.

**Files:** `cmd/falak/daemon_cmds.go`.

---

### F9. Bug #15 — Auth handshake had zero progress logging on both sides

**Observed:** node2 starts, logs `auth message loop started`, then sits silent for 5-15 seconds while gossipsub meshes with node1. node1 stays equally silent until the JoinClusterRequest arrives. Operators routinely killed node2 mid-handshake assuming it had hung; the resulting stream resets surfaced as ERROR logs on node1, making the situation look even worse.

**Fix:** Added progress logs on both sides + a libp2p `network.Notifiee` so connect/disconnect events surface immediately:
- **Joiner (node2):** `dialing bootstrap peer X` → `bootstrap peer connected, requesting auth (gossipsub mesh forming, may take 5-15s)` → `still waiting for voucher response (5s|10s|15s elapsed)` every 5s → existing `authenticated with bootstrap peer` (now includes elapsed)
- **Voucher (node1):** `peer connected peer=X direction=inbound addr=...` (via Notifiee) → `received join request, validating PSK` → `PSK validated, signing certificate` → existing `authenticated new member`
- `peer disconnected` on the inverse event

**Verified end-to-end:** A 2-node bootstrap now produces a complete narrative on both sides; 15-second silent window replaced with three INFO heartbeats.

**Files:** `node/node.go` (Notifiee in `initializeHost`), `node/auth/authenticator.go` (`bootstrapAndAuthenticate` + `authProgressHeartbeat`), `node/auth/handler.go` (voucher-side milestones).

---

### F8. Bug #16 — Graceful shutdown took 16-30s for peers to notice

**Observed:** `falak daemon stop` on node1 caused node2 to spend ~16-30s walking through SWIM's `suspected → quarantined → failed` state machine before removing node1 from the phonebook. The `NodeDeparting` event was published locally but never broadcast to peers — the comment in `node.Drain()` literally said *"today this only reaches local subscribers"*.

**Fix:**
1. Refactored `node.Drain()` to broadcast the event and return immediately (no more in-function `Stop()` + arbitrary `time.Sleep`).
2. Added a `departureBroadcastLoop` in the health monitor that subscribes to `events.TypeNodeDeparting` and forwards it as a `ScoreUpdate{Alive:false, Reason:"node_departing"}` on the health PubSub topic.
3. In the receiver's `onScoreUpdate`, a special-case for `Reason=="node_departing"` calls `phonebook.Remove` directly, bypassing the score machinery.
4. Daemon SIGTERM/SIGINT handler now calls `Drain()` → `sleep(--drain-timeout)` → `Stop()` via the new `shutdownNode` helper. Default grace is `3s`; the broadcast actually completes in ~50ms but the timeout gives slow networks margin.

**Verified end-to-end:** node2 detected node1's departure in **sub-second** wall time (was 16-30s). The departure log line on node2 is `peer announced graceful departure`, not the SWIM suspect/quarantine path.

**Files:** `node/node.go` (`Drain`), `node/health/monitor.go` (`departureBroadcastLoop`, `ReasonNodeDeparting`, `onScoreUpdate` special-case), `cmd/falak/daemon_cmds.go` (`shutdownNode` helper, `--drain-timeout` flag).

---

### F7. Bug #10 — Zap dev-mode emitted stack traces at every WARN/ERROR

**Observed:** Half of every log dump was 10-line stack traces under benign WARN lines like `api port shifted`, `no peers available for sync request`, `node quarantined`. Operators had to mentally filter them as noise; new ones couldn't tell signal from noise at all.

**Fix:** In `setupLogger`, set `Development: false` + `DisableStacktrace: true` on the zap config. Panics and fatals still get stacks (those bypass config); everything else is one clean line. If a specific call site genuinely needs a stack, use `zap.AddStacktrace(zap.ErrorLevel)` at that site only.

**Verified end-to-end:** A full 2-node bootstrap + graceful shutdown cycle produced **zero** stack-trace lines in either log (was 5-8 per cycle before).

**Files:** `cmd/falak/daemon_cmds.go` `setupLogger`.

---

### F1. Phonebook stale-self ghost causes SWIM false-positive failures

**Observed:** Single-node `daemon start` logged `dial to self attempted`
every 2s; SWIM walked the ghost through suspect → quarantined → failed →
removed, also reported `clusterSize=2` while only one node was running.

**Root cause:** `node/auth/authenticator.go:1098` publishes
`NewMemberAnnounced` for self so peers can verify during sync. The
phonebook subscriber adds it. If the data dir is reused but `node.key`
rotates (new peer ID at same multiaddrs), the old self entry survives.
SWIM picks it, libp2p refuses with "dial to self".

**Fix:** Added `health.PingWithError` + `health.IsDialToSelfError`. The
SWIM monitor (`node/health/monitor.go runProtocolPeriod`) now evicts the
phonebook entry immediately on dial-to-self instead of going through the
score machinery.

**Files:** `node/health/handler.go`, `node/health/monitor.go`.

---

### F2. API server (gRPC + HTTP) never started by the daemon

**Observed:** `curl http://localhost:9090/healthz` → connection refused
even though README and PROGRESS.md said port 9090 was the API surface.
Every CLI command except `daemon` failed to dial.

**Root cause:** Nothing in `cmd/falak/` imported `api/grpc`, `api/core`, or
`api/http`. The entire API layer was dead code from the running binary's
perspective.

**Fix:** Added `--api-listen` (default `:9090`) and `--api-disable` flags
to `falak daemon start`. New `cmd/falak/daemon_api.go startAPIServer`
builds `core.New(facade)` + `apigrpc.NewServer`, started after `n.Start()`
and stopped before `n.Stop()` in both `runDaemon` and `runDaemonFromConfig`.

**Files:** `cmd/falak/daemon_cmds.go`, `cmd/falak/daemon_api.go`,
`node/api_facade.go` (new — implements `core.NodeFacade`),
`node/api_service_facade.go` (new — implements `core.ServiceFacade`).

---

### F3. gRPC requests fail with "frame too large" on plain TCP

**Observed:** After F2, `curl /healthz` worked but every gRPC call
returned `"error reading server preface: http2: failed reading the frame
payload: http2: frame too large"`.

**Root cause:** `api/grpc/server.go` ran the multiplexed handler under
`http.Server` defaults, which speak HTTP/1.1 over plain TCP. gRPC needs
HTTP/2; without TLS it can only be negotiated via H2C (prior knowledge).

**Fix:** Wrapped the mux handler in `h2c.NewHandler(handler, &http2.Server{})`
from `golang.org/x/net/http2/h2c`.

**Files:** `api/grpc/server.go`.

---

### F4. 11 CLI subcommands returned "not yet implemented"

**Observed:** `falak node list|get|health`, `falak cluster join|leave|list|members`,
`falak system info`, `falak config use-context|list-contexts|set-context`,
`falak capsule update` all returned the placeholder error.

**Root cause:** `cmd/falak/main.go` registered `RunE` stubs for each;
`cmd/falak/{cluster,node,system,config}/` directories existed but were
empty. CLI client factory (`internal/client.go`) only exposed `Capsules`
and `Services` typed stubs.

**Fix:** Added `cluster_cmds.go`, `node_cmds.go`, `system_cmds.go`,
`config_cmds.go`. Replaced the stubs in `main.go` with real constructors.
Added `Clusters` and `System` typed stubs + `Conn()` accessor to
`internal/client.go`. `capsule update` is still a stub — see Open #1.

**Files:** `cmd/falak/main.go`, `cmd/falak/cluster_cmds.go`,
`cmd/falak/node_cmds.go`, `cmd/falak/system_cmds.go`,
`cmd/falak/config_cmds.go`, `cmd/falak/internal/client.go`.

---

### F6. API port collision required manual `--api-listen` per daemon

**Observed:** Running multiple daemons on one host failed the second one
with `bind: address already in use` on `:9090`. The operator had to pass
`--api-listen=:9091` for each extra daemon, and bug #0 (the leak path)
fired every time it was forgotten.

**Root cause:** `api/grpc/server.go Start` called `net.Listen("tcp", addr)`
once and returned the error on failure. No retry.

**Fix:** Added `listenWithRetry` (Vite/webpack-dev-server style). On
EADDRINUSE the server walks the port forward up to `DefaultMaxPortRetries`
(20) attempts, binds the first free one, logs WARN with both requested
and bound addresses. `Server.Addr()` returns the actually-bound address
so the daemon log + CLI introspection see the resolved port. Strict mode
available via `WithMaxPortRetries(0)`.

**Verified:** Three daemons started back-to-back with default
`--api-listen=:9090` ended up on `:9090`, `:9091`, `:9092`. CLI hits all
three correctly.

**Files:** `api/grpc/server.go`, `cmd/falak/daemon_api.go`.

---

### F5. `system info` reported empty `Clusters` list

**Observed:** `falak system info` showed `clusters: ` (empty) even when
the node had joined a cluster.

**Root cause:** `core.GetInfo` never populated `SystemInfo.Clusters` —
the field existed but nothing wrote to it.

**Fix:** `core.GetInfo` now calls `c.node.ClusterList(ctx)` and copies the
paths into `info.Clusters`. Best-effort: a facade error doesn't fail the
info call.

**Files:** `api/core/api.go`.

---

## Notes for fixers

- The Go workspace requires **go 1.25.7**; the system `go` (often 1.24)
  produces a misleading `go: go.work requires go >= 1.25.7` error. Always
  `source ~/.gvm/scripts/gvm && gvm use go1.25` first.
- LSP diagnostics in this repo are noisy because the editor often runs
  with system Go. Ignore those `go.work requires go >= 1.25.7` messages
  in the diagnostic stream as long as `go build ./...` is clean under gvm.
- Manual verification recipe lives in `docs/SELF_TESTING.md` (layered).
  After each fix, re-run the layer that exercises the fixed subsystem.
