// Package dns implements Falak's per-node DNS responder.
//
// Each per-group bridge routes the fixed link-local address
// 169.254.169.250:53 to the local responder, so every container in
// every group resolves capsule names through the same address. The
// responder consults the local endpoints.Registry, applies a
// visibility predicate, and replies with A records carrying the
// per-group bridge IPs of all alive replicas.
//
// The fixed link-local IP is the keystone of CRIU snapshot safety: a
// container's /etc/resolv.conf points at 169.254.169.250 regardless of
// the node it is currently running on, so a restored container reaches
// the new node's responder without any post-restore rewrite.
package dns
