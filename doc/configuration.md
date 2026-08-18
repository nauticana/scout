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
| `agent_temperature` | 0.7 | Agent model sampling temperature, 0.0–2.0 |
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

## Principals and scoped configuration (`service/scope`, `service/principal`)

| Field | Flag | Rule |
|---|---|---|
| `scope.Compiler.MaxDepth` | `agent_max_scope_depth` | non-negative; zero takes 8. A longer scope chain fails compilation |
| `principal.ChainVerifier.MaxDepth` | `agent_max_delegation_hops` | non-negative; zero takes 4. Bounds the chain regardless of what a grant conveys |
| `approval.Gate.Deadline` | `agent_approval_deadline` | seconds; zero leaves a request open with no escalation |
| `toolgateway.BoundCredentialProvider.DefaultTTL` | `agent_credential_ttl` | seconds; a binding's own `MaxTTL` always wins when tighter |
| `observability.TableAuditSink` page size | `agent_audit_page_size` | positive; clamped to `MaxDecisionPageSize` (1000) |

Both are configuration-time ceilings, not runtime hints: exceeding either is a typed error, never a
truncation.

Every service owning timers or goroutines (`BatchingEmbedder`, `MemoryReplyHub`,
`DistributedTenantRateLimiter`) exposes an idempotent `Close`; the composing binary owns the call
during shutdown. Clocks are injected as `Now func() time.Time` (nil takes `time.Now`), so a
downstream test can drive every window, TTL, and budget deterministically.
