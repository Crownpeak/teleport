#!/usr/bin/env bash
# OIDC runtime light gate.
#
# Boots the built Teleport image and verifies the OIDC fork patch survived:
#   Check 1 (entitlement): tctl create oidc connector must succeed — no "Enterprise" error.
#   Check 2 (service reg): curl OIDC console endpoint, inspect logs — must not see
#                          "Enterprise" error or nil/not-implemented panic; connector-not-found
#                          or discovery-error is the expected (passing) outcome.
#
# No external IdP required.
#
# Usage: TELEPORT_IMAGE=intranet.fredhopper.com/teleport:18.8.0 bash run.sh
# Or from repo root: TELEPORT_IMAGE=... bash build.assets/oidc-gate/run.sh

set -euo pipefail

TELEPORT_IMAGE="${TELEPORT_IMAGE:?TELEPORT_IMAGE not set}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE_DIR="$(mktemp -d /tmp/teleport-gate-XXXX)"
NETWORK_NAME="teleport-gate-$$"
CONTAINER_NAME="teleport-gate-$$"
DIAG_PORT=3000
PROXY_PORT=3080

cleanup() {
    echo "=== [oidc-gate] Cleanup ==="
    docker rm -f "$CONTAINER_NAME" 2>/dev/null || true
    docker network rm "$NETWORK_NAME" 2>/dev/null || true
    rm -rf "$GATE_DIR"
}
trap cleanup EXIT

echo "=== [oidc-gate] Generating self-signed TLS cert ==="
openssl req -x509 -newkey rsa:2048 \
    -keyout "$GATE_DIR/tls.key" -out "$GATE_DIR/tls.crt" \
    -days 1 -nodes -subj "/CN=localhost" \
    -addext "subjectAltName=IP:127.0.0.1,DNS:localhost" 2>/dev/null

echo "=== [oidc-gate] Writing Teleport config ==="
cat > "$GATE_DIR/teleport.yaml" <<EOF
teleport:
  data_dir: /var/lib/teleport-gate
  diag_addr: "0.0.0.0:${DIAG_PORT}"
  log:
    output: stdout
    severity: INFO
auth_service:
  enabled: true
  cluster_name: gate-test
ssh_service:
  enabled: false
proxy_service:
  enabled: true
  web_listen_addr: "0.0.0.0:${PROXY_PORT}"
  tunnel_listen_addr: "0.0.0.0:3024"
  https_keypairs:
    - key_file: /gate/tls.key
      cert_file: /gate/tls.crt
EOF

cp "$SCRIPT_DIR/oidc-connector.yaml" "$GATE_DIR/oidc-connector.yaml"

echo "=== [oidc-gate] Creating docker network ==="
docker network create "$NETWORK_NAME"

echo "=== [oidc-gate] Starting Teleport container ==="
docker run -d --name "$CONTAINER_NAME" \
    --network "$NETWORK_NAME" \
    -v "$GATE_DIR:/gate" \
    -p "127.0.0.1:${DIAG_PORT}:${DIAG_PORT}" \
    -p "127.0.0.1:${PROXY_PORT}:${PROXY_PORT}" \
    --entrypoint /usr/local/bin/teleport \
    "$TELEPORT_IMAGE" \
    start --config /gate/teleport.yaml

echo "=== [oidc-gate] Waiting for Teleport to be ready (up to 60s) ==="
READY=0
for i in $(seq 1 60); do
    if curl -sf "http://127.0.0.1:${DIAG_PORT}/readyz" > /dev/null 2>&1; then
        echo "✓ Teleport ready after ${i}s"
        READY=1
        break
    fi
    # Fail fast if container exited
    if ! docker inspect --format '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null | grep -q "true"; then
        echo "✗ Container exited unexpectedly"
        docker logs "$CONTAINER_NAME" | tail -40
        exit 1
    fi
    sleep 1
done
if [ "$READY" = "0" ]; then
    echo "✗ Teleport failed to become ready in 60s"
    docker logs "$CONTAINER_NAME" | tail -40
    exit 1
fi

# ─── Check 1: tctl create OIDC connector ─────────────────────────────────────
echo ""
echo "=== [oidc-gate] Check 1: tctl create OIDC connector ==="
echo "    Expect: exit 0 (connector created or already exists)"
echo "    Fail:   'only available in Teleport Enterprise' (entitlement patch missing)"

# Give the auth server a moment to write its admin identity before tctl connects.
sleep 3

TCTL_OUTPUT=$(docker exec "$CONTAINER_NAME" /usr/local/bin/tctl \
    --config /gate/teleport.yaml \
    create /gate/oidc-connector.yaml 2>&1) || TCTL_EXIT=$?
TCTL_EXIT=${TCTL_EXIT:-0}

echo "    tctl exit code: $TCTL_EXIT"
echo "    tctl output: $TCTL_OUTPUT"

if echo "$TCTL_OUTPUT" | grep -qi "only available in Teleport Enterprise"; then
    echo "✗ FAIL Check 1: OIDC is Enterprise-gated — fork entitlement patch missing!"
    exit 1
fi
if [ "$TCTL_EXIT" != "0" ] && ! echo "$TCTL_OUTPUT" | grep -qi "already exist"; then
    echo "✗ FAIL Check 1: tctl returned exit $TCTL_EXIT with unexpected error"
    echo "    This may mean tctl could not authenticate — check auth server logs:"
    docker logs "$CONTAINER_NAME" | tail -30
    exit 1
fi
echo "✓ PASS Check 1: connector created (no Enterprise gate)"

# ─── Check 2: OIDC login console endpoint → inspect logs ────────────────────
echo ""
echo "=== [oidc-gate] Check 2: OIDC login console endpoint ==="
echo "    Hitting /webapi/oidc/login/console with probe connector..."
echo "    Expect: connector-not-found / discovery error in logs (service wired)"
echo "    Fail:   'Enterprise' or 'not implemented' in logs (patch or registration broken)"

curl -sk -X POST \
    -H "Content-Type: application/json" \
    -d '{"connector_id":"oidc-gate-probe","redirect_url":"http://127.0.0.1:12345"}' \
    "https://127.0.0.1:${PROXY_PORT}/webapi/oidc/login/console" > /dev/null 2>&1 || true

LOGS=$(docker logs "$CONTAINER_NAME" 2>&1)

if echo "$LOGS" | grep -qi "only available in Teleport Enterprise"; then
    echo "✗ FAIL Check 2: Enterprise gate fired in server logs — entitlement patch broken!"
    echo "Relevant log lines:"
    echo "$LOGS" | grep -i "Enterprise" | head -5
    exit 1
fi

if echo "$LOGS" | grep -qi "not implemented"; then
    echo "✗ FAIL Check 2: 'not implemented' in logs — OIDC service not wired (SetOIDCService dropped?)"
    echo "Relevant log lines:"
    echo "$LOGS" | grep -i "not implemented" | head -5
    exit 1
fi

if echo "$LOGS" | grep -qi "nil pointer\|nil dereference\|panic:"; then
    echo "✗ FAIL Check 2: panic in logs — possible nil OIDC service (registration dropped?)"
    echo "Relevant log lines:"
    echo "$LOGS" | grep -i "nil pointer\|nil dereference\|panic:" | head -5
    exit 1
fi

echo "✓ PASS Check 2: no Enterprise gate or service panic in logs"
echo ""
echo "=== [oidc-gate] All checks passed — OIDC patch survived the build ==="
