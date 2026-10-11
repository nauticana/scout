package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// RubricJudge contains a configurable judge version and grading callback.
type RubricJudge struct {
	EvaluatorVersion domain.EvaluatorVersion
	GradeFunc        func(context.Context, domain.PrincipalRef, domain.EvaluationCase) (domain.RubricVerdict, error)
}

var _ contract.RubricJudge = (*RubricJudge)(nil)

// Version returns the configured evaluator version.
func (judge *RubricJudge) Version() domain.EvaluatorVersion { return judge.EvaluatorVersion }

// Grade invokes GradeFunc when configured.
func (judge *RubricJudge) Grade(ctx context.Context, principal domain.PrincipalRef, evalCase domain.EvaluationCase) (domain.RubricVerdict, error) {
	if judge.GradeFunc == nil {
		return domain.RubricVerdict{}, nil
	}
	return judge.GradeFunc(ctx, principal, evalCase)
}
