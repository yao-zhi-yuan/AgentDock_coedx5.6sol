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

## Phase 4 implementation note

The phase-4 implementation resolves the configured helper image tag to an
immutable local image ID and mounts only a detached Run/Attempt worktree at
`/workspace`. The helper uses Go `os.Root` for descriptor-relative repository
I/O, while the host layer applies lexical/path-policy checks. No caller-facing
contract exposes the helper's security probes or a generic command.

The worktree is registered with `--no-checkout`; a scrubbed fixed Git command
surface materializes blobs without hooks, filters, replacement refs, lazy
fetch, or inherited credentials. Its absolute `.git` pointer is sanitized
and over-mounted read-only while mounted; the private root can therefore
remain non-sticky for portable top-level atomic replacement. The pointer is
restored only for Destroy. Containers carry a random
per-Sandbox owner token plus phase/Run/Attempt labels, and lifecycle operations
verify those labels and then address the immutable container ID. Name
collisions or mismatched labels are rejected without deleting the foreign
container. Removal errors remain tracked, are returned and audited, and can be
retried by Destroy. Once Destroy starts, execution stays disabled; a failed
worktree removal re-sanitizes `.git` before returning. The production
caller-environment allowlist is empty and Go control variables are fixed.
Output uses one combined stdout/stderr budget.
