# Falak Development Makefile

.PHONY: help build test test-unit test-integration clean dev-env quick-test docker-test

# Default target
help: ## Show this help message
	@echo "Falak Development Commands"
	@echo "=========================="
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Building
build: ## Build the main falak binary
	@echo "🔨 Building Falak..."
	@go work sync
	@mkdir -p bin
	@cd cmd && go build -o ../bin/falak .
	@echo "✅ Build completed: bin/falak"

build-dev: ## Build for development with debug info
	@echo "🔨 Building Falak (development)..."
	@go work sync
	@mkdir -p tmp
	@cd cmd && go build -gcflags="all=-N -l" -o ../tmp/falak .
	@echo "✅ Development build completed: tmp/falak"

##@ Testing
test: test-unit ## Run all tests

test-unit: ## Run unit tests
	@echo "🧪 Running unit tests..."
	@cd internal/node && go test ./tests/ -v -race -timeout=30s

test-integration: ## Run integration tests with real environment
	@echo "🌐 Running integration tests..."
	@./scripts/dev-env.sh test

test-privileged: ## Run privileged kernel tests (VXLAN + XFRM IPsec). MUST run as root in a Linux env with vxlan/esp4/xfrm_user modules loaded.
	@echo "🛡️  Running privileged kernel tests..."
	@if [ "$$(id -u)" != "0" ]; then \
		echo "ERROR: test-privileged must be run as root. Try: sudo make test-privileged"; \
		exit 1; \
	fi
	@if [ "$$(uname -s)" != "Linux" ]; then \
		echo "ERROR: test-privileged requires Linux (VXLAN + XFRM are kernel features)."; \
		exit 1; \
	fi
	@modprobe vxlan      || { echo "ERROR: failed to modprobe vxlan"; exit 1; }
	@modprobe esp4       || { echo "ERROR: failed to modprobe esp4"; exit 1; }
	@modprobe xfrm_user  || { echo "ERROR: failed to modprobe xfrm_user"; exit 1; }
	@rp="$$(sysctl -n net.ipv4.conf.all.rp_filter)"; \
		if [ "$$rp" != "1" ]; then \
			echo "WARN: net.ipv4.conf.all.rp_filter = $$rp (recommended: 1)"; \
		fi
	@source ~/.gvm/scripts/gvm 2>/dev/null && gvm use go1.25 2>/dev/null; \
		cd network && go test -race -count=1 -timeout=600s ./overlay/...
	@echo "✅ Privileged tests completed"

test-coverage: ## Run tests with coverage report
	@echo "📊 Generating test coverage..."
	@cd internal/node && go test ./tests/ -race -coverprofile=coverage.out -covermode=atomic
	@cd internal/node && go tool cover -html=coverage.out -o coverage.html
	@echo "📋 Coverage report: internal/node/coverage.html"

##@ Development Environment
dev-env: ## Start development environment (3 nodes)
	@echo "🚀 Starting development environment..."
	@./scripts/dev-env.sh start

dev-env-stop: ## Stop development environment
	@echo "🛑 Stopping development environment..."
	@./scripts/dev-env.sh stop

dev-env-restart: ## Restart development environment
	@echo "🔄 Restarting development environment..."
	@./scripts/dev-env.sh restart

dev-env-status: ## Show development environment status
	@./scripts/dev-env.sh status

dev-env-logs: ## Show development environment logs
	@./scripts/dev-env.sh logs

##@ Quick Testing
quick-test: ## Start quick 2-node test environment
	@echo "⚡ Starting quick test environment..."
	@./scripts/quick-test.sh

##@ Docker Environment
docker-build: ## Build Docker image
	@echo "🐳 Building Docker image..."
	@docker build -f Dockerfile.dev -t falak:dev .

docker-test: ## Run Docker-based test environment
	@echo "🐳 Starting Docker test environment..."
	@./scripts/docker-test.sh start

docker-test-stop: ## Stop Docker test environment
	@echo "🐳 Stopping Docker test environment..."
	@./scripts/docker-test.sh stop

docker-test-logs: ## Show Docker test logs
	@./scripts/docker-test.sh logs

docker-integration: ## Run integration tests in Docker
	@echo "🐳🧪 Running Docker integration tests..."
	@./scripts/docker-test.sh test

##@ Development Tools
air-dev: ## Start development with Air hot reloading (multi-node)
	@./scripts/air-dev-multi.sh

air-dev-stop: ## Stop Air development environment
	@./scripts/air-dev-multi.sh stop

air-dev-logs: ## Monitor all Air development logs
	@tail -f logs/air/*.log

air-dev-interactive: ## Start interactive PubSub monitor
	@./scripts/interactive-pubsub.sh

air-dev-tui: ## Start TUI monitor with split-screen interface
	@./scripts/tui-monitor.sh

##@ Maintenance
clean: ## Clean build artifacts and temporary files
	@echo "🧹 Cleaning up..."
	@rm -rf bin/ tmp/ logs/
	@cd internal/node && rm -f coverage.out coverage.html
	@./scripts/dev-env.sh clean
	@echo "✅ Cleanup completed"

clean-docker: ## Clean Docker containers and images
	@echo "🐳🧹 Cleaning Docker resources..."
	@./scripts/docker-test.sh clean

lint: ## Run linter
	@echo "🔍 Running linter..."
	@cd internal/node && golangci-lint run
	@echo "✅ Linting completed"

format: ## Format code
	@echo "💅 Formatting code..."
	@go fmt ./...
	@echo "✅ Formatting completed"

##@ Examples
example-two-nodes: build ## Example: Run two connected nodes
	@echo "📖 Starting two-node example..."
	@echo "Terminal 1: Starting bootstrap node..."
	@./bin/falak --name bootstrap --address "/ip4/0.0.0.0/tcp/4001" &
	@sleep 3
	@echo "Terminal 2: Starting second node..."
	@./bin/falak --name node2 --address "/ip4/0.0.0.0/tcp/4002" --peers "$$(./bin/falak --name temp --address '/ip4/0.0.0.0/tcp/9999' 2>&1 | grep -o 'ID: [A-Za-z0-9]*' | cut -d' ' -f2 | head -1)"

##@ Information
deps: ## Show project dependencies
	@echo "📦 Project Dependencies:"
	@go list -m all

workspace-sync: ## Sync Go workspace
	@echo "🔄 Syncing Go workspace..."
	@go work sync
	@echo "✅ Workspace synced"

##@ Quick Commands
all: clean build test ## Clean, build, and test everything

demo: build ## Run a quick demo
	@echo "🎬 Running Falak demo..."
	@./scripts/quick-test.sh

# Environment variables
NODES_COUNT ?= 3
BASE_PORT ?= 4001

# Multi-node development environment
dev-multi: ## Start multi-node development environment
	@echo "🌐 Starting $(NODES_COUNT)-node development environment..."
	@NODES_COUNT=$(NODES_COUNT) BASE_PORT=$(BASE_PORT) ./scripts/dev-env.sh start