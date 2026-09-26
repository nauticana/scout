# Knowledge ingestion: pipeline, manifests, aliases, and GC

Reference implementations live in `service/knowledge/`: `IngestPipeline` (`ingest_pipeline.go`); the reference
ports `ObjectStorageLoader`, `PlainTextDecoder`, `SectionChunker`, `PolicyRedactor`, `ObjectChunkStore`; and the
versioning services `VersionPublisher`, `ManifestStore`, `VersionAliaser`, `TableSourceChangeSource`, `Reconciler`,
`GarbageCollector` — each in the file its name suggests.

## Stages and backpressure

`IngestBatch` is a bounded synchronous executor over three stages connected by buffered channels of
`QueueDepth` (0 = rendezvous): **prepare** (load → verify SHA-256 against `ContentDigest` → decode → chunk →
redact), **embed** (`EmbeddingGateway` — `knowledge.BatchingEmbedder` over
`modelgateway.GovernedEmbedder`, `EmbedFanOut` chunks of one document at a time), **publish**
(chunk store → relational transaction → vector index → manifest activation). `Embedder` and `Index` are set
together or not at all; without them a document is ingested for whole reads only. Each stage has its own worker
count; a slow index blocks publish, which fills the handoff channel and stops the loaders — real backpressure,
no unbounded queue. Every stage closes its output channel only after all of its workers have exited, so the
batch returns with no goroutine still running.

Failure classification decides the blast radius. A **systemic** failure — canceled/expired context, or an error
`IsSystemic` accepts (default `ErrCircuitOpen`, `ErrDegraded`) — cancels the batch; remaining items get a
non-terminal `batch canceled` failure and `IngestBatch` returns the error with the partial result. An
**isolated** document failure (digest mismatch, undecodable media, nil entitlements, conflicting republish)
records a terminal `IngestItemResult` and the batch continues. Items always come back in input order.

## Idempotency and publish ordering

The idempotency key is SHA-256 over tenant | knowledge base | knowledge version | document | content digest
(`IngestIdempotencyKey`). Prepare looks the document up in `knowledge_document` first: same digest → the item is
a replay (no load, embed, index, or insert; the existing chunk count is reported), different digest under the
same immutable version → terminal `ErrConflict`. Chunk identity is equally deterministic —
`ChunkID` = SHA-256 over tenant | knowledge base | document | source version | chunker version | chunk number —
so the same source re-chunked by the same `Chunker.Version()` produces the same ids and object keys.

Publish order is fixed: chunk content to object storage, then one transaction inserting `knowledge_document`
+ all `knowledge_chunk` rows, then `KnowledgeVectorIndex.Index` for the whole document, then manifest
activation. Rows come first because a vector row references its chunk; neither is visible to retrieval or a
whole read until the manifest activates the version. If indexing fails, the pipeline deletes the document's
rows again and joins any deletion error onto the original. `vector_ref` stays empty until indexing finishes
(a whole-only ingest fills it at insert), so a retry rebuilds rows a crash left behind instead of activating an
incomplete document, `VersionAliaser.Swap` never repoints a manifest at them, and a pipeline without an index
refuses them with `ErrNotReady`. What a crash leaves behind is caught by `Reconciler` (orphan chunks) and
reclaimed by `GarbageCollector`, whose `Index` is optional for the same reason.

## Reference ports

`ObjectStorageLoader` resolves `<scheme>://bucket/key` through a keel `storage.ObjectStorage` bound to that bucket, under a byte cap; a URI naming another bucket is `ErrForbidden`, so a source bucket and a chunk bucket each get their own loader.
`PlainTextDecoder` handles `text/plain` (paragraph sections) and `text/markdown` (heading sections, fenced code
respected) keeping `Text` byte-identical to the source so offsets stay source offsets. `SectionChunker` packs
whole sections into `MaxTokens` windows with `Overlap` carried into the next window, splitting an oversized
section on paragraph, line, word, then rune-safe byte boundaries, with an injectable token estimator and a
version stamped into every chunk id. `PolicyRedactor` masks `field: value` lines whose field a versioned
`RedactionPolicy` allowlist does not permit, and recomputes the chunk digest. Text extraction from PDF, DOCX and
scans has nothing agent-specific in it and is requested from keel ([todo_upstream_keel.md](../todo_upstream_keel.md));
Scout wraps it as a `MediaDecoder` once it ships. Product-specific decoders — SAP document/table extraction — stay
downstream.

## Versions, manifests, aliases, tombstones, GC

`VersionPublisher.EnsureVersion` creates `knowledge_base` (when missing) and `knowledge_base_version` under the
base's advisory lock before ingestion writes under them. It is a replay for an existing version with the same
embedding and `ErrConflict` for a different one; empty embedding fields mean a whole-read-only version. It is an
explicit publisher call, never a side effect of `IngestBatch`.

`ManifestStore` owns the per-document pointer in `knowledge_document_manifest`: build the new version fully,
then `Activate` switches `active_version` and marks the old one `superseded_version` + `gc_pending`.
Re-activating the same version and digest is a replay; a different digest, or a switch while a previous version
still awaits GC, is `ErrConflict` — no chunk set is ever orphaned untracked. `Tombstone` marks the document
deleted so retrieval excludes it immediately, long before its rows and vectors are reclaimed.

`VersionAliaser` is the knowledge-base generation pointer in `knowledge_base_alias`. A rechunk/re-embed builds a
new `knowledge_base_version` side by side; when it validates, `Swap(expected, new)` is a compare-and-set
(`ErrConflict` on a stale expectation) that also repoints, in the same transaction, every manifest whose
document is already published in the new generation. Retrieval reads `Active`, so a generation is either fully
in front of readers or not at all.

`GarbageCollector.Sweep(ctx, limit)` drains `gc_pending` manifests in bounded batches: `Index.Remove` per
reclaimable version first (idempotent), then, in one transaction under the manifest advisory lock and after
re-reading the manifest so a concurrent activation cannot lose a live chunk set, `Objects.DeleteChunks` per
version followed by the chunk/document/manifest deletes. `Objects` is required: `ObjectChunkStore.DeleteChunks`
lists and removes the document version's key prefix, so objects whose rows a crash already lost are reclaimed
too, and a failed object delete rolls the rows back for the next sweep. Per-document failures are joined and
reported; the sweep continues. Run it from a periodic worker.

## CDC contract and worker composition

Producers record source changes with `EnqueueSourceChange` inside their own transaction (merge
`SourceChangeWriteQueries()` into the transaction's query map) — the outbox guarantee: the source write and the
change event commit together. An event carries tenant, knowledge base, object id, source version, op
(`upsert`/`delete`), authorization attributes, and occurrence time. `TableSourceChangeSource.Poll` returns
unacked events in occurrence order and `Ack` marks them applied. `Reconciler.Reconcile` reports freshness lag
(age of the oldest unacked event), orphan chunks, and outstanding tombstones per knowledge base.

Scout ships no binary. A downstream ingestion worker is a keel `worker.JobWorker` composing these services:

```go
func (w *IngestWorker) ProcessQueue(ctx context.Context, journal logger.ApplicationLogger, db port.DatabaseRepository, quota port.QuotaService, qs port.QueryService) {
    events, err := w.Changes.Poll(ctx, w.TenantID, w.KnowledgeBaseID, w.BatchSize)
    if err != nil || len(events) == 0 {
        return
    }
    batch := domain.IngestBatch{TenantContext: w.Tenant, KnowledgeBaseID: w.KnowledgeBaseID, KnowledgeVersion: w.Version}
    for _, event := range events {
        if event.Op == domain.SourceUpserted {
            batch.Documents = append(batch.Documents, w.documentFor(event))
        }
    }
    result, err := w.Pipeline.IngestBatch(ctx, batch)   // systemic error → retry next tick, nothing acked
    if err != nil {
        journal.Error(ctx, "ingest batch", err)
        return
    }
    w.ackApplied(ctx, events, result)                   // ack deletes after Tombstone, upserts on success/terminal
}
```

Deletes call `ManifestStore.Tombstone` before acking; only successful or terminal items are acked, so a
transient failure is redelivered. `GarbageCollector.Sweep` and `Reconciler.Reconcile` belong on the same
worker's tick or a slower one, never on an HTTP path.
