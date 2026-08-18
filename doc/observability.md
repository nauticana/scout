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

## Audit trail

`AuditingObservationRecorder` wraps any recorder and writes a `domain.AuditEvent` for every `rejected` or
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
