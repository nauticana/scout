# Configuration: every Scout limit and the flag behind it

Scout libraries read no environment and hold no package globals: every limit is a constructor or
struct field validated on first use, and unsafe zero/negative combinations are rejected loudly.
A downstream binary declares one `flag` per value in `common/variables.go` and passes it in at
composition time. Names below are the recommended flag spelling; only the mapping matters.

## Repository-owned flags (`scout_config.go`)

These are the only values Scout reads through keel's `application_config_flag` catalog rather than
through a constructor field. `LoadConfig` fails loudly when a flag is missing from the seed, so the
catalog and `ScoutConfig` cannot drift.

| Flag | Default | Meaning |
|---|---:|---|
| `agent_max_tokens` | 8192 | Max output tokens per agent model completion |
| `agent_temperature` | — | Sampling temperature, 0.0–2.0, sent only to models whose catalog entry has the `sampling` capability; empty sends none |
| `agent_run_retention_days` | 0 | Days to keep agent run activity; 0 keeps it forever |
| `agent_turn_rate` / `agent_turn_burst` | 2 / 10 | Per-tenant admitted turns per second and burst |
| `agent_tool_rate` / `agent_tool_burst` | 10 / 20 | Per-tenant tool calls per second and burst |
| `agent_model_rate` / `agent_model_burst` | 2 / 5 | Per-tenant model calls per second and burst |
| `agent_fleet_turn_rate` / `agent_fleet_turn_burst` | 100 / 200 | Process-wide turn rate and burst |
| `agent_fleet_tool_rate` / `agent_fleet_tool_burst` | 500 / 1000 | Process-wide tool rate and burst |
| `agent_fleet_model_rate` / `agent_fleet_model_burst` | 100 / 200 | Process-wide model rate and burst |
| `agent_max_tenants` | 4096 | Maximum in-memory tenant limiter entries |
| `agent_model_capacity_pool` | `shared` | Shared model capacity pool name |
| `agent_model_capacity` | 32 | Concurrent model capacity slots |
| `agent_model_max_waiters` | 4096 | Maximum queued model requests |
| `agent_max_scope_depth` | 8 | Longest scope chain a release may compile over |
| `agent_max_delegation_hops` | 4 | Longest authority chain a principal may present |
| `agent_approval_deadline` | 3600 | Seconds a reviewer has before escalation; 0 leaves a request open |
| `agent_credential_ttl` | 300 | Default lifetime of a just-in-time tool credential |
| `agent_audit_page_size` | 100 | Decision records per audit query page |
| `agent_loop_max_iterations` | 12 | Model decisions one tool loop step may make |
| `agent_loop_max_tool_calls` | 24 | Governed tool calls one tool loop step may make |
| `agent_loop_max_tokens` | 200000 | Input plus output tokens one tool loop step may spend |
| `agent_loop_max_cost` | 0 | Minor-unit cost one tool loop step may spend; 0 leaves cost to the turn budget |
| `agent_loop_max_repeats` | 3 | Identical calls of one tool before a loop is declared |
| `agent_loop_deadline` | 300 | Wall-clock seconds one tool loop step may run |
| `agent_state_bucket` | (none) | Private bucket for turn input and conversation state; empty refuses to compose the data plane |
| `agent_state_max_bytes` | 4194304 | Largest single state or input payload in bytes |
| `agent_queue_partitions` | 64 | Fixed turn-queue partition pool; changing it reshuffles tenants |
| `agent_queue_shards` | 4 | Partitions one tenant spreads over; at most agent_queue_partitions |
| `agent_queue_max_attempts` | 5 | Deliveries of one turn before it is dead-lettered |
| `agent_queue_lease` | 900 | Seconds one claimed turn stays leased to its worker; keep it above `agent_loop_deadline` |
| `agent_queue_batch` | 8 | Turns one worker tick claims |
| `agent_session_cache_size` | 4096 | Conversations held in the in-memory session cache |
| `agent_session_cache_ttl` | 300 | Seconds an in-memory session snapshot lives |
| `agent_graph_cache_size` | 1024 | Execution graphs held in the in-memory graph cache |
| `agent_graph_cache_ttl` | 3600 | Seconds an in-memory execution graph lives |
| `agent_step_claim_lease` | 360 | Seconds a step claim blocks other workers; keep it above agent_loop_deadline |
| `agent_turn_max_steps` | 16 | Graph steps one turn may execute |
| `agent_tool_timeout` | 30 | Seconds one governed tool call may run when the tool registers none |
| `agent_tool_max_attempts` | 3 | Deliveries of one tool call when the tool registers none |
| `agent_guardrail_max_input_bytes` | 262144 | Baseline byte ceiling on turn input, tool arguments, and retrieved content |
| `agent_guardrail_max_output_bytes` | 1048576 | Baseline byte ceiling on model and tool output |
| `agent_model_region` | (none) | Residency region stamped on a candidate model with no route row; empty leaves it unknown |
| `agent_pilot_max_members` | 200 | Open pilot memberships one agent may hold, which bounds what starting a conversation reads; 0 refuses every pilot add |
| `agent_pilot_position_timeout` | 2 | Seconds one product position lookup may take while a new conversation chooses its release; 0 takes the 2s default |
| `agent_studio_max_agents_per_type` | 20 | Agents of one type a tenant may keep, archived ones included; 0 refuses every Studio create |
| `agent_governance_required_fields` | (none) | Comma-separated Charter governance fields a release must carry; empty requires every field Charter's AgentDefinition requires, `none` requires none; an unknown or repeated name fails the load |
| `agent_evaluation_max_open_runs` | 4 | Queued and running evaluations one agent may hold; 0 refuses every evaluation |
| `agent_evaluation_concurrency` | 4 | Examples of one evaluation run in flight; below 2 runs them one at a time |
| `agent_evaluation_max_run_cost` | 10000 | Minor units of `agent_evaluation_currency` one evaluation run may spend before it stops early |
| `agent_evaluation_currency` | (none) | ISO currency every evaluation cost is priced in; empty refuses to compose the evaluation worker |
| `agent_evaluation_lease` | 900 | Seconds each recorded result extends an evaluation run's lease; keep it above `agent_loop_deadline` |
| `agent_evaluation_batch` | 4 | Evaluation runs one worker tick claims |
| `agent_evaluation_max_attempts` | 3 | Claims of one evaluation run before an expired lease fails it |
| `agent_evaluation_case_max_bytes` | 32768 | Largest saved test case input or expected behavior in bytes |
| `agent_evaluation_max_cases` | 500 | Saved test cases one golden set may hold |
| `agent_activation_batch` | 20 | Due scheduled release activations one worker poll handles |
| `agent_activation_poll` | 30 | Seconds between polls of the release activation worker |
| `agent_activation_max_attempts` | 5 | Attempts of a scheduled activation that fails transiently before it is refused |

## Admission (`service/isolation`)

| Field | Flag | Rule |
|---|---|---|
| `RateLimiterConfig.{Turn,Tool,Model}.{PerSecond,Burst}` | `--rate_{turn,tool,model}_per_second`, `--..._burst` | both zero (lane off) or both positive |
| `RateLimiterConfig.Fleet{Turn,Tool,Model}` | `--rate_fleet_{turn,tool,model}_{per_second,burst}` | process-wide ceiling, same rule |
| `RateLimiterConfig.MaxTenants` | `--rate_max_tenants` | positive; bounds the tenant bucket map |
| `DistributedRateLimiterConfig.{Turn,Tool,Model}.{Limit,Window}` | `--drate_{turn,tool,model}_limit`, `--..._window` | fixed window shared by all replicas; both zero or both positive |
| `DistributedRateLimiterConfig.Fleet{Turn,Tool,Model}` | `--drate_fleet_{turn,tool,model}_{limit,window}` | same rule |
| `DistributedRateLimiterConfig.KeyPrefix` | `--drate_key_prefix` | required; namespaces the shared counters |
| `DistributedRateLimiterConfig.StoreTimeout` | `--drate_store_timeout` | positive; a slower store counts as unreachable |
| `DistributedRateLimiterConfig.FallbackFraction` | `--drate_fallback_fraction` | in (0, 1]; fleet overshoot while degraded is `replicas × fraction × limit` |
| `DistributedRateLimiterConfig.FallbackMaxTenants` | `--drate_fallback_max_tenants` | positive |
| `DistributedRateLimiterConfig.RecoveryProbe` | `--drate_recovery_probe` | positive; degraded dwell before one probe |
| `FairSlotLimiter.{Capacity,MaxWaiters}` | `--concurrency_capacity`, `--concurrency_max_waiters` | both positive |

## Budgets, cost, and loops (`service/isolation`)

| Field | Flag | Rule |
|---|---|---|
| `BudgetLedger.ReservationTTL` | `--budget_reservation_ttl` | zero takes 15m; otherwise at least 1s |
| `ReleaseLimits.Window` | `--release_budget_window` | rolling window a release's `budget` applies to; zero takes `Fallback.Window`; a principal reserves only over a window of at least 1s, and the Studio limits report shows spend only when one is set |
| `WindowedCostBreaker.{Tenant,Agent,Fleet}Limit` | `--cost_{tenant,agent,fleet}_limit` | minor units per window; zero disables that scope, negative rejected |
| `WindowedCostBreaker.{Currency,Window,Buckets,MaxEntries}` | `--cost_currency`, `--cost_window`, `--cost_buckets`, `--cost_max_entries` | window positive and larger than the bucket count; entries positive |
| `MemoryLoopDetector.{Threshold,MaxConversations}` | `--loop_threshold`, `--loop_max_conversations` | positive |
| `MemoryLoopDetector.{Window,MaxFingerprints}` | `--loop_window`, `--loop_max_fingerprints` | non-negative; zero keeps history until `Reset` / takes 1024 |
| `TenantRuntimePolicy.{MaxSteps,MaxTokens,MaxCostMinorUnits,TurnTimeout}` | `--turn_max_{steps,tokens,cost}`, `--turn_timeout` | per-tenant policy; flags supply the default row |

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
| `dataplane.CacheReplyHub.{Retention,PollInterval}` | `--reply_retention`, `--reply_poll_interval` | non-positive takes 10m / 500ms |
| `dataplane.TableTurnCanceller.PollInterval` | `--turn_cancel_poll_interval` | non-positive takes 1s |

## Principals and scoped configuration (`service/scope`, `service/principal`)

| Field | Flag | Rule |
|---|---|---|
| `scope.Compiler.MaxDepth` | `agent_max_scope_depth` | non-negative; zero takes 8. A longer scope chain fails compilation |
| `principal.ChainVerifier.MaxDepth` | `agent_max_delegation_hops` | non-negative; zero takes 4. Bounds the chain regardless of what a grant conveys |
| `approval.Gate.Deadline` | `agent_approval_deadline` | seconds; zero leaves a request open with no escalation |
| `approval.Resumer.MaxRevisedArgumentBytes` | `--approval_max_revised_argument_bytes` | non-positive takes 64 KiB; bounds an edited verdict's revised arguments |
| `controlplane.StudioService.MaxAgentsPerType` | `agent_studio_max_agents_per_type` | non-negative; zero refuses every create |
| `controlplane.StudioDelegations.MaxDepth` | `agent_max_delegation_hops` | non-negative; the re-delegation depth a Studio grant may convey |
| `toolgateway.BoundCredentialProvider.DefaultTTL` | `agent_credential_ttl` | seconds; a binding's own `MaxTTL` always wins when tighter |
| `observability.TableAuditSink` page size | `agent_audit_page_size` | positive; clamped to `MaxDecisionPageSize` (1000) |
| `dataplane.ToolLoopExecutor.Limits` | `agent_loop_max_iterations`, `agent_loop_max_tool_calls`, `agent_loop_max_tokens`, `agent_loop_max_repeats`, `agent_loop_deadline` | all positive, or construction fails; a step's configuration and the release's `limits` setting may only narrow them |
| `dataplane.ToolLoopExecutor.Limits.MaxCostMinorUnits` | `agent_loop_max_cost` | non-negative minor units; zero leaves cost to the turn budget and the delegated bound |
| `dataplane.BaseDataPlane.Settings` | `agent_state_*`, `agent_queue_*`, `agent_session_cache_*`, `agent_graph_cache_*`, `agent_step_claim_lease`, `agent_turn_max_steps`, `agent_tool_timeout`, `agent_tool_max_attempts`, `agent_guardrail_max_*_bytes` | all positive; an empty `agent_state_bucket` is `ErrNotReady`; shards at most partitions; the claim lease above `agent_loop_deadline` |

Both are configuration-time ceilings, not runtime hints: exceeding either is a typed error, never a
truncation.

## Studio features and their workers

| Field | Flag | Rule |
|---|---|---|
| `controlplane.StudioGovernance.Required` | `agent_governance_required_fields` | nil requires every Charter-required field, an empty list none; an unknown name fails every governance check |
| `release.StudioPilots.MaxMembers` | `agent_pilot_max_members` | non-negative; zero refuses every add |
| `release.TablePilotResolver.PositionTimeout` | `agent_pilot_position_timeout` | seconds; non-positive takes `DefaultPilotPositionTimeout` (2s). The product port runs while a new conversation holds its lock, and a late answer fails the resolution |
| `policy.StudioRestrictions.MaxEntries` | `--studio_restriction_max_entries` | non-negative; zero takes 256. Bounds a tenant layer's denials and, separately, its guardrail rules |
| `evaluation.StudioEvaluation.MaxOpenRuns` | `agent_evaluation_max_open_runs` | non-negative; zero refuses every evaluation |
| `evaluation.GoldenCaseStore.{MaxInputBytes,MaxCases,VersionsShown}` | `agent_evaluation_case_max_bytes`, `agent_evaluation_max_cases`, `--evaluation_versions_shown` | non-negative; zero takes 32 KiB / 500 / 20 |
| `evaluation.NewAgentEvaluationWorker(lease, batch, maxAttempts)` | `agent_evaluation_lease`, `agent_evaluation_batch`, `agent_evaluation_max_attempts` | lease at least 1s and longer than the slowest example; batch and attempts positive |
| `evaluation.AgentEvaluationWorker.{Concurrency,MaxRunCostMinorUnits,Currency}` | `agent_evaluation_concurrency`, `agent_evaluation_max_run_cost`, `agent_evaluation_currency` | concurrency non-negative, below 2 sequential; cost positive minor units; a three-letter currency, so an empty flag refuses the worker |
| `controlplane.ReleaseActivationWorker.Settings` | `agent_activation_batch`, `agent_activation_poll`, `agent_activation_max_attempts` through `ScoutConfig.ReleaseActivationSettings()` | all positive; poll at least 1s; a transient failure retries after attempts × poll |
| `controlplane.ReleaseActivations.Timeout` | `--activation_timeout` | non-positive takes 2m; bounds one activation's checks and swap |

Every service owning timers or goroutines (`BatchingEmbedder`, `MemoryReplyHub`, `CacheReplyHub`, `TableTurnCanceller`,
`DistributedTenantRateLimiter`) exposes an idempotent `Close`; the composing binary owns the call
during shutdown. Clocks are injected as `Now func() time.Time` (nil takes `time.Now`), so a
downstream test can drive every window, TTL, and budget deterministically.
