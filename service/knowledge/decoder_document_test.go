package knowledge

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nauticana/keel/extract"

	"github.com/nauticana/scout/domain"
)

const mediaDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

type stubExtractor struct {
	extracted extract.Extracted
	err       error
}

func (stubExtractor) Supports(mediaType string) bool { return mediaType == "application/pdf" }

func (s stubExtractor) Extract(context.Context, string, []byte) (extract.Extracted, error) {
	return s.extracted, s.err
}

func decodeDocument(extractor extract.TextExtractor, mediaType string, raw []byte) (domain.DecodedDocument, error) {
	return DocumentDecoder{Extractor: extractor}.Decode(context.Background(), domain.KnowledgeDocument{DocumentID: "d", MediaType: mediaType}, raw)
}

func sectionText(decoded domain.DecodedDocument, i int) string {
	s := decoded.Sections[i]
	return string(decoded.Text[s.StartOffset:s.EndOffset])
}

func TestDocumentDecoderMapsExtractedSections(t *testing.T) {
	text := "Intro\n  page one  \f\f"
	decoded, err := decodeDocument(stubExtractor{extracted: extract.Extracted{Text: text, Sections: []extract.Section{
		{Kind: extract.Heading, Level: 2, Start: 0, End: 5},
		{Kind: extract.Page, Page: 1, Start: 6, End: 18},
		{Kind: extract.Page, Page: 2, Start: 19, End: 19},
	}}}, "application/pdf", []byte("%PDF"))
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Sections) != 2 {
		t.Fatalf("a page without a text layer must be dropped: %+v", decoded.Sections)
	}
	if s := decoded.Sections[0]; s.Kind != SectionHeading || s.Title != "Intro" || s.Depth != 2 {
		t.Errorf("heading = %+v", s)
	}
	if s := decoded.Sections[1]; s.Kind != SectionPage || sectionText(decoded, 1) != "page one" {
		t.Errorf("page = %+v %q", s, sectionText(decoded, 1))
	}
}

func TestDocumentDecoderRefusals(t *testing.T) {
	scanned := stubExtractor{extracted: extract.Extracted{Text: "\f", Sections: []extract.Section{{Kind: extract.Page, Page: 1}}}}
	if _, err := decodeDocument(scanned, "application/pdf", nil); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("no extractable text = %v", err)
	}
	for _, cause := range []error{extract.ErrTooLarge, extract.ErrEncrypted, errors.New("extract pdf: malformed document")} {
		if _, err := decodeDocument(stubExtractor{err: cause}, "application/pdf", nil); !errors.Is(err, domain.ErrValidation) || !errors.Is(err, cause) {
			t.Errorf("%v = %v", cause, err)
		}
	}
	if _, err := decodeDocument(stubExtractor{}, "image/png", nil); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("unsupported = %v", err)
	}
	if _, err := decodeDocument(nil, "application/pdf", nil); err == nil || errors.Is(err, domain.ErrValidation) {
		t.Errorf("a missing extractor is a configuration error, not a document error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := DocumentDecoder{Extractor: stubExtractor{err: context.Canceled}}.Decode(ctx, domain.KnowledgeDocument{MediaType: "application/pdf"}, nil)
	if !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrValidation) {
		t.Errorf("cancellation must stay systemic: %v", err)
	}
}

func TestDocumentDecoderKeepsTextSourceOffsets(t *testing.T) {
	decoded, err := decodeDocument(nil, "text/markdown", []byte("intro\r\n\r\n# Title\r\nbody"))
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded.Text) != "intro\r\n\r\n# Title\r\nbody" || decoded.Sections[1].Title != "Title" {
		t.Errorf("markdown must go through PlainTextDecoder: %+v", decoded)
	}
}

func TestDocumentDecoderReadsDOCX(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	fmt.Fprint(w, `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`+
		`<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Payment terms</w:t></w:r></w:p>`+
		`<w:p><w:r><w:t>Net 30 days.</w:t></w:r></w:p></w:body></w:document>`)
	zw.Close()
	decoded, err := decodeDocument(extract.Native{MaxBytes: 1 << 20}, mediaDOCX, buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Sections) != 2 || decoded.Sections[0].Title != "Payment terms" || decoded.Sections[0].Depth != 1 || sectionText(decoded, 1) != "Net 30 days." {
		t.Fatalf("docx sections = %+v in %q", decoded.Sections, decoded.Text)
	}
	chunks, err := (&SectionChunker{MaxTokens: 64, Overlap: 8}).Chunk(context.Background(), domain.KnowledgeDocument{}, decoded)
	if err != nil || len(chunks) == 0 {
		t.Fatalf("docx must chunk: %d %v", len(chunks), err)
	}
}
