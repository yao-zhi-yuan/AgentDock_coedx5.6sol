# Phase 5 status: Eino Adapter and recorded Coding Agent

- Implementation date: 2026-08-03
- Branch: `codex/agentdock-verify`
- Exact phase-4 base:
  `b14437a6ddd414e6fe59eae677d24a0323edcd9c`
- Construction contract: repository-root `AgentDock-Verify-施工计划.md`
- Candidate commit: the single phase-5 commit containing this report; its exact
  SHA is reported to the controller after commit creation.
- Push/PR/Release: not performed

## Status

The phase-5 automated Gate is **PASS**. The final independent read-only code
review is **Ready Yes, Critical 0 / Important 0 / Minor 0**.

This repository is nevertheless handed off as a **candidate**, not declared a
released next phase. Controller-side independent acceptance is still pending.
The optional Live Model acceptance and live-versus-recorded manual
demonstration in sections 14.5/14.6 of the construction plan were not run
because no model credential was supplied or read. Live is not a CI hard gate,
so this does not invalidate the automated Gate; it remains explicit outstanding
manual evidence. Phase 6 has not started.

## Implemented scope

Only phase 5 is implemented on top of the released phase-4 base:

- the framework-neutral `Reasoner` interface returns a normalized `Stream`;
- `Request` has exactly Messages, available Tool Contracts, task summary,
  current failure evidence, and Budget;
- stream output has only Text Delta, Tool Call, Usage, Finish, and Error;
- `FakeReasoner`, `ReplayReasoner`, and an Eino-only `EinoReasoner` adapter;
- conversion between internal messages/contracts and Eino messages/full JSON
  Schemas;
- streaming text, atomic Tool-call ID/name, argument fragments, Usage, Finish,
  interruption, and provider-error normalization;
- one authoritative Coding Agent System Contract, with task/failure context
  encoded as delimited lower-authority user data;
- exactly one Usage event before every successful Finish, per-turn and
  cumulative token-budget enforcement, and fail-closed missing/duplicate
  accounting;
- immutable Tool Registry snapshots and a complete pre-Reasoner equality check
  against the bound phase-4 Tool Service;
- a `CodingAgent` that accepts only `*tools.Service` and routes every call
  through Contract, Policy, audit, and Sandbox;
- one example Go repository with fixed `NormalizeName` and divide-by-zero bugs;
- two deterministic normalized cassettes and recorded Scenario definitions;
- per-Scenario `allowedPath` binding across Reasoner contracts, Registry, and
  Policy (`internal/user/` and `internal/mathutil/`);
- a repeatable no-credential Docker recorded demo.

Controller imports no Eino type. Production Reasoner packages have no direct
database, Store, Sandbox, process, or host-filesystem import. The architecture
test proves this direct-import boundary; it is not presented as a general
transitive capability proof.

## Test-first and remediation evidence

The initial deterministic acceptance tests were written before the phase-5
types and implementation. The first regular Reasoner command exited 1 with
missing `Cassette`, normalized Event/Stream, and error-class APIs. The first
integration command also exited 1 with missing Eino module sums and adapter
APIs. Neither red result is counted as Gate evidence.

The first independent review returned Ready No, Critical 0 / Important 9 /
Minor 3. Focused red regressions were then added for terminal Finish behavior,
missing/duplicate Usage, asynchronous 401/429 classification, Eino atomic
Tool-call fields, complete JSON Schema conversion, caller-controlled system
authority, cassette terminal/secret validation, and concrete Tool Service
binding. All nine Important findings were repaired.

The second review returned Ready No, Critical 0 / Important 1 / Minor 3. It
found that Reasoner-visible contracts could differ from the actual Service
Registry, widening `AllowedPaths`. New tests first failed to compile because
immutable `Registry.Contracts` and `Service.Contracts` did not exist. The fix
deep-copies Registry input/output, exposes sorted immutable snapshots, compares
every contract field before Reasoner runs, scopes both fixed demos, and proves
that a cross-Scenario path is denied before Sandbox execution.

The third review returned Ready Yes, Critical 0 / Important 0 / Minor 2. Both
minor findings were fixed: `CodingAgent.Run` now clones its Reasoner request at
entry, and the negative path test asserts `errors.Is(err, policy.ErrDenied)`.
The final incremental review returned Ready Yes, Critical 0 / Important 0 /
Minor 0.

## Exact phase-5 Gate

These commands were rerun against the frozen candidate after the last code
change. Docker commands used explicitly approved host access because the
filesystem sandbox cannot reach the Docker daemon.

| Command | Exit | Key evidence |
|---|---:|---|
| `go test ./internal/reasoner/...` | 0 | Reasoner `0.549s`; Eino adapter `0.777s` |
| `go test -tags=integration ./internal/reasoner/...` | 0 | Reasoner `1.461s`; Eino adapter `1.446s` |
| `make demo-eino-recorded` | 0 | both fixed Scenarios completed in 4 turns/3 Tool calls; totals 77 and 74 tokens; origins unchanged; owned containers 0; credentials not required; phase-6 verification explicitly not implemented |
| `git diff --check` | 0 | no whitespace errors in the complete candidate/report diff |

The exact acceptance assertions are covered:

- the same cassette creates identical cloned normalized events;
- EOF/error before Finish becomes a recoverable streaming error;
- asynchronous provider authentication/rate failures retain sanitized stable
  classes;
- an unregistered Tool name is rejected;
- Tool arguments outside the registered input Schema are rejected;
- missing, duplicate, decreasing, or over-budget Usage stops the turn;
- Finish is terminal and trailing source events cannot escape;
- Reasoner input cannot directly carry Store/Sandbox/host-file authority;
- Eino imports remain confined to `internal/reasoner/eino`;
- model-visible and Service contracts cannot drift;
- the other Scenario path is denied before Sandbox invocation;
- Fake and Replay do not read provider credentials;
- committed cassettes pass strict JSON, event-order, terminal-state, size, and
  common credential-shape checks.

## Fresh full regression and prior-phase Gates

| Command | Exit | Key evidence |
|---|---:|---|
| `make doctor` | 0 | Go `go1.26.5`, Docker daemon, Compose `5.1.3`, ports, YAML, and Compose model passed |
| `docker compose up -d postgres` | 0 | project PostgreSQL running |
| `make migrate` | 0 | migrations applied idempotently |
| `make test` | 0 | every Go package plus `scripts/configcheck` passed; wall `24.619s` |
| `make test-race` | 0 | every Go package plus configcheck passed under Race; wall `28.932s` |
| `make lint` | 0 | repository vet target passed |
| `go vet ./...` | 0 | explicit full vet passed |
| `go mod verify` | 0 | `all modules verified` |
| `make test-integration` | 0 | Store `6.210s`, Lease `1.786s`, Controller `8.380s`, Migration `3.416s`, CLI `2.865s` |
| `make test-rebuild-state` | 0 | golden/1000-event domain reduction `0.344s`; PostgreSQL checkpoint/full-log rebuild `4.304s` |
| `make chaos-worker-kill` | 0 | 100/100 killed, 100/100 succeeded, WaitingApproval 0; package `42.183s` |
| `make demo-phase3` | 0 | token 1 worker killed, token 2 takeover, stale append rejected, final `Succeeded` |
| `go test -count=1 ./internal/policy/... ./internal/tools/...` | 0 | phase-4 Policy/Tool contract baseline passed |
| `go test -tags=integration -count=1 ./internal/sandbox/...` | 0 | Docker/worktree integration passed in `18.412s` |
| `make sandbox-security-test` | 0 | pinned image rebuilt; complete negative/security suite passed in `27.462s` |
| `make demo-phase4` | 0 | five Tool path, host-sensitive path denial, network denial, patched test, origin digest/status preservation, Destroy, and audit artifact all passed |
| `make demo-fake` | 0 | deterministic successful Run and pause/resume path both reached `Succeeded` |

The phase-4 demo retained the origin digest
`sha256:04167e14a3fa2521b6a138aaaf39e264439da230a54c5e00cee762e830071983`
before and after execution. Its accepted audit artifact digest was
`sha256:6cbca6e744dcac0e18f91922a1a6939c896101465664ff347d40a69841b41158`.

## Resource convergence

Final read-only checks found:

- zero containers with label `agentdock.phase=4`;
- no child entry under the configured temporary `agentdock-worktrees` root;
- exactly one Git worktree: this repository at base HEAD before commit;
- the project PostgreSQL Compose service remained intentionally running and
  healthy for integration evidence; no volume was deleted;
- branch remained `codex/agentdock-verify` and pre-commit HEAD remained exactly
  `b14437a6ddd414e6fe59eae677d24a0323edcd9c`.

## Failed attempts and environment reruns

Failed attempts are retained rather than converted into pass evidence:

- the first sandboxed `make doctor` could not reach Docker; the accepted host
  rerun exited 0;
- sandboxed image build, Sandbox integration, and recorded/phase-4 demos were
  denied Docker socket access; identical approved host commands were rerun and
  only their exit-0 results are accepted;
- the first sandboxed `go mod tidy` and later default-cache test/cache-clean
  attempts hit Go cache permission errors. `go mod tidy` and
  `go clean -testcache` were rerun with approved host access; tests were rerun
  with a writable private cache or approved host access and exited 0;
- an early phase-4 demo process returned incomplete output and was not accepted;
  the complete command was rerun and exited 0;
- all pre-remediation review/test passes were invalidated by later Important
  findings; the tables above use results after the final code change.

## Known limitations and honest boundary

- No provider credential loader is shipped. A live Eino `ChatModel` must be
  injected manually and configured only from an external secure source.
- Live Model/manual acceptance was not executed. The committed cassettes are
  deterministic CI-safe normalized fixtures, not claimed live recordings.
  Their `recording_mode=recorded` and `redacted=true` fields are self-declared;
  operator review and a dedicated secret scanner remain necessary for any
  future live-derived cassette, together with model/version/provenance.
- Credential-shape scanning reduces accidental disclosure; it does not prove
  that a cassette or repository contains no sensitive data.
- Docker is risk reduction for a dedicated development machine or disposable
  runner, not a strong multi-tenant security boundary.
- The execution model remains at-least-once plus scoped idempotency and fencing.
  It does not claim exactly-once behavior or production scale.
- The model-turn limit is a runtime safety bound only. Phase-6 Verification
  Hub, verifier evidence, Repair, and bounded repair are not implemented.
- Phase-7 Event Replay, divergence comparison, OTel, and fault injection are
  not implemented. Kubernetes, UI, MCP, multi-tenancy, and arbitrary host Shell
  also remain out of scope.

## Handoff

Automated Gate 5 and code review are green. Create one phase-5 commit whose
parent is the exact phase-4 base, verify a clean worktree, and hand only the
candidate SHA plus this evidence to the controller. Do not push, create a PR or
Release, or start phase 6.
