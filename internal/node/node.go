package node

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"maps"
	"os"
	"strings"
	"sync"
	"time"

	mrand "math/rand"

	"github.com/cenkalti/backoff/v5"
	libp2p "github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	tls "github.com/libp2p/go-libp2p/p2p/security/tls"
	tcp "github.com/libp2p/go-libp2p/p2p/transport/tcp"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/thoas/go-funk"
	"google.golang.org/protobuf/proto"

	"github.com/tareksalem/falak/internal/node/protobuf/models"
)

// Global topic definitions
const (
	TopicMembership = "members/membership"
	TopicJoined     = "members/joined"
	TopicEvents     = "events"
	TopicHeartbeat  = "heartbeat"
)

// TopicType defines the type of pubsub implementation for a topic
type TopicType string

const (
	topicTypeFlood  TopicType = "flood"
	topicTypeGossip TopicType = "gossip"
)

var TopicTypeEnum = struct {
	Flood  TopicType
	Gossip TopicType
}{
	Flood:  topicTypeFlood,
	Gossip: topicTypeGossip,
}

// PubSubType defines the type of pubsub to use
type PubSubType string

const (
	pubSubTypeFlood  PubSubType = "flood"
	pubSubTypeGossip PubSubType = "gossip"
)

var PubSubTypeEnum = struct {
	Flood  PubSubType
	Gossip PubSubType
}{
	Flood:  pubSubTypeFlood,
	Gossip: pubSubTypeGossip,
}

// Topic represents a pubsub topic configuration with dynamic path support
type Topic struct {
	Name        string            `json:"name"`
	Type        TopicType         `json:"type"`
	Path        string            `json:"path"`             // URL-like path template (required)
	Params      map[string]string `json:"params,omitempty"` // Dynamic parameters for path substitution
	Description string            `json:"description,omitempty"`
}

// GetFullName returns the full topic name with dynamic parameter substitution
func (t *Topic) GetFullName() string {
	if t.Path == "" {
		return t.Name
	}

	path := t.Path

	// Replace all dynamic parameters in the path
	for key, value := range t.Params {
		placeholder := "{" + key + "}"
		path = strings.ReplaceAll(path, placeholder, value)
	}

	// Replace built-in parameters
	path = strings.ReplaceAll(path, "{name}", t.Name)

	return path
}

// SetParam sets a dynamic parameter for the topic
func (t *Topic) SetParam(key, value string) {
	if t.Params == nil {
		t.Params = make(map[string]string)
	}
	t.Params[key] = value
}

// GetParam gets a dynamic parameter value
func (t *Topic) GetParam(key string) (string, bool) {
	if t.Params == nil {
		return "", false
	}
	value, exists := t.Params[key]
	return value, exists
}

// GetTopicName returns the full topic name for a cluster
func GetTopicName(cluster, topic string) string {
	return fmt.Sprintf("falak/%s/%s", cluster, topic)
}

// Global default topic configurations with flexible URL-like paths
var DefaultTopics = []Topic{
	{
		Name:        TopicMembership,
		Type:        TopicTypeEnum.Gossip,
		Path:        "falak/{cluster}/{name}",
		Description: "Cluster membership announcements",
	},
	{
		Name:        TopicJoined,
		Type:        TopicTypeEnum.Gossip,
		Path:        "falak/{cluster}/{name}",
		Description: "Node join notifications",
	},
	{
		Name:        TopicEvents,
		Type:        TopicTypeEnum.Gossip,
		Path:        "falak/{cluster}/{name}",
		Description: "General cluster events",
	},
	{
		Name:        TopicHeartbeat,
		Type:        TopicTypeEnum.Flood,
		Path:        "falak/{cluster}/{name}",
		Description: "Node health heartbeats",
	},
}

// Examples of different topic path patterns
var ExampleTopics = []Topic{
	{
		Name:        "global-announcements",
		Type:        TopicTypeEnum.Flood,
		Path:        "falak/global/{name}",
		Description: "Global system announcements",
	},
	{
		Name:        "metrics",
		Type:        TopicTypeEnum.Gossip,
		Path:        "{name}",
		Description: "Simple topic without any prefix",
	},
	{
		Name:        "user-data",
		Type:        TopicTypeEnum.Gossip,
		Path:        "api/{version}/users/{userId}/{name}",
		Description: "User-specific data with version and user ID",
	},
	{
		Name:        "region-sync",
		Type:        TopicTypeEnum.Flood,
		Path:        "{datacenter}/{region}/sync/{name}",
		Description: "Regional data synchronization",
	},
}

// GetDefaultTopics returns the default topics for a cluster
func GetDefaultTopics(cluster string) []Topic {
	topics := make([]Topic, len(DefaultTopics))
	for i, topic := range DefaultTopics {
		newTopic := Topic{
			Name:        topic.Name,
			Type:        topic.Type,
			Path:        topic.Path,
			Description: topic.Description,
		}
		// Set the cluster parameter for path substitution
		newTopic.SetParam("cluster", cluster)
		topics[i] = newTopic
	}
	return topics
}

// GetDefaultTopicNames returns just the topic names for backward compatibility
func GetDefaultTopicNames(cluster string) []string {
	topics := GetDefaultTopics(cluster)
	names := make([]string, len(topics))
	for i, topic := range topics {
		names[i] = topic.GetFullName()
	}
	return names
}

type Tags map[string]interface{}

type NodeOption struct {
	id       string
	cb       func(*Node) interface{}
	runType  string
	isP2pArg bool
}

type ConnectedTopic struct {
	Name  string
	Topic *pubsub.Topic
	Sub   *pubsub.Subscription
	// onMessage   func(msg *pubsub.Message)
	localPeerID peer.ID
}

func (ct *ConnectedTopic) onMessage(msg *pubsub.Message) {
	// Check if this is a membership or heartbeat topic and process accordingly
	if ct.isMembershipTopic() {
		log.Printf("📋 Membership event received on topic %s", ct.Name)
	} else if ct.isHeartbeatTopic() {
		// Heartbeat messages are processed in JoinTopic listener
		// Just log reception for debugging (reduce noise)
		if strings.Contains(string(msg.Data), "seq\":10") { // Log every 10th heartbeat
			log.Printf("💓 Heartbeat received on topic %s", ct.Name)
		}
	} else {
		// Regular message - log content
		log.Printf("📨 Received message on topic %s from %s: %s",
			ct.Name, msg.ReceivedFrom.ShortString(), string(msg.Data))
	}
}

// isMembershipTopic checks if this topic is a membership topic
func (ct *ConnectedTopic) isMembershipTopic() bool {
	return strings.Contains(ct.Name, "/members/") || strings.Contains(ct.Name, "/joined")
}

// isHeartbeatTopic checks if this topic is a heartbeat topic
func (ct *ConnectedTopic) isHeartbeatTopic() bool {
	return strings.Contains(ct.Name, "/heartbeat")
}

// isSuspicionTopic checks if this topic is a suspicion topic
func (ct *ConnectedTopic) isSuspicionTopic() bool {
	return strings.Contains(ct.Name, "/suspicion")
}

// isRefutationTopic checks if this topic is a refutation topic
func (ct *ConnectedTopic) isRefutationTopic() bool {
	return strings.Contains(ct.Name, "/refutation")
}

// isQuorumTopic checks if this topic is a quorum topic
func (ct *ConnectedTopic) isQuorumTopic() bool {
	return strings.Contains(ct.Name, "/quorum")
}

// isFailureTopic checks if this topic is a failure topic
func (ct *ConnectedTopic) isFailureTopic() bool {
	return strings.Contains(ct.Name, "/failure")
}

type Node struct {
	clusters           []string
	name               string
	id                 string
	addr               ma.Multiaddr
	port               uint
	tags               Tags
	status             NodeStatus
	phonebook          *Phonebook
	host               host.Host
	pubSub             *pubsub.PubSub
	connectedTopics    map[string]*ConnectedTopic
	privateKey         crypto.PrivKey
	publicKey          crypto.PubKey
	rootCAPath         string // Path to the shared Root CA certificate
	ctx                context.Context
	mu                 sync.RWMutex
	topics             sync.Map
	certManager        *CertificateManager
	bootManager        *BootManager
	connManager        *ConnectionManager
	healthMonitor      *ConnectionHealthMonitor
	optimizer          *ConnectionOptimizer
	dataCenter         string   // Primary data center (for backward compatibility)
	dataCenters        []string // List of all data centers this node belongs to
	connectionStrategy string
	membershipManager  *MembershipManager
	heartbeatManager   *HeartbeatManager
	healthRegistry     *HealthRegistry
	swimProber         *SWIMProber
	suspicionManager   *SuspicionManager
	quorumManager      *QuorumManager
	selfHealingManager *SelfHealingManager
	tlsAuthenticator   *TLSAuthenticator
	phonebookSyncer    *PhonebookSyncer
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

// Simple hash function to convert string to int64
func hash(s string) uint64 {
	h := fnv.New64()
	h.Write([]byte(s))
	return h.Sum64()
}

func WithPeers(peers []PeerInput) NodeOption {
	return NodeOption{
		id: "peers",
		cb: func(n *Node) interface{} {
			log.Println("Connecting to peers...", peers)
			if len(peers) == 0 {
				log.Println("No peers specified, skipping connection")
				return nil
			}
			for _, p := range peers {
				if err := n.ConnectPeer(p); err != nil {
					log.Printf("Failed to connect peer %s: %v", p.ID, err)
				} else {
					log.Printf("Connected to peer %s at %s", p.ID, p.Addr)
				}
			}
			
			// CRITICAL FIX: For FloodSub to work properly, we need to rejoin topics after peer connections
			// This ensures pubsub mesh formation with the newly connected peers
			log.Printf("🔄 Rejoining topics after peer connections to fix FloodSub mesh formation...")
			n.rejoinAllTopics()
			
			return nil
		},
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

func withPubSub() NodeOption {
	return NodeOption{
		id: "pubsub",
		cb: func(n *Node) interface{} {
			log.Printf("🔧 Initializing PubSub for node %s", n.name)

			if n.pubSub != nil {
				log.Println("Using existing PubSub instance")
				return nil
			}

			if n.ctx == nil {
				log.Printf("❌ ERROR: node context is nil!")
				return nil
			}

			if n.host == nil {
				log.Printf("❌ ERROR: node host is nil!")
				return nil
			}

			log.Printf("Creating PubSub instance (type: gossip)...")

			// Use GossipSub for better peer discovery and mesh formation
			ps, err := pubsub.NewGossipSub(n.ctx, n.host)
			if err == nil {
				log.Printf("✅ GossipSub created successfully")
			}

			if err != nil {
				panic("Failed to create PubSub: " + err.Error())
			}
			n.pubSub = ps

			log.Printf("Clusters to join: %v", n.clusters)
			for _, cluster := range n.clusters {
				topicNames := GetDefaultTopicNames(cluster)
				log.Printf("Joining topics for cluster '%s': %v", cluster, topicNames)
				for _, topicName := range topicNames {
					log.Printf("Attempting to join topic: %s", topicName)
					_, err := n.JoinTopic(topicName)
					if err != nil {
						log.Printf("❌ Failed to join cluster topic %s: %v", topicName, err)
					} else {
						log.Printf("✅ Successfully joined topic: %s", topicName)
					}
				}
			}
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

// WithCertificates configures both root CA and node certificate for authentication
func WithCertificates(rootCAPath, nodeCertPath string) NodeOption {
	return NodeOption{
		id: "certificates",
		cb: func(n *Node) interface{} {
			if n.privateKey == nil {
				log.Printf("Private key not available, deferring certificate initialization")
				return nil
			}

			certManager, err := NewCertificateManager(rootCAPath, nodeCertPath, n.privateKey)
			if err != nil {
				log.Printf("Failed to initialize certificate manager: %v", err)
				return nil
			}

			n.certManager = certManager
			log.Printf("Certificate manager initialized successfully")
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
			if n.privateKey == nil {
				log.Printf("Private key not available, deferring TLS auth initialization")
				return nil
			}

			tlsAuth, err := NewTLSAuthenticator(n, rootCAPath, clientCertPath, clientKeyPath)
			if err != nil {
				log.Printf("Failed to initialize TLS authenticator: %v", err)
				return nil
			}

			n.tlsAuthenticator = tlsAuth
			log.Printf("TLS authenticator initialized successfully")
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

// WithDataCenter configures a single data center (legacy support)
func WithDataCenter(dcID string) NodeOption {
	return NodeOption{
		id: "dataCenter",
		cb: func(n *Node) interface{} {
			n.dataCenters = []string{dcID}
			n.dataCenter = dcID // Keep for backward compatibility
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

// WithConnectionStrategy configures the connection strategy
func WithConnectionStrategy(strategy string) NodeOption {
	return NodeOption{
		id: "connectionStrategy",
		cb: func(n *Node) interface{} {
			n.connectionStrategy = strategy
			return nil
		},
		runType: "preInitialize",
	}
}

// WithAdvancedBoot enables the advanced boot process with all new components
func WithAdvancedBoot(bootConfig *BootConfig) NodeOption {
	return NodeOption{
		id: "advancedBoot",
		cb: func(n *Node) interface{} {
			if bootConfig == nil {
				bootConfig = DefaultBootConfig()
			}

			// Override with node-specific settings
			if n.dataCenter != "" {
				bootConfig.DataCenter = n.dataCenter
			}
			if n.connectionStrategy != "" {
				bootConfig.ConnectionStrategy = n.connectionStrategy
			}

			// Initialize boot manager (will be started later)
			n.bootManager = NewBootManager(n, bootConfig)

			return nil
		},
		runType: "postInitialize",
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
		ctx:             ctx,
		status:          NodeStatusEnum.Initializing,
		phonebook:       NewPhonebook(),
		connectedTopics: make(map[string]*ConnectedTopic),
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
	node.privateKey = h.Peerstore().PrivKey(h.ID()) // Extract private key for certificate generation
	
	// Use Root CA path from node configuration (set via WithRootCA option)
	// Fall back to environment variable if not set via option
	rootCAPath := node.rootCAPath
	if rootCAPath == "" {
		rootCAPath = os.Getenv("FALAK_ROOT_CA")
	}
	
	// MANDATORY: Initialize certificates V2 (using shared Root CA)
	certInitializer := NewCertificateInitializerV2(node)
	if err := certInitializer.InitializeCertificatesV2(rootCAPath); err != nil {
		// Fallback to V1 for backward compatibility (generates own CA)
		log.Printf("⚠️  Warning: V2 certificate initialization failed: %v", err)
		log.Printf("⚠️  Falling back to V1 (self-signed) - NOT RECOMMENDED for production")
		
		certInitializerV1 := NewCertificateInitializer(node)
		if err := certInitializerV1.InitializeCertificates("", "", ""); err != nil {
			return nil, fmt.Errorf("CRITICAL: Certificate initialization failed: %w", err)
		}
	}
	
	// Apply options (phonebook already initialized)
	for _, opt := range afterInitOpts {
		opt.cb(node)
	}
	// Register NodeConnector with detailed logging
	nodeConnector := &NodeConnector{node: node}
	h.Network().Notify(nodeConnector)
	log.Printf("🔗 Registered NodeConnector for node %s", node.name)
	// Ensure we have pubsub
	withPubSub().cb(node)

	// Register authentication stream handler
	authHandler := NewAuthenticationHandler(node)
	h.SetStreamHandler(AuthProtocolID, authHandler.HandleStream)
	log.Printf("🔐 Registered authentication stream handler: %s", AuthProtocolID)

	// Initialize membership manager
	node.membershipManager = NewMembershipManager(node)
	log.Printf("👥 Initialized membership manager")

	// Initialize health registry
	node.healthRegistry = NewHealthRegistry(node)
	log.Printf("📊 Initialized health registry")

	// Initialize heartbeat manager
	node.heartbeatManager = NewHeartbeatManager(node)
	log.Printf("💓 Initialized heartbeat manager")

	// Initialize SWIM prober
	node.swimProber = NewSWIMProber(node)
	node.swimProber.RegisterHandlers()
	log.Printf("🏓 Initialized SWIM prober")

	// Initialize suspicion manager
	node.suspicionManager = NewSuspicionManager(node)
	log.Printf("📢 Initialized suspicion manager")

	// Initialize quorum manager
	node.quorumManager = NewQuorumManager(node)
	log.Printf("⚖️ Initialized quorum manager")

	// Initialize self-healing manager
	node.selfHealingManager = NewSelfHealingManager(node)
	log.Printf("🔄 Initialized self-healing manager")

	// Initialize phonebook syncer
	node.phonebookSyncer = NewPhonebookSyncer(node)
	log.Printf("📚 Initialized phonebook syncer")

	// Subscribe to membership and heartbeat topics for all clusters
	node.subscribeToClusterTopics()

	// Start heartbeat publishing
	if err := node.heartbeatManager.Start(); err != nil {
		log.Printf("⚠️ Failed to start heartbeat manager: %v", err)
	}

	// Start self-healing monitoring
	if err := node.selfHealingManager.StartMonitoring(); err != nil {
		log.Printf("⚠️ Failed to start self-healing monitoring: %v", err)
	}

	// Start phonebook synchronization
	if err := node.phonebookSyncer.StartSynchronization(); err != nil {
		log.Printf("⚠️ Failed to start phonebook synchronization: %v", err)
	}

	// Start periodic health monitoring
	go node.periodicHealthMonitoring()

	// Log topic status after initialization
	node.LogTopicStatus()

	node.status = NodeStatusEnum.Ready
	return node, nil
}

func (n *Node) GetStats() NodeStatus     { return n.status }
func (n *Node) ID() string               { return n.id }
func (n *Node) GetTags() Tags            { return n.tags }
func (n *Node) GetPhonebook() *Phonebook { return n.phonebook }
func (n *Node) GetHost() host.Host       { return n.host }

func (n *Node) ConnectPeer(in PeerInput) error {
	addr, err := ma.NewMultiaddr(in.Addr)
	if err != nil {
		return err
	}
	info, err := peer.AddrInfoFromP2pAddr(addr)
	if err != nil {
		return err
	}

	// Connection with retry logic
	operation := func() (string, error) {
		err := n.host.Connect(n.ctx, *info)
		return "", err
	}
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 500 * time.Millisecond
	bo.MaxInterval = 5 * time.Second
	_, err = backoff.Retry(n.ctx, operation, backoff.WithBackOff(bo), backoff.WithMaxTries(100))
	if err != nil {
		return err
	}

	// Perform authentication using the new stream protocol
	log.Printf("🔐 Starting authentication with peer %s", info.ID)

	// Use first cluster for authentication
	clusterID := "default"
	if len(n.clusters) > 0 {
		clusterID = n.clusters[0]
	}

	if err := n.AuthenticateWithPeer(info.ID, clusterID); err != nil {
		log.Printf("❌ Authentication failed with peer %s: %v", info.ID, err)
		// Close connection on authentication failure
		if conn := n.host.Network().ConnsToPeer(info.ID); len(conn) > 0 {
			for _, c := range conn {
				c.Close()
			}
		}
		return fmt.Errorf("authentication failed: %w", err)
	}
	log.Printf("✅ Successfully authenticated with peer %s", info.ID)

	// Add to phonebook after successful authentication
	peer := Peer{
		ID:         in.ID,
		Status:     "active",
		AddrInfo:   *info,
		Tags:       in.Tags,
		DataCenter: n.dataCenter, // Use node's data center as default
		LastSeen:   time.Now(),
		TrustScore: 1.0, // Default trust score for manually connected peers
	}

	// Override data center if specified in tags
	if dc, exists := in.Tags["datacenter"]; exists {
		if dcStr, ok := dc.(string); ok {
			peer.DataCenter = dcStr
		}
	}

	n.phonebook.AddPeer(peer)

	// Register with health monitor if available
	if n.healthMonitor != nil {
		n.healthMonitor.RegisterPeer(peer.ID, peer.DataCenter)
	}

	// Publish NodeJoined event after successful authentication and phonebook update
	if n.membershipManager != nil {
		// Get list of connected peers
		connectedPeers := n.getConnectedPeerIDs()

		// Publish NodeJoined event to the cluster
		if err := n.membershipManager.PublishNodeJoined(info.ID.String(), clusterID, connectedPeers); err != nil {
			log.Printf("⚠️ Failed to publish NodeJoined event: %v", err)
		}
	}

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

	// Start listener for this topic
	go func(local peer.ID, name string, s *pubsub.Subscription, cb *ConnectedTopic) {
		log.Printf("Listening on topic: %s", name)
		for {
			msg, err := s.Next(n.ctx)
			if err != nil {
				log.Printf("Subscription closed for topic %s: %v", name, err)
				return
			}
			// CRITICAL DEBUG: Log ALL messages before filtering
			// log.Printf("🔍 CRITICAL: %s processing message on topic %s from ReceivedFrom=%s, Local=%s",
			// 	n.name, name, msg.ReceivedFrom.ShortString(), local.ShortString())

			// For heartbeat topics, check the actual message content to filter self
			if cb.isHeartbeatTopic() {
				// Parse heartbeat to check if it's from ourselves
				var heartbeat models.Heartbeat
				if err := proto.Unmarshal(msg.Data, &heartbeat); err == nil {
					normalizedMsgNodeID := normalizePeerID(heartbeat.NodeId)
					normalizedLocalNodeID := normalizePeerID(n.id)
					if normalizedMsgNodeID == normalizedLocalNodeID {
						// This is our own heartbeat, ignore it
						// log.Printf("🔄 %s ignoring self heartbeat", n.name)
						continue
					} else {
						log.Printf("🔍 CRITICAL: %s heartbeat normalization: msg.NodeId=%s -> %s, local.id=%s -> %s",
							n.name, heartbeat.NodeId, normalizedMsgNodeID, n.id, normalizedLocalNodeID)
					}
					// This is a legitimate heartbeat from another node - log it for debugging
					log.Printf("💓 CRITICAL: %s received heartbeat from peer %s on topic %s (ReceivedFrom: %s, Local: %s)",
						n.name, normalizedMsgNodeID, name, msg.ReceivedFrom.ShortString(), local.ShortString())
				} else {
					log.Printf("❌ CRITICAL: %s failed to unmarshal heartbeat: %v", n.name, err)
				}
			} else {
				// For non-heartbeat topics, use the original ReceivedFrom check
				if msg.ReceivedFrom == local {
					log.Printf("🔄 Ignoring self message on topic %s from %s", name, msg.ReceivedFrom.ShortString())
					continue
				}
			}

			// Log processing (less verbose for heartbeat topics)
			if !cb.isHeartbeatTopic() {
				log.Printf("📨 Processing message on topic %s from %s (local: %s)",
					name, msg.ReceivedFrom.ShortString(), local.ShortString())
			}

			// Check topic type and process accordingly
			if cb.isMembershipTopic() && n.membershipManager != nil {
				// Process membership event
				if err := n.membershipManager.ProcessMembershipEvent(name, msg.Data); err != nil {
					log.Printf("❌ Failed to process membership event on topic %s: %v", name, err)
				}
			} else if cb.isHeartbeatTopic() && n.heartbeatManager != nil {
				// Process heartbeat
				if err := n.heartbeatManager.ProcessHeartbeat(name, msg.Data); err != nil {
					log.Printf("❌ Failed to process heartbeat on topic %s: %v", name, err)
				}
			} else if cb.isSuspicionTopic() && n.suspicionManager != nil {
				// Process suspicion message
				if err := n.suspicionManager.ProcessSuspicionMessage(msg.Data); err != nil {
					log.Printf("❌ Failed to process suspicion message on topic %s: %v", name, err)
				}
			} else if cb.isRefutationTopic() && n.suspicionManager != nil {
				// Process refutation message
				if err := n.suspicionManager.ProcessRefutationMessage(msg.Data); err != nil {
					log.Printf("❌ Failed to process refutation message on topic %s: %v", name, err)
				}
			} else if cb.isQuorumTopic() && n.quorumManager != nil {
				// Process quorum verdict message
				if err := n.quorumManager.ProcessQuorumVerdict(msg.Data); err != nil {
					log.Printf("❌ Failed to process quorum verdict on topic %s: %v", name, err)
				}
			} else if cb.isFailureTopic() && n.quorumManager != nil {
				// Process failure announcement message
				if err := n.quorumManager.ProcessFailureAnnouncement(msg.Data); err != nil {
					log.Printf("❌ Failed to process failure announcement on topic %s: %v", name, err)
				}
			} else {
				// Regular message processing
				cb.onMessage(msg)
			}
		}
	}(n.host.ID(), topicName, sub, ct)

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
func (n *Node) rejoinAllTopics() {
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
			ct.Sub.Cancel() // stop subscription
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

// GetPrimaryDataCenter returns the primary data center (first in the list)
func (n *Node) GetPrimaryDataCenter() string {
	return n.dataCenter
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

// getOrCreateTopic safely gets or creates a topic with proper synchronization
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

// GetNetworkDiagnostics returns detailed network connectivity information for debugging
func (n *Node) GetNetworkDiagnostics() map[string]interface{} {
	connectedPeers := n.host.Network().Peers()

	// Debug: Log raw network peer information for troubleshooting
	if len(connectedPeers) > 0 {
		log.Printf("🐛 DEBUG: Raw network peers for %s:", n.name)
		for i, peerID := range connectedPeers {
			conns := n.host.Network().ConnsToPeer(peerID)
			log.Printf("   [%d] Peer: %s, Connections: %d", i, peerID.ShortString(), len(conns))
			for j, conn := range conns {
				log.Printf("       Conn[%d]: %s -> %s (Direction: %s)",
					j, conn.LocalPeer().ShortString(), conn.RemotePeer().ShortString(),
					conn.Stat().Direction)
			}
		}
	} else {
		log.Printf("🐛 DEBUG: No network peers found for %s", n.name)
	}

	diagnostics := map[string]interface{}{
		"node_id":           n.id,
		"node_name":         n.name,
		"connected_peers":   len(connectedPeers),
		"peer_details":      make([]map[string]interface{}, 0),
		"pubsub_topics":     make([]string, 0),
		"phonebook_entries": len(n.phonebook.GetPeers()),
		"health_registry":   make(map[string]interface{}),
	}

	// Add peer details
	for _, peerID := range connectedPeers {
		peerInfo := map[string]interface{}{
			"peer_id": peerID.String(),
			"addresses": func() []string {
				addrs := n.host.Peerstore().Addrs(peerID)
				strAddrs := make([]string, len(addrs))
				for i, addr := range addrs {
					strAddrs[i] = addr.String()
				}
				return strAddrs
			}(),
		}
		diagnostics["peer_details"] = append(diagnostics["peer_details"].([]map[string]interface{}), peerInfo)
	}

	// Add pubsub topic information
	topicsWithPeers := make([]map[string]interface{}, 0)
	n.topics.Range(func(key, value interface{}) bool {
		topicName := key.(string)
		topic := value.(*pubsub.Topic)

		// Get topic peers (who else is subscribed to this topic)
		topicPeers := topic.ListPeers()

		topicInfo := map[string]interface{}{
			"name":       topicName,
			"peer_count": len(topicPeers),
			"peers":      make([]string, len(topicPeers)),
		}

		for i, peerID := range topicPeers {
			topicInfo["peers"].([]string)[i] = peerID.ShortString()
		}

		topicsWithPeers = append(topicsWithPeers, topicInfo)
		diagnostics["pubsub_topics"] = append(diagnostics["pubsub_topics"].([]string), topicName)
		return true
	})

	diagnostics["topic_details"] = topicsWithPeers

	// Add health registry information
	if n.healthRegistry != nil {
		healthy, suspected, failed := n.healthRegistry.GetHealthySuspectedPeers()
		allPeers := n.healthRegistry.GetAllPeers()

		healthInfo := map[string]interface{}{
			"total_tracked": len(allPeers),
			"healthy_count": healthy,
			"suspect_count": suspected,
			"failed_count":  failed,
			"peer_health":   make([]map[string]interface{}, 0),
		}

		// Add individual peer health details
		for nodeID, peerHealth := range allPeers {
			peerHealthDetail := map[string]interface{}{
				"node_id":     nodeID[:12] + "...", // Truncate for readability
				"status":      string(peerHealth.Status),
				"phi_score":   peerHealth.Phi,
				"last_seen":   int(time.Since(peerHealth.LastHeartbeat).Seconds()),
				"incarnation": peerHealth.Incarnation,
			}
			healthInfo["peer_health"] = append(healthInfo["peer_health"].([]map[string]interface{}), peerHealthDetail)
		}

		diagnostics["health_registry"] = healthInfo
	}

	return diagnostics
}

// StartAdvancedBoot initiates the advanced boot process if configured
func (n *Node) StartAdvancedBoot() error {
	if n.bootManager == nil {
		return fmt.Errorf("advanced boot not configured - use WithAdvancedBoot() option")
	}

	log.Printf("Starting advanced boot process for node %s", n.name)

	// Execute the boot sequence
	if err := n.bootManager.ExecuteBootSequence(); err != nil {
		return fmt.Errorf("advanced boot sequence failed: %w", err)
	}

	// Initialize other components after successful boot
	if err := n.initializeAdvancedComponents(); err != nil {
		return fmt.Errorf("failed to initialize advanced components: %w", err)
	}

	log.Printf("Advanced boot completed successfully for node %s", n.name)
	return nil
}

// LogTopicStatus logs the currently subscribed topics for debugging
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

// initializeAdvancedComponents initializes health monitoring and other advanced features
func (n *Node) initializeAdvancedComponents() error {
	// Initialize optimizer if we have the required data
	if n.dataCenter != "" && n.bootManager != nil {
		n.optimizer = NewConnectionOptimizer(n, n.dataCenter, nil)
	}

	// Initialize health monitoring if we have a connection manager
	if n.bootManager != nil && n.bootManager.connManager != nil {
		n.connManager = n.bootManager.connManager
		n.healthMonitor = NewConnectionHealthMonitor(n.connManager, n.optimizer, nil)

		// Start health monitoring
		if err := n.healthMonitor.StartMonitoring(); err != nil {
			log.Printf("Warning: failed to start health monitoring: %v", err)
		} else {
			log.Printf("Health monitoring started")
		}
	}

	return nil
}

// GetBootStatus returns the current boot status if advanced boot is enabled
func (n *Node) GetBootStatus() *BootStatus {
	if n.bootManager == nil {
		return nil
	}

	status := n.bootManager.GetBootStatus()
	return &status
}

// GetConnectionHealth returns connection health information
func (n *Node) GetConnectionHealth() *HealthMonitorStatus {
	if n.healthMonitor == nil {
		return nil
	}

	status := n.healthMonitor.GetHealthStatus()
	return &status
}

// SavePhonebookCache saves the current phonebook to cache if available
func (n *Node) SavePhonebookCache() error {
	if n.bootManager == nil {
		return fmt.Errorf("boot manager not available")
	}

	return n.bootManager.SavePhonebookCache()
}

// Close gracefully shuts down the node and all its components
func (n *Node) Close() error {
	log.Printf("Shutting down node %s", n.name)

	// Stop heartbeat manager
	if n.heartbeatManager != nil {
		if err := n.heartbeatManager.Stop(); err != nil {
			log.Printf("Warning: failed to stop heartbeat manager: %v", err)
		}
	}

	// Stop self-healing manager
	if n.selfHealingManager != nil {
		if err := n.selfHealingManager.StopMonitoring(); err != nil {
			log.Printf("Warning: failed to stop self-healing manager: %v", err)
		}
	}

	// Stop health monitoring
	if n.healthMonitor != nil {
		if err := n.healthMonitor.Stop(); err != nil {
			log.Printf("Warning: failed to stop health monitor: %v", err)
		}
	}

	// Save phonebook cache before shutdown
	if n.bootManager != nil {
		if err := n.SavePhonebookCache(); err != nil {
			log.Printf("Warning: failed to save phonebook cache: %v", err)
		}

		if err := n.bootManager.Close(); err != nil {
			log.Printf("Warning: failed to close boot manager: %v", err)
		}
	}

	// Close libp2p host
	if n.host != nil {
		if err := n.host.Close(); err != nil {
			log.Printf("Warning: failed to close libp2p host: %v", err)
		}
	}

	log.Printf("Node %s shutdown complete", n.name)
	return nil
}

// IsAdvancedBootEnabled returns true if advanced boot features are enabled
func (n *Node) IsAdvancedBootEnabled() bool {
	return n.bootManager != nil
}

// subscribeToClusterTopics subscribes to all cluster topics (membership, heartbeat, etc.)
func (n *Node) subscribeToClusterTopics() {
	for _, cluster := range n.clusters {
		// Subscribe to membership topic
		membershipTopic := fmt.Sprintf("falak/%s/members/membership", cluster)
		_, err := n.JoinTopic(membershipTopic)
		if err != nil {
			log.Printf("❌ Failed to join membership topic %s: %v", membershipTopic, err)
		} else {
			log.Printf("✅ Subscribed to membership topic: %s", membershipTopic)
		}

		// Subscribe to joined topic
		joinedTopic := fmt.Sprintf("falak/%s/members/joined", cluster)
		_, err = n.JoinTopic(joinedTopic)
		if err != nil {
			log.Printf("❌ Failed to join joined topic %s: %v", joinedTopic, err)
		} else {
			log.Printf("✅ Subscribed to joined topic: %s", joinedTopic)
		}

		// Subscribe to heartbeat topic
		heartbeatTopic := fmt.Sprintf("falak/%s/heartbeat", cluster)
		_, err = n.JoinTopic(heartbeatTopic)
		if err != nil {
			log.Printf("❌ Failed to join heartbeat topic %s: %v", heartbeatTopic, err)
		} else {
			log.Printf("💓 Subscribed to heartbeat topic: %s", heartbeatTopic)
		}

		// Subscribe to suspicion topic
		suspicionTopic := fmt.Sprintf("falak/%s/suspicion", cluster)
		_, err = n.JoinTopic(suspicionTopic)
		if err != nil {
			log.Printf("❌ Failed to join suspicion topic %s: %v", suspicionTopic, err)
		} else {
			log.Printf("📢 Subscribed to suspicion topic: %s", suspicionTopic)
		}

		// Subscribe to refutation topic
		refutationTopic := fmt.Sprintf("falak/%s/refutation", cluster)
		_, err = n.JoinTopic(refutationTopic)
		if err != nil {
			log.Printf("❌ Failed to join refutation topic %s: %v", refutationTopic, err)
		} else {
			log.Printf("🛡️ Subscribed to refutation topic: %s", refutationTopic)
		}

		// Subscribe to quorum topic
		quorumTopic := fmt.Sprintf("falak/%s/quorum", cluster)
		_, err = n.JoinTopic(quorumTopic)
		if err != nil {
			log.Printf("❌ Failed to join quorum topic %s: %v", quorumTopic, err)
		} else {
			log.Printf("⚖️ Subscribed to quorum topic: %s", quorumTopic)
		}

		// Subscribe to failure topic
		failureTopic := fmt.Sprintf("falak/%s/failure", cluster)
		_, err = n.JoinTopic(failureTopic)
		if err != nil {
			log.Printf("❌ Failed to join failure topic %s: %v", failureTopic, err)
		} else {
			log.Printf("💀 Subscribed to failure topic: %s", failureTopic)
		}
	}
}

// GetMembershipManager returns the membership manager
func (n *Node) GetMembershipManager() *MembershipManager {
	return n.membershipManager
}

// getConnectedPeerIDs returns a list of currently connected peer IDs
func (n *Node) getConnectedPeerIDs() []string {
	peers := n.host.Network().Peers()
	peerIDs := make([]string, len(peers))
	for i, peerID := range peers {
		peerIDs[i] = peerID.String()
	}
	return peerIDs
}

// GetHeartbeatManager returns the heartbeat manager
func (n *Node) GetHeartbeatManager() *HeartbeatManager {
	return n.heartbeatManager
}

// GetHealthRegistry returns the health registry
func (n *Node) GetHealthRegistry() *HealthRegistry {
	return n.healthRegistry
}

// GetSWIMProber returns the SWIM prober
func (n *Node) GetSWIMProber() *SWIMProber {
	return n.swimProber
}

// GetSuspicionManager returns the suspicion manager
func (n *Node) GetSuspicionManager() *SuspicionManager {
	return n.suspicionManager
}

// GetQuorumManager returns the quorum manager
func (n *Node) GetQuorumManager() *QuorumManager {
	return n.quorumManager
}

// GetSelfHealingManager returns the self-healing manager
func (n *Node) GetSelfHealingManager() *SelfHealingManager {
	return n.selfHealingManager
}

// GetTLSAuthenticator returns the TLS authenticator
func (n *Node) GetTLSAuthenticator() *TLSAuthenticator {
	return n.tlsAuthenticator
}

// GetPhonebookSyncer returns the phonebook syncer
func (n *Node) GetPhonebookSyncer() *PhonebookSyncer {
	return n.phonebookSyncer
}

// periodicHealthMonitoring runs periodic health checks and SWIM probing
func (n *Node) periodicHealthMonitoring() {
	healthTicker := time.NewTicker(5 * time.Second) // Health checks every 5 seconds
	syncTicker := time.NewTicker(30 * time.Second)  // Phonebook sync every 30 seconds
	defer healthTicker.Stop()
	defer syncTicker.Stop()

	log.Printf("📊 Starting periodic health monitoring")

	for {
		select {
		case <-n.ctx.Done():
			log.Printf("📊 Stopping periodic health monitoring")
			return

		case <-healthTicker.C:
			if n.healthRegistry != nil {
				n.healthRegistry.PeriodicHealthCheck()
			}

		case <-syncTicker.C:
			if n.healthRegistry != nil {
				n.healthRegistry.SyncWithPhonebook()
			}
		}
	}
}
