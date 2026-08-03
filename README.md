# AgentDock Verify

AgentDock Verify is a managed runtime for Eino-based coding agents that is designed to make execution recoverable, sandboxed, deterministic to verify, and replayable.

The five-week MVP is an engineering demonstration, not a claim of production-scale multi-tenancy or a strong security boundary. The repository contains the **phase 5 candidate: Eino Adapter and recorded Coding Agent**. It retains the phase-3 recovery semantics and phase-4 Docker/Tool/Policy boundary, and adds a framework-neutral streaming Reasoner contract, credential-free Fake/Replay modes, an Eino-only adapter package, provider-error normalization, fail-closed token accounting, a fixed Coding Agent System Contract, one example Go repository, and two recorded Bug Scenarios. Automated Gate 5 and independent review evidence are recorded in `docs/status/phase-5.md`; optional Live Model/manual acceptance remains pending. Phase 6 verifier/evidence/repair, phase 7 product replay/OTel/fault injection, Kubernetes, UI, MCP, and multi-tenancy remain unimplemented.

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

## Phase 5 check

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
go test ./internal/reasoner/...
go test -tags=integration ./internal/reasoner/...
make demo-eino-recorded
make demo-fake
git diff --check
```

`make demo-fake` keeps the fast memory-backed deterministic path. `make test-integration` exercises PostgreSQL transaction rollback, version competition, idempotency, reconnect, checkpoints, Artifacts, migration atomicity, and fresh-process Controller recovery. `make doctor` validates the pinned Go toolchain, Docker daemon, Compose, configured ports, required example parameters, YAML syntax, and the Compose model. A busy configured port is accepted only when the expected service in this Compose project publishes that exact host/container-port mapping.

`make sandbox-security-test` builds the helper image from a digest-pinned Go base and then runs real Docker/worktree negative acceptance. `make demo-phase4` creates a local fixture repository, records its digest and Git status, invokes all five tools through contract/schema/policy/audit, applies a worktree-only patch, rejects `/etc/passwd`, runs a test that attempts network access, destroys the sandbox, and proves the fixture origin is unchanged. It requires no model credential.

`make demo-eino-recorded` replays two committed normalized cassettes. Each cassette carries explicit recorded/redacted markers, contains exactly one Usage event before each Finish, and passes terminal-order plus credential-shape validation; those markers are self-declared fixture metadata, not provenance or a substitute for operator review. Each Scenario binds the Reasoner-visible contract, Tool Service, and Policy to its own `allowedPath`; Coding Agent rejects a complete-contract mismatch before Reasoner runs. It routes `repo.read`, `repo.apply_patch`, and `repo.test` through the same phase-4 Tool Service and Docker Sandbox, then proves each temporary origin repository is unchanged and no owned container remains. This is model-dependency replay, not phase-7 Event Replay or phase-6 verification evidence.

The sample values in `.env.example` are local-only defaults, not production credentials. Fake/Replay and CI require no model credential. A live Eino `ChatModel` is an optional manual injection; any credential must come from external secure configuration and must never enter the repository, events, cassettes, logs, or status reports.

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
