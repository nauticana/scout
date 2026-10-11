# HTTP API surface

Scout ships transport-neutral services and thin HTTP bridges over them. Every handler authenticates through keel, derives the tenant and the acting person from the session, calls one service method, and serializes the result. Tenant and actor identity never come from request JSON; services authorize every call themselves, so a product service calling them directly is held to the same grants as a person.

The compile-time DTOs live in `api/`. Agent Studio's compatibility profile is `studio-v2`: stable route names and snake-case fields, independent of the Scout domain.

## Agent Studio

`handler.StudioHandler` serves the core routes over one `contract.AgentStudioHTTPBackend`, implemented by `controlplane.StudioService`. Paths are relative to the authenticated API origin.

| Method | Path | Action | Backend method |
|---|---|---|---|
| `GET` | `/api/agent-studio/features` | — | the feature manifest |
| `GET` | `/api/agent-studio/agents` | `VIEW` | `ListAgents` |
| `POST` | `/api/agent-studio/create` | `CREATE` on the type | `Create` |
| `GET` | `/api/agent-studio/agent?agent_name=<id>` | `VIEW` | `GetDraft` |
| `GET` | `/api/agent-studio/settings?agent_name=<id>` | `VIEW` | `Settings` |
| `POST` | `/api/agent-studio/setting` | `EDIT` | `SaveSetting` |
| `POST` | `/api/agent-studio/draft` | `EDIT` | `SaveDraft` |
| `POST` | `/api/agent-studio/enabled` | `EDIT` | `SetEnabled` |
| `POST` | `/api/agent-studio/rename` | `EDIT` | `Rename` |
| `POST` | `/api/agent-studio/archive` | `ARCHIVE` | `Archive` |
| `POST` | `/api/agent-studio/unarchive` | `ARCHIVE` | `Unarchive` |
| `POST` | `/api/agent-studio/test` | `TEST` | `TestDraft` |
| `POST` | `/api/agent-studio/publish` | `PUBLISH` | `Publish` |
| `POST` | `/api/agent-studio/restore` | `RESTORE` | `Restore` |
| `POST` | `/api/agent-studio/reset` | `EDIT` | `Reset` |
| `POST` | `/api/agent-studio/set-default` | `PUBLISH` | `SetDefault` |
| `GET` | `/api/agent-studio/history?agent_name=<id>` | `VIEW` | `History` |
| `GET` | `/api/agent-studio/audit?agent_name=<id>` | `VIEW` | `AuditLog` |
| `GET` | `/api/agent-studio/release-sections?agent_name=<id>&version=<n>` | `VIEW` | `ReleaseSections` |
| `GET` | `/api/agent-studio/models` | a valid actor | `Models` |

### Authorization

Actions are on the `AGENT_STUDIO` authorization object, checked in the service at the value `agent:<agent_id>`; `CREATE` is checked at `agent_type:<type_id>` because the agent has no id yet, and a copy also needs `VIEW` on its source. keel matches a grant's `low_limit` exactly or as `*`, so a role holds every agent of its tenant (`*`) or named agents. A group of agents is expanded to explicit grants. `ListAgents` returns only the agents the actor may view; direct access to any other fails closed. `StudioService.Principals` and `Roles` are required.

`domain.StudioActor` carries the tenant and a `domain.PrincipalRef`: a person (`human`, the decimal `user_account.id`) or a service such as a product installer (`service`, an agent principal registered for it). `agent_studio_event` records the principal's kind and id; the user-only columns `modified_by`, `published_by`, and `bound_by` take the person and stay empty for a service.

Scout seeds `AGENT_STUDIO` with `VIEW`, `CREATE`, `EDIT`, `TEST`, `PUBLISH`, `RESTORE`, `ARCHIVE`, and `APPROVE_PUBLISH`; `AGENT_ADMIN` holds all of them and `AGENT_OPER` holds `VIEW`, each at `*`. Restriction layers are tenant-wide and use their own object, `AGENT_RESTRICTION`, with `VIEW` and `REPLACE_TENANT` checked at the value `tenant`; `AGENT_ADMIN` holds both at `*`. Applications assign roles and narrower grants.

### Feature manifest

`GET /api/agent-studio/features` returns `[{id, version, actions, object?}]`: the core `agents` feature, then each optional feature the product mounted and listed in `StudioHandler.Features`. `object` names the authorization object the actions are granted on; absent means `AGENT_STUDIO`. A client intersects the manifest with the caller's grants to build navigation, and ignores an id it does not know.

| Feature | Handler | Routes |
|---|---|---|
| `traffic` | `StudioTrafficHandler` over `release.StudioTraffic` | `POST canary {agent_name, version, percentage}`, `POST promote {agent_name, version}`, `POST rollback {agent_name}` (`PUBLISH`); `GET explain?conversation_id=` (`VIEW`) |
| `delegation` | `StudioDelegationHandler` over `controlplane.StudioDelegations` | `GET delegations?agent_name=` (`VIEW`); `POST delegate`, `POST undelegate {agent_name, grant_id}` (`EDIT`) |
| `usage` | `StudioUsageHandler` over `dataplane.StudioUsage` | `GET usage?agent_name=&from=&to=` (`VIEW`), RFC 3339 times |
| `feedback` | `StudioFeedbackHandler` over `dataplane.TurnFeedback` | `GET feedback?agent_name=&from=&to=` (`VIEW`) |
| `runs` | `StudioRunsHandler` over `runtime.StudioRuns` | `GET runs?agent_name=&status=&before=&limit=` (`VIEW`) |
| `pause` | `StudioPauseHandler` over `dataplane.StudioPause` | `POST pause {agent_name, policy, reason, expected_agent_revision}` (`EDIT`) |
| `pilot` | `StudioPilotHandler` over `release.StudioPilots` | `GET pilot?agent_name=` (`VIEW`); `POST pilot/add`, `POST pilot/remove {agent_name, version, subject_kind, subject_id, reason}` (`PUBLISH`) |
| `activation` | `StudioActivationHandler` over `controlplane.ReleaseActivations` | `GET activations?agent_name=` (`VIEW`); `POST activation/cancel {agent_name, activation_id, reason?}` (`PUBLISH`) |
| `limits` | `StudioLimitsHandler` over `dataplane.StudioLimits` | `GET limits?agent_name=` (`VIEW`) |
| `guardrails` | `StudioGuardrailsHandler` over `controlplane.StudioGuardrails` | `GET guardrails?agent_name=`, `GET safety-events?agent_name=&before=&limit=` (`VIEW`); `POST guardrails {agent_name, rules, expected_agent_revision}` (`EDIT`) |
| `restrictions` | `StudioRestrictionHandler` over `policy.StudioRestrictions` | object `AGENT_RESTRICTION`: `GET restrictions?agent_name=` (`VIEW`); `POST restrictions {expected_digest, denials, guardrails}` (`REPLACE_TENANT`) |
| `governance` | `StudioGovernanceHandler` over `controlplane.StudioGovernance` | `GET governance?agent_name=` (`VIEW`); `POST governance {agent_name, draft, expected_agent_revision}` (`EDIT`) |
| `evaluation` | `StudioEvaluationHandler` over `evaluation.StudioEvaluation` | `GET cases?agent_name=`, `GET evaluation?run_id=&before=&limit=` (`VIEW`); `POST cases`, `POST cases/freeze` (`EDIT`); `POST evaluate {agent_name, set_version, subject}` (`TEST`) |
| `publish_review` | `StudioPublishReviewHandler` over `controlplane.StudioPublishReviews` | `GET publish-review?agent_name=` (`VIEW`); `POST publish-review {agent_name, note?, sod_exception_id?}` (`EDIT`); `POST publish-review/approve`, `POST publish-review/reject {agent_name, review_id, note?}` (`APPROVE_PUBLISH`) |
| `comments` | `StudioDraftCommentHandler` over `controlplane.StudioDraftComments` | `GET comments?agent_name=&status=` (`VIEW`); `POST comment`, `POST comment/resolve {agent_name, comment_id}` (`EDIT` or `APPROVE_PUBLISH`) |

Each feature handler's `Feature()` is its manifest entry. Feature services authorize through `contract.StudioAuthorizer`, which `StudioService` implements; `policy.StudioRestrictions` authorizes `AGENT_RESTRICTION` itself. Each feature's backend is a `contract.Studio*Backend` interface, so a product may substitute its own.

### Field mappings

| HTTP field | Scout domain field | Persistence source |
|---|---|---|
| `agent_name` | `AgentID` | `agent_profile.agent_id` |
| `agent_type` | `AgentTypeID` | `agent_profile.agent_type_id` |
| `prompt_header_id` | `PromptSectionID` | `prompt_section.id` |
| `agent_revision` / `expected_agent_revision` | draft revision | `agent_draft.draft_revision` |
| `type_defaults_revision` / `expected_type_defaults_revision` | prompt profile revision | `agent_alias.revision` |
| `published_version` / `version` | immutable version | `agent_version.agent_version` |
| `is_default` | alias target equality | `agent_alias.agent_id` |
| `archived` | archived state | `agent_profile.state_code = 'archived'` |
| `active` | stable deployment equality | `agent_deployment.stable_version` |
| `enabled` | operational and release-enabled value | `agent_profile.state_code`, `agent_draft.enabled` |

`studio-v2` represents versions as JSON numbers. Restore and release-section requests require positive decimal versions, and release and history responses reject a stored non-decimal version. Summary and drift responses use zero when an internal version is not decimal. Scout treats versions as opaque strings internally.

Model fields use identifiers under `models`: `text_model`, `image_model`, and `video_model`. `StudioService` resolves each to a `(provider_id, model_id)` pair and fails on zero or several matches.

Each prompt section carries `layers`, widest scope first, with `effective_text` and `effective_output`:

| Layer field | Meaning |
|---|---|
| `scope_id`, `scope_kind` | the contributing scope; the first layer is the selected platform baseline, `scope_kind` `platform`, `scope_id` its baseline key |
| `merge_mode` | `append` (the default for a layer Studio writes) or `replace` |
| `sealed` | set by the layer's own scope; no narrower layer may follow it |
| `editable` | response only: true for the agent's scope and its type's tenant-default scope |
| `instruction`, `output` | the layer's own text, not the merged text |
| `bound_by` | response only: the user who last edited the layer; absent on the baseline |

A draft response names the two scopes a layer may be added at, `agent_scope_id` and `type_scope_id`. A save writes the editable layers it is sent and ends an editable binding it is not sent, so a client returns the layers it did not change. Layers that are not editable are ignored on the way in. Release sections carry `source`: the `scope_id`, `scope_kind`, `merge_mode`, and `sealed` of the layer that decided the section when the release was compiled.

### Mutation contracts

- `Create {agent_type, display_name, copy_from?, template_id?, idempotency_key}` makes a disabled agent in state `draft` with no release. Its settings are a copy of `copy_from` — models, extension, and every binding in force at the source agent's scope — or else the type's tenant defaults. It refuses a type the tenant or the product catalog does not offer, a display name another agent of the type has, and a type already holding `agent_studio_max_agents_per_type` agents. The type row is locked for the transaction. `agent_create_request` keys the idempotency record by tenant, principal, and key: a replay returns the agent it created, and the same key with another request is a conflict. `template_id` is refused until a template catalog is composed.
- `SaveDraft` checks the agent and prompt-profile revisions, updates the agent configuration and prompt bindings, advances the draft revision and any changed prompt-profile revision, records the actor, and returns a fresh draft. `enabled` updates both the operational switch and the prospective release value.
- `SetEnabled` is the kill switch: it updates operational and prospective release state behind the draft revision only, and records its optional `reason`. A disabled agent admits no new turn opened through the data plane's turn record store: it reads the agent's state under a share lock in the transaction that opens a turn, so no turn opens after the switch commits. A product that opens turns through `TurnLedger` checks the state itself.
- `Rename` changes `display_name` behind the draft revision; the agent id is permanent because grants and evidence key on it.
- `Archive` withdraws an agent; its releases, audit, and conversations remain. It refuses the type's default, an agent named by a delegation grant that has not ended (when `StudioService.Delegations` is set), and whatever a product validator refuses in phase `archive`, such as a schedule. An archived agent admits no turn and is read-only. `Unarchive` returns it disabled.
- `TestDraft` executes the current saved draft; it does not test unsaved content or publish a version.
- `Publish` checks both revisions, compiles every language, writes one immutable `agent_version`, and deploys it: as the stable release, or with `canary_percentage` between 1 and 99 as a canary beside the current stable one; 0 or 100 deploys it stable. An agent's first release cannot be a canary. With `effective_at` (RFC 3339) in the future, at most 366 days ahead, the version is written and frozen but not deployed: the response has `active: false` and an `activation`, and the activation worker deploys it later (see `activation` below). An agent holds at most one pending activation.
- `Restore` copies an immutable source version and its frozen settings into a new stable version and records `restored_from_version`; it never mutates history. A version without frozen settings cannot serve a turn and is refused (`409`).
- `Reset` ends prompt bindings only: `agent_override` those of the agent's scope, `type_default` those of its type's scope, `platform_baseline` both; optionally narrowed to one section, one language, or both.
- `SetDefault` moves the type alias with optimistic revision checking; an agent without a deployment remains unpublished.

`GET settings?agent_name=` returns the non-prompt bindings on the scope chain and each effective value with its winning and superseded sources. `POST setting` writes or removes one binding at the returned agent or type scope behind the matching revision, compiles the whole chain before writing, and refuses widening or sealed overrides. Set, budget, autonomy, and limits kinds use the kind name as `resource_id`; policy may use several ids. A `limits` value is `{turns_per_minute, max_steps, max_tool_calls, max_duration_seconds}`, 0 meaning not limited at that scope; a narrower scope may lower a field but not raise or unset it.

Publishing stores a canonical JSON definition containing models, compiled prompts, effective settings, tool and skill versions from the type narrowed by their bindings, named product sections, and extension data. Runtime reads only immutable definitions. A product extends publication through `StudioService.Guards` (`contract.PublishGuard`, which may refuse a publish or restore), `Sections` (`contract.DefinitionSection`, a named part frozen into `AgentDefinition.Sections` and its digest), and `ReleaseWriters` (writes in the release transaction). The optional features that shape a release are composed there too: `StudioGuardrails` as a section and a release writer, `StudioGovernance` as a section and a guard, and `StudioPublishReviews` as a guard and a release writer.

### Optional features

- `canary`, `promote`, and `rollback` change which release new conversations take, through `contract.AgentVersionTrafficManager`; a live conversation keeps its release. Each records a Studio event.
- `explain?conversation_id=` returns the release a conversation runs on, the rule that chose it (`stable`, `canary`, `experiment_cohort`, `pilot`, `compliance_pin`, `tenant_pin`, or `request_pin`), the alias it was requested through, a pin's reason, and the definition digest.
- `pilot` serves one published, unquarantined release to named people and positions; it decides only the release a new conversation of that person starts on, after compliance and tenant pins and before cohorts and canaries. `subject_kind` is `principal` (a canonical decimal user id) or `position` (the product's opaque reference, matched through `contract.PositionResolver`). A subject pilots one version of an agent at a time, re-adding an open membership returns it unchanged, `remove` ends it, and an agent holds at most `agent_pilot_max_members` open memberships.
- `activations` lists an agent's 100 most recent scheduled publishes, latest `effective_at` first, each `pending`, `activated`, `refused` with a reason code, or `cancelled`. `activation/cancel` withdraws a pending one. The activation worker (`controlplane.NewReleaseActivationWorker`, run from a product worker binary) re-checks the scheduler's `PUBLISH` authority, the definition and settings digests, every publish guard, and that the deployment has not moved since scheduling, then swaps it.
- `delegate {agent_name, grantee, action_scope, max_depth, budget_minor_units?, currency?, approval_required, valid_from?, valid_to}` lets `agent_name` hand work within `action_scope` to another agent of the tenant; its author needs `EDIT` on the grantor and `VIEW` on the grantee. A grant must end in the future, its depth is at most `agent_max_delegation_hops`, a budget names its currency, and it is refused when the grantee already delegates, directly or through others, back to the grantor. At use, `principal.GrantAuthorizer` still checks that the grantor holds what the grant conveys. `undelegate` ends one of the agent's own grants.
- `pause` disables the agent with a reason and then applies its policy to live turns: `finish` lets them complete, `cancel` asks each to stop through the durable turn canceller. It returns the new revision and how many turns were asked to stop.
- `feedback` counts the ratings, 1 to 5, each release received in a window of at most 92 days. It never reports a person.
- `runs` pages the agent's settled runs, newest first: release, task kind, request, status, and completion time.
- `usage` returns lines of settled usage per release, model (`provider`, `model`), person, product work reference, and currency, with the currency's exponent: turns, input and output tokens, tool calls, and cost in minor units. Usage no model performed, such as tool calls, is on a line without a model. `turns` counts the turns whose work the line holds, so a turn that used two models counts on both lines and turns do not add up across models. A window spans at most 92 days and a report at most 1000 lines; a larger one is refused.
- `limits` reports the stable release's frozen execution limits, the agent's turns opened in the last minute, and its budget `{tokens, cost_minor_units, currency, window_seconds}` when the release binds one. `spend` is the settled usage over `[now - window, now)`, with the currency's catalog exponent, and appears only when the composition sets a budget window.
- `guardrails` shows the rules an agent inherits, widest first (`baseline`, `platform`, `tenant`, `type`), and its own rules; a save replaces the own rules as a whole, behind the draft revision. An own rule may add a rule or tighten an inherited rule of the same id, never loosen it (`403`); `problem` reports own rules that no longer fit after an inherited layer changed. Publication freezes the rule set as section `guardrails` and pins it as the release's guardrail version. `safety-events` pages the agent's guardrail hits newest first, `limit` 1 to 500 (default 50) and `before` the last id seen; events carry rule ids and attribution, never inspected content.
- `restrictions` shows the platform restriction layer, read-only, and the tenant's, which `POST` replaces as a whole when it is still at `expected_digest` (empty before the first write; a stale digest is `409`). With `agent_name`, `overrides` lists the restrictions that take away tools or policy allow statements from the settings the agent's next publish would freeze, marking a denial with conditions `conditional`; `settings_problem` says why they cannot be computed. A layer holds at most 256 denials and 256 guardrail rules by default.
- `governance` edits the governance fields of the agent's Charter `AgentDefinition`, in Charter's member names, plus `triggerChannels` (`[{trigger, channel, binding?}]`, channel `conversation`, `mcp`, `api`, `schedule`, or `event`; Charter's `triggers` derives from them). `governance` is what a publish would freeze now and `draft` what Studio keeps; fields an enterprise `contract.GovernanceSource` supplies are read-only and refused on save. A schedule or event trigger needs a binding the product's `contract.TriggerBindingVerifier` proves, on save and on every publish. `problems` lists what a publish would refuse now; publication freezes section `governance`, and the flag `agent_governance_required_fields` names the fields a release must carry.
- `cases` shows and edits the agent's golden set: `op` `associate` (`golden_set_id`, `name`), `put` (`case`), or `remove` (`case_id`), the last two behind `expected_revision`; hidden gate cases are only counted. `cases/freeze` snapshots every case into an immutable set version. `evaluate` queues a run of one frozen set version against `subject` `{draft: true}` (frozen as a publish would, not persisted) or `{release}`; an agent holds at most `agent_evaluation_max_open_runs` open runs (`429`). `evaluation?run_id=` returns the run, its cost in minor units, and one page of results, `limit` up to 200 (default 50), continued by `next_before`; a run of an agent the caller may not view is `404`. The evaluation worker runs from a product worker binary.
- `publish-review` asks a second person to approve the exact release the draft would freeze. A review's authors are its requester, the last editors of every resource it adds or changes, and everyone who changed the draft since the last publish (`diff.editors`); none may approve it. Where `contract.PublishSeparation` requires two-person publish (`controlplane.BasePublishSeparation` for all or listed tenants, or `charter.PublishSeparation` from Charter separation-of-duties constraints), `publish` is refused unless the agent's latest review is approved, unconsumed, and for the candidate digest, no draft comment is open, and the publisher is no author, save the requester under the `sod_exception_id` they named while the separation still admits it. A publish consumes the review; a newer request or a rejection withdraws an earlier approval; restore is not gated. `GET` reports `required`, `stale` when the draft changed since the request, and the open comments.
- `comment` attaches a remark to the current `draft_revision`, anchored to the `draft`, a `prompt_section` (`prompt_section_id`, optional `language_code`), or a `setting` (`resource_kind`, `resource_id`); `comments` lists at most 500, newest first, optionally by `status` `open` or `resolved`.

## Conversation

`handler.ConversationHandler` mounts under `BasePath` (default `/api/conversation/`) over a composed `contract.DataPlane`. Its `CallerResolver` maps the session to the tenant context and the person a turn acts for; `handler.SessionCaller` takes the session's partner and user.

| Method | Path | Body or query | Result |
|---|---|---|---|
| `POST` | `turn` | `{request_id, conversation_id, agent_type, agent_name?, release?, release_reason?, work_ref?, input}` | `202 {request_id}` |
| `GET` | `turn/stream?request_id=&cursor=` | — | SSE frames from `cursor` |
| `POST` | `turn/cancel` | `{request_id, reason?}` | `202 {request_id}` |
| `GET` | `agents?agent_type=` | — | `[{agent_name, display_name, is_default, release}]` |
| `POST` | `turn/feedback` | `{request_id, rating, reason?}` | `202 {request_id}` |

`agent_type` is the type alias. Without `agent_name` the conversation's agent serves the turn, and a new conversation takes the alias target; with it, that agent must be of the alias's type, and no other agent is substituted on failure. The data plane resolves the serving agent's principal, refuses an agent that does not admit work (disabled, suspended, draining, archived, retired), and authorizes the person for `AGENT_CONVERSATION` `USE` at `agent:<id>`. `release` pins a new conversation to one immutable, unquarantined release instead of the traffic policy; it needs `PIN_RELEASE` and `release_reason`, and a conversation already on another release refuses it. `work_ref` is the product's opaque reference to the work the turn serves, carried onto usage and decision records. `agent_type`, `agent_name`, and `release` are at most 80 bytes and `release_reason` and `work_ref` at most 200. The resolved session must name a person. A new turn beyond the `turns_per_minute` limit of the conversation's release is `429` with `Retry-After`; a replayed `request_id` is never refused. `agents` lists the deployed agents of a type the person may use, the default first. `turn/feedback` rates the signed-in person's own completed turn from 1 to 5; a second rating replaces the first, and reports never identify the giver. It needs `ConversationHandler.Feedback`, a `dataplane.TurnFeedback`.

## Approval

`handler.ApprovalHandler` serves the approval desk, `contract.ApprovalDesk`, implemented by `approval.Resumer`.

| Method | Path | Body | Result |
|---|---|---|---|
| `GET` | `/api/approval/pending` | — | the requests the signed-in person may decide now, soonest deadline first |
| `POST` | `/api/approval/resolve` | `{request_id, execution_step_id, verdict, proposed_digest, reason?, revised_arguments?}` | the decided request |
| `POST` | `/api/approval/link` | `{token, tenant_id, request_id, execution_step_id, verdict, reason?}` | the decided request; no session |

A verdict is `approved`, `rejected`, or `edited`. `Resolve` authorizes the decider, records the verdict once against the exact `proposed_digest`, requeues an approved turn's delivery, and fails a rejected one. `edited` applies to a tool-call proposal and carries `revised_arguments`, a JSON object of at most 64 KiB by default, which no other verdict may carry. The arguments are archived through `contract.EvidenceArchive`, the original call never runs, and the requeued turn runs the revised call in its place through the full tool governance; one that needs approval again parks as a new request whose `revises_step_id` names the edited one. A call takes at most 8 revisions. Repeating the same edit replays; a different edit already recorded is `409`.

A decision link is a single-use keel `actiontoken` bound to one person, one request, one verdict (`approved` or `rejected`, never an edit), and the proposal digest. The product's notifier delivers it: `Resumer.MintLink` authorizes the person, refuses a request riskier than `Resumer.LinkMaxRisk` (empty refuses every link), and lapses at the request's deadline. Redemption authorizes the person again and decides under their identity. A spent, expired, or mismatched link, or any refusal before the token is verified, is `404`; a valid link whose person may no longer decide, or whose decision is already settled differently, is `403`.

## Errors

| Domain condition | HTTP status |
|---|---:|
| malformed request or `domain.ErrValidation` | `400` |
| unauthenticated session, `domain.ErrUnauthorized`, or `domain.ErrPrincipalUnknown` | `401` |
| missing partner context, `domain.ErrForbidden`, or a widening or loosening change / `domain.ErrAuthorityExceeded` | `403` |
| missing agent, release, conversation, or link / `domain.ErrNotFound` | `404` |
| stale revision, archived agent, disabled agent / `domain.ErrConflict` or `domain.ErrRevisionConflict`; a layer under a sealed one / `domain.ErrSealed` | `409` |
| quota, turn rate, or budget limit / `domain.ErrRateLimited` or `domain.ErrBudgetExceeded` | `429`, with `Retry-After` when the error carries one |
| no executable release / `domain.ErrNotReady` or `domain.ErrCircuitOpen` | `503` |
| unclassified failure | `500` |

Studio and approval request bodies decode strictly and are capped at keel's `MaxRequestSize`: an unknown member, trailing data, or an oversized body is `400`. Errors use keel's `application/problem+json` envelope. A field-validation failure serializes `api.ValidationProblem` — a message and `fields` of `{field, message}` — into the problem's `detail`. A `5xx` detail is replaced by a request ID; the cause is logged.

## Compatibility boundary

The promise covers the routes above. Product capability catalogs, provisioning, prompt seed content, and task execution remain application contracts.
