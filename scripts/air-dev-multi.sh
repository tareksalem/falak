#!/bin/bash

# Simple multi-node Air development environment
set -e

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
BLUE='\033[0;34m'
PURPLE='\033[0;35m'
NC='\033[0m'

# Configuration
NODES_COUNT=${NODES_COUNT:-3}
BASE_PORT=${BASE_PORT:-4001}

# Function to stop all nodes
stop_all() {
    echo -e "${RED}Stopping all Air processes...${NC}"
    if [ -f "logs/air/pids.txt" ]; then
        while IFS=':' read -r pid name port; do
            if kill -0 "$pid" 2>/dev/null; then
                echo "  Stopping ${name} (PID: ${pid})..."
                kill "$pid" 2>/dev/null || true
            fi
        done < "logs/air/pids.txt"
    fi
    pkill -f "air.*falak" 2>/dev/null || true
    rm -rf logs/air 2>/dev/null || true
    echo -e "${GREEN}✅ All processes stopped${NC}"
    exit 0
}

# Handle stop command
if [ "$1" = "stop" ]; then
    stop_all
fi

echo -e "${PURPLE}🔥 Starting Falak Air Development - ${NODES_COUNT} nodes with hot reloading${NC}"

# Clean up
pkill -f "air.*falak" 2>/dev/null || true
rm -rf logs/air 2>/dev/null || true
mkdir -p logs/air

# Function to create and start node
start_node() {
    local node_num=$1
    local node_name="node${node_num}"
    local port=$((BASE_PORT + node_num - 1))
    local peers_arg=""
    
    # If not the first node, connect to node1
    if [ $node_num -gt 1 ]; then
        # Wait a bit for node1 to be ready and get its peer ID
        if [ -f "logs/air/node1_peer_id.txt" ]; then
            local node1_peer_id=$(cat logs/air/node1_peer_id.txt)
            peers_arg=" --peers /ip4/127.0.0.1/tcp/${BASE_PORT}/p2p/${node1_peer_id}"
        fi
    fi
    
    # Create air config
    cat > "logs/air/.air-${node_name}.toml" << EOF
root = "."
tmp_dir = "logs/air/tmp-${node_name}"

[build]
cmd = "cd cmd && go build -o ../logs/air/tmp-${node_name}/main ."
bin = "logs/air/tmp-${node_name}/main"
full_bin = "./logs/air/tmp-${node_name}/main --name ${node_name} --address /ip4/0.0.0.0/tcp/${port}${peers_arg}"
include_ext = ["go"]
exclude_dir = ["logs", "tmp", "vendor", "dev", ".git", "scripts", "docs"]
include_dir = ["cmd", "internal", "pkg"]
kill_delay = 500

[log]
time = true

[misc]
clean_on_exit = false
EOF

    # Start air in background
    echo -e "${BLUE}Starting ${node_name} on port ${port}...${NC}"
    air -c "logs/air/.air-${node_name}.toml" > "logs/air/${node_name}.log" 2>&1 &
    local pid=$!
    echo "${pid}:${node_name}:${port}" >> logs/air/pids.txt
    
    # If this is node1, extract and save its peer ID
    if [ $node_num -eq 1 ]; then
        echo -e "${BLUE}Waiting for node1 to initialize...${NC}"
        local attempts=0
        while [ $attempts -lt 10 ]; do
            if [ -f "logs/air/${node_name}.log" ]; then
                local peer_id=$(grep "Node created successfully with ID:" "logs/air/${node_name}.log" | sed 's/.*ID: \([^ ]*\).*/\1/' | tail -1)
                if [ ! -z "$peer_id" ]; then
                    echo "$peer_id" > logs/air/node1_peer_id.txt
                    echo -e "${GREEN}Node1 peer ID: ${peer_id}${NC}"
                    break
                fi
            fi
            sleep 1
            attempts=$((attempts + 1))
        done
    fi
}

# Start all nodes
for i in $(seq 1 $NODES_COUNT); do
    start_node $i
    sleep 2
done

echo -e "${GREEN}✅ All nodes started!${NC}"
echo ""
echo "📊 Node Status:"
while IFS=':' read -r pid name port; do
    echo "  - ${name} (PID: ${pid}, Port: ${port})"
done < logs/air/pids.txt

echo ""
echo -e "${PURPLE}🔥 Edit any .go file and watch nodes auto-restart!${NC}"
echo ""
echo "📝 Available interfaces:"
echo "  1. TUI Monitor (split-screen):     ./scripts/tui-monitor.sh"
echo "  2. Simple Interactive:             ./scripts/interactive-pubsub.sh"
echo "  3. Raw logs:                       tail -f logs/air/*.log"
echo "  4. Stop all nodes:                 ./scripts/air-dev-multi.sh stop"
echo ""
echo -e "${CYAN}Starting TUI Monitor (split-screen interface)...${NC}"
echo "─────────────────────────────────────────────────────────"

# Start TUI monitor
./scripts/tui-monitor.sh