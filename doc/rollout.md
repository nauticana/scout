# Platform rollout and version pinning

Scout keeps two rollout controls apart and gives each its own persisted identity:

- **Agent version** — one tenant's immutable agent definition, routed by `AgentVersionTrafficManager`, persisted on `agent_conversation.agent_version`.
- **Platform release** — the certified platform artifact and the component set it ships, advanced by `PlatformReleaseRolloutController` through tenant rings, persisted on `conversation_release.platform_version`.

A conversation resolves both once, at creation, and reads them back on every later turn. Rolling one back never rewrites the other. Merging them would force a ring change on every tenant publish and make rollback ambiguous; the release bundle is a manifest naming what a rollback restores, not a third traffic control.

## Release bundle

`domain.ReleaseBundle` is the signed set a platform release certifies: model, provider, tokenizer, runtime, decoding defaults, prompt, embedding/reranker and index generation, tool versions, safety policy version, schema-migration set, provenance/signature/signer key, compatibility constraints, residency policy, and the **rollback target**. `release.CanonicalBundle` produces the canonical JSON and its SHA-256 digest; `TableReleaseBundleStore` stores that content verbatim and re-verifies the digest on read, so a tampered row fails with `ErrConflict` instead of serving.

The resolved identity travels with the turn: put the platform version (or bundle digest) in `domain.ComponentVersions.Release`, alongside `Agent`, `Model`, `Prompt`, `Index`, `Tool`, and `Guardrail`, on every observation, usage event, and audit record. Without it, a regression cannot be attributed to the release that caused it.

## State machine

`build → offline_replay → shadow → internal_canary → tenant_canary → regional_ramp → global_default → retired`, plus `rolled_back` and `quarantined` reachable from any live stage. Stages come from `RolloutPlan`; `DefaultRolloutPlan` ships conservative ring, traffic, minimum-sample, and minimum-duration values.

Each cycle, `RolloutController`:

1. takes the per-release lease (`RolloutLease`) — a second controller gets `ErrConflict`, not a concurrent write;
2. asks `RolloutHealthEvaluator` for a healthy, unhealthy, or inconclusive verdict (an evaluation error is inconclusive);
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
3. pilot: the release a named person, or a position they hold, pilots,
4. experiment cohort by stable hash of tenant, agent, experiment, salt, and subject,
5. deployment default: canary by stable hash, else stable.

A pilot is explicit, effective-dated membership in `agent_pilot_member`, not a hash cohort: Studio's `pilot` feature adds or ends one subject's membership of one published, unquarantined release (a `principal` by user id or a product `position` reference; one open membership per subject per agent, at most `agent_pilot_max_members` per agent). `TablePilotResolver` sees only the trusted subject, `VersionRequest.Subject`, which the data plane takes from the turn's `OnBehalfOf`; a turn acting for no one, a non-human subject, and alias resolution without a session are in no pilot. The person's own membership wins, otherwise the newest membership of a position the product's `PositionResolver` says they hold, each call bounded by `PositionTimeout` (`agent_pilot_position_timeout`). A release quarantined after it was added is skipped and resolution falls through. A pilot resolver error fails the resolution; it never falls through to cohort or deployment.

A pin whose `CompatiblePolicyVersions`/`CompatibleIndexVersions` exclude the running policy or index is rejected with `ErrConflict` — a pinned request never silently drifts onto another version. A manager with an audit sink records every resolution with the rule that won (`domain.AgentVersionResolution.Source`).

The data plane resolves once, when a conversation starts: `TableTurnRecordStore.Versions` (by default a `PinnedTrafficManager` over the agent's deployment; a product with the `release` module adds its pins, pilots, and cohorts) chooses the release, and `agent_conversation` records it with `version_source`. A caller holding `AGENT_CONVERSATION` `PIN_RELEASE` may instead pin a new conversation to one unquarantined release with a reason (`request_pin`). Studio publication deploys a release as stable or, with a canary percentage, beside it, at once or at a scheduled time; Studio's `traffic` feature then canaries, promotes, and rolls back, and `explain` reports which release a conversation runs on and why. `release.StudioTraffic` requires a `contract.PublishSeparation`, the same one Studio's publish reviews use: where it separates restores, its traffic manager implements `ReviewedAgentVersionTrafficManager`, a canary only re-weights the current canary, a promote targets only the canary or the stable release, and a rollback only ends a canary. Returning any other release to service is a restore that takes an approved [restore review](governance.md#two-person-publish-and-restore). Pilots and version pins are not restores. `PinAwareGarbageCollector.Collectable` refuses to drop a version that is deployed, pinned, in a cohort, piloted, or held by an open conversation.

## Scheduled activation

A publish with a future `effective_at` (at most `MaxActivationLead`, 366 days ahead) writes the immutable release, its frozen settings, and every release writer's output as usual, but instead of deploying records one pending `agent_release_activation` with the definition digest and the stable and canary versions it will replace; the release returns with `active: false`. An agent holds at most one pending activation, and a scheduled canary needs an existing stable release. Studio's `activation` feature lists activations and cancels a pending one.

`ReleaseActivations.ActivateDue` is the worker pass, run by `ReleaseActivationWorker` from a product worker binary (`agent_activation_batch`, `agent_activation_poll`, `agent_activation_max_attempts`). It claims each row due by the database clock with `FOR UPDATE SKIP LOCKED` and, in one transaction bounded by `Timeout`, repeats what publication checked: the scheduler's PUBLISH authority, the definition digest, the frozen settings against `SettingsDigest`, and every `PublishGuard`; it then locks the agent's draft row, refuses an archived agent, and compare-and-swaps `agent_deployment` against the versions recorded at scheduling. A deployment already serving the release as planned counts as activated, so a replay is harmless; any other change since scheduling (an immediate publish, restore, promote, rollback, or canary) refuses it as `DC` (deployment changed) rather than overwriting it. A definite failure refuses the activation with a reason code and an `ACTIVATION_REFUSED` audit; a transient one rolls the claim back and retries after attempts × poll period until the attempt ceiling refuses it as `AE` (attempts exhausted). A stopping worker is not charged an attempt. Only the deployment changes, so live conversations keep the release they were created on.

## Stickiness and drain

`StickyReleaseResolver` resolves both identities at session creation and only reads them afterwards; asking for a different agent version on an existing conversation is `ErrConflict`, never a switch. `SessionDrainer` applies `SessionDrainPolicy` at turn boundaries:

- release still live → nothing changes;
- rolled back → existing conversations keep serving until `Window` elapses, then migrate to the current safe release (agent version unchanged);
- quarantined → migrate at the next turn, and with `CancelOnCriticalSafety` cancel the running turn through `TurnCanceller`, returning `ErrTurnCanceled` so the caller records an explicit partial status and retains state.

Version changes only ever happen between turns. Tokens are never spliced across releases mid-stream.

## Drills and probes

`RollbackDrillHarness` rehearses a rollback and reports per-check results: the bundle declares a reachable rollback target, alias propagation works, capacity is restorable, session drain behaves, and the release has an alert owner. `ProbeRunner` executes synthetic probes through `ContractTestExecutor`, running holdout probes against the bundle's rollback target so candidate and baseline are comparable.
