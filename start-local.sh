#!/bin/bash
# Quick start script for Teleport OSS local development

set -e

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --help)
            echo "Usage: $0 [OPTIONS]"
            echo ""
            echo "Options:"
            echo "  --help           Show this help message"
            echo ""
            echo "Builds Teleport with full web UI and aggressive Docker layer caching."
            echo ""
            echo "Build time:"
            echo "  - First build: ~10-15 minutes (compiling from source)"
            echo "  - Subsequent builds: ~2-5 minutes (using Docker cache)"
            echo ""
            echo "Docker caching strategy:"
            echo "  - System dependencies (Go, Rust, Node.js): Cached indefinitely"
            echo "  - Go modules: Cached until go.mod/go.sum changes"
            echo "  - Node modules: Cached until package.json/pnpm-lock.yaml changes"
            echo "  - Source code compilation: Rebuilds only when source changes"
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            echo "Run with --help for usage information"
            exit 1
            ;;
    esac
done

echo "========================================="
echo "Teleport OSS Local Development Setup"
echo "========================================="
echo ""

# Check if Docker is running
if ! docker info > /dev/null 2>&1; then
    echo "❌ Error: Docker is not running"
    echo "Please start Docker Desktop and try again"
    exit 1
fi

echo "✓ Docker is running"
echo ""

# Check if config exists
if [ ! -f "teleport-config/teleport.yaml" ]; then
    echo "❌ Error: Configuration file not found"
    echo "Please ensure teleport-config/teleport.yaml exists"
    exit 1
fi

echo "✓ Configuration file found"
echo ""

echo "Building and starting Teleport..."
echo "🌐 Building with FULL web UI and optimized caching"
echo ""
echo "⏳ First build: ~10-15 minutes (installing dependencies and compiling)"
echo "⏳ Rebuilds: ~2-5 minutes (using cached layers)"
echo ""
echo "Docker caching layers:"
echo "  1. System dependencies (Go, Rust, Node.js) - cached indefinitely"
echo "  2. Go/Rust/Node modules - cached until lock files change"
echo "  3. Source compilation - rebuilds only on code changes"
echo ""

# Enable Docker BuildKit for better caching and performance
export DOCKER_BUILDKIT=1
export COMPOSE_DOCKER_CLI_BUILD=1

# Build and start
docker-compose -f docker-compose-local.yaml up --build -d

echo ""
echo "⏳ Waiting for Teleport to start..."
sleep 10

# Check if running
if docker ps | grep -q teleport-local; then
    echo ""
    echo "========================================="
    echo "✅ Teleport is running!"
    echo "========================================="
    echo ""
    echo "Web UI: https://localhost:3080"
    echo ""
    echo "To create your first admin user, run:"
    echo "  docker exec -it teleport-local tctl users add admin --roles=editor,access --logins=root"
    echo ""
    echo "To view logs:"
    echo "  docker logs -f teleport-local"
    echo ""
    echo "To stop:"
    echo "  docker-compose -f docker-compose-local.yaml down"
    echo ""
else
    echo ""
    echo "❌ Error: Teleport failed to start"
    echo "Check logs with: docker logs teleport-local"
    exit 1
fi
