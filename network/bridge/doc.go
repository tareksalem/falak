// Package bridge owns the per-group Linux bridge plumbing for Falak's
// service-networking layer: subnet allocation, Podman network
// create/destroy, gateway IP assignment, and cross-bridge isolation rules.
//
// Layout:
//   - allocator.go — SQLite-persisted /24 subnet allocator (task 11A.1).
//   - bridge.go    — Manager: atomic Podman bridge create + iptables
//     isolation install, reaper-idempotent Destroy, Inspect (tasks 11A.2
//     and 11A.15b, landed atomically per the plan's implementation order).
//   - podman.go    — narrow PodmanNetworkClient interface and a minimal
//     HTTP adapter that hits Podman's libpod REST API directly so the
//     network/ module is free of any runtime/ import.
//
// The allocator hands out /24 child subnets from a configurable /16
// parent pool (default 10.88.0.0/16, service-networking.md decision #25),
// idempotent per groupID, survives node restarts via shared/migrations,
// and is safe under concurrent callers via an in-process Mutex layered
// on SQLite's single-writer guarantee.
//
// Manager.Create allocates a subnet, asks Podman to create the bridge
// network, then installs three FILTER-table FORWARD rules: within-bridge
// ACCEPT, cross-bridge DROP in both directions. The three steps are a
// single atomic operation — failure at any step rolls back earlier steps
// so there is never a window where a bridge exists without isolation
// rules (the 11A.15b atomic install requirement).
package bridge
