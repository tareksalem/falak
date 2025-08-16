#!/bin/bash

# Docker-based Test Environment Script
# Uses Docker Compose for isolated testing

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

cd "$PROJECT_ROOT"

# Colors
GREEN='\033[0;32m'
BLUE='\033[0;34m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m'

print_status() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

print_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

print_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

# Function to check Docker and Docker Compose
check_dependencies() {
    if ! command -v docker &> /dev/null; then
        print_error "Docker is not installed or not in PATH"
        exit 1
    fi
    
    if ! command -v docker-compose &> /dev/null && ! docker compose version &> /dev/null; then
        print_error "Docker Compose is not installed or not in PATH"
        exit 1
    fi
    
    print_success "Docker dependencies verified"
}

# Function to start the environment
start_env() {
    print_status "Starting Falak Docker test environment..."
    
    # Create logs directory
    mkdir -p logs
    
    # Build and start services
    docker-compose up --build -d bootstrap node2 node3
    
    print_status "Waiting for services to be healthy..."
    sleep 10
    
    # Check service status
    docker-compose ps
    
    print_success "Docker environment started successfully!"
    print_status "View logs with: docker-compose logs -f [service_name]"
}

# Function to stop the environment
stop_env() {
    print_status "Stopping Docker environment..."
    docker-compose down
    print_success "Environment stopped"
}

# Function to show logs
show_logs() {
    local service=$1
    if [ -z "$service" ]; then
        docker-compose logs -f
    else
        docker-compose logs -f "$service"
    fi
}

# Function to run tests
run_tests() {
    print_status "Running integration tests in Docker..."
    
    # Start test runner with testing profile
    docker-compose --profile testing up --build test-runner
    
    local exit_code=$?
    if [ $exit_code -eq 0 ]; then
        print_success "Tests completed successfully"
    else
        print_error "Tests failed with exit code $exit_code"
    fi
    
    return $exit_code
}

# Function to show status
show_status() {
    print_status "Docker environment status:"
    docker-compose ps
}

# Function to exec into a container
exec_container() {
    local service=$1
    if [ -z "$service" ]; then
        print_error "Please specify a service name (bootstrap, node2, node3)"
        exit 1
    fi
    
    docker-compose exec "$service" sh
}

# Function to clean up
cleanup() {
    print_status "Cleaning up Docker environment..."
    docker-compose down -v --remove-orphans
    docker system prune -f
    print_success "Cleanup completed"
}

# Main script logic
case "${1:-help}" in
    start)
        check_dependencies
        start_env
        ;;
    stop)
        stop_env
        ;;
    restart)
        stop_env
        sleep 2
        start_env
        ;;
    status)
        show_status
        ;;
    logs)
        show_logs $2
        ;;
    test)
        check_dependencies
        run_tests
        ;;
    exec)
        exec_container $2
        ;;
    clean)
        cleanup
        ;;
    help|--help|-h)
        echo "Falak Docker Test Environment Manager"
        echo ""
        echo "Usage: $0 [COMMAND] [OPTIONS]"
        echo ""
        echo "Commands:"
        echo "  start           Start the Docker test environment"
        echo "  stop            Stop all containers"
        echo "  restart         Restart the environment"
        echo "  status          Show container status"
        echo "  logs [SERVICE]  Show logs for all or specific service"
        echo "  test            Run integration tests"
        echo "  exec SERVICE    Execute shell in container"
        echo "  clean           Clean up all containers and volumes"
        echo "  help            Show this help message"
        echo ""
        echo "Services:"
        echo "  bootstrap       Bootstrap node (port 4001)"
        echo "  node2           Node 2 (port 4002)"
        echo "  node3           Node 3 (port 4003)"
        echo "  node4           Node 4 (port 4004)"
        echo ""
        echo "Examples:"
        echo "  $0 start                    # Start environment"
        echo "  $0 logs bootstrap           # Show bootstrap logs"
        echo "  $0 exec bootstrap           # Shell into bootstrap"
        echo "  $0 test                     # Run tests"
        ;;
    *)
        print_error "Unknown command: $1"
        echo "Use '$0 help' for usage information"
        exit 1
        ;;
esac