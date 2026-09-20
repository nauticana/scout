package controlplane

import (
	"context"
	"testing"

	keelmodel "github.com/nauticana/keel/model"

	"github.com/nauticana/scout/contract"
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

// promptChain is a fixed tenant → type → agent ancestry, widest first.
type promptChain struct{ contract.ScopeRepository }

func (promptChain) Chain(context.Context, int64, string) (domain.ScopeChain, error) {
	return domain.ScopeChain{
		{ScopeID: RootPromptScopeID, ScopeKind: "tenant"},
		{ScopeID: "t:writer", ScopeKind: "agent_type"},
		{ScopeID: "a:writer-a", ScopeKind: "agent"},
	}, nil
}

func TestPromptRepositoryLayersTheBaselineUnderTheChainsBindings(t *testing.T) {
	repository := &PromptRepository{
		Selector: baselineSelector{keys: []string{"tenant-plan", "global"}}, Scopes: promptChain{},
		qs: promptSourceFake{rows: map[string][][]any{
			qPromptAgent: {{"writer"}},
			qPromptBaselines: {
				{"global", int64(1), "task", "Task", int64(1), "global task", "global output"},
				{"tenant-plan", int64(1), "task", "Task", int64(1), "plan task", "plan output"},
				{"ignored", int64(2), "tone", "Tone", int64(2), "ignored", nil},
			},
			// Rows arrive in no particular order; the chain orders them.
			qPromptBindings: {
				{"a:writer-a", int64(1), "task", "Task", int64(1), "replace", false, "v2", int64(9), `{"instruction":"agent task"}`},
				{"t:writer", int64(1), "task", "Task", int64(1), "append", true, "v1", nil, `{"instruction":"tenant task","output":"tenant output"}`},
				{RootPromptScopeID, int64(2), "tone", "Tone", int64(2), "append", false, "v1", nil, `{"instruction":"company tone"}`},
			},
		}},
	}

	resolved, err := repository.Resolve(context.Background(), 7, "writer-a", "en-US")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.BaselineKey != "tenant-plan" || len(resolved.Sections) != 2 || resolved.Scopes.TypeScopeID != "t:writer" {
		t.Fatalf("unexpected resolution: %+v", resolved)
	}
	task := resolved.Sections[0].Layers
	if len(task) != 3 || task[0].ScopeKind != domain.ScopeKindPlatform || task[0].Instruction != "plan task" || task[0].Editable {
		t.Fatalf("the best-ranked baseline must be the first layer: %+v", task)
	}
	if task[1].ScopeID != "t:writer" || !task[1].Sealed || !task[1].Editable || task[1].Output != "tenant output" ||
		task[2].ScopeID != "a:writer-a" || task[2].MergeMode != domain.MergeReplace || *task[2].BoundBy != 9 {
		t.Fatalf("bindings must follow the chain, widest first: %+v", task)
	}
	// An unranked baseline never applies, and a scope Studio does not edit is read-only.
	tone := resolved.Sections[1].Layers
	if len(tone) != 1 || tone[0].ScopeID != RootPromptScopeID || tone[0].Editable {
		t.Fatalf("tone layers = %+v", tone)
	}
}

func TestPromptRepositoryLanguages(t *testing.T) {
	repository := &PromptRepository{
		Selector: baselineSelector{keys: []string{"global"}}, Scopes: promptChain{},
		qs: promptSourceFake{rows: map[string][][]any{
			qPromptAgent:            {{"writer"}},
			qPromptBaseLanguages:    {{"global", "en-US"}, {"ignored", "de-DE"}},
			qPromptBindingLanguages: {{"fr-FR"}, {"en-US"}, {"tr-TR"}},
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
