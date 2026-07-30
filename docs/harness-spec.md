# HarnessSpec v1alpha1

Phase 0 freezes the minimum shape of a `HarnessRun`. It does not implement a CRD, a generic schema migration framework, or a parser.

The canonical example is [`../examples/scenarios/harness-run.example.yaml`](../examples/scenarios/harness-run.example.yaml).

## Required fields

| Path | Meaning |
|---|---|
| `apiVersion` | `agentdock.dev/v1alpha1` |
| `kind` | `HarnessRun` |
| `metadata.name` | stable scenario name |
| `target.adapter` | adapter selector: Fake is implemented in phase 1; Eino and Replay adapters are implemented in phase 5 |
| `target.repository` | repository path copied into a disposable worktree |
| `target.revision` | immutable or resolved Git revision |
| `task.prompt` | requested repository change |
| `task.allowedPaths` | paths the patch may modify |
| `runtime.maxRepairRounds` | at most `3` |
| `runtime.timeout` | whole-run budget |
| `runtime.tokenBudget` | model usage budget |
| `sandbox.*` | network, CPU, memory, PID, and command-timeout limits |
| `verification[]` | deterministic verifier plan |
| `faults[]` | optional bounded fault injection |

## Frozen semantics

- A target repository is never modified directly.
- `network: none` is the default.
- Allowed paths constrain writes even when tests would pass.
- Repair is bounded to three rounds.
- Verification failure is blocking unless the verifier is explicitly defined as non-blocking by a later schema.
- Live model credentials are not part of this document.
- Changing the normalized spec changes `spec_hash` and invalidates earlier evidence.

Future schema changes require an ADR and explicit compatibility tests. They are not implemented in phase 0.
