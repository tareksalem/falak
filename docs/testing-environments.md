# Testing Environments Guide

This guide covers the various automated testing environments available for Falak development.

## 🎯 Overview

Falak provides multiple ways to set up test environments for different development scenarios:

| Tool | Use Case | Complexity | Isolation |
|------|----------|------------|-----------|
| **Quick Test** | Rapid development/debugging | Low | Process-level |
| **Dev Environment** | Feature development | Medium | Process-level |
| **Docker Environment** | CI/CD, Integration testing | High | Container-level |
| **Air + Manual** | Live development | Low | Process-level |

## 🚀 Quick Start Options

### 1. Makefile Commands (Recommended)

```bash
# Quick 2-node setup
make quick-test

# Full development environment (3 nodes)
make dev-env

# Docker-based testing
make docker-test

# Run all tests
make test
```

### 2. Direct Script Usage

```bash
# Simple 2-node environment
./scripts/quick-test.sh

# Advanced multi-node environment
./scripts/dev-env.sh start

# Docker environment
./scripts/docker-test.sh start
```

## 📋 Detailed Environment Options

### 🏃‍♂️ Quick Test Environment

**Best for**: Rapid prototyping, simple debugging

```bash
# Start quick environment
make quick-test
# OR
./scripts/quick-test.sh

# Customize
NODES_COUNT=4 BASE_PORT=5001 ./scripts/quick-test.sh
```

**Features**:
- ✅ 2 nodes by default (configurable)
- ✅ Automatic peer discovery
- ✅ Background processes
- ✅ Simple log files
- ✅ Easy cleanup with Ctrl+C

**Output**:
```
🚀 Starting Falak Quick Test Environment
📦 Building project...
🌟 Starting bootstrap node...
✅ Bootstrap node started with ID: 12D3KooW...
🔗 Starting node2 on port 4002...
🎉 All nodes started successfully!

📊 Node Information:
  Bootstrap: localhost:4001 (PID: 12345)
  Node2: localhost:4002 (PID: 12346)
```

### 🛠️ Development Environment

**Best for**: Feature development, multi-node testing

```bash
# Start dev environment
make dev-env
# OR
./scripts/dev-env.sh start

# Customize
NODES_COUNT=5 BASE_PORT=6001 ./scripts/dev-env.sh start
```

**Features**:
- ✅ 3+ nodes with different data centers
- ✅ Advanced logging and monitoring
- ✅ Health checks and status reporting
- ✅ Integration test runner
- ✅ Persistent logs

**Commands**:
```bash
# Management
./scripts/dev-env.sh status    # Show node status
./scripts/dev-env.sh logs      # Show all logs
./scripts/dev-env.sh logs node1 # Show specific node logs
./scripts/dev-env.sh stop      # Stop all nodes
./scripts/dev-env.sh restart   # Restart environment
./scripts/dev-env.sh test      # Run integration tests
```

### 🐳 Docker Environment

**Best for**: CI/CD, isolated testing, production simulation

```bash
# Start Docker environment
make docker-test
# OR
./scripts/docker-test.sh start
```

**Features**:
- ✅ Complete isolation
- ✅ Network simulation
- ✅ Health checks
- ✅ Volume mounting for logs
- ✅ Multi-stage builds

**Services**:
- `bootstrap` - Bootstrap node (port 4001)
- `node2` - Second node (port 4002) 
- `node3` - Third node (port 4003)
- `node4` - Fourth node (port 4004)
- `test-runner` - Integration test runner

**Commands**:
```bash
# Management
./scripts/docker-test.sh status           # Show container status
./scripts/docker-test.sh logs bootstrap   # Show bootstrap logs
./scripts/docker-test.sh exec bootstrap   # Shell into container
./scripts/docker-test.sh test             # Run integration tests
./scripts/docker-test.sh clean            # Full cleanup
```

## 🔄 Development Workflows

### Rapid Development Cycle

```bash
# 1. Start quick environment
make quick-test

# 2. Make code changes
# Edit files in cmd/ or internal/

# 3. Test changes
# The quick environment will need manual restart
# Ctrl+C and run again, or use Air for hot reloading
```

### Feature Development Cycle

```bash
# 1. Start development environment
make dev-env

# 2. Check status
make dev-env-status

# 3. Make changes and test
./scripts/dev-env.sh test

# 4. View logs
make dev-env-logs

# 5. Restart when needed
make dev-env-restart
```

### CI/CD Integration

```bash
# In your CI pipeline
make docker-test           # Start environment
make docker-integration    # Run integration tests
make clean-docker          # Cleanup
```

## 🔧 Configuration Options

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `NODES_COUNT` | 3 | Number of nodes to start |
| `BASE_PORT` | 4001 | Starting port number |

### Examples

```bash
# Start 5-node environment on different ports
NODES_COUNT=5 BASE_PORT=5001 make dev-env

# Quick test with 4 nodes
NODES_COUNT=4 make quick-test

# Docker with custom configuration
NODES_COUNT=6 docker-compose up --scale node2=3
```

## 📊 Monitoring and Debugging

### Log Files

```bash
# Quick test logs
tail -f tmp/bootstrap.log
tail -f tmp/node2.log

# Dev environment logs
tail -f logs/node1.log
tail -f logs/node2.log

# Docker logs
docker-compose logs -f bootstrap
docker-compose logs -f node2
```

### Status Monitoring

```bash
# Dev environment status
./scripts/dev-env.sh status

# Docker status
./scripts/docker-test.sh status

# Process monitoring
ps aux | grep falak
```

### Health Checks

```bash
# Check if nodes are connected
grep "inbound connection" logs/*.log

# Check for errors
grep "ERROR" logs/*.log

# Monitor peer connections
grep "Connected to peer" logs/*.log
```

## 🧪 Testing Integration

### Unit Tests

```bash
make test-unit              # Run unit tests
make test-coverage          # Generate coverage report
```

### Integration Tests

```bash
make test-integration       # Run with dev environment
make docker-integration     # Run with Docker environment
```

### Custom Tests

```bash
# Run tests against running environment
cd internal/node
go test ./tests/ -v -timeout=60s

# Run specific test
go test ./tests/ -run TestNodeConnection -v
```

## 🛡️ Troubleshooting

### Common Issues

1. **Port conflicts**
   ```bash
   # Use different base port
   BASE_PORT=5001 make dev-env
   ```

2. **Build failures**
   ```bash
   # Clean and rebuild
   make clean
   make build
   ```

3. **Connection issues**
   ```bash
   # Check logs
   make dev-env-logs
   
   # Restart environment
   make dev-env-restart
   ```

4. **Docker issues**
   ```bash
   # Clean Docker resources
   make clean-docker
   
   # Rebuild images
   docker-compose build --no-cache
   ```

### Debug Commands

```bash
# Show all running processes
ps aux | grep falak

# Show network connections
netstat -tulpn | grep :400[1-9]

# Check Docker containers
docker ps -a

# Show Docker logs
docker-compose logs --tail=50
```

## 🎯 Best Practices

### Development

1. **Use Quick Test** for rapid iteration
2. **Use Dev Environment** for feature work
3. **Use Docker** for final testing before PR

### Testing

1. **Run unit tests first** - `make test-unit`
2. **Test with integration environment** - `make test-integration`
3. **Verify with Docker** - `make docker-integration`

### Debugging

1. **Check logs first** - `make dev-env-logs`
2. **Use status commands** - `make dev-env-status`
3. **Clean environment** - `make clean` when in doubt

## 📚 Examples

### Example 1: Quick Development Loop

```bash
# Start environment
make quick-test

# Make changes to internal/node/node.go
# Ctrl+C to stop, then restart
make quick-test

# Test changes
tail -f tmp/bootstrap.log
```

### Example 2: Feature Development

```bash
# Start full environment
make dev-env

# Make changes
vim internal/node/peer.go

# Test changes
./scripts/dev-env.sh test

# Check specific node
./scripts/dev-env.sh logs node2
```

### Example 3: Integration Testing

```bash
# Start Docker environment
make docker-test

# Run integration tests
make docker-integration

# Debug issues
./scripts/docker-test.sh logs bootstrap
./scripts/docker-test.sh exec bootstrap
```

This testing environment setup provides comprehensive coverage for all development scenarios while maintaining simplicity and ease of use.