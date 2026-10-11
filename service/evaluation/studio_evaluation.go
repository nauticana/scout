package evaluation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const qStudioEvaluationEvent = "scout_studio_evaluation_event"

var studioEvaluationQueries = map[string]string{
	qStudioEvaluationEvent: `
INSERT INTO agent_studio_event (id, partner_id, agent_id, event, detail, principal_kind, principal_id)
VALUES (NEXTVAL('agent_studio_event_seq'), ?, ?, ?, ?, ?, ?)`,
	qStudioComparedDraft: `
SELECT definition_json, settings_json FROM evaluation_run_subject WHERE partner_id = ? AND run_id = ? AND subject_code = 'D'`,
	qStudioComparedRelease: `
SELECT definition FROM agent_version WHERE partner_id = ? AND agent_id = ? AND agent_version = ?`,
}

// Studio lifecycle events this service records.
const (
	StudioEventCases     = "CASES"
	StudioEventFreeze    = "CASES_FREEZE"
	StudioEventEvaluate  = "EVALUATE"
	studioEventDetailMax = 400
)

// Defaults of StudioEvaluation's result pages: PageSize when a caller names no limit, MaxPageSize the largest.
const (
	DefaultStudioPageSize    = 50
	DefaultStudioMaxPageSize = 200
)

// DefaultTrajectoryMaxBytes is the largest stored trajectory Studio reads when no flag names one.
const DefaultTrajectoryMaxBytes = 1 << 20

// StudioEvaluation serves an agent's saved test cases and evaluations to Studio. Every
// Studio actor works in the dev golden scope: gate cases are never returned, written,
// or executed for them, and a run's gate results are only counted.
type StudioEvaluation struct {
	DB          keelport.DatabaseRepository
	Authorizer  contract.StudioAuthorizer
	Drafts      contract.DraftFreezer
	GoldenCases contract.GoldenCaseStore
	Runs        contract.AgentEvaluationStore
	// MaxOpenRuns caps an agent's queued and running evaluations; zero refuses every evaluation.
	MaxOpenRuns int
	// PageSize answers a result page that names no limit and MaxPageSize caps one that does; zero
	// takes DefaultStudioPageSize and DefaultStudioMaxPageSize. PageSize may not exceed MaxPageSize.
	PageSize    int
	MaxPageSize int
	// BaselineStore keeps the human baselines a comparison may name; Outputs reads the stored
	// answers a comparison page shows. Both are required by the compare feature only.
	BaselineStore contract.EvaluationBaselineStore
	Outputs       contract.ObjectReader
	// Releases reads a compared release's frozen settings for the diff; nil uses scope.TableEffectiveReleaseStore.
	Releases contract.EffectiveReleaseRepository
	// MaxPageAnswerBytes bounds the stored answers one comparison page returns; answers past it
	// are omitted and marked. Zero takes DefaultComparisonPageAnswerBytes.
	MaxPageAnswerBytes int
	// MaxTrajectoryBytes bounds the stored trajectory Trajectory reads, through Outputs; zero
	// takes DefaultTrajectoryMaxBytes.
	MaxTrajectoryBytes int

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.StudioEvaluationBackend = (*StudioEvaluation)(nil)

func (s *StudioEvaluation) pageSize() int { return orDefault(s.PageSize, DefaultStudioPageSize) }

func (s *StudioEvaluation) maxPageSize() int {
	return orDefault(s.MaxPageSize, DefaultStudioMaxPageSize)
}

func (s *StudioEvaluation) init(ctx context.Context) error {
	if s.DB == nil || s.Authorizer == nil || s.Drafts == nil || s.GoldenCases == nil || s.Runs == nil {
		return fmt.Errorf("studio evaluation: database, authorizer, draft freezer, case store, and run store are required")
	}
	if s.MaxOpenRuns < 0 {
		return fmt.Errorf("%w: max open runs cannot be negative", domain.ErrValidation)
	}
	if s.MaxPageAnswerBytes < 0 || s.MaxTrajectoryBytes < 0 {
		return fmt.Errorf("%w: comparison page answer bytes and trajectory bytes cannot be negative", domain.ErrValidation)
	}
	if s.PageSize < 0 || s.MaxPageSize < 0 || s.pageSize() > s.maxPageSize() {
		return fmt.Errorf("%w: result page size %d must be non-negative and within the largest page %d", domain.ErrValidation, s.pageSize(), s.maxPageSize())
	}
	s.once.Do(func() { s.qs = s.DB.GetQueryService(ctx, studioEvaluationQueries) })
	if s.qs == nil {
		return fmt.Errorf("studio evaluation: query service is required")
	}
	return nil
}

func (s *StudioEvaluation) authorize(ctx context.Context, actor domain.StudioActor, action domain.StudioAction, agentID string) error {
	if err := s.init(ctx); err != nil {
		return err
	}
	return s.Authorizer.Authorize(ctx, actor, action, agentID)
}

func (s *StudioEvaluation) audit(ctx context.Context, actor domain.StudioActor, agentID, event, detail string) error {
	if _, err := s.qs.Query(ctx, qStudioEvaluationEvent, actor.TenantID, agentID, event, nullable(truncate(detail, studioEventDetailMax)),
		string(actor.Principal.Kind), actor.Principal.ID); err != nil {
		return fmt.Errorf("record studio %s event: %w", strings.ToLower(event), err)
	}
	return nil
}

// Cases returns the agent's dev-scope cases, how many gate cases are withheld, and recent versions.
func (s *StudioEvaluation) Cases(ctx context.Context, actor domain.StudioActor, agentID string) (domain.GoldenCaseSet, error) {
	if err := s.authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return domain.GoldenCaseSet{}, err
	}
	return s.devCases(ctx, actor, agentID)
}

// devCases returns the agent's dev-scope cases with promoted production inputs withheld
// from an actor lacking REVIEW on the agent they came from.
func (s *StudioEvaluation) devCases(ctx context.Context, actor domain.StudioActor, agentID string) (domain.GoldenCaseSet, error) {
	set, err := s.GoldenCases.Cases(ctx, domain.GoldenScopeDev, actor.TenantID, agentID)
	if err != nil {
		return domain.GoldenCaseSet{}, err
	}
	return withholdReviewInputs(ctx, s.Authorizer, actor, set)
}

// withholdReviewInputs blanks the input of every case promoted from a production review
// unless the actor holds REVIEW on the reviewed turn's agent.
func withholdReviewInputs(ctx context.Context, authorizer contract.StudioAuthorizer, actor domain.StudioActor, set domain.GoldenCaseSet) (domain.GoldenCaseSet, error) {
	allowed := map[string]bool{}
	cases := make([]domain.GoldenCase, len(set.Cases))
	for i, goldenCase := range set.Cases {
		if goldenCase.ReviewItemID != 0 {
			reader, known := allowed[goldenCase.ReviewAgentID]
			if !known {
				err := authorizer.Authorize(ctx, actor, domain.StudioReview, goldenCase.ReviewAgentID)
				if err != nil && !errors.Is(err, domain.ErrForbidden) && !errors.Is(err, domain.ErrValidation) {
					return domain.GoldenCaseSet{}, err
				}
				reader = err == nil
				allowed[goldenCase.ReviewAgentID] = reader
			}
			if !reader {
				goldenCase.Input, goldenCase.InputWithheld = nil, true
			}
		}
		cases[i] = goldenCase
	}
	set.Cases = cases
	return set, nil
}

// ChangeCases associates the agent with a golden set, or adds, replaces, or removes one dev case.
func (s *StudioEvaluation) ChangeCases(ctx context.Context, actor domain.StudioActor, change domain.GoldenCaseChange) (domain.GoldenCaseSet, error) {
	if err := s.authorize(ctx, actor, domain.StudioEdit, change.AgentID); err != nil {
		return domain.GoldenCaseSet{}, err
	}
	var detail string
	switch change.Op {
	case domain.GoldenCaseAssociate:
		owner, err := s.shareableOwner(ctx, actor, change.AgentID, change.GoldenSetID)
		if err != nil {
			return domain.GoldenCaseSet{}, err
		}
		if err = s.GoldenCases.Associate(ctx, domain.GoldenSetAssociation{TenantID: actor.TenantID, AgentID: change.AgentID,
			GoldenSetID: change.GoldenSetID, Name: change.SetName, Owner: owner, By: actor.Principal}); err != nil {
			return domain.GoldenCaseSet{}, err
		}
		detail = "associate " + change.GoldenSetID
	case domain.GoldenCasePut, domain.GoldenCaseRemove:
		goldenSetID, _, err := s.GoldenCases.AgentSet(ctx, actor.TenantID, change.AgentID)
		if err != nil {
			return domain.GoldenCaseSet{}, err
		}
		if change.Op == domain.GoldenCasePut {
			goldenCase := change.Case
			goldenCase.TenantID, goldenCase.GoldenSetID, goldenCase.ModifiedBy = actor.TenantID, goldenSetID, actor.Principal
			if _, err = s.GoldenCases.PutCase(ctx, domain.GoldenScopeDev, goldenCase, change.ExpectedRevision); err != nil {
				return domain.GoldenCaseSet{}, err
			}
			detail = "put " + goldenSetID + "/" + goldenCase.CaseID
		} else {
			if _, err = s.GoldenCases.RemoveCase(ctx, domain.GoldenScopeDev, actor.TenantID, goldenSetID, change.CaseID, change.ExpectedRevision); err != nil {
				return domain.GoldenCaseSet{}, err
			}
			detail = "remove " + goldenSetID + "/" + change.CaseID
		}
	default:
		return domain.GoldenCaseSet{}, fmt.Errorf("%w: op must be associate, put, or remove", domain.ErrValidation)
	}
	if err := s.audit(ctx, actor, change.AgentID, StudioEventCases, detail); err != nil {
		return domain.GoldenCaseSet{}, err
	}
	return s.devCases(ctx, actor, change.AgentID)
}

// shareableOwner returns the owner the actor may associate the agent's set under: a new set
// is the agent's own, an existing one requires EDIT on its owning agent, and a set no
// agent owns belongs to a product and is refused.
func (s *StudioEvaluation) shareableOwner(ctx context.Context, actor domain.StudioActor, agentID, goldenSetID string) (string, error) {
	owner, err := s.GoldenCases.SetOwner(ctx, actor.TenantID, goldenSetID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return agentID, nil
	case err != nil:
		return "", err
	case owner == "":
		return "", fmt.Errorf("%w: golden set %q is not owned by an agent", domain.ErrForbidden, goldenSetID)
	case owner != agentID:
		if err = s.Authorizer.Authorize(ctx, actor, domain.StudioEdit, owner); err != nil {
			return "", err
		}
	}
	return owner, nil
}

// FreezeCases snapshots the agent's golden set, gate cases included, into a new immutable version.
func (s *StudioEvaluation) FreezeCases(ctx context.Context, actor domain.StudioActor, agentID string, expectedRevision int64) (domain.GoldenCaseSet, domain.GoldenSetVersion, error) {
	if err := s.authorize(ctx, actor, domain.StudioEdit, agentID); err != nil {
		return domain.GoldenCaseSet{}, domain.GoldenSetVersion{}, err
	}
	goldenSetID, _, err := s.GoldenCases.AgentSet(ctx, actor.TenantID, agentID)
	if err != nil {
		return domain.GoldenCaseSet{}, domain.GoldenSetVersion{}, err
	}
	version, _, err := s.GoldenCases.Freeze(ctx, actor.TenantID, goldenSetID, expectedRevision)
	if err != nil {
		return domain.GoldenCaseSet{}, domain.GoldenSetVersion{}, err
	}
	if err = s.audit(ctx, actor, agentID, StudioEventFreeze, fmt.Sprintf("%s version %d", goldenSetID, version.SetVersion)); err != nil {
		return domain.GoldenCaseSet{}, domain.GoldenSetVersion{}, err
	}
	set, err := s.devCases(ctx, actor, agentID)
	return set, version, err
}

// Evaluate pins a set version and the agent's frozen draft or one release and queues the run.
func (s *StudioEvaluation) Evaluate(ctx context.Context, actor domain.StudioActor, request domain.AgentEvaluationRequest) (domain.AgentEvaluationRun, error) {
	if err := s.authorize(ctx, actor, domain.StudioTest, request.AgentID); err != nil {
		return domain.AgentEvaluationRun{}, err
	}
	release := strings.TrimSpace(request.Release)
	if request.Draft == (release != "") {
		return domain.AgentEvaluationRun{}, fmt.Errorf("%w: the subject is either the draft or one release", domain.ErrValidation)
	}
	if request.SetVersion <= 0 {
		return domain.AgentEvaluationRun{}, fmt.Errorf("%w: set_version must be positive", domain.ErrValidation)
	}
	enqueue := domain.AgentEvaluationEnqueue{
		TenantID: actor.TenantID, AgentID: request.AgentID, SetVersion: request.SetVersion, Scope: domain.GoldenScopeDev,
		Subject:     domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, AgentID: request.AgentID, Version: release},
		RequestedBy: actor.Principal, MaxOpenRuns: s.MaxOpenRuns,
	}
	if request.Draft {
		frozen, err := s.Drafts.FreezeDraft(ctx, actor, request.AgentID, nil)
		if err != nil {
			return domain.AgentEvaluationRun{}, err
		}
		enqueue.Subject, enqueue.Draft, enqueue.DraftSettings = frozen.Subject(), &frozen.Definition, frozen.Settings.Resources
	}
	run, err := s.Runs.Enqueue(ctx, enqueue)
	if err != nil {
		return domain.AgentEvaluationRun{}, err
	}
	subject := "release " + run.Subject.Version
	if run.Subject.Kind == domain.SubjectDraft {
		subject = "draft " + run.Subject.DefinitionDigest
	}
	detail := fmt.Sprintf("run %d of %s version %d on %s", run.RunID, run.GoldenSetID, run.SetVersion, subject)
	if err = s.audit(ctx, actor, request.AgentID, StudioEventEvaluate, detail); err != nil {
		return domain.AgentEvaluationRun{}, err
	}
	return run, nil
}

// Evaluation returns a run and one page of its dev-scope results; limit zero means PageSize.
func (s *StudioEvaluation) Evaluation(ctx context.Context, actor domain.StudioActor, runID int64, before string, limit int) (domain.AgentEvaluationPage, error) {
	if err := s.init(ctx); err != nil {
		return domain.AgentEvaluationPage{}, err
	}
	if runID <= 0 || limit < 0 || limit > s.maxPageSize() {
		return domain.AgentEvaluationPage{}, fmt.Errorf("%w: run_id must be positive and limit within 0..%d", domain.ErrValidation, s.maxPageSize())
	}
	if limit == 0 {
		limit = s.pageSize()
	}
	run, err := s.viewableRun(ctx, actor, runID)
	if err != nil {
		return domain.AgentEvaluationPage{}, err
	}
	if run.Baseline != nil {
		return domain.AgentEvaluationPage{}, fmt.Errorf("%w: evaluation run %d is a comparison", domain.ErrNotFound, runID)
	}
	results, err := s.Runs.Results(ctx, domain.GoldenScopeDev, actor.TenantID, runID, before, limit+1)
	if err != nil {
		return domain.AgentEvaluationPage{}, err
	}
	page := domain.AgentEvaluationPage{Run: run, Results: results}
	if len(results) > limit {
		page.Results = results[:limit]
		page.NextBefore = results[limit-1].ExampleID
	}
	return page, nil
}

// viewableRun reads a dev-scope run of an agent the actor may VIEW; any other run reads as not found.
func (s *StudioEvaluation) viewableRun(ctx context.Context, actor domain.StudioActor, runID int64) (domain.AgentEvaluationRun, error) {
	run, err := s.Runs.Run(ctx, domain.GoldenScopeDev, actor.TenantID, runID)
	if err != nil {
		return domain.AgentEvaluationRun{}, err
	}
	if err = s.Authorizer.Authorize(ctx, actor, domain.StudioView, run.AgentID); err != nil {
		if errors.Is(err, domain.ErrForbidden) {
			return domain.AgentEvaluationRun{}, fmt.Errorf("%w: evaluation run %d", domain.ErrNotFound, runID)
		}
		return domain.AgentEvaluationRun{}, err
	}
	return run, nil
}

// Trajectory reads one dev result's stored trajectory as role ran it: arm b (the candidate) of
// any run, or arm a (the baseline) of a comparison. A run of an agent the actor cannot VIEW, a
// gate example, and a result without a trajectory read as not found.
func (s *StudioEvaluation) Trajectory(ctx context.Context, actor domain.StudioActor, runID int64, exampleID string, role domain.EvaluationRole) (domain.CaseTrajectory, error) {
	if err := s.init(ctx); err != nil {
		return domain.CaseTrajectory{}, err
	}
	if runID <= 0 || strings.TrimSpace(exampleID) == "" || !role.Valid() {
		return domain.CaseTrajectory{}, fmt.Errorf("%w: run_id, case_id, and an arm are required", domain.ErrValidation)
	}
	if s.Outputs == nil {
		return domain.CaseTrajectory{}, fmt.Errorf("%w: reading trajectories is not composed", domain.ErrNotReady)
	}
	run, err := s.viewableRun(ctx, actor, runID)
	if err != nil {
		return domain.CaseTrajectory{}, err
	}
	if role == domain.RoleBaseline && run.Baseline == nil {
		return domain.CaseTrajectory{}, fmt.Errorf("%w: evaluation run %d has no arm a", domain.ErrNotFound, runID)
	}
	ref, err := s.Runs.Trajectory(ctx, domain.GoldenScopeDev, actor.TenantID, runID, exampleID, role)
	if err != nil {
		return domain.CaseTrajectory{}, err
	}
	content, err := s.Outputs.Read(ctx, ref)
	if err != nil {
		return domain.CaseTrajectory{}, fmt.Errorf("read trajectory of case %q: %w", exampleID, err)
	}
	if maxBytes := orDefault(s.MaxTrajectoryBytes, DefaultTrajectoryMaxBytes); len(content) > maxBytes {
		return domain.CaseTrajectory{}, fmt.Errorf("%w: trajectory of case %q exceeds %d bytes", domain.ErrValidation, exampleID, maxBytes)
	}
	if sha256Hex(content) != strings.ToLower(ref.Digest) {
		return domain.CaseTrajectory{}, fmt.Errorf("trajectory of case %q does not match its digest", exampleID)
	}
	events, err := decodeTrajectory(content)
	if err != nil {
		// A stored trajectory that does not decode is a server fault, so its validation class is not kept.
		return domain.CaseTrajectory{}, fmt.Errorf("stored trajectory of case %q is malformed: %v", exampleID, err)
	}
	return domain.CaseTrajectory{RunID: runID, ExampleID: exampleID, Role: role, Events: events}, nil
}
