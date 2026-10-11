# Configuration: every Scout limit and the flag behind it

Scout libraries read no environment and hold no package globals: every limit is a constructor or
struct field validated on first use, and unsafe zero/negative combinations are rejected loudly.
A downstream binary declares one `flag` per value in `common/variables.go` and passes it in at
composition time. An `agent_*` name below is a catalog flag; a `--` name is the recommended
spelling for a value with no catalog flag, where only the mapping matters.

## Repository-owned flags (`scout_config.go`)

These are the only values Scout reads through keel's `application_config_flag` catalog rather than
through a constructor field. `LoadConfig` fails loudly when a flag is missing from the seed, so the
catalog and `ScoutConfig` cannot drift. A zero or empty value takes the documented default wherever
the Meaning says so; elsewhere zero is a real setting or is refused.

Flag, Type, and Default are the seed row. Min and Max are the bounds Scout enforces, either when
`ScoutConfig.Apply` loads the catalog or when the consuming component is constructed or composed; a
flag name there means the bound is that flag's value, and "—" means nothing enforces a bound.

`needs_restart` in the seed is catalog metadata; Scout does not act on it. When keel publishes a
changed configuration, only two consumers pick it up while running: `ActiveControls` rebuilds the
rate limiter and model capacity scheduler from the `agent_*_rate`, `agent_*_burst`,
`agent_max_tenants`, and `agent_model_capacity*` flags, and the release activation worker calls
its settings function on every poll, so one that returns `scout.Config().ReleaseActivationSettings()`
follows the `agent_activation_*` flags. Every other flag is copied into a component when the
product composes it, so a change takes effect only after the product re-composes that component or
restarts, whatever its `needs_restart` value says.

### Models, admission, and rate limits

`NewControlSet` and `ActiveControls` build the tenant rate limiter and the model capacity scheduler
from these flags; a rate and its burst must be both zero (that lane off) or both positive.

| Flag | Type | Default | Min | Max | Meaning |
|---|---|---|---|---|---|
| `agent_max_tokens` | int | 8192 | 1 | — | Output tokens one agent model completion may produce, passed as `BaseDataPlane.MaxOutputTokens`; the data plane refuses to compose without a positive value. |
| `agent_temperature` | float | `""` | 0.0 | 2.0 | Sampling temperature `provider.Factory` sends only to models whose catalog entry has the `sampling` capability; empty sends none and lets each model use its own default. |
| `agent_model_region` | string | `""` | — | — | Residency region `TableCandidateCatalog` stamps on a candidate model that has no `model_route` row of its own; empty leaves that model's region unknown. |
| `agent_turn_rate` | float | 2 | 0 | — | Turns per second one tenant may start (token-bucket refill); 0 turns the per-tenant turn lane off and needs `agent_turn_burst` 0. |
| `agent_turn_burst` | int | 10 | 0 | — | Turns one tenant may start at once above `agent_turn_rate`; 0 only together with a 0 rate. |
| `agent_tool_rate` | float | 10 | 0 | — | Governed tool calls per second one tenant may make; 0 turns the lane off and needs `agent_tool_burst` 0. |
| `agent_tool_burst` | int | 20 | 0 | — | Tool calls one tenant may make at once above `agent_tool_rate`; 0 only together with a 0 rate. |
| `agent_model_rate` | float | 2 | 0 | — | Model calls per second one tenant may make; 0 turns the lane off and needs `agent_model_burst` 0. |
| `agent_model_burst` | int | 5 | 0 | — | Model calls one tenant may make at once above `agent_model_rate`; 0 only together with a 0 rate. |
| `agent_fleet_turn_rate` | float | 100 | 0 | — | Turns per second the whole process admits across all tenants; 0 turns the fleet turn lane off and needs `agent_fleet_turn_burst` 0. |
| `agent_fleet_turn_burst` | int | 200 | 0 | — | Turns the process admits at once above `agent_fleet_turn_rate`; 0 only together with a 0 rate. |
| `agent_fleet_tool_rate` | float | 500 | 0 | — | Tool calls per second the whole process admits; 0 turns the lane off and needs `agent_fleet_tool_burst` 0. |
| `agent_fleet_tool_burst` | int | 1000 | 0 | — | Tool calls the process admits at once above `agent_fleet_tool_rate`; 0 only together with a 0 rate. |
| `agent_fleet_model_rate` | float | 100 | 0 | — | Model calls per second the whole process admits; 0 turns the lane off and needs `agent_fleet_model_burst` 0. |
| `agent_fleet_model_burst` | int | 200 | 0 | — | Model calls the process admits at once above `agent_fleet_model_rate`; 0 only together with a 0 rate. |
| `agent_max_tenants` | int | 4096 | 1 | — | Tenants whose rate-limit buckets the in-memory limiter keeps per lane; size it above the number of concurrently active tenants. |
| `agent_model_capacity_pool` | string | `shared` | non-empty | — | Name of the model capacity pool the fair slot scheduler reports; blank after trimming refuses the control set. |
| `agent_model_capacity` | int | 32 | 1 | — | Model calls that may run concurrently in this process; further calls wait in the fair queue. |
| `agent_model_max_waiters` | int | 4096 | 1 | — | Model calls that may wait for a capacity slot; one more is refused instead of queued. |

### Tool loop and turn execution

`ScoutConfig.ToolLoopLimits()` feeds `NewToolLoopExecutor`; a step's configuration and a release's
`limits` setting may only narrow these ceilings.

| Flag | Type | Default | Min | Max | Meaning |
|---|---|---|---|---|---|
| `agent_loop_max_iterations` | int | 12 | 1 | — | Model decisions one tool-loop step may make before it fails with an execution limit. |
| `agent_loop_max_tool_calls` | int | 24 | 1 | — | Governed tool calls one tool-loop step may make; also the tool-call count the default turn estimator reserves. |
| `agent_loop_max_tokens` | int | 200000 | 1 | — | Input plus output tokens one tool-loop step may spend; also the token count the default turn estimator reserves against the budget. |
| `agent_loop_max_cost` | int | 0 | 0 | — | Cost in currency minor units one tool-loop step may spend, and the cost the default turn estimator holds; 0 leaves cost to the turn budget, and an agent with a rolling, day, or month cost budget then fails its turns with `no_cost_quote`. |
| `agent_loop_max_repeats` | int | 3 | 1 | — | Identical calls of one tool before a loop is declared, in the tool loop and in the data plane's default loop detector. |
| `agent_loop_deadline` | int | 300 | 1 | < `agent_queue_lease` and `agent_step_claim_lease` | Wall-clock seconds one tool-loop step may run; both leases must outlast it, or a second worker replays a step that is still running. |
| `agent_loop_max_call_revisions` | int | 8 | 0 | — | Times approval deciders may revise one proposed tool call within a turn; 0 uses the default 8. |
| `agent_turn_max_steps` | int | 16 | 1 | — | Execution-graph steps one turn may run in `TurnRuntime`. |
| `agent_tool_timeout` | int | 30 | 1 | — | Seconds one governed tool call may run when the tool registers no timeout of its own (tool registry and governed gateway). |
| `agent_tool_max_attempts` | int | 3 | 1 | — | Deliveries of one tool call (first try plus retries) when the tool registers no ceiling of its own. |

### State, queue, sessions, and caches

`ScoutConfig.DataPlaneSettings()` feeds `BaseDataPlane.Settings`; `BaseDataPlane.Compose` refuses a
value outside these bounds.

| Flag | Type | Default | Min | Max | Meaning |
|---|---|---|---|---|---|
| `agent_state_bucket` | string | `""` | non-empty | — | Private object-storage bucket for turn input and conversation state; it must equal the bucket the injected storage is bound to, and empty refuses to compose the data plane (`ErrNotReady`). |
| `agent_state_max_bytes` | int | 4194304 | 1 | — | Largest single state or turn-input object in bytes the state store writes or reads. |
| `agent_queue_partitions` | int | 64 | 1, ≥ `agent_queue_shards` | — | Fixed pool of turn-queue partitions tenants hash into; changing it reshuffles which partitions every tenant uses. |
| `agent_queue_shards` | int | 4 | 1 | `agent_queue_partitions` | Partitions one tenant's turns spread over; more shards let one busy tenant use more workers in parallel. |
| `agent_queue_max_attempts` | int | 5 | 1 | — | Deliveries of one turn before it is dead-lettered. |
| `agent_queue_lease` | int | 900 | > `agent_loop_deadline` | — | Seconds a claimed turn stays leased to its worker before another worker may reclaim it. |
| `agent_queue_batch` | int | 8 | 1 | — | Turns one runtime worker tick claims. |
| `agent_step_claim_lease` | int | 360 | > `agent_loop_deadline` | — | Seconds a step's idempotency claim blocks other workers from running the same step. |
| `agent_session_cache_size` | int | 4096 | 1 | — | Conversations held in the in-memory session cache; also the conversations the default loop detector and tenants and agents the default cost breaker track. |
| `agent_session_cache_ttl` | int | 300 | 1 | — | Seconds an in-memory session snapshot lives before it is re-read from the durable store. |
| `agent_graph_cache_size` | int | 1024 | 1 | — | Compiled execution graphs held in the in-memory graph cache. |
| `agent_graph_cache_ttl` | int | 3600 | 1 | — | Seconds an in-memory execution graph lives before it is re-read. |
| `agent_session_revision_memory` | int | 4096 | 0 | — | Conversations whose last written revision a process remembers so it can discard stale cache hits; 0 uses the default 4096. |
| `agent_result_card_max_bytes` | int | 65536 | 0 | — | Largest response in bytes copied into turn history; a larger one is stored by reference; 0 uses the default 65536. |
| `agent_cost_window` | int | 3600 | 0 | — | Seconds of trailing spend the data plane's default cost breaker sums; 0 uses the default 3600. The default breaker has no limits, so it records spend without tripping. |
| `agent_run_retention_days` | int | 0 | 0 | — | Days of settled agent run activity `AgentRunStore.Purge` keeps when a product's retention worker calls it; 0 keeps activity forever. |
| `agent_run_page_max` | int | 500 | 0 | — | Largest page of settled agent runs one `AgentRunStore` query returns; 0 uses the default 500. |

### Reply delivery and cancellation

| Flag | Type | Default | Min | Max | Meaning |
|---|---|---|---|---|---|
| `agent_reply_retention` | int | 600 | 0 | — | Seconds each reply frame stays in the delivery cache for streaming and reconnects; after that a client reads the durable turn record; 0 uses the default 600. |
| `agent_reply_poll_ms` | int | 500 | 0 | — | Milliseconds a reply subscriber waits before re-reading the cache after a lost wake-up, which bounds delivery latency; 0 uses the default 500. |
| `agent_reply_record_read` | int | 5 | 0 | — | Seconds between a reply subscriber's durable turn-record reads, which throttles database load from waiting clients; 0 uses the default 5. |
| `agent_cancel_poll_ms` | int | 1000 | 0 | — | Milliseconds between a running turn's cancellation reads, which bounds how long a turn outlives its cancellation; 0 uses the default 1000. |
| `agent_cancel_max_read_failures` | int | 3 | 0 | — | Consecutive failed cancellation reads after which a running turn is stopped; 0 uses the default 3. |

### Authority, guardrails, and restrictions

| Flag | Type | Default | Min | Max | Meaning |
|---|---|---|---|---|---|
| `agent_max_scope_depth` | int | 8 | 1 | 64 | Longest scope chain a release's settings may compile over (`scope.Compiler.MaxDepth`, `StudioService.MaxScopeDepth`); a longer chain fails compilation. |
| `agent_max_delegation_hops` | int | 4 | — | — | Longest authority chain a principal may present (`principal.ChainVerifier`, where 0 or less takes 4) and the re-delegation depth a Studio grant may convey (`StudioDelegations.MaxDepth`). |
| `agent_work_item_max_depth` | int | 32 | 0 | — | Deepest chain of delegated work items `TableWorkItemStore` accepts and walks; 0 uses the default 32. |
| `agent_credential_ttl` | int | 300 | — | — | Lifetime in seconds of a just-in-time tool credential (`BoundCredentialProvider.DefaultTTL`) when the tool binding sets no positive `MaxTTL` of its own. |
| `agent_guardrail_max_input_bytes` | int | 262144 | 1 | — | Baseline byte ceiling on turn input, tool arguments, and retrieved content, and the most knowledge bytes a release may read whole into a turn. |
| `agent_guardrail_max_output_bytes` | int | 1048576 | 1 | — | Baseline byte ceiling on model and tool output. |
| `agent_restriction_cache_ttl_ms` | int | 5000 | 0 | — | Milliseconds a read tenant restriction layer is reused before the store is asked again, so a Studio change takes up to this long to apply; 0 uses the default 5000. |
| `agent_restriction_cache_entries` | int | 4096 | 0 | — | Tenant restriction layers held in the in-memory cache; 0 uses the default 4096. |

### Approvals and MCP confirmations

The route flags tighten every `approval_routing` value Studio saves, imports, or adopts
(`ScoutConfig.ApprovalRoutingLimits()` into `StudioService.ApprovalRoutingLimits`); they may only
tighten the structural bounds shown as Max.

| Flag | Type | Default | Min | Max | Meaning |
|---|---|---|---|---|---|
| `agent_approval_deadline` | int | 3600 | — | — | Seconds a reviewer has before escalation (`approval.Gate.Deadline`); 0 or less leaves a request open with no escalation, and a matching route's `deadline_seconds` replaces it. |
| `agent_approval_max_candidates` | int | 20 | 0 | — | Eligible holders one approver resolution considers (`approval.ReleaseRouter.MaxCandidates`); 0 uses the default 20. |
| `agent_approval_max_asks_per_call` | int | 8 | 0 | — | Approval requests one tool call may owe (`approval.Gate.MaxAsksPerCall`); lowering it leaves a call's later recorded requests unread; 0 uses the default 8. |
| `agent_approval_max_revised_argument_bytes` | int | 65536 | 0 | — | Largest revised arguments in bytes an edited approval verdict may carry (`approval.Resumer.MaxRevisedArgumentBytes`); 0 uses the default 65536. |
| `agent_approval_max_routes_per_binding` | int | 32 | 0 | 32 | Routes one `approval_routing` binding may hold; 0 uses 32. |
| `agent_approval_max_routes` | int | 64 | 0 | 64 | Effective approval routes one published agent may hold, also enforced at publish by `StudioApprovalRouting.MaxRoutes`; 0 uses 64. |
| `agent_approval_max_route_tools` | int | 16 | 0 | 16 | Tool patterns one approval route may match; 0 uses 16. |
| `agent_approval_min_route_duration` | int | 60 | 0 or 60 | 2592000, ≤ `agent_approval_max_route_duration` and `agent_approval_max_route_window` | Shortest route deadline or extension in seconds; it may only raise the floor of 60; 0 uses 60. |
| `agent_approval_max_route_duration` | int | 2592000 | 0 or 60, ≥ `agent_approval_min_route_duration` | 2592000 | Longest route deadline or extension in seconds (30 days); 0 uses 2592000. |
| `agent_approval_max_route_window` | int | 7776000 | 0 or 60, ≥ `agent_approval_min_route_duration` | 7776000 | Longest route deadline plus all its extensions in seconds (90 days); 0 uses 7776000. |
| `agent_confirmation_max_document_bytes` | int | 1048576 | 0 | — | Largest MCP confirmation payload, preview, requirements, or result in bytes (`confirmation.TableStore` and `confirmation.Executor`); a larger result is recorded unknown and never run again; 0 uses the default 1048576. |
| `agent_confirmation_batch` | int | 100 | 0 | `agent_confirmation_max_batch` | MCP confirmations one list or sweep reads when its caller names no size; 0 uses the default 100. |
| `agent_confirmation_max_batch` | int | 1000 | 0, else ≥ `agent_confirmation_batch` | — | Most MCP confirmations one list or sweep may read; 0 uses the default 1000. |
| `agent_confirmation_lease` | int | 60 | 0 | 2147483647 | Seconds one MCP confirmation execution claim lasts (`confirmation.Executor.Lease`), renewed every third of it; keep it well above a confirmed run's normal duration; 0 uses the default 60. |

### Studio

| Flag | Type | Default | Min | Max | Meaning |
|---|---|---|---|---|---|
| `agent_studio_max_agents_per_type` | int | 20 | — | — | Agents of one type a tenant may keep, archived ones included (`StudioService.MaxAgentsPerType`); 0 refuses every Studio create. |
| `agent_studio_max_templates_per_type` | int | 20 | — | — | Templates of one agent type a tenant may own (`StudioService.TemplateLimits.MaxPerType`); 0 refuses every new template. |
| `agent_studio_max_template_versions` | int | 50 | — | — | Versions one template may keep (`StudioService.TemplateLimits.MaxVersions`); 0 refuses every template save. |
| `agent_studio_max_portable_bindings` | int | 1024 | 0 | — | Bindings one portable document or template may carry; 0 uses the default 1024. |
| `agent_studio_max_references` | int | 1024 | 0 | — | Reference mappings one import or template create may name; 0 uses the default 1024. |
| `agent_studio_max_portable_extension_bytes` | int | 262144 | 0 | — | Bytes of one portable draft extension; 0 uses the default 262144. |
| `agent_studio_max_template_audience` | int | 100 | 0 | — | Tenants one template publication may name; 0 uses the default 100. |
| `agent_studio_usable_templates` | int | 200 | 0 | — | Templates of each owner class (own, shared, platform) one template list returns (`TableAgentTemplateStore.UsableLimit`); 0 uses the default 200. |
| `agent_studio_max_governance_items` | int | 32 | 0 | — | Items one Studio-authored governance list may hold (`StudioGovernance.MaxItems`); 0 uses the default 32. |
| `agent_studio_max_comments_shown` | int | 500 | 0 | — | Newest draft comments one list returns; 0 uses the default 500. |
| `agent_studio_max_restriction_entries` | int | 256 | 0 | — | Denials, and separately guardrail rules, one tenant restriction layer written through Studio may hold (`policy.StudioRestrictions.MaxEntries`); 0 uses the default 256. |
| `agent_studio_report_window_days` | int | 92 | 0 | — | Days one Studio usage or feedback report may span; a wider window is refused rather than cut short; 0 uses the default 92. |
| `agent_studio_usage_max_lines` | int | 1000 | 0 | — | Lines one Studio usage report may return; a report that would return more is refused; 0 uses the default 1000. |
| `agent_pause_batch` | int | 200 | 0 | — | Live turns one Studio pause listing asks to stop per round when an agent is paused with the cancel policy; 0 uses the default 200. |
| `agent_pilot_max_members` | int | 200 | 0 | — | Open pilot memberships one agent may hold, which bounds what starting a conversation reads (`release.StudioPilots.MaxMembers`); 0 refuses every pilot add. |
| `agent_pilot_position_timeout` | int | 2 | — | — | Seconds one product position lookup may take while a new conversation chooses its release (`release.TablePilotResolver.PositionTimeout`); a late answer fails the resolution; 0 or less uses the default 2. |
| `agent_governance_required_fields` | string | `""` | — | — | Comma-separated Charter governance fields every release must carry, from `purpose`, `accountable`, `responsibilityIds`, `triggers`, `requiredInputs`, `expectedOutcomes`, `capabilityIds`, `policies`, `confidenceBoundaries`, `escalationConditions`; empty requires every field Charter's AgentDefinition requires, `none` requires none, and an unknown or repeated name fails the load. |

### Evaluation

The judge bounds apply only when a judge model is configured; `ScoutConfig.RubricJudgeSettings()`
validates them at load.

| Flag | Type | Default | Min | Max | Meaning |
|---|---|---|---|---|---|
| `agent_evaluation_max_open_runs` | int | 4 | 0 | — | Queued and running evaluations one agent may hold (`StudioEvaluation.MaxOpenRuns`); 0 refuses every evaluation. |
| `agent_evaluation_concurrency` | int | 4 | 0 | — | Examples of one evaluation run executing at once; below 2 runs them one at a time. A run may overshoot its cost ceiling by at most this many examples' cost. |
| `agent_evaluation_max_run_cost` | int | 10000 | 1 | — | Minor units of `agent_evaluation_currency` one evaluation run may spend, judge spend included, before it stops early. |
| `agent_evaluation_currency` | string | `""` | 3 characters | 3 characters | ISO 4217 currency every evaluation cost is priced in; a result priced in another currency fails its run, and empty refuses to compose the evaluation worker. |
| `agent_evaluation_lease` | int | 900 | 1 | — | Seconds each recorded result extends an evaluation run's lease; keep it above `agent_loop_deadline` and the slowest example, or another worker reclaims a live run. |
| `agent_evaluation_batch` | int | 4 | 1 | — | Evaluation runs one worker tick claims. |
| `agent_evaluation_max_attempts` | int | 3 | 1 | — | Claims of one evaluation run before an expired lease fails it. |
| `agent_evaluation_case_max_bytes` | int | 32768 | 0 | — | Largest saved test-case input or expected behavior in bytes (`GoldenCaseStore.MaxInputBytes`); 0 uses the default 32768. |
| `agent_evaluation_max_cases` | int | 500 | 0 | — | Saved test cases one golden set may hold (`GoldenCaseStore.MaxCases`); 0 uses the default 500. |
| `agent_evaluation_page_size` | int | 50 | 0 | `agent_evaluation_max_page_size` | Evaluation results one Studio page returns when the caller names no limit; 0 uses the default 50. |
| `agent_evaluation_max_page_size` | int | 200 | 0, else ≥ `agent_evaluation_page_size` | — | Largest evaluation result page a Studio caller may ask for; 0 uses the default 200. |
| `agent_evaluation_judge_provider` | string | `""` | — | — | Provider of the rubric judge model; empty together with the model and version composes no judge, and setting only some of the three fails the load. |
| `agent_evaluation_judge_model` | string | `""` | — | — | Rubric judge model id; required when any judge flag names a model. |
| `agent_evaluation_judge_model_version` | string | `""` | — | — | Model version the rubric judge is pinned to; required when any judge flag names a model. |
| `agent_evaluation_judge_min_score` | int | 1 | 0 | < `agent_evaluation_judge_max_score` | Lowest integer rubric grade. |
| `agent_evaluation_judge_max_score` | int | 5 | > `agent_evaluation_judge_min_score` | `agent_evaluation_judge_min_score` + 100 | Highest integer rubric grade; a grade is normalized to 0..1 over the range. |
| `agent_evaluation_judge_pass_score` | int | 4 | `agent_evaluation_judge_min_score` | `agent_evaluation_judge_max_score` | Lowest rubric grade that passes. |
| `agent_evaluation_judge_max_tokens` | int | 512 | 1 | — | Output tokens one rubric verdict may use. |
| `agent_evaluation_judge_max_input_bytes` | int | 131072 | 1 | — | Largest rubric judge prompt in bytes; a case whose prompt is larger fails its run. |
| `agent_evaluation_judge_max_verdict_bytes` | int | 8192 | 0 | — | Bytes of one raw rubric verdict, rationale included; a larger verdict is an evaluator error; 0 uses the default 8192. |
| `agent_evaluation_gate_max_results` | int | 2000 | 0 | — | Results of one evaluation run an evaluation gate decision may read (`StudioEvaluationGate.MaxResults`); a larger run refuses the decision; 0 uses the default 2000. |
| `agent_evaluation_baseline_max_bytes` | int | 32768 | 0 | — | Bytes of one recorded human baseline output (`EvaluationBaselineStore.MaxOutputBytes`); 0 uses the default 32768. |
| `agent_evaluation_max_baselines` | int | 20 | 0 | — | Human baselines one golden set version may hold (`EvaluationBaselineStore.MaxBaselines`); 0 uses the default 20. |
| `agent_evaluation_compare_page_bytes` | int | 1048576 | 0 | — | Bytes of stored answers one Studio comparison page returns (`StudioEvaluation.MaxPageAnswerBytes`); an answer past it is omitted and marked; 0 uses the default 1048576. |
| `agent_review_feedback_threshold` | int | 2 | 0 | 5 | Highest turn rating that queues a rated turn for Studio review (`ReviewIntake.FeedbackThreshold`); 0 queues none. |
| `agent_review_max_pending` | int | 1000 | 0 | — | Pending sampled and rated turns one agent's review queue holds before new ones are skipped (`ReviewQueueStore.MaxPending`); concurrent enqueues may overshoot it slightly; 0 uses the default 1000. |
| `agent_review_page_size` | int | 50 | 0 | `agent_review_max_page_size` | Review items one Studio page returns when the caller names no limit; 0 uses the default 50. |
| `agent_review_max_page_size` | int | 200 | 0, else ≥ `agent_review_page_size` | — | Largest review item page a Studio caller may ask for; 0 uses the default 200. |
| `agent_review_max_turn_bytes` | int | 1048576 | 0 | — | Bytes of a reviewed turn's input or response Studio reads or promotes (`StudioReview.MaxTurnBytes`); a larger one is refused; 0 uses the default 1048576. |

### Release activation

`ScoutConfig.ReleaseActivationSettings()` feeds `ReleaseActivationWorker` and
`ReleaseActivations.ActivateDue`, which re-read it on every poll.

| Flag | Type | Default | Min | Max | Meaning |
|---|---|---|---|---|---|
| `agent_activation_batch` | int | 20 | 1 | — | Due scheduled release activations one worker poll handles. |
| `agent_activation_poll` | int | 30 | 1 | — | Seconds between polls of the release activation worker; a transient failure retries after attempts × this period. |
| `agent_activation_max_attempts` | int | 5 | 1 | — | Attempts of a scheduled activation that fails transiently before it is refused with `attempts_exhausted`. |
| `agent_activation_timeout` | int | 120 | 0 | — | Seconds one scheduled activation's checks and swap may take; 0 uses the default 120. |
| `agent_activation_max_lead_days` | int | 366 | 0 | — | Days ahead of publication a publish may schedule its activation (`StudioService.MaxActivationLead`); 0 uses the default 366. |

### Observability and knowledge

| Flag | Type | Default | Min | Max | Meaning |
|---|---|---|---|---|---|
| `agent_audit_page_size` | int | 100 | 0 | `agent_audit_max_page_size` | Decision records an audit query page returns when it names no limit (`TableAuditSink.PageSize`); 0 uses the default 100. |
| `agent_audit_max_page_size` | int | 1000 | 0, else ≥ `agent_audit_page_size` | — | Largest decision-record page an audit query may ask for; a larger request is clamped to it; 0 uses the default 1000. |
| `agent_safety_event_max_page` | int | 500 | 0 | — | Largest page of safety events one query may ask for; a larger limit is refused; 0 uses the default 500. |
| `agent_metric_label_max_length` | int | 64 | 0 | — | Bytes of one fleet metric label value (`observability.LabelPolicy.MaxValueLength`); a longer value is refused, which keeps free text out of metric dimensions; 0 uses the default 64. |
| `agent_knowledge_max_source_bytes` | int | 33554432 | 0 | — | Bytes of one knowledge source object the ingestion loader reads (`knowledge.ObjectStorageLoader.MaxBytes`); 0 uses the default 33554432. |
| `agent_knowledge_rrf_offset` | int | 60 | 0 | — | Reciprocal rank fusion constant k of hybrid retrieval (`knowledge.HybridRetriever.RRFOffset`), scoring rank r as 1/(k+r); a larger k flattens the advantage of top ranks; 0 uses the default 60. |

### Defaults

| Flag | Type | Default | Min | Max | Meaning |
|---|---|---|---|---|---|
| `agent_default_language` | string | `en-US` | — | 16 bytes | Language code a published runtime or Studio draft test falls back to when the agent has no prompt in the requested language; empty uses `en-US`. |
| `agent_default_priority_class` | string | `standard` | — | 30 bytes | `priority_class` code a model access grant takes when the caller names none (`controlplane.ModelAccess.PriorityClass`); empty uses `standard`. |
| `agent_default_test_task` | string | `""` | — | — | Task text a Studio draft test runs when the caller sends none (`BaseDraftTester.DefaultTask`); empty uses Scout's built-in sample task. |

## Admission (`service/isolation`)

| Field | Flag | Rule |
|---|---|---|
| `RateLimiterConfig.{Turn,Tool,Model}.{PerSecond,Burst}` | `agent_turn_rate`, `agent_turn_burst`, `agent_tool_rate`, `agent_tool_burst`, `agent_model_rate`, `agent_model_burst` | both zero (lane off) or both positive; `NewControlSet` builds the limiter from these flags |
| `RateLimiterConfig.Fleet{Turn,Tool,Model}` | `agent_fleet_turn_rate`, `agent_fleet_turn_burst`, `agent_fleet_tool_rate`, `agent_fleet_tool_burst`, `agent_fleet_model_rate`, `agent_fleet_model_burst` | process-wide ceiling, same rule |
| `RateLimiterConfig.MaxTenants` | `agent_max_tenants` | positive; bounds the tenant bucket map |
| `DistributedRateLimiterConfig.{Turn,Tool,Model}.{Limit,Window}` | `--drate_{turn,tool,model}_limit`, `--..._window` | fixed window shared by all replicas; both zero or both positive |
| `DistributedRateLimiterConfig.Fleet{Turn,Tool,Model}` | `--drate_fleet_{turn,tool,model}_{limit,window}` | same rule |
| `DistributedRateLimiterConfig.KeyPrefix` | `--drate_key_prefix` | required; namespaces the shared counters |
| `DistributedRateLimiterConfig.StoreTimeout` | `--drate_store_timeout` | positive; a slower store counts as unreachable |
| `DistributedRateLimiterConfig.FallbackFraction` | `--drate_fallback_fraction` | in (0, 1]; fleet overshoot while degraded is `replicas × fraction × limit` |
| `DistributedRateLimiterConfig.FallbackMaxTenants` | `--drate_fallback_max_tenants` | positive |
| `DistributedRateLimiterConfig.RecoveryProbe` | `--drate_recovery_probe` | positive; degraded dwell before one probe |
| `modelgateway.FairCapacityConfig.{Pool,Slots,MaxWaiters}` | `agent_model_capacity_pool`, `agent_model_capacity`, `agent_model_max_waiters` | pool non-empty; slots and waiters positive; they size keel's `FairSlotLimiter` |

## Budgets, cost, and loops (`service/isolation`)

| Field | Flag | Rule |
|---|---|---|
| `BudgetLedger.ReservationTTL` | `--budget_reservation_ttl` | zero takes 15m; otherwise at least 1s |
| `BudgetLedger.Releases` | — | required: the data plane's `PrincipalLimits`; `BaseDataPlane.Compose` refuses a ledger without it |
| `ReleaseLimits.Fallback` | — | budget of a release that binds none, reported with source `fallback`; zero is unbounded, and a bounded one needs a valid period |
| `WindowedCostBreaker.{Tenant,Agent,Fleet}Limit` | `--cost_{tenant,agent,fleet}_limit` | minor units per window; zero disables that scope, negative rejected |
| `WindowedCostBreaker.{Currency,Window,Buckets,MaxEntries}` | `--cost_currency`, `agent_cost_window`, `--cost_buckets`, `agent_session_cache_size` | window positive and larger than the bucket count; entries positive; the data plane's default governor takes the window and entries from these flags, a zero window meaning 1h |
| `MemoryLoopDetector.{Threshold,MaxConversations}` | `agent_loop_max_repeats`, `agent_session_cache_size` | positive; the data plane's default governor takes them from these flags |
| `MemoryLoopDetector.{Window,MaxFingerprints}` | `--loop_window`, `--loop_max_fingerprints` | non-negative; zero keeps history until `Reset` / takes 1024 |
| `TenantRuntimePolicy.{MaxSteps,MaxTokens,MaxCostMinorUnits,TurnTimeout}` | `--turn_max_{steps,tokens,cost}`, `--turn_timeout` | per-tenant policy; flags supply the default row |

A release's `budget` setting is data, not a flag: `period` is `turn`, `rolling` (`window_seconds` 60 to 31,622,400), `day`, or `month` (an IANA `timezone` of at most 64 characters, never `Local`); `tokens` and `cost_minor_units` run from 0 to 10^18, 0 leaving that axis unlimited, and a value caps at least one; a cost ceiling names a `currency` and an `exponent` (0–6) that must match the currency catalog. A value without a period is refused.

## Latency budget (`service/isolation`)

| Field | Flag | Rule |
|---|---|---|
| `LatencyBudgetConfig.{PromptBuild,Guardrail}.{Min,Max}` | `--budget_{prompt,guardrail}_{min,max}` | `0 < Min <= Max`; both stages are on the minimum path |
| `LatencyBudgetConfig.{Embedding,Retrieval,Rerank}.{Min,Max}` | `--budget_{embedding,retrieval,rerank}_{min,max}` | `0 <= Min <= Max`; `Max = 0` disables the optional stage |
| `StaticStageLatencyModel.Table` | `--stage_p95_<stage>` | starting p95 table; `MinGeneration` must be positive and `<= Generation` |

## Data and knowledge plane

| Field | Flag | Rule |
|---|---|---|
| `knowledge.BatchingEmbedder.{MaxBatch,MaxWait,Timeout}` | `--embed_max_batch`, `--embed_max_wait`, `--embed_timeout` | non-negative; zero takes 16 / 25ms / 30s |
| `knowledge.HybridRetriever.{Overfetch,MinRerankBudget}` | `--retrieval_overfetch`, `--retrieval_min_rerank_budget` | overfetch positive; budget non-negative |
| `dataplane.MemoryReplyHub.{SubscriberBuffer,RetainedFrames,MaxStreams}` | `--reply_{subscriber_buffer,retained_frames,max_streams}` | non-negative; zero takes 16 / 64 / 4096 |
| `dataplane.MemoryReplyHub.{Linger,IdleTTL}` | `--reply_linger`, `--reply_idle_ttl` | non-negative; zero takes 30s / 10m |
| `dataplane.CacheReplyHub.{Retention,PollInterval,RecordReadInterval}` | `agent_reply_retention`, `agent_reply_poll_ms`, `agent_reply_record_read` | negative refused; zero takes 10m / 500ms / 5s |
| `dataplane.TableTurnCanceller.{PollInterval,MaxReadFailures}` | `agent_cancel_poll_ms`, `agent_cancel_max_read_failures` | non-positive takes 1s / 3 |

## Principals and scoped configuration (`service/scope`, `service/principal`)

| Field | Flag | Rule |
|---|---|---|
| `scope.Compiler.MaxDepth` | `agent_max_scope_depth` | non-negative; zero takes 8. A longer scope chain fails compilation |
| `principal.ChainVerifier.MaxDepth` | `agent_max_delegation_hops` | non-negative; zero takes 4. Bounds the chain regardless of what a grant conveys |
| `approval.Gate.Deadline` | `agent_approval_deadline` | seconds; zero leaves a request open with no escalation; a matching route's `deadline_seconds` replaces it |
| `approval_routing` setting | — | at most 32 routes per binding and 64 per agent, 16 tool patterns per route, 3 escalation steps; deadlines and extensions 60s–30d, a whole route at most 90 days; one tool call owes at most 8 requests. The route flags (`agent_approval_max_routes_per_binding` through `agent_approval_max_route_window`) may only tighten what Studio writes, and `agent_approval_max_asks_per_call` sets the per-call requests |
| `approval.Resumer.MaxRevisedArgumentBytes` | `agent_approval_max_revised_argument_bytes` | negative refused; zero takes 64 KiB; bounds an edited verdict's revised arguments |
| `controlplane.StudioService.MaxAgentsPerType` | `agent_studio_max_agents_per_type` | non-negative; zero refuses every create |
| `controlplane.StudioDelegations.MaxDepth` | `agent_max_delegation_hops` | non-negative; the re-delegation depth a Studio grant may convey |
| `toolgateway.BoundCredentialProvider.DefaultTTL` | `agent_credential_ttl` | seconds; a binding's own positive `MaxTTL` replaces it |
| `observability.TableAuditSink.{PageSize,MaxPageSize}` | `agent_audit_page_size`, `agent_audit_max_page_size` | non-negative; zero takes 100 / 1000; the page size may not exceed the largest page, and a larger request is clamped to it |
| `dataplane.ToolLoopExecutor.Limits` | `agent_loop_max_iterations`, `agent_loop_max_tool_calls`, `agent_loop_max_tokens`, `agent_loop_max_repeats`, `agent_loop_deadline` | all positive, or construction fails; a step's configuration and the release's `limits` setting may only narrow them |
| `dataplane.ToolLoopExecutor.Limits.{MaxCostMinorUnits,MaxCallRevisions}` | `agent_loop_max_cost`, `agent_loop_max_call_revisions` | non-negative; a zero cost leaves cost to the turn budget and the delegated bound, and zero revisions take 8 |
| `dataplane.BaseDataPlane.Settings` | `agent_state_*`, `agent_queue_*`, `agent_session_cache_*`, `agent_graph_cache_*`, `agent_step_claim_lease`, `agent_turn_max_steps`, `agent_tool_timeout`, `agent_tool_max_attempts`, `agent_guardrail_max_*_bytes`, `agent_model_region`, `agent_cost_window`, `agent_reply_*`, `agent_cancel_*`, `agent_session_revision_memory`, `agent_result_card_max_bytes`, `agent_run_page_max`, `agent_audit_page_size`, `agent_audit_max_page_size`, `agent_safety_event_max_page`, `agent_restriction_cache_*` through `ScoutConfig.DataPlaneSettings()` | sizes, attempts, steps, timeouts, and caches positive; the delivery, cancellation, memory, page, and restriction-cache values non-negative, zero taking each default; an empty `agent_state_bucket` is `ErrNotReady`; shards at most partitions; the queue and claim leases above `agent_loop_deadline` |

Both are configuration-time ceilings, not runtime hints: exceeding either is a typed error, never a
truncation.

## Studio features and their workers

| Field | Flag | Rule |
|---|---|---|
| `controlplane.StudioGovernance.Required` | `agent_governance_required_fields` | nil requires every Charter-required field, an empty list none; an unknown name fails every governance check |
| `release.StudioPilots.MaxMembers` | `agent_pilot_max_members` | non-negative; zero refuses every add |
| `release.TablePilotResolver.PositionTimeout` | `agent_pilot_position_timeout` | seconds; non-positive takes `DefaultPilotPositionTimeout` (2s). The product port runs while a new conversation holds its lock, and a late answer fails the resolution |
| `policy.StudioRestrictions.MaxEntries` | `agent_studio_max_restriction_entries` | non-negative; zero takes 256. Bounds a tenant layer's denials and, separately, its guardrail rules |
| `evaluation.StudioEvaluation.MaxOpenRuns` | `agent_evaluation_max_open_runs` | non-negative; zero refuses every evaluation |
| `evaluation.GoldenCaseStore.{MaxInputBytes,MaxCases,VersionsShown}` | `agent_evaluation_case_max_bytes`, `agent_evaluation_max_cases`, `--evaluation_versions_shown` | non-negative; zero takes 32 KiB / 500 / 20 |
| `evaluation.NewAgentEvaluationWorker(lease, batch, maxAttempts)` | `agent_evaluation_lease`, `agent_evaluation_batch`, `agent_evaluation_max_attempts` | lease at least 1s and longer than the slowest example; batch and attempts positive |
| `evaluation.AgentEvaluationWorker.{Concurrency,MaxRunCostMinorUnits,Currency}` | `agent_evaluation_concurrency`, `agent_evaluation_max_run_cost`, `agent_evaluation_currency` | concurrency non-negative, below 2 sequential; cost positive minor units, counting judge spend; a three-letter currency, so an empty flag refuses the worker |
| `evaluation.NewGatewayRubricJudge(gateway, inputs, pricer, settings)` as `AgentEvaluationWorker.Judge` | `agent_evaluation_judge_*` through `ScoutConfig.RubricJudgeSettings()` | compose only when `Configured()`; settings must `Validate()`; the judge route must declare `structured_output` or every graded run fails |
| `evaluation.AgentEvaluationWorker.{PairJudge,Outputs}` | — | both required for comparison runs, which otherwise fail; `evaluation.GatewayJudge` calls its route strictly through `structured_output` and charges the run's requester |
| `evaluation.StudioEvaluation.{BaselineStore,Outputs,MaxPageAnswerBytes}` | `agent_evaluation_compare_page_bytes` | the store and reader are required by the `compare` feature only; bytes non-negative, zero takes 1 MiB |
| `evaluation.EvaluationBaselineStore.{MaxOutputBytes,MaxBaselines}` | `agent_evaluation_baseline_max_bytes`, `agent_evaluation_max_baselines` | non-negative; zero takes 32 KiB / 20 |
| `controlplane.StudioEvaluationGate.{MaxResults,Audit}` | `agent_evaluation_gate_max_results` | non-negative, zero takes 2000; overrides are refused without `Audit`. Compose it in `StudioService.Guards`, or a publish or restore of a gated agent is refused |
| `controlplane.StudioService.Audit` | — | required to remove or loosen an evaluation gate |
| `evaluation.StudioReview.{PageSize,MaxPageSize,MaxTurnBytes}` | `agent_review_page_size`, `agent_review_max_page_size`, `agent_review_max_turn_bytes` | non-negative, the page size within the largest page; zero takes 50 / 200 / 1 MiB |
| `evaluation.ReviewQueueStore.{MaxPending,MaxExpectedBytes}` | `agent_review_max_pending`, `agent_evaluation_case_max_bytes` | non-negative; zero takes 1000 / 32 KiB |
| `evaluation.ReviewIntake.FeedbackThreshold` | `agent_review_feedback_threshold` | 0 to 5; zero queues no rated turn. Set the intake as `BaseDataPlane.Review` and `TurnFeedback.Review`; both offer turns best-effort |
| `evaluation.ReleaseCaseExecutor.ReleaseLimits` | — | required: the data plane's `PrincipalLimits`; the replayed release's frozen `limits` only narrow the executor's tool-call, step, and deadline ceilings |
| `controlplane.StudioService.TemplateLimits.{MaxPerType,MaxVersions}` | `agent_studio_max_templates_per_type`, `agent_studio_max_template_versions` | non-negative; a zero `MaxPerType` refuses every new template and a zero `MaxVersions` every save |
| `controlplane.ReleaseActivationWorker.Settings` | `agent_activation_batch`, `agent_activation_poll`, `agent_activation_max_attempts` through `ScoutConfig.ReleaseActivationSettings()` | all positive; poll at least 1s; a transient failure retries after attempts × poll |
| `domain.ReleaseActivationSettings.Timeout` | `agent_activation_timeout` through `ScoutConfig.ReleaseActivationSettings()` | negative refused; zero takes 2m; bounds one activation's checks and swap |

Every service owning timers or goroutines (`BatchingEmbedder`, `MemoryReplyHub`, `CacheReplyHub`, `TableTurnCanceller`,
`DistributedTenantRateLimiter`) exposes an idempotent `Close`; the composing binary owns the call
during shutdown. Clocks are injected as `Now func() time.Time` (nil takes `time.Now`), so a
downstream test can drive every window, TTL, and budget deterministically.
