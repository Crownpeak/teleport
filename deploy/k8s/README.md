# Teleport Kubernetes Deployment

This directory contains Kubernetes manifests for deploying Teleport with OIDC/SAML support.

## Prerequisites

- Kubernetes cluster
- kubectl configured
- nginx ingress controller (or modify ingress.yaml for your ingress)
- TLS certificate (or cert-manager)

## Quick Start

1. **Update configuration:**

   Edit `configmap.yaml` and set:
   - `cluster_name`: Your Teleport cluster name (e.g., `teleport.example.com`)
   - `public_addr`: Your public URL (e.g., `teleport.example.com:443`)

   Edit `ingress.yaml` and set:
   - `host`: Your domain name
   - `secretName`: Your TLS secret name

2. **Deploy:**

   ```bash
   # Using kustomize
   kubectl apply -k .

   # Or apply individually
   kubectl apply -f namespace.yaml
   kubectl apply -f configmap.yaml
   kubectl apply -f pvc.yaml
   kubectl apply -f deployment.yaml
   kubectl apply -f service.yaml
   kubectl apply -f ingress.yaml
   ```

3. **Create admin user:**

   ```bash
   # Get pod name
   POD=$(kubectl get pod -n teleport -l app=teleport -o jsonpath='{.items[0].metadata.name}')

   # Create admin user with password
   kubectl exec -n teleport $POD -- tctl users add admin --roles=editor,access,admin
   ```

4. **Configure OIDC (optional):**

   Edit `oidc-connectors.yaml` with your OIDC provider details, then:

   ```bash
   # Copy to pod
   kubectl cp oidc-connectors.yaml teleport/$POD:/tmp/oidc.yaml

   # Create connector
   kubectl exec -n teleport $POD -- tctl create /tmp/oidc.yaml

   # Verify
   kubectl exec -n teleport $POD -- tctl get oidc
   ```

5. **Set OIDC as default login (optional):**

   Update `configmap.yaml` authentication section:
   ```yaml
   authentication:
     type: oidc
     connector_name: keycloak
   ```

   Then restart the pod:
   ```bash
   kubectl rollout restart deployment/teleport -n teleport
   ```

## Files

| File | Description |
|------|-------------|
| `namespace.yaml` | Teleport namespace |
| `configmap.yaml` | Teleport configuration |
| `pvc.yaml` | Persistent volume for data |
| `deployment.yaml` | Teleport deployment |
| `service.yaml` | ClusterIP service |
| `ingress.yaml` | Ingress with TLS |
| `oidc-connectors.yaml` | Example OIDC configurations |
| `kustomization.yaml` | Kustomize configuration |

## Keycloak Setup

To configure Keycloak as OIDC provider:

1. Create a new client in Keycloak:
   - Client ID: `teleport`
   - Client Protocol: `openid-connect`
   - Access Type: `confidential`
   - Valid Redirect URIs: `https://teleport.example.com/v1/webapi/oidc/callback`

2. Add groups mapper:
   - Go to Client → Mappers → Create
   - Mapper Type: `Group Membership`
   - Token Claim Name: `groups`
   - Add to ID token: ON

3. Get client secret from Credentials tab

4. Update `oidc-connectors.yaml` with your values

## Troubleshooting

```bash
# Check pod status
kubectl get pods -n teleport

# View logs
kubectl logs -n teleport -l app=teleport -f

# Check OIDC status
kubectl exec -n teleport $POD -- tctl get oidc

# Check auth connectors
kubectl exec -n teleport $POD -- tctl get connectors

# Restart deployment
kubectl rollout restart deployment/teleport -n teleport
```

## Image

- Image: `intranet.fredhopper.com/teleport:18.5.1`
- Architecture: `linux/amd64`
- Features: OIDC enabled, SAML enabled, Web UI included
