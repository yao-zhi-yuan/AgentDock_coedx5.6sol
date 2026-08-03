# Threat model

## Scope and current status

This threat model covers the five-week local MVP through phase 4. Controls assigned to later phases remain described as planned.

The MVP is not a multi-tenant service and must not be presented as one.

## Assets

- source repository and its Git history;
- disposable worktree contents;
- patches and verifier evidence;
- event log integrity and ordering;
- lease and fencing state;
- artifact and cassette confidentiality/integrity;
- model credentials supplied outside the repository;
- database credentials supplied outside committed files;
- host filesystem, Docker daemon, kernel, CPU, memory, disk, and network;
- trace and audit data.

## Trust boundaries

```mermaid
flowchart LR
    Operator["Trusted operator"] --> Runtime["AgentDock processes"]
    Runtime --> DB["PostgreSQL"]
    Runtime --> Docker["Docker daemon"]
    Docker --> Container["Untrusted repository + generated code"]
    Runtime --> Provider["External model provider"]
    Runtime --> Telemetry["OTel Collector / Jaeger"]
    Runtime --> Artifacts["Artifact directory"]
```

The model output, target repository, generated patch, tool arguments, and sandbox process are untrusted. PostgreSQL, artifact storage, the Docker daemon, and the host are trusted infrastructure for the local MVP.

## Principal threats and planned controls

| Threat | Planned control | Residual risk |
|---|---|---|
| Prompt/tool injection | **Implemented in phase 4:** five fixed contracts, input/output JSON Schema, capability/path/network/budget policy | semantic manipulation within allowed operations |
| Path traversal | **Implemented in phase 4:** lexical normalization, allowlists, host symlink checks, container-side `os.Root` I/O | filesystem/kernel implementation flaws |
| Host modification | **Implemented in phase 4:** no-checkout worktree, hook/filter-free fixed Git materialization, sanitized worktree pointer, source checkout not mounted, no host-shell tool | trusted host Git and Docker daemon still read repository objects and manage the bind mount |
| Network exfiltration | **Implemented in phase 4:** `network=none`; no network-enabled Tool Contract | host-side future model call still leaves the machine |
| Credential leakage | **Implemented in phase 4 execution:** scrubbed host Git environment, lazy fetch disabled, empty production caller-environment allowlist, fixed Go environment, audit of key names only | image/operator/provider misconfiguration |
| Container breakout | **Implemented in phase 4:** non-root, read-only root, dropped capabilities, no-new-privileges, bounded resources | shared kernel means breakout cannot be ruled out |
| Denial of service | **Implemented in phase 4:** CPU/memory/PID/time/output limits plus kill/remove and worktree cleanup | disk pressure or Docker daemon impact |
| Stale or identity-less execution writes | **Implemented in phase 3:** durable Lease-row mode, PostgreSQL owner/token/expiry checks, legacy-event rejection, and reducer mixed-path defense | non-database side effect must still be scoped-idempotent/receipted |
| Forged verifier success | verifier identity/role and digest-bound evidence | compromised verifier worker or store |
| Replay disclosure | redacted immutable cassettes, credential scanning | prompts or repository text may still be sensitive |
| Replay false equivalence | pinned versions/digests and explicit divergence | unavoidable external or platform nondeterminism |
| Event tampering | transactional append, uniqueness, audit metadata | trusted database administrator can alter data |

## Docker security boundary

Docker is selected because coding-agent workloads need native Go tooling, Git, tests, and process execution. wazero is well suited to pure WebAssembly tools but cannot by itself run the required repository workload without a parallel execution system.

Docker is not a strong security boundary:

- containers share the host kernel;
- the Docker daemon is highly privileged;
- bind mounts can expose host paths if configured incorrectly;
- resource controls do not eliminate kernel or daemon denial of service;
- local developer machines often contain valuable credentials.

The MVP must use a disposable worktree, minimal mounts, non-root user, read-only root filesystem, dropped capabilities, default-denied network, and bounded resources. Even with those controls, hostile third-party code should run on a disposable dedicated runner, not a sensitive workstation.

## Secrets

Committed files contain only documented local example values. Real model keys and production database credentials must enter through an external secret mechanism or untracked `.env`, must be allowlisted per process, and must never be stored in events, artifacts, cassettes, traces, screenshots, or status reports.

## Highest risks

1. Recovery correctness across ambiguous action windows.
2. The limits of Docker as an isolation boundary.
3. Replay consistency and the risk of leaking recorded data.

Each later phase must update this document with implemented controls and test evidence. A planned control is not evidence that the risk is mitigated.

## Phase 3 recovery controls

- Worker registration and heartbeat are durable PostgreSQL rows; process
  heartbeat runs independently of Lease polling and renewal.
- Lease acquisition/takeover is serialized per Run; takeover increments the
  fencing token.
- Managed Event and Receipt transactions reject stale or expired authority
  before idempotency replay, using wall-clock time after lock waits.
- A durable Lease row permanently selects managed execution. The Store rejects
  unleased or legacy lifecycle execution events for that Run, while explicit
  operator desired-state changes remain available.
- Unknown unsafe outcomes stop in `WaitingApproval`.
- Malformed Receipt output or changed Artifact bytes cannot complete a Run;
  recovery dry-reduces and re-hashes the evidence, then stops in
  `WaitingApproval`.
- The 100-iteration Worker Kill harness uses real OS processes and verifies
  terminal convergence plus unique Receipt/Artifact accounting.

These controls do not prevent a stale process from consuming CPU or performing
a non-database side effect. Safety still depends on the action's scoped
idempotency and durable Receipt/Artifact evidence.

## Phase 4 sandbox and Tool controls

- The source checkout is used only as the Git worktree owner and object source.
  Each Run/Attempt gets a private detached `--no-checkout` worktree. Fixed
  `ls-tree`/`cat-file` materialization disables hooks, checkout filters,
  fsmonitor, replacement objects, lazy fetch, interactive protocol use, and
  inherited host credentials. Docker mounts only that directory, with its
  absolute `.git` pointer replaced by a fixed non-host path and over-mounted
  read-only. The private parent root protects the non-sticky disposable
  directory needed for top-level atomic patch replacement.
- Docker resolves the configured sandbox image to a local immutable image ID,
  runs with a numeric non-root user, read-only RootFS, no network, no
  capabilities, `no-new-privileges`, and CPU/memory/PID limits.
- The only caller-visible Tool Contracts are `repo.list`, `repo.read`,
  `repo.search`, `repo.apply_patch`, and `repo.test`. No Tool accepts a host
  executable or shell string.
- Host path normalization rejects absolute paths and `..` before policy. The
  container helper repeats access through `os.Root`; the negative suite covers
  absolute paths, traversal, fixed symlink escape, and a concurrent symlink
  replacement probe.
- The committed caller-environment allowlist is empty. Go control variables
  are fixed (`GOENV=off`, empty `GOFLAGS`) and cannot be overridden. Audit
  events contain key names, contract/policy versions, decisions, effective
  limits, immutable image ID, exit state, and counts, never values or command
  output.
- Allow, deny, completion/failure, timeout, truncation, and Destroy facts are
  written to the phase-4 JSONL audit artifact. They are operational evidence,
  not `VerificationPassed` or any phase-6 Verifier result.
- A cryptographic per-Sandbox owner token plus exact phase/Run/Attempt labels
  gates create/start/kill/remove/failure cleanup/Destroy; operations use the
  immutable container ID after creation. Foreign name collisions and forged
  or scope-mismatched labels are never removed. Timeout/cancellation kills and
  removes the owned container. Destroy is retryable, audits both failure and
  success, fixes permissions with an owned cleanup container, disables further
  Execute calls as soon as cleanup starts, and removes the temporary worktree
  without global `git worktree prune`. A failed worktree removal restores the
  sanitized `.git` pointer before returning. Pending ownership is recorded
  before worktree/container side effects; failed provisioning rollback returns
  a cleanup handle and remains visible to Provider `Cleanup`. An
  outcome-unknown Docker create stays tracked through bounded re-inspection and
  every later empty snapshot. Time passage, name `not found`, and an empty
  owner-token scan do not prove absence; Destroy returns and audits an explicit
  retryable failure while retaining owner/pending/worktree state. Destroy scans
  by full owner token but re-validates exact phase/Run/Attempt labels before any
  fallback removal. Docker Provider cleanup retains the worktree while any
  owned container outcome remains unresolved. Acceptance compares the source
  repository digest and Git status before and after.

Residual risks remain: Docker Desktop/daemon and the shared kernel are trusted;
disk exhaustion is not completely bounded; repositories without vendored or
image-preloaded modules can fail `go test` because network is deliberately
disabled; and a malicious kernel or daemon can defeat these controls.
