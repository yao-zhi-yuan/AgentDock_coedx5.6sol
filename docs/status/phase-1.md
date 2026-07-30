# Phase 1 status: deterministic domain model and state machine

- Date: 2026-07-30
- Branch: `codex/agentdock-verify`
- Phase 0 baseline: `8b0a46b8da0f736bac243c2beb28f1c9a242d281`
- Planned commit: `feat: add deterministic runtime state machine`
- Construction contract: repository-root `AgentDock-Verify-施工计划.md`

## Scope

Only phase 1 was implemented. This phase adds a deterministic in-memory runtime without PostgreSQL, migrations, leases/fencing, Docker/worktrees, Tool Contracts, Policy, Eino adapters, ReplayReasoner, real Verifiers/Repair, fault injection, OTel, UI, or Kubernetes.

The phase 1 workspace/provision/patch/verification events are inert state-machine facts used to exercise the lifecycle. They do not provision a workspace, execute a tool, apply a patch, or run a verifier.

## Gate 0 preflight

The required baseline was rerun before phase 1 implementation:

| Command | Exit | Key evidence |
|---|---:|---|
| `go mod verify` | 0 | `all modules verified` |
| `go test -race ./...` | 0 | phase 0 configcheck/doctor package passed |
| `go vet ./...` | 0 | no output |
| `make doctor` in approved host context | 0 | Go, Docker daemon, Compose, required parameters, configured ports, YAML, and Compose model passed |
| `git diff --check` | 0 | no output |

The first sandboxed `make doctor` exited 2 because the restricted process could not access the Docker socket. The required host-context rerun reached the daemon and exited 0. No phase 0 code repair was necessary.

## Completed work

1. Added explicit `Run`, `Attempt`, `Event`, `EventData`, `State`, `Command`, desired-state, observed-state, and event/command vocabulary types.
2. Added pure `Reduce(events) -> State` behavior:
   - requires contiguous sequence numbers starting at one;
   - requires one Run ID, versioned payloads, and unique idempotency keys;
   - rejects missing, out-of-order, duplicate, unsupported, paused-work, and terminal-follow-up events;
   - obtains timestamps only from events and has no clock, random source, network client, or process-global state.
3. Added pure `Decide(state) -> Command` behavior with deterministic SHA-256-derived action IDs and durable pending-action reuse.
4. Added the program-owned transition table. Reasoner results contain no target state.
5. Added Pause/Resume behavior that preserves the exact interrupted observed state and pending action ID. New actions are blocked after Pause, while matching results for already planned Reasoning and Verification actions remain auditable and update the saved resume path/evidence.
6. Added Cancel convergence from active and paused states. Terminal Runs cannot advance or accept new desired state.
7. Added the minimum framework-neutral `Reasoner` seam and race-safe `FakeReasoner`.
8. Added the inert phase 1 `phase1.patch` Tool Call marker. Unknown Tool names and malformed results persist `ToolCallFailed`; the next active Reconcile deterministically decides `FailRun` and persists `RunFailed`. No phase 4 Tool Contract or execution exists.
9. Added a race-safe in-memory Event Store:
   - Store-assigned sequence numbers;
   - full candidate-log validation before visibility;
   - defensive Load copies;
   - exact idempotent replay returns the existing event;
   - same key with different content returns `ErrIdempotencyConflict`;
   - concurrent identical append produces one event;
   - concurrent conflicting Create facts produce one success and one explicit conflict.
10. Added single-process Reconcile. Each call loads events, reduces state, decides one Command, and persists its phase 1 intent/result events.
11. Added CLI commands `run create/get/step/pause/resume/cancel`.
12. Added `agentdock session`, which runs newline-delimited commands against one memory Store so the six commands form a usable real CLI chain without introducing a file Store or phase 2 persistence.
13. Added `make lint`, `make test`, `make test-race`, and `make demo-fake`.
14. Updated README, architecture, state-machine, event-model, and demo documentation to distinguish implemented phase 1 behavior from planned later phases.

## Test-first and red-green evidence

The following failures were intentionally observed and were not treated as passing evidence:

1. Initial phase 1 acceptance tests were added before implementation.
   - Command: `go test ./internal/domain/... ./internal/controller/... ./internal/reasoner/... ./internal/store/... ./internal/cli/...`
   - First restricted attempt: exit 1 because the sandbox could not write the host Go build cache; this was an environment failure, not accepted as functional red.
   - Approved host-context rerun: exit 1 with `no non-test Go files` for all five target packages.
   - After implementation: the same package set exited 0.
2. Independent review found that Paused state masked Cancelled intent.
   - Red command: `go test -count=1 ./internal/domain ./internal/controller -run 'TestDecideCancelIntentPreemptsPausedNoop|TestCancelConvergesFromPausedRun'`
   - Exit: 1.
   - Key failures: `Decide(paused cancel intent) = Noop, want CancelRun`; Controller remained `Paused`.
   - After moving Cancel decision ahead of Paused Noop: the same command exited 0.
3. Independent review found silent idempotency conflicts and an unusable cross-command binary lifecycle.
   - Red command: `go test -count=1 ./internal/store ./internal/cli ./cmd/agentdock -run 'TestMemoryStoreRejectsIdempotencyKeyWithDifferentEvent|TestSessionRunsRequiredCLIChainInOneProcess|TestRunEntryPointKeepsSessionStateForRequiredCommands'`
   - Exit: 1.
   - Key failures: undefined `ErrIdempotencyConflict`, unknown `session` command, and missing entrypoint `run` seam.
   - After explicit conflict validation and the one-process CLI session: the same command exited 0.
4. A post-commit Gatekeeper review rejected Gate 1 with Critical 0 and Important 2.
   - Red command: `go test -count=1 -v ./internal/domain ./internal/controller -run 'TestReduceAcceptsPlannedActionResultsWhilePaused|TestToolCallFailedPrefixDecidesStableFailRun|TestIllegalFakeToolCallBecomesControlledFailure|TestReasoningCompletionPersistsAcrossConcurrentPause|TestReasoningFailurePersistsAcrossConcurrentPauseAndConverges|TestVerificationCompletionPersistsAcrossConcurrentPause'`
   - Exit: 1.
   - Paused result failures:
     - `ReasoningCompleted`: required `desired=Running observed=Reasoning`, got `desired=Paused observed=Paused`;
     - `ToolCallFailed`: the same invalid transition;
     - `VerificationPassed`: required `desired=Running observed=Verifying`, got `desired=Paused observed=Paused`.
   - Failure-prefix assertion: `Decide(failed prefix) = RunReasoner, want FailRun`.
   - Controller assertions: the old implementation either returned the invalid transition above or immediately reached `Failed` instead of leaving an auditable `ToolCallFailed` prefix.
   - After separating planned-result validation from new-action validation, updating the saved resume path/evidence, making `FailureReason` decide stable `FailRun`, and separating the two failure appends: the identical command exited 0 and all six named tests printed PASS.

The Cobra content and its two selected transitive modules initially lacked content checksums because phase 0 used only their `go.mod` hashes. `go mod download` added the pinned content hashes; the two indirect modules were recorded in `go.mod` without running `go mod tidy`, which would have removed dependencies intentionally pinned for later phases.

## Gatekeeper remediation acceptance

Fresh commands after the Gatekeeper fixes and before the required follow-up review:

| Command | Exit | Key evidence |
|---|---:|---|
| targeted red command above, before implementation | 1 | all three paused result classes plus failure-prefix convergence failed with the recorded assertions |
| identical targeted command after implementation | 0 | domain prefix/result tests and three logical-concurrency Controller tests passed |
| `go test -count=1 ./internal/domain/... ./internal/controller/...` | 0 | complete phase 1 domain/controller set passed |
| `go test -race -count=1 ./internal/domain/... ./internal/controller/...` | 0 | complete set, including channel-controlled concurrency windows, passed under Race |
| `go test -count=1 ./...` | 0 | all Go packages passed |
| `go test -race -count=1 ./...` | 0 | all Go packages passed under Race |
| `go vet ./...` | 0 | no output |
| `go mod verify` | 0 | `all modules verified` |
| `make demo-fake` | 0 | successful event path and Pause/Resume demo unchanged |
| `git diff --check` | 0 | no output |

## Required automatic acceptance

Final fresh commands after the Gatekeeper remediation review:

| Command | Exit | Key evidence |
|---|---:|---|
| Gatekeeper six-test red command above | 0 | all pure-state and logical-concurrency regressions printed PASS |
| three channel-controlled concurrency tests with `-count=50` | 0 | Reasoning completion/failure and Verification completion windows passed 50 repetitions |
| `go test -count=1 ./internal/domain/... ./internal/controller/...` | 0 | domain and controller passed uncached |
| `go test -race -count=1 ./internal/domain/... ./internal/controller/...` | 0 | domain and controller passed under Race |
| `go test -count=1 ./...` | 0 | cmd, CLI, controller, domain, reasoner, store, and phase 0 configcheck passed |
| `go test -race -count=1 ./...` | 0 | all Go packages passed under Race |
| `make test` | 0 | all Go packages passed through the unified Make target |
| `make test-race` | 0 | all Go packages passed under Race through the unified Make target |
| `make lint` | 0 | `go vet ./...` completed with no output |
| `go vet ./...` | 0 | no output |
| `go mod verify` | 0 | `all modules verified` |
| `make doctor` in approved host context | 0 | Docker daemon, Compose, parameters, ports, YAML, and Compose model passed |
| `make demo-fake` | 0 | required event sequence and Pause/Resume output shown |
| `git diff --check` | 0 | no output |
| `git diff --cached --check` | 0 | no output; staged scope contains only the nine phase 1 remediation and status files |

The sandboxed final `make doctor` attempt exited 2 only because the restricted process could not reach the Docker socket. The required host-context rerun exited 0 with every declared check passing.

The exact `make demo-fake` output was:

```text
=== successful fake Run ===
RunCreated
AttemptStarted
WorkspaceProvisioned
ReasoningPlanned
ReasoningCompleted
PatchProduced
VerificationPlanned
VerificationPassed
RunSucceeded
FinalState=Succeeded Events=9
=== pause / resume ===
BeforePause=Provisioning
Paused=Paused ResumeTarget=Provisioning
Resumed=Provisioning
PauseResumeFinal=Succeeded
```

## Named invariant and negative acceptance

The combined named test command exited 0 and printed PASS for:

- empty event log returns `InitialState`;
- byte-for-byte JSON-equivalent repeated reduction;
- reducer source rejects imports of time, random, network, HTTP, and OS packages;
- first seq not one, sequence gap, out-of-order event, duplicate idempotency key, missing `RunCreated`, unknown payload version, and work while paused;
- terminal event follow-up and direct `Succeeded -> Acting`;
- stable action ID for the same state;
- pending action ID preserved across Pause/Resume;
- matching `ReasoningCompleted`, `ToolCallFailed`, and `VerificationPassed` accepted after Pause without producing a new command;
- channel-controlled Reasoning completion/failure and Verification completion races preserve results and do not repeat completed work after Resume;
- `ToolCallFailed` prefix decides a stable `FailRun`, and the next Reconcile converges to `RunFailed` without a second Reasoner call;
- paused Cancel intent preempts Noop and converges to Cancelled;
- identical idempotent append is stored once;
- different content under the same idempotency key returns a conflict;
- concurrent identical append stores one event;
- concurrent conflicting Create facts produce one success and one conflict;
- ten paused Reconcile calls produce no Command, Event, or Reasoner call;
- illegal FakeReasoner Tool Call produces controlled failure;
- injected CLI and real `cmd/agentdock` entrypoint execute the required command chain in one process.

## Manual CLI acceptance

The real entrypoint was run as a newline-delimited `agentdock session` with:

```text
run create run-blackbox --scenario scenario --spec-hash spec
run get run-blackbox
run step run-blackbox
run pause run-blackbox
run resume run-blackbox
run cancel run-blackbox
```

The command exited 0. Its observed-state path was:

```text
Queued -> Queued -> Provisioning -> Paused -> Provisioning -> Cancelled
```

The `run step` output included `command=StartAttempt`, payload version 1, seq 2, and a stable action ID. The final response included `desired_state=Cancelled`, `observed_state=Cancelled`, version 6, and `last_event_type=RunCancelled`.

The same real-entrypoint session was rerun after the Gatekeeper remediation review and exited 0 with the same observed-state path and terminal assertions.

## Boundary checks

| Check | Exit | Evidence |
|---|---:|---|
| actual `cmd/agentdock` dependency graph forbidden-package scan | 0 | no Eino, PGX/PostgreSQL, Docker, OTel, or Kubernetes package matched |
| `find migrations -type f ! -name .gitkeep -print` | 0 | no output |
| source/directory inspection | 0 | no lease/fencing, sandbox/worktree, Tool Contract/Policy, Eino/Replay adapters, real Verifier/Repair, fault, OTel, UI, or Kubernetes implementation |

The Go module still pins future dependencies selected in phase 0, but the phase 1 executable dependency graph does not import them.

## Acceptance layers

- Static: `go vet`, dependency-boundary scan, migration-boundary scan, and whitespace checks passed.
- Unit: domain, Reasoner, Store, Controller, CLI, and entrypoint tests passed.
- Integration: no external service integration is applicable in phase 1. The real CLI entrypoint session is the in-process component integration and completed all six commands.
- Negative: malformed logs, invalid transitions, terminal advancement, paused side effects, paused cancellation, illegal Tool Call, idempotency content conflict, and concurrent conflicts are rejected or converge to the required controlled result.
- Manual: `make demo-fake` and the real CLI session both exited 0 with the recorded paths.
- Regression: all phase 0 and phase 1 packages passed uncached full tests, full Race tests, and vet.
- Evidence archive: this status report records commands, exits, key assertions, red-green history, limitations, boundaries, and Gate conclusions.

## Independent review

The initial independent review read the complete phase 1 contract, all required phase 0 documents, all implementation/tests, and the actual untracked workspace. It reported:

- Critical: none;
- Important: Paused Cancel masking (reported early and fixed), real CLI lifecycle, silent idempotency conflict, and missing phase 1 status evidence;
- Minor: none in its initial verdict.

The Paused Cancel, CLI lifecycle, and idempotency issues have regression tests and passing fixes. This report supplies the missing status evidence.

At that review checkpoint, the independent follow-up review reported:

- Critical: none;
- Important: none; all prior Important issues closed;
- Minor: one duplicated demo sentence, fixed immediately; and a request to run a staged whitespace check so untracked additions are included; the staged check exited 0;
- Assessment: `Ready to merge? Yes`.

A later Gatekeeper review evaluated the committed phase 1 result and withheld Gate 1:

- Critical: none;
- Important 1: Pause rejected matching completion/failure events for already planned Reasoning and Verification actions;
- Important 2: a `ToolCallFailed` prefix decided `RunReasoner` instead of stable `FailRun`, so interruption between failure appends did not converge.

Both issues now have red-green pure-state and logical-concurrency regression tests and passing fixes. A fresh independent read-only review of this remediation then reported:

- Critical: none;
- Important: none;
- Minor: none;
- targeted tests, three logical-concurrency tests repeated 50 times, full tests, full Race, vet, module verification, demo, whitespace checks, and phase-boundary scans passed;
- Assessment: `Ready to amend? Yes`.

No Critical, Important, or unresolved Minor issue remains. The final full verification pass recorded above also completed without a Gate failure.

## Incomplete work

Nothing required by phase 1 remains intentionally omitted.

All phase 2 through 8 implementation remains absent: PostgreSQL and migrations, durable cross-process recovery, version CAS, leases/fencing, workers, Docker/worktrees, Tool Contracts/Policy, EinoReasoner/ReplayReasoner, real verifier evidence and repair, faults, OTel, replay, artifacts, example service, and the five-minute product demo.

## Known limitations

- The Event Store is memory-only. Standalone CLI invocations start empty; users must use `agentdock session` for a multi-command phase 1 run. Cross-process durability is phase 2.
- The session parser accepts whitespace-delimited arguments and comments; it does not support shell quoting or prompts containing spaces.
- Session execution is single-process and sequential. It provides no inter-process lock, CAS, lease, fencing token, or crash recovery.
- `WorkspaceProvisioned`, `PatchProduced`, and `VerificationPassed` are deterministic phase 1 facts, not proof that Docker, a tool, a patch, or a verifier ran.
- The minimal `phase1.patch` marker validates only the allowed name and JSON syntax. It is not a Tool Contract, schema/capability system, Policy, or host execution path.
- Later-phase event names are declared to preserve the frozen vocabulary, but their reducer semantics return `ErrUnsupportedEvent` until their assigned phase.
- The phase 1 action ID material covers the single initial attempt/path. Repair-round identity and durable receipts belong to later phases.
- Timestamps are optional event data in phase 1; the reducer never reads a clock.
- The runtime makes no exactly-once, production-scale, strong isolation, or multi-tenant claim.

## Gate 1

| Gate item | Conclusion | Evidence |
|---|---|---|
| Reducer is deterministic | PASS | repeated full-state JSON is identical; source import guard passes; no clock/random/network/global input |
| State transitions are program-controlled | PASS | transition table plus invalid-order, paused-work, terminal, and `Succeeded -> Acting` tests |
| FakeReasoner completes one Run | PASS | demo reaches `RunSucceeded` and `Succeeded` with the required intermediate events |
| Pause/Resume/Cancel invariants hold | PASS | ten paused no-op cycles, exact resume state/pending action preservation, active and paused cancellation |
| Planned results survive Pause | PASS | pure reducer plus channel-controlled Reasoning completion/failure and Verification completion windows |
| Action IDs are stable | PASS | repeated Decide and durable pending-action tests |
| Event ordering/idempotency behavior is explicit | PASS | gap/order/duplicate tests plus exact replay, content conflict, and concurrent conflict tests |
| Invalid Fake Tool Call fails under control | PASS | durable `ToolCallFailed` prefix, stable `FailRun`, next-Reconcile `RunFailed`, terminal Noop afterward |
| Required CLI surface is usable in one memory process | PASS | real entrypoint session runs create/get/step/pause/resume/cancel |
| Race tests pass | PASS | targeted and full uncached Race commands exit 0 |
| No Eino or Docker runtime dependency | PASS | executable dependency graph forbidden-package scan has no matches |
| No phase 2+ implementation | PASS | boundary scans and source review |
| Gatekeeper Critical/Important issues closed | PASS | fresh independent read-only review reported Critical none, Important none, Minor none, `Ready to amend? Yes` |

**Gate 1 conclusion: PASS.**

Phase 2 was not started.
