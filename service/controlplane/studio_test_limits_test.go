package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// The tester receives the limits frozen with the draft, whatever the caller sent.
func TestTestDraftPassesTheFrozenLimitsNotTheCallers(t *testing.T) {
	query := &studioQueryFake{rows: draftRows(), args: map[string][]any{}}
	var got domain.ExecutionLimits
	service := testDraftService(query, fake.DraftTester{
		ExecuteFunc: func(_ context.Context, _ domain.StudioActor, request domain.AgentTestRequest, definition domain.AgentDefinition) (domain.AgentTestResult, error) {
			got = request.Limits
			return domain.AgentTestResult{AgentID: definition.AgentID, LanguageCode: "en-US", Output: "sample"}, nil
		},
	})
	service.Scopes = studioScopes([]domain.ScopedBinding{{ScopeID: "a:writer-a", ResourceKind: domain.ResourceLimits, ResourceID: "limits",
		ResourceVersion: "v1", MergeMode: domain.MergeReplace, Value: []byte(`{"max_steps":2,"max_tool_calls":3,"max_duration_seconds":30}`)}})
	widened := domain.ExecutionLimits{MaxSteps: 100, MaxToolCalls: 100, MaxDuration: time.Hour}
	if _, err := service.TestDraft(context.Background(), humanActor(8, 9), domain.AgentTestRequest{AgentID: "writer-a", Limits: widened}); err != nil {
		t.Fatalf("TestDraft: %v", err)
	}
	if got != (domain.ExecutionLimits{MaxSteps: 2, MaxToolCalls: 3, MaxDuration: 30 * time.Second}) {
		t.Fatalf("tester limits = %+v, want the draft's frozen limits", got)
	}
}
