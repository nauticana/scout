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
| `toolgateway.BoundCredentialProvider.DefaultTTL` | `agent_credential_ttl` | seconds; a binding's own `MaxTTL` always wins when tighter |
| `observability.TableAuditSink` page size | `agent_audit_page_size` | positive; clamped to `MaxDecisionPageSize` (1000) |
| `dataplane.ToolLoopExecutor.Limits` | `agent_loop_max_iterations`, `agent_loop_max_tool_calls`, `agent_loop_max_tokens`, `agent_loop_max_repeats`, `agent_loop_deadline` | all positive, or construction fails; a step's configuration may only narrow them |
| `dataplane.ToolLoopExecutor.Limits.MaxCostMinorUnits` | `agent_loop_max_cost` | non-negative minor units; zero leaves cost to the turn budget and the delegated bound |
| `dataplane.BaseDataPlane.Settings` | `agent_state_*`, `agent_queue_*`, `agent_session_cache_*`, `agent_graph_cache_*`, `agent_step_claim_lease`, `agent_turn_max_steps`, `agent_tool_timeout`, `agent_tool_max_attempts`, `agent_guardrail_max_*_bytes` | all positive; an empty `agent_state_bucket` is `ErrNotReady`; shards at most partitions; the claim lease above `agent_loop_deadline` |

Both are configuration-time ceilings, not runtime hints: exceeding either is a typed error, never a
truncation.

Every service owning timers or goroutines (`BatchingEmbedder`, `MemoryReplyHub`, `CacheReplyHub`, `TableTurnCanceller`,
`DistributedTenantRateLimiter`) exposes an idempotent `Close`; the composing binary owns the call
during shutdown. Clocks are injected as `Now func() time.Time` (nil takes `time.Now`), so a
downstream test can drive every window, TTL, and budget deterministically.
