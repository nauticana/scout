package knowledge

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Section kinds produced by the reference decoders.
const (
	SectionParagraph = "paragraph"
	SectionHeading   = "heading"
)

// PlainTextDecoder decodes text/plain into paragraph sections and
// text/markdown into heading-delimited sections; product-specific decoders
// (SAP documents, tables, binaries) are downstream MediaDecoder implementations.
type PlainTextDecoder struct{}

var _ contract.MediaDecoder = (*PlainTextDecoder)(nil)

// Decode keeps Text byte-identical to the source so section offsets are source offsets.
func (PlainTextDecoder) Decode(_ context.Context, document domain.KnowledgeDocument, raw []byte) (domain.DecodedDocument, error) {
	if !utf8.Valid(raw) {
		return domain.DecodedDocument{}, fmt.Errorf("%w: document %q is not valid UTF-8", domain.ErrValidation, document.DocumentID)
	}
	mediaType, _, _ := strings.Cut(document.MediaType, ";")
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "text/plain", "":
		return domain.DecodedDocument{Text: raw, Sections: paragraphSections(raw)}, nil
	case "text/markdown":
		return domain.DecodedDocument{Text: raw, Sections: markdownSections(raw)}, nil
	}
	return domain.DecodedDocument{}, fmt.Errorf("%w: media type %q is not supported by the plain text decoder", domain.ErrValidation, document.MediaType)
}

// paragraphSections splits on blank lines; whitespace-only stretches produce no section.
func paragraphSections(text []byte) []domain.DocumentSection {
	var sections []domain.DocumentSection
	offset := 0
	for offset < len(text) {
		next := bytes.Index(text[offset:], []byte("\n\n"))
		end := len(text)
		if next >= 0 {
			end = offset + next
		}
		if start, stop, ok := trimmedSpan(text, offset, end); ok {
			sections = append(sections, domain.DocumentSection{Kind: SectionParagraph, StartOffset: start, EndOffset: stop})
		}
		if next < 0 {
			break
		}
		offset = end + 2
	}
	return sections
}

// markdownSections opens a section at every ATX heading outside fenced code; text before the first heading is a paragraph.
func markdownSections(text []byte) []domain.DocumentSection {
	var sections []domain.DocumentSection
	current := domain.DocumentSection{Kind: SectionParagraph}
	inFence := false
	offset := 0
	for offset <= len(text) {
		lineEnd := len(text)
		if next := bytes.IndexByte(text[offset:], '\n'); next >= 0 {
			lineEnd = offset + next
		}
		line := text[offset:lineEnd]
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("```")) || bytes.HasPrefix(trimmed, []byte("~~~")) {
			inFence = !inFence
		} else if !inFence {
			if depth, title, ok := headingLine(trimmed); ok {
				sections = appendSection(sections, text, current, offset)
				current = domain.DocumentSection{Kind: SectionHeading, Title: title, StartOffset: offset, Depth: depth}
			}
		}
		if lineEnd == len(text) {
			break
		}
		offset = lineEnd + 1
	}
	return appendSection(sections, text, current, len(text))
}

func headingLine(line []byte) (depth int, title string, ok bool) {
	for depth < len(line) && line[depth] == '#' {
		depth++
	}
	if depth == 0 || depth > 6 || depth == len(line) || line[depth] != ' ' {
		return 0, "", false
	}
	return depth, strings.TrimSpace(strings.TrimRight(string(line[depth:]), "#")), true
}

func appendSection(sections []domain.DocumentSection, text []byte, section domain.DocumentSection, end int) []domain.DocumentSection {
	start, stop, ok := trimmedSpan(text, section.StartOffset, end)
	if !ok {
		return sections
	}
	section.StartOffset, section.EndOffset = start, stop
	return append(sections, section)
}

// trimmedSpan shrinks [start,end) past surrounding whitespace; ok is false for an empty span.
func trimmedSpan(text []byte, start, end int) (int, int, bool) {
	for start < end && isSpace(text[start]) {
		start++
	}
	for end > start && isSpace(text[end-1]) {
		end--
	}
	return start, end, start < end
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
