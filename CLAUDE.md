# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Teleport: identity-aware access proxy + certificate authority for SSH, Kubernetes, databases, web apps, Windows desktops. Shipped as a single Go binary (`teleport`) plus CLI tools, with a TypeScript web UI and a few Rust components. AGPL-3.0; enterprise code lives in a private submodule (`e/`).

## Build

Toolchain versions pinned in `build.assets/versions.mk` (Go 1.25.x, Node 22.x, Rust via `rust-toolchain.toml`). Build deps: Go, Rust, Node, libfido2, pkg-config.

- `make full` — full build (binaries + web assets) into `build/`.
- `make build/teleport` / `build/tsh` / `build/tctl` / `build/tbot` — single binary.
- `make -C build.assets build-binaries` — dockerized build (no local toolchain needed).
- `make teleport-hot-reload` — rebuild+restart on Go source change (needs `CompileDaemon`; pass `TELEPORT_ARGS='start --config=...'`).
- libfido2 linking via `FIDO2=dynamic|static|off` on `build/tsh`.

### Web UI
- `make docker-ui` or `pnpm build-ui` — build UI.
- `pnpm start-teleport` — dev server; `pnpm start-term` — Teleport Connect (teleterm/Electron).
- pnpm workspace (`pnpm-workspace.yaml`); packages under `web/packages/`.

## Test

- `make test` — everything (helm, sh, api, go, rust, operator, terraform-provider).
- `make test-go-unit` — base Go unit tests (`-race -shuffle on`); excludes e2e, integration, tsh, operator, access, integrations/lib.
- `make test-api` — tests for the separate `api/` module (runs `cd api && go test`).
- `make test-go-tsh` — `tool/tsh/...` tests.
- `make integration` — integration tests (`./integration/...`, 30m timeout; some need `KUBECONFIG`/`TEST_KUBE`).
- `make test-rust`, `make test-sh` (bats), `make test-helm`, `make test-operator`.
- `pnpm test` — JS/TS (jest); `pnpm test-update-snapshot` to refresh snapshots.

Run a single Go test directly (Make targets wrap whole suites):
```
go test ./lib/auth/... -run TestName -race
```
Most server packages need CGO and build tags; if a package fails to build, check the tags in `test-go-unit` (`PAM_TAG`, `BPF_TAG`, `FIPS_TAG`, etc.) and pass `-tags`.

## Lint / format

- `make lint` — api, go (golangci-lint, config in `.golangci.yml`), protos, tools.
- `make lint-go`, `make lint-rust` (clippy + fmt), `pnpm lint` (eslint + prettier), `pnpm type-check`.
- `make fix-imports` — fix Go import grouping. `make fix-license` — add license headers.

## Protobuf / codegen

- Proto sources in `proto/` and `api/proto/`; managed with `buf` (`buf.yaml`, `buf-*.gen.yaml`).
- `make grpc` — regenerate gRPC stubs in a container. `make grpc/host` — run locally (`build.assets/genproto.sh`).
- After changing `.proto` files you must regenerate; `protos-up-to-date` checks staleness in CI.

## Architecture

- **Single binary, many services.** `lib/service` is the daemon supervisor that starts/stops the configured services (Auth, Proxy, SSH node, Kube, App, Database, etc.) and wires their dependencies. Config parsing in `lib/config`. Entrypoint binaries in `tool/teleport`.
- **Auth server** (`lib/auth`) is the cluster's certificate authority and source of truth: issues short-lived certs, handles join/registration, RBAC enforcement (`lib/authz`), and exposes the gRPC API. Resource types (Roles, Users, Nodes, etc.) defined in `api/types` and served via `lib/services` (business logic) over `lib/backend` (pluggable storage: etcd, DynamoDB, Firestore, postgres, sqlite).
- **Proxy** (`lib/proxy`, `lib/web`, `lib/reversetunnel`) is the user-facing entry point: terminates client connections, hosts the web UI/API, and multiplexes connections to agents through reverse tunnels (for resources behind NAT/firewall). `lib/multiplexer` demuxes protocols on a single port.
- **`api/` is a separate Go module** (`api/go.mod`, Apache-2.0) — the public client library and shared types. Imported by the main module via replace. Keep client-facing types and the gRPC client here; don't pull server-only deps into it.
- **CLI tools** (`tool/`): `tsh` (user client), `tctl` (admin), `tbot` (Machine ID / cert renewal agent, impl in `lib/tbot`), `teleport-update` (auto-update), `fdpass-teleport`.
- **Web** (`web/`): React/TypeScript UI for the proxy, plus Teleport Connect desktop app (`web/packages/teleterm`, backed by Go in `lib/teleterm`). Shared design system in `web/packages/design`.
- **Rust** (`Cargo.toml` workspace): RDP client (`rdpclient` / IronRDP) for Windows desktop access, compiled and linked into the Go binary; some WASM (`build-ironrdp-wasm`) for the web UI.
- **`integrations/`**: standalone deployables with their own Makefiles/modules — Kubernetes Operator, Terraform provider, access-request plugins (Slack, PagerDuty, etc.), event-handler. Not part of the main binary.
- **`e/`**: enterprise submodule (private; empty unless checked out). `e_imports.go` lists the enterprise import surface.

## Conventions

- Design changes go through **RFDs** (`rfd/`, numbered markdown). Testing guidelines: `rfd/0001-testing-guidelines.md`.
- New Go deps must be approved and use Go modules (see `CONTRIBUTING.md`).
- Contributions: maintainers create a "buddy PR" incorporating accepted changes.
