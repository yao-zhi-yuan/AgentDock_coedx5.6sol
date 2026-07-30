# Phase 2 status: durable PostgreSQL Event Store and reconciliation

- Date: 2026-07-30
- Branch: `codex/agentdock-verify`
- Gate 1 baseline: `981c5d10263152a54bf84642034e704061b99829`
- Planned commit: `feat: add durable postgres event store`
- Construction contract: repository-root `AgentDock-Verify-施工计划.md`

## Scope

Only phase 2 was implemented. This phase replaces process memory as the production CLI authority with a transactional PostgreSQL Event Log while retaining the compatible `MemoryEventStore` for unit tests and the fast fake demo. Both stores use the same `EventStore` interface and the Controller uses one Reconcile path.

This phase does not implement Worker registration, lease acquisition or renewal, heartbeat, fencing enforcement, takeover, Worker Kill, or any phase 3 recovery behavior. The migration creates the frozen `leases` table only. It also does not implement Eino, Docker Sandbox/worktrees, Tool Contract/Policy, real Verifiers/Repair, fault injection, OTel, Replay product behavior, Kubernetes, or UI.

## Gate 1 preflight

The required phase 1 baseline was rerun before phase 2 tests or implementation:

| Command | Exit | Key evidence |
|---|---:|---|
| Gatekeeper six-test command from `phase-1.md` | 0 | all planned-result/Pause, failure-prefix, and controlled-failure tests printed PASS |
| three channel-controlled Controller tests with `-count=50` | 0 | all 50 repetitions passed |
| `go test -count=1 ./internal/domain/... ./internal/controller/...` | 0 | domain and Controller passed uncached |
| `go test -race -count=1 ./internal/domain/... ./internal/controller/...` | 0 | domain and Controller passed under Race |
| `go test -count=1 ./...` | 0 | all phase 0/1 packages passed |
| `go test -race -count=1 ./...` | 0 | all phase 0/1 packages passed under Race |
| `make test && make test-race && make lint` | 0 | unified full test, full Race, and vet targets passed |
| `go vet ./... && go mod verify && git diff --check` | 0 | modules verified and no static/whitespace failure |
| `make demo-fake` plus real `agentdock session` chain | 0 | fake Run succeeded; create/get/step/pause/resume/cancel reached Cancelled |
| first sandboxed `make doctor` | 2 | Docker socket was unreachable; not accepted as Gate evidence |
| host-context `make doctor` | 0 | Docker daemon, Compose, ports, YAML, and Compose model passed |

The branch was `codex/agentdock-verify`, HEAD exactly matched the Gate 1 baseline, and `git status --short` was empty before phase 2 work.

## Completed work

1. Added explicit transactional SQL migrations for `runs`, `events`, `attempts`, `artifacts`, and schema-only `leases`. Artifact metadata also has a composite `(run_id, attempt_id)` foreign key, so an Attempt from another Run cannot be registered.
2. Added database uniqueness constraints for `(run_id, seq)` and `(run_id, idempotency_key)`, plus an append-only update/delete guard for `events`.
3. Evolved the common Store interface to require `expectedVersion`, explicit `ErrVersionConflict`, `Load`, `Rebuild`, and one-event `Append`.
4. Kept `MemoryEventStore` compatible with the same interface and CAS/idempotency/payload validation behavior.
5. Added a `pgx/v5` PostgreSQL Store with:
   - row-locked expected-version comparison;
   - exact idempotent replay before stale-version rejection;
   - candidate-log reducer validation;
   - event insert, derived Attempt insert, and Run version/projection update in one transaction;
   - PostgreSQL-assigned transaction time returned as UTC RFC 3339;
   - no process-local Run cache.
6. Added verified checkpoint snapshots:
   - snapshot state digest and authoritative event-prefix digest;
   - checkpoint projection compared field by field with `Reduce(events[:checkpointSeq])`;
   - suffix reduction only after checkpoint validation;
   - full Event Log fallback for missing, stale, corrupt, or invalid checkpoints;
   - checkpoint writes do not advance or replace the authoritative Run version.
7. Added complete-first Artifact persistence:
   - temporary file;
   - streamed SHA-256;
   - sync and close;
   - atomic rename of complete bytes;
   - database metadata insert only afterward with database time.
   - this proves completeness and digest agreement at registration, not post-registration immutability.
8. Added payload rejection for credential-shaped JSON keys, full environment containers, common secret markers, raw `sk-proj`/AWS/Service token forms, assignments, and credential-bearing URLs before either Store persists an event. Every current and future string-typed `EventData` field is scanned from its raw value and recursively parsed when it contains JSON.
9. Added an explicit migration runner and `make migrate`, real `make test-integration`, and `make test-rebuild-state`. Repository migrations are explicitly transaction-wrapped; failed application DDL rolls back, while golang-migrate's dirty marker is intentionally retained to prevent an unverified automatic retry.
10. Added a root Compose include so the required `docker compose up -d postgres` works from repository root. The local host port is `55433` to avoid an unrelated invisible 5432 conflict; the container still listens on 5432.
11. Updated `make doctor` so a busy PostgreSQL, OTel gRPC, OTel HTTP, or Jaeger UI port is accepted only when the expected service in the same Compose project publishes the exact configured host/container mapping; unrelated port conflicts remain failures.
12. Made `AGENTDOCK_DATABASE_URL` mandatory for standalone `run` commands. Every invocation opens a new Store/Controller and reconstructs the same Run; there is no silent authoritative-memory fallback. The explicit memory demo/session compatibility paths remain available.
13. Added `agentdock run events RUN_ID` to inspect the authoritative ordered log.
14. Added a black-box integration test whose create/step/events/get commands each execute in a separate OS process and prove contiguous Event sequence plus field-equal final reconstruction.
15. Updated README, architecture, state-machine, event-model, and demo documentation to describe phase 2 behavior and preserve phase 3 boundaries.

## Test-first and red-green evidence

The following failures were intentionally observed and were not treated as passing evidence:

1. Phase 2 unit acceptance tests and fixed golden files were added before implementation.
   - Red command: `go test -count=1 ./internal/domain ./internal/store`
   - Exit: 1.
   - Missing behavior: `ReduceFromCheckpoint`, CAS-aware `Append`, `ErrVersionConflict`, and `ErrSensitivePayload`.
   - Green command after implementation: `GOCACHE=/private/tmp/agentdock-go-cache go test -count=1 ./internal/domain ./internal/store ./internal/controller ./cmd/agentdock ./cmd/migrate ./internal/migration`
   - Exit: 0.
2. PostgreSQL/Migration/Controller integration acceptance tests were added before implementation.
   - Red command: `go test -tags=integration -count=1 ./internal/store/... ./internal/controller/... ./internal/migration/...`
   - Exit: 1.
   - Missing behavior: PostgreSQL Store, Migration package, Artifact Store, durable Controller restart, plus missing pgx content checksums.
   - Green Store/Controller command: `GOCACHE=/private/tmp/agentdock-go-cache go test -tags=integration -count=1 ./internal/store/... ./internal/controller/...`
   - Exit: 0.
   - Green Migration command: `GOCACHE=/private/tmp/agentdock-go-cache go test -tags=integration -count=1 ./internal/migration/...`
   - Exit: 0.
3. A verbose repeated integration run exposed a test-isolation bug in the Artifact acceptance test.
   - Command: `go test -tags=integration -count=1 -v ./internal/store/... ./internal/controller/... ./internal/migration/...`
   - Exit: 1.
   - Failure: the test reused the global primary key `artifact-complete` left by an earlier successful run.
   - Fix: derive Artifact IDs from the unique Run ID.
   - Focused green: `go test -tags=integration -count=1 -v ./internal/store -run TestArtifactRegisteredOnlyAfterCompleteDigestWrite`
   - Exit: 0.
4. The first full regression after changing the isolated PostgreSQL port exposed stale phase 0 Doctor test fixtures.
   - Command: `go test -count=1 ./...`
   - Exit: 1.
   - Failures: four Doctor assertions still expected/replaced port 5432.
   - Fix: update the fixtures to the configured 55433 value and teach Doctor to distinguish the running project service from unrelated listeners.
   - Identical full-test green: `GOCACHE=/private/tmp/agentdock-go-cache go test -count=1 ./...`
   - Exit: 0.
5. The independent review's cross-Run Artifact and Load-snapshot regressions were added before the schema fix.
   - Red command: `go test -tags=integration -count=1 ./internal/store -run 'TestArtifactAttemptMustBelongToSameRun|TestPostgresLoadUsesOneConsistentSnapshot'`
   - Exit: 1.
   - Failure: the old schema accepted an Artifact whose `attempt_id` belonged to another Run. The controlled `Load` snapshot assertion already passed against the Repeatable Read implementation.
   - The red write left one precisely identified cross-Run test row in the local pre-migration database. That single test fixture was deleted after a read-only ownership query; no application data or unrelated Run was removed.
   - First migration attempt then exited 2 because the stricter foreign key correctly rejected that row and left only golang-migrate's dirty marker; PostgreSQL had rolled back both new constraints.
   - Fix: add the composite foreign key and strengthen migration failure acceptance to prove application DDL rolls back while a protective dirty marker blocks an unverified retry. The dirty marker is never auto-forced backward because a post-commit acknowledgement failure can be ambiguous.
   - Green focused command: `go test -tags=integration -count=1 ./internal/store ./internal/migration ./cmd/agentdock -run 'TestArtifactAttemptMustBelongToSameRun|TestPostgresLoadUsesOneConsistentSnapshot|TestUpMigratesEmptySchemaWithAllPhase2Tables|TestFailedMigrationLeavesNoPartialApplicationTables|TestCLIContinuesRunAcrossSeparateProcesses'`
   - Exit: 0.
6. A pre-review `make doctor` exited 2 after correctly reporting that a separate Compose project owned 4317.
   - No unrelated container was stopped.
   - The repository-local example host ports moved to unused 14317/14318/16687; container ports remain conventional.
   - The final default `make doctor` exited 0 and validated the exact PostgreSQL publication plus all four configured host ports.
7. External Gatekeeper checkpoint and PostgreSQL payload regressions:
   - Red command: `GOCACHE=/private/tmp/agentdock-go-cache AGENTDOCK_DATABASE_URL=... go test -tags=integration -count=1 ./internal/store -run 'TestPostgresRebuildRejectsSemanticallyForgedCheckpointState|TestPostgresRejectsNestedCredentialInReasonWithoutAppending'`
   - Exit: 1.
   - Failures: both no-suffix and with-suffix Rebuild returned a self-consistent forged `ScenarioID`; nested credential JSON in `Reason` was appended.
   - Fix: compare the checkpoint projection field by field with authoritative prefix reduction before suffix reduction; scan every raw string-typed `EventData` field and recursively inspect nested JSON.
   - Identical green command: exit 0.
8. External Gatekeeper all-string-field credential regression:
   - Red command: `GOCACHE=/private/tmp/agentdock-go-cache go test -count=1 ./internal/store -run 'TestMemoryStoreRejectsNestedCredentialJSONInEveryEventDataStringField|TestMemoryStoreCredentialJSONVariantsAndNormalText'`
   - Exit: 1.
   - Failures: nested JSON was accepted in `ScenarioID`, `SpecHash`, `DesiredState`, `AttemptID`, `ActionID`, `ToolName`, and `Reason`; the AWS separator variant was also accepted.
   - A follow-up JSON-as-string case, `{"wrapper":"{\"OpenAI.Api-Key\":\"opaque-credential\"}"}`, then produced another real exit 1 because a string child was not decoded again.
   - Fix: reflection-based enumeration of every string-typed field, recursive object/array and JSON-string inspection with a conservative depth limit, normalized case/separators, and explicit AWS/Service token keys.
   - Identical green command after the complete fix: exit 0; normal nested JSON is accepted in every field.
9. External Gatekeeper full-Compose Doctor regression:
   - Red command: `GOCACHE=/private/tmp/agentdock-go-cache go test -count=1 ./scripts/configcheck -run 'TestDoctorAcceptsAllBusyPortsOwnedByExpectedComposeServices|TestDoctorRejectsBusyTelemetryPortOwnedElsewhere'`
   - Exit: 1.
   - Failure: the expected running OTel service on its exact configured port was rejected as a conflict.
   - Fix: map all four configured host ports to their expected Compose service/container port.
   - Identical green command: exit 0; a wrong telemetry publication remains rejected.

The first `go mod download all` attempt also exited 1 because it tried to fetch unrelated future-phase dependency graphs in the restricted network/cache context. It was not accepted as verification. Only the already pinned phase 2 pgx/golang-migrate packages and their selected transitive modules were subsequently resolved; `go mod verify` passes.

## Phase 2 automatic acceptance

Final fresh commands after the external Gatekeeper remediation and independent read-only re-review:

| Command | Exit | Key evidence |
|---|---:|---|
| `docker compose up -d postgres` | 0 | `agent-postgres-1` running and healthy on `127.0.0.1:55433` |
| `GOCACHE=/private/tmp/agentdock-go-cache make migrate` | 0 | `migrations applied` |
| `GOCACHE=/private/tmp/agentdock-go-cache go test -tags=integration -count=1 ./internal/store/... ./internal/controller/...` | 0 | PostgreSQL Store and durable Controller restart passed |
| `GOCACHE=/private/tmp/agentdock-go-cache go test -tags=integration -count=1 ./internal/migration/... ./cmd/agentdock` | 0 | empty-schema/failure migration and true separate-process CLI continuation passed |
| `GOCACHE=/private/tmp/agentdock-go-cache go test -race -count=1 ./internal/store/... ./internal/controller/...` | 0 | common Store and Controller paths passed under Race |
| `GOCACHE=/private/tmp/agentdock-go-cache make test-rebuild-state` | 0 | golden full-state, 1000-event checkpoint suffix, and PostgreSQL rebuild passed |
| `GOCACHE=/private/tmp/agentdock-go-cache go test -count=1 ./...` | 0 | all packages passed uncached |
| `GOCACHE=/private/tmp/agentdock-go-cache go test -race -count=1 ./...` | 0 | all packages passed under Race |
| `GOCACHE=/private/tmp/agentdock-go-cache make test` | 0 | unified full Go test target passed |
| `GOCACHE=/private/tmp/agentdock-go-cache make test-race` | 0 | unified full Race target passed |
| `GOCACHE=/private/tmp/agentdock-go-cache make lint` | 0 | Make vet target passed |
| `GOCACHE=/private/tmp/agentdock-go-cache go vet ./...` | 0 | no output |
| `go mod verify` | 0 | `all modules verified` |
| `docker compose up -d` | 0 | complete current-project PostgreSQL, OTel Collector, and Jaeger stack started |
| `GOCACHE=/private/tmp/agentdock-go-cache make doctor` in host context | 0 | all four busy ports mapped to the exact expected current-project services; Compose and YAML passed |
| `docker compose stop otel-collector jaeger` | 0 | stopped only the two services added for the Doctor scenario; PostgreSQL remained running |
| Gatekeeper six-test command with `-count=1 -v` | 0 | all six phase 1 regressions printed PASS |
| three channel-controlled Controller tests with `-count=50` | 0 | all 50 repetitions passed |
| `GOCACHE=/private/tmp/agentdock-go-cache make demo-fake` | 0 | memory compatibility demo reached Succeeded |
| executable dependency, phase-3 behavior, plan-diff boundary scan | 0 | no later-phase executable dependency or phase-3 behavior; construction plan unchanged |
| `git diff --check` | 0 | no whitespace errors |

The first exact `docker compose up -d postgres` attempt exited 1 because host port 5432 was genuinely unavailable. Host inspection found no safely terminable process, while an unrelated healthy container used 55432. The repository-local development port was moved to unused 55433; the identical required command then exited 0. The first restricted `make migrate` exited 2 because localhost access was denied; the required host-context rerun exited 0.

## Named invariant and negative acceptance

The phase 2 tests explicitly assert:

- an empty schema receives all five required tables;
- a deliberately failing transaction-wrapped migration leaves no partially created application table;
- the same failed migration leaves a protective dirty marker and a subsequent unverified retry is rejected;
- both required per-Run uniqueness constraints exist in PostgreSQL;
- Artifact metadata cannot pair a Run with an Attempt owned by another Run;
- database time replaces a caller-supplied 1900 timestamp;
- event insert and Run version advance remain equal;
- two Store instances writing different valid facts at the same expected version produce one success and one explicit version conflict;
- exact idempotent replay returns the original event without another row, while different content under the same key returns `ErrIdempotencyConflict`;
- a database trigger failure after event insert rolls back both the event and Run version;
- terminating the pool's PostgreSQL backend is followed by a successful new connection and Load;
- a controlled create between `Load`'s event query and existence check cannot produce a mixed snapshot;
- 1000 valid events load and reduce to the same full State;
- checkpoint-prefix plus suffix reconstruction equals full-log reduction field by field;
- missing, malformed, digest-corrupt, and semantically forged checkpoint state cannot override the Event Log and triggers correct full-log fallback;
- self-consistent state+digest tampering with an unchanged valid event-prefix digest is rejected both with and without a suffix;
- an Artifact reader failure leaves neither a database row nor a partial file;
- a complete Artifact's bytes, size, SHA-256 digest, path, and database timestamp agree;
- credential-bearing event payloads are rejected before persistence across every string-typed `EventData` field, including nested JSON objects/arrays and case/separator variants; ordinary nested text remains accepted;
- busy ports for PostgreSQL, both OTel endpoints, and Jaeger are accepted only when the expected service in this Compose project publishes the exact mapping;
- discarding the first Store, Controller, and FakeReasoner objects and creating new ones continues the same Run to Succeeded;
- black-box CLI create/step/events/get commands in distinct OS processes continue the same Run to Succeeded and rebuild the same State;
- the recovered Event Log has contiguous sequence numbers.

## Golden Trace acceptance

The fixed files are:

- `testdata/golden/phase-2-events.json`
- `testdata/golden/phase-2-state.json`

`TestReduceGoldenEventsMatchesGoldenStateFieldByField` unmarshals both and compares the complete `domain.State` with `reflect.DeepEqual`. It checks Run identity, scenario/spec, desired and observed states, attempt/version, created/updated times, attempt ID, reasoning output, tool data, patch and verification facts, pending/resume/failure zero values, and last event type. It does not compare only the terminal status.

## Manual CLI restart acceptance

A real binary was invoked as independent processes with PostgreSQL:

1. `run create run-phase2-restart-demo` produced `Queued`, version 1.
2. Two separate `run step` processes produced `Provisioning` version 2 and `Reasoning` version 3.
3. Those processes exited, so no Controller or Store object remained.
4. Four new `run step` processes continued the same Run through:
   - `RunReasoner`: versions 4 and 5;
   - `ApplyPatch`: version 6;
   - `Verify`: versions 7 and 8;
   - `SucceedRun`: version 9.
5. A final independent `run events` process printed exactly seq 1 through 9 in order.

The final state was `Succeeded`, with `RunCreated`, `AttemptStarted`, `WorkspaceProvisioned`, `ReasoningPlanned`, `ReasoningCompleted`, `PatchProduced`, `VerificationPlanned`, `VerificationPassed`, and `RunSucceeded`.

## Boundary checks

| Check | Conclusion |
|---|---|
| `leases` | table only; no registration, acquire, renew, heartbeat, expiry, takeover, or fencing decision code |
| `fencing_token` | persisted only as the frozen Event envelope field; no phase 3 validation behavior |
| Worker/Chaos | no Worker runtime or Kill harness |
| Eino | no Eino adapter or model dependency imported into executable behavior |
| Sandbox/Tool/Policy | no Docker execution, worktree, Tool Contract, or Policy implementation |
| Verifier/Repair | phase 1 inert verification facts only; no real Verifier or repair loop |
| OTel/Replay | no telemetry or Replay product behavior |
| Kubernetes/UI | absent |
| claims | documentation explicitly rejects exactly-once, production scale, and strong multi-tenant isolation claims |
| construction plan | `AgentDock-Verify-施工计划.md` is unchanged |

## Incomplete work

Nothing required by phase 2 is intentionally omitted.

All phase 3 through 8 behavior remains absent. In particular, the `leases` schema is inert until phase 3.

## Known limitations

- PostgreSQL reconstruction is validated at local MVP scale. Append validates the complete candidate log, so this phase does not claim production event throughput.
- Checkpoints are caches protected by snapshot/event-prefix digests and an authoritative prefix projection comparison. A missing, corrupt, or semantically forged checkpoint falls back to the full log. The phase 2 safety check re-reduces the prefix, so it does not claim checkpoint-driven production performance; this is not a separate Replay product.
- A database registration failure after a complete Artifact rename can leave an unregistered complete orphan file; it cannot leave a registered partial file. Orphan cleanup is not implemented in phase 2.
- Artifact bytes and metadata agree at registration, but phase 2 does not make the 0600 file read-only or reject later metadata UPDATE/DELETE. Post-registration immutability and tamper prevention are not claimed.
- Payload scanning is defense in depth over the narrow `EventData` schema, not a general DLP system.
- Repository migrations must remain explicitly transaction-wrapped. On failure before `COMMIT`, PostgreSQL rolls back application DDL; golang-migrate intentionally retains a dirty marker until an operator verifies the database and explicitly repairs the version. This avoids guessing after an ambiguous post-commit connection failure and does not make arbitrary non-transactional migration SQL safe.
- The reconnect test terminates a live PostgreSQL backend and verifies pool recovery. It is not phase 3 Worker/process takeover or a Chaos Kill harness.
- The local PostgreSQL host port is 55433 because 5432 was unavailable and no safe owner could be identified. Local telemetry example ports are 14317/14318/16687 because a separate Compose project owns the conventional ports; no unrelated container was stopped. Container ports are unchanged.
- The FakeReasoner lifecycle facts remain inert: they do not execute a model, patch, sandbox, or verifier.
- No exactly-once, production-scale, strong multi-tenant isolation, Worker recovery, or fencing claim is made.

## Independent review

The required independent read-only review ran in three passes:

1. Initial review: Critical 0; Important 3; Minor 3.
   - Important: cross-Run Artifact ownership, incomplete raw credential detection, and a possible `Load` read skew.
   - Minor: Doctor did not compare the exact published PostgreSQL port, README had the old host port, and automated recovery used new objects rather than distinct OS processes.
   - All six findings received focused tests and fixes.
2. First follow-up: the six original findings were closed, but a new Important found that automatic `Force(previousVersion)` after an ambiguous migration failure could make committed DDL disagree with the migration version.
   - The automatic Force behavior was removed.
   - Failure acceptance now proves no partial application table, one protective dirty marker, and rejection of an unverified retry.
3. Final follow-up: Critical 0; Important 0; Minor 0.
   - The reviewer explicitly closed the migration consistency finding and released the change to full fresh verification.
4. A separate external Gatekeeper then rejected Gate 2 with Critical 0, Important 2, Minor 2.
   - Important: checkpoint self-hashes did not prove equality with the authoritative event-prefix projection; nested credential JSON could bypass scanning in string fields other than `Output`/`ToolArguments`.
   - Minor: full Compose telemetry ports were not recognized as current-project publications; Artifact documentation overstated post-registration immutability.
   - All four findings now have focused red-green evidence or an explicit documentation downscope.
5. A new independent read-only reviewer inspected the complete baseline-to-phase-2 range and the uncommitted Gatekeeper remediation.
   - It found the JSON-as-string bypass and two stale checkpoint-acceleration comments while the review was active; both received tests or documentation fixes before the final verdict.
   - Final verdict: Critical 0; Important 0; Minor 0; all four external Gatekeeper findings closed; `Ready to amend? Yes`.

## Gate 2

**READY FOR EXTERNAL RE-REVIEW; NOT YET RELEASED.** The external Gatekeeper findings are remediated, the required independent read-only re-review reports Critical/Important/Minor 0, and the complete post-remediation verification suite passes. Final Gate 2 release still belongs to the external Gatekeeper reviewing the amended HEAD. Phase 3 remains intentionally unstarted.
