package tests

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tareksalem/falak/internal/node"
)

func TestPhonebook_AddPeer(t *testing.T) {
	phonebook := node.NewPhonebook()
	peer := CreateTestPeer("test-peer", "us-west-1")
	
	phonebook.AddPeer(peer)
	
	retrievedPeer, exists := phonebook.GetPeer(peer.ID)
	assert.True(t, exists)
	assert.Equal(t, peer.ID, retrievedPeer.ID)
	assert.Equal(t, peer.Status, retrievedPeer.Status)
	assert.Equal(t, peer.DataCenter, retrievedPeer.DataCenter)
}

func TestPhonebook_GetPeers(t *testing.T) {
	phonebook := node.NewPhonebook()
	
	// Add multiple peers
	peers := []node.Peer{
		CreateTestPeer("peer-1", "us-west-1"),
		CreateTestPeer("peer-2", "us-east-1"),
		CreateTestPeer("peer-3", "eu-west-1"),
	}
	
	for _, peer := range peers {
		phonebook.AddPeer(peer)
	}
	
	allPeers := phonebook.GetPeers()
	assert.Len(t, allPeers, 3)
	
	// Check that all peers are present
	peerIDs := make(map[string]bool)
	for _, peer := range allPeers {
		peerIDs[peer.ID] = true
	}
	
	for _, originalPeer := range peers {
		assert.True(t, peerIDs[originalPeer.ID], "Peer %s should be in phonebook", originalPeer.ID)
	}
}

func TestPhonebook_RemovePeer(t *testing.T) {
	phonebook := node.NewPhonebook()
	peer := CreateTestPeer("test-peer", "us-west-1")
	
	// Add and then remove peer
	phonebook.AddPeer(peer)
	assert.Len(t, phonebook.GetPeers(), 1)
	
	phonebook.RemovePeer(peer.ID)
	assert.Len(t, phonebook.GetPeers(), 0)
	
	_, exists := phonebook.GetPeer(peer.ID)
	assert.False(t, exists)
}

func TestPhonebook_Clear(t *testing.T) {
	phonebook := node.NewPhonebook()
	
	// Add multiple peers
	for i := 0; i < 5; i++ {
		peer := CreateTestPeer(fmt.Sprintf("peer-%d", i), "us-west-1")
		phonebook.AddPeer(peer)
	}
	
	assert.Len(t, phonebook.GetPeers(), 5)
	
	phonebook.Clear()
	assert.Len(t, phonebook.GetPeers(), 0)
}

func TestPhonebook_GetPeersByDataCenter(t *testing.T) {
	phonebook := node.NewPhonebook()
	
	// Add peers from different data centers
	usWestPeers := []node.Peer{
		CreateTestPeer("us-west-1", "us-west-1"),
		CreateTestPeer("us-west-2", "us-west-1"),
	}
	
	usEastPeers := []node.Peer{
		CreateTestPeer("us-east-1", "us-east-1"),
	}
	
	for _, peer := range append(usWestPeers, usEastPeers...) {
		phonebook.AddPeer(peer)
	}
	
	// Test filtering by data center
	westPeers := phonebook.GetPeersByDataCenter("us-west-1")
	assert.Len(t, westPeers, 2)
	
	eastPeers := phonebook.GetPeersByDataCenter("us-east-1")
	assert.Len(t, eastPeers, 1)
	
	nonExistentPeers := phonebook.GetPeersByDataCenter("non-existent")
	assert.Len(t, nonExistentPeers, 0)
}

func TestPhonebook_GetDataCenters(t *testing.T) {
	phonebook := node.NewPhonebook()
	
	expectedDCs := []string{"us-west-1", "us-east-1", "eu-west-1"}
	
	// Add peers from different data centers
	for i, dc := range expectedDCs {
		peer := CreateTestPeer(fmt.Sprintf("peer-%d", i), dc)
		phonebook.AddPeer(peer)
	}
	
	dataCenters := phonebook.GetDataCenters()
	assert.Len(t, dataCenters, len(expectedDCs))
	
	// Check that all expected data centers are present
	dcSet := make(map[string]bool)
	for _, dc := range dataCenters {
		dcSet[dc] = true
	}
	
	for _, expectedDC := range expectedDCs {
		assert.True(t, dcSet[expectedDC], "Data center %s should be present", expectedDC)
	}
}

func TestPhonebook_UpdatePeerLastSeen(t *testing.T) {
	phonebook := node.NewPhonebook()
	peer := CreateTestPeer("test-peer", "us-west-1")
	
	originalTime := time.Now().Add(-1 * time.Hour)
	peer.LastSeen = originalTime
	phonebook.AddPeer(peer)
	
	// Update last seen
	phonebook.UpdatePeerLastSeen(peer.ID)
	
	updatedPeer, exists := phonebook.GetPeer(peer.ID)
	require.True(t, exists)
	assert.True(t, updatedPeer.LastSeen.After(originalTime))
}

func TestPhonebook_UpdatePeerTrustScore(t *testing.T) {
	phonebook := node.NewPhonebook()
	peer := CreateTestPeer("test-peer", "us-west-1")
	
	originalScore := 1.0
	peer.TrustScore = originalScore
	phonebook.AddPeer(peer)
	
	newScore := 0.75
	phonebook.UpdatePeerTrustScore(peer.ID, newScore)
	
	updatedPeer, exists := phonebook.GetPeer(peer.ID)
	require.True(t, exists)
	assert.Equal(t, newScore, updatedPeer.TrustScore)
}

func TestPhonebook_GetStalePeers(t *testing.T) {
	phonebook := node.NewPhonebook()
	
	now := time.Now()
	staleDuration := 1 * time.Hour
	
	// Create peers with different last seen times
	freshPeer := CreateTestPeer("fresh-peer", "us-west-1")
	freshPeer.LastSeen = now.Add(-30 * time.Minute) // 30 minutes ago
	
	stalePeer := CreateTestPeer("stale-peer", "us-west-1")
	stalePeer.LastSeen = now.Add(-2 * time.Hour) // 2 hours ago
	
	phonebook.AddPeer(freshPeer)
	phonebook.AddPeer(stalePeer)
	
	stalePeers := phonebook.GetStalePeers(staleDuration)
	assert.Len(t, stalePeers, 1)
	assert.Equal(t, stalePeer.ID, stalePeers[0].ID)
}

func TestPhonebook_ConcurrentAccess(t *testing.T) {
	phonebook := node.NewPhonebook()
	numRoutines := 10
	peersPerRoutine := 10
	
	done := make(chan bool, numRoutines)
	
	// Concurrent writers
	for i := 0; i < numRoutines; i++ {
		go func(routineID int) {
			for j := 0; j < peersPerRoutine; j++ {
				peer := CreateTestPeer(fmt.Sprintf("peer-%d-%d", routineID, j), "us-west-1")
				phonebook.AddPeer(peer)
			}
			done <- true
		}(i)
	}
	
	// Concurrent readers
	for i := 0; i < numRoutines; i++ {
		go func() {
			for j := 0; j < peersPerRoutine; j++ {
				phonebook.GetPeers()
				phonebook.GetDataCenters()
			}
			done <- true
		}()
	}
	
	// Wait for all routines to complete
	for i := 0; i < numRoutines*2; i++ {
		<-done
	}
	
	// Verify that all peers were added
	allPeers := phonebook.GetPeers()
	assert.Equal(t, numRoutines*peersPerRoutine, len(allPeers))
}

func TestPhonebook_UpdateNonExistentPeer(t *testing.T) {
	phonebook := node.NewPhonebook()
	
	// Try to update non-existent peer
	phonebook.UpdatePeerLastSeen("non-existent")
	phonebook.UpdatePeerTrustScore("non-existent", 0.5)
	
	// Should not panic and should have no effect
	assert.Len(t, phonebook.GetPeers(), 0)
}