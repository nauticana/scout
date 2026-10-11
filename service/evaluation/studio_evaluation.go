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
	return s.GoldenCases.Cases(ctx, domain.GoldenScopeDev, actor.TenantID, agentID)
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
	return s.GoldenCases.Cases(ctx, domain.GoldenScopeDev, actor.TenantID, change.AgentID)
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
	set, err := s.GoldenCases.Cases(ctx, domain.GoldenScopeDev, actor.TenantID, agentID)
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
		frozen, err := s.Drafts.FreezeDraft(ctx, actor, request.AgentID)
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
	run, err := s.Runs.Run(ctx, domain.GoldenScopeDev, actor.TenantID, runID)
	if err != nil {
		return domain.AgentEvaluationPage{}, err
	}
	if err = s.Authorizer.Authorize(ctx, actor, domain.StudioView, run.AgentID); err != nil {
		if errors.Is(err, domain.ErrForbidden) {
			return domain.AgentEvaluationPage{}, fmt.Errorf("%w: evaluation run %d", domain.ErrNotFound, runID)
		}
		return domain.AgentEvaluationPage{}, err
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
