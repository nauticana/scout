# Configuration: every Scout limit and the flag behind it

Scout libraries read no environment and hold no package globals: every limit is a constructor or
struct field validated on first use, and unsafe zero/negative combinations are rejected loudly.
A downstream binary declares one `flag` per value in `common/variables.go` and passes it in at
composition time. Names below are the recommended flag spelling; only the mapping matters.

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

Every service owning timers or goroutines (`BatchingEmbedder`, `MemoryReplyHub`,
`DistributedTenantRateLimiter`) exposes an idempotent `Close`; the composing binary owns the call
during shutdown. Clocks are injected as `Now func() time.Time` (nil takes `time.Now`), so a
downstream test can drive every window, TTL, and budget deterministically.
