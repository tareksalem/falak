package service

import "time"

// CapsuleLookup is the read-only seam through which the service manager
// resolves backend capsule names to capsule IDs. Implemented externally
// (e.g. by capsule.Manager via an adapter) so the service module does
// not depend on the capsule package.
type CapsuleLookup interface {
	// Lookup returns the capsule ID and true when a capsule with the
	// given name exists locally, or "", false otherwise.
	Lookup(name string) (id string, exists bool)
}

// backendStatesByName indexes a BackendState slice by Name for quick lookup.
func backendStatesByName(states []BackendState) map[string]BackendState {
	out := make(map[string]BackendState, len(states))
	for _, s := range states {
		out[s.Name] = s
	}
	return out
}

// preserveCapturedIDs copies forward CapturedCapsuleID from prior backends
// onto new spec backends sharing the same capsule name. Updates that
// replace the entire backend list with a fresh map otherwise lose the
// resolved identity.
func preserveCapturedIDs(next, prev []ServiceBackend) []ServiceBackend {
	if len(prev) == 0 {
		return next
	}
	prevByName := make(map[string]string, len(prev))
	for _, b := range prev {
		if b.CapturedCapsuleID != "" {
			prevByName[b.Capsule] = b.CapturedCapsuleID
		}
	}
	for i := range next {
		if next[i].CapturedCapsuleID != "" {
			continue
		}
		if cid, ok := prevByName[next[i].Capsule]; ok {
			next[i].CapturedCapsuleID = cid
		}
	}
	return next
}

// mergeBackendStates rebuilds the BackendStates slice on Update so it
// matches the new backend set. Entries for dropped backends are
// discarded; entries for new backends start in Unresolved unless
// preserveCapturedIDs already captured an ID for them.
func mergeBackendStates(backends []ServiceBackend, existing map[string]BackendState, now time.Time) []BackendState {
	out := make([]BackendState, 0, len(backends))
	for _, b := range backends {
		if prev, ok := existing[b.Capsule]; ok {
			if b.CapturedCapsuleID != "" && prev.CapturedCapsuleID == "" {
				prev.CapturedCapsuleID = b.CapturedCapsuleID
				prev.Resolution = BackendResolutionEnum.Resolved()
				prev.LastResolvedAt = now
			}
			out = append(out, prev)
			continue
		}
		state := BackendState{Name: b.Capsule, LastResolvedAt: now}
		if b.CapturedCapsuleID != "" {
			state.CapturedCapsuleID = b.CapturedCapsuleID
			state.Resolution = BackendResolutionEnum.Resolved()
		} else {
			state.Resolution = BackendResolutionEnum.Unresolved()
		}
		out = append(out, state)
	}
	return out
}

// ensureBackendState returns the BackendState for name, creating a
// fresh Unresolved entry on the Service when none exists.
func ensureBackendState(svc *Service, name string, now time.Time) BackendState {
	for _, s := range svc.BackendStates {
		if s.Name == name {
			return s
		}
	}
	state := BackendState{
		Name:           name,
		Resolution:     BackendResolutionEnum.Unresolved(),
		LastResolvedAt: now,
	}
	svc.BackendStates = append(svc.BackendStates, state)
	return state
}

// writeBackBackendState replaces the in-place BackendState for state.Name
// on svc.BackendStates.
func writeBackBackendState(svc *Service, state BackendState) {
	for i := range svc.BackendStates {
		if svc.BackendStates[i].Name == state.Name {
			svc.BackendStates[i] = state
			return
		}
	}
	svc.BackendStates = append(svc.BackendStates, state)
}

// findBackendState returns the BackendState named name or nil.
func findBackendState(states []BackendState, name string) *BackendState {
	for i := range states {
		if states[i].Name == name {
			return &states[i]
		}
	}
	return nil
}
