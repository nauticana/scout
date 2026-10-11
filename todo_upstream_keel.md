# keel upstream gaps

## Atomic compare-and-set in `cache.CacheService`

`CacheService` offers `Get`, `Set`, and increments but no conditional write. A fenced reply hub must advance a request's `(fence, sequence)` head atomically, refusing a lower fence, across processes. Add a neutral operation such as `CompareAndSet(ctx, key, expected, value string, ttl time.Duration) (bool, error)` (one round trip, a script on Redis). Until then, turn control composes only with an in-process reply hub or a product hub that provides it.

## OCR text extractor

`extract.TextExtractor` (`extract.Native`) reads PDF text layers and DOCX only, so a scanned page contributes its invisible OCR layer when it has one and nothing otherwise. Add a pluggable OCR extractor behind the same interface, run isolated like `extract.NewIsolated` with the same size, page, and deadline bounds. Scout's `knowledge.DocumentDecoder` adopts it without a contract change.
