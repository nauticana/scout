# Platform rollout and version pinning

Scout keeps two rollout controls apart and gives each its own persisted identity:

- **Agent version** — one tenant's immutable agent definition, routed by `AgentVersionTrafficManager`, persisted on `agent_conversation.agent_version`.
- **Platform release** — the certified platform artifact and the component set it ships, advanced by `PlatformReleaseRolloutController` through tenant rings, persisted on `conversation_release.platform_version`.

A conversation resolves both once, at creation, and reads them back on every later turn. Rolling one back never rewrites the other.

## Release bundle

`domain.ReleaseBundle` is the signed set a platform release certifies: model, provider, tokenizer, runtime, decoding defaults, prompt, embedding/reranker and index generation, tool versions, safety policy version, schema-migration set, provenance/signature/signer key, compatibility constraints, residency policy, and the **rollback target**. `release.CanonicalBundle` produces the canonical JSON and its SHA-256 digest; `TableReleaseBundleStore` stores that content verbatim and re-verifies the digest on read, so a tampered row fails with `ErrConflict` instead of serving.

The resolved identity travels with the turn: put the platform version (or bundle digest) in `domain.ComponentVersions.Release`, alongside `Agent`, `Model`, `Prompt`, `Index`, `Tool`, and `Guardrail`, on every observation, usage event, and audit record. Without it, a regression cannot be attributed to the release that caused it.

## State machine

`build → offline_replay → shadow → internal_canary → tenant_canary → regional_ramp → global_default → retired`, plus `rolled_back` and `quarantined` reachable from any live stage. Stages come from `RolloutPlan`; `DefaultRolloutPlan` ships conservative ring, traffic, minimum-sample, and minimum-duration values.

Each cycle, `RolloutController`:

1. takes the per-release lease (`RolloutLease`) — a second controller gets `ErrConflict`, not a concurrent write;
2. asks `DetailedRolloutHealthEvaluator` (falling back to `RolloutHealthEvaluator`: true = healthy, false = unhealthy, error = inconclusive);
3. `healthy` → advance, but only past the stage's minimum samples, minimum duration, and post-breach cooldown;
4. `inconclusive` (including evaluator errors) → pause with the reason recorded; an operator resumes;
5. `unhealthy` → hard breach (isolation, severe safety, corrupt output, availability) rolls back and quarantines immediately; a soft breach needs `SoftBreachWindows` consecutive breached windows, and `HealthyToClear` clean windows reset the counter;
6. writes the new state with a monotonic **generation CAS** under the lease and records an audited `platform_rollout_transition`.

`Tick` runs one cycle over every live, unpaused release. Rollback is idempotent and ordered so a partial failure is safe: it points the ring alias at the bundle's rollback target and restores that release's capacity *before* committing the rolled-back state, so a failed alias switch leaves the durable state live and the whole step retries rather than declaring a rollback that never took effect. Live conversations keep the release they were created on and the evidence stays in place. Calling it twice changes nothing and audits once.

Bypasses are scoped (`evidence`, `minimums`, `all`), time-boxed, and require an approver distinct from the requester; every bypass is audited before it can waive a gate.

The shadow stage mirrors a bounded share of authenticated, redacted traffic through `ShadowTrafficSampler`. The mirrored copy must produce no user-visible output and no side effects, and the controller pauses the stage when the observed shadow-to-live ratio exceeds `MaxShadowAmplification`.

## Version pin precedence

`PinnedTrafficManager` resolves an agent version in one order, and `PublishedAgentResolver` delegates to it:

1. compliance pin (approved and signed),
2. approved tenant pin, effective and not expired, matching the region,
3. experiment cohort by stable hash of tenant, agent, experiment, salt, and subject,
4. deployment default: canary by stable hash, else stable.

A pin whose `CompatiblePolicyVersions`/`CompatibleIndexVersions` exclude the running policy or index is rejected with `ErrConflict` — a pinned request never silently drifts onto another version. Every resolution is audited with the rule that won (`domain.AgentVersionResolution.Source`). `PinAwareGarbageCollector.Collectable` refuses to drop a version that is deployed, pinned, in a cohort, or held by an open conversation.

## Stickiness and drain

`StickyReleaseResolver` resolves both identities at session creation and only reads them afterwards; asking for a different agent version on an existing conversation is `ErrConflict`, never a switch. `SessionDrainer` applies `SessionDrainPolicy` at turn boundaries:

- release still live → nothing changes;
- rolled back → existing conversations keep serving until `Window` elapses, then migrate to the current safe release (agent version unchanged);
- quarantined → migrate at the next turn, and with `CancelOnCriticalSafety` cancel the running turn through `TurnCanceller`, returning `ErrTurnCanceled` so the caller records an explicit partial status and retains state.

Version changes only ever happen between turns. Tokens are never spliced across releases mid-stream.

## Drills and probes

`RollbackDrillHarness` rehearses a rollback and reports per-check results: the bundle declares a reachable rollback target, alias propagation works, capacity is restorable, session drain behaves, and the release has an alert owner. `ProbeRunner` executes synthetic probes through `ContractTestExecutor`, running holdout probes against the bundle's rollback target so candidate and baseline are comparable.
