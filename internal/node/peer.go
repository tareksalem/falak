package node

import (
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

type Peer struct {
	ID         string
	Status     string
	AddrInfo   peer.AddrInfo
	Tags       Tags
	DataCenter string    // NEW: Data center identifier
	LastSeen   time.Time // NEW: Last seen timestamp
	TrustScore float64   // NEW: Trust score for the peer
}
