# Event model

## Authority

The PostgreSQL event log is the authoritative record for a Run in phase 2. Derived Run rows and checkpoints are disposable caches that do not replace ordered events. Phase 2 verifies a checkpoint by re-reducing its event prefix and therefore makes no checkpoint read-acceleration claim. A process restart can discard all in-memory state and rebuild from durable data; an inconsistent checkpoint falls back to complete Event Log reduction.

## Event envelope

Every event will carry:

- `run_id`;
- monotonic `seq`;
- `event_type`;
- versioned payload;
- `idempotency_key`;
- `causation_id`;
- `correlation_id`;
- `worker_id` when produced by a worker;
- `fencing_token` for lease-sensitive writes;
- database-assigned `created_at`.

The append transaction enforces uniqueness on `(run_id, seq)` and `(run_id, idempotency_key)`, compares the expected Run version, validates the candidate state, inserts the event, and advances the Run projection in one transaction. PostgreSQL assigns the timestamp. Payloads containing credential-shaped keys, environment containers, credential URLs, or common secret markers are rejected before persistence.

## Minimum event vocabulary

```text
RunCreated
RunDesiredStateChanged
LeaseAcquired
LeaseRenewed
LeaseExpired
AttemptStarted
WorkspaceProvisionPlanned
WorkspaceProvisioned
ReasoningPlanned
ReasoningCompleted
ToolCallPlanned
ToolCallCompleted
ToolCallFailed
PatchProduced
VerificationPlanned
VerificationPassed
VerificationFailed
RepairScheduled
ApprovalRequested
ApprovalResolved
CheckpointSaved
RunSucceeded
RunFailed
RunCancelled
```

Phase 2 persists the narrow, versioned `EventData` payload used by deterministic success, Pause/Resume/Cancel, and controlled reasoner-failure paths. Later-phase payload semantics remain deferred.

## Intent, execution, and result

An external action uses a stable `action_id`:

```text
ActionPlanned(action_id, input_digest, fencing_token)
→ external execution
→ ActionCompleted(action_id, receipt, output_digest)
   or ActionFailed(action_id, error_class, evidence_ref)
```

Planning and result appends are durable facts. The external action is not part of the database transaction, which is why execution is at-least-once.

In phase 1, Pause rejects a new planned fact but does not reject the matching result of an already planned Reasoning or Verification action. The reducer validates the pending `action_id`, records the result while observed state remains `Paused`, and updates the saved resume path or evidence. `ToolCallFailed` is itself a durable result prefix; `RunFailed` is emitted by the next active `FailRun` decision, so an interruption between those two appends converges without calling the Reasoner again.

## Crash interpretation

- **Before planning:** nothing durable authorizes execution; Reconcile may plan normally.
- **After planning, before execution:** a planned action without a receipt is eligible for a safe idempotent retry.
- **During execution:** outcome is ambiguous; inspect a stable receipt or workspace digest. If safety cannot be established, request approval.
- **After execution, before completion append:** search by `action_id` and validate the external receipt before retrying.
- **After completion append:** completion is authoritative and the action must not run again.

## Lease takeover (phase 3)

The phase 2 schema includes `leases`, but no lease operation or fencing check is implemented. In phase 3, lease expiry will permit a new worker to acquire a larger fencing token without assuming the old process stopped; delayed stale writes will then be rejected.

## Event Replay versus Execution Replay

Event Replay is a pure read/reduce operation over historical events. Its output is derived state and an audit timeline.

Execution Replay is a new controlled execution using immutable model/tool cassettes. It exercises adapters, tools, and verifiers without live model or tool calls. It compares normalized events, artifact digests, and verifier evidence with the recording.

A divergence is evidence to investigate, not a reason to mutate the source event log.

## Artifact relationship

Large output, patches, verifier logs, cassettes, and trace evidence live outside event payloads. Phase 2 writes Artifact bytes to a temporary file, hashes and syncs them, closes and atomically renames the complete file, and only then registers metadata with database time. The digest records the bytes present at registration. Phase 2 does not make the file read-only, reject later Artifact metadata updates/deletes, or otherwise provide a strong post-registration immutability guarantee; consumers that need later integrity must re-hash the bytes. Events can reference an Artifact by ID and digest. Verification evidence later binds to:

```text
run_id
attempt_id
workspace_digest
spec_hash
verifier_version
artifact digest
```

Changing code, specification, or verifier version invalidates older results.
