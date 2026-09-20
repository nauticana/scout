# Scout — open work

Only what is still open. Shipped work is recorded per release in [migration_guide.json](migration_guide.json) and designed in `doc/`; the gap analyses that produced it are [IDEAS.md](IDEAS.md) and [IDEAS.DWF.md](IDEAS.DWF.md). IDs are the ones those documents and the release notes use.

Severity: **MED** = needed before a first customer, **LOW** = adopt on demand.

| ID | Item | Severity | Blocked on |
|---|---|---|---|
| H5 | Reference the notification delivery record from `approval_request`. keel's `outbox_event` is the stable row, but the foreign key makes keel's `outbox` schema module a dependency of `approval`. | MED | decision on that dependency |
| R5s | Provider-native streaming in the `provider` adapters. They are single-frame today, so `text_delta` events and mid-step frames have nothing to carry; typed events travel on each step's frame. | MED | a consumer rendering partial output |
| R7 | Versioned skill/procedure artifact: immutable instructions, allowed tool bindings, inputs, eval-set reference, release lifecycle, agent binding. | LOW | two real downstream procedures exposing the same lifecycle |
| K7 | HANA vector adapter, same contract as `knowledge.PgVectorIndex`. Build when a tenant corpus outgrows one pgvector tier, ingest competes with search, filtered recall stays below target after `iterative_scan`/`ef_search` tuning, the source already lives in HANA under a residency requirement, or a separate PostgreSQL vector tier costs more. | LOW | demand |
| A3 | OPA or cedar-go evaluator behind `contract.PolicyDecisionPoint`. Policy state stays in Scout; an external engine only evaluates. | LOW | demand |
| K4 | SPIFFE SVID workload identity behind `PrincipalResolver` and RFC 8693 token exchange for the delegated half, with a keel-issued fallback the readiness check reports. | LOW | demand |
| D5 | A2A adapter behind `contract.AgentInvoker` for cross-process delegation; `input-required` is the wire form of a suspended turn. The type version's AgentCard ships with it. | LOW | demand |
| V4 | Map `domain.Observation` onto the OTel GenAI semantic conventions, version-pinned. | LOW | the conventions leaving Development stability |
| P4 | Cache resolved principals with an explicit TTL and a revocation invalidation path. | LOW | a profile showing resolution matters |
| V2 | GRC export format over `contract.AuditQuery`. | LOW | a customer naming a format |
| E1 | OpenFGA (first) or SpiceDB, only when entitlements must derive from relationships rather than label subsets. | LOW | that requirement |

Won't do: GPU placement, gang scheduling, autoscaling, continuous batching, or KV-cache management inside Scout (serving layer); `organization_unit`, `position`, or HR semantics (commercial application); Temporal or another durable-execution platform as the runtime; agents as rows in `user_account`.
