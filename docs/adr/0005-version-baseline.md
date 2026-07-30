# ADR 0005: Phase 0 version baseline

- Status: Accepted
- Date: 2026-07-30

## Context

The construction plan intentionally leaves versions to phase 0. The local bootstrap toolchain was Go 1.22.12 on Darwin/arm64, Docker client 29.5.2, Docker Compose 5.1.3, and GNU Make 3.81. The Docker daemon was not running during the initial inventory and must be running for Gate 0.

Go 1.22 is outside the current Go support window. The latest pgx and OpenTelemetry modules require Go 1.25, so retaining the local bootstrap version would freeze an unsupported and incompatible baseline.

## Decision

### Go compatibility

- Module path: `github.com/agentdock/agentdock-verify`.
- Module language minimum: Go `1.25.0`.
- Preferred and automatically selected toolchain: Go `1.26.5`.
- CI and recorded evidence must use the preferred toolchain unless an ADR deliberately changes it.

Go 1.26.5 was the current stable patch release on the check date. Go's official policy supports a major release until two newer major releases exist. The `toolchain` directive lets the existing Go 1.22 bootstrap command download/select 1.26.5 instead of pretending the old installation is supported.

No Git remote exists during phase 0. The module path is the frozen publication assumption; if the repository owner changes it, that change must happen before phase 1 introduces imports and must update this ADR.

### Go modules

| Purpose | Module | Pinned version | Reason |
|---|---|---:|---|
| Eino adapter | `github.com/cloudwego/eino` | `v0.9.13` | current official release; isolated behind adapter |
| PostgreSQL | `github.com/jackc/pgx/v5` | `v5.10.0` | current stable v5; requires Go 1.25 |
| SQL migration | `github.com/golang-migrate/migrate/v4` | `v4.19.1` | single explicit migration tool |
| CLI | `github.com/spf13/cobra` | `v1.10.2` | current stable release; no Viper dependency selected |
| telemetry | `go.opentelemetry.io/otel` | `v1.44.0` | current stable API/SDK line; supports Go 1.26 |
| YAML | `go.yaml.in/yaml/v3` | `v3.0.5` | maintained v3 module and used by phase 0 config check |

`go.mod` is the machine-readable pin. `go.sum` supplies integrity hashes. Unused libraries remain unimplemented architecture choices; adding them to `go.mod` does not authorize phase 1 work.

### Tooling and service images

| Dependency | Pinned version |
|---|---:|
| golangci-lint | `v2.12.2` |
| PostgreSQL image | `postgres:18.4-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15` |
| OpenTelemetry Collector image | `ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector:0.157.0@sha256:4019ce4d7e7791a1a255fffb2f407af66d5017cc65543469ba565c4f47f795b8` |
| Jaeger image | `jaegertracing/jaeger:2.18.0@sha256:d2cbd047e44b50c11454820f84fd5f34c24a435e5301cf94fe026bf055db80b3` |

Host Docker/Compose are validated for availability rather than downloaded by the repository. Service images use explicit release tags and never `latest`.

The three manifest indexes were resolved on the check date and each included `linux/arm64`, matching the local Docker engine. Tag plus digest prevents silent image mutation.

## Official evidence checked

- [Go release history and support policy](https://go.dev/doc/devel/release)
- [Eino v0.9.13 release](https://github.com/cloudwego/eino/releases/tag/v0.9.13)
- [pgx version/support policy](https://github.com/jackc/pgx) and [v5.10.0 tag](https://github.com/jackc/pgx/releases/tag/v5.10.0)
- [OpenTelemetry Go compatibility](https://github.com/open-telemetry/opentelemetry-go) and [v1.44.0 release](https://github.com/open-telemetry/opentelemetry-go/releases/tag/v1.44.0)
- [Cobra releases](https://github.com/spf13/cobra/releases)
- [go-yaml v3.0.5 release](https://github.com/yaml/go-yaml/releases/tag/v3.0.5)
- [PostgreSQL versioning policy](https://www.postgresql.org/support/versioning/) and [18.4 release notes](https://www.postgresql.org/docs/release/18.4/)
- [OpenTelemetry Collector v0.157.0 release](https://github.com/open-telemetry/opentelemetry-collector-releases/releases/tag/v0.157.0)
- [Jaeger releases](https://github.com/jaegertracing/jaeger/releases)
- [golangci-lint releases](https://github.com/golangci/golangci-lint/releases)
- [golang-migrate releases](https://github.com/golang-migrate/migrate/releases)

Module proxy metadata was also queried with `go list -m -json <module>@latest` to confirm the exact tags and declared minimum Go versions.

## Consequences

- First use may download the pinned Go toolchain and modules.
- Go 1.25 is the minimum because current pgx and OTel releases require it.
- Upgrades are deliberate ADR changes with regression evidence, not floating tags.
- The version set is a reproducible baseline, not a promise that every future phase will use every library.
