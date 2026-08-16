package evaluation

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// GatewayJudge is a blinded pairwise LLM judge over ModelGateway: outputs are
// shown as A and B in a seed-derived order, with rubric and evidence only.
type GatewayJudge struct {
	Gateway   contract.ModelGateway
	Selection domain.ModelSelection
	// PromptVersion pins the judge prompt template and is part of the evaluator version.
	PromptVersion string
	// Seed drives presentation order; the same seed and pair always yield the same order.
	Seed int64
	// Metrics are scored per output; default correctness, groundedness, instruction following, safety.
	Metrics         []string
	MaxOutputTokens int64
}

var _ contract.JudgeEvaluator = (*GatewayJudge)(nil)

var defaultJudgeMetrics = []string{domain.MetricCorrectness, domain.MetricGroundedness, domain.MetricInstructionFollowing, domain.MetricSafety}

// Version identifies prompt template and judge model together.
func (judge *GatewayJudge) Version() domain.EvaluatorVersion {
	return domain.EvaluatorVersion{Kind: evaluatorKindJudge, Version: judge.PromptVersion + "@" + judge.Selection.Provider + "/" + judge.Selection.Model + "/" + judge.Selection.ModelVersion}
}

type judgeOutput struct {
	Label      string   `json:"label"`
	Text       string   `json:"text"`
	Evidence   []string `json:"evidence,omitempty"`
	Trajectory []string `json:"trajectory,omitempty"`
}

type judgePrompt struct {
	PromptVersion    string        `json:"prompt_version"`
	Task             string        `json:"task"`
	Rubric           string        `json:"rubric,omitempty"`
	ExpectedBehavior string        `json:"expected_behavior,omitempty"`
	Metrics          []string      `json:"metrics"`
	Outputs          []judgeOutput `json:"outputs"`
	ResponseFormat   string        `json:"response_format"`
}

type judgeResponse struct {
	Scores     map[string]map[string]float64 `json:"scores"`
	Preferred  string                        `json:"preferred"`
	Confidence float64                       `json:"confidence"`
	Rationale  string                        `json:"rationale"`
}

// Compare judges the blinded pair and maps A/B back to roles.
func (judge *GatewayJudge) Compare(ctx context.Context, pair domain.EvaluationPair) (domain.JudgeVerdict, error) {
	if judge.Gateway == nil || strings.TrimSpace(judge.PromptVersion) == "" || strings.TrimSpace(judge.Selection.Model) == "" {
		return domain.JudgeVerdict{}, fmt.Errorf("gateway judge: gateway, prompt version, and model selection are required")
	}
	if pair.Example.TenantID <= 0 || strings.TrimSpace(pair.Example.ExampleID) == "" {
		return domain.JudgeVerdict{}, fmt.Errorf("%w: judge pair needs a tenant-scoped example", domain.ErrValidation)
	}
	metrics := judge.Metrics
	if len(metrics) == 0 {
		metrics = defaultJudgeMetrics
	}
	swap := judge.candidateFirst(pair)
	first, second := pair.Baseline, pair.Candidate
	if swap {
		first, second = pair.Candidate, pair.Baseline
	}
	prompt := judgePrompt{
		PromptVersion:    judge.PromptVersion,
		Task:             "Score each output on every metric in [0,1] using only the rubric, expected behavior, and evidence shown. Then state which output is better overall, or tie.",
		Rubric:           pair.Example.RubricRef,
		ExpectedBehavior: string(pair.Example.ExpectedBehavior),
		Metrics:          metrics,
		Outputs:          []judgeOutput{blindOutput("A", first), blindOutput("B", second)},
		ResponseFormat:   `{"scores":{"A":{"<metric>":0.0},"B":{"<metric>":0.0}},"preferred":"A|B|tie","confidence":0.0,"rationale":""}`,
	}
	encoded, err := json.Marshal(prompt)
	if err != nil {
		return domain.JudgeVerdict{}, fmt.Errorf("gateway judge: encode prompt: %w", err)
	}
	digest := sha256Hex(encoded)
	result, err := judge.Gateway.Generate(ctx, judge.Selection, domain.ModelRequest{
		TenantContext:   domain.TenantContext{TenantID: pair.Example.TenantID},
		RequestID:       "judge-" + digest[:32],
		Prompt:          encoded,
		MaxOutputTokens: judge.MaxOutputTokens,
		Idempotent:      true,
	})
	if err != nil {
		return domain.JudgeVerdict{}, fmt.Errorf("gateway judge: %w", err)
	}
	var response judgeResponse
	if err := json.Unmarshal(result.Output, &response); err != nil {
		return domain.JudgeVerdict{}, fmt.Errorf("gateway judge: decode verdict: %w", err)
	}
	version := judge.Version()
	verdict := domain.JudgeVerdict{
		Confidence:  clamp01(response.Confidence),
		Rationale:   response.Rationale,
		InputDigest: digest,
	}
	scoresA, scoresB := judgeScores(metrics, response.Scores["A"], version), judgeScores(metrics, response.Scores["B"], version)
	if swap {
		verdict.Candidate, verdict.Baseline = scoresA, scoresB
	} else {
		verdict.Baseline, verdict.Candidate = scoresA, scoresB
	}
	switch strings.ToUpper(strings.TrimSpace(response.Preferred)) {
	case "A":
		verdict.Preferred = roleAt(swap, true)
	case "B":
		verdict.Preferred = roleAt(swap, false)
	}
	return verdict, nil
}

// candidateFirst decides presentation order from the seed and the two outputs
// only, so the order is independent of which arm produced which text.
func (judge *GatewayJudge) candidateFirst(pair domain.EvaluationPair) bool {
	baselineDigest, candidateDigest := sha256Hex(pair.Baseline.Output), sha256Hex(pair.Candidate.Output)
	low, high := baselineDigest, candidateDigest
	if low > high {
		low, high = high, low
	}
	hasher := fnv.New64a()
	var seed [8]byte
	binary.LittleEndian.PutUint64(seed[:], uint64(judge.Seed))
	hasher.Write(seed[:])
	hasher.Write([]byte(pair.Example.ExampleID))
	hasher.Write([]byte(low))
	hasher.Write([]byte(high))
	lowerFirst := hasher.Sum64()&1 == 0
	return (candidateDigest <= baselineDigest) == lowerFirst
}

func blindOutput(label string, evalCase domain.EvaluationCase) judgeOutput {
	output := judgeOutput{Label: label, Text: string(evalCase.Output)}
	for _, match := range evalCase.Retrieval {
		output.Evidence = append(output.Evidence, match.DocumentID+": "+string(match.Content))
	}
	for _, event := range evalCase.Trajectory {
		if event.Kind == domain.TrajectoryToolCall || event.Kind == domain.TrajectoryObservation {
			output.Trajectory = append(output.Trajectory, string(event.Kind)+" "+event.Name+": "+string(event.Payload))
		}
	}
	return output
}

func judgeScores(metrics []string, values map[string]float64, version domain.EvaluatorVersion) []domain.EvaluationScore {
	scores := make([]domain.EvaluationScore, 0, len(metrics))
	for _, metric := range metrics {
		value, ok := values[metric]
		if !ok {
			continue
		}
		scores = append(scores, domain.EvaluationScore{Metric: metric, Value: clamp01(value), Confidence: 1, Evaluator: version})
	}
	return scores
}

func roleAt(candidateFirst, first bool) domain.EvaluationRole {
	if candidateFirst == first {
		return domain.RoleCandidate
	}
	return domain.RoleBaseline
}

func clamp01(value float64) float64 {
	if math.IsNaN(value) {
		return 0
	}
	return math.Max(0, math.Min(1, value))
}

func fnvSum(text string) uint64 {
	hasher := fnv.New64a()
	hasher.Write([]byte(text))
	return hasher.Sum64()
}
