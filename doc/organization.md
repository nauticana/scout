# Agent types, lifecycle, and delegation

[doc/authority.md](authority.md) covers who is acting and what their configuration is;
[doc/governance.md](governance.md) covers whether an action is allowed and what is written down. This
covers where an agent comes from, what state it is in, and how it may hand work to another agent.

## Agent types

`agent_type` is a reusable behaviour template; `agent_type_version` is an immutable published one.
`agent_profile.agent_type_id` is now a real foreign key — it was a bare `VARCHAR` referencing nothing.
The whole codebase says `agent_type_id` where it used to say `agent_kind`, which also aligns Go with
the `agent_type` field the studio-v1 wire format already used.

`agent_capability_package` is a named, versioned bundle of resource values. A type version requires packages
through `agent_type_capability`, and `AgentTypeStore.Instantiate` expands them into `config_scope_binding`
rows at the instance scope. Packages are not a second binding mechanism — they are a way of writing
the same bindings once.

Instantiation is where the type's authority becomes the instance's ceiling: the overlay is
narrowing-checked against the expanded package values, so **an instance can never be born broader
than its type**.

`Conformance` reports instances pinned to an older type version, with the packages they lack. It
never upgrades one. A running instance's pinned version is its contract, and silently re-pointing it
would change what already-approved work is allowed to do.

## Lifecycle

`agent_profile.is_active` — a boolean with no reason, actor, or timestamp — is now `state_code` over
the `agent_state` catalog, with `state_reason`, `state_changed_by`, and `state_changed_at`.

```text
draft ──→ active ──→ draining ──→ retired
  │         ↕            ↑
  │      suspended ──────┘
  └──────────────────────→ retired
```

`draining` finishes in-flight work and admits none; `suspended` stops new work immediately and is
reversible; `retired` is terminal. `Transition` refuses an illegal move with `domain.ErrConflict`,
compare-and-swaps on the current state so a concurrent change cannot be lost, and emits a decision
record with reason and actor.

Studio's enabled toggle maps to active/suspended only. Draining and retirement are operational
transitions, not authoring ones.

`agent_version_quarantine` withdraws **one version** from all traffic. It overrides every pin and
deployment pointer instead of editing them, so quarantining does not disturb the rollout state a team
will want back after the incident.

## Entitlements

`knowledge.ReleaseEntitlements` derives retrieval labels from the effective release rather than
trusting the request path. Compilation already checked the labels are a subset of every parent
scope's, so a caller cannot widen them by asserting more, and an agent whose release binds no
entitlements retrieves nothing.

## Delegation

`delegation_grant` is the typed bound: grantor, grantee, action scope, max depth, budget, whether
approval is required, and a validity window. `GrantAuthorizer.Authorize` checks the grant is in
force, covers the action, and — the part that matters — that **the grantor still holds what it is
passing on**, evaluated through the same authorization objects both principal kinds use. A revoked
role stops flowing through an older grant immediately.

`principal.Narrow` computes what one hop passes to the next. Every field may only shrink:

| Bound | Rule |
| --- | --- |
| Depth | decrements each hop; zero refuses the delegation |
| Budget | the tighter of parent and child |
| Scope | may not move; a hop cannot relocate work |
| Currency | may not change |
| Approval | sticky — once required, it stays required |

`agent_work_item` addresses work to a principal rather than to a conversation, so an agent can be assigned
work it did not start. It is unique on `(tenant, request_id)`, so a redelivered delegation re-attaches
instead of fanning out twice.

`dataplane.AgentStepExecutor` is the `agent` graph step. It authorizes the grant, narrows the bounds,
refuses a cycle, records the work item, and hands the call to an injected `contract.AgentInvoker` —
so an in-process runtime and an A2A client compose identically while Scout keeps the authority.

Cycle detection walks the work-item ancestry and refuses a target already in the chain, including
self-delegation. Without it a mutual delegation would burn budget until a ceiling stopped it, long
after the loop was obvious.

## Explaining a release

`scope.Explainer` answers "why is this the value?" from the provenance frozen at publication — the
losing candidates were kept precisely so the answer survives a later binding change. `Diff` compares
two compiled releases of one agent, classifying each resource as added, removed, or modified.

## What is not here yet

Tracked in [TODO_DWF.md](../TODO_DWF.md): the A2A adapter behind `AgentInvoker` (D5), and the
prompt retrofit onto `config_scope_binding` (C5), which is the last item keeping two inheritance
mechanisms in the tree.
