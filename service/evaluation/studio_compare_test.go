package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

type baselineStoreFake struct {
	scope    domain.GoldenScope
	recorded domain.EvaluationBaseline
	outputs  []domain.BaselineOutput
	listed   string
}

func (store *baselineStoreFake) Record(_ context.Context, scope domain.GoldenScope, baseline domain.EvaluationBaseline, outputs []domain.BaselineOutput) (domain.EvaluationBaseline, error) {
	store.scope, store.recorded, store.outputs = scope, baseline, outputs
	baseline.BaselineID = 4
	return baseline, nil
}

func (store *baselineStoreFake) List(_ context.Context, _ int64, goldenSetID string, setVersion int64) ([]domain.EvaluationBaseline, error) {
	store.listed = goldenSetID
	return []domain.EvaluationBaseline{{BaselineID: 4, GoldenSetID: goldenSetID, SetVersion: setVersion}}, nil
}

type releaseSettingsFake struct{ resources []domain.EffectiveResource }

func (releaseSettingsFake) Put(context.Context, domain.EffectiveRelease) error { return nil }

func (store releaseSettingsFake) Get(context.Context, int64, string, string) (domain.EffectiveRelease, error) {
	return domain.EffectiveRelease{Resources: store.resources}, nil
}

func TestStudioCompareQueuesTheTwoArms(t *testing.T) {
	ctx := context.Background()
	service, _, runs, query := studioEvaluation(grants{domain.StudioTest: true})
	var models []*domain.ModelReference
	service.Drafts = freezerFunc(func(_ context.Context, _ domain.StudioActor, agentID string, model *domain.ModelReference) (domain.FrozenDraft, error) {
		models = append(models, model)
		definition := domain.AgentDefinition{AgentID: agentID, DefinitionDigest: draftDigest, SettingsDigest: scope.SettingsDigest(draftSettings())}
		if model != nil {
			definition.Models.Text = &domain.ModelReference{ProviderID: "provider-b", ModelID: model.ModelID}
		}
		return domain.FrozenDraft{Definition: definition, Settings: domain.EffectiveRelease{Resources: draftSettings()}}, nil
	})
	run, err := service.Compare(ctx, studioActor, domain.AgentCompareRequest{AgentID: "writer-a", SetVersion: 3,
		A: domain.CompareSubject{Release: " v2 "}, B: domain.CompareSubject{Draft: true, TextModel: "model-b"}})
	if err != nil {
		t.Fatal(err)
	}
	enqueued := runs.enqueued
	if enqueued.Scope != domain.GoldenScopeDev || enqueued.Subject.Kind != domain.SubjectDraft || enqueued.Draft == nil ||
		enqueued.Subject.Model == nil || *enqueued.Subject.Model != (domain.ModelReference{ProviderID: "provider-b", ModelID: "model-b"}) ||
		enqueued.Baseline == nil || enqueued.Baseline.Subject.Kind != domain.SubjectRelease || enqueued.Baseline.Subject.Version != "v2" ||
		enqueued.RequestedBy.ID != "9" || enqueued.MaxOpenRuns != 2 || run.Baseline == nil {
		t.Fatalf("enqueued = %+v", enqueued)
	}
	if len(models) != 1 || models[0].ModelID != "model-b" {
		t.Fatalf("draft frozen on %v", models)
	}
	if args := query.args[qStudioEvaluationEvent]; args[2] != StudioEventCompare ||
		args[3] != "run 11 of support version 3: a release v2, b draft "+draftDigest+" on provider-b/model-b" {
		t.Fatalf("audit args = %v", args)
	}
	if _, err := service.Compare(ctx, studioActor, domain.AgentCompareRequest{AgentID: "writer-a", SetVersion: 3,
		A: domain.CompareSubject{BaselineID: 5}, B: domain.CompareSubject{Draft: true}}); err != nil {
		t.Fatal(err)
	}
	if subject := runs.enqueued.Baseline.Subject; subject.Kind != domain.SubjectBaseline || subject.BaselineID != 5 || runs.enqueued.Subject.Model != nil {
		t.Fatalf("baseline arm = %+v", subject)
	}
	for name, request := range map[string]domain.AgentCompareRequest{
		"baseline as b":       {A: domain.CompareSubject{Draft: true}, B: domain.CompareSubject{BaselineID: 5}},
		"two kinds in one":    {A: domain.CompareSubject{Draft: true, Release: "v2"}, B: domain.CompareSubject{Draft: true}},
		"empty arm":           {A: domain.CompareSubject{}, B: domain.CompareSubject{Draft: true}},
		"model on a release":  {A: domain.CompareSubject{Release: "v2", TextModel: "model-b"}, B: domain.CompareSubject{Draft: true}},
		"negative baseline":   {A: domain.CompareSubject{BaselineID: -1}, B: domain.CompareSubject{Draft: true}},
		"blank release":       {A: domain.CompareSubject{Release: "  "}, B: domain.CompareSubject{Draft: true}},
		"missing set version": {A: domain.CompareSubject{Release: "v2"}, B: domain.CompareSubject{Draft: true}},
	} {
		request.AgentID = "writer-a"
		if name != "missing set version" {
			request.SetVersion = 3
		}
		if _, err := service.Compare(ctx, studioActor, request); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s = %v, want validation", name, err)
		}
	}
	blind, _, blindRuns, _ := studioEvaluation(grants{domain.StudioView: true})
	if _, err := blind.Compare(ctx, studioActor, domain.AgentCompareRequest{AgentID: "writer-a", SetVersion: 3,
		A: domain.CompareSubject{Release: "v2"}, B: domain.CompareSubject{Draft: true}}); !errors.Is(err, domain.ErrForbidden) || blindRuns.enqueued.AgentID != "" {
		t.Fatalf("compare without TEST = %v", err)
	}
}

func comparedDraftRow(t *testing.T) []any {
	t.Helper()
	draft := domain.AgentDefinition{AgentID: "writer-a", DefinitionDigest: draftDigest, SettingsDigest: scope.SettingsDigest(draftSettings()),
		Models:    domain.AgentModelSelection{Text: &domain.ModelReference{ProviderID: "provider-b", ModelID: "model-b"}},
		Languages: []domain.CompiledPrompt{{LanguageCode: "en-US", Sections: []domain.CompiledPromptSection{{PromptSectionID: 1, Caption: "Role", Instruction: "be brief"}}}}}
	definition, _ := json.Marshal(draft)
	settings, _ := json.Marshal(draftSettings())
	return []any{string(definition), string(settings)}
}

func TestStudioComparisonShowsAnswersTallyAndTheDraftsChanges(t *testing.T) {
	ctx := context.Background()
	service, _, runs, query := studioEvaluation(grants{domain.StudioView: true})
	answers := map[string]string{"object://out/a": "release answer", "object://out/b": "draft answer"}
	service.Outputs = objectReaderFunc(func(_ context.Context, ref domain.ObjectRef) ([]byte, error) { return []byte(answers[ref.URI]), nil })
	service.Releases = releaseSettingsFake{resources: []domain.EffectiveResource{{ResourceKind: domain.ResourceLimits, ResourceID: "limits", Value: []byte(`{"max_duration_seconds":60}`)}}}
	release, _ := json.Marshal(domain.AgentDefinition{AgentID: "writer-a", Models: domain.AgentModelSelection{Text: &domain.ModelReference{ProviderID: "provider-a", ModelID: "model-a"}},
		Languages: []domain.CompiledPrompt{{LanguageCode: "en-US", Sections: []domain.CompiledPromptSection{{PromptSectionID: 1, Caption: "Role", Instruction: "be verbose"}}}}})
	query.rows[qStudioComparedDraft] = [][]any{comparedDraftRow(t)}
	query.rows[qStudioComparedRelease] = [][]any{{string(release)}}
	runs.run = domain.AgentEvaluationRun{Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectDraft, DefinitionDigest: draftDigest},
		Baseline: &domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, Version: "v2"}}
	runs.tally = domain.ComparisonTally{A: 1, B: 2}
	runs.comparisons = []domain.AgentCaseComparison{
		{ExampleID: "c", Verdict: domain.ComparisonPrefersB,
			A: domain.AgentCaseResult{Output: domain.ObjectRef{URI: "object://out/a", Digest: sha256Hex([]byte("release answer"))}},
			B: domain.AgentCaseResult{Output: domain.ObjectRef{URI: "object://out/b", Digest: sha256Hex([]byte("draft answer"))}}},
		{ExampleID: "b"}, {ExampleID: "a"},
	}
	page, err := service.Comparison(ctx, studioActor, 11, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Cases) != 2 || page.NextBefore != "b" || runs.limit != 3 || page.Tally.B != 2 {
		t.Fatalf("page = %+v", page)
	}
	if string(page.Cases[0].A.Answer) != "release answer" || string(page.Cases[0].B.Answer) != "draft answer" || page.Cases[1].A.Answer != nil {
		t.Fatalf("answers = %q / %q", page.Cases[0].A.Answer, page.Cases[0].B.Answer)
	}
	diff := page.Diff
	if diff == nil || diff.Release != "v2" || len(diff.Models) != 1 || diff.Models[0].After.ModelID != "model-b" ||
		len(diff.PromptSections) != 1 || diff.PromptSections[0].Change != domain.DiffChanged ||
		len(diff.Settings) != 1 || string(diff.Settings[0].Before) != `{"max_duration_seconds":60}` {
		t.Fatalf("diff = %+v", diff)
	}
	if args := query.args[qStudioComparedRelease]; args[0] != int64(7) || args[1] != "writer-a" || args[2] != "v2" {
		t.Fatalf("release args = %v", args)
	}
	for _, scope := range runs.scopes {
		if scope != domain.GoldenScopeDev {
			t.Fatalf("a Studio reader read the %s scope", scope)
		}
	}

	runs.comparisons[0].B.Output.Digest = sha256Hex([]byte("other"))
	if _, err := service.Comparison(ctx, studioActor, 11, "", 2); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("an answer not matching its digest = %v", err)
	}
	runs.comparisons[0].B.Output.Digest = sha256Hex([]byte("draft answer"))
	query.rows[qStudioComparedDraft] = [][]any{{comparedDraftRow(t)[0], `[]`}}
	if _, err := service.Comparison(ctx, studioActor, 11, "", 2); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("pinned settings not matching the draft = %v", err)
	}

	runs.run.Baseline = &domain.AgentEvaluationSubject{Kind: domain.SubjectBaseline, BaselineID: 4}
	if page, err = service.Comparison(ctx, studioActor, 11, "", 2); err != nil || page.Diff != nil {
		t.Fatalf("a comparison with a baseline has no diff: %+v, %v", page.Diff, err)
	}
	runs.run.Baseline = nil
	if _, err := service.Comparison(ctx, studioActor, 11, "", 2); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an evaluation read as a comparison = %v", err)
	}
	runs.run.Baseline = &domain.AgentEvaluationSubject{Kind: domain.SubjectBaseline, BaselineID: 4}
	if _, err := service.Evaluation(ctx, studioActor, 11, "", 2); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a comparison read as an evaluation = %v", err)
	}
	blind, _, blindRuns, _ := studioEvaluation(grants{})
	blind.Outputs = service.Outputs
	blindRuns.run = runs.run
	if _, err := blind.Comparison(ctx, studioActor, 11, "", 2); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a comparison of an agent the actor cannot view = %v, want not found", err)
	}
	service.Outputs = nil
	if _, err := service.Comparison(ctx, studioActor, 11, "", 2); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("no output reader = %v", err)
	}
}

func TestStudioBaselinesAreEditedAndViewedInTheDevScope(t *testing.T) {
	ctx := context.Background()
	service, _, _, query := studioEvaluation(grants{domain.StudioEdit: true, domain.StudioView: true})
	store := &baselineStoreFake{}
	service.BaselineStore = store
	outputs := []domain.BaselineOutput{{CaseID: "refund", Output: []byte("we refund")}}
	baseline, err := service.RecordBaseline(ctx, studioActor, domain.BaselineRecord{AgentID: "writer-a", SetVersion: 3, Name: "Desk", Outputs: outputs})
	if err != nil || baseline.BaselineID != 4 {
		t.Fatalf("baseline = %+v, %v", baseline, err)
	}
	if store.scope != domain.GoldenScopeDev || store.recorded.TenantID != 7 || store.recorded.GoldenSetID != "support" || store.recorded.RecordedBy.ID != "9" || len(store.outputs) != 1 {
		t.Fatalf("recorded %+v in %s", store.recorded, store.scope)
	}
	if args := query.args[qStudioEvaluationEvent]; args[2] != StudioEventBaseline || !strings.HasPrefix(args[3].(string), `baseline 4 "Desk" of support version 3`) {
		t.Fatalf("audit args = %v", args)
	}
	if listed, err := service.Baselines(ctx, studioActor, "writer-a", 3); err != nil || len(listed) != 1 || store.listed != "support" {
		t.Fatalf("baselines = %+v, %v", listed, err)
	}

	viewer, _, _, _ := studioEvaluation(grants{domain.StudioView: true})
	viewer.BaselineStore = &baselineStoreFake{}
	if _, err := viewer.RecordBaseline(ctx, studioActor, domain.BaselineRecord{AgentID: "writer-a", SetVersion: 3, Name: "Desk", Outputs: outputs}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("record without EDIT = %v", err)
	}
	if _, err := viewer.Baselines(ctx, studioActor, "writer-b", 3); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("another agent's baselines = %v", err)
	}
	viewer.BaselineStore = nil
	if _, err := viewer.Baselines(ctx, studioActor, "writer-a", 3); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("no baseline store = %v", err)
	}
}
