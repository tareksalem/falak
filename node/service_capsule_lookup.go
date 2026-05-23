package node

import "github.com/tareksalem/falak/capsule"

// capsuleLookupAdapter satisfies service.CapsuleLookup using a
// capsule.Manager. The adapter is intentionally minimal — Lookup is
// the only method the service manager needs.
type capsuleLookupAdapter struct {
	m *capsule.Manager
}

// Lookup returns (capsuleID, true) when a capsule with the requested
// name exists on this node, or ("", false) otherwise.
func (a capsuleLookupAdapter) Lookup(name string) (string, bool) {
	if a.m == nil {
		return "", false
	}
	c := a.m.GetByName(name)
	if c == nil {
		return "", false
	}
	return string(c.ID), true
}
