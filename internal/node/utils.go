package node

import (
	"hash/fnv"
	"strings"
)

func hash(s string) uint64 {
	h := fnv.New64()
	h.Write([]byte(s))
	return h.Sum64()
}

func normalizePeerID(peerID string) string {
	// If it's already a plain peer ID (starts with typical prefixes)
	if strings.HasPrefix(peerID, "12D3KooW") || strings.HasPrefix(peerID, "Qm") {
		return peerID
	}

	// If it's a multiaddr format, extract the peer ID part
	parts := strings.Split(peerID, "/p2p/")
	if len(parts) == 2 {
		return parts[1]
	}

	// If it doesn't match expected patterns, return as-is
	return peerID
}
