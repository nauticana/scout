package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

const (
	qStudioComparedDraft   = "scout_studio_evaluation_compared_draft"
	qStudioComparedRelease = "scout_studio_evaluation_compared_release"
)

// DefaultComparisonPageAnswerBytes bounds the stored answers of one comparison page.
const DefaultComparisonPageAnswerBytes = 1 << 20

// Studio lifecycle events of comparisons and baselines.
const (
	StudioEventCompare  = "COMPARE"
	StudioEventBaseline = "BASELINE"
)

var _ contract.StudioCompareBackend = (*StudioEvaluation)(nil)

// Compare pins a set version and two subjects, a the reference and b the challenger, and
// queues the comparison run. Studio actors compare in the dev scope only.
func (s *StudioEvaluation) Compare(ctx context.Context, actor domain.StudioActor, request domain.AgentCompareRequest) (domain.AgentEvaluationRun, error) {
	if err := s.authorize(ctx, actor, domain.StudioTest, request.AgentID); err != nil {
		return domain.AgentEvaluationRun{}, err
	}
	if request.SetVersion <= 0 {
		return domain.AgentEvaluationRun{}, fmt.Errorf("%w: set_version must be positive", domain.ErrValidation)
	}
	if err := errors.Join(validCompareSubject("a", request.A, true), validCompareSubject("b", request.B, false)); err != nil {
		return domain.AgentEvaluationRun{}, err
	}
	b, err := s.compareArm(ctx, actor, request.AgentID, request.B)
	if err != nil {
		return domain.AgentEvaluationRun{}, err
	}
	a, err := s.compareArm(ctx, actor, request.AgentID, request.A)
	if err != nil {
		return domain.AgentEvaluationRun{}, err
	}
	run, err := s.Runs.Enqueue(ctx, domain.AgentEvaluationEnqueue{
		TenantID: actor.TenantID, AgentID: request.AgentID, SetVersion: request.SetVersion, Scope: domain.GoldenScopeDev,
		Subject: b.Subject, Draft: b.Draft, DraftSettings: b.DraftSettings, Baseline: &a,
		RequestedBy: actor.Principal, MaxOpenRuns: s.MaxOpenRuns,
	})
	if err != nil {
		return domain.AgentEvaluationRun{}, err
	}
	detail := fmt.Sprintf("run %d of %s version %d: a %s, b %s", run.RunID, run.GoldenSetID, run.SetVersion, describeSubject(a.Subject), describeSubject(run.Subject))
	if err = s.audit(ctx, actor, request.AgentID, StudioEventCompare, detail); err != nil {
		return domain.AgentEvaluationRun{}, err
	}
	return run, nil
}

// validCompareSubject refuses an arm that is not exactly one of the draft, one release, or,
// for the reference arm only, one baseline, and a model named for anything but a draft.
func validCompareSubject(arm string, subject domain.CompareSubject, reference bool) error {
	kinds := 0
	for _, named := range []bool{subject.Draft, strings.TrimSpace(subject.Release) != "", subject.BaselineID != 0} {
		if named {
			kinds++
		}
	}
	switch {
	case kinds != 1:
		return fmt.Errorf("%w: arm %s is the draft, one release, or one baseline", domain.ErrValidation, arm)
	case strings.TrimSpace(subject.TextModel) != "" && !subject.Draft:
		return fmt.Errorf("%w: arm %s names a model, which only a draft is frozen on", domain.ErrValidation, arm)
	case subject.BaselineID != 0 && (!reference || subject.BaselineID < 0):
		return fmt.Errorf("%w: only arm a may be a recorded baseline, named by a positive id", domain.ErrValidation)
	}
	return nil
}

// compareArm pins one arm: the draft frozen as publish would, on its own text model or the one
// named, a release by version, or a baseline by id.
func (s *StudioEvaluation) compareArm(ctx context.Context, actor domain.StudioActor, agentID string, subject domain.CompareSubject) (domain.EvaluationArm, error) {
	switch {
	case subject.Draft:
		var model *domain.ModelReference
		if text := strings.TrimSpace(subject.TextModel); text != "" {
			model = &domain.ModelReference{ModelID: text}
		}
		frozen, err := s.Drafts.FreezeDraft(ctx, actor, agentID, model)
		if err != nil {
			return domain.EvaluationArm{}, err
		}
		arm := domain.EvaluationArm{Subject: frozen.Subject(), Draft: &frozen.Definition, DraftSettings: frozen.Settings.Resources}
		if model != nil && frozen.Definition.Models.Text != nil {
			resolved := *frozen.Definition.Models.Text
			arm.Subject.Model = &resolved
		}
		return arm, nil
	case subject.BaselineID != 0:
		return domain.EvaluationArm{Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectBaseline, AgentID: agentID, BaselineID: subject.BaselineID}}, nil
	default:
		return domain.EvaluationArm{Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, AgentID: agentID, Version: strings.TrimSpace(subject.Release)}}, nil
	}
}

func describeSubject(subject domain.AgentEvaluationSubject) string {
	switch subject.Kind {
	case domain.SubjectDraft:
		if subject.Model != nil {
			return "draft " + subject.DefinitionDigest + " on " + subject.Model.ProviderID + "/" + subject.Model.ModelID
		}
		return "draft " + subject.DefinitionDigest
	case domain.SubjectBaseline:
		return fmt.Sprintf("baseline %d", subject.BaselineID)
	default:
		return "release " + subject.Version
	}
}

// Comparison returns a comparison run, its verdict tally, one page of its dev-scope examples
// with both stored answers, and, when it compares the draft with a release, what the draft
// changes against that release. Limit zero means PageSize.
func (s *StudioEvaluation) Comparison(ctx context.Context, actor domain.StudioActor, runID int64, before string, limit int) (domain.AgentComparisonPage, error) {
	if err := s.init(ctx); err != nil {
		return domain.AgentComparisonPage{}, err
	}
	if runID <= 0 || limit < 0 || limit > s.maxPageSize() {
		return domain.AgentComparisonPage{}, fmt.Errorf("%w: run_id must be positive and limit within 0..%d", domain.ErrValidation, s.maxPageSize())
	}
	if limit == 0 {
		limit = s.pageSize()
	}
	if s.Outputs == nil {
		return domain.AgentComparisonPage{}, fmt.Errorf("%w: comparison outputs are not readable", domain.ErrNotReady)
	}
	run, err := s.viewableRun(ctx, actor, runID)
	if err != nil {
		return domain.AgentComparisonPage{}, err
	}
	if run.Baseline == nil {
		return domain.AgentComparisonPage{}, fmt.Errorf("%w: comparison run %d", domain.ErrNotFound, runID)
	}
	page := domain.AgentComparisonPage{Run: run}
	if page.Tally, err = s.Runs.Tally(ctx, domain.GoldenScopeDev, actor.TenantID, runID); err != nil {
		return domain.AgentComparisonPage{}, err
	}
	cases, err := s.Runs.Comparisons(ctx, domain.GoldenScopeDev, actor.TenantID, runID, before, limit+1)
	if err != nil {
		return domain.AgentComparisonPage{}, err
	}
	if len(cases) > limit {
		cases, page.NextBefore = cases[:limit], cases[limit-1].ExampleID
	}
	budget := orDefault(s.MaxPageAnswerBytes, DefaultComparisonPageAnswerBytes)
	for i := range cases {
		for _, arm := range []*domain.AgentCaseResult{&cases[i].A, &cases[i].B} {
			if arm.Output.URI == "" {
				continue
			}
			if budget <= 0 {
				arm.AnswerOmitted = true
				continue
			}
			answer, err := s.answer(ctx, arm.Output)
			if err != nil {
				return domain.AgentComparisonPage{}, fmt.Errorf("example %q: %w", cases[i].ExampleID, err)
			}
			if len(answer) > budget {
				arm.AnswerOmitted = true
				continue
			}
			arm.Answer, budget = answer, budget-len(answer)
		}
	}
	page.Cases = cases
	if page.Diff, err = s.comparisonDiff(ctx, actor.TenantID, run); err != nil {
		return domain.AgentComparisonPage{}, err
	}
	return page, nil
}

func (s *StudioEvaluation) answer(ctx context.Context, ref domain.ObjectRef) ([]byte, error) {
	if ref.URI == "" {
		return nil, nil
	}
	content, err := s.Outputs.Read(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("read stored answer: %w", err)
	}
	if sha256Hex(content) != strings.ToLower(ref.Digest) {
		return nil, fmt.Errorf("%w: stored answer does not match its digest", domain.ErrConflict)
	}
	return content, nil
}

// comparisonDiff is what the compared draft changes against the compared release, from the
// definitions and settings the run pinned; nil unless one arm is each.
func (s *StudioEvaluation) comparisonDiff(ctx context.Context, tenantID int64, run domain.AgentEvaluationRun) (*domain.ComparisonDiff, error) {
	var release, digest string
	switch {
	case run.Subject.Kind == domain.SubjectDraft && run.Baseline.Kind == domain.SubjectRelease:
		release, digest = run.Baseline.Version, run.Subject.DefinitionDigest
	case run.Subject.Kind == domain.SubjectRelease && run.Baseline.Kind == domain.SubjectDraft:
		release, digest = run.Subject.Version, run.Baseline.DefinitionDigest
	default:
		return nil, nil
	}
	pinned, err := s.qs.Query(ctx, qStudioComparedDraft, tenantID, run.RunID)
	if err != nil {
		return nil, fmt.Errorf("load compared draft: %w", err)
	}
	if len(pinned.Rows) == 0 {
		return nil, fmt.Errorf("%w: compared draft of run %d", domain.ErrNotFound, run.RunID)
	}
	var draft domain.AgentDefinition
	var draftSettings []domain.EffectiveResource
	if err = errors.Join(json.Unmarshal([]byte(common.AsString(pinned.Rows[0][0])), &draft),
		json.Unmarshal([]byte(common.AsString(pinned.Rows[0][1])), &draftSettings)); err != nil {
		return nil, fmt.Errorf("decode compared draft of run %d: %w", run.RunID, err)
	}
	if draft.DefinitionDigest != digest || scope.SettingsDigest(draftSettings) != draft.SettingsDigest {
		return nil, fmt.Errorf("%w: compared draft of run %d does not match its digests", domain.ErrConflict, run.RunID)
	}
	published, err := s.qs.Query(ctx, qStudioComparedRelease, tenantID, run.AgentID, release)
	if err != nil {
		return nil, fmt.Errorf("load compared release: %w", err)
	}
	if len(published.Rows) == 0 {
		return nil, fmt.Errorf("%w: release %q of agent %q", domain.ErrNotFound, release, run.AgentID)
	}
	var definition domain.AgentDefinition
	if err = json.Unmarshal([]byte(common.AsString(published.Rows[0][0])), &definition); err != nil {
		return nil, fmt.Errorf("decode compared release %q: %w", release, err)
	}
	settings, err := s.releases().Get(ctx, tenantID, run.AgentID, release)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("load compared release settings: %w", err)
	}
	return &domain.ComparisonDiff{
		Release:        release,
		Models:         modelChanges(definition.Models, draft.Models),
		PromptSections: scope.PromptSectionChanges(definition.Languages, draft.Languages),
		Settings:       scope.SettingChanges(settings.Resources, draftSettings),
	}, nil
}

func (s *StudioEvaluation) releases() contract.EffectiveReleaseRepository {
	if s.Releases != nil {
		return s.Releases
	}
	return &scope.TableEffectiveReleaseStore{DB: s.DB}
}

func modelChanges(before, after domain.AgentModelSelection) []domain.ModelChange {
	var changes []domain.ModelChange
	for _, modality := range []struct {
		name          string
		before, after *domain.ModelReference
	}{{"text", before.Text, after.Text}, {"image", before.Image, after.Image}, {"video", before.Video, after.Video}} {
		if (modality.before == nil) != (modality.after == nil) || modality.before != nil && *modality.before != *modality.after {
			changes = append(changes, domain.ModelChange{Modality: modality.name, Before: modality.before, After: modality.after})
		}
	}
	return changes
}

// RecordBaseline records human reference outputs for every dev case of a frozen version of
// the agent's golden set.
func (s *StudioEvaluation) RecordBaseline(ctx context.Context, actor domain.StudioActor, record domain.BaselineRecord) (domain.EvaluationBaseline, error) {
	if err := s.authorize(ctx, actor, domain.StudioEdit, record.AgentID); err != nil {
		return domain.EvaluationBaseline{}, err
	}
	if s.BaselineStore == nil {
		return domain.EvaluationBaseline{}, fmt.Errorf("%w: baselines are not composed", domain.ErrNotReady)
	}
	goldenSetID, _, err := s.GoldenCases.AgentSet(ctx, actor.TenantID, record.AgentID)
	if err != nil {
		return domain.EvaluationBaseline{}, err
	}
	baseline, err := s.BaselineStore.Record(ctx, domain.GoldenScopeDev, domain.EvaluationBaseline{
		TenantID: actor.TenantID, GoldenSetID: goldenSetID, SetVersion: record.SetVersion, Name: record.Name, RecordedBy: actor.Principal,
	}, record.Outputs)
	if err != nil {
		return domain.EvaluationBaseline{}, err
	}
	detail := fmt.Sprintf("baseline %d %q of %s version %d", baseline.BaselineID, baseline.Name, goldenSetID, baseline.SetVersion)
	if err = s.audit(ctx, actor, record.AgentID, StudioEventBaseline, detail); err != nil {
		return domain.EvaluationBaseline{}, err
	}
	return baseline, nil
}

// Baselines lists the baselines of a version of the agent's golden set, newest first.
func (s *StudioEvaluation) Baselines(ctx context.Context, actor domain.StudioActor, agentID string, setVersion int64) ([]domain.EvaluationBaseline, error) {
	if err := s.authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return nil, err
	}
	if s.BaselineStore == nil {
		return nil, fmt.Errorf("%w: baselines are not composed", domain.ErrNotReady)
	}
	goldenSetID, _, err := s.GoldenCases.AgentSet(ctx, actor.TenantID, agentID)
	if err != nil {
		return nil, err
	}
	return s.BaselineStore.List(ctx, actor.TenantID, goldenSetID, setVersion)
}
