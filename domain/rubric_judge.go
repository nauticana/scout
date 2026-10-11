package domain

import (
	"fmt"
	"strings"
)

// Metrics a rubric judge reports: the grade normalized to [0,1] over the judge's
// score range, and 1 or 0 for whether the grade reached the pass score.
const (
	MetricRubricScore = "rubric_score"
	MetricRubricPass  = "rubric_pass"
)

// RubricVerdict is a rubric judge's grade of one executed example. Scores is empty
// when the example has no rubric. Usage is the judge's own spend, present even when
// grading failed after the model ran; Unpriced marks spend the judge could not price.
type RubricVerdict struct {
	Scores   []EvaluationScore
	Usage    Usage
	Unpriced bool
}

// RubricJudgeSettings configures the model judge that grades saved test cases. Model
// pins provider, model, and model version; a grade is an integer in MinScore..MaxScore
// and passes at PassScore or above. MaxInputBytes bounds the judge's whole prompt and
// MaxVerdictBytes its raw verdict, zero taking the judge's default.
type RubricJudgeSettings struct {
	Model           ModelSelection
	MinScore        int
	MaxScore        int
	PassScore       int
	MaxOutputTokens int64
	MaxInputBytes   int
	MaxVerdictBytes int
}

// RubricJudgeMaxScale bounds the width of a rubric judge's score range.
const RubricJudgeMaxScale = 100

// Configured reports whether any part of the judge model is set; an unconfigured judge is not composed.
func (settings RubricJudgeSettings) Configured() bool {
	return settings.Model.Provider != "" || settings.Model.Model != "" || settings.Model.ModelVersion != ""
}

// Validate refuses a judge that is not pinned to one model version, a score range that
// is negative, empty, or wider than RubricJudgeMaxScale, a pass score outside it, or missing bounds.
func (settings RubricJudgeSettings) Validate() error {
	model := settings.Model
	switch {
	case strings.TrimSpace(model.Provider) == "" || strings.TrimSpace(model.Model) == "" || strings.TrimSpace(model.ModelVersion) == "":
		return fmt.Errorf("%w: a rubric judge needs a provider, model, and model version", ErrValidation)
	case settings.MinScore < 0 || settings.MinScore >= settings.MaxScore || settings.MaxScore-settings.MinScore > RubricJudgeMaxScale:
		return fmt.Errorf("%w: rubric judge scores need a non-negative range of 1 to %d steps", ErrValidation, RubricJudgeMaxScale)
	case settings.PassScore < settings.MinScore || settings.PassScore > settings.MaxScore:
		return fmt.Errorf("%w: rubric judge pass score must lie within its score range", ErrValidation)
	case settings.MaxOutputTokens <= 0 || settings.MaxInputBytes <= 0:
		return fmt.Errorf("%w: rubric judge output tokens and input bytes must be positive", ErrValidation)
	case settings.MaxVerdictBytes < 0:
		return fmt.Errorf("%w: rubric judge verdict bytes cannot be negative", ErrValidation)
	}
	return nil
}
