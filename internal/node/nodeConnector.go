package node

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/multiformats/go-multiaddr"
)

type NodeConnector struct {
	node *Node
}

func (c *NodeConnector) Listen(n network.Network, addr multiaddr.Multiaddr) {
	log.Printf("🎧 NodeConnector.Listen: %s listening on %s", c.node.name, addr.String())
}
func (c *NodeConnector) ListenClose(n network.Network, addr multiaddr.Multiaddr) {
	log.Printf("🚫 NodeConnector.ListenClose: %s stopped listening on %s", c.node.name, addr.String())
}
func (c *NodeConnector) OpenedStream(n network.Network, s network.Stream) {
	log.Printf("📂 NodeConnector.OpenedStream: %s opened stream with %s", c.node.name, s.Conn().RemotePeer().ShortString())
}
func (c *NodeConnector) ClosedStream(n network.Network, s network.Stream) {
	log.Printf("📪 NodeConnector.ClosedStream: %s closed stream with %s", c.node.name, s.Conn().RemotePeer().ShortString())
}

func (c *NodeConnector) Connected(n network.Network, conn network.Conn) {
	remotePeer := conn.RemotePeer()
	
	// Force log this connection for debugging
	log.Printf("🔌 NodeConnector.Connected: %s -> %s (Direction: %s)", 
		remotePeer.ShortString(), c.node.name, conn.Stat().Direction)
	
	// CRITICAL DEBUG: Check protocol support and pubsub after connection
	go func() {
		time.Sleep(1 * time.Second) // Allow protocol negotiation time
		
		// Check if peer supports pubsub protocols
		protocols, err := c.node.host.Peerstore().GetProtocols(remotePeer)
		if err != nil {
			log.Printf("❌ CRITICAL: Failed to get protocols for %s: %v", remotePeer.ShortString(), err)
		} else {
			log.Printf("🔍 CRITICAL: %s protocols for peer %s:", c.node.name, remotePeer.ShortString())
			pubsubFound := false
			for i, proto := range protocols {
				log.Printf("   [%d] Protocol: %s", i, proto)
				if strings.Contains(string(proto), "floodsub") || strings.Contains(string(proto), "gossipsub") {
					pubsubFound = true
					log.Printf("       ✅ PUBSUB PROTOCOL FOUND!")
				}
			}
			if !pubsubFound {
				log.Printf("       ❌ NO PUBSUB PROTOCOLS FOUND!")
			}
		}
		
		// Check pubsub topic peers after protocol negotiation
		c.node.mu.Lock()
		for topicName, ct := range c.node.connectedTopics {
			peers := ct.Topic.ListPeers()
			log.Printf("🔍 CRITICAL: %s after connection - topic %s has %d peers", c.node.name, topicName, len(peers))
			for j, peerID := range peers {
				log.Printf("   [%d] Topic peer: %s", j, peerID.ShortString())
			}
		}
		c.node.mu.Unlock()
	}()
	
	// According to docs: DO NOT add to phonebook immediately
	// Peers should only be added after successful authentication via ServerAck
	// This ensures proper authentication flow: connect -> clientHello -> challenge -> authenticate -> phonebook
	log.Printf("📋 Connection established, waiting for authentication before adding to phonebook")
}

func (c *NodeConnector) Disconnected(n network.Network, conn network.Conn) {
	remotePeer := conn.RemotePeer()
	fmt.Printf("❌ Disconnected from: %s\n", remotePeer.ShortString())
	
	// Remove the peer from our phonebook
	if c.node.phonebook != nil {
		normalizedPeerID := normalizePeerID(remotePeer.String())
		c.node.phonebook.RemovePeer(normalizedPeerID)
		fmt.Printf("📚 Removed disconnected peer %s from phonebook\n", remotePeer.ShortString())
	}
}
