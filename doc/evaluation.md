# Quality evaluation pipeline

Contract tests prove compatibility. Evaluation proves quality, and a rollout gate
consumes only evidence that can be reproduced and audited.

## Evidence model

An `EvaluationManifest` is immutable and content-addressed: its `ManifestID` is
the SHA-256 of the canonical JSON of every other field, so any change to a pinned
agent, model, prompt, knowledge, index, tool, guardrail, decoding setting,
evaluator version, safety policy, or dataset revision produces a different
manifest. `ManifestBuilder.Build` stamps the id; `Verify` refuses a mutated one.
`DatasetRevision` digests the golden examples order-independently.

Golden examples carry provenance, consent and retention class, risk tier, domain
and language slices, rubric reference, expected behavior, and a payload
`ObjectRef` — large content stays in object storage. Examples marked `Hidden`
live under `scope_code = 'gate'`; `GoldenSetStore` takes an authorization scope on
every read and write, so prompt authors working in the dev scope can neither list
nor fetch a gate example, and cannot write one either.

## Agent golden sets

Studio authors an agent's golden set as saved test cases. `GoldenCaseStore` keeps
the editable working copy in `golden_case`; every add, replace, remove, and freeze
names the set's `working_revision` and advances it, so a stale edit is
`ErrRevisionConflict`. `Freeze` uploads each input to object storage under its
SHA-256 content key, then in one transaction writes the next immutable
`golden_set_version` with one `golden_example` per case, dev and gate alike. Runs
read examples back through `GoldenSetStore`, so the scope rule above decides what
runs. An agent uses one set (`agent_golden_set`). A set created through Studio is
owned by its agent (`golden_set.owner_agent_id`); sharing an existing set needs
EDIT on its owning agent, and a set no agent owns belongs to the product and is
refused.

Studio actors always work in the dev scope: gate cases are never returned,
written, or run for them, only counted (`hidden_cases`, `hidden_results`). The
product writes gate cases through `GoldenCaseStore` in the gate scope.

`StudioEvaluation.Evaluate` pins a frozen set version and one subject: the agent's
draft, frozen exactly as publication would freeze it (`contract.DraftFreezer`,
nothing persisted) and pinned by definition digest, or one release pinned by
version and digest. `AgentEvaluationStore.Enqueue` queues the run in
`evaluation_run` with its subject in `evaluation_run_subject`, and refuses an agent
already holding `agent_evaluation_max_open_runs` open runs (`ErrRateLimited`) or a
set version with no examples in the caller's scope. These runs score a single
subject with `contract.HeuristicEvaluator`s; a run of an agent the caller cannot
VIEW reads as not found.

## Ordering

`Runner` replays baseline and candidate on identical examples through an injected
`CaseExecutor`. `ReleaseCaseExecutor` is the production one: it resolves the arm's
published graph and runs its tool-loop steps with the release's prompt and tools
under the executor's tool-loop limits (not the release's `limits` binding), a fresh `ToolSandbox` per example as the only tool path, the arm's
`Decoding` (`temperature`, `max_tokens`; any other key refuses the replay), and the
release's output guardrails when composed. The example payload is the turn input,
verified against its digest. An agent that exhausts a limit, loops, or answers
invalidly ends its trajectory in a failed state instead of failing the run.
Component versions other than agent and model are labels of what the release pins:
an ablation arm must name a release that pins its component. When both arms pin the same knowledge base, index, and index
generation, the candidate is handed the baseline's retrieval so only the changed
component varies. Examples run with bounded ordered concurrency; the first error
cancels the rest.

Per example: heuristics first (deterministic, no model call), then the judge, then
routing. `GatewayJudge` sees rubric, expected behavior, evidence, and two outputs
labelled A and B in a seed-derived order computed from the content digests alone —
never from the role — and its scores are mapped back to roles afterwards. Judge
verdicts are cached in a bounded keel `cache.LRU` keyed by the blinded input digest,
so replays cost nothing. Low judge confidence, heuristic/judge disagreement above
half a scale point, and configured high-risk tiers route the pair to
`HumanReviewQueue`; the results still record the reason.

`PairedScorer` produces per-slice paired deltas (`all`, `domain:`, `language:`,
`risk:`) with sample counts and percentile bootstrap confidence intervals from an
injected seed. Promotion requires the minimum sample count, no critical safety
failure, and a CI lower bound within tolerance on the aggregate and on every
protected or high-risk slice. `AblationMatrix` derives one arm per differing
component for regression attribution. `Decided` reports when the primary effect is
settled either way, which lets the runner stop sequentially between batches.

## Gate semantics

`GateIssuer` turns a summary into a `GateDecision`: manifest and dataset revision,
judge versions, deltas, confidence, verdict, explicit approvals, issue and expiry
timestamps, and telemetry freshness. Evidence older than `MaxTelemetryAge` is
refused with `ErrStaleEvidence`. The canonical payload is signed through the
pluggable `GateSigner` (`HMACSigner` is the stdlib HMAC-SHA256 reference) and every
decision is audited.

`GateHealthEvaluator` implements `contract.DetailedRolloutHealthEvaluator`. It
reads the latest decision for a platform build and returns `RolloutInconclusive` —
never healthy — when it is missing, expired, or fails signature or content-id
verification, and likewise when online telemetry is stale or under the sample
floor. A breached online metric is `RolloutUnhealthy`; otherwise the stored verdict
stands. Losing trustworthy evidence pauses promotion, it never advances it.

## Worker composition

Evaluation is a separate keel Worker binary, never the serving path. Compose the
runner with a low-priority model route and batch judge calls; a serving-critical
capacity pool must not queue behind an evaluation run. Give each change an
explicit spend budget and track the cost of a run against the regressions it
detected — a gate that costs more than the incidents it prevents is misconfigured.
`EvaluationRun` records sample counts and minor-unit cost per run for exactly that
ratio.

`AgentEvaluationWorker` (`NewAgentEvaluationWorker`) is the keel leased queue worker
(queue `agent-evaluation`) for queued agent runs. A draft subject runs each example
input, checked against its digest, through the governed
`contract.AgentDraftTestExecutor` with the pinned definition; a release subject
replays through `contract.CaseExecutor`. Examples run with bounded concurrency.
Each result is recorded under the lease, which it renews on the store clock, and
added to the run totals in the same transaction, so a reclaimed run skips what an
earlier attempt recorded. A run stops as `stopped_early` once its settled cost
reaches `MaxRunCostMinorUnits` in `Currency`; examples in flight finish, so the
overshoot is at most `Concurrency` examples. A failed execution fails the run. A
failed evaluator still records the example's usage, without scores, and fails the
run; usage the totals cannot hold (negative, or priced in another currency) fails
it as unsettled. A lost lease, a store failure, or cancellation leaves the run for
reclaim, and a lease that expires on the `agent_evaluation_max_attempts`-th claim
fails it. Reasons carry only the error class and never name a gate example.
Authorization is checked at enqueue, not again when the worker executes.

## Sampling, storage, adjudication

`SamplingPolicyEnforcer` reads a per-tenant `SamplingPolicy` and applies, in order:
opt-out, redaction requirement, residency, elevated rates for risky, escalated, or
uncertain turns, then the hard per-tenant window cap. It sees only content-free
signals, so nothing samplable ever reaches a metric label.

`EncryptedSampleStore` seals payloads with keel `crypto.Seal` (AES-256-GCM, key
from the keystore) into object storage and keeps only the URI, digest, retention
class, and region in `evaluation_sample`. Reads are tenant-scoped, digest-verified,
and refuse a sample past its retention. `Adjudicator` folds production failures
into digest buckets so one recurring failure is reviewed once, and only an explicit
accept makes a bucket eligible for a golden set. `ScoreDriftDetector` reports a
practical effect size and only calls drift sustained after consecutive windows.

## Calibration

`Calibrator` reports Cohen's κ on pass/fail, Krippendorff's α on interval scores,
precision and recall on critical failures, and position and self-preference bias
from swapped-order trials, per judge version. Recalibrate whenever the judge
prompt or model changes — the judge version embeds both — and treat a κ or critical
recall drop as a reason to widen human review, not to lower thresholds.

## Retrieval evaluation

`RetrievalScorer` evaluates golden queries independently of generation: recall@K,
MRR, nDCG@K, filter selectivity, citation precision, abstention quality, and
ingestion and tombstone lag. Every golden query names the principal and
entitlements it must be answered for; any returned match outside them is counted as
an authorization leak and emitted as a critical score, which blocks promotion
regardless of how good the ranking metrics look.
