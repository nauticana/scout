# SEO Agent Studio migration

This guide moves the shared Agent Studio and MCP infrastructure into Scout while leaving product policy in the SEO application. It targets the current SEO Studio implementation and the `studio-v1` HTTP payload used by its frontend.

## Ownership after migration

| Concern | Owner |
|---|---|
| Prompt inheritance, draft assembly, optimistic revisions, kill switch, publish, restore, reset, history, release sections, Studio audit | Scout |
| Named SQL and transactions for Scout Studio tables | `service/controlplane.StudioService` through keel database interfaces |
| Authenticated `studio-v1` HTTP routes and error mapping | `handler.StudioHandler` |
| MCP transports, provider registration, envelopes, resources, text bundles, field catalogs, conformance checks | Scout `mcp` and `mcp/mcptest` |
| Sessions, authorization checks, query services, transactions, quota, secrets, metadata REST | keel |
| Agent kinds, line-of-business baseline precedence, purpose labels, product validation, provider construction, test execution, capability tasks | SEO application |
| Angular Agent Studio and product navigation | SEO application |

Scout contains no SEO purpose codes, line-of-business values, prompt copy, provider price list, copilot task, or product capability rule.

## Extension ports SEO must keep

Implement these Scout contracts in SEO and inject them into `controlplane.StudioService`:

- `PromptBaselineSelector`: map the partner line of business and wildcard policy to ordered `baseline_key` values.
- `AgentDraftValidator`: enforce rules such as which SEO agent kinds support media or require approval. Credential checks belong to the `ValidateRelease` phase so a draft still saves on a node without provider keys.
- `AgentDraftTestExecutor`: call the governed SEO runtime, quota, and provider construction path.
- `AgentKindCatalog`: return SEO labels and purpose text for the configured agent kinds.
- `StudioModelCatalog`: expose permitted models, resolve legacy model ids, validate capabilities, and project product credit guidance.

Optionally implement `AgentActivityReporter` to surface last-run times from the product's own execution record.

Keep SEO capabilities and provisioning policy behind SEO-owned services, but read and write Scout tables only through Scout: `controlplane.AgentProvisioner` seeds tenant, profile, draft, and alias rows, and `runtime.DeployedAgentIndex` reports alias state and deployed definitions for the capability API. Do not add BL, RR, QR, SD, SA, CP, line-of-business ids, copilot tasks, Google/OpenAI checks, or SEO prompt text to Scout.

## Schema mapping

Install keel `core`, `geo`, and `tenant_management`, then the Scout schema. Migrate data with explicit SQL owned by the SEO deployment; Scout does not guess product mappings.

| SEO table | Scout destination | Mapping notes |
|---|---|---|
| `ai_agent_config` | `agent_profile`, `agent_draft` | `partner_id` becomes `tenant_id`; split identity/operational state from mutable release settings; resolve every model to `(provider_id, model_id)` |
| `partner_default_agent` | `agent_alias`, `agent_deployment` | use the SEO agent type as `alias_id` and `agent_kind`; alias stores the selected agent and defaults revision; active version becomes the selected agent's stable deployment |
| `prompt_header` | `prompt_section` | preserve ids, captions, order, and descriptions; advance `prompt_section_seq` beyond imported ids |
| `business_prompt` | `prompt_baseline` | convert `lob_id` to a product-owned `baseline_key`; keep `agent_type` as `agent_kind` |
| `partner_prompt_default` | `tenant_prompt_default` | preserve the shared kind revision through `agent_alias.revision` |
| `ai_agent_prompt` | `agent_prompt_override` | preserve overwrite, language, instruction, and output |
| `ai_agent_release` plus language/source/section tables | `agent_version` | build one canonical definition JSON containing compiled languages and source provenance |
| `partner_default_agent.active_version` | `agent_deployment.stable_version` | create a deployment only when the referenced immutable definition migrated successfully |
| `agent_studio_audit` | `agent_studio_event` | map event, detail, actor, and time; only rows with a valid agent should enter the agent-scoped table |
| `ai_model` | `model_provider`, `model_definition`, `model_price` | split provider configuration, model limits, and currency-denominated integer pricing |

`copilot_conversation`, `copilot_turn`, SEO capability state, and SEO content tables remain downstream. Decide separately whether `agent_run` maps to Scout runtime/usage history or remains a product record.

## Backend cutover

1. Pin released Scout and keel module versions. Do not add `replace` directives or filesystem imports.
2. Implement the five SEO extension ports above and cover the existing kind matrix with tests.
3. Construct `controlplane.KeelPromptSourceRepository` with the SEO baseline selector.
4. Construct `controlplane.StudioService` with keel `DatabaseRepository`, the shared prompt compiler and assembler, and the SEO extensions.
5. Construct `handler.StudioHandler` with the keel abstract handler, database, and shared Studio service. Set `MapModel` and `MapTestResult` to preserve SEO's legacy credit display, then mount every handler returned by `Routes()`.
6. Replace local active-release selection with `runtime.PublishedAgentResolver`; keep SEO provider construction and execution after the immutable definition is resolved.
7. Keep the current Angular code on `studio-v1`; frontend migration is not part of Scout.
8. Assign `AGENT_ADMIN` or `AGENT_OPER` to the appropriate partner users. Scout seeds the roles, Studio verbs, page permission, and read-only table permissions, but it does not choose users.
9. Move MCP imports from `github.com/nauticana/keel/mcp` to `github.com/nauticana/scout/mcp` and conformance imports to `github.com/nauticana/scout/mcp/mcptest`.

The Scout menu and page key is `agent_studio`. During the frontend cutover, either change the SEO route permission from `ai_agent_configs` to `agent_studio` or retain a temporary SEO-owned compatibility grant; remove that grant with the old page.

The custom Studio handler is for lifecycle operations. Continue using keel-generated CRUD only for ordinary catalog reads; never expose direct draft, alias, override, default, version, deployment, or audit writes through table CRUD.

## Remove duplicate SEO code

After the data and route cutover is verified, remove or reduce these local components:

| SEO component | Action |
|---|---|
| `internal/service/prompt_compiler.go` | remove; use `controlplane.PromptCompiler` |
| `internal/service/prompt_source_repository.go` | remove; use `controlplane.KeelPromptSourceRepository` and retain only the SEO selector |
| shared lifecycle portions of `internal/service/agent_studio_service.go` | remove; retain product validator, tester, capability, provisioning, and model catalog implementations in focused files |
| active-release selection in `internal/service/service_factory.go` | replace with `runtime.PublishedAgentResolver`; retain provider factories and SEO execution policy |
| shared DTOs in `internal/model/agent_studio.go` | replace with Scout domain/API types; retain only product capability DTOs |
| shared interfaces in `internal/port/agent_studio.go` | replace with Scout contracts; retain product-only ports |
| `internal/handler/handler_agent_studio.go` | remove after mounting `handler.StudioHandler`; retain product capability handlers |
| `todo_upstream_mcp.md` | remove after consumers use Scout MCP; its implemented Keel MCP items now live in Scout |
| generic Studio rows in `sql/agent_seed.sql` and `sql/agent_rbac.sql` | remove after Scout seeds are installed |
| old Studio tables in `sql/agent_pgsql.sql` | remove only after migration validation and rollback retention |

Do not copy Scout services back into SEO under different package names. Product adapters should depend on Scout interfaces and be wired once in the SEO controller.

## Seed split

Scout now owns:

- `AGENT_STUDIO` with `VIEW`, `EDIT`, `TEST`, `PUBLISH`, and `RESTORE`;
- `AGENT_ADMIN` and `AGENT_OPER`;
- the `agent_studio` page and read-only permissions for Studio tables;
- Studio REST metadata and foreign-key lookup records;
- execution-step and capacity constants;
- supported language-code constants and prompt-column lookups.

SEO keeps:

- its agent-kind constants and captions;
- line-of-business and prompt baseline rows;
- provider/model catalog and product credit values;
- provisioning actions and their authorization object;
- SEO roles, pages, reports, content tables, and product capability permissions.

## Migration checks and risks

Perform the cutover in a maintenance window or with a single writer. The old and new revision models must not accept concurrent writes.

- Back up every source table and record row counts per partner and agent.
- Verify model ids resolve to exactly one provider. Scout rejects ambiguous legacy ids.
- Preserve kill-switch state in both `agent_profile.is_active` and `agent_draft.enabled`.
- Preserve alias and draft revisions; stale frontend writes must still return `409`.
- Recompute each migrated language and definition digest and compare it with the source release before activating deployment.
- Confirm every active pointer references the intended agent and version after the alias/deployment split.
- Verify actor ids still exist in keel `user_account`; quarantine invalid audit rows rather than dropping attribution silently.
- Advance imported surrogate-id sequences before accepting new prompt sections or audit events.
- Compare Studio list, draft, history, audit, release-section, model, enable/disable, publish, restore, reset, and set-default responses against `studio-v1` fixtures.
- Verify the frontend route and menu use the `agent_studio` page key before removing the `ai_agent_configs` compatibility permission.
- Verify role assignment during partner registration; seeded roles alone do not grant a user access.
- Keep the old tables read-only for the rollback period. Roll back code and routing together; do not point old code at Scout tables.

The SEO team owns its production migration SQL, maintenance window, validation evidence, and rollback decision.
