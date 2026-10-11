package domain

import "fmt"

// ApprovalRoutingLimits tighten the approval_routing Studio saves, imports, adopts, and publishes
// below the structural bounds every decode enforces. Zero takes that bound; a limit may only
// tighten it, so the minimum duration may only rise and every maximum only fall.
type ApprovalRoutingLimits struct {
	MaxRoutesPerBinding int
	// MaxRoutes bounds the effective routes of one agent.
	MaxRoutes          int
	MaxToolPatterns    int
	MinDurationSeconds int64
	MaxDurationSeconds int64
	MaxWindowSeconds   int64
}

// Resolve returns the limits with every zero replaced by its bound, refusing a limit that
// loosens a bound or leaves no duration a route could use.
func (l ApprovalRoutingLimits) Resolve() (ApprovalRoutingLimits, error) {
	or := func(value, bound int64) int64 {
		if value == 0 {
			return bound
		}
		return value
	}
	resolved := ApprovalRoutingLimits{
		MaxRoutesPerBinding: int(or(int64(l.MaxRoutesPerBinding), MaxApprovalRoutesPerBinding)),
		MaxRoutes:           int(or(int64(l.MaxRoutes), MaxApprovalRoutes)),
		MaxToolPatterns:     int(or(int64(l.MaxToolPatterns), MaxApprovalRouteToolPatterns)),
		MinDurationSeconds:  or(l.MinDurationSeconds, MinApprovalRouteDurationSeconds),
		MaxDurationSeconds:  or(l.MaxDurationSeconds, MaxApprovalRouteDurationSeconds),
		MaxWindowSeconds:    or(l.MaxWindowSeconds, MaxApprovalRouteWindowSeconds),
	}
	var problem string
	switch {
	case resolved.MaxRoutesPerBinding < 1 || resolved.MaxRoutesPerBinding > MaxApprovalRoutesPerBinding:
		problem = fmt.Sprintf("routes per binding must be from 1 to %d", MaxApprovalRoutesPerBinding)
	case resolved.MaxRoutes < 1 || resolved.MaxRoutes > MaxApprovalRoutes:
		problem = fmt.Sprintf("routes per agent must be from 1 to %d", MaxApprovalRoutes)
	case resolved.MaxToolPatterns < 1 || resolved.MaxToolPatterns > MaxApprovalRouteToolPatterns:
		problem = fmt.Sprintf("tool patterns per route must be from 1 to %d", MaxApprovalRouteToolPatterns)
	case !approvalDuration(resolved.MinDurationSeconds, MaxApprovalRouteDurationSeconds):
		problem = fmt.Sprintf("the minimum duration must be from %d to %d seconds", MinApprovalRouteDurationSeconds, MaxApprovalRouteDurationSeconds)
	case !approvalDuration(resolved.MaxDurationSeconds, MaxApprovalRouteDurationSeconds):
		problem = fmt.Sprintf("the maximum duration must be from %d to %d seconds", MinApprovalRouteDurationSeconds, MaxApprovalRouteDurationSeconds)
	case !approvalDuration(resolved.MaxWindowSeconds, MaxApprovalRouteWindowSeconds):
		problem = fmt.Sprintf("the route window must be from %d to %d seconds", MinApprovalRouteDurationSeconds, MaxApprovalRouteWindowSeconds)
	case resolved.MinDurationSeconds > resolved.MaxDurationSeconds || resolved.MinDurationSeconds > resolved.MaxWindowSeconds:
		problem = fmt.Sprintf("the minimum duration of %d seconds exceeds the maximum duration or the route window", resolved.MinDurationSeconds)
	default:
		return resolved, nil
	}
	return ApprovalRoutingLimits{}, fmt.Errorf("%w: approval routing limits: %s", ErrValidation, problem)
}

func approvalDuration(seconds, ceiling int64) bool {
	return seconds >= MinApprovalRouteDurationSeconds && seconds <= ceiling
}
