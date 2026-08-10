package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/internal/fake"
)

var (
	testedAt = time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	ranAt    = time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
)

func listAgentsService(query *studioQueryFake) *StudioService {
	return &StudioService{
		DB: studioDBFake{qs: query}, Sources: unusedSources{}, Assembler: unusedAssembler{}, Compiler: unusedCompiler{},
	}
}

func agentRow() []any {
	return []any{"writer-a", "writer", "Writer", true, true, int64(4), int64(2), true, "3", nil, nil}
}

// Recorded execution history and Studio's own test events are both "last
// run"; the newer one wins.
func TestListAgentsMergesRuntimeActivityWithStudioTests(t *testing.T) {
	query := &studioQueryFake{rows: map[string][][]any{
		qStudioListAgents: {agentRow()},
		qStudioLastTest:   {{"writer-a", testedAt}},
	}, args: map[string][]any{}}
	service := listAgentsService(query)
	service.Activity = fake.ActivityReporter{LastRunFunc: func(context.Context, int64) (map[string]time.Time, error) {
		return map[string]time.Time{"writer-a": ranAt}, nil
	}}

	summaries, err := service.ListAgents(context.Background(), 8)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if summaries[0].LastRunAt == nil || !summaries[0].LastRunAt.Equal(ranAt) {
		t.Fatalf("last run = %v, want %v", summaries[0].LastRunAt, ranAt)
	}
}

// Without an activity reporter, a Studio test is still the last run.
func TestListAgentsFallsBackToStudioTests(t *testing.T) {
	query := &studioQueryFake{rows: map[string][][]any{
		qStudioListAgents: {agentRow()},
		qStudioLastTest:   {{"writer-a", testedAt}},
	}, args: map[string][]any{}}

	summaries, err := listAgentsService(query).ListAgents(context.Background(), 8)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if summaries[0].LastRunAt == nil || !summaries[0].LastRunAt.Equal(testedAt) {
		t.Fatalf("last run = %v, want %v", summaries[0].LastRunAt, testedAt)
	}
}

// An agent that has never run reports no time rather than a zero timestamp.
func TestListAgentsWithoutActivity(t *testing.T) {
	query := &studioQueryFake{rows: map[string][][]any{qStudioListAgents: {agentRow()}}, args: map[string][]any{}}
	summaries, err := listAgentsService(query).ListAgents(context.Background(), 8)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if summaries[0].LastRunAt != nil {
		t.Fatalf("last run = %v, want nil", summaries[0].LastRunAt)
	}
}

func TestListAgentsFailsLoudlyOnActivityError(t *testing.T) {
	query := &studioQueryFake{rows: map[string][][]any{qStudioListAgents: {agentRow()}}, args: map[string][]any{}}
	service := listAgentsService(query)
	broken := errors.New("activity store down")
	service.Activity = fake.ActivityReporter{LastRunFunc: func(context.Context, int64) (map[string]time.Time, error) {
		return nil, broken
	}}
	if _, err := service.ListAgents(context.Background(), 8); !errors.Is(err, broken) {
		t.Fatalf("want the activity failure surfaced, got %v", err)
	}
}

// Product validators see the phase, so release-only rules (credentials,
// executability) do not block an ordinary draft save.
func TestValidateDraftPassesPhaseToProductValidators(t *testing.T) {
	query := &studioQueryFake{rows: map[string][][]any{}, args: map[string][]any{}}
	service := listAgentsService(query)
	var seen []domain.ValidationPhase
	service.Catalog = studioModelCatalogFake{}
	service.Validators = []contract.AgentDraftValidator{fake.DraftValidator{
		ValidateFunc: func(_ context.Context, _ int64, _ domain.AgentDraft, phase domain.ValidationPhase) ([]domain.AgentFieldError, error) {
			seen = append(seen, phase)
			return nil, nil
		},
	}}
	draft := domain.AgentDraft{
		AgentID: "writer-a", DisplayName: "Writer", ExpectedDraftRevision: 1, ExpectedPromptProfileRevision: 1,
		Models:    domain.AgentModelSelection{Text: &domain.ModelReference{ProviderID: "p", ModelID: "m"}},
		Languages: []domain.AgentLanguageDraft{{LanguageCode: "en-US"}},
	}
	if err := service.validateDraft(context.Background(), 8, draft, false); err != nil {
		t.Fatalf("draft validation: %v", err)
	}
	if err := service.validateDraft(context.Background(), 8, draft, true); err != nil {
		t.Fatalf("release validation: %v", err)
	}
	if len(seen) != 2 || seen[0] != domain.ValidateDraft || seen[1] != domain.ValidateRelease {
		t.Fatalf("phases = %v", seen)
	}
}
