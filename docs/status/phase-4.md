# Phase 4 status: Docker Sandbox, Git worktree, and Policy

- Implementation date: 2026-07-31
- Security-review remediation date: 2026-08-03
- Branch: `codex/agentdock-verify`
- Phase-3/Gate-4 base:
  `8873984c85ed7244fc46b13ff0417df013a28385`
  (`fix: stabilize worker gates and receipt constraints`)
- Construction contract: repository-root `AgentDock-Verify-施工计划.md`
- Proposed subject after all gates:
  `feat: add docker sandbox and policy enforcement`

## Scope

Only phase 4 is implemented. It adds:

- one detached temporary Git worktree per Run/Attempt;
- Docker execution with an immutable local image ID, fixed non-root user,
  read-only RootFS, no network, dropped capabilities, no-new-privileges,
  CPU/memory/PID limits, timeout kill, one combined stdout/stderr budget, and
  an empty production caller-environment allowlist;
- host path normalization plus container-side race-resistant `os.Root` I/O;
- five versioned Tool Contracts;
- input/output JSON Schema validation;
- default-deny static YAML Policy;
- JSONL audit events and a digest-addressed phase-4 audit artifact;
- idempotent container/worktree Destroy;
- a repeatable no-credential security suite and demonstration.

This phase does not add Eino, `EinoReasoner`, `ReplayReasoner`, a real model,
Verifier/Repair or `VerificationPassed` evidence, OTel/product Replay,
Kubernetes, UI, MCP, multi-tenancy, or a generic host-shell Tool.

Phase 3's execution claim remains:

```text
at-least-once execution
+ scoped idempotency
+ fencing
```

No exactly-once or strong multi-tenant isolation claim is made.

## Preflight and Gate-3 baseline

Before edits:

- branch was exactly `codex/agentdock-verify`;
- HEAD was exactly `8873984c85ed7244fc46b13ff0417df013a28385`;
- `git status --porcelain=v1` was empty;
- `AgentDock-Verify-施工计划.md` had no diff;
- the construction plan, phase-3 status, architecture, state machine, event
  model, threat model, Docker ADR, Compose/config, Artifact, Worker, and
  Controller boundaries were read.

Fresh phase-3 baseline:

| Command | Exit | Key evidence |
|---|---:|---|
| `docker version --format '{{.Server.Version}}'` | 0 | Docker server `29.5.2` |
| `docker compose up -d postgres` | 0 | repository PostgreSQL running |
| first sandboxed `make migrate` | 2 | default Go cache was sandbox-denied; not accepted |
| sandboxed `make migrate GOCACHE=/private/tmp/agentdock-go-cache-phase4-baseline` | 2 | localhost was sandbox-denied; not accepted |
| host-context identical migration with explicit cache | 0 | `migrations applied` |
| host-context `go test -tags=integration -count=1 ./internal/lease/... ./internal/controller/...` | 0 | Lease `1.814s`; Controller `8.706s` |
| host-context `go test -tags=chaos -count=1 ./internal/controller/...` | 0 | Controller Chaos `44.620s` |
| host-context `go test -race -count=1 ./internal/...` | 0 | all internal packages passed Race |
| host-context `make chaos-worker-kill` | 0 | `100/100` killed/converged; `artifact_present_at_kill=47` |
| host-context `make demo-phase3` | 0 | token `1→2`, stale A rejected, final `Succeeded` |

The two sandbox access failures were not reused. Only approved host-context
results are baseline evidence.

## Test-first red evidence

1. Policy and Tool surface:
   - Command:
     `go test -count=1 ./internal/policy/... ./internal/tools/...`
   - Exit: 1.
   - Evidence: `NewMemoryAuditRecorder`, Policy `Config/Request`, Contract
     registry, Sandbox types, and all implementation symbols were absent.
     `internal/tools` also reported that `internal/sandbox` had no non-test Go
     files.
2. Sandbox integration:
   - Command:
     `go test -tags=integration -count=1 ./internal/sandbox/...`
   - Exit: 1.
   - Evidence: `internal/policy` had no non-test Go files and the Sandbox
     implementation did not exist.
3. First green compile attempt:
   - Exit: 1.
   - Evidence: the new test helper name collided with the production
     `gitOutput`; the helper was renamed.
4. First real Docker attempt:
   - Exit: 1.
   - Evidence: Docker rejected an invalid `--mount` `rw` field. The bind mount
     now relies on `--mount`'s read-write default.
5. Sandboxed Docker-socket attempt:
   - Exit: 1.
   - Evidence: Docker socket access was denied. It was not accepted as product
     failure or pass.
6. First host-context Docker integration:
   - Exit: 1.
   - The Docker/path behavior tests themselves reached their assertions, but
     Go cleanup raced with Codex Git trace writes under the fixture
     `.git/ai`. Fixture Git commands now set `GIT_TRACE2_EVENT=0`.
   - The required fresh rerun after this fix remains pending because the host
     approval service returned an account usage-limit rejection. The failed
     run is not treated as green.
7. First worktree-pointer hardening:
   - Exit: 1.
   - Evidence: deleting the linked-worktree `.git` file prevented
     `git worktree remove`; the pointer is now transactionally replaced by a
     fixed non-host value and restored only for Destroy.
8. First scrubbed-Git worktree integration:
   - Exit: 1.
   - Evidence: replacing the whole host environment omitted `TMPDIR`; Apple
     Git emitted a warning that polluted machine-readable combined output.
     Git stdout/stderr are now separate and bounded, with fixed `TMPDIR=/tmp`.
9. Canonical cleanup regression:
   - Exit: 1.
   - Evidence: a lexical `/var/...` worktree did not compare within its
     canonical `/private/var/...` root. The provider now uses one canonical
     path domain from registration through retryable cleanup. The identical
     focused worktree suite then exited 0.
10. Independent review found caller `GOFLAGS` could inject `-run`, `-exec`,
    `-toolexec`, overlay, or modfile semantics. The committed Policy now has
    an empty allowlist; Docker fixes empty `GOFLAGS` and `GOENV=off` and rejects
    overrides. The Docker negative suite attempts all three representative
    injections.
11. Owner-label regression red→green:
    - A fixed-token-only mutation of `ownsLabels` made
      `TestSandboxOwnerTokensAreRandomAndScopeLabelsMustMatchExactly` exit 1
      with `mismatched label agentdock.phase="3" was accepted`.
    - Restoring exact phase + Run + Attempt + full random-token comparison
      made the identical command exit 0. The real Docker collision suite also
      keeps foreign containers alive across start/kill/remove/Destroy and
      create-failure cleanup attempts.
12. First full `make sandbox-security-test` after ownership remediation:
    - Exit: 2.
    - Evidence: the concurrent TOCTOU probe exceeded the fixture's two-second
      default command deadline before reaching its assertion. The fixture
      budget was raised to five seconds; the timeout control remains a
      separate 150 ms caller deadline.
13. Cancellation-cleanup stability red→green:
    - Repeating `TestDockerSandboxSecurityControlsAndAudit` five times exposed
      a cancellation race: ownership inspect inherited the already-canceled
      caller context and returned `inspect container ownership: signal:
      killed`.
    - Lifecycle ownership inspection now uses a bounded independent context,
      while Docker start still uses the caller deadline. The identical
      five-run command then exited 0 in `28.188s`.
14. Review-remediation compile red→green:
    - The first local suite exited 1 because the new invalid-resource test
      omitted its `time` import.
    - After adding the import, the identical package set exited 0. Invalid
      CPU/memory/PID/time/output limits are rejected before Docker and emit a
      create-failure audit.
15. First resumed frozen-index reviews (not final):
    - Security increment: **Critical 0 / Important 6 / Minor 1; Ready: No**.
    - Complete `8873984...→index`: **Critical 0 / Important 5 / Minor 3;
      Ready: No**.
    - Both confirmed the random owner token, exact phase/Run/Attempt/token
      labels, immutable-ID lifecycle, scope binding, fixed environment,
      combined output cap, and phase-5 exclusion. They blocked release for
      cleanup error loss, unresolved create cleanup, portable top-level patch,
      patch idempotency, case-aliased root containment, missing lazy-fetch
      proof, create success audit, and Execute-after-Destroy audit.
16. Lifecycle/top-level-patch remediation:
    - The new integration suite first failed to compile because the cleanup
      failure-injection seam did not exist.
    - Real Docker then proved cleanup failure is returned, audited as failure,
      and retained for Destroy; an empty-ID owned name is re-resolved and
      removed. The first top-level patch assertion exited 1 because its JSON
      fixture double-escaped a newline, although the patch command exited 0.
      Correcting that test input made all three named Docker tests pass in
      `8.184s`.
17. Case alias and patch replay red→green:
    - `TestGitWorktreeProviderRejectsCaseAliasRootInsideOrigin` exited 1 after
      the provider accepted an uppercase spelling of the same
      case-insensitive macOS origin and created a root inside it.
    - `TestServiceRejectsReplayablePatchAndEndLineWithoutStartLine` exited 1
      because `old="x", new="xy"` was accepted.
    - Filesystem-identity ancestry checks and the exact-replacement constraint
      made both identical commands exit 0. The controlled missing-promisor
      fixture also exited 0 and proved the unsafe Git command ran its helper
      while the provider's scrubbed Git path did not.
18. Scoped cleanup/evidence Minors:
    - The audit evidence test first failed to compile because requested-scope
      fields did not exist.
    - Audit now records bound plus requested scope and policy/contract versions
      on early denials. Provider active-container queries match only owner
      tokens and Run/Attempt scopes created by that Provider; the real
      cross-Provider collision suite exited 0.
19. Final-range review remediation before Gate execution:
    - A fresh complete `8873984...→index` review reported **Critical 0 /
      Important 3 / Minor 2; Ready: No**. It found overlapping exact
      replacement (`aaa: aa -> a`) still replayable, missing explicit cleanup
      failure audit on timeout/cancel/non-zero paths, and a Destroy failure
      window that could re-expose the real `.git` pointer while Execute
      remained available. The Minors were an owner registry entry retained
      after Destroy and tab/newline ambiguity in the active-container text
      protocol.
    - The helper now rejects any complete patch result that retains the old
      text. Every termination branch explicitly audits owned-container cleanup
      failure and leaves it tracked. Destroy becomes one-way before cleanup,
      independently attempts rollback cleanup, and re-sanitizes `.git` when
      worktree removal fails. Scope IDs reject text delimiters and successful
      Destroy deletes the provider owner entry.
    - Focused local packages exited 0. A freshly rebuilt Docker image then ran
      the overlapping-patch security assertion plus timeout/cancel/non-zero
      cleanup-failure injection tests; all passed in `10.051s`. These are
      remediation checks only, not final Gate evidence.
20. Destroy convergence re-review remediation:
    - The next frozen-index reviews reported **Critical 0 / Important 1 /
      Minor 0** for the repair increment and **Critical 0 / Important 2 /
      Minor 0** for the complete range. Both found the same combination:
      `.git` restore can fail while the worktree provider has already removed
      the workspace. The complete review also caught an integration assertion
      that still expected Execute after a one-way Destroy failure.
    - A successful worktree-provider Destroy now establishes the resource
      terminal state, deletes the owner registry entry, records successful
      destruction, and returns any pointer-restore error only as an additional
      diagnostic. A second Destroy does not try to mount the removed path.
      Collision execution is proved before destruction; a separate test proves
      ownership-denied Destroy is audited, permanently denies Execute, leaves
      the foreign container intact, and succeeds when cleanup is retried after
      the foreign tracking entry is removed.
    - Focused local packages exited 0. The corrected real Docker collision and
      one-way Destroy tests both passed (`4.434s`). These remain remediation
      checks, not final Gate evidence.

## Completed implementation

### Worktree lifecycle

- `GitWorktreeProvider` requires a repository top level, safe revision,
  non-empty Run/Attempt identity, and an owned, private, canonical external
  worktree root.
- A caller-owned existing worktree root must already be mode `0700`; the
  provider rejects a public root without rewriting its permissions.
- `git worktree add --no-checkout --detach` registers a unique path. Scrubbed
  fixed `ls-tree`/`cat-file` commands materialize the resolved commit while
  disabling hooks, fsmonitor, checkout filters, replacement refs, lazy fetch,
  protocol-from-user, inherited Git config, and inherited credentials.
- Host Git stdout/stderr and tree-list output are bounded. Reserved `.git` and
  `.agentdock` paths are rejected case-insensitively.
- Existing ancestors are compared by filesystem identity, so a case or Unicode
  alias cannot place the root physically inside the origin. A controlled
  missing-promisor-object test proves no configured lazy-fetch helper runs.
- Docker mounts only the worktree, never the source checkout.
- The absolute linked-worktree `.git` pointer is sanitized before mount and
  over-mounted read-only. The private parent protects a non-sticky worktree
  root, allowing UID 65532 to atomically replace top-level files. The pointer
  is restored for bounded `git worktree remove --force`; no global prune
  touches unrelated worktrees. Partial-create failure and retry are cleaned by
  the unique canonical path.
- Integration acceptance records tracked-content digest and
  `git status --porcelain=v1` before and after and requires exact equality.

### Docker Sandbox

- The helper image uses a digest-pinned Go `1.26.5` base. Docker resolves the
  built tag to a local immutable SHA-256 image ID before provisioning.
- CPU, memory, PID, command-time, and output-limit configuration is validated
  as strictly positive before Docker is invoked. Provisioning failures emit a
  bounded audit, and any worktree cleanup error is preserved in the returned
  error instead of being discarded.
- Containers run as `65532:65532` with:
  - `--read-only`;
  - `--network none`;
  - `--ipc none`;
  - `--cap-drop ALL`;
  - `no-new-privileges`;
  - CPU, memory, and PID cgroup limits;
  - only `/workspace` as a read-write bind mount, plus its sanitized `.git`
    pointer over-mounted read-only.
- There is no caller-writable tmpfs. Go home/cache/temp state is under
  `/workspace/.agentdock`; `GOENV=off`, `GOFLAGS` is empty, and callers cannot
  override the fixed Go environment.
- Every Sandbox has a cryptographically random owner token. The full token,
  phase, Run, and Attempt are Docker labels. Create/start/kill/remove/failure
  cleanup/Destroy verify every label and then operate on the immutable
  container ID. Cross-Provider name collisions and forged/scope-mismatched
  labels are rejected without removing the foreign container.
- Each command has a bounded context. Deadline expiry kills the container;
  command completion and Destroy remove it.
- Successful provisioning emits `sandbox_created`. Execution removal happens
  before a success audit; failures remain tracked and are returned for Destroy
  retry. An unresolved owned name is re-inspected with all ownership labels.
  Execute after Destroy is rejected with a denial audit.
- A caller cancellation before start is classified and audited as canceled.
  Ownership inspection and lifecycle cleanup use short independent contexts,
  so cancellation cannot turn ownership into an unresolved state; the
  original caller result remains canceled/timeout even when cleanup also
  reports an error.
- stdout and stderr share one combined cap; writers continue consuming after
  the cap so the child cannot block on a full pipe.
- Caller environment keys must pass both Policy and Sandbox allowlists. Audit
  stores key names only.
- The internal container command allowlist exposes only
  `agentdock-sandbox-helper`. Raw `sh`, absolute executables, raw `go`, raw
  `git`, `go test -exec`, and `-toolexec` are not accepted.

### Path and TOCTOU boundary

- Host normalization rejects NUL, absolute/volume paths, lexical `..`, and
  existing symlink escape before Policy. Runtime `.git` and `.agentdock`
  namespaces are case-insensitively reserved.
- The helper repeats all repository I/O through Go `os.Root` at `/workspace`.
  On supported Unix this uses descriptor-relative resolution and prevents an
  escaping symlink between validation and open.
- Negative acceptance includes traversal, absolute path, fixed symlink
  escape, and a concurrent symlink toggle between an in-root file and
  `/etc/passwd`. Only in-root bytes or rejection are allowed.

### Tool Contract, Policy, and audit

The only registered names are:

```text
repo.list
repo.read
repo.search
repo.apply_patch
repo.test
```

Every Contract declares name/version, input/output JSON Schema, capability,
read-only status, timeout, output cap, allowed paths, network permission, and
idempotency semantics. Service order is:

```text
contract lookup
→ input Schema
→ command construction
→ path normalization
→ Contract path allowlist
→ static Policy decision
→ Docker Sandbox
→ output Schema
→ audit
```

The YAML parser rejects unknown fields and any default other than `deny`.
Policy rules constrain capability, read-only property, paths, network,
timeout, output cap, and environment key names.

Invocation Run/Attempt must exactly match the Sandbox-bound scope before any
policy decision or execution. Contract registration recursively rejects
unknown or malformed schema keywords instead of silently accepting a
fail-open declaration.

`repo.apply_patch` rejects a replacement when either the new fragment or the
complete replacement result still contains the old text. This covers
overlapping matches such as `aaa: aa -> a`; a successful exact replacement
therefore removes its precondition and a replay is rejected.

Allow, deny, Sandbox create success/failure, completion/failure/cancellation,
timeout, truncation, Destroy failure, and successful Destroy events are written
to a synced JSONL file.
Zero-valued offline/read-only/success fields remain explicit. Records bind
contract/policy versions, effective paths/network/time/output budgets,
immutable image ID, and CPU/memory/PID limits without copying environment
values or command output. `Artifact()` returns its SHA-256 digest, size, path,
and type `phase-4-audit-jsonl`. This operational artifact cannot emit or stand
in for a phase-6 Verifier result.

## Final frozen-candidate verification (2026-08-03)

All accepted results below were run after the final production-code freeze and
after both implementation reviews reached Critical 0 / Important 0. Only this
status/evidence document was updated afterward; the resulting documentation-
only index was separately reviewed and passed `diff --check`. Commands use
`GOCACHE=/private/tmp/agentdock-go-cache-phase4-final-gate`; PostgreSQL commands
also use the repository-local database URL. An incomplete-output Chaos call and
an incomplete-output first Phase-4 demo call were not accepted; the identical
commands were rerun to a captured exit 0.

### Exact Phase-4 Gate and demonstration

| Command | Exit | Key evidence |
|---|---:|---|
| `go test ./internal/policy/... ./internal/tools/...` | 0 | Policy `0.586s`; Tools `0.873s` |
| `go test -tags=integration ./internal/sandbox/...` | 0 | complete Docker/worktree integration passed in `16.719s` |
| `make sandbox-security-test` | 0 | freshly built pinned image; all traversal/absolute/symlink/TOCTOU, network, timeout/cancel, PID, output, non-root, RootFS/workspace, ownership collision, cleanup/audit, worktree and lazy-fetch cases passed; test time `16.726s` |
| `git diff --check` | 0 | no whitespace errors |
| `make demo-phase4` | 0 | all five tools exercised; sensitive host path rejected; network test printed `network-request-denied`; patched tests passed |

The accepted Phase-4 demo recorded:

```text
origin_before digest=sha256:04167e14a3fa2521b6a138aaaf39e264439da230a54c5e00cee762e830071983 status=""
origin_after  digest=sha256:04167e14a3fa2521b6a138aaaf39e264439da230a54c5e00cee762e830071983 status=""
destroy_verified containers=0 worktree_removed=true origin_unchanged=true
audit_artifact type=phase-4-audit-jsonl size=6956
```

### Phase-3 core Gate

| Command | Exit | Key evidence |
|---|---:|---|
| `go test -tags=integration -count=1 ./internal/lease/... ./internal/controller/...` | 0 | Lease `1.942s`; Controller `8.276s` |
| `go test -tags=chaos -count=1 ./internal/controller/...` | 0 | Controller passed in `42.763s` |
| `go test -race -count=1 ./internal/...` | 0 | all internal packages passed Race |
| `make chaos-worker-kill` | 0 | `iterations=100 killed=100 succeeded=100 waiting_approval=0 artifact_present_at_kill=43` |
| `make demo-phase3` | 0 | A token 1 killed, B token 2 takeover, stale A append rejected, final `Succeeded` |

### Phase-2 and Phase-1 regression

| Command | Exit | Key evidence |
|---|---:|---|
| `go test -tags=integration -count=1 ./internal/store/... ./internal/controller/...` | 0 | Store `5.964s`; Controller `7.255s` |
| `go test -tags=integration -count=1 ./internal/migration/... ./cmd/agentdock` | 0 | Migration `1.444s`; CLI `1.537s` |
| `go test -race -count=1 ./internal/store/... ./internal/controller/...` | 0 | both packages passed Race |
| `make test-rebuild-state` | 0 | domain golden/checkpoint and PostgreSQL 1000-event rebuild passed |
| `make test-integration` | 0 | Store, Lease, Controller, Migration, and CLI passed |
| Phase-1 six named reducer/controller regressions, `-count=1 -v` | 0 | all pause/resume, illegal Tool call, reasoning and verification concurrency cases passed |
| Phase-1 three concurrent pause races, `-count=50` | 0 | all 50 repetitions passed |

### Full repository and environment

| Command | Exit | Key evidence |
|---|---:|---|
| `go test -count=1 ./...` | 0 | every repository package, including configcheck, passed |
| `go test -race -count=1 ./...` | 0 | every repository package passed Race |
| `make test` | 0 | full package suite passed |
| `make test-race` | 0 | full package Race suite passed |
| `make lint` | 0 | `go vet ./...` target passed |
| `go vet ./...` | 0 | no diagnostics |
| `go mod verify` | 0 | `all modules verified` |
| `make migrate` (initial and final idempotency rerun) | 0 | both printed `migrations applied` |
| `make doctor` | 0 | Go `1.26.5`, reachable Docker daemon, Compose `5.1.3`, ports, YAML, and Compose configuration passed |
| `make demo-fake` | 0 | successful fake Run and pause/resume both converged to `Succeeded` |

The first two `make doctor` attempts exited 2 because another repository's
already-running OTel/Jaeger containers owned the default ports. With explicit
user approval those exact two containers were temporarily stopped; default
`make doctor` then exited 0, both containers were immediately restarted, and
Docker inspect confirmed both `Running=true`. No failed or alternate-port
doctor result is counted.

Final resource and freeze checks found no container with label
`agentdock.phase=4`, only the repository's main Git worktree, no unstaged diff,
no construction-plan diff, and both cached and working-tree `diff --check`
clean. The branch and pre-commit HEAD remained exactly
`codex/agentdock-verify` and `8873984c85ed7244fc46b13ff0417df013a28385`.

## Known limitations

- Docker and its daemon share/trust the host kernel. This is not a VM, a
  hostile-tenant boundary, or a production multi-tenant sandbox.
- The Docker daemon is privileged trusted infrastructure. A daemon or kernel
  compromise can bypass these controls.
- Disk consumption is not fully quota-isolated.
- Network is always disabled in phase 4. A repository without vendored or
  image-preloaded modules can fail `repo.test`; phase 4 does not add an egress
  proxy or credential broker.
- `repo.apply_patch` is deliberately an exact single replacement, not a
  general patch language or host Git client.
- The Tool Service is ready to be injected behind the phase-3 managed
  `ActionExecutor`, but phase 4 does not connect Eino/Reasoner Tool calls.
- Audit JSONL is local operational evidence. PostgreSQL registration or
  phase-6 verification semantics are not fabricated.
- `RecordBounded` supplies a five-second cooperative context, but Go cannot
  preempt a recorder already blocked in a local mutex, write, flush, or
  `fsync`. Audit errors remain fail-closed and are returned; the deadline is
  not a hard kernel-I/O timeout.

## Independent review

`requesting-code-review` inspected both each repair increment and the complete
`8873984c85ed7244fc46b13ff0417df013a28385..index` range from independent
read-only contexts. Earlier No verdicts and their red→green remediation are
recorded above; none was reused as a pass.

Final production-code frozen verdicts:

| Review | Critical | Important | Minor | Ready |
|---|---:|---:|---:|---|
| final Destroy repair increment | 0 | 0 | 0 | Yes |
| final complete BASE→index range | 0 | 0 | 0 | Yes |

The complete review also confirmed Phase-4/Phase-5 scope compliance and found
no Eino/ReplayReasoner, real model, Verifier/Repair, OTel Replay, Kubernetes,
UI, or MCP implementation.

## Gate 4

**PASS.** The final frozen-candidate exact Phase-4 Gate, manual demonstration,
Phase-3 core Gate, Phase-2/Phase-1 regression, full repository test/Race/static
and environment checks, resource cleanup checks, and both independent reviews
all pass with Critical 0 / Important 0. The delivery commit and post-commit
parent/branch/clean-worktree check are the remaining mechanical handoff steps.
Phase 5 remains unstarted.
