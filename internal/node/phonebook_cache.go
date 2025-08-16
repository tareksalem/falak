package node

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// PhonebookCache handles persistence of phonebook data to local storage
type PhonebookCache struct {
	filePath string
	mu       sync.RWMutex
}

// CachedPhonebookData represents the serializable structure for phonebook persistence
type CachedPhonebookData struct {
	Version     string                 `json:"version"`
	UpdatedAt   time.Time              `json:"updated_at"`
	NodeID      string                 `json:"node_id,omitempty"`
	DataCenters map[string][]CachedPeer `json:"datacenters"`
}

// CachedPeer represents a peer in the cache format
type CachedPeer struct {
	ID         string            `json:"node_id"`
	Multiaddrs []string          `json:"multiaddrs"`
	DataCenter string            `json:"datacenter"`
	Labels     map[string]string `json:"labels"`
	Status     string            `json:"status"`
	LastSeen   time.Time         `json:"last_seen"`
	TrustScore float64           `json:"trust_score"`
}

// NewPhonebookCache creates a new phonebook cache instance
func NewPhonebookCache(filePath string) *PhonebookCache {
	return &PhonebookCache{
		filePath: filePath,
	}
}

// Save persists the phonebook to disk with atomic writes
func (pc *PhonebookCache) Save(phonebook *Phonebook) error {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	// Ensure directory exists
	dir := filepath.Dir(pc.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Convert phonebook to cacheable format
	cacheData := pc.convertToCache(phonebook)

	// Use atomic write with temp file
	tempFile := pc.filePath + ".tmp"
	
	// Write to temp file
	file, err := os.Create(tempFile)
	if err != nil {
		return fmt.Errorf("failed to create temp cache file: %w", err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(cacheData); err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("failed to encode phonebook cache: %w", err)
	}

	if err := file.Sync(); err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("failed to sync cache file: %w", err)
	}

	file.Close()

	// Atomic rename
	if err := os.Rename(tempFile, pc.filePath); err != nil {
		os.Remove(tempFile)
		return fmt.Errorf("failed to rename cache file: %w", err)
	}

	log.Printf("Phonebook cache saved to %s with %d data centers", 
		pc.filePath, len(cacheData.DataCenters))
	
	return nil
}

// Load reads the phonebook from disk
func (pc *PhonebookCache) Load() (*Phonebook, error) {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	// Check if file exists
	if _, err := os.Stat(pc.filePath); os.IsNotExist(err) {
		log.Printf("No cached phonebook found at %s", pc.filePath)
		return NewPhonebook(), nil
	}

	// Read cache file
	data, err := os.ReadFile(pc.filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read cache file: %w", err)
	}

	// Parse JSON
	var cacheData CachedPhonebookData
	if err := json.Unmarshal(data, &cacheData); err != nil {
		// If parsing fails, return empty phonebook and log warning
		log.Printf("Warning: failed to parse cached phonebook, starting fresh: %v", err)
		return NewPhonebook(), nil
	}

	// Validate cache version
	if cacheData.Version != "1.0" {
		log.Printf("Warning: unsupported cache version %s, starting fresh", cacheData.Version)
		return NewPhonebook(), nil
	}

	// Convert to phonebook
	phonebook, err := pc.convertFromCache(&cacheData)
	if err != nil {
		log.Printf("Warning: failed to convert cached data, starting fresh: %v", err)
		return NewPhonebook(), nil
	}

	totalPeers := 0
	for _, peers := range cacheData.DataCenters {
		totalPeers += len(peers)
	}
	
	log.Printf("Loaded phonebook cache with %d peers across %d data centers", 
		totalPeers, len(cacheData.DataCenters))

	return phonebook, nil
}

// Clear removes the cache file
func (pc *PhonebookCache) Clear() error {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if err := os.Remove(pc.filePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to clear cache: %w", err)
	}

	log.Printf("Phonebook cache cleared: %s", pc.filePath)
	return nil
}

// Exists checks if a cache file exists
func (pc *PhonebookCache) Exists() bool {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	_, err := os.Stat(pc.filePath)
	return err == nil
}

// GetCacheInfo returns information about the cache file
func (pc *PhonebookCache) GetCacheInfo() (*CachInfo, error) {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	stat, err := os.Stat(pc.filePath)
	if err != nil {
		return nil, err
	}

	return &CachInfo{
		Path:     pc.filePath,
		Size:     stat.Size(),
		ModTime:  stat.ModTime(),
		Mode:     stat.Mode(),
	}, nil
}

// CachInfo provides information about the cache file
type CachInfo struct {
	Path    string
	Size    int64
	ModTime time.Time
	Mode    fs.FileMode
}

// convertToCache converts a Phonebook to cacheable format
func (pc *PhonebookCache) convertToCache(phonebook *Phonebook) *CachedPhonebookData {
	cacheData := &CachedPhonebookData{
		Version:     "1.0",
		UpdatedAt:   time.Now(),
		DataCenters: make(map[string][]CachedPeer),
	}

	peers := phonebook.GetPeers()
	
	// Group peers by data center
	for _, peer := range peers {
		dc := peer.DataCenter
		if dc == "" {
			dc = "default"
		}

		// Extract multiaddrs from AddrInfo
		var multiaddrs []string
		for _, addr := range peer.AddrInfo.Addrs {
			multiaddrs = append(multiaddrs, addr.String())
		}

		// Convert tags to string map
		labels := make(map[string]string)
		for k, v := range peer.Tags {
			if str, ok := v.(string); ok {
				labels[k] = str
			} else {
				labels[k] = fmt.Sprintf("%v", v)
			}
		}

		cachedPeer := CachedPeer{
			ID:         peer.ID,
			Multiaddrs: multiaddrs,
			DataCenter: peer.DataCenter,
			Labels:     labels,
			Status:     peer.Status,
			LastSeen:   peer.LastSeen,
			TrustScore: peer.TrustScore,
		}

		cacheData.DataCenters[dc] = append(cacheData.DataCenters[dc], cachedPeer)
	}

	return cacheData
}

// convertFromCache converts cached data back to a Phonebook
func (pc *PhonebookCache) convertFromCache(cacheData *CachedPhonebookData) (*Phonebook, error) {
	phonebook := NewPhonebook()

	for _, cachedPeers := range cacheData.DataCenters {
		for _, cachedPeer := range cachedPeers {
			// Convert multiaddrs
			var addrs []ma.Multiaddr
			for _, addrStr := range cachedPeer.Multiaddrs {
				addr, err := ma.NewMultiaddr(addrStr)
				if err != nil {
					log.Printf("Warning: invalid multiaddr %s for peer %s: %v", 
						addrStr, cachedPeer.ID, err)
					continue
				}
				addrs = append(addrs, addr)
			}

			// Create peer ID
			peerID, err := peer.Decode(cachedPeer.ID)
			if err != nil {
				log.Printf("Warning: invalid peer ID %s: %v", cachedPeer.ID, err)
				continue
			}

			// Convert labels to Tags
			tags := make(Tags)
			for k, v := range cachedPeer.Labels {
				tags[k] = v
			}

			// Create peer
			peer := Peer{
				ID:         cachedPeer.ID,
				Status:     cachedPeer.Status,
				AddrInfo: peer.AddrInfo{
					ID:    peerID,
					Addrs: addrs,
				},
				Tags:       tags,
				DataCenter: cachedPeer.DataCenter,
				LastSeen:   cachedPeer.LastSeen,
				TrustScore: cachedPeer.TrustScore,
			}

			phonebook.AddPeer(peer)
		}
	}

	return phonebook, nil
}

// SaveAsync saves the phonebook asynchronously
func (pc *PhonebookCache) SaveAsync(phonebook *Phonebook) <-chan error {
	errChan := make(chan error, 1)
	
	go func() {
		defer close(errChan)
		errChan <- pc.Save(phonebook)
	}()
	
	return errChan
}

// DefaultCachePath returns the default cache file path for the current user
func DefaultCachePath() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		// Fallback to current directory
		return ".falak/phonebook.json"
	}
	
	return filepath.Join(homeDir, ".falak", "phonebook.json")
}