# AgentDock Verify

AgentDock Verify is a managed runtime for Eino-based coding agents that is designed to make execution recoverable, sandboxed, deterministic to verify, and replayable.

The five-week MVP is an engineering demonstration, not a claim of production-scale multi-tenancy or a strong security boundary. The repository is currently at **phase 4: Docker sandbox, disposable Git worktrees, Tool Contract, and static Policy**. It retains phase 3's pure reducer/decider, PostgreSQL Event Log, Worker lease/fencing, durable Action Receipts, and crash recovery, and adds an attempt-scoped no-checkout worktree, a non-root/read-only/no-network Docker execution layer, bounded resources/time/combined output, an empty-by-default caller environment allowlist, scope-bound container ownership, five declared repository tools, default-deny YAML policy, and JSONL audit artifacts. Eino/ReplayReasoner, real model use, verifier/repair, product replay, telemetry, Kubernetes, UI, and MCP remain unimplemented.

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

Phase 1 defined the minimal framework-neutral Reasoner seam and `FakeReasoner`; Controller depends only on that seam. Phase 2 made PostgreSQL events authoritative. Phase 3 adds leased workers and Receipt-guided recovery without changing the reducer/decision ownership boundary. Phase 5 adds `EinoReasoner`, `ReplayReasoner`, and streaming/tool/usage/finish/error normalization behind the seam. Eino types never cross into Controller.

## Phase 4 check

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
go test -tags=integration ./internal/lease/... ./internal/controller/...
go test -tags=chaos ./internal/controller/...
make chaos-worker-kill
make demo-phase3
go test ./internal/policy/... ./internal/tools/...
make sandbox-image
go test -tags=integration ./internal/sandbox/...
make sandbox-security-test
make demo-phase4
make demo-fake
git diff --check
```

`make demo-fake` keeps the fast memory-backed deterministic path. `make test-integration` exercises PostgreSQL transaction rollback, version competition, idempotency, reconnect, checkpoints, Artifacts, migration atomicity, and fresh-process Controller recovery. `make doctor` validates the pinned Go toolchain, Docker daemon, Compose, configured ports, required example parameters, YAML syntax, and the Compose model. A busy configured port is accepted only when the expected service in this Compose project publishes that exact host/container-port mapping.

`make sandbox-security-test` builds the helper image from a digest-pinned Go base and then runs real Docker/worktree negative acceptance. `make demo-phase4` creates a local fixture repository, records its digest and Git status, invokes all five tools through contract/schema/policy/audit, applies a worktree-only patch, rejects `/etc/passwd`, runs a test that attempts network access, destroys the sandbox, and proves the fixture origin is unchanged. It requires no model credential.

The sample values in `.env.example` are local-only defaults, not production credentials. Live model credentials are not required in phase 4 and will never be a default CI dependency.

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

`run step` is the phase-2 compatibility executor for a PostgreSQL Run that has
never acquired a Lease. Once a Lease row exists, the Run is permanently in
managed mode: `run step` is rejected and execution must use `cmd/worker` plus
`ReconcileLeased`. Operator `pause`, `resume`, and `cancel` commands still
persist desired-state intent without a Worker token; the leased Worker observes
and converges that intent. Initial Lease acquisition returns a retryable held
result while an unleased compatibility action has a durable plan but no result,
so one action cannot be split across the legacy and managed event models.

## Consistency and security claims

This phase does not claim exactly-once. PostgreSQL makes Event append, Run version, and fencing validation atomic, while an external action still cannot share that transaction. The implemented claim remains at-least-once plus scoped idempotency and fencing. An unsafe ambiguous outcome enters `WaitingApproval`.

The only phase-4 tool names are `repo.list`, `repo.read`, `repo.search`, `repo.apply_patch`, and `repo.test`. Each call must pass input/output Schema validation, capability/path/network/budget policy, and audit. There is no host-shell tool. Docker reduces accidental and common hostile access, but a container shares the host kernel and is not a strong multi-tenant isolation boundary. The MVP must run untrusted code only on a dedicated development machine or disposable runner appropriate for the risk.

See:

- [`docs/architecture.md`](docs/architecture.md)
- [`docs/state-machine.md`](docs/state-machine.md)
- [`docs/event-model.md`](docs/event-model.md)
- [`docs/threat-model.md`](docs/threat-model.md)
- [`docs/harness-spec.md`](docs/harness-spec.md)
- [`docs/adr/`](docs/adr/)

## License

Apache License 2.0. See [`LICENSE`](LICENSE).
