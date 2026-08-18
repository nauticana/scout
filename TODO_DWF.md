# Scout — Digital Work Force work plan

Task list closing the gaps in [IDEAS.DWF.md](IDEAS.DWF.md), so Scout can carry the Enterprise Agent Organization Control Plane (idea-12) as **shared building blocks**. Nothing here implements the product: no organizational unit, position, HR semantic, policy pack or Sail screen. Those stay in the commercial application per idea-12 §12.

Severity: **HIGH** = blocks the idea-12 §15 MVP, **MED** = required before a first customer, **LOW** = required at scale. Status: ✅ done, 🚧 in progress, ⏳ open, ⛔ won't do.

**Waves 1, 2, and 3 shipped on 2026-08-17**, except C5 and the four external adapters (A3, K4, V4, D5). See [doc/authority.md](doc/authority.md), [doc/governance.md](doc/governance.md), and [doc/organization.md](doc/organization.md) for the design and `migration_guide.json` for the old→new mapping. There are no production downstreams, so every break is clean: no shims, no data migrations.

Conventions this plan inherits (global engineering rules + [TODO.md](TODO.md)): interfaces in `contract/`, values in `domain/`, implementations in `service/<area>/`, compile-time assertions for every implementation, named-SQL consts with one package-level map and a cached `QueryService`, `BIGINT` PK + explicit `CREATE SEQUENCE` + `table_sequence_usage` (never `BIGSERIAL`), every implied parent→child relationship a real declared FK, integer minor units for money with a currency-derived exponent, injected `Now func() time.Time`, every limit a documented flag in `scout_config.go`, and clean breaks with the old→new mapping recorded in `migration_guide.json`.

---

## Status overview

| ID | Title | Idea | Wave | Severity | Status |
|---|---|---|---|---|---|
| P1 | `domain.Principal` and authority chain | DWF-1 | 1 | **HIGH** | ✅ done |
| P2 | Thread the principal through every governed contract | DWF-1 | 1 | **HIGH** | ✅ done |
| P3 | `agent_permission` + principal-aware keel RBAC evaluation | DWF-1a | 1 | **HIGH** | ✅ done — Scout side; the keel signature change is P3a |
| P4 | `PrincipalResolver` + optional external principal source | DWF-1 | 1 | MED | ✅ done — port and table resolver; adapters are K4 |
| C1 | `scope` + `config_scope_binding` schema | DWF-2 | 1 | **HIGH** | ✅ done |
| C2 | `EffectiveConfigCompiler` with per-kind mergers | DWF-2 | 1 | **HIGH** | ✅ done |
| C3 | `NarrowingChecker` — monotonic restriction at publish | DWF-2 | 1 | **HIGH** | ✅ done |
| C4 | `domain.Provenance` + `effective_agent_release` | DWF-2 | 1 | **HIGH** | ✅ done |
| C5 | Retrofit prompt inheritance onto the compiler (clean break) | DWF-2 | 1 | MED | ⏳ open — the only wave-1 item left |
| P3a | Principal-aware `CheckPermission` signature in keel | DWF-1a | 1 | **HIGH** | ⏳ open — keel release |
| A1 | `PolicyDecisionPoint` port + native evaluator | DWF-5 | 2 | **HIGH** | ✅ done |
| A2 | Obligations consumed at the tool boundary | DWF-5 | 2 | **HIGH** | ✅ done |
| A3 | OPA / cedar-go adapter behind the port | DWF-5 | 2 | LOW | ⏳ open — port exists; adopt on demand |
| H1 | `suspended` turn lifecycle: suspend, resume, replay | DWF-3 | 2 | **HIGH** | ✅ done |
| H2 | `approval_request` + decision record | DWF-3 | 2 | **HIGH** | ✅ done |
| H3 | `ApprovalInbox` read port | DWF-3 | 2 | **HIGH** | ✅ done |
| H4 | Deadline, escalation, backup and absence routing | DWF-3 | 2 | MED | ✅ done |
| H5 | `Notifier` port over keel messaging | DWF-12 | 2 | MED | ✅ done — port and gate wiring; keel owns delivery |
| K1 | `tool_credential_binding` at principal scope | DWF-4 | 2 | **HIGH** | ✅ done |
| K2 | Principal-scoped credential port + JIT resolution | DWF-4 | 2 | **HIGH** | ✅ done |
| K3 | Delegated-authority record and revocation signal | DWF-4 | 2 | MED | ✅ done |
| K4 | SPIFFE / RFC 8693 adapters behind the port | DWF-4 | 2 | LOW | ⏳ open — port exists; adopt on demand |
| B1 | Scope-keyed budget, quota and concurrency policy | DWF-6 | 2 | **HIGH** | ✅ done — budget and autonomy; per-principal concurrency is B1a |
| B2 | Principal + scope dimensions on the usage ledger | DWF-6 | 2 | **HIGH** | ✅ done |
| B3 | Autonomy-mode time windows and schedules | DWF-6 | 2 | MED | ✅ done |
| V1 | `domain.DecisionRecord` replacing `AuditEvent` | DWF-7 | 2 | **HIGH** | ✅ done |
| V2 | `AuditQuery` read port | DWF-7 | 2 | **HIGH** | ✅ done |
| V3 | Emit decision records at every governed boundary | DWF-7 | 2 | **HIGH** | ✅ done |
| V4 | OTel GenAI semconv mapping, version-pinned | DWF-7 | 2 | LOW | ⏳ open — conventions still Development-stability |
| B1a | Per-principal concurrency ceiling on `TenantWeightPolicy` | DWF-6 | 2 | MED | ⏳ open |
| T1 | `agent_type` + `agent_type_version` | DWF-8 | 3 | MED | ✅ done |
| T2 | `agent_capability_package` | DWF-8 | 3 | MED | ✅ done |
| T3 | Typed capability/tool/knowledge/policy sets on `AgentDefinition` | DWF-8 | 3 | MED | ✅ done — as a pinned type ref plus packages; the sets stay in the effective release |
| T4 | Instantiate-from-type + conformance check | DWF-8 | 3 | MED | ✅ done |
| L1 | Audited agent state machine | DWF-10 | 3 | MED | ✅ done |
| L2 | Per-agent-version quarantine | DWF-10 | 3 | MED | ✅ done |
| L3 | `begda`/`endda` on every scoped binding and grant | DWF-10 | 3 | MED | ✅ done |
| E1 | `EntitlementResolver` + publish-time subset enforcement | DWF-11 | 3 | MED | ✅ done |
| D1 | `agent` execution step kind | DWF-9 | 3 | MED | ✅ done |
| D2 | `delegation_grant` with typed bounds | DWF-9 | 3 | MED | ✅ done |
| D3 | Work item addressed to a principal | DWF-9 | 3 | MED | ✅ done |
| D4 | Depth, budget and scope propagation + cycle detection | DWF-9 | 3 | MED | ✅ done |
| D5 | A2A adapter for cross-process delegation | DWF-9 | 3 | LOW | ⏳ open — `AgentInvoker` is the port; adopt on demand |
| X1 | Effective-configuration explain read model | DWF-13 | 3 | MED | ✅ done |
| X2 | Effective-release diff | DWF-13 | 3 | LOW | ✅ done |
| — | `organization_unit` / `position` / HR semantics in Scout | — | — | — | ⛔ won't do — commercial app |
| — | Temporal or another durable-execution platform as the runtime | — | — | — | ⛔ won't do — Scout owns durable turns; see H1 |
| — | SpiceDB as a Scout dependency | — | — | — | ⛔ won't do now — see E1 conditions |
| — | Agents as rows in `user_account` | — | — | — | ⛔ won't do — decided in IDEAS.DWF.md §DWF-1a |

---

## Wave 1 — the spine

Both items break contracts widely. Do them in one release, one `migration_guide.json` entry, and land them before anything in wave 2 multiplies the call sites.

### P1 — `domain.Principal` and authority chain (HIGH)

- [x] `domain/principal.go`: `PrincipalKind` (`agent`, `human`, `service`), `Principal{Kind, ID, TenantID, ScopeID, Release, EntitlementsDigest, Authority}`. `Release` was added beyond the plan: the governed gateway needs the pinned release to resolve bindings, and carrying it on the principal keeps one carrier instead of widening `ToolCall` twice.
- [x] `AuthorityChain []AuthorityHop` — each hop carries the delegating principal, the `delegation_grant` id, the bound it conveys, and its validity window. Ordered outermost-first, matching the RFC 8693 `act` claim so an external identity plane maps rather than translates.
- [x] Depth and bound checks live in `principal.ChainVerifier`, not on the value — `domain/` holds values only, no methods. A chain is verified before it acts rather than at construction.
- [x] Sentinels in `domain/errors.go`: `ErrPrincipalUnknown`, `ErrAuthorityExceeded`, `ErrDelegationDepth`, `ErrGrantExpired`, plus `ErrSealed` for an override of a sealed binding.
- [x] The zero `Principal` is never authorized — fail closed, with a test that asserts it.
- [x] Never serialize a credential into the chain; it carries references and bounds only.

### P2 — Thread the principal through every governed contract (HIGH, breaking)

- [x] `domain.ToolCall` gains `Principal` — today it is `{TenantContext, RequestID, ConversationID, ToolID, ToolVersion, Arguments}` and the governed gateway cannot tell which agent is calling.
- [x] `domain.TurnRequest` gains `Principal` and `OnBehalfOf`; `TurnDispatch` carries them through its `Turn`.
- [ ] `StepInput` and `StepResult` still reach a step executor without the principal — they carry `Step` and `Snapshot` only. Needed by D1, when a step can invoke another agent.
- [x] `domain.KnowledgeQuery.Principal string` becomes `domain.Principal`; entitlements stop being caller-asserted (see E1).
- [x] `domain.Observation`, `domain.SafetyEvent`, and `domain.GuardrailSubject` gain principal attribution.
- [ ] `domain.ModelRequest` deliberately does **not**: it crosses into provider adapters, and principal identity has no business leaving the platform. Attribution happens on the `Observation` the call produces.
- [x] `service/toolgateway/governed_gateway.go`: authorize against `agent_tool_binding` for the calling principal **at invoke time**. Today the binding exists in schema but is only consulted at publication; `ToolRegistry.Get` is keyed on `(tenant, tool, version)` alone.
- [x] `TenantContext` gains `ScopeID` so org placement reaches routing, retrieval, budgets and the usage ledger.
- [x] Fakes in `internal/fake/principal.go` and every affected test updated; no `…WithPrincipal` overload kept alongside the old signature.
- [x] Tests: a call with a zero principal is rejected; a principal without the tool binding is rejected even when the tenant holds it; an expired authority hop is rejected mid-turn.

### P3 — `agent_permission` and principal-aware RBAC (HIGH, keel)

Decision and rationale: [IDEAS.DWF.md §DWF-1a](IDEAS.DWF.md). The authorization engine is untouched; only the subject side is new.

- [x] `schema/agent_authorization/agent_permission.yml`: `(tenant_id, agent_id, role_id, begda, endda, granted_by)`, PK `(tenant_id, agent_id, role_id, begda)`, FKs to `agent_profile` and `authorization_role`, mirroring `user_permission` including effective dating. Composite child table — no sequence. It lives in Scout, not keel, because it references `agent_profile`, which keel cannot see.
- [x] Second named query beside `QCheckAuthorization`, joining `agent_permission`, identical in `low_limit` / `high_limit` / `bypass_scope` handling so the semantics stay one implementation.
- [ ] **P3a, keel release.** `Principal{Kind, ID}` replaces the bare `userID` on `AbstractRepository.CheckActionPermission`, `AbstractTableService.CheckPermission`, `RestService.GetPermission` and the table-action middleware. Clean break — no `…ForUser` wrapper. Scout evaluates agents through `principal.RoleAuthorizer` until then, which runs the same grant query against `agent_permission`.
- [ ] Where a column records "who did this" and either kind is possible: two nullable FK columns with an XOR check constraint, never a polymorphic pair. No such column exists yet — the pattern lands with H2 and V1.
- [ ] Agent authority never exceeds the delegating human's effective permission set. The model now makes this a set comparison over one lattice; the comparison itself lands with `delegation_grant` (D2).
- [x] Scout's existing `user_account` FKs (`agent_version.published_by`, `agent_alias.modified_by`, `agent_studio_event.actor_id`) stay human-only; an agent that publishes does so under a recorded delegation grant.
- [ ] keel migration guide entry for P3a: the old→new signature mapping, generically, naming no downstream.

### P4 — `PrincipalResolver` and external sources (MED)

- [x] `contract.PrincipalResolver`: transport credential → verified `Principal`, failing closed.
- [x] `contract.ExternalPrincipalSource` (optional): lets Entra Agent ID, Okta or AWS Agent Registry be the identity authority without Scout depending on any of them — idea-12 §12 option C.
- [x] Reference implementation over keel sessions and `agent_permission`; the external adapters are K4.
- [ ] Cache resolved principals with an explicit TTL and a revocation invalidation path. Resolution is one indexed read today; add the cache when a profile shows it matters, not before.

### C1 — `scope` and `config_scope_binding` schema (HIGH)

- [x] `schema/configuration/scope.yml`: `(tenant_id, scope_id, parent_scope_id, config_scope_kind, display_name)`. `config_scope_kind` is an **opaque catalog code** the product names (company, division, plant, team); Scout never interprets it.
- [x] Self-FK for `parent_scope_id`; a `scope_parent_ck` constraint rejects self-parenting, `TableScopeRepository.Chain` refuses a longer cycle and caps its walk, and `Compiler.MaxDepth` bounds the chain from `--agent_max_scope_depth`. The cycle check is on read rather than on write: a write-time check needs a recursive query, and the read is where a cycle would actually do damage.
- [x] `schema/configuration/config_scope_binding.yml`: `(tenant_id, scope_id, resource_kind_code, resource_id, resource_version, merge_mode_code, sealed, resource_value, begda, endda, bound_by)`. One table serves policy, tool, knowledge, model, budget and prompt bindings.
- [x] `config_resource_kind` and `config_merge_mode` (`replace`, `append`, `intersect`) as catalog tables — FK targets, seeded.
- [x] `sealed BOOLEAN` is set by the **parent** binding, not the child, so a company safety clause is genuinely non-overridable. This is the fix for `agent_prompt_override.overwrite`, which the child controls today.
- [x] `schema/dependency.yml`: new `scope` module, `depends_on: [tenancy]`; every downstream module that binds resources depends on it.

### C2 — `EffectiveConfigCompiler` (HIGH)

- [x] `contract.ScopeResolver`: principal → ordered scope chain (platform → tenant → scope ancestry → type → instance).
- [x] `contract.ResourceMerger` per `config_resource_kind`; a registry resolves them, exactly as `StepExecutorRegistry` resolves step kinds.
- [x] `contract.EffectiveConfigCompiler`: chain + bindings → frozen artefact with a digest. Deterministic — same inputs, byte-identical output, asserted by test.
- [x] Compilation happens at **publish**, never at request time. The runtime pins a compiled release and never walks the scope chain on the hot path.
- [x] `service/controlplane/effective_compiler.go`, composed into the existing `AgentPublisher` alongside `AgentCompiler`.
- [x] `scope.Digest` reuses the length-prefixed field encoding from `prompt_compiler.go`, so digests stay one convention.

### C3 — `NarrowingChecker` (HIGH — the security-relevant half)

- [x] `contract.NarrowingChecker`: per-kind lattice comparator answering "does this child binding broaden the parent?".
- [x] Comparators for: model set (⊆ of `tenant_model_access`), tool set (⊆), knowledge/entitlement labels (⊆), budgets and quotas (≤), autonomy mode (ordered: human-only < advise < draft < execute-with-approval < bounded-autonomous), permission grants (⊆ over `authorization_object` + action + `low_limit`/`high_limit` range).
- [x] Publication **fails** on a broadening override. A genuine broadening is a separately authorized policy change against the parent scope, recorded as its own decision record — never an ordinary override.
- [x] A `sealed` parent value cannot be overridden at all, in any direction.
- [x] Table-driven tests per comparator, including the range case: a child `low_limit`/`high_limit` outside the parent's range is a broadening.

### C4 — Provenance and `effective_agent_release` (HIGH)

- [x] `domain.Provenance{ScopeID, ScopeKind, BindingID, ResourceVersion, Approver, CompiledAt, Rule}` retained **per effective field**, inside the frozen artefact — not in a parallel array as `AgentDefinition.Sources` does today.
- [x] `schema/configuration/effective_agent_release.yml`: the immutable compiled result, digest-addressed, keyed 1:1 on `agent_version`. A conversation already pins its agent version through `conversation_release`, so it transitively pins the effective release; no new pin column was needed.
- [x] Losing candidates retained alongside winners so X1 can explain *why* a value won without recompiling.
- [ ] Adopt W3C PROV naming for the provenance fields where a term exists; auditors read that vocabulary. Deferred to V1, so the decision-record and provenance vocabularies land together.

### C5 — Retrofit prompts onto the compiler (MED, breaking)

- [ ] `prompt_baseline`, `tenant_prompt_default` and `agent_prompt_override` become bindings of `config_resource_kind = prompt_section` over `config_scope_binding`; `PromptSourceLevel`'s closed three-value enum is deleted.
- [ ] `CompiledPromptSection` carries its `Provenance` so the frozen section is self-describing.
- [ ] No data migration: there are no production downstreams, so the old tables are dropped and re-seeded rather than converted.
- [ ] **No compatibility shim.** Two inheritance mechanisms in one codebase is exactly the drift the shared-library rule forbids — and one is what ships today, so this is the item that closes wave 1.

---

## Wave 2 — governance

This wave is what makes the idea-12 §15 MVP demonstrable end to end.

### A1 — `PolicyDecisionPoint` port (HIGH)

- [x] `contract.PolicyDecisionPoint`: `Decide(ctx, principal, action, resource, environment) → domain.Decision`.
- [x] `domain.Decision{Allow, Obligations, PolicyID, PolicyVersion, Reason, EvaluatedAt}` — the reason is auditable text, not a log line.
- [x] `policy` becomes a `config_resource_kind` in C1 so it binds at any scope and inherits monotonically.
- [x] Native evaluator in `service/policy/` over the compiled policy set; ship this first so no external engine is on the critical path.
- [x] Fail closed on evaluator error, expiry or stale policy — reuse the `ErrStaleEvidence` posture already used by the gate evaluator.

### A2 — Obligations at the tool boundary (HIGH)

- [x] `domain.Obligation` kinds: `require_approval` (→ H1/H2), `redact` (→ guardrails), `cap_spend` (→ B1), `record_evidence` (→ V1), `notify` (→ H5).
- [x] `GovernedGateway` consumes obligations before egress, in the documented order: policy → guardrail → egress → admission → credential.
- [x] An unrecognized obligation is a **hard failure**, never a silently ignored one.
- [x] Idea-12 §2's operating modes are enforced here, as obligations — never in a prompt. `§16: prompts are never security controls`.

### A3 — External policy adapter (LOW)

- [ ] `service/policy/opa_evaluator.go` behind the port (Go-embeddable, CNCF). Watch the 2026 Styra tooling transition; the engine itself stays CNCF-governed.
- [ ] Evaluate cedar-go as the alternative — embeddable-only and statically analysable, which pairs well with C3 if provability becomes a customer requirement.
- [x] Canonical policy state stays in Scout: `policy` is a `config_scope_binding` resource kind compiled into the effective release, so an external engine can only ever evaluate, never own.

### H1 — Suspended turn lifecycle (HIGH)

Today a pending approval wraps `ErrForbidden` and **fails the turn** ([service/guardrail/layered_enforcer.go:30-31](service/guardrail/layered_enforcer.go#L30-L31)).

- [x] `turn_status` seed gains `suspended` (`is_terminal = false`).
- [x] `TurnRecordStore.Suspend(ctx, tenantID, requestID, reason, resumeAfter)` and `.Resume(ctx, tenantID, requestID, decision)`.
- [x] Budget reservation is **held**, not settled, across a suspension; the reservation TTL must not silently expire a legitimately waiting turn. Extend or re-reserve on resume, with a test for the expiry race.
- [x] Resume re-enters at the pending step through the existing `StepIdempotencyStore`, so no completed side effect re-executes — this is the guarantee LangGraph's `interrupt()` explicitly does not give.
- [x] The worker holds no lease and consumes no slot while suspended; the queue lease is released and reacquired on resume.
- [x] A suspended turn survives worker restart, rebalance and rollout drain.
- [x] `ErrApprovalPending` stops being a `ErrForbidden` and becomes a control-flow signal the runtime handles.
- [x] Crash/resume test with a real approval, matching idea-12 §15 item 8.

### H2 — `approval_request` and decision (HIGH)

- [x] `schema/approval/approval_request.yml`: `(id, tenant_id, request_id, execution_step_id, principal ref, approval_risk_tier, action_summary, evidence_ref, proposed_action_digest, deadline_at, status_code, created_at)`. `BIGINT` PK + `approval_request_seq` + `table_sequence_usage`.
- [x] `approval_decision`: verdict, deciding principal (two nullable FKs + XOR check per P3), reason, decided_at, and the authority the decider used.
- [x] `approval_status` catalog: `pending`, `approved`, `rejected`, `edited`, `expired`, `escalated`, `withdrawn`.
- [x] Evidence is a redacted object reference — never inline content, never a secret. Reuse `ObjectRef`.
- [x] The proposed action is digest-bound: approving a *changed* action is impossible.
- [x] Do **not** reuse `human_review_item` — it FKs `evaluation_run` and `golden_example` and is an evaluation labelling queue.

### H3 — `ApprovalInbox` read port (HIGH)

- [x] `contract.ApprovalInbox`: pending items for a principal, by scope, risk tier and deadline, with the evidence, proposed action and an unambiguous approve / reject / edit path (idea-12 §5).
- [x] Every actionable output declares its class — advice, draft, approval request, or completed action — as a typed field, so the UI cannot mislabel it.
- [x] Filtering by delegated authority: a person sees what they may actually decide, not everything in the tenant.

### H4 — Deadline, escalation and absence (MED)

- [x] `contract.EscalationPolicy`: timeout → backup approver → abandon, each transition recorded.
- [x] Durable timers survive restart. Temporal's model (durable timer + automatic escalation when a reviewer goes quiet) is the reference for correctness, not a dependency.
- [x] **Absence and reassignment** (idea-12 §5): when a counterpart loses authority, in-flight requests route to the configured backup or the agent stops. Never continue under orphaned delegation — the grant's `endda` is the trigger.
- [x] Escalation must not silently widen authority: the backup needs its own grant.

### H5 — `Notifier` port (MED, keel-owned)

- [x] Thin `contract.Notifier` in Scout, emitted from `approval.Gate`; delivery is keel messaging/outbox, per the horizontal-concerns rule.
- [ ] Delivery record referenced from `approval_request` so "nobody was told" is provable. Needs keel's outbox row to reference.
- [x] Never include evidence content or a proposed-action payload in a notification — reference only.

### K1 — `tool_credential_binding` (HIGH)

- [x] `schema/tool/tool_credential_binding.yml`: `(tenant_id, principal ref, tool_id, purpose, credential_ref, scopes, begda, endda)`. `credential_ref` points at a keel-held scoped identity or OAuth connection — **never secret material**, per idea-12 §4 and the global secrets rule.
- [x] Replaces the shared `tool_version.credential_ref`, which gives every agent using a tool version one indistinguishable identity.
- [x] Check constraint: a binding without a validity window is rejected.

### K2 — Principal-scoped credential port (HIGH, breaking)

- [x] `ToolCredentialProvider.Credential(ctx, tenantID, toolID)` becomes `Credential(ctx, principal, tool, action, purpose) → (credential, authority domain.AuthorityRef, err)`.
- [x] Resolution is just-in-time, at the authorized call, after policy and guardrails — the existing order in `governed_gateway.go` is already right; only the key changes.
- [x] The returned authority is recorded in the decision record (V1); the secret never is.
- [x] Prefer short-lived credentials; a provider returning a long-lived secret logs a bounded warning and is flagged in the readiness check.

### K3 — Delegated authority and revocation (MED)

- [x] Record whose authority a user-delegated OAuth connection exercises, so §4's "record whose authority is being used" is satisfiable.
- [x] Revocation/reauthorization signal when the relationship ends; bound work stops or routes to the configured fallback rather than continuing.
- [x] Test: revoking a human's connection stops the agent's use of it within the configured window.

### K4 — SPIFFE and token-exchange adapters (LOW)

- [ ] SVID-based workload identity behind `PrincipalResolver` — short-lived, auto-rotating, per-instance.
- [ ] RFC 8693 token exchange for the delegated half; the two-layer split (mTLS answers "which workload", JWT `act` answers "on whose behalf") is the conventional pattern and the two verify independently.
- [ ] Optional: an air-gapped install without SPIRE falls back to keel-issued credentials, and the readiness check reports which mode is live.

### B1 — Scope-keyed budgets (HIGH)

- [x] `budget` and `quota` become `config_resource_kind`s in C1, so a child narrows and can never broaden (C3).
- [x] Budget and autonomy resolve per principal through `isolation.ReleaseLimits` over the frozen release.
- [ ] **B1a.** `TenantWeightPolicy` still has no principal dimension, so per-agent concurrency ceilings are not enforced.
- [ ] `isolation.BudgetLedger` reserves per principal within the tenant envelope. `ReleaseLimits` resolves the ceiling; wiring it into the ledger's reservation key is B1a.
- [x] Money stays integer minor units with a currency-derived exponent — no assumption of two decimal places.
- [ ] Test: an agent exhausting its own budget does not consume its tenant's remainder, and vice versa. Needs B1a.

### B2 — Usage ledger attribution (HIGH, schema)

- [x] `usage_event` gains agent/principal and `scope_id` columns; today it keys `(tenant, conversation, turn)` only, so cost cannot be attributed to an agent, let alone an organizational unit.
- [x] `domain.Observation` gains the same, within the bounded-cardinality label policy — exact per-principal accounting goes through `TenantLedger`, never fleet metric labels. Reuse the `E1`/`heavyhitters` discipline already in place.
- [x] Enables idea-12 §14's per-managed-agent subscription metric and §15's "cost per completed business outcome"; without it neither is measurable.

### B3 — Time windows and schedules (MED)

- [x] Autonomy-mode-aware operating windows: an agent in `bounded_autonomous` may act only inside its declared window; outside it, the mode degrades to `execute_with_approval` rather than failing.
- [x] Window is a binding, so it inherits and narrows like everything else.

### V1 — `DecisionRecord` (HIGH, breaking)

- [x] `domain.DecisionRecord{Principal, Authority, Action, Resource, EffectiveReleaseVersion, PolicyID, PolicyVersion, Decision, Obligations, Reason, EvidenceRef, OccurredAt}` replaces `domain.AuditEvent{TenantID, Category, Payload, OccurredAt}`.
- [x] `audit_event` gains typed columns for the above; `payload_uri` + `payload_digest` remain for the redacted evidence blob.
- [x] `SafetyEvent` gains principal attribution.
- [x] Keep evidence structurally separate from telemetry: metrics are sampled and lossy, evidence is not. Never derive one from the other.
- [x] Preloop's evidence shape (matched policy, inputs, approver, runtime principal, outcome, exportable and queryable for GRC) is the reference target.

### V2 — `AuditQuery` read port (HIGH)

- [x] `contract.AuditQuery`: timeline by principal, by resource, by conversation, by scope, by decision class, with retention-aware paging. `AuditSink` is write-only today, so §15 items 5 and 9 have no source.
- [x] Tenant isolation is structural: a query resolves to exactly one tenant, or to the platform-wide records that name none; reading across tenants is not expressible in `DecisionQuery`.
- [ ] Export path for GRC consumption. `AuditQuery` is the read side; the export format is a product concern until a customer names one.

### V3 — Emit at every governed boundary (HIGH)

- [x] Tool invoke, model call, retrieval, approval, publication, agent state change, delegation grant and revocation, credential resolution, narrowing rejection.
- [ ] One record per decision, idempotent under turn replay. Records are append-only today, so a redelivered turn can write a second row.
- [ ] Test: a full turn produces a complete, gap-free chain from ingress to terminal state. Needs an integration harness, not a unit test.

### V4 — OTel GenAI mapping (LOW)

- [ ] Map `domain.Observation` onto the GenAI semantic conventions and **pin the version**: as of mid-2026 every `gen_ai.*` attribute still carries Development stability and the conventions moved to their own repository at v1.42.0.
- [x] Audit records never depend on the mapping: `DecisionRecord` is a separate durable type from `Observation`.

---

## Wave 3 — organization and delegation

### T1 — `agent_type` and `agent_type_version` (MED)

- [x] `schema/agent/agent_type.yml` and `agent_type_version.yml`: publishable, digest-addressed, with change summary and publisher, mirroring `agent_version`'s discipline.
- [x] `agent_profile.agent_kind` — today a bare `VARCHAR(80)` that is **not an FK to anything** — becomes a real FK to `agent_type`.
- [x] `agent_kind` is renamed to `agent_type_id` across the schema, the domain values, and the contracts; a downstream seeds `agent_type` rows before applying the new foreign key.
- [x] Human `position_type` stays in the commercial app; Scout models only the agent type.

### T2 — `agent_capability_package` (MED)

- [x] A named, versioned bundle of tool, knowledge and policy bindings, bound at any scope through `config_scope_binding` — not a new binding mechanism.
- [x] Requested scopes are declared least-privilege at the type level and narrowed at the instance (idea-12 §4 step 2).

### T3 — Typed sets on `AgentDefinition` (MED, breaking)

- [x] `AgentTypeVersion` carries the typed autonomy and capability sets, and `AgentDefinition` pins the type version. The concrete tool, knowledge, and policy values stay in the effective release rather than being duplicated into the definition.
- [x] Capability packages expand into `config_scope_binding` rows at instantiation and freeze into `effective_agent_release` with provenance, so publication genuinely freezes what idea-12 §3 requires.

### T4 — Instantiate and conform (MED)

- [x] `InstantiateFromType(typeVersion, scope, overlay) → agent draft`, with the overlay narrowing-checked at creation.
- [x] Conformance check: after a type republishes, report which live instances no longer satisfy it. Never auto-upgrade a running instance — the pinned release is the contract.
- [ ] A2A AgentCard as the type version's published, discoverable surface. Worth doing with D5, not before.

### L1 — Audited agent state machine (MED)

- [x] `agent_profile.is_active` — a plain boolean with no reason, actor, timestamp or audit row — becomes a state code: `draft → active → suspended → draining → retired`.
- [x] Every transition records reason and acting principal, and emits a decision record (V3).
- [x] `draining` lets in-flight turns finish; `suspended` stops new work immediately. The kill switch stays a single, obvious operation.

### L2 — Per-agent-version quarantine (MED)

- [x] Withdraw one agent version from all traffic without editing `agent_deployment` — idea-12 §7 identifies this gap explicitly.
- [x] Mirror the platform-release `Quarantine(version, actor, reason)` shape already in `contract/release.go` so operations learn one idiom.
- [x] Interaction with `agent_version_pin`: a quarantined version overrides a compliance pin, and the conflict is reported rather than silently resolved.

### L3 — Effective dating everywhere (MED)

- [x] `begda` / `endda` on every scoped binding, delegation grant and assignment — keel's `user_permission` convention, adopted verbatim rather than reinvented.
- [x] Resolution is as-of-now by default and as-of-time for audit reconstruction: "what was this agent allowed to do on 3 March?" must be answerable.

### E1 — Entitlement resolution (MED)

- [x] `contract.EntitlementResolver`: principal + scope → entitlement labels. Today `KnowledgeQuery.Entitlements` is caller-asserted; the index compiles it into the query and fails closed, which is good, but nothing derives or verifies the claim.
- [x] Subset enforcement at publication (C3): an agent's labels ⊆ its type's ⊆ its tenant's.
- [x] Resolved labels freeze into the effective release, so retrieval never trusts a runtime caller.
- [ ] Reopen OpenFGA or SpiceDB only when entitlements must derive from *relationships* rather than label subsets. **OpenFGA first** — usable as a Go library, so the consistency boundary stays in-process.

### D1 — `agent` execution step kind (MED)

- [x] Fourth step kind beside `model`, `tool`, `knowledge` — today an agent cannot invoke another agent inside the governed graph at all.
- [x] Executor resolves the target agent's pinned release, constructs the delegated principal (P1), and enforces the grant's bound before dispatch.
- [ ] Sub-turn usage settles against the delegating agent's budget. The bounds carry the ceiling to the invoker; wiring it into the budget ledger is B1a.
- [x] The `wait` step kind is seeded alongside `agent`, so H1's suspension has a graph representation; the executor for it is composed downstream.

### D2 — `delegation_grant` (MED)

- [x] `(tenant_id, grantor ref, grantee ref, action_scope, max_depth, budget_bound, approval_required, begda, endda, granted_by)`.
- [x] The grant may convey **only a subset** of the grantor's own effective permission set — a set comparison over the same authorization objects, actions and `low_limit`/`high_limit` ranges, which is exactly why P3 chose to extend keel's model rather than build a parallel one.
- [x] A reporting line grants nothing by itself. Idea-12 §1: placement, supervision, delegation and authorization are four separate relationships and Scout must not collapse them.
- [x] Revocation is immediate and propagates to in-flight work.

### D3 — Work item addressed to a principal (MED)

- [x] All work currently enters through `ConversationIngress.OpenTurn`, so it is conversation-shaped; `agent_run` records only completed runs, with no pending state.
- [x] A work item with its own lifecycle, addressed to a principal, that a conversation turn may satisfy — so "assign work to an agent" is representable.
- [x] `agent_work_item` records the assignment and its chain; dispatch still runs through the existing durable queue and fair scheduler. No second queue.

### D4 — Bounds and cycle detection (MED)

- [x] Depth, budget and scope propagate as **narrowing** constraints down every delegation hop; a hop may never widen one.
- [x] Cycle detection over the delegation graph, building on `isolation.MemoryLoopDetector`.
- [x] Test: a three-hop chain exhausting its budget at hop two stops cleanly, settles usage, and records evidence at each hop.

### D5 — A2A adapter (LOW)

- [ ] Adopt A2A for **cross-process** delegation, the way the official MCP Go SDK is adopted for tools. Its task lifecycle maps onto Scout's turn states plus `suspended` from H1.
- [ ] `input-required` is the wire form of H1's suspension — reuse it rather than inventing a second remote-wait protocol.
- [x] The residue A2A leaves to the implementer — identity, policy, budget, control plane — is P1, A1 and B1, and all three are now built, so the adapter is a transport job rather than a design one.
- [x] `contract.AgentInvoker` is that port: `AgentStepExecutor` owns the authority and hands only the call to it, so an in-process runtime and an A2A client compose identically.

### X1 — Explain read model (MED)

- [x] Effective value + winning source + losing candidates + narrowing outcome, per resource kind, over C4's provenance and V1's decision records.
- [x] Answers idea-12 §15 item 5, "why is this allowed?", without recompiling.
- [x] Studio's existing `AgentPromptSection` view becomes one consumer of this model, not a parallel mechanism.

### X2 — Effective-release diff (LOW)

- [x] Structural diff between two `effective_agent_release` versions, by resource kind, with provenance on both sides — the source for the Sail provenance-diff screen in idea-12 §7.

---

## Breaking-change register

For `migration_guide.json` when wave 1 lands. Clean breaks, no deprecated aliases (global shared-library rule).

| Old | New | Item |
|---|---|---|
| `domain.ToolCall{TenantContext, RequestID, ConversationID, ToolID, ToolVersion, Arguments}` | adds `Principal` | P2 |
| `domain.TurnRequest{…, AgentID}` | adds `Principal`, `OnBehalfOf` | P2 |
| `domain.KnowledgeQuery.Principal string` | `domain.Principal` | P2 |
| `domain.TenantContext` | adds `ScopeID` | P2 |
| `ToolCredentialProvider.Credential(ctx, tenantID, toolID)` | `Credential(ctx, principal, tool, action, purpose)` returning the exercised authority | K2 |
| `domain.AuditEvent{TenantID, Category, Payload, OccurredAt}` | `domain.DecisionRecord` | V1 |
| `domain.PromptSourceLevel` (closed 3-value enum) | removed; scope chain from `ScopeResolver` | C5 |
| `agent_prompt_override.overwrite` (child-set) | `config_scope_binding.merge_mode` + `sealed` (parent-set) | C1/C5 |
| `tool_version.credential_ref` | `tool_credential_binding` at principal scope | K1 |
| `agent_profile.is_active BOOLEAN` | agent state code + transition audit | L1 |
| `agent_profile.agent_kind VARCHAR(80)` (no FK) | FK to `agent_type` | T1 |
| keel `CheckPermission(ctx, userID int, …)` | `CheckPermission(ctx, principal, …)` | P3 |
| `ErrApprovalPending` wrapping `ErrForbidden` | control-flow signal handled by the runtime | H1 |

New schema modules: `scope` (scope, config_scope_binding, effective_agent_release, catalogs), `approval` (approval_request, approval_decision, approval_status), plus `agent_type` / `agent_type_version` / `agent_capability_package` in `agent`, `tool_credential_binding` in `tool`, `delegation_grant` in `scope`, and keel's `agent_permission` in `core`. Register each in `schema/dependency.yml` so a downstream installs only what it uses.

---

## Verification gates

All three waves are in the tree. Eight of the nine gates are closed.

- [x] A turn suspended for approval resumes without re-executing a completed side effect — the reservation stays held and `Resumer` requeues the turn so the worker replays from its last checkpoint. Unit-covered; the crash drill against a live database is still owed.
- [x] A child binding that broadens any comparator is rejected at publish, and an instantiation overlay is checked the same way — `TestCompileRejectsBroadeningOverride`, `TestCompileRejectsAppendThatWidensASet`, `TestCompileRejectsRaisedCeilings`, `checkPolicy`, `AgentTypeStore.checkOverlay`.
- [x] A sealed company clause cannot be overridden by any child, in any resource kind — `TestCompileRejectsAnyOverrideOfASealedBinding`.
- [x] Two agents on the same tool version resolve **different** credentials, and each call records which authority it exercised — `TestTwoPrincipalsOnOneToolResolveDifferentCredentials`, `TestCredentialRecordsDelegatedAuthority`.
- [x] An expired or revoked grant stops being usable: `TestVerifyRejectsAnExpiredGrant`, `TestAuthorizeIgnoresAnExpiredOrUncoveredGrant`, and `TestAuthorizeRefusesWhenTheGrantorNoLongerHoldsTheAction`. Acting on `CredentialRevoker` for work already in flight still needs a worker.
- [x] `usage_event` attributes a completed turn to an agent and an organizational scope — `TestTableTurnRecordStoreFailAndUsageArgumentOrder`.
- [x] The audit query cannot cross a tenant boundary — a query resolves to one tenant or to the platform-wide rows, never to several tenants. Reconstructing a full chain for one turn needs the integration harness in V3.
- [x] "What was this agent allowed to do on <past date>?" — `config_scope_binding`, `agent_permission`, `tool_credential_binding`, and `delegation_grant` are all effective-dated, `CompileRequest.AsOf` selects them, and `scope.Explainer` reads the frozen provenance without recompiling.
- [x] A prompt is never the enforcement point: `BindingAuthorizer` for tools, `SetEvaluator` for policy, obligations failing closed at the gateway, `LatticeChecker` at publish, and `GrantAuthorizer` plus `Narrow` for delegation. All tested; none consults prompt text.
- [ ] A delegation chain exhausting its budget at hop two stops cleanly, settles usage, and records evidence at each hop. Depth and cycle bounds are enforced and tested; budget settlement per hop needs B1a.

---

## Scope discipline

Idea-12 §16 lists "scope exceeds a small team's capacity" as a principal risk and makes the §15 MVP the ceiling for release one. Applied here: **wave 1 and wave 2 are the commitment**; wave 3 is sequenced but not promised. All three waves are now in the tree. The remaining risk is the four external adapters and C5, none of which blocks the idea-12 §15 MVP. If they exceed their time-box, that is the signal idea-12 §12 names for re-opening Option B — report it rather than extending the schedule.

Nothing in this plan should be started before the reference workflow pack idea-12 §17 calls for exists. It is the specification these building blocks are shaped to serve, and building the substrate without it risks generic machinery that fits no real process.
