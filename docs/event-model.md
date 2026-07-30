# Event model

## Authority

The PostgreSQL event log will be the authoritative record for a Run. Derived Run rows and checkpoints accelerate reads but do not replace ordered events. A process restart must be able to discard all in-memory state and rebuild from durable data.

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

The append transaction will enforce uniqueness on `(run_id, seq)` and `(run_id, idempotency_key)` and compare the expected Run version. Payloads must not contain model keys, database passwords, complete environments, or unredacted sensitive prompts.

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

Payload schemas and migrations are deferred to their planned implementation phases.

## Intent, execution, and result

An external action uses a stable `action_id`:

```text
ActionPlanned(action_id, input_digest, fencing_token)
→ external execution
→ ActionCompleted(action_id, receipt, output_digest)
   or ActionFailed(action_id, error_class, evidence_ref)
```

Planning and result appends are durable facts. The external action is not part of the database transaction, which is why execution is at-least-once.

## Crash interpretation

- **Before planning:** nothing durable authorizes execution; Reconcile may plan normally.
- **After planning, before execution:** a planned action without a receipt is eligible for a safe idempotent retry.
- **During execution:** outcome is ambiguous; inspect a stable receipt or workspace digest. If safety cannot be established, request approval.
- **After execution, before completion append:** search by `action_id` and validate the external receipt before retrying.
- **After completion append:** completion is authoritative and the action must not run again.

## Lease takeover

Lease expiry permits a new worker to acquire a larger fencing token. It does not stop the old process. PostgreSQL rejects lease-sensitive writes with the stale token, including delayed action results. The new worker reconciles the planned action and any receipt through the normal path.

## Event Replay versus Execution Replay

Event Replay is a pure read/reduce operation over historical events. Its output is derived state and an audit timeline.

Execution Replay is a new controlled execution using immutable model/tool cassettes. It exercises adapters, tools, and verifiers without live model or tool calls. It compares normalized events, artifact digests, and verifier evidence with the recording.

A divergence is evidence to investigate, not a reason to mutate the source event log.

## Artifact relationship

Large output, patches, verifier logs, cassettes, and trace evidence live outside event payloads. Events reference immutable artifacts by ID and digest. An artifact is registered only after bytes are complete, size-limited, and hashed. Verification evidence binds to:

```text
run_id
attempt_id
workspace_digest
spec_hash
verifier_version
artifact digest
```

Changing code, specification, or verifier version invalidates older results.
