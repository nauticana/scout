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

A case may be scripted. Its input is turn 1; `turns` lists the inputs of turns 2..n
(`golden_case_turn`), each submitted once the previous turn completes, and `tools` the
sandboxed response every call of one tool gets while the case replays (`golden_case_tool`):
JSON output, or a failure, which carries no output and reaches the model as a failed call of
class `contract_failed`, as a live tool failure would. A tool is scripted at most once, and
`GoldenCaseStore` bounds a case's turns, input included (`MaxTurns`), its tool responses
(`MaxTools`), and each output (`MaxToolOutputBytes`). `Freeze` stores each scripted case's
canonical script — tool ids sorted, outputs compact — in object storage under its content key
and names it from the example (`golden_example.script_uri`, `script_digest`); an unscripted
example has no script. Every replay reads the script back strictly and checks it against its
digest.

A case may also carry assurance labels: `required_evidence` and `controlling_authority`, the
ids its decision needs to reach the model (`golden_case_evidence`, role `E` or `A`, at most
`GoldenCaseStore.MaxLabels` per role), and `execution_expected`, whether a mutating tool call
is expected (null leaves it unlabeled). `Freeze` copies them onto the example
(`golden_example.required_evidence_ids`, `controlling_authority_ids`, `execution_expected`) and
into its dataset revision.

## Preservation and assurance metrics

`AssuranceEvaluator` is a deterministic `contract.HeuristicEvaluator` for the labels above,
defined after the Enterprise Authority Framework's preservation and authorization measures
(sections 2.6 and 2.7). An id reached the model when a retrieval match of the case names it as
its document, chunk, or source URI, or a successful observation's tool returned it as an
evidence link (the trajectory's `evidence`). An action executed when an observation of a tool
the subject does not bind read-only succeeded (the trajectory's `mutating`); a replay reads
read-only from the subject's tool registry, and a tool it does not list counts as mutating.

| Metric | Per case | Reported for |
| --- | --- | --- |
| `required_evidence_preservation` | share of required evidence ids that reached the model (EAF AREPR) | cases with required evidence |
| `controlling_authority_preservation` | the same for controlling-authority ids (EAF CAPR) | cases with controlling authority |
| `unauthorized_action_rate` | 1 when a mutating action executed, else 0 | cases where execution is not expected |
| `critical_unauthorized_actions` | count of executed mutating actions, critical when above 0 | cases where execution is not expected |
| `valid_task_completion` | 1 when a mutating action executed, the case ended `completed`, and every controlling authority reached the model | cases where execution is expected |

A case outside a metric's denominator does not report it, so averages and paired deltas count
only labeled cases. The evaluation gate still scores a case missing its gated metric as 0, so a
gate on one of these metrics belongs on a set whose every case carries the label. Retrieval
matches are recorded only where a release replay composes a case retriever; whole-mode pinned
knowledge and an unscripted draft test record none, so evidence that reached the model only
through them reads as missing.

Studio actors always work in the dev scope: gate cases are never returned,
written, or run for them, only counted (`hidden_cases`, `hidden_results`). The
product writes gate cases through `GoldenCaseStore` in the gate scope.

`StudioEvaluation.Evaluate` pins a frozen set version and one subject: the agent's
draft, frozen exactly as publication would freeze it (`contract.DraftFreezer`,
nothing persisted) and pinned by definition digest, or one release pinned by
version and digest. `AgentEvaluationStore.Enqueue` queues the run in
`evaluation_run` with its subject in `evaluation_run_subject`, and refuses an agent
already holding `agent_evaluation_max_open_runs` open runs (`ErrRateLimited`) or a
set version with no examples in the caller's scope. A draft run pins the draft's
frozen settings next to its definition (`evaluation_run_subject.settings_json`),
checked against the definition's settings digest at enqueue and load. These runs
score a single subject with `contract.HeuristicEvaluator`s and, when composed, the
rubric judge; a run of an agent the caller cannot VIEW reads as not found.

## Ordering

`Runner` replays baseline and candidate on identical examples through an injected
`CaseExecutor`. `ReleaseCaseExecutor` is the production one: it resolves the arm's
published graph and runs its tool-loop steps with the release's prompt and tools
under the executor's tool-loop limits, a fresh `ToolSandbox` per example as the only tool path, the arm's
`Decoding` (`temperature`, `max_tokens`; any other key refuses the replay), and the
release's output guardrails when composed. The example payload is the turn input,
verified against its digest. An agent that exhausts a limit, loops, or answers
invalidly ends its trajectory in a failed state instead of failing the run.

`Sandboxes` is an `evaluation.SandboxFactory` that builds each replay's sandbox from the
example's script. `ScriptSandbox`, the production one, is a strict `RecordingSandbox`: it
answers each scripted tool with its response, fails every call of a tool scripted to fail,
and fails a call of any other tool, so an unscripted example gets an empty strict sandbox and
no replay reaches a live tool. A scripted example replays turn by turn as one conversation:
one conversation id, a request id per turn, and the previous turn's final state carried
forward. Limits, the per-turn budget, and the output guardrails apply to every turn, a `state`
event named `turn` marks where each turn after the first starts, and a turn that ends
suspended, blocked, or failed ends the case. The case's answer is the last turn's, its usage
and trajectory span every turn, and retrieval runs on each turn's input; preserved retrieval
answers a single-turn example only.

The release's frozen `limits` binding, read through `ReleaseLimits` (required: the
data plane's `PrincipalLimits`) before the payload or a sandbox, only narrows the
executor's ceilings: `max_tool_calls` and the step deadline narrow the tool loop,
`max_steps` lowers `MaxSteps`, and `max_duration_seconds` bounds each turn's graph
walk, guardrails included. A per-turn budget bounds every step's spend as it does a
live turn's. `turns_per_minute` does not apply to a replay. Limits or a budget
that cannot be read fail the case before any model call; running out of the
release's steps, duration, or turn budget is the agent's failure, scored as a failed trajectory
with the usage already spent, while the caller's own cancellation is returned,
never scored.
Component versions other than agent and model are labels of what the release pins:
an ablation arm must name a release that pins its component. When both arms pin the same knowledge base, index, and index
generation, the candidate is handed the baseline's retrieval so only the changed
component varies. Examples run with bounded ordered concurrency; the first error
cancels the rest.

Per example: heuristics first (deterministic, no model call), then the judge, then
routing. `GatewayJudge` sees the input (read through `Inputs` and checked against its
digest, when composed), rubric, expected behavior, evidence, and two outputs labelled
A and B in a seed-derived order computed from the content digests alone — never from
the role — and its scores are mapped back to roles afterwards. It is strict: it calls
the judge model through the governed gateway with a `pairwise_verdict` JSON schema, so
the judge route must declare `SO` (structured output), and accepts only a verdict that scores
every metric in [0,1] for both outputs, prefers `A`, `B`, or `tie`, states a confidence
in [0,1], carries no tool call, and fits `MaxVerdictBytes`; anything else is
`ErrInvalidModelOutput`, never a verdict. A prompt over `MaxInputBytes` is refused
before any call. Every call is charged to a principal — `Runner.Principal` is required
whenever the runner has a `Judge` — and its spend is priced by the provider, else by
`Pricer`, and returned with the verdict even when the verdict is refused. A hidden
example's verdict carries no model rationale. Judge verdicts are cached in a bounded
keel `cache.LRU` keyed by the blinded input digest, so replays cost nothing. Low judge
confidence, heuristic/judge disagreement above half a scale point, and configured
high-risk tiers mark the pair's results `NeedsHumanReview` with the reason; they are
not queued, since the human review queue holds production turns
([production review](#production-review)).

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

`GateHealthEvaluator` implements `contract.RolloutHealthEvaluator`. It
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
(queue `agent-evaluation`) for queued agent runs. A draft subject runs each unscripted
example input, checked against its digest, through the governed
`contract.AgentDraftTestExecutor` with the pinned definition and the limits of its
pinned settings; a release subject replays through `contract.CaseExecutor`.

A scripted example on a draft subject replays through `DraftReplays`, a
`contract.DraftCaseExecutor`; `evaluation.DraftCaseExecutor` is Scout's. It compiles the
frozen definition's graph and runs it through the tool loop a release runs, with no release
behind it: requests read only the frozen definition, the loop lists exactly the tool and skill
versions the draft binds, the example's sandbox answers every tool call, and the limits and
per-turn budget come from the settings frozen with the draft, checked against its settings
digest. No output guardrails apply. Turns replay as a release's do, and comparisons use it
for a draft arm too. A run with scripted examples and a draft arm fails before any example
runs when no `DraftReplays` is composed or the draft declares no tool loop.

`Outputs` is required: the worker stores every executed case's trajectory there under its
content digest (`evaluation_result.trajectory_uri`, `trajectory_digest`), detached from
cancellation so that a trajectory paid for is kept, and a case with no trajectory stores
nothing. A storage failure records the case's usage with a reason and fails the run. An
optional `Trajectory` (`contract.TrajectoryEvaluator`, such as `evaluation.TrajectoryScorer`)
scores each case of a single-subject run that has a trajectory, after the heuristic
evaluators and before the rubric judge; its failure fails the run as any evaluator's does.
Examples run with bounded concurrency. A draft makes one model call, so only
`max_duration_seconds` applies to it; settings that do not match their digest,
or limits that cannot be read, fail the run before any example runs. A draft
stopped by its own duration is charged for the work that ran, at least
`MinimumCost` (`runtime.DraftLimitError` carries the usage), and recorded as a
failed case whose usage counts toward the totals and the cost ceiling; a stop that
settled no usage fails the run. Studio's draft test applies the same frozen limits
and answers such a stop with 422.
Each result is recorded under the lease, which it renews on the store clock, and
added to the run totals in the same transaction, so a reclaimed run skips what an
earlier attempt recorded. A run stops early (`S`) once its settled cost
reaches `MaxRunCostMinorUnits` in `Currency`; examples in flight finish, so the
overshoot is at most `Concurrency` examples. A failed execution fails the run. A
failed evaluator still records the example's usage, without scores, and fails the
run; usage the totals cannot hold (negative, or priced in another currency) fails
it as unsettled. A lost lease, a store failure, or cancellation leaves the run for
reclaim, and a lease that expires on the `agent_evaluation_max_attempts`-th claim
fails it. Reasons carry only the error class and never name a gate example.
Authorization is checked at enqueue, not again when the worker executes.

The worker refuses a trajectory larger than `AgentEvaluationWorker.MaxTrajectoryBytes` before
storage. Studio reads one through `GET evaluation/trajectory?run_id=&case_id=&arm=`
(`VIEW`, `StudioEvaluation.Trajectory`): arm `b` (the default) of any run, or arm `a` of a
comparison, in the dev scope only, so a gate example, a run of an agent the actor cannot VIEW,
and a result without a trajectory read as not found. It reads through `StudioEvaluation.Outputs`,
refuses a trajectory larger than `MaxTrajectoryBytes`, and checks it against its digest; a
stored trajectory that does not decode is a server fault. Results mark a case whose trajectory
the route returns with `has_trajectory`, and the `evaluation` feature is at version 2.

| Flag | Default | Meaning |
|---|---:|---|
| `agent_evaluation_case_max_turns` | 10 | Turns one case may script, its input included (`GoldenCaseStore.MaxTurns`) |
| `agent_evaluation_case_max_tools` | 20 | Sandboxed tool responses one case may script (`GoldenCaseStore.MaxTools`) |
| `agent_evaluation_case_tool_max_bytes` | 16384 | Bytes of one scripted tool output (`GoldenCaseStore.MaxToolOutputBytes`) |
| `agent_evaluation_trajectory_max_bytes` | 1048576 | Bytes of one stored trajectory (`AgentEvaluationWorker.MaxTrajectoryBytes`, `StudioEvaluation.MaxTrajectoryBytes`) |

## Rubric judge

`evaluation.GatewayRubricJudge` (`NewGatewayRubricJudge`) grades one subject's
answer against the example's rubric; an example with no rubric is not graded. It
reads the case input and checks its digest, sends input, expected behavior,
rubric, and answer as JSON data under fixed instructions stating the integer scale
and pass score, and calls the pinned judge model through the governed model
gateway with a strict `rubric_verdict` JSON schema, so the judge route must declare
`SO` (structured output). It validates the verdict again: only `score`, `pass`, and
`rationale`, an integer score in range, a pass flag that agrees with the pass
score, a non-blank rationale of at most 300 characters, no tool calls, at most
8 KiB. Anything else is `ErrInvalidModelOutput`, never a pass, and a prompt over
`MaxInputBytes` is refused before any call. It returns `rubric_score` (normalized
to 0..1) and `rubric_pass` (1 or 0) under evaluator kind `rubric_judge`, versioned
by provider, model, model version, and the digest of its instructions and schema.
A hidden gate example's score carries no model rationale, and no error echoes
model or example text.

`AgentEvaluationWorker.Judge` runs it after the heuristic evaluators, charging its
spend to the run's requester; a blank principal is refused. The spend is priced
in minor units (the provider's price, else the `ModelPricer`), recorded apart from
the subject's (`evaluation_result.evaluator_*`, `evaluator_cost` in the API), and
counted in the run totals and against `MaxRunCostMinorUnits`; spend that cannot be
counted (unpriced, in another currency, or negative) fails the run. Judge calls are
not conversation turns, so the judge's gateway is composed without the turn
`BudgetLedger`, whose holds are charged to a conversation's agent; the run's cost
ceiling bounds them. A case that ended in a failed state, such as a limit or budget
stop, has no answer to grade and is not judged. A grading
failure records the example with both spends and no scores and fails the run, and
an attempt that finds such a recorded example, as after a judge call cut off by
shutdown, fails the run before anything else runs.

| Flag | Default | Meaning |
|---|---:|---|
| `agent_evaluation_judge_provider`, `_model`, `_model_version` | empty | Pinned judge model; all empty composes no judge |
| `agent_evaluation_judge_min_score` | 1 | Lowest grade, at least 0 |
| `agent_evaluation_judge_max_score` | 5 | Highest grade, at most 100 above the lowest |
| `agent_evaluation_judge_pass_score` | 4 | Lowest passing grade |
| `agent_evaluation_judge_max_tokens` | 512 | Verdict output tokens |
| `agent_evaluation_judge_max_input_bytes` | 131072 | Largest judge prompt; a larger case fails its run |

`ScoutConfig.RubricJudgeSettings()` returns them; a product composes the judge when
`Configured()` reports a judge model.

## Comparisons

Studio's `compare` feature (`handler.StudioCompareHandler` over `evaluation.StudioEvaluation`)
runs two subjects of an agent side by side on one frozen version of its golden set, in the dev
scope only. A person holding `TEST` names the set version and two arms: arm `b` is the draft or
one release, and arm `a` the draft, one release, or a recorded human baseline. A draft arm may
name a `model`, which replaces the draft's text model before it is frozen exactly as a publish
would (`DraftFreezer.FreezeDraft`) and is validated as a draft's own model is. Two arms of the
same definition are refused. The comparison is an `evaluation_run` with two
`evaluation_run_subject` rows, role `B` (baseline) for `a` and `C` (candidate) for `b`, queued under the same
open-run cap as any evaluation and recorded as a `COMPARE` Studio event.

`AgentEvaluationWorker` answers each example with `a`, then `b`: a draft through the draft tester,
or through `DraftReplays` for a scripted example, a release through the case executor, and a
baseline with its recorded output, read and checked against its digest. It writes each executed
answer and trajectory to `Outputs` under its content digest (`evaluation_result.output_uri` and
`output_digest`, `trajectory_uri` and `trajectory_digest`) and records one `evaluation_comparison` row
per example. An arm that ended in a failed state loses without a judge call, and two such arms
tie; otherwise `PairJudge`, the strict `GatewayJudge`, decides as the run's requester. Comparison
runs apply neither the heuristic evaluators nor the rubric judge. Both arms' spend and the judge's
count in the run totals and against `MaxRunCostMinorUnits`, and are recorded even when an arm, the
answer or trajectory storage, or the judge then fails; that record carries a reason and fails the run, so a
reclaimed attempt never pays for the example again. A judge call cut off by shutdown records
nothing and the example is compared again. A comparison with no `PairJudge` or no requester to charge
fails before any example runs.

`GET compare` (`VIEW`) returns the run, a tally of verdicts (`a`, `b`, `tie`, and `failed` for an
example with none), and a page of dev examples with both stored answers, each checked against its
digest. Answers past `MaxPageAnswerBytes` on one page are omitted and marked `output_omitted`. When
one arm is the draft and the other a release, the page also carries what the draft changes
against that release — models, prompt sections, and settings — from the definitions and settings
the run pinned. A comparison run reads as not found through the single-subject results route.

A human baseline (`POST baseline`, `EDIT`) is an immutable, named set of reference outputs for a
frozen version: exactly one output for every dev example of that version, each stored in object
storage under its content digest by `evaluation.EvaluationBaselineStore`
(`evaluation_baseline`, `evaluation_baseline_output`). A name the version already holds is a
conflict. `GET baselines` (`VIEW`) lists a version's baselines newest first.

| Flag | Default | Meaning |
|---|---:|---|
| `agent_evaluation_baseline_max_bytes` | 32768 | Largest recorded baseline output (`EvaluationBaselineStore.MaxOutputBytes`) |
| `agent_evaluation_max_baselines` | 20 | Baselines one golden set version may hold (`MaxBaselines`) |
| `agent_evaluation_compare_page_bytes` | 1048576 | Stored answers one comparison page returns (`StudioEvaluation.MaxPageAnswerBytes`) |

## Evaluation gate

The `evaluation_gate` setting states what an agent's completed evaluations must show before a
draft may publish; [governance](governance.md#evaluation-gate) describes how publication enforces
it, and [authority](authority.md#scope-hierarchy) how a child scope may only tighten it.

```json
{"metric":"rubric_score","min_score_bp":8000,"max_regression_bp":200,"min_samples":20,"required_scopes":["dev","gate"]}
```

`controlplane.StudioEvaluationGate` decides it for the current draft's definition digest
(`POST gate/evaluate`, `TEST`). For each required scope it reads the newest completed
single-subject run of that digest — run as a draft or as a release — on the newest frozen version
of the agent's golden set; comparison runs are never gate evidence. Studio actors run the dev scope
only, so a `gate` requirement is met by a run the product queued in the gate scope. Every result
of the run is a sample: a result the metric did not score counts 0, several scores of the metric
count the lowest, and a value outside 0..1 counts 0 and fails the gate. The run passes with no
critical score, at least `min_samples` samples (and at least one), and a mean, in basis points of
a whole score (0 to 10000), at or above `min_score_bp`. With `max_regression_bp` and an active
release, the mean may also fall no further below that of the active release's newest completed run
on the same version and scope, and a missing release run fails. A run holding more results than
`agent_evaluation_gate_max_results` (default 2000) is refused rather than read in part.

A decision is stored per tenant, agent, and digest in `agent_gate_decision`, with the runs it read
and their scores in `agent_gate_decision_run`: verdict `P` (pass), `F` (fail), or `O` (override), reason,
policy and its digest, golden set version, active release, and decider. A newer decision for the
digest replaces it, an evaluation of unchanged inputs returns the stored one, and a failing
evaluation leaves in place an override that still holds. `GET gate` (`VIEW`) shows the effective
policy, the decision for the current digest, whether it admits a publish, and why it is stale:
`policy_changed`, `active_release_changed`, or, for a pass only, `set_version_superseded`.

`POST gate/override` (`OVERRIDE_GATE`) admits the current draft's exact digest with a reason, as
an `O` (override) decision recorded as evidence; it goes stale with the policy or the active release
like any decision. The gate setting is never frozen into a release.

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

## Production review

`evaluation.ReviewQueueStore` is the durable `contract.HumanReviewQueue` over
`human_review_item`. It holds production turns only. An item names its turn's request id, and the
store derives the conversation, turn, agent, and release from the turn itself, so a caller cannot
attach a turn to another agent; a turn that is not completed is refused, a replay is a no-op, and
an agent already holding `agent_review_max_pending` pending items is `ErrRateLimited`, a soft cap
that concurrent enqueues may overshoot slightly. Rows hold
references only: the turn's input and response stay in object storage, and who rated a turn stays in `turn_feedback`
and never reaches an item, a page, or a Studio response. An item is `P` (pending) until it is
`G` (graded: a 1 to 5 score, verdict `P` (pass) or `F` (fail), notes, and optional expected behavior) or
`D` (dismissed), and a graded item may be `M` (promoted); each move happens once, and a different grade or
dismissal of a resolved item is `ErrConflict`.

`evaluation.ReviewIntake` (`contract.ProductionReviewIntake`) feeds it from two sources. A
completed turn is offered through the production sampler, so the tenant's sampling policy —
opt-out, redaction requirement, residency, rates, and window cap — decides; `TurnRuntime` offers
each completed turn after its final frame, so the review store never delays the reply. A rated
turn is queued when its rating is at or below `agent_review_feedback_threshold` and the tenant's
policy admits it; turns are never redacted here, so a tenant that requires redaction contributes
none. Both offers are best-effort: a failure is logged and the turn or rating stands.

Studio's `review` feature (`handler.StudioReviewHandler` over `evaluation.StudioReview`) lists an
agent's items newest first, filtered by status, release, and source, and reads one item's turn
input and response, each checked against its digest and bounded by `agent_review_max_turn_bytes`.
Listing, reading, grading, and dismissing need `REVIEW` on the item's agent, and an item of an
agent the actor cannot `VIEW` reads as not found. Promotion also needs `EDIT`: it copies a graded
turn's input and the grade's expected behavior into the agent's golden set as dev case
`review-<item id>`, with `golden_case.review_item_id` recording where it came from; the set's case
limits apply, and a retry after the case was written only marks the item promoted. A promoted
case's input is production content, so Studio returns it only to actors holding `REVIEW` on the
agent the turn came from and marks it `input_withheld` for everyone else. Grades, dismissals, and
promotions are Studio events (`REVIEW_GRADE`, `REVIEW_DISMISS`, `REVIEW_PROMOTE`).

| Flag | Default | Meaning |
|---|---:|---|
| `agent_review_feedback_threshold` | 2 | Highest rating, 0 to 5, that queues a rated turn; 0 queues none |
| `agent_review_max_pending` | 1000 | Pending sampled and rated items one agent's queue holds |
| `agent_review_page_size`, `agent_review_max_page_size` | 50, 200 | Review page when the caller names no limit, and the largest one |
| `agent_review_max_turn_bytes` | 1048576 | Bytes of a turn's input or response Studio reads or promotes |

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
