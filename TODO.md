# Scout — deferred work, and what closed it

Task lists for the mechanisms that were missing behind existing contracts, now implemented. Items are grouped by the production design they complete (`~/dev/jobprep/mlsol/q61`–`q65`), reference the idea IDs in [IDEAS.md](IDEAS.md), and follow the ownership rule there: generic primitives → keel, agent-platform policy → Scout, product behavior/provider choice → downstream. Severity: **HIGH** = blocks a production deployment, **MED** = hardening/completeness, **LOW** = optimization. Status: ✅ done, 🚧 in progress, ⏳ open, ⛔ won't do.

Last reconciled against the working tree on 2026-08-16, after the implementation pass that closed every item below. Each entry now records what shipped; the design references live under `doc/`.

---

## Status overview

| ID | Title | Design | Idea | Severity | Status |
|---|---|---|---|---|---|
| G1 | Policy-driven `ModelRouter` reference implementation | q61 | B6 | **HIGH** | ✅ done |
| G2 | Evolve `ModelSelection` with version/region/route identity | q61/q65 | B6 | **HIGH** | ✅ done |
| G3 | Durable turn vertical slice (dispatch → idempotency → checkpoint → guarded reply → ack) | q61 | B0 | **HIGH** | ✅ done |
| G4 | Schema/DTO identity alignment for persistence | q61 | B0a | **HIGH** | ✅ done |
| G5 | Distributed tenant × fleet limiter with local fallback | q61 | A4 | MED | ✅ done |
| G6 | Hedged generation across replicas | q61 | B1 | LOW | ✅ done |
| G7 | Request-aware latency budget allocator | q61/q62 | E3 | MED | ✅ done |
| G8 | Structured stage/version-attributed observations | q61/q63 | E2 | MED | ✅ done |
| G9 | Bounded-cardinality tenant metrics | q61 | E1 | MED | ✅ done |
| G10 | Gateway resilience: pre-token retry, split streaming deadlines, partial completion, signed routing snapshots, fail-closed policy expiry | q61 | B0/B6 | **HIGH** | ✅ done |
| G11 | Two-tier `HotSessionCache` discipline (write ordering, invalidation, revision checks, TTL bounds) | q41–q45 | C3 | MED | ✅ done |
| T1 | Default `ToolCircuitBreaker` (closed/open/half-open, probe, generation fencing) | q65 | A8 | **HIGH** | ✅ done |
| K1 | `KnowledgeVectorIndex` reference adapter (pgvector) | q62 | D0 | **HIGH** | ✅ done |
| K2 | Chunk-aware knowledge contract (`KnowledgeChunk`, chunker port) | q62 | D0 | **HIGH** | ✅ done |
| K3 | Bounded ingestion pipeline + ingestion Worker | q62 | F3 | **HIGH** | ✅ done |
| K4 | Document manifest versioning, tombstones, atomic alias swap | q62 | F3 | **HIGH** | ✅ done |
| K5 | Entitlement-scoped retrieval cache | q62 | C3/C4 | MED | ✅ done |
| K6 | Sharded merge with early stop | q62 | D3 | LOW | ✅ done |
| K7 | HANA vector adapter | q62 | — | LOW | ⏳ open — criteria recorded, driven by demand |
| K8 | Retrieval evaluation: golden queries with permissions, recall@K/MRR/nDCG, citation precision, abstention, ingestion and tombstone lag | q62 | D0/E7 | MED | ✅ done |
| E1 | `EvaluationManifest` + golden-set evidence model | q63 | E7 | **HIGH** | ✅ done |
| E2 | Pluggable evaluators: heuristic, LLM judge, human review | q63 | E7 | **HIGH** | ✅ done |
| E3 | Paired baseline/candidate scoring by slice with confidence | q63 | E7 | **HIGH** | ✅ done |
| E4 | Signed, expiring gate decision consumed by rollout | q63/q65 | E7/E5 | **HIGH** | ✅ done |
| E5 | Evaluation Worker + production sampling policy | q63 | E7 | MED | ✅ done |
| E6 | Judge–human agreement calibration | q63 | E7 | MED | ✅ done |
| E7 | Trajectory (agentic/streaming) evaluation | q63 | E7 | LOW | ✅ done |
| R1 | `PlatformReleaseRolloutController` reference implementation | q65 | E5 | **HIGH** | ✅ done |
| R2 | Version pin precedence (compliance → tenant → cohort → default) | q65 | E6 | **HIGH** | ✅ done |
| R3 | Decide release-bundle vs separate agent/platform rollout | q65 | E6 | **HIGH** | ✅ done — keep the split, add `release_bundle` |
| R4 | Session stickiness and drain-on-rollback semantics | q65 | E6 | MED | ✅ done |
| R5 | Stateful, layered `GuardrailEnforcer` (baseline + release-specific) | q65 | B7 | MED | ✅ done |
| R6 | Rollback drills and synthetic probes | q65 | — | LOW | ✅ done |
| S1 | Serving-signal export for external autoscaler | q64 | A5/A6 | MED | ✅ done |
| — | GPU placement / gang scheduling / autoscaler inside Scout | q64 | §I | — | ⛔ won't do |
| — | Continuous batching / KV cache management | q61 | §I | — | ⛔ won't do — serving layer |
| X1 | Inject clock (keel convention) | all | G1 | MED | ✅ done |
| X2 | Leak/lifecycle tests for every goroutine-owning service | all | G2 | MED | ✅ done |
| X3 | Validated configuration structs and documented flags for every limit | all | G3 | MED | ✅ done |

---

## G. Inference gateway (q61)

Shipped: `modelgateway.Gateway`, `AdaptiveCapacityScheduler`, `FairSlotLimiter`, `isolation.TenantRateLimiter`, `BudgetLedger` (reserve → settle actual → expire), `WindowedCostBreaker`, `dataplane.StreamPump`, `MemoryTurnCanceller`, `MemoryReplyHub`, providers for Anthropic/OpenAI/Google.

### G1 — Policy-driven `ModelRouter` (HIGH)
- [x] `service/modelgateway/router.go`: `PolicyRouter` implementing `contract.ModelRouter` over injected immutable inputs.
- [x] Input ports: `ModelCandidateCatalog` (capability/version/tenant access/region/context limits — backed by `model_definition`, `model_capability`, `tenant_model_access`), `CapacitySnapshotSource` (health, predicted queue delay, freshness timestamp).
- [x] Ranking: filter incompatible → rank by deadline feasibility, quality class, capacity locality, estimated minor-unit cost via `ModelPricer`.
- [x] Return an auditable routing reason and catalog/snapshot generation in the selection; record via `AuditSink`.
- [x] Explicit degradation/fallback from tenant policy (`tenant_runtime_policy`); never implicit provider substitution.
- [x] Session/prefix affinity as a scored preference only while predicted latency stays within budget.
- [x] Table-driven tests: residency exclusion, stale snapshot rejection, deadline-infeasible candidate skipped, cost tiebreak.

### G2 — Evolve `ModelSelection` (HIGH, before G1/G6/R* consumers multiply)
- [x] Add `ModelVersion`, `Region`, `RouteID`/replica identity, `RoutingGeneration`, `Reason`.
- [x] Propagate into `usage_event`, `RuntimeMetrics.RecordDependency`, and audit events.
- [x] Clean break: update `provider/*`, `Gateway`, `MediaGateway`, fakes; document in `migration_guide.json`.

### G3 — Durable turn vertical slice (HIGH)
- [x] `DurableSessionStore` over `conversation_turn`, `step_checkpoint`, `session_snapshot`; large state via injected keel object storage, only URI+digest committed.
- [x] `StepIdempotencyStore` over `step_idempotency` with named SQL and transactional state transitions; define `Begin`/`Abandon` replay transition (current `abandoned` is terminal).
- [x] `TurnDispatcher` + `FairTurnScheduler` reference adapters over keel dispatcher/worker lifecycle (no Scout goroutines from HTTP).
- [x] `DeadLetterQueue` reference adapter; terminal retry exhaustion routes there.
- [x] `ConversationIngress` reference implementation: authenticate → admit → reserve budget → subscribe reply route → durable enqueue; returns the subscription.
- [x] Order of durability: idempotency result → checkpoint → guarded *intermediate* frames (sequence-deduplicated by the reply hub, so replay republishes the same sequences) → terminal result + settlement persisted → final frame → ack. The final frame is never client-visible before the terminal record exists; a crash between the two replays the frame from the durable result, never re-executes.
- [x] Budget settlement or refund on every terminal path (success, failure, cancel, dead-letter); one immutable `usage_event` per settled turn; audit record for every terminal transition (reuse `TurnLedger` semantics or compose it).
- [x] Client-disconnect billing policy: disconnect cancels delivery, provider work already produced is metered from provider-confirmed usage, never from frames delivered.
- [x] Durable publication boundary: reply publication is at-least-once with sequence deduplication; where a product needs cross-process durability, the final frame is written through keel outbox in the terminal transaction.
- [x] `ConversationRuntime` composition + conformance suite runnable by Scout references and vendor adapters.
- [x] Failure tests: worker crash mid-step replays stored result, not the side effect; duplicate delivery is a no-op; crash after final persistence but before final frame replays the frame only.

### G4 — Schema/DTO identity alignment (HIGH, precedes G3)
- [x] `ExecutionStep.StepID` (string) vs `step_idempotency.execution_step_id` / `step_checkpoint.execution_step_id` (BIGINT): carry compiled numeric ID in the domain value or resolve tenant+agent+version+logical ID before write.
- [x] `StepCheckpoint` vs `step_checkpoint`: add `state_uri`, digest, fingerprint, currency; drop or derive `CheckpointID`/`RequestID`.
- [x] `SessionSnapshot` inline state vs `session_snapshot` URI/digest: define hydrate/dehydrate; verify digest before returning state.
- [x] `StepIdempotencyStore.Begin` returning `StepResult` requires injected object store/codec + claim lease timeout.
- [x] One persistence design doc covering transaction boundaries and orphan object cleanup.

### G5 — Distributed limiter with local fallback (MED)
- [x] Atomic tenant+fleet admission over keel `cache.CacheService` (Lua/CAS or keel primitive extended upstream — extend keel, do not reimplement).
- [x] Local token bucket fallback with reduced quota when the shared store is unreachable; surface degraded mode in metrics.
- [x] Multi-replica test: N gateway replicas do not multiply tenant quota.

### G6 — Hedged generation (LOW, after G1/G2 and BudgetLedger attempts)
- [x] `HedgingGateway` wrapping `ModelGateway`: delayed second attempt on a different route, cancel loser. Every started attempt may be billed, including the cancelled loser: one logical request holds independently fenced per-attempt reservations (unique attempt id, `BudgetLedger` attempt semantics) each settled against provider-confirmed usage; never a single reservation sized for one attempt.
- [x] Hedge only idempotent, non-side-effecting requests; per-tenant hedge budget; kill switch flag.

### G7 — Latency budget allocator (MED)
- [x] `domain.TurnBudget`: reserve generation first, allocate embedding/retrieval/rerank/prompt-build/guardrail slices from the request deadline.
- [x] Retrieval and reranker stop at their slice and return typed degraded results (`KnowledgeQuery.Budget` already exists — wire it from the allocator).
- [x] Reject at admission when remaining deadline cannot cover the minimum path.

### G8 — Structured observations (MED)
- [x] `domain.Observation` DTO: stage, versions (model/prompt/knowledge/tool/guardrail/evaluator), timing, usage, outcome, error class.
- [x] Emit from `internal/stage` wrappers already present in streaming and retrieval; add to `RuntimeMetrics`.
- [x] TTFT, TPOT, queue wait, admission rejection, reservation-vs-actual delta as first-class fields.

### G9 — Bounded-cardinality metrics (MED)
- [x] Label allowlist (tenant tier, model, region, stage, release, verdict); never tenant ID, prompt, or free text.
- [x] Per-tenant series only via sketch/top-K (`internal/` heavy-hitters) with explicit cap.

### G10 — Gateway resilience (HIGH)
- [x] Retry/hedge only before the first streamed token; after first token an interrupted stream ends with an explicit partial/interrupted completion (`FinishReason`), never a silent restart or spliced output.
- [x] Separate streaming deadlines: TTFT, idle-token gap, total; each a typed error class attributed to `StageModel`.
- [x] Short-lived signed routing/config snapshots (`ModelCandidateSet`, `RoutingPolicy`, tenant quota policy) so the gateway keeps serving through a control-plane outage until expiry; expired authorization/quota policy fails closed.
- [x] Tests: retry suppressed after first token; idle deadline fires; snapshot expiry rejects.

### G11 — Two-tier session cache discipline (MED)
- [x] Authoritative durable write completes before cache write; overlapping reads invalidate rather than repopulate a superseded revision; cache entries carry `Revision` and a stale revision is never returned over a newer durable one.
- [x] Local TTL never exceeds the remote/shared TTL; explicit invalidation on `Complete`/`Checkpoint`; singleflight on miss (already wired).
- [x] Reference `SessionCoordinator` tests for read-after-write, concurrent checkpoint + read, and cache-loss recovery.

---

## T. Tool gateway

### T1 — Default `ToolCircuitBreaker` (HIGH)
- [x] `service/toolgateway/circuit_breaker.go`: closed → open → half-open with one recovery probe, generation fencing so a stale probe cannot close a newer generation, failure classification (timeouts and 5xx count; validation and authorization rejections do not), bounded tenant×tool cardinality with eviction, optional shared dependency-health view across tools of one destination.
- [x] `GovernedGateway` composes it by default; `Now`-injected; table-driven state-machine tests.

---

## K. Enterprise RAG (q62)

Shipped: `KnowledgeQuery` with principal/entitlements/digest/budget, `knowledge.HybridRetriever` (RRF + budget-gated reranker), `knowledge.BatchingEmbedder`.

### K1 — pgvector `KnowledgeVectorIndex` adapter (HIGH)
- [x] `service/knowledge/pgvector_index.go` over keel query service; named SQL only.
- [x] Entitlement predicates compiled into the WHERE clause (tenant partition + document/row entitlements) — never post-filter; fail closed when entitlements are absent or stale.
- [x] Vector + BM25 (`tsvector`) retrieval in one adapter or two ports feeding `HybridRetriever`; overfetch factor configurable.
- [x] Return document ID, chunk no, source URI, offsets, source version, score; never raw embeddings.
- [x] Authorization-leak test: two principals differing by one entitlement.

### K2 — Chunk-aware knowledge contract (HIGH)
- [x] `domain.KnowledgeChunk` (deterministic ID from tenant+doc+source version+chunker version+position, offsets, redaction policy version) aligned with `knowledge_chunk` table.
- [x] `KnowledgeVectorIndex.Index` takes chunks, not whole documents (clean break, migration note).
- [x] Ports: `SourceLoader`, `MediaDecoder`, `Chunker` (structure-preserving; SAP document sections/tables as first target).
- [x] Column-level policy → redacted derivative chunk before embedding.

### K3 — Bounded ingestion pipeline + Worker (HIGH)
- [x] `service/knowledge/ingest_pipeline.go`: load+verify digest → decode/chunk → embed (via `BatchingEmbedder`) → index+persist, bounded handoff channels, per-stage worker counts, correlated per-document results.
- [x] Bulk contract (`IngestBatch`) alongside per-document `Ingest`; systemic error cancels batch, isolated bad document records terminal item result.
- [x] Idempotency key tenant+knowledge version+document+content digest.
- [x] `cmd`-less: Scout ships the pipeline; downstream wires the keel Worker binary. Provide a reference Worker composition in `internal/fake` or docs.
- [x] Chunk content in object storage; relational chunk metadata + vector refs published atomically only after successful index; reconciliation for partial writes.

### K4 — Manifest versioning, tombstones, alias swap (HIGH)
- [x] Document manifest per (tenant, KB, document): build new version fully → switch active pointer → async GC old chunks.
- [x] Delete writes an authorization-visible tombstone checked by retrieval before bulk index cleanup.
- [x] Re-embed/rechunk as side-by-side `knowledge_base_version` generation with checkpoint + validation + atomic alias swap.
- [x] CDC/outbox port for source change events carrying tenant, object ID, source version, op, authorization attributes; periodic reconciliation reports freshness lag and orphan chunks.

### K5 — Entitlement-scoped retrieval cache (MED)
- [x] Cache key = tenant + entitlements digest + query digest + embedding model version + index generation + policy version.
- [x] Invalidate on entitlement or index generation change; singleflight on miss (reuse `internal/singleflight`).
- [x] Semantic response cache stays a research item (IDEAS.md C4) until a leak-safety proof exists.
- [x] Scope: retrieval only. The general two-tier session cache discipline (C3) is G11.

### K6 — Sharded merge with early stop (LOW)
- [x] k-way merge over sharded sorted result streams with score-bound early termination; only after K1 measured need.

### K7 — HANA vector adapter (LOW, demand-driven)
- [ ] Same contract as K1. Build only when one of these holds: a tenant corpus outgrows a single pgvector tier; sustained ingest competes with search on the same instance; entitlement-filtered recall stays below target after `iterative_scan` and `ef_search` tuning; the source data already lives in HANA under a replication or residency requirement; or a separate PostgreSQL vector tier costs more than the HANA vector engine.

### K8 — Retrieval evaluation (MED, connects to E1–E3)
- [x] Versioned golden queries with expected documents *and* the principal/entitlements they must be visible to; retrieval evaluated separately from generation.
- [x] Metrics: recall@K, MRR, nDCG, filter selectivity, citation precision, abstention quality, ingestion lag, tombstone lag; results are `EvaluationResult`s under an `EvaluationManifest` slice.
- [x] Authorization-leak check as an evaluation metric: any match outside the golden principal's entitlements is a critical failure.

---

## E. Quality evaluation pipeline (q63)

Shipped: `release.ContractTestRunner` (bounded ordered concurrency) over `contract_test_case/_run/_result` — deterministic compatibility only.

### E1 — Evidence model (HIGH)
- [x] `domain.EvaluationManifest`: candidate + baseline agent/model/prompt/knowledge/index/tool/guardrail versions, dataset revision, decoding settings, evaluator versions, safety policy — immutable, content-addressed.
- [x] `domain.GoldenExample` metadata: provenance, consent/retention class, risk tier, domain/language slice, rubric ref, expected behavior, payload URI+digest, review history.
- [x] Schema: `evaluation_manifest`, `golden_set`, `golden_set_version`, `golden_example`, `evaluation_run`, `evaluation_result` (keel conventions: `BIGINT` + explicit sequence, real FKs, minor-unit cost + currency exponent).
- [x] Gate set hidden from prompt authors: separate storage/authorization scope from dev set.

### E2 — Pluggable evaluators (HIGH)
- [x] Ports: `HeuristicEvaluator`, `JudgeEvaluator` (versioned prompt+model, blinded, randomized order), `HumanReviewQueue`.
- [x] Ordering: heuristics → judge → route disagreements/low-confidence/high-risk to human queue.
- [x] Judge sees rubric + evidence, never candidate label; deterministic judge inputs cached.
- [x] Reference impls: regex/schema/citation-support heuristics; judge over `ModelGateway`; memory review queue for tests.

### E3 — Paired scoring by slice (HIGH)
- [x] Deterministic replay of baseline and candidate on identical examples and preserved retrieval outputs.
- [x] Per-slice paired deltas, sample counts, bootstrap CIs; promotion requires min sample size, no critical safety failure, thresholds on aggregate and protected/high-risk slices.
- [x] Factorial ablation matrix (change one component) for regression attribution — reuse G8 observation versions.
- [x] Metrics: correctness, retrieval recall, citation support, groundedness, safety, instruction following, latency, tokens, minor-unit cost; agent metrics per E7.

### E4 — Gate decision (HIGH)
- [x] `domain.GateDecision`: manifest ID, data/judge versions, metric deltas, confidence, exemptions/approvals, expiry, telemetry freshness; signed via keel crypto.
- [x] `DetailedRolloutHealthEvaluator` reference impl consumes the latest unexpired decision + online metrics; missing/expired → `RolloutInconclusive`.
- [x] Persist to `platform_release` / `agent_version` linkage; audit every decision.

### E5 — Evaluation Worker + sampling (MED)
- [x] Separate keel Worker; never on the serving path; batched low-priority model calls; monthly/per-change budget with cost-per-detected-regression metric.
- [x] Production sampling policy: risk/uncertainty-aware, hard per-tenant caps, tenant opt-out, redaction/tokenization before eval store, residency/retention enforced, no content in metric labels.
- [x] Join samples to explicit feedback, correction/retry, completion, escalation, KPIs; holdout traffic on stable version.
- [x] Drift detectors on score distributions; alert on sustained practical effect size; sequential early stopping when the effect is decided.
- [x] Sample storage encrypted and access-controlled (keel secret/crypto + object storage); production failures deduplicated and adjudicated before they enter the golden set.

### E6 — Judge calibration (MED)
- [x] Stratified human-labeled calibration set; Cohen's κ / Krippendorff's α + precision/recall on critical failures per judge version.
- [x] Position/self-preference bias tests; recalibrate on judge change.

### E7 — Trajectory evaluation (LOW)
- [x] Sequenced trace of tokens/tool calls/observations/state/timing; score tool choice/arguments, policy compliance, trajectory efficiency, recoverability, final state; TTFT/interruptibility/partial safety for streams.
- [x] Deterministic tool sandboxes for replay.

---

## R. Safe rollout and guardrails (q65)

Shipped: three-state `RolloutHealth` (inconclusive pauses), `runtime.PublishedAgentResolver` (stable/canary hashing), rollout schema (`platform_release`, `platform_rollout`, `tenant_ring`, `tenant_ring_member`, `agent_alias`, `agent_deployment`).

### R1 — Rollout controller (HIGH)
- [x] `service/release/rollout_controller.go` implementing `PlatformReleaseRolloutController` over `platform_release`/`platform_rollout`/`tenant_ring`.
- [x] State machine: build → offline replay → shadow → internal canary → tenant canary → regional ramp → global default → retired; rollback/quarantine from any live stage.
- [x] Each stage: min samples + min duration; advance only on `RolloutHealthy` from E4; `RolloutInconclusive` pauses; operator pause/resume; scoped, approved, audited bypasses.
- [x] Rollback is an idempotent alias change: stop new assignments, preserve evidence, keep active sessions on resolved release, restore previous capacity, quarantine.
- [x] Controller safety: keel lease, monotonic rollout generation, audited transitions; hard-guardrail immediate rollback on confirmed isolation breach/severe safety/corrupt output/availability failure; soft guardrails require consecutive breached windows + hysteresis + cooldown.
- [x] Shadow stage: sampled copy after auth/redaction, no user-visible output or side effects, resource-amplification check.

### R2 — Agent-version pin precedence (HIGH; agent identity only, see R3)
- [x] `domain.VersionPin` {scope: compliance|tenant, effective/expiry dates, region, reason, owner, approval and signature metadata, compatible policy/index versions}; table + `AgentVersionTrafficManager` reference impl.
- [x] Resolution: compliance pin → approved tenant pin → experiment cohort (stable hash of tenant/user/session + experiment ID) → global default.
- [x] Reject incompatible pinned request rather than silently drifting; propagate the resolved agent version into audit; pin-aware garbage collection never drops a pinned or live-conversation version.

### R3 — Release-bundle decision (HIGH, decision before R1; R2/R4 identities depend on it)
- [x] Decide: single immutable release bundle vs Scout's current split of tenant agent-version rollout and platform-release rings.
- [x] Recommendation to evaluate: keep the split, but add a `release_bundle` that pins the *set* of versions a platform release certifies, so rollback semantics stay unambiguous. Record decision in `IDEAS.md`/`migration_guide.json`.
- [x] Acceptance (q65): the bundle names model + provider version + tokenizer, runtime, decoding defaults, prompt, embedding/reranker + index generation, tools, safety policy, schema-migration set, signing/provenance, compatibility constraints, rollback target, residency policy; the resolved release identity propagates into every request, usage, and audit record.

### R4 — Session stickiness and drain (MED; conditional on R3)
- [x] Two distinct persisted identities per conversation: the agent version (`agent_conversation.agent_version`, exists) and the resolved platform release/bundle (new column or `agent_conversation_release` row); both resolved at session creation and read, never re-resolved, on later turns.
- [x] Rollback blocks new sessions; existing drain within bounded window; critical safety → cancel with explicit partial status, retain state, next turn on safe release.
- [x] Never splice tokens across models mid-stream.

### R5 — Layered `GuardrailEnforcer` (MED)
- [x] Release-independent edge baseline (PII, malware, toxicity, prompt-injection, jailbreak, tool policy) + release-specific stricter policy; composable, stateful across a stream.
- [x] Retrieved content marked untrusted; irreversible tool actions require approval; safety events feed rollout controller and audit.

### R6 — Drills and probes (LOW)
- [x] Rollback drill harness: alias propagation, capacity restoration, session behavior, alert ownership.
- [x] Synthetic probes and holdout traffic per release.

---

## S. Serving-fleet signals (q64)

Scout is not a GPU scheduler (IDEAS.md §I). It supplies signals to and consumes outcomes from the serving control plane.

### S1 — Signal export (MED)
- [x] Export queued token work (predicted prefill tokens, decode token-seconds), per-tenant queue wait (from `FairSlotLimiter`), admission rejections, TTFT/TPOT, KV pressure if reported by provider, capacity outcomes per `ModelSelection` route.
- [x] Consume capacity/drain snapshots via G1's `CapacitySnapshotSource`; the snapshot carries drain state + drain deadline, warm/loading state, active ownership, live service rate, and prefill/decode capacity, not only health and queue delay.
- [x] Draining route → no new admissions; existing streams finish only until the grace deadline, after which they are cancelled with an explicit partial/resumable completion.
- [x] Document the contract for external autoscalers (K8s/KServe/vLLM/Ray) in `doc/`.

---

## X. Cross-cutting — prerequisites, not cleanup

These are acceptance criteria for every item above, adopted before the mechanisms are written (IDEAS.md G1–G3), and applied retroactively to existing services.

- **X1** — Inject clock everywhere per the `Now func() time.Time` convention; no `time.Now()`, `time.Sleep`, or unbounded timers in policy code; waits are ctx-bounded or injectable.
- **X2** — Leak/lifecycle tests for every goroutine-owning service (`BatchingEmbedder`, `StreamPump`, `ReplyHub`, pipeline stages, hedging, breakers): blocks-until-cancel, one worker fails early, empty input, invalid worker count; idempotent `Close`/`Shutdown`.
- **X3** — Every limit (rates, bursts, tenants, queue lengths, cache bytes, batch sizes/waits, windows, replay frames, sweep sizes, concurrency) is a validated constructor input rejecting unsafe zero/negative combinations, and each deployable value is a documented `flag`; no package globals or environment reads.

---

## What landed

The order followed IDEAS.md §H with X1–X3 as standing prerequisites: **G4 → G2 → G1 → G10 → T1 → G3** (executable governed turn), then **K2 → K1 → K3 → K4** (safe knowledge baseline), then **R3 → E1 → E2 → E3 → E4 → R1 → R2 → R4** (reproducible, pausable, sticky rollout with quality evidence), then the MED/LOW items (G5, G7–G9, G11, K5, K8, E5–E6, R5, S1) and the measured optimizations (G6, K6, E7). K7 remains demand-driven.

| Area | Shipped | Reference |
|---|---|---|
| Inference gateway | `modelgateway.PolicyRouter`, `TableCandidateCatalog`, `MemoryCapacitySnapshotSource`, `SnapshotCache`, `ResilientGateway`, `HedgingGateway`, `ServingSignalCollector` | [doc/serving_signals.md](doc/serving_signals.md) |
| Durable turn | `dataplane.TurnIngress`, `QueueTurnDispatcher`, `QueueTurnScheduler`, `TableDeadLetterQueue`, `TurnRuntime`, `DurableSessionStore`, `StepIdempotencyStore`, `ObjectStateStore`, `MemoryTurnQueue`, `dataplanetest` suites | [doc/persistence.md](doc/persistence.md) |
| Isolation | `isolation.DistributedTenantRateLimiter`, `LatencyBudgetAllocator`, `StaticStageLatencyModel` | [doc/configuration.md](doc/configuration.md) |
| Observability | `internal/stage` spans, `observability.BoundedRuntimeMetrics`, `LabelPolicy`, `TenantHeavyHitters`, `AuditingObservationRecorder` | [doc/observability.md](doc/observability.md) |
| Knowledge | `knowledge.IngestPipeline`, `SectionChunker`, `ManifestStore`, `VersionAliaser`, `GarbageCollector`, `PgVectorIndex`, `CachedRetriever`, `ShardedRetriever` | [doc/knowledge_ingestion.md](doc/knowledge_ingestion.md) |
| Evaluation | `evaluation.Runner`, `ManifestBuilder`, `PairedScorer`, `GatewayJudge`, `GateIssuer`, `GateHealthEvaluator`, `RetrievalScorer` | [doc/evaluation.md](doc/evaluation.md) |
| Rollout | `release.RolloutController`, `PinnedTrafficManager`, `TableConversationReleaseStore`, `SessionDrainer`, `RollbackDrillHarness` | [doc/rollout.md](doc/rollout.md) |
| Guardrails | `guardrail.LayeredEnforcer`, `RuleSetCompiler`, `toolgateway.CircuitBreaker` | [doc/guardrails.md](doc/guardrails.md) |

## Schema modules

The schema is twelve selectable modules (`catalog`, `tenancy`, `prompt`, `model`, `agent`, `tool`, `graph`, `knowledge`, `knowledge_vector`, `runtime`, `release`, `evaluation`) so a downstream creates only the tables its product uses: 27 Scout tables for Agent Studio alone, 82 for the full platform. The generated DDL and seed output for the full set are byte-identical to the previous five-group layout.

## Remaining

- **K7** — HANA vector adapter, on demand only; the trigger criteria are recorded above.
- **Upstream keel** — `cache.CacheService` needs one atomic multi-scope admission primitive so `DistributedTenantRateLimiter` can charge tenant and fleet scopes in a single round trip. Until it exists, the limiter charges tenant then fleet and compensates on rejection, which leaves a documented over-admission window equal to the number of in-flight admissions.
- **Schema** — `model_definition` has no model version, region, or quality-class column, so `TableCandidateCatalog` derives one route per model, takes the region from the deployment, and treats every model as one quality class. Routing by version, region, or quality needs those columns or a `model_route` child table.
