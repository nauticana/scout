package observability

import (
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestLabelPolicySanitize(t *testing.T) {
	policy := DefaultLabelPolicy()
	cases := []struct {
		name   string
		labels map[string]string
		wantOK bool
	}{
		{"allowlisted", map[string]string{LabelTenantTier: "gold", LabelModel: "claude-3.5:2024/10", LabelStage: ""}, true},
		{"forbidden tenant id", map[string]string{LabelTenantTier: "gold", LabelTenantID: "42"}, false},
		{"forbidden request id", map[string]string{LabelRequestID: "r-1"}, false},
		{"forbidden conversation id", map[string]string{LabelConversationID: "c-1"}, false},
		{"unknown key", map[string]string{"prompt_hash": "abc"}, false},
		{"free text value", map[string]string{LabelErrorClass: "rate limited by upstream"}, false},
		{"over-long value", map[string]string{LabelModel: strings.Repeat("m", DefaultMaxLabelValueLength+1)}, false},
		{"exact length value", map[string]string{LabelModel: strings.Repeat("m", DefaultMaxLabelValueLength)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clean, err := policy.Sanitize(tc.labels)
			if tc.wantOK != (err == nil) {
				t.Fatalf("err = %v", err)
			}
			if err != nil && !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if err == nil && len(clean) != len(tc.labels) {
				t.Fatalf("clean = %v", clean)
			}
		})
	}
}

func TestLabelPolicyValidate(t *testing.T) {
	if err := (LabelPolicy{}).Validate(); err != nil {
		t.Fatalf("zero policy = %v", err)
	}
	if err := (LabelPolicy{Allowed: []string{LabelTenantID}}).Validate(); err == nil {
		t.Fatal("forbidden key in allowlist accepted")
	}
	if err := (LabelPolicy{Allowed: []string{LabelModel}, MaxValueLength: -1}).Validate(); err == nil {
		t.Fatal("negative length accepted")
	}
	// A shorter custom bound applies.
	short := LabelPolicy{Allowed: []string{LabelModel}, MaxValueLength: 3}
	if _, err := short.Sanitize(map[string]string{LabelModel: "abcd"}); err == nil {
		t.Fatal("custom bound ignored")
	}
}
