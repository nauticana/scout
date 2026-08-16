package evaluation

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/lru"
)

const (
	runStatusRunning      = "running"
	runStatusCompleted    = "completed"
	runStatusStoppedEarly = "stopped_early"
	runStatusFailed       = "failed"
)

// Runner replays baseline and candidate on identical examples, scores both arms
// with heuristics, an optional blinded judge, and an optional trajectory
// evaluator, routes doubtful pairs to human review, and stops early once the
// primary effect is decided.
type Runner struct {
	Executor   contract.CaseExecutor
	Heuristics []contract.HeuristicEvaluator
	Judge      contract.JudgeEvaluator
	Trajectory contract.TrajectoryEvaluator
	Review     contract.HumanReviewQueue
	// Results persists runs and results when set.
	Results contract.EvaluationResultStore
	Scorer  *PairedScorer
	Scope   domain.GoldenScope
	// Concurrency bounds parallel examples; values below two run sequentially.
	Concurrency int
	// EarlyStopBatch is how many examples run between sequential checks; zero disables early stopping.
	EarlyStopBatch int
	// MinJudgeConfidence routes verdicts below it to human review; zero means 0.6.
	MinJudgeConfidence float64
	// HighRiskTiers are always routed to human review.
	HighRiskTiers []string
	// JudgeCacheSize bounds cached judge verdicts by input digest; zero means 1024.
	JudgeCacheSize int
	JudgeCacheTTL  time.Duration
	Now            func() time.Time

	once  sync.Once
	cache *lru.Cache[string, domain.JudgeVerdict]
}

var _ contract.EvaluationRunner = (*Runner)(nil)

func (runner *Runner) now() time.Time {
	if runner.Now != nil {
		return runner.Now()
	}
	return time.Now()
}

func (runner *Runner) validate(manifest domain.EvaluationManifest, examples []domain.GoldenExample) error {
	if runner.Executor == nil || runner.Scorer == nil || runner.Review == nil {
		return fmt.Errorf("evaluation runner: executor, scorer, and review queue are required")
	}
	if runner.Concurrency < 0 || runner.EarlyStopBatch < 0 || runner.JudgeCacheSize < 0 || runner.JudgeCacheTTL < 0 || runner.MinJudgeConfidence < 0 || runner.MinJudgeConfidence > 1 {
		return fmt.Errorf("%w: runner limits cannot be negative and judge confidence must be in [0,1]", domain.ErrValidation)
	}
	if err := (&ManifestBuilder{}).Verify(manifest); err != nil {
		return err
	}
	if len(examples) == 0 {
		return fmt.Errorf("%w: at least one example is required", domain.ErrValidation)
	}
	seen := make(map[string]struct{}, len(examples))
	for _, example := range examples {
		if strings.TrimSpace(example.ExampleID) == "" || example.TenantID != manifest.TenantID {
			return fmt.Errorf("%w: example id is required and must belong to the manifest tenant", domain.ErrValidation)
		}
		if _, dup := seen[example.ExampleID]; dup {
			return fmt.Errorf("%w: duplicate example %q", domain.ErrValidation, example.ExampleID)
		}
		seen[example.ExampleID] = struct{}{}
	}
	return nil
}

// Run evaluates every example and returns the paired summary; on failure the
// run is recorded as failed and the error returned.
func (runner *Runner) Run(ctx context.Context, manifest domain.EvaluationManifest, examples []domain.GoldenExample) (domain.EvaluationSummary, error) {
	if err := runner.validate(manifest, examples); err != nil {
		return domain.EvaluationSummary{}, err
	}
	runner.once.Do(func() {
		size := runner.JudgeCacheSize
		if size == 0 {
			size = 1024
		}
		runner.cache = lru.New[string, domain.JudgeVerdict](size, runner.now)
	})
	scope := runner.Scope
	if scope == "" {
		scope = domain.GoldenScopeDev
	}
	run := domain.EvaluationRun{TenantID: manifest.TenantID, ManifestID: manifest.ManifestID, Scope: scope, Status: runStatusRunning, StartedAt: runner.now()}
	if runner.Results != nil {
		runID, err := runner.Results.StartRun(ctx, run)
		if err != nil {
			return domain.EvaluationSummary{}, fmt.Errorf("start evaluation run: %w", err)
		}
		run.RunID = runID
	}
	summary, err := runner.evaluateAll(ctx, manifest, examples, &run)
	run.CompletedAt = runner.now()
	run.Samples = summary.Samples
	run.Usage = summary.Usage
	switch {
	case err != nil:
		run.Status = runStatusFailed
	case summary.StoppedEarly:
		run.Status = runStatusStoppedEarly
	default:
		run.Status = runStatusCompleted
	}
	if runner.Results != nil {
		if finishErr := runner.Results.FinishRun(ctx, run); finishErr != nil && err == nil {
			err = fmt.Errorf("finish evaluation run: %w", finishErr)
		}
	}
	if err != nil {
		return domain.EvaluationSummary{}, err
	}
	return summary, nil
}

type pairOutcome struct {
	results []domain.EvaluationResult
	review  *domain.HumanReviewItem
}

func (runner *Runner) evaluateAll(ctx context.Context, manifest domain.EvaluationManifest, examples []domain.GoldenExample, run *domain.EvaluationRun) (domain.EvaluationSummary, error) {
	batch := runner.EarlyStopBatch
	if batch == 0 {
		batch = len(examples)
	}
	var results []domain.EvaluationResult
	var summary domain.EvaluationSummary
	for start := 0; start < len(examples); start += batch {
		end := min(start+batch, len(examples))
		outcomes := make([]pairOutcome, end-start)
		err := runOrdered(ctx, end-start, runner.Concurrency, func(ctx context.Context, i int) error {
			outcome, err := runner.evaluatePair(ctx, manifest, examples[start+i])
			outcomes[i] = outcome
			return err
		})
		if err != nil {
			return domain.EvaluationSummary{}, err
		}
		var batchResults []domain.EvaluationResult
		for _, outcome := range outcomes {
			batchResults = append(batchResults, outcome.results...)
			if outcome.review != nil {
				if err := runner.Review.Enqueue(ctx, *outcome.review); err != nil {
					return domain.EvaluationSummary{}, fmt.Errorf("enqueue human review: %w", err)
				}
			}
		}
		if runner.Results != nil {
			if err := runner.Results.PutResults(ctx, manifest.TenantID, run.RunID, batchResults); err != nil {
				return domain.EvaluationSummary{}, fmt.Errorf("persist evaluation results: %w", err)
			}
		}
		results = append(results, batchResults...)
		summary, err = runner.Scorer.Score(manifest.ManifestID, examples, results)
		if err != nil {
			return domain.EvaluationSummary{}, err
		}
		if runner.EarlyStopBatch > 0 && end < len(examples) && runner.Scorer.Decided(summary) {
			summary.StoppedEarly = true
			break
		}
	}
	return summary, nil
}

// evaluatePair replays both arms; the candidate reuses the baseline's retrieval
// whenever both pin the same knowledge and index so only the changed component varies.
func (runner *Runner) evaluatePair(ctx context.Context, manifest domain.EvaluationManifest, example domain.GoldenExample) (pairOutcome, error) {
	baseline, err := runner.Executor.Execute(ctx, manifest.Baseline, example, nil)
	if err != nil {
		return pairOutcome{}, fmt.Errorf("execute baseline for %q: %w", example.ExampleID, err)
	}
	var preserved []domain.KnowledgeMatch
	if sameRetrievalConfig(manifest.Baseline, manifest.Candidate) {
		preserved = append([]domain.KnowledgeMatch{}, baseline.Retrieval...)
	}
	candidate, err := runner.Executor.Execute(ctx, manifest.Candidate, example, preserved)
	if err != nil {
		return pairOutcome{}, fmt.Errorf("execute candidate for %q: %w", example.ExampleID, err)
	}
	baseline.ManifestID, baseline.Example, baseline.Role = manifest.ManifestID, example, domain.RoleBaseline
	candidate.ManifestID, candidate.Example, candidate.Role = manifest.ManifestID, example, domain.RoleCandidate

	baseScores, err := runner.deterministicScores(ctx, baseline)
	if err != nil {
		return pairOutcome{}, err
	}
	candScores, err := runner.deterministicScores(ctx, candidate)
	if err != nil {
		return pairOutcome{}, err
	}
	var reasons []string
	var verdict domain.JudgeVerdict
	if runner.Judge != nil {
		pair := domain.EvaluationPair{Example: example, Baseline: baseline, Candidate: candidate}
		verdict, err = runner.judge(ctx, pair)
		if err != nil {
			return pairOutcome{}, fmt.Errorf("judge %q: %w", example.ExampleID, err)
		}
		baseScores = append(baseScores, verdict.Baseline...)
		candScores = append(candScores, verdict.Candidate...)
		if verdict.Confidence < runner.minJudgeConfidence() {
			reasons = append(reasons, fmt.Sprintf("judge confidence %.2f below %.2f", verdict.Confidence, runner.minJudgeConfidence()))
		}
		if disagrees(baseScores) || disagrees(candScores) {
			reasons = append(reasons, "heuristic and judge disagree")
		}
	}
	for _, tier := range runner.HighRiskTiers {
		if example.RiskTier == tier {
			reasons = append(reasons, "high risk tier "+tier)
			break
		}
	}
	reason := strings.Join(reasons, "; ")
	outcome := pairOutcome{results: []domain.EvaluationResult{
		{ManifestID: manifest.ManifestID, ExampleID: example.ExampleID, Role: domain.RoleBaseline, Scores: baseScores, Latency: baseline.Latency, Usage: baseline.Usage, NeedsHumanReview: reason != "", Reason: reason},
		{ManifestID: manifest.ManifestID, ExampleID: example.ExampleID, Role: domain.RoleCandidate, Scores: candScores, Latency: candidate.Latency, Usage: candidate.Usage, NeedsHumanReview: reason != "", Reason: reason},
	}}
	if reason != "" {
		outcome.review = &domain.HumanReviewItem{
			ItemID:     sha256Hex([]byte(manifest.ManifestID + "|" + example.ExampleID))[:32],
			TenantID:   manifest.TenantID,
			ManifestID: manifest.ManifestID,
			ExampleID:  example.ExampleID,
			RiskTier:   example.RiskTier,
			Reason:     reason,
			Pair:       domain.EvaluationPair{Example: example, Baseline: baseline, Candidate: candidate},
			Verdict:    verdict,
			EnqueuedAt: runner.now(),
		}
	}
	return outcome, nil
}

func (runner *Runner) deterministicScores(ctx context.Context, evalCase domain.EvaluationCase) ([]domain.EvaluationScore, error) {
	var scores []domain.EvaluationScore
	for _, heuristic := range runner.Heuristics {
		if heuristic == nil {
			return nil, fmt.Errorf("evaluation runner: heuristics cannot be nil")
		}
		found, err := heuristic.Evaluate(ctx, evalCase)
		if err != nil {
			return nil, fmt.Errorf("heuristic %s on %q: %w", heuristic.Version().Version, evalCase.Example.ExampleID, err)
		}
		scores = append(scores, found...)
	}
	if runner.Trajectory != nil && len(evalCase.Trajectory) > 0 {
		found, err := runner.Trajectory.Score(ctx, evalCase)
		if err != nil {
			return nil, fmt.Errorf("trajectory %q: %w", evalCase.Example.ExampleID, err)
		}
		scores = append(scores, found...)
	}
	return scores, nil
}

// judge serves identical blinded inputs from the digest cache so replays never pay twice.
func (runner *Runner) judge(ctx context.Context, pair domain.EvaluationPair) (domain.JudgeVerdict, error) {
	key := judgeCacheKey(runner.Judge.Version(), pair)
	if verdict, ok := runner.cache.Get(key); ok {
		return verdict, nil
	}
	verdict, err := runner.Judge.Compare(ctx, pair)
	if err != nil {
		return domain.JudgeVerdict{}, err
	}
	runner.cache.Set(key, verdict, runner.JudgeCacheTTL)
	return verdict, nil
}

func (runner *Runner) minJudgeConfidence() float64 {
	if runner.MinJudgeConfidence > 0 {
		return runner.MinJudgeConfidence
	}
	return 0.6
}

func judgeCacheKey(version domain.EvaluatorVersion, pair domain.EvaluationPair) string {
	parts := []string{version.Kind, version.Version, pair.Example.ExampleID, pair.Example.RubricRef, sha256Hex(pair.Example.ExpectedBehavior),
		sha256Hex(pair.Baseline.Output), sha256Hex(pair.Candidate.Output), retrievalDigest(pair.Baseline.Retrieval), retrievalDigest(pair.Candidate.Retrieval)}
	return sha256Hex([]byte(strings.Join(parts, "\x1f")))
}

func retrievalDigest(matches []domain.KnowledgeMatch) string {
	var builder strings.Builder
	for _, match := range matches {
		builder.WriteString(match.ChunkID)
		builder.WriteString("\x1f")
		builder.WriteString(sha256Hex(match.Content))
		builder.WriteString("\x1e")
	}
	return sha256Hex([]byte(builder.String()))
}

func sameRetrievalConfig(baseline, candidate domain.EvaluationSubject) bool {
	return baseline.Versions.Knowledge == candidate.Versions.Knowledge &&
		baseline.Versions.Index == candidate.Versions.Index &&
		baseline.IndexGeneration == candidate.IndexGeneration
}

// disagrees reports a heuristic and a judge more than half a scale apart on the same metric.
func disagrees(scores []domain.EvaluationScore) bool {
	byMetric := make(map[string]map[string]float64)
	for _, score := range scores {
		if byMetric[score.Metric] == nil {
			byMetric[score.Metric] = make(map[string]float64)
		}
		byMetric[score.Metric][score.Evaluator.Kind] = score.Value
	}
	for _, kinds := range byMetric {
		heuristic, hasHeuristic := kinds[evaluatorKindHeuristic]
		judge, hasJudge := kinds[evaluatorKindJudge]
		if hasHeuristic && hasJudge && math.Abs(heuristic-judge) > 0.5 {
			return true
		}
	}
	return false
}

// runOrdered runs fn for every index with bounded concurrency; the first error cancels the rest.
func runOrdered(ctx context.Context, count, concurrency int, fn func(context.Context, int) error) error {
	if count == 0 {
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	workers := min(max(concurrency, 1), count)
	jobs := make(chan int)
	var wg sync.WaitGroup
	var errOnce sync.Once
	var firstErr error
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for index := range jobs {
				if runCtx.Err() != nil {
					return
				}
				if err := fn(runCtx, index); err != nil {
					errOnce.Do(func() {
						firstErr = err
						cancel()
					})
					return
				}
			}
		}()
	}
dispatch:
	for index := range count {
		select {
		case jobs <- index:
		case <-runCtx.Done():
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}
