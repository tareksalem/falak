#!/bin/bash

# Falak PubSub Interactive Test Tool
# Sends messages to the running nodes via a test topic

set -e

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
PURPLE='\033[0;35m'
CYAN='\033[0;36m'
NC='\033[0m'

# Configuration
BASE_PORT=${BASE_PORT:-4001}
TOPIC="/falak/test/messages"

print_header() {
    clear
    echo -e "${PURPLE}╔══════════════════════════════════════════════════╗${NC}"
    echo -e "${PURPLE}║       Falak PubSub Interactive Test Tool        ║${NC}"
    echo -e "${PURPLE}╚══════════════════════════════════════════════════╝${NC}"
    echo ""
}

print_status() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

print_success() {
    echo -e "${GREEN}[✓]${NC} $1"
}

print_error() {
    echo -e "${RED}[✗]${NC} $1"
}

check_nodes() {
    if [ ! -f "logs/air/pids.txt" ]; then
        print_error "No nodes running. Start with: make air-dev"
        exit 1
    fi
    
    local count=0
    while IFS=':' read -r pid name port; do
        if kill -0 "$pid" 2>/dev/null; then
            count=$((count + 1))
        fi
    done < "logs/air/pids.txt"
    
    if [ $count -eq 0 ]; then
        print_error "No nodes running. Start with: make air-dev"
        exit 1
    fi
    
    echo -e "${GREEN}✓ Found $count running nodes${NC}"
    return $count
}

show_instructions() {
    echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${YELLOW}Instructions:${NC}"
    echo "  • Messages will be broadcast to all nodes via pubsub"
    echo "  • Watch the node logs to see messages propagate"
    echo "  • Type 'quit' or press Ctrl+C to exit"
    echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo ""
}

send_test_message() {
    local node_port=$1
    local message=$2
    
    # This would normally use an HTTP API or gRPC endpoint
    # For now, we'll create a simple test client
    echo -e "${BLUE}→ Sending: \"$message\" via node on port $node_port${NC}"
    
    # Create a temporary Go program to send the message
    cat > /tmp/pubsub-sender.go << 'EOF'
package main

import (
    "context"
    "fmt"
    "os"
    "time"
    libp2p "github.com/libp2p/go-libp2p"
    pubsub "github.com/libp2p/go-libp2p-pubsub"
    "github.com/libp2p/go-libp2p/core/peer"
    ma "github.com/multiformats/go-multiaddr"
)

func main() {
    if len(os.Args) < 4 {
        fmt.Println("Usage: pubsub-sender <peer-addr> <topic> <message>")
        os.Exit(1)
    }
    
    ctx := context.Background()
    
    // Create a new libp2p host
    h, err := libp2p.New(
        libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"),
    )
    if err != nil {
        panic(err)
    }
    defer h.Close()
    
    // Create pubsub
    ps, err := pubsub.NewGossipSub(ctx, h)
    if err != nil {
        panic(err)
    }
    
    // Parse peer address and connect
    peerAddr, err := ma.NewMultiaddr(os.Args[1])
    if err != nil {
        panic(err)
    }
    
    peerInfo, err := peer.AddrInfoFromP2pAddr(peerAddr)
    if err != nil {
        panic(err)
    }
    
    if err := h.Connect(ctx, *peerInfo); err != nil {
        fmt.Printf("Failed to connect: %v\n", err)
    }
    
    // Join topic and publish
    topic, err := ps.Join(os.Args[2])
    if err != nil {
        panic(err)
    }
    defer topic.Close()
    
    time.Sleep(2 * time.Second) // Wait for mesh to form
    
    if err := topic.Publish(ctx, []byte(os.Args[3])); err != nil {
        panic(err)
    }
    
    fmt.Println("Message sent!")
    time.Sleep(1 * time.Second) // Give time for propagation
}
EOF
    
    # Build and run the sender
    cd /tmp && go mod init pubsub-sender 2>/dev/null || true
    go get github.com/libp2p/go-libp2p@latest 2>/dev/null
    go get github.com/libp2p/go-libp2p-pubsub@latest 2>/dev/null
    go build -o pubsub-sender pubsub-sender.go 2>/dev/null
    
    # Get the peer ID of the first node
    if [ -f "logs/air/node1_peer_id.txt" ]; then
        local peer_id=$(cat logs/air/node1_peer_id.txt)
        ./pubsub-sender "/ip4/127.0.0.1/tcp/$node_port/p2p/$peer_id" "$TOPIC" "$message" 2>/dev/null || true
    fi
    
    rm -f /tmp/pubsub-sender /tmp/pubsub-sender.go /tmp/go.mod /tmp/go.sum 2>/dev/null || true
}

# Main interactive loop
main() {
    print_header
    check_nodes
    show_instructions
    
    # Start monitoring logs in background
    echo -e "${CYAN}📡 Monitoring node logs for pubsub messages...${NC}"
    echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    
    # Show recent log entries with pubsub messages
    tail -f logs/air/*.log | grep -E "Received message|inbound|Topic" --line-buffered &
    TAIL_PID=$!
    
    trap "kill $TAIL_PID 2>/dev/null; exit" INT TERM
    
    echo ""
    echo -e "${GREEN}Ready to send messages!${NC}"
    echo ""
    
    while true; do
        echo -ne "${YELLOW}Enter message (or 'quit' to exit):${NC} "
        read -r message
        
        if [ "$message" = "quit" ] || [ "$message" = "exit" ]; then
            break
        fi
        
        if [ -z "$message" ]; then
            continue
        fi
        
        # Send to first node
        send_test_message $BASE_PORT "$message"
        echo ""
    done
    
    kill $TAIL_PID 2>/dev/null || true
    echo -e "${GREEN}Goodbye!${NC}"
}

# Handle simple pubsub test without external dependencies
simple_test() {
    print_header
    check_nodes
    
    echo -e "${YELLOW}Simple PubSub Test Mode${NC}"
    echo "Since nodes need HTTP/gRPC endpoints for external messages,"
    echo "this will demonstrate the concept. To fully test:"
    echo ""
    echo "1. Add an HTTP endpoint to nodes for receiving test messages"
    echo "2. Or add a CLI flag to enable interactive mode"
    echo ""
    echo -e "${CYAN}Current Implementation:${NC}"
    echo "  • Nodes auto-join topic: /falak/default/members/joined"
    echo "  • Messages are logged when received"
    echo "  • You can see connections in the logs"
    echo ""
    echo -e "${CYAN}Watching logs for pubsub activity...${NC}"
    echo "(Press Ctrl+C to stop)"
    echo ""
    
    # Filter and highlight pubsub-related messages
    tail -f logs/air/*.log | grep -E --color=always "message|topic|Topic|pubsub|Connected to peer|inbound connection" --line-buffered
}

# Check if we should run simple mode
if [ "$1" = "--simple" ]; then
    simple_test
else
    # For now, default to simple mode since we need to add HTTP endpoints to nodes
    echo -e "${YELLOW}Note: Full interactive mode requires HTTP/gRPC endpoints in nodes.${NC}"
    echo -e "${YELLOW}Running in simple observation mode...${NC}"
    echo ""
    sleep 2
    simple_test
fi