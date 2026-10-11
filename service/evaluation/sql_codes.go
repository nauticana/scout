package evaluation

import "github.com/nauticana/scout/domain"

// Codes quoted for the named SQL of this package.
const (
	sqlRunQueued     = "'" + string(domain.EvaluationQueued) + "'"
	sqlRunRunning    = "'" + string(domain.EvaluationRunning) + "'"
	sqlRunFailed     = "'" + string(domain.EvaluationFailed) + "'"
	sqlRoleBaseline  = "'" + string(domain.RoleBaseline) + "'"
	sqlRoleCandidate = "'" + string(domain.RoleCandidate) + "'"
	sqlScopeGate     = "'" + string(domain.GoldenScopeGate) + "'"
)
