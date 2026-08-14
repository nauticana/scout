package controlplane

import (
	"context"
	"testing"

	keelmodel "github.com/nauticana/keel/model"

	"github.com/nauticana/scout/domain"
)

type promptSourceFake struct {
	rows map[string][][]any
}

func (f promptSourceFake) Query(_ context.Context, name string, _ ...any) (*keelmodel.QueryResult, error) {
	return &keelmodel.QueryResult{Rows: f.rows[name]}, nil
}

func (promptSourceFake) GenID() int64 { return 0 }

type baselineSelector struct {
	keys []string
}

func (s baselineSelector) Select(context.Context, int64, string, string) (domain.PromptBaselineSelection, error) {
	return domain.PromptBaselineSelection{Keys: s.keys}, nil
}

func TestKeelPromptSourceRepositoryResolve(t *testing.T) {
	repository := &PromptRepository{
		Selector: baselineSelector{keys: []string{"tenant-plan", "global"}},
		qs: promptSourceFake{rows: map[string][][]any{
			qPromptAgent: {{"writer"}},
			qPromptBaselines: {
				{"global", int64(1), "task", "Task", int64(1), "global task", "global output"},
				{"tenant-plan", int64(1), "task", "Task", int64(1), "plan task", "plan output"},
				{"ignored", int64(2), "tone", "Tone", int64(2), "ignored", nil},
			},
			qPromptTenantDefaults: {{int64(1), "task", "Task", int64(1), "tenant task", nil}},
			qPromptAgentOverrides: {{int64(2), "tone", "Tone", int64(2), true, "agent tone", "agent output"}},
		}},
	}

	resolved, err := repository.Resolve(context.Background(), 7, "writer-a", "en-US")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.BaselineKey != "tenant-plan" || len(resolved.Rows) != 3 {
		t.Fatalf("unexpected resolution: %+v", resolved)
	}
	if resolved.Rows[0].Instruction != "plan task" || resolved.Rows[1].SourceLevel != domain.PromptSourceTenantDefault {
		t.Fatalf("baseline precedence or source order changed: %+v", resolved.Rows)
	}
	if !resolved.Rows[2].Overwrite || resolved.Rows[2].SourceKey != "writer-a" {
		t.Fatalf("override metadata missing: %+v", resolved.Rows[2])
	}
}

func TestKeelPromptSourceRepositoryLanguages(t *testing.T) {
	repository := &PromptRepository{
		Selector: baselineSelector{keys: []string{"global"}},
		qs: promptSourceFake{rows: map[string][][]any{
			qPromptAgent:           {{"writer"}},
			qPromptBaseLanguages:   {{"global", "en-US"}, {"ignored", "de-DE"}},
			qPromptTenantLanguages: {{"fr-FR"}},
			qPromptAgentLanguages:  {{"en-US"}, {"tr-TR"}},
		}},
	}

	languages, err := repository.Languages(context.Background(), 7, "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"en-US", "fr-FR", "tr-TR"}
	for i := range want {
		if languages[i] != want[i] {
			t.Fatalf("languages = %v, want %v", languages, want)
		}
	}
}
