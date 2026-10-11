# Agent types, lifecycle, and delegation

[doc/authority.md](authority.md) covers who is acting and what their configuration is;
[doc/governance.md](governance.md) covers whether an action is allowed and what is written down. This
covers where an agent comes from, what state it is in, and how it may hand work to another agent.

## Agent types

`agent_type` is a reusable behaviour template; `agent_type_version` is an immutable published one.
`agent_profile.agent_type_id` is a foreign key to `agent_type`; Go and Studio use the matching
`agent_type` name.

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

`agent_profile.state_code` over the `agent_state` catalog holds an agent's state, with
`state_reason`, `state_changed_by`, and `state_changed_at` recording why, by whom, and when.

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

## Governance and triggers

An agent carries the governance fields of a Charter `AgentDefinition`, in Charter's names and shapes:
`purpose`, `accountable` (a `Position` or `OrganizationUnit`), `responsibilityIds`, `capabilityIds`,
`triggers`, `requiredInputs`, `expectedOutcomes`, `policies`, `confidenceBoundaries`, and
`escalationConditions`. `controlplane.StudioGovernance` keeps Studio's draft of them in
`agent_governance` behind the draft revision and, as a definition section, freezes the effective
governance into every release under `governance`, covered by the definition digest.

Studio declares triggers as `triggerChannels`: a trigger text and the channel it arrives on —
`conversation`, `mcp`, `api`, `schedule`, or `event`. Charter's `triggers` list is derived from them,
never edited. A `schedule` or `event` trigger names the product-owned schedule or topic it binds,
which the product's `contract.TriggerBindingVerifier` must prove on save and again on every publish
and restore; with no verifier composed, such a trigger is refused. Channels are declarations: the
transports that deliver work are product wiring.

An enterprise may own the fields instead. Every field a `contract.GovernanceSource` snapshot sets is
read-only in Studio and never stored by Scout, and the release freezes the snapshot's document,
version, and digest. An unusable snapshot or a source error fails the call.

As a publish guard the same service refuses a release whose governance is malformed, binds a schedule
or topic the verifier does not prove, lacks a field the active Charter profile requires
(`agent_governance_required_fields`: empty requires all ten, `none` none), or does not carry the
source's current snapshot, so a restore cannot reactivate superseded enterprise governance. Restoring
a release frozen before the agent had governance skips the required fields.

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

Tracked in [TODO.md](../TODO.md): the A2A adapter behind `AgentInvoker` (D5). In process,
`dataplane.TurnAgentInvoker` runs a delegated call as the delegate's own turn, so each hop is
admitted, reserved, and settled under its own principal within the budget passed down.
