package knowledge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nauticana/keel/extract"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// DocumentDecoder decodes text/plain and text/markdown through PlainTextDecoder,
// so their offsets stay source offsets, and every other media type the keel
// extractor supports (PDF and DOCX with extract.Native) through Extractor.
// Pages without a text layer contribute nothing until an OCR extractor is
// configured; a document with no extractable text is refused.
type DocumentDecoder struct {
	Extractor extract.TextExtractor
}

var _ contract.MediaDecoder = (*DocumentDecoder)(nil)

func (decoder DocumentDecoder) Decode(ctx context.Context, document domain.KnowledgeDocument, raw []byte) (domain.DecodedDocument, error) {
	mediaType, _, _ := strings.Cut(document.MediaType, ";")
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "", "text/plain", "text/markdown":
		return PlainTextDecoder{}.Decode(ctx, document, raw)
	}
	if decoder.Extractor == nil {
		return domain.DecodedDocument{}, errors.New("document decoder: Extractor is required")
	}
	if !decoder.Extractor.Supports(document.MediaType) {
		return domain.DecodedDocument{}, fmt.Errorf("%w: media type %q is not supported by the document decoder", domain.ErrValidation, document.MediaType)
	}
	extracted, err := decoder.Extractor.Extract(ctx, document.MediaType, raw)
	if err != nil {
		if ctx.Err() != nil {
			return domain.DecodedDocument{}, err
		}
		return domain.DecodedDocument{}, fmt.Errorf("%w: document %q: %w", domain.ErrValidation, document.DocumentID, err)
	}
	decoded := domain.DecodedDocument{Text: []byte(extracted.Text)}
	for _, section := range extracted.Sections {
		start, end, ok := trimmedSpan(decoded.Text, section.Start, section.End)
		if !ok {
			continue
		}
		out := domain.DocumentSection{Kind: section.Kind, StartOffset: start, EndOffset: end}
		if section.Kind == extract.Heading {
			out.Title, out.Depth = string(decoded.Text[start:end]), section.Level
		}
		decoded.Sections = append(decoded.Sections, out)
	}
	if len(decoded.Sections) == 0 {
		return domain.DecodedDocument{}, fmt.Errorf("%w: document %q has no extractable text", domain.ErrValidation, document.DocumentID)
	}
	return decoded, nil
}
