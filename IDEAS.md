# Scout improvement ideas

Derived from a review of the interview study set in `~/dev/jobprep/mlsol` (files `q11` … `q65`, 40 files) against Scout at `3d7bca0`.

The study set is not scaffolding to copy. It is a catalogue of the exact mechanisms Scout currently declares as interfaces and leaves unimplemented. This document maps each study family onto a concrete Scout gap and proposes where the mechanism should live.

## 1. The finding in one paragraph

Scout owns an unusually complete *vocabulary* for a multi-tenant agent platform — thirteen contract files, 56 tables, immutable versioning, tenant scoping at every boundary — and almost none of the *mechanisms* those contracts describe. `contract/isolation.go` declares six control concerns; `service/isolation/` implements one. `contract/data_plane.go` declares thirteen ports; `service/dataplane/` implements the three that are pure composition. Every limiter, breaker, cache, scheduler, hedger, batcher, and fan-out in the study set corresponds to a Scout interface with no product-neutral implementation behind it. The result is that every downstream platform must write the same concurrent, hard-to-test code — and Scout's stated promise ("Do not add a Scout abstraction that duplicates a keel primitive. Add reusable horizontal behavior to keel; add agent-domain behavior to Scout") is only half kept: the abstractions landed, the behavior did not.

| Contract | Declared | Product-neutral implementation in `service/` |
|---|---|---|
| `TenantRateLimiter` | `contract/isolation.go` | none |
| `TenantBudgetManager` | `contract/isolation.go` | none |
| `ConcurrencyLimiter` / `ConcurrencyLease` | `contract/isolation.go` | none |
| `CostCircuitBreaker` | `contract/isolation.go` | none |
| `LoopDetector` | `contract/isolation.go` | none |
| `ExecutionGovernor` / `ExecutionPermit` | `contract/isolation.go` | `isolation.ExecutionGovernor` |
| `CapacityScheduler` / `CapacityLease` | `contract/model_runtime.go` | none |
| `ModelRouter` | `contract/model_runtime.go` | none |
| `HotSessionCache` | `contract/data_plane.go` | none |
| `TurnReplyPublisher` / `Subscriber` | `contract/data_plane.go` | none |
| `FairTurnScheduler` / `TurnDispatcher` | `contract/data_plane.go` | none |
| `StepIdempotencyStore` | `contract/data_plane.go` | none |
| `DeadLetterQueue` | `contract/data_plane.go` | none |
| `ToolCircuitBreaker` | `contract/tool_gateway.go` | none |
| `KnowledgeRetriever` / `KnowledgeVectorIndex` | `contract/knowledge.go` | none |
| `RolloutHealthEvaluator` | `contract/release_and_observability.go` | none |

`SessionCoordinator` and `DefinitionResolver` both hard-fail when their cache is nil, so today a downstream cannot run a single-node deployment without first writing a cache adapter.

## 2. Layering rule applied throughout

Every proposal below states which repository should own it. The test used:

| Question | Owner |
|---|---|
| Would this code be identical in a payments or logistics backend? | keel |
| Does it need tokens, cost minor units, model/agent versions, turns, prompts, or guardrails to make sense? | Scout |
| Does it encode which agents exist, what prompts say, or which provider to buy? | downstream |

So: a bare token bucket is keel; a bucket charged in *estimated prompt + max-new tokens* and refunded on early stop is Scout. A generic KV cache is keel (`cache.CacheService` already exists); a cache keyed by *pinned agent version + guardrail policy version + tenant entitlement fingerprint* is Scout.

Two of the proposals require a small **additive** keel change, noted inline. Neither is breaking.

---

## A. Isolation and admission — from `q14`, `q21`–`q25`

### A1. Ship a product-neutral `TenantRateLimiter` (tenant bucket × fleet bucket)

**Gap.** No implementation. Every downstream writes its own, and the two-bucket acquisition (per-tenant *and* global) is the part people get wrong — partial acquisition leaks tokens from the bucket that succeeded.

**Proposal.** `service/isolation/tenant_rate_limiter.go`: one mutex covering both buckets so acquisition is atomic, monotonic-clock refill, no per-tenant ticker, hard cap on tracked tenants with a cooldown-throttled sweep of fully-refilled buckets. `AllowTurn` / `AllowToolCall` / `AllowModelCall` differ only in which limit set they charge.

**From.** `q21`. Its sweep argument is worth preserving verbatim in the code comment: evicting a *drained* bucket hands its owner a full one, so eviction must only drop buckets that are already full, and refuse (`ErrTenantCapacity`) rather than evict when at cap. That is a real multi-tenant abuse vector and not obvious.

**Owner.** Scout — the three methods are agent-domain (`domain.ToolCall`, `domain.ModelRequest`). The bucket arithmetic itself could sit in keel as a primitive.

### A2. Make `ErrRateLimited` carry `RetryAfter`

**Gap.** `domain.ErrRateLimited` is a bare sentinel. `handler/studio.go:383` maps it to a status code and cannot emit a `Retry-After` header, because the limiter contract discards the one number it computed. Every one of `q21`, `q22`, `q23` treats accurate retry-after as a first-class output.

**Proposal.** Add a typed error to `domain/errors.go`:

```go
type RateLimitedError struct {
    Scope      string        // tenant | fleet | tool | model
    RetryAfter time.Duration
    Err        error         // wraps ErrRateLimited
}
```

`errors.Is(err, domain.ErrRateLimited)` keeps working; handlers gain `errors.As` for the header. Same shape for `ErrBudgetExceeded`.

**From.** `q23`, whose follow-up answer is the honest caveat to document: retry-after computed under the admission lock is *advice*, not a reservation — a later request can invalidate it. Say so in the doc comment rather than implying a guarantee.

**Owner.** Scout (`domain/`). Cheap, unblocks correct HTTP behavior downstream, no new dependency.

### A3. Cost-weighted admission with reserve/refund, priority aging, and deadline feasibility

**Gap.** `TenantBudgetManager` has the right shape (`Reserve` → `Commit`/`Release`) and no implementation. Separately, `isolation.ExecutionGovernor` never asks whether the work can *finish* in time — it checks `TurnTimeout` elapsed, never whether the expected admission delay already exceeds the caller's remaining deadline. Under load that converts to work admitted, paid for, and thrown away at the deadline.

**Proposal.** `service/isolation/cost_admission.go` implementing `TenantBudgetManager` over a weighted bucket:

- charge `PromptTokens + MaxNewTokens` up front, refund the unused remainder on `Commit`;
- per-tier rate/burst selected from `domain.TenantRuntimePolicy` (tiers already exist as `PriorityClass` / `CapacityClass`);
- priority queue with **aging** — bounded priority plus elapsed-wait so a low-priority tenant cannot starve;
- **fail fast**: if the computed admission delay exceeds `ctx` deadline, return `DeadlineExceeded` immediately instead of queueing.

**From.** `q22`. Note its own admission about the API shape: an error-only `Admit` cannot return a reservation handle, so the caller can never refund. Scout's `Reserve` already returns `domain.BudgetReservation` — Scout got that right and should keep it.

**Owner.** Scout. This is cost-in-minor-units + token estimation; it is meaningless outside an LLM platform.

### A4. Distributed limiter with local fallback, over keel's existing cache

**Gap.** Scout's recommended topology runs several `conversation-api` replicas. A purely local limiter enforces *R × limit*. There is no shared-counter contract in Scout at all.

**Proposal.** `service/isolation/distributed_rate_limiter.go`:

- local per-replica bucket as fast path *and* as the authority during a store outage;
- one coalesced batch per hot key (single in-flight increment, callers admitted by prefix of the returned count) — this is what stops a hot key from becoming a store stampede;
- bounded store timeout, circuit with a **generation counter** so a stale in-flight success cannot clear a newer failure, single recovery probe;
- documented overshoot: `max(0, R×localLimit − limit)`.

**keel note (additive).** `cache.CacheService.IncrementWithTTL(key, ttl)` is already `q24`'s `Store` interface — same fixed-window atomic-increment semantics — except it increments by 1. Coalescing needs `IncrementByWithTTL(key string, n int64, ttl time.Duration)`. That is a one-method addition to `cache.CacheService` plus memory/redis impls; no existing caller changes.

**Owner.** keel for the counter method; Scout for the limiter policy.

### A5. Fair weighted concurrency limiter for scarce capacity

**Gap.** `ConcurrencyLimiter.Acquire` returns a lease with no weight and no fairness contract. `CapacityScheduler.Acquire` is the same shape for model capacity. Nothing prevents one tenant from holding every slot, and a large-weight request can starve behind a stream of small ones forever.

**Proposal.** `service/isolation/fair_slot_limiter.go`, satisfying both interfaces:

- per-tenant FIFO, tenants in round-robin rotation;
- weighted atomic reservation — a blocked head *reserves* freed capacity instead of letting smaller requests bypass it, which is what bounds large-request wait;
- private wake channel per waiter (no thundering herd);
- `Release` idempotent via `sync.Once` so a panicking step cannot leak a slot.

**Contract change.** `ConcurrencyLease.Release() error` should take a context to match `CapacityLease.Release(ctx, usage)`, and `Acquire` needs a weight — otherwise the interface cannot express "this turn needs four units". Per the global rules this is a clean break, not an overload.

**From.** `q25`, including its per-tenant queue-wait instrumentation, which is the input `q64` wants for autoscaling.

**Owner.** Scout — weights are token/GPU units and the fairness key is `domain.TenantContext`.

### A6. Adaptive provider concurrency (AIMD / latency-gradient)

**Gap.** `CapacityScheduler` is static. Real provider capacity is discovered, not configured: a fixed limit is either too low (wasted throughput) or too high (429 storms and queueing inside the provider, where you cannot see it).

**Proposal.** `service/modelgateway/adaptive_capacity.go` — additive increase on success, multiplicative decrease on error *or* on latency above a multiple of observed minimum RTT, sampled over a full concurrency-sized window to damp oscillation, clamped to `[min, max]`. Expose `Limit()` and rejection rate; `q64`'s autoscaling design consumes exactly those.

**From.** `q14`. Its two structural points matter more than the math: the lock covers *accounting only*, never `fn` execution; and the window resets on every limit change so samples measured under the old limit cannot drive the next decision.

**Owner.** Scout. Latency signal is per model/provider; the notion of "overload" here is inference-specific (prefill vs decode).

### A7. Windowed cost accounting and a half-open `CostCircuitBreaker`

**Gap.** `CostCircuitBreaker.Record` is documented as adding usage to "tenant, agent, and fleet cost windows" — no implementation exists and the contract never says how long a window is or how it closes.

**Proposal.** `service/isolation/cost_circuit_breaker.go` with a sliding time window per scope (the expiry mechanics in `q53` — timestamped entries, expire-on-read, caller-supplied `now`), plus the three-state breaker with a generation-guarded single probe from `q24`. `Allow` returns `RateLimitedError`-style retry advice (A2) rather than a bare `ErrCircuitOpen`.

**Owner.** Scout. Money in minor units with an explicit currency is invariant 10.

---

## B. Model gateway and streaming — from `q15`, `q31`–`q35`

### B1. Hedged generation across replicas

**Gap.** `ModelRouter.Select` returns exactly one selection; `Gateway.Stream` calls exactly one provider. Tail latency is therefore whatever the slowest replica does, and the README's 600–900 ms TTFT budget has no defense.

**Proposal.** `service/modelgateway/hedged_gateway.go` decorating `ModelGateway`: start one attempt, hedge after a delay *without a first token*, commit permanently to the first replica that produces a token, cancel the losers, never interleave two replicas into one stream.

**From.** `q34`. Its third follow-up is the one that makes this a Scout concern rather than a generic RPC concern: **every started attempt is billable even when canceled**. So the hedge must be admitted through the same `TenantBudgetManager` as the primary, hedges need their own budget cap, and hedging must be disabled fleet-wide when saturated. A hedger that improves p99 while doubling token spend is a regression under invariant 10, and only Scout knows that.

**Owner.** Scout.

### B2. Reply fan-out hub with replay cursor

**Gap.** The README promises reconnect ("the reply broker may retain frames briefly for reconnect") and the contract cannot express it: `TurnReplySubscriber.Subscribe(ctx, tenantID, requestID)` has no cursor, and a subscription is implicitly 1:1. Two browser tabs on one generation, or a reconnect after a dropped socket, are both unrepresentable. `domain.TurnReply` already carries `Sequence` and `Final`, so the data model is ready and only the port is missing.

**Proposal.**

- `Subscribe(ctx, tenantID, requestID string, fromSequence int64)`, with an explicit resync-required error when the cursor is older than the retained ring.
- `service/dataplane/reply_hub.go`: bounded per-subscriber queue, **disconnect on first overflow** rather than silent drop — a gap in a token stream is worse than a clean failure — plus a bounded replay ring, and snapshot-then-register under one lock so live frames cannot slip between replay and registration.

**From.** `q35`, whose "what is your lag policy" answer is the design decision Scout should make once, centrally, instead of leaving each downstream to invent it.

**Owner.** Scout. keel's `port.WebSocketHub` broadcasts to a *user*; this fans out one *generation* with per-subscriber backpressure and ordering. Not a duplicate.

### B3. One shared, tested stream pump

**Gap.** Every downstream will write the same loop: read `ModelStream`, apply `GuardrailEnforcer.AfterModelChunk`, publish a `TurnReply`, honor backpressure, propagate cancellation upstream, and stop at max output tokens. It is short, it is subtle, and it is where goroutine leaks live.

**Proposal.** `service/dataplane/stream_pump.go` — a single synchronous forwarder (no extra goroutine to leak), nil-out-closed-channel select so token and error channels may close in either order, explicit error precedence (published model error beats a concurrent context cancel), a send failure cancelling generation, and `MaxOutputTokens` enforced by *counting successfully published frames* and cancelling — never by reading token max+1.

**From.** `q31` and `q32`. `q31`'s error-precedence block and `q32`'s send-failure-cancels-generation are exactly the two behaviors invariant 8 depends on.

**Owner.** Scout.

### B4. Turn cancellation is entirely absent

**Gap.** `grep -rn "Cancel" contract/ domain/` returns nothing. There is no way for a client to stop a running turn, and no stated policy for a new prompt arriving mid-generation. For a chat product this is a launch blocker, not a refinement.

**Proposal.** Add `TurnCanceller` to `contract/data_plane.go` (`Cancel(ctx, tenantID, requestID, reason)`), and make the mid-turn policy explicit as a `domain.TurnAdmissionPolicy` enum — *queue* or *cancel-and-replace* — resolved from tenant policy rather than assumed. Per-turn cancellation must be a child of the session context so cancelling a turn never tears down the session.

**From.** `q33`, which also supplies the drain detail worth keeping: after cancelling a turn, drain the provider channels in the background, or an unbuffered in-flight result strands its goroutine.

**Owner.** Scout.

### B5. Micro-batching for embeddings

**Gap.** `EmbeddingGateway.Embed(ctx, tenant, content)` embeds one document at a time; `KnowledgeIngestor.Ingest` ingests one document at a time. Embedding providers are an order of magnitude cheaper per item when batched, and ingesting a corpus one HTTP round trip at a time is the single largest avoidable cost in the knowledge path.

**Proposal.** `service/knowledge/batching_embedder.go` — a decorator, not a new port: flush at `maxBatch` items or `maxWait` since the *first* queued item, one timer per open batch, per-caller cancellation that withdraws from the pending batch without corrupting it, and a hard failure when the provider returns a response count that does not match the request count.

**From.** `q15`. That last rule is the safety-critical one: a count mismatch must fail the whole batch, never deliver response *i* to caller *j*. In a multi-tenant platform that is a cross-tenant data leak, not a bug.

**Owner.** Scout.

---

## C. Caching — from `q41`–`q45`

### C1. An in-process `HotSessionCache` so single-node deployments work out of the box

**Gap.** `SessionCoordinator` and `DefinitionResolver` both return an error when their cache is nil. A downstream cannot run *anything* until it has written a cache adapter, even though invariant 6 explicitly says cache loss affects latency, not correctness — which is precisely the license to ship a simple in-memory default.

**Proposal.** `service/dataplane/memory_session_cache.go`: sharded map + per-shard LRU with TTL, bounded round-robin background sweeper (fixed scan budget per tick, cursor per shard, so no stop-the-world pause), `Close()` that stops the sweeper deterministically.

**From.** `q41` and `q43`.

**keel note.** keel's `cache.MemoryCacheService` is string-valued, unbounded, and has no LRU — fine for its purposes, wrong for hot session snapshots. Adding bounded LRU there is a reasonable keel-side improvement; Scout's typed session cache is a separate, agent-domain concern.

**Owner.** Scout.

### C2. Singleflight on every cache-miss path

**Gap.** `DefinitionResolver.Resolve`, `SessionCoordinator.Load`, `PublishedAgentResolver`, and `ReadinessResolver` all follow read-cache → miss → read-database → populate, with no coalescing. The moment that matters is exactly the moment it will happen: immediately after a publish or a rollout, when every worker in the fleet misses on the same new version simultaneously and stampedes the database.

**Proposal.** A small `internal/singleflight`, applied to those four paths. Two details from `q42` that are easy to get wrong: run the shared load under a context **detached from the first waiter's cancellation** (otherwise the caller who happens to arrive first can cancel everybody's load), and publish the result *before* closing the done channel so waiters get a happens-before edge.

**Owner.** Scout. The mechanism is generic enough to belong in keel; the four call sites are Scout's.

### C3. Two-tier cache discipline, stated once

**Gap.** `HotSessionCache` is a single tier. A realistic deployment has an in-process tier in front of Redis, and the correctness rules — write-through ordering, invalidation-versus-in-flight-read races, local TTL never outliving remote TTL — are exactly what downstreams will get wrong silently.

**Proposal.** Either a `service/dataplane/tiered_session_cache.go` implementing `HotSessionCache` over a local cache plus keel's `cache.CacheService`, or (cheaper) a documented rule set in the README. The one non-obvious mechanic worth encoding either way: a successful write must mark any *overlapping in-flight read* as invalidated, or a slow read can promote pre-write data over the value just written.

**From.** `q44`.

**Owner.** Scout.

### C4. Semantic response cache — Scout is unusually well positioned for this

**Gap.** None exists. But the reason to build it here rather than downstream is structural: semantic caching is dangerous because a cache hit can cross an intent, authorization, model-version, or policy boundary. Scout already pins *every one of those* — conversations pin agent, guardrail, tool, and knowledge versions (invariant 5), and every operation is tenant-scoped (invariant 1). Scout can construct a safe cache namespace that a downstream would have to reassemble by hand.

**Proposal.** `contract.ResponseCache` + `service/modelgateway/semantic_cache.go`:

- namespace = tenant + agent version + guardrail policy version + model + language, so a hit can never cross a pinned boundary;
- pluggable `SimilarityIndex` (exact brute force now, ANN later) with the tenant filter applied **inside** the index query, never as a post-filter over nearest neighbours;
- a precision floor the caller cannot lower, and explicit refusal to store non-deterministic or time-sensitive responses;
- hit *quality* metrics, not just hit rate.

**From.** `q45`, plus `q62`'s cache-key rule (entitlement fingerprint in the key).

**Owner.** Scout. Risk is real — this is the one proposal here that can produce a confidently wrong answer — so it should ship behind a flag, default off, with shadow-mode measurement before it can affect served responses.

---

## D. Retrieval and ranking — from `q51`–`q55`, `q62`

### D1. `KnowledgeQuery` has no user, no filters, and no budget

**Gap.** `domain.KnowledgeQuery` carries `TenantContext`, ids, `Query`, `TopK`. There is no acting user, no entitlement set, no metadata filter, and no latency budget. Tenant-level isolation is necessary and not sufficient: enterprise RAG fails on *row and column level* authorization within a tenant, and there is currently no place to put it.

**Proposal.** Extend `KnowledgeQuery` with the acting principal, a compiled entitlement predicate (opaque to Scout, applied by the index), an entitlement-version fingerprint for cache namespacing, and a retrieval deadline. Document the rule that makes it work: entitlement predicates go *into* the index query. Post-filtering nearest neighbours leaks existence and can select a forbidden document before the application ever sees it.

**From.** `q62`.

**Owner.** Scout.

### D2. A reranker seam and hybrid fusion

**Gap.** `KnowledgeRetriever.Retrieve` returns ranked matches in one shot. There is no seam for retrieve-then-rerank, and no way to express a keyword leg alongside the vector leg — so a downstream that needs either has to replace the whole port.

**Proposal.** Add `contract.KnowledgeReranker` and a `service/knowledge/hybrid_retriever.go` that runs vector and keyword legs concurrently under one deadline, fuses with reciprocal-rank fusion, reranks the overfetched authorized candidate set, and **skips the optional rerank stage when its remaining budget is insufficient** rather than blowing the deadline.

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

**Gap.** `RuntimeMetrics.RecordTurn/RecordStep/RecordDependency` all take a raw `tenantID`. A downstream that labels metrics by tenant gets unbounded cardinality and will eventually take down its metrics backend. Nearly every follow-up answer in the study set independently arrives at the same rule: bound tenant labels, keep a top-N, aggregate the rest into "other".

**Proposal.** `service/observability/tenant_cardinality.go` — a Count-Min sketch plus a bounded candidate heap that answers "is this tenant currently a top-N talker", so metric adapters can label the heavy hitters and bucket the tail as `other` while exact per-tenant accounting stays in the ledger where it belongs. Sublinear memory, mergeable across replicas.

**From.** `q54`. This is the least obvious idea in the document and the cheapest to ship.

**Owner.** Scout (the policy); the sketch itself would be equally at home in keel.

### E2. Stage-attributed errors across the turn lifecycle

**Gap.** `domain/errors.go` has fourteen sentinels describing *what* failed, and nothing describing *where*. When a turn fails, "was that retrieval, prompt construction, the model, the guardrail, or the reply publish?" is unanswerable from the error alone — and `q63`'s hardest follow-up ("separate model regressions from prompt / RAG / data regressions") is that question asked at fleet scale.

**Proposal.** One wrapper type in `domain/`:

```go
type StageError struct {
    Stage TurnStage // admission | retrieval | prompt | model | guardrail | tool | publish | checkpoint
    Err   error
}
```

with `Unwrap`, so every existing `errors.Is` keeps working. Then require the stage in `RuntimeMetrics` recordings. This makes failure attribution a property of the platform instead of a per-downstream logging convention.

**From.** `q32`'s `RAGError` and `q33`'s `ChatError`, generalized.

**Owner.** Scout.

### E3. Enforce the latency budget the README already publishes

**Gap.** The README ships a stage-by-stage p95 table and nothing reads it. Stage budgets that exist only in prose are aspirations.

**Proposal.** `domain.LatencyBudget` plus a budget allocator that derives per-stage deadlines from the caller's remaining time, and the discipline that goes with it — **reserve generation time first**, refuse to start a stage whose budget is already spent, and return a typed degraded result instead of starting the model call late.

**From.** `q32` ("if retrieval exceeds its budget, return a deadline-style error rather than starting generation late") and `q62`.

**Owner.** Scout.

### E4. Reservation reconciliation and orphan sweeping

**Gap.** `TenantBudgetManager` reserves and commits, but nothing describes what happens when a worker dies between the two. Reservations leak and the tenant is silently throttled by budget that was never spent.

**Proposal.** Give reservations a lease with an expiry, and specify a bounded sweeper that reconciles expired reservations — the same shape as `AgentRunStore.Purge`, which already establishes the batched-drain pattern in this codebase. Also state the policy question explicitly rather than leaving it implicit: when a client disconnects mid-stream, generated tokens were still produced and billed by the provider.

**From.** `q61`.

**Owner.** Scout.

### E5. `RolloutHealthEvaluator` cannot say "I don't know"

**Gap.** `Healthy(ctx, target) (bool, error)` is binary. But the correct behavior when telemetry is stale or samples are insufficient is to **pause** promotion — neither advance nor roll back. Today that state is unrepresentable, so an implementation must either lie (`true`, and promote on no evidence) or misuse `error`.

**Proposal.** Return a three-state verdict — `healthy` / `unhealthy` / `insufficient-evidence` — carrying sample count, window, and the breached metric. Add hysteresis inputs: consecutive breached windows and a minimum observation duration, so a single noisy window cannot trigger a rollback.

**From.** `q65`, which states the rule directly: loss of trustworthy metrics pauses promotion; it does not declare success or failure.

**Owner.** Scout. Small contract change, disproportionate correctness gain.

### E6. Version pinning has no precedence rule

**Gap.** `AgentVersionTrafficManager.ResolveVersion(tenantID, agentID, conversationID)` resolves per conversation — good, stable hashing is possible — but there is no pin concept. `q65` requires a resolution order (compliance pin → approved tenant pin → experiment cohort → global default) and pins with expiry, region, owner, and reason, for reproducibility and audit.

**Proposal.** Add a pin to the control plane with those attributes, define the precedence order in the contract doc comment, and state the invariant that a release cannot be garbage-collected while a pin references it. Also: a release resolved at session start must stay resolved for the whole session — rollback stops *new* assignments and drains existing ones rather than splicing a different model mid-stream.

**Owner.** Scout.

---

## F. Concurrency hygiene in existing code — from `q11`–`q13`

### F1. `toolgateway.RetryPolicy` has no jitter

**Gap.** `service/toolgateway/retry_policy.go` is deterministic exponential backoff. When a shared dependency blips, every worker in the fleet retries in lockstep at exactly `base`, `2×base`, `4×base` — a self-inflicted thundering herd on a dependency that is already struggling.

**Proposal.** Full jitter (`rand` in `[0, delay]`) with an injected randomness source so tests stay deterministic — the current design's testability is a feature and should not be lost to fix this. Consider also a fleet-level retry budget: a per-tool cap on the *fraction* of calls that may be retries, which is what actually prevents a retry storm; a per-call attempt cap does not.

**From.** `q12`.

**Owner.** Scout. Smallest diff in this document, and arguably the highest ratio of risk removed to lines changed.

### F2. `ContractTestRunner` is strictly sequential

**Gap.** `service/release/contract_test_runner.go` executes a risk-stratified corpus one case at a time, each a full governed agent turn. For a corpus of any realistic size this makes the release gate too slow to run per build, which means it will be skipped.

**Proposal.** Bounded ordered concurrency: at most *N* concurrent executions, results written by index so input order is preserved, first error cancels remaining work, no unbounded buffering. Concurrency comes from the caller — a release gate and a smoke test want different values.

**From.** `q11`, which is precisely this function.

**Owner.** Scout.

### F3. Knowledge ingestion wants a bounded pipeline

**Gap.** `KnowledgeIngestor.Ingest` is per-document and synchronous. Ingestion is inherently decode → embed → index, with wildly different per-stage costs and rate limits.

**Proposal.** `service/knowledge/ingest_pipeline.go` — per-stage worker counts, **bounded handoff channels between stages** so a slow index applies real backpressure to embedding and decode, a single item error reported on an error channel rather than tearing down the run, and ordered shutdown that closes each stage only after its writers exit. Pairs naturally with B5.

**From.** `q13` — whose worked example is literally "decode → embed → index".

**Owner.** Scout.

---

## G. Testability

### G1. Inject the clock

`isolation.ExecutionGovernor.Now` is the only injectable clock in the codebase; `controlplane.StudioService` and `runtime.BaseDraftTester` call `time.Now()` directly. Every time-dependent mechanism proposed above — buckets, breakers, TTLs, sweepers, windows, hedge delays, backoff — is untestable without a fake clock, and `sleep`-based tests are how flaky suites are born. `q22`, `q23`, `q41`, `q43`, and `q53` all inject a clock, and `q23` goes further by taking `now` as a *parameter* so the limiter never reads a clock at all.

Adopt one convention before writing the mechanisms, not after. The clock abstraction itself is horizontal and belongs in **keel**.

### G2. Prove the absence of leaks

New concurrent services need leak tests, not just behavior tests: goroutine-count assertions around construct/use/close, `-race` in CI, and cases for the four shapes `q11` names — the function blocks until cancellation, one worker fails early, input is empty, and the worker count is invalid. Extend `internal/fake` with a controllable clock and a blocking provider so these can be written without timing dependence.

---

## H. Priority

Ordered by (unblocks downstream work) × (cost of getting it wrong later) ÷ (effort).

| # | Idea | Effort | Why now |
|---|---|---|---|
| 1 | F1 — jitter in tool retry | hours | Live retry-storm risk; smallest diff in this document |
| 2 | A2 — `RateLimitedError` with `RetryAfter` | hours | Contract change; cheaper before downstreams depend on the bare sentinel |
| 3 | E5 — three-state rollout verdict | hours | Contract change; today the safe behavior is unrepresentable |
| 4 | C1 — in-process `HotSessionCache` | 1–2 days | Unblocks running Scout at all without a cache adapter |
| 5 | C2 — singleflight on resolver paths | 1 day | Stampede risk is highest at publish/rollout, exactly when it hurts most |
| 6 | A1 + A5 — tenant limiter + fair slot limiter | ~1 week | The two controls every downstream must otherwise write |
| 7 | E2 — stage-attributed errors | 1–2 days | Cheap now, invasive later; unlocks failure attribution |
| 8 | B4 — turn cancellation contract | 2–3 days | Missing entirely; a chat product cannot ship without it |
| 9 | A3 + E4 — cost admission and reconciliation | ~1 week | Correct token/cost metering is the platform's commercial core |
| 10 | B2 — reply hub with replay cursor | ~1 week | The README already promises reconnect |
| 11 | F2, F3, B5, D2 — concurrency and retrieval quality | ~1 week | Throughput and cost, no contract risk |
| 12 | A4, A6, A7, D1, E1, E3 | ~2 weeks | High value, each needs a design decision first |
| 13 | B1 — hedging | ~1 week | Only after budget accounting (A3) exists, or it doubles spend invisibly |
| 14 | C4 — semantic cache | ~2 weeks | Highest upside, highest risk; flag-gated, shadow-measured, default off |

## I. Deliberately not proposed

- **A generic KV cache, HTTP client, or queue.** keel owns these (`cache.CacheService`, `common/http_client.go`, `dispatcher`, `outbox`). C1 and C4 are typed, agent-domain caches layered on top, not replacements.
- **A generic clock, hash, or heap package in Scout.** Horizontal; keel or the standard library.
- **A provider adapter for a new vendor.** `provider/` already covers Anthropic, OpenAI, and Google; breadth is a downstream concern.
- **Replacing keel's `port.WebSocketHub`.** B2 is per-generation token fan-out with backpressure and replay; the keel hub is per-user broadcast. Different problems.
- **Anything from the study set with no Scout counterpart.** `q51` and `q53` in their original form are stream-processing exercises; only their *mechanics* are reused (E1, A7), not the APIs.

## J. What the study set does not cover

Worth stating so this document is not mistaken for a complete backlog. The study set contributes nothing to Agent Studio, prompt inheritance and compilation, the MCP adapter layer, the keel schema, or the `studio-v1` HTTP profile — which together are roughly half of Scout's current surface and the half that is genuinely differentiated. The overlap is concentrated in the data plane, which is precisely the half that is currently contracts without mechanisms.

---

*Reviewed: 40 files, `q11`–`q65`, `~/dev/jobprep/mlsol`, against Scout `3d7bca0`. 2026-08-12.*
