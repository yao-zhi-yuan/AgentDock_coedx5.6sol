# Phase 3 recovery matrix

## Execution contract

Phase 3 provides:

```text
at-least-once execution
+ scoped idempotency
+ monotonically increasing fencing tokens
= effectively-once outcome only for the scoped actions proved below
```

It does not provide or claim exactly-once. An external action and its
PostgreSQL completion event do not share one transaction. Every managed action
uses a stable `action_id`, first appends `ActionPlanned`, records a fenced
Action Receipt after execution, and finally appends `ActionCompleted` or
`ActionFailed`.

The phase-3 executor is intentionally narrow. Its only external data side
effect is a deterministic, complete-at-registration receipt Artifact for the
synthetic ApplyPatch action. There is no user-repository write, model billing,
business API call, sandbox, worktree, Tool Contract, or policy engine.

Receipt evidence is action-specific. Every inline output has a canonical
SHA-256 independent of Artifact bytes. ApplyPatch additionally requires the
stable `action-<action_id>` Artifact ID and binds its Run, planned Attempt,
phase-3 Artifact type, and SHA-256. Recovery and the PostgreSQL
`ActionCompleted` append path both re-hash the file; missing, changed, or
cross-Run evidence cannot advance the Run.

## Required crash windows

| # | Crash window | Durable observation after restart | Recovery behavior through `ReconcileLeased` | Retry? | Idempotency and Receipt rule | Expected state |
|---:|---|---|---|---|---|---|
| 1 | Before `ActionPlanned` | No intent and no Receipt | Reload Event Log, reduce, and make the normal deterministic decision | This is a new execution, not a retry | Stable command material produces the same `action_id` | The fixed safe scenario converges to `Succeeded` |
| 2 | After `ActionPlanned`, before execution | Pending stable `action_id`; no Receipt | Inspect Receipt, then execute only when the event declares `scoped-idempotent` | Yes for scoped actions; no for unsafe actions | Same `action_id`; unsafe missing-Receipt recovery appends ambiguous `ActionFailed` | `Succeeded` for scoped actions; `WaitingApproval` for unsafe actions |
| 3 | During execution | Planned intent; Receipt may be absent or present | Look up Receipt first. Complete from it when present; otherwise use the declared scope | At-least-once retry only for scoped actions | Stable action ID and deterministic Artifact ID/digest; no inference from process death | `Succeeded` when safe; otherwise `WaitingApproval` |
| 4 | Action finished, before `ActionCompleted` | Planned intent. A Receipt may already prove completion, or the process may have died immediately before receipt persistence | With Receipt: append `ActionCompleted` without executing again. Without Receipt: retry only a scoped action; unsafe outcome becomes ambiguous | Conditional | Receipt lookup is mandatory. The Artifact store returns the existing row for the same ID/content/digest, and rejects conflicting content | `Succeeded` for the fixed scoped scenario; `WaitingApproval` when safety is unprovable |
| 5 | After `ActionCompleted` | Completion Event and Receipt are durable | Event reduction advances to the next command; the completed `action_id` is not executed again | No | `ActionCompleted` must reference an existing matching Receipt | The fixed scenario converges to `Succeeded` |
| 6 | Lease expires just as old Worker resumes | Lease row names a new Worker with a strictly larger token; Event Log may contain a pending action | New Worker follows the same pending-action/Receipt reconciliation. Old Worker append and Receipt writes are rejected before idempotent replay is considered | New Worker follows rows 2–4; stale Worker never retries legally | PostgreSQL checks current Worker, token, and database-time expiry in the append/receipt transaction | New Worker reaches an allowed state; old Worker cannot change it |
| 7 | Two Workers attempt expired takeover concurrently | One expired Lease row with prior token | PostgreSQL row lock serializes takeover; exactly one claimant increments the token and the other receives `ErrLeaseHeld` | Only the winner may reconcile | One row per Run and monotonic `fencing_token + 1` under lock | One valid Lease; Run then follows the normal reconcile path |

## Automated evidence mapping

| Requirement | Test |
|---|---|
| Windows 1, 2, 4, and 5, including before/after Receipt | `TestCrashWindowsRecoverThroughTheSameLeasedReconcilePath` |
| Window 3 with the Worker cancelled while blocked inside `Executor.Execute` | `TestCrashWhileExecutorIsInFlightRetriesScopedAction` |
| Unsafe missing-Receipt ambiguity | `TestUnsafeAmbiguousRecoveryEntersWaitingApproval` |
| Malformed durable Receipt cannot poison recovery | `TestMalformedDurableReceiptEntersWaitingApproval` |
| Missing ApplyPatch Artifact, tampered inline digest, or cross-Run Artifact | `TestApplyPatchReceiptWithoutArtifactEntersWaitingApproval`, `TestTamperedInlineReceiptDigestEntersWaitingApproval`, and `TestCrossRunReceiptArtifactEntersWaitingApproval` |
| Artifact bytes changed after Receipt persistence, including direct EventStore completion append | `TestRecoveredReceiptRejectsTamperedArtifact` |
| Cancel wins between StartAttempt plan and completion without attempt zero | `TestCancelRacingStartAttemptCompletionDoesNotInsertAttemptZero` |
| Expired takeover, larger token, stale append rejection | `TestOneValidLeaseMonotonicTakeoverAndStaleAppendRejection` |
| Stale replay of an event that was already appended | `TestStaleWorkerCannotReplayPreviouslyAppendedEvent` |
| Two simultaneous expired takeovers | `TestTwoWorkersSimultaneousExpiredTakeoverHasOneWinner` |
| Heartbeat and same-token renewal | `TestHeartbeatRenewsWithoutChangingFencingToken` |
| Heartbeat is independent of a one-second Lease polling interval | `TestWorkerHeartbeatsWhileWaitingForHeldLease` |
| Append, Receipt, and takeover use wall-clock time after lock waits | `TestLeaseSensitiveOperationsUseClockAfterLockWait` |
| Pause/Resume/Cancel observed by another Worker process | `TestPauseResumeCancelAcrossWorkerProcesses` |
| Real Worker A kill, Worker B takeover, old-token rejection | `TestTwoWorkerKillTakeoverRejectsRestartedStaleToken` |
| 100 real Worker process kills | `TestWorkerKillChaos100Converges` |
| Idempotent Receipt cost/Artifact accounting | `TestReceiptIsFencedAndIdempotentByStableActionID` and `TestArtifactRegisteredOnlyAfterCompleteDigestWrite` |

## Honest limits

- PostgreSQL fencing prevents stale database events and Receipt writes. It
  cannot revoke CPU time or undo a side effect already performed by an old
  process.
- A Worker ID identifies one process incarnation and is never reused. A
  restarted logical Worker registers a new ID; presenting the prior
  incarnation's token remains stale.
- The safe retry proof applies only to the synthetic phase-3 actions and
  deterministic receipt Artifact. A future tool must declare and prove its own
  idempotency scope.
- `CostUnits` is unique durable MVP accounting per `action_id`; it is not a
  claim that an external model or provider would bill exactly once.
- An unsafe action without conclusive Receipt evidence stops at
  `WaitingApproval`. Phase 3 does not implement a phase-4 policy or approval UI.
