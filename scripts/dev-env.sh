#!/bin/bash

# Falak Development Environment Setup Script
# This script creates a multi-node test environment for development

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
NODES_COUNT=${NODES_COUNT:-3}
BASE_PORT=${BASE_PORT:-4001}
NETWORK_NAME="falak-testnet"
LOG_DIR="./logs"

# Function to print colored output
print_status() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

print_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

print_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Function to cleanup existing processes
cleanup() {
    print_status "Cleaning up existing processes..."
    
    # Kill any existing falak processes
    pkill -f "tmp/main" 2>/dev/null || true
    pkill -f "falak" 2>/dev/null || true
    
    # Clean tmp directory
    rm -rf tmp/
    rm -rf ${LOG_DIR}/
    
    print_success "Cleanup completed"
}

# Function to build the project
build_project() {
    print_status "Building Falak project..."
    
    # Sync workspace and build
    go work sync
    mkdir -p tmp
    go build -o ./tmp/falak ./cmd/main.go
    
    if [ ! -f "./tmp/falak" ]; then
        print_error "Build failed - falak binary not found"
        exit 1
    fi
    
    print_success "Build completed successfully"
}

# Function to start a node
start_node() {
    local node_name=$1
    local port=$2
    local peers=$3
    local extra_args=$4
    
    mkdir -p ${LOG_DIR}
    
    local cmd="./tmp/falak --name ${node_name} --address \"/ip4/0.0.0.0/tcp/${port}\""
    
    if [ ! -z "$peers" ]; then
        cmd="${cmd} --peers \"${peers}\""
    fi
    
    if [ ! -z "$extra_args" ]; then
        cmd="${cmd} ${extra_args}"
    fi
    
    print_status "Starting ${node_name} on port ${port}..."
    
    # Start node in background and redirect output to log file
    eval "${cmd}" > ${LOG_DIR}/${node_name}.log 2>&1 &
    local pid=$!
    
    # Store PID for later cleanup
    echo $pid > ${LOG_DIR}/${node_name}.pid
    
    # Wait a moment for node to start
    sleep 2
    
    # Check if process is still running
    if ! kill -0 $pid 2>/dev/null; then
        print_error "Failed to start ${node_name}"
        cat ${LOG_DIR}/${node_name}.log
        exit 1
    fi
    
    print_success "${node_name} started successfully (PID: $pid)"
}

# Function to get peer ID from log
get_peer_id() {
    local node_name=$1
    local log_file="${LOG_DIR}/${node_name}.log"
    
    # Wait for peer ID to appear in log
    local attempts=0
    while [ $attempts -lt 10 ]; do
        if [ -f "$log_file" ]; then
            local peer_id=$(grep -o "Node created successfully with ID: [A-Za-z0-9]*" "$log_file" | cut -d' ' -f6 | head -1)
            if [ ! -z "$peer_id" ]; then
                echo "$peer_id"
                return 0
            fi
        fi
        sleep 1
        attempts=$((attempts + 1))
    done
    
    print_error "Could not extract peer ID for ${node_name}"
    return 1
}

# Function to show node status
show_status() {
    print_status "Node Status:"
    echo "============"
    
    for i in $(seq 1 $NODES_COUNT); do
        local node_name="node${i}"
        local pid_file="${LOG_DIR}/${node_name}.pid"
        
        if [ -f "$pid_file" ]; then
            local pid=$(cat "$pid_file")
            if kill -0 $pid 2>/dev/null; then
                local port=$((BASE_PORT + i - 1))
                echo -e "${GREEN}✓${NC} ${node_name} (PID: $pid, Port: $port)"
            else
                echo -e "${RED}✗${NC} ${node_name} (Dead)"
            fi
        else
            echo -e "${RED}✗${NC} ${node_name} (Not started)"
        fi
    done
    echo ""
}

# Function to show logs
show_logs() {
    local node_name=$1
    
    if [ -z "$node_name" ]; then
        print_status "Available logs:"
        ls -la ${LOG_DIR}/*.log 2>/dev/null || echo "No logs found"
        return
    fi
    
    local log_file="${LOG_DIR}/${node_name}.log"
    if [ -f "$log_file" ]; then
        print_status "Showing logs for ${node_name}:"
        tail -f "$log_file"
    else
        print_error "Log file not found: $log_file"
    fi
}

# Function to stop all nodes
stop_all() {
    print_status "Stopping all nodes..."
    
    for pid_file in ${LOG_DIR}/*.pid; do
        if [ -f "$pid_file" ]; then
            local pid=$(cat "$pid_file")
            local node_name=$(basename "$pid_file" .pid)
            
            if kill -0 $pid 2>/dev/null; then
                print_status "Stopping ${node_name} (PID: $pid)..."
                kill $pid
                
                # Wait for graceful shutdown
                local attempts=0
                while kill -0 $pid 2>/dev/null && [ $attempts -lt 5 ]; do
                    sleep 1
                    attempts=$((attempts + 1))
                done
                
                # Force kill if still running
                if kill -0 $pid 2>/dev/null; then
                    print_warning "Force killing ${node_name}..."
                    kill -9 $pid
                fi
                
                print_success "${node_name} stopped"
            fi
            
            rm -f "$pid_file"
        fi
    done
}

# Function to start the test environment
start_env() {
    print_status "Starting Falak test environment with ${NODES_COUNT} nodes..."
    
    # Clean up any existing setup
    cleanup
    
    # Build the project
    build_project
    
    # Start bootstrap node
    start_node "node1" $BASE_PORT "" "--data-center us-west-1"
    
    # Get bootstrap peer ID
    local bootstrap_peer_id=$(get_peer_id "node1")
    local bootstrap_addr="/ip4/127.0.0.1/tcp/${BASE_PORT}/p2p/${bootstrap_peer_id}"
    
    print_success "Bootstrap node peer ID: $bootstrap_peer_id"
    
    # Start remaining nodes
    for i in $(seq 2 $NODES_COUNT); do
        local port=$((BASE_PORT + i - 1))
        local node_name="node${i}"
        local dc="us-east-1"
        
        if [ $i -eq 3 ]; then
            dc="eu-west-1"
        fi
        
        start_node "$node_name" $port "$bootstrap_addr" "--data-center $dc"
        
        # Small delay between nodes
        sleep 1
    done
    
    print_success "Test environment started successfully!"
    echo ""
    show_status
    
    print_status "Useful commands:"
    echo "  $0 status              - Show node status"
    echo "  $0 logs [node_name]    - Show logs (tail -f)"
    echo "  $0 stop                - Stop all nodes"
    echo "  $0 restart             - Restart environment"
    echo ""
    print_status "Log files are in: ${LOG_DIR}/"
    print_status "To connect to node1: /ip4/127.0.0.1/tcp/${BASE_PORT}/p2p/${bootstrap_peer_id}"
}

# Function to restart environment
restart_env() {
    stop_all
    sleep 2
    start_env
}

# Function to run tests
run_tests() {
    print_status "Running integration tests..."
    
    # Check if environment is running
    if [ ! -f "${LOG_DIR}/node1.pid" ]; then
        print_error "Test environment not running. Start it first with: $0 start"
        exit 1
    fi
    
    # Run node tests
    cd internal/node
    go test ./tests/ -v -timeout=30s
    cd ../..
    
    print_success "Tests completed"
}

# Main script logic
case "${1:-start}" in
    start)
        start_env
        ;;
    stop)
        stop_all
        ;;
    restart)
        restart_env
        ;;
    status)
        show_status
        ;;
    logs)
        show_logs $2
        ;;
    test)
        run_tests
        ;;
    clean)
        cleanup
        ;;
    help|--help|-h)
        echo "Falak Development Environment Manager"
        echo ""
        echo "Usage: $0 [COMMAND]"
        echo ""
        echo "Commands:"
        echo "  start     Start the test environment (default)"
        echo "  stop      Stop all nodes"
        echo "  restart   Restart the environment"
        echo "  status    Show node status"
        echo "  logs      Show logs for all nodes"
        echo "  logs NODE Show logs for specific node (e.g., node1)"
        echo "  test      Run integration tests"
        echo "  clean     Clean up files and processes"
        echo "  help      Show this help message"
        echo ""
        echo "Environment Variables:"
        echo "  NODES_COUNT  Number of nodes to start (default: 3)"
        echo "  BASE_PORT    Base port number (default: 4001)"
        echo ""
        echo "Examples:"
        echo "  $0 start                    # Start 3 nodes"
        echo "  NODES_COUNT=5 $0 start      # Start 5 nodes"
        echo "  $0 logs node1               # Show node1 logs"
        echo "  BASE_PORT=5001 $0 start     # Start on different ports"
        ;;
    *)
        print_error "Unknown command: $1"
        echo "Use '$0 help' for usage information"
        exit 1
        ;;
esac