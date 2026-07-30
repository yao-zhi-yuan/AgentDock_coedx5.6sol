# ADR 0001: Event log as authoritative state

- Status: Accepted
- Date: 2026-07-30

## Context

Managed Runs must survive controller and worker restarts, expose an audit trail, support pause/resume/cancel, and let recovery use the same path as normal execution. In-memory workflow objects cannot satisfy those requirements.

## Decision

An append-only PostgreSQL event log is the authority for Run state. Reconcile loads events, reduces state, decides at most one action, persists intent, executes, and persists the result. Derived Run rows and checkpoints are caches that must be reproducible from events.

External actions use stable IDs and planned/completed/failed events. Artifacts are immutable digest-addressed evidence referenced by events.

## Consequences

- Restart and audit behavior share one data source.
- Reducers and event schemas require strict deterministic tests and version discipline.
- Log growth requires checkpoints, but checkpoint corruption cannot be allowed to redefine history.
- The external side effect and event append cannot be atomic, so execution remains at-least-once and requires idempotency/receipts.
- Kafka is unnecessary: the MVP needs a transactional per-Run log and worker coordination, not an independent streaming platform.
