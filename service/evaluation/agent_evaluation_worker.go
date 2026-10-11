package evaluation

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/logger"
	"github.com/nauticana/keel/port"
	"github.com/nauticana/keel/worker"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/saturate"
	"github.com/nauticana/scout/internal/stage"
	"github.com/nauticana/scout/service/isolation"
	"github.com/nauticana/scout/service/scope"
)

// AgentEvaluationWorker is the keel leased queue worker that executes queued agent
// evaluations. A draft subject runs each example through the governed draft tester
// with the run's pinned definition; a release subject replays through the release
// case executor. Examples run with bounded concurrency until the run's cost ceiling,
// each result is recorded under the lease, and an example an earlier attempt
// recorded is not run again.
type AgentEvaluationWorker struct {
	worker.AbstractWorker
	Queue    contract.AgentEvaluationQueue
	Examples contract.GoldenSetStore
	// Payloads reads a draft example's input; the worker verifies its digest.
	Payloads contract.ObjectReader
	Drafts   contract.AgentDraftTestExecutor
	Releases contract.CaseExecutor
	// Evaluators score every executed example; each score names its evaluator version.
	Evaluators []contract.HeuristicEvaluator
	// Judge, when set, grades every example that has a rubric after the evaluators, as the
	// run's requester; its spend is the result's evaluator usage and counts against the
	// run's cost ceiling.
	Judge contract.RubricJudge
	// Concurrency bounds the examples of one run in flight; values below two run sequentially.
	Concurrency int
	// MaxRunCostMinorUnits stops a run once its settled cost reaches it; examples already
	// in flight finish, so a run may exceed it by at most Concurrency examples' cost.
	MaxRunCostMinorUnits int64
	// Currency is the ISO code every priced result must carry; another currency fails the run.
	Currency string
	// ExtraQueries joins the worker's named SQL, for services sharing its query service.
	ExtraQueries map[string]string

	lease                   time.Duration
	queries                 map[string]string
	pending, claim, reclaim string
}

var _ worker.LeasedQueueWorker = (*AgentEvaluationWorker)(nil)

// NewAgentEvaluationWorker builds the queue SQL; lease is renewed on every recorded
// result, so it must outlast the slowest single example.
func NewAgentEvaluationWorker(lease time.Duration, batch, maxAttempts int) (*AgentEvaluationWorker, error) {
	if lease < time.Second || batch <= 0 || maxAttempts <= 0 {
		return nil, fmt.Errorf("%w: agent evaluation worker needs a lease of at least a second, a positive batch, and an attempt ceiling", domain.ErrValidation)
	}
	w := &AgentEvaluationWorker{lease: lease}
	w.queries, w.pending, w.claim, w.reclaim = AgentEvaluationQueueQueries(lease, batch, maxAttempts)
	return w, nil
}

func (w *AgentEvaluationWorker) GetOLTPQueries() map[string]string {
	queries := maps.Clone(w.queries)
	maps.Copy(queries, w.ExtraQueries)
	return queries
}

func (w *AgentEvaluationWorker) LeaseClaim() bool { return true }

func (w *AgentEvaluationWorker) QueueQueries() (pending, claim, reclaim, name string) {
	return w.pending, w.claim, w.reclaim, "agent-evaluation"
}

// HandleJob executes one claimed run; the claim row is id, partner_id, lease_token.
func (w *AgentEvaluationWorker) HandleJob(ctx context.Context, _ logger.ApplicationLogger, _ port.DatabaseRepository, _ port.QuotaService, _ port.QueryService, _ int64, row []any) error {
	if len(row) < 3 {
		return fmt.Errorf("agent evaluation claim row: expected 3 columns, got %d", len(row))
	}
	return w.Execute(ctx, domain.AgentEvaluationLease{TenantID: common.AsInt64(row[1]), RunID: common.AsInt64(row[0]), Token: common.AsInt64(row[2]), TTL: w.lease})
}

func (w *AgentEvaluationWorker) validate() error {
	switch {
	case w.lease < time.Second || w.queries == nil:
		return fmt.Errorf("agent evaluation worker: build it with NewAgentEvaluationWorker")
	case w.Queue == nil || w.Examples == nil:
		return fmt.Errorf("agent evaluation worker: queue and golden set store are required")
	case w.Concurrency < 0:
		return fmt.Errorf("%w: concurrency cannot be negative", domain.ErrValidation)
	case w.MaxRunCostMinorUnits <= 0 || len(w.Currency) != 3:
		return fmt.Errorf("%w: agent evaluation worker needs a positive run cost ceiling and its ISO currency", domain.ErrValidation)
	}
	for _, evaluator := range w.Evaluators {
		if evaluator == nil {
			return fmt.Errorf("agent evaluation worker: evaluators cannot be nil")
		}
	}
	return nil
}

// caseFailure is a failure of one example's execution or scoring; it fails the run.
// unsettled marks an executed example whose usage the run's totals could not hold;
// recorded is the reason an earlier attempt stored for an unscored example.
type caseFailure struct {
	example   domain.GoldenExample
	err       error
	unsettled bool
	recorded  string
}

func (failure *caseFailure) Error() string {
	return fmt.Sprintf("example %q: %v", failure.example.ExampleID, failure.err)
}

func (failure *caseFailure) Unwrap() error { return failure.err }

// Execute runs every unrecorded example of the leased run and settles it. A lost
// lease, a store failure, or cancellation leaves the run to the lease's reclaim; an
// example an earlier attempt recorded unscored fails the run.
func (w *AgentEvaluationWorker) Execute(ctx context.Context, lease domain.AgentEvaluationLease) error {
	if err := w.validate(); err != nil {
		return err
	}
	claim, err := w.Queue.Load(ctx, lease)
	if err != nil {
		return fmt.Errorf("load evaluation run %d: %w", lease.RunID, err)
	}
	run := claim.Run
	switch {
	case run.Subject.Kind == domain.SubjectDraft && (claim.Draft == nil || w.Drafts == nil || w.Payloads == nil):
		return w.Queue.Finish(ctx, lease, domain.EvaluationFailed, "draft evaluation is not composed")
	case run.Subject.Kind == domain.SubjectRelease && w.Releases == nil:
		return w.Queue.Finish(ctx, lease, domain.EvaluationFailed, "release evaluation is not composed")
	}
	var draftLimits domain.ExecutionLimits
	if run.Subject.Kind == domain.SubjectDraft {
		if draftLimits, err = pinnedDraftLimits(claim); err != nil {
			return w.Queue.Finish(ctx, lease, domain.EvaluationFailed, "draft limits cannot be read: "+stage.ErrorClass(err))
		}
	}
	examples, err := w.Examples.ListExamples(ctx, run.Scope, run.TenantID, run.GoldenSetID, run.SetVersion)
	if err != nil {
		return fmt.Errorf("list examples of run %d: %w", lease.RunID, err)
	}
	for _, example := range examples {
		if reason, unscored := claim.Unscored[example.ExampleID]; unscored {
			return w.Queue.Finish(ctx, lease, domain.EvaluationFailed, failureReason(&caseFailure{example: example, recorded: reason}))
		}
	}
	if len(claim.Unscored) > 0 {
		return w.Queue.Finish(ctx, lease, domain.EvaluationFailed, "a recorded example was not scored")
	}
	var spent int64
	for _, usage := range claim.Recorded {
		if usage.CostMinorUnits > 0 && usage.Currency != w.Currency {
			return w.Queue.Finish(ctx, lease, domain.EvaluationFailed, "recorded cost is in "+usage.Currency+", not "+w.Currency)
		}
		spent = saturate.Add(spent, usage.CostMinorUnits)
	}
	pending := make([]domain.GoldenExample, 0, len(examples))
	for _, example := range examples {
		if _, done := claim.Recorded[example.ExampleID]; !done {
			pending = append(pending, example)
		}
	}
	var mu sync.Mutex
	stopped := false
	err = runOrdered(ctx, len(pending), w.Concurrency, func(ctx context.Context, i int) error {
		mu.Lock()
		if spent >= w.MaxRunCostMinorUnits {
			stopped = true
		}
		halt := stopped
		mu.Unlock()
		if halt {
			return nil
		}
		example := pending[i]
		evalCase, err := w.execute(ctx, claim, draftLimits, example)
		if err != nil {
			return &caseFailure{example: example, err: err}
		}
		if err = w.settleable(evalCase); err != nil {
			return &caseFailure{example: example, err: err, unsettled: true}
		}
		result, scoreErr := w.score(ctx, claim.Run.RequestedBy, example, evalCase)
		// An executed example's usage is settled even when a sibling's failure cancelled the run.
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lease.TTL)
		err = w.Queue.Record(recordCtx, lease, result)
		cancel()
		if errors.Is(err, domain.ErrValidation) {
			return &caseFailure{example: example, err: err, unsettled: true}
		}
		if err != nil {
			return fmt.Errorf("record example %q: %w", example.ExampleID, err)
		}
		mu.Lock()
		spent = saturate.Add(spent, saturate.Add(result.Usage.CostMinorUnits, result.EvaluatorUsage.CostMinorUnits))
		mu.Unlock()
		if scoreErr != nil {
			return &caseFailure{example: example, err: scoreErr, unsettled: errors.Is(scoreErr, errJudgeSpendUnsettled)}
		}
		return nil
	})
	var failure *caseFailure
	switch {
	case errors.As(err, &failure) && ctx.Err() == nil:
		return w.Queue.Finish(ctx, lease, domain.EvaluationFailed, failureReason(failure))
	case err != nil:
		return err
	case stopped:
		return w.Queue.Finish(ctx, lease, domain.EvaluationStoppedEarly, fmt.Sprintf("cost ceiling of %d %s minor units reached", w.MaxRunCostMinorUnits, w.Currency))
	default:
		return w.Queue.Finish(ctx, lease, domain.EvaluationCompleted, "")
	}
}

// failureReason names the failing example and the error class, never provider text;
// a hidden example is not named, since the run may be read in the dev scope.
func failureReason(failure *caseFailure) string {
	class := failure.recorded
	if class == "" {
		class = stage.ErrorClass(failure.err)
	}
	reason := "example " + failure.example.ExampleID + " failed: " + class
	if failure.example.Hidden {
		reason = "a gate example failed: " + class
	}
	if failure.unsettled {
		reason += "; its usage was not settled"
	}
	return reason
}

// pinnedDraftLimits reads the limits frozen with the run's draft, refusing settings its
// definition does not digest.
func pinnedDraftLimits(claim domain.AgentEvaluationClaim) (domain.ExecutionLimits, error) {
	if claim.Draft.SettingsDigest == "" || scope.SettingsDigest(claim.DraftSettings) != claim.Draft.SettingsDigest {
		return domain.ExecutionLimits{}, fmt.Errorf("%w: pinned draft settings do not match the definition", domain.ErrConflict)
	}
	return isolation.FrozenExecutionLimits(claim.DraftSettings)
}

func (w *AgentEvaluationWorker) execute(ctx context.Context, claim domain.AgentEvaluationClaim, draftLimits domain.ExecutionLimits, example domain.GoldenExample) (domain.EvaluationCase, error) {
	if example.TenantID != claim.Run.TenantID {
		return domain.EvaluationCase{}, fmt.Errorf("%w: example belongs to another tenant", domain.ErrForbidden)
	}
	var evalCase domain.EvaluationCase
	var err error
	if claim.Run.Subject.Kind == domain.SubjectDraft {
		evalCase, err = w.executeDraft(ctx, claim, draftLimits, example)
	} else {
		evalCase, err = w.Releases.Execute(ctx, domain.EvaluationSubject{AgentID: claim.Run.AgentID, AgentVersion: claim.Run.Subject.Version}, example, nil)
	}
	if err != nil {
		return domain.EvaluationCase{}, err
	}
	evalCase.Example, evalCase.Role = example, domain.RoleCandidate
	return evalCase, nil
}

// settleable refuses an outcome the run's totals cannot hold: negative usage or latency, or a priced result in another currency.
func (w *AgentEvaluationWorker) settleable(evalCase domain.EvaluationCase) error {
	usage := evalCase.Usage
	if usage.CostMinorUnits < 0 || usage.InputTokens < 0 || usage.OutputTokens < 0 || evalCase.Latency < 0 ||
		usage.CostMinorUnits > 0 && usage.Currency != w.Currency {
		return fmt.Errorf("%w: example usage of %d %s is not a non-negative amount in %s", domain.ErrValidation, usage.CostMinorUnits, usage.Currency, w.Currency)
	}
	return nil
}

// score runs every evaluator, then the judge as principal; when one fails the result
// still carries the subject's and the judge's usage, with no scores and the error class.
func (w *AgentEvaluationWorker) score(ctx context.Context, principal domain.PrincipalRef, example domain.GoldenExample, evalCase domain.EvaluationCase) (domain.EvaluationResult, error) {
	result := domain.EvaluationResult{ExampleID: example.ExampleID, Role: domain.RoleCandidate, Latency: evalCase.Latency, Usage: evalCase.Usage}
	for _, evaluator := range w.Evaluators {
		found, err := evaluator.Evaluate(ctx, evalCase)
		if err != nil {
			result.Scores, result.Reason = nil, "scoring failed: "+stage.ErrorClass(err)
			return result, fmt.Errorf("evaluator %s: %w", evaluator.Version().Version, err)
		}
		result.Scores = append(result.Scores, found...)
	}
	if w.Judge == nil || stopped(evalCase) {
		return result, nil
	}
	verdict, err := w.Judge.Grade(ctx, principal, evalCase)
	var spendErr error
	result.EvaluatorUsage, spendErr = w.judgeSpend(verdict)
	if err = errors.Join(spendErr, err); err != nil {
		result.Scores, result.Reason = nil, "judging failed: "+stage.ErrorClass(err)
		return result, fmt.Errorf("rubric judge %s: %w", w.Judge.Version().Version, err)
	}
	result.Scores = append(result.Scores, verdict.Scores...)
	return result, nil
}

// errJudgeSpendUnsettled marks judge spend the run's totals cannot hold.
var errJudgeSpendUnsettled = fmt.Errorf("%w: judge spend cannot be settled", domain.ErrValidation)

// judgeSpend is the judge's spend as the result's evaluator usage. Unpriced spend or
// spend in another currency keeps its tokens only, and negative spend keeps nothing;
// both fail, since the run's cost ceiling could not count them.
func (w *AgentEvaluationWorker) judgeSpend(verdict domain.RubricVerdict) (domain.Usage, error) {
	spend := verdict.Usage
	if spend.InputTokens < 0 || spend.OutputTokens < 0 || spend.SearchQueries < 0 || spend.CostMinorUnits < 0 {
		return domain.Usage{}, errJudgeSpendUnsettled
	}
	if verdict.Unpriced || spend.CostMinorUnits > 0 && spend.Currency != w.Currency {
		return domain.Usage{InputTokens: spend.InputTokens, OutputTokens: spend.OutputTokens, SearchQueries: spend.SearchQueries}, errJudgeSpendUnsettled
	}
	if spend.CostMinorUnits == 0 {
		spend.Currency = ""
	}
	return spend, nil
}

// executeDraft runs the example's input as the task of a governed draft test of the pinned
// definition, bounded by the limits frozen with it.
func (w *AgentEvaluationWorker) executeDraft(ctx context.Context, claim domain.AgentEvaluationClaim, limits domain.ExecutionLimits, example domain.GoldenExample) (domain.EvaluationCase, error) {
	input, err := w.Payloads.Read(ctx, example.Payload)
	if err != nil {
		return domain.EvaluationCase{}, fmt.Errorf("read input: %w", err)
	}
	if sha256Hex(input) != strings.ToLower(example.Payload.Digest) {
		return domain.EvaluationCase{}, fmt.Errorf("%w: input does not match its digest", domain.ErrValidation)
	}
	actor := domain.StudioActor{TenantID: claim.Run.TenantID, Principal: claim.Run.RequestedBy}
	result, err := w.Drafts.Execute(ctx, actor, domain.AgentTestRequest{AgentID: claim.Run.AgentID, LanguageCode: example.Language, Task: string(input), Limits: limits}, *claim.Draft)
	var spent interface{ SpentUsage() domain.Usage }
	if errors.Is(err, domain.ErrExecutionLimit) && ctx.Err() == nil {
		// A stop is scored only with the usage it settled, so it counts toward the cost ceiling.
		if !errors.As(err, &spent) {
			return domain.EvaluationCase{}, fmt.Errorf("draft stopped by its limit reported no settled usage: %w", err)
		}
		return domain.EvaluationCase{
			Usage:      spent.SpentUsage(),
			Trajectory: []domain.TrajectoryEvent{{Sequence: 1, Kind: domain.TrajectoryState, Name: stage.ErrorClass(err), Failed: true}},
		}, nil
	}
	if err != nil {
		return domain.EvaluationCase{}, err
	}
	return domain.EvaluationCase{
		Output: []byte(result.Output), Latency: time.Duration(result.LatencyMs) * time.Millisecond, Usage: result.Usage,
	}, nil
}

// stopped reports a case that ended in a failed state, such as a limit or budget stop, which
// leaves no complete answer for the judge to grade.
func stopped(evalCase domain.EvaluationCase) bool {
	for _, event := range evalCase.Trajectory {
		if event.Kind == domain.TrajectoryState && event.Failed {
			return true
		}
	}
	return false
}
