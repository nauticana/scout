# Upstream to keel — shared backend work

| ID | Pri | Gap | Proposed API | Blocks |
|---|---:|---|---|---|
| SK-K1 | P3 | text extraction from PDF, DOCX and scanned documents is horizontal (nothing agent-specific), yet every consumer would pull its own parser stack | a `document`-package extractor: `document.TextExtractor` with `Extract(ctx, mediaType string, raw []byte) (Extracted, error)` returning UTF-8 text plus section offsets (heading, paragraph, table, page), selected per media type through a factory and `--extract_mode` flag (native Go for PDF/DOCX, an OCR provider behind the same interface) | `contract.MediaDecoder` implementations for `application/pdf` and DOCX in `service/knowledge`, wrapping the extractor; until then downstreams decode only `text/plain` and `text/markdown` |
