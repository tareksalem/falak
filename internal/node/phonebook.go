package node

import (
	"log"
	"sync"
	"time"

	"github.com/tareksalem/falak/internal/node/protobuf/models"
)

type Phonebook struct {
	ID    string
	mu    sync.RWMutex
	peers map[string]Peer
}

func NewPhonebook() *Phonebook {
	return &Phonebook{
		ID:    "phonebook",
		peers: make(map[string]Peer),
	}
}

func (p *Phonebook) AddPeer(peer Peer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.peers[peer.ID] = peer
}

func (p *Phonebook) GetPeers() []Peer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	peers := make([]Peer, 0, len(p.peers))
	for _, peer := range p.peers {
		peers = append(peers, peer)
	}
	return peers
}

func (p *Phonebook) GetPeer(id string) (Peer, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	peer, exists := p.peers[id]
	return peer, exists
}

func (p *Phonebook) RemovePeer(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.peers, id)
}

func (p *Phonebook) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.peers = make(map[string]Peer)
}

// UpdateFromDelta updates the phonebook with data from a PhonebookDelta
func (p *Phonebook) UpdateFromDelta(delta *models.PhonebookDelta) {
	if delta == nil {
		return
	}
	
	p.mu.Lock()
	defer p.mu.Unlock()
	
	for _, dcUpdate := range delta.DataCenters {
		p.mergeDataCenterUpdateLocked(dcUpdate)
	}
	
	log.Printf("Updated phonebook from delta: %d data centers", len(delta.DataCenters))
}

// GetPeersByDataCenter returns all peers in a specific data center
func (p *Phonebook) GetPeersByDataCenter(dc string) []Peer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	
	var dcPeers []Peer
	for _, peer := range p.peers {
		if peer.DataCenter == dc {
			dcPeers = append(dcPeers, peer)
		}
	}
	
	return dcPeers
}

// MergeDataCenterUpdate merges a data center update into the phonebook
func (p *Phonebook) MergeDataCenterUpdate(dcUpdate *models.DataCenterUpdate) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mergeDataCenterUpdateLocked(dcUpdate)
}

// mergeDataCenterUpdateLocked performs the actual merge operation (must hold lock)
func (p *Phonebook) mergeDataCenterUpdateLocked(dcUpdate *models.DataCenterUpdate) {
	if dcUpdate == nil {
		return
	}
	
	for _, peerInfo := range dcUpdate.Peers {
		// Convert protobuf PeerInfo to internal Peer struct
		peer := Peer{
			ID:         peerInfo.NodeId,
			Status:     p.convertNodeStatus(peerInfo.NodeStatus),
			DataCenter: dcUpdate.DcId,
			LastSeen:   time.Now(),
			TrustScore: 1.0, // Default trust score
			Tags:       make(Tags),
		}
		
		// Copy labels to tags
		for k, v := range peerInfo.Labels {
			peer.Tags[k] = v
		}
		
		// Only update if this is authoritative or we don't have the peer
		if dcUpdate.IsAuthoritative {
			p.peers[peer.ID] = peer
		} else if _, exists := p.peers[peer.ID]; !exists {
			p.peers[peer.ID] = peer
		}
	}
	
	log.Printf("Merged data center update for DC %s: %d peers", dcUpdate.DcId, len(dcUpdate.Peers))
}

// convertNodeStatus converts protobuf NodeStatus to string
func (p *Phonebook) convertNodeStatus(status models.NodeStatus) string {
	switch status {
	case models.NodeStatus_ACTIVE:
		return "active"
	case models.NodeStatus_JOINING:
		return "joining"
	case models.NodeStatus_LEAVING:
		return "leaving"
	case models.NodeStatus_UNREACHABLE:
		return "unreachable"
	case models.NodeStatus_AUTHENTICATED:
		return "authenticated"
	default:
		return "unknown"
	}
}

// GetDataCenters returns a list of all data centers known to this phonebook
func (p *Phonebook) GetDataCenters() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	
	dcSet := make(map[string]bool)
	for _, peer := range p.peers {
		if peer.DataCenter != "" {
			dcSet[peer.DataCenter] = true
		}
	}
	
	var datacenters []string
	for dc := range dcSet {
		datacenters = append(datacenters, dc)
	}
	
	return datacenters
}

// UpdatePeerLastSeen updates the last seen timestamp for a peer
func (p *Phonebook) UpdatePeerLastSeen(peerID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	
	if peer, exists := p.peers[peerID]; exists {
		peer.LastSeen = time.Now()
		p.peers[peerID] = peer
	}
}

// UpdatePeerTrustScore updates the trust score for a peer
func (p *Phonebook) UpdatePeerTrustScore(peerID string, score float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	
	if peer, exists := p.peers[peerID]; exists {
		peer.TrustScore = score
		p.peers[peerID] = peer
	}
}

// GetStaleePeers returns peers that haven't been seen for a specified duration
func (p *Phonebook) GetStalePeers(staleDuration time.Duration) []Peer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	
	threshold := time.Now().Add(-staleDuration)
	var stalePeers []Peer
	
	for _, peer := range p.peers {
		if peer.LastSeen.Before(threshold) {
			stalePeers = append(stalePeers, peer)
		}
	}
	
	return stalePeers
}

// GeneratePhonebookDelta creates a PhonebookDelta with current peer information
func (p *Phonebook) GeneratePhonebookDelta() *models.PhonebookDelta {
	p.mu.RLock()
	defer p.mu.RUnlock()
	
	// Group peers by data center
	dcPeers := make(map[string][]*models.PeerInfo)
	
	for _, peer := range p.peers {
		dc := peer.DataCenter
		if dc == "" {
			dc = "default"
		}
		
		peerInfo := &models.PeerInfo{
			NodeId:      peer.ID,
			Multiaddrs:  []string{}, // TODO: Extract from AddrInfo
			Labels:      make(map[string]string),
			NodeStatus:  p.convertStringToNodeStatus(peer.Status),
		}
		
		// Copy tags to labels
		for k, v := range peer.Tags {
			if strVal, ok := v.(string); ok {
				peerInfo.Labels[k] = strVal
			}
		}
		
		dcPeers[dc] = append(dcPeers[dc], peerInfo)
	}
	
	// Create data center updates
	var dcUpdates []*models.DataCenterUpdate
	for dc, peers := range dcPeers {
		dcUpdate := &models.DataCenterUpdate{
			DcId:            dc,
			Peers:           peers,
			IsAuthoritative: true, // This node is authoritative for its own data
		}
		dcUpdates = append(dcUpdates, dcUpdate)
	}
	
	return &models.PhonebookDelta{
		DataCenters: dcUpdates,
	}
}

// convertStringToNodeStatus converts string status to protobuf NodeStatus
func (p *Phonebook) convertStringToNodeStatus(status string) models.NodeStatus {
	switch status {
	case "active":
		return models.NodeStatus_ACTIVE
	case "joining":
		return models.NodeStatus_JOINING
	case "leaving":
		return models.NodeStatus_LEAVING
	case "unreachable":
		return models.NodeStatus_UNREACHABLE
	case "authenticated":
		return models.NodeStatus_AUTHENTICATED
	default:
		return models.NodeStatus_UNKNOWN
	}
}
