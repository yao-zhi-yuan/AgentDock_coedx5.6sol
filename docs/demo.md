# Demo

The deterministic memory-backed demonstration remains available:

```bash
make demo-fake
```

It prints the event log for a successful FakeReasoner Run, including:

```text
RunCreated
AttemptStarted
ReasoningCompleted
VerificationPassed
RunSucceeded
```

It then pauses a second Run in `Provisioning`, executes ten no-op Reconcile cycles, resumes to `Provisioning`, and converges to `Succeeded`.

The five-minute product demo remains a phase 8 deliverable. Its frozen eventual sequence is:

1. problem and architecture;
2. create a Run;
3. first patch passes tests but fails an architecture verifier;
4. structured evidence drives bounded repair;
5. kill a worker and show lease takeover plus stale-token rejection;
6. inject a bounded tool error;
7. inspect the Jaeger trace;
8. replay without a live model;
9. state trade-offs and limitations.

Phase 2 adds a real cross-process PostgreSQL demonstration:

```bash
docker compose up -d postgres
make migrate
export AGENTDOCK_DATABASE_URL='postgres://agentdock:agentdock_dev_only@127.0.0.1:55433/agentdock?sslmode=disable'
go run ./cmd/agentdock run create restart-demo --scenario phase-2 --spec-hash phase-2-spec
go run ./cmd/agentdock run step restart-demo
go run ./cmd/agentdock run step restart-demo
# Every following command is a fresh process with no retained Controller object.
go run ./cmd/agentdock run step restart-demo
go run ./cmd/agentdock run events restart-demo
```

Continue `run step` until `Succeeded`; `run events` shows a contiguous
sequence. This is deliberately a phase-2 compatibility Run that has never
acquired a Lease. Once any Worker acquires a Run Lease, `run step` is rejected
and all execution must use `cmd/worker`/`ReconcileLeased`; only operator
pause/resume/cancel intent remains unfenced. The phase-2 sequence demonstrates
database reconstruction, not lease takeover, Worker Kill recovery, fencing,
Docker isolation, Eino, real verifiers, repair, fault injection, OTel, or
product Replay.

Phase 3 adds the required two-Worker recovery demonstration:

```bash
docker compose up -d postgres
make migrate
make demo-phase3
```

The demonstration starts Workers A and B together. A acquires the Lease while
B stays registered, heartbeats independently, and waits. It kills A, waits for
TTL expiry, observes B take over with a larger token, then starts the Worker
binary again as an explicit A/old-token probe. That process presents A's old
token to a lease-sensitive append, prints `stale_probe_rejected`, and exits
successfully only when PostgreSQL rejects it. Worker B then shows the Run
converging to `Succeeded`.

The full randomized process harness is:

```bash
make chaos-worker-kill
```

It performs 100 real Worker process kills over seeded random windows spanning
planning, in-executor work, the post-action/pre-Receipt gap, and later actions.
The harness requires at least one Artifact-present Kill window and verifies
unique Receipt/Artifact accounting. It is a local MVP reliability test, not a
production scheduler or strong isolation claim.

## Phase 4 Sandbox and Policy demonstration

The phase-4 demonstration is deterministic and requires no model credential:

```bash
make demo-phase4
```

It creates a temporary Git fixture repository and records its tracked-content
digest plus `git status --porcelain=v1`. One Run/Attempt worktree is created,
and all five caller-visible tools go through Contract validation, static YAML
Policy, Docker Sandbox, and the JSONL audit recorder:

```text
repo.list
repo.read
repo.search
repo.apply_patch
repo.test
```

The patch changes only the detached worktree. An absolute `/etc/passwd` read is
rejected before Docker I/O. `repo.test` runs a Go test inside the no-network
container; that test makes a real TCP attempt and passes only when the request
is denied. Destroy removes the container set and worktree. The command then
requires the origin digest and Git status to match their before values and
prints the retained audit-artifact path.

The complete negative suite is:

```bash
make sandbox-security-test
```

It verifies traversal/absolute/symlink/TOCTOU rejection, case-insensitive
runtime-path reservation, no host hook/filter execution, no network, timeout
and cancellation kill, PID exhaustion, combined-output truncation audit,
non-root UID, read-only/non-workspace filesystem, writable workspace, cgroup
limits, fixed/credential-free environment, cryptographic container ownership,
cross-Provider name-collision safety, retryable Destroy, and removal of owned
containers/worktrees. It also exercises a real top-level atomic patch, a
read-only `.git` pointer mount, case-aliased root rejection on case-insensitive
filesystems, and a controlled missing-promisor-object fixture that proves no
lazy-fetch helper executes. Cleanup failures are explicit on normal, non-zero,
timeout, and cancellation paths; once Destroy starts, execution cannot resume.
Docker remains a local MVP risk-reduction boundary, not strong multi-tenant
isolation.
