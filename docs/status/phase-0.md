# Phase 0 status: repository bootstrap and architecture freeze

- Date: 2026-07-30
- Branch: `codex/agentdock-verify`
- Planned commit: `chore: bootstrap agentdock verify`
- Construction contract: repository-root `AgentDock-Verify-施工计划.md`
- Contract blob/hash: `527eaffb1c6b94828efbc3d42aed9b90acf4847a`

## Scope

Only phase 0 was executed. No phase 1 or later runtime/business implementation was started.

The five-week required scope, explicit non-goals, and optional-after-Gate-8 rule are frozen in `docs/scope.md`. The repository and documentation do not add Kafka, Redis, Temporal, Kubernetes, OPA, a service mesh, a queue, multi-agent collaboration, UI, multi-tenancy, MCP, credential proxy, or a second sandbox.

## Completed work

1. Preserved the sole construction plan and placed it under version control without changing its content.
2. Created and switched to `codex/agentdock-verify`.
3. Created the target directory tree. Future business directories contain only phase-labelled `.gitkeep` placeholders.
4. Created Go module `github.com/agentdock/agentdock-verify`, with Go 1.25 minimum and the Go 1.26.5 preferred toolchain.
5. Added README, license, scope freeze, architecture, state machine, event model, threat model, HarnessSpec, and demo placeholder documents.
6. Added the four required ADRs plus ADR 0005 for the verified version baseline.
7. Pinned Go modules and service images. Image tags are also pinned to manifest digests.
8. Added local-only `.env.example`, runtime/OTel/rule/scenario YAML skeletons, and Docker Compose for PostgreSQL, OTel Collector, and Jaeger.
9. Bound all published Compose ports to `127.0.0.1` and mounted PostgreSQL 18 storage at `/var/lib/postgresql`.
10. Added Makefile placeholders that fail clearly until their planned phase, rather than reporting false success.
11. Implemented `make doctor` for Go, Docker daemon, Compose, required parameters, configured distinct/free ports, YAML parsing, and Compose parsing.
12. Added positive and failure-path tests for YAML validation and doctor behavior.
13. Performed the initial implementation review, fixed its Critical/Important issues, and received a clean follow-up verdict for that review scope.
14. Closed the Gate 0 Reasoner phase-boundary conflict: phase 1 owns the framework-neutral seam and FakeReasoner; phase 5 owns EinoReasoner, ReplayReasoner, streaming/tool/usage/finish/error normalization, and compatibility validation of the existing Fake.
15. Ran a fresh independent Reasoner-boundary review: it found no Critical or Important issues; its two editorial consistency notes were also resolved before revalidation.

## Files

### Existing contract, now tracked

- `AgentDock-Verify-施工计划.md` — preserved at hash `527eaffb1c6b94828efbc3d42aed9b90acf4847a`.

### Root and build/tooling

- `.env.example`
- `.gitignore`
- `.golangci.yml`
- `LICENSE`
- `Makefile`
- `README.md`
- `go.mod`
- `go.sum`
- `scripts/doctor.sh`
- `scripts/not-implemented.sh`
- `scripts/configcheck/main.go`
- `scripts/configcheck/main_test.go`
- `scripts/configcheck/doctor_test.go`

### Configuration and protocol skeletons

- `configs/agentdock.yaml`
- `configs/architecture-rules.example.yaml`
- `configs/otel-collector.yaml`
- `deployments/docker-compose.yml`
- `examples/scenarios/harness-run.example.yaml`

### Documentation

- `docs/architecture.md`
- `docs/state-machine.md`
- `docs/event-model.md`
- `docs/threat-model.md`
- `docs/scope.md`
- `docs/harness-spec.md`
- `docs/demo.md`
- `docs/adr/0001-event-log.md`
- `docs/adr/0002-postgresql.md`
- `docs/adr/0003-docker-sandbox.md`
- `docs/adr/0004-eino-boundary.md`
- `docs/adr/0005-version-baseline.md`
- `docs/status/phase-0.md`
- `docs/acceptance/.gitkeep`

### Phase-labelled empty structure

- `cmd/{agentdock,controller,worker}/.gitkeep`
- `internal/{artifact,controller,domain,fault,lease,policy,reasoner,repair,replay,sandbox,store,telemetry,tools,verifier}/.gitkeep`
- `migrations/.gitkeep`
- `examples/buggy-go-service/.gitkeep`
- `testdata/{cassettes,golden,policies}/.gitkeep`

## Version evidence

Official release/support pages and exact selection reasons are recorded in `docs/adr/0005-version-baseline.md`.

The final pins are:

| Dependency | Pin |
|---|---|
| Go language minimum | `1.25.0` |
| Go toolchain | `go1.26.5` |
| Eino | `v0.9.13` |
| pgx | `v5.10.0` |
| golang-migrate | `v4.19.1` |
| Cobra | `v1.10.2` |
| OpenTelemetry Go | `v1.44.0` |
| go-yaml | `v3.0.5` |
| golangci-lint | `v2.12.2` |
| PostgreSQL | `18.4-alpine` plus manifest digest |
| OTel Collector | `0.157.0` plus manifest digest |
| Jaeger | `2.18.0` plus manifest digest |

`docker buildx imagetools inspect` exited 0 for all three images and confirmed `linux/arm64` manifests:

- PostgreSQL index `sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15`;
- OTel Collector index `sha256:4019ce4d7e7791a1a255fffb2f407af66d5017cc65543469ba565c4f47f795b8`;
- Jaeger index `sha256:d2cbd047e44b50c11454820f84fd5f34c24a435e5301cf94fe026bf055db80b3`.

## Required automatic acceptance

| Command | Exit | Key evidence |
|---|---:|---|
| `go version` | 0 | `go version go1.26.5 darwin/arm64` |
| `docker version` | 0 | Client and Engine `29.5.2`; Docker Desktop server reachable |
| `docker compose version` | 0 | `Docker Compose version v5.1.3` |
| `go mod verify` | 0 | `all modules verified` |
| `make doctor` | 0 | All checks passed: Go, daemon, Compose, parameters, configured ports, YAML, Compose model |
| `git diff --check` | 0 | no output |

Because the branch had no initial commit, the ordinary diff did not cover new files until they were staged. The stage-aware command `git diff --cached --check` was therefore also required and exited 0 with no output.

## Additional acceptance

| Command | Exit | Key evidence |
|---|---:|---|
| `go test -v ./scripts/configcheck` | 0 | 13 named positive/failure-path tests passed |
| `go test -race ./...` | 0 | configcheck/doctor tests passed under the race detector |
| `go vet ./...` | 0 | no output |
| `sh -n scripts/doctor.sh` | 0 | no output |
| `sh -n scripts/not-implemented.sh` | 0 | no output |
| `docker compose --env-file .env.example -f deployments/docker-compose.yml config --quiet` | 0 | no output |
| `find cmd internal migrations examples/buggy-go-service testdata -type f ! -name .gitkeep -print` | 0 | no output; no business implementation exists |
| required-file existence checks | 0 | all phase 0 documents/configuration/build files exist |
| forbidden product-directory check | 0 | no Kubernetes Operator, web UI, or multi-agent product directory |
| committed-secret-pattern scan | 1 | expected no-match exit; no key/private-key/token pattern found |

Negative evidence:

| Command | Exit | Expected assertion |
|---|---:|---|
| `go run ./scripts/configcheck AgentDock-Verify-施工计划.md` | 1 | non-config input is rejected |
| `make test` | 2 | phase guard says test implementation starts in phase 1; no false passing placeholder |

## Reasoner boundary remediation revalidation

After closing the Gate 0 documentation conflict, the following commands were rerun:

| Command | Exit | Key evidence |
|---|---:|---|
| `go mod verify` | 0 | `all modules verified` |
| `go test -race ./...` | 0 | configcheck/doctor package passed |
| `go vet ./...` | 0 | no output |
| `make doctor` | 0 | Go, Docker daemon, Compose, parameters, configured ports, YAML, and Compose model all passed |
| `docker compose --env-file .env.example -f deployments/docker-compose.yml config --quiet` | 0 | no output |
| `git diff --check` | 0 | no unstaged whitespace errors |
| `git diff --cached --check` | 0 | no staged whitespace errors across all seven intended remediation files |
| `git diff --cached --name-only` | 0 | only README, architecture/ADR/scope/HarnessSpec/status docs, and `internal/reasoner/.gitkeep` |
| `find cmd internal migrations examples/buggy-go-service testdata -type f ! -name .gitkeep -print` | 0 | no output; no phase 1 implementation exists |
| `git hash-object AgentDock-Verify-施工计划.md` | 0 | unchanged `527eaffb1c6b94828efbc3d42aed9b90acf4847a` |
| obsolete-boundary `rg` audit | 1 | expected no-match exit; no obsolete first-definition wording remains |

The fresh independent Reasoner-boundary review reported zero Critical and zero Important issues and a Gate-ready verdict. Its two Minor editorial notes were fixed before the final rerun.

## Acceptance layers

- Static: Go vet, shell parsing, required-file checks, scope audit, secret scan, Compose parse, and whitespace checks passed.
- Unit: YAML/config and doctor logic have 13 passing tests.
- Integration: real `make doctor` used the selected Go toolchain and live Docker daemon/Compose.
- Negative: malformed/empty/scalar YAML, missing parameters, invalid/duplicate/busy ports, wrong Go, daemon/Compose failures, non-config input, and premature Make targets are rejected.
- Manual: `docs/architecture.md` traces a user request through controller, event log, Reconcile, reasoner, policy/sandbox, verifier, and Artifact; it isolates Eino to the reasoner adapter.
- Manual phase-boundary review: architecture, ADR 0004, README, scope, HarnessSpec, and the Reasoner placeholder consistently assign the seam/Fake to phase 1 and Eino/Replay adapters plus streaming/tool/usage/finish/error normalization to phase 5.
- Manual risk review: recovery correctness, Docker isolation boundary, and replay consistency are explicitly identified as the three highest risks.
- Regression: `go test -race ./...`, `go vet ./...`, and both diff checks passed after review fixes.
- Evidence archive: this report records commands, real exit codes, key assertions, N/A explanations, limitations, and Gate conclusions.

No runtime unit/integration/chaos/e2e demo is applicable in phase 0 because those components are explicitly prohibited in this phase. Their Make targets fail with phase ownership instead of masquerading as acceptance.

## Remediation history

The following failures were not hidden:

1. Initial `go mod download` exited 1 because the Codex workspace sandbox could not write the external Go toolchain cache. The same command was rerun with approved local dependency-write permission and exited 0.
2. Initial sandboxed `make doctor` exited 2 because the nested shell could not access the Docker socket. Running in the approved host context reached the daemon.
3. The first host-context doctor then exited 2 because `go.sum` had only the YAML module's `go.mod` checksum. `go mod download go.yaml.in/yaml/v3` added the content checksum; the next and final doctor runs exited 0.
4. The first stage-aware whitespace check exited 2 and identified trailing blank lines in newly added files. Those files were normalized; the next staged and unstaged whitespace checks exited 0.
5. Independent review found PostgreSQL 18 persistence-path, loopback-binding, configured-port, testing, status, and staged-diff issues. All were fixed and re-reviewed.
6. Gate 0 control review found ADR 0004 incorrectly deferred the internal `Reasoner` seam to phase 5, conflicting with the phase 1 Fake Reasoner Run and the Controller boundary. The plan was already correct and remains unchanged. Architecture, ADR 0004, README, scope, HarnessSpec, the Reasoner placeholder, and this report were synchronized: phase 1 defines the minimal framework-neutral seam and FakeReasoner; phase 5 implements EinoReasoner, ReplayReasoner, and normalization with only backward-compatible seam evolution. Fresh independent review found no Critical/Important issue; it noted that phase 5's repeated Fake item should be described as compatibility hardening and that summaries should include finish/error normalization, and both notes were fixed.

## Incomplete work

Nothing required by phase 0 remains incomplete.

All phase 1 through 8 implementation remains intentionally absent: domain model, reducer, reconcile, framework-neutral Reasoner seam, FakeReasoner, runtime CLI/processes, migrations/store, leases/fencing, EinoReasoner, ReplayReasoner, sandbox/worktree/tools/policy, verifier/repair, fault injection, runtime telemetry, execution replay, example Go service, demos, and UI/Kubernetes/multi-agent capabilities.

## Known limitations

- This repository cannot execute a Run; phase 0 supplies architecture and environment evidence only.
- Compose configuration is parsed and image manifests are verified, but phase 0 does not start services or apply database migrations.
- The phase 0 config checker validates YAML syntax and mapping roots, not the future runtime's complete semantic schema.
- `make doctor` requires a running Docker daemon and free configured localhost ports.
- The module path assumes future publication at `github.com/agentdock/agentdock-verify`; there is no Git remote yet. A different owner must be decided before phase 1 imports make the path costly to change.
- Docker is not a strong multi-tenant isolation boundary.
- Default Make targets for later phases intentionally return non-zero until those phases implement them.

## Gate 0

| Gate item | Conclusion | Evidence |
|---|---|---|
| All required documents exist | PASS | required docs plus scope, HarnessSpec, version ADR, demo placeholder, and this status report exist |
| Scope freeze list exists | PASS | `docs/scope.md` mirrors required, explicit non-goals, and optional-after-Gate-8 boundaries |
| `make doctor` passes | PASS | exit 0 with all declared checks |
| No business code | PASS | future implementation directories contain only `.gitkeep`; tooling is limited to environment/config validation |
| No early multi-agent, UI, or Kubernetes | PASS | no such implementation or product directory exists |
| Manual request-to-Artifact explanation | PASS | `docs/architecture.md` diagram and ten-step path |
| Manual Eino boundary explanation | PASS | architecture and ADR 0004 agree that phase 1 defines the seam/Fake, phase 5 adds Eino/Replay adapters plus streaming/tool/usage/finish/error normalization and Fake compatibility validation, and Controller never imports Eino types |
| Three highest risks identified | PASS | architecture and threat model name recovery correctness, Docker boundary, and replay consistency |

**Gate 0 conclusion: PASS.**

Phase 1 was not started.
