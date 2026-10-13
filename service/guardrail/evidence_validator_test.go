package guardrail

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

type digestVerifier struct{ trusted string }

func (verifier digestVerifier) VerifyObject(_ context.Context, _ int64, object domain.ObjectRef) error {
	if object.Digest != verifier.trusted {
		return errors.New("digest mismatch")
	}
	return nil
}

func TestEvidenceValidatorNamesTheUnsupportedClaimAndKeepsTheRest(t *testing.T) {
	digest := strings.Repeat("a", 64)
	validator := &EvidenceValidator{Objects: digestVerifier{trusted: digest}}
	scope := domain.EvidenceScope{TenantID: 1, ToolResults: []domain.ToolEvidence{{CallID: "call-1", ResourceURIs: []string{"mcp://catalog/item/9"}}}}
	answer := domain.EvidencedAnswer{
		Answer: "four claims",
		Evidence: []domain.EvidenceRef{
			{ID: "doc", Source: domain.EvidenceObject, Object: domain.ObjectRef{URI: "s3://kb/doc", Digest: digest}},
			{ID: "tampered", Source: domain.EvidenceObject, Object: domain.ObjectRef{URI: "s3://kb/other", Digest: strings.Repeat("b", 64)}},
			{ID: "tool", Source: domain.EvidenceToolResult, CallID: "call-1"},
			{ID: "link", Source: domain.EvidenceMCPResource, CallID: "call-1", URI: "mcp://catalog/item/9"},
			{ID: "web", Source: "url", URI: "https://example.com/made-up"},
			{ID: "stray-link", Source: domain.EvidenceMCPResource, CallID: "call-1", URI: "https://example.com/made-up"},
		},
		Claims: []domain.Claim{
			{Text: "from a verified document", EvidenceIDs: []string{"doc"}},
			{Text: "from this turn's tool and its link", EvidenceIDs: []string{"tool", "link"}},
			{Text: "from a model-supplied URL", EvidenceIDs: []string{"web"}},
			{Text: "from a link no tool returned", EvidenceIDs: []string{"stray-link"}},
			{Text: "from a tampered object", EvidenceIDs: []string{"doc", "tampered"}},
			{Text: "from nothing", EvidenceIDs: nil},
			{Text: "from evidence that is not there", EvidenceIDs: []string{"ghost"}},
		},
	}
	report, err := validator.Validate(context.Background(), scope, answer)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(report.Supported) != 2 || report.Supported[0].Text != "from a verified document" {
		t.Fatalf("supported = %+v", report.Supported)
	}
	var indexes []int
	for _, finding := range report.Unsupported {
		indexes = append(indexes, finding.ClaimIndex)
		if finding.Reason == "" || finding.Claim == "" {
			t.Fatalf("finding must name the claim and the reason: %+v", finding)
		}
	}
	if len(indexes) != 5 || indexes[0] != 2 || indexes[4] != 6 {
		t.Fatalf("unsupported claim indexes = %v, want 2..6", indexes)
	}
}

func TestEvidenceValidatorFailsClosedWithoutAnObjectVerifier(t *testing.T) {
	answer := domain.EvidencedAnswer{Evidence: []domain.EvidenceRef{
		{ID: "doc", Source: domain.EvidenceObject, Object: domain.ObjectRef{URI: "s3://kb/doc", Digest: strings.Repeat("a", 64)}},
	}}
	if _, err := (&EvidenceValidator{}).Validate(context.Background(), domain.EvidenceScope{TenantID: 1}, answer); !errors.Is(err, domain.ErrDegraded) {
		t.Fatalf("want ErrDegraded, got %v", err)
	}
}

func TestEvidenceValidatorRejectsAmbiguousDuplicateEvidenceIDs(t *testing.T) {
	answer := domain.EvidencedAnswer{
		Evidence: []domain.EvidenceRef{
			{ID: "same", Source: domain.EvidenceToolResult, CallID: "call-1"},
			{ID: "same", Source: domain.EvidenceToolResult, CallID: "call-2"},
		},
		Claims: []domain.Claim{{Text: "ambiguous", EvidenceIDs: []string{"same"}}},
	}
	report, err := (&EvidenceValidator{}).Validate(context.Background(), domain.EvidenceScope{
		TenantID: 1, ToolResults: []domain.ToolEvidence{{CallID: "call-1"}, {CallID: "call-2"}},
	}, answer)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(report.Unsupported) != 1 || !strings.Contains(report.Unsupported[0].Reason, "duplicated") {
		t.Fatalf("report = %+v", report)
	}
}

type factTable []domain.VerifiedFact

func (facts factTable) VerifiedFacts(context.Context, int64) ([]domain.VerifiedFact, error) {
	return facts, nil
}

type spanDetector []domain.DetectedClaim

func (spans spanDetector) Detect(context.Context, int64, string) ([]domain.DetectedClaim, error) {
	return spans, nil
}

func spans(texts ...string) spanDetector {
	detected := make(spanDetector, 0, len(texts))
	for _, text := range texts {
		detected = append(detected, domain.DetectedClaim{Text: text})
	}
	return detected
}

func TestEvidenceValidatorChecksFactsAndUndeclaredClaims(t *testing.T) {
	validator := &EvidenceValidator{Facts: factTable{{Key: "price", Value: "The kit costs $49 and ships free"}}, Claims: spans("$49", "10-year warranty")}
	answer := domain.EvidencedAnswer{
		Answer:   "Only $49, with a 10-year warranty.",
		Evidence: []domain.EvidenceRef{{ID: "f", Source: domain.EvidenceFact, FactKey: "price"}, {ID: "x", Source: domain.EvidenceFact, FactKey: "warranty"}},
		Claims:   []domain.Claim{{Text: "costs $49", EvidenceIDs: []string{"f"}}, {Text: "10-year warranty", EvidenceIDs: []string{"x"}}},
	}
	report, err := validator.Validate(context.Background(), domain.EvidenceScope{TenantID: 1}, answer)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Supported) != 1 || report.Supported[0].Text != "costs $49" {
		t.Fatalf("supported = %+v", report.Supported)
	}
	// The unverified fact fails its declared claim, and the detector catches the same statement in the text.
	if len(report.Unsupported) != 2 || report.Unsupported[0].ClaimIndex != 1 || report.Unsupported[1].ClaimIndex != -1 || report.Unsupported[1].Claim != "10-year warranty" {
		t.Fatalf("unsupported = %+v", report.Unsupported)
	}

	// A supported claim citing a fact that does not state the detected value does not cover it.
	answer = domain.EvidencedAnswer{
		Answer:   "Only $39.",
		Evidence: []domain.EvidenceRef{{ID: "f", Source: domain.EvidenceFact, FactKey: "price"}},
		Claims:   []domain.Claim{{Text: "only $39", EvidenceIDs: []string{"f"}}},
	}
	validator.Claims = spans("$39")
	if report, err = validator.Validate(context.Background(), domain.EvidenceScope{TenantID: 1}, answer); err != nil || len(report.Unsupported) != 1 || report.Unsupported[0].ClaimIndex != -1 {
		t.Fatalf("report = %+v, err = %v", report, err)
	}

	// An undeclared claim fails even when the answer declares none.
	if report, err = validator.Validate(context.Background(), domain.EvidenceScope{TenantID: 1}, domain.EvidencedAnswer{Answer: "Only $39."}); err != nil || len(report.Unsupported) != 1 {
		t.Fatalf("report = %+v, err = %v", report, err)
	}

	unsourced := &EvidenceValidator{}
	fact := domain.EvidencedAnswer{Evidence: []domain.EvidenceRef{{ID: "f", Source: domain.EvidenceFact, FactKey: "price"}}, Claims: []domain.Claim{{Text: "x", EvidenceIDs: []string{"f"}}}}
	if _, err := unsourced.Validate(context.Background(), domain.EvidenceScope{TenantID: 1}, fact); !errors.Is(err, domain.ErrDegraded) {
		t.Fatalf("no fact source err = %v, want degraded", err)
	}
	fact.Evidence[0].FactKey = " "
	if report, err := unsourced.Validate(context.Background(), domain.EvidenceScope{TenantID: 1}, fact); err != nil || len(report.Unsupported) != 1 {
		t.Fatalf("blank key report = %+v, err = %v", report, err)
	}
}

func TestEvidenceValidatorHonoursFactKindsAndNormalization(t *testing.T) {
	validator := &EvidenceValidator{
		Facts:  factTable{{Key: "list", Kind: "price", Value: "$4,950"}, {Key: "promo", Value: "save $200"}},
		Claims: spanDetector{{Text: "$4950", Kind: "price"}},
	}
	answer := domain.EvidencedAnswer{
		Answer:   "Now $4950.",
		Evidence: []domain.EvidenceRef{{ID: "l", Source: domain.EvidenceFact, FactKey: "list"}},
		Claims:   []domain.Claim{{Text: "now $4950", EvidenceIDs: []string{"l"}}},
	}
	scope := domain.EvidenceScope{TenantID: 1}
	if report, err := validator.Validate(context.Background(), scope, answer); err != nil || len(report.Unsupported) != 1 {
		t.Fatalf("without normalization the literal values differ: %+v, %v", report, err)
	}
	validator.Normalize = func(text string) string { return strings.ReplaceAll(text, ",", "") }
	if report, err := validator.Validate(context.Background(), scope, answer); err != nil || len(report.Unsupported) != 0 {
		t.Fatalf("normalized report = %+v, err = %v", report, err)
	}

	answer.Evidence[0].FactKey = "promo"
	validator.Facts = factTable{{Key: "promo", Kind: "discount", Value: "$4950 off"}}
	if report, err := validator.Validate(context.Background(), scope, answer); err != nil || len(report.Unsupported) != 1 || report.Unsupported[0].ClaimIndex != -1 {
		t.Fatalf("a price claim is not stated by a discount fact: %+v, %v", report, err)
	}

	validator.Facts = factTable{{Key: "promo"}, {Key: "promo"}}
	if _, err := validator.Validate(context.Background(), scope, answer); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate fact err = %v, want conflict", err)
	}
}

func TestEvidenceValidatorValidatesTextThatDeclaresNoClaims(t *testing.T) {
	validator := &EvidenceValidator{
		Facts: factTable{
			{Key: "price", Kind: "price", Value: "The kit costs $49"},
			{Key: "sale", Kind: "price", Value: "$49 through May"},
			{Key: "warranty", Kind: "warranty", Value: "10 year warranty"},
		},
		Claims:    spanDetector{{Text: "$49", Kind: "price"}, {Text: "10-year", Kind: "warranty"}, {Text: "$49", Kind: "warranty"}},
		Normalize: func(text string) string { return strings.ReplaceAll(text, "-", " ") },
	}
	report, err := validator.ValidateText(context.Background(), domain.EvidenceScope{TenantID: 1}, "The $49 kit, 10-year warranty")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Supported) != 2 || len(report.Supported[0].EvidenceIDs) != 2 || report.Supported[1].EvidenceIDs[0] != "warranty" {
		t.Fatalf("supported = %+v", report.Supported)
	}
	if len(report.Unsupported) != 1 || report.Unsupported[0].ClaimIndex != -1 || report.Unsupported[0].Claim != "$49" {
		t.Fatalf("unsupported = %+v", report.Unsupported)
	}

	if _, err := (&EvidenceValidator{Facts: validator.Facts}).ValidateText(context.Background(), domain.EvidenceScope{TenantID: 1}, "x"); !errors.Is(err, domain.ErrDegraded) {
		t.Fatalf("no detector err = %v, want degraded", err)
	}
	if _, err := (&EvidenceValidator{Claims: validator.Claims}).ValidateText(context.Background(), domain.EvidenceScope{TenantID: 1}, "x"); !errors.Is(err, domain.ErrDegraded) {
		t.Fatalf("no fact source err = %v, want degraded", err)
	}
	if _, err := validator.ValidateText(context.Background(), domain.EvidenceScope{}, "x"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("no tenant err = %v, want validation", err)
	}
	validator.Facts = factTable{{Key: "price"}, {Key: "price"}}
	if _, err := validator.ValidateText(context.Background(), domain.EvidenceScope{TenantID: 1}, "x"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate fact err = %v, want conflict", err)
	}
	validator.Claims = spanDetector{{}}
	if _, err := validator.ValidateText(context.Background(), domain.EvidenceScope{TenantID: 1}, "x"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty claim err = %v, want validation", err)
	}
}
