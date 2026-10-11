package domain

import (
	"errors"
	"testing"
)

func TestApprovalRoutingLimitsResolve(t *testing.T) {
	resolved, err := ApprovalRoutingLimits{}.Resolve()
	want := ApprovalRoutingLimits{
		MaxRoutesPerBinding: MaxApprovalRoutesPerBinding, MaxRoutes: MaxApprovalRoutes, MaxToolPatterns: MaxApprovalRouteToolPatterns,
		MinDurationSeconds: MinApprovalRouteDurationSeconds, MaxDurationSeconds: MaxApprovalRouteDurationSeconds,
		MaxWindowSeconds: MaxApprovalRouteWindowSeconds,
	}
	if err != nil || resolved != want {
		t.Fatalf("zero limits = %+v, %v, want the structural bounds %+v", resolved, err, want)
	}
	tight := ApprovalRoutingLimits{MaxRoutesPerBinding: 2, MaxRoutes: 3, MaxToolPatterns: 1, MinDurationSeconds: 600, MaxDurationSeconds: 3600, MaxWindowSeconds: 7200}
	if resolved, err = tight.Resolve(); err != nil || resolved != tight {
		t.Fatalf("tight limits = %+v, %v", resolved, err)
	}
	for name, limits := range map[string]ApprovalRoutingLimits{
		"negative routes per binding": {MaxRoutesPerBinding: -1},
		"routes per binding above":    {MaxRoutesPerBinding: MaxApprovalRoutesPerBinding + 1},
		"routes above":                {MaxRoutes: MaxApprovalRoutes + 1},
		"tools above":                 {MaxToolPatterns: MaxApprovalRouteToolPatterns + 1},
		"minimum below the floor":     {MinDurationSeconds: MinApprovalRouteDurationSeconds - 1},
		"maximum above the ceiling":   {MaxDurationSeconds: MaxApprovalRouteDurationSeconds + 1},
		"maximum below the floor":     {MaxDurationSeconds: 30},
		"window above the ceiling":    {MaxWindowSeconds: MaxApprovalRouteWindowSeconds + 1},
		"minimum above the maximum":   {MinDurationSeconds: 7200, MaxDurationSeconds: 3600},
		"minimum above the window":    {MinDurationSeconds: 7200, MaxWindowSeconds: 3600},
	} {
		if _, err := limits.Resolve(); !errors.Is(err, ErrValidation) {
			t.Fatalf("%s: want ErrValidation, got %v", name, err)
		}
	}
}
