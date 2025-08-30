.PHONY: help build build-watch build-informer clean test test-informer

# Default target
help:
	@echo "Kubernetes Resource Watcher Build Targets:"
	@echo ""
	@echo "  build           - Build the application"
	@echo "  clean           - Remove build artifacts"
	@echo "  test            - Run tests"
	@echo "  dev             - Run in development mode"
	@echo "  benchmark       - Run performance test"
	@echo "  docker          - Build Docker image"
	@echo "  install         - Install dependencies"
	@echo ""

build: build-informer

build-informer:
	@echo "Building Informer-based version..."
	go build -o bin/resource-watcher-informer main.go
	@echo "Informer-based version built: bin/resource-watcher-informer"

clean:
	@echo "Cleaning build artifacts..."
	rm -rf bin/
	@echo "Cleaned build artifacts"

install:
	@echo "Installing dependencies..."
	go mod download
	go mod tidy
	@echo "Dependencies installed"

test:
	@echo "Running tests for Watch API version..."
	go test ./pkg/... -v

test-all: test

dev: build-informer
	@echo "Starting resource watcher in development mode..."
	./bin/resource-watcher-informer -config test/test-config.yaml

benchmark: build-informer
	@echo "Running performance test..."
	time ./bin/resource-watcher-informer -config test/test-config.yaml

docker:
	@echo "Building Docker image..."
	docker build -t k8s-resource-watcher:latest .

version:
	@echo "Kubernetes Resource Watcher"
	@echo "Module: $(shell go list -m)"
	@echo "Go version: $(shell go version)"
	@echo "Available builds:"
	@ls -la bin/ 2>/dev/null || echo "No builds found. Run 'make build' first." 