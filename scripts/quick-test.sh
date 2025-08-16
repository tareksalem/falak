#!/bin/bash

# Quick Test Environment Script
# Simpler alternative to dev-env.sh for basic testing

set -e

# Configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
NODES_COUNT=${NODES_COUNT:-2}
BASE_PORT=${BASE_PORT:-4001}

cd "$PROJECT_ROOT"

# Colors
GREEN='\033[0;32m'
BLUE='\033[0;34m'
RED='\033[0;31m'
NC='\033[0m'

echo -e "${BLUE}🚀 Starting Falak Quick Test Environment${NC}"

# Build project
echo -e "${BLUE}📦 Building project...${NC}"
go work sync
mkdir -p tmp
go build -o ./tmp/falak ./cmd/main.go

# Start bootstrap node in background
echo -e "${BLUE}🌟 Starting bootstrap node...${NC}"
./tmp/falak --name bootstrap --address "/ip4/0.0.0.0/tcp/$BASE_PORT" > tmp/bootstrap.log 2>&1 &
BOOTSTRAP_PID=$!
echo "Bootstrap PID: $BOOTSTRAP_PID"

# Wait for bootstrap to start and extract peer ID
sleep 3
PEER_ID=$(grep -o "Node created successfully with ID: [A-Za-z0-9]*" tmp/bootstrap.log | cut -d' ' -f6 | head -1)

if [ -z "$PEER_ID" ]; then
    echo -e "${RED}❌ Failed to get bootstrap peer ID${NC}"
    kill $BOOTSTRAP_PID 2>/dev/null || true
    exit 1
fi

echo -e "${GREEN}✅ Bootstrap node started with ID: $PEER_ID${NC}"

# Start additional nodes
for i in $(seq 2 $NODES_COUNT); do
    port=$((BASE_PORT + i - 1))
    echo -e "${BLUE}🔗 Starting node${i} on port $port...${NC}"
    
    ./tmp/falak --name "node${i}" --address "/ip4/0.0.0.0/tcp/$port" \
        --peers "/ip4/127.0.0.1/tcp/$BASE_PORT/p2p/$PEER_ID" \
        > "tmp/node${i}.log" 2>&1 &
    
    eval "NODE${i}_PID=\$!"
    echo "Node${i} PID: $(eval echo \$NODE${i}_PID)"
    sleep 2
done

echo -e "${GREEN}🎉 All nodes started successfully!${NC}"
echo ""
echo "📊 Node Information:"
echo "  Bootstrap: localhost:$BASE_PORT (PID: $BOOTSTRAP_PID)"
for i in $(seq 2 $NODES_COUNT); do
    port=$((BASE_PORT + i - 1))
    pid=$(eval echo \$NODE${i}_PID)
    echo "  Node${i}: localhost:$port (PID: $pid)"
done

echo ""
echo "📋 Useful commands:"
echo "  tail -f tmp/bootstrap.log    # View bootstrap logs"
echo "  tail -f tmp/node2.log        # View node2 logs"
echo "  ps aux | grep falak          # See running processes"
echo ""
echo "🛑 To stop all nodes:"
echo "  kill $BOOTSTRAP_PID $(for i in $(seq 2 $NODES_COUNT); do eval echo \$NODE${i}_PID; done | tr '\n' ' ')"

# Wait for user input
echo "Press Ctrl+C to stop all nodes..."
trap "echo 'Stopping all nodes...'; kill $BOOTSTRAP_PID $(for i in $(seq 2 $NODES_COUNT); do eval echo \$NODE${i}_PID; done | tr '\n' ' ') 2>/dev/null || true; exit 0" INT

# Keep script running
wait