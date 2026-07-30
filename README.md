# AgentDock Verify

AgentDock Verify is a managed runtime for Eino-based coding agents that is designed to make execution recoverable, sandboxed, deterministic to verify, and replayable.

The five-week MVP is an engineering demonstration, not a claim of production-scale multi-tenancy or a strong security boundary. The repository is currently at **phase 2: durable PostgreSQL Event Store and reconciliation**. It has a pure reducer/decider, program-owned transitions, a framework-neutral `Reasoner` seam with `FakeReasoner`, compatible memory and PostgreSQL Event Stores, transactional version CAS, verified checkpoints, digest-addressed Artifact registration, one Reconcile path, and a durable CLI. Worker registration, lease behavior and fencing, sandboxing, real Eino integration, verifiers, repair, replay, and telemetry remain unimplemented.

The project contract and sole construction plan is [`AgentDock-Verify-施工计划.md`](AgentDock-Verify-施工计划.md). The frozen five-week scope is restated in [`docs/scope.md`](docs/scope.md).

## Why this exists

An agent saying that work is complete is not evidence that it is correct. AgentDock Verify will place a thin workflow around a durable runtime, isolated tools, deterministic verifiers, bounded repair, and replayable evidence:

```mermaid
flowchart LR
    U["User request"] --> C["CLI / Controller"]
    C --> E["PostgreSQL event log"]
    E --> R["Reconcile one action"]
    R --> A["Reasoner adapter"]
    A --> P["Tool contract + policy"]
    P --> S["Disposable worktree in Docker"]
    S --> V["Deterministic verifiers"]
    V --> E
    V --> F["Evidence artifacts"]
```

Phase 1 defined the minimal framework-neutral Reasoner seam and `FakeReasoner`; Controller depends only on that seam. Phase 2 makes PostgreSQL events the authority without changing that Controller path. Phase 5 adds `EinoReasoner`, `ReplayReasoner`, and streaming/tool/usage/finish/error normalization behind it, while revalidating Fake compatibility. Eino types never cross into Controller. AgentDock owns lifecycle, persistence, and the later lease/fencing, sandbox policy, verification, fault injection, telemetry, and replay layers.

## Phase 2 check

Prerequisites:

- a Go installation capable of automatic toolchain selection;
- Docker with a running daemon;
- Docker Compose;
- local ports `55433`, `14317`, `14318`, and `16687` available (or already owned by this Compose project).

Run:

```bash
make doctor
docker compose up -d postgres
make migrate
go mod verify
make test
make test-race
make lint
make test-integration
make test-rebuild-state
make demo-fake
git diff --check
```

`make demo-fake` keeps the fast memory-backed deterministic path. `make test-integration` exercises PostgreSQL transaction rollback, version competition, idempotency, reconnect, checkpoints, Artifacts, migration atomicity, and fresh-process Controller recovery. `make doctor` validates the pinned Go toolchain, Docker daemon, Compose, configured ports, required example parameters, YAML syntax, and the Compose model. A busy configured port is accepted only when the expected service in this Compose project publishes that exact host/container-port mapping.

The sample values in `.env.example` are local-only defaults, not production credentials. Live model credentials are not required in phase 2 and will never be a default CI dependency.

## Durable CLI

Set `AGENTDOCK_DATABASE_URL` to select PostgreSQL. Every standalone invocation then creates a fresh process and reconstructs the Run from the same authoritative Event Log:

```bash
export AGENTDOCK_DATABASE_URL='postgres://agentdock:agentdock_dev_only@127.0.0.1:55433/agentdock?sslmode=disable'
go run ./cmd/agentdock run create demo-cli --scenario phase-2 --spec-hash phase-2-spec
go run ./cmd/agentdock run step demo-cli
go run ./cmd/agentdock run get demo-cli
go run ./cmd/agentdock run events demo-cli
```

Standalone `run` commands reject a missing `AGENTDOCK_DATABASE_URL`; they never silently create an authoritative memory-only Run. The compatible memory Store remains available through dependency injection, the explicit `session` compatibility command, and `demo-fake`. Checkpoints are disposable verified snapshots: Rebuild compares their projection with the authoritative event prefix, and a missing or inconsistent checkpoint falls back to complete Event Log reduction.

## Consistency and security claims

This phase does not claim exactly-once. PostgreSQL makes event append and Run version update atomic, while external actions still cannot share that transaction. Lease and fencing behavior begins only in phase 3.

Docker reduces accidental and common hostile access, but a container shares the host kernel and is not a strong multi-tenant isolation boundary. The MVP must run untrusted code only on a dedicated development machine or disposable runner appropriate for the risk.

See:

- [`docs/architecture.md`](docs/architecture.md)
- [`docs/state-machine.md`](docs/state-machine.md)
- [`docs/event-model.md`](docs/event-model.md)
- [`docs/threat-model.md`](docs/threat-model.md)
- [`docs/harness-spec.md`](docs/harness-spec.md)
- [`docs/adr/`](docs/adr/)

## License

Apache License 2.0. See [`LICENSE`](LICENSE).
