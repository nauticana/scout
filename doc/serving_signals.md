# Serving signals: the contract with an external autoscaler

Scout is not a GPU scheduler. It publishes what it knows about queued work and observed latency per route, and
consumes capacity/drain state produced by the serving control plane (K8s/KServe, vLLM, Ray Serve, or a vendor
gateway). Placement, replica counts, gang scheduling, and node drain stay outside.

## What Scout exports

`modelgateway.ServingSignalCollector` aggregates `domain.ServingSample` values into one `domain.ServingSignal`
per route per window and pushes them to the injected `contract.ServingSignalExporter` on `Flush(ctx)` — call it
from a periodic worker; the collector owns no goroutine and no timer.

A route is `(Provider, Model, ModelVersion, Region, RouteID)` — the identity fields of `domain.ModelSelection`.
Capacity pool, routing generation, and reason vary per request and are deliberately excluded so a route's series
stays stable and bounded.

| Field | Meaning |
|---|---|
| `QueuedPrefillTokens` | Estimated prompt tokens admitted to the route this window. |
| `QueuedDecodeTokenS` | Admitted output tokens priced by the route's latest observed TPOT, in token-seconds. |
| `QueueWaitP50/P95` | Admission-to-capacity wait, from the gateway and `FairSlotLimiter`. |
| `TimeToFirstP95`, `TimePerOutputP95` | Observed TTFT and per-output-token latency. |
| `AdmissionRejections` | Tenant-level rejections that never reached the route. |
| `CapacityOutcomes` | Bounded counters: `granted`, `rejected`, `completed`, `failed`, `canceled`. |
| `KVPressure` | Provider-reported cache pressure in `[0,1]`, when reported. |
| `Draining` | Drain state of the route at flush time. |

Zero durations and counts mean *not observed*, never *observed as zero*. Percentiles are nearest-rank over the
last `MaxSamples` observations per route; `MaxRoutes` bounds cardinality and refused samples are counted by
`Dropped()`.

Feed the collector by setting it as `Gateway.Signals` (`contract.ServingSignalObserver`); schedulers and limiters
may call `ObserveServing` directly with the same route identity.

## What Scout consumes

The serving control plane publishes `domain.CapacitySnapshot` per route through
`contract.CapacitySnapshotPublisher` (`modelgateway.MemoryCapacitySnapshotSource` is the in-process reference).
Beyond `Healthy` and `PredictedQueueDelay`, a snapshot carries `Warm` (model loaded), `Owner` (the owning serving
unit), `ServiceRate` (live decode tokens/s), `PrefillCapacity`/`DecodeCapacity`, `KVPressure`, `Draining`,
`DrainDeadline`, `ObservedAt`, and `Generation`.

Freshness is mandatory: a snapshot older than the router's `MaxSnapshotAge` (or the tenant policy's) is treated
as unknown, and an unknown route is ineligible unless the tenant policy sets `AllowUnknownCapacity`. Publish on
a cadence shorter than that bound.

## Drain semantics

`Draining = true` means **no new admissions**: `PolicyRouter` drops the route from candidates and
`ResilientGateway` refuses `Generate`/`Stream` with `domain.ErrNoRoute`. Streams already running continue until
`DrainDeadline`. At that instant the gateway cancels the provider stream and ends the client stream with an
explicit partial completion — `FinishReason = "interrupted"` plus a `StreamDeadlineError{Kind: "drain"}`
attributed to `StageModel` — never a silent restart and never spliced output. A zero `DrainDeadline` on a
draining route means "no grace": the running stream is cut at the next frame boundary.

The autoscaler therefore owns the grace period. Set `DrainDeadline` to the longest generation you are willing to
wait for; Scout stops sending new work immediately and guarantees the route is idle after it.
