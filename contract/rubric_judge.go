package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// RubricJudge grades one executed example's output against the example's rubric and
// expected behavior with a model. Malformed or out-of-range model output is an error,
// never a passing grade; the verdict carries the judge's spend even with an error.
// The model spend is attributed to principal, the person or service the evaluation
// acts for; a zero principal is refused.
type RubricJudge interface {
	Version() domain.EvaluatorVersion
	Grade(ctx context.Context, principal domain.PrincipalRef, evalCase domain.EvaluationCase) (domain.RubricVerdict, error)
}
