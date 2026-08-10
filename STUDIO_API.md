# Agent Studio HTTP compatibility reference

This reference fixes the backend contract that an HTTP adapter must expose while Studio handlers are implemented. The handler authenticates through keel, derives `tenant_id` and `actor_id` from the session, calls one `port.AgentStudioHTTPBackend` method, and serializes the result. It never accepts tenant or actor identity from request JSON.

The initial compatibility profile is `seo-v1`. It preserves the existing SEO route names and snake-case payload fields so SEO can replace its backend before changing its frontend. The compile-time DTOs live in `api/studio.go`; Scout domain names remain product-neutral and the HTTP adapter performs the mappings below.

## Routes and authorization

All paths are relative to the authenticated API origin.

| Method | Path | Keel action | Backend method |
|---|---|---|---|
| `GET` | `/api/agent-studio/agents` | `VIEW` | `ListAgents` |
| `GET` | `/api/agent-studio/agent?agent_name=<id>` | `VIEW` | `GetDraft` |
| `POST` | `/api/agent-studio/draft` | `EDIT` | `SaveDraft` |
| `POST` | `/api/agent-studio/test` | `TEST` | `TestDraft` |
| `POST` | `/api/agent-studio/publish` | `PUBLISH` | `Publish` |
| `POST` | `/api/agent-studio/restore` | `RESTORE` | `Restore` |
| `POST` | `/api/agent-studio/reset` | `EDIT` | `Reset` |
| `POST` | `/api/agent-studio/set-default` | `PUBLISH` | `SetDefault` |
| `GET` | `/api/agent-studio/history?agent_name=<id>` | `VIEW` | `History` |
| `GET` | `/api/agent-studio/models` | `VIEW` | `Models` |

The authorization object and scope remain application configuration. SEO may continue using `AGENT_STUDIO` and `agent_studio`; Scout does not seed product roles.

## Stable field mappings

| HTTP field | Scout domain field | Persistence source |
|---|---|---|
| `agent_name` | `AgentID` | `agent_profile.agent_id` |
| `agent_type` | `AgentKind` | `agent_profile.agent_kind` |
| `prompt_header_id` | `PromptSectionID` | `prompt_section.id` |
| `agent_revision` / `expected_agent_revision` | draft revision | `agent_draft.draft_revision` |
| `type_defaults_revision` / `expected_type_defaults_revision` | prompt profile revision | `agent_alias.revision` |
| `published_version` / `version` | immutable version | `agent_version.agent_version` |
| `is_default` | alias target equality | `agent_alias.agent_id` |
| `active` | stable deployment equality | `agent_deployment.stable_version` |
| `enabled` | operational and release-enabled compatibility value | `agent_profile.is_active`, `agent_draft.enabled` |

The `seo-v1` adapter emits versions as JSON numbers and rejects non-decimal versions. Scout treats versions as opaque strings internally so later profiles can use other version formats.

Model fields retain the SEO shape under `models`: `text_model`, `image_model`, and `video_model`. The adapter resolves each legacy model id to a Scout `(provider_id, model_id)` pair and must fail on zero or multiple matches.

Prompt sections retain `business_text`, `business_output`, `default_text`, `default_output`, `override_text`, `override_output`, `overwrite`, `effective_text`, and `effective_output`. In Scout terminology, `business_*` is the selected platform baseline and `default_*` is the tenant default.

## Mutation contracts

- `SaveDraft` atomically checks the submitted agent and prompt-profile revisions, updates the mutable agent configuration and prompt rows, advances every changed revision, records the actor, and returns a fresh draft. Under `seo-v1`, `enabled` updates both the operational switch and prospective release value.
- `TestDraft` executes the current saved draft; it does not test unsaved request content or publish a version.
- `Publish` checks both revisions, compiles every language, writes one immutable `agent_version`, and promotes it only when the agent is the selected alias target.
- `Restore` copies an immutable source version into a new version and records `restored_from_version`; it never mutates history.
- `Reset` changes draft prompt rows only. The accepted scopes remain `agent_override`, `type_default`, and `platform_baseline`.
- `SetDefault` changes the logical-kind alias with optimistic revision checking and uses the selected agent's stable deployment when present; an agent without a deployment remains unpublished.

Publishing stores a canonical JSON definition containing models, approval policy, compiled language sections, source provenance, and product extension data. Runtime reads only the immutable definition selected through `agent_alias` and `agent_deployment`; it never compiles live draft rows.

## Responses

Successful reads and mutations return HTTP `200` with the existing SEO payload shape. The compatibility adapter preserves the following aggregate fields:

- summaries: readiness, reason, both revisions, published version, and publication time;
- drafts: model selection, approval policy, languages, full prompt provenance, drift, and both expected revisions;
- tests: model, digest, output, latency, token usage, product credits, and section captions;
- releases: version, definition digest, change summary, actor, time, active state, and languages;
- models: provider, modality, display name, and product credit guidance.

Product credits are derived by the SEO adapter from Scout usage and model rates. Scout accounting remains integer minor units with explicit currency.

## Error mapping

| Domain condition | HTTP status |
|---|---:|
| malformed request or `domain.ErrValidation` | `400` |
| unauthenticated session or `domain.ErrUnauthorized` | `401` |
| failed keel action or `domain.ErrForbidden` | `403` |
| missing agent or release / `domain.ErrNotFound` | `404` |
| stale revision / `domain.ErrConflict` or `domain.ErrRevisionConflict` | `409` |
| quota or budget limit / `domain.ErrRateLimited` or `domain.ErrBudgetExceeded` | `429` |
| no executable release / `domain.ErrNotReady` or `domain.ErrCircuitOpen` | `503` |
| unclassified failure | `500` |

Validation responses include a top-level message and a `fields` array of `{field, message}` entries. Internal errors must not be downgraded to empty lists or successful responses.

## Compatibility boundary

The compatibility promise covers the Agent Studio routes above. `/api/copilot/*`, product capability catalogs, provisioning, prompt seed content, and SEO task execution remain SEO contracts and are not part of Scout Agent Studio.
