# Five-week scope freeze

This document restates the binding scope in `AgentDock-Verify-施工计划.md`. It prevents later phases from treating architectural placeholders as permission to expand the product.

## Required MVP

- one Go monorepo;
- PostgreSQL as the durable authority;
- one controller and multiple workers;
- a framework-neutral Reasoner seam and FakeReasoner in phase 1, followed by EinoReasoner and ReplayReasoner adapters in phase 5;
- a Docker sandbox and a disposable Git worktree per task;
- one synthetic Go example repository;
- one default coding workflow;
- three deterministic verifiers;
- tool timeout, tool error, invalid tool result, and worker-kill injection;
- OpenTelemetry traces;
- a CLI;
- a recorded mode that completes without model credentials.

The runtime must explicitly model Run, Stage, Attempt, Event, and Artifact; use leases and fencing; support pause, resume, and cancel; reconcile normal and recovery paths through the same loop; bind verification to code/spec/version digests; and limit repair to three rounds.

## Explicit non-goals

The five-week MVP must not implement:

- multi-agent collaboration;
- a general DAG editor;
- a web administration UI;
- a Kubernetes Operator or other Kubernetes runtime;
- a service mesh;
- an MCP or Skill marketplace;
- productized memory;
- a credential proxy;
- multi-tenancy or complete authentication;
- cross-cluster scheduling;
- dual wazero and Docker sandboxes;
- an LLM-as-judge platform;
- arbitrary programming-language repositories;
- arbitrary agent-framework adapters;
- a GitHub App or automatic pull-request publication;
- an exactly-once claim;
- a claim that containers are a strong multi-tenant boundary.

Kafka, Redis, Temporal, Kubernetes, OPA, service discovery, and a separate message queue are also excluded from the MVP architecture.

## Optional work

Only after Gates 0 through 8 all pass may the project choose one:

- a simple read-only run detail page;
- one wazero pure-function tool;
- a Kubernetes Deployment example;
- an MCP tool adapter;
- a second model provider.

Optional work cannot replace recovery testing, verifier evidence, the recorded demo, or honest limitation documentation.

## Phase 0 boundary

Phase 0 contains repository structure, documentation, ADRs, dependency pins, configuration examples, Docker Compose base services, and environment validation.

Phase 0 does not contain domain types, reducers, reconciliation, the Reasoner seam, FakeReasoner, storage, migrations, leases, EinoReasoner, ReplayReasoner, sandboxes, tools, policies, verifiers, repair, fault injection, replay implementation, telemetry instrumentation, example application code, process binaries, or user-interface code.
