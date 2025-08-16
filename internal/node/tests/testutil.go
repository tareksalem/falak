package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
	"github.com/tareksalem/falak/internal/node"
	ma "github.com/multiformats/go-multiaddr"
)

// TestContext creates a context with timeout for tests
func TestContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// CreateTestNode creates a node with minimal configuration for testing
func CreateTestNode(t *testing.T, name string, port int) (*node.Node, func()) {
	ctx := TestContext(t)
	
	address := fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", port)
	
	n, err := node.NewNode(ctx,
		node.WithName(name),
		node.WithAddress(address),
		node.WithDeterministicID(name),
		node.WithTags(map[string]interface{}{
			"test": true,
			"name": name,
		}),
	)
	require.NoError(t, err)
	require.NotNil(t, n)
	
	cleanup := func() {
		if err := n.Close(); err != nil {
			t.Logf("Error closing node %s: %v", name, err)
		}
	}
	
	return n, cleanup
}

// CreateTestNodePair creates two connected test nodes
func CreateTestNodePair(t *testing.T) (*node.Node, *node.Node, func()) {
	node1, cleanup1 := CreateTestNode(t, "test-node-1", 0) // 0 = random port
	node2, cleanup2 := CreateTestNode(t, "test-node-2", 0) // 0 = random port
	
	// Connect node2 to node1
	node1Addrs := node1.GetHost().Addrs()
	require.NotEmpty(t, node1Addrs, "Node1 should have addresses")
	
	// Build full multiaddr for node1
	node1ID := node1.GetHost().ID()
	fullAddr := node1Addrs[0].Encapsulate(ma.StringCast(fmt.Sprintf("/p2p/%s", node1ID.String())))
	
	peerInput := node.PeerInput{
		ID:   node1ID.String(),
		Addr: fullAddr.String(),
		Tags: map[string]interface{}{"test": true},
	}
	
	err := node2.ConnectPeer(peerInput)
	require.NoError(t, err)
	
	// Wait for connection to be established
	WaitForConnection(t, node1.GetHost(), node2.GetHost().ID())
	WaitForConnection(t, node2.GetHost(), node1.GetHost().ID())
	
	cleanup := func() {
		cleanup1()
		cleanup2()
	}
	
	return node1, node2, cleanup
}

// WaitForConnection waits for a connection to be established between hosts
func WaitForConnection(t *testing.T, host host.Host, peerID peer.ID) {
	require.Eventually(t, func() bool {
		return host.Network().Connectedness(peerID).String() == "Connected"
	}, 5*time.Second, 100*time.Millisecond, "Connection should be established")
}

// CreateTestPeer creates a test peer struct
func CreateTestPeer(name string, dc string) node.Peer {
	// Generate a deterministic peer ID for testing
	privKey, _, err := crypto.GenerateKeyPairWithReader(crypto.Ed25519, -1, nil)
	if err != nil {
		panic(fmt.Sprintf("Failed to generate test key: %v", err))
	}
	
	peerID, err := peer.IDFromPrivateKey(privKey)
	if err != nil {
		panic(fmt.Sprintf("Failed to get peer ID: %v", err))
	}
	
	addr, err := ma.NewMultiaddr("/ip4/127.0.0.1/tcp/4001")
	if err != nil {
		panic(fmt.Sprintf("Failed to create test address: %v", err))
	}
	
	addrInfo := peer.AddrInfo{
		ID:    peerID,
		Addrs: []ma.Multiaddr{addr},
	}
	
	return node.Peer{
		ID:         peerID.String(),
		Status:     "active",
		AddrInfo:   addrInfo,
		Tags:       map[string]interface{}{"test": true, "name": name},
		DataCenter: dc,
		LastSeen:   time.Now(),
		TrustScore: 1.0,
	}
}

// AssertEventuallyConnected asserts that two nodes are eventually connected
func AssertEventuallyConnected(t *testing.T, node1, node2 *node.Node) {
	// First check libp2p network level connection
	require.Eventually(t, func() bool {
		peers1 := node1.GetHost().Network().Peers()
		peers2 := node2.GetHost().Network().Peers()
		
		node1HasNode2 := false
		for _, p := range peers1 {
			if p == node2.GetHost().ID() {
				node1HasNode2 = true
				break
			}
		}
		
		node2HasNode1 := false
		for _, p := range peers2 {
			if p == node1.GetHost().ID() {
				node2HasNode1 = true
				break
			}
		}
		
		return node1HasNode2 && node2HasNode1
	}, 10*time.Second, 500*time.Millisecond, "Nodes should be connected at libp2p level")
}

// WaitForTopicPeers waits for a topic to have the expected number of peers
func WaitForTopicPeers(t *testing.T, n *node.Node, topicName string, expectedPeers int) {
	topics := n.GetConnectedTopics()
	topic, exists := topics[topicName]
	require.True(t, exists, "Topic should exist")
	
	require.Eventually(t, func() bool {
		peers := topic.Topic.ListPeers()
		return len(peers) == expectedPeers
	}, 10*time.Second, 500*time.Millisecond, 
		"Topic should have %d peers", expectedPeers)
}