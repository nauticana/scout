# Scout — open work

Only what is still open. Shipped work is recorded per release in [migration_guide.json](migration_guide.json) and designed in `doc/`.

Severity: **MED** = needed before a first customer, **LOW** = adopt on demand.

| ID | Item | Severity | Blocked on |
|---|---|---|---|
| K7 | HANA vector adapter, same contract as `knowledge.PgVectorIndex`. Build when a tenant corpus outgrows one pgvector tier, ingest competes with search, filtered recall stays below target after `iterative_scan`/`ef_search` tuning, the source already lives in HANA under a residency requirement, or a separate PostgreSQL vector tier costs more. | LOW | demand |
| A3 | OPA or cedar-go evaluator behind `contract.PolicyDecisionPoint`. Policy state stays in Scout; an external engine only evaluates. | LOW | demand |
| K4 | SPIFFE SVID workload identity behind `PrincipalResolver` and RFC 8693 token exchange for the delegated half, with a keel-issued fallback the readiness check reports. | LOW | demand |
| D5 | A2A adapter behind `contract.AgentInvoker` for cross-process delegation; `input-required` is the wire form of a suspended turn. The type version's AgentCard ships with it. | LOW | demand |
| V4 | Map `domain.Observation` onto the OTel GenAI semantic conventions, version-pinned. | LOW | the conventions leaving Development stability |
| P4 | Cache resolved principals with an explicit TTL and a revocation invalidation path. | LOW | a profile showing resolution matters |
| V2 | GRC export format over `contract.AuditQuery`. | LOW | a customer naming a format |
| E1 | OpenFGA (first) or SpiceDB, only when entitlements must derive from relationships rather than label subsets. | LOW | that requirement |
| C4 | Semantic response cache, default off and shadow-measured. Its namespace is the tenant, the immutable release identity, model and provider version, prompt, guardrail, knowledge and tool-contract versions, entitlement fingerprint, language, and decoding parameters; the tenant filter runs inside the similarity index, never after it; the precision floor cannot be lowered by a caller; non-deterministic, time-sensitive, tool-using, and conversation-state responses are never stored; hit quality is measured, not only hit rate. Stateless deterministic tasks first. | LOW | a leak-safety proof in shadow mode |
| F1 | Amend the agent-organization source analysis (idea-12): §12's authorization change is only the subject side (`agent_permission`), not the engine; §7 splits the agent principal into keel authorization and Scout runtime threading, and adds durable human-in-the-loop; §7's match with the prompt chain is conceptual, generalised by the scope compiler; §14's per-agent price is measured from scope-attributed usage. | LOW | the idea-12 owner |

Won't do: GPU placement, gang scheduling, autoscaling, continuous batching, or KV-cache management inside Scout (serving layer); `organization_unit`, `position`, or HR semantics (commercial application); Temporal or another durable-execution platform as the runtime; agents as rows in `user_account`; a generic cache, HTTP client, queue, clock, hash, or heap (keel or the standard library); replacing keel's `port.WebSocketHub` with the reply hub; a model-vendor adapter added only for breadth.
