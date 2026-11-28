package guardrail

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func classifierWith(output string, seen *domain.ModelRequest) *ModelClassifier {
	return &ModelClassifier{
		Model: domain.ModelSelection{Provider: "local", Model: "guard"},
		Gateway: &fake.ModelGateway{GenerateFunc: func(_ context.Context, _ domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
			if seen != nil {
				*seen = request
			}
			return domain.ModelResult{Output: []byte(output)}, nil
		}},
	}
}

var classifierSubject = domain.GuardrailSubject{TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "a1"}, RequestID: "r1", ConversationID: "c1"}

func TestModelClassifierLocatesExcerptsAndChargesSubject(t *testing.T) {
	var seen domain.ModelRequest
	content := []byte("call 555-1234 or 555-1234 today")
	got, err := classifierWith(`{"score":0.9,"excerpts":["555-1234"]}`, &seen).Classify(context.Background(), classifierSubject, domain.GuardrailKindPII, content)
	if err != nil {
		t.Fatal(err)
	}
	if got.Score != 0.9 || len(got.Spans) != 2 || got.Spans[0] != (domain.ContentSpan{Start: 5, End: 13}) || got.Spans[1] != (domain.ContentSpan{Start: 17, End: 25}) {
		t.Fatalf("classification = %+v", got)
	}
	if seen.TenantContext.TenantID != 7 || seen.Principal.ID != "a1" || seen.ConversationID != "c1" || !strings.HasPrefix(seen.RequestID, "r1:classify:pii:") {
		t.Fatalf("request not charged to the subject: %+v", seen)
	}
	if seen.MaxOutputTokens != DefaultClassifierMaxOutputTokens || seen.Output.Mode != domain.OutputModeJSONSchema || string(seen.Prompt) != string(content) {
		t.Fatalf("request = %+v", seen)
	}
}

func TestModelClassifierRejectsBadVerdicts(t *testing.T) {
	for name, output := range map[string]string{
		"unknown excerpt": `{"score":0.9,"excerpts":["absent"]}`,
		"score range":     `{"score":1.5,"excerpts":[]}`,
		"extra field":     `{"score":0.1,"excerpts":[],"why":"x"}`,
		"not json":        `maybe`,
	} {
		if _, err := classifierWith(output, nil).Classify(context.Background(), classifierSubject, domain.GuardrailKindToxicity, []byte("hello")); !errors.Is(err, domain.ErrInvalidModelOutput) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestModelClassifierFailsClosed(t *testing.T) {
	classifier := classifierWith(`{"score":0,"excerpts":[]}`, nil)
	if _, err := classifier.Classify(context.Background(), domain.GuardrailSubject{TenantID: 7}, domain.GuardrailKindPII, []byte("x")); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("missing principal err = %v", err)
	}
	if _, err := classifier.Classify(context.Background(), classifierSubject, domain.GuardrailKindRegex, []byte("x")); !errors.Is(err, domain.ErrCapabilityUnsupported) {
		t.Fatalf("unsupported kind err = %v", err)
	}
	var seen domain.ModelRequest
	if got, err := classifierWith(`{"score":1,"excerpts":[]}`, &seen).Classify(context.Background(), classifierSubject, domain.GuardrailKindPII, nil); err != nil || got.Score != 0 || seen.RequestID != "" {
		t.Fatalf("empty content: %+v %v, called = %q", got, err, seen.RequestID)
	}
	if _, err := (&ModelClassifier{}).Classify(context.Background(), classifierSubject, domain.GuardrailKindPII, []byte("x")); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("unconfigured err = %v", err)
	}
}
