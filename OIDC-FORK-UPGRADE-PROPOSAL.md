# Proposal: A Repeatable Process for Upgrading the Crownpeak Teleport Fork

**Status:** Proposal (not yet adopted)
**Author:** Platform / Infra
**Date:** 2026-06-25
**Applies to:** https://github.com/Crownpeak/teleport

---

## 1. Background

Crownpeak runs a fork of [gravitational/teleport](https://github.com/gravitational/teleport) so that **OIDC SSO works in the OSS build** without an Enterprise license. Critical infrastructure access depends on this fork. New builds are produced **rarely** — only when we adopt a newer Teleport release — so the overriding priority is **confidence that OIDC still works after an upgrade**, not build speed.

### What the fork actually changes

The patch is **not** a single flag. It has two inseparable parts:

1. **Entitlement bypass** — `lib/modules/modules.go` forces the OIDC and SAML entitlements to `Enabled: true`. Required so the OIDC connector CRUD gate in `lib/auth/auth_with_roles.go` (which otherwise rejects with *"OIDC is only available in Teleport Enterprise"*) is passed.
2. **A custom OSS OIDC login implementation** — upstream OSS ships only an empty seam (`OIDCService` interface in `lib/auth/oidc.go`); the real login flow lives in the private Enterprise `e/` submodule. The fork fills that seam itself:
   - `lib/auth/oidc_service.go` (~1000 lines): discovery, callback validation, nonce check, claims→roles mapping, user creation, cert issuance.
   - `lib/auth/auth.go`: registers the service in `NewServer`.
   - `lib/auth/apiserver.go`: `POST /:version/oidc/requests/validate`.
   - `lib/web/oidc.go` + `lib/web/apiserver.go`: `/webapi/oidc/login/web`, `/callback`, `/login/console`.
   - `api/proto/.../types.proto`: `OIDCAuthRequest.nonce` field (+ regenerated `types.pb.go`).
   - `Jenkinsfile`, `build.assets/Dockerfile.oidc`, deploy manifests.

This whole surface — not just `modules.go` — must survive every upgrade.

## 2. Problems with the current process

The fork currently creates **a brand-new branch per version** (`add-oidc-support-v18.5.1-v2`, `add-oidc-support-v18.7.6`, ...), each hand-edited. This causes:

1. **No upstream reference points.** The repo has *no* `upstream` remote and *zero* Teleport release tags. The base commit `Release 18.7.6` sits in history **untagged**. We literally cannot run `git diff v18.5.1 v18.7.6` to see what Teleport changed.
2. **Can't follow our own patch across versions.** "What did we have to adapt going from 18.5.1 to 18.7.6?" is answerable only by manual eyeballing.
3. **Branch drift.** The `Verify Image` Jenkins stage was *added* on the 18.7.6 branch but *removed* on the 18.5.1 branch — divergent hand-edits, no single source of truth.
4. **Risk of silently dropping the patch.** A botched re-create could ship a build where OIDC quietly reverts to "Enterprise only".

## 3. Goals

- See exactly **what Teleport changed** between the old and new version, scoped to the code our patch touches.
- See exactly **what our patch had to change** to adapt — commit by commit.
- A **single source of truth** for the patch, replayed onto each release (no per-branch hand-edits).
- **Immutable, reproducible** release points.
- A clear, short **runbook** any team member can follow.

## 4. Proposed model

> **One curated patch series, replayed onto each upstream release tag, with upstream tags available for diffing.**

This is the standard "carry a small patch set on top of upstream releases" model.

### 4.1 Reference points (one-time)

Add the upstream remote and fetch its tags so every Teleport release is locally available:

```bash
git remote add upstream https://github.com/gravitational/teleport.git
git fetch upstream --tags
```

### 4.2 The patch branch

Maintain **one** branch, `oidc-patch`, containing the OIDC work as a **clean, ordered series of logical commits** (squashed from today's 19 noisy commits):

```
oidc: bypass OIDC/SAML entitlement gate         # lib/modules/modules.go
oidc: OSS auth service implementation           # lib/auth/oidc_service.go, auth.go, apiserver.go
oidc: web login routes                          # lib/web/oidc.go, apiserver.go
oidc: add OIDCAuthRequest.nonce proto field     # api/proto + regenerated pb.go
ci: jenkins pipeline + docker packaging         # Jenkinsfile, Dockerfile.oidc, deploy/
```

Keeping each concern in its own commit means conflicts during an upgrade land in a small, well-labelled place.

### 4.3 Branch / tag naming

| Ref | Meaning | Mutable? |
|-----|---------|----------|
| `master` | Pure mirror of upstream master. Never patched. | tracks upstream |
| `oidc-patch` | The current patch series, based on the latest adopted release. | rebased forward |
| `crownpeak/v18.7.6` | Immutable snapshot = `v18.7.6` + patch series. What Jenkins builds. | **no** |

`master` stays an untouched upstream mirror — do **not** merge version branches into it (it tracks a different/newer release line; merging produces a tree that builds neither version cleanly).

## 5. Per-upgrade runbook (e.g. 18.7.6 → 18.8.0)

```bash
# 0. Refresh upstream tags
git fetch upstream --tags

# 1. SEE WHAT TELEPORT CHANGED in the code our patch touches
git log --oneline v18.7.6..v18.8.0
git diff --stat v18.7.6 v18.8.0 -- lib/auth lib/web lib/modules api/proto
#   -> this is the "what might break our patch" checklist

# 2. REPLAY our patch onto the new release
git rebase --onto v18.8.0 v18.7.6 oidc-patch
#   -> conflicts appear ONLY where upstream touched code our patch touches.
#      Resolve them (this is the real adaptation work, e.g. the v18.7.6 nonce-field move).

# 3. Regenerate generated code if proto/build inputs changed
make grpc            # if api/proto changed
make ensure-webassets

# 4. Tag an immutable release point
git tag crownpeak/v18.8.0
git push origin oidc-patch crownpeak/v18.8.0

# 5. SEE WHAT WE HAD TO ADAPT (commit-by-commit)
git range-diff v18.7.6..crownpeak/v18.7.6  v18.8.0..crownpeak/v18.8.0
#   -> unchanged commits marked '=', adapted commits show exact line edits.
#      Attach this output to the upgrade PR/ticket as the review artifact.

# 6. BUILD + PUBLISH the image
#    Jenkins: run the pipeline with CHECKOUT_BRANCH=crownpeak/v18.8.0 and
#    TAG_PUSH_VERSION=18.8.0. It pushes intranet.fredhopper.com/teleport:18.8.0.

# 7. DEPLOY via ArgoCD (separate repo: argo-app-config)
#    The upgrade is NOT done at the git tag — the image is only deployed once
#    the Argo Application points at the new version. Bump targetRevision
#    in the teleport-cluster.yaml, then PR + merge (Argo auto-syncs).
```

> **The deploy lives in a second repo.** Teleport is deployed by the upstream
> `teleport-cluster` Helm chart via an ArgoCD `Application`, with the image
> overridden to our fork. Example manifest:
> `argo-app-config/.../shared-infra-utils/teleport-cluster.yaml`.
>
> Per upgrade, change **one number** — `targetRevision`:
>
> ```yaml
> source:
>   targetRevision: 18.8.0          # Helm CHART version (must match the fork's Teleport version)
>   helm:
>     values: |
>       image: intranet.fredhopper.com/teleport   # fork repo; tag is NOT set here
> ```
>
> No image tag is set, so the chart resolves the image tag from its own
> `appVersion` → it pulls `intranet.fredhopper.com/teleport:<appVersion>`. This
> works **only if** Jenkins pushed an image tagged with that same version (see
> warning below). The team has chosen to rely on this implicit coupling for now
> rather than pin `imageTag` explicitly.
>
> Argo `syncPolicy.automated` has `prune` + `selfHeal`, so a merged change
> **redeploys immediately** on a production, critical-infra cluster. Land it
> only after the verification gate (§6) is green, and check for
> **other instances** of `teleport-cluster.yaml` (other zones/regions) that need
> the same bump.
>
> **Warning — keep these versions aligned.** A successful deploy requires all of
> these to be the same number. Since `imageTag` is **not** pinned, the pulled tag
> is derived from the chart's `appVersion`, so the only number the team sets is
> `targetRevision` — everything else must already match it:
>
> | Version | Set in | How |
> |---|---|---|
> | Fork binary version | `api/version.go` | from the upstream tag the patch is rebased onto |
> | Image tag pushed | `Jenkinsfile` `TAG_PUSH_VERSION` | pipeline parameter — **must equal the chart `appVersion`** |
> | Image tag pulled | (implicit) chart `appVersion` | derived; not set in the manifest |
> | Helm chart version | `teleport-cluster.yaml` `targetRevision` | the one number the team bumps |
>
> Because the pulled tag is implicit, the failure mode to watch is **chart
> `appVersion` ≠ the tag Jenkins pushed** → `ImagePullBackOff`. Confirm Jenkins
> `TAG_PUSH_VERSION` matches the chart's `appVersion` for the target
> `targetRevision`.
>
> Never bump `targetRevision` (chart) past the Teleport version the fork image
> was built from — a newer chart can expect config/schema the older image does
> not support. Avoid relying on a `:latest` image tag; deploy only versioned tags.

## 6. Verification gate (before publishing the image)

A successful compile does **not** prove OIDC works. Every upgrade must pass, in order:

1. **Unit canaries** (fast, fail early): assert the entitlement bypass holds (OIDC/SAML enabled, HSM/Policy still disabled) and that `oidc_service.go`'s pure logic (claims→roles, nonce validation) is intact.
2. **Full `tsh login` E2E** (the real gate): boot the built Docker image + a hermetic OIDC IdP (Keycloak), perform a real headless `tsh login --auth=oidc`, assert a cert is issued and the output does **not** contain *"only available in Teleport Enterprise"*.

The Jenkins pipeline runs (1) before **Build Binaries** and (2) before **Push Docker Image**. A red gate blocks the push. *(Detailed test design tracked separately.)*

## 7. Rollback

Release points are immutable tags. To roll back, redeploy the previous `crownpeak/vX.Y.Z` image (still in the registry) or rebuild from its tag. No branch surgery required.

## 8. One-time migration to adopt this model

1. Add `upstream` remote, fetch tags (§4.1).
2. Tag the current base/release retroactively: `git tag crownpeak/v18.7.6 <current HEAD>`.
3. Curate the 19 commits on `add-oidc-support-v18.7.6` into the ~5-commit `oidc-patch` series.
4. Verify `git range-diff` between the old branch and the new series shows **no net code change** (pure history cleanup).
5. Point the Jenkins `CHECKOUT_BRANCH` default at the tag/branch and document the runbook (§5) in the repo README.

## 9. Trade-offs / FAQ

- **Why rebase, not merge?** Rebase keeps the patch as a readable series replayable onto any tag, and makes `range-diff` meaningful. Merging tangles patch and upstream history, turning each upgrade into a conflict slog.
- **Conflicts every upgrade?** Only where upstream edited the exact lines we patch — a small, bounded surface (`lib/auth`, `lib/web`, `lib/modules`, `api/proto`). The runbook surfaces them up front (§5 step 1).
- **What if upstream restructures the OIDC seam?** `range-diff` + the E2E gate catch it. The adaptation commit becomes larger that cycle; the process is unchanged.
- **Do we still keep old version branches?** No need — the immutable `crownpeak/vX.Y.Z` tags replace them.

---

### Appendix A — Quick command reference

```bash
# what Teleport changed between versions (scoped to our surface)
git diff --stat vOLD vNEW -- lib/auth lib/web lib/modules api/proto

# how our patch had to adapt
git range-diff vOLD..crownpeak/vOLD vNEW..crownpeak/vNEW

# replay patch onto new release
git rebase --onto vNEW vOLD oidc-patch
```
