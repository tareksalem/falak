#!/bin/bash

# Air-powered Development Environment Script
# Provides hot-reloading for multi-node development

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
PURPLE='\033[0;35m'
NC='\033[0m' # No Color

# Configuration
NODES_COUNT=${NODES_COUNT:-3}
BASE_PORT=${BASE_PORT:-4001}
LOG_DIR="./logs/air"
PIDS_FILE="./logs/air/pids.txt"

# Function to print colored output
print_status() {
    echo -e "${BLUE}[AIR-DEV]${NC} $1"
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

print_air() {
    echo -e "${PURPLE}[AIR]${NC} $1"
}

# Check if Air is installed
check_air() {
    if ! command -v air &> /dev/null; then
        print_error "Air is not installed!"
        echo "Install Air with one of these commands:"
        echo "  go install github.com/cosmtrek/air@latest"
        echo "  curl -sSfL https://raw.githubusercontent.com/cosmtrek/air/master/install.sh | sh -s -- -b \$(go env GOPATH)/bin"
        exit 1
    fi
    print_success "Air found: $(air -v)"
}

# Create Air configuration for specific node
create_air_config() {
    local node_name=$1
    local port=$2
    local peers=$3
    local extra_args=$4
    local config_file="./logs/air/.air-${node_name}.toml"
    
    # Create node-specific Air config
    cat > "$config_file" << EOF
# Air config for ${node_name}
root = "."
tmp_dir = "logs/air/tmp-${node_name}"

[build]
cmd = "go work sync && go build -o ./logs/air/tmp-${node_name}/${node_name} ./cmd/main.go"
bin = "logs/air/tmp-${node_name}/${node_name}"
full_bin = "./logs/air/tmp-${node_name}/${node_name} --name ${node_name} --address /ip4/0.0.0.0/tcp/${port}${peers}${extra_args}"
include_ext = ["go", "tpl", "tmpl", "html"]
exclude_dir = ["logs", "tmp", "vendor", "dev", ".git"]
include_dir = ["cmd", "internal"]
kill_delay = 500
env = ["GO111MODULE=on", "GOWORK=go.work"]

[log]
time = true

[color]
main = "cyan"
watcher = "yellow"
build = "green"
runner = "magenta"

[misc]
clean_on_exit = true
EOF
    
    echo "$config_file"
}

# Function to cleanup existing processes
cleanup() {
    print_status "Cleaning up existing Air processes..."
    
    # Kill any existing air processes for this project
    pkill -f "air.*falak" 2>/dev/null || true
    pkill -f "tmp.*falak" 2>/dev/null || true
    
    # Clean directories
    rm -rf logs/air/tmp-*
    rm -rf logs/air/.air-*.toml
    rm -f "$PIDS_FILE"
    
    print_success "Cleanup completed"
}

# Function to start bootstrap node with Air
start_bootstrap_with_air() {
    local node_name="bootstrap"
    local port=$BASE_PORT
    local extra_args=" --data-center us-west-1"
    
    mkdir -p "logs/air/tmp-${node_name}"
    
    print_air "Starting ${node_name} with Air on port ${port}..."
    
    # Create Air config
    local config_file=$(create_air_config "$node_name" "$port" "" "$extra_args")
    
    # Start Air in background
    cd "$(pwd)" && air -c "$config_file" > "logs/air/${node_name}.log" 2>&1 &
    local air_pid=$!
    
    echo "${air_pid}:${node_name}" >> "$PIDS_FILE"
    
    print_success "${node_name} started with Air (PID: $air_pid)"
    
    # Wait for bootstrap to initialize and get peer ID
    print_status "Waiting for bootstrap to initialize..."
    local attempts=0
    while [ $attempts -lt 15 ]; do
        if [ -f "logs/air/${node_name}.log" ]; then
            local peer_id=$(grep "Node created successfully with ID:" "logs/air/${node_name}.log" | sed 's/.*ID: \([^ ]*\).*/\1/' | head -1)
            if [ ! -z "$peer_id" ]; then
                echo "$peer_id"
                return 0
            fi
        fi
        sleep 1
        attempts=$((attempts + 1))
    done
    
    print_error "Could not get bootstrap peer ID"
    return 1
}

# Function to start regular node with Air
start_node_with_air() {
    local node_name=$1
    local port=$2
    local bootstrap_peer_id=$3
    local dc=$4
    
    mkdir -p "logs/air/tmp-${node_name}"
    
    local peers=" --peers /ip4/127.0.0.1/tcp/${BASE_PORT}/p2p/${bootstrap_peer_id}"
    local extra_args=" --data-center ${dc}"
    
    print_air "Starting ${node_name} with Air on port ${port}..."
    
    # Create Air config
    local config_file=$(create_air_config "$node_name" "$port" "$peers" "$extra_args")
    
    # Start Air in background  
    cd "$(pwd)" && air -c "$config_file" > "logs/air/${node_name}.log" 2>&1 &
    local air_pid=$!
    
    echo "${air_pid}:${node_name}" >> "$PIDS_FILE"
    
    print_success "${node_name} started with Air (PID: $air_pid)"
    
    sleep 2
}

# Function to show real-time logs
show_logs() {
    local node_name=$1
    
    if [ -z "$node_name" ]; then
        print_status "Available nodes:"
        if [ -f "$PIDS_FILE" ]; then
            cat "$PIDS_FILE" | cut -d: -f2
        else
            echo "No nodes running"
        fi
        return
    fi
    
    local log_file="logs/air/${node_name}.log"
    if [ -f "$log_file" ]; then
        print_status "Showing real-time logs for ${node_name} (Ctrl+C to exit):"
        tail -f "$log_file"
    else
        print_error "Log file not found: $log_file"
    fi
}

# Function to show status
show_status() {
    print_status "Air Development Environment Status:"
    echo "===================================="
    
    if [ ! -f "$PIDS_FILE" ]; then
        echo "No nodes running"
        return
    fi
    
    while IFS=':' read -r pid node_name; do
        if kill -0 "$pid" 2>/dev/null; then
            # Extract port from log file
            local port_info=""
            if [ -f "logs/air/${node_name}.log" ]; then
                local port=$(grep -o "tcp/[0-9]*" "logs/air/${node_name}.log" | head -1 | cut -d'/' -f2)
                if [ ! -z "$port" ]; then
                    port_info=" (Port: $port)"
                fi
            fi
            echo -e "${GREEN}✓${NC} ${node_name} - Air PID: ${pid}${port_info} - Hot reloading active"
        else
            echo -e "${RED}✗${NC} ${node_name} - Air process dead (PID: ${pid})"
        fi
    done < "$PIDS_FILE"
    echo ""
}

# Function to stop all Air processes
stop_all() {
    print_status "Stopping all Air development processes..."
    
    if [ -f "$PIDS_FILE" ]; then
        while IFS=':' read -r pid node_name; do
            if kill -0 "$pid" 2>/dev/null; then
                print_status "Stopping Air for ${node_name} (PID: ${pid})..."
                kill "$pid"
                
                # Wait for graceful shutdown
                local attempts=0
                while kill -0 "$pid" 2>/dev/null && [ $attempts -lt 5 ]; do
                    sleep 1
                    attempts=$((attempts + 1))
                done
                
                # Force kill if still running
                if kill -0 "$pid" 2>/dev/null; then
                    print_warning "Force killing Air for ${node_name}..."
                    kill -9 "$pid"
                fi
                
                print_success "Stopped Air for ${node_name}"
            fi
        done < "$PIDS_FILE"
    fi
    
    # Final cleanup
    cleanup
}

# Function to start the Air development environment
start_env() {
    print_air "Starting Falak Air Development Environment with ${NODES_COUNT} nodes..."
    print_air "🔥 Hot reloading enabled - code changes will automatically restart nodes"
    
    # Setup
    check_air
    cleanup
    mkdir -p "$LOG_DIR"
    
    # Start bootstrap node with Air
    local bootstrap_peer_id=$(start_bootstrap_with_air)
    if [ -z "$bootstrap_peer_id" ]; then
        print_error "Failed to start bootstrap node"
        exit 1
    fi
    
    print_success "Bootstrap peer ID: $bootstrap_peer_id"
    
    # Start remaining nodes with Air
    for i in $(seq 2 $NODES_COUNT); do
        local port=$((BASE_PORT + i - 1))
        local node_name="node${i}"
        local dc="us-east-1"
        
        case $i in
            3) dc="eu-west-1" ;;
            4) dc="us-west-2" ;;
            5) dc="ap-south-1" ;;
        esac
        
        start_node_with_air "$node_name" "$port" "$bootstrap_peer_id" "$dc"
        sleep 1
    done
    
    print_success "🔥 Air development environment started successfully!"
    echo ""
    show_status
    
    print_air "Development Features:"
    echo "  🔥 Hot Reloading: Edit any .go file and nodes will restart automatically"
    echo "  📊 Individual Logs: Each node has separate Air output"
    echo "  ⚡ Fast Rebuilds: Air only rebuilds when files change"
    echo "  🔄 Auto Reconnect: Nodes reconnect after restarts"
    echo ""
    print_status "Useful commands:"
    echo "  $0 status              - Show node status"
    echo "  $0 logs [node_name]    - Show real-time logs"
    echo "  $0 stop                - Stop all Air processes"
    echo ""
    print_status "Log files are in: ${LOG_DIR}/"
    print_air "🎯 Make changes to any .go file and watch the magic happen!"
}

# Function to monitor all logs
monitor_all() {
    print_status "Monitoring all Air logs (Ctrl+C to exit)..."
    
    if [ ! -f "$PIDS_FILE" ]; then
        print_error "No Air processes running"
        exit 1
    fi
    
    # Get all log files
    local log_files=""
    while IFS=':' read -r pid node_name; do
        if [ -f "logs/air/${node_name}.log" ]; then
            log_files="$log_files logs/air/${node_name}.log"
        fi
    done < "$PIDS_FILE"
    
    if [ ! -z "$log_files" ]; then
        tail -f $log_files
    else
        print_error "No log files found"
    fi
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
        stop_all
        sleep 2
        start_env
        ;;
    status)
        show_status
        ;;
    logs)
        show_logs $2
        ;;
    monitor)
        monitor_all
        ;;
    clean)
        cleanup
        ;;
    help|--help|-h)
        echo "Falak Air Development Environment Manager"
        echo ""
        echo "🔥 Hot-reloading multi-node development environment powered by Air"
        echo ""
        echo "Usage: $0 [COMMAND]"
        echo ""
        echo "Commands:"
        echo "  start           Start Air development environment (default)"
        echo "  stop            Stop all Air processes"
        echo "  restart         Restart the environment"
        echo "  status          Show Air process status"
        echo "  logs [NODE]     Show logs for specific node"
        echo "  monitor         Monitor all logs simultaneously"
        echo "  clean           Clean up Air files and processes"
        echo "  help            Show this help message"
        echo ""
        echo "Environment Variables:"
        echo "  NODES_COUNT     Number of nodes to start (default: 3)"
        echo "  BASE_PORT       Base port number (default: 4001)"
        echo ""
        echo "Features:"
        echo "  🔥 Hot Reloading    - Automatic restart on code changes"
        echo "  📊 Individual Logs  - Separate logs per node"
        echo "  ⚡ Fast Rebuilds   - Only rebuilds when files change"
        echo "  🔄 Auto Reconnect  - Nodes reconnect after restarts"
        echo ""
        echo "Examples:"
        echo "  $0 start                    # Start 3-node Air environment"
        echo "  NODES_COUNT=5 $0 start      # Start 5-node environment"
        echo "  $0 logs bootstrap           # Show bootstrap logs"
        echo "  $0 monitor                  # Monitor all logs"
        echo ""
        echo "Development Workflow:"
        echo "  1. $0 start                 # Start environment"
        echo "  2. Edit any .go file        # Air detects changes"
        echo "  3. Watch automatic restart  # Nodes rebuild & reconnect"
        echo "  4. $0 logs [node]          # Debug if needed"
        ;;
    *)
        print_error "Unknown command: $1"
        echo "Use '$0 help' for usage information"
        exit 1
        ;;
esac