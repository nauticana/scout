# Scout improvement ideas

Derived from a review of the interview study set in `~/dev/jobprep/mlsol`: 30 numbered studies (`q11`–`q15`, `q21`–`q25`, `q31`–`q35`, `q41`–`q45`, `q51`–`q55`, and `q61`–`q65`) plus two `q22` companion files. The repository comparison is against Scout `ce07dee` (`v0.2.1`).

The study set is not scaffolding to copy. It is a catalogue of mechanisms that closely match Scout contracts whose reusable implementations have not yet been completed. This document maps each study family onto concrete Scout gaps and proposes where each mechanism should live.

## Implementation status (2026-08-12)

| Idea | Shipped as |
|---|---|
| A1 | `isolation.TenantRateLimiter` — tenant × fleet buckets per lane, full-bucket-only sweep |
| A2 | `isolation.LimitError` + optional `contract.RetryAfterError` capability |
| A3 + E4 | attempt-aware `isolation.BudgetLedger` with expiry fencing, actual settlement, and bounded `Expire` |
| A4 | Distributed admission remains open; the prior one-key counter primitive could not atomically enforce tenant and fleet scopes |
| A5 | `isolation.FairSlotLimiter` (round-robin) + weighted `SlotCapacityScheduler`; the existing turn contract stays one-slot |
| A6 | `modelgateway.AdaptiveCapacityScheduler` (latency-gradient AIMD) |
| A7 | `isolation.WindowedCostBreaker` — pre-work capacity admission and non-failing completed records |
| B2 | `dataplane.MemoryReplyHub` + replay and verified publish deduplication |
| B3 | `dataplane.StreamPump` |
| B4 | `contract.TurnCanceller`, `dataplane.MemoryTurnCanceller`, `domain.TurnAdmissionPolicy` |
| B5 | `knowledge.BatchingEmbedder` over `contract.BatchEmbedder` |
| C1 | `dataplane.MemorySessionCache` / `MemoryGraphCache` over `internal/lru` |
| C2 | `internal/singleflight` wired into `SessionCoordinator` and `DefinitionResolver` |
| D1 | `KnowledgeQuery` principal, entitlements(+digest), budget |
| D2 | `knowledge.HybridRetriever` (RRF fusion, budget-gated `KnowledgeReranker`) |
| E2 | internal stage wrappers in streaming and retrieval; structured observation DTOs remain open |
| E5 | optional `DetailedRolloutHealthEvaluator.Evaluate` → three-state `domain.RolloutHealth` |
| F1 | full jitter in `toolgateway.RetryPolicy` |
| F2 | bounded ordered concurrency in `release.ContractTestRunner` |
| — | extra gap closed: `isolation.MemoryLoopDetector` (`LoopDetector` had no impl) |

Still open after the 2026-08-16 pass: C4 semantic response cache (research until a leak-safety proof exists) and a HANA vector adapter (demand-driven). Everything else in this document now has a reference implementation; see [TODO.md](TODO.md) for the shipped inventory and `doc/` for the design references.

## Implementation status (2026-08-16)

| Idea | Shipped as |
|---|---|
| A4 | `isolation.DistributedTenantRateLimiter` — tenant × fleet fixed windows over keel cache, coalesced hot keys, bounded local fallback with a degraded flag. Needs an atomic multi-scope keel primitive to close the documented over-admission window |
| A8 | `toolgateway.CircuitBreaker` — closed/open/half-open, one generation-fenced probe, LRU-bounded tenant × tool, shared destination health, injected failure classifier |
| B0 + B0a | `dataplane.TurnIngress`, `QueueTurnDispatcher`, `QueueTurnScheduler`, `TableDeadLetterQueue`, `TurnRuntime`, `DurableSessionStore`, `StepIdempotencyStore`, `ObjectStateStore`, `MemoryTurnQueue`, `dataplanetest` conformance suites; DTO/table identity resolved in `doc/persistence.md` |
| B1 | `modelgateway.HedgingGateway` — one delayed attempt on a different route, idempotent requests only, per-tenant budget and kill switch, independently fenced per-attempt reservations |
| B6 | `modelgateway.PolicyRouter` over `ModelCandidateCatalog` and `CapacitySnapshotSource`, with an auditable reason and routing generation; `ModelSelection` carries version, region, route, generation |
| B7 | `guardrail.LayeredEnforcer` + `RuleSetCompiler` — baseline composed with pinned release policy, typed versioned envelope compiled per digest, stateful streaming sessions with bounded lookback |
| C3 | Cache subordination in `SessionCoordinator`: durable write first, revision floor, overlapping-read invalidation, local TTL bounded by `RemoteTTL` |
| D0 | `knowledge.PgVectorIndex` — entitlement predicates compiled into the WHERE clause, tombstone-aware, fail-closed on a missing or stale entitlements digest |
| D3 | `knowledge.ShardedRetriever` — k-way merge with score-bound early stop |
| E1 | `observability.LabelPolicy` + `BoundedRuntimeMetrics` + `TenantHeavyHitters` — allowlisted labels, exact tenant accounting only through `TenantLedger`, stable rank slots |
| E2 | `domain.Observation` + `internal/stage` spans emitted from streaming and retrieval |
| E3 | `isolation.LatencyBudgetAllocator` — generation reserved first, optional stages clamped, `ErrDeadlineInfeasible` at admission |
| E6 | `release.PinnedTrafficManager` — compliance pin → tenant pin → cohort → deployment, with `agent_version_pin` and pin-aware garbage collection |
| E7 | `service/evaluation` — content-addressed manifests, golden sets with hidden gate scope, heuristic and blinded judge evaluators, paired slice scoring with bootstrap CIs, signed expiring gate decisions, trajectory and retrieval scoring |
| E5 (impl) | `evaluation.GateHealthEvaluator` — verified unexpired decision plus online metrics, inconclusive on anything doubtful |
| F3 | `knowledge.IngestPipeline` — bounded stages, correlated per-document results, publish-after-index ordering with compensation |
| G1 | `Now func() time.Time` on every time-dependent service; `doc/configuration.md` records the convention |
| G2 | Lifecycle and leak tests for every goroutine-owning service, with idempotent `Close` |
| G3 | Validated constructor config everywhere, each limit mapped to a documented flag in `doc/configuration.md` |
| — | Beyond the study set: `release.RolloutController` state machine, `release_bundle`, session drain, rollback drills, and serving-signal export (`modelgateway.ServingSignalCollector`) |

### R3 decision (2026-08-16)

Keep the split between tenant agent-version rollout and platform-release rings, and add a signed `release_bundle` that pins the set of component versions a platform release certifies. Merging the two controls would make rollback ambiguous exactly when it matters, force a ring change on every tenant publish, and mint a composite identity per tenant-agent-platform triple. The bundle is a manifest, not a third traffic control: nothing routes on it, and it exists so "roll back to the previous release" names what it restores. A conversation persists both identities — `agent_conversation.agent_version` and `conversation_release.platform_version`.

## 1. Core finding

Scout owns an unusually complete *vocabulary* for a multi-tenant agent platform — thirteen contract files, 98 interfaces, 57 tables, immutable versioning, and tenant scoping at the important boundaries. Its implementations are concentrated in Studio, publication, the published-agent runtime, and gateway composition. The study set exposes a narrower but real gap: isolation and advanced data-plane policies are mostly ports without reusable Scout policy behind them. `contract/isolation.go` declares six control families; `service/isolation/` implements the execution-governor family. `contract/data_plane.go` declares fifteen interfaces; `service/dataplane/` implements three composition services.

The missing mechanisms are best understood as an unfinished implementation sequence, not an intentional interfaces-only boundary. Time and priority put contracts and core verticals first; the next phase should complete the reusable mechanisms behind them. The ownership rule still matters: generic concurrency/cache machinery belongs in keel, while Scout owns agent-platform policy and can ship replaceable reference implementations or adapters for queues, caches, reply brokers, vector stores, and providers where doing so makes the platform usable. Vendor-specific choices and product policy remain injectable. A contract establishes replaceability; it does not excuse leaving the common implementation work to every downstream.

Selected study-mapped gaps:

| Contract | Declared | Default Scout implementation in `service/` |
|---|---|---|
| `ConversationIngress` / `ConversationRuntime` | `contract/data_plane.go` | none |
| `TenantRateLimiter` | `contract/isolation.go` | none |
| `TenantBudgetManager` | `contract/isolation.go` | none |
| `ConcurrencyLimiter` / `ConcurrencyLease` | `contract/isolation.go` | none |
| `CostCircuitBreaker` | `contract/isolation.go` | none |
| `LoopDetector` | `contract/isolation.go` | none |
| `ExecutionGovernor` / `ExecutionPermit` | `contract/isolation.go` | `isolation.ExecutionGovernor` |
| `CapacityScheduler` / `CapacityLease` | `contract/model_runtime.go` | none |
| `ModelRouter` | `contract/model_runtime.go` | none |
| `HotSessionCache` | `contract/data_plane.go` | none |
| `DurableSessionStore` | `contract/data_plane.go` | none |
| `ExecutionGraphRepository` / `ExecutionGraphCache` | `contract/control_plane.go` | none |
| `TurnReplyPublisher` / `Subscriber` | `contract/data_plane.go` | none |
| `FairTurnScheduler` / `TurnDispatcher` | `contract/data_plane.go` | none |
| `StepIdempotencyStore` | `contract/data_plane.go` | none |
| `DeadLetterQueue` | `contract/data_plane.go` | none |
| `ToolCircuitBreaker` | `contract/tool_gateway.go` | none |
| `GuardrailEnforcer` | `contract/guardrail.go` | none |
| `KnowledgeRetriever` / `KnowledgeVectorIndex` | `contract/knowledge.go` | none |
| `RolloutHealthEvaluator` | `contract/release_and_observability.go` | none |
| `AgentVersionTrafficManager` | `contract/control_plane.go` | none |

`SessionCoordinator` and `DefinitionResolver` both require cache adapters. That prevents use of those two reference compositions without extra wiring; it does **not** prevent the existing published-agent runtime from running. A local-development adapter would improve adoption, but it should not be confused with production distributed-cache semantics.

## 2. Layering rule applied throughout

Every proposal below states which repository should own it. The test used:

| Question | Owner |
|---|---|
| Would this code be identical in a payments or logistics backend? | keel |
| Does it need tokens, cost minor units, model/agent versions, turns, prompts, or guardrails to make sense? | Scout |
| Does it encode which agents exist, what prompts say, or which provider to buy? | downstream |

So: a bare token bucket is keel; a bucket charged in *estimated prompt + max-new tokens* and refunded on early stop is Scout. A generic KV cache is keel (`cache.CacheService` already exists); a cache keyed by *pinned agent version + guardrail policy version + tenant entitlement fingerprint* is Scout.

Several proposals would benefit from small keel primitives, noted inline. They must be introduced as new narrow interfaces, new types, or configuration on existing implementations. Adding a method to an exported Go interface is source-breaking for every external implementation even when no current keel caller changes.

One more rule applies to the rest of this document: preserve the current small interfaces unless a demonstrated use case cannot be expressed. When evolution is necessary, prefer a new optional capability interface or a major-version contract change over making every existing provider implement an advanced feature.

---

## A. Isolation and admission — from `q14`, `q21`–`q25`, `q41`, `q43`, `q53`

### A1. Ship a safe default `TenantRateLimiter` (tenant bucket × fleet bucket)

**Gap.** No implementation. Every downstream writes its own, and the two-bucket acquisition (per-tenant *and* global) is the part people get wrong — partial acquisition leaks tokens from the bucket that succeeded.

**Proposal.** `service/isolation/tenant_rate_limiter.go`: one mutex covering both buckets so acquisition is atomic, monotonic-clock refill, no per-tenant ticker, hard cap on tracked tenants with a cooldown-throttled sweep of fully-refilled buckets. `AllowTurn` / `AllowToolCall` / `AllowModelCall` differ only in which configured limit set they charge. The single lock is the simplest correctness-first default; expose contention metrics and shard only after measurement, because an attempted lock optimization can easily destroy tenant×fleet atomicity.

Scout does not currently model request/tool/model call rates in `TenantRuntimePolicy`; `tenant_quota` models token and cost windows. Define the policy source before implementing the limiter: fleet defaults come from validated binary flags, while per-tenant overrides come from a versioned repository/config contract. Cache the resolved policy with bounded TTL and fail according to an explicit stale/missing-policy rule. Do not smuggle rates into unrelated priority/capacity fields.

**From.** `q21`. Its sweep argument is worth preserving verbatim in the code comment: evicting a *drained* bucket hands its owner a full one, so eviction must only drop buckets that are already full, and refuse (`ErrTenantCapacity`) rather than evict when at cap. That is a real multi-tenant abuse vector and not obvious.

**Owner.** Scout for the adapter and policy — the three methods are agent-domain (`domain.ToolCall`, `domain.ModelRequest`). The bucket arithmetic belongs in keel only if another application needs the same primitive.

### A2. Preserve `ErrRateLimited` while adding retry advice

**Gap.** `domain.ErrRateLimited` is a bare sentinel. `handler/studio.go:383` maps it to a status code and cannot emit a `Retry-After` header, because the limiter contract discards the one number it computed. Every one of `q21`, `q22`, `q23` treats accurate retry-after as a first-class output.

**Proposal.** Keep `domain.ErrRateLimited` as the stable sentinel and add an optional capability in `contract/`, implemented by a concrete error in `service/isolation/`:

```go
type RetryAfterError interface {
    error
    RetryAfter() time.Duration
}

type LimitError struct {
    Scope domain.LimitScope
    After time.Duration
    Err   error // wraps domain.ErrRateLimited
}
```

`domain.LimitScope` is a value-only enum (`tenant`, `fleet`, `tool`, `model`). `LimitError` supplies `Error`, `Unwrap`, and `RetryAfter`; `errors.Is(err, domain.ErrRateLimited)` keeps working and handlers can `errors.As` to the capability without depending on the implementation. This keeps behavior out of Scout's value-only `domain/` package. The current Studio mapping replaces the original error with keel's status/message-only `APIError`, so an HTTP adapter that emits the header must inspect the typed error before that information is discarded (or keel must gain a header-capable error response). Round a positive duration up to whole seconds because `Retry-After` does not carry sub-second durations. Do not automatically use the same type for `ErrBudgetExceeded`: a hard monetary quota may have no meaningful retry time. Return retry advice for a budget failure only when the quota window and reset time are known.

**From.** `q23`, whose follow-up answer is the honest caveat to document: retry-after computed under the admission lock is *advice*, not a reservation — a later request can invalidate it. Say so in the doc comment rather than implying a guarantee.

**Owner.** Scout (`contract/` capability and `service/isolation/` implementation). Small, unblocks correct HTTP behavior, no new dependency.

### A3. Separate durable budget reservation from weighted queueing

**Gap.** `TenantBudgetManager` has the right lifecycle (`Reserve` → `Commit`/`Release`) and no implementation, while `budget_reservation`, `tenant_quota`, and `usage_event` already establish a durable accounting model. The earlier version of this proposal mixed that financial lifecycle with an in-memory priority queue. They are different failure domains: a process-local weighted bucket cannot be the source of truth for money, survive a worker crash, or make settlement idempotent. Separately, `isolation.ExecutionGovernor` checks elapsed `TurnTimeout` but does not predict whether queued work can still finish before the caller's deadline.

**Proposal.** Build two composable mechanisms:

- A durable `TenantBudgetManager` in `service/isolation/` using named SQL and a transaction. Serialize each tenant, key attempts by tenant+request+attempt, and count settled usage plus live holds. A live attempt replays idempotently; an expired attempt is fenced before a nonterminal turn receives the next attempt. `Commit` records actual usage, including overruns. No `float64` enters the accounting path.
- A separate admission/capacity decorator that obtains priority from `TenantContext`, uses aging to prevent starvation, estimates queue delay, and fails with `DeadlineExceeded` when the work cannot fit the remaining deadline. It calls the durable reservation service before irreversible work and releases the reservation on every pre-execution terminal path.

The reservation caller estimates prompt tokens plus `MaxOutputTokens` and prices that estimate for the selected model. The manager should not tokenize prompts or select models itself; those are injected Scout concerns and keeping them outside the ledger makes settlement auditable. The lifecycle order is: cheap validation/rate/queue-feasibility checks, durable turn identity, model/cost estimate, durable reserve, then enqueue; release if enqueue fails. Long queue leases coordinate with E4 reservation expiry so admitted work does not begin after its budget silently expires.

**From.** `q22`. Its API observation remains important: an error-only `Admit` cannot return a reservation handle, so the caller can never refund. Scout's `Reserve` already returns `domain.BudgetReservation`; keep that lifecycle and add expiry metadata as part of E4.

**Owner.** Scout for reservation semantics, token/cost estimation, and admission composition. A generic aging priority queue belongs in keel only if it has another consumer.

### A4. Distributed limiter with local fallback, over keel's existing cache

**Gap.** Scout's recommended topology runs several `conversation-api` replicas. A purely local limiter enforces *R × limit*, while the available one-key increment cannot atomically enforce both tenant and fleet scopes.

**Proposal.** `service/isolation/distributed_rate_limiter.go`:

- a local per-replica safety ceiling alongside the distributed decision, and an explicit outage policy (`fail_closed` or bounded local fallback) chosen at composition time;
- one coalesced batch per hot key (single in-flight increment, callers admitted by prefix of the returned count) — this is what stops a hot key from becoming a store stampede;
- bounded store timeout, circuit with a **generation counter** so a stale in-flight success cannot clear a newer failure, single recovery probe;
- documented worst-case overshoot for bounded local fallback: `max(0, R×localLimit − distributedLimit)`.

The local limiter cannot be both an unconditional fast-path admission and a second full distributed limit without changing the effective policy. In healthy operation the shared counter remains authoritative; the local ceiling protects the process and bounds the chosen fail-open behavior.

**keel note.** A future backend contract needs one atomic operation covering every charged scope. The earlier `SharedCounter` subset was removed because composing independent increments could consume one scope while rejecting another.

**Owner.** keel for the atomic-counter capability and backend implementations; Scout for limiter policy and composition.

### A5. Fair weighted concurrency limiter for scarce capacity

**Gap.** `ConcurrencyLimiter.Acquire` returns a one-slot lease with no fairness contract. `CapacityScheduler.Acquire` receives the full model request/selection and can derive a weight, but it states no fairness or reservation semantics. Nothing prevents one tenant from holding every slot, and a large model-capacity request can starve behind a stream of small ones forever.

**Proposal.** A shared weighted-fair allocator with two thin Scout adapters:

- per-tenant FIFO, tenants in round-robin rotation;
- weighted atomic reservation — a blocked head *reserves* freed capacity instead of letting smaller requests bypass it, which is what bounds large-request wait;
- private wake channel per waiter (no thundering herd);
- `Release` idempotent via `sync.Once` so a panicking step cannot leak a slot.

One concrete Go type cannot satisfy both current interfaces because they both define `Acquire` with different signatures; Go has no method overloading. Keep `ConcurrencyLimiter` as one unit per turn and adapt it with weight 1. The `CapacityScheduler` adapter can derive model-capacity weight from `ModelRequest` and `ModelSelection`. This avoids a breaking contract change until a real non-model weighted-concurrency use case appears. Likewise, do not add a context to `ConcurrencyLease.Release` merely for symmetry; only evolve it if release performs fallible remote I/O.

Strict reservation for a large head bounds starvation but can idle capacity. Make that fairness-versus-utilization trade-off explicit and configurable (for example, a bounded bypass count or reservation age), then test the starvation bound rather than claiming both properties for free.

**From.** `q25`, including its per-tenant queue-wait instrumentation, which is the input `q64` wants for autoscaling.

**Owner.** Scout for tenant/model adapters; keel for the allocator only if it is reused outside Scout.

### A6. Adaptive provider concurrency (AIMD / latency-gradient)

**Gap.** `CapacityScheduler` has no implementation, and its current acquire/release contract exposes no outcome feedback for adapting capacity. Real provider capacity is discovered, not merely configured: a fixed limit is either too low (wasted throughput) or too high (429 storms and queueing inside the provider, where Scout cannot see it).

**Proposal.** `service/modelgateway/adaptive_capacity.go` — additive increase on success, multiplicative decrease on error *or* on latency above a multiple of observed minimum RTT, sampled over a full concurrency-sized window to damp oscillation, clamped to `[min, max]`. Expose the current limit and rejection rate; deployment autoscaling can consume those signals.

The current `CapacityLease.Release(ctx, usage)` reports neither latency nor outcome, so `CapacityScheduler` alone cannot implement this algorithm correctly. Put the feedback loop around the provider call in `modelgateway.Gateway`, or introduce a narrow `CapacityOutcomeRecorder` capability carrying latency, terminal outcome, and overload classification. Do not infer overload from every error: validation, cancellation, and authorization failures must not reduce provider capacity.

**From.** `q14`. Its two structural points matter more than the math: the lock covers *accounting only*, never `fn` execution; and the window resets on every limit change so samples measured under the old limit cannot drive the next decision.

**Owner.** Scout. Latency signal is per model/provider; the notion of "overload" here is inference-specific (prefill vs decode).

### A7. Windowed cost accounting and a half-open `CostCircuitBreaker`

**Gap.** `CostCircuitBreaker.Record` is documented as adding usage to "tenant, agent, and fleet cost windows" — no implementation exists and the contract never says how long a window is or how it closes. `Allow` also receives projected minor units without a currency, which makes a fleet-wide threshold invalid when tenants or models use different currencies.

**Proposal.** First make the contract currency-safe. Add bounded sliding windows per scope. Tracking capacity rejects in `Allow`, before work starts; `Record` never turns completed work into a failed step and counts records with an untracked scope. Document whether state is process-local or coordinated; otherwise a fleet of `R` replicas silently gets `R` independent cost ceilings.

A3 remains the authoritative tenant quota and settlement ledger. A7 is a faster protective stop for agent/fleet spend anomalies and must consume the same settled usage facts; it must not become a second accounting source whose balance can disagree with the ledger.

**Owner.** Scout. Money in minor units with an explicit currency is invariant 10.

### A8. Complete the tool gateway with a default circuit breaker

**Gap.** `toolgateway.GovernedGateway` requires a `ToolCircuitBreaker`, but Scout supplies only a fake. This is a more immediate reuse gap than adaptive model concurrency: every real tool call must wire a breaker before the existing gateway can run.

**Proposal.** `service/toolgateway/circuit_breaker.go` with closed/open/half-open states, a single probe in half-open, generation-guarded transitions so a stale completion cannot overwrite newer state, injected time, bounded tenant×tool key cardinality, and explicit failure classification. Track both tenant×tool health and shared tool health when many tenants use the same endpoint; otherwise a provider-wide outage is rediscovered independently by every tenant. Configuration defines the rolling window, minimum samples, failure threshold, open duration, and whether validation failures count as dependency health failures.

The current gateway calls `RecordFailure` for transport failures, invalid output, and retryable results. Keep that sequencing visible, but add a classifier before tuning breaker thresholds: caller cancellation, tenant input errors, and authorization failures must never trip a dependency breaker. `Release`/record operations remain observable; no silent suppression.

**From.** `q24`'s generation counter and single recovery probe, applied to the existing tool contract.

**Owner.** Scout; the state machine may move to keel if another backend needs the identical primitive.

### A9. Complete `ExecutionGovernor` with a bounded loop detector

**Gap.** `ExecutionGovernor` is implemented but cannot be constructed without a `LoopDetector` and `CostCircuitBreaker`, neither of which has a default. The cost breaker is A7; the loop detector is otherwise absent from the backlog.

**Proposal.** `service/isolation/loop_detector.go`: retain a bounded recent sequence of step fingerprints per tenant+conversation, detect both immediate repetition and short cycles, expire idle conversations, and cap total tracked conversations without evicting active state into a clean slate. `Reset` is idempotent and terminal. Thresholds and history length are policy inputs; fingerprints must be digests of normalized behavior, never raw prompts, tool arguments, or model output.

For multi-worker replay, decide explicitly whether detection is local advisory protection or durable/distributed enforcement. The safe first implementation can be process-local only if a conversation is affinity-routed and the limitation is documented; otherwise the history must live behind a coordinated store.

**From.** The bounded-window and expiry mechanics in `q41`, `q43`, and `q53`; the agent-specific detection policy comes from Scout's existing contract.

**Owner.** Scout.

---

## B. Model gateway and streaming — from `q15`, `q31`–`q35`, `q61`, `q64`

### B0. Prove one recoverable turn vertical slice before adding optimizers

**Gap.** The current service layer has useful pieces (`SessionCoordinator`, `DefinitionResolver`, `StepExecutorRegistry`, governed model/tool gateways) but no reference composition that carries one turn through durable dispatch, step idempotency, checkpoint, guarded reply, and acknowledgement. `TurnDispatcher`, `FairTurnScheduler`, `StepIdempotencyStore`, `DeadLetterQueue`, and `ConversationRuntime` are ports with no Scout mechanism. This is more important than hedging or semantic caching because it is where invariants 7 and 8 either become true or remain prose.

**Proposal.** Define and test a minimal vertical slice before optimizing individual stages:

- compose keel's worker/dispatcher lifecycle rather than creating a Scout background goroutine or generic queue;
- implement `DurableSessionStore` over `conversation_turn`, `step_checkpoint`, and `session_snapshot`, storing large state through injected keel object storage and committing only URI+digest metadata;
- implement the Scout-specific durable `StepIdempotencyStore` over the existing `step_idempotency` schema with named SQL and transactional state transitions;
- persist the idempotency result before checkpointing it so a crash can replay the stored result rather than the side effect; checkpoint and idempotency state must both be durable before queue acknowledgement, with stable request/step keys propagated to idempotent external tools;
- publish only guardrail-approved frames, persist the final result, then acknowledge; terminal retry exhaustion goes to the injected `DeadLetterQueue`;
- ship usable Scout reference implementations/adapters for dispatch, scheduling, replies, and dead-letter handling over the appropriate keel primitives, while keeping their contracts replaceable;
- provide conformance tests that both Scout's references and vendor-specific adapters can run.

This turns the existing ports into an executable reference lifecycle. It does not require hard-coding Redis, Kafka, or another vendor into the orchestration: provider adapters remain separate implementations selected at composition time, while the common lifecycle and at least one usable path ship with Scout.

**From.** `q61`, with `q31`–`q33` supplying cancellation and streaming failure semantics.

**Owner.** Scout for orchestration, persistence state machines, typed reference adapters, and conformance tests; keel for generic worker/queue/cache primitives; downstream for provider selection and deployable worker wiring.

### B0a. Resolve schema/DTO identity before implementing persistence

The vertical slice has several pre-existing representation mismatches that should be fixed first, while no production implementation depends on them:

- `domain.ExecutionStep.StepID` and `StepIdempotencyStore` use string step IDs, while `step_idempotency.execution_step_id` and `step_checkpoint.execution_step_id` are `BIGINT` FKs. Persistence either needs the compiled numeric ID in the domain value or must resolve the tenant+agent+version+logical step ID before each write; do not coerce strings to integers ad hoc.
- `domain.StepCheckpoint` exposes `CheckpointID`, `RequestID`, and inline `State`, while `step_checkpoint` stores no checkpoint ID/request ID and requires `state_uri`, digest, fingerprint, and currency. Align the DTO with the schema/object-store boundary or introduce a dedicated persistence command DTO; the repository must not invent missing digests/fingerprints.
- `domain.SessionSnapshot` carries inline state and agent version, while `session_snapshot` stores a URI/digest pointer and derives version through conversation/checkpoint relations. Define hydration/dehydration explicitly and verify every fetched object digest before returning state.
- `StepIdempotencyStore.Begin` can return a full `StepResult`, but the table stores only result URI+digest. Its implementation therefore requires an injected object store/codec and a lease/claim timeout; the current `abandoned` state is terminal and `Begin`/`Abandon` semantics need a defined replay transition rather than assuming a terminal row can simply be reclaimed.

Resolve these as one persistence design, including transaction boundaries and object-write orphan cleanup. Otherwise the service will either violate the schema or quietly weaken the typed contracts.

### B1. Hedged generation across replicas

**Gap.** `ModelRouter.Select` returns exactly one selection; `Gateway.Stream` calls exactly one provider. Tail latency is therefore whatever the slowest replica does, and the README's 600–900 ms TTFT budget has no defense.

**Proposal.** `service/modelgateway/hedged_gateway.go` decorating `ModelGateway`: start one attempt, hedge after a delay *without a first token*, commit permanently to the first replica that produces a token, cancel the losers, never interleave two replicas into one stream.

There is a contract prerequisite: `ModelRouter.Select` returns one provider/model/pool, not distinct eligible replicas, and calling the same provider twice may route back to the same replica. Hedging needs a candidate/exclusion capability or a provider adapter that guarantees attempt diversity. It also needs one logical request ID plus unique attempt IDs so metering records every billed attempt without making the user-visible request non-idempotent.

**From.** `q34`. Its third follow-up is the one that makes this a Scout concern rather than a generic RPC concern: **every started attempt is billable even when canceled**. So the hedge must be admitted through the same `TenantBudgetManager` as the primary, hedges need their own budget cap, and hedging must be disabled fleet-wide when saturated. A hedger that improves p99 while doubling token spend is a regression under invariant 10, and only Scout knows that.

**Owner.** Scout.

### B2. Reply fan-out hub with replay cursor

**Gap.** The README promises reconnect ("the reply broker may retain frames briefly for reconnect"), but `TurnReplySubscriber.Subscribe(ctx, tenantID, requestID)` has no cursor and states no fan-out semantics. An implementation could independently support multiple subscribers, but portable reconnect from a known frame is unexpressed. `domain.TurnReply` already carries `Sequence` and `Final`, so the data model is ready and the capability is missing from the port.

**Proposal.**

- Add an optional replay capability such as `ReplayTurnReplySubscriber.SubscribeFrom(ctx, tenantID, requestID, afterSequence)`, with an explicit resync-required error when the cursor is older than retained history. This avoids immediately breaking every implementation of the existing subscriber contract.
- `service/dataplane/reply_hub.go`: bounded per-subscriber queue, **disconnect on first overflow** rather than silent drop — a gap in a token stream is worse than a clean failure — plus a bounded replay ring, and snapshot-then-register under one lock so live frames cannot slip between replay and registration.
- Treat a matching retained sequence as an idempotent publish. Reject divergent duplicates and duplicates too old to verify.

An in-memory hub is useful only for a combined single-process deployment. Reconnect through a different `conversation-api` replica requires a shared broker or durable reply adapter, and its retention/ordering guarantees should be captured by the same conformance suite. The final result remains recoverable from durable turn state even when frame replay has expired.

**From.** `q35`, whose "what is your lag policy" answer is the design decision Scout should make once, centrally, instead of leaving each downstream to invent it.

**Owner.** Scout. keel's `port.WebSocketHub` broadcasts to a *user*; this fans out one *generation* with per-subscriber backpressure and ordering. Not a duplicate.

### B3. One shared, tested stream pump

**Gap.** Every downstream will write the same loop: read `ModelStream`, apply `GuardrailEnforcer.AfterModelChunk`, publish a `TurnReply`, honor backpressure, propagate cancellation upstream, and stop at max output tokens. It is short, it is subtle, and it is where goroutine leaks live.

**Proposal.** `service/dataplane/stream_pump.go` — a single synchronous `ModelStream.Receive` → guardrail → reply-publish loop, with a publish failure immediately closing the upstream stream, explicit terminal-error precedence, monotonically validated sequence numbers, and exactly one final frame. The nil-channel `select` technique from the studies does not apply to Scout's pull-based `ModelStream` interface, which exposes `Receive` and `Close`, not token and error channels.

Do not enforce `MaxOutputTokens` by counting frames: providers may batch several tokens into one frame, and non-native streaming adapters intentionally return an entire completion as one frame. Enforce the provider request limit at submission and accumulate `ModelChunk.Usage.OutputTokens` when the provider reports incremental usage. If a provider reports usage only at the end, exact mid-stream enforcement is impossible without a tokenizer/count capability; state that limitation instead of treating frames as tokens.

**From.** `q31` and `q32`. Their error-precedence and send-failure-cancels-generation rules carry over even though their channel mechanics do not.

**Owner.** Scout.

### B4. Durable turn cancellation is absent

**Gap.** Context cancellation and `ModelStream.Close` can stop work still owned by one process, but there is no `TurnCanceller` or durable cancellation signal in `contract/`/`domain/`. A client connected to one API replica cannot reliably stop a turn leased by another worker, and there is no stated policy for a new prompt arriving mid-generation. For a distributed chat product this is a launch blocker, not a refinement.

**Proposal.** Add `TurnCanceller` to `contract/data_plane.go` (`Cancel(ctx, tenantID, requestID, reason)`), and make the mid-turn policy explicit as a `domain.TurnAdmissionPolicy` enum — *queue*, *reject while active*, or *cancel-and-replace* — resolved from tenant policy rather than assumed. Per-turn cancellation must be a child of the session context so cancelling a turn never tears down the session. Cancellation is an idempotent durable state transition so another API replica can observe it.

**From.** `q33`. Its channel-drain warning translates into a contract requirement here: `ModelStream.Close` must unblock any in-flight `Receive` and release provider resources. The Scout pump should not start an unbounded background drain to compensate for a broken provider adapter.

**Owner.** Scout.

### B5. Micro-batching for embeddings

**Gap.** `EmbeddingGateway.Embed(ctx, tenant, content)` embeds one document at a time; `KnowledgeIngestor.Ingest` ingests one document at a time. Batch-capable embedding providers can reduce per-item request overhead and improve accelerator utilization; the current port cannot exploit that capability.

**Proposal.** `service/knowledge/batching_embedder.go` preserving the current single-item `EmbeddingGateway.Embed` API for callers, backed by a new narrow batch-capable provider port (for example, `EmbeddingBatchProvider.EmbedBatch`). A decorator over the existing single-item gateway alone cannot create a provider batch. Flush at `maxBatch` items or `maxWait` since the *first* queued item, use one timer per open batch, allow per-caller cancellation before flush, and fail the entire batch when the provider response count does not match the request count.

The current embedding call does not carry a model selection. Construct one batcher per immutable knowledge-version embedding configuration, or evolve the request DTO to carry that identity; never infer it from content or tenant alone. Batch only requests with the same provider, model/version, vector dimension, region/residency policy, and any other provider-affecting option. Prefer tenant-homogeneous batches initially; cross-tenant batching is acceptable only when payload/result correlation, observability, and memory clearing are proven not to leak content or attribution.

**From.** `q15`. That last rule is the safety-critical one: a count mismatch must fail the whole batch, never deliver response *i* to caller *j*. In a multi-tenant platform that is a cross-tenant data leak, not a bug.

**Owner.** Scout.

### B6. Implement a policy-driven `ModelRouter` before adaptive or hedged routing

**Gap.** `ModelRouter` has no implementation, and `ModelSelection` carries only provider, model, and capacity pool. The existing gateway therefore assumes a selection was produced elsewhere, while A6 and B1 need richer candidate, health, version, region, and attempt-diversity information. Without a reference router, each downstream will make tenant entitlement and fallback decisions differently.

**Proposal.** Add a Scout router over injected, immutable inputs:

- a candidate catalogue filtered by model capability/version, tenant access, region/residency, and required context/output limits;
- a health/capacity view with freshness timestamps and predicted queue delay;
- a deterministic policy that ranks compatible candidates by deadline feasibility, quality class, capacity locality, and estimated minor-unit cost;
- an auditable routing reason and the immutable catalogue/health generation used for the decision;
- explicit degradation/fallback policy from tenant configuration, never an implicit provider substitution.

The current `ModelSelection` is too small for rollout and hedging provenance. Evolve it deliberately—for example with model version, region, route/replica identity, and routing generation—before consumers proliferate. Do not place live provider discovery inside the router; adapters publish health/capacity snapshots and the router remains a testable policy service.

**From.** `q61`'s request lifecycle and routing criteria, plus `q64`'s capacity/placement signals.

**Owner.** Scout for routing policy and DTOs; provider health discovery remains an injected adapter.

### B7. Make guardrail enforcement stateful, composable, and usable

**Gap.** Invariant 8 depends on `GuardrailEnforcer`, but Scout has no implementation. `GuardrailConfig.Rules` is opaque bytes, so there is no versioned provider-neutral rule envelope, validation contract, or reference engine. More subtly, `AfterModelChunk(config, chunk)` looks stateless: PII, injection, toxicity, secrets, or forbidden phrases can span chunk boundaries, and buffering policy affects TTFT. An implementation that keeps hidden request state in a global map would recreate lifecycle and leak risks.

**Proposal.** Separate orchestration from rule providers:

- a Scout `GuardrailPipeline` composes a release-independent mandatory baseline with the pinned versioned tenant/agent policy; tenant rules may strengthen but never disable the baseline;
- typed/versioned rule envelopes are validated at publication and compiled once per immutable digest, with bounded caching; add the schema's `rules_digest` to `domain.GuardrailConfig` so runtime code can verify and key that compilation;
- streaming output opens an explicit per-request inspection session (`InspectChunk`/`Close`) that owns cross-chunk state and bounded buffering, rather than hiding state behind the current chunk method;
- input, tool-argument, tool-result, and output stages fail closed according to typed policy outcomes and record redacted rule IDs/version/duration, never the rejected secret/content;
- reference rules cover deterministic structural controls (size, schema, destination/tool allowlists, exact/regex policies where safe), while PII/toxicity/malware/jailbreak classifiers remain replaceable providers behind focused interfaces.

Evolve the current interface through an optional stateful-output capability or the next contract version. Wire the pipeline into the reference turn composition and governed tool/model paths: today `GovernedGateway` validates a tool result schema but does not call `GuardrailEnforcer.BeforeTool`/`AfterTool`, so the existence of the port does not make the boundary unavoidable. Define what happens to already-buffered output on a later violation: no unapproved bytes are published, the provider stream is cancelled, the turn ends with a policy-safe terminal frame, and the security/audit event is durable.

**From.** `q32`'s guarded streaming path and `q65`'s release-independent baseline plus release-specific policy.

**Owner.** Scout for pipeline, lifecycle, rule envelope, and deterministic reference rules; classifier providers remain injectable.

---

## C. Caching — from `q41`–`q45`, `q62`

### C1. Bounded local cache adapters for the reference data plane

**Status.** `dataplane.NewMemorySessionCache` and `NewMemoryGraphCache` expose bounded, closable local adapters with explicit capacity and TTL. `SessionCoordinator` and `DefinitionResolver` accept those adapters through their existing contracts. Distributed adapters remain downstream choices.

**Proposal.** Put a bounded TTL/LRU memory-cache primitive in keel, then add thin Scout adapters that serialize and identity-check `SessionSnapshot` and immutable `ExecutionGraph` values. Configure explicit maximum entries/bytes and TTL; expose `Close()` so the owning binary stops any sweeper deterministically. This gives local development and single-process deployments a safe default without duplicating generic cache mechanics in Scout.

**From.** `q41` and `q43`.

**keel note.** keel's `cache.MemoryCacheService` is string-valued, unbounded, and has no LRU. Capacity control is horizontal and belongs there (or in a new keel bounded-cache type); the typed key/value mapping and tenant identity checks belong in Scout.

**Owner.** keel for bounded cache mechanics; Scout for `HotSessionCache` and `ExecutionGraphCache` adapters.

### C2. Singleflight on every cache-miss path

**Status.** `DefinitionResolver.Resolve` and `SessionCoordinator.Load` coalesce concurrent misses by immutable graph key and tenant conversation key. `PublishedAgentResolver` and `ReadinessResolver` are not cache-miss call sites.

**Proposal.** Reuse `golang.org/x/sync/singleflight` if its cancellation/lifetime semantics fit, or add a reusable load-coalescing primitive to keel; do not create a generic Scout-only package. Apply it to the two real cache-miss paths. Two details from `q42` are easy to get wrong: the shared load must not inherit only the first waiter's cancellation, and it still needs its own bounded timeout so a detached load cannot live forever; publish the result *before* waking waiters so they get a happens-before edge. Consider short negative caching only for immutable definitions that are definitively absent, not for sessions whose creation may be racing the read.

**Owner.** keel or an upstream Go package for the mechanism; Scout for the two call sites.

### C3. Two-tier cache discipline, stated once

**Gap.** `HotSessionCache` is a single tier. A realistic deployment has an in-process tier in front of Redis, and the correctness rules — write-through ordering, invalidation-versus-in-flight-read races, local TTL never outliving remote TTL — are exactly what downstreams will get wrong silently.

**Proposal.** Either a `service/dataplane/tiered_session_cache.go` implementing `HotSessionCache` over a local cache plus keel's `cache.CacheService`, or (cheaper) a documented rule set in the README. The one non-obvious mechanic worth encoding either way: a successful durable write/invalidation must mark any *overlapping in-flight read* as invalidated, or a slow read can promote pre-write data over the new revision. Compare snapshot revisions before local promotion, keep local TTL no longer than remote TTL, and perform invalidation only after the authoritative write succeeds.

**From.** `q44`.

**Owner.** Scout.

### C4. Semantic response cache — research item, not an early public contract

**Gap.** None exists. But the reason to build it here rather than downstream is structural: semantic caching is dangerous because a cache hit can cross intent, authorization, model-version, conversation-state, or policy boundaries. Scout already pins agent, guardrail, tool, and knowledge versions and scopes operations by tenant; D1 supplies the missing within-tenant entitlement identity. Together those let Scout construct a safer cache namespace than each downstream reassembling it by hand, though intent and mutable conversation state still require conservative cacheability rules.

**Proposal.** Do not add `contract.ResponseCache` until a narrow safe use case is demonstrated in shadow mode. A later `service/modelgateway/semantic_cache.go` would need:

- namespace = tenant + full immutable release identity + model/provider version + prompt/agent digest + guardrail version + knowledge/index version + tool-contract set + entitlement fingerprint + language + decoding parameters. Conversation state and time-dependent/tool-using requests are non-cacheable unless that state is also represented;
- pluggable `SimilarityIndex` (exact brute force now, ANN later) with the tenant filter applied **inside** the index query, never as a post-filter over nearest neighbours;
- a precision floor the caller cannot lower, and explicit refusal to store non-deterministic or time-sensitive responses;
- hit *quality* metrics, not just hit rate, plus deletion/retention behavior matching the source request.

**From.** `q45`, plus `q62`'s cache-key rule (entitlement fingerprint in the key).

**Owner.** Scout only after the safety envelope is proven. This proposal can return a confidently wrong or unauthorized answer, so start as an internal experiment, default off, and shadow-measured. Stateless deterministic tasks are the first candidate; RAG, tools, mutable entitlements, and ongoing conversations should initially be excluded.

---

## D. Retrieval and ranking — from `q51`–`q55`, `q62`

### D0. Implement the safe baseline `KnowledgeRetriever` first

**Gap.** The document jumped directly to hybrid search and reranking, but the base `KnowledgeRetriever` and `KnowledgeVectorIndex` have no Scout implementation. More fundamentally, the contracts do not match the schema: `KnowledgeVectorIndex.Index` accepts one document+embedding, while search returns `KnowledgeMatch.ChunkNo`; `KnowledgeDocumentStore.Get` loads a whole document, while `knowledge_chunk` stores the authorized chunk URI/digest and vector reference. `KnowledgeVectorIndex.Search` also returns `KnowledgeResult`, whose matches can already contain content/source URI, even though the README requires relational revalidation *before* content is loaded. A correct chunked RAG path cannot be expressed safely end to end yet.

**Proposal.** First introduce a value-only `KnowledgeChunk` identity and aligned chunk store/index ports (or make the existing breaking contract change before implementations exist): indexing receives tenant+base+version+document+chunk number, content digest/token count, and embedding; vector search returns content-free candidate identities and scores; retrieval loads the authorized chunk by that same composite identity and alone constructs `KnowledgeMatch` with content/source attribution. Then `service/knowledge/retriever.go` composes, in order: query validation and deadline budget; governed query embedding; vector search with tenant, knowledge-base, immutable-version, and entitlement filters applied at the index; deterministic top-K normalization/deduplication; relational authorization/binding revalidation; then authorized chunk loading and source attribution. Bound overfetch, total bytes, per-document chunks, and all dependency calls. Return no content for a candidate that fails revalidation, and record the mismatch as a security/consistency signal rather than silently treating it as ordinary low recall.

Ship at least one replaceable reference `KnowledgeVectorIndex` adapter over a supported keel/database capability when practical, plus a conformance suite for tenant/version filtering, deterministic ordering, cancellation, and authorization. Hybrid retrieval and reranking in D2 are decorators over this safe baseline, not substitutes for it.

**From.** `q51`, `q55`, and `q62`.

**Owner.** Scout for governed retrieval and conformance; storage-specific index implementations remain interchangeable.

### D1. `KnowledgeQuery` has no user, no filters, and no budget

**Gap.** `domain.KnowledgeQuery` carries `TenantContext`, ids, `Query`, `TopK`. There is no acting user, no entitlement set, no metadata filter, and no latency budget. Tenant-level isolation is necessary and not sufficient: enterprise RAG fails on *row and column level* authorization within a tenant, and there is currently no place to put it.

**Proposal.** Extend `KnowledgeQuery` with the acting principal/reference, a provider-neutral structured authorization scope and metadata filter, an entitlement-version fingerprint for cache namespacing, and a retrieval deadline/budget. Do not accept caller-supplied SQL or an opaque "compiled predicate" that adapters cannot validate. Each vector/keyword adapter compiles the structured scope into its native filter and applies it *inside* the index query. Post-filtering nearest neighbours leaks existence, harms recall, and can select a forbidden document before the application ever sees it. Keep the existing relational revalidation before loading chunk content as defense in depth, not as the primary filter.

**From.** `q62`.

**Owner.** Scout.

### D2. A reranker seam and hybrid fusion

**Gap.** `KnowledgeRetriever.Retrieve` returns ranked matches in one shot. There is no seam for retrieve-then-rerank, and no way to express a keyword leg alongside the vector leg — so a downstream that needs either has to replace the whole port.

**Proposal.** Add `contract.KnowledgeReranker`, and add a narrow keyword-search port only if a concrete adapter needs it. A `service/knowledge/hybrid_retriever.go` can then run vector and keyword legs concurrently under one deadline, apply the same authorization scope in both legs, fuse with reciprocal-rank fusion, rerank the overfetched authorized candidate set, and **skip the optional rerank stage when its remaining budget is insufficient** rather than blowing the deadline. The existing `KnowledgeVectorIndex` cannot supply a keyword leg by itself.

**From.** `q55` (two-stage retrieve-then-rerank, exact scoring to repair approximate ANN ordering) and `q62`.

**Owner.** Scout.

### D3. Sharded merge with early stop

**Gap.** When a tenant's knowledge version spans index partitions, the naive merge reads everything from every shard.

**Proposal.** A `MergeTopK` helper over sorted shard iterators: max-heap over shard heads, lazy advance, stop as soon as K is known, dedupe by first-seen id (correct precisely because global score order guarantees the first copy is the highest), memory `O(N + K)`.

**From.** `q52`, including the caveat to document: early stop is only exact if shard streams are truly sorted; approximate streams need a certified upper bound per shard or an explicitly stated recall target.

**Owner.** Scout, as an internal helper — not a new public contract.

---

## E. Observability, cost, and release — from `q54`, `q61`, `q63`, `q65`

### E1. Bounded-cardinality tenant metrics

**Gap.** `RuntimeMetrics.RecordTurn/RecordStep/RecordDependency` all expose tenant identity to the metrics adapter. That identity is necessary for exact accounting and tenant-scoped diagnostics, but an adapter that uses it as a global time-series label creates unbounded cardinality. The contract currently states no rule preventing that mistake.

**Proposal.** First define a label policy: fleet metrics use bounded dimensions such as tenant tier, priority class, model, region, and outcome; exact tenant usage stays in the ledger, tenant-scoped logs/traces, or a protected query path. Although `RecordTurn` receives full request/result DTOs, adapters must never place prompts, responses, tool arguments, document IDs, request IDs, or conversation IDs into labels. A Count-Min sketch plus bounded candidate heap can power a separate top-N operational view, but it should not dynamically place raw tenant IDs into the main metric labels: membership churn creates high cardinality over time even when only N tenants are visible at once. If top-N time series are required, export stable rank slots (`tenant_rank=1..N`) with identity resolved outside the metrics backend, and merge sketches across replicas with compatible hash seeds/windows.

**From.** `q54`. The sketch is an optional bounded analytics mechanism; the cheapest and safest first change is the explicit metric-label policy.

**Owner.** Scout (the policy); the sketch itself would be equally at home in keel.

### E2. Stage- and version-attributed observations across the turn lifecycle

**Gap.** `domain/errors.go` has fourteen sentinels describing *what* failed, and runtime metrics do not explicitly describe *where*. When a turn fails, "was that retrieval, prompt construction, the model, the guardrail, or reply publication?" must be reconstructed from logs. `q63`'s harder attribution question also needs the exact model, prompt, retrieval/index, tool, guardrail, and evaluator versions—a stage label alone is necessary but insufficient.

**Proposal.** Add value-only observation DTOs, for example:

```go
type StageObservation struct {
    Stage     TurnStage
    Component string
    Version   string
    Duration  time.Duration
    Usage     Usage
    Outcome   string
}
```

Evolve `RuntimeMetrics` to accept structured observations (or add a new optional recorder interface during migration), while continuing to pass the original error separately for `errors.Is`/`errors.As`. This respects Scout's rule that `domain/` contains values rather than behavior; a `StageError` with `Error`/`Unwrap` methods in `domain/` would violate that convention. Orchestration code may use private wrappers internally, but durable/metric attribution should be explicit data with immutable component provenance and a redacted trace ID.

**From.** `q32`'s `RAGError` and `q33`'s `ChatError`, generalized.

**Owner.** Scout.

### E3. Turn the latency measurement plan into request-aware budgets

**Gap.** The README ships a stage-by-stage **starting p95 measurement plan**, not a universal per-request deadline. Nothing yet turns a tenant/request deadline and observed stage latency distributions into admission decisions. Treating the table itself as hard timeouts would incorrectly reject valid slow-tier or long-generation requests.

**Proposal.** Add an injected `LatencyPolicy`/budget allocator that derives per-stage deadlines from the caller's remaining deadline, tenant SLO class, request shape, and measured/predicted stage cost. **Reserve generation time first**, refuse to start a stage that cannot fit, and define which optional stages may degrade (for example, skip reranking) versus which must fail closed (authorization, guardrails). A degraded result must carry an explicit reason; do not silently omit retrieval or safety work.

**From.** `q32` ("if retrieval exceeds its budget, return a deadline-style error rather than starting generation late") and `q62`.

**Owner.** Scout.

### E4. Reservation reconciliation and orphan sweeping

**Gap.** `TenantBudgetManager` reserves and commits, but nothing describes what happens when a worker dies between the two. Reservations leak and the tenant is silently throttled by budget that was never spent.

**Proposal.** Store `expires_at` and an attempt number. A nonterminal redelivery fences an expired attempt before reserving the next; terminal turns cannot renew. A separate Worker expires rows in bounded batches. Expiry is not proof of zero spend, so reconciliation still uses immutable usage events and terminal turn state.

Also state the billing policy explicitly: client disconnect cancels delivery, but provider work produced before cancellation is still metered. Reconciliation uses provider-confirmed/tokenizer-confirmed usage, not the number of frames successfully sent to the client.

**From.** `q61`.

**Owner.** Scout.

### E5. `RolloutHealthEvaluator` conflates "I don't know" with failure

**Gap.** `Healthy(ctx, target) (bool, error)` has only a binary domain result. An implementation can return an error for stale telemetry or insufficient samples, so pause is technically representable, but infrastructure failure and a valid "not enough evidence yet" decision become indistinguishable. That ambiguity invites controllers to handle the safe state inconsistently.

**Proposal.** Add an optional detailed evaluator returning a structured verdict — `healthy` / `unhealthy` / `insufficient_evidence` — carrying telemetry freshness, sample count, observation window, breached metric/slice, practical effect size, and confidence. Reserve `error` for inability to evaluate. The controller pauses on insufficient evidence or evaluation error, advances only on healthy, and rolls back/quarantines only according to explicit hard/soft breach policy. Add consecutive breached windows, minimum duration, and cooldown so one noisy window cannot flap the rollout.

**From.** `q65`, which states the rule directly: loss of trustworthy metrics pauses promotion; it does not declare success or failure.

**Owner.** Scout. Small contract change, disproportionate correctness gain.

### E6. Version pinning has no precedence rule

**Gap.** `AgentVersionTrafficManager.ResolveVersion(tenantID, agentID, conversationID)` can perform sticky canary selection but has no explicit pin contract or implementation. `runtime.PublishedAgentResolver` currently implements stable/canary hashing directly, and the `agent_conversation` table already persists the selected `agent_version`. `q65`, however, describes a broader immutable release bundle (model+prompt+RAG+tools+safety), while Scout deliberately separates tenant agent-version rollout from platform-release rings. Conflating those two controls would make rollback semantics ambiguous.

**Proposal.** First make `AgentVersionTrafficManager` the single policy used by `PublishedAgentResolver`, then add an `AgentVersionPin` with scope, region, effective/expiry dates, owner, reason, and approval/audit metadata. Define agent resolution precedence (compliance pin → approved tenant pin → canary cohort → stable deployment), persist the result when the conversation is created, and always read the persisted version on later turns. Rollback changes only new conversation assignments; existing conversations drain on their pinned immutable definition unless a critical safety policy explicitly cancels them.

Handle **platform release** pinning separately in `PlatformReleaseRolloutController`/deployment control. If Scout later introduces a composite runtime release that bundles agent, model, retrieval, tools, and safety, model it explicitly rather than overloading `agent_version`. In every case, garbage collection must respect live conversations, pins, and audit retention.

**Owner.** Scout.

### E7. Quality evaluation needs immutable evidence, not only pass/fail contract tests

**Gap.** `ContractTestCase` contains agent/version/input/assertions, and `ContractTestResult` contains only pass/fail plus failure strings. That is appropriate for functional compatibility but cannot support `RolloutHealthEvaluator`'s promised quality decision: there is no dataset revision, baseline/candidate manifest, evaluator/judge version, slice, metric, sample count, confidence, cost, or evidence freshness. Without those, a rollout can say "quality regressed" but cannot reproduce or audit why.

**Proposal.** Keep `AgentContractTestRunner` focused on deterministic compatibility and introduce a separate evaluation workflow:

- an immutable `EvaluationManifest` pinning candidate and baseline agent/model/prompt/knowledge/index/tool/guardrail versions, dataset revision, decoding settings, and every evaluator version;
- versioned golden-set metadata with provenance, consent/retention class, risk and domain/language slices, rubric, and payload URI+digest rather than large content in relational rows;
- pluggable heuristic, calibrated LLM-judge, and human-review evaluators behind separate interfaces, with disagreement/low-confidence routing and judge–human agreement tracked by evaluator version;
- paired baseline/candidate results by slice with sample counts, practical deltas, confidence intervals, latency, usage, and minor-unit cost;
- an immutable, expiring gate decision consumed by E5, including explicit exemptions/approvals and telemetry freshness.

Run evaluation in a separate Worker, not the serving path. Production samples require tenant opt-in/policy, redaction before the evaluation store, residency/retention enforcement, strict sampling and spend budgets, and no prompt/content in metrics labels. Business-specific rubrics and KPIs remain downstream inputs; Scout owns manifest provenance, orchestration, common evidence types, and rollout-gate semantics.

**From.** `q63`, connected to `q65`.

**Owner.** Scout for the evaluation framework and evidence model; downstream for domain truth, rubrics, and business outcomes.

---

## F. Concurrency hygiene in existing code — from `q11`–`q13`

### F1. `toolgateway.RetryPolicy` has no jitter

**Gap.** `service/toolgateway/retry_policy.go` is deterministic exponential backoff. When a shared dependency blips, every worker in the fleet retries in lockstep at exactly `base`, `2×base`, `4×base` — a self-inflicted thundering herd on a dependency that is already struggling.

**Proposal.** Full jitter (`rand` in `[0, delay]`) with an injected concurrency-safe randomness function/source so tests stay deterministic. Preserve exponential capping before jitter and honor the remaining context deadline—do not schedule a retry that cannot start in time. Separately, add a retry-budget decorator keyed by tool/dependency that caps retries as a fraction of recent successful original calls; a per-call attempt cap alone does not prevent a fleet-wide retry storm. The budget's coordinated-versus-local semantics must be explicit.

**From.** `q12`.

**Owner.** Scout. Smallest diff in this document, and arguably the highest ratio of risk removed to lines changed.

### F2. `ContractTestRunner` is strictly sequential

**Gap.** `service/release/contract_test_runner.go` executes a risk-stratified corpus one case at a time, each a full governed agent turn. For a corpus of any realistic size this makes the release gate too slow to run per build, which means it will be skipped.

**Proposal.** Prevalidate the entire corpus (including duplicate IDs) before starting any work, then run at most *N* cases concurrently and write results by input index. Put `MaxConcurrency` on runner configuration injected from the owning binary; the current `Run` signature has no caller-supplied concurrency. Define partial-result semantics before implementation: on infrastructure failure, cancel work not yet started, wait for started cases to exit, and return successful completed results in input order plus the earliest indexed error. If the release gate needs a result for every case, evolve `ContractTestResult` to carry infrastructure failure instead of overloading the method-level error.

**From.** `q11`, which is precisely this function.

**Owner.** Scout.

### F3. Knowledge ingestion wants a bounded pipeline

**Gap.** `KnowledgeIngestor.Ingest` is per-document and synchronous, `KnowledgeDocument` carries a source URI/digest rather than content, and the current vector-index port cannot represent chunks (D0). Ingestion is inherently load+verify → decode/chunk → embed → index+persist, with wildly different per-stage costs and rate limits.

**Proposal.** Add injected source-loader/media-decoder/chunker ports and a bulk-ingestion contract because the current one-document `Ingest` method cannot express corpus progress or per-item errors. `service/knowledge/ingest_pipeline.go` is a bounded synchronous batch executor called by a separate ingestion Worker binary: verify the source digest before decoding, use deterministic versioned chunking, apply per-stage worker counts and **bounded handoff channels** so a slow index applies real backpressure, return correlated per-document results rather than an uncorrelated error channel, and close stages only after every writer exits. Persist chunk content through object storage and atomically publish relational chunk metadata/vector references only after successful indexing, with cleanup/reconciliation for partial external writes. It pairs naturally with B5 but does not start a durable background goroutine from an HTTP service.

Define item-level retry/idempotency around tenant+knowledge-version+document+content-digest. A systemic error (canceled context, provider outage, invalid configuration) cancels the batch; an isolated invalid document records a terminal item result without discarding unrelated work.

**From.** `q13` — whose worked example is literally "decode → embed → index".

**Owner.** Scout.

---

## G. Testability

### G1. Inject the clock

`isolation.ExecutionGovernor.Now` is the only injectable clock in Scout's services. `controlplane.StudioService`, `runtime.BaseDraftTester`, `mcp/envelope.go`, `toolgateway` retry waiting, and Google video polling read wall time or construct timers directly. Not all of those need abstraction immediately, but every new mechanism above—buckets, breakers, TTLs, sweepers, windows, hedge delays, and backoff—needs deterministic time. `q22`, `q23`, `q41`, `q43`, and `q53` all inject a clock, and `q23` goes further by taking `now` as a parameter so the limiter never reads a clock at all.

Adopt one convention before writing the mechanisms, not after. A useful abstraction must cover both `Now` and waiting/timers; injecting only `Now` still leaves sleep-based tests. The abstraction is horizontal and belongs in **keel**, with Scout services receiving it through constructors or fields.

### G2. Prove the absence of leaks

New concurrent services need lifecycle tests, not just happy-path behavior tests: deterministic construct/use/close completion, `-race` in CI, and cases for the four shapes `q11` names—the function blocks until cancellation, one worker fails early, input is empty, and the worker count is invalid. Raw `runtime.NumGoroutine` equality is noisy; prefer explicit wait groups/hooks or a proven leak checker around isolated tests. Extend `internal/fake` with a controllable clock, blocking provider/stream, and close acknowledgements so shutdown can be asserted without sleeps.

### G3. Make limits and lifecycle explicit configuration

Every proposal introduces limits: rates, bursts, maximum tenants, queue lengths, cache bytes, batch sizes/waits, breaker windows, replay frames, sweep batch sizes, and concurrency. Library constructors should accept validated config structs and reject unsafe zero/negative combinations; deployable binaries map those values from documented `flag` declarations. Do not hide fleet behavior in package globals or environment reads. Any service that owns timers, goroutines, or subscriptions also exposes idempotent `Close` and a clear owner responsible for calling it during worker shutdown.

---

## H. Dependency-aware implementation sequence

The earlier ranking over-rewarded small diffs. Jitter is valuable, but it does not make the platform executable. The better sequence finishes one governed, recoverable vertical slice and only then optimizes it. Effort estimates should be made after the contract decisions and provider choices below; false precision here would obscure dependencies.

| Phase | Scope | Exit criterion |
|---|---|---|
| 0. Contract and test foundations | G1, G3, A2, E2, E5, B0a, D0's chunk-contract alignment; decide optional capability interfaces versus the next breaking contract version | Time, limits, identity mappings, retry advice, rollout verdicts, observations, and lifecycle ownership are explicit and deterministically testable |
| 1. Fill the reusable policy dependencies | A1, A5, A7, A8, A9, B6, B7, C1; include F1 as a parallel quick win | Scout ships safe defaults/reference adapters for the common policy/cache dependencies currently missing from `ExecutionGovernor`, model/tool gateways, guarded streaming, `SessionCoordinator`, and `DefinitionResolver`; product-specific authorization, credentials, and transports remain injected |
| 2. Governed recoverable turn | A3, E4, B0, B3, B4, B2, C2; queue/reply/idempotency conformance suites included | One turn can be admitted, budgeted, dispatched, resumed after worker failure, guarded, streamed/cancelled, settled, and acknowledged without downstream reimplementation of the lifecycle |
| 3. Distributed correctness and production hardening | A4, C3, E1, E3; multi-replica failure, partition, overload, and shutdown tests | Scaling API/worker replicas does not multiply quotas, corrupt cache state, create unbounded labels, or start work that cannot meet its deadline |
| 4. Safe knowledge baseline | D0, D1, F3, B5, then D2 and D3 | Authorized immutable knowledge can be ingested and retrieved end to end; batching, hybrid fusion, and reranking improve that baseline without weakening isolation |
| 5. Release evidence and throughput | F2, E5 implementation, E6, E7 | Agent/platform rollout decisions are reproducible, adequately sampled, pausable, and sticky for active conversations |
| 6. Measured optimizations | A6, B1, C4 | Adaptive concurrency, hedging, and semantic caching ship only with cost/quality baselines, budgets, kill switches, and shadow/canary evidence |

Within a phase, prefer the smallest end-to-end increment that proves the phase exit criterion. Avoid landing a collection of disconnected primitives without wiring at least one real Scout composition through them.

## I. Deliberately not proposed

- **A duplicate generic KV cache, HTTP client, or queue inside Scout.** keel owns these primitives (`cache.CacheService`, `common/http_client.go`, dispatcher/outbox and worker lifecycle). Scout should still implement its typed adapters and agent-specific queue/cache/reply behavior over them, as proposed in B0 and C1–C3.
- **A generic clock, hash, or heap package in Scout.** Horizontal; keel or the standard library.
- **Another model-vendor adapter solely for breadth.** `provider/` already covers Anthropic, OpenAI, and Google. Add another only for a concrete capability/market need; the study set points to orchestration and resilience gaps first.
- **A full GPU bin-packer/autoscaler inside Scout.** `q64`'s placement, gang scheduling, topology, spot-capacity, and node-drain controller belongs to the model-serving infrastructure or a dedicated serving control plane. Scout should expose/use queued-token work, TTFT/TPOT, rejection, model-version, KV pressure, and capacity outcome signals (A5, A6, B6), not become a Kubernetes/GPU scheduler.
- **Replacing keel's `port.WebSocketHub`.** B2 is per-generation token fan-out with backpressure and replay; the keel hub is per-user broadcast. Different problems.
- **Public Scout APIs copied from interview exercises.** `q51` and `q53` are useful mechanics for D0/A7/A9, not domain contracts to copy verbatim.

## J. What the study set does not cover

Worth stating so this document is not mistaken for a complete backlog. The study set contributes nothing to Agent Studio, prompt inheritance and compilation, the MCP adapter layer, the keel schema, or the `studio-v1` HTTP profile — which together are roughly half of Scout's current surface and the half that is genuinely differentiated. The overlap is concentrated in the data plane, which is precisely the half that is currently contracts without mechanisms.

---

*Reviewed: 30 numbered studies plus two companion files, `q11`–`q65`, `~/dev/jobprep/mlsol`, against Scout `ce07dee` (`v0.2.1`). 2026-08-12.*
