#!/bin/bash
# Quick start script for Teleport OSS local development

set -e

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

# Build and start
echo "Building and starting Teleport..."
echo "⏳ This will take 15-30 minutes on first run (compiling from source)"
echo ""

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
