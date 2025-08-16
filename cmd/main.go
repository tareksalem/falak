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
	
	// New advanced boot options
	cacheDir := flag.String("cache-dir", "", "Directory for phonebook cache (default: ~/.falak/)")
	seedFiles := flag.String("seed-files", "", "Comma-separated list of seed data files")
	dataCenters := flag.String("data-centers", "default", "Comma-separated list of data center identifiers this node belongs to")
	clusters := flag.String("clusters", "default", "Comma-separated list of clusters this node belongs to")
	connectionStrategy := flag.String("connection-strategy", "balanced", "Connection strategy: aggressive, conservative, balanced")
	enableAdvancedBoot := flag.Bool("advanced-boot", false, "Enable advanced boot process with DC-aware connections")
	
	// Certificate options
	rootCA := flag.String("root-ca", "", "Path to shared Root CA certificate (REQUIRED for production)")
	nodeCert := flag.String("node-cert", "", "Path to node certificate (optional - will be auto-generated)")
	
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
		node.WithClusters(clustersList),        // Pass list of clusters
		node.WithConnectionStrategy(*connectionStrategy),
		node.WithTags(node.Tags{
			"datacenters": dataCentersList,
			"clusters":    clustersList,
			"locations":   []string{"earth"},
		}),
	}
	
	// Add legacy peer connections if provided
	if len(peersList) > 0 {
		nodeOptions = append(nodeOptions, node.WithPeers(peersList))
	}
	
	// Add Root CA certificate if provided (highly recommended for production)
	if *rootCA != "" {
		nodeOptions = append(nodeOptions, node.WithRootCA(*rootCA))
		log.Printf("🔐 Using shared Root CA from: %s", *rootCA)
	} else {
		log.Printf("⚠️  WARNING: No Root CA provided - will use self-signed certificates (NOT for production)")
	}
	
	// Add node certificate if provided (optional - will be auto-generated if not provided)
	if *nodeCert != "" {
		nodeOptions = append(nodeOptions, node.WithNodeCert(*nodeCert))
		log.Printf("🔐 Using existing node certificate from: %s", *nodeCert)
	}
	
	// Add advanced boot if enabled
	if *enableAdvancedBoot {
		bootConfig := node.DefaultBootConfig()
		if *cacheDir != "" {
			bootConfig.CacheDir = *cacheDir
		}
		if *seedFiles != "" {
			bootConfig.SeedPaths = strings.Split(*seedFiles, ",")
		}
		// Use the first data center as primary for boot config
		if len(dataCentersList) > 0 {
			bootConfig.DataCenter = dataCentersList[0]
		}
		bootConfig.ConnectionStrategy = *connectionStrategy
		
		nodeOptions = append(nodeOptions, node.WithAdvancedBoot(bootConfig))
		log.Printf("Advanced boot enabled with primary DC: %s, strategy: %s", bootConfig.DataCenter, *connectionStrategy)
	}
	
	// Create the node
	n, err := node.NewNode(ctx, nodeOptions...)
	if err != nil {
		log.Fatalf("Failed to create node: %v", err)
	}
	log.Println("Node created successfully with ID:", n.ID(), n.GetHost().Addrs())
	
	// Print network diagnostics for debugging
	go func() {
		// Wait a bit for connections to establish
		for i := 0; i < 6; i++ {
			time.Sleep(5 * time.Second)
			diagnostics := n.GetNetworkDiagnostics()
			log.Printf("🔍 Network Diagnostics: connected_peers=%d, phonebook_entries=%d, topics=%d", 
				diagnostics["connected_peers"], diagnostics["phonebook_entries"], len(diagnostics["pubsub_topics"].([]string)))
			
			// Print peer details
			if peerDetails, ok := diagnostics["peer_details"].([]map[string]interface{}); ok && len(peerDetails) > 0 {
				log.Printf("🔗 Connected peers:")
				for _, peer := range peerDetails {
					log.Printf("   - %s", peer["peer_id"].(string)[:12]+"...")
				}
			} else {
				log.Printf("⚠️  No connected peers found")
			}
			
			// Print topic subscription details
			if topicDetails, ok := diagnostics["topic_details"].([]map[string]interface{}); ok && len(topicDetails) > 0 {
				log.Printf("📡 Topic subscriptions:")
				for _, topic := range topicDetails {
					peerCount := topic["peer_count"].(int)
					topicName := topic["name"].(string)
					if peerCount > 0 {
						peers := topic["peers"].([]string)
						log.Printf("   - %s: %d peers (%v)", topicName, peerCount, peers)
					} else {
						log.Printf("   - %s: 0 peers (isolated)", topicName)
					}
				}
			}
			
			// Print health registry details
			if healthInfo, ok := diagnostics["health_registry"].(map[string]interface{}); ok && len(healthInfo) > 0 {
				totalTracked := healthInfo["total_tracked"].(int)
				healthyCount := healthInfo["healthy_count"].(int)
				suspectCount := healthInfo["suspect_count"].(int)
				failedCount := healthInfo["failed_count"].(int)
				
				log.Printf("🏥 Health Registry: %d tracked (%d healthy, %d suspect, %d failed)", 
					totalTracked, healthyCount, suspectCount, failedCount)
					
				if peerHealthList, exists := healthInfo["peer_health"].([]map[string]interface{}); exists && len(peerHealthList) > 0 {
					for _, peerHealth := range peerHealthList {
						nodeID := peerHealth["node_id"].(string)
						status := peerHealth["status"].(string)
						phiScore := peerHealth["phi_score"].(float64)
						lastSeen := peerHealth["last_seen"].(int)
						log.Printf("   - %s: %s (φ=%.1f, %ds ago)", nodeID, status, phiScore, lastSeen)
					}
				}
			}
		}
	}()
	
	// Start advanced boot if enabled
	if *enableAdvancedBoot && n.IsAdvancedBootEnabled() {
		log.Println("Starting advanced boot process...")
		if err := n.StartAdvancedBoot(); err != nil {
			log.Fatalf("Advanced boot failed: %v", err)
		}
		log.Println("Advanced boot completed successfully")
		
		// Print boot status
		if status := n.GetBootStatus(); status != nil {
			log.Printf("Boot status: %s", status.GetSummary())
		}
		
		// Print connection health
		if health := n.GetConnectionHealth(); health != nil {
			log.Printf("Connection health: monitoring %d peers", health.TrackedPeers)
		}
	}

	// keep running
	sign := <-signChannel
	log.Printf("Received signal: %v", sign)
	
	// Graceful shutdown
	log.Println("Shutting down node...")
	if err := n.Close(); err != nil {
		log.Printf("Error during shutdown: %v", err)
	}
	log.Println("Node shutdown complete")
}
