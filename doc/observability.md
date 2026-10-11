# Observability: stage attribution and bounded cardinality

Reference implementations live in `service/observability/`: `LabelPolicy` (`label_policy.go`),
`BoundedRuntimeMetrics` (`bounded_runtime_metrics.go`), `TenantHeavyHitters` (`tenant_heavy_hitters.go`),
`AuditingObservationRecorder` (`auditing_observation_recorder.go`), and `KeelMetricSink` (`keel_metric_sink.go`).
The measurement primitive is `internal/stage`; the sketch math is `internal/heavyhitters`.

## Where an observation comes from

`stage.Begin(now, stage, component, versions)` opens a `*Span`; the caller fills the tenant, route, and
streaming fields on `span.Observation`, then `span.End(now, outcome, usage, err)` returns an immutable
`domain.Observation`. Duration is `now - StartedAt`, the error class comes from `errors.Is` over the
`domain/errors.go` sentinels plus `context.Canceled`/`DeadlineExceeded` — never from error text — and an
unmatched error is `internal`. An empty outcome is derived (`ok`, `canceled`, `error`); pass `OutcomeDegraded`
or `OutcomeRejected` explicitly. When the error is a `*stage.Error`, the observation is re-attributed to the
failing stage, so a publish failure inside the model stage is reported against `publish`.

Emitting services take an optional `Observer contract.ObservationRecorder`; nil skips all measurement.
`dataplane.StreamPump` reports the model stage with `TimeToFirst` = first approved frame minus stage start and
`TimePerOutput` = (last − first) / (outputTokens − 1) — approved frames only, so guardrail-blocked bytes never
count as throughput. A run that ends with a canceled context reports `canceled`, not `error`.
`knowledge.HybridRetriever` reports the retrieval stage and reports `degraded` whenever the result carries
degradations (partial legs, reranker failure) even though the query itself succeeded.

## The label rule

Adapters receive tenant identity in `RuntimeMetrics` and `domain.Observation` because exact accounting needs
it. `Observation.Principal` and `Observation.ScopeID` are the same kind of dimension: exact accounting for
the tenant ledger, never a fleet label. None of them may become a time-series label. `LabelPolicy.Sanitize` enforces the rule: keys must be
allowlisted (`tenant_tier`, `priority_class`, `model`, `provider`, `region`, `stage`, `component`, `release`,
`outcome`, `error_class`, `verdict`, `tenant_rank`, `currency`), `tenant_id`/`request_id`/`conversation_id`
and prompt/response/document keys are refused outright, and values are bounded to 64 bytes of
`[A-Za-z0-9._:/-]` so free text cannot slip in through a value.

`BoundedRuntimeMetrics` implements both `contract.RuntimeMetrics` and `contract.ObservationRecorder` and
writes only names from `Catalog` (`metric_catalog.go`) to a `contract.MetricLabelSink`, always through the
policy. A sample whose labels violate the policy is dropped and counted as `scout_metric_label_rejections_total`
rather than silently exported. Exact per-tenant accounting happens only on the optional
`contract.TenantLedger`, which is the single consumer allowed to key on `TenantID`; a ledger write failure is
counted (`scout_ledger_errors_total`) and never suppresses the fleet series.

## Top-N without unbounded churn

`TenantHeavyHitters` decorates an `ObservationRecorder`. It feeds a Count-Min sketch (`Width`, `Depth`, `Seed`)
and a bounded top-K heap, both reset when `Window` elapses on the injected `Now`. `Export` writes exactly
`TopK` series labeled `tenant_rank=1..K`; empty slots export zero, so tenant membership churn cannot add
series over time. Identity is resolved outside the metrics backend through `Snapshot()`, which returns
`(windowStart, []TenantHeavyHitter{Rank, TenantID, Estimate})` for a protected diagnostics path. `Merge`
folds a replica's sketch in and refuses an incompatible width, depth, or seed, so cross-replica aggregation
is only ever done on compatible sketches. The sketch never underestimates; its additive error is
`e/Width × window total`, exceeded with probability at most `e^-Depth`.

## Exact records per agent and model

Fleet series carry no tenant, agent, or person, so per-agent views read exact records instead:

- **Usage per model.** `Usage.Models` (`domain.ModelSpend`) is the share each model performed, costed in the
  usage currency; the model gateway attributes usage to the route that served the call. Settlement writes one
  `usage_event` row per provider and model, plus one row with no model for the remainder (tool calls, and spend
  nothing attributed). A turn and category settle once, under an advisory lock in the caller's transaction, and
  a breakdown that exceeds the totals, repeats or blanks a model, or is negative is refused before any write. The
  ledger stages the breakdown in `conversation_turn_detail.staged_model_usage`, so crash recovery writes the same
  rows. Studio's `usage` report groups by release, model, person, work item, and currency; a turn that used two
  models counts on both model lines.
- **Safety events.** `safety_event` keeps each guardrail intervention's attribution — inspected agent, request id,
  stage, layer, action, severity, rule ids, release, policy version — never the inspected content.
  `TableSafetyEventSink.SafetyEvents` pages one agent's events newest first, at most `MaxSafetyEventPage` per page,
  behind Studio's `safety-events` route.
- **Limits and spend.** Studio's `limits` report (`dataplane.StudioLimits`) shows the stable release's frozen
  limits, the agent's turns opened in the last minute, and its budget: ceilings, currency and frozen exponent,
  period with its window or timezone, and `source` (`release`, or `fallback` for the composed default). For a rolling, day, or month budget, `spend` reads the enforcing `BudgetLedger`
  (`StudioLimits.Spend`) over the current period by the database clock: usage settled in it and live holds,
  apart, with cost counted only in the budget's currency. A budget the ledger cannot enforce reports its
  `problem` code instead of spend; see [release budgets](governance.md#release-budgets).

## Agent health and alerts

`dataplane.AgentHealth` measures one agent's production health from the same exact records, by the
database clock: its turns, through `agent_conversation`, counted by queue time, and its `safety_event`
rows. A measure counts turns per status, the failure rate in basis points as failed over completed
plus failed turns (cancelled turns are not judged), the p50 and p95 duration and time to first
response from queueing over completed turns, the latest completion and failure, and safety events per
severity.

When a measure alerts is the agent's `health_alerts` setting, compiled from the `health_alerts`
bindings on its scope chain alone (see [scope hierarchy](authority.md#scope-hierarchy)):

```json
{"window_minutes":60,"min_turns":20,"max_failure_rate_bp":500,"max_p95_latency_ms":30000,"max_hard_safety_events":0,"max_silence_minutes":120}
```

Every field is a non-negative integer and 0 is not evaluated. `window_minutes` (at most a week; 0
takes `agent_health_window`) is the measurement window, and `min_turns` the sample floor: the
fewest judged turns, never below one, on which a failure rate or latency is read. A measure must
exceed a `max_*` threshold to open an alert; `max_silence_minutes` (at most a week) is the longest an
active agent with a stable release may serve no turn. No release freezes the setting, so a change
applies at the next evaluation without a publish.

Studio's `health` feature (`handler.StudioHealthHandler` over `AgentHealth`, `VIEW`) serves the
health detail and the alert list. `GET health` returns the measure over the policy window, the
effective `policy` or the `policy_problem` that keeps the chain from compiling, `insufficient_samples`
when the judged turns are below the sample floor, so no rate or latency verdict may be read, the open
alerts, `last_evaluated_at`, and `telemetry`: `stale` when the agent is active, its policy evaluates
something, and the evaluator has not measured it within `agent_health_stale_periods` ×
`agent_health_period`; else `missing` when the window holds no turn; else `fresh`. `GET alerts` pages
an agent's alerts newest first, optionally by status. An agent the actor cannot VIEW is not found.

`agent_alert` holds the alerts: `kind_code` `F` (failure rate, in basis points), `L` (p95 latency, in
milliseconds), `H` (hard safety events), or `S` (silence: turns in the span and the span's minutes),
with the measured value, the threshold it exceeded, and the window. `AgentHealth.EvaluateAll` opens
and resolves them in passes:

- It pages active agents by (tenant, agent) and skips one whose policy evaluates nothing.
- An alert opens only on a measure that breaches its threshold. Opening inserts only when no alert of
  that kind is open, under the partial unique index on open alerts, so a replayed pass or a concurrent
  replica never opens one twice; only a newly inserted alert counts as opened.
- An alert resolves only on a measure with at least the sample floor of judged turns that no
  longer breaches; latency also needs that many completed turns and a p95. The absence of data never
  resolves a failure rate, latency, or safety alert.
- Removing a policy or disabling one threshold resolves any open alert for that threshold.
- Silence is judged only for an agent with a stable release that has been serving, active under its
  current deployment, for the whole span: no turn queued in the span opens it, and a turn resolves it.
- Each write names one tenant and agent and is a conditional insert or update; nothing takes a lock.
  The measure is recorded in `agent_health_state`, whose `evaluated_at` decides staleness.

`AgentHealth.Notifier` (`contract.AgentAlertNotifier`) delivers each newly opened alert to the people or
systems the product routes it to. A failed delivery leaves the alert open, is not retried, and is
counted as undelivered. One agent's failure does not stop the pass; the pass reports agents
evaluated, skipped, and failed, and alerts opened, resolved, and undelivered.
`dataplane.NewAgentHealthWorker` is the keel `JobWorker` a product runs from its own worker binary:
it composes `AgentHealth` on its first pass, takes its interval from `agent_health_period` once
configuration loads, and journals each pass. `ScoutConfig.AgentHealthSettings()` returns the flags.

| Flag | Default | Meaning |
|---|---:|---|
| `agent_health_period` | 60 | Seconds between evaluator passes |
| `agent_health_batch` | 100 | Active agents one page of a pass reads |
| `agent_health_window` | 60 | Minutes a measure covers when the policy names no window |
| `agent_health_stale_periods` | 3 | Periods an agent with a policy may go unevaluated before its telemetry is stale |
| `agent_alert_page_size`, `agent_alert_max_page_size` | 50, 200 | Alert page when the caller names no limit, and the largest one |

## Audit trail

`AuditingObservationRecorder` wraps any recorder and writes a `domain.DecisionRecord` for every `rejected` or
`error` observation. The payload is the redacted subset — stage, component, versions, provider/model, region,
tier, outcome, error class, duration, token counts, and the already-redacted trace id — with no tenant id in
the body, no request or conversation id, and no prompt or response bytes. Audit write failures go to the
injected error handler; they are never swallowed.

```go
metrics, err := observability.NewBoundedRuntimeMetrics(observability.BoundedRuntimeMetricsConfig{
    Sink: sink, Release: "2026.08.1", Ledger: ledger,
})
hitters, err := observability.NewTenantHeavyHitters(observability.TenantHeavyHittersConfig{
    Width: 2048, Depth: 4, Seed: 0x5C007, TopK: 10, Window: time.Minute, Next: metrics,
    Weight: func(o domain.Observation) int64 { return o.Usage.InputTokens + o.Usage.OutputTokens },
})
pump := &dataplane.StreamPump{Guardrails: guardrails, Publisher: publisher, Observer: hitters}
```
