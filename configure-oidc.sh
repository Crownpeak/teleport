#!/bin/bash
# Script to configure OIDC connector in Teleport

set -e

echo "========================================="
echo "Configuring OIDC Connector in Teleport"
echo "========================================="
echo ""

# Check if Teleport is running
if ! docker ps | grep -q teleport-local; then
    echo "❌ Error: Teleport container is not running"
    echo "Start it with: docker-compose -f docker-compose-local.yaml up -d"
    exit 1
fi

echo "✓ Teleport container is running"
echo ""

# Wait for Teleport to be ready
echo "Waiting for Teleport to be ready..."
for i in {1..30}; do
    if docker exec teleport-local tctl status > /dev/null 2>&1; then
        echo "✓ Teleport is ready"
        break
    fi
    if [ $i -eq 30 ]; then
        echo "❌ Timeout waiting for Teleport"
        exit 1
    fi
    sleep 2
done
echo ""

# Check if OIDC connector file exists
if [ ! -f "oidc-connector.yaml" ]; then
    echo "❌ Error: oidc-connector.yaml not found"
    exit 1
fi

echo "Creating OIDC connector..."
if docker exec -i teleport-local tctl create < oidc-connector.yaml; then
    echo "✓ OIDC connector created successfully"
else
    echo "⚠️  Note: Connector may already exist. Updating..."
    # Try to delete and recreate
    docker exec teleport-local tctl rm oidc/keycloak 2>/dev/null || true
    if docker exec -i teleport-local tctl create < oidc-connector.yaml; then
        echo "✓ OIDC connector updated successfully"
    else
        echo "❌ Failed to create OIDC connector"
        exit 1
    fi
fi
echo ""

# List connectors to verify
echo "Current OIDC connectors:"
docker exec teleport-local tctl get oidc
echo ""

echo "========================================="
echo "✅ OIDC Configuration Complete!"
echo "========================================="
echo ""
echo "Next steps:"
echo "1. Access Teleport Web UI: https://localhost:3080"
echo "2. Click 'Sign in with SSO'"
echo "3. Select 'Keycloak SSO'"
echo "4. You'll be redirected to Keycloak for authentication"
echo ""
echo "To view logs:"
echo "  docker logs -f teleport-local"
echo ""
echo "To view audit events:"
echo "  docker logs -f teleport-local | grep -i 'user.login'"
echo ""
