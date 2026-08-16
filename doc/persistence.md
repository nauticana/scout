# Persistence design: durable turns, checkpoints, and step idempotency

Reference implementations live in `service/dataplane/`: `ObjectStateStore` (`state_codec.go`),
`DurableSessionStore` (`durable_session_store.go`), `StepIdempotencyStore`
(`step_idempotency_store.go`), and the cache discipline in `SessionCoordinator` (`session_coordinator.go`).

## DTO ↔ table identity

| DTO | Table | Notes |
|---|---|---|
| `domain.ExecutionStep.ExecutionStepID` | `execution_step.id` | Compiled surrogate id; the only step identity persistence keys on. `StepID` is the logical name and is derived on load via `execution_step.step_id`. |
| `domain.StepCheckpoint` | `step_checkpoint` | `(tenant_id, conversation_id, turn_no, step_no)`; `IdempotencyKey`, `Fingerprint` (64-hex), `Usage` (currency required) map 1:1. `State` is dehydrated to `state_uri`/`state_digest` (`StateRef`). No checkpoint id or request id column exists; neither is stored. |
| `domain.SessionSnapshot` | `session_snapshot` + `agent_conversation.agent_version` + latest checkpoint's `execution_step.step_id` | `Revision` = `session_snapshot.revision`; `State` is hydrated from `state_uri`/`state_digest`, digest verified before return. A conversation with no checkpoint loads at revision 0 with empty state. |
| `domain.TurnResult.Response` | `conversation_turn.response_uri`/`response_digest` | Written by `Complete` on the conversation's oldest non-terminal turn (`queued`/`running`/`streaming`). |
| `domain.StepResult` (JSON) | `step_idempotency.result_uri`/`result_digest` | Keyed by `(tenant_id, request_id, execution_step_id)`. |
| `domain.ObjectRef{URI, Digest}` | any `*_uri`/`*_digest` pair | URI `<scheme>://<bucket>/<key>`, digest lowercase SHA-256 hex. |

The repository never invents a fingerprint, currency, or digest: `Checkpoint` rejects a checkpoint that lacks them
(`domain.ErrValidation`).

## Object keys and orphan cleanup

`ObjectStateStore` writes `<KeyPrefix>/<name>/<sha256>` where `<name>` is deterministic:
`checkpoint/<tenant>/<conversation>/<turn>/<step>`, `turn/<tenant>/<conversation>/<turn>/response`,
`step/<tenant>/<request>/<execution_step_id>`. Identical retries overwrite the same object; different content
from a concurrent writer never collides, so a loser can never corrupt the winner's referenced bytes.

Object upload always precedes the row write. On row failure the store deletes its upload only after confirming
the durable row does not reference that digest (`scout_session_checkpoint_digest`, `scout_session_response_digest`,
`scout_step_find`); a concurrent identical replay may legitimately share the object. Delete failures are joined
onto the original error, never swallowed. What remains unreferenced (delete failed, crash between upload and row
write) is reclaimed by a reconciliation sweeper that lists the prefix and drops objects no row references —
the same worker that expires reservations (IDEAS E4). Reads always fail closed: a missing or tampered object
is `ErrDigestMismatch`/download error, never empty state.

## Transaction boundaries

- `Checkpoint`: upload → **one tx** { insert `step_checkpoint`; `revision = expected → expected+1` CAS on
  `session_snapshot` (expected 0 inserts revision 1 with `ON CONFLICT DO NOTHING`) } → commit. Zero CAS rows is
  `domain.ErrRevisionConflict` and rolls the checkpoint insert back.
- `Complete`: read the executing turn and current revision (autocommit) → conflict early if the revision moved
  → upload response → single guarded `UPDATE conversation_turn` (status IN non-terminal AND snapshot revision =
  expected). Zero rows is `ErrRevisionConflict`. Revision is not bumped by `Complete`. Budget settlement and the
  `usage_event` insert are `TurnLedger`'s terminal transaction; a runtime that composes both runs `Complete`
  inside that same terminal transaction or immediately before it, never after the final frame.
- `StepIdempotencyStore`: every transition is a single-row CAS on `status_code` (autocommit); `updated_at` is
  written from the injected clock so lease arithmetic never mixes worker and database time.

## Order of durability for one step / turn

1. `StepIdempotencyStore.Begin` claims the step (or replays the committed result — no side effect re-runs).
2. Execute; `Commit` uploads the JSON result, then CAS `claimed → committed`.
3. `SessionCoordinator.Checkpoint` (durable checkpoint + snapshot CAS, then cache invalidation).
4. Guardrail-approved *intermediate* frames publish (sequence-deduplicated by the reply hub, so replay republishes the same sequences).
5. Terminal result + settlement persist (`Complete` / `TurnLedger` terminal tx, one `usage_event`).
6. Final frame publishes. It is never client-visible before step 5 exists; a crash between 5 and 6 replays the frame from the durable result.
7. Queue ack. Nothing is acked from cache state.

## Idempotency state machine

```
(none) --Begin--> claimed --Commit--> committed   (terminal; Begin replays the stored result)
claimed --Abandon--> abandoned --Begin--> claimed   (the replay transition; abandoned is reclaimable)
claimed --lease expired (updated_at + ClaimLease <= now)--Begin--> claimed (CAS re-lease)
claimed (live) --Begin--> ErrConflict
```
`Commit` on a lost claim is `ErrConflict` unless the row is already `committed` with the same digest (duplicate
delivery → no-op). `Abandon` twice is a no-op; `Abandon` after `committed` is `ErrConflict`. The schema has no
owner column, so a worker whose lease expired can still commit if it finishes before the reclaimer; both ran the
side effect (inherent to lease expiry) and the second `Commit` observes `ErrConflict`.

## Cache discipline (`SessionCoordinator`, `MemorySessionCache`)

- Durable write completes before any cache write; the cache is only ever *invalidated* by writers.
- Every cache entry carries `Revision`. `MemorySessionCache.Put` never replaces a newer cached revision with an older one.
- The coordinator remembers the highest revision it wrote per conversation (bounded LRU, `RevisionMemory`).
  A cache hit below that floor is invalidated and reloaded; a durable read below it fails with `ErrStaleEvidence`
  instead of regressing.
- An in-flight durable load overlapping a successful write is marked superseded: after its `Put` it invalidates
  again rather than leaving a superseded revision cached.
- Failed `Checkpoint`/`Complete` still invalidate (the cached entry is stale by definition or of unknown standing).
- Local TTL never exceeds the remote/shared TTL: `MemoryCacheConfig.RemoteTTL` is validated by the factories.
- Misses coalesce through singleflight; a lost cache costs latency only.
