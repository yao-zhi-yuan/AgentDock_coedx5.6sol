# ADR 0002: PostgreSQL for persistence and coordination

- Status: Accepted
- Date: 2026-07-30

## Context

The MVP needs ordered event append, uniqueness, compare-and-swap Run versions, leases, fencing tokens, checkpoints, and inspectable local operation. Five weeks does not justify separate state, queue, lock, and log systems.

## Decision

Use PostgreSQL 18.4 as the only durable coordination database. Use `pgx/v5` for Go access and `golang-migrate` for explicit SQL migrations in phase 2. Event append, Run version update, and fencing checks will share PostgreSQL transactions.

## Why not Temporal

Temporal provides a durable workflow platform, but adopting it would move the project's primary learning and evidence—event reduction, reconciliation, leases, fencing, and crash windows—behind another runtime. It also adds services and operational concepts that the fixed MVP does not need.

## Why not Kafka

The MVP does not need high-throughput fan-out or independent consumer groups. Kafka would add a second authority and make transaction boundaries across events, Run versions, leases, and artifacts harder to explain. PostgreSQL can support the intended local scale with fewer failure modes.

## Why not Redis

Redis would add a second consistency model for leases or queues without removing the PostgreSQL requirement. PostgreSQL advisory/row-level coordination and transactional fencing are sufficient for the planned scale.

## Consequences

- One database is easier to run and inspect.
- Schema and transaction design become critical.
- The design is not a claim of unlimited event throughput.
- A later scale-driven split requires measured evidence and a new ADR.
