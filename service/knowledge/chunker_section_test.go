package knowledge

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

func decodeText(t *testing.T, mediaType, text string) domain.DecodedDocument {
	t.Helper()
	decoded, err := PlainTextDecoder{}.Decode(context.Background(), domain.KnowledgeDocument{DocumentID: "d", MediaType: mediaType}, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestPlainTextDecoderSections(t *testing.T) {
	plain := decodeText(t, "text/plain; charset=utf-8", "first para\nstill first\n\n\n  second para  \n\n")
	if len(plain.Sections) != 2 || plain.Sections[0].Kind != SectionParagraph || string(plain.Text[plain.Sections[1].StartOffset:plain.Sections[1].EndOffset]) != "second para" {
		t.Fatalf("plain sections = %+v", plain.Sections)
	}
	markdown := decodeText(t, "text/markdown", "intro line\n\n# Title\nbody\n```\n# not a heading\n```\n## Sub ##\nmore\n")
	if len(markdown.Sections) != 3 {
		t.Fatalf("markdown sections = %+v", markdown.Sections)
	}
	if markdown.Sections[0].Kind != SectionParagraph || markdown.Sections[1].Title != "Title" || markdown.Sections[1].Depth != 1 || markdown.Sections[2].Title != "Sub" || markdown.Sections[2].Depth != 2 {
		t.Fatalf("markdown sections = %+v", markdown.Sections)
	}
	if !strings.Contains(string(markdown.Text[markdown.Sections[1].StartOffset:markdown.Sections[1].EndOffset]), "# not a heading") {
		t.Fatalf("fenced heading split a section: %+v", markdown.Sections[1])
	}
	if _, err := (PlainTextDecoder{}).Decode(context.Background(), domain.KnowledgeDocument{MediaType: "application/pdf"}, []byte("x")); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unsupported = %v", err)
	}
	if _, err := (PlainTextDecoder{}).Decode(context.Background(), domain.KnowledgeDocument{MediaType: "text/plain"}, []byte{0xff, 0xfe}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid utf8 = %v", err)
	}
}

func TestSectionChunkerIsDeterministicAndBounded(t *testing.T) {
	words := make([]string, 0, 300)
	for i := range 300 {
		words = append(words, "word"+string(rune('a'+i%26)))
	}
	text := "# Heading\n\n" + strings.Join(words[:100], " ") + "\n\n" + strings.Join(words[100:], " ")
	decoded := decodeText(t, "text/markdown", text)
	chunker := &SectionChunker{MaxTokens: 40, Overlap: 8, Tokens: func(span []byte) int { return len(bytes.Fields(span)) }}
	first, err := chunker.Chunk(context.Background(), domain.KnowledgeDocument{}, decoded)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := chunker.Chunk(context.Background(), domain.KnowledgeDocument{}, decoded)
	if len(first) < 5 || len(first) != len(second) {
		t.Fatalf("chunks = %d / %d", len(first), len(second))
	}
	for i, chunk := range first {
		if chunk.ChunkNo != i || chunk.TokenCount > 40 || chunk.TokenCount <= 0 || string(chunk.Content) != string(decoded.Text[chunk.StartOffset:chunk.EndOffset]) || string(second[i].Content) != string(chunk.Content) {
			t.Fatalf("chunk %d = %+v", i, chunk)
		}
		if i > 0 && chunk.StartOffset <= first[i-1].StartOffset {
			t.Fatalf("chunk %d does not advance: %d after %d", i, chunk.StartOffset, first[i-1].StartOffset)
		}
	}
	// Overlap: consecutive windows within the long paragraph share their boundary words.
	if !strings.HasPrefix(string(first[2].Content), lastWords(string(first[1].Content), 8)) {
		t.Fatalf("no overlap between %q and %q", first[1].Content, first[2].Content)
	}
}

func lastWords(text string, n int) string {
	fields := strings.Fields(text)
	return strings.Join(fields[len(fields)-n:], " ")
}

func TestSectionChunkerHardSplitsAndValidates(t *testing.T) {
	long := strings.Repeat("ünbroken", 200)
	chunker := &SectionChunker{MaxTokens: 50, Overlap: 5}
	chunks, err := chunker.Chunk(context.Background(), domain.KnowledgeDocument{}, domain.DecodedDocument{Text: []byte(long)})
	if err != nil || len(chunks) < 2 {
		t.Fatalf("chunks = %d, %v", len(chunks), err)
	}
	covered := 0
	for _, chunk := range chunks {
		if !bytes.Equal(chunk.Content, []byte(long)[chunk.StartOffset:chunk.EndOffset]) || chunk.TokenCount > 50 || !utf8Valid(chunk.Content) || chunk.StartOffset > covered {
			t.Fatalf("chunk = %+v", chunk)
		}
		covered = chunk.EndOffset
	}
	if covered != len(long) {
		t.Fatalf("hard split lost bytes: covered %d of %d", covered, len(long))
	}
	if _, err := (&SectionChunker{MaxTokens: 10, Overlap: 10}).Chunk(context.Background(), domain.KnowledgeDocument{}, domain.DecodedDocument{Text: []byte("x")}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("overlap >= max = %v", err)
	}
	if _, err := chunker.Chunk(context.Background(), domain.KnowledgeDocument{}, domain.DecodedDocument{Text: []byte("x"), Sections: []domain.DocumentSection{{StartOffset: 0, EndOffset: 5}}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad offsets = %v", err)
	}
	if (&SectionChunker{}).Version() != defaultChunkerVersion || (&SectionChunker{AlgorithmVersion: "x/v2"}).Version() != "x/v2" {
		t.Fatal("version")
	}
}

func utf8Valid(content []byte) bool {
	return strings.ToValidUTF8(string(content), "�") == string(content)
}

func TestPolicyRedactorMasksDisallowedFields(t *testing.T) {
	redactor := &PolicyRedactor{Policies: []RedactionPolicy{&AllowlistPolicy{PolicyVersion: "p1", Fields: []string{"Name", "city"}}}, Mask: "***"}
	chunk := domain.KnowledgeChunk{Content: []byte("name: Ada\nsalary: 100\nfree text: with colon later\nnote without value:\ncity: Paris"), RedactionPolicyVersion: "p1"}
	redacted, err := redactor.Redact(context.Background(), chunk)
	if err != nil {
		t.Fatal(err)
	}
	if string(redacted.Content) != "name: Ada\nsalary: ***\nfree text: with colon later\nnote without value:\ncity: Paris" || redacted.ContentDigest != sha256Bytes(redacted.Content) {
		t.Fatalf("redacted = %q", redacted.Content)
	}
	passthrough, err := redactor.Redact(context.Background(), domain.KnowledgeChunk{Content: []byte("salary: 100")})
	if err != nil || string(passthrough.Content) != "salary: 100" {
		t.Fatalf("passthrough = %q, %v", passthrough.Content, err)
	}
	if _, err := redactor.Redact(context.Background(), domain.KnowledgeChunk{Content: []byte("x"), RedactionPolicyVersion: "unknown"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown policy = %v", err)
	}
}
