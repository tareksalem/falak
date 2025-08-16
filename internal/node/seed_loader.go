package node

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// SeedLoader handles loading of initial seed data for node discovery
type SeedLoader struct {
	seedPaths []string
}

// SeedData represents the structure of seed data files
type SeedData struct {
	Version     string                     `json:"version"`
	CreatedAt   time.Time                  `json:"created_at,omitempty"`
	Description string                     `json:"description,omitempty"`
	DataCenters map[string]*SeedDataCenter `json:"datacenters"`
}

// SeedDataCenter represents a data center in seed data
type SeedDataCenter struct {
	Name     string     `json:"name,omitempty"`
	Region   string     `json:"region,omitempty"`
	Provider string     `json:"provider,omitempty"`
	Peers    []SeedPeer `json:"peers"`
}

// SeedPeer represents a peer in seed data
type SeedPeer struct {
	NodeID     string            `json:"node_id"`
	Multiaddrs []string          `json:"multiaddrs"`
	Labels     map[string]string `json:"labels,omitempty"`
	Role       string            `json:"role,omitempty"`
	Priority   int               `json:"priority,omitempty"`
	Enabled    bool              `json:"enabled,omitempty"`
}

// NewSeedLoader creates a new seed loader with the given file paths
func NewSeedLoader(paths []string) *SeedLoader {
	return &SeedLoader{
		seedPaths: paths,
	}
}

// LoadSeedData loads and merges seed data from all configured paths
func (sl *SeedLoader) LoadSeedData() (*Phonebook, error) {
	phonebook := NewPhonebook()

	if len(sl.seedPaths) == 0 {
		log.Printf("No seed data paths configured")
		return phonebook, nil
	}

	totalLoaded := 0
	validFiles := 0

	for _, path := range sl.seedPaths {
		if path == "" {
			continue
		}

		seedData, err := sl.loadSeedFile(path)
		if err != nil {
			log.Printf("Warning: failed to load seed file %s: %v", path, err)
			continue
		}

		loaded, err := sl.mergeSeedIntoPhonebook(seedData, phonebook)
		if err != nil {
			log.Printf("Warning: failed to merge seed data from %s: %v", path, err)
			continue
		}

		totalLoaded += loaded
		validFiles++
		log.Printf("Loaded %d peers from seed file: %s", loaded, path)
	}

	log.Printf("Seed loading complete: %d peers from %d files", totalLoaded, validFiles)
	return phonebook, nil
}

// loadSeedFile loads and validates a single seed data file
func (sl *SeedLoader) loadSeedFile(path string) (*SeedData, error) {
	// Check if file exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("seed file does not exist: %s", path)
	}

	// Read file
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read seed file: %w", err)
	}

	// Parse JSON
	var seedData SeedData
	if err := json.Unmarshal(data, &seedData); err != nil {
		return nil, fmt.Errorf("failed to parse seed JSON: %w", err)
	}

	// Validate seed data
	if err := sl.ValidateSeedData(&seedData); err != nil {
		return nil, fmt.Errorf("seed data validation failed: %w", err)
	}

	return &seedData, nil
}

// ValidateSeedData validates the structure and content of seed data
func (sl *SeedLoader) ValidateSeedData(data *SeedData) error {
	// Check version
	if data.Version == "" {
		return fmt.Errorf("missing version field")
	}

	if data.Version != "1.0" {
		return fmt.Errorf("unsupported seed data version: %s", data.Version)
	}

	// Check data centers
	if len(data.DataCenters) == 0 {
		return fmt.Errorf("no data centers defined")
	}

	totalPeers := 0
	for dcID, dc := range data.DataCenters {
		if dcID == "" {
			return fmt.Errorf("empty data center ID")
		}

		if len(dc.Peers) == 0 {
			log.Printf("Warning: data center %s has no peers", dcID)
			continue
		}

		// Validate peers in this data center
		for i, peer := range dc.Peers {
			if err := sl.validateSeedPeer(peer, dcID, i); err != nil {
				return fmt.Errorf("invalid peer in DC %s[%d]: %w", dcID, i, err)
			}
		}

		totalPeers += len(dc.Peers)
	}

	if totalPeers == 0 {
		return fmt.Errorf("no valid peers found in seed data")
	}

	return nil
}

// validateSeedPeer validates a single seed peer
func (sl *SeedLoader) validateSeedPeer(seedPeer SeedPeer, dcID string, index int) error {
	// Check node ID
	if seedPeer.NodeID == "" {
		return fmt.Errorf("missing node_id")
	}

	// Validate peer ID format
	if _, err := peer.Decode(seedPeer.NodeID); err != nil {
		return fmt.Errorf("invalid node_id format: %w", err)
	}

	// Check multiaddrs
	if len(seedPeer.Multiaddrs) == 0 {
		return fmt.Errorf("no multiaddrs specified")
	}

	// Validate each multiaddr
	for i, addrStr := range seedPeer.Multiaddrs {
		if _, err := ma.NewMultiaddr(addrStr); err != nil {
			return fmt.Errorf("invalid multiaddr[%d] '%s': %w", i, addrStr, err)
		}
	}

	return nil
}

// mergeSeedIntoPhonebook merges seed data into the phonebook
func (sl *SeedLoader) mergeSeedIntoPhonebook(seedData *SeedData, phonebook *Phonebook) (int, error) {
	loaded := 0

	for dcID, dc := range seedData.DataCenters {
		for _, seedPeer := range dc.Peers {
			// Skip disabled peers
			if !seedPeer.Enabled && seedPeer.Enabled {
				// Only skip if explicitly set to false, default (zero value) is enabled
				continue
			}

			peer, err := sl.convertSeedPeer(seedPeer, dcID)
			if err != nil {
				log.Printf("Warning: failed to convert seed peer %s: %v", seedPeer.NodeID, err)
				continue
			}

			// Add seed-specific metadata
			if peer.Tags == nil {
				peer.Tags = make(Tags)
			}
			peer.Tags["source"] = "seed"
			peer.Tags["seed_priority"] = seedPeer.Priority
			if seedPeer.Role != "" {
				peer.Tags["role"] = seedPeer.Role
			}

			phonebook.AddPeer(*peer)
			loaded++
		}
	}

	return loaded, nil
}

// convertSeedPeer converts a SeedPeer to a Peer struct
func (sl *SeedLoader) convertSeedPeer(seedPeer SeedPeer, dcID string) (*Peer, error) {
	// Parse peer ID
	peerID, err := peer.Decode(seedPeer.NodeID)
	if err != nil {
		return nil, fmt.Errorf("invalid peer ID: %w", err)
	}

	// Parse multiaddrs
	var addrs []ma.Multiaddr
	for _, addrStr := range seedPeer.Multiaddrs {
		addr, err := ma.NewMultiaddr(addrStr)
		if err != nil {
			return nil, fmt.Errorf("invalid multiaddr %s: %w", addrStr, err)
		}
		addrs = append(addrs, addr)
	}

	// Create tags from labels
	tags := make(Tags)
	for k, v := range seedPeer.Labels {
		tags[k] = v
	}

	peerStruct := &Peer{
		ID:     seedPeer.NodeID,
		Status: "seed", // Special status for seed peers
		AddrInfo: peer.AddrInfo{
			ID:    peerID,
			Addrs: addrs,
		},
		Tags:       tags,
		DataCenter: dcID,
		LastSeen:   time.Time{}, // Never seen yet
		TrustScore: 0.8,         // Default seed trust score
	}

	return peerStruct, nil
}

// GetSeedPeersByDataCenter returns seed peers organized by data center
func (sl *SeedLoader) GetSeedPeersByDataCenter() (map[string][]SeedPeer, error) {
	result := make(map[string][]SeedPeer)

	for _, path := range sl.seedPaths {
		seedData, err := sl.loadSeedFile(path)
		if err != nil {
			log.Printf("Warning: failed to load seed file %s: %v", path, err)
			continue
		}

		for dcID, dc := range seedData.DataCenters {
			// Filter enabled peers
			var enabledPeers []SeedPeer
			for _, peer := range dc.Peers {
				if peer.Enabled || (!peer.Enabled && peer.Enabled) {
					enabledPeers = append(enabledPeers, peer)
				}
			}

			// Sort by priority (higher priority first)
			sort.Slice(enabledPeers, func(i, j int) bool {
				return enabledPeers[i].Priority > enabledPeers[j].Priority
			})

			result[dcID] = append(result[dcID], enabledPeers...)
		}
	}

	return result, nil
}

// GetHighPrioritySeeds returns only high-priority seed nodes
func (sl *SeedLoader) GetHighPrioritySeeds(minPriority int) (*Phonebook, error) {
	phonebook := NewPhonebook()

	seedsByDC, err := sl.GetSeedPeersByDataCenter()
	if err != nil {
		return phonebook, err
	}

	loaded := 0
	for dcID, peers := range seedsByDC {
		for _, seedPeer := range peers {
			if seedPeer.Priority >= minPriority {
				peer, err := sl.convertSeedPeer(seedPeer, dcID)
				if err != nil {
					log.Printf("Warning: failed to convert high-priority seed peer %s: %v",
						seedPeer.NodeID, err)
					continue
				}

				if peer.Tags == nil {
					peer.Tags = make(Tags)
				}
				peer.Tags["source"] = "high_priority_seed"
				peer.Tags["seed_priority"] = seedPeer.Priority

				phonebook.AddPeer(*peer)
				loaded++
			}
		}
	}

	log.Printf("Loaded %d high-priority seed peers (min priority: %d)", loaded, minPriority)
	return phonebook, nil
}

// CreateExampleSeedFile creates an example seed file for reference
func CreateExampleSeedFile(path string) error {
	example := SeedData{
		Version:     "1.0",
		CreatedAt:   time.Now(),
		Description: "Example Falak seed data file",
		DataCenters: map[string]*SeedDataCenter{
			"us-east-1": {
				Name:     "US East 1",
				Region:   "us-east",
				Provider: "aws",
				Peers: []SeedPeer{
					{
						NodeID: "12D3KooWExamplePeer1HashHereForUSEast1",
						Multiaddrs: []string{
							"/ip4/10.0.1.5/tcp/4001",
							"/ip4/172.16.1.5/tcp/4001",
						},
						Labels: map[string]string{
							"role": "bootstrap",
							"zone": "us-east-1a",
						},
						Role:     "bootstrap",
						Priority: 100,
						Enabled:  true,
					},
				},
			},
			"us-west-2": {
				Name:     "US West 2",
				Region:   "us-west",
				Provider: "aws",
				Peers: []SeedPeer{
					{
						NodeID: "12D3KooWExamplePeer2HashHereForUSWest2",
						Multiaddrs: []string{
							"/ip4/10.0.2.5/tcp/4001",
						},
						Labels: map[string]string{
							"role": "bootstrap",
							"zone": "us-west-2a",
						},
						Role:     "bootstrap",
						Priority: 90,
						Enabled:  true,
					},
				},
			},
		},
	}

	data, err := json.MarshalIndent(example, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal example: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write example file: %w", err)
	}

	return nil
}
