package service

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// Rebind clears the captured ID on the named backend of service id and
// re-resolves it against the current capsule store. Used by the
// operator after an intentional delete+recreate to resume routing
// (Decision #14).
func (m *Manager) Rebind(_ context.Context, id ServiceID, backendName string) error {
	svc := m.store.Get(id)
	if svc == nil {
		return fmt.Errorf("%w: %s", ErrServiceNotFound, id)
	}
	now := time.Now()
	found := false
	// Restore the spec weight from BackendState.PriorWeight when the
	// previous identity-change had zeroed it; otherwise leave the
	// operator-supplied weight untouched.
	priorWeight := int32(0)
	for i := range svc.BackendStates {
		if svc.BackendStates[i].Name == backendName {
			priorWeight = svc.BackendStates[i].PriorWeight
			break
		}
	}
	for i := range svc.Spec.Backends {
		if svc.Spec.Backends[i].Capsule == backendName {
			svc.Spec.Backends[i].CapturedCapsuleID = ""
			if svc.Spec.Backends[i].Weight == 0 && priorWeight > 0 {
				svc.Spec.Backends[i].Weight = priorWeight
			}
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("service: backend %q not present on service %s", backendName, id)
	}
	for i := range svc.BackendStates {
		if svc.BackendStates[i].Name == backendName {
			svc.BackendStates[i].CapturedCapsuleID = ""
			svc.BackendStates[i].Resolution = BackendResolutionEnum.Unresolved()
			svc.BackendStates[i].PriorWeight = 0
		}
	}
	m.resolveBackend(svc, backendName, now)
	if err := m.store.Update(svc); err != nil {
		return fmt.Errorf("service: persist rebind: %w", err)
	}
	m.logger.Info("service backend rebound",
		zap.String("service", id.String()),
		zap.String("backend", backendName))
	if state := findBackendState(svc.BackendStates, backendName); state != nil &&
		state.Resolution == BackendResolutionEnum.Resolved() {
		m.emit(EventServiceBackendResolved, svc, map[string]string{
			MetaBackendName:  backendName,
			MetaNewCapsuleID: state.CapturedCapsuleID,
		})
	}
	return nil
}

// OnCapsuleReceived is the hook invoked by the capsule subsystem when a
// capsule appears (or is updated) on the local node. The service
// manager re-evaluates every backend referencing this capsule name and
// emits the appropriate resolution event.
func (m *Manager) OnCapsuleReceived(capsuleName, capsuleID string) {
	if capsuleName == "" || capsuleID == "" {
		return
	}
	now := time.Now()
	for _, svc := range m.store.ListReferencingCapsule(capsuleName) {
		if m.reconcileBackendIdentity(svc, capsuleName, capsuleID, now) {
			svc.UpdatedAt = now
			if err := m.store.Update(svc); err != nil {
				m.logger.Error("service: persist resolution update",
					zap.String("service", svc.ID.String()), zap.Error(err))
			}
		}
	}
}

// reconcileBackendIdentity applies the identity-binding rules to a
// single Service for the named capsule's current ID. Returns whether
// any backend state was mutated.
func (m *Manager) reconcileBackendIdentity(svc *Service, name, id string, now time.Time) bool {
	changed := false
	for i := range svc.Spec.Backends {
		b := &svc.Spec.Backends[i]
		if b.Capsule != name {
			continue
		}
		state := ensureBackendState(svc, name, now)
		switch state.Resolution {
		case BackendResolutionEnum.Unresolved(),
			BackendResolutionEnum.UnresolvedCapsuleDeleted():
			b.CapturedCapsuleID = id
			state.CapturedCapsuleID = id
			state.Resolution = BackendResolutionEnum.Resolved()
			state.LastResolvedAt = now
			changed = true
			m.metrics.IncBackendResolved(svc.ClusterID)
			m.logger.Info("service backend resolved",
				zap.String("service", svc.ID.String()),
				zap.String("backend", name),
				zap.String("capsule", id))
			m.emit(EventServiceBackendResolved, svc, map[string]string{
				MetaBackendName:  name,
				MetaNewCapsuleID: id,
			})
		case BackendResolutionEnum.Resolved():
			if state.CapturedCapsuleID == id {
				continue
			}
			previous := state.CapturedCapsuleID
			state.Resolution = BackendResolutionEnum.UnresolvedIdentityChanged()
			// Capture the operator-supplied weight before we zero it
			// so Rebind can restore it on a successful re-resolve.
			if b.Weight > 0 {
				state.PriorWeight = b.Weight
			}
			// Zero the spec weight for this backend so downstream
			// strategy engines (canary auto-abort, static drop) see the
			// inadmissible backend on the next Update. Decision #14:
			// identity-changed backends are not routable until an
			// explicit Rebind.
			b.Weight = 0
			changed = true
			m.metrics.IncBackendUnresolved(svc.ClusterID, "identity_changed")
			m.logger.Warn("service backend identity changed",
				zap.String("service", svc.ID.String()),
				zap.String("backend", name),
				zap.String("previous_capsule", previous),
				zap.String("new_capsule", id))
			m.emit(EventServiceBackendIdentityChanged, svc, map[string]string{
				MetaBackendName:       name,
				MetaPreviousCapsuleID: previous,
				MetaNewCapsuleID:      id,
			})
			// Re-emit Updated so subscribers (proxy / strategy engines)
			// pick up the zeroed weight without waiting for the next
			// operator Update call.
			m.emit(EventServiceUpdated, svc, nil)
		case BackendResolutionEnum.UnresolvedIdentityChanged():
			// Already flagged; no-op until operator runs Rebind.
		}
		writeBackBackendState(svc, state)
	}
	return changed
}

// OnCapsuleDeleted is the hook invoked by the capsule subsystem when a
// capsule disappears. Resolved backends keep their captured ID so a
// subsequent OnCapsuleReceived with the same ID restores them; a
// different ID flags identity-changed and requires explicit Rebind.
func (m *Manager) OnCapsuleDeleted(capsuleName string) {
	if capsuleName == "" {
		return
	}
	now := time.Now()
	for _, svc := range m.store.ListReferencingCapsule(capsuleName) {
		changed := false
		for i := range svc.Spec.Backends {
			if svc.Spec.Backends[i].Capsule != capsuleName {
				continue
			}
			state := ensureBackendState(svc, capsuleName, now)
			if state.Resolution == BackendResolutionEnum.Resolved() {
				state.Resolution = BackendResolutionEnum.UnresolvedCapsuleDeleted()
				changed = true
				m.metrics.IncBackendUnresolved(svc.ClusterID, "capsule_deleted")
				m.logger.Info("service backend unresolved: capsule deleted",
					zap.String("service", svc.ID.String()),
					zap.String("backend", capsuleName))
				m.emit(EventServiceBackendUnresolved, svc, map[string]string{
					MetaBackendName: capsuleName,
				})
			}
			writeBackBackendState(svc, state)
		}
		if changed {
			svc.UpdatedAt = now
			if err := m.store.Update(svc); err != nil {
				m.logger.Error("service: persist unresolved update",
					zap.String("service", svc.ID.String()), zap.Error(err))
			}
		}
	}
}

// resolveAllBackends runs first-resolve against every backend on a
// freshly-created Service. Resolves are best-effort: missing capsules
// leave the backend in Unresolved state (lenient mode, Decision #15).
func (m *Manager) resolveAllBackends(svc *Service, now time.Time) {
	if svc == nil || len(svc.Spec.Backends) == 0 {
		return
	}
	if svc.BackendStates == nil {
		svc.BackendStates = make([]BackendState, 0, len(svc.Spec.Backends))
	}
	for i := range svc.Spec.Backends {
		b := &svc.Spec.Backends[i]
		state := BackendState{Name: b.Capsule, LastResolvedAt: now}
		if m.capsules != nil {
			if id, exists := m.capsules.Lookup(b.Capsule); exists {
				b.CapturedCapsuleID = id
				state.CapturedCapsuleID = id
				state.Resolution = BackendResolutionEnum.Resolved()
			} else {
				state.Resolution = BackendResolutionEnum.Unresolved()
			}
		} else {
			state.Resolution = BackendResolutionEnum.Unresolved()
		}
		svc.BackendStates = append(svc.BackendStates, state)
	}
}

// resolveBackend re-runs identity resolution for one named backend. Used
// by Rebind to flip back to Resolved when the capsule now exists.
func (m *Manager) resolveBackend(svc *Service, name string, now time.Time) {
	if m.capsules == nil {
		return
	}
	id, exists := m.capsules.Lookup(name)
	if !exists {
		return
	}
	for i := range svc.Spec.Backends {
		if svc.Spec.Backends[i].Capsule == name {
			svc.Spec.Backends[i].CapturedCapsuleID = id
		}
	}
	state := ensureBackendState(svc, name, now)
	state.CapturedCapsuleID = id
	state.Resolution = BackendResolutionEnum.Resolved()
	state.LastResolvedAt = now
	writeBackBackendState(svc, state)
}
