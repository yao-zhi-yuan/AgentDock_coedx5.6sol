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

Continue `run step` until `Succeeded`; `run events` shows a contiguous sequence. This demonstrates database reconstruction and the same Reconcile path, not lease takeover, Worker Kill recovery, fencing, Docker isolation, Eino, real verifiers, repair, fault injection, OTel, or product Replay.
