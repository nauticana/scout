# Scout database reference

Scout requires keel `tenant_management`, which declares direct dependencies on keel `core` and `geo`. Install the groups from the same pinned `github.com/nauticana/keel` module in dependency order: `core`, `geo`, `tenant_management`, then Scout. Keel supplies `business_partner`, users and sessions, authorization, consent, metadata-driven REST, and `table_sequence_usage`. `agent_tenant` is a one-to-one child of `business_partner`, and Studio actor fields reference `user_account`; Scout does not copy either YAML definition.

The diagrams below intentionally show only Scout-owned tables. Keel-owned tables, including the parent `business_partner`, are omitted because they are dependencies rather than part of the agent domain. Generate both layers together as documented in [README.md](README.md#generate-dialect-specific-ddl).

The YAML files under `schema/` are authoritative. This document explains ownership and relationships; it is not another schema source.

## Storage boundaries

| State | Authoritative location |
|---|---|
| Tenants, policies, immutable definitions, graph metadata, usage, rollout, audit indexes | Relational database |
| Large definitions, turn content, checkpoints, and audit payloads | Object storage by URI and digest |
| Hot session and graph entries | Cache backed by durable relational/object state |
| Turn dispatch and reply frames | Durable queue and short-lived reply broker |
| Embedding vectors | Tenant-partitioned vector index |
| Provider credentials | keel secret provider |

All relational identifiers are lowercase. Tenant-owned relations carry `tenant_id` through their primary or foreign keys. Structured payload columns use canonical JSON stored as `TEXT` so keel can emit PostgreSQL or MySQL DDL from the same YAML.

`execution_step.step_kind_code`, `tenant_runtime_policy.capacity_class_code`, and Studio prompt language columns use keel constant domains through `constant_lookup`; none is a foreign key or a Scout-owned table. The execution kinds are `model`, `tool`, and `knowledge`; the capacity classes are `shared` and `dedicated`.

## REST relationship names

Keel uses each foreign-key constraint name as the generated parent-side relation field, and `rest_api_child.constraint_name` selects that exact relation. These names are therefore API contracts. An owning relationship uses the plural child collection; a second path to another parent adds a role that explains the collection from that parent's perspective.

| Parent | Foreign key / relation field | Child objects |
|---|---|---|
| `business_partner` | `agent_tenants` | `agent_tenant[]` |
| `agent_tenant` | `agent_profiles` | `agent_profile[]` |
| `agent_profile` | `agent_versions` | `agent_version[]` |
| `agent_profile` | `agent_studio_events` | `agent_studio_event[]` |
| `agent_version` | `stable_agent_deployments` | `agent_deployment[]` |
| `agent_version` | `canary_agent_deployments` | `agent_deployment[]` |
| `execution_step` | `outgoing_execution_transitions` | `execution_transition[]` |
| `execution_step` | `incoming_execution_transitions` | `execution_transition[]` |

The YAML foreign-key definitions are the complete relationship-name registry. Any future `foreign_key_lookup` or `rest_api_child` seed must reference those names verbatim. Renaming one is an API compatibility change even when its columns do not change.

## Tenant and model governance

```mermaid
erDiagram
    tenant_current_policy |o--|| tenant_runtime_policy : tenant_current_policies
    priority_class ||--o{ tenant_runtime_policy : priority_runtime_policies
    model_provider ||--o{ model_definition : model_definitions
    model_definition ||--o{ tenant_model_access : model_tenant_accesses
    model_definition ||--o{ model_price : model_prices
    priority_class ||--o{ tenant_model_access : priority_tenant_accesses
    tenant_quota }o--|| agent_tenant : tenant_quotas
    tenant_runtime_policy }o--|| agent_tenant : tenant_runtime_policies
    tenant_model_access }o--|| agent_tenant : tenant_model_accesses

    agent_tenant {
        bigint partner_id PK,FK
        varchar tenant_key UK
        varchar home_region
    }
    tenant_runtime_policy {
        bigint tenant_id PK,FK
        varchar policy_version PK
        varchar priority_class_code FK
        varchar capacity_class_code
        char cost_currency_code FK
    }
    tenant_current_policy {
        bigint tenant_id PK,FK
        varchar policy_version FK
    }
    tenant_quota {
        bigint tenant_id PK,FK
        varchar quota_name PK
        char currency_code FK
    }
    priority_class {
        varchar code PK
        smallint dispatch_rank UK
    }
    currency {
        char code PK
        smallint exponent
    }
    model_provider {
        varchar provider_id PK
        text endpoint_uri
        text credential_ref
    }
    model_definition {
        varchar provider_id PK,FK
        varchar model_id PK
        bigint context_token_limit
        bigint output_token_limit
    }
    model_price {
        varchar provider_id PK,FK
        varchar model_id PK,FK
        char currency_code PK,FK
        timestamp effective_at PK
    }
    tenant_model_access {
        bigint tenant_id PK,FK
        varchar provider_id PK,FK
        varchar model_id PK,FK
        varchar priority_class_code FK
    }
```

`tenant_runtime_policy`, `tenant_quota`, and `model_price` reference `currency`; those edges are omitted from the diagram to keep the layout readable. Policies are immutable by `(tenant_id, policy_version)`, and `tenant_current_policy` changes only the active pointer.

## Agent, tool, and execution control plane

```mermaid
erDiagram
    agent_tenant ||--o{ agent_profile : agent_profiles
    agent_tenant ||--o{ tool_profile : tool_profiles
    agent_profile ||--o{ guardrail_config : guardrail_configs
    agent_profile ||--o{ agent_version : agent_versions
    guardrail_config ||--o{ agent_version : agent_versions_guardrail
    agent_profile ||--o| agent_deployment : agent_deployments
    agent_version ||--o{ agent_deployment : stable_agent_deployments
    agent_version ||--o{ agent_deployment : canary_agent_deployments
    agent_version ||--o| execution_graph : execution_graphs
    agent_version ||--o{ agent_tool_binding : agent_tool_bindings
    tool_profile ||--o{ tool_version : tool_versions
    tool_version ||--o{ tool_egress_rule : tool_egress_rules
    tool_version ||--o{ agent_tool_binding : tool_agent_bindings
    execution_graph ||--|{ execution_step : execution_steps
    execution_graph ||--|| execution_graph_entry : execution_graph_entries
    execution_step ||--o| execution_graph_entry : step_graph_entries
    execution_step ||--o{ execution_transition : outgoing_execution_transitions
    execution_step ||--o{ execution_transition : incoming_execution_transitions

    agent_tenant {
        bigint partner_id PK,FK
    }
    agent_profile {
        bigint tenant_id PK,FK
        varchar agent_id PK
        varchar agent_kind
        varchar display_name
        boolean is_active
    }
    guardrail_config {
        bigint tenant_id PK,FK
        varchar agent_id PK,FK
        varchar guardrail_version PK
        text rules
    }
    agent_version {
        bigint tenant_id PK,FK
        varchar agent_id PK,FK
        varchar agent_version PK
        varchar guardrail_version FK
        text definition
        bigint draft_revision
        bigint prompt_profile_revision
        bigint published_by FK
    }
    agent_deployment {
        bigint tenant_id PK,FK
        varchar agent_id PK,FK
        varchar stable_version FK
        varchar canary_version FK
        smallint canary_percentage
    }
    tool_profile {
        bigint tenant_id PK,FK
        varchar tool_id PK
        varchar display_name
    }
    tool_version {
        bigint tenant_id PK,FK
        varchar tool_id PK,FK
        varchar tool_version PK
        text endpoint_uri
        text credential_ref
    }
    tool_egress_rule {
        bigint tenant_id PK,FK
        varchar tool_id PK,FK
        varchar tool_version PK,FK
        varchar protocol PK
        varchar host PK
        int port PK
    }
    agent_tool_binding {
        bigint tenant_id PK,FK
        varchar agent_id PK,FK
        varchar agent_version PK,FK
        varchar tool_id PK,FK
        varchar tool_version FK
    }
    execution_graph {
        bigint tenant_id PK,FK
        varchar agent_id PK,FK
        varchar agent_version PK,FK
        char graph_digest
    }
    execution_step {
        bigint id PK
        bigint tenant_id FK
        varchar agent_id FK
        varchar agent_version FK
        varchar step_id
        varchar step_kind_code
    }
    execution_graph_entry {
        bigint tenant_id PK,FK
        varchar agent_id PK,FK
        varchar agent_version PK,FK
        bigint execution_step_id FK,UK
    }
    execution_transition {
        bigint source_step_id PK,FK
        varchar transition_key PK
        bigint target_step_id FK
    }
```

Agent, guardrail, and tool versions are immutable. `agent_deployment` holds stable and canary pointers. The compiled graph is normalized into steps, one entry, and directed transitions so publication can validate references before runtime traffic arrives.

## Agent Studio authoring

```mermaid
erDiagram
    agent_profile ||--o| agent_draft : agent_drafts
    agent_profile ||--o{ agent_alias : agent_aliases
    agent_draft ||--o{ agent_prompt_override : agent_prompt_overrides
    agent_alias ||--o{ tenant_prompt_default : tenant_prompt_defaults
    prompt_baseline }o--|| prompt_section : prompt_baselines
    tenant_prompt_default }o--|| prompt_section : section_tenant_prompt_defaults
    agent_prompt_override }o--|| prompt_section : section_agent_prompt_overrides

    agent_profile {
        bigint tenant_id PK,FK
        varchar agent_id PK
        varchar agent_kind
    }
    agent_draft {
        bigint tenant_id PK,FK
        varchar agent_id PK,FK
        bigint draft_revision
        boolean enabled
        boolean require_approval
        text extension
    }
    agent_alias {
        bigint tenant_id PK,FK
        varchar alias_id PK
        varchar agent_kind FK
        varchar agent_id FK
        bigint revision
    }
    prompt_section {
        bigint id PK
        varchar caption UK
        bigint display_order
    }
    prompt_baseline {
        varchar baseline_key PK
        varchar agent_kind PK
        bigint prompt_section_id PK,FK
        varchar language_code PK
    }
    tenant_prompt_default {
        bigint tenant_id PK,FK
        varchar agent_kind PK,FK
        bigint prompt_section_id PK,FK
        varchar language_code PK
    }
    agent_prompt_override {
        bigint tenant_id PK,FK
        varchar agent_id PK,FK
        bigint prompt_section_id PK,FK
        varchar language_code PK
        boolean overwrite
    }
```

`agent_alias.revision` guards both alias changes and tenant defaults for that logical kind, preserving one shared optimistic token. Agent overrides are guarded by `agent_draft.draft_revision`. Platform baseline selection is supplied by `PromptBaselineSelector`; baseline data remains product-owned.

Publication freezes compiled languages and source provenance into `agent_version.definition`. Release metadata records the source revisions, change summary, publisher, publication time, and optional restored version without duplicating live draft rows. `agent_studio_event` keeps queryable lifecycle actions and actor/time metadata; it is omitted from the ER layout to preserve the authoring flow.

## Knowledge

```mermaid
erDiagram
    agent_tenant ||--o{ knowledge_base : knowledge_bases
    knowledge_base ||--o{ knowledge_base_version : knowledge_base_versions
    knowledge_base_version ||--o{ knowledge_document : knowledge_documents
    knowledge_document ||--o{ knowledge_chunk : knowledge_chunks
    agent_version ||--o{ agent_knowledge_binding : agent_knowledge_bindings
    knowledge_base_version ||--o{ agent_knowledge_binding : knowledge_base_agent_bindings

    agent_tenant {
        bigint partner_id PK,FK
    }
    agent_version {
        bigint tenant_id PK
        varchar agent_id PK
        varchar agent_version PK
    }
    knowledge_base {
        bigint tenant_id PK,FK
        varchar knowledge_base_id PK
        varchar display_name
    }
    knowledge_base_version {
        bigint tenant_id PK,FK
        varchar knowledge_base_id PK,FK
        varchar knowledge_version PK
        varchar embedding_provider
        varchar embedding_model
    }
    knowledge_document {
        bigint tenant_id PK,FK
        varchar knowledge_base_id PK,FK
        varchar knowledge_version PK,FK
        varchar document_id PK
        text source_uri
        char content_digest
    }
    knowledge_chunk {
        bigint tenant_id PK,FK
        varchar knowledge_base_id PK,FK
        varchar knowledge_version PK,FK
        varchar document_id PK,FK
        int chunk_no PK
        text content_uri
        text vector_ref
    }
    agent_knowledge_binding {
        bigint tenant_id PK,FK
        varchar agent_id PK,FK
        varchar agent_version PK,FK
        varchar knowledge_base_id PK,FK
        varchar knowledge_version FK
    }
```

Relational rows prove tenant and version ownership. Document and chunk content remain external by URI and digest, while `vector_ref` points to the tenant-partitioned vector index.

## Conversation runtime and metering

```mermaid
erDiagram
    agent_tenant {
        bigint partner_id PK,FK
    }
    agent_version {
        bigint tenant_id PK
        varchar agent_id PK
        varchar agent_version PK
    }
    execution_step {
        bigint id PK
    }
    conversation {
        bigint tenant_id PK,FK
        varchar conversation_id PK
        varchar agent_id FK
        varchar agent_version FK
        varchar end_user_ref
    }
    conversation_turn {
        bigint tenant_id PK,FK
        varchar conversation_id PK,FK
        bigint turn_no PK
        varchar request_id UK
        varchar status_code FK
        text input_uri
        text response_uri
    }
    turn_status {
        varchar code PK
        boolean is_terminal
    }
    step_checkpoint {
        bigint tenant_id PK,FK
        varchar conversation_id PK,FK
        bigint turn_no PK,FK
        int step_no PK
        bigint execution_step_id FK
        varchar idempotency_key UK
        text state_uri
    }
    session_snapshot {
        bigint tenant_id PK,FK
        varchar conversation_id PK,FK
        bigint latest_turn_no FK
        int latest_step_no FK
        bigint revision
    }
    step_idempotency {
        bigint tenant_id PK,FK
        varchar request_id PK,FK
        bigint execution_step_id PK,FK
        varchar status_code FK
        text result_uri
    }
    idempotency_status {
        varchar code PK
        boolean is_terminal
    }
    budget_reservation {
        bigint tenant_id PK,FK
        varchar reservation_id PK
        varchar request_id FK
        varchar status_code FK
        char currency_code FK
    }
    reservation_status {
        varchar code PK
        boolean is_terminal
    }
    usage_event {
        bigint id PK
        bigint tenant_id FK
        varchar conversation_id FK
        bigint turn_no FK
        varchar category_code FK
        char currency_code FK
    }
    usage_category {
        varchar code PK
    }
    agent_run {
        bigint id PK
        bigint tenant_id FK
        varchar agent_id FK
        varchar agent_version FK
        varchar task_kind
        timestamp completed_at
    }
    agent_ops_event {
        bigint id PK
        bigint tenant_id FK
        varchar event
        text detail
        timestamp occurred_at
    }
    business_partner {
        bigint id PK
    }

    agent_tenant ||--o{ conversation : conversations
    agent_version ||--o{ conversation : agent_version_conversations
    conversation ||--|{ conversation_turn : conversation_turns
    turn_status ||--o{ conversation_turn : status_conversation_turns
    conversation_turn ||--o{ step_checkpoint : step_checkpoints
    conversation ||--o| session_snapshot : session_snapshots
    step_checkpoint ||--o| session_snapshot : checkpoint_session_snapshots
    conversation_turn ||--o{ step_idempotency : step_idempotencies
    step_checkpoint }o--|| execution_step : execution_step_checkpoints
    step_idempotency }o--|| execution_step : execution_step_idempotencies
    step_idempotency }o--|| idempotency_status : status_step_idempotencies
    conversation_turn ||--o{ budget_reservation : budget_reservations
    budget_reservation }o--|| reservation_status : status_budget_reservations
    conversation_turn ||--o{ usage_event : usage_events
    usage_event }o--|| usage_category : category_usage_events
    agent_version ||--o{ agent_run : agent_run_versions
    business_partner ||--o{ agent_ops_event : tenant_agent_ops_events
```

`step_checkpoint`, `budget_reservation`, and `usage_event` reference `currency`; those edges are omitted from the diagram to keep the layout readable. `agent_ops_event` deliberately references the Keel business partner directly because provisioning failures can occur before `agent_tenant` exists. The durable checkpoint precedes cache refresh and queue acknowledgement, and `step_idempotency` makes at-least-once delivery replay-safe.

## Platform release safety and audit

```mermaid
erDiagram
    tenant_ring ||--o{ platform_rollout : ring_platform_rollouts
    agent_version ||--o{ contract_test_case : contract_test_cases
    platform_release ||--o{ platform_rollout : platform_rollouts
    platform_release ||--o{ contract_test_run : contract_test_runs
    contract_test_run ||--|{ contract_test_result : contract_test_results
    contract_test_case ||--o{ contract_test_result : case_test_results
    platform_rollout }o--|| rollout_status : status_platform_rollouts
    tenant_ring ||--o{ tenant_ring_member : ring_members
    tenant_ring_member |o--|| agent_tenant : tenant_ring_members
    agent_tenant ||--o{ audit_event : audit_events

    agent_tenant {
        bigint partner_id PK,FK
    }
    agent_version {
        bigint tenant_id PK
        varchar agent_id PK
        varchar agent_version PK
    }
    tenant_ring {
        varchar ring_code PK
        smallint rollout_order UK
    }
    tenant_ring_member {
        bigint tenant_id PK,FK
        varchar ring_code FK
    }
    platform_release {
        varchar platform_version PK
        char artifact_digest
    }
    platform_rollout {
        varchar platform_version PK,FK
        varchar ring_code PK,FK
        smallint traffic_percentage
        varchar status_code FK
    }
    rollout_status {
        varchar code PK
        boolean is_terminal
    }
    contract_test_case {
        bigint tenant_id PK,FK
        varchar test_case_id PK
        varchar agent_id FK
        varchar agent_version FK
        text input_uri
    }
    contract_test_run {
        bigint id PK
        varchar platform_version FK
    }
    contract_test_result {
        bigint run_id PK,FK
        bigint tenant_id PK,FK
        varchar test_case_id PK,FK
        boolean passed
    }
    audit_event {
        bigint id PK
        bigint tenant_id FK
        varchar category
        text payload_uri
    }
```

Agent canaries in `agent_deployment` remain independent from platform rings in `platform_rollout`. Compatibility results, rollout health, and redacted audit events provide the evidence required to advance or restore a release.

## Scout table inventory

| Group | Tables |
|---|---|
| Catalog | `currency`, `priority_class`, `turn_status`, `idempotency_status`, `reservation_status`, `rollout_status`, `usage_category` |
| Tenancy | `agent_tenant`, `tenant_runtime_policy`, `tenant_current_policy`, `tenant_quota` |
| Control plane | `agent_profile`, `agent_draft`, `agent_alias`, `prompt_section`, `prompt_baseline`, `tenant_prompt_default`, `agent_prompt_override`, `guardrail_config`, `tool_profile`, `tool_version`, `tool_egress_rule`, `agent_version`, `agent_tool_binding`, `execution_graph`, `execution_step`, `execution_graph_entry`, `execution_transition`, `agent_deployment`, `knowledge_base`, `knowledge_base_version`, `knowledge_document`, `knowledge_chunk`, `agent_knowledge_binding`, `model_provider`, `model_definition`, `model_price`, `tenant_model_access` |
| Runtime | `conversation`, `conversation_turn`, `step_checkpoint`, `session_snapshot`, `step_idempotency`, `budget_reservation`, `usage_event`, `agent_run`, `agent_ops_event` |
| Release | `platform_release`, `tenant_ring`, `tenant_ring_member`, `contract_test_case`, `contract_test_run`, `contract_test_result`, `platform_rollout`, `audit_event` |
