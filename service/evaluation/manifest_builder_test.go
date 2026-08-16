package evaluation

import (
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestManifestBuilderIsDeterministicAndContentAddressed(t *testing.T) {
	examples := []domain.GoldenExample{testExample("a"), testExample("b")}
	first := testManifest(t, examples...)
	second := testManifest(t, examples...)
	if first.ManifestID != second.ManifestID || len(first.ManifestID) != 64 {
		t.Fatalf("manifest ids = %q, %q", first.ManifestID, second.ManifestID)
	}
	// Reordering the same examples must not change the dataset revision.
	if DatasetRevision(examples) != DatasetRevision([]domain.GoldenExample{examples[1], examples[0]}) {
		t.Fatal("dataset revision depends on example order")
	}

	changed := first
	changed.Candidate.Versions.Model = "m3"
	rebuilt, err := (&ManifestBuilder{Now: fixedClock(testClock)}).Build(changed)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ManifestID == first.ManifestID {
		t.Fatal("changing a pinned version did not change the manifest id")
	}
}

func TestManifestBuilderVerifyRejectsTamperedManifest(t *testing.T) {
	manifest := testManifest(t, testExample("a"))
	builder := &ManifestBuilder{Now: fixedClock(testClock)}
	if err := builder.Verify(manifest); err != nil {
		t.Fatalf("verify: %v", err)
	}
	manifest.SafetyPolicyVersion = "safety-2"
	if err := builder.Verify(manifest); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("tampered verify = %v", err)
	}
}

func TestManifestBuilderValidation(t *testing.T) {
	builder := &ManifestBuilder{Now: fixedClock(testClock)}
	valid := testManifest(t, testExample("a"))
	cases := map[string]func(*domain.EvaluationManifest){
		"tenant":     func(m *domain.EvaluationManifest) { m.TenantID = 0 },
		"agent":      func(m *domain.EvaluationManifest) { m.Candidate.AgentVersion = "" },
		"same agent": func(m *domain.EvaluationManifest) { m.Baseline.AgentID = "other" },
		"set":        func(m *domain.EvaluationManifest) { m.GoldenSetVersion = 0 },
		"revision":   func(m *domain.EvaluationManifest) { m.DatasetRevision = "short" },
		"evaluators": func(m *domain.EvaluationManifest) { m.Evaluators = nil },
		"safety":     func(m *domain.EvaluationManifest) { m.SafetyPolicyVersion = " " },
		"decoding":   func(m *domain.EvaluationManifest) { m.Candidate.Decoding = []byte("{not json") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			draft := valid
			mutate(&draft)
			if _, err := builder.Build(draft); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("%s = %v", name, err)
			}
		})
	}
}
