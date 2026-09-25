package evaluation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// skillSelectionStop answers use_skill in a selection case, which scores only the choice.
const skillSelectionStop = "Selection check: stop here and reply with the id of the skill you loaded."

// SkillSelection turns each skill's example requests into golden cases scored on
// the first skill the agent loads through use_skill.
type SkillSelection struct {
	// Template carries every example field but the id, expected behavior and payload.
	Template domain.GoldenExample
	// Revision versions the choice heuristic.
	Revision string
	// Payload encodes one request as the turn input the agent receives.
	Payload func(request string) ([]byte, error)
	// PayloadURI names where the payload of one example is stored.
	PayloadURI func(exampleID string) string
}

// SkillSelectionCase is one example request that should load Skill.
type SkillSelectionCase struct {
	Skill     string
	Request   string
	Example   domain.GoldenExample
	Payload   []byte
	Heuristic *SkillChoiceHeuristic
	// Response answers use_skill in the case's sandbox and ends the run after the choice.
	Response domain.ToolResult
}

// Cases builds one case per example of every skill, with ids "skill-<skill id>-<n>".
func (selection SkillSelection) Cases(skills []domain.SkillDefinition) ([]SkillSelectionCase, error) {
	if strings.TrimSpace(selection.Revision) == "" || selection.Payload == nil || selection.PayloadURI == nil {
		return nil, fmt.Errorf("%w: skill selection needs a revision, a payload encoder, and a payload URI", domain.ErrValidation)
	}
	expected, err := json.Marshal(TrajectoryExpectation{Tools: []string{domain.UseSkillToolID}})
	if err != nil {
		return nil, fmt.Errorf("encode skill selection expectation: %w", err)
	}
	var cases []SkillSelectionCase
	for _, skill := range skills {
		response, err := json.Marshal(map[string]any{"skill": skill.SkillID, "procedure": skillSelectionStop, "tools": []string{}})
		if err != nil {
			return nil, fmt.Errorf("encode use_skill response for %s: %w", skill.SkillID, err)
		}
		for i, request := range skill.Examples {
			payload, err := selection.Payload(request)
			if err != nil {
				return nil, fmt.Errorf("encode example %d of skill %s: %w", i+1, skill.SkillID, err)
			}
			sum := sha256.Sum256(payload)
			example := selection.Template
			example.ExampleID = fmt.Sprintf("%s%s-%d", skillCasePrefix, skill.SkillID, i+1)
			example.ExpectedBehavior = expected
			example.Payload = domain.ObjectRef{URI: selection.PayloadURI(example.ExampleID), Digest: hex.EncodeToString(sum[:])}
			cases = append(cases, SkillSelectionCase{
				Skill: skill.SkillID, Request: request, Example: example, Payload: payload,
				Heuristic: &SkillChoiceHeuristic{Revision: selection.Revision, Skill: skill.SkillID},
				Response:  domain.ToolResult{Output: response},
			})
		}
	}
	return cases, nil
}

// Accuracy is the share of skill selection cases whose choice scored 1.
func (selection SkillSelection) Accuracy(scores map[string][]domain.EvaluationScore) float64 {
	cases, right := 0, 0
	for exampleID, caseScores := range scores {
		if !strings.HasPrefix(exampleID, skillCasePrefix) {
			continue
		}
		cases++
		for _, score := range caseScores {
			if score.Evaluator.Kind == evaluatorKindHeuristic && score.Evaluator.Version == selection.Revision && score.Value == 1 {
				right++
				break
			}
		}
	}
	if cases == 0 {
		return 0
	}
	return float64(right) / float64(cases)
}

const skillCasePrefix = "skill-"

// SkillChoiceHeuristic scores 1 when the first skill the agent loaded is Skill.
type SkillChoiceHeuristic struct {
	Revision string
	Skill    string
}

var _ contract.HeuristicEvaluator = (*SkillChoiceHeuristic)(nil)

func (heuristic *SkillChoiceHeuristic) Version() domain.EvaluatorVersion {
	return domain.EvaluatorVersion{Kind: evaluatorKindHeuristic, Version: heuristic.Revision}
}

func (heuristic *SkillChoiceHeuristic) Evaluate(_ context.Context, evalCase domain.EvaluationCase) ([]domain.EvaluationScore, error) {
	if strings.TrimSpace(heuristic.Revision) == "" || strings.TrimSpace(heuristic.Skill) == "" {
		return nil, fmt.Errorf("skill choice heuristic: revision and skill are required")
	}
	chosen := ""
	for _, event := range evalCase.Trajectory {
		if event.Kind != domain.TrajectoryToolCall || event.Name != domain.UseSkillToolID {
			continue
		}
		var args struct {
			Skill string `json:"skill"`
		}
		if json.Unmarshal(event.Payload, &args) == nil {
			chosen = args.Skill
		}
		break
	}
	rationale := "loaded " + chosen
	switch {
	case chosen == "":
		rationale = "loaded no skill; wanted " + heuristic.Skill
	case chosen != heuristic.Skill:
		rationale += "; wanted " + heuristic.Skill
	}
	return []domain.EvaluationScore{scoreOf(domain.MetricCorrectness, chosen == heuristic.Skill, heuristic.Version(), false, rationale)}, nil
}
