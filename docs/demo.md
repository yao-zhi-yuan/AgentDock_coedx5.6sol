# Demo

Phase 1 provides a deterministic framework-free demonstration:

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

The phase 1 command is evidence only for the pure state machine and in-memory FakeReasoner runtime. It does not demonstrate PostgreSQL recovery, leases/fencing, Docker isolation, Eino, real verifiers, repair, fault injection, OTel, or replay.
