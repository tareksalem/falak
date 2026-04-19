package node

import (
	"context"
	"fmt"
	"log"
	"maps"
	mrand "math/rand"
	"os"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	tls "github.com/libp2p/go-libp2p/p2p/security/tls"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	ma "github.com/multiformats/go-multiaddr"

	authentication "github.com/tareksalem/falak/internal/node/authentication"
	enums "github.com/tareksalem/falak/internal/node/enums"
	"github.com/thoas/go-funk"
)

type Tags map[string]interface{}

type NodeOption struct {
	id       string
	cb       func(*Node) interface{}
	runType  string
	isP2pArg bool
}

type Node struct {
	clusters        []string
	name            string
	id              string
	addr            ma.Multiaddr
	port            uint
	tags            Tags
	status          enums.NodeStatus
	phonebook       *Phonebook
	host            host.Host
	pubSub          *pubsub.PubSub
	connectedTopics map[string]*ConnectedTopic
	privateKey      crypto.PrivKey
	publicKey       crypto.PubKey
	rootCAPath      string // Path to the shared Root CA certificate
	ctx             context.Context
	mu              sync.RWMutex
	// topics          sync.Map
	authManager *authentication.AuthenticationManager
	topics      sync.Map

	// bootManager        *BootManager
	// connManager        *ConnectionManager
	// healthMonitor      *ConnectionHealthMonitor
	// optimizer          *ConnectionOptimizer
	dataCenter  string   // Primary data center (for backward compatibility)
	dataCenters []string // List of all data centers this node belongs to
	// connectionStrategy string
	// TLS authentication component
	// tlsAuthenticator *TLSAuthenticator

	// Reactor pattern components (primary system)
	// protocolRegistry    *ProtocolHandlerRegistry
	// reactiveAuthHandler *ReactiveAuthHandler
	// nodeStateMachine    *NodeStateMachineV2
	// peerStateMachine    *PeerStateMachine

	// Reactor integrations (replace legacy managers)
	// reactor               *ReactorIntegrationExample
	// membershipIntegration *MembershipReactorIntegration
	// lifecycleStateMachine *NodeLifecycleStateMachine

	// Complete event-driven system components
	// heartbeatPublisher  *HeartbeatPublisher
	// heartbeatListener   *HeartbeatListener
	// swimProber          *SWIMProber
	// suspicionHandler    *SuspicionHandler
	// selfRecoveryHandler *SelfRecoveryHandler

	// Node incarnation for failure detection
	incarnation uint64
}

type PeerInput struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
	Tags Tags   `json:"tags"`
}

func WithTags(tags Tags) NodeOption {
	return NodeOption{
		id: "tags",
		cb: func(n *Node) interface{} {
			n.tags = tags
			return nil
		},
	}
}

func generateDeterministicKey(seed string) (crypto.PrivKey, crypto.PubKey, error) {
	// Create a deterministic private key from the seed
	r := mrand.New(mrand.NewSource(int64(hash(seed))))
	return crypto.GenerateKeyPairWithReader(crypto.Ed25519, -1, r)
}

func WithDeterministicID(seed string) NodeOption {
	return NodeOption{
		id: "deterministicID",
		cb: func(n *Node) interface{} {
			prKey, pubKey, err := generateDeterministicKey(seed)
			if err != nil {
				log.Printf("Failed to generate deterministic key: %v", err)
				return nil
			}
			n.privateKey = prKey
			n.publicKey = pubKey
			return libp2p.Identity(prKey)
		},
		runType:  "preInitialize",
		isP2pArg: true,
	}
}

func WithName(name string) NodeOption {
	return NodeOption{
		id: "name",
		cb: func(n *Node) interface{} {
			n.name = name
			return nil
		},
	}
}

func WithClusters(clusters []string) NodeOption {
	return NodeOption{
		id: "clusters",
		cb: func(n *Node) interface{} {
			n.clusters = funk.UniqString(clusters)
			if len(n.clusters) == 0 {
				log.Println("No clusters specified, using default cluster 'default'")
				n.clusters = []string{"default"}
			}
			return nil
		},
		runType: "preInitialize",
	}
}

func WithPort(port uint) NodeOption {
	return NodeOption{
		id: "port",
		cb: func(n *Node) interface{} {
			n.port = port
			return nil
		},
		runType: "preInitialize",
	}
}

func WithAddress(address string) NodeOption {
	return NodeOption{
		id: "address",
		cb: func(n *Node) interface{} {
			addr, err := ma.NewMultiaddr(address)
			if err != nil {
				log.Printf("Invalid address %s: %v", address, err)
				return nil
			}
			n.addr = addr
			return libp2p.ListenAddrStrings(address)
		},
		runType:  "preInitialize",
		isP2pArg: true,
	}
}

func WithExistingPubSub(ps *pubsub.PubSub) NodeOption {
	return NodeOption{
		id: "existingPubSub",
		cb: func(n *Node) interface{} {
			n.pubSub = ps
			return nil
		},
	}
}

// WithRootCA configures the node to use a specific root CA certificate for authentication
// This is the shared Root CA for the entire cluster
func WithRootCA(caPath string) NodeOption {
	return NodeOption{
		id: "rootCA",
		cb: func(n *Node) interface{} {
			// Store the CA path for later use during certificate initialization
			n.rootCAPath = caPath
			log.Printf("🔐 Root CA path configured: %s", caPath)
			return nil
		},
		runType: "preInitialize",
	}
}

// WithNodeCert configures the node to use a specific node certificate for authentication
func WithNodeCert(certPath string) NodeOption {
	return NodeOption{
		id: "nodeCert",
		cb: func(n *Node) interface{} {
			// Will be initialized later when private key is available
			return nil
		},
		runType: "postInitialize",
	}
}

// WithClusterAuth configures the node for cluster-based authentication
func WithClusterAuth(clusterId string) NodeOption {
	return NodeOption{
		id: "clusterAuth",
		cb: func(n *Node) interface{} {
			if clusterId != "" {
				n.clusters = []string{clusterId}
			}
			return nil
		},
		runType: "preInitialize",
	}
}

// WithCertificates is deprecated - certificates are now managed by AuthManager
// This function is kept for compatibility but does nothing
func WithCertificates(rootCAPath, nodeCertPath string) NodeOption {
	return NodeOption{
		id: "certificates_deprecated",
		cb: func(n *Node) interface{} {
			log.Printf("⚠️ WithCertificates is deprecated - certificates are now managed by AuthManager")
			return nil
		},
		runType: "postInitialize",
	}
}

// WithTLSAuth configures TLS mutual authentication
func WithTLSAuth(rootCAPath, clientCertPath, clientKeyPath string) NodeOption {
	return NodeOption{
		id: "tlsAuth",
		cb: func(n *Node) interface{} {
			return nil
		},
		runType: "postInitialize",
	}
}

// WithPhonebookCache configures phonebook caching
func WithPhonebookCache(cacheDir string) NodeOption {
	return NodeOption{
		id: "phonebookCache",
		cb: func(n *Node) interface{} {
			// Cache configuration will be handled by boot manager
			return nil
		},
		runType: "preInitialize",
	}
}

// WithSeedData configures seed data paths
func WithSeedData(seedPaths []string) NodeOption {
	return NodeOption{
		id: "seedData",
		cb: func(n *Node) interface{} {
			// Seed paths will be handled by boot manager
			return nil
		},
		runType: "preInitialize",
	}
}

// WithDataCenters configures multiple data centers for the node
func WithDataCenters(dcs []string) NodeOption {
	return NodeOption{
		id: "dataCenters",
		cb: func(n *Node) interface{} {
			n.dataCenters = dcs
			// Use first DC as primary
			if len(dcs) > 0 {
				n.dataCenter = dcs[0]
			}
			log.Printf("🌍 Node configured for data centers: %v (primary: %s)", dcs, n.dataCenter)
			return nil
		},
		runType: "preInitialize",
	}
}

func NewNode(ctx context.Context, opts ...NodeOption) (*Node, error) {
	clusterOption := funk.Find(opts, func(o NodeOption) bool {
		return o.id == "clusters"
	})
	if clusterOption == nil {
		log.Println("No clusters specified, using default cluster 'default'")
		opts = append(opts, WithClusters([]string{"default"}))
	}
	preInitOpts := funk.Filter(opts, func(o NodeOption) bool {
		return o.runType == "preInitialize"
	}).([]NodeOption)
	afterInitOpts := funk.Filter(opts, func(o NodeOption) bool {
		return o.runType != "preInitialize"
	}).([]NodeOption)
	preInitOptsWithReturn := funk.Filter(preInitOpts, func(o NodeOption) bool {
		return o.cb != nil && o.isP2pArg
	}).([]NodeOption)

	node := &Node{
		ctx:       ctx,
		status:    enums.NodeStatusEnum.Initializing,
		phonebook: NewPhonebook(),
		// connectedTopics: make(map[string]*ConnectedTopic),
	}

	// Apply ALL preInit options to set node properties
	for _, opt := range preInitOpts {
		if opt.cb != nil && !opt.isP2pArg {
			opt.cb(node)
		}
	}

	p2pArgs := funk.Map(preInitOptsWithReturn, func(o NodeOption) libp2p.Option {
		return o.cb(node).(libp2p.Option)
	}).([]libp2p.Option)

	// Add TLS security transport and TCP transport to the configuration
	p2pArgs = append(p2pArgs,
		libp2p.Security(tls.ID, tls.New),
		libp2p.Transport(tcp.NewTCPTransport),
	)

	h, err := libp2p.New(p2pArgs...)
	if err != nil {
		return nil, err
	}
	node.host = h
	node.id = h.ID().String()
	node.incarnation = 1                            // Initialize incarnation for failure detection
	node.privateKey = h.Peerstore().PrivKey(h.ID()) // Extract private key for certificate generation

	// Use Root CA path from node configuration (set via WithRootCA option)
	// Fall back to environment variable if not set via option
	rootCAPath := node.rootCAPath
	if rootCAPath == "" {
		rootCAPath = os.Getenv("FALAK_ROOT_CA")
	}

	// Store root CA path for authentication manager to use
	node.rootCAPath = rootCAPath

	// Initialize Authentication Manager
	if err := node.initializeAuthenticationManager(); err != nil {
		log.Printf("⚠️ Failed to initialize authentication manager: %v", err)
		log.Printf("🔄 Node will continue without authentication")
	}

	for i, opt := range afterInitOpts {
		log.Printf("🔧 Applying option %d: %s", i, opt.id)
		opt.cb(node)
	}
	return node, nil
}

// initializeAuthenticationManager sets up and starts the authentication manager
func (n *Node) initializeAuthenticationManager() error {
	// Create authentication config
	authConfig := authentication.DefaultConfig()
	authConfig.NodeID = n.id
	authConfig.Clusters = n.clusters
	authConfig.DataCenters = n.dataCenters
	authConfig.RootCAPath = n.rootCAPath
	authConfig.Host = n.host
	authConfig.EnableRealCrypto = true // Default to real crypto mode

	// Set default primary cluster and datacenter if none provided
	if len(authConfig.Clusters) == 0 {
		authConfig.Clusters = []string{"default"}
	}
	if len(authConfig.DataCenters) == 0 {
		authConfig.DataCenters = []string{"dc1"}
	}

	// Create authentication manager
	authManager := authentication.NewAuthenticationManager(
		authentication.WithConfig(authConfig),
		authentication.WithHost(n.host),
	)

	// Start the authentication manager
	if err := authManager.Start(); err != nil {
		return fmt.Errorf("failed to start authentication manager: %w", err)
	}

	n.authManager = authManager
	log.Printf("✅ Authentication manager initialized successfully")
	return nil
}

func (n *Node) JoinTopic(topicName string) (*ConnectedTopic, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if ct, exists := n.connectedTopics[topicName]; exists {
		return ct, nil
	}

	topic, err := n.pubSub.Join(topicName)
	if err != nil {
		return nil, err
	}
	sub, err := topic.Subscribe()
	if err != nil {
		_ = topic.Close()
		return nil, err
	}

	ct := &ConnectedTopic{
		Name:        topicName,
		Topic:       topic,
		Sub:         sub,
		node:        n,
		localPeerID: n.host.ID(),
	}

	// CRITICAL DEBUG: Check topic peers immediately after join
	initialPeers := topic.ListPeers()
	log.Printf("🔍 CRITICAL: %s joined topic %s - initial peers: %d", n.name, topicName, len(initialPeers))
	for i, peerID := range initialPeers {
		log.Printf("   [%d] Initial peer: %s", i, peerID.ShortString())
	}

	// Store connected topic before starting listener
	n.connectedTopics[topicName] = ct

	// CRITICAL DEBUG: Monitor topic peer changes periodically
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-n.ctx.Done():
				return
			case <-ticker.C:
				currentPeers := topic.ListPeers()
				if len(currentPeers) > 0 {
					log.Printf("📡 CRITICAL: %s topic %s now has %d peers", n.name, topicName, len(currentPeers))
					for i, peerID := range currentPeers {
						log.Printf("   [%d] Topic peer: %s", i, peerID.ShortString())
					}
				}
			}
		}
	}()

	// Start listener for this topic - delegate all message processing to onMessage
	go func(name string, s *pubsub.Subscription, cb *ConnectedTopic) {
		log.Printf("Listening on topic: %s", name)
		for {
			msg, err := s.Next(n.ctx)
			if err != nil {
				log.Printf("Subscription closed for topic %s: %v", name, err)
				return
			}

			// Delegate all message processing to onMessage handler
			cb.onMessage(msg)
		}
	}(topicName, sub, ct)

	return ct, nil
}

func (n *Node) LeaveTopic(topicName string) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	ct, exists := n.connectedTopics[topicName]
	if !exists {
		return nil
	}
	defer ct.Sub.Cancel() // stop subscription
	if err := ct.Topic.Close(); err != nil {
		return err
	}
	delete(n.connectedTopics, topicName)
	return nil
}

func (n *Node) GetConnectedTopics() map[string]*ConnectedTopic {
	n.mu.RLock()
	defer n.mu.RUnlock()
	// shallow copy to avoid races
	out := make(map[string]*ConnectedTopic, len(n.connectedTopics))
	maps.Copy(out, n.connectedTopics)
	return out
}

// rejoinAllTopics leaves and rejoins all connected topics to fix FloodSub mesh formation after peer connections
func (n *Node) RejoinAllTopics() {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Get current topic names
	var topicNames []string
	for topicName := range n.connectedTopics {
		topicNames = append(topicNames, topicName)
	}

	log.Printf("🔄 Rejoining %d topics to refresh FloodSub mesh: %v", len(topicNames), topicNames)

	// Leave all topics first
	for _, topicName := range topicNames {
		if ct, exists := n.connectedTopics[topicName]; exists {
			ct.Sub.Cancel()  // stop subscription
			ct.Topic.Close() // close topic
			delete(n.connectedTopics, topicName)
			log.Printf("🚪 Left topic: %s", topicName)
		}
	}

	// Small delay to allow cleanup
	time.Sleep(100 * time.Millisecond)

	// Rejoin all topics
	for _, topicName := range topicNames {
		log.Printf("🔗 Rejoining topic: %s", topicName)
		if _, err := n.JoinTopic(topicName); err != nil {
			log.Printf("❌ Failed to rejoin topic %s: %v", topicName, err)
		} else {
			log.Printf("✅ Successfully rejoined topic: %s", topicName)
		}
	}

	log.Printf("🎯 Completed topic rejoin process")
}

func (n *Node) GetQualifiedID() string {
	return fmt.Sprintf("%s/p2p/%s", n.addr, n.id)
}

func (n *Node) GetClusters() []string {
	return n.clusters
}

// GetDataCenters returns the data centers this node belongs to
func (n *Node) GetDataCenters() []string {
	return n.dataCenters
}

// GetHost returns the libp2p host instance
func (n *Node) GetHost() host.Host {
	return n.host
}

// InitializeAuthenticationManager initializes the authentication manager with configuration
func (n *Node) InitializeAuthenticationManager() error {
	return nil
}

func (n *Node) PublishToTopic(topicName string, data []byte) error {
	if n.pubSub == nil {
		return fmt.Errorf("pubsub not initialized")
	}

	// Get or create topic with proper synchronization to prevent race conditions
	topic, err := n.getOrCreateTopic(topicName)
	if err != nil {
		return fmt.Errorf("failed to get or create topic %s: %w", topicName, err)
	}

	// Publish the message
	err = topic.Publish(n.ctx, data)
	if err != nil {
		return fmt.Errorf("failed to publish to topic %s: %w", topicName, err)
	}

	return nil
}

func (n *Node) getOrCreateTopic(topicName string) (*pubsub.Topic, error) {
	// First, try to load the existing topic from publish cache
	if t, ok := n.topics.Load(topicName); ok {
		return t.(*pubsub.Topic), nil
	}

	// Use a mutex to prevent race conditions during topic access
	n.mu.Lock()
	defer n.mu.Unlock()

	// Double-check pattern: check again inside the lock
	if t, ok := n.topics.Load(topicName); ok {
		return t.(*pubsub.Topic), nil
	}

	// Check if we already have this topic in connected topics (from JoinTopic)
	if ct, exists := n.connectedTopics[topicName]; exists {
		// Reuse the existing topic from connected topics
		n.topics.Store(topicName, ct.Topic)
		log.Printf("📡 Reusing existing joined topic for publishing: %s", topicName)
		return ct.Topic, nil
	}

	// Topic doesn't exist in either location, create it
	topic, err := n.pubSub.Join(topicName)
	if err != nil {
		return nil, fmt.Errorf("failed to join topic: %w", err)
	}

	// Store for future use
	n.topics.Store(topicName, topic)
	log.Printf("📡 Created and joined topic for publishing: %s", topicName)

	return topic, nil
}

func (n *Node) LogTopicStatus() {
	log.Printf("🔍 Active topics for node %s:", n.name)
	if len(n.connectedTopics) == 0 {
		log.Printf("  No topics joined yet")
	} else {
		for topicName := range n.connectedTopics {
			log.Printf("  ✅ Subscribed to: %s", topicName)
		}
	}
}

// Authentication Manager Integration Methods

// AuthenticatePeer initiates authentication with a specific peer ID
func (n *Node) AuthenticatePeer(peerID string) error {
	if n.authManager == nil {
		return fmt.Errorf("authentication manager not initialized")
	}

	// Parse the peer ID string into peer.ID
	pid, err := peer.Decode(peerID)
	if err != nil {
		return fmt.Errorf("invalid peer ID format: %w", err)
	}

	// Attempt connection and authentication
	err = n.connectAndAuthenticate(pid)
	if err != nil {
		log.Printf("❌ Failed to connect and authenticate with peer %s: %v", pid.ShortString(), err)

		// Schedule retry through authentication manager
		if retryManager := n.authManager.GetRetryManager(); retryManager != nil {
			retryFunc := func() error {
				return n.connectAndAuthenticate(pid)
			}
			retryManager.ScheduleRetry(pid, err, retryFunc)
		}
		return err
	}

	return nil
}

// connectAndAuthenticate performs both connection and authentication as a single operation
func (n *Node) connectAndAuthenticate(pid peer.ID) error {
	// Connect to the peer first if not already connected
	if err := n.connectToPeer(pid); err != nil {
		return fmt.Errorf("failed to connect to peer: %w", err)
	}
	fmt.Println("connected to peer successfully")

	// Initiate authentication
	return n.authManager.AuthenticatePeer(pid)
}

// AuthenticateKnownPeers attempts to authenticate all peers in the phonebook
func (n *Node) AuthenticateKnownPeers() error {
	if n.authManager == nil {
		return fmt.Errorf("authentication manager not initialized")
	}

	peers := n.phonebook.GetPeers()
	successCount := 0
	failureCount := 0

	log.Printf("🔐 Starting authentication for %d known peers", len(peers))

	for _, phonebookPeer := range peers {
		// Parse peer ID
		pid, err := peer.Decode(phonebookPeer.ID)
		if err != nil {
			log.Printf("❌ Invalid peer ID in phonebook: %s - %v", phonebookPeer.ID, err)
			failureCount++
			continue
		}

		// Connect to peer
		if err := n.connectToPeer(pid); err != nil {
			log.Printf("❌ Failed to connect to peer %s: %v", phonebookPeer.ID, err)
			failureCount++
			continue
		}

		// Authenticate peer
		if err := n.authManager.AuthenticatePeer(pid); err != nil {
			log.Printf("❌ Failed to authenticate peer %s: %v", phonebookPeer.ID, err)
			failureCount++
			continue
		}

		log.Printf("✅ Authentication initiated for peer: %s", phonebookPeer.ID)
		successCount++
	}

	log.Printf("🔐 Authentication summary: %d success, %d failures", successCount, failureCount)
	return nil
}

// connectToPeer establishes a connection to a peer using phonebook info
func (n *Node) connectToPeer(peerID peer.ID) error {
	// Check if already connected
	if n.host.Network().Connectedness(peerID) == 1 { // Connected
		return nil
	}

	// Get peer info from phonebook
	phonebookPeer, exists := n.phonebook.GetPeer(peerID.String())
	if !exists {
		return fmt.Errorf("peer %s not found in phonebook", peerID)
	}

	// Connect using the address info from phonebook
	if err := n.host.Connect(n.ctx, phonebookPeer.AddrInfo); err != nil {
		return fmt.Errorf("failed to connect using phonebook info: %w", err)
	}

	return nil
}

// IsAuthenticated checks if a peer is authenticated
func (n *Node) IsAuthenticated(peerID string) bool {
	if n.authManager == nil {
		return false
	}

	pid, err := peer.Decode(peerID)
	if err != nil {
		return false
	}

	return n.authManager.IsAuthenticated(pid)
}

// GetAuthenticatedPeers returns all currently authenticated peers
func (n *Node) GetAuthenticatedPeers() []string {
	if n.authManager == nil {
		return []string{}
	}

	peers := n.authManager.GetAuthenticatedPeers()
	result := make([]string, len(peers))
	for i, peer := range peers {
		result[i] = peer.String()
	}
	return result
}

// ID returns the node's peer ID as a string
func (n *Node) ID() string {
	return n.id
}

// GetNetworkDiagnostics returns basic network diagnostics
func (n *Node) GetNetworkDiagnostics() map[string]interface{} {
	connectedPeers := n.host.Network().Peers()
	phonebookPeers := n.phonebook.GetPeers()

	var pubsubTopics []string
	for topicName := range n.connectedTopics {
		pubsubTopics = append(pubsubTopics, topicName)
	}

	var peerDetails []map[string]interface{}
	for _, peer := range connectedPeers {
		peerDetails = append(peerDetails, map[string]interface{}{
			"peer_id": peer.String(),
		})
	}

	return map[string]interface{}{
		"connected_peers":   len(connectedPeers),
		"phonebook_entries": len(phonebookPeers),
		"pubsub_topics":     pubsubTopics,
		"peer_details":      peerDetails,
	}
}

// WithPeers adds peers to phonebook and initiates connections
func WithPeers(peers []PeerInput) NodeOption {
	return NodeOption{
		id: "peers",
		cb: func(n *Node) interface{} {
			for _, p := range peers {
				// Parse the peer address to extract peer ID and multiaddr
				addr, err := ma.NewMultiaddr(p.Addr)
				if err != nil {
					log.Printf("❌ Invalid peer address: %s - %v", p.Addr, err)
					continue
				}

				// Extract peer ID from the multiaddr
				peerIDStr, err := addr.ValueForProtocol(ma.P_P2P)
				if err != nil {
					log.Printf("❌ No peer ID in address: %s", p.Addr)
					continue
				}

				// Parse peer ID
				peerID, err := peer.Decode(peerIDStr)
				if err != nil {
					log.Printf("❌ Invalid peer ID: %s - %v", peerIDStr, err)
					continue
				}

				// Create peer entry for phonebook
				peerEntry := Peer{
					ID:       peerID.String(),
					Status:   "pending",
					AddrInfo: peer.AddrInfo{ID: peerID, Addrs: []ma.Multiaddr{addr}},
					Tags:     p.Tags,
				}

				// Add to phonebook
				n.phonebook.AddPeer(peerEntry)
				log.Printf("📞 Added peer to phonebook: %s", peerID.ShortString())

				// Use AuthenticatePeer which handles both connection and authentication
				go func(peerIDStr string) {
					if err := n.AuthenticatePeer(peerIDStr); err != nil {
						log.Printf("❌ Failed to authenticate peer %s: %v", peerIDStr, err)
					} else {
						log.Printf("✅ Authentication initiated for peer: %s", peerIDStr)
					}
				}(peerID.String())
			}
			return nil
		},
	}
}
