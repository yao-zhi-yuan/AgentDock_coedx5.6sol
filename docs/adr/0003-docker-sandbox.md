# ADR 0003: Docker sandbox with disposable Git worktrees

- Status: Accepted
- Date: 2026-07-30

## Context

A coding agent must read, modify, and test a Go repository using Git and native process tooling without changing the user's original checkout. The execution environment must apply network, process, resource, timeout, output, and path controls.

## Decision

Use one disposable Git worktree per Attempt and execute declared tools in Docker. The planned default is no network, non-root user, read-only root filesystem, a writable worktree mount only, dropped capabilities, bounded CPU/memory/PIDs/time/output, an environment allowlist, and guaranteed cleanup.

No arbitrary host shell tool will be exposed.

## Why not wazero alone

wazero is attractive for pure WebAssembly functions, but the MVP workload needs Git, the Go toolchain, repository tests, and general subprocess behavior. Implementing both a wazero tool system and a Docker repository sandbox would duplicate policy and evidence work and is explicitly out of scope.

## Security boundary

Docker shares the host kernel and depends on a privileged daemon. It reduces risk but is not a strong multi-tenant boundary. The MVP must not run hostile third-party repositories on a sensitive workstation or claim container isolation is equivalent to a VM or dedicated tenant boundary.

## Consequences

- The recorded demo depends on a functioning Docker daemon.
- Worktree lifecycle and mount/path validation require negative tests.
- Image versions must be pinned for replay evidence.
- Stronger tenant isolation is a future architectural replacement, not a phase 0 abstraction layer.
