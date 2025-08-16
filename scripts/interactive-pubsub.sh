#!/bin/bash

# Interactive PubSub Test for Falak Nodes
# This creates an interface to send messages and monitor the cluster

set -e

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
PURPLE='\033[0;35m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

# Configuration
BASE_PORT=${BASE_PORT:-4001}
CLUSTER_ID="default"
TEST_TOPIC="falak/test/interactive"

print_header() {
    clear
    echo -e "${PURPLE}${BOLD}╔══════════════════════════════════════════════════════════╗${NC}"
    echo -e "${PURPLE}${BOLD}║           Falak Interactive PubSub Monitor              ║${NC}"
    echo -e "${PURPLE}${BOLD}╚══════════════════════════════════════════════════════════╝${NC}"
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

print_warning() {
    echo -e "${YELLOW}[⚠]${NC} $1"
}

check_nodes() {
    if [ ! -f "logs/air/pids.txt" ]; then
        print_error "No nodes running. Start with: make air-dev"
        exit 1
    fi
    
    local running_nodes=()
    local count=0
    while IFS=':' read -r pid name port; do
        if kill -0 "$pid" 2>/dev/null; then
            running_nodes+=("$name:$port")
            count=$((count + 1))
        fi
    done < "logs/air/pids.txt"
    
    if [ $count -eq 0 ]; then
        print_error "No nodes running. Start with: make air-dev"
        exit 1
    fi
    
    print_success "Found $count running nodes: ${running_nodes[*]}"
    return 0
}

show_help() {
    echo -e "${CYAN}═══════════════════════════════════════════════════════════${NC}"
    echo -e "${YELLOW}${BOLD}Available Commands:${NC}"
    echo ""
    echo -e "${GREEN}📤 msg <message>${NC}      - Send test message to all nodes"
    echo -e "${GREEN}📊 status${NC}             - Show cluster status and topics"
    echo -e "${GREEN}🔍 topics${NC}             - List active topics"
    echo -e "${GREEN}📺 monitor${NC}            - Monitor live pubsub activity"
    echo -e "${GREEN}📜 logs [node]${NC}        - Show live logs (all nodes or specific node)"
    echo -e "${GREEN}💝 heartbeat${NC}          - Send a heartbeat-like message"
    echo -e "${GREEN}🏠 join${NC}               - Send a join message"
    echo -e "${GREEN}👋 leave${NC}              - Send a leave message"  
    echo -e "${GREEN}❓ help${NC}               - Show this help"
    echo -e "${GREEN}🚪 quit${NC}               - Exit"
    echo ""
    echo -e "${CYAN}═══════════════════════════════════════════════════════════${NC}"
}

show_quick_guide() {
    echo -e "${CYAN}┌─────────────────────────────────────────────────────────────────────────────┐${NC}"
    echo -e "${CYAN}│${NC} ${YELLOW}Quick Guide:${NC} ${GREEN}status${NC} │ ${GREEN}logs${NC} │ ${GREEN}logs node1${NC} │ ${GREEN}monitor${NC} │ ${GREEN}msg hello${NC} │ ${GREEN}help${NC} │ ${GREEN}quit${NC} ${CYAN}│${NC}"
    echo -e "${CYAN}└─────────────────────────────────────────────────────────────────────────────┘${NC}"
}

get_node_peer_id() {
    local node_name=$1
    if [ -f "logs/air/${node_name}.log" ]; then
        grep "Node created successfully with ID:" "logs/air/${node_name}.log" | \
        sed 's/.*ID: \([^ ]*\).*/\1/' | tail -1
    fi
}

send_test_message() {
    local message="$1"
    local timestamp=$(date -Iseconds)
    
    print_status "Broadcasting message via HTTP to nodes..."
    
    # For now, we'll simulate message sending by writing to a test file
    # that the nodes could monitor, since we'd need to add HTTP endpoints
    
    local test_file="logs/air/test_messages.log"
    echo "[$timestamp] TEST_MESSAGE: $message" >> "$test_file"
    
    print_success "Message logged: $message"
    print_warning "Note: This is a simulation. To send real pubsub messages, nodes need HTTP/gRPC endpoints."
    
    return 0
}

show_cluster_status() {
    echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${YELLOW}${BOLD}Cluster Status${NC}"
    echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo ""
    
    local count=0
    while IFS=':' read -r pid name port; do
        if kill -0 "$pid" 2>/dev/null; then
            local peer_id=$(get_node_peer_id "$name")
            local uptime=$(ps -o etime= -p "$pid" 2>/dev/null | tr -d ' ')
            echo -e "${GREEN}●${NC} ${BOLD}$name${NC}"
            echo "   Port: $port"
            echo "   PID: $pid"
            echo "   Uptime: ${uptime:-unknown}"
            echo "   Peer ID: ${peer_id:-unknown}"
            echo ""
            count=$((count + 1))
        fi
    done < "logs/air/pids.txt"
    
    echo -e "${BLUE}Total Active Nodes: $count${NC}"
}

show_active_topics() {
    echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${YELLOW}${BOLD}Active Topics (from logs)${NC}"
    echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo ""
    
    # Expected topics based on the protocol documentation
    echo -e "${GREEN}Expected Cluster Topics:${NC}"
    echo "  • falak/$CLUSTER_ID/members/membership"
    echo "  • falak/$CLUSTER_ID/members/joined"
    echo "  • falak/$CLUSTER_ID/heartbeat"
    echo "  • falak/$CLUSTER_ID/suspicion"
    echo "  • falak/$CLUSTER_ID/quorum"
    echo "  • falak/$CLUSTER_ID/failed"
    echo ""
    
    # Check what topics are actually mentioned in logs
    echo -e "${BLUE}Topics found in logs:${NC}"
    if ls logs/air/*.log >/dev/null 2>&1; then
        grep -h "Listening on topic\|join.*topic\|Topic" logs/air/*.log 2>/dev/null | \
        grep -o "falak[^[:space:]]*" | sort | uniq | \
        sed 's/^/  • /' || echo "  No topic activity found in logs"
    fi
}

monitor_pubsub() {
    # Set up trap to return to main interface
    trap 'echo -e "\n${YELLOW}Returning to interactive mode...${NC}"; return 0' INT
    
    echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${YELLOW}${BOLD}Monitoring Live PubSub Activity${NC}"
    echo -e "${CYAN}Press Ctrl+C to return to interactive mode${NC}"
    echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo ""
    
    # Monitor logs for pubsub-related activity
    tail -f logs/air/*.log | grep -E --color=always \
        "Received message|Listening on topic|Connected to peer|inbound connection|Topic|message|falak" \
        --line-buffered || true
    
    # Remove trap when function exits
    trap - INT
}

show_live_logs() {
    local node_filter="$1"
    
    # Set up trap to return to main interface
    trap 'echo -e "\n${YELLOW}Returning to interactive mode...${NC}"; return 0' INT
    
    if [ -z "$node_filter" ]; then
        # Show all nodes
        echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
        echo -e "${YELLOW}${BOLD}Live Logs from All Nodes${NC}"
        echo -e "${CYAN}Press Ctrl+C to return to interactive mode${NC}"
        echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
        echo ""
        
        # Use different colors for different nodes
        tail -f logs/air/*.log | sed -u \
            -e 's/^.*node1\.log:/'"${GREEN}[node1]${NC}"'/' \
            -e 's/^.*node2\.log:/'"${BLUE}[node2]${NC}"'/' \
            -e 's/^.*node3\.log:/'"${PURPLE}[node3]${NC}"'/' \
            -e 's/building\.\.\./'"${YELLOW}[BUILD]${NC}"' building.../' \
            -e 's/running\.\.\./'"${GREEN}[RUN]${NC}"' running.../' \
            -e 's/Connected to peer/'"${CYAN}[PEER]${NC}"' Connected to peer/' \
            -e 's/inbound connection/'"${BLUE}[CONN]${NC}"' inbound connection/' \
            -e 's/Received message/'"${PURPLE}[MSG]${NC}"' Received message/' \
            -e 's/Listening on topic/'"${YELLOW}[TOPIC]${NC}"' Listening on topic/'
    else
        # Show specific node
        local log_file="logs/air/${node_filter}.log"
        if [ ! -f "$log_file" ]; then
            print_error "Log file not found: $log_file"
            print_status "Available nodes:"
            ls logs/air/*.log 2>/dev/null | sed 's|logs/air/||; s|\.log$||' | sed 's/^/  • /' || echo "  No logs found"
            trap - INT  # Remove trap
            return 1
        fi
        
        echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
        echo -e "${YELLOW}${BOLD}Live Logs from ${node_filter}${NC}"
        echo -e "${CYAN}Press Ctrl+C to return to interactive mode${NC}"
        echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
        echo ""
        
        # Show logs with syntax highlighting
        tail -f "$log_file" | sed -u \
            -e 's/building\.\.\./'"${YELLOW}[BUILD]${NC}"' building.../' \
            -e 's/running\.\.\./'"${GREEN}[RUN]${NC}"' running.../' \
            -e 's/Connected to peer/'"${CYAN}[PEER]${NC}"' Connected to peer/' \
            -e 's/inbound connection/'"${BLUE}[CONN]${NC}"' inbound connection/' \
            -e 's/Received message/'"${PURPLE}[MSG]${NC}"' Received message/' \
            -e 's/Listening on topic/'"${YELLOW}[TOPIC]${NC}"' Listening on topic/' \
            -e 's/ERROR/'"${RED}[ERROR]${NC}"'/' \
            -e 's/WARN/'"${YELLOW}[WARN]${NC}"'/' \
            -e 's/INFO/'"${BLUE}[INFO]${NC}"'/'
    fi
    
    # Remove trap when function exits
    trap - INT
}

send_heartbeat() {
    local timestamp=$(date -Iseconds)
    local message="HEARTBEAT from interactive tool at $timestamp"
    
    echo "[$timestamp] HEARTBEAT_TEST: $message" >> "logs/air/test_messages.log"
    print_success "Heartbeat simulation sent"
}

send_join_message() {
    local timestamp=$(date -Iseconds)
    local message="JOIN_TEST from interactive tool at $timestamp"
    
    echo "[$timestamp] JOIN_TEST: $message" >> "logs/air/test_messages.log"
    print_success "Join message simulation sent"
}

send_leave_message() {
    local timestamp=$(date -Iseconds)
    local message="LEAVE_TEST from interactive tool at $timestamp"
    
    echo "[$timestamp] LEAVE_TEST: $message" >> "logs/air/test_messages.log"
    print_success "Leave message simulation sent"
}

# Main interactive loop
main() {
    trap 'echo -e "\n${YELLOW}Goodbye!${NC}"; exit 0' INT
    
    print_header
    check_nodes
    show_help
    
    # Initialize test messages file
    mkdir -p logs/air
    touch logs/air/test_messages.log
    
    while true; do
        echo ""
        show_quick_guide
        echo -ne "${BOLD}falak-pubsub>${NC} "
        read -r input
        
        # Keep the command visible after execution
        if [ ! -z "$input" ]; then
            echo -e "${BLUE}> $input${NC}"
        fi
        
        # Parse command
        cmd=$(echo "$input" | awk '{print $1}')
        args=$(echo "$input" | cut -d' ' -f2- 2>/dev/null || echo "")
        
        case "$cmd" in
            "msg")
                if [ -z "$args" ] || [ "$args" = "$cmd" ]; then
                    print_error "Usage: msg <message>"
                else
                    send_test_message "$args"
                fi
                ;;
            "status")
                show_cluster_status
                ;;
            "topics")
                show_active_topics
                ;;
            "monitor")
                monitor_pubsub
                ;;
            "logs")
                if [ "$args" = "$cmd" ]; then
                    show_live_logs
                else
                    show_live_logs "$args"
                fi
                ;;
            "heartbeat")
                send_heartbeat
                ;;
            "join")
                send_join_message
                ;;
            "leave")
                send_leave_message
                ;;
            "help" | "?")
                show_help
                ;;
            "quit" | "exit" | "q")
                break
                ;;
            "")
                # Empty command, continue
                ;;
            *)
                print_error "Unknown command: $cmd"
                print_status "Type 'help' for available commands"
                ;;
        esac
        
        # Show quick completion message for non-empty commands that don't take over terminal
        if [ ! -z "$input" ] && [ "$cmd" != "logs" ] && [ "$cmd" != "monitor" ] && [ "$cmd" != "help" ] && [ "$cmd" != "quit" ] && [ "$cmd" != "exit" ] && [ "$cmd" != "q" ] && [ "$cmd" != "" ]; then
            echo -e "${GREEN}✓ Command completed${NC}"
        fi
    done
    
    echo -e "${GREEN}Thanks for using Falak Interactive PubSub Monitor!${NC}"
}

# Check if running in non-interactive mode
if [ "$1" = "--monitor-only" ]; then
    print_header
    check_nodes
    monitor_pubsub
else
    main
fi