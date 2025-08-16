#!/bin/bash

# Falak TUI Monitor with Split Screen
# Fixed command area at bottom, scrolling logs at top

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

# Terminal control
SAVE_CURSOR='\033[s'
RESTORE_CURSOR='\033[u'
CLEAR_LINE='\033[2K'
HIDE_CURSOR='\033[?25l'
SHOW_CURSOR='\033[?25h'

# Global variables
TERMINAL_HEIGHT=$(tput lines)
TERMINAL_WIDTH=$(tput cols)
LOG_AREA_HEIGHT=$((TERMINAL_HEIGHT - 6))  # Reserve 6 lines for command area
COMMAND_AREA_START=$((LOG_AREA_HEIGHT + 1))
CURRENT_MODE="interactive"
LOG_PID=""

# Setup terminal
setup_terminal() {
    # Hide cursor and clear screen
    printf "${HIDE_CURSOR}"
    clear
    
    # Set up signal handlers
    trap cleanup EXIT INT TERM
    trap handle_resize WINCH
    
    # Draw initial layout
    draw_layout
}

# Cleanup terminal
cleanup() {
    # Kill log monitoring if running
    if [ ! -z "$LOG_PID" ]; then
        kill "$LOG_PID" 2>/dev/null || true
    fi
    
    # Restore terminal
    printf "${SHOW_CURSOR}"
    tput cnorm
    clear
    echo "Thanks for using Falak TUI Monitor!"
    exit 0
}

# Handle terminal resize
handle_resize() {
    TERMINAL_HEIGHT=$(tput lines)
    TERMINAL_WIDTH=$(tput cols)
    LOG_AREA_HEIGHT=$((TERMINAL_HEIGHT - 6))
    COMMAND_AREA_START=$((LOG_AREA_HEIGHT + 1))
    draw_layout
}

# Draw the fixed layout
draw_layout() {
    clear
    
    # Draw header
    printf "\033[1;1H"
    echo -e "${PURPLE}${BOLD}╔$(printf '═%.0s' $(seq 1 $((TERMINAL_WIDTH-2))))╗${NC}"
    echo -e "${PURPLE}${BOLD}║$(printf ' %.0s' $(seq 1 $((TERMINAL_WIDTH/2-10))))Falak TUI Monitor$(printf ' %.0s' $(seq 1 $((TERMINAL_WIDTH/2-9))))║${NC}"
    echo -e "${PURPLE}${BOLD}╚$(printf '═%.0s' $(seq 1 $((TERMINAL_WIDTH-2))))╝${NC}"
    
    # Draw separator line
    printf "\033[${LOG_AREA_HEIGHT};1H"
    echo -e "${CYAN}$(printf '─%.0s' $(seq 1 $TERMINAL_WIDTH))${NC}"
    
    # Draw command area background
    printf "\033[${COMMAND_AREA_START};1H"
    echo -e "${CYAN}┌─ Quick Commands ─$(printf '─%.0s' $(seq 1 $((TERMINAL_WIDTH-20))))┐${NC}"
    printf "\033[$((COMMAND_AREA_START+1));1H"
    echo -e "${CYAN}│${NC} ${GREEN}status${NC} │ ${GREEN}topics${NC} │ ${GREEN}logs${NC} │ ${GREEN}msg <topic> <message>${NC} │ ${GREEN}help${NC} │ ${GREEN}quit${NC} $(printf ' %.0s' $(seq 1 $((TERMINAL_WIDTH-90))))${CYAN}│${NC}"
    printf "\033[$((COMMAND_AREA_START+2));1H"
    echo -e "${CYAN}└$(printf '─%.0s' $(seq 1 $((TERMINAL_WIDTH-2))))┘${NC}"
    
    # Position cursor for input
    printf "\033[$((COMMAND_AREA_START+3));1H"
    echo -ne "${BOLD}falak>${NC} "
}

# Print to log area
log_print() {
    local message="$1"
    local line_num="$2"
    
    if [ -z "$line_num" ]; then
        line_num=4
    fi
    
    # Move to log area and print
    printf "\033[${line_num};1H${CLEAR_LINE}"
    echo -e "$message"
    
    # Return cursor to input position
    printf "\033[${COMMAND_AREA_START};1H"
    echo -ne "${BOLD}falak>${NC} "
}

# Monitor logs in background
monitor_logs() {
    local node_filter="$1"
    local line_counter=4
    
    # Kill existing log monitoring
    if [ ! -z "$LOG_PID" ]; then
        kill "$LOG_PID" 2>/dev/null || true
    fi
    
    if [ -z "$node_filter" ]; then
        # Monitor all logs
        tail -f logs/air/*.log 2>/dev/null | while IFS= read -r line; do
            # Color code by node
            if [[ "$line" =~ node1\.log ]]; then
                colored_line="${GREEN}[node1]${NC} ${line#*:}"
            elif [[ "$line" =~ node2\.log ]]; then
                colored_line="${BLUE}[node2]${NC} ${line#*:}"
            elif [[ "$line" =~ node3\.log ]]; then
                colored_line="${PURPLE}[node3]${NC} ${line#*:}"
            else
                colored_line="$line"
            fi
            
            # Print to specific line in log area
            printf "\033[${line_counter};1H${CLEAR_LINE}"
            echo -e "$colored_line" | cut -c1-$((TERMINAL_WIDTH-1))
            
            # Scroll if we reach the bottom of log area
            line_counter=$((line_counter + 1))
            if [ $line_counter -gt $LOG_AREA_HEIGHT ]; then
                line_counter=4
            fi
            
            # Keep cursor in command area
            printf "\033[$((COMMAND_AREA_START+3));1H"
            printf "${BOLD}falak>${NC} "
        done &
        LOG_PID=$!
    else
        # Monitor specific node
        local log_file="logs/air/${node_filter}.log"
        if [ ! -f "$log_file" ]; then
            log_print "${RED}[ERROR]${NC} Log file not found: $log_file" 4
            return 1
        fi
        
        tail -f "$log_file" 2>/dev/null | while IFS= read -r line; do
            # Add syntax highlighting
            colored_line="$line"
            colored_line="${colored_line//building.../${YELLOW}[BUILD]${NC} building...}"
            colored_line="${colored_line//running.../${GREEN}[RUN]${NC} running...}"
            colored_line="${colored_line//Connected to peer/${CYAN}[PEER]${NC} Connected to peer}"
            colored_line="${colored_line//inbound connection/${BLUE}[CONN]${NC} inbound connection}"
            
            # Print to specific line in log area
            printf "\033[${line_counter};1H${CLEAR_LINE}"
            echo -e "$colored_line" | cut -c1-$((TERMINAL_WIDTH-1))
            
            # Scroll if we reach the bottom of log area
            line_counter=$((line_counter + 1))
            if [ $line_counter -gt $LOG_AREA_HEIGHT ]; then
                line_counter=4
            fi
            
            # Keep cursor in command area
            printf "\033[$((COMMAND_AREA_START+3));1H"
            printf "${BOLD}falak>${NC} "
        done &
        LOG_PID=$!
    fi
}

# Stop log monitoring
stop_logs() {
    if [ ! -z "$LOG_PID" ]; then
        kill "$LOG_PID" 2>/dev/null || true
        LOG_PID=""
    fi
    
    # Clear log area
    for i in $(seq 4 $LOG_AREA_HEIGHT); do
        printf "\033[${i};1H${CLEAR_LINE}"
    done
    
    log_print "${YELLOW}Log monitoring stopped. Use 'logs' or 'logs node1' to start monitoring.${NC}" 4
}

# Show cluster status
show_status() {
    local line=4
    
    stop_logs
    
    log_print "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}" $line
    line=$((line + 1))
    log_print "${YELLOW}${BOLD}Cluster Status${NC}" $line
    line=$((line + 1))
    log_print "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}" $line
    line=$((line + 1))
    
    if [ -f "logs/air/pids.txt" ]; then
        local count=0
        while IFS=':' read -r pid name port; do
            if kill -0 "$pid" 2>/dev/null; then
                local peer_id=$(grep "Node created successfully with ID:" "logs/air/${name}.log" 2>/dev/null | sed 's/.*ID: \([^ ]*\).*/\1/' | tail -1)
                log_print "${GREEN}●${NC} ${BOLD}$name${NC} (PID: $pid, Port: $port)" $line
                line=$((line + 1))
                if [ ! -z "$peer_id" ]; then
                    log_print "   Peer ID: ${peer_id:0:20}..." $line
                    line=$((line + 1))
                fi
                count=$((count + 1))
            fi
        done < "logs/air/pids.txt"
        
        line=$((line + 1))
        log_print "${BLUE}Total Active Nodes: $count${NC}" $line
    else
        log_print "${RED}No nodes running. Start with: make air-dev${NC}" $line
    fi
}

# Send message to specified pubsub topic
send_message() {
    local input="$1"
    local topic=$(echo "$input" | awk '{print $1}')
    local message=$(echo "$input" | cut -d' ' -f2- 2>/dev/null || echo "")
    
    # Check if both topic and message are provided
    if [ -z "$topic" ] || [ -z "$message" ] || [ "$message" = "$topic" ]; then
        log_print "${RED}[ERROR]${NC} Usage: msg <topic> <message>" 4
        log_print "${YELLOW}Examples:${NC}" 5
        log_print "  msg falak/default/members/membership 'Node online'" 6
        log_print "  msg falak/default/test/interactive 'Hello cluster'" 7
        return 1
    fi
    
    stop_logs
    
    local timestamp=$(date -Iseconds)
    
    # Log locally for debugging
    echo "[$timestamp] PUBSUB_MESSAGE: $topic -> $message" >> "logs/air/test_messages.log"
    
    # Try to send real pubsub message
    if send_pubsub_message "$topic" "$message"; then
        log_print "${GREEN}✅ Message sent successfully!${NC}" 4
        log_print "${BLUE}📡 Topic: $topic${NC}" 5
        log_print "${BLUE}📤 Message: $message${NC}" 6
        log_print "${YELLOW}🔍 Watch logs to see if nodes receive it${NC}" 7
    else
        log_print "${RED}❌ Failed to send message${NC}" 4
        log_print "${YELLOW}⚠ Check if nodes are running (make air-dev)${NC}" 5
        log_print "${CYAN}💡 Message logged locally: logs/air/test_messages.log${NC}" 6
    fi
}

# Send real pubsub message using Go client
send_pubsub_message() {
    local topic="$1"
    local message="$2"
    
    # Get bootstrap peer from running nodes
    local bootstrap_peer=$(get_bootstrap_peer)
    if [ -z "$bootstrap_peer" ]; then
        return 1
    fi
    
    # Use the pre-compiled pubsub client
    local script_dir="$(dirname "$0")"
    if [ -x "$script_dir/pubsub-client" ]; then
        if ! "$script_dir/pubsub-client" "$bootstrap_peer" "$topic" "$message" >/dev/null 2>&1; then
            return 1
        fi
    else
        # Fallback to go run if binary doesn't exist
        if ! cd "$script_dir" && go run pubsub-client.go "$bootstrap_peer" "$topic" "$message" >/dev/null 2>&1; then
            return 1
        fi
    fi
    
    return 0
}

# Get bootstrap peer address from running nodes
get_bootstrap_peer() {
    if [ -f "logs/air/pids.txt" ]; then
        while IFS=':' read -r pid name port; do
            if kill -0 "$pid" 2>/dev/null; then
                local peer_id=$(grep "Node created successfully with ID:" "logs/air/${name}.log" 2>/dev/null | sed 's/.*ID: \([^ ]*\).*/\1/' | tail -1)
                if [ ! -z "$peer_id" ]; then
                    echo "/ip4/127.0.0.1/tcp/$port/p2p/$peer_id"
                    return 0
                fi
            fi
        done < "logs/air/pids.txt"
    fi
    return 1
}

# Show active topics
show_topics() {
    local line=4
    
    stop_logs
    
    log_print "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}" $line
    line=$((line + 1))
    log_print "${YELLOW}${BOLD}Active PubSub Topics${NC}" $line
    line=$((line + 1))
    log_print "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}" $line
    line=$((line + 1))
    
    log_print "${GREEN}Expected Cluster Topics:${NC}" $line
    line=$((line + 1))
    log_print "  • falak/default/members/membership" $line
    line=$((line + 1))
    log_print "  • falak/default/members/joined" $line
    line=$((line + 1))
    log_print "  • falak/default/heartbeat" $line
    line=$((line + 1))
    log_print "  • falak/default/suspicion" $line
    line=$((line + 1))
    log_print "  • falak/default/quorum" $line
    line=$((line + 1))
    log_print "  • falak/default/failed" $line
    line=$((line + 2))
    
    log_print "${BLUE}Topics found in logs:${NC}" $line
    line=$((line + 1))
    if ls logs/air/*.log >/dev/null 2>&1; then
        local topics_found=$(grep -h "Listening on topic\|join.*topic\|Topic" logs/air/*.log 2>/dev/null | \
                           grep -o "falak[^[:space:]]*" | sort | uniq 2>/dev/null || echo "")
        if [ ! -z "$topics_found" ]; then
            while IFS= read -r topic; do
                if [ ! -z "$topic" ]; then
                    log_print "  • $topic" $line
                    line=$((line + 1))
                fi
            done <<< "$topics_found"
        else
            log_print "  No topic activity found in logs" $line
        fi
    else
        log_print "  No logs found" $line
    fi
    
    line=$((line + 2))
    log_print "${GREEN}Common topics to try:${NC}" $line
    line=$((line + 1))
    log_print "  • falak/default/test/interactive" $line
    line=$((line + 1))
    log_print "  • falak/default/members/membership" $line
    line=$((line + 1))
    log_print "  • falak/default/members/joined" $line
}

# Show help
show_help() {
    local line=4
    
    stop_logs
    
    log_print "${CYAN}═══════════════════════════════════════════════════════════${NC}" $line
    line=$((line + 1))
    log_print "${YELLOW}${BOLD}Available Commands:${NC}" $line
    line=$((line + 2))
    log_print "${GREEN}📤 msg <topic> <message>${NC} - Send message to specific topic" $line
    line=$((line + 1))
    log_print "${GREEN}📊 status${NC}             - Show cluster status and topics" $line
    line=$((line + 1))
    log_print "${GREEN}🔍 topics${NC}             - List active pubsub topics" $line
    line=$((line + 1))
    log_print "${GREEN}📜 logs [node]${NC}        - Monitor live logs (all nodes or specific node)" $line
    line=$((line + 1))
    log_print "${GREEN}🛑 stop${NC}               - Stop log monitoring" $line
    line=$((line + 1))
    log_print "${GREEN}❓ help${NC}               - Show this help" $line
    line=$((line + 1))
    log_print "${GREEN}🚪 quit${NC}               - Exit" $line
    line=$((line + 2))
    log_print "${CYAN}═══════════════════════════════════════════════════════════${NC}" $line
}

# Process commands
process_command() {
    local input="$1"
    local cmd=$(echo "$input" | awk '{print $1}')
    local args=$(echo "$input" | cut -d' ' -f2- 2>/dev/null || echo "")
    
    case "$cmd" in
        "msg")
            if [ -z "$args" ] || [ "$args" = "$cmd" ]; then
                log_print "${RED}[ERROR]${NC} Usage: msg <topic> <message>" 4
                log_print "${YELLOW}Example: msg falak/default/test/interactive hello${NC}" 5
            else
                send_message "$args"
            fi
            ;;
        "status")
            show_status
            ;;
        "topics")
            show_topics
            ;;
        "logs")
            if [ "$args" = "$cmd" ]; then
                log_print "${YELLOW}Starting log monitoring for all nodes...${NC}" 4
                monitor_logs
            else
                log_print "${YELLOW}Starting log monitoring for $args...${NC}" 4
                monitor_logs "$args"
            fi
            ;;
        "stop")
            stop_logs
            ;;
        "help" | "?")
            show_help
            ;;
        "quit" | "exit" | "q")
            cleanup
            ;;
        "")
            # Empty command
            ;;
        *)
            log_print "${RED}[ERROR]${NC} Unknown command: $cmd. Type 'help' for available commands." 4
            ;;
    esac
}

# Main function
main() {
    # Check if nodes are running
    if [ ! -f "logs/air/pids.txt" ]; then
        echo "No nodes running. Start with: make air-dev"
        exit 1
    fi
    
    # Setup terminal UI
    setup_terminal
    
    # Show initial status
    show_status
    
    # Main input loop
    while true; do
        # Position cursor for input
        printf "\033[$((COMMAND_AREA_START+3));1H"
        printf "${CLEAR_LINE}${BOLD}falak>${NC} "
        
        # Read input without echoing (we'll handle display)
        read -r input
        
        # Show the command that was entered
        printf "\033[$((COMMAND_AREA_START+3));1H"
        printf "${CLEAR_LINE}${BOLD}falak>${NC} ${BLUE}$input${NC}"
        
        # Process the command
        process_command "$input"
        
        # Brief pause to show the command
        sleep 0.1
    done
}

# Run main function
main "$@"