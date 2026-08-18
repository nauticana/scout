# Policy, approvals, credentials, and evidence

Where [doc/authority.md](authority.md) establishes *who* is acting and *what* their
configuration is, this covers what happens at the boundary: whether the action is allowed, what
conditions ride along, who has to say yes, which identity the call uses, and what is written down.

## Decision point

`policy.SetEvaluator` implements `contract.PolicyDecisionPoint` over the policy statements frozen
into the principal's effective release, read by `policy.ReleaseResolver`. Using the release rather
than the scope's current bindings means a decision uses the policy the work was published with.

Three rules make the default safe:

- **Deny wins**, regardless of statement order.
- **No match is a deny.** An unbound resource is refused, never allowed by omission.
- **An evaluator failure is a deny**, and it returns an auditable reason rather than an error alone.

Patterns match exactly or with a single trailing `*`. Nothing richer, so a pattern cannot widen in a
way its author did not intend.

Statements bind at any scope through `config_scope_binding` with `config_resource_kind = policy`, and the
`policy` narrowing rule enforces the asymmetry that matters: **a child may drop an allow or add a
deny, never add an allow.**

## Obligations

An allow may carry obligations — `require_approval`, `redact`, `cap_spend`, `record_evidence`,
`notify`. `GovernedGateway` applies them before egress, and an obligation with no registered
enforcer is a hard failure (`domain.ErrDegraded`). Silently skipping one would turn a conditional
allow into an unconditional one, which is the whole point of the mechanism.

This is where the operating modes live. `advise`, `draft`, `execute_with_approval`, and
`bounded_autonomous` are enforced as obligations and limits, never as prompt text.

## Durable approvals

An irreversible action does not fail for lack of a human — it waits.

`approval.Gate` implements `contract.ToolApprovalGate` over durable requests. The first call opens an
`approval_request` and returns pending; later calls return whatever verdict was recorded. Open is
idempotent on `(tenant, request, execution_step)`, so a replayed turn re-attaches instead of asking
the same person twice.

`ProposalDigest` binds a verdict to the exact call — tenant, request, tool, version, and arguments.
The digest is also a `WHERE` predicate in the resolve statement, so approving a changed action
updates nothing rather than resolving the wrong proposal.

The turn lifecycle gained `suspended`:

| Step | What happens |
| --- | --- |
| Pending approval | `guardrail.ErrApprovalPending` wraps `domain.ErrApprovalPending`, not `ErrForbidden` |
| `TurnRuntime.suspend` | Records the suspension, publishes a final frame, acks the delivery |
| Budget | The reservation stays **held** — settling would resume with no budget, releasing would let a tenant park work to dodge its ceiling |
| Verdict | `approval.Resumer` records it, returns the turn to `queued`, and re-dispatches |
| Replay | The worker resumes from the last checkpoint through `StepIdempotencyStore`; committed steps do not re-execute |

Resolving without re-dispatching would leave the turn parked forever, so `Resumer` owns all three
steps rather than leaving the last one to a caller.

`approval.Sweeper` moves overdue requests on: `BackupEscalation` routes to a configured backup and,
with none, expires the request. A backup needs its own grant — escalation moves *who decides*, never
*what may be decided*.

## Credentials

`ToolCredentialProvider` is keyed on the principal, not the tenant:

```go
Credential(ctx, principal, toolID, action, purpose) ([]byte, domain.AuthorityRef, error)
```

`tool_credential_binding` maps `(tenant, principal, tool, purpose)` to a reference into keel's secret or
OAuth-connection store — never secret material, so a binding is safe in logs, definitions, and audit
payloads. `BoundCredentialProvider` resolves it just in time, after policy, guardrails, egress, and
admission have all passed.

Two agents on the same tool version therefore resolve **different** identities. The returned
`AuthorityRef` names whose authority was exercised, including the human behind a delegated OAuth
connection; the gateway records it and never the secret. `TableCredentialBindings.Revoked` reports
bindings whose delegation ended so bound work can be stopped rather than continuing orphaned.

## Evidence

`domain.DecisionRecord` replaces the old opaque audit event. It carries principal, authority chain,
scope, action, resource, release, policy id and version, outcome, obligations, reason, and a
reference to redacted evidence in object storage.

`observability.TableAuditSink` is both sides: `Record` writes, `Decisions` reads. Evidence with no
way to read it answers nothing, which is why `AuditQuery` ships with the sink rather than later.
A query reads exactly one tenant, or — with `TenantID` zero — only the platform-wide records that
name no tenant, such as a rollout transition. Reading across tenants is not expressible in
`domain.DecisionQuery`, and a platform-wide record may not name a tenant scope.

Evidence is deliberately not telemetry. Metrics are sampled and lossy; a decision record is not, and
never derives from one.

Records are emitted at model routing, tool invocation, guardrail hits, credential resolution,
approval verdicts, rollout transitions, version resolution, and turn terminal states.

## Attribution

`usage_event` gained `principal_kind`, `principal_id`, and `scope_id`, and `RecordUsage` takes a
`domain.UsageAttribution`. Cost is now reportable per agent and per organizational scope, not only
per tenant — which is what a per-managed-agent price and a cost-per-outcome metric need.

`isolation.ReleaseLimits` reads the budget and autonomy frozen into the release. Compilation already
narrowed both against every parent scope, so nothing here re-derives inheritance: it reads one row.
Outside its operating window a `bounded_autonomous` agent degrades to `execute_with_approval` rather
than stopping, so out-of-hours work is queued for a human instead of lost.

## What is not here yet

Tracked in [TODO_DWF.md](../TODO_DWF.md): the OPA/cedar-go adapter behind the decision point (A3),
SPIFFE and RFC 8693 credential adapters (K4), the OpenTelemetry GenAI mapping (V4), and keel's
outbound `Notifier` delivery (H5 ships the port; keel owns the transport).
