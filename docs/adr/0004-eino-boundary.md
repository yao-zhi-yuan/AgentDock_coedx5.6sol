# ADR 0004: Eino is confined to the reasoner adapter

- Status: Accepted
- Date: 2026-07-30

## Context

Eino already provides model, message, tool-calling, streaming, and agent components. AgentDock still needs durable managed lifecycle, recovery, isolation, verification, replay, and audit semantics that must survive or substitute the model framework.

## Decision

Phase 1 defines the minimum internal, framework-neutral `Reasoner` seam in `internal/reasoner` and implements `FakeReasoner`. The seam contains only the request/result behavior needed for the pure state-machine Run, including a minimal internal tool-call result so Gate 1 can convert an illegal fake tool call into a controlled failure. It must not import Eino, expose provider types, require streaming, PostgreSQL, Docker, or model credentials, or implement the phase 4 Tool Contract. Controller depends only on this seam.

Phase 5 implements `EinoReasoner` and `ReplayReasoner` behind the existing seam. It adds normalization that maps Eino/recorded messages, streaming chunks, tool calls, usage, finish, and provider errors into the internal result model. If phase 5 needs to evolve the seam, the change must be additive or otherwise backward-compatible for the Controller and phase 1 `FakeReasoner`; an Eino interface must not replace the internal contract.

The construction plan's repeated phase 5 `Reasoner` interface and `FakeReasoner` items therefore mean hardening the seam and verifying `FakeReasoner` remains compatible beside the real and recorded adapters, not defining or implementing either one for the first time.

Eino owns ChatModel integration, messages, tool calling, streaming, usage extraction, and adapter-local events.

AgentDock owns Run/session/event persistence, desired and observed state, reconcile, leases and fencing, checkpoints, tool contracts, sandbox/policy, artifacts, verification, repair limits, fault injection, telemetry, and replay.

The controller must not import Eino agent state. Eino cannot write the store, execute host operations directly, or submit verifier success.

## Consequences

- Gate 1 can complete a deterministic Run with `FakeReasoner` and no Eino dependency.
- Phase 5 adds live and recorded adapters without changing what the Controller imports.
- Fake and replay reasoners can run CI without credentials once their respective planned phases are implemented.
- Eino can evolve without redefining durable Run state.
- Phase 5 adapter code must normalize streaming interruption, usage, tool calls, finish, and provider errors.
- The boundary adds translation code but prevents framework state from becoming the runtime authority.

## Phase 5 implementation note

The implemented `reasoner.Request` is frozen to Messages, available Tool
Contracts, current task summary, current failure evidence, and Budget. It does
not carry Run/Attempt identity, Store handles, Sandbox handles, or provider
configuration. The runtime-owned output stream contains only Text Delta, Tool
Call, Usage, Finish, and Error variants.

`internal/reasoner/eino` is the only production package allowed to import Eino.
It accepts an injected `model.BaseChatModel`, installs the fixed System
Contract as the sole system-authority message, converts internal messages and
complete Tool Contract JSON Schemas, preserves atomic Tool-call ID/name fields
while joining only argument fragments, emits exactly one normalized Usage
before Finish, and classifies both setup-time and asynchronous provider errors.
It does not instantiate a provider, read credentials, write files, or execute
Tools.

The runtime `CodingAgent` keeps Run/Attempt Tool scope outside the Reasoner and
requires the Reasoner-visible contracts to exactly match the immutable contract
snapshot from its bound Tool Service before Reasoner is called. It then routes
every normalized call through that Service. ReplayReasoner
plays credential-scanned normalized cassettes; this is not Event Replay or the
phase-7 divergence system. Controller continues to import no Eino type.
