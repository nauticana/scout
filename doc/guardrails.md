# Guardrails

`service/guardrail` implements Invariant 8: raw model or tool output never reaches a client, a
destination, or the graph before policy approval.

## Layers

`LayeredEnforcer` composes two compiled rule sets. The **baseline** is release-independent operator
policy compiled once at construction. The **release** layer is the tenant's pinned
`domain.GuardrailConfig`, compiled per `RulesDigest`. Both layers run at every stage, so a release
rule can only add hits — it can never disable, weaken, or shadow a baseline rule of the same id.

Rules are a typed envelope (`domain.GuardrailRuleSet{SchemaVersion, Rules}`). `RuleSetCompiler`
validates at publication (`contract.GuardrailRuleCompiler.Validate`) and compiles once per digest at
runtime into a bounded LRU. Publication stores `Digest(rules)` in `guardrail_config.rules_digest`;
runtime recomputes it and fails closed with `domain.ErrValidation` on any mismatch, so a tampered or
truncated envelope can never execute.

Structural kinds are deterministic and implemented here: `max_input_bytes`, `max_output_bytes`,
`json_schema` (tiny subset: type, properties, required, additionalProperties, items, enum, lengths,
ranges, maxItems, maxProperties), `tool_allowlist`, `destination_allowlist`, `exact_phrase`,
`regex` (bounded by a required `max_match_bytes`), `untrusted_content_marker`, and
`irreversible_tool_approval`. Classifier kinds (`pii`, `toxicity`, `malware`, `prompt_injection`,
`jailbreak`) delegate to injected `contract.ClassifierProvider`s; a rule whose provider is missing
fails closed with `domain.ErrDegraded` — never silently skipped.

Every rule hit produces a `domain.SafetyEvent` through `contract.SafetyEventSink` and, optionally,
`contract.AuditSink`: rule ids, layer, action, severity, policy and release version, acting principal, duration. No
inspected content, no matched substring, ever leaves the enforcer.

## Stages

| Stage | Entry point | Notes |
| --- | --- | --- |
| input | `BeforeModel` | prompt size, phrases, classifiers |
| output | `AfterModelChunk`, `OpenOutputSession` | prefer the session |
| tool_input | `BeforeTool` | tool and destination allowlists, approval gate |
| tool_output | `AfterTool` | schema, classifiers, untrusted fencing |
| retrieval | `InspectRetrieved` | retrieved content is fenced as untrusted |

Tool and retrieved content are wrapped in `<untrusted_content>` … `</untrusted_content>` after all
other rules have run, with embedded closing markers stripped so injected text cannot escape the
fence. `irreversible_tool_approval` consults `contract.ToolApprovalGate`; `pending` and `denied` both
block, and `guardrail.IsPending(err)` distinguishes the resumable case.

A blocking rule returns `*ViolationError` wrapping `domain.ErrForbidden` and carrying the blocking
rule ids, stage, severity, and policy version. `TerminalFrame` builds the payload-free final
`domain.TurnReply` (`ErrorCode = "guardrail"`) for a stream that must end on policy.

## Streaming

`AfterModelChunk` sees one frame at a time, so a phrase or bounded regex spanning a chunk boundary
would escape. `OpenOutputSession` returns a `contract.GuardrailOutputSession` that owns the
cross-chunk state: it holds back the last `lookback-1` bytes (the longest phrase or `max_match_bytes`
over both layers), rescans the held tail plus the new bytes, and releases only what can no longer
participate in a future match. On a violation the retained buffer is dropped, the session latches
failed, and every later `Inspect`/`Flush` returns the same error — no unapproved byte is ever
published. `Flush` releases the tail at end of stream; `Close` is idempotent.

### StreamPump adoption (owner: dataplane)

`service/dataplane/stream_pump.go` calls `AfterModelChunk` per frame. To adopt sessions, in `run`:

1. Before the loop, type-assert the enforcer and open a session:
   `if streaming, ok := pump.Guardrails.(contract.StreamingGuardrail); ok { session, err := streaming.OpenOutputSession(ctx, config, domain.GuardrailSubject{TenantID: turn.TenantContext.TenantID, RequestID: turn.RequestID, ConversationID: turn.ConversationID, ReleaseVersion: agentVersion}); if err != nil { return usage, pump.fail(ctx, turn, route, agentVersion, 0, domain.StageGuardrail, err) }; defer session.Close() }`.
2. Replace the `AfterModelChunk` call with `approved, _, err := session.Inspect(ctx, chunk)` and
   collect `approved.Payload` into a `payloads [][]byte`; when `done`, append every non-empty payload
   from `session.Flush(ctx)`.
3. Publish each collected payload with its own `sequence++`, marking only the last one `final` when
   `done`; when `done` and nothing was collected, publish the existing payload-free final frame.
4. Keep the existing `pump.fail(..., domain.StageGuardrail, err)` path: its terminal frame already
   carries `ErrorCode = "guardrail"`, which equals `guardrail.ErrorCodePolicyViolation`.

Held bytes delay TTFT by at most one lookback window; a policy with no phrase or regex output rule
has `lookback == 0` and streams unchanged.

## Composition

```go
compiler, _ := guardrail.NewRuleSetCompiler(guardrail.CompilerConfig{})
enforcer, _ := guardrail.NewLayeredEnforcer(guardrail.EnforcerConfig{
    Baseline:    guardrail.DefaultBaseline(128<<10, 256<<10),
    Compiler:    compiler,
    Classifiers: map[domain.GuardrailRuleKind]contract.ClassifierProvider{domain.GuardrailKindPII: pii},
    Approvals:   approvals,
    Events:      safetyEvents,
    Now:         clock,
})
```

The tool path wires the same enforcer: `GovernedGateway.Guardrails` plus a
`contract.ToolGuardrailConfigResolver`. `BeforeTool` runs after authorization and before egress,
credentials, and transport, so blocked arguments never leave the platform; `AfterTool` runs after
output validation, before `Invoke` returns.
