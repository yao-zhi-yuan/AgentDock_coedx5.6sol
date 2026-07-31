# Phase 3 status: Worker lease, fencing, and crash recovery

- Implementation date: 2026-07-30
- Pre-remediation host verification date: 2026-07-31
- External Gate 3 remediation date: 2026-07-31
- Branch: `codex/agentdock-verify`
- Gate 2 baseline: `898b876ebbf919acd11b99b886bd931f4b6ecffa`
- Phase-3 implementation commit:
  `d9341f2b39de4f84e11aadb5a5f0229929c529d0`
  (`feat: add lease fencing and crash recovery`)
- Gate-3 remediation increment: this unpublished follow-up commit
- Construction contract: repository-root `AgentDock-Verify-施工计划.md`

## Scope

Only phase 3 is implemented. PostgreSQL now coordinates Worker registration,
heartbeat, one Run Lease, TTL expiry, takeover, monotonically increasing
fencing tokens, Action Receipts, and fenced managed Event appends. First
execution and recovery both call `Controller.ReconcileLeased` and rebuild from
the PostgreSQL Event Log plus Receipts.

This phase does not implement phase 4 Docker Sandbox, Git worktrees, Tool
Contract/Policy, host execution, or a security execution layer. It also does
not add Eino/ReplayReasoner, a real model, Verifier/Repair, OTel or product
Replay, Kubernetes, UI, multi-tenancy, Redis, Kafka, Temporal, or a queue.

## Gate 2 preflight

The branch was `codex/agentdock-verify`, HEAD was exactly
`898b876ebbf919acd11b99b886bd931f4b6ecffa`, `git status --short` was empty,
and the construction plan was read but not changed.

| Command | Exit | Key evidence |
|---|---:|---|
| `docker compose up -d postgres` | 0 | repository PostgreSQL was running |
| first sandboxed `GOCACHE=... make migrate` | 2 | localhost connection was denied; not accepted as Gate evidence |
| host-context identical `GOCACHE=... make migrate` | 0 | `migrations applied` |
| phase-2 Store/Controller integration command | 0 | PostgreSQL Store and durable Controller restart passed |
| phase-2 Migration/separate-process CLI integration command | 0 | migrations and CLI continuation passed |
| phase-2 Store/Controller Race command | 0 | both packages passed under Race |
| `make test-rebuild-state` | 0 | golden, 1000-event, and checkpoint rebuilds passed |
| Gatekeeper six-test phase-1 command | 0 | all six planned-result/Pause/failure tests printed PASS |
| three channel-controlled phase-1 tests with `-count=50` | 0 | all repetitions passed |
| `make test` | 0 | full Go test target passed |
| `make test-race` | 0 | full Race target passed |
| `go test -count=1 ./...` | 0 | all baseline packages passed uncached |
| `go test -race -count=1 ./...` | 0 | all baseline packages passed uncached under Race |
| `make lint` | 0 | vet target passed |
| `go mod verify` | 0 | `all modules verified` |
| first sandboxed `make doctor` | 2 | Docker daemon was unreachable; not accepted as Gate evidence |
| host-context identical `make doctor` | 0 | Go, Docker, Compose, ports, YAML, and Compose model passed |
| `make demo-fake` | 0 | success plus Pause/Resume scenario converged |
| `git diff --check` | 0 | baseline workspace had no whitespace errors |

Only the two localhost/Docker checks were rerun with host permission. No old
result was used as current evidence.

## Test-first red evidence

The following failures were intentionally produced before implementation and
were not counted as acceptance:

1. Domain managed-action tests:
   - Command: `GOCACHE=/private/tmp/agentdock-go-cache go test -count=1 ./internal/domain`
   - Exit: 1.
   - Missing symbols included `EventActionPlanned`,
     `EventActionCompleted`, idempotency scope, Receipt fields, and managed
     projection state.
2. Phase-3 integration tests:
   - Command:
     `go test -tags=integration -count=1 ./internal/lease/... ./internal/controller/...`
   - Exit: 1.
   - Failure: `internal/lease` had no non-test Go implementation.
3. Worker Kill target:
   - Command: `make chaos-worker-kill`
   - Exit: 2.
   - Failure: the target was the deliberate phase-3 placeholder.
4. Early lease green attempt:
   - Exit: 1 because the test used a non-deterministic action ID instead of
     the real `Decide` result.
   - Fix: use the stable domain command ID in the acceptance event.
5. Early Receipt green attempt:
   - Exit: 1 because a receipt ID was only action-local and collided globally.
   - Fix: default Receipt IDs now include Run and action identity.
6. First cross-process Pause/Resume/Cancel attempt:
   - Exit: 1 because a Pause won the CAS race before `ActionPlanned`, while
     managed append retried the stale decision.
   - Fix: a planned-event version conflict reloads state and returns `Noop`;
     it never retries an obsolete plan.
7. First authorized final phase-3 integration run:
   - Command:
     `go test -tags=integration ./internal/lease/... ./internal/controller/...`
   - Exit: 1.
   - Failures: a Receipt transaction that waited across TTL still returned
     success, and the old-token demo exposed a PostgreSQL deadlock between
     Run-first Event append and Lease-first Receipt work.
   - Fix: every fenced append now locks Lease before Run, rechecks TTL after
     the Run lock wait, and Receipt/Validate/Renew read wall-clock time only
     after acquiring the Lease row lock.
8. Second full integration attempt:
   - Exit: 1.
   - Failures: Renew needed an explicit `timestamptz` cast, and Event append
     needed the post-Run-lock TTL recheck described above.
   - Focused lock-wait, renewal, crash-matrix, and two-Worker tests then exited
     0; both the original full command and a final `-count=1` rerun exited 0.
9. First directly captured final `make chaos-worker-kill`:
   - Exit: 2.
   - Failure: all 100 random delays occurred before ApplyPatch Artifact
     publication, so the harness correctly rejected its own
   `artifact_present_at_kill > 0` assertion.
   - Fix: the test Worker gained an ApplyPatch-only post-Artifact/pre-Receipt
     delay, and the seeded random window expanded across early action and
     Artifact uncertainty windows. The identical target then exited 0 with 42
     Artifact-present kills.
10. Final-review Receipt evidence regressions:
    - Red command:
      `go test -tags=integration -count=1 -v ./internal/controller -run 'TestApplyPatchReceiptWithoutArtifactEntersWaitingApproval|TestTamperedInlineReceiptDigestEntersWaitingApproval|TestCrossRunReceiptArtifactEntersWaitingApproval'`.
    - Exit: 1.
    - Failures: a missing ApplyPatch Artifact advanced to `Verifying`, and a
      tampered inline-output digest advanced to `Acting`; the cross-Run case
      was already rejected.
    - Fix: persist independent canonical inline-output and Artifact-byte
      digests; require ApplyPatch's stable Artifact ID; bind Run, planned
      Attempt, phase-3 Artifact type, and digest. The identical three-test
      command then printed all PASS.
11. Real phase-2 upgrade and reversible-migration regressions:
    - The first red build failed because exact-version `migration.To` did not
      exist.
    - The next host run failed because pgx does not allow multiple
      parameterized commands in one prepared statement; the seed was split
      into explicit Run/Event/Attempt/Artifact/Lease writes.
    - The first re-up then failed because migration 000003 down left
      `leases_expiry_idx`; its down migration now removes that index.
    - Final focused integration exited 0 and preserved real v2 rows through
      `2→5→4→3→2→5`, including legacy Lease Worker registration backfill.
12. EventStore direct-completion Artifact regression:
    - Red command:
      `go test -tags=integration -count=1 -v ./internal/controller -run TestRecoveredReceiptRejectsTamperedArtifact`.
    - Exit: 1 because a fully matching direct `PostgresEventStore.Append`
      returned nil after the Artifact file had been changed.
    - Fix: the Event Store now loads Artifact path/size/digest and re-hashes
      bytes inside `ActionCompleted` transaction validation. The identical
      command exited 0 and proves the rejected append changes neither Run
      version nor pending action; normal recovery enters `WaitingApproval`.
13. External blank-task Gate 3 managed-Run bypass:
    - Review task `019fb656-45db-7d41-bb97-fce4db7ef31e` reported
      **Critical 1 / Important 0 / Minor 0** after reproducing the issue with
      real PostgreSQL. While Worker A held token 1 and a fenced
      `ActionPlanned(ApplyPatch)` was pending, durable `agentdock run step`
      called the unfenced compatibility `Controller.Reconcile` and appended an
      identity-less legacy `PatchProduced`. The Run advanced from `Acting` to
      `Verifying`, A's legitimate `ActionCompleted` could no longer apply, and
      takeover could only enter `WaitingApproval`.
    - Domain red command:
      `go test -count=1 -v ./internal/domain -run TestManagedPendingApplyPatchRejectsLegacyPatchProduced`.
      Exit: 1 because the reducer accepted the legacy result.
    - PostgreSQL red command:
      `go test -tags=integration -count=1 -v ./cmd/agentdock -run TestDurableRunStepAndLegacyResultsCannotBypassManagedLease`.
      Exit: 1 because `run step` succeeded and appended a no-Worker/no-token
      `PatchProduced` at sequence 9.
    - Fix: the durable Lease row is now a permanent managed-mode marker.
      Acquisition and non-operator Event append serialize on the same per-Run
      PostgreSQL advisory lock. Managed Run appends require current fencing
      regardless of caller fields, and legacy lifecycle execution facts are
      rejected even with the current token. `Controller.Reconcile` refuses
      managed Runs, while the Event Store remains the authoritative defense
      against races and direct calls. The reducer independently rejects legacy
      results while a generic action is pending.
    - Both identical red commands then exited 0. The first PostgreSQL green
      attempt had one test-only assertion that inspected the latest reconcile
      call's returned slice instead of the authoritative Event Log; the Run
      had already reached `Verifying`. After correcting that assertion to read
      the Event Log, the same test passed and proved `run step`, identity-less
      direct append, and current-token legacy append leave Run version, state,
      and Event count unchanged; the legitimate Worker still completes.
14. Fresh full-range review initial-acquisition split:
    - The first post-Critical complete-range read-only review reported
      **Critical 1 / Important 0 / Minor 1**. It found that the advisory lock
      initially covered individual appends but not the interval between a
      compatibility `ReasoningPlanned` and its result. Initial Acquire could
      insert the permanent managed marker while the old Controller was inside
      the Reasoner, after which its legacy result was rejected and the mixed
      pending projection could not converge.
    - PostgreSQL red command:
      `go test -tags=integration -count=1 -v ./cmd/agentdock -run TestInitialLeaseWaitsForInFlightLegacyReconcile`.
      Exit: 1. Acquire returned nil, then the released old Reconcile failed its
      result append with `stale worker fencing token`.
    - Fix: while holding the same per-Run advisory lock, initial Acquire checks
      the Event Log for an unresolved compatibility `ReasoningPlanned` or
      `VerificationPlanned`. It returns
      `ErrLegacyExecutionInProgress`, which wraps retryable `ErrLeaseHeld`, and
      creates neither a Lease row nor a token. After the matching result
      commits, acquisition succeeds with initial token 1.
    - The identical command then exited 0. Its controlled blocking Reasoner
      proves no Lease exists during the old external action, old Reconcile
      completes normally, and only the subsequent Acquire selects managed
      mode.

Before applying migration 000004 to the long-lived local development database,
117 obsolete synthetic phase-3 red/early-green Receipt rows referenced fake
Artifact IDs. They were deleted with an exact predicate limited to the five
known test scenario IDs; no Event, Run, valid Artifact, or business row was
deleted. Fresh-schema migration tests and the validated foreign-key query pass.

The corresponding focused commands were rerun after each fix and exited 0.

## Completed work

1. Added `workers` and `action_receipts` migrations, Worker foreign-key
   ownership for Leases, Receipt-to-Artifact foreign-key ownership, expiry
   indexes, per-Run/action Receipt uniqueness, and unique non-null Artifact
   accounting. Migration 000005 adds paired Artifact digests; the exact-version
   migration harness proves v2 data preservation and full down/up reversibility.
2. Added Worker registration and database-time heartbeat on an independent
   process ticker, including while Lease polling is slow or blocked.
   Worker IDs are non-reusable process-incarnation identities; duplicate
   registration is explicitly rejected so two processes cannot share a token.
3. Added one Lease row per Run with:
   - initial token 1;
   - database-time TTL;
   - same-token renewal;
   - PostgreSQL advisory plus row locking for acquisition;
   - expired takeover with token `previous + 1`;
   - `clock_timestamp()` validation and expiry writes after lock waits;
   - explicit held, expired, stale, and missing errors.
4. Added PostgreSQL Event fencing:
   - every generic action/lease event must include Worker ID and token;
   - current owner, exact token, and unexpired TTL are checked inside Append;
   - checking occurs before idempotent replay;
   - stale events return `ErrStaleFencingToken`;
   - the first durable Lease row permanently places the Run in managed mode;
   - initial acquisition and Event append serialize mode selection on the same
     per-Run advisory lock;
   - initial acquisition refuses unresolved compatibility plan/result work
     with a retryable held result, so no external action is split across modes;
   - every managed lifecycle append is fenced based on persisted mode, not on
     whether the caller supplied Worker fields;
   - old lifecycle execution facts are rejected for managed Runs even when
     carrying the current token, and `Controller.Reconcile` cannot operate a
     managed Run;
   - operator `DesiredStateChanged` remains the explicit unfenced exception so
     Pause, Resume, and Cancel work across processes.
5. Added stable `ActionPlanned / ActionCompleted / ActionFailed` facts and a
   deterministic reducer projection for pending action type/scope, Receipt,
   completion, known failure, and ambiguous `WaitingApproval`.
6. Added durable Receipts:
   - fenced writes;
   - one Receipt per stable action ID;
   - binding to a matching pending `ActionPlanned`;
   - credential-shaped payload rejection;
   - action-specific output validation before persistence;
   - canonical inline-output digest recomputation;
   - independent ApplyPatch Artifact digest, stable ID, Run, planned Attempt,
     and type binding;
   - exact payload/digest/Artifact comparison and file-byte re-hash before
     `ActionCompleted`, including direct EventStore append;
   - conflicting replay rejection.
7. Added `Controller.ReconcileLeased`. Clean execution, restart, and takeover
   share this path. It looks up a Receipt before retry, never re-executes a
   completed action, retries only `scoped-idempotent` missing-Receipt work, and
   sends unsafe ambiguity to `WaitingApproval`.
8. Added a complete-at-registration phase-3 Receipt Artifact for ApplyPatch.
   The ID, content, and digest are stable. An identical retry returns the
   existing Artifact row; conflicting content is rejected. Recovery dry-reduces
   Receipt output and re-hashes referenced Artifact bytes before completion;
   malformed or changed evidence enters `WaitingApproval`.
9. Added a minimal `cmd/worker` process with database URL, Worker/Run IDs,
   TTL, heartbeat, polling, signal handling, test-only action-window delays,
   and an explicit old-token demonstration probe. It
   automatically registers, waits for/acquires a Lease, renews concurrently,
   and exits at a terminal or `WaitingApproval` state.
10. Added cross-process Pause/Resume/Cancel acceptance. A paused Worker keeps
    heartbeating but adds no Receipt or side-effect action; Resume continues the
    same path; Cancel clears older pending work and converges to `Cancelled`.
11. Added a real OS-process Worker Kill harness. It builds one Worker binary,
    kills Worker A at seeded random time windows spanning early actions and the
    enlarged ApplyPatch post-Artifact/pre-Receipt gap, lets Worker B wait for
    TTL and take over, requires an Artifact-present Kill window, validates
    terminal state and unique Receipt/Artifact accounting, and repeats 100
    times. Deterministic fault tests cover the complete seven-window matrix.
12. Added the required two-Worker demonstration and the complete seven-window
    recovery matrix in `docs/acceptance/recovery-matrix.md`.

## Named recovery and negative acceptance

The tests explicitly prove:

- at most one current valid Lease for a Run;
- initial acquisition and simultaneous expired takeover each have one winner;
- successful takeover increments the prior token;
- renewal changes expiry/heartbeat without changing the token;
- a stale Worker cannot append a new event or replay an event already present;
- a stale Worker cannot write a Receipt;
- durable `run step` cannot operate a managed Run;
- an identity-less legacy lifecycle result cannot append to a managed Run;
- a current-token legacy lifecycle result also cannot mix with generic managed
  actions;
- every rejected compatibility write preserves Run version, state, and Event
  count, while the legitimate Worker can still complete;
- reducer projection rejects legacy lifecycle results while a generic action
  is pending;
- initial Acquire cannot insert a Lease between a legacy planned fact and its
  external execution/result; it issues token 1 only after legacy completion;
- `ActionCompleted` requires an existing matching Receipt and cannot alter its
  output;
- a Receipt requires a matching pending `ActionPlanned`;
- malformed action-specific Receipt output is rejected before persistence;
- missing ApplyPatch Artifact, tampered inline digest, cross-Run Artifact,
  malformed durable Receipt, or changed Artifact bytes enters
  `WaitingApproval` rather than poisoning every recovery;
- a direct EventStore completion append cannot bypass Artifact byte re-hashing;
- first execution and restart/takeover both call `ReconcileLeased`;
- a controlled crash occurs while blocked inside `Executor.Execute`, leaving a
  plan and no Receipt before scoped retry;
- a safe action completed before Receipt persistence is retried at least once;
- a persisted Receipt completes without re-execution;
- a completed Event is not executed again;
- confirmed Receipt cost and Artifact IDs are unique per action;
- the real Artifact row and bytes/digest agree, and an identical write remains
  one row;
- unsafe missing-Receipt ambiguity reaches `WaitingApproval`;
- Pause creates a stable no-action window across processes;
- Resume converges to `Succeeded`;
- Cancel converges to `Cancelled`;
- Cancel racing StartAttempt completion creates no attempt number zero and
  still converges to `Cancelled`;
- process heartbeat advances while Lease polling is one second;
- Event append, Receipt write, and takeover lock waits cannot reuse
  transaction-start time across TTL expiry;
- Worker A kill, TTL expiry, Worker B takeover, and restarted-A old-token append
  probe rejection converge to `Succeeded`;
- 100 seeded random real process kills converge to the allowed terminal state.

## Execution semantics

The implemented claim is:

```text
at-least-once execution
+ scoped idempotency
+ fencing
```

This produces an effectively-once outcome only for the synthetic actions whose
stable Receipt/Artifact behavior is tested. It is not exactly-once. `CostUnits`
is unique MVP accounting, not a guarantee about external provider billing.

## Known limitations

- PostgreSQL is both Event Log and local coordination authority. This is a
  single-node MVP proof, not a production-scale scheduler.
- A fencing token prevents stale database writes; it cannot stop CPU execution
  or undo a non-database effect already performed by an old process.
- The phase-3 executor is synthetic and uses FakeReasoner. Its only concrete
  external data side effect is a deterministic Receipt Artifact.
- Artifact registration can still leave an unregistered complete orphan if the
  database fails after file publication, as documented in phase 2. It never
  registers partial bytes.
- No phase-4 sandbox, worktree, Tool Contract, path/network policy, or strong
  isolation claim exists.
- `WaitingApproval` is a safe terminal-for-worker hold state in phase 3. An
  approval resolution workflow belongs to a later phase.

## Independent review

The required `requesting-code-review` fresh-context, read-only review was
reopened after the first host pass. The first final review reported
**Critical 1 / Important 1 / Minor 0**: Receipt evidence could omit an
ApplyPatch Artifact or carry a tampered inline digest, and migration tests did
not exercise real v2 data or down/up. After those fixes, the next review closed
both original findings but reported **Critical 0 / Important 1 / Minor 1**
because direct EventStore completion still checked only Artifact metadata and
the status evidence still referenced schema v4. The direct-write byte-check
Important received the red-green test recorded above; this document now
records v5. The final read-only follow-up reported
**Critical 0 / Important 0 / Minor 0; Ready: Yes**, explicitly closed the
direct-write finding, and found no phase-4 boundary crossing.

Earlier implementation reviews also fixed:

- preserving `WaitingApproval` as an operator-owned hold;
- making desired-state CAS races retry safely;
- making Cancel win a StartAttempt completion race without attempt zero;
- running process heartbeat independently of Lease polling;
- validating Receipt output and Artifact bytes before recovery completion;
- using wall-clock database time after lock waits across TTL;
- exercising a crash from inside `Executor.Execute`;
- starting both demo Workers before the Kill and using a real old-token probe
  process;
- allowing StartAttempt failure to converge through `FailRun`.

The reviews also confirmed the Worker concurrency Race tests, per-test Chaos
Artifact directory isolation, and no phase-4 boundary crossing. These reviews
are not the external blank-task Gatekeeper release decision.

The subsequent external blank-task Gatekeeper review task
`019fb656-45db-7d41-bb97-fce4db7ef31e` rejected the earlier Gate conclusion
with **Critical 1 / Important 0 / Minor 0** for the managed-Run bypass recorded
in red evidence item 13. Therefore the earlier internal review result and the
acceptance tables below are pre-remediation baseline evidence only. They are
not accepted as proof of the remediation. Fresh read-only reviews of both the
fix increment and the complete Gate-2-base-to-working-tree phase-3 range, plus
the complete host Gate, are required before Gate 3 can return to pass.

The first complete-range remediation review then found the additional
initial-acquisition split recorded in red evidence item 14 and reported
**Critical 1 / Important 0 / Minor 1; Ready: No**. Its Critical was fixed with
the deterministic PostgreSQL red→green above. Its Minor (the stale
`Planned commit` label) was also corrected. Because that review observed the
pre-fix snapshot, new incremental and complete-range read-only reviews are
required before final verification.

The replacement fresh-context reviews observed the final fix:

- remediation increment: **Critical 0 / Important 0 / Minor 1; Ready: Yes**;
  its only Minor was the stale `Final host verification date` label, which is
  now explicitly marked pre-remediation;
- complete Gate-2 baseline `898b876e` through the current working tree:
  **Critical 0 / Important 0 / Minor 0; Ready: Yes**.

Both reviews were read-only and explicitly verified the existing-Lease bypass,
initial Acquire interleaving, advisory/Lease/Run lock order, Worker polling,
all production Event/Receipt write entrances, operator desired-state
exception, Receipt/Artifact evidence, and phase-4 boundary.

## Pre-remediation automatic acceptance

All commands below were executed before the external Critical was reported.
They document the previous baseline but are not reused as remediation Gate
evidence. The
Go cache was `GOCACHE=/private/tmp/agentdock-go-cache-phase3-final`; PostgreSQL
commands used
`AGENTDOCK_DATABASE_URL=postgres://agentdock:agentdock_dev_only@127.0.0.1:55433/agentdock?sslmode=disable`.

### Required phase-3 Gate

| Command | Exit | Key evidence |
|---|---:|---|
| `docker compose up -d postgres` | 0 | `agent-postgres-1` healthy on `127.0.0.1:55433` |
| `make migrate` | 0 | `migrations applied` |
| `go test -tags=integration ./internal/lease/... ./internal/controller/...` | 0 | Lease and Controller full integration passed |
| final uncached `go test -tags=integration -count=1 ./internal/lease/... ./internal/controller/...` | 0 | Lease 2.108s; Controller 10.317s |
| `go test -tags=chaos ./internal/controller/...` | 0 | final post-review 100-Kill Chaos package passed in 48.729s |
| `go test -race ./internal/...` | 0 | exact required command passed; the immediately preceding uncached full-repository Race run also passed |
| `make chaos-worker-kill` | 0 | final post-review run: `iterations=100 killed=100 succeeded=100 waiting_approval=0 artifact_present_at_kill=39` |
| `make demo-phase3` | 0 | A token 1 killed; already-running B took token 2; restarted-A probe rejected; final `Succeeded` |

The required assertions are exercised by named tests: one valid Lease per Run,
monotonic takeover token, stale append/Receipt rejection before idempotent
replay, 100/100 allowed convergence, unique Receipt cost/Artifact accounting,
Artifact re-hash, malformed/unsafe ambiguity to `WaitingApproval`, and
Pause/Resume/Cancel across processes.

### Phase-2 and phase-1 regression

| Command | Exit | Key evidence |
|---|---:|---|
| `go test -tags=integration -count=1 ./internal/store/... ./internal/controller/...` | 0 | PostgreSQL Store and durable Controller restart passed |
| `go test -tags=integration -count=1 ./internal/migration/... ./cmd/agentdock` | 0 | empty/failing migrations and separate-process CLI passed |
| `go test -race -count=1 ./internal/store/... ./internal/controller/...` | 0 | Store/Controller common paths passed under Race |
| `make test-rebuild-state` | 0 | golden, 1000-event, and PostgreSQL checkpoint rebuild passed |
| `make test-integration` | 0 | unified Store/Lease/Controller/Migration/CLI integration target passed |
| phase-1 six-test Gatekeeper command with `-count=1 -v` | 0 | all six named Pause/planned-result/failure regressions printed PASS |
| three channel-controlled Controller tests with `-count=50` | 0 | all 50 repetitions passed |

### Full repository, environment, and demonstrations

| Command | Exit | Key evidence |
|---|---:|---|
| `go test -count=1 ./...` | 0 | all packages passed uncached |
| `go test -race -count=1 ./...` | 0 | all packages, including Worker concurrency tests, passed uncached under Race |
| `make test` | 0 | unified Go test target passed |
| `make test-race` | 0 | unified Race target passed |
| `make lint` | 0 | Make vet target completed with no diagnostics |
| `go vet ./...` | 0 | no diagnostics |
| `go mod verify` | 0 | `all modules verified` |
| `docker compose up -d` | 0 | PostgreSQL, OTel Collector, and Jaeger started for Doctor |
| `make doctor` with host access | 0 | Go, Docker, Compose, ports, YAML, and Compose model passed |
| `docker compose stop otel-collector jaeger` | 0 | stopped only services added for Doctor; PostgreSQL remained healthy |
| `make demo-fake` | 0 | normal and Pause/Resume runs converged to `Succeeded` |
| schema query | 0 | migration `5|false`; validated Receipt Artifact FK and Artifact-digest pair constraints |

The first combined schema query printed `5|false` but exited 1 because
PostgreSQL's internal `"char"` constraint type required an explicit text cast;
it was not counted as passing evidence. The corrected read-only query exited 0
and printed:

```text
5|false
action_receipts_artifact_digest_pair|c|true
action_receipts_artifact_fk|f|true
```

Final whitespace, scope, review, parent, commit, and clean-worktree checks are
performed after this document is finalized.

## Post-remediation final automatic acceptance

All results in this section were freshly produced from the final remediation
code on 2026-07-31. They do not reuse the pre-remediation table.

### Required phase-3 Gate and managed-mode regressions

| Command | Exit | Key evidence |
|---|---:|---|
| `docker compose up -d postgres` | 0 | repository PostgreSQL running |
| `make migrate` | 0 | `migrations applied` |
| `go test -tags=integration ./internal/lease/... ./internal/controller/...` | 0 | Lease 2.603s; Controller 10.861s |
| `go test -tags=chaos ./internal/controller/...` | 0 | exact required Chaos package command passed |
| `go test -race ./internal/...` | 0 | all internal packages passed under Race |
| focused two-Critical PostgreSQL command | 0 | existing-Lease bypass and initial-Acquire interleaving both printed PASS |
| `go test -tags=integration -count=1 ./internal/lease/... ./cmd/agentdock` | 0 | Lease and durable CLI package passed |
| `make chaos-worker-kill` | 0 | `iterations=100 killed=100 succeeded=100 waiting_approval=0 artifact_present_at_kill=46`; test 44.43s |
| `make demo-phase3` | 0 | A token 1 killed; B token 2 takeover; restarted A stale append rejected; final `Succeeded` |

The first `make chaos-worker-kill` tool transport returned only the test start
line and no final status/statistics, so it was not accepted. The identical
command was rerun to an explicit process exit 0 and the complete statistics
above.

The focused PostgreSQL regressions assert:

- durable `run step`, identity-less direct legacy append, and current-token
  legacy append all fail without changing Run version, state, or Event count;
- the legitimate Worker/token still completes the pending generic action;
- Pause/Resume/Cancel desired-state writes remain cross-process operator
  controls and Cancel converges only through the leased Worker;
- initial Acquire during a blocked compatibility Reasoner returns retryable
  `ErrLegacyExecutionInProgress`, creates no Lease/token, lets the old result
  commit, then succeeds with token 1.

### Phase-2 and phase-1 regression

| Command | Exit | Key evidence |
|---|---:|---|
| `go test -tags=integration -count=1 ./internal/store/... ./internal/controller/...` | 0 | Store 6.117s; Controller 8.087s |
| `go test -tags=integration -count=1 ./internal/migration/... ./cmd/agentdock` | 0 | Migration 1.626s; CLI 1.291s |
| `go test -race -count=1 ./internal/store/... ./internal/controller/...` | 0 | both packages passed |
| `make test-rebuild-state` | 0 | domain golden/1000 Event and PostgreSQL Checkpoint rebuild passed |
| `make test-integration` | 0 | Store, Lease, Controller, Migration, and CLI passed |
| phase-1 six-test Gatekeeper command with `-count=1 -v` | 0 | all six named regressions printed PASS |
| three phase-1 concurrent Pause tests with `-count=50` | 0 | all 50 repetitions passed |

### Full repository, environment, and demonstrations

| Command | Exit | Key evidence |
|---|---:|---|
| `go test -count=1 ./...` | 0 | all packages passed uncached |
| `go test -race -count=1 ./...` | 0 | all packages passed uncached under Race |
| `make test` | 0 | full Go target passed |
| `make test-race` | 0 | full Race target passed |
| `make lint` | 0 | no diagnostics |
| `go vet ./...` | 0 | no diagnostics |
| `go mod verify` | 0 | `all modules verified` |
| `docker compose up -d` | 0 | PostgreSQL, OTel Collector, and Jaeger started |
| `make doctor` with host access | 0 | Go, Docker, Compose, four port mappings, YAML, and Compose model passed |
| `docker compose stop otel-collector jaeger` | 0 | Doctor-only services stopped; PostgreSQL retained |
| final `make migrate` | 0 | `migrations applied` |
| `make demo-fake` | 0 | normal and Pause/Resume scenarios reached `Succeeded` |
| read-only schema queries | 0 | `5|false`; Receipt Artifact digest-pair and FK constraints both validated |
| `git diff --check` before status finalization | 0 | no whitespace errors |
| plan/scope/VCS pre-commit check | 0 | branch correct; HEAD `d9341f2`, parent `898b876e`; construction plan unchanged |

## Gate 3

**INTERNAL GATE PASS; READY FOR EXTERNAL GATE 3 RE-REVIEW.** Both reported
Critical paths have stable PostgreSQL red→green coverage, the replacement
read-only full-range review reports Critical/Important/Minor 0, and every fresh
post-remediation Gate above exits 0. Phase 4 remains unstarted. This internal
result does not override the external blank-task Gatekeeper as final release
authority.
