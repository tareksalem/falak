package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tareksalem/falak/internal/node"
	"github.com/thoas/go-funk"
)

func main() {
	log.Println("Hello, Falak!")
	signChannel := make(chan os.Signal, 1)

	// Capture OS signals
	signal.Notify(signChannel, os.Interrupt, syscall.SIGTERM)
	log.Println("Listening for OS signals...")

	// capture arguments
	nodeName := flag.String("name", "falak-node", "Name of the Falak node")
	addr := flag.String("address", "/ip4/0.0.0.0/tcp/0", "Address of the Falak node")
	peers := flag.String("peers", "", "Comma-separated list of peer addresses")

	dataCenters := flag.String("data-centers", "default", "Comma-separated list of data center identifiers this node belongs to")
	clusters := flag.String("clusters", "default", "Comma-separated list of clusters this node belongs to")

	flag.Parse()
	log.Println("Node name:", *nodeName)
	log.Println("Node address:", *addr)
	log.Println("Peers:", *peers, strings.Split(*peers, ","))
	// Start the main node
	ctx := context.Background()
	var peersList []node.PeerInput
	if *peers != "" {
		peersList = funk.Map(strings.Split(*peers, ","), func(p string) node.PeerInput {
			return node.PeerInput{
				ID:   p,
				Addr: p,
			}
		}).([]node.PeerInput)
	}
	log.Println("Parsed peers:", peersList)

	// Parse data centers and clusters lists
	dataCentersList := strings.Split(*dataCenters, ",")
	clustersList := strings.Split(*clusters, ",")

	// Trim whitespace from each item
	for i := range dataCentersList {
		dataCentersList[i] = strings.TrimSpace(dataCentersList[i])
	}
	for i := range clustersList {
		clustersList[i] = strings.TrimSpace(clustersList[i])
	}

	log.Printf("Data centers: %v", dataCentersList)
	log.Printf("Clusters: %v", clustersList)

	// Build node options
	nodeOptions := []node.NodeOption{
		node.WithName(*nodeName),
		node.WithAddress(*addr),
		node.WithDeterministicID(*nodeName),
		node.WithDataCenters(dataCentersList), // Pass list of data centers
		node.WithClusters(clustersList),       // Pass list of clusters
		node.WithTags(node.Tags{
			"datacenters": dataCentersList,
			"clusters":    clustersList,
			"locations":   []string{"earth"},
		}),
	}

	// Add peer connections if provided
	if len(peersList) > 0 {
		nodeOptions = append(nodeOptions, node.WithPeers(peersList))
	}

	// Create the node
	n, err := node.NewNode(ctx, nodeOptions...)
	if err != nil {
		log.Fatalf("Failed to create node: %v", err)
	}
	log.Println("Node created successfully with ID:", n.ID(), n.GetHost().Addrs())

	// Print basic network diagnostics for debugging
	go func() {
		// Wait a bit for connections to establish
		for i := 0; i < 6; i++ {
			time.Sleep(5 * time.Second)
			diagnostics := n.GetNetworkDiagnostics()
			log.Printf("🔍 Network Diagnostics: connected_peers=%d, phonebook_entries=%d",
				diagnostics["connected_peers"], diagnostics["phonebook_entries"])

			// Print peer details
			if peerDetails, ok := diagnostics["peer_details"].([]map[string]interface{}); ok && len(peerDetails) > 0 {
				log.Printf("🔗 Connected peers:")
				for _, peer := range peerDetails {
					log.Printf("   - %s", peer["peer_id"].(string)[:12]+"...")
				}
			} else {
				log.Printf("⚠️  No connected peers found")
			}
		}
	}()

	// keep running
	sign := <-signChannel
	log.Printf("Received signal: %v", sign)

	// Graceful shutdown
	log.Println("Shutting down node...")
	// Node will be cleaned up automatically when context is done
	log.Println("Node shutdown complete")
}
