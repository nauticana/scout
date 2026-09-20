# Agent Studio HTTP compatibility reference

This reference fixes the backend contract exposed by `handler.StudioHandler`. The handler authenticates through keel, derives `tenant_id` and `actor_id` from the session, calls one `contract.AgentStudioHTTPBackend` method, and serializes the result. It never accepts tenant or actor identity from request JSON.

The initial compatibility profile is `studio-v2`. It defines stable route names and snake-case payload fields independently of the transport-neutral Scout domain. The compile-time DTOs live in `api/studio.go`, and the HTTP adapter performs the mappings below.

## Routes and authorization

All paths are relative to the authenticated API origin.

| Method | Path | Keel action | Backend method |
|---|---|---|---|
| `GET` | `/api/agent-studio/agents` | `VIEW` | `ListAgents` |
| `GET` | `/api/agent-studio/agent?agent_name=<id>` | `VIEW` | `GetDraft` |
| `POST` | `/api/agent-studio/draft` | `EDIT` | `SaveDraft` |
| `POST` | `/api/agent-studio/enabled` | `EDIT` | `SetEnabled` |
| `POST` | `/api/agent-studio/test` | `TEST` | `TestDraft` |
| `POST` | `/api/agent-studio/publish` | `PUBLISH` | `Publish` |
| `POST` | `/api/agent-studio/restore` | `RESTORE` | `Restore` |
| `POST` | `/api/agent-studio/reset` | `EDIT` | `Reset` |
| `POST` | `/api/agent-studio/set-default` | `PUBLISH` | `SetDefault` |
| `GET` | `/api/agent-studio/history?agent_name=<id>` | `VIEW` | `History` |
| `GET` | `/api/agent-studio/audit?agent_name=<id>` | `VIEW` | `AuditLog` |
| `GET` | `/api/agent-studio/release-sections?agent_name=<id>&version=<n>` | `VIEW` | `ReleaseSections` |
| `GET` | `/api/agent-studio/models` | `VIEW` | `Models` |

Scout seeds the `AGENT_STUDIO` authorization object, route verbs, `AGENT_ADMIN` and `AGENT_OPER`, the `agent_studio` page grant, and read-only Studio table grants. Applications assign roles to users and retain their product permissions.

## Stable field mappings

| HTTP field | Scout domain field | Persistence source |
|---|---|---|
| `agent_name` | `AgentID` | `agent_profile.agent_id` |
| `agent_type` | `AgentTypeID` | `agent_profile.agent_type_id` |
| `prompt_header_id` | `PromptSectionID` | `prompt_section.id` |
| `agent_revision` / `expected_agent_revision` | draft revision | `agent_draft.draft_revision` |
| `type_defaults_revision` / `expected_type_defaults_revision` | prompt profile revision | `agent_alias.revision` |
| `published_version` / `version` | immutable version | `agent_version.agent_version` |
| `is_default` | alias target equality | `agent_alias.agent_id` |
| `active` | stable deployment equality | `agent_deployment.stable_version` |
| `enabled` | operational and release-enabled compatibility value | `agent_profile.state_code`, `agent_draft.enabled` |

The `studio-v2` adapter emits versions as JSON numbers and rejects non-decimal versions. Scout treats versions as opaque strings internally so later profiles can use other version formats.

Model fields use compatibility identifiers under `models`: `text_model`, `image_model`, and `video_model`. The adapter resolves each identifier to a Scout `(provider_id, model_id)` pair and must fail on zero or multiple matches.

Each prompt section carries `layers`, widest scope first, with `effective_text` and `effective_output`:

| Layer field | Meaning |
|---|---|
| `scope_id`, `scope_kind` | the contributing scope; the first layer is the selected platform baseline, `scope_kind` `platform`, `scope_id` its baseline key |
| `merge_mode` | `append` (the default for a layer Studio writes) or `replace` |
| `sealed` | set by the layer's own scope; no narrower layer may follow it |
| `editable` | response only: true for the agent's scope and its type's tenant-default scope |
| `instruction`, `output` | the layer's own text, not the merged text |

A draft response names the two scopes a layer may be added at, `agent_scope_id` and `type_scope_id`. A save writes the editable layers it is sent and ends an editable binding it is not sent, so a client returns the layers it did not change. Layers that are not editable are ignored on the way in; the stored ones remain the authority, including for sealing. Release sections carry `source`: the `scope_id`, `scope_kind`, `merge_mode`, and `sealed` of the layer that decided the section when the release was compiled.

`studio-v2` replaces the fixed `business_*`, `default_*`, `override_*`, and `overwrite` fields of `studio-v1`; nothing reads or writes them.

## Mutation contracts

- `SaveDraft` atomically checks the submitted agent and prompt-profile revisions, updates the mutable agent configuration and prompt rows, advances every changed revision, records the actor, and returns a fresh draft. Under `studio-v2`, `enabled` updates both the operational switch and prospective release value.
- `SetEnabled` is a targeted kill switch. It updates operational and prospective release state with only the draft revision check and does not run publish readiness or product validators.
- `TestDraft` executes the current saved draft; it does not test unsaved request content or publish a version.
- `Publish` checks both revisions, compiles every language, writes one immutable `agent_version`, and promotes it only when the agent is the selected alias target.
- `Restore` copies an immutable source version into a new version and records `restored_from_version`; it never mutates history.
- `Reset` ends prompt bindings only: `agent_override` those of the agent's scope, `type_default` those of its type's scope, `platform_baseline` both; optionally narrowed to one section, one language, or both.
- `SetDefault` changes the logical-kind alias with optimistic revision checking and uses the selected agent's stable deployment when present; an agent without a deployment remains unpublished.

Publishing stores a canonical JSON definition containing models, approval policy, compiled language sections with the provenance of each, and product extension data. Runtime reads only the immutable definition selected through `agent_alias` and `agent_deployment`; it never compiles live draft rows.

## Responses

Successful reads and mutations return HTTP `200` with the `studio-v2` payload shape. The compatibility adapter preserves the following aggregate fields:

- summaries: readiness, reason, both revisions, published version, and publication time;
- drafts: model selection, approval policy, languages, full prompt provenance, drift, and both expected revisions;
- tests: model, digest, output, latency, token usage, product credits, and section captions;
- releases: version, definition digest, change summary, actor, time, active state, and languages;
- models: provider, modality, display name, and product credit guidance;
- audit: event, detail, actor, and time;
- release sections: frozen language, sequence, section identity, copy, instruction, and output.

The shared handler maps identity, modality, tokens, and integer cost by default. Products that retain another display-credit scale supply `StudioHandler.MapModel` and `MapTestResult`; Scout accounting remains integer minor units with explicit currency.

## Error mapping

| Domain condition | HTTP status |
|---|---:|
| malformed request or `domain.ErrValidation` | `400` |
| unauthenticated session or `domain.ErrUnauthorized` | `401` |
| failed keel action or `domain.ErrForbidden` | `403` |
| missing agent or release / `domain.ErrNotFound` | `404` |
| stale revision / `domain.ErrConflict` or `domain.ErrRevisionConflict`; a layer under a sealed one / `domain.ErrSealed` | `409` |
| quota or budget limit / `domain.ErrRateLimited` or `domain.ErrBudgetExceeded` | `429` |
| no executable release / `domain.ErrNotReady` or `domain.ErrCircuitOpen` | `503` |
| unclassified failure | `500` |

Validation responses include a top-level message and a `fields` array of `{field, message}` entries. Internal errors must not be downgraded to empty lists or successful responses.

## Compatibility boundary

The compatibility promise covers the Agent Studio routes above. Product capability catalogs, provisioning, prompt seed content, and task execution remain application contracts and are not part of Scout Agent Studio.
