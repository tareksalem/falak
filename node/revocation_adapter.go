package node

import (
	"github.com/tareksalem/falak/node/auth"
	"github.com/tareksalem/falak/node/auth/certs"
	nodesync "github.com/tareksalem/falak/node/sync"
)

// revocationAdapter bridges the auth package's RevocationList to the sync
// package's RevocationSource interface. This avoids circular imports between
// the auth and sync packages by adapting at the node wiring level.
type revocationAdapter struct {
	authenticator *auth.Authenticator
}

// newRevocationAdapter creates a new adapter.
func newRevocationAdapter(authenticator *auth.Authenticator) *revocationAdapter {
	return &revocationAdapter{authenticator: authenticator}
}

// GetRevocations returns all revocation entries for a cluster.
func (a *revocationAdapter) GetRevocations(clusterPath string) []nodesync.RevocationEntry {
	rl := a.authenticator.GetRevocationList(clusterPath)
	entries := rl.List()

	result := make([]nodesync.RevocationEntry, len(entries))
	for i, e := range entries {
		result[i] = nodesync.RevocationEntry{
			CertFingerprint: e.CertFingerprint,
			NodeID:          e.NodeID,
			ClusterPath:     e.ClusterPath,
			RevokedAt:       e.RevokedAt,
			Reason:          e.Reason,
			RevokedBy:       e.RevokedBy,
		}
	}
	return result
}

// MergeRevocations merges remote revocation entries into the local revocation list.
func (a *revocationAdapter) MergeRevocations(clusterPath string, entries []nodesync.RevocationEntry) (int, error) {
	rl := a.authenticator.GetRevocationList(clusterPath)

	certEntries := make([]certs.RevocationEntry, len(entries))
	for i, e := range entries {
		certEntries[i] = certs.RevocationEntry{
			CertFingerprint: e.CertFingerprint,
			NodeID:          e.NodeID,
			ClusterPath:     e.ClusterPath,
			RevokedAt:       e.RevokedAt,
			Reason:          e.Reason,
			RevokedBy:       e.RevokedBy,
		}
	}

	return rl.Sync(certEntries)
}
