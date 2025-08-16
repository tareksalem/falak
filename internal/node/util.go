package node

import "strings"

// normalizePeerID normalizes peer ID by extracting just the peer ID part
// Handles both formats: "12D3KooW..." and "/ip4/127.0.0.1/tcp/4001/p2p/12D3KooW..."
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