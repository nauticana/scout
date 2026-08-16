package knowledge

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// SectionChunker packs whole sections into token windows and splits an
// oversized section on paragraph, line, then rune boundaries; every window
// after the first repeats Overlap tokens of context. Same input, same
// version, same chunks.
type SectionChunker struct {
	// MaxTokens bounds one chunk; default 512.
	MaxTokens int
	// Overlap is the context carried into the next window of the same section; default 64, must be below MaxTokens.
	Overlap int
	// Tokens estimates the token count of a span; nil uses ceil(bytes/4).
	Tokens func([]byte) int
	// AlgorithmVersion is stamped into every chunk id; default "section/v1".
	AlgorithmVersion string
}

var _ contract.Chunker = (*SectionChunker)(nil)

const defaultChunkerVersion = "section/v1"

// Version returns the algorithm version pinned into chunk identity.
func (chunker *SectionChunker) Version() string {
	if chunker.AlgorithmVersion == "" {
		return defaultChunkerVersion
	}
	return chunker.AlgorithmVersion
}

func (chunker *SectionChunker) limits() (maxTokens, overlap int, err error) {
	maxTokens, overlap = chunker.MaxTokens, chunker.Overlap
	if maxTokens == 0 {
		maxTokens = 512
	}
	if overlap == 0 && chunker.MaxTokens == 0 {
		overlap = 64
	}
	if maxTokens < 0 || overlap < 0 || overlap >= maxTokens {
		return 0, 0, fmt.Errorf("%w: chunker needs positive max tokens and overlap below it", domain.ErrValidation)
	}
	return maxTokens, overlap, nil
}

func (chunker *SectionChunker) tokens(span []byte) int {
	if chunker.Tokens != nil {
		if count := chunker.Tokens(span); count > 0 {
			return count
		}
		return 1
	}
	return (len(span) + 3) / 4
}

type span struct{ start, end int }

// Chunk emits chunks with source offsets, content, and token counts; identity fields are stamped by the pipeline.
func (chunker *SectionChunker) Chunk(ctx context.Context, _ domain.KnowledgeDocument, decoded domain.DecodedDocument) ([]domain.KnowledgeChunk, error) {
	maxTokens, overlap, err := chunker.limits()
	if err != nil {
		return nil, err
	}
	sections := append([]domain.DocumentSection(nil), decoded.Sections...)
	if len(sections) == 0 {
		sections = []domain.DocumentSection{{Kind: SectionParagraph, StartOffset: 0, EndOffset: len(decoded.Text)}}
	}
	sort.SliceStable(sections, func(i, j int) bool { return sections[i].StartOffset < sections[j].StartOffset })
	var chunks []domain.KnowledgeChunk
	for _, section := range sections {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if section.StartOffset < 0 || section.EndOffset > len(decoded.Text) || section.StartOffset >= section.EndOffset {
			return nil, fmt.Errorf("%w: section offsets [%d,%d) are outside the text", domain.ErrValidation, section.StartOffset, section.EndOffset)
		}
		for _, window := range chunker.windows(decoded.Text, section, maxTokens, overlap) {
			content := decoded.Text[window.start:window.end]
			chunks = append(chunks, domain.KnowledgeChunk{
				ChunkNo: len(chunks), StartOffset: window.start, EndOffset: window.end,
				Content: append([]byte(nil), content...), TokenCount: chunker.tokens(content),
			})
		}
	}
	return chunks, nil
}

// windows packs units greedily; the tail of each full window seeds the next one.
func (chunker *SectionChunker) windows(text []byte, section domain.DocumentSection, maxTokens, overlap int) []span {
	units := chunker.units(text, span{section.StartOffset, section.EndOffset}, maxTokens, overlap)
	var windows []span
	var current []span
	currentTokens := 0
	for _, unit := range units {
		unitTokens := chunker.tokens(text[unit.start:unit.end])
		if len(current) > 0 && currentTokens+unitTokens > maxTokens {
			windows = append(windows, span{current[0].start, current[len(current)-1].end})
			current, currentTokens = chunker.overlapTail(text, current, overlap)
		}
		current = append(current, unit)
		currentTokens += unitTokens
	}
	if len(current) > 0 {
		windows = append(windows, span{current[0].start, current[len(current)-1].end})
	}
	return windows
}

func (chunker *SectionChunker) overlapTail(text []byte, current []span, overlap int) ([]span, int) {
	var tail []span
	tokens := 0
	for i := len(current) - 1; i >= 0 && overlap > 0; i-- {
		unitTokens := chunker.tokens(text[current[i].start:current[i].end])
		if tokens+unitTokens > overlap {
			break
		}
		tail = append([]span{current[i]}, tail...)
		tokens += unitTokens
	}
	// A carried tail must leave room for new content, otherwise the window never advances.
	if len(tail) == len(current) {
		return nil, 0
	}
	return tail, tokens
}

// units splits a section into paragraphs, oversized paragraphs into lines,
// oversized lines into words, and oversized words into rune-safe byte grains
// no larger than the overlap so a carried tail always exists.
func (chunker *SectionChunker) units(text []byte, section span, maxTokens, overlap int) []span {
	grain := overlap
	if grain <= 0 {
		grain = maxTokens
	}
	var units []span
	for _, paragraph := range splitSpans(text, section, []byte("\n\n")) {
		if chunker.tokens(text[paragraph.start:paragraph.end]) <= maxTokens {
			units = append(units, paragraph)
			continue
		}
		for _, line := range splitSpans(text, paragraph, []byte("\n")) {
			if chunker.tokens(text[line.start:line.end]) <= maxTokens {
				units = append(units, line)
				continue
			}
			for _, word := range wordSpans(text, line) {
				if chunker.tokens(text[word.start:word.end]) <= grain {
					units = append(units, word)
					continue
				}
				units = append(units, chunker.hardSplit(text, word, grain)...)
			}
		}
	}
	return units
}

func wordSpans(text []byte, within span) []span {
	var spans []span
	start := -1
	for i := within.start; i < within.end; i++ {
		switch {
		case isSpace(text[i]) && start >= 0:
			spans = append(spans, span{start, i})
			start = -1
		case !isSpace(text[i]) && start < 0:
			start = i
		}
	}
	if start >= 0 {
		spans = append(spans, span{start, within.end})
	}
	return spans
}

func splitSpans(text []byte, within span, separator []byte) []span {
	var spans []span
	offset := within.start
	for offset < within.end {
		next := bytes.Index(text[offset:within.end], separator)
		end := within.end
		if next >= 0 {
			end = offset + next
		}
		if start, stop, ok := trimmedSpan(text, offset, end); ok {
			spans = append(spans, span{start, stop})
		}
		if next < 0 {
			break
		}
		offset = end + len(separator)
	}
	return spans
}

// hardSplit cuts a unit into byte windows sized from its own bytes-per-token ratio, never inside a rune.
func (chunker *SectionChunker) hardSplit(text []byte, unit span, grainTokens int) []span {
	unitTokens := chunker.tokens(text[unit.start:unit.end])
	limit := (unit.end - unit.start) * grainTokens / unitTokens
	if limit < utf8.UTFMax {
		limit = utf8.UTFMax
	}
	var spans []span
	start := unit.start
	for start < unit.end {
		end := start + limit
		if end >= unit.end {
			spans = append(spans, span{start, unit.end})
			break
		}
		for !utf8.RuneStart(text[end]) {
			end--
		}
		spans = append(spans, span{start, end})
		start = end
	}
	return spans
}
