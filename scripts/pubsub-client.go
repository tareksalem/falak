package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
	tls "github.com/libp2p/go-libp2p/p2p/security/tls"
	tcp "github.com/libp2p/go-libp2p/p2p/transport/tcp"
	ma "github.com/multiformats/go-multiaddr"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Printf("Usage: %s <bootstrap-peer> <topic> <message>\n", os.Args[0])
		fmt.Printf("Example: %s /ip4/127.0.0.1/tcp/4001/p2p/12D3KooWLvpdqYGS7kv5R8qAitapnVCGZfvQZTdvWm86omX2HoZW falak/default/test/interactive 'Hello from client!'\n")
		os.Exit(1)
	}

	bootstrapPeer := os.Args[1]
	topicName := os.Args[2]
	message := strings.Join(os.Args[3:], " ")

	ctx := context.Background()

	// Create a libp2p host
	host, err := libp2p.New(
		libp2p.ListenAddrStrings("/ip4/0.0.0.0/tcp/0"), // Random port
		libp2p.Security(tls.ID, tls.New),
		libp2p.Transport(tcp.NewTCPTransport),
	)
	if err != nil {
		log.Fatalf("Failed to create libp2p host: %v", err)
	}
	defer host.Close()

	log.Printf("Client node created with ID: %s", host.ID())

	// Connect to bootstrap peer
	addr, err := ma.NewMultiaddr(bootstrapPeer)
	if err != nil {
		log.Fatalf("Invalid bootstrap peer address: %v", err)
	}

	info, err := peer.AddrInfoFromP2pAddr(addr)
	if err != nil {
		log.Fatalf("Failed to parse peer info: %v", err)
	}

	log.Printf("Connecting to bootstrap peer: %s", info.ID)
	err = host.Connect(ctx, *info)
	if err != nil {
		log.Fatalf("Failed to connect to bootstrap peer: %v", err)
	}

	log.Printf("Successfully connected to bootstrap peer")

	// Create pubsub instance (FloodSub to match the nodes)
	ps, err := pubsub.NewFloodSub(ctx, host)
	if err != nil {
		log.Fatalf("Failed to create pubsub: %v", err)
	}

	// Join the topic
	log.Printf("Joining topic: %s", topicName)
	topic, err := ps.Join(topicName)
	if err != nil {
		log.Fatalf("Failed to join topic: %v", err)
	}
	defer topic.Close()

	// Subscribe to the topic to ensure proper propagation
	sub, err := topic.Subscribe()
	if err != nil {
		log.Fatalf("Failed to subscribe to topic: %v", err)
	}
	defer sub.Cancel()

	// Give some time for the network to propagate
	log.Printf("Waiting for network propagation...")
	time.Sleep(3 * time.Second)

	// Publish the message
	log.Printf("Publishing message: %s", message)
	err = topic.Publish(ctx, []byte(message))
	if err != nil {
		log.Fatalf("Failed to publish message: %v", err)
	}

	log.Printf("✅ Message published successfully to topic: %s", topicName)
	log.Printf("📤 Message: %s", message)

	// Wait a bit to ensure message is sent
	time.Sleep(2 * time.Second)
}