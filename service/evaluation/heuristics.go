package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	evaluatorKindHeuristic  = "heuristic"
	evaluatorKindJudge      = "judge"
	evaluatorKindTrajectory = "trajectory"
	evaluatorKindRetrieval  = "retrieval"
)

// RegexHeuristic scores 1 when the output matches Pattern (or does not, when Forbidden).
type RegexHeuristic struct {
	Revision string
	Pattern  *regexp.Regexp
	// Metric defaults to MetricPatternMatch.
	Metric string
	// Forbidden inverts the rule: a match is a failure.
	Forbidden bool
	// Critical marks a failure as promotion-blocking, e.g. a leaked secret pattern.
	Critical bool
}

var _ contract.HeuristicEvaluator = (*RegexHeuristic)(nil)

// Version identifies this heuristic revision.
func (heuristic *RegexHeuristic) Version() domain.EvaluatorVersion {
	return domain.EvaluatorVersion{Kind: evaluatorKindHeuristic, Version: heuristic.Revision}
}

// Evaluate returns one score for the configured metric.
func (heuristic *RegexHeuristic) Evaluate(_ context.Context, evalCase domain.EvaluationCase) ([]domain.EvaluationScore, error) {
	if heuristic.Pattern == nil || strings.TrimSpace(heuristic.Revision) == "" {
		return nil, fmt.Errorf("regex heuristic: version and pattern are required")
	}
	matched := heuristic.Pattern.Match(evalCase.Output)
	passed := matched != heuristic.Forbidden
	return []domain.EvaluationScore{scoreOf(metricOr(heuristic.Metric, domain.MetricPatternMatch), passed, heuristic.Version(), heuristic.Critical,
		fmt.Sprintf("pattern %q matched=%t forbidden=%t", heuristic.Pattern.String(), matched, heuristic.Forbidden))}, nil
}

// SchemaHeuristic scores 1 when the output is a JSON object carrying every
// required key with the declared JSON type (string, number, boolean, object, array).
type SchemaHeuristic struct {
	Revision string
	Required []string
	Types    map[string]string
	Metric   string
	Critical bool
}

var _ contract.HeuristicEvaluator = (*SchemaHeuristic)(nil)

// Version identifies this heuristic revision.
func (heuristic *SchemaHeuristic) Version() domain.EvaluatorVersion {
	return domain.EvaluatorVersion{Kind: evaluatorKindHeuristic, Version: heuristic.Revision}
}

// Evaluate returns one schema-validity score.
func (heuristic *SchemaHeuristic) Evaluate(_ context.Context, evalCase domain.EvaluationCase) ([]domain.EvaluationScore, error) {
	if strings.TrimSpace(heuristic.Revision) == "" {
		return nil, fmt.Errorf("schema heuristic: version is required")
	}
	metric := metricOr(heuristic.Metric, domain.MetricSchemaValid)
	var object map[string]json.RawMessage
	if err := json.Unmarshal(evalCase.Output, &object); err != nil || object == nil {
		return []domain.EvaluationScore{scoreOf(metric, false, heuristic.Version(), heuristic.Critical, "output is not a JSON object")}, nil
	}
	var problems []string
	for _, key := range heuristic.Required {
		if _, ok := object[key]; !ok {
			problems = append(problems, "missing "+key)
		}
	}
	for key, want := range heuristic.Types {
		raw, ok := object[key]
		if !ok {
			continue
		}
		if got := jsonTypeOf(raw); got != want {
			problems = append(problems, fmt.Sprintf("%s is %s, want %s", key, got, want))
		}
	}
	return []domain.EvaluationScore{scoreOf(metric, len(problems) == 0, heuristic.Version(), heuristic.Critical, strings.Join(problems, "; "))}, nil
}

func jsonTypeOf(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "":
		return "missing"
	case trimmed[0] == '"':
		return "string"
	case trimmed[0] == '{':
		return "object"
	case trimmed[0] == '[':
		return "array"
	case trimmed == "true" || trimmed == "false":
		return "boolean"
	case trimmed == "null":
		return "null"
	default:
		return "number"
	}
}

// CitationSupportHeuristic scores the fraction of cited documents that appear
// in the arm's retrieval evidence; citations to unseen documents are unsupported.
type CitationSupportHeuristic struct {
	Revision string
	// Citation captures the document id in its first group; default `\[doc:([^\]\s]+)\]`.
	Citation *regexp.Regexp
	// RequireCitations scores an uncited answer 0 instead of 1.
	RequireCitations bool
	Metric           string
}

var _ contract.HeuristicEvaluator = (*CitationSupportHeuristic)(nil)

var defaultCitation = regexp.MustCompile(`\[doc:([^\]\s]+)\]`)

// Version identifies this heuristic revision.
func (heuristic *CitationSupportHeuristic) Version() domain.EvaluatorVersion {
	return domain.EvaluatorVersion{Kind: evaluatorKindHeuristic, Version: heuristic.Revision}
}

// Evaluate returns the supported-citation ratio.
func (heuristic *CitationSupportHeuristic) Evaluate(_ context.Context, evalCase domain.EvaluationCase) ([]domain.EvaluationScore, error) {
	if strings.TrimSpace(heuristic.Revision) == "" {
		return nil, fmt.Errorf("citation heuristic: version is required")
	}
	pattern := heuristic.Citation
	if pattern == nil {
		pattern = defaultCitation
	}
	if pattern.NumSubexp() < 1 {
		return nil, fmt.Errorf("citation heuristic: pattern needs a capturing group")
	}
	retrieved := make(map[string]struct{}, len(evalCase.Retrieval))
	for _, match := range evalCase.Retrieval {
		retrieved[match.DocumentID] = struct{}{}
	}
	cited := CitedDocuments(pattern, evalCase.Output)
	metric := metricOr(heuristic.Metric, domain.MetricCitationSupport)
	version := heuristic.Version()
	if len(cited) == 0 {
		return []domain.EvaluationScore{scoreOf(metric, !heuristic.RequireCitations, version, false, "no citations")}, nil
	}
	supported := 0
	for _, id := range cited {
		if _, ok := retrieved[id]; ok {
			supported++
		}
	}
	return []domain.EvaluationScore{{
		Metric: metric, Value: float64(supported) / float64(len(cited)), Confidence: 1, Evaluator: version,
		Rationale: fmt.Sprintf("%d of %d citations supported", supported, len(cited)),
	}}, nil
}

// CitedDocuments extracts distinct document ids in first-citation order.
func CitedDocuments(pattern *regexp.Regexp, output []byte) []string {
	seen := make(map[string]struct{})
	var ids []string
	for _, match := range pattern.FindAllSubmatch(output, -1) {
		id := string(match[1])
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

func scoreOf(metric string, passed bool, version domain.EvaluatorVersion, critical bool, rationale string) domain.EvaluationScore {
	value := 0.0
	if passed {
		value = 1
	}
	return domain.EvaluationScore{Metric: metric, Value: value, Confidence: 1, Evaluator: version, Rationale: rationale, Critical: critical && !passed}
}

func metricOr(metric, fallback string) string {
	if strings.TrimSpace(metric) == "" {
		return fallback
	}
	return metric
}
