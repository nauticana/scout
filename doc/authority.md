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
| `tool`, `knowledge`, `model`, `entitlement` | `["a","b"]` | `SetMerger` | `subset` |
| `policy` | `[{"id":"…","effect":"allow","actions":[…],"resources":[…]}]` | `PolicyMerger` | `policy` |
| `budget` | `{"tokens":0,"cost_minor_units":0,"currency":"EUR"}` | `BudgetMerger` | `at_most` |
| `autonomy` | `{"mode":"draft","window_from_minute":0,"window_to_minute":0}` | `AutonomyMerger` | `ordered` |

A set cannot express deny-wins, so `policy` has its own merger and rule: a child may drop an allow or
add a deny, never add an allow, and an allow it keeps may not widen the parent's actions, resources,
or conditions, nor drop an obligation the parent attached. `autonomy` narrows on two axes — the mode
rank and the bounded operating window, which may only shrink; a child window that misses the parent's
entirely degrades the mode to `execute_with_approval` rather than granting an empty one.

`config_resource_kind.narrowing_rule` is the seeded source of that last column; `PlatformNarrowingRules`
mirrors it in code. An unregistered kind fails compilation and an unmapped rule fails the check —
both closed, never permissive.

## Compilation

`Compiler` folds a scope chain, widest scope first, into one `domain.EffectiveRelease`. It runs at
**publication**: `AgentPublisher` freezes the result, and the runtime pins it instead of resolving
inheritance per request.

For each resource, per binding in chain order:

1. If a previous binding was sealed, reject with `domain.ErrSealed`.
2. Merge the binding onto what was inherited, using the merger for its kind.
3. Check the **merged result** against what was inherited. Checking the result rather than the
   candidate is what makes one rule cover every merge mode: a `replace` that grants more and an
   `append` that widens a set both fail the same subset comparison.
4. Record provenance; the previous winner moves onto `Superseded`.

`sealed` is set by the binding's own scope, so a company safety clause genuinely cannot be
overridden — unlike a child-controlled override flag. `BudgetMerger` and `AutonomyMerger` clamp to
the tighter inherited value, so a raised ceiling is silently impossible as well as explicitly checked.

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

The compiler is the generic half. Still open, tracked in [TODO_DWF.md](../TODO_DWF.md): prompt
inheritance still uses its own three tables rather than `config_scope_binding` (C5). Everything that
consumes these primitives — the policy decision point, durable approvals, credential bindings,
scope-keyed budgets, delegation — is in [doc/governance.md](governance.md) and
[doc/organization.md](organization.md).
