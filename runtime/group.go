package runtime

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// MemberInfo describes a group member's dependency context as seen by
// the runtime handler. Returned by GroupView.MemberInfo for capsules that
// belong to a CapsuleGroup; non-members return (zero, false).
type MemberInfo struct {
	// GroupID is the parent group capsule's ID.
	GroupID string

	// Name is this member's name within the group.
	Name string

	// DependsOn lists the names of other members in the same group whose
	// Running state must be reached before this member starts. May be empty.
	DependsOn []string

	// Siblings maps every member name in the group (including this one)
	// to that member's current state from the local view. The runtime
	// handler uses this to decide whether all DependsOn entries are
	// already Running. The map is a snapshot — subsequent state changes
	// arrive via Handler.OnDependencyRunning.
	Siblings map[string]SiblingState
}

// SiblingState is the per-member view used for dependency gating.
type SiblingState struct {
	// CapsuleID is the sibling's capsule ID.
	CapsuleID string

	// Running is true when the sibling's lifecycle has reached Running
	// on at least one node visible to the local view.
	Running bool
}

// GroupView is the narrow interface the runtime handler uses to look up
// group membership and dependency state. Satisfied by capsule.Manager
// behind an adapter in the node wiring layer; keeps the runtime package
// free of capsule module imports.
type GroupView interface {
	// MemberInfo returns the dependency context for the given capsule.
	// ok is false when the capsule is not a group member or the group is
	// not yet known to the local view.
	MemberInfo(capsuleID string) (info MemberInfo, ok bool)

	// CapsuleIDByMemberName resolves a sibling member's name to its
	// capsule ID within the same group. Returns ("", false) if the name
	// is unknown. Called by the handler when EventCapsuleRunning fires
	// for a capsule that may be releasing parked siblings.
	CapsuleIDByMemberName(groupID, memberName string) (capsuleID string, ok bool)

	// Colocation returns the colocation mode string ("same-node",
	// "same-orbit") for the given group. ok is false when the group is
	// not visible locally. Used by the runtime handler to decide
	// whether a member start failure should be rolled back through the
	// MemberPlacementFailed group-rollback path or through the
	// per-replica MarkFailed path.
	Colocation(groupID string) (mode string, ok bool)
}

// ColocationSameNode is the canonical string value the GroupView returns
// from Colocation for groups that place every member on the same node.
// The runtime package keeps the string here rather than importing the
// capsule module to preserve the package's independence.
const ColocationSameNode = "same-node"

// defaultDependencyTimeout bounds how long a parked start waits for its
// dependencies before giving up and reporting placement failure. Five
// minutes covers typical first-boot scenarios (image pull + container
// start + healthcheck) on a multi-member group.
const defaultDependencyTimeout = 5 * time.Minute

// parkedStart holds a deferred ElectionWon event waiting for one or more
// group dependencies to reach Running. Each parked entry has its own
// cancellation context so the handler can release individual entries
// without disturbing siblings.
type parkedStart struct {
	event    ElectionWon
	waiting  map[string]struct{} // dep names not yet running
	deadline time.Time
	cancel   context.CancelFunc
}

// groupCoord is the parked-start coordinator embedded in Handler. It
// owns the parked map, the per-capsule "released past first boot" set,
// and the per-park goroutines that wait on the deadline timer.
type groupCoord struct {
	mu         sync.Mutex
	view       GroupView
	timeout    time.Duration
	parked     map[string]*parkedStart // capsuleID -> parked entry
	released   map[string]bool         // capsuleID -> already released past first boot
	logger     *zap.Logger
	startFn    func(ElectionWon)       // invoked when a parked start is released
	failFn     func(capsuleID, reason string)
	wg         *sync.WaitGroup         // shared with Handler so Stop waits for park goroutines
}

// newGroupCoord constructs a coordinator. wg is the Handler's
// sync.WaitGroup so parked-start goroutines block Stop() until they exit.
// startFn is called when a parked entry is released (deps satisfied);
// failFn is called when a parked entry's deadline fires.
func newGroupCoord(
	view GroupView,
	timeout time.Duration,
	logger *zap.Logger,
	startFn func(ElectionWon),
	failFn func(capsuleID, reason string),
	wg *sync.WaitGroup,
) *groupCoord {
	if timeout <= 0 {
		timeout = defaultDependencyTimeout
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &groupCoord{
		view:     view,
		timeout:  timeout,
		parked:   make(map[string]*parkedStart),
		released: make(map[string]bool),
		logger:   logger,
		startFn:  startFn,
		failFn:   failFn,
		wg:       wg,
	}
}

// shouldPark inspects an ElectionWon event and returns the set of
// dependency names that are NOT yet Running. If the capsule is not a
// group member, has no dependencies, or has already been released past
// first boot, the returned slice is empty and the start should proceed.
//
// The "released past first boot" check enforces the first-boot-only
// dependency semantics: once a member has reached Running on some node
// at least once, dependents are released permanently. A subsequent
// crash + re-election does NOT re-park; the application is responsible
// for reconnecting on its own.
func (g *groupCoord) shouldPark(event ElectionWon) []string {
	if g == nil || g.view == nil {
		return nil
	}

	g.mu.Lock()
	if g.released[event.CapsuleID] {
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()

	info, ok := g.view.MemberInfo(event.CapsuleID)
	if !ok {
		return nil
	}
	if len(info.DependsOn) == 0 {
		// No deps; mark released so future re-elections skip the lookup.
		g.markReleased(event.CapsuleID)
		return nil
	}

	var unmet []string
	for _, dep := range info.DependsOn {
		state, present := info.Siblings[dep]
		if !present {
			// Dep is in the spec but not visible locally yet — treat as
			// unmet. The dep-running watcher will release this park when
			// the dep eventually arrives + reaches Running.
			unmet = append(unmet, dep)
			continue
		}
		if !state.Running {
			unmet = append(unmet, dep)
		}
	}
	if len(unmet) == 0 {
		g.markReleased(event.CapsuleID)
		return nil
	}
	return unmet
}

// park records a deferred start for capsule waiting on the given deps
// and arms the per-park deadline timer. Re-entry is idempotent: a
// second park request for the same capsule is dropped.
//
// ctx is the Handler's long-lived context — when it cancels (Stop), the
// per-park goroutine exits cleanly without firing the deadline.
func (g *groupCoord) park(ctx context.Context, event ElectionWon, deps []string) {
	g.mu.Lock()
	if _, exists := g.parked[event.CapsuleID]; exists {
		g.mu.Unlock()
		return
	}

	parkCtx, cancel := context.WithCancel(ctx)
	waiting := make(map[string]struct{}, len(deps))
	for _, d := range deps {
		waiting[d] = struct{}{}
	}
	entry := &parkedStart{
		event:    event,
		waiting:  waiting,
		deadline: time.Now().Add(g.timeout),
		cancel:   cancel,
	}
	g.parked[event.CapsuleID] = entry
	timeout := g.timeout
	g.mu.Unlock()

	g.logger.Info("group dependency wait: start parked",
		zap.String("capsule_id", event.CapsuleID),
		zap.String("replica_id", event.ReplicaID),
		zap.Strings("waiting_on", deps),
		zap.Duration("deadline", timeout))

	if g.wg != nil {
		g.wg.Add(1)
	}
	go func() {
		if g.wg != nil {
			defer g.wg.Done()
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()

		select {
		case <-parkCtx.Done():
			// Cancelled — released by deps satisfied OR Stop().
			return
		case <-timer.C:
			g.fireDeadline(event.CapsuleID, timeout)
		}
	}()
}

// onDependencyRunning is called when a capsule reaches Running. It
// removes the released capsule from any parked siblings' waiting set
// and triggers start on entries whose waiting set has become empty.
//
// When the released capsule is itself a group member, the watcher
// resolves its name within the group and clears that name from
// dependents' waiting sets. Non-group capsules are ignored.
func (g *groupCoord) onDependencyRunning(capsuleID string) {
	if g == nil || g.view == nil {
		return
	}

	// Mark released so future ElectionWon events for this capsule skip
	// dep gating entirely (first-boot-only semantics).
	g.markReleased(capsuleID)

	info, ok := g.view.MemberInfo(capsuleID)
	if !ok {
		// Not a group member — nothing parked depends on it.
		return
	}

	// Iterate parked entries and release any whose waiting set becomes
	// empty after removing this dep name.
	var ready []*parkedStart

	g.mu.Lock()
	for parkedID, entry := range g.parked {
		// Only entries belonging to the SAME group can be waiting on this
		// capsule. Look up the parked entry's group from the view.
		parkedInfo, ok := g.view.MemberInfo(parkedID)
		if !ok || parkedInfo.GroupID != info.GroupID {
			continue
		}
		if _, waiting := entry.waiting[info.Name]; !waiting {
			continue
		}
		delete(entry.waiting, info.Name)
		if len(entry.waiting) == 0 {
			delete(g.parked, parkedID)
			entry.cancel() // stop the deadline goroutine
			g.released[parkedID] = true
			ready = append(ready, entry)
		}
	}
	g.mu.Unlock()

	for _, entry := range ready {
		g.logger.Info("group dependency wait: released",
			zap.String("capsule_id", entry.event.CapsuleID),
			zap.String("replica_id", entry.event.ReplicaID),
			zap.String("released_by", info.Name))
		if g.startFn != nil {
			g.startFn(entry.event)
		}
	}
}

// fireDeadline runs when a parked entry's deadline expires before its
// dependencies are satisfied. Marks the capsule failed (which will
// trigger re-election by the upstream lifecycle handler).
func (g *groupCoord) fireDeadline(capsuleID string, waited time.Duration) {
	g.mu.Lock()
	entry, ok := g.parked[capsuleID]
	if !ok {
		g.mu.Unlock()
		return
	}
	delete(g.parked, capsuleID)
	waitingNow := make([]string, 0, len(entry.waiting))
	for name := range entry.waiting {
		waitingNow = append(waitingNow, name)
	}
	g.mu.Unlock()

	g.logger.Warn("group dependency wait: deadline expired",
		zap.String("capsule_id", capsuleID),
		zap.String("replica_id", entry.event.ReplicaID),
		zap.Duration("waited", waited),
		zap.Strings("still_waiting_on", waitingNow))

	if g.failFn != nil {
		g.failFn(capsuleID, "group dependencies not running within "+waited.String())
	}
}

// cancelGroup cancels every parked start whose capsule belongs to one
// of the given group IDs. The matching parked entries are removed and
// their per-park goroutines exit through the parkCtx.Done branch
// (without firing the deadline timer or invoking startFn). Unknown
// group IDs are silently ignored.
//
// Called by Handler.CancelGroupStarts when the node-side rollback path
// (events.MemberPlacementFailed) wants to abort every still-parked
// sibling of a failing same-node group. Releasing the parked entries
// without triggering startFn is essential — invoking startFn would
// race a fresh start against the rollback path's stop calls.
func (g *groupCoord) cancelGroup(groupIDs ...string) int {
	if g == nil || len(groupIDs) == 0 {
		return 0
	}
	wanted := make(map[string]struct{}, len(groupIDs))
	for _, id := range groupIDs {
		if id == "" {
			continue
		}
		wanted[id] = struct{}{}
	}
	if len(wanted) == 0 {
		return 0
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	cancelled := 0
	for parkedID, entry := range g.parked {
		info, ok := g.view.MemberInfo(parkedID)
		if !ok {
			continue
		}
		if _, match := wanted[info.GroupID]; !match {
			continue
		}
		entry.cancel()
		delete(g.parked, parkedID)
		cancelled++
		g.logger.Info("group rollback: parked start cancelled",
			zap.String("group_id", info.GroupID),
			zap.String("capsule_id", parkedID),
			zap.String("replica_id", entry.event.ReplicaID))
	}
	return cancelled
}

// stop cancels every pending parked-start. Called from Handler.Stop;
// the per-park goroutines exit through the parkCtx.Done branch.
func (g *groupCoord) stop() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, entry := range g.parked {
		entry.cancel()
		delete(g.parked, id)
	}
}

// markReleased records that a capsule has reached Running at least once,
// so future re-elections skip dep gating (first-boot-only semantics).
func (g *groupCoord) markReleased(capsuleID string) {
	g.mu.Lock()
	g.released[capsuleID] = true
	g.mu.Unlock()
}

// isReleased reports whether a capsule has been released past first boot.
// Exported for tests.
func (g *groupCoord) isReleased(capsuleID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.released[capsuleID]
}

// pendingCount reports how many parked starts the coordinator is currently
// holding. Exported for tests and observability.
func (g *groupCoord) pendingCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.parked)
}
