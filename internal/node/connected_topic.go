package node

import (
	"log"
	"strings"

	"github.com/gogo/protobuf/proto"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/tareksalem/falak/internal/node/protobuf/models"
)

type ConnectedTopic struct {
	Name        string
	Topic       *pubsub.Topic
	Sub         *pubsub.Subscription
	node        *Node // Reference to the node for accessing managers
	localPeerID peer.ID
}

func (ct *ConnectedTopic) onMessage(msg *pubsub.Message) {
	// Self-message filtering logic
	if ct.isHeartbeatTopic() {
		// Parse heartbeat to check if it's from ourselves
		var heartbeat models.Heartbeat
		if err := proto.Unmarshal(msg.Data, &heartbeat); err == nil {
			normalizedMsgNodeID := normalizePeerID(heartbeat.NodeId)
			normalizedLocalNodeID := normalizePeerID(ct.node.id)
			if normalizedMsgNodeID == normalizedLocalNodeID {
				// This is our own heartbeat, ignore it
				return
			}
			// This is a legitimate heartbeat from another node - log it for debugging
			log.Printf("💓 CRITICAL: %s received heartbeat from peer %s on topic %s (ReceivedFrom: %s, Local: %s)",
				ct.node.name, normalizedMsgNodeID, ct.Name, msg.ReceivedFrom.ShortString(), ct.localPeerID.ShortString())
		} else {
			log.Printf("❌ CRITICAL: %s failed to unmarshal heartbeat: %v", ct.node.name, err)
			return
		}
	} else {
		// For non-heartbeat topics, use the original ReceivedFrom check
		if msg.ReceivedFrom == ct.localPeerID {
			log.Printf("🔄 Ignoring self message on topic %s from %s", ct.Name, msg.ReceivedFrom.ShortString())
			return
		}
	}

	// Log processing (less verbose for heartbeat topics)
	if !ct.isHeartbeatTopic() {
		log.Printf("📨 Processing message on topic %s from %s (local: %s)",
			ct.Name, msg.ReceivedFrom.ShortString(), ct.localPeerID.ShortString())
	}

	// Topic message processing is now handled by the complete event router
	// All messages are automatically routed through the reactor event system
	log.Printf("📨 Message received on topic %s from %s (processed by event router)",
		ct.Name, msg.ReceivedFrom.ShortString())
}

// isHeartbeatTopic checks if this topic is a heartbeat topic
func (ct *ConnectedTopic) isHeartbeatTopic() bool {
	return strings.Contains(ct.Name, "/heartbeat")
}
