# Persistence design: durable turns, checkpoints, and step idempotency

Reference implementations live in `service/dataplane/`: `ObjectStateStore` (`state_codec.go`),
`DurableSessionStore` (`durable_session_store.go`), `StepIdempotencyStore`
(`step_idempotency_store.go`), the turn fence (`turn_fence.go`, `turn_runtime_records.go`, `table_turn_control.go`),
the turn settlement (`turn_settlement.go`), call outcomes (`call_outcome_store.go`, `tool_loop_outcomes.go`),
and the cache discipline in `SessionCoordinator` (`session_coordinator.go`).

## DTO ↔ table identity

| DTO | Table | Notes |
|---|---|---|
| `domain.ExecutionStep.ExecutionStepID` | `execution_step.id` | Compiled surrogate id; the only step identity persistence keys on. `StepID` is the logical name and is derived on load via `execution_step.step_id`. |
| `domain.StepCheckpoint` | `step_checkpoint` | `(partner_id, conversation_id, turn_no, step_no)`; `IdempotencyKey`, `Fingerprint` (64-hex), `Usage` (currency required) map 1:1. `State` is dehydrated to `state_uri`/`state_digest` (`StateRef`). No checkpoint id or request id column exists; neither is stored. |
| `domain.SessionSnapshot` | `session_snapshot` + `agent_conversation.agent_version` + latest checkpoint's `execution_step.step_id` | `Revision` = `session_snapshot.revision`; `State` is hydrated from `state_uri`/`state_digest`, digest verified before return. A conversation with no checkpoint loads at revision 0 with empty state. |
| `domain.TurnResult.Response` | `conversation_turn.response_uri`/`response_digest` | Written by `Complete` on the owner's turn, which must be the conversation's oldest non-terminal turn and `R` (running) or `S` (streaming) at the owner's fence. |
| `domain.StepResult` (JSON) | `step_idempotency.result_uri`/`result_digest` | Keyed by `(partner_id, request_id, execution_step_id)`. |
| `domain.CallOutcome` | `turn_call_outcome` (+ `turn_call_attestation`, `turn_call_confirmation`) | Keyed by `CallOutcomeKey` `(partner_id, request_id, execution_step_id, call_id)`; the `domain.CallRecord` (output, usage, evidence links, effect, or acceptance) is dehydrated to `result_uri`/`result_digest`. |
| `domain.ObjectRef{URI, Digest}` | any `*_uri`/`*_digest` pair | URI `<scheme>://<bucket>/<key>`, digest lowercase SHA-256 hex. |

The repository never invents a fingerprint, currency, or digest: `Checkpoint` rejects a checkpoint that lacks them
(`domain.ErrValidation`).

## Object keys and orphan cleanup

`ObjectStateStore` writes `<KeyPrefix>/<name>/<sha256>` where `<name>` is deterministic:
`checkpoint/<tenant>/<conversation>/<turn>/<step>`, `turn/<tenant>/<conversation>/<turn>/response`,
`step/<tenant>/<request>/<execution_step_id>`, `loop/<tenant>/<request>/<execution_step_id>/<entry_no>`,
`call-outcome/<tenant>/<request>/<execution_step_id>/<call_id>`. Identical retries overwrite the same object; different content
from a concurrent writer never collides, so a loser can never corrupt the winner's referenced bytes.

Object upload always precedes the row write. On row failure the store deletes its upload only after confirming
the durable row does not reference that digest (`scout_session_checkpoint_digest`, `scout_session_response_digest`,
`scout_step_find`); a concurrent identical replay may legitimately share the object. Delete failures are joined
onto the original error, never swallowed. What remains unreferenced (delete failed, crash between upload and row
write) is reclaimed by a reconciliation sweeper that lists the prefix and drops objects no row references —
the same worker that expires reservations. Reads always fail closed: a missing or tampered object
is `ErrDigestMismatch`/download error, never empty state.

## Transaction boundaries

- `Checkpoint`: upload → **one tx** { guarded touch of the owner's turn, which must be the checkpoint's turn;
  insert `step_checkpoint`; `revision = expected → expected+1` CAS on `session_snapshot` (expected 0 inserts
  revision 1 with `ON CONFLICT DO NOTHING`) } → commit. Zero touch rows is `domain.ErrFenceLost`; zero CAS rows is
  `domain.ErrRevisionConflict` and rolls the checkpoint insert back.
- `Complete`: read the executing turn and current revision (autocommit) → the executing turn must be the owner's,
  else `ErrFenceLost` → conflict early if the revision moved → upload response → **one tx** { guarded touch of the
  owner's turn, which must be the executing turn; the settlement (below); guarded `UPDATE conversation_turn`
  (request and fence = the owner's AND status `R`/`S` AND snapshot revision = expected) } → commit. Zero rows is
  `ErrFenceLost` when the fence or status moved, else `ErrRevisionConflict`, and either rolls the settlement back.
  A reservation no longer held refuses the completion. Revision is not bumped by `Complete`.
- `Fail` (owner): upload the error code → **one tx** { guarded touch, which names the turn and conversation; the
  settlement; the failing `UPDATE` with `AND fence = ?` } → commit. A reservation no longer held does not keep the
  turn from failing: the failure and its usage events commit and `Fail` returns `ErrBudgetUnsettled`.
- **Turn settlement** (`TurnSettlementWriter`, inside `Complete` and `Fail`): `BudgetLedger.SettleTurnTx` binds the
  ledger's queries into the caller's transaction (keel `TxQueryCatalog`), takes the tenant advisory lock, and
  commits the reservation at the turn's usage, or releases it when a failed turn spent nothing; then the turn's
  `usage_event` lines are inserted once per turn and category, without the usage lock, because the guarded touch
  already serializes the turn. Locks are taken in the order turn row, then tenant. A fence that moved before the
  touch refuses the whole unit, and once the touch is taken the fence cannot move until commit, so a reservation
  is never settled while its turn's end can still be refused. `TurnLedger`'s synchronous terminal transaction
  remains the other settlement path.
- **Call outcomes:** `SettleAndAppend` is **one tx** { guarded touch; settle the open row; insert the observation
  entry first-writer-wins }; `Accept` is **one tx** { fenced `I`→`A`; the `turn_call_confirmation` link };
  `Attest` is **one tx** { conditional touch of the outcome row; the attestation row; the settlement once enough
  people agree }; `confirmation.TableStore.Reconcile` settles linked rows in the confirmation's own transaction.
  Every other call-outcome write is one statement.
- `StepIdempotencyStore`: every transition is one fenced statement, the owner's guarded touch plus a single-row
  CAS on `status_code` (autocommit); `updated_at` is written from the injected clock so lease arithmetic never
  mixes worker and database time.

## Order of durability for one step / turn

0. `TurnRecordStore.Acquire` takes the turn with the delivery's queue claim and moves its fence; every write
   below presents that owner.
1. `StepIdempotencyStore.Begin` claims the step (or replays the committed result — no side effect re-runs).
2. Execute; `Commit` uploads the JSON result, then CAS `C` (claimed) → `M` (committed).
3. `SessionCoordinator.Checkpoint` (durable checkpoint + snapshot CAS, then cache invalidation).
4. Guardrail-approved *intermediate* frames publish (sequence-deduplicated by the reply hub, so replay republishes the same sequences).
5. Terminal result + settlement persist in one transaction (`Complete` or `Fail` with its `TurnSettlement`, or the `TurnLedger` terminal tx; one `usage_event` set per turn).
6. Final frame publishes. It is never client-visible before step 5 exists; a crash between 5 and 6 replays the frame from the durable result.
7. Queue ack. Nothing is acked from cache state. A delivery refused with `ErrFenceLost` or `ErrTurnHeld` is acked,
   or dropped when its lease is already gone; it is never retried or dead-lettered.

## Turn fence

`conversation_turn.fence` is the epoch of the turn's owner. Every transition that changes who may write the turn
increments it in SQL, and every runtime write presents the `domain.TurnOwner{TenantID, RequestID, Fence}` its
acquisition or last transition returned. Once the fence moved on, the write is refused with `domain.ErrFenceLost`.
A missing or forged owner fails closed the same way in table, memory, and fake stores; the memory stores check
owners against `MemoryTurnFences`, and `dataplanetest.RunFenceSuite` is the conformance suite.

```mermaid
stateDiagram-v2
    [*] --> Q: Open (fence 0)
    Q --> R: Acquire (+1)
    R --> R: Acquire under the current lease after a crash (+1)
    R --> S: stream
    R --> U: Suspend (+1)
    U --> Q: Resume (+1)
    Q --> H: RequestHold (+1)
    U --> H: RequestHold (+1)
    R --> H: Hold at a checkpoint or on an unknown outcome (+1)
    R --> H: Preempt (+1)
    Q --> H: Preempt (+1)
    U --> H: Preempt (+1)
    H --> Q: Release at the held fence, no open outcome (+1, requeue)
    H --> Q: cancel wake (+1, requeue)
    U --> Q: cancel wake (+1, requeue)
    R --> C: Complete (fenced)
    R --> F: Fail (fenced)
    R --> X: Fail as cancelled (fenced)
    Q --> F: FailParked
    U --> F: FailParked
```

`S` (streaming) takes every transition `R` does.

- **Acquire.** `TurnRecordStore.Acquire(claim)` moves a `Q` turn, or an `R`/`S` turn redelivered after a crash, to
  `R` with the fence incremented, in one statement whose `EXISTS` checks that the claim's queue id, lease token, and
  worker id are the current lease of the turn's `turn_queue` row (`status_code = 'L'`,
  `lease_until > CURRENT_TIMESTAMP`). A refused claim reads as: not the current lease `ErrFenceLost`, an `H` or `U`
  turn `ErrTurnHeld`, an ended one `ErrConflict`.
- **Guarded touch.** A write that is not itself a transition of `conversation_turn` opens with
  `UPDATE conversation_turn SET fence = fence WHERE partner_id = ? AND request_id = ? AND fence = ? AND status_code IN ('R','S') RETURNING turn_no`,
  as the first CTE of one statement or the first statement of its transaction. The implicit row lock of that
  conditional write serializes the owner's write against a hold or reacquisition of the same row, and PostgreSQL
  re-evaluates `fence = ?` against the committed row, so each write lands entirely before or after the transition.
  A single-statement write reports two counts: zero owner rows is `ErrFenceLost`, an owned write that changed
  nothing is a key conflict.
- **Fenced writes.** The loop journal append, every step idempotency transition, and the owner's call-outcome
  intent, redispatch, acceptance, and unknown marking use the single-statement form; `Checkpoint`, `Complete`,
  `Fail`, and `SettleAndAppend` open their transaction with the touch (the turn settlement before its tenant
  advisory lock); `Complete`, `Suspend`, `Fail`, and `Hold` also carry `AND fence = ?` in their own update. A refused write leaves its uploaded object to the reconciliation sweep,
  since the current owner may reference the same content-addressed object.
- **On `ErrFenceLost`** the runtime stops the delivery with no terminal record, settlement, terminal audit, or final
  frame: the turn belongs to whoever moved the fence.
- **Conversation order.** Both queue claim paths and `qSessionActiveTurn` block a turn behind every earlier turn of
  its conversation, by `turn_no`, that is `Q`, `R`, `S`, `U`, or `H`, so a later turn never runs while an earlier
  one is parked or held, and `Complete` refuses a turn that is not the oldest live one. Queue timestamps do not
  order a conversation.
- **Hold.** `TableTurnControl.RequestHold` holds a `Q` or `U` turn at once (`H`, fence +1, `hold_code`, `held_at`)
  and asks an `R`/`S` turn to hold by setting `hold_requested_at` and `hold_reason`, the first request winning; a
  cancelled turn is not held. The running worker honors the request at its next durable boundary — after a step's
  checkpoint and frame when another step follows, before each loop iteration after the first, and before each
  non-read-only tool call once its decision is journaled — abandoning the open step claim under its fence, then
  `TurnRecordStore.Hold` moves the turn to `H` with `hold_code` `C`, clears the request, and keeps the budget
  reservation. The worker holds the same way with `hold_code` `U` when a call's outcome is unknown
  ([call outcomes](#call-outcomes)).
- **Preempt.** `TableTurnControl.Preempt` moves a live, uncancelled `Q`, `U`, `R`, or `S` turn to `H` at once
  (fence +1, `hold_code` `P`, pending hold request cleared) in one statement and wakes the watcher through the
  cache, so a running worker's next fenced write is `ErrFenceLost`. A cancellation requested first wins; an
  already held turn reports held. A hold or suspension yields to a cancellation requested before it: the transition is refused with
  `ErrTurnCanceled` and the worker ends the turn `X` instead of parking it out of the canceller's reach. A delivery
  that finds its turn `H` or `U` is acknowledged without running.
- **Suspend and resume.** `Suspend` moves the owner's turn to `U` (fence +1) and keeps the reservation;
  `Resume` moves `U` to `Q` (fence +1), is a no-op on `Q`, and returns `ErrTurnHeld` for a held turn. `FailParked`
  fails a `Q` or `U` turn no worker runs; owner `Fail` applies to `R`/`S` only.
- **Release.** `TableTurnControl.Release` moves `H` to `Q` only at the expected held fence and only while no call
  of the turn has an open outcome (fence +1), clears every hold column, and requeues the queue row in the same
  transaction; any other state or fence, or an open outcome, is `ErrConflict`.
  Cancelling a `U` or `H` turn wakes it to `Q` the same way, and the worker that picks it up ends it `X`.
  `qQueueRequeue` requeues only a `Q` turn's row, including one still leased by a worker that found the turn
  parked.
- **Watcher.** `TableTurnControl.Watch(owner)` derives the running turn's context. One poll every
  `agent_cancel_poll_ms`, woken early by the cache, reads `cancel_requested_at`, `hold_requested_at`, `fence`, and
  `status_code`: a cancellation fails the context with `ErrTurnCanceled`, a fence other than the owner's or a status
  other than `R`/`S` with `ErrFenceLost`, and a hold request only sets the flag `TurnWatch.HoldRequested` reports.
  `agent_cancel_max_read_failures` consecutive failed reads stop the turn. `MemoryTurnControl` has no fence
  authority and never cancels on a fence move; the memory stores refuse the stale owner's writes instead.

The CHECKs on `conversation_turn` keep the hold columns consistent: `fence >= 0`, the request columns both set or
both null, `hold_code` in `C`, `A`, `P`, or `U`, and `hold_code` and `held_at` set exactly while the status is `H`.
[Turn takeover](turn_takeover.md) is the accepted design for what remains — takeover and handback — and lists
the open findings in its §1.5.

## Idempotency state machine

```
(none) --Begin--> C claimed --Commit--> M committed   (terminal; Begin replays the stored result)
C claimed --Abandon--> A abandoned --Begin--> C claimed   (the replay transition; A is reclaimable)
C claimed --lease expired (updated_at + ClaimLease <= now)--Begin--> C claimed (CAS re-lease)
C claimed (live) --Begin--> ErrConflict
```
`Commit` on a lost claim is `ErrConflict` unless the row is already `M` (committed) with the same digest (duplicate
delivery → no-op). `Abandon` twice is a no-op; `Abandon` after `M` is `ErrConflict`. Every transition is behind the
owner's guarded touch, so a worker whose turn fence moved claims, commits, and abandons nothing. The row itself has
no fence column and its claim lease runs on the injected clock: a new owner that reaches a claim younger than
`ClaimLease` gets `ErrConflict` until the claim expires.

## Tool-loop journal

An `L` (tool loop) step is one row of `step_idempotency`, but it makes several decisions and effects before it
commits. `step_loop_entry` records them one entry at a time, keyed `(tenant, request, execution_step_id, entry_no)`
and numbered from 1 without gaps; the body is an object, the row keeps URI and digest.

```
model decision  --Append--> entry n        (the proposed calls are now fixed)
tool call k     --Invoke--> --Append--> entry n+k   (the observation is now fixed)
pending approval            --Append--> approval_pending entry, step returns ErrApprovalPending
```
`Append` is first-writer-wins on the primary key, behind the owner's guarded touch, so a fenced-out worker
journals nothing: a concurrent worker gets the stored entry back and deletes its own upload; if the stored content differs from what it offered, the step fails with `ErrConflict` rather than
continuing on a decision it did not make. On redelivery or approval resume the executor reads the journal and consumes it in order — a
journaled decision is never re-asked, a journaled observation is never re-invoked — and appends only past its
end. A crash between a read-only tool's answer and its entry repeats that call, under the same
`ToolCall.IdempotencyKey` (`loop:` + SHA-256 of tenant, request, execution step, and call id). A non-read-only call
is never repeated that way: its outcome row decides ([call outcomes](#call-outcomes)). The step result, not the
journal, remains what `step_idempotency` replays once the step commits.

## Call outcomes

`turn_call_outcome` records every non-read-only call of the tool loop, keyed by the call. The row is written before
the call leaves, so a call with no row never left, and its status moves only toward resolution: once `C` or `F`
it never changes again.

```mermaid
stateDiagram-v2
    [*] --> I: Intend (fenced, before the call leaves)
    I --> [*]: Retract (never left, by dispatch fence)
    I --> A: Accept (validated ToolAcceptance)
    I --> C: SettleAndAppend / Report / reconcile
    I --> F: SettleAndAppend / Report / reconcile
    I --> U: MarkUnknown, or cancel
    A --> U: MarkUnknown past its deadline, or cancel
    A --> C: lookup / Report / confirmation
    A --> F: lookup / Report / confirmation
    U --> I: Redispatch (declared deduplication)
    A --> I: Redispatch
    U --> C: lookup / attestation / Report / confirmation
    U --> F: lookup / attestation / Report / confirmation
```

- **Intent.** `Intend` is the fenced CTE: the owner's guarded touch, then an insert of `I` with `dispatch_fence` =
  the owner's fence, `dispatched_at = CURRENT_TIMESTAMP`, and `deadline_at = CURRENT_TIMESTAMP` plus the dispatch
  window (tool timeout × attempts + `agent_outcome_attest_grace`), `ON CONFLICT DO NOTHING`. An idempotency key
  already naming another call is `ErrConflict` (`turn_call_outcome_key_uq`).
- **Owner transitions.** `Redispatch` (open → `I`, new fence and deadline), `Accept` (`I` → `A` at the owner's
  dispatch fence; `deadline_at = GREATEST(deadline_at, now + LookupWithin + grace)`), and `MarkUnknown` (`I`/`A` →
  `U`) are fenced CTEs. `SettleAndAppend` opens its transaction with the touch, settles the open row with resolution
  and `resolved_fence`, and appends the observation; a repeat of the same settlement is accepted, any other is
  `ErrConflict`.
- **Late report.** A worker refused with `ErrFenceLost` reports its answer with one
  `UPDATE ... SET resolution_code = 'W', resolved_fence = dispatch_fence WHERE status_code IN ('I','A','U') AND dispatch_fence = ?`.
  It is evidence, not turn progress. `Retract` deletes only an `I` row at the worker's dispatch fence that carries
  no attestation.
- **Outcome-aware resume.** The next owner reads the row of an unobserved call at the journal cursor: a settled
  row's observation is journaled from its result object with `LoopEntry.Outcome`; an open row is redispatched
  under declared deduplication, or reconciled, or left `U` with the turn held again.
- **By a person.** `Resolve` (lookup, `E`/`L`) and `Attest` (`T`) act only while the turn is held or ended at the
  fence the person saw, a predicate inside the same statement; attestation also only once
  `deadline_at <= CURRENT_TIMESTAMP`. Each person's statement is a `turn_call_attestation` row, and the row settles
  when the required number of distinct people agree. The confirmation hook settles linked open rows `M` through
  `turn_call_confirmation`, scoped by tenant.
- **Cancel** moves the turn's `I` and `A` rows to `U` in the cancelling transaction.
- **Release gate.** The release `UPDATE` carries
  `AND NOT EXISTS (SELECT 1 FROM turn_call_outcome o WHERE ... AND o.status_code IN ('I','A','U'))`. After a hold no
  fenced writer adds a row, and rows only settle or are retracted, so the gate needs no lock.

Every statement is keyed by `partner_id` and uses the store clock; there is no `FOR UPDATE` and no advisory lock.

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
