# Local Development Guide

This guide shows you how to build, run, and test the Falak project locally using Air for hot reloading.

## Prerequisites

- Go 1.21 or later
- [Air](https://github.com/cosmtrek/air) for hot reloading
- Git

## Installation

### 1. Clone the Repository

```bash
git clone <repository-url>
cd falak
```

### 2. Install Air (if not already installed)

```bash
# Install Air globally
go install github.com/cosmtrek/air@latest

# Or using curl
curl -sSfL https://raw.githubusercontent.com/cosmtrek/air/master/install.sh | sh -s -- -b $(go env GOPATH)/bin
```

### 3. Sync Go Workspace

```bash
go work sync
```

## Running Nodes with Air

The project is configured with Air for hot reloading during development. The `.air.toml` configuration automatically builds and runs the main CLI.

### Single Node

Run a single node with default settings using Air:

```bash
# Start node with Air (with hot reloading)
air -- --name node1 --address "/ip4/0.0.0.0/tcp/4001"
```

Air will automatically rebuild and restart the node when you make code changes.

### Two Connected Nodes

#### Step 1: Start the First Node

```bash
# Terminal 1 - Start first node with Air
air -- --name node1 --address "/ip4/0.0.0.0/tcp/4001"
```

The node will output something like:
```
2025/08/15 09:00:00 Hello, Falak!
2025/08/15 09:00:00 Listening for OS signals...
2025/08/15 09:00:00 Node name: node1
2025/08/15 09:00:00 Node address: /ip4/0.0.0.0/tcp/4001
2025/08/15 09:00:00 Node created successfully with ID: 12D3KooWAbc123... [/ip4/0.0.0.0/tcp/4001]
```

Copy the peer ID from the output (e.g., `12D3KooWAbc123...`).

#### Step 2: Start the Second Node

In a new terminal, run:

```bash
# Terminal 2 - Start second node with Air
air -- --name node2 --address "/ip4/0.0.0.0/tcp/4002" --peers "/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWAbc123..."
```

Replace `12D3KooWAbc123...` with the actual peer ID from step 1.

> **Note**: When using Air, each terminal will have its own instance with hot reloading. If you modify the code, both nodes will automatically restart.

#### Expected Output

Node 2 will connect to Node 1 and you should see connection messages:

```
🔌 inbound connection from: 12D3KooWDef456... /ip4/127.0.0.1/tcp/4002/p2p/12D3KooWAbc123...
2025/08/15 09:00:05 No certificate manager configured, skipping authentication
2025/08/15 09:00:05 Connected to peer 12D3KooWAbc123... at /ip4/127.0.0.1/tcp/4001/p2p/12D3KooWAbc123...
```

### Multiple Nodes Example

You can connect multiple nodes to create a network using Air:

```bash
# Terminal 1 - Bootstrap node with Air
air -- --name bootstrap --address "/ip4/0.0.0.0/tcp/4001"

# Terminal 2 - Node 2 connects to bootstrap
air -- --name node2 --address "/ip4/0.0.0.0/tcp/4002" \
  --peers "/ip4/127.0.0.1/tcp/4001/p2p/BOOTSTRAP_PEER_ID"

# Terminal 3 - Node 3 connects to bootstrap  
air -- --name node3 --address "/ip4/0.0.0.0/tcp/4003" \
  --peers "/ip4/127.0.0.1/tcp/4001/p2p/BOOTSTRAP_PEER_ID"

# Terminal 4 - Node 4 connects to node 2
air -- --name node4 --address "/ip4/0.0.0.0/tcp/4004" \
  --peers "/ip4/127.0.0.1/tcp/4002/p2p/NODE2_PEER_ID"
```

## Advanced Configuration

### Using Advanced Boot Features

```bash
air -- --name advanced-node \
  --address "/ip4/0.0.0.0/tcp/4001" \
  --advanced-boot \
  --data-center "us-west-1" \
  --connection-strategy "balanced" \
  --cache-dir "./cache"
```

### With Authentication (if certificates are available)

```bash
air -- --name secure-node \
  --address "/ip4/0.0.0.0/tcp/4001" \
  --root-ca "./certs/ca.pem" \
  --node-cert "./certs/node.pem" \
  --cluster-id "production"
```

## Development Workflow with Air

### Hot Reloading Benefits

When using Air for development:

1. **Automatic Rebuilds**: Air watches for file changes and rebuilds automatically
2. **Fast Restart**: Nodes restart quickly when code changes
3. **Multi-Node Development**: Each terminal runs independently with hot reloading
4. **Debugging**: Easy to test changes across multiple connected nodes

### Air Configuration

The project uses `.air.toml` with these settings:

- **Build Command**: `go work sync && go build -o ./tmp/main ./cmd/main.go`
- **Watched Directories**: `cmd/`, `internal/`
- **File Extensions**: `.go`, `.tpl`, `.tmpl`, `.html`
- **Excluded Directories**: `tmp/`, `vendor/`, `dev/`

### Making Changes

1. Edit any Go file in `cmd/` or `internal/`
2. Air automatically detects the change
3. Rebuilds the binary
4. Restarts the node with the same arguments
5. Node reconnects to peers automatically

## Command Line Options

| Option | Description | Example |
|--------|-------------|---------|
| `--name` | Node name identifier | `--name node1` |
| `--address` | Listen address for the node | `--address "/ip4/0.0.0.0/tcp/4001"` |
| `--peers` | Comma-separated list of peer addresses to connect to | `--peers "/ip4/127.0.0.1/tcp/4001/p2p/12D3..."` |
| `--advanced-boot` | Enable advanced boot process | `--advanced-boot` |
| `--data-center` | Data center identifier | `--data-center "us-west-1"` |
| `--connection-strategy` | Connection strategy (aggressive, conservative, balanced) | `--connection-strategy "balanced"` |
| `--cache-dir` | Directory for phonebook cache | `--cache-dir "./cache"` |
| `--seed-files` | Comma-separated list of seed data files | `--seed-files "./seeds/dc1.json,./seeds/dc2.json"` |
| `--root-ca` | Path to root CA certificate | `--root-ca "./certs/ca.pem"` |
| `--node-cert` | Path to node certificate | `--node-cert "./certs/node.pem"` |
| `--cluster-id` | Cluster ID for authentication | `--cluster-id "production"` |

## Testing

### Run Unit Tests

```bash
# Test the node module
cd internal/node
go test ./tests/ -v

# Run tests with race detection
go test ./tests/ -race -v

# Run tests with coverage
go test ./tests/ -cover -v
```

### Test Coverage Report

```bash
cd internal/node
go test ./tests/ -coverprofile=coverage.out
go tool cover -html=coverage.out -o coverage.html
```

## Network Topology Examples

### Star Topology

```
    Node2 ←→ Bootstrap ←→ Node3
                ↕
              Node4
```

1. Start bootstrap node on port 4001
2. Connect all other nodes to bootstrap

### Mesh Topology

```
    Node1 ←→ Node2
      ↕        ↕
    Node4 ←→ Node3
```

Connect each node to multiple peers:

```bash
# Node 1
air -- --name node1 --address "/ip4/0.0.0.0/tcp/4001"

# Node 2 (connects to Node 1)
air -- --name node2 --address "/ip4/0.0.0.0/tcp/4002" \
  --peers "/ip4/127.0.0.1/tcp/4001/p2p/NODE1_ID"

# Node 3 (connects to Node 2)  
air -- --name node3 --address "/ip4/0.0.0.0/tcp/4003" \
  --peers "/ip4/127.0.0.1/tcp/4002/p2p/NODE2_ID"

# Node 4 (connects to Node 1 and Node 3)
air -- --name node4 --address "/ip4/0.0.0.0/tcp/4004" \
  --peers "/ip4/127.0.0.1/tcp/4001/p2p/NODE1_ID,/ip4/127.0.0.1/tcp/4003/p2p/NODE3_ID"
```

## Troubleshooting

### Common Issues

1. **Port already in use**
   - Change the port number in the address: `--address "/ip4/0.0.0.0/tcp/4005"`

2. **Connection refused**
   - Ensure the bootstrap node is running before starting other nodes
   - Check that the peer ID is correct
   - Verify firewall settings

3. **Authentication failed**
   - Ensure certificate paths are correct
   - Check that nodes are using compatible cluster IDs

4. **Air-specific issues**
   - **Air not found**: Install Air with `go install github.com/cosmtrek/air@latest`
   - **Build errors**: Run `go work sync` to sync workspace dependencies
   - **Multiple restarts**: Air restarts all instances when code changes, this is expected
   - **Stale processes**: Kill with `Ctrl+C` and restart if nodes don't restart cleanly

### Debug Logging

The node outputs connection events and status messages. Look for:
- `🔌 inbound connection from:` - New peer connections
- `❌ disconnected:` - Peer disconnections  
- `No certificate manager configured` - Authentication status

### Testing Connectivity

You can verify nodes are connected by checking the logs for connection messages and ensuring no disconnection events occur.

## Next Steps

Once you have nodes running locally, you can:

1. Implement capsule deployment (coming soon)
2. Test orbit-based execution (coming soon)
3. Experiment with gravity-based node selection (coming soon)
4. Deploy to multiple machines for true distributed testing

For more advanced configuration and deployment options, see the other documentation in the `docs/` directory.