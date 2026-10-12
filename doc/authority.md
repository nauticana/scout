# Principals, scopes, and effective releases

`service/principal` and `service/scope` supply the two primitives every governed decision needs: who
is acting, and what their configuration actually is. Both are agent-platform mechanism — the
organizational meaning attached to them (units, positions, human counterparts) belongs to the
consuming product, not to Scout.

## Principal

`domain.Principal` is the acting subject of one governed operation: kind, id, tenant, scope, the
release it is pinned to, its entitlements digest, and an authority chain. **The zero value is never
authorized.** Every enforcement point rejects it rather than treating an absent principal as a
tenant-wide one.

`Principal` travels with the work: `TurnRequest` → `StepInput` → `ToolCall` → `KnowledgeQuery`, and
its `PrincipalRef` is attributed on `Observation`, `SafetyEvent`, and `GuardrailSubject`. A tool call
whose principal tenant differs from the call tenant is `domain.ErrForbidden`.

A person's tool call in a turn they [operate](governance.md#live-turn-operation) keeps the turn's pinned
acting principal as `ToolCall.Principal` and names the person in `ToolCall.Operator`, a human of the
call's tenant (otherwise `ErrForbidden`). Its authority is the intersection of the tools the pinned
release binds, the turn principal's bindings and authority chain, policy decided as both that principal
and the person, and the release's guardrails. The person's own credential is resolved for the call,
`OnBehalfOf` stays the turn's end user, and the gateway's credential, invocation, and guardrail records
name the person. The agent's operating mode governs only the agent.

`AuthorityChain` is the delegation path in the shape of the RFC 8693 `act` claim: the immediate
delegator first, the original authority last. Each hop carries a grant id, grantor, remaining depth,
budget bound, and validity window — references and bounds, never a credential. `ChainVerifier` checks
shape, configured depth, per-grant depth, and validity, and resolves each hop against its stored
`delegation_grant` so a revoked or rewritten grant cannot keep flowing through a presented chain.
Whether a grantor still holds what it conveys is `GrantAuthorizer`'s check — see
[doc/organization.md](organization.md).

## Authorization

Agents and humans evaluate through **one** authorization model. Keel's `authorization_object`,
`authorization_object_action`, `authorization_role`, and `authorization_role_permission` are
subject-agnostic, and `low_limit`/`high_limit` already express bounded authority. Only the subject
side differs, so Scout adds `agent_permission` — the mirror of keel's `user_permission`, including
`begda`/`endda` effective dating — and `RoleAuthorizer` runs the same query against whichever
assignment table the principal kind selects.

The consequence matters more than the table: because both kinds resolve to the same objects, actions,
and limits, "a delegated grant may convey no more than its grantor holds" is a subset comparison in
one lattice rather than a translation between two models.

Scout's own objects are scoped by value. `AGENT_STUDIO` actions are checked at `agent:<id>` —
`CREATE` and `TEMPLATE` at `agent_type:<id>` — and `AGENT_CONVERSATION` `USE` and `PIN_RELEASE` at
`agent:<id>`, so a role holds every agent of its tenant with `*` or named agents only; Studio lists
only the agents its actor may view. `AGENT_STUDIO` `APPROVE_PUBLISH`, deciding another person's
publish or restore review, is checked at `agent:<id>` too, and `TEMPLATE_PUBLISH`, sharing a
template with other tenants, at `template_audience:all` or `template_audience:tenants`; no role holds
it by default. `OVERRIDE_GATE`, publishing or restoring a draft its [evaluation gate](governance.md#evaluation-gate)
does not admit and removing or loosening that gate, is checked at `agent:<id>` and seeded to no role.
`REVIEW`, reading, grading, and dismissing an agent's sampled and low-rated production turns, is
checked at `agent:<id>` and seeded to `AGENT_ADMIN`. `AGENT_OPERATION` `VIEW` (an agent's live turns, their call
outcomes, and operator entries), `OPERATE` (holding, releasing, taking over, acting in, handing back, and
finishing them and looking up a call's outcome; only a person operates a turn), `PREEMPT` (holding a running
turn at once), and `RECONCILE` (attesting an unknown call outcome) are checked at `agent:<id>`; `AGENT_ADMIN` holds
all four and `AGENT_OPER` `VIEW` and `OPERATE`. An attestation also passes `contract.OperationSeparation`, which
fails closed and requires a second person for a high-risk call. No authoring action implies them, which keeps operating a running agent apart from changing it (see
[live turn operation](governance.md#live-turn-operation)). `AGENT_RESTRICTION` `VIEW` and `REPLACE_TENANT` govern the restriction
layers and are checked at the tenant-wide value `tenant`, as is `AGENT_AUDIT` `VIEW`, reading the tenant's
decision records. `AGENT_APPROVAL` `DECIDE` is checked at the
request's scope.

Agents are deliberately **not** rows in `user_account`. That table is a credential store —
`passtext`, `passdate`, `login_attempts`, `lock_time`, `twofa_*`, `single_device_session`, unique
email and phone — and an agent needs none of it. Sharing the table would subject machine identities
to password expiry, lockout sweeps, and 2FA enforcement, and would make "human or agent?" a column
value rather than a structural fact in the audit trail.

## Scope hierarchy

`config_scope` is a per-tenant tree whose `scope_kind_code` is **opaque to Scout**: the product names its
own levels, and Scout owns only the `tenant` root kind. `config_scope_binding` attaches one versioned,
effective-dated resource value to a scope. One binding table serves every resource kind, so adding a
level or a kind never adds a table.

Binding values are canonical JSON, fixed per kind:

| Kind | Value | Merger | Narrowing rule |
|---|---|---|---|
| `prompt_section` | `{"instruction":"…","output":"…"}` | `PromptMerger` | `text` |
| `tool`, `skill`, `knowledge`, `model`, `entitlement` | `["a","b"]` | `SetMerger` | `subset` |
| `policy` | `[{"id":"…","effect":"allow","actions":[…],"resources":[…]}]` | `PolicyMerger` | `policy` |
| `budget` | `{"tokens":0,"cost_minor_units":50000,"currency":"EUR","exponent":2,"period":"day","timezone":"Europe/Berlin"}` | `BudgetMerger` | `at_most` |
| `autonomy` | `{"mode":"draft","window_from_minute":0,"window_to_minute":0}` | `AutonomyMerger` | `ordered` |
| `limits` | `{"turns_per_minute":0,"max_steps":0,"max_tool_calls":0,"max_duration_seconds":0}` | `LimitsMerger` | `at_most` |
| `approval_routing` | `{"routes":[{"id":"…","match":{…},"approver":{"kind":"role","ref":"…"},"deadline_seconds":3600,"on_exhausted":"expire"}]}` | `RoutingMerger` | `routing` |
| `evaluation_gate` | `{"metric":"rubric_score","min_score_bp":8000,"max_regression_bp":200,"min_samples":20,"required_scopes":["dev","gate"]}` | `EvaluationGateMerger` | `gate` |
| `health_alerts` | `{"window_minutes":60,"min_turns":20,"max_failure_rate_bp":500,"max_p95_latency_ms":30000,"max_hard_safety_events":0,"max_silence_minutes":120,"max_unresolved_minutes":30}` | `HealthAlertsMerger` | `alerts` |

A set cannot express deny-wins, so `policy` has its own merger and rule: a child may drop an allow or
add a deny, never add an allow, and an allow it keeps may not widen the parent's actions, resources,
or conditions, nor drop an obligation the parent attached. `autonomy` narrows on two axes — the mode
rank and the bounded operating window, which may only shrink; a child window that misses the parent's
entirely degrades the mode to `execute_with_approval` rather than granting an empty one.
`limits` narrows per field, with 0 meaning unlimited at that scope: a child may lower a ceiling its
parent sets, never raise or unset it, and may set one its parent leaves at 0. `budget` narrows only
over the parent's period, window, timezone, and currency unit (see
[release budgets](governance.md#release-budgets)). `approval_routing` overlays its parent route by
route and may only tighten each inherited route (see [approval routing](governance.md#approval-routing)).
`evaluation_gate` keeps the inherited metric and may only raise `min_score_bp` and `min_samples`, lower
`max_regression_bp` (never unset it once a parent sets it), and add `required_scopes`.
`health_alerts` binds by replace only and may only tighten an alert policy: a child keeps the
window once a parent sets one, may lower a threshold, the sample floor `min_turns`, or the
silence or unresolved-outcome span its parent sets, never raise or unset it, and may set one its parent leaves at 0
(see [agent health](observability.md#agent-health-and-alerts)). `evaluation_gate` and
`health_alerts` are operational settings: the gate governs publication and health alerts govern
alerting, not runtime behavior, so publication and draft freezes drop both
(`scope.WithoutOperationalSettings`) and no release freezes either (see
[evaluation gate](governance.md#evaluation-gate)).

`config_resource_kind.narrowing_rule` is the seeded source of that last column; `PlatformNarrowingRules`
mirrors it in code. An unregistered kind fails compilation and an unmapped rule fails the check —
both closed, never permissive. Set, budget, autonomy, limits, approval routing, evaluation gate, and health alerts
bindings use their kind as `resource_id`, while policy may use several ids.

A release's `limits` only lower the tenant runtime policy and the executor's flag ceilings.
`turns_per_minute` caps the agent's turns opened in any trailing minute, by the database clock, when
a turn opens (`domain.ErrRateLimited`, HTTP 429 with `Retry-After`); a replayed request is never
refused. `max_steps` and `max_duration_seconds` lower the tenant runtime policy's step count and turn
timeout, and `max_tool_calls` and `max_duration_seconds` the tool loop's call ceiling and step
deadline; exceeding one is `domain.ErrExecutionLimit`. A value above a ceiling leaves it unchanged.

## Compilation

`Compiler` folds a scope chain, widest scope first, into one `domain.EffectiveRelease`. It runs at
**publication**: `AgentPublisher` freezes the result, and the runtime pins it instead of resolving
inheritance per request.

For each resource, per binding in chain order:

1. If a previous binding was sealed, reject with `domain.ErrSealed`.
2. Merge the binding onto what was inherited, using the merger for its kind.
3. Check the **merged result** against what was inherited. Checking the result rather than the
   candidate is what makes one rule cover every merge mode: a replace (`R`) that grants more and an
   append (`A`) that widens a set both fail the same subset comparison.
4. Record provenance; the previous winner moves onto `Superseded`.

`sealed` is set by the binding's own scope, so a company safety clause genuinely cannot be
overridden — unlike a child-controlled override flag. `BudgetMerger`, `AutonomyMerger`,
`LimitsMerger`, `EvaluationGateMerger`, and `HealthAlertsMerger` clamp to the tighter inherited value, so a raised ceiling is silently impossible as
well as explicitly checked.

Compilation is deterministic: resources sort by `(kind, id)`, bindings by scope depth, and `Digest`
covers every effective value with its winning scope and version. The same inputs in any order produce
the same digest, which is what makes the frozen release comparable across environments.

Every effective resource carries the provenance of the binding that won **and** of each binding it
superseded, so an explain view answers "why is this the value?" without recompiling.

## Limits

| Flag | Default | Meaning |
|---|---:|---|
| `agent_max_scope_depth` | 8 | Longest scope chain a release may compile over |
| `agent_max_delegation_hops` | 4 | Longest authority chain a principal may present |

`TableScopeRepository.Chain` additionally refuses a parent cycle and caps its walk, so a malformed
hierarchy fails loudly instead of looping.

## What is not here yet

The compiler is the generic half. Prompt inheritance runs on it too: tenant defaults and agent
prompts are `prompt_section` bindings, folded by `controlplane.PromptCompiler` over the product's
baseline, so `sealed` governs a prompt clause exactly as it governs a policy. Everything that
consumes these primitives — the policy decision point, durable approvals, credential bindings,
scope-keyed budgets, delegation — is in [doc/governance.md](governance.md) and
[doc/organization.md](organization.md).
