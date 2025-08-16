package node

import (
	"log"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
)

func Bootstrap() {
	log.Println("Bootstrapping Falak node...")
}

func CreateNode() (host.Host, error) {
	node, err := libp2p.New()
	if err != nil {
		log.Fatalf("Failed to create node: %v", err)
		return nil, err
	}
	log.Println("Node created with ID:", node.ID())
	log.Println("Node is running with address:", node.Addrs())
	log.Println("Node started successfully")
	return node, nil
}
