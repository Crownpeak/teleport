# Crownpeak Teleport Fork — Upgrade Runbook

This repo is a fork of [gravitational/teleport](https://github.com/gravitational/teleport) that adds OIDC SSO support to the OSS build. This document describes how to upgrade to a new Teleport version.

---

## One-time setup (do this once, skip if already done)

```bash
git remote add upstream https://github.com/gravitational/teleport.git
```

---

## Upgrading to a new version (e.g. 18.8.0 → 18.9.0)

### 1. Fetch the new upstream tag

```bash
git fetch upstream tag v18.9.0 --no-tags
```

### 2. See what Teleport changed in our patch surface

```bash
git diff --stat v18.8.0 v18.9.0 -- lib/auth lib/web lib/modules api/proto
```

Review the output — these are the files where conflicts are most likely to appear.

### 3. Create a new branch from the current patch branch

```bash
git checkout oidc-patch
git checkout -b add-oidc-support-v18.9.0
```

### 4. Rebase onto the new upstream tag

```bash
git rebase --onto v18.9.0 v18.8.0 add-oidc-support-v18.9.0
```

If there are conflicts, resolve them. The most common conflict areas are:
- `lib/auth/auth.go` — if upstream changed `NewServer` near our `SetOIDCService` call
- `api/proto/teleport/legacy/types/types.proto` — if upstream added new fields to `OIDCAuthRequest` after field 26, our nonce field (27) may need to move
- `go.mod` — if upstream changed the `go-oidc/v3` dependency

After resolving each conflict: `git add <file> && git rebase --continue`

### 5. Review what changed in our patch (attach to upgrade PR)

```bash
git range-diff v18.8.0..crownpeak/v18.8.0  v18.9.0..add-oidc-support-v18.9.0
```

Lines marked `=` carried over unchanged. Lines with diffs show what had to be adapted. Include this output in the upgrade PR description.

### 6. Run unit tests locally

```bash
go test ./lib/modules/... -race -v
```

All tests must pass, especially the `TestFork*` canaries. If any fail, the OIDC patch was lost or broken during the rebase.

### 7. Tag and push

```bash
git tag crownpeak/v18.9.0
git push origin add-oidc-support-v18.9.0
git push origin crownpeak/v18.9.0
```

---

## Build the image (Jenkins)

Jenkins is configured as a **Multibranch Pipeline** scanning `add-oidc-support-v*` branches. Pushing the branch in step 7 is enough for Jenkins to discover it.

1. Open the Jenkins **teleport-build** multibranch job.
2. If the branch is not yet listed, click **Scan Multibranch Pipeline Now**.
3. Open the `add-oidc-support-v18.9.0` sub-pipeline.
4. Click **Build Now**.

The pipeline will:
- Verify the OIDC/SAML patch is present in the code
- Run unit canary tests (fails fast if patch is broken)
- Build web assets + binaries inside the buildbox container
- Build and push `intranet.fredhopper.com/teleport:18.9.0`

---

## Deploy

### Dev cluster first (playground)

In `argo-app-config`, update:
```
aws-account/fms-dev/zones/fdp-utils/eu-west-3/eks-cluster/fdp-devutil-main-eks-eu-west-3/shared-infra-utils/teleport-cluster.yaml
```

Change:
```yaml
targetRevision: 18.8.0   →   targetRevision: 18.9.0
```

Merge the PR. ArgoCD auto-deploys within minutes. Verify OIDC login works on the dev cluster.

### Production — root and leaf clusters

Once dev is verified, bump `targetRevision` in **all** production `teleport-cluster.yaml` manifests across AWS accounts — both the root cluster and all adjacent leaf clusters.

> **Warning:** Production ArgoCD has `prune: true` and `selfHeal: true` — it redeploys immediately on merge. Only merge after the image is confirmed pushed and dev is green.

---

## Rollback

Each release is tagged `crownpeak/vX.Y.Z`. To roll back:

1. Revert `targetRevision` in `argo-app-config` to the previous version and merge — ArgoCD redeploys the old image automatically.
2. The old image is still in the registry unless explicitly deleted.

No branch surgery required.

---

## Quick reference

```bash
# What did Teleport change between versions (scoped to our patch surface)?
git diff --stat vOLD vNEW -- lib/auth lib/web lib/modules api/proto

# What did our patch have to change?
git range-diff vOLD..crownpeak/vOLD  vNEW..add-oidc-support-vNEW

# Replay patch onto new release
git rebase --onto vNEW vOLD add-oidc-support-vNEW

# Run fork canary tests
go test ./lib/modules/... -race -v -run TestFork
```
