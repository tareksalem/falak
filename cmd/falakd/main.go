package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/tareksalem/falak/config"
	"github.com/tareksalem/falak/election/gravity"
	"github.com/tareksalem/falak/node"
	"github.com/tareksalem/falak/node/auth/certs"
)

// buildElectionConfig converts the CUE-derived ElectionConfig into the
// native node.ClusterElectionConfig expected by Node.Join. Parses the
// timeout duration string and merges weight overrides onto gravity
// defaults. Returns an error only when the duration fails to parse.
func buildElectionConfig(src *config.ElectionConfig) (*node.ClusterElectionConfig, error) {
	if src == nil {
		return nil, nil
	}
	out := &node.ClusterElectionConfig{
		Algorithm: src.Algorithm,
	}
	if src.Timeout != "" {
		d, err := time.ParseDuration(src.Timeout)
		if err != nil {
			return nil, fmt.Errorf("election.timeout: %w", err)
		}
		out.Timeout = d
	}
	if src.Weights != nil {
		w := gravity.Weights{
			CPUHeadroom:        src.Weights.CPUHeadroom,
			MemoryHeadroom:     src.Weights.MemoryHeadroom,
			DiskHeadroom:       src.Weights.DiskHeadroom,
			SoftPlacementMatch: src.Weights.SoftPlacementMatch,
			AffinityProximity:  src.Weights.AffinityProximity,
			HardwareLabelMatch: src.Weights.HardwareLabelMatch,
			LoadPenalty:        src.Weights.LoadPenalty,
			Reliability:        src.Weights.Reliability,
			Diversity:          src.Weights.Diversity,
		}
		out.Weights = &w
	}
	return out, nil
}

func main() {
	// Config file flag
	configFile := flag.String("config", "", "Path to CUE configuration file (overrides CLI flags for cluster config)")
	schemaDir := flag.String("schema-dir", "", "Path to directory containing falak.cue schema (optional, for validation)")

	// Node configuration flags (used when --config is not provided)
	name := flag.String("name", "", "Node name (used for deterministic peer ID and data directory)")
	region := flag.String("region", "default", "Node region")
	datacenter := flag.String("datacenter", "default", "Node datacenter")
	port := flag.Int("port", 0, "TCP port to listen on (0 = random)")
	listenAddr := flag.String("listen", "", "Listen address (multiaddr format, overrides --port)")
	dataDir := flag.String("data-dir", "", "Data directory (default: ~/Library/Application Support/falak/<name> on macOS, ~/.local/share/falak/<name> on Linux)")
	logLevel := flag.String("log-level", "info", "Log level (debug, info, warn, error)")

	// Cluster join flags (single cluster, used when --config is not provided)
	clusterPath := flag.String("cluster", "", "Cluster path to join (e.g., us-east/dc1/prod)")
	psk := flag.String("psk", "", "Pre-shared key for cluster authentication")
	bootstrapPeers := flag.String("bootstrap", "", "Comma-separated list of bootstrap peer multiaddrs")

	// External CA flags (apply to the cluster specified by --cluster)
	caCertPath := flag.String("ca-cert", "", "Path to CA certificate for external PKI mode (PEM-encoded)")
	caKeyPath := flag.String("ca-key", "", "Path to CA private key for external PKI mode (PEM-encoded, enables voucher signing)")
	nodeCertPath := flag.String("node-cert", "", "Path to pre-signed node certificate (PEM-encoded)")
	nodeKeyPath := flag.String("node-key", "", "Path to node private key (PEM-encoded)")

	// Testing flags
	forceRejectAuth := flag.Bool("force-reject-auth", false, "Force rejection of all incoming auth announcements (for testing)")

	flag.Parse()

	// If --config is provided, load from CUE and run in config mode
	if *configFile != "" {
		runFromConfig(*configFile, *schemaDir, *forceRejectAuth)
		return
	}

	// Setup logger
	logger, err := setupLogger(*logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	// Build node options
	opts := []node.Option{
		node.WithLogger(logger),
		node.WithRegion(*region),
		node.WithDatacenter(*datacenter),
	}

	if *name != "" {
		opts = append(opts, node.WithName(*name))
	}

	// Determine listen address
	var finalListenAddr string
	if *listenAddr != "" {
		finalListenAddr = *listenAddr
	} else {
		finalListenAddr = fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", *port)
	}
	opts = append(opts, node.WithListenAddrs(finalListenAddr))

	if *dataDir != "" {
		opts = append(opts, node.WithDataDir(*dataDir))
	}

	if *forceRejectAuth {
		opts = append(opts, node.WithRejectAllAuth(true))
		logger.Warn("TESTING MODE: force-reject-auth is enabled, all incoming auth will be rejected")
	}

	// Create node
	n := node.New(opts...)

	// Start node
	logger.Info("starting falakd")
	if err := n.Start(); err != nil {
		logger.Fatal("failed to start node", zap.Error(err))
	}

	logger.Info("node started",
		zap.String("id", n.ID().String()),
		zap.String("name", n.Name()),
		zap.String("dataDir", n.DataDir()),
		zap.Strings("addrs", n.Addrs()))

	// Join cluster if specified
	if *clusterPath != "" {
		if *psk == "" {
			logger.Fatal("PSK is required when joining a cluster")
		}

		// Validate external CA flag combinations
		if *caKeyPath != "" && *caCertPath == "" {
			logger.Fatal("--ca-key requires --ca-cert")
		}
		if *nodeCertPath != "" && *caCertPath == "" {
			logger.Fatal("--node-cert requires --ca-cert")
		}
		if *nodeKeyPath != "" && *caCertPath == "" && *nodeCertPath == "" {
			logger.Fatal("--node-key requires --ca-cert or --node-cert")
		}

		cfg := node.ClusterConfig{
			Path: *clusterPath,
			PSK:  []byte(*psk),
		}

		if *bootstrapPeers != "" {
			cfg.BootstrapPeers = strings.Split(*bootstrapPeers, ",")
		}

		// Build external CA config if any cert flags provided
		if *caCertPath != "" {
			cfg.Certificates = &certs.ClusterCertConfig{
				CACertPath:   *caCertPath,
				CAKeyPath:    *caKeyPath,
				NodeCertPath: *nodeCertPath,
				NodeKeyPath:  *nodeKeyPath,
			}
			logger.Info("external CA mode enabled",
				zap.String("ca-cert", *caCertPath),
				zap.Bool("has-ca-key", *caKeyPath != ""),
				zap.Bool("has-node-cert", *nodeCertPath != ""),
				zap.Bool("has-node-key", *nodeKeyPath != ""))
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := n.Join(ctx, cfg); err != nil {
			cancel()
			logger.Error("failed to join cluster", zap.Error(err))
			logger.Info("node will continue running — retry joining manually or restart with correct credentials")
		} else {
			cancel()
			logger.Info("joined cluster", zap.String("cluster", *clusterPath))
		}
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	logger.Info("received shutdown signal", zap.String("signal", sig.String()))

	// Graceful shutdown
	if err := n.Stop(); err != nil {
		logger.Error("error during shutdown", zap.Error(err))
	}

	logger.Info("falakd stopped")
}

// runFromConfig loads a CUE config file and starts the node with all clusters.
func runFromConfig(configPath, schemaDir string, forceRejectAuth bool) {
	cfg, err := config.Load(configPath, schemaDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	logger, err := setupLogger(cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	logger.Info("loaded config from CUE",
		zap.String("file", configPath),
		zap.String("name", cfg.Name),
		zap.Int("clusters", len(cfg.Clusters)))

	// Build node options
	opts := []node.Option{
		node.WithName(cfg.Name),
		node.WithLogger(logger),
		node.WithRegion(cfg.Region),
		node.WithDatacenter(cfg.Datacenter),
	}

	if cfg.Listen != "" {
		opts = append(opts, node.WithListenAddrs(cfg.Listen))
	} else {
		opts = append(opts, node.WithListenAddrs(fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", cfg.Port)))
	}

	if cfg.DataDir != "" {
		opts = append(opts, node.WithDataDir(cfg.DataDir))
	}

	if forceRejectAuth {
		opts = append(opts, node.WithRejectAllAuth(true))
		logger.Warn("TESTING MODE: force-reject-auth enabled")
	}

	// Create and start node
	n := node.New(opts...)

	logger.Info("starting falakd")
	if err := n.Start(); err != nil {
		logger.Fatal("failed to start node", zap.Error(err))
	}

	logger.Info("node started",
		zap.String("id", n.ID().String()),
		zap.String("name", n.Name()),
		zap.Strings("addrs", n.Addrs()))

	// Join all clusters from config
	for clusterPath, clusterCfg := range cfg.Clusters {
		joinCfg := node.ClusterConfig{
			Path:           clusterPath,
			PSK:            []byte(clusterCfg.PSK),
			BootstrapPeers: clusterCfg.Bootstrap,
		}

		if clusterCfg.Certificates != nil {
			joinCfg.Certificates = &certs.ClusterCertConfig{
				CACertPath:   clusterCfg.Certificates.CACert,
				CAKeyPath:    clusterCfg.Certificates.CAKey,
				NodeCertPath: clusterCfg.Certificates.NodeCert,
				NodeKeyPath:  clusterCfg.Certificates.NodeKey,
			}
		}

		if clusterCfg.Election != nil {
			ec, err := buildElectionConfig(clusterCfg.Election)
			if err != nil {
				logger.Error("failed to parse election config",
					zap.String("cluster", clusterPath),
					zap.Error(err))
				continue
			}
			joinCfg.Election = ec
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := n.Join(ctx, joinCfg); err != nil {
			cancel()
			logger.Error("failed to join cluster",
				zap.String("cluster", clusterPath),
				zap.Error(err))
			continue
		}
		cancel()
		logger.Info("joined cluster", zap.String("cluster", clusterPath))
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	logger.Info("received shutdown signal", zap.String("signal", sig.String()))

	if err := n.Stop(); err != nil {
		logger.Error("error during shutdown", zap.Error(err))
	}

	logger.Info("falakd stopped")
}

func setupLogger(level string) (*zap.Logger, error) {
	var zapLevel zapcore.Level
	switch strings.ToLower(level) {
	case "debug":
		zapLevel = zapcore.DebugLevel
	case "info":
		zapLevel = zapcore.InfoLevel
	case "warn", "warning":
		zapLevel = zapcore.WarnLevel
	case "error":
		zapLevel = zapcore.ErrorLevel
	default:
		zapLevel = zapcore.InfoLevel
	}

	config := zap.Config{
		Level:            zap.NewAtomicLevelAt(zapLevel),
		Development:      true,
		Encoding:         "console",
		EncoderConfig:    zap.NewDevelopmentEncoderConfig(),
		OutputPaths:      []string{"stdout"},
		ErrorOutputPaths: []string{"stderr"},
	}

	return config.Build()
}
