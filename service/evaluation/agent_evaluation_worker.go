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
	"github.com/nauticana/scout/internal/stage"
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
// unsettled marks an executed example whose usage the run's totals could not hold.
type caseFailure struct {
	example   domain.GoldenExample
	err       error
	unsettled bool
}

func (failure *caseFailure) Error() string {
	return fmt.Sprintf("example %q: %v", failure.example.ExampleID, failure.err)
}

func (failure *caseFailure) Unwrap() error { return failure.err }

// Execute runs every unrecorded example of the leased run and settles it. A lost
// lease, a store failure, or cancellation leaves the run to the lease's reclaim.
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
	examples, err := w.Examples.ListExamples(ctx, run.Scope, run.TenantID, run.GoldenSetID, run.SetVersion)
	if err != nil {
		return fmt.Errorf("list examples of run %d: %w", lease.RunID, err)
	}
	var spent int64
	for _, usage := range claim.Recorded {
		if usage.CostMinorUnits > 0 && usage.Currency != w.Currency {
			return w.Queue.Finish(ctx, lease, domain.EvaluationFailed, "recorded cost is in "+usage.Currency+", not "+w.Currency)
		}
		spent += usage.CostMinorUnits
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
		evalCase, err := w.execute(ctx, claim, example)
		if err != nil {
			return &caseFailure{example: example, err: err}
		}
		if err = w.settleable(evalCase); err != nil {
			return &caseFailure{example: example, err: err, unsettled: true}
		}
		result, scoreErr := w.score(ctx, example, evalCase)
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
		spent += result.Usage.CostMinorUnits
		mu.Unlock()
		if scoreErr != nil {
			return &caseFailure{example: example, err: scoreErr}
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
	reason := "example " + failure.example.ExampleID + " failed: " + stage.ErrorClass(failure.err)
	if failure.example.Hidden {
		reason = "a gate example failed: " + stage.ErrorClass(failure.err)
	}
	if failure.unsettled {
		reason += "; its usage was not settled"
	}
	return reason
}

func (w *AgentEvaluationWorker) execute(ctx context.Context, claim domain.AgentEvaluationClaim, example domain.GoldenExample) (domain.EvaluationCase, error) {
	var evalCase domain.EvaluationCase
	var err error
	if claim.Run.Subject.Kind == domain.SubjectDraft {
		evalCase, err = w.executeDraft(ctx, claim, example)
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

// score runs every evaluator; when one fails the result still carries the usage, with no scores and the error class.
func (w *AgentEvaluationWorker) score(ctx context.Context, example domain.GoldenExample, evalCase domain.EvaluationCase) (domain.EvaluationResult, error) {
	result := domain.EvaluationResult{ExampleID: example.ExampleID, Role: domain.RoleCandidate, Latency: evalCase.Latency, Usage: evalCase.Usage}
	for _, evaluator := range w.Evaluators {
		found, err := evaluator.Evaluate(ctx, evalCase)
		if err != nil {
			result.Scores, result.Reason = nil, "scoring failed: "+stage.ErrorClass(err)
			return result, fmt.Errorf("evaluator %s: %w", evaluator.Version().Version, err)
		}
		result.Scores = append(result.Scores, found...)
	}
	return result, nil
}

// executeDraft runs the example's input as the task of a governed draft test of the pinned definition.
func (w *AgentEvaluationWorker) executeDraft(ctx context.Context, claim domain.AgentEvaluationClaim, example domain.GoldenExample) (domain.EvaluationCase, error) {
	input, err := w.Payloads.Read(ctx, example.Payload)
	if err != nil {
		return domain.EvaluationCase{}, fmt.Errorf("read input: %w", err)
	}
	if sha256Hex(input) != strings.ToLower(example.Payload.Digest) {
		return domain.EvaluationCase{}, fmt.Errorf("%w: input does not match its digest", domain.ErrValidation)
	}
	actor := domain.StudioActor{TenantID: claim.Run.TenantID, Principal: claim.Run.RequestedBy}
	result, err := w.Drafts.Execute(ctx, actor, domain.AgentTestRequest{AgentID: claim.Run.AgentID, LanguageCode: example.Language, Task: string(input)}, *claim.Draft)
	if err != nil {
		return domain.EvaluationCase{}, err
	}
	return domain.EvaluationCase{
		Output: []byte(result.Output), Latency: time.Duration(result.LatencyMs) * time.Millisecond, Usage: result.Usage,
	}, nil
}
