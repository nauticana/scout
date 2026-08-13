package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func testDraftService(query *studioQueryFake, tester fake.DraftTester) *StudioService {
	compiler := &PromptCompiler{}
	return &StudioService{
		DB: studioDBFake{qs: query}, Compiler: compiler,
		Assembler: &PromptDraftAssembler{Compiler: compiler},
		Sources: studioSourcesFake{resolved: domain.ResolvedPrompts{
			AgentID: "writer-a", AgentKind: "writer", BaselineKey: "global", LanguageCode: "en-US",
			Rows: []domain.PromptSourceRow{{
				PromptSectionID: 1, Caption: "task", DisplayOrder: 1,
				SourceLevel: domain.PromptSourceBaseline, SourceKey: "global", Instruction: "write",
			}},
		}},
		Catalog: studioModelCatalogFake{}, Tester: tester,
	}
}

func draftRows() map[string][][]any {
	return map[string][][]any{
		qStudioGetDraft: {{"writer", "Writer", true, true, false, "provider-a", "model-a", nil, nil, nil, nil, nil, int64(4), int64(2), true}},
	}
}

// Studio owns the lifecycle audit; the product tester only executes.
func TestTestDraftRecordsItsOwnAudit(t *testing.T) {
	query := &studioQueryFake{rows: draftRows(), args: map[string][]any{}}
	service := testDraftService(query, fake.DraftTester{
		ExecuteFunc: func(_ context.Context, _ domain.StudioActor, _ domain.AgentTestRequest, definition domain.AgentDefinition) (domain.AgentTestResult, error) {
			return domain.AgentTestResult{AgentID: definition.AgentID, LanguageCode: "en-US", Output: "sample"}, nil
		},
	})
	result, err := service.TestDraft(context.Background(), domain.StudioActor{TenantID: 8, ActorID: 9},
		domain.AgentTestRequest{AgentID: "writer-a", LanguageCode: "en-US"})
	if err != nil {
		t.Fatalf("TestDraft: %v", err)
	}
	if result.Output != "sample" {
		t.Fatalf("result = %+v", result)
	}
	args, recorded := query.args[qStudioAudit]
	if !recorded {
		t.Fatal("a successful test must leave a TEST event")
	}
	if args[2] != "TEST" || args[1] != "writer-a" || args[4] != int64(9) {
		t.Fatalf("audit args = %v", args)
	}
}

// A failed test is not a lifecycle event.
func TestTestDraftFailureRecordsNoAudit(t *testing.T) {
	query := &studioQueryFake{rows: draftRows(), args: map[string][]any{}}
	broken := errors.New("provider down")
	service := testDraftService(query, fake.DraftTester{
		ExecuteFunc: func(context.Context, domain.StudioActor, domain.AgentTestRequest, domain.AgentDefinition) (domain.AgentTestResult, error) {
			return domain.AgentTestResult{}, broken
		},
	})
	if _, err := service.TestDraft(context.Background(), domain.StudioActor{TenantID: 8, ActorID: 9},
		domain.AgentTestRequest{AgentID: "writer-a"}); !errors.Is(err, broken) {
		t.Fatalf("want the tester failure surfaced, got %v", err)
	}
	if _, recorded := query.args[qStudioAudit]; recorded {
		t.Fatal("a failed test must not be audited as a TEST event")
	}
}
